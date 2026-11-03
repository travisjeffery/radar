package radar

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// RepoTools gives a review model read-only access to a checkout of the pull
// request head through function calls, so a chat-completion model such as
// GLM-5.3 can read the code around a change as the Codex CLI does.
// Every path is confined to Root after resolving symlinks, .git is off
// limits, nothing is written and nothing reaches the network.
type RepoTools struct {
	root string
}

const (
	maxToolOutputChars = 24000
	maxReadLines       = 400
	maxGrepMatches     = 80
	maxGrepFileBytes   = 1 << 20
	maxListEntries     = 300
	maxGitLogEntries   = 30
)

// NewRepoTools confines the tools to dir, which must be a directory.
func NewRepoTools(dir string) (*RepoTools, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("radar: repository tools need a directory, got %s", dir)
	}
	return &RepoTools{root: real}, nil
}

// ToolDefinitions are the tools in the OpenAI-compatible function format.
func (t *RepoTools) ToolDefinitions() []map[string]any {
	fn := func(name, desc string, props map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "function", "function": map[string]any{
			"name": name, "description": desc,
			"parameters": map[string]any{"type": "object", "properties": props, "required": required},
		}}
	}
	str := func(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
	num := func(d string) map[string]any { return map[string]any{"type": "integer", "description": d} }
	return []map[string]any{
		fn("read_file", "Read a file in the pull request's repository, with line numbers. Paths are relative to the repository root.",
			map[string]any{"path": str("file path"), "start_line": num("first line, 1-based; default 1"), "end_line": num("last line; default start_line+399")}, "path"),
		fn("grep", "Search the repository for a regular expression (RE2). Returns matching lines with file and line number.",
			map[string]any{"pattern": str("regular expression"), "path": str("directory or file to search; default the whole repository")}, "pattern"),
		fn("list_dir", "List a directory in the repository.", map[string]any{"path": str("directory; default the repository root")}),
		fn("git_log", "Recent commit subjects that touched a path.", map[string]any{"path": str("file or directory"), "n": num("how many, at most 30")}, "path"),
	}
}

// Exec runs one tool call and returns its output for the model. An error
// is returned to the model as text, never fatal to the review.
func (t *RepoTools) Exec(ctx context.Context, name, arguments string) string {
	var args struct {
		Path      string `json:"path"`
		Pattern   string `json:"pattern"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
		N         int    `json:"n"`
	}
	if strings.TrimSpace(arguments) != "" {
		if err := json.Unmarshal([]byte(arguments), &args); err != nil {
			return "error: arguments are not a JSON object: " + err.Error()
		}
	}
	var out string
	var err error
	switch name {
	case "read_file":
		out, err = t.readFile(args.Path, args.StartLine, args.EndLine)
	case "grep":
		out, err = t.grep(ctx, args.Pattern, args.Path)
	case "list_dir":
		out, err = t.listDir(args.Path)
	case "git_log":
		out, err = t.gitLog(ctx, args.Path, args.N)
	default:
		err = fmt.Errorf("unknown tool %q", name)
	}
	if err != nil {
		return "error: " + err.Error()
	}
	return clipText(out, maxToolOutputChars)
}

// resolve maps a model-supplied path to a real path under the root, or
// refuses it: absolute paths, escapes through .. or symlinks, and .git.
func (t *RepoTools) resolve(p string) (string, string, error) {
	p = strings.TrimSpace(p)
	if p == "" || p == "." || p == "/" {
		return t.root, ".", nil
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "~") {
		return "", "", errors.New("paths are relative to the repository root")
	}
	clean := path.Clean(filepath.ToSlash(p))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", "", errors.New("path is outside the repository")
	}
	if clean == ".git" || strings.HasPrefix(clean, ".git/") || strings.Contains(clean, "/.git/") || strings.HasSuffix(clean, "/.git") {
		return "", "", errors.New(".git is not readable")
	}
	full := filepath.Join(t.root, filepath.FromSlash(clean))
	real, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", "", fmt.Errorf("%s does not exist", clean)
	}
	if real != t.root && !strings.HasPrefix(real, t.root+string(filepath.Separator)) {
		return "", "", errors.New("path resolves outside the repository")
	}
	return real, clean, nil
}

// resolveHistorical validates a path that may no longer exist: the same
// rules as resolve, applied to its nearest existing ancestor.
func (t *RepoTools) resolveHistorical(p string) (string, error) {
	if _, clean, err := t.resolve(p); err == nil {
		return clean, nil
	} else if !strings.HasSuffix(err.Error(), "does not exist") {
		return "", err
	}
	clean := path.Clean(filepath.ToSlash(strings.TrimSpace(p)))
	for dir := path.Dir(clean); ; dir = path.Dir(dir) {
		if _, _, err := t.resolve(dir); err == nil {
			return clean, nil
		} else if !strings.HasSuffix(err.Error(), "does not exist") {
			return "", err
		}
		if dir == "." || dir == "/" {
			return "", errors.New("path is outside the repository")
		}
	}
}

func (t *RepoTools) readFile(p string, start, end int) (string, error) {
	real, clean, err := t.resolve(p)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory; use list_dir", clean)
	}
	f, err := os.Open(real)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if start < 1 {
		start = 1
	}
	if end < start || end-start >= maxReadLines {
		end = start + maxReadLines - 1
	}
	var b strings.Builder
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	n := 0
	for scanner.Scan() {
		n++
		if n < start {
			continue
		}
		if n > end {
			fmt.Fprintf(&b, "... (more lines; ask for start_line %d)\n", end+1)
			break
		}
		line := scanner.Text()
		if bytes.IndexByte([]byte(line), 0) >= 0 {
			return "", fmt.Errorf("%s is a binary file", clean)
		}
		fmt.Fprintf(&b, "%6d  %s\n", n, line)
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("reading %s stopped at line %d: %w", clean, n+1, err)
	}
	if b.Len() == 0 {
		return fmt.Sprintf("%s has %d lines; nothing at line %d", clean, n, start), nil
	}
	return clean + ":\n" + b.String(), nil
}

func (t *RepoTools) grep(ctx context.Context, pattern, p string) (string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("bad pattern: %w", err)
	}
	real, _, err := t.resolve(p)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	matches := 0
	walkErr := filepath.WalkDir(real, func(file string, d fs.DirEntry, err error) error {
		if err != nil || ctx.Err() != nil {
			return ctx.Err()
		}
		// In a worktree .git is a file, not a directory: skip either.
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxGrepFileBytes {
			return nil
		}
		data, err := os.ReadFile(file)
		if err != nil || bytes.IndexByte(data, 0) >= 0 {
			return nil
		}
		rel, _ := filepath.Rel(t.root, file)
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				fmt.Fprintf(&b, "%s:%d: %s\n", filepath.ToSlash(rel), i+1, clipText(line, 300))
				matches++
				if matches >= maxGrepMatches {
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}
	if matches == 0 {
		return "no matches", nil
	}
	if matches >= maxGrepMatches {
		fmt.Fprintf(&b, "... (stopped at %d matches; narrow the pattern or path)\n", maxGrepMatches)
	}
	return b.String(), nil
}

func (t *RepoTools) listDir(p string) (string, error) {
	real, clean, err := t.resolve(p)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(real)
	if err != nil {
		return "", err
	}
	var names []string
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > maxListEntries {
		names = append(names[:maxListEntries], fmt.Sprintf("... (%d more)", len(names)-maxListEntries))
	}
	return clean + "/\n" + strings.Join(names, "\n"), nil
}

func (t *RepoTools) gitLog(ctx context.Context, p string, n int) (string, error) {
	// A deleted or renamed-away path is gone from the checkout but still in
	// history, so confinement is checked on its nearest existing parent.
	clean, err := t.resolveHistorical(p)
	if err != nil {
		return "", err
	}
	if n <= 0 || n > maxGitLogEntries {
		n = maxGitLogEntries
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// Commit subjects only: no patches, so no diff drivers or filters run.
	cmd := exec.CommandContext(ctx, "git", "-C", t.root, "-c", "core.fsmonitor=false", "log",
		fmt.Sprintf("-n%d", n), "--no-ext-diff", "--format=%h %ad %s", "--date=short", "--", clean)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0"}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git log: %w", err)
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return "no commits (a shallow checkout may hold only the head)", nil
	}
	return string(out), nil
}
