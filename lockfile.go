package radar

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// PullRequestLockfiles turns on lockfile review: recognised lockfiles are
// never withheld as generated, the agents get a per-package summary next to
// the raw lines, and the backstops below route a change to a human.
type PullRequestLockfiles struct {
	// RequireHumanWithoutManifest routes to a human a lockfile that changes
	// without a matching manifest change in the same pull request.
	RequireHumanWithoutManifest bool `json:"require_human_without_manifest,omitempty"`
	// RequireHumanOnSourceChange routes to a human any change to where a
	// package is resolved from: a registry, index, git or URL source.
	RequireHumanOnSourceChange bool `json:"require_human_on_source_change,omitempty"`
}

// LockfileKind names a lockfile format Radar can summarise.
type LockfileKind string

const (
	LockfileGradle LockfileKind = "gradle"
	LockfilePoetry LockfileKind = "poetry"
	LockfileUV     LockfileKind = "uv"
	LockfileGoSum  LockfileKind = "go-sum"
	LockfileNPM    LockfileKind = "npm"
)

func lockfileKindOf(p string) (LockfileKind, bool) {
	switch path.Base(p) {
	case "gradle.lockfile":
		return LockfileGradle, true
	case "poetry.lock":
		return LockfilePoetry, true
	case "uv.lock":
		return LockfileUV, true
	case "go.sum":
		return LockfileGoSum, true
	case "package-lock.json":
		return LockfileNPM, true
	}
	return "", false
}

// LockfileChange is one package's change in a lockfile.
type LockfileChange struct {
	Package string `json:"package"`
	// Kind is upgrade, downgrade, added, removed, hash-only or source-change.
	Kind   string `json:"kind"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// LockfileSummary is what Radar could read from one lockfile's patch.
type LockfileSummary struct {
	Path    string           `json:"path"`
	Kind    LockfileKind     `json:"kind"`
	Changes []LockfileChange `json:"changes,omitempty"`
	// Unattributed counts changed lines Radar could not tie to a package,
	// usually because the hunk omits the package's name; the raw lines are
	// still shown.
	Unattributed int `json:"unattributed,omitempty"`
}

// SupplyChainSignals lists the changes reviewers must treat as blocking.
func (s LockfileSummary) SupplyChainSignals() []LockfileChange {
	var out []LockfileChange
	for _, c := range s.Changes {
		switch c.Kind {
		case "downgrade", "hash-only", "source-change":
			out = append(out, c)
		}
	}
	return out
}

type pkgDelta struct {
	removed, added             []string // versions
	removedHashes, addedHashes []string
	removedSrc, addedSrc       []string
}

type deltas struct {
	order []string
	by    map[string]*pkgDelta
	// known are sources the lockfile already used: on unchanged or
	// removed lines.
	known map[string]bool
}

func (d *deltas) know(src string) {
	if d.known == nil {
		d.known = map[string]bool{}
	}
	d.known[src] = true
}

func (d *deltas) get(name string) *pkgDelta {
	if d.by == nil {
		d.by = map[string]*pkgDelta{}
	}
	if p, ok := d.by[name]; ok {
		return p
	}
	p := &pkgDelta{}
	d.by[name] = p
	d.order = append(d.order, name)
	return p
}

// summarizeLockfile reads per-package changes from a unified patch. It only
// sees what the hunks show, so it reports what it cannot attribute instead
// of guessing.
func summarizeLockfile(p string, kind LockfileKind, patch string) LockfileSummary {
	sum := LockfileSummary{Path: p, Kind: kind}
	var d deltas
	switch kind {
	case LockfileGradle:
		sum.Unattributed = parseGradleLock(patch, &d)
	case LockfileGoSum:
		sum.Unattributed = parseGoSum(patch, &d)
	case LockfilePoetry, LockfileUV:
		sum.Unattributed = parseTOMLLock(patch, &d)
	case LockfileNPM:
		sum.Unattributed = parseNPMLock(patch, &d)
	}
	for _, name := range d.order {
		sum.Changes = append(sum.Changes, classifyDelta(name, d.by[name], d.known)...)
	}
	return sum
}

func patchLines(patch string, fn func(op byte, text string)) {
	for _, line := range strings.Split(patch, "\n") {
		if line == "" || strings.HasPrefix(line, "@@") || strings.HasPrefix(line, `\`) {
			continue
		}
		fn(line[0], line[1:])
	}
}

func parseGradleLock(patch string, d *deltas) int {
	unattributed := 0
	patchLines(patch, func(op byte, text string) {
		if op != '+' && op != '-' {
			return
		}
		coord, _, _ := strings.Cut(strings.TrimSpace(text), "=")
		parts := strings.Split(coord, ":")
		if len(parts) != 3 {
			if !strings.HasPrefix(text, "#") && !strings.HasPrefix(text, "empty=") {
				unattributed++
			}
			return
		}
		p := d.get(parts[0] + ":" + parts[1])
		addVersion(p, op, parts[2])
	})
	return unattributed
}

func parseGoSum(patch string, d *deltas) int {
	unattributed := 0
	patchLines(patch, func(op byte, text string) {
		if op != '+' && op != '-' {
			return
		}
		fields := strings.Fields(text)
		if len(fields) != 3 {
			unattributed++
			return
		}
		version := strings.TrimSuffix(fields[1], "/go.mod")
		p := d.get(fields[0])
		addVersion(p, op, version)
		addHash(p, op, version+" "+fields[2])
	})
	return unattributed
}

var (
	tomlName    = regexp.MustCompile(`^name = "([^"]+)"`)
	tomlVersion = regexp.MustCompile(`^version = "([^"]+)"`)
	tomlHash    = regexp.MustCompile(`(?:hash|sha256) = "([^"]+)"`)
	// uv records a source on every package; poetry only in [package.source].
	tomlUVSource = regexp.MustCompile(`^source = \{`)
)

// parseTOMLLock handles poetry.lock and uv.lock, whose [[package]] blocks
// start with name and version lines.
func parseTOMLLock(patch string, d *deltas) int {
	unattributed := 0
	current := ""
	inSource := false
	patchLines(patch, func(op byte, text string) {
		t := strings.TrimSpace(text)
		switch {
		case t == "[[package]]" || t == "[metadata]" || strings.HasPrefix(t, "[metadata."):
			current, inSource = "", false
			return
		case strings.HasPrefix(t, "content-hash = "):
			// Changes with every manifest edit; the manifest check covers it.
			return
		case strings.HasPrefix(t, "[package.source]"):
			inSource = true
			return
		case strings.HasPrefix(t, "[") && !strings.HasPrefix(t, "[["):
			inSource = false
		}
		if m := tomlName.FindStringSubmatch(t); m != nil {
			current = m[1]
			d.get(current)
			return
		}
		isSource := (inSource && t != "") || tomlUVSource.MatchString(t)
		if isSource && op != '+' {
			d.know(t)
		}
		if op != '+' && op != '-' {
			return
		}
		if current == "" {
			unattributed++
			return
		}
		p := d.get(current)
		switch {
		case tomlVersion.MatchString(t):
			addVersion(p, op, tomlVersion.FindStringSubmatch(t)[1])
		case isSource:
			addSource(p, op, t)
		case tomlHash.MatchString(t):
			addHash(p, op, tomlHash.FindStringSubmatch(t)[1])
		}
	})
	return unattributed
}

var (
	npmKey       = regexp.MustCompile(`^"((?:node_modules/)?(?:@[^"/]+/)?[^"/]+(?:/node_modules/(?:@[^"/]+/)?[^"/]+)*)": \{`)
	npmVersion   = regexp.MustCompile(`^"version": "([^"]+)"`)
	npmResolved  = regexp.MustCompile(`^"resolved": "([^"]+)"`)
	npmIntegrity = regexp.MustCompile(`^"integrity": "([^"]+)"`)
)

func parseNPMLock(patch string, d *deltas) int {
	unattributed := 0
	current := ""
	patchLines(patch, func(op byte, text string) {
		t := strings.TrimSpace(text)
		if m := npmKey.FindStringSubmatch(t); m != nil {
			current = m[1]
			if i := strings.LastIndex(current, "node_modules/"); i >= 0 {
				current = current[i+len("node_modules/"):]
			}
			return
		}
		if m := npmResolved.FindStringSubmatch(t); m != nil && op != '+' {
			d.know(registryOf(m[1]))
		}
		if op != '+' && op != '-' {
			return
		}
		if current == "" {
			unattributed++
			return
		}
		p := d.get(current)
		switch {
		case npmVersion.MatchString(t):
			addVersion(p, op, npmVersion.FindStringSubmatch(t)[1])
		case npmResolved.MatchString(t):
			addSource(p, op, registryOf(npmResolved.FindStringSubmatch(t)[1]))
		case npmIntegrity.MatchString(t):
			addHash(p, op, npmIntegrity.FindStringSubmatch(t)[1])
		}
	})
	return unattributed
}

// registryOf keeps the part of a resolved URL that names where packages come
// from, so a version bump on the same registry is not a source change.
func registryOf(resolved string) string {
	u, err := url.Parse(resolved)
	if err != nil || u.Host == "" {
		return resolved
	}
	return u.Scheme + "://" + u.Host
}

func addVersion(p *pkgDelta, op byte, v string) {
	if op == '+' {
		p.added = append(p.added, v)
	} else {
		p.removed = append(p.removed, v)
	}
}

func addHash(p *pkgDelta, op byte, h string) {
	if op == '+' {
		p.addedHashes = append(p.addedHashes, h)
	} else {
		p.removedHashes = append(p.removedHashes, h)
	}
}

func addSource(p *pkgDelta, op byte, s string) {
	if op == '+' {
		p.addedSrc = append(p.addedSrc, s)
	} else {
		p.removedSrc = append(p.removedSrc, s)
	}
}

func nonEmptyUnique(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range values {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func classifyDelta(name string, p *pkgDelta, known map[string]bool) []LockfileChange {
	var out []LockfileChange
	removed, added := nonEmptyUnique(p.removed), nonEmptyUnique(p.added)
	// A version on both sides is unchanged; a go.sum line pair for the
	// same version is a hash change, handled below.
	both := map[string]bool{}
	for _, v := range removed {
		for _, w := range added {
			if v == w {
				both[v] = true
			}
		}
	}
	removed, added = without(removed, both), without(added, both)
	switch {
	case len(removed) == 0 && len(added) > 0:
		out = append(out, LockfileChange{Package: name, Kind: "added", To: strings.Join(added, ", ")})
	case len(added) == 0 && len(removed) > 0:
		out = append(out, LockfileChange{Package: name, Kind: "removed", From: strings.Join(removed, ", ")})
	case len(added) > 0:
		from, to := highest(removed), highest(added)
		kind := "upgrade"
		if compareVersions(to, from) < 0 {
			kind = "downgrade"
		}
		out = append(out, LockfileChange{Package: name, Kind: kind, From: strings.Join(removed, ", "), To: strings.Join(added, ", ")})
	}
	versionChanged := len(removed) > 0 || len(added) > 0
	if hr, ha := nonEmptyUnique(p.removedHashes), nonEmptyUnique(p.addedHashes); !versionChanged && len(hr)+len(ha) > 0 && !sameSet(hr, ha) {
		out = append(out, LockfileChange{Package: name, Kind: "hash-only", Detail: fmt.Sprintf("%d hash(es) removed, %d added with no version change", len(hr), len(ha))})
	}
	// A source is new unless the package already had it or the lockfile
	// uses it elsewhere; a package that is only removed adds no source.
	sr, sa := nonEmptyUnique(p.removedSrc), nonEmptyUnique(p.addedSrc)
	had := map[string]bool{}
	for _, v := range sr {
		had[v] = true
	}
	var fresh []string
	for _, v := range sa {
		if !had[v] && !known[v] {
			fresh = append(fresh, v)
		}
	}
	if len(fresh) > 0 || (len(sr) > 0 && len(sa) > 0 && !sameSet(sr, sa)) {
		out = append(out, LockfileChange{Package: name, Kind: "source-change", From: strings.Join(sr, "; "), To: strings.Join(sa, "; ")})
	}
	return out
}

func without(values []string, drop map[string]bool) []string {
	var out []string
	for _, v := range values {
		if !drop[v] {
			out = append(out, v)
		}
	}
	return out
}

func sameSet(a, b []string) bool {
	return strings.Join(a, "\x00") == strings.Join(b, "\x00")
}

func highest(versions []string) string {
	best := ""
	for _, v := range versions {
		if best == "" || compareVersions(v, best) > 0 {
			best = v
		}
	}
	return best
}

var versionPart = regexp.MustCompile(`\d+|[A-Za-z]+`)

// compareVersions orders dotted versions numerically segment by segment; a
// pre-release suffix sorts before the release it precedes.
func compareVersions(a, b string) int {
	pa, pb := versionPart.FindAllString(strings.TrimPrefix(a, "v"), -1), versionPart.FindAllString(strings.TrimPrefix(b, "v"), -1)
	for i := 0; i < len(pa) || i < len(pb); i++ {
		switch {
		case i >= len(pa):
			return releaseOrder(pb[i])
		case i >= len(pb):
			return -releaseOrder(pa[i])
		}
		na, ea := strconv.Atoi(pa[i])
		nb, eb := strconv.Atoi(pb[i])
		switch {
		case ea == nil && eb == nil && na != nb:
			if na < nb {
				return -1
			}
			return 1
		case ea == nil && eb != nil:
			return 1
		case ea != nil && eb == nil:
			return -1
		case ea != nil && eb != nil && pa[i] != pb[i]:
			return strings.Compare(pa[i], pb[i])
		}
	}
	return 0
}

// releaseOrder is how a version compares to one that has an extra trailing
// segment: 1.0 > 1.0-rc1 but 1.0 < 1.0.1.
func releaseOrder(extra string) int {
	if _, err := strconv.Atoi(extra); err == nil {
		return -1
	}
	return 1
}

// manifestFor reports whether a changed path is a manifest a lockfile of
// this kind is generated from. Gradle lockfiles are regenerated from build
// scripts and version catalogs anywhere in the build; the others from the
// manifest beside them.
func manifestFor(kind LockfileKind, lockPath, candidate string) bool {
	base := path.Base(candidate)
	switch kind {
	case LockfileGradle:
		return strings.HasSuffix(base, ".gradle") || strings.HasSuffix(base, ".gradle.kts") ||
			base == "gradle.properties" || (strings.HasSuffix(base, ".toml") && path.Base(path.Dir(candidate)) == "gradle")
	}
	if path.Dir(candidate) != path.Dir(lockPath) {
		return false
	}
	switch kind {
	case LockfilePoetry, LockfileUV:
		return base == "pyproject.toml"
	case LockfileGoSum:
		return base == "go.mod"
	case LockfileNPM:
		return base == "package.json"
	}
	return false
}

// lockfileBackstop is the deterministic human-required rule for lockfiles.
func lockfileBackstop(cfg PullRequestLockfiles, files []PullRequestFile, summaries []LockfileSummary) (bool, string) {
	for _, s := range summaries {
		if cfg.RequireHumanOnSourceChange {
			for _, c := range s.Changes {
				if c.Kind == "source-change" {
					return true, fmt.Sprintf("%s changes where %s is resolved from", s.Path, c.Package)
				}
			}
		}
		if cfg.RequireHumanWithoutManifest {
			matched := false
			for _, f := range files {
				if manifestFor(s.Kind, s.Path, f.Path) {
					matched = true
					break
				}
			}
			if !matched {
				return true, s.Path + " changes without a matching manifest change"
			}
		}
	}
	return false, "lockfile changes have matching manifests and no source changes"
}

// renderLockfileSummaries formats the summaries for the review prompt.
// Lockfiles with identical summaries, such as one dependency bump
// regenerating many Gradle lockfiles, are listed once.
func renderLockfileSummaries(summaries []LockfileSummary) string {
	type group struct {
		summary LockfileSummary
		paths   []string
	}
	var groups []*group
	byKey := map[string]*group{}
	for _, s := range summaries {
		key := fmt.Sprintf("%s\x00%v\x00%d", s.Kind, s.Changes, s.Unattributed)
		if g, ok := byKey[key]; ok {
			g.paths = append(g.paths, s.Path)
			continue
		}
		g := &group{summary: s, paths: []string{s.Path}}
		byKey[key] = g
		groups = append(groups, g)
	}
	var b strings.Builder
	b.WriteString("--- lockfile summary (parsed from the patches below; check it against them) ---\n")
	for _, g := range groups {
		s := g.summary
		if len(g.paths) == 1 {
			fmt.Fprintf(&b, "%s (%s):", s.Path, s.Kind)
		} else {
			fmt.Fprintf(&b, "%d %s lockfiles with the same changes (%s):", len(g.paths), s.Kind, strings.Join(g.paths, ", "))
		}
		if len(s.Changes) == 0 {
			b.WriteString(" no package changes parsed")
		}
		b.WriteString("\n")
		for _, c := range s.Changes {
			switch c.Kind {
			case "added":
				fmt.Fprintf(&b, "  added %s %s\n", c.Package, c.To)
			case "removed":
				fmt.Fprintf(&b, "  removed %s %s\n", c.Package, c.From)
			case "upgrade":
				fmt.Fprintf(&b, "  upgrade %s %s -> %s\n", c.Package, c.From, c.To)
			case "downgrade":
				fmt.Fprintf(&b, "  SUPPLY-CHAIN SIGNAL downgrade %s %s -> %s\n", c.Package, c.From, c.To)
			case "hash-only":
				fmt.Fprintf(&b, "  SUPPLY-CHAIN SIGNAL hash-only %s: %s\n", c.Package, c.Detail)
			case "source-change":
				fmt.Fprintf(&b, "  SUPPLY-CHAIN SIGNAL source-change %s: %q -> %q\n", c.Package, c.From, c.To)
			}
		}
		if s.Unattributed > 0 {
			fmt.Fprintf(&b, "  %d changed line(s) not tied to a package; read the patch\n", s.Unattributed)
		}
	}
	b.WriteString("\n")
	return b.String()
}

// changeLinesKey identifies a lockfile change by its +/- lines and summary,
// ignoring context, so identical regenerations can be shown once.
func changeLinesKey(c Change, s LockfileSummary) string {
	var lines []string
	patchLines(c.Content, func(op byte, text string) {
		if op == '+' || op == '-' {
			lines = append(lines, string(op)+text)
		}
	})
	sort.Strings(lines)
	return fmt.Sprintf("%s\x00%v\x00%d\x00%s", s.Kind, s.Changes, s.Unattributed, strings.Join(lines, "\n"))
}

// splitLockfilePatch splits one lockfile patch into pieces under budget at
// hunk boundaries, and a single oversized hunk at line boundaries.
func splitLockfilePatch(content string, budget int) []string {
	var pieces []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			pieces = append(pieces, cur.String())
			cur.Reset()
		}
	}
	for _, line := range strings.SplitAfter(content, "\n") {
		if strings.HasPrefix(line, "@@") && cur.Len() > 0 && cur.Len()+len(line) > budget/2 {
			flush()
		}
		if cur.Len()+len(line) > budget {
			flush()
		}
		cur.WriteString(line)
	}
	flush()
	return pieces
}
