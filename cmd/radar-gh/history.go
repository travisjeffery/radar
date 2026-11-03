package main

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/travisjeffery/radar"
)

// priorState is the round before the one a comment shows: its head and the
// findings Radar reported there. The comment keeps it so that every stage at
// a head, the verdict and clarify included, compares with the same round.
type priorState struct {
	Head     string         `json:"head"`
	Round    int            `json:"round,omitempty"`
	Findings []stateFinding `json:"findings,omitempty"`
	// Truncated is set when its blocking findings were cut to fit.
	Truncated bool `json:"truncated,omitempty"`
}

// roundBaseline returns head's review round and the round before it. A
// comment for head itself (the verdict after the findings stage, or a
// clarification) keeps the baseline it recorded; a comment for another head
// becomes the baseline. A comment from before rounds were recorded counts as
// round 1.
func roundBaseline(prev commentState, hasPrev bool, head string) (int, *priorState) {
	if !hasPrev || prev.Head == "" {
		return 1, nil
	}
	round := max(prev.Round, 1)
	if prev.Head == head {
		return round, prev.Prior
	}
	return round + 1, &priorState{Head: prev.Head, Round: round, Findings: prev.Findings, Truncated: prev.Truncated}
}

// loadHistory sets in.Prior and in.ResolvedFindings from Radar's previous
// comment, the comparison of its head with this one, and the pull request's
// resolved Radar threads. Each part is best effort: one that
// cannot be read is left out and logged, and every blocking finding then
// counts as in a first review, which is never less strict.
func loadHistory(repo string, number int, in *radar.PullRequestInput, previous string) {
	warn := func(what string, err error) {
		fmt.Fprintf(os.Stderr, "radar-gh: review history: %s: %v\n", what, err)
	}
	prev, ok := parseCommentState(previous)
	if _, base := roundBaseline(prev, ok, in.HeadSHA); base != nil {
		p := &radar.PriorReview{Head: base.Head, Round: base.Round}
		p.Findings = priorFindingsOf(base.Findings)
		changes, complete, err := fetchChanges(repo, base.Head, in.HeadSHA)
		if err != nil {
			warn(fmt.Sprintf("comparing %s with round %d's head %s", shortSHA(in.HeadSHA), base.Round, shortSHA(base.Head)), err)
		}
		if base.Truncated {
			// Without every prior blocker, a still-open one would look new.
			warn(fmt.Sprintf("round %d's findings", base.Round), fmt.Errorf("its blocking findings were cut to fit the comment; every blocking finding counts"))
			complete = false
		}
		p.Changes, p.ChangesComplete = changes, complete
		in.Prior = p
	}
	resolved, err := fetchResolvedFindings(repo, number, in.Author)
	if err != nil {
		warn("resolved threads", err)
	}
	in.ResolvedFindings = resolved
}

// priorFindingsOf is the baseline a later round compares with. A finding
// raised late is left out, so that raised again on lines still unchanged it
// stays apart; one held back there did not block, so it counts at P2 and
// cannot block again as "already blocking".
func priorFindingsOf(state []stateFinding) []radar.ReviewFinding {
	var out []radar.ReviewFinding
	for _, f := range state {
		if f.Late {
			continue
		}
		severity := f.Severity
		if f.Held {
			severity = "P2"
		}
		out = append(out, radar.ReviewFinding{ID: f.ID, Severity: severity, Title: f.Title, File: f.File, Line: f.Line, RuleID: f.RuleID})
	}
	return out
}

// fetchChanges returns the files that differ between base and head. It is
// complete only when head descends from base: after a force-push the
// three-dot comparison GitHub offers is against their merge base, not base.
func fetchChanges(repo, base, head string) ([]radar.PullRequestFile, bool, error) {
	var cmp struct {
		Status string       `json:"status"`
		Files  []githubFile `json:"files"`
	}
	if err := ghJSON(&cmp, "api", fmt.Sprintf("repos/%s/compare/%s...%s", repo, base, head)); err != nil {
		return nil, false, err
	}
	switch cmp.Status {
	case "identical":
		return nil, true, nil
	case "ahead":
	default:
		return nil, false, fmt.Errorf("%s is %s of %s (a force-push?); every blocking finding counts", shortSHA(head), cmp.Status, shortSHA(base))
	}
	if len(cmp.Files) >= maxCompareFiles {
		return nil, false, fmt.Errorf("the comparison lists %d files, GitHub's limit; every blocking finding counts", len(cmp.Files))
	}
	files := toPullRequestFiles(cmp.Files)
	for i := range files {
		// A patch GitHub cut short would hide changed lines: without a
		// patch the file counts as wholly changed.
		if !files[i].ContentComplete {
			files[i].Patch = ""
		}
	}
	return files, true, nil
}

// inlineTitle reads the finding title from an inline comment's first line,
// "**P1: title** (Radar, ...".
var inlineTitle = regexp.MustCompile(`^\*\*P[0-3]: (.*?)\*\* \(Radar`)

var inlineMarker = regexp.MustCompile(`<!-- radar-finding ([0-9a-f]+) -->`)

// fetchResolvedFindings returns Radar's inline finding threads that a
// person resolved and whose lines have not changed since: GitHub marks a
// thread outdated once a push changes them, and an outdated one is a
// finding to look at afresh.
func fetchResolvedFindings(repo string, number int, author string) ([]radar.ResolvedFinding, error) {
	owner, name, _ := strings.Cut(repo, "/")
	const query = `query($owner:String!,$name:String!,$number:Int!,$cursor:String){repository(owner:$owner,name:$name){pullRequest(number:$number){reviewThreads(first:100,after:$cursor){nodes{isResolved isOutdated path startLine line resolvedBy{login} comments(first:1){nodes{author{login __typename} body}}} pageInfo{hasNextPage endCursor}}}}}`
	var out []radar.ResolvedFinding
	cursor := ""
	for {
		args := []string{"api", "graphql", "-f", "query=" + query, "-f", "owner=" + owner, "-f", "name=" + name, "-F", "number=" + strconv.Itoa(number)}
		if cursor != "" {
			args = append(args, "-f", "cursor="+cursor)
		}
		var response struct {
			Data struct {
				Repository struct {
					PullRequest struct {
						ReviewThreads struct {
							Nodes    []resolvedThreadNode `json:"nodes"`
							PageInfo struct {
								HasNextPage bool   `json:"hasNextPage"`
								EndCursor   string `json:"endCursor"`
							} `json:"pageInfo"`
						} `json:"reviewThreads"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
		}
		if err := ghJSON(&response, args...); err != nil {
			return out, err
		}
		threads := response.Data.Repository.PullRequest.ReviewThreads
		out = append(out, resolvedFindings(threads.Nodes, author)...)
		if !threads.PageInfo.HasNextPage || threads.PageInfo.EndCursor == "" || threads.PageInfo.EndCursor == cursor {
			return out, nil
		}
		cursor = threads.PageInfo.EndCursor
	}
}

type resolvedThreadNode struct {
	IsResolved bool   `json:"isResolved"`
	IsOutdated bool   `json:"isOutdated"`
	Path       string `json:"path"`
	StartLine  int    `json:"startLine"`
	Line       int    `json:"line"`
	ResolvedBy *struct {
		Login string `json:"login"`
	} `json:"resolvedBy"`
	Comments struct {
		Nodes []struct {
			Author struct {
				Login    string `json:"login"`
				Typename string `json:"__typename"`
			} `json:"author"`
			Body string `json:"body"`
		} `json:"nodes"`
	} `json:"comments"`
}

// resolvedFindings keeps the resolved, current threads Radar opened: a
// bot's comment that ends with the finding marker the workflow posts.
func resolvedFindings(nodes []resolvedThreadNode, author string) []radar.ResolvedFinding {
	var out []radar.ResolvedFinding
	for _, t := range nodes {
		if !t.IsResolved || t.IsOutdated || t.Line <= 0 || len(t.Comments.Nodes) == 0 {
			continue
		}
		c := t.Comments.Nodes[0]
		m := inlineMarker.FindStringSubmatch(c.Body)
		if m == nil || !strings.HasSuffix(c.Body, m[0]) || c.Author.Typename != "Bot" {
			continue
		}
		// A thread on a range is anchored at its last line.
		r := radar.ResolvedFinding{FindingID: m[1], File: t.Path, Line: t.Line}
		if t.StartLine > 0 && t.StartLine < t.Line {
			r.Line, r.EndLine = t.StartLine, t.Line
		}
		if tm := inlineTitle.FindStringSubmatch(c.Body); tm != nil {
			r.Title = tm[1]
		}
		if t.ResolvedBy != nil {
			r.ResolvedBy = t.ResolvedBy.Login
		}
		// A thread whose resolver is unknown counts as the author's.
		r.ByPRAuthor = r.ResolvedBy == "" || strings.EqualFold(r.ResolvedBy, author)
		out = append(out, r)
	}
	return out
}
