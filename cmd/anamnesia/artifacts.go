// artifacts.go: `anamnesia artifacts`, and the backfill that recovers the
// ones published before anamnesia started watching for them.
//
// The backfill is deliberately not part of `anamnesia recover`. Recover is
// driven by the per-session read offsets and feeds what it finds to the
// extractor; both are wrong here. Every artifact worth recovering sits in
// bytes that were already read, so an offset-driven sweep skips them by
// construction, and there are 49 offset files against 2,502 transcripts,
// so it can only see the recent sliver anyway. And a URL must not go
// through a gate that decides whether it was interesting.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/flohs/anamnesia/internal/artifacts"
)

var (
	artifactsLimit       int
	artifactsBackfillAll bool
	artifactsDryRun      bool
)

var artifactsCmd = func() *cobra.Command {
	c := &cobra.Command{
		Use:   "artifacts",
		Short: "List the artifacts Claude Code has published",
		Long: "Artifacts are the pages Claude Code publishes to claude.ai. They are\n" +
			"recorded as they are made, and surfaced again when a prompt matches\n" +
			"one. This lists them newest first.",
		Args: cobra.NoArgs,
		RunE: runArtifactsList,
	}
	c.Flags().IntVar(&artifactsLimit, "limit", 25, "how many to show")
	c.AddCommand(artifactsBackfillCmd())
	return c
}()

func artifactsBackfillCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "backfill",
		Short: "Recover artifacts from transcripts that predate the hook",
		Long: "Read every Claude Code transcript, pair each Artifact tool call with\n" +
			"its result, and record what was published.\n\n" +
			"Idempotent: an artifact is keyed by its own id, so re-running it\n" +
			"updates rather than duplicates. That also makes it the repair path\n" +
			"when the server was down at the moment something was published.\n\n" +
			"The page text is only recoverable while the published file is still\n" +
			"on disk, and a session scratchpad is cleaned up, so most recovered\n" +
			"artifacts are pointers without their content.",
		Args: cobra.NoArgs,
		RunE: runArtifactsBackfill,
	}
	c.Flags().BoolVar(&artifactsDryRun, "dry-run", false, "report what would be recorded and change nothing")
	c.Flags().BoolVar(&artifactsBackfillAll, "all", false, "scan every project, not only this one")
	return c
}

func runArtifactsList(cmd *cobra.Command, _ []string) error {
	hc, err := loadHostConfig()
	if err != nil {
		return err
	}
	q := fmt.Sprintf("?user=%s&limit=%d", url.QueryEscape(hc.User()), artifactsLimit)
	if p := hc.Project(); p != "" && !artifactsBackfillAll {
		q += "&project=" + url.QueryEscape(p)
	}
	var resp struct {
		Artifacts []struct {
			URL         string    `json:"url"`
			Title       string    `json:"title"`
			Description string    `json:"description"`
			Project     string    `json:"project"`
			OccurredAt  time.Time `json:"occurred_at"`
		} `json:"artifacts"`
	}
	if err := httpGetJSON(cmd.Context(), hc, "/v1/artifacts"+q, &resp); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if len(resp.Artifacts) == 0 {
		fmt.Fprintln(out, "no artifacts recorded yet")
		fmt.Fprintln(out, "  `anamnesia artifacts backfill` reads the ones already in your transcripts")
		return nil
	}
	for _, a := range resp.Artifacts {
		label := a.Title
		if label == "" {
			label = a.Description
		}
		fmt.Fprintf(out, "%s  %-24s %s\n", a.OccurredAt.Local().Format("2006-01-02 15:04"),
			truncateRunes(a.Project, 24), truncateRunes(label, 60))
		fmt.Fprintf(out, "%s%s\n", strings.Repeat(" ", 18), a.URL)
	}
	return nil
}

// found is one artifact reconstructed from a transcript.
type found struct {
	pub         artifacts.Publish
	title       string
	description string
	firstSeen   time.Time
	lastSeen    time.Time
	firstCWD    string
}

func runArtifactsBackfill(cmd *cobra.Command, _ []string) error {
	hc, err := loadHostConfig()
	if err != nil {
		return err
	}
	root, err := claudeProjectsDir()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()

	files, err := transcriptFiles(root)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "scanning %d transcripts under %s\n", len(files), root)

	byID := map[uuid.UUID]*found{}
	mentioned := map[string]bool{}
	for _, f := range files {
		scanTranscript(f, byID, mentioned)
	}

	// A URL that appears only as prose, with no tool call to pair it
	// with, yields a link and no idea what it is. Reported rather than
	// dropped in silence: a backfill that hid them would read as
	// complete when it was not.
	unpaired := 0
	for u := range mentioned {
		if id, ok := artifacts.UUIDFromURL(u); !ok || byID[id] == nil {
			unpaired++
		}
	}

	list := make([]*found, 0, len(byID))
	for _, f := range byID {
		list = append(list, f)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].lastSeen.Before(list[j].lastSeen) })

	withBody, recorded := 0, 0
	for _, f := range list {
		req := artifactRequest{
			User:         hc.User(),
			Project:      projectForTranscript(f.firstCWD, hc),
			ArtifactUUID: f.pub.UUID.String(),
			URL:          f.pub.URL,
			Title:        f.title,
			Description:  f.description,
			FilePath:     f.pub.FilePath,
			OccurredAt:   f.lastSeen,
			Meta:         map[string]any{"origin": "backfill"},
		}
		if title, body, err := readArtifactFile(f.pub.FilePath); err == nil {
			req.Body = body
			withBody++
			if req.Title == "" {
				req.Title = title
			}
		} else {
			req.Meta["body_missing"] = true
		}
		if artifactsDryRun {
			fmt.Fprintf(out, "  would record %s  %s  %s\n",
				f.lastSeen.Format("2006-01-02"), req.Project, truncateRunes(labelOf(req), 56))
			continue
		}
		var resp artifactResponse
		if err := httpPost(cmd.Context(), hc, "/v1/artifacts", req, &resp); err != nil {
			return fmt.Errorf("record %s: %w", f.pub.URL, err)
		}
		recorded++
	}

	verb := "recorded"
	if artifactsDryRun {
		verb = "would record"
		recorded = len(list)
	}
	fmt.Fprintf(out, "✦ %s %d artifacts, %d with their page text\n", verb, recorded, withBody)
	if withBody < len(list) {
		fmt.Fprintf(out, "  %d had no readable file left, so they are pointers without content\n",
			len(list)-withBody)
	}
	if unpaired > 0 {
		fmt.Fprintf(out, "  skipped %d URLs that appear only as text, with no publish to describe them\n", unpaired)
	}
	if artifactsDryRun {
		fmt.Fprintln(out, "\nNothing was changed. Run again without --dry-run to record them.")
	}
	return nil
}

func labelOf(r artifactRequest) string {
	if r.Title != "" {
		return r.Title
	}
	return r.Description
}

// scanTranscript pairs Artifact tool calls with their results.
func scanTranscript(path string, byID map[uuid.UUID]*found, mentioned map[string]bool) {
	f, err := os.Open(path) //nolint:gosec // the user's own transcript
	if err != nil {
		return
	}
	defer f.Close()

	type call struct {
		in artifactToolInput
		ts time.Time
		wd string
	}
	calls := map[string]call{}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 32<<20)
	for sc.Scan() {
		line := sc.Bytes()
		for _, u := range artifacts.URLsIn(string(line)) {
			mentioned[u] = true
		}
		var rec struct {
			CWD       string    `json:"cwd"`
			Timestamp time.Time `json:"timestamp"`
			Message   struct {
				Content []struct {
					Type      string          `json:"type"`
					ID        string          `json:"id"`
					Name      string          `json:"name"`
					Input     json.RawMessage `json:"input"`
					ToolUseID string          `json:"tool_use_id"`
					Content   json.RawMessage `json:"content"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		for _, c := range rec.Message.Content {
			switch {
			case c.Type == "tool_use" && c.Name == "Artifact":
				var in artifactToolInput
				_ = json.Unmarshal(c.Input, &in)
				calls[c.ID] = call{in: in, ts: rec.Timestamp, wd: rec.CWD}
			case c.Type == "tool_result" && c.ToolUseID != "":
				prev, ok := calls[c.ToolUseID]
				if !ok {
					continue
				}
				pub, ok := artifacts.ParsePublish(string(c.Content))
				if !ok {
					continue
				}
				if prev.in.FilePath != "" {
					pub.FilePath = prev.in.FilePath
				}
				record(byID, pub, prev.in, prev.ts, prev.wd)
			}
		}
	}
}

// record merges one publish into the set. The project comes from the
// earliest publish, matching the store's rule that an artifact belongs to
// the work that made it; the timestamp and the labels come from the
// latest, because that is the state it is in now.
func record(byID map[uuid.UUID]*found, pub artifacts.Publish, in artifactToolInput, ts time.Time, cwd string) {
	f, ok := byID[pub.UUID]
	if !ok {
		byID[pub.UUID] = &found{
			pub: pub, title: in.Title, description: in.Description,
			firstSeen: ts, lastSeen: ts, firstCWD: cwd,
		}
		return
	}
	if ts.Before(f.firstSeen) {
		f.firstSeen, f.firstCWD = ts, cwd
	}
	if ts.After(f.lastSeen) {
		f.lastSeen = ts
		f.pub = pub
		if in.Title != "" {
			f.title = in.Title
		}
		if in.Description != "" {
			f.description = in.Description
		}
	}
}

// projectForTranscript resolves the slug a hook running in that directory
// would have filed under, falling back to the configured project when the
// directory is gone.
func projectForTranscript(cwd string, hc *hostConfig) string {
	if cwd != "" {
		if slug := projectForDir(cwd); slug != "" {
			return slug
		}
	}
	return hc.Project()
}

func claudeProjectsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

func transcriptFiles(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable corner is not a reason to abandon the sweep
		}
		if !d.IsDir() && strings.HasSuffix(p, ".jsonl") {
			out = append(out, p)
		}
		return nil
	})
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("no transcripts at %s", root)
	}
	return out, err
}

func httpGetJSON(ctx context.Context, hc *hostConfig, path string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hc.ServerURL()+path, nil)
	if err != nil {
		return err
	}
	if tok := hc.Token(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("server %s: %s: %s", path, res.Status, strings.TrimSpace(string(raw)))
	}
	return json.Unmarshal(raw, dst)
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
