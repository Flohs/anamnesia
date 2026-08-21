// recover.go: ingesting transcript tails that no checkpoint ever claimed.
//
// A checkpoint fires on PreCompact and SessionEnd. If a session dies
// without either — a crash, a kill, a machine losing power — the work
// since the last checkpoint is never ingested. It is not lost: Claude
// Code writes the transcript to disk continuously, and the offset file
// records exactly how far anamnesia read, including the transcript's
// absolute path. Nothing ever went back for the rest.
//
// So this is a catch-up, not a second capture mechanism. It reads the
// same transcripts, cuts the same segments, and advances the same
// offsets. The only new judgement is deciding a session is over, which
// is a question about the file rather than the session: a transcript
// nobody has written to for ingest.recover_idle is not being written to
// any more.

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var recoverDryRun bool

var recoverCmd = func() *cobra.Command {
	c := &cobra.Command{
		Use:   "recover",
		Short: "Ingest transcript tails from sessions that ended without a checkpoint",
		Long: "Find sessions whose transcript has unread content and has not been\n" +
			"written to for a while, and ingest what is missing.\n\n" +
			"A session that crashes never fires SessionEnd, so its last stretch of\n" +
			"work is never checkpointed. The transcript is still on disk, so this\n" +
			"reads it. Run automatically at session start; safe to run by hand.",
		Args: cobra.NoArgs,
		RunE: runRecover,
	}
	c.Flags().BoolVar(&recoverDryRun, "dry-run", false, "report what would be ingested without ingesting it")
	return c
}()

// strandedSession is one transcript with bytes anamnesia has not read.
type strandedSession struct {
	SessionID string
	Path      string
	Offset    int64
	Size      int64
}

// Unread is how much of the transcript was never checkpointed.
func (s strandedSession) Unread() int64 { return s.Size - s.Offset }

// strandedSessions lists transcripts carrying unread bytes whose file has
// been untouched for at least idle.
//
// Four things disqualify a candidate, and each is a real case rather than
// defensive padding: a transcript that no longer exists (offset files
// outlive them), one already read to the end (the ordinary case), one
// written to recently (still live — ingesting it would race the session's
// own checkpoint and pay for extraction twice), and one shorter than its
// recorded offset (replaced or truncated, so there is no tail to read).
func strandedSessions(idle time.Duration) ([]strandedSession, error) {
	dir, err := offsetsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	cutoff := time.Now().Add(-idle)
	var out []strandedSession
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var rec offsetRecord
		if err := json.Unmarshal(raw, &rec); err != nil || rec.Path == "" {
			continue
		}
		info, err := os.Stat(rec.Path)
		if err != nil {
			continue
		}
		if info.Size() <= rec.Offset {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		out = append(out, strandedSession{
			SessionID: strings.TrimSuffix(e.Name(), ".json"),
			Path:      rec.Path,
			Offset:    rec.Offset,
			Size:      info.Size(),
		})
	}
	return out, nil
}

// transcriptCWD reads the working directory a transcript records.
//
// The offset file knows only the transcript's path, and the directory
// name in that path is the working directory with every separator
// flattened to a dash — /Users/floh/Work/smoxy/hub-2.0/hub-api becomes
// -Users-floh-Work-smoxy-hub-2-0-hub-api, which cannot be reversed. The
// transcript records the real one, so a recovered tail is filed under the
// project it came from rather than wherever this command happens to run.
func transcriptCWD(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for i := 0; sc.Scan() && i < 200; i++ {
		var rec struct {
			CWD string `json:"cwd"`
		}
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue
		}
		if rec.CWD != "" {
			return rec.CWD
		}
	}
	return ""
}

// projectForDir resolves the slug a hook running in dir would file under:
// the nearest .anamnesia.toml wins, otherwise the repository directory
// name, which is what identity.project defaults to.
func projectForDir(dir string) string {
	root := gitToplevelOrCWD(dir)
	if root == "" {
		root = dir
	}
	for _, candidate := range []string{root, dir} {
		p := filepath.Join(candidate, projectConfigName)
		if !fileExists(p) {
			continue
		}
		if slug := projectSlugFromFile(p); slug != "" {
			return slug
		}
	}
	return sanitizeSlug(filepath.Base(root))
}

// projectSlugFromFile reads identity.project out of a project config.
func projectSlugFromFile(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	section := ""
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.Trim(line, "[]")
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || section != "identity" || strings.TrimSpace(k) != "project" {
			continue
		}
		return sanitizeSlug(strings.Trim(strings.TrimSpace(v), `"'`))
	}
	return ""
}

func runRecover(cmd *cobra.Command, _ []string) error {
	hc, err := loadHostConfig()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if !hc.Bool("ingest.recover_stranded") && !recoverDryRun {
		fmt.Fprintln(out, "ingest.recover_stranded is off; nothing to do")
		return nil
	}
	stranded, err := strandedSessions(hc.Dur("ingest.recover_idle"))
	if err != nil {
		return err
	}
	if len(stranded) == 0 {
		fmt.Fprintln(out, "no stranded transcripts")
		return nil
	}
	ctx := context.Background()
	recovered := 0
	for _, s := range stranded {
		cwd := transcriptCWD(s.Path)
		if cwd == "" {
			// Without a working directory there is no way to know which
			// project the tail belongs to, and filing it under the wrong
			// one is worse than leaving it.
			fmt.Fprintf(out, "  skip %s: the transcript records no working directory\n", s.SessionID)
			continue
		}
		project := projectForDir(cwd)
		fmt.Fprintf(out, "  %s  %d unread byte(s) -> %s\n", s.SessionID, s.Unread(), project)
		if recoverDryRun {
			continue
		}
		note, err := doCheckpoint(ctx, hc, claudeHookInput{
			SessionID:      s.SessionID,
			CWD:            cwd,
			TranscriptPath: s.Path,
		}, "recover", checkpointScope{Project: project})
		if err != nil {
			// One unreadable transcript must not stop the rest.
			fmt.Fprintf(out, "    failed: %v\n", err)
			continue
		}
		fmt.Fprintf(out, "    %s\n", note)
		recovered++
	}
	if recoverDryRun {
		fmt.Fprintf(out, "\nNothing was changed. %d transcript(s) would be recovered.\n", len(stranded))
		return nil
	}
	fmt.Fprintf(out, "\n✦ recovered %d of %d stranded transcript(s)\n", recovered, len(stranded))
	return nil
}

// spawnRecover starts `anamnesia recover` detached and returns at once.
//
// Called from the session-start hook, which must stay fast and must never
// break a session. Recovery is usually a stat() per offset file and an
// immediate exit, but a crashed session can leave megabytes to segment
// and post, and a hook is not the place to discover that. Detaching also
// means the sweep survives the hook process, so a long recovery finishes
// rather than being cut off when Claude Code moves on.
//
// Every failure here is silent on purpose: not recovering a tail is a
// missed opportunity, and the tail stays on disk for the next session to
// find. Reporting it into the hook's output would put noise in front of
// the user for something that costs them nothing.
func spawnRecover(hc *hostConfig) {
	if !hc.Bool("ingest.recover_stranded") {
		return
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	// Cheap enough to check inline: if there is nothing stranded, spawning
	// a process to discover that is more expensive than the answer.
	stranded, err := strandedSessions(hc.Dur("ingest.recover_idle"))
	if err != nil || len(stranded) == 0 {
		return
	}
	cmd := exec.Command(self, "recover")
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	cmd.SysProcAttr = detachAttr()
	if err := cmd.Start(); err != nil {
		return
	}
	// Released rather than waited on: this hook is about to exit, and the
	// sweep must outlive it.
	_ = cmd.Process.Release()
}
