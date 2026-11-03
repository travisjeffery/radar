package radar

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func toolsRepo(t *testing.T) (*RepoTools, string) {
	t.Helper()
	root := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "svc", "internal"), 0o755))
	must(os.WriteFile(filepath.Join(root, "svc", "up.go"), []byte("package svc\n\nfunc Upload() error {\n\treturn retry(3)\n}\n"), 0o644))
	must(os.WriteFile(filepath.Join(root, "svc", "internal", "retry.go"), []byte("package internal\n\nfunc retry(n int) error { return nil }\n"), 0o644))
	must(os.WriteFile(filepath.Join(root, "bin.dat"), []byte{0, 1, 2}, 0o644))
	outside := t.TempDir()
	must(os.WriteFile(filepath.Join(outside, "secret"), []byte("TOKEN=abc\n"), 0o644))
	must(os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "escape")))
	must(os.MkdirAll(filepath.Join(root, ".git"), 0o755))
	must(os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[core]\n"), 0o644))
	tools, err := NewRepoTools(root)
	must(err)
	return tools, root
}

func TestRepoToolsRead(t *testing.T) {
	tools, _ := toolsRepo(t)
	ctx := context.Background()
	if out := tools.Exec(ctx, "read_file", `{"path":"svc/up.go","start_line":3,"end_line":4}`); !strings.Contains(out, "     3  func Upload() error {") || strings.Contains(out, "package svc") {
		t.Fatalf("read_file = %q", out)
	}
	if out := tools.Exec(ctx, "grep", `{"pattern":"retry\\(","path":"svc"}`); !strings.Contains(out, "svc/up.go:4:") || !strings.Contains(out, "svc/internal/retry.go:3:") {
		t.Fatalf("grep = %q", out)
	}
	if out := tools.Exec(ctx, "list_dir", `{}`); !strings.Contains(out, "svc/") || strings.Contains(out, ".git") {
		t.Fatalf("list_dir = %q", out)
	}
	if out := tools.Exec(ctx, "read_file", `{"path":"bin.dat"}`); !strings.Contains(out, "binary") {
		t.Fatalf("binary = %q", out)
	}
}

func TestRepoToolsStayInTheCheckout(t *testing.T) {
	tools, root := toolsRepo(t)
	ctx := context.Background()
	for _, args := range []string{
		`{"path":"/etc/passwd"}`, `{"path":"../../etc/passwd"}`, `{"path":"svc/../../x"}`,
		`{"path":"escape"}`, `{"path":".git/config"}`, `{"path":"svc/../.git/config"}`, `{"path":"~/.ssh/id_rsa"}`,
	} {
		if out := tools.Exec(ctx, "read_file", args); !strings.HasPrefix(out, "error:") || strings.Contains(out, "TOKEN") {
			t.Fatalf("read_file %s = %q", args, out)
		}
	}
	if out := tools.Exec(ctx, "grep", `{"pattern":"TOKEN"}`); strings.Contains(out, "TOKEN=abc") {
		t.Fatalf("grep followed a symlink out of the checkout: %q", out)
	}
	if out := tools.Exec(ctx, "grep", `{"pattern":"core"}`); strings.Contains(out, ".git/config") {
		t.Fatalf("grep read .git: %q", out)
	}
	if out := tools.Exec(ctx, "write_file", `{"path":"x"}`); !strings.HasPrefix(out, "error: unknown tool") {
		t.Fatalf("unknown tool = %q", out)
	}
	if _, err := os.Stat(filepath.Join(root, "x")); err == nil {
		t.Fatal("nothing may be written")
	}
}

func TestRepoToolsGitLog(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.email=x@y", "-c", "user.name=x", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "Add a.go")
	tools, err := NewRepoTools(root)
	if err != nil {
		t.Fatal(err)
	}
	if out := tools.Exec(context.Background(), "git_log", `{"path":"a.go"}`); !strings.Contains(out, "Add a.go") {
		t.Fatalf("git_log = %q", out)
	}
}

func TestRepoToolsSkipWorktreeGitFileAndReportLongLines(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: /secret/worktrees/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "long.txt"), []byte("short\n"+strings.Repeat("x", 5<<20)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tools, err := NewRepoTools(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if out := tools.Exec(ctx, "grep", `{"pattern":"gitdir"}`); strings.Contains(out, "/secret/") {
		t.Fatalf("grep read the worktree's .git file: %q", out)
	}
	if out := tools.Exec(ctx, "read_file", `{"path":".git"}`); !strings.HasPrefix(out, "error:") {
		t.Fatalf("read_file .git = %q", out)
	}
	if out := tools.Exec(ctx, "read_file", `{"path":"long.txt"}`); !strings.HasPrefix(out, "error: reading long.txt stopped at line 2") {
		t.Fatalf("a line over the buffer must be reported, got %q", out[:min(len(out), 120)])
	}
}

func TestRepoToolsGitLogFollowsDeletedPaths(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.email=x@y", "-c", "user.name=x", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.MkdirAll(filepath.Join(root, "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "old", "gone.go"), []byte("package old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "Add gone.go")
	run("rm", "-rq", "old")
	run("commit", "-qm", "Remove gone.go")
	tools, err := NewRepoTools(root)
	if err != nil {
		t.Fatal(err)
	}
	if out := tools.Exec(context.Background(), "git_log", `{"path":"old/gone.go"}`); !strings.Contains(out, "Remove gone.go") || !strings.Contains(out, "Add gone.go") {
		t.Fatalf("git_log of a deleted path = %q", out)
	}
	for _, bad := range []string{`{"path":"../x"}`, `{"path":".git/HEAD"}`, `{"path":"/etc"}`} {
		if out := tools.Exec(context.Background(), "git_log", bad); !strings.HasPrefix(out, "error:") {
			t.Fatalf("git_log %s = %q", bad, out)
		}
	}
}
