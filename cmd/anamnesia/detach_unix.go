//go:build unix

// detach_unix.go puts the spawned server in its own session so it survives
// the CLI (or the Claude Code hook) that started it. Without a new session
// the server would receive the terminal's SIGHUP and die the moment the
// shell that triggered auto-start exits.
package main

import "syscall"

func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
