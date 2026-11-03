//go:build linux

package bedrock

import "syscall"

// hardenProcess marks this process non-dumpable, which makes its
// /proc/<pid>/environ and memory unreadable to other processes of the same
// user, such as a Codex agent this process starts. Codex itself is exec'd
// and dumpable again.
func hardenProcess() error {
	_, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
