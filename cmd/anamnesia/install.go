// install.go patches Claude Code's settings.json (hooks) and its MCP
// config so Claude Code calls Anamnesia. Idempotent and atomic.
//
// Two properties matter more than anything else here, because getting them
// wrong is invisible until memory has already been lost or duplicated:
//
// Hooks are written with the absolute, symlink-resolved path of this
// binary, never the bare name `anamnesia`. A hook runs in whatever shell
// Claude Code spawns, and that shell's PATH is regularly not the one from
// an interactive terminal — a GUI-launched session typically has no
// /usr/local/bin at all. A bare name there fails silently on every hook.
//
// Ownership is decided by the command, not only by a marker key. Marking
// entries with _anamnesia_managed is how uninstall knows what to remove,
// but an install that trusted the marker alone appended a second copy of
// every hook when it met entries written by an earlier version or by hand.
// Anything that runs `anamnesia hook` is ours to replace.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// managedKey marks the hook entries this command owns.
const managedKey = "_anamnesia_managed"

// managedVersionKey records the binary version that wrote an entry, so
// doctor can tell the user when their hooks predate their binary.
const managedVersionKey = "_anamnesia_version"

type hookSpec struct {
	event   string
	verb    string
	timeout int // seconds; keeps a wedged hook from stalling a session
}

// anamnesiaHooks is the hook layout.
//
//	SessionStart     read facts and experiences into Claude's context
//	UserPromptSubmit retrieve for this prompt (no ingest)
//	PreCompact       checkpoint the transcript before context is compacted
//	SessionEnd       checkpoint the transcript when the session finishes
//	SubagentStop     record what a subagent concluded
//
// SessionEnd rather than Stop is deliberate. Stop fires every time the
// agent finishes a response, so checkpointing there re-sent the entire
// growing transcript on every turn: ingest volume grew with the square of
// the session length and the same content was extracted repeatedly at full
// model cost. SessionEnd fires once.
var anamnesiaHooks = []hookSpec{
	{event: "SessionStart", verb: "session-start", timeout: 15},
	{event: "UserPromptSubmit", verb: "retrieve", timeout: 10},
	{event: "PreCompact", verb: "pre-compact", timeout: 30},
	{event: "SessionEnd", verb: "session-end", timeout: 30},
	// A subagent runs in its own transcript that no other hook reads, so
	// without this everything an agent worked out is invisible to memory.
	// Only its final message is taken: see subagentPayload.
	{event: "SubagentStop", verb: "subagent-stop", timeout: 15},
}

type installFlags struct {
	scope     string
	configDir string
	dryRun    bool
}

var (
	installF     installFlags
	uninstallF   installFlags
	uninstallAll bool
)

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Wire Claude Code's hooks and MCP config to Anamnesia",
	Long: "Patch Claude Code's settings so it calls this binary. Idempotent:\n" +
		"re-run it after upgrading to refresh the hook set and the recorded\n" +
		"binary path.",
	RunE: runInstall,
}

var uninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove Anamnesia's entries from Claude Code's config",
	RunE:  runUninstall,
}

func init() {
	installCmd.Flags().StringVar(&installF.scope, "scope", "user", "user (~/.claude) or project ($PWD/.claude)")
	installCmd.Flags().StringVar(&installF.configDir, "config-dir", "", "override the config directory (testing escape hatch)")
	installCmd.Flags().BoolVar(&installF.dryRun, "dry-run", false, "print the resulting JSON instead of writing it")

	uninstallCmd.Flags().StringVar(&uninstallF.scope, "scope", "user", "user or project")
	uninstallCmd.Flags().StringVar(&uninstallF.configDir, "config-dir", "", "override the config directory")
	uninstallCmd.Flags().BoolVar(&uninstallF.dryRun, "dry-run", false, "print the resulting JSON instead of writing it")
	uninstallCmd.Flags().BoolVar(&uninstallAll, "purge", false, "also remove the postgres container, its volume, and ~/.anamnesia")
}

type configPaths struct {
	settings string
	mcp      string
}

func resolvePaths(scope, configDir string) (configPaths, error) {
	if configDir != "" {
		return configPaths{
			settings: filepath.Join(configDir, "settings.json"),
			mcp:      filepath.Join(configDir, ".claude.json"),
		}, nil
	}
	switch scope {
	case "user", "":
		home, err := os.UserHomeDir()
		if err != nil {
			return configPaths{}, fmt.Errorf("home dir: %w", err)
		}
		return configPaths{
			settings: filepath.Join(home, ".claude", "settings.json"),
			mcp:      filepath.Join(home, ".claude.json"),
		}, nil
	case "project":
		wd, err := os.Getwd()
		if err != nil {
			return configPaths{}, fmt.Errorf("getwd: %w", err)
		}
		return configPaths{
			settings: filepath.Join(wd, ".claude", "settings.json"),
			mcp:      filepath.Join(wd, ".mcp.json"),
		}, nil
	default:
		return configPaths{}, fmt.Errorf("invalid --scope %q (want user or project)", scope)
	}
}

func runInstall(cmd *cobra.Command, _ []string) error {
	hc, err := loadHostConfig()
	if err != nil {
		return err
	}
	return applyInstall(hc, cmd.OutOrStdout())
}

// applyInstall does the patching. Shared with `anamnesia setup` and
// `anamnesia update` so all three write byte-identical results.
func applyInstall(hc *hostConfig, out io.Writer) error {
	self, err := selfPath()
	if err != nil {
		return err
	}
	paths, err := resolvePaths(installF.scope, installF.configDir)
	if err != nil {
		return err
	}

	settingsObj, err := readJSONObject(paths.settings)
	if err != nil {
		return fmt.Errorf("read %s: %w", paths.settings, err)
	}
	replaced := patchSettings(settingsObj, self)

	mcpObj, err := readJSONObject(paths.mcp)
	if err != nil {
		return fmt.Errorf("read %s: %w", paths.mcp, err)
	}
	patchMCP(mcpObj, hc.ServerURL())

	settingsBytes, err := marshalJSON(settingsObj)
	if err != nil {
		return err
	}
	mcpBytes, err := marshalJSON(mcpObj)
	if err != nil {
		return err
	}

	if installF.dryRun {
		fmt.Fprintf(out, "# %s\n%s", paths.settings, settingsBytes)
		fmt.Fprintf(out, "# %s\n%s", paths.mcp, mcpBytes)
		return nil
	}
	if err := backupOnce(paths.settings); err != nil {
		return err
	}
	if err := writeFileAtomic(paths.settings, settingsBytes); err != nil {
		return err
	}
	if err := backupOnce(paths.mcp); err != nil {
		return err
	}
	if err := writeFileAtomic(paths.mcp, mcpBytes); err != nil {
		return err
	}

	fmt.Fprintf(out, "  wired %d hooks in %s\n", len(anamnesiaHooks), paths.settings)
	if replaced > 0 {
		fmt.Fprintf(out, "    replaced %d existing Anamnesia hook entries\n", replaced)
	}
	fmt.Fprintf(out, "  wired MCP server in %s (%s/mcp)\n", paths.mcp, hc.ServerURL())
	fmt.Fprintf(out, "  hooks call %s\n", self)
	return nil
}

func runUninstall(cmd *cobra.Command, _ []string) error {
	paths, err := resolvePaths(uninstallF.scope, uninstallF.configDir)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()

	settingsObj, err := readJSONObject(paths.settings)
	if err != nil {
		return fmt.Errorf("read %s: %w", paths.settings, err)
	}
	settingsRemoved := unpatchSettings(settingsObj)

	mcpObj, err := readJSONObject(paths.mcp)
	if err != nil {
		return fmt.Errorf("read %s: %w", paths.mcp, err)
	}
	mcpRemoved := unpatchMCP(mcpObj)

	settingsBytes, err := marshalJSON(settingsObj)
	if err != nil {
		return err
	}
	mcpBytes, err := marshalJSON(mcpObj)
	if err != nil {
		return err
	}
	if uninstallF.dryRun {
		fmt.Fprintf(out, "# %s\n%s", paths.settings, settingsBytes)
		fmt.Fprintf(out, "# %s\n%s", paths.mcp, mcpBytes)
		return nil
	}
	if settingsRemoved > 0 && fileExists(paths.settings) {
		if err := writeFileAtomic(paths.settings, settingsBytes); err != nil {
			return err
		}
	}
	if mcpRemoved && fileExists(paths.mcp) {
		if err := writeFileAtomic(paths.mcp, mcpBytes); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "✦ removed %d hook entries from %s\n", settingsRemoved, paths.settings)
	if mcpRemoved {
		fmt.Fprintf(out, "✦ removed mcpServers.anamnesia from %s\n", paths.mcp)
	}

	if !uninstallAll {
		fmt.Fprintln(out, "  the stack and your stored memory are untouched (use --purge to remove them)")
		return nil
	}
	hc, err := loadHostConfig()
	if err != nil {
		return err
	}
	if err := stopServer(hc, out); err != nil {
		return err
	}
	if err := removePostgres(cmd.Context(), hc, out, true); err != nil {
		return err
	}
	home, err := anamnesiaHome()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "  removing %s\n", home)
	return os.RemoveAll(home)
}

// patchSettings installs the managed hook entries, returning how many
// pre-existing Anamnesia entries were replaced.
func patchSettings(obj map[string]any, self string) int {
	hooks, _ := obj["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		obj["hooks"] = hooks
	}
	replaced := 0

	// Anamnesia entries can be left behind on events we no longer use
	// (Stop, before SessionEnd replaced it). Sweep every event, not only
	// the ones being written, or those keep firing forever.
	for event, raw := range hooks {
		entries, ok := raw.([]any)
		if !ok {
			continue
		}
		kept, dropped := stripAnamnesiaEntries(entries)
		replaced += dropped
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}

	for _, h := range anamnesiaHooks {
		entries, _ := hooks[h.event].([]any)
		hooks[h.event] = append(entries, managedEntry(h, self))
	}
	return replaced
}

// managedEntry is one hook entry as Anamnesia writes it.
func managedEntry(h hookSpec, self string) map[string]any {
	return map[string]any{
		managedKey:        true,
		managedVersionKey: version,
		"matcher":         "",
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": hookCommand(self, h.verb),
				"timeout": h.timeout,
			},
		},
	}
}

// stripAnamnesiaEntries removes every entry Anamnesia owns, returning the
// survivors and how many were dropped.
func stripAnamnesiaEntries(entries []any) (kept []any, dropped int) {
	for _, e := range entries {
		em, ok := e.(map[string]any)
		if !ok {
			kept = append(kept, e)
			continue
		}
		if managed, _ := em[managedKey].(bool); managed {
			dropped++
			continue
		}
		if entryHasAnamnesiaCommand(em) {
			dropped++
			continue
		}
		kept = append(kept, e)
	}
	return kept, dropped
}

func unpatchSettings(obj map[string]any) int {
	hooks, _ := obj["hooks"].(map[string]any)
	if hooks == nil {
		return 0
	}
	removed := 0
	for event, raw := range hooks {
		entries, ok := raw.([]any)
		if !ok {
			continue
		}
		kept, dropped := stripAnamnesiaEntries(entries)
		removed += dropped
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	if len(hooks) == 0 {
		delete(obj, "hooks")
	}
	return removed
}

func patchMCP(obj map[string]any, serverURL string) {
	servers, _ := obj["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
		obj["mcpServers"] = servers
	}
	servers["anamnesia"] = map[string]any{
		"type": "http",
		"url":  strings.TrimRight(serverURL, "/") + "/mcp",
	}
}

func unpatchMCP(obj map[string]any) bool {
	servers, _ := obj["mcpServers"].(map[string]any)
	if servers == nil {
		return false
	}
	if _, ok := servers["anamnesia"]; !ok {
		return false
	}
	delete(servers, "anamnesia")
	if len(servers) == 0 {
		delete(obj, "mcpServers")
	}
	return true
}

// hookCommand renders the shell command for a hook, quoting the binary
// path when it contains anything a shell would split on.
func hookCommand(self, verb string) string {
	return shellQuote(self) + " hook " + verb
}

// shellQuote wraps s in single quotes when needed.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"'\\$`*?[]();&|<>#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// entryHasAnamnesiaCommand reports whether a hook entry runs `anamnesia
// hook`, whatever path or quoting was used to reach the binary.
func entryHasAnamnesiaCommand(entry map[string]any) bool {
	hooks, _ := entry["hooks"].([]any)
	for _, h := range hooks {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		cmd, _ := hm["command"].(string)
		if isAnamnesiaHookCommand(cmd) {
			return true
		}
	}
	return false
}

// isAnamnesiaHookCommand recognises any invocation of `anamnesia hook`:
// bare on the PATH, an absolute path, or a quoted path with spaces.
//
// The name is compared without its file extension, so a Windows
// `anamnesia.exe` and a test binary are recognised as ours too. Requiring
// an exact basename match meant install did not recognise its own hooks
// there and would have appended duplicates.
func isAnamnesiaHookCommand(cmd string) bool {
	tokens := shellFields(cmd)
	if len(tokens) < 2 || tokens[1] != "hook" {
		return false
	}
	base := filepath.Base(tokens[0])
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	return base == "anamnesia"
}

// shellFields splits a command line the way a POSIX shell would, honouring
// single quotes, double quotes and backslash escapes.
//
// Backslashes matter because shellQuote emits the standard `'\”` idiom for
// a path containing an apostrophe, and a tokenizer that ignored escapes
// would read that path back wrong. This is not a general shell parser; it
// covers the command forms hooks are written in.
func shellFields(s string) []string {
	var (
		out   []string
		cur   strings.Builder
		quote rune
		esc   bool
	)
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case esc:
			// A backslash inside single quotes is literal; everywhere else
			// it escapes the next character.
			cur.WriteRune(r)
			esc = false
		case quote == '\'':
			if r == '\'' {
				quote = 0
				continue
			}
			cur.WriteRune(r)
		case quote == '"':
			switch r {
			case '"':
				quote = 0
			case '\\':
				esc = true
			default:
				cur.WriteRune(r)
			}
		case r == '\\':
			esc = true
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// ─── file helpers ────────────────────────────────────────────────────

func readJSONObject(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return map[string]any{}, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	if obj == nil {
		obj = map[string]any{}
	}
	return obj, nil
}

func marshalJSON(obj map[string]any) ([]byte, error) {
	b, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// backedUp tracks paths already copied in this process, so a command that
// writes a file twice keeps the original rather than a copy of its own
// first write.
var backedUp = map[string]bool{}

// backupOnce keeps a timestamped copy of a file before it is first
// modified. These are Claude Code's own config files, so a bad write is
// the user's problem, not ours to make unrecoverable.
func backupOnce(path string) error {
	if backedUp[path] || !fileExists(path) {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s for backup: %w", path, err)
	}
	dst := fmt.Sprintf("%s.anamnesia-%s.bak", path, time.Now().UTC().Format("20060102-150405"))
	if err := os.WriteFile(dst, raw, 0o600); err != nil {
		return fmt.Errorf("write backup %s: %w", dst, err)
	}
	backedUp[path] = true
	return nil
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".anamnesia-*")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
