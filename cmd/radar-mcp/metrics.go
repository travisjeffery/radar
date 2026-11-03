package main

import "github.com/prometheus/client_golang/prometheus"

// metrics carry operation metadata only: tool, outcome and duration, never a
// repository's findings, a token or who asked (that is in the logs).
type metrics struct {
	calls          *prometheus.CounterVec
	callDurations  *prometheus.HistogramVec
	admissions     *prometheus.CounterVec
	reviews        *prometheus.CounterVec
	reviewDuration *prometheus.HistogramVec
	inFlight       prometheus.Gauge
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		calls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "radar_mcp_tool_calls_total", Help: "Radar MCP tool calls by tool and result.",
		}, []string{"tool", "result"}),
		callDurations: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "radar_mcp_tool_call_duration_seconds", Help: "Radar MCP tool call latency.",
			Buckets: []float64{0.1, 0.5, 1, 5, 15, 30, 45, 60},
		}, []string{"tool"}),
		admissions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "radar_mcp_review_admissions_total", Help: "Decisions to start a review, by result.",
		}, []string{"result"}),
		reviews: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "radar_mcp_reviews_total", Help: "Reviews this replica ran, by result (completed, rejected, failed).",
		}, []string{"result"}),
		// Labelled by the reviewer configuration (radar-gh's -agent spec, a
		// handful of values from the config) and outcome, so a review_wait
		// limit and per-reviewer timeouts can be chosen from real durations.
		reviewDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "radar_mcp_review_duration_seconds", Help: "Review latency, start to stored result, by reviewer configuration and result.",
			Buckets: []float64{30, 60, 120, 240, 480, 900, 1800, 2700},
		}, []string{"agent", "result"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "radar_mcp_reviews_in_flight", Help: "Reviews running on this replica.",
		}),
	}
	reg.MustRegister(m.calls, m.callDurations, m.admissions, m.reviews, m.reviewDuration, m.inFlight)
	return m
}
