package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readSettings loads a settings.json written by the install path.
func readSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	obj, err := readJSONObject(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return obj
}

// hookEntries returns the entries recorded for one event.
func hookEntries(t *testing.T, obj map[string]any, event string) []any {
	t.Helper()
	hooks, ok := obj["hooks"].(map[string]any)
	if !ok {
		return nil
	}
	entries, _ := hooks[event].([]any)
	return entries
}

// countAnamnesiaCommands counts every hook command that invokes us,
// anywhere in the file.
func countAnamnesiaCommands(t *testing.T, obj map[string]any) int {
	t.Helper()
	hooks, _ := obj["hooks"].(map[string]any)
	n := 0
	for _, raw := range hooks {
		entries, _ := raw.([]any)
		for _, e := range entries {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if entryHasAnamnesiaCommand(em) {
				n++
			}
		}
	}
	return n
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// installInto runs the install path against a scratch config directory.
func installInto(t *testing.T, dir string) {
	t.Helper()
	installF = installFlags{scope: "user", configDir: dir}
	backedUp = map[string]bool{}
	hc := testHostConfig(t)
	if err := applyInstall(hc, &strings.Builder{}); err != nil {
		t.Fatalf("applyInstall: %v", err)
	}
}

// testHostConfig builds a config pointing at a scratch home.
func testHostConfig(t *testing.T) *hostConfig {
	t.Helper()
	t.Setenv(homeEnv, t.TempDir())
	hc, err := loadHostConfig()
	if err != nil {
		t.Fatalf("loadHostConfig: %v", err)
	}
	return hc
}

// TestInstallIsIdempotent guards the property the whole hook layout rests
// on: running install twice must leave exactly one entry per event.
func TestInstallIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	installInto(t, dir)
	first := readSettings(t, filepath.Join(dir, "settings.json"))
	installInto(t, dir)
	second := readSettings(t, filepath.Join(dir, "settings.json"))

	if got, want := countAnamnesiaCommands(t, first), len(anamnesiaHooks); got != want {
		t.Fatalf("after first install: %d anamnesia hooks, want %d", got, want)
	}
	if got, want := countAnamnesiaCommands(t, second), len(anamnesiaHooks); got != want {
		t.Fatalf("after second install: %d anamnesia hooks, want %d", got, want)
	}
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Errorf("install is not idempotent:\nfirst:  %s\nsecond: %s", a, b)
	}
}

// TestInstallReplacesUnmarkedHooks is the regression test for the bug that
// doubled a user's hooks. Entries written by an older version carry no
// _anamnesia_managed marker, and an install that keyed off the marker alone
// appended a second copy of every hook instead of replacing them.
func TestInstallReplacesUnmarkedHooks(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")

	// Exactly the shape an earlier version left behind: right commands, no
	// marker, bare binary name.
	writeJSONFile(t, settings, map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{
				"matcher": "",
				"hooks":   []any{map[string]any{"type": "command", "command": "anamnesia hook session-start"}},
			}},
			"UserPromptSubmit": []any{map[string]any{
				"matcher": "",
				"hooks":   []any{map[string]any{"type": "command", "command": "anamnesia hook retrieve"}},
			}},
		},
	})

	installInto(t, dir)
	obj := readSettings(t, settings)

	if got, want := countAnamnesiaCommands(t, obj), len(anamnesiaHooks); got != want {
		t.Fatalf("got %d anamnesia hook entries, want %d (unmarked entries were not replaced)", got, want)
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit"} {
		if n := len(hookEntries(t, obj, event)); n != 1 {
			t.Errorf("%s has %d entries, want 1", event, n)
		}
	}
}

// TestInstallRemovesRetiredEvents checks that hooks on events this version
// no longer uses are swept, not left firing forever. Stop was replaced by
// SessionEnd precisely because it fired on every turn.
func TestInstallRemovesRetiredEvents(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	writeJSONFile(t, settings, map[string]any{
		"hooks": map[string]any{
			"Stop": []any{map[string]any{
				managedKey: true,
				"matcher":  "",
				"hooks":    []any{map[string]any{"type": "command", "command": "anamnesia hook session-end"}},
			}},
		},
	})

	installInto(t, dir)
	obj := readSettings(t, settings)

	if entries := hookEntries(t, obj, "Stop"); len(entries) != 0 {
		t.Errorf("Stop still has %d entries; retired events must be removed", len(entries))
	}
	if entries := hookEntries(t, obj, "SessionEnd"); len(entries) != 1 {
		t.Errorf("SessionEnd has %d entries, want 1", len(entries))
	}
}

// TestInstallPreservesForeignConfig makes sure we only ever touch our own
// entries: these are the user's files, shared with other tools.
func TestInstallPreservesForeignConfig(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	mcp := filepath.Join(dir, ".claude.json")

	writeJSONFile(t, settings, map[string]any{
		"model": "opus",
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{
				"matcher": "",
				"hooks":   []any{map[string]any{"type": "command", "command": "my-own-tool notify"}},
			}},
			"PreToolUse": []any{map[string]any{
				"matcher": "Bash",
				"hooks":   []any{map[string]any{"type": "command", "command": "/usr/local/bin/gate.sh"}},
			}},
		},
	})
	writeJSONFile(t, mcp, map[string]any{
		"numStartups": 42,
		"mcpServers":  map[string]any{"other": map[string]any{"type": "http", "url": "http://x/mcp"}},
	})

	installInto(t, dir)
	obj := readSettings(t, settings)

	if obj["model"] != "opus" {
		t.Errorf("unrelated setting lost: model = %v", obj["model"])
	}
	if n := len(hookEntries(t, obj, "PreToolUse")); n != 1 {
		t.Errorf("foreign PreToolUse hook lost (%d entries)", n)
	}
	var foundForeign bool
	for _, e := range hookEntries(t, obj, "SessionStart") {
		em := e.(map[string]any)
		if !entryHasAnamnesiaCommand(em) {
			foundForeign = true
		}
	}
	if !foundForeign {
		t.Error("foreign SessionStart hook was removed")
	}

	mcpObj := readSettings(t, mcp)
	if mcpObj["numStartups"] != float64(42) {
		t.Errorf("unrelated key lost: numStartups = %v", mcpObj["numStartups"])
	}
	servers := mcpObj["mcpServers"].(map[string]any)
	if _, ok := servers["other"]; !ok {
		t.Error("foreign MCP server was removed")
	}
	if _, ok := servers["anamnesia"]; !ok {
		t.Error("anamnesia MCP server was not added")
	}
}

// TestUninstallRoundTrip checks that uninstall restores what install found.
func TestUninstallRoundTrip(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	mcp := filepath.Join(dir, ".claude.json")

	original := map[string]any{
		"model": "opus",
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{
				"matcher": "",
				"hooks":   []any{map[string]any{"type": "command", "command": "my-own-tool notify"}},
			}},
		},
	}
	writeJSONFile(t, settings, original)
	writeJSONFile(t, mcp, map[string]any{"mcpServers": map[string]any{"other": map[string]any{"type": "http", "url": "http://x/mcp"}}})

	before, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	installInto(t, dir)

	uninstallF = installFlags{scope: "user", configDir: dir}
	obj := readSettings(t, settings)
	removed := unpatchSettings(obj)
	if removed != len(anamnesiaHooks) {
		t.Errorf("unpatchSettings removed %d entries, want %d", removed, len(anamnesiaHooks))
	}
	out, err := marshalJSON(obj)
	if err != nil {
		t.Fatal(err)
	}

	var a, b any
	if err := json.Unmarshal(before, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &b); err != nil {
		t.Fatal(err)
	}
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	if string(x) != string(y) {
		t.Errorf("uninstall did not restore the original:\nbefore: %s\nafter:  %s", x, y)
	}
}

// TestInstallWritesAbsoluteBinaryPath covers the failure mode where a hook
// runs in a shell whose PATH does not contain the binary, which is the
// normal case for a GUI-launched session.
func TestInstallWritesAbsoluteBinaryPath(t *testing.T) {
	dir := t.TempDir()
	installInto(t, dir)
	obj := readSettings(t, filepath.Join(dir, "settings.json"))

	self, err := selfPath()
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range anamnesiaHooks {
		entries := hookEntries(t, obj, h.event)
		if len(entries) != 1 {
			t.Fatalf("%s: %d entries, want 1", h.event, len(entries))
		}
		em := entries[0].(map[string]any)
		inner := em["hooks"].([]any)[0].(map[string]any)
		cmd := inner["command"].(string)
		tokens := shellFields(cmd)
		if len(tokens) < 3 {
			t.Fatalf("%s: unexpected command %q", h.event, cmd)
		}
		if !filepath.IsAbs(tokens[0]) {
			t.Errorf("%s: command %q does not use an absolute path", h.event, cmd)
		}
		if tokens[0] != self {
			t.Errorf("%s: command uses %q, want %q", h.event, tokens[0], self)
		}
		if tokens[2] != h.verb {
			t.Errorf("%s: verb is %q, want %q", h.event, tokens[2], h.verb)
		}
		if em[managedVersionKey] != version {
			t.Errorf("%s: version stamp is %v, want %q", h.event, em[managedVersionKey], version)
		}
	}
}

// TestInstallBacksUpExistingFiles: these are the user's own Claude Code
// files, so a bad write must not be unrecoverable.
func TestInstallBacksUpExistingFiles(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	writeJSONFile(t, settings, map[string]any{"model": "opus"})

	installInto(t, dir)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "settings.json.anamnesia-") && strings.HasSuffix(e.Name(), ".bak") {
			found = true
		}
	}
	if !found {
		t.Errorf("no backup of settings.json was written; files present: %v", entries)
	}
}

func TestIsAnamnesiaHookCommand(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"anamnesia hook retrieve", true},
		{"/usr/local/bin/anamnesia hook session-start", true},
		{"'/Users/me/My Tools/anamnesia' hook session-end", true},
		{`"/opt/an amnesia/anamnesia" hook pre-compact`, true},
		{"/usr/local/bin/anamnesia serve", false},
		{"anamnesia", false},
		{"my-own-tool notify", false},
		{"", false},
		{"anamnesiaX hook retrieve", false},
		{"notanamnesia hook retrieve", false},
	}
	for _, c := range cases {
		if got := isAnamnesiaHookCommand(c.cmd); got != c.want {
			t.Errorf("isAnamnesiaHookCommand(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

func TestShellQuoteRoundTrip(t *testing.T) {
	for _, path := range []string{
		"/usr/local/bin/anamnesia",
		"/Users/me/My Tools/anamnesia",
		"/tmp/it's here/anamnesia",
	} {
		cmd := hookCommand(path, "retrieve")
		tokens := shellFields(cmd)
		if len(tokens) != 3 {
			t.Fatalf("hookCommand(%q) produced %q, split into %v", path, cmd, tokens)
		}
		if tokens[0] != path {
			t.Errorf("round trip lost the path: got %q, want %q (command %q)", tokens[0], path, cmd)
		}
	}
}
