package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRefilingCreatesTheProjectFileWhenThereIsNone is the half of the
// move that is easy to leave out and silently undoes the other half.
//
// A repository with no .anamnesia.toml takes its slug from the directory
// name. Moving its rows into "smoxy" without writing the file means the
// very next session resolves "hub-api" again, files new memories there,
// and the split you just repaired reopens — with the older memories now
// somewhere else, which is worse than before the move.
func TestRefilingCreatesTheProjectFileWhenThereIsNone(t *testing.T) {
	dir := t.TempDir()
	path, err := refileProjectConfig(dir, "smoxy")
	if err != nil {
		t.Fatalf("refile: %v", err)
	}
	if path != filepath.Join(dir, projectConfigName) {
		t.Errorf("path = %q, want the repository's %s", path, projectConfigName)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(body), `project = "smoxy"`) {
		t.Errorf("written file does not file this repository under smoxy:\n%s", body)
	}
}

// TestRefilingAnExistingFileKeepsEverythingElse: the file is committed
// with the repository and may carry other overrides. Rewriting it
// wholesale to change one key would silently drop them.
func TestRefilingAnExistingFileKeepsEverythingElse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, projectConfigName)
	existing := "# hand written\n\n[identity]\nproject = \"hub-api\"\nuser = \"floh\"\n"
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := refileProjectConfig(dir, "smoxy"); err != nil {
		t.Fatalf("refile: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, `project = "smoxy"`) {
		t.Errorf("project was not updated:\n%s", got)
	}
	if strings.Contains(got, `project = "hub-api"`) {
		t.Errorf("the old slug is still there:\n%s", got)
	}
	if !strings.Contains(got, `user = "floh"`) {
		t.Errorf("an unrelated override was dropped:\n%s", got)
	}
	if !strings.Contains(got, "# hand written") {
		t.Errorf("a hand written comment was dropped:\n%s", got)
	}
}
