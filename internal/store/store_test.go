package store

import (
	"context"
	"os"
	"testing"

	"github.com/flohs/anamnesia/pkg/anamnesia"
)

func TestDeleteUserCascadesItsMemory(t *testing.T) {
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	uid, err := st.EnsureUser(ctx, "eval-cascade-test")
	if err != nil {
		t.Fatal(err)
	}
	scope := anamnesia.Scope{UserID: uid}
	if err := st.UpsertFact(ctx, &anamnesia.Fact{
		Scope: scope, Key: "eval.cascade", Value: map[string]any{"v": 1},
	}); err != nil {
		t.Fatal(err)
	}

	deleted, err := st.DeleteUser(ctx, "eval-cascade-test")
	if err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if !deleted {
		t.Fatal("DeleteUser reported no row deleted")
	}
	if _, ok, err := st.LookupUser(ctx, "eval-cascade-test"); err != nil || ok {
		t.Errorf("user still resolves after delete (ok=%v, err=%v)", ok, err)
	}
	var n int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM facts WHERE user_id = $1`, uid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d facts survived the user delete; the cascade did not fire", n)
	}
}

func TestDeleteUserThatDoesNotExistIsNotAnError(t *testing.T) {
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	deleted, err := st.DeleteUser(ctx, "no-such-eval-user-ever")
	if err != nil {
		t.Errorf("deleting an absent user should be a no-op, got %v", err)
	}
	if deleted {
		t.Error("reported a deletion that cannot have happened")
	}
}

// TestDeleteUserIsBlockedByACommitment pins the one exception to the cascade.
// Every other table that references users(id) declares ON DELETE CASCADE;
// commitments (migration 0007) does not, and a DELETE is atomic, so a single
// commitment row aborts the whole delete and nothing is removed — not the
// commitment, not the facts, not the experiences.
//
// `anamnesia eval` works around this by deleting the scope's commitments
// first. This test is the only executable record of why that workaround
// exists: if a later migration adds the cascade, this fails and the
// workaround can go.
func TestDeleteUserIsBlockedByACommitment(t *testing.T) {
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	uid, err := st.EnsureUser(ctx, "eval-commitment-block-test")
	if err != nil {
		t.Fatal(err)
	}
	scope := anamnesia.Scope{UserID: uid}
	if err := st.UpsertFact(ctx, &anamnesia.Fact{
		Scope: scope, Key: "eval.blocked", Value: map[string]any{"v": 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordCommitment(ctx, &anamnesia.Commitment{
		Scope: scope, Body: "something owed, blocking the delete",
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = st.Pool.Exec(context.Background(), `DELETE FROM commitments WHERE user_id = $1`, uid)
		_, _ = st.DeleteUser(context.Background(), "eval-commitment-block-test")
	})

	if _, err := st.DeleteUser(ctx, "eval-commitment-block-test"); err == nil {
		t.Fatal("DeleteUser succeeded with a commitment row present; the cascade gap may have been fixed — if so, delete eval's workaround in deleteEvalScope and this test")
	}

	// The delete is atomic: the fact must still be there.
	var n int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM facts WHERE user_id = $1`, uid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d facts survived the aborted delete, want 1: the DELETE was not atomic", n)
	}

	// And the workaround must work: commitments first, then the user.
	if _, err := st.Pool.Exec(ctx, `DELETE FROM commitments WHERE user_id = $1`, uid); err != nil {
		t.Fatal(err)
	}
	deleted, err := st.DeleteUser(ctx, "eval-commitment-block-test")
	if err != nil {
		t.Fatalf("delete after clearing commitments: %v", err)
	}
	if !deleted {
		t.Error("DeleteUser reported no row deleted after the commitments were cleared")
	}
}
