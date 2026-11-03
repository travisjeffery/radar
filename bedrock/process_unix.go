//go:build unix

package bedrock

import (
	"os/exec"
	"syscall"
)

// killProcessGroup runs cmd in its own process group and kills the whole
// group on cancellation, so the commands Codex started die with it.
func killProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
