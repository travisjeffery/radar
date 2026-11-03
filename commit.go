package radar

import (
	"fmt"
	"strings"
)

// StageCommit marks a review of a pushed commit that has no pull request. It
// runs the verdict's reviewers, criteria and decision, skips the gates only a
// pull request can satisfy, and is report-only: its action is at most
// would-approve, whatever the policy's mode.
const StageCommit = "commit"

// CommitInput is a pushed commit reviewed as if it were a pull request into
// BaseRef. Adapters fetch it themselves, never from the caller: Files are the
// changes from the merge base of BaseSHA and HeadSHA to HeadSHA, exactly the
// files a pull request from HeadSHA into BaseRef would show.
type CommitInput struct {
	Repository string `json:"repository"`
	// BaseRef is the branch the commit would merge into.
	BaseRef string `json:"base_ref"`
	// BaseSHA is BaseRef's tip when the commit was reviewed: the trusted
	// commit the policy, rules and guidance were read from, as CI reads them
	// from the default branch.
	BaseSHA string `json:"base_sha"`
	// MergeBaseSHA is where HeadSHA's changes start.
	MergeBaseSHA string `json:"merge_base_sha"`
	HeadSHA      string `json:"head_sha"`
	// Author is the head commit's author login, when the provider knows it.
	Author string `json:"author,omitempty"`
	// Commits are the commit messages from the merge base to HeadSHA, oldest
	// first, so the last is HeadSHA's. With no pull request title or
	// description, they tell the reviewers what the change intends, and the
	// deny phrases are matched against them.
	Commits []string          `json:"commits,omitempty"`
	Files   []PullRequestFile `json:"files"`
}

// ID names the reviewed commit, as owner/repo@sha.
func (in CommitInput) ID() string {
	return in.Repository + "@" + in.HeadSHA
}

// PullRequestInput is the input the commit is reviewed as: the same files and
// base as a pull request, its commit messages as the only context, and none
// of the state a pull request carries (checks, threads, labels, title).
func (in CommitInput) PullRequestInput() PullRequestInput {
	var context *PullRequestContext
	if len(in.Commits) > 0 {
		context = &PullRequestContext{Commits: in.Commits}
	}
	return PullRequestInput{
		ID: in.ID(), Repository: in.Repository, BaseRef: in.BaseRef, HeadSHA: in.HeadSHA,
		Author: in.Author, Files: in.Files, Context: context,
	}
}

// ReviewCommit reviews a pushed commit with the same reviewers, criteria and
// decision as a pull request's verdict. It never mutates a provider and
// never approves.
func (r *PullRequestReviewer) ReviewCommit(in CommitInput) PullRequestReview {
	out, _ := r.evaluate(in.PullRequestInput(), StageCommit)
	return out
}

// commitStateGate keeps only the gates a commit can meet: it must name its
// head and merge into an allowed base branch. Checks, approvals, review
// threads, drafts and labels belong to a pull request.
func commitStateGate(policy PullRequestPolicy, in PullRequestInput) (bool, string) {
	switch {
	case strings.TrimSpace(in.ID) == "" || strings.TrimSpace(in.HeadSHA) == "":
		return false, "request id and head SHA are required"
	case !matchesAnyPath(in.BaseRef, policy.AllowedBaseBranches):
		return false, "base branch is not allowed"
	}
	return true, fmt.Sprintf("commit reviewed against %s; report-only, so pull request checks, approvals and review threads do not apply", in.BaseRef)
}
