//go:build !linux

package bedrock

// hardenProcess has nothing to do where /proc does not expose a process's
// environment to its siblings.
func hardenProcess() error { return nil }
