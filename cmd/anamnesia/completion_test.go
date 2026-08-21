package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// completionHome points both HOME and ANAMNESIA_HOME at a temp directory.
// Installing completion edits a shell rc, so a test that skipped this
// would rewrite the developer's own ~/.zshrc.
func completionHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv(homeEnv, filepath.Join(dir, ".anamnesia"))
	backedUp = map[string]bool{} // package-level; a stale entry skips the backup
	return dir
}

// countSourcingLines counts rc lines naming the completion script. The
// block names the path twice on one line (the guard and the source), so
// counting raw occurrences reports a duplicate that is not there.
func countSourcingLines(body string) int {
	n := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "anamnesia.zsh") {
			n++
		}
	}
	return n
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func TestCompletionWritesTheScriptAndSourcesIt(t *testing.T) {
	home := completionHome(t)
	rc := filepath.Join(home, ".zshrc")
	original := "# my zshrc\nexport FOO=1\n"
	if err := os.WriteFile(rc, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := installCompletionFor(shellZsh, io.Discard); err != nil {
		t.Fatalf("install: %v", err)
	}

	script := filepath.Join(home, ".anamnesia", "completions", "anamnesia.zsh")
	if body := readFile(t, script); !strings.Contains(body, "#compdef anamnesia") {
		t.Errorf("%s does not look like a zsh completion script:\n%s", script, body)
	}
	body := readFile(t, rc)
	if !strings.HasPrefix(body, original) {
		t.Errorf("the existing rc was not preserved:\n%s", body)
	}
	if !strings.Contains(body, completionMarker) {
		t.Errorf("no marker in the rc:\n%s", body)
	}
	if !strings.Contains(body, "$HOME/.anamnesia/completions/anamnesia.zsh") {
		t.Errorf("the rc does not source the script:\n%s", body)
	}
}

func TestCompletionInstallIsIdempotent(t *testing.T) {
	home := completionHome(t)
	if err := os.WriteFile(filepath.Join(home, ".zshrc"), []byte("export FOO=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := installCompletionFor(shellZsh, io.Discard); err != nil {
			t.Fatalf("install %d: %v", i, err)
		}
	}
	body := readFile(t, filepath.Join(home, ".zshrc"))
	if n := countSourcingLines(body); n != 1 {
		t.Errorf("the source line appears %d times, want 1:\n%s", n, body)
	}
	if n := strings.Count(body, completionMarker); n != 1 {
		t.Errorf("the marker appears %d times, want 1:\n%s", n, body)
	}
}

// A line the user added by hand carries no marker. Keying idempotency off
// the marker alone is what once appended a second copy of every hook.
func TestCompletionDoesNotDuplicateAHandAddedLine(t *testing.T) {
	home := completionHome(t)
	rc := filepath.Join(home, ".zshrc")
	byHand := "source ~/.anamnesia/completions/anamnesia.zsh\n"
	if err := os.WriteFile(rc, []byte(byHand), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := installCompletionFor(shellZsh, io.Discard); err != nil {
		t.Fatalf("install: %v", err)
	}
	body := readFile(t, rc)
	if n := countSourcingLines(body); n != 1 {
		t.Errorf("the source line appears %d times, want 1:\n%s", n, body)
	}
	if strings.Contains(body, completionMarker) {
		t.Errorf("a block was added on top of the hand-added line:\n%s", body)
	}
}

func TestCompletionUninstallLeavesTheRCAsItWas(t *testing.T) {
	home := completionHome(t)
	rc := filepath.Join(home, ".zshrc")
	original := "# my zshrc\nexport FOO=1\n\nalias ll='ls -l'\n"
	if err := os.WriteFile(rc, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := installCompletionFor(shellZsh, io.Discard); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := removeCompletion(io.Discard); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if body := readFile(t, rc); body != original {
		t.Errorf("the rc did not come back unchanged:\nwant %q\ngot  %q", original, body)
	}
	script := filepath.Join(home, ".anamnesia", "completions", "anamnesia.zsh")
	if fileExists(script) {
		t.Errorf("%s survived the uninstall", script)
	}
}

// Uninstall sweeps every shell, because the one installed for is not
// necessarily the one $SHELL says today.
func TestCompletionUninstallSweepsEveryShell(t *testing.T) {
	home := completionHome(t)
	for _, sh := range []shellKind{shellZsh, shellBash, shellFish} {
		if err := installCompletionFor(sh, io.Discard); err != nil {
			t.Fatalf("install %s: %v", sh, err)
		}
	}
	if err := removeCompletion(io.Discard); err != nil {
		t.Fatalf("remove: %v", err)
	}
	for _, name := range []string{
		filepath.Join(home, ".anamnesia", "completions", "anamnesia.zsh"),
		filepath.Join(home, ".anamnesia", "completions", "anamnesia.bash"),
		filepath.Join(home, ".config", "fish", "completions", "anamnesia.fish"),
	} {
		if fileExists(name) {
			t.Errorf("%s survived the uninstall", name)
		}
	}
	for _, rc := range []string{".zshrc", ".bashrc"} {
		body, err := os.ReadFile(filepath.Join(home, rc))
		if err == nil && strings.Contains(string(body), "anamnesia") {
			t.Errorf("%s still mentions anamnesia:\n%s", rc, body)
		}
	}
}

func TestCompletionBacksUpTheRC(t *testing.T) {
	home := completionHome(t)
	rc := filepath.Join(home, ".zshrc")
	original := "# my zshrc\n"
	if err := os.WriteFile(rc, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := installCompletionFor(shellZsh, io.Discard); err != nil {
		t.Fatalf("install: %v", err)
	}
	backups, err := filepath.Glob(rc + ".anamnesia-*.bak")
	if err != nil || len(backups) != 1 {
		t.Fatalf("want exactly one backup, got %v (%v)", backups, err)
	}
	if body := readFile(t, backups[0]); body != original {
		t.Errorf("backup holds %q, want %q", body, original)
	}
}

// Fish autoloads its completions directory, so there is no rc to edit.
func TestFishCompletionNeedsNoRCEdit(t *testing.T) {
	home := completionHome(t)
	if err := installCompletionFor(shellFish, io.Discard); err != nil {
		t.Fatalf("install: %v", err)
	}
	if !fileExists(filepath.Join(home, ".config", "fish", "completions", "anamnesia.fish")) {
		t.Error("no fish completion written")
	}
	rc, err := shellFish.rcPath()
	if err != nil {
		t.Fatal(err)
	}
	if rc != "" {
		t.Errorf("fish reported an rc file to edit: %q", rc)
	}
}

// An unfamiliar shell is not a failure. Completion is a convenience, and
// `anamnesia install` must still wire the hooks for someone on ksh.
func TestUnknownShellIsSkippedNotAnError(t *testing.T) {
	completionHome(t)
	t.Setenv("SHELL", "/bin/ksh")
	var buf bytes.Buffer
	if err := installCompletion(&buf); err != nil {
		t.Fatalf("install: %v", err)
	}
	if !strings.Contains(buf.String(), "skipped") {
		t.Errorf("nothing said the step was skipped: %q", buf.String())
	}
}

// completionValues strips the tab-separated descriptions cobra carries.
func completionValues(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, strings.SplitN(s, "\t", 2)[0])
	}
	return out
}

func TestCompleteSettingKeyFiltersByPrefix(t *testing.T) {
	got, _ := completeSettingKey(nil, nil, "embed.")
	vals := completionValues(got)
	if len(vals) == 0 {
		t.Fatal("no keys completed for prefix embed.")
	}
	for _, v := range vals {
		if !strings.HasPrefix(v, "embed.") {
			t.Errorf("%q does not start with embed.", v)
		}
	}
	if !contains(vals, "embed.dims") {
		t.Errorf("embed.dims missing from %v", vals)
	}
}

func TestCompleteSettingKeyCarriesDescriptions(t *testing.T) {
	got, _ := completeSettingKey(nil, nil, "embed.dims")
	if len(got) != 1 {
		t.Fatalf("want one match, got %v", got)
	}
	if !strings.Contains(got[0], "\t") {
		t.Errorf("%q carries no description", got[0])
	}
}

func TestCompleteSettingValueOffersEnumsAndBools(t *testing.T) {
	enum, _ := completeSettingKey(nil, []string{"rerank.provider"}, "")
	for _, want := range []string{"none", "cohere", "openrouter"} {
		if !contains(completionValues(enum), want) {
			t.Errorf("%q missing from %v", want, enum)
		}
	}
	// The empty value means "unset"; there is nothing to type for it.
	if contains(completionValues(enum), "") {
		t.Errorf("the empty value was offered: %v", enum)
	}

	bools, _ := completeSettingKey(nil, []string{"server.autostart"}, "")
	if v := completionValues(bools); len(v) != 2 || !contains(v, "true") || !contains(v, "false") {
		t.Errorf("bool values = %v, want true and false", v)
	}
}

// A free-form value cannot be enumerated, and guessing would be worse
// than offering nothing.
func TestCompleteSettingValueIsSilentWhenItCannotKnow(t *testing.T) {
	for _, key := range []string{"embed.model", "openrouter.api_key", "embed.dims", "nonsense.key"} {
		got, _ := completeSettingKey(nil, []string{key}, "")
		if len(got) != 0 {
			t.Errorf("%s completed to %v, want nothing", key, got)
		}
	}
}

// The third argument has no meaning for `config set`.
func TestCompleteSettingStopsAfterTheValue(t *testing.T) {
	got, _ := completeSettingKey(nil, []string{"embed.dims", "1536"}, "")
	if len(got) != 0 {
		t.Errorf("a third argument completed to %v, want nothing", got)
	}
}

// The whole point, exercised the way a shell exercises it.
func TestCompleteThroughTheCommandTree(t *testing.T) {
	completionHome(t)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"__complete", "config", "set", "embed."})
	t.Cleanup(func() {
		root.SetArgs(nil)
		root.SetOut(nil)
		root.SetErr(nil)
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("__complete: %v", err)
	}
	if !strings.Contains(out.String(), "embed.dims") {
		t.Errorf("embed.dims not offered:\n%s", out.String())
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
