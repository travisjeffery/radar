//go:build linux

package main

import (
	"os/exec"
	"syscall"
)

// hardenProcess marks this process non-dumpable, so the GitHub tokens and
// credentials URLs it passes through are unreadable from /proc/<pid> to the
// Codex agent, which runs as the same user in this container.
func hardenProcess() error {
	_, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// killProcessGroup runs cmd in its own process group and kills the group on
// cancellation, so Codex and the commands it started die with radar-gh.
func killProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
