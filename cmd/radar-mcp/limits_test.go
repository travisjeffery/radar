package main

import (
	"testing"
	"time"
)

func TestAdmission(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	a := newAdmission(limitsConfig{ReviewsPerHour: 2, InFlightPerUser: 1, ConcurrentReviews: 2})
	a.now = func() time.Time { return now }

	rel, got := a.admit("a@example.com")
	if got != admitted {
		t.Fatalf("first: %s", got)
	}
	if _, got := a.admit("a@example.com"); got != admitInFlight {
		t.Fatalf("second while first runs: %s, want %s", got, admitInFlight)
	}
	rel(false)
	rel(false) // releasing twice is harmless
	rel2, got := a.admit("a@example.com")
	if got != admitted {
		t.Fatalf("after release: %s", got)
	}
	rel2(false)
	if _, got := a.admit("a@example.com"); got != admitRateLimit {
		t.Fatalf("third in the hour: %s, want %s", got, admitRateLimit)
	}

	now = now.Add(30 * time.Minute) // refills one of two
	rel3, got := a.admit("a@example.com")
	if got != admitted {
		t.Fatalf("after half an hour: %s", got)
	}
	relB, got := a.admit("b@example.com")
	if got != admitted {
		t.Fatalf("another person: %s", got)
	}
	if _, got := a.admit("c@example.com"); got != admitBusy {
		t.Fatalf("third concurrent review on the pod: %s, want %s", got, admitBusy)
	}
	rel3(false)
	relB(false)
	if len(a.slots) != 0 || len(a.inFlights) != 0 {
		t.Fatalf("leaked slots %d, in-flight %v", len(a.slots), a.inFlights)
	}
}
