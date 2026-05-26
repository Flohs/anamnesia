// install.go patches Claude Code's settings.json (hooks) and .claude.json
// (mcpServers) so Claude Code calls Anamnesia. Idempotent and atomic.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// managedKey marks the hook entries this command owns so re-runs can
// upgrade them and `anamnesia uninstall` can remove them.
const managedKey = "_anamnesia_managed"

type hookSpec struct {
	event   string
	matcher string
	verb    string
}

var anamnesiaHooks = []hookSpec{
	{event: "SessionStart", matcher: "", verb: "session-start"},
	{event: "UserPromptSubmit", matcher: "", verb: "retrieve"},
	{event: "PostToolUse", matcher: "Edit|Write|Bash|mcp__.*", verb: "capture"},
	{event: "Stop", matcher: "", verb: "session-end"},
}

type installFlags struct {
	scope     string
	configDir string
	dryRun    bool
	force     bool
	server    string
}

var (
	installF   installFlags
	uninstallF installFlags
)

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Wire Claude Code's hooks + MCP config to Anamnesia",
	RunE:  runInstall,
}

var uninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove Anamnesia entries from Claude Code's config files",
	RunE:  runUninstall,
}

func init() {
	installCmd.Flags().StringVar(&installF.scope, "scope", "user", "user (patches ~/.claude/…) or project (patches $PWD/.claude/…)")
	installCmd.Flags().StringVar(&installF.configDir, "config-dir", "", "override config directory (testing escape hatch)")
	installCmd.Flags().BoolVar(&installF.dryRun, "dry-run", false, "print resulting JSON to stdout, don't write")
	installCmd.Flags().BoolVar(&installF.force, "force", false, "also drop unmanaged `anamnesia hook` entries")
	installCmd.Flags().StringVar(&installF.server, "server", "", "Anamnesia server URL used for the MCP entry")

	uninstallCmd.Flags().StringVar(&uninstallF.scope, "scope", "user", "user or project")
	uninstallCmd.Flags().StringVar(&uninstallF.configDir, "config-dir", "", "override config directory")
	uninstallCmd.Flags().BoolVar(&uninstallF.dryRun, "dry-run", false, "print resulting JSON to stdout, don't write")
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
	serverURL := installF.server
	if serverURL == "" {
		hc, _ := resolveHostConfig()
		if hc != nil && hc.ServerURL != "" {
			serverURL = hc.ServerURL
		}
	}
	if serverURL == "" {
		serverURL = defaultServerURL
	}

	paths, err := resolvePaths(installF.scope, installF.configDir)
	if err != nil {
		return err
	}

	settingsObj, err := readJSONObject(paths.settings)
	if err != nil {
		return fmt.Errorf("read %s: %w", paths.settings, err)
	}
	patchSettings(settingsObj, installF.force)

	mcpObj, err := readJSONObject(paths.mcp)
	if err != nil {
		return fmt.Errorf("read %s: %w", paths.mcp, err)
	}
	patchMCP(mcpObj, serverURL)

	settingsBytes, err := marshalJSON(settingsObj)
	if err != nil {
		return err
	}
	mcpBytes, err := marshalJSON(mcpObj)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if installF.dryRun {
		fmt.Fprintf(out, "# %s\n%s", paths.settings, settingsBytes)
		fmt.Fprintf(out, "# %s\n%s", paths.mcp, mcpBytes)
		return nil
	}
	if err := writeFileAtomic(paths.settings, settingsBytes); err != nil {
		return err
	}
	if err := writeFileAtomic(paths.mcp, mcpBytes); err != nil {
		return err
	}
	fmt.Fprintf(out, "✦ wrote %s (%d hooks)\n", paths.settings, len(anamnesiaHooks))
	fmt.Fprintf(out, "✦ wrote %s (mcp url: %s/mcp)\n", paths.mcp, strings.TrimRight(serverURL, "/"))
	fmt.Fprintln(out, "  run `anamnesia doctor` to confirm")
	return nil
}

func runUninstall(cmd *cobra.Command, _ []string) error {
	paths, err := resolvePaths(uninstallF.scope, uninstallF.configDir)
	if err != nil {
		return err
	}
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
	out := cmd.OutOrStdout()
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
	return nil
}

func patchSettings(obj map[string]any, force bool) {
	hooks, _ := obj["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		obj["hooks"] = hooks
	}
	for _, h := range anamnesiaHooks {
		entries, _ := hooks[h.event].([]any)
		var preserved []any
		for _, e := range entries {
			em, ok := e.(map[string]any)
			if !ok {
				preserved = append(preserved, e)
				continue
			}
			if managed, _ := em[managedKey].(bool); managed {
				continue
			}
			if force && entryHasAnamnesiaCommand(em) {
				continue
			}
			preserved = append(preserved, e)
		}
		managedEntry := map[string]any{
			managedKey: true,
			"matcher":  h.matcher,
			"hooks": []any{
				map[string]any{
					"type":    "command",
					"command": "anamnesia hook " + h.verb,
				},
			},
		}
		preserved = append(preserved, managedEntry)
		hooks[h.event] = preserved
	}
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
		var preserved []any
		for _, e := range entries {
			em, ok := e.(map[string]any)
			if !ok {
				preserved = append(preserved, e)
				continue
			}
			if managed, _ := em[managedKey].(bool); managed {
				removed++
				continue
			}
			preserved = append(preserved, e)
		}
		if len(preserved) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = preserved
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

func entryHasAnamnesiaCommand(entry map[string]any) bool {
	hooks, _ := entry["hooks"].([]any)
	for _, h := range hooks {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		cmd, _ := hm["command"].(string)
		if strings.HasPrefix(strings.TrimSpace(cmd), "anamnesia hook") {
			return true
		}
	}
	return false
}

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

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".anamnesia-install-*")
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
