//go:build !unix

// detach_other.go keeps the tree building on platforms without POSIX
// sessions. Anamnesia is developed and tested on macOS and Linux; the
// server still starts here, it just inherits the parent's process group.
package main

import "syscall"

func detachAttr() *syscall.SysProcAttr { return nil }
