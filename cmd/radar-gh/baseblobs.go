package main

import (
	"fmt"
	"net/url"
	"path"
	"strings"
)

// blobAt reads the blob SHA a path had in the commit a file list was
// compared against.
type blobAt func(path string) (string, error)

// baseBlobs is a blobAt over one commit. It reads one directory listing per
// directory, so moving a folder of unedited files costs one request. The
// commit is resolved on first use: most reviews have no rename without a
// patch.
type baseBlobs struct {
	repo   string
	commit func() (string, error)
	sha    string
	dirs   map[string]map[string]string
}

func newBaseBlobs(repo string, commit func() (string, error)) *baseBlobs {
	return &baseBlobs{repo: repo, commit: commit, dirs: map[string]map[string]string{}}
}

// atCommit is a commit known up front.
func atCommit(sha string) func() (string, error) {
	return func() (string, error) { return sha, nil }
}

func (b *baseBlobs) blob(file string) (string, error) {
	dir, name := path.Dir(file), path.Base(file)
	entries, ok := b.dirs[dir]
	if !ok {
		if b.sha == "" {
			sha, err := b.commit()
			if err != nil {
				return "", err
			}
			b.sha = sha
		}
		endpoint := fmt.Sprintf("repos/%s/git/trees/%s", b.repo, b.sha)
		if dir != "." {
			segments := strings.Split(dir, "/")
			for i, s := range segments {
				segments[i] = url.PathEscape(s)
			}
			endpoint += ":" + strings.Join(segments, "/")
		}
		var tree struct {
			Truncated bool `json:"truncated"`
			Tree      []struct {
				Path string `json:"path"`
				SHA  string `json:"sha"`
				Type string `json:"type"`
			} `json:"tree"`
		}
		if err := ghJSON(&tree, "api", endpoint); err != nil {
			return "", err
		}
		if tree.Truncated {
			return "", fmt.Errorf("GitHub cut short the listing of %s at %s", dir, shortSHA(b.sha))
		}
		entries = map[string]string{}
		for _, e := range tree.Tree {
			if e.Type == "blob" {
				entries[e.Path] = e.SHA
			}
		}
		b.dirs[dir] = entries
	}
	sha, ok := entries[name]
	if !ok {
		return "", fmt.Errorf("%s is not a file at %s", file, shortSHA(b.sha))
	}
	return sha, nil
}

// mergeBaseOf is the commit GitHub's three-dot diff of head against base
// starts from, which is what a pull request's file list is relative to.
func mergeBaseOf(repo, base, head string) (string, error) {
	var cmp struct {
		MergeBase struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
	}
	if err := ghJSON(&cmp, "api", fmt.Sprintf("repos/%s/compare/%s...%s?per_page=1", repo, base, head)); err != nil {
		return "", fmt.Errorf("reading the merge base of %s and %s: %w", shortSHA(base), shortSHA(head), err)
	}
	if cmp.MergeBase.SHA == "" {
		return "", fmt.Errorf("GitHub returned no merge base for %s and %s", shortSHA(base), shortSHA(head))
	}
	return cmp.MergeBase.SHA, nil
}
