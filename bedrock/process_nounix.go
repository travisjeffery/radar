//go:build !unix

package bedrock

import "os/exec"

// killProcessGroup leaves cancellation to exec.CommandContext, which kills
// the Codex process itself.
func killProcessGroup(*exec.Cmd) {}
