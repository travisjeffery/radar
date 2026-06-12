package radar

import (
	"math"
	"sort"
	"strings"
)

// RiskScorer produces a Diff Risk Score (DRS): a raw, unbounded-direction risk
// value where higher means more likely to cause a negative outcome (a
// production incident). It models Meta's proprietary DRS model (paper §2.3);
// the raw value is turned into a percentile by a Calibrator before gating.
type RiskScorer interface {
	// Score returns the raw risk value for a diff. Higher is riskier.
	Score(Diff) float64
}

// HeuristicScorer is a transparent, dependency-free stand-in for Meta's learned
// DRS model. It is NOT a retrained equivalent — it approximates risk from
// observable diff features (size, complexity, risky paths, and the presence of
// ACR risk signals) so the funnel can be exercised end-to-end offline. Swap in
// a real model by implementing RiskScorer.
type HeuristicScorer struct{}

// riskyPathFragments mark areas that historically correlate with incidents.
var riskyPathFragments = []string{
	"auth", "crypto", "secret", "payment", "billing", "migration", "schema",
	"security", "prod", "config",
}

// Score combines diff features into a raw risk value. The weights are
// illustrative; what matters is monotonicity (more lines, higher complexity,
// risky paths, and risk signals all increase risk).
func (HeuristicScorer) Score(d Diff) float64 {
	score := 0.0
	score += float64(d.LinesChanged()) * 0.05
	score += float64(d.MaxComplexity()) * 1.5

	for _, c := range d.Changes {
		lower := strings.ToLower(c.File)
		for _, frag := range riskyPathFragments {
			if strings.Contains(lower, frag) {
				score += 3.0
				break
			}
		}
		for _, sig := range c.Signals {
			if isRiskSignal(sig) {
				score += 5.0
			}
			if isSafeSignal(sig) {
				score -= 0.5
			}
		}
	}
	if score < 0 {
		score = 0
	}
	return score
}

// Calibrator converts a raw DRS value into a percentile using the empirical CDF
// of a calibration sample (paper §2.3: "expressed as a percentile PX"). A diff
// at percentile p is riskier than p% of the calibration sample, so lower p is
// safer. A gate with threshold X passes iff the diff's percentile <= X.
type Calibrator struct {
	// sorted holds the calibration sample in ascending order.
	sorted []float64
}

// NewCalibrator builds a Calibrator from a calibration sample of raw scores. A
// copy is sorted and retained. An empty sample yields a Calibrator that fails
// closed, mapping every score to percentile 100 (nothing passes a DRS gate) —
// callers should provide a representative sample (the replay harness derives
// one from the input set; see also DefaultCalibrationSample).
func NewCalibrator(sample []float64) *Calibrator {
	s := make([]float64, len(sample))
	copy(s, sample)
	sort.Float64s(s)
	return &Calibrator{sorted: s}
}

// Percentile returns the percentile rank (0–100) of raw within the calibration
// sample: the percentage of sample values strictly less than raw. With an empty
// sample it returns 100, treating every diff as maximally risky (fail closed).
func (c *Calibrator) Percentile(raw float64) float64 {
	n := len(c.sorted)
	if n == 0 {
		// Fail closed: with no calibration data the risk gate must block,
		// not wave everything through.
		return 100
	}
	// Number of sample values strictly less than raw.
	below := sort.Search(n, func(i int) bool { return c.sorted[i] >= raw })
	return math.Min(100, float64(below)/float64(n)*100)
}

// NewCalibratorFromDiffs builds a Calibrator by scoring every diff in the set
// with the given scorer. The replay harness uses this to calibrate DRS
// percentiles against the population of diffs being processed (paper §2.3:
// percentile relative to the diff distribution).
func NewCalibratorFromDiffs(s RiskScorer, diffs []Diff) *Calibrator {
	sample := make([]float64, len(diffs))
	for i, d := range diffs {
		sample[i] = s.Score(d)
	}
	return NewCalibrator(sample)
}

// DefaultCalibrationSample returns a synthetic reference distribution of raw DRS
// values for use when classifying a single diff with no population to calibrate
// against. It is skewed toward low-risk values, like a real diff population:
// most diffs are safe, a few are risky.
func DefaultCalibrationSample() []float64 {
	var sample []float64
	// 90 low-risk values in [0,5), 8 medium in [5,15), 2 high in [15,30).
	for i := 0; i < 90; i++ {
		sample = append(sample, float64(i)*5.0/90.0)
	}
	for i := 0; i < 8; i++ {
		sample = append(sample, 5.0+float64(i)*10.0/8.0)
	}
	for i := 0; i < 2; i++ {
		sample = append(sample, 15.0+float64(i)*15.0/2.0)
	}
	return sample
}
