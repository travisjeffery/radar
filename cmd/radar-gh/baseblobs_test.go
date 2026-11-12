package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestToPullRequestFilesPatchCompleteness(t *testing.T) {
	str := func(s string) *string { return &s }
	const blob, other = "b10b000000000000000000000000000000000001", "b10b000000000000000000000000000000000002"
	atBase := func(path string) (string, error) {
		if path == "gone.tsx" {
			return "", errors.New("GitHub is down")
		}
		return blob, nil
	}
	consulted := func(string) (string, error) {
		t.Fatal("base blob read for a file that is not a rename without a patch")
		return "", nil
	}
	rename := githubFile{Filename: "b.tsx", PreviousFilename: "a.tsx", Status: "renamed", SHA: blob}
	with := func(f githubFile, edit func(*githubFile)) githubFile { edit(&f); return f }

	tests := []struct {
		name string
		file githubFile
		base blobAt
		want bool
	}{
		{"rename whose blob is the old path's", rename, atBase, true},
		// A binary renamed and edited comes back renamed, 0/0, with no patch.
		{"0/0 rename with a different blob", with(rename, func(f *githubFile) { f.SHA = other }), atBase, false},
		{"rename with no blob SHA", with(rename, func(f *githubFile) { f.SHA = "" }), atBase, false},
		{"rename with no previous path", with(rename, func(f *githubFile) { f.PreviousFilename = "" }), atBase, false},
		{"rename whose old blob cannot be read", with(rename, func(f *githubFile) { f.PreviousFilename = "gone.tsx" }), atBase, false},
		{"rename with no way to read the old blob", rename, nil, false},
		{"rename with edits and its patch", with(rename, func(f *githubFile) {
			f.SHA, f.Additions, f.Deletions, f.Patch = other, 1, 1, str("@@ -1 +1 @@\n-a\n+b")
		}), consulted, true},
		{"rename with edits whose patch is cut short", with(rename, func(f *githubFile) {
			f.Additions, f.Deletions, f.Patch = 2, 1, str("@@ -1 +1 @@\n-a\n+b")
		}), consulted, false},
		{"rename with edits and no patch", with(rename, func(f *githubFile) { f.SHA, f.Additions = other, 400 }), atBase, false},
		{"modified without a patch", githubFile{Filename: "a.tsx", Status: "modified", Additions: 400, SHA: blob}, consulted, false},
		{"modified with zero counts and no patch", githubFile{Filename: "a.bin", Status: "modified", SHA: blob}, consulted, false},
	}
	for _, tt := range tests {
		got := toPullRequestFiles([]githubFile{tt.file}, tt.base)[0].ContentComplete
		if got != tt.want {
			t.Errorf("%s: ContentComplete = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// stubTrees answers git/trees requests from listings keyed by the tree
// argument ("<commit>:<dir>", or "<commit>" for the root) and counts them.
func stubTrees(t *testing.T, listings map[string]any) *[]string {
	t.Helper()
	prev := runGH
	t.Cleanup(func() { runGH = prev })
	var asked []string
	runGH = func(args ...string) ([]byte, error) {
		endpoint := args[len(args)-1]
		tree, ok := strings.CutPrefix(endpoint, "repos/o/r/git/trees/")
		if !ok {
			return nil, fmt.Errorf("unexpected gh call %v", args)
		}
		asked = append(asked, tree)
		listing, ok := listings[tree]
		if !ok {
			return nil, fmt.Errorf("no listing for %s", tree)
		}
		return json.Marshal(listing)
	}
	return &asked
}

func listing(truncated bool, entries ...[3]string) map[string]any {
	var tree []map[string]string
	for _, e := range entries {
		tree = append(tree, map[string]string{"path": e[0], "sha": e[1], "type": e[2]})
	}
	return map[string]any{"truncated": truncated, "tree": tree}
}

func TestBaseBlobs(t *testing.T) {
	const commit = "c0ffee0000000000000000000000000000000000"
	asked := stubTrees(t, map[string]any{
		commit + ":src/pages":      listing(false, [3]string{"a.tsx", "aaa", "blob"}, [3]string{"b.tsx", "bbb", "blob"}, [3]string{"sub", "ttt", "tree"}),
		commit + ":my%20dir/x%23y": listing(false, [3]string{"c.tsx", "ccc", "blob"}),
		commit:                     listing(false, [3]string{"root.md", "rrr", "blob"}),
		commit + ":big":            listing(true, [3]string{"a.tsx", "aaa", "blob"}),
	})
	resolved := 0
	b := newBaseBlobs("o/r", func() (string, error) { resolved++; return commit, nil })
	if resolved != 0 || len(*asked) != 0 {
		t.Fatal("the commit and listings must be read on first use, not up front")
	}

	for _, c := range [][2]string{{"src/pages/a.tsx", "aaa"}, {"src/pages/b.tsx", "bbb"}, {"my dir/x#y/c.tsx", "ccc"}, {"root.md", "rrr"}} {
		got, err := b.blob(c[0])
		if err != nil || got != c[1] {
			t.Errorf("blob(%q) = %q, %v; want %q", c[0], got, err, c[1])
		}
	}
	if resolved != 1 {
		t.Errorf("commit resolved %d times, want once", resolved)
	}
	// Two files in src/pages cost one request, and the root is a bare commit.
	want := []string{commit + ":src/pages", commit + ":my%20dir/x%23y", commit}
	if got, wantAll := strings.Join(*asked, "\n"), strings.Join(want, "\n"); got != wantAll {
		t.Errorf("requests %q, want one per directory %q", *asked, want)
	}

	for _, file := range []string{"src/pages/sub", "src/pages/missing.tsx", "big/a.tsx"} {
		if got, err := b.blob(file); err == nil {
			t.Errorf("blob(%q) = %q, want an error: a tree, a missing file and a cut-short listing prove nothing", file, got)
		}
	}
}

func TestBaseBlobsCommitFailure(t *testing.T) {
	stubTrees(t, nil)
	b := newBaseBlobs("o/r", func() (string, error) { return "", errors.New("no merge base") })
	if _, err := b.blob("a.tsx"); err == nil {
		t.Fatal("an unresolvable commit must be an error, not an empty blob")
	}
}

// A pull request's rename with no patch is checked against the merge base of
// the pull request, read only when there is such a rename.
func TestMergeBaseOf(t *testing.T) {
	prev := runGH
	t.Cleanup(func() { runGH = prev })
	runGH = func(args ...string) ([]byte, error) {
		if got := args[len(args)-1]; got != "repos/o/r/compare/b1...h1?per_page=1" {
			return nil, fmt.Errorf("unexpected gh call %v", args)
		}
		return []byte(`{"merge_base_commit":{"sha":"mb1"}}`), nil
	}
	if got, err := mergeBaseOf("o/r", "b1", "h1"); err != nil || got != "mb1" {
		t.Fatalf("mergeBaseOf = %q, %v; want mb1", got, err)
	}
	runGH = func(...string) ([]byte, error) { return []byte(`{}`), nil }
	if _, err := mergeBaseOf("o/r", "b1", "h1"); err == nil {
		t.Fatal("a response with no merge base must be an error")
	}
}

// review-commit compares against the merge base, so a rename there is checked
// against that commit.
func TestFetchCommitInputRename(t *testing.T) {
	base, head := strings.Repeat("b", 40), strings.Repeat("a", 40)
	const same, edited = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	files := []githubFile{
		{Filename: "new/a.tsx", PreviousFilename: "old/a.tsx", Status: "renamed", SHA: same},
		{Filename: "new/b.png", PreviousFilename: "old/b.png", Status: "renamed", SHA: edited},
	}
	stubCommitGitHub(t, base, head, files, "ahead")
	inner := runGH
	runGH = func(args ...string) ([]byte, error) {
		if endpoint := args[len(args)-1]; endpoint == "repos/o/r/git/trees/"+base+":old" {
			return json.Marshal(listing(false, [3]string{"a.tsx", same, "blob"}, [3]string{"b.png", "3333333333333333333333333333333333333333", "blob"}))
		}
		return inner(args...)
	}
	in, err := fetchCommitInput("o/r", head, "")
	if err != nil {
		t.Fatal(err)
	}
	if !in.Files[0].ContentComplete || in.Files[1].ContentComplete {
		t.Fatalf("complete = %v, %v; want the unedited rename complete and the edited one not", in.Files[0].ContentComplete, in.Files[1].ContentComplete)
	}
}

// A comparison between two heads has the same rule: a rename that cannot be
// proven unedited counts as wholly changed.
func TestFetchChangesRename(t *testing.T) {
	const base, head = "b1", "h1"
	const same, edited = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	prev := runGH
	t.Cleanup(func() { runGH = prev })
	runGH = func(args ...string) ([]byte, error) {
		switch endpoint := args[len(args)-1]; endpoint {
		case "repos/o/r/compare/" + base + "..." + head:
			return json.Marshal(map[string]any{"status": "ahead", "files": []githubFile{
				{Filename: "new/a.tsx", PreviousFilename: "old/a.tsx", Status: "renamed", SHA: same},
				{Filename: "new/b.png", PreviousFilename: "old/b.png", Status: "renamed", SHA: edited},
			}})
		case "repos/o/r/git/trees/" + base + ":old":
			return json.Marshal(listing(false, [3]string{"a.tsx", same, "blob"}, [3]string{"b.png", "3333333333333333333333333333333333333333", "blob"}))
		}
		return nil, fmt.Errorf("unexpected gh call %v", args)
	}
	files, complete, err := fetchChanges("o/r", base, head)
	if err != nil || !complete {
		t.Fatalf("fetchChanges = %v, %v", complete, err)
	}
	if !files[0].ContentComplete || files[1].ContentComplete {
		t.Fatalf("complete = %v, %v; want the unedited rename complete and the edited one not", files[0].ContentComplete, files[1].ContentComplete)
	}
}
