// paths.go owns the on-disk layout Anamnesia keeps for a user.
//
// Everything lives under one directory so there is exactly one place to
// look, one place to back up, and one place to delete:
//
//	~/.anamnesia/
//	  config.toml      the only file a user needs to edit
//	  server.log       the background server's output
//	  server.pid       pid of the running server, if any
//	  start.lock       serialises concurrent auto-starts from hooks
//	  hooks.log        one JSONL line per hook run, for `anamnesia doctor`
//	  offsets/         per-session transcript read offsets
//	  completions/     the shell completion script `install` writes
//
// ANAMNESIA_HOME overrides the root, which is what lets the tests exercise
// the real code paths without touching the developer's own install.
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// homeEnv overrides the Anamnesia state directory.
const homeEnv = "ANAMNESIA_HOME"

// anamnesiaHome returns the state directory, creating nothing.
func anamnesiaHome() (string, error) {
	if v := os.Getenv(homeEnv); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".anamnesia"), nil
}

// ensureHome returns the state directory, creating it if absent.
func ensureHome() (string, error) {
	dir, err := anamnesiaHome()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	return dir, nil
}

// homeFile joins a name onto the state directory.
func homeFile(name string) (string, error) {
	dir, err := anamnesiaHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

func globalConfigPath() (string, error) { return homeFile("config.toml") }
func serverLogPath() (string, error)    { return homeFile("server.log") }
func serverPIDPath() (string, error)    { return homeFile("server.pid") }
func startLockPath() (string, error)    { return homeFile("start.lock") }
func hookLogPath() (string, error)      { return homeFile("hooks.log") }
func offsetsDir() (string, error)       { return homeFile("offsets") }

// projectConfigPath is the optional per-repository override file.
const projectConfigName = ".anamnesia.toml"
