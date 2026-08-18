package main

import (
	"strings"
	"testing"
)

// TestSudoRefusal covers the guard added after `sudo anamnesia update` left a
// user's config, Claude Code's settings.json and .claude.json, and the memory
// server's pid file all owned by root.
func TestSudoRefusal(t *testing.T) {
	tests := []struct {
		name      string
		euid      int
		invoker   string
		command   string
		allowRoot bool
		refuse    bool
	}{
		{name: "normal user", euid: 501, invoker: "", command: "update", refuse: false},
		{name: "sudo from a user", euid: 0, invoker: "floh", command: "update", refuse: true},
		{name: "sudo from a user, setup", euid: 0, invoker: "floh", command: "setup", refuse: true},
		{name: "sudo from a user, start", euid: 0, invoker: "floh", command: "start", refuse: true},

		// A genuine root account is not the same as escalating from a user.
		{name: "genuinely root", euid: 0, invoker: "", command: "update", refuse: false},
		{name: "root running sudo", euid: 0, invoker: "root", command: "update", refuse: false},

		// Read-only commands write nothing into the user's home.
		{name: "sudo doctor", euid: 0, invoker: "floh", command: "doctor", refuse: false},
		{name: "sudo version", euid: 0, invoker: "floh", command: "version", refuse: false},
		{name: "sudo status", euid: 0, invoker: "floh", command: "status", refuse: false},

		// An explicit override stays available.
		{name: "override", euid: 0, invoker: "floh", command: "update", allowRoot: true, refuse: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := sudoRefusal(tc.euid, tc.invoker, tc.command, tc.allowRoot)
			if tc.refuse && err == nil {
				t.Fatalf("expected %s to be refused", tc.command)
			}
			if !tc.refuse && err != nil {
				t.Fatalf("unexpected refusal: %v", err)
			}
			if err == nil {
				return
			}
			// The message has to name who ran it and what to run instead.
			for _, want := range []string{tc.invoker, "anamnesia " + tc.command, "--allow-root"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("message does not mention %q: %v", want, err)
				}
			}
		})
	}
}
