package radar

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
)

// ReviewRecord holds the verdicts the review agents returned for one pull
// request head, so a later evaluation of the same head reuses them instead of
// calling the agents again. The findings stage writes it before checks
// finish; the verdict stage reads it once they have, so both rest on the same
// reviews.
type ReviewRecord struct {
	SchemaVersion int    `json:"schema_version"`
	HeadSHA       string `json:"head_sha"`
	// Reviews are keyed by RequestKey: the agent and the exact request it
	// was sent, so a verdict is reused only for a byte-identical request.
	Reviews []RecordedReview `json:"reviews"`
}

// RecordedReview is one agent's verdict on one request.
type RecordedReview struct {
	Reviewer string    `json:"reviewer"`
	Request  string    `json:"request"`
	Result   ACRResult `json:"result"`
}

const reviewRecordSchemaVersion = 1

// RequestKey identifies what an agent was asked: its provenance, the system
// prompt and the rendered diff. Any change to the policy's criteria, the
// chunking, the withheld files or the patches changes the key.
func RequestKey(reviewer string, d Diff) string {
	// The pull request context (threads, check states) changes between the
	// findings and verdict stages; the review is of the patches, so the key
	// leaves it out and the verdict reuses the findings stage's review.
	d.PRContext = ""
	h := sha256.New()
	for _, part := range []string{reviewer, ReviewSystemPrompt(d), renderDiffForReview(d)} {
		fmt.Fprintf(h, "%d:%s", len(part), part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// promptSHA256 hashes a request prefix for ReviewUsage.PrefixSHA256.
func promptSHA256(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:%s", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// reviewMemo records each agent's verdicts and answers repeated requests
// from them.
type reviewMemo struct {
	// recordedOnly answers a request no record holds with a failed review
	// instead of calling the agent (see RequireRecordedReviews).
	recordedOnly bool
	mu           sync.Mutex
	byKey        map[string]ACRResult
	order        []RecordedReview
	reused       int
	// prior holds an earlier head's recorded reviews (UsePriorReviewRecord).
	// One is used only when a request at this head is byte-identical to it,
	// and joins this head's record only then.
	prior     map[string]RecordedReview
	priorHead string
	// reusedFrom is priorHead once a review has been reused from it.
	reusedFrom string
}

func (m *reviewMemo) lookup(key string) (ACRResult, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	res, ok := m.byKey[key]
	if !ok {
		var review RecordedReview
		if review, ok = m.prior[key]; ok {
			res = review.Result
			m.byKey[key] = res
			m.order = append(m.order, review)
			m.reusedFrom = m.priorHead
		}
	}
	if ok {
		m.reused++
	}
	return res, ok
}

// store keeps only complete verdicts. A failure (no reviewed files, as every
// fail-safe result has) or a response cut off at the output limit is left
// out, so the next stage tries again instead of inheriting a timeout, a
// provider error or a truncation.
func (m *reviewMemo) store(reviewer, key string, res ACRResult) {
	if res.Failed || res.Truncated || len(res.ReviewedFiles) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byKey[key]; ok {
		return
	}
	m.byKey[key] = res
	m.order = append(m.order, RecordedReview{Reviewer: reviewer, Request: key, Result: res})
}

// memoAgent answers from the memo when it can and records what the wrapped
// agent returns otherwise.
type memoAgent struct {
	agent ReviewAgent
	name  string
	memo  *reviewMemo
}

// ReviewVariantAgent is implemented by agents whose requests differ by mode
// beyond the prompt and diff, such as with or without repository tools; the
// variant is part of a recorded review's key.
type ReviewVariantAgent interface {
	ReviewVariant() string
}

func (a memoAgent) Review(d Diff) ACRResult {
	name := a.name
	if v, ok := a.agent.(ReviewVariantAgent); ok && v.ReviewVariant() != "" {
		name += "\x00" + v.ReviewVariant()
	}
	key := RequestKey(name, d)
	if res, ok := a.memo.lookup(key); ok {
		// Reused, it cost nothing now; the record keeps what it cost then.
		res.Usage = &ReviewUsage{ReusedRequests: 1}
		return res
	}
	if a.memo.recordedOnly {
		return ACRResult{Summary: "not reviewed at the findings stage; no new review was requested", Failed: true}
	}
	res := a.agent.Review(d)
	if res.Usage != nil {
		// A copy: the agent may hand the same usage to later results.
		u := *res.Usage
		u.CostUSD, u.Unpriced = 0, false
		if cost, ok := EstimateReviewCostUSD(a.name, &u); ok {
			u.CostUSD = cost
		} else {
			u.Unpriced = true
		}
		res.Usage = &u
	}
	a.memo.store(a.name, key, res)
	return res
}

func (a memoAgent) ConcurrentReviewSafe() bool { return concurrentReviewSafe(a.agent) }

func (a memoAgent) Describe() string { return a.name }

// UseReviewRecord primes the reviewer with recorded verdicts. A record for a
// different head is an error: the caller asked for the wrong record.
func (r *PullRequestReviewer) UseReviewRecord(rec ReviewRecord, head string) error {
	if err := checkReviewRecord(rec, head); err != nil {
		return err
	}
	r.memo.mu.Lock()
	defer r.memo.mu.Unlock()
	for _, review := range rec.Reviews {
		if _, ok := r.memo.byKey[review.Request]; ok {
			continue
		}
		r.memo.byKey[review.Request] = review.Result
		r.memo.order = append(r.memo.order, review)
	}
	return nil
}

// UsePriorReviewRecord offers the reviews recorded for an earlier head of the
// same pull request. A later push often leaves a reviewer's
// request byte-identical, as when the base branch is merged in and the
// pull request's own patches do not change; such a request is answered from
// the earlier review instead of a new model call. Any other request is
// reviewed anew. rec must be for priorHead, which must not be head: the
// caller binds the record to a trusted run of that earlier head, as it binds
// UseReviewRecord's to this one.
func (r *PullRequestReviewer) UsePriorReviewRecord(rec ReviewRecord, priorHead, head string) error {
	if priorHead == "" || priorHead == head {
		return fmt.Errorf("prior head %q is not an earlier head than %q", priorHead, head)
	}
	if err := checkReviewRecord(rec, priorHead); err != nil {
		return err
	}
	prior := make(map[string]RecordedReview, len(rec.Reviews))
	for _, review := range rec.Reviews {
		prior[review.Request] = review
	}
	r.memo.mu.Lock()
	defer r.memo.mu.Unlock()
	r.memo.prior, r.memo.priorHead = prior, priorHead
	return nil
}

// checkReviewRecord validates every entry before any is used, so a record
// rejected here leaves nothing behind.
func checkReviewRecord(rec ReviewRecord, head string) error {
	if rec.SchemaVersion != reviewRecordSchemaVersion {
		return fmt.Errorf("review record schema version %d, want %d", rec.SchemaVersion, reviewRecordSchemaVersion)
	}
	if rec.HeadSHA == "" || rec.HeadSHA != head {
		return fmt.Errorf("review record is for head %q, not %q", rec.HeadSHA, head)
	}
	for _, review := range rec.Reviews {
		if review.Request == "" || review.Reviewer == "" {
			return fmt.Errorf("review record entry without reviewer or request key")
		}
		if review.Result.Failed || review.Result.Truncated || len(review.Result.ReviewedFiles) == 0 {
			return fmt.Errorf("review record holds an incomplete review from %s", review.Reviewer)
		}
	}
	return nil
}

// RequireRecordedReviews makes the reviewer answer only from recorded
// reviews: a request no record holds fails instead of calling an agent. A
// clarification re-checks findings already posted and must not pay for a
// new review.
func (r *PullRequestReviewer) RequireRecordedReviews() { r.memo.recordedOnly = true }

// ReviewRecord returns every verdict recorded or reused so far, bound to head.
func (r *PullRequestReviewer) ReviewRecord(head string) ReviewRecord {
	r.memo.mu.Lock()
	defer r.memo.mu.Unlock()
	return ReviewRecord{
		SchemaVersion: reviewRecordSchemaVersion,
		HeadSHA:       head,
		Reviews:       append([]RecordedReview{}, r.memo.order...),
	}
}
