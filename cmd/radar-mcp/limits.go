package main

import (
	"sync"
	"time"
)

const (
	admitted       = "admitted"
	admitRateLimit = "rate_limited"
	admitInFlight  = "user_in_flight"
	admitBusy      = "busy"
	maxIdentities  = 1024
)

// admission decides whether a person may start a review now. Limits are per
// replica: with two replicas a person's effective
// hourly limit is up to twice the configured one, which is enough to stop a
// runaway client without shared state.
type admission struct {
	perHour   int
	perUser   int
	slots     chan struct{}
	now       func() time.Time
	mu        sync.Mutex
	buckets   map[string]*bucket
	inFlights map[string]int
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newAdmission(l limitsConfig) *admission {
	return &admission{
		perHour: l.ReviewsPerHour, perUser: l.InFlightPerUser,
		slots: make(chan struct{}, l.ConcurrentReviews), now: time.Now,
		buckets: map[string]*bucket{}, inFlights: map[string]int{},
	}
}

// admit takes a pod slot, a place among the person's in-flight reviews and
// one of their hourly reviews, all or nothing. release returns the slot and
// the place, and the hourly review too when refund is set: the review was
// never started (another replica had just started it).
func (a *admission) admit(user string) (release func(refund bool), result string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.inFlights[user] >= a.perUser {
		return nil, admitInFlight
	}
	select {
	case a.slots <- struct{}{}:
	default:
		return nil, admitBusy
	}
	now := a.now()
	b := a.buckets[user]
	if b == nil {
		a.prune(now)
		b = &bucket{tokens: float64(a.perHour), at: now}
		a.buckets[user] = b
	}
	b.tokens = min(float64(a.perHour), b.tokens+now.Sub(b.at).Hours()*float64(a.perHour))
	b.at = now
	if b.tokens < 1 {
		<-a.slots
		return nil, admitRateLimit
	}
	b.tokens--
	a.inFlights[user]++
	var once sync.Once
	return func(refund bool) {
		once.Do(func() {
			a.mu.Lock()
			if a.inFlights[user]--; a.inFlights[user] <= 0 {
				delete(a.inFlights, user)
			}
			if refund {
				b.tokens = min(float64(a.perHour), b.tokens+1)
			}
			a.mu.Unlock()
			<-a.slots
		})
	}, admitted
}

// prune forgets people whose bucket has refilled, keeping the map bounded.
func (a *admission) prune(now time.Time) {
	if len(a.buckets) < maxIdentities {
		return
	}
	for user, b := range a.buckets {
		if a.inFlights[user] == 0 && b.tokens+now.Sub(b.at).Hours()*float64(a.perHour) >= float64(a.perHour) {
			delete(a.buckets, user)
		}
	}
}
