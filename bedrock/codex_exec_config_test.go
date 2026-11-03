package bedrock

import (
	"strings"
	"testing"
)

// Without ToolsCommand, Codex's config is the one CI runs: its shell in its
// read-only sandbox and no MCP server.
func TestCodexExecConfigWithoutRepoTools(t *testing.T) {
	a := &CodexExecAgent{Model: "openai.gpt-6.1-sol", Region: "us-east-1", Effort: "high"}
	config := a.config("http://127.0.0.1:1", "Review this change.")
	for _, absent := range []string{"[mcp_servers", "[features]", "shell_tool", "tool search"} {
		if strings.Contains(config, absent) {
			t.Fatalf("config without ToolsCommand must not contain %q:\n%s", absent, config)
		}
	}
	if !strings.Contains(config, `sandbox_mode = "read-only"`) {
		t.Fatalf("config = %s", config)
	}
}

// With ToolsCommand (radar-gh on Windows), Codex has no shell, the
// command is its radar MCP server, and both prompts tell it to load the tools.
func TestCodexExecConfigWithRepoTools(t *testing.T) {
	a := &CodexExecAgent{
		Model: "openai.gpt-6.1-sol", Region: "us-east-1", Effort: "high",
		ToolsCommand: []string{`C:\Program Files\Radar\radar-gh.exe`, "repo-tools-mcp", "-root", `C:\Temp\radar-commit-1\head`},
	}
	for _, instructions := range []string{"Review this change." + codexAgentInstructions, "Re-check these findings." + codexClarifyInstructions} {
		config := a.config("http://127.0.0.1:1", instructions)
		for _, want := range []string{
			`sandbox_mode = "read-only"`,
			"\n[features]\nshell_tool = false\nunified_exec = false\n",
			"\n[mcp_servers.radar]\ncommand = \"C:\\\\Program Files\\\\Radar\\\\radar-gh.exe\"\nargs = [\"repo-tools-mcp\", \"-root\", \"C:\\\\Temp\\\\radar-commit-1\\\\head\"]\n",
			"load them with tool search",
		} {
			if !strings.Contains(config, want) {
				t.Fatalf("config missing %q:\n%s", want, config)
			}
		}
	}
}
