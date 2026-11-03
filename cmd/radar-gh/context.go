package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/travisjeffery/radar"
)

// issueKey matches a tracker key such as ENG-123 in a title or branch name.
var issueKey = regexp.MustCompile(`(?i)\b([A-Z][A-Z0-9]{1,9}-[0-9]{1,6})\b`)

// fetchPRContext gathers what the reviewers are told about the pull request
// beyond its patches. Every part is best effort: one that cannot be read is
// left out and logged, never failing the review, since the reviewers can
// review without it and the verdict never reads it.
func fetchPRContext(repo string, number int, title, head string) *radar.PullRequestContext {
	ctx := &radar.PullRequestContext{}
	warn := func(what string, err error) {
		fmt.Fprintf(os.Stderr, "radar-gh review: pull request context: %s: %v\n", what, err)
	}
	if msgs, err := fetchCommitMessages(repo, number); err != nil {
		warn("commits", err)
	} else {
		ctx.Commits = msgs
	}
	if threads, err := fetchUnresolvedThreads(repo, number); err != nil {
		warn("review threads", err)
	} else {
		ctx.Threads = threads
	}
	if checks, err := fetchCheckStates(repo, head); err != nil {
		warn("checks", err)
	} else {
		ctx.Checks = checks
	}
	var ref struct {
		Head struct {
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if err := ghJSON(&ref, "api", fmt.Sprintf("repos/%s/pulls/%d", repo, number)); err != nil {
		warn("head branch", err)
	}
	if key := linkedIssueKey(title, ref.Head.Ref); key != "" {
		if issue, err := fetchLinearIssue(key); err != nil {
			warn("linked issue "+key, err)
		} else {
			ctx.LinkedIssue = issue
		}
	}
	return ctx
}

func linkedIssueKey(title, branch string) string {
	for _, s := range []string{title, branch} {
		if m := issueKey.FindStringSubmatch(s); m != nil {
			return strings.ToUpper(m[1])
		}
	}
	return ""
}

// maxContextThreads is how many unresolved threads the context keeps.
const maxContextThreads = 40

// fetchUnresolvedThreads returns each unresolved review thread's location,
// opening comment and first replies, reading every page until it has maxContextThreads.
func fetchUnresolvedThreads(repo string, number int) ([]radar.ReviewThreadState, error) {
	owner, name, _ := strings.Cut(repo, "/")
	const query = `query($owner:String!,$name:String!,$number:Int!,$cursor:String){repository(owner:$owner,name:$name){pullRequest(number:$number){reviewThreads(first:100,after:$cursor){nodes{isResolved path line comments(first:10){nodes{author{login} body}}} pageInfo{hasNextPage endCursor}}}}}`
	var out []radar.ReviewThreadState
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
							Nodes []struct {
								IsResolved bool   `json:"isResolved"`
								Path       string `json:"path"`
								Line       int    `json:"line"`
								Comments   struct {
									Nodes []struct {
										Author struct {
											Login string `json:"login"`
										} `json:"author"`
										Body string `json:"body"`
									} `json:"nodes"`
								} `json:"comments"`
							} `json:"nodes"`
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
		for _, t := range threads.Nodes {
			if t.IsResolved || len(t.Comments.Nodes) == 0 {
				continue
			}
			c := t.Comments.Nodes[0]
			out = append(out, radar.ReviewThreadState{Path: t.Path, Line: t.Line, Author: c.Author.Login, Body: threadBody(t.Comments.Nodes)})
			if len(out) >= maxContextThreads {
				return out, nil
			}
		}
		if !threads.PageInfo.HasNextPage || threads.PageInfo.EndCursor == "" || threads.PageInfo.EndCursor == cursor {
			return out, nil
		}
		cursor = threads.PageInfo.EndCursor
	}
}

// threadBody is a thread's opening comment and then its replies, so the
// reviewers see what people answered, /radar clarifications included.
// Radar's hidden markers are dropped. Each thread is capped downstream, so
// the opening comment is shortened and the newest replies are kept first.
func threadBody(comments []struct {
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	Body string `json:"body"`
}) string {
	const openingChars, replyChars, repliesChars = 400, 350, 1000
	var replies []string
	used := 0
	for i := len(comments) - 1; i >= 1; i-- {
		r := fmt.Sprintf("\n(reply from %s): %s", comments[i].Author.Login, clipRunes(oneLineText(stripMarkers(comments[i].Body)), replyChars))
		if used+len(r) > repliesChars {
			break
		}
		used += len(r)
		replies = append([]string{r}, replies...)
	}
	if skipped := len(comments) - 1 - len(replies); skipped > 0 {
		replies = append([]string{fmt.Sprintf("\n(%d earlier replies omitted)", skipped)}, replies...)
	}
	return clipRunes(stripMarkers(comments[0].Body), openingChars) + strings.Join(replies, "")
}

// fetchCheckStates lists the head's check runs and commit statuses, latest
// per producer: check runs keyed by app and name, statuses by context, so a
// same-named check from another app or a legacy status never hides a failure.
func fetchCheckStates(repo, sha string) ([]radar.CheckState, error) {
	var runs struct {
		CheckRuns []githubCheckRun `json:"check_runs"`
	}
	if err := ghJSON(&runs, "api", fmt.Sprintf("repos/%s/commits/%s/check-runs?per_page=100", repo, sha)); err != nil {
		return nil, err
	}
	var statuses []githubStatus
	if err := ghJSON(&statuses, "api", fmt.Sprintf("repos/%s/commits/%s/statuses?per_page=100", repo, sha)); err != nil {
		return nil, err
	}
	return checkStates(runs.CheckRuns, statuses), nil
}

func checkStates(runs []githubCheckRun, statuses []githubStatus) []radar.CheckState {
	type entry struct {
		id    int64
		state radar.CheckState
	}
	latest := map[string]entry{}
	var order []string
	add := func(key, name, state string, id int64) {
		prior, ok := latest[key]
		if !ok {
			order = append(order, key)
		} else if prior.id > id {
			return
		}
		latest[key] = entry{id: id, state: radar.CheckState{Name: name, State: state}}
	}
	for _, r := range runs {
		state := strings.ToLower(r.Status)
		if r.Conclusion != "" {
			state = strings.ToLower(r.Conclusion)
		}
		name := r.Name
		if r.App.Slug != "" && r.App.Slug != "github-actions" {
			name = r.App.Slug + "/" + r.Name
		}
		add("check:"+r.App.Slug+"/"+r.Name, name, state, r.ID)
	}
	for _, s := range statuses {
		add("status:"+s.Context, s.Context+" (status)", strings.ToLower(s.State), s.ID)
	}
	out := make([]radar.CheckState, 0, len(order))
	for _, k := range order {
		out = append(out, latest[k].state)
	}
	return out
}

// linearGraphQLURL is Linear's API; tests point it elsewhere.
var linearGraphQLURL = "https://api.linear.app/graphql"

// fetchLinearIssue reads the issue's title and description with
// $LINEAR_API_KEY. Without the key it returns an error and the review goes
// on without the issue.
func fetchLinearIssue(key string) (*radar.LinkedIssue, error) {
	apiKey := os.Getenv("LINEAR_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("LINEAR_API_KEY is not set")
	}
	body, _ := json.Marshal(map[string]any{
		"query":     `query($id:String!){issue(id:$id){identifier title description}}`,
		"variables": map[string]string{"id": key},
	})
	req, err := http.NewRequest(http.MethodPost, linearGraphQLURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", apiKey)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Data struct {
			Issue *struct {
				Identifier  string `json:"identifier"`
				Title       string `json:"title"`
				Description string `json:"description"`
			} `json:"issue"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("status %d: %w", resp.StatusCode, err)
	}
	if len(out.Errors) > 0 {
		return nil, fmt.Errorf("%s", out.Errors[0].Message)
	}
	if out.Data.Issue == nil {
		return nil, fmt.Errorf("not found")
	}
	return &radar.LinkedIssue{Key: out.Data.Issue.Identifier, Title: out.Data.Issue.Title, Summary: out.Data.Issue.Description}, nil
}

func oneLineText(s string) string { return strings.Join(strings.Fields(s), " ") }
