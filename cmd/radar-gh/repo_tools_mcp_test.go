package main

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The codex agent's radar MCP server offers Radar's repository tools,
// read-only, and keeps every read inside the checkout.
func TestRepoToolsServer(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(root, "checkout")
	if err := os.MkdirAll(filepath.Join(checkout, "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "api", "handler.go"), []byte("package api\n\nfunc Handle() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("outside the checkout"), 0o644); err != nil {
		t.Fatal(err)
	}
	server, err := newRepoToolsServer(checkout)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Fatalf("%s must be marked read-only", tool.Name)
		}
	}
	sort.Strings(names)
	if got := strings.Join(names, ","); got != "git_log,grep,list_dir,read_file" {
		t.Fatalf("tools = %s", got)
	}

	call := func(name string, args map[string]any) string {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		var text strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				text.WriteString(tc.Text)
			}
		}
		return text.String()
	}
	if out := call("read_file", map[string]any{"path": "api/handler.go"}); !strings.Contains(out, "func Handle() {}") {
		t.Fatalf("read_file = %q", out)
	}
	if out := call("grep", map[string]any{"pattern": "func Handle"}); !strings.Contains(out, "api/handler.go") {
		t.Fatalf("grep = %q", out)
	}
	if out := call("list_dir", map[string]any{}); !strings.Contains(out, "api") {
		t.Fatalf("list_dir = %q", out)
	}
	for _, path := range []string{"../secret.txt", filepath.Join(root, "secret.txt")} {
		if out := call("read_file", map[string]any{"path": path}); !strings.HasPrefix(out, "error:") || strings.Contains(out, "outside the checkout") {
			t.Fatalf("read_file %s must be refused, got %q", path, out)
		}
	}
}

func TestRunRepoToolsMCPNeedsACheckout(t *testing.T) {
	if code := runRepoToolsMCP(nil); code != 2 {
		t.Fatalf("no -root: exit %d, want 2", code)
	}
	if code := runRepoToolsMCP([]string{"-root", filepath.Join(t.TempDir(), "missing")}); code != 1 {
		t.Fatalf("missing checkout: exit %d, want 1", code)
	}
}
