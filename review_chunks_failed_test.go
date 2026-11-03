package radar

import "testing"

func TestMergedReviewIsFailedWhenAnyPartFailed(t *testing.T) {
	ok := ACRResult{Confidence: 9, ReviewedFiles: []string{"a.go"}, Summary: "fine"}
	failed := ACRResult{Summary: "ACR Fireworks error, failing safe: timeout", Failed: true}
	if got := mergeVerdicts([]ACRResult{ok, failed}, func(int) string { return "part" }); !got.Failed {
		t.Fatal("a merged review with a failed part must be failed")
	}
	if got := mergeVerdicts([]ACRResult{ok, ok}, func(int) string { return "part" }); got.Failed {
		t.Fatal("a merged review of finished parts must not be failed")
	}
	if _, err := chunkDiff(Diff{Changes: []Change{{File: "big", Content: string(make([]byte, 100))}}}, 10); err == nil {
		t.Fatal("expected an over-budget file")
	}
	if got := reviewInChunks(RuleBasedAgent{}, Diff{Changes: []Change{{File: "big", Content: string(make([]byte, 100))}}}, 10); !got.Failed {
		t.Fatal("a diff that cannot be sent is a failed review")
	}
}

// Usage keeps the first request's split and the prefix hash when requests
// and chunk parts are summed (measurement only).
func TestUsageKeepsTheFirstRequest(t *testing.T) {
	var total *ReviewUsage
	total = total.Add(&ReviewUsage{InputTokens: 100, CacheReadTokens: 90, Requests: 1, PrefixSHA256: "p"})
	total = total.Add(&ReviewUsage{InputTokens: 300, CacheWriteTokens: 200, Requests: 1, PrefixSHA256: "p"})
	if total.FirstRequest == nil || *total.FirstRequest != (RequestUsage{InputTokens: 100, CacheReadTokens: 90}) {
		t.Fatalf("first request = %+v", total.FirstRequest)
	}
	if total.InputTokens != 400 || total.Requests != 2 || total.PrefixSHA256 != "p" {
		t.Fatalf("total = %+v", total)
	}
	// A later part's own first request does not replace the first part's.
	merged := (&ReviewUsage{}).Add(total).Add(&ReviewUsage{Requests: 3, FirstRequest: &RequestUsage{InputTokens: 7}})
	if merged.FirstRequest.InputTokens != 100 || merged.Requests != 5 {
		t.Fatalf("merged = %+v first=%+v", merged, merged.FirstRequest)
	}
}
