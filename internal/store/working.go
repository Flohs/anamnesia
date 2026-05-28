package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/flohs/anamnesia-open-source/pkg/anamnesia"
)

// AppendWorking inserts the next position for (scope, session). Position
// is assigned server-side by selecting max+1 in the same tx.
func (s *Store) AppendWorking(ctx context.Context, w *anamnesia.WorkingEntry) error {
	if w.SessionID == uuid.Nil {
		return errors.New("working: session_id required")
	}
	if !w.Role.Valid() {
		w.Role = anamnesia.WorkingObservation
	}
	if w.CreatedAt.IsZero() {
		w.CreatedAt = time.Now().UTC()
	}
	if w.ExpiresAt.IsZero() {
		w.ExpiresAt = w.CreatedAt.Add(24 * time.Hour)
	}
	var metaJSON []byte
	if w.Meta != nil {
		b, err := json.Marshal(w.Meta)
		if err != nil {
			return fmt.Errorf("marshal meta: %w", err)
		}
		metaJSON = b
	}
	return s.Tx(ctx, func(tx pgx.Tx) error {
		var pos int
		err := tx.QueryRow(ctx,
			`SELECT COALESCE(MAX(position),-1)+1 FROM working_memory
			 WHERE user_id = $1 AND session_id = $2`,
			w.Scope.UserID, w.SessionID,
		).Scan(&pos)
		if err != nil {
			return err
		}
		w.Position = pos
		return tx.QueryRow(ctx, `
			INSERT INTO working_memory
				(user_id, project_id, session_id, position, role, body, meta, expires_at, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9)
			RETURNING id`,
			w.Scope.UserID, w.Scope.ProjectID, w.SessionID, w.Position,
			string(w.Role), w.Body, nilOrJSON(metaJSON), w.ExpiresAt, w.CreatedAt,
		).Scan(&w.ID)
	})
}

// RecallWorking returns the in-session entries newest-last, optionally
// limited to the most recent N.
func (s *Store) RecallWorking(ctx context.Context, scope anamnesia.Scope, sessionID uuid.UUID, limit int) ([]*anamnesia.WorkingEntry, error) {
	q := `SELECT id, user_id, project_id, session_id, position, role, body, meta, folded_into, expires_at, created_at
		  FROM working_memory
		  WHERE user_id = $1 AND session_id = $2 AND folded_into IS NULL
		  ORDER BY position ASC`
	args := []any{scope.UserID, sessionID}
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT $%d", len(args)+1)
		args = append(args, limit)
	}
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*anamnesia.WorkingEntry
	for rows.Next() {
		w, err := scanWorking(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// FoldWorking marks every unfolded entry for (scope, session) as
// belonging to the given experience. Returns the count folded.
func (s *Store) FoldWorking(ctx context.Context, scope anamnesia.Scope, sessionID, experienceID uuid.UUID) (int, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE working_memory
		   SET folded_into = $3
		 WHERE user_id = $1 AND session_id = $2 AND folded_into IS NULL`,
		scope.UserID, sessionID, experienceID)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// PurgeExpiredWorking deletes any unfolded entries past their TTL. Run
// from the background worker.
func (s *Store) PurgeExpiredWorking(ctx context.Context) (int, error) {
	tag, err := s.Pool.Exec(ctx,
		`DELETE FROM working_memory WHERE folded_into IS NULL AND expires_at < now()`)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// ─── audit log ────────────────────────────────────────────────────────

// WriteAudit appends one row to audit_log.
func (s *Store) WriteAudit(ctx context.Context, e *anamnesia.AuditEntry) error {
	if e.Op == "" || e.Target == "" {
		return errors.New("audit: op + target required")
	}
	var payloadJSON []byte
	if e.Payload != nil {
		b, err := json.Marshal(e.Payload)
		if err != nil {
			return fmt.Errorf("marshal audit payload: %w", err)
		}
		payloadJSON = b
	}
	return s.Pool.QueryRow(ctx, `
		INSERT INTO audit_log (user_id, project_id, op, target, target_id, actor, payload)
		VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb)
		RETURNING id, at`,
		e.UserID, e.ProjectID, e.Op, e.Target, e.TargetID,
		actorOrSystem(e.Actor), nilOrJSON(payloadJSON),
	).Scan(&e.ID, &e.At)
}

// AuditTail returns the most recent N audit rows, optionally filtered.
func (s *Store) AuditTail(ctx context.Context, scope anamnesia.Scope, n int) ([]*anamnesia.AuditEntry, error) {
	if n <= 0 {
		n = 50
	}
	args := []any{scope.UserID}
	where := []string{"user_id = $1"}
	if scope.ProjectID != nil {
		args = append(args, *scope.ProjectID)
		where = append(where, fmt.Sprintf("project_id = $%d", len(args)))
	}
	args = append(args, n)
	q := fmt.Sprintf(`SELECT id, at, user_id, project_id, op, target, target_id, actor, payload
		FROM audit_log WHERE %s ORDER BY at DESC LIMIT $%d`,
		strings.Join(where, " AND "), len(args))
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*anamnesia.AuditEntry
	for rows.Next() {
		var (
			e   anamnesia.AuditEntry
			pay []byte
		)
		if err := rows.Scan(&e.ID, &e.At, &e.UserID, &e.ProjectID, &e.Op, &e.Target,
			&e.TargetID, &e.Actor, &pay); err != nil {
			return nil, err
		}
		if len(pay) > 0 {
			_ = json.Unmarshal(pay, &e.Payload)
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// AuditForSubject returns audit_log rows whose target/target_id match,
// newest first. `kind` is e.g. "fact" | "experience" | "entity" |
// "commitment"; `id` is the row's primary key. Powers the per-subject
// provenance view (where did this fact come from, who changed it).
func (s *Store) AuditForSubject(ctx context.Context, kind string, id uuid.UUID, n int) ([]*anamnesia.AuditEntry, error) {
	if n <= 0 {
		n = 50
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id, at, user_id, project_id, op, target, target_id, actor, payload
		FROM audit_log
		WHERE target = $1 AND target_id = $2
		ORDER BY at DESC
		LIMIT $3`, kind, id, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*anamnesia.AuditEntry
	for rows.Next() {
		var (
			e   anamnesia.AuditEntry
			pay []byte
		)
		if err := rows.Scan(&e.ID, &e.At, &e.UserID, &e.ProjectID, &e.Op, &e.Target,
			&e.TargetID, &e.Actor, &pay); err != nil {
			return nil, err
		}
		if len(pay) > 0 {
			_ = json.Unmarshal(pay, &e.Payload)
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

func actorOrSystem(a string) string {
	if a == "" {
		return "system"
	}
	return a
}

func scanWorking(row rowScanner) (*anamnesia.WorkingEntry, error) {
	var (
		w        anamnesia.WorkingEntry
		project  *uuid.UUID
		role     string
		metaJSON []byte
		folded   *uuid.UUID
	)
	err := row.Scan(
		&w.ID, &w.Scope.UserID, &project, &w.SessionID, &w.Position,
		&role, &w.Body, &metaJSON, &folded, &w.ExpiresAt, &w.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	w.Scope.ProjectID = project
	w.Role = anamnesia.WorkingRole(role)
	w.FoldedInto = folded
	if len(metaJSON) > 0 {
		_ = json.Unmarshal(metaJSON, &w.Meta)
	}
	return &w, nil
}
