package radar

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strings"
)

// PullRequestGeneratedFiles configures which changed files are withheld from
// the review agents as generated. Withheld files are still listed to the agent
// with their status and line counts, and every policy gate (deny paths, deny
// phrases, file coverage) still sees them.
type PullRequestGeneratedFiles struct {
	// Gitattributes is ".gitattributes" to read the root .gitattributes of
	// the trusted checkout the policy is read from; its linguist-generated
	// entries mark generated paths. It must never be read from the pull
	// request head, or a change could mark its own files generated.
	Gitattributes string `json:"gitattributes,omitempty"`
	// Paths are additional generated path globs.
	Paths []string `json:"paths,omitempty"`
	// HeaderMarkers are regular expressions matched against a file's first
	// lines. A header never withholds a file, since anyone can write one: a
	// file carrying one outside every generated path is reviewed in full and
	// listed in the decision's generated_unlisted, so its path can be added.
	HeaderMarkers []string `json:"header_markers,omitempty"`
	// Note is passed to the agent alongside the withheld list, e.g. which CI
	// checks verify generated output.
	Note string `json:"note,omitempty"`
}

// headerScanLines bounds how far into a file a generated-code header may sit.
const headerScanLines = 10

// GeneratedFile is a changed file withheld from the review agents.
type GeneratedFile struct {
	Path      string `json:"path"`
	Status    string `json:"status,omitempty"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	// Reason is "path" (a configured or .gitattributes pattern) or "header".
	Reason string `json:"reason"`
}

type generatedMatcher struct {
	rules   []generatedRule
	headers []*regexp.Regexp
}

// generatedRule is one ordered path rule; as in git, the last rule matching
// a path decides.
type generatedRule struct {
	glob      string
	generated bool
}

func (p PullRequestGeneratedFiles) validate() error {
	// Patterns in a nested .gitattributes are relative to its directory;
	// only the root file's patterns are anchored where Radar applies them.
	if p.Gitattributes != "" && p.Gitattributes != ".gitattributes" {
		return fmt.Errorf("radar: generated_files.gitattributes must be the root .gitattributes")
	}
	for _, pattern := range p.Paths {
		if _, err := compilePathGlob(pattern); err != nil {
			return fmt.Errorf("radar: generated_files.paths: %w", err)
		}
	}
	for _, marker := range p.HeaderMarkers {
		if _, err := regexp.Compile(marker); err != nil {
			return fmt.Errorf("radar: generated_files.header_markers: %w", err)
		}
	}
	return nil
}

// ParseGitattributesGenerated returns, in file order, every .gitattributes
// rule that sets or unsets linguist-generated, translated to Radar's glob
// syntax. Later rules override earlier ones for the paths they match.
func ParseGitattributesGenerated(data []byte) ([]GitattributesRule, error) {
	var rules []GitattributesRule
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		generated, mentioned := false, false
		for _, attr := range fields[1:] {
			switch attr {
			case "linguist-generated", "linguist-generated=true":
				generated, mentioned = true, true
			case "-linguist-generated", "!linguist-generated", "linguist-generated=false":
				generated, mentioned = false, true
			}
		}
		if !mentioned {
			continue
		}
		glob, err := gitattributesGlob(fields[0])
		if err != nil {
			return nil, err
		}
		rules = append(rules, GitattributesRule{Glob: glob, Generated: generated})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return rules, nil
}

// GitattributesRule is one linguist-generated setting from .gitattributes.
type GitattributesRule struct {
	Glob      string
	Generated bool
}

// gitattributesGlob converts a .gitattributes pattern: one without a slash
// matches a basename at any depth, and a leading slash anchors at the root.
func gitattributesGlob(pattern string) (string, error) {
	if strings.HasPrefix(pattern, "!") || strings.HasPrefix(pattern, "\"") {
		return "", fmt.Errorf("radar: unsupported .gitattributes pattern %q", pattern)
	}
	glob := strings.TrimPrefix(pattern, "/")
	if !strings.Contains(pattern, "/") {
		glob = "**/" + glob
	}
	if _, err := compilePathGlob(glob); err != nil {
		return "", err
	}
	return glob, nil
}

func newGeneratedMatcher(p PullRequestGeneratedFiles, attrs []GitattributesRule) generatedMatcher {
	var m generatedMatcher
	for _, rule := range attrs {
		m.rules = append(m.rules, generatedRule{glob: rule.Glob, generated: rule.Generated})
	}
	// Policy paths come last so a .gitattributes entry cannot unset them.
	for _, glob := range p.Paths {
		m.rules = append(m.rules, generatedRule{glob: glob, generated: true})
	}
	for _, marker := range p.HeaderMarkers {
		m.headers = append(m.headers, regexp.MustCompile(marker))
	}
	return m
}

func (m generatedMatcher) pathGenerated(value string) bool {
	generated := false
	for _, rule := range m.rules {
		if matchesAnyPath(value, []string{rule.glob}) {
			generated = rule.generated
		}
	}
	return generated
}

// classify reports whether a file is withheld and why. Only a generated path
// withholds, and only for a file that already existed there: a file the
// change adds or copies, or moves in from a hand-written path, is reviewed in
// full, so naming a new file like generated code cannot hide it. unlisted is set for
// a reviewed file that carries a generated-code header outside every
// generated path.
func (m generatedMatcher) classify(f PullRequestFile) (reason string, unlisted bool) {
	if m.pathGenerated(f.Path) {
		switch f.Status {
		case "modified", "removed", "changed":
			return "path", false
		case "renamed":
			if f.PreviousPath != "" && m.pathGenerated(f.PreviousPath) {
				return "path", false
			}
		}
		// added and copied create a new file, so they are always reviewed.
		return "", false
	}
	if len(m.headers) > 0 && m.hasHeader(f.Patch) {
		return "", true
	}
	return "", false
}

// hasHeader reports a generated-code marker in the first lines of a hunk
// that starts at the top of the file.
func (m generatedMatcher) hasHeader(patch string) bool {
	lines := strings.Split(patch, "\n")
	if len(lines) == 0 || !firstHunkStartsAtTop(lines[0]) {
		return false
	}
	newLine := 0
	for _, line := range lines[1:] {
		if strings.HasPrefix(line, "@@") || newLine >= headerScanLines {
			break
		}
		if line == "" || line[0] == '-' {
			continue
		}
		newLine++
		if m.matchesHeader(line[1:]) {
			return true
		}
	}
	return false
}

func (m generatedMatcher) matchesHeader(text string) bool {
	for _, re := range m.headers {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

func firstHunkStartsAtTop(header string) bool {
	match := hunkHeader.FindStringSubmatch(header)
	return match != nil && (match[2] == "1" || match[2] == "0")
}
