// config_host.go resolves the host-side runtime config used by the
// hook / install / doctor subcommands. Server-side serve runs from
// internal/config; this is the user-facing equivalent.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const defaultServerURL = "http://localhost:8181"

// hostConfig is the resolved view of (CLI flag > env > config file > defaults).
type hostConfig struct {
	ServerURL   string
	ServerToken string // optional shared secret if the server requires it
	User        string
	Project     string
}

// resolveHostConfig reads project + global TOML files, env vars, and the
// active flag set.
func resolveHostConfig() (*hostConfig, error) {
	cfg := &hostConfig{
		ServerURL:   defaultServerURL,
		ServerToken: os.Getenv("ANAMNESIA_SERVER_TOKEN"),
		User:        os.Getenv("ANAMNESIA_USER"),
		Project:     os.Getenv("ANAMNESIA_PROJECT"),
	}
	if v := os.Getenv("ANAMNESIA_URL"); v != "" {
		cfg.ServerURL = v
	}

	// Project-level config (./.anamnesia.toml), then global (~/.anamnesia/config.toml).
	for _, p := range projectConfigPaths() {
		if err := mergeTOMLInto(cfg, p); err != nil {
			return nil, err
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		_ = mergeTOMLInto(cfg, filepath.Join(home, ".anamnesia", "config.toml"))
	}
	if rf.configPath != "" {
		if err := mergeTOMLInto(cfg, rf.configPath); err != nil {
			return nil, err
		}
	}

	// Flag overrides win.
	if rf.serverURL != "" {
		cfg.ServerURL = rf.serverURL
	}
	if rf.user != "" {
		cfg.User = rf.user
	}
	if rf.project != "" {
		cfg.Project = rf.project
	}

	// Derived defaults.
	if cfg.User == "" {
		cfg.User = os.Getenv("USER")
	}
	if cfg.User == "" {
		cfg.User = "default"
	}
	if cfg.Project == "" {
		cfg.Project = deriveProjectFromCWD()
	}
	cfg.ServerURL = strings.TrimRight(cfg.ServerURL, "/")
	return cfg, nil
}

func projectConfigPaths() []string {
	wd, err := os.Getwd()
	if err != nil {
		return nil
	}
	root := gitToplevelOrCWD(wd)
	out := []string{filepath.Join(root, ".anamnesia.toml")}
	if root != wd {
		out = append(out, filepath.Join(wd, ".anamnesia.toml"))
	}
	return out
}

func gitToplevelOrCWD(cwd string) string {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return cwd
	}
	return strings.TrimSpace(string(out))
}

func deriveProjectFromCWD() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	top := gitToplevelOrCWD(wd)
	if top == "" {
		top = wd
	}
	return sanitizeSlug(filepath.Base(top))
}

var slugRE = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// sanitizeSlug normalises a string into a stable project slug.
func sanitizeSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugRE.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	return s
}

// mergeTOMLInto reads a tiny key=value TOML file (no sections needed)
// and overlays values onto cfg. Silently no-op if the file is absent.
func mergeTOMLInto(cfg *hostConfig, path string) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.Trim(strings.TrimSpace(line[eq+1:]), `"'`)
		switch key {
		case "server":
			cfg.ServerURL = val
		case "token":
			cfg.ServerToken = val
		case "user":
			cfg.User = val
		case "project":
			cfg.Project = val
		}
	}
	return sc.Err()
}
