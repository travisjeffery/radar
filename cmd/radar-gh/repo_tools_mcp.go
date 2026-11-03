package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/travisjeffery/radar"
)

// runRepoToolsMCP serves Radar's repository tools for one checkout as an MCP
// server on stdin and stdout. The codex agent starts it on Windows, where
// Codex has no sandbox that confines a command's reads to the checkout and
// keeps the network out, so Codex reads the checkout through these tools
// instead of a shell. They are the bedrock and fireworks agents'
// tools: confined to the checkout, .git off limits, nothing written, no
// network.
func runRepoToolsMCP(args []string) int {
	flags := flag.NewFlagSet("radar-gh repo-tools-mcp", flag.ContinueOnError)
	root := flags.String("root", "", "checkout the tools read (required)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *root == "" {
		fmt.Fprintln(os.Stderr, "radar-gh repo-tools-mcp: -root is required")
		return 2
	}
	server, err := newRepoToolsServer(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "radar-gh repo-tools-mcp:", err)
		return 1
	}
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "radar-gh repo-tools-mcp:", err)
		return 1
	}
	return 0
}

// newRepoToolsServer offers each repository tool for root as a read-only MCP
// tool with the same name, description and parameters.
func newRepoToolsServer(root string) (*mcp.Server, error) {
	tools, err := radar.NewRepoTools(root)
	if err != nil {
		return nil, err
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "radar", Version: "1"}, nil)
	for _, def := range tools.ToolDefinitions() {
		fn, _ := def["function"].(map[string]any)
		name, _ := fn["name"].(string)
		description, _ := fn["description"].(string)
		params, _ := fn["parameters"].(map[string]any)
		schema := map[string]any{}
		for k, v := range params {
			schema[k] = v
		}
		if required, _ := schema["required"].([]string); len(required) == 0 {
			delete(schema, "required")
		}
		server.AddTool(&mcp.Tool{
			Name: name, Description: description, InputSchema: schema,
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			out := tools.Exec(ctx, name, string(req.Params.Arguments))
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out}}}, nil
		})
	}
	return server, nil
}
