//go:build !linux

package main

import "os/exec"

// hardenProcess is a no-op off Linux; the runner is deployed only on Linux.
func hardenProcess() error { return nil }

func killProcessGroup(*exec.Cmd) {}
