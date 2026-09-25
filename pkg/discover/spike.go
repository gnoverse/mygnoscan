package discover

import (
	"math"
	"sort"
)

// The spike detector: is today unusual for this subject, and unusual enough to
// tell somebody about.
//
// Three gates, and they are complementary rather than belt-and-braces. On the
// calibration series each one stops a different day, and dropping any of them
// changes the answer:
//
//   z >= 3.5          is this statistically unusual
//   x - m >= F(kind)  is the excess big enough to be worth a sentence
//   x >= 2 * m        can you honestly write "twice its usual"
//
// The statistics are median and MAD, never mean and standard deviation. What
// you are detecting is inside the window you measure against: on the real
// mainnet series, 09-17's baseline contains 09-16's 400, which drags the mean
// to 101.8 and the standard deviation to 168, so a plain z-score answers "is 87
// unusual" with a baseline the anomaly already poisoned. The median absorbs it.
//
// 0.6745 and 1.253314 are the Iglewicz-Hoaglin constants that make MAD and the
// mean absolute deviation consistent estimators of sigma for a normal
// distribution, which is what lets `z >= 3.5` keep the conventional meaning it
// has in the literature instead of being a number somebody liked.

const (
	// SpikeWindow is the baseline, in days.
	SpikeWindow = 7
	// SpikeMinBaseline is the fewest baseline days that produce a verdict at
	// all. Below it the answer is "warm-up", never "quiet" and never a spike.
	SpikeMinBaseline = 3
	// SpikeZ is the statistical gate.
	SpikeZ = 3.5
	// SpikeRatio is the headline gate. A tight baseline with a big absolute
	// excess can clear the other two gates on a 22% move, and you cannot write
	// "twice its usual" about a 22% move.
	SpikeRatio = 2.0
)

// SpikeFloors is F(kind): the minimum excess over the baseline median.
//
// These are the only hand-set numbers in the detector. Measured on mainnet
// 2026-09-21. Re-derive one when that series' 28-day median passes 10 * F; the
// point of the comment is that the next reader can tell when it was last true.
//
//	chain.spike    100  the middle of a stable 75-to-200 plateau (see the test);
//	                    mainnet's typical day is 62 to 87 new addresses
//	package.spike   25  r/gov/dao's real traffic is 0 to 6 calls a day, so
//	                    anything lower makes one governance vote a "spike"
var SpikeFloors = map[string]float64{
	"chain.spike":   100,
	"package.spike": 25,
}

// Point is one day of a daily series. Day is an RFC3339 date, "2026-09-18".
type Point struct {
	Day string  `json:"day"`
	X   float64 `json:"x"`
}

// Spike is the verdict for one day, with every intermediate value it was
// reached through.
//
// The intermediates are returned rather than discarded because they are the
// facts layer 2 and 3 are written from (§10.2): "603 new wallets, against a
// normal day of 46" is grounded only if the median is a value the detector
// actually handed over.
type Spike struct {
	Day    string  `json:"day"`
	X      float64 `json:"x"`
	N      int     `json:"baseline_days"`
	Median float64 `json:"baseline_median"`
	MAD    float64 `json:"mad"`
	Z      float64 `json:"z"`
	Excess float64 `json:"excess"`
	Ratio  float64 `json:"ratio"`
	Fired  bool    `json:"fired"`
	// Why names the gate that stopped it, or "fire". Worth carrying: "quiet"
	// with no reason is the kind of output nobody can debug six weeks later.
	Why string `json:"why"`
}

// Detect runs the rule over a daily series.
//
// start is the subject's own first-live day, and clipping the baseline to it is
// the part of this that everybody gets wrong. Days before a package existed are
// not quiet days, they are absence; counting them as zeros makes the day a
// realm goes live fire against a baseline of nothing, and the day after fire
// again against a baseline of one. On the real wugnot series that is two false
// spikes on consecutive days for an activation that should be reported once, as
// package.first_call, which is both the honest description and the better
// headline. Pass an empty start to measure against the whole series.
func Detect(series []Point, floor float64, start string) []Spike {
	out := make([]Spike, 0, len(series))
	for i, p := range series {
		var baseline []float64
		for j := i - 1; j >= 0 && j >= i-SpikeWindow; j-- {
			if start != "" && series[j].Day < start {
				continue
			}
			baseline = append(baseline, series[j].X)
		}

		s := Spike{Day: p.Day, X: p.X, N: len(baseline)}
		if len(baseline) < SpikeMinBaseline {
			s.Why = "warm-up"
			out = append(out, s)
			continue
		}

		s.Median = median(baseline)
		dev := make([]float64, len(baseline))
		for k, b := range baseline {
			dev[k] = math.Abs(b - s.Median)
		}
		s.MAD = median(dev)

		// The MeanAD fallback is not a nicety. MAD goes to zero constantly on a
		// quiet chain: six days of 0 gives MAD = 0, and (x - 0) / 0 is not a
		// detector, it is a divide by zero that fires on every non-zero day
		// forever after.
		var sigma float64
		switch {
		case s.MAD > 0:
			sigma = s.MAD / 0.6745
		default:
			if mean := meanOf(dev); mean > 0 {
				sigma = 1.253314 * mean
			}
		}

		s.Excess = p.X - s.Median
		switch {
		case sigma > 0:
			s.Z = s.Excess / sigma
		case p.X > s.Median:
			s.Z = math.Inf(1)
		}
		if s.Median != 0 {
			s.Ratio = p.X / s.Median
		}

		switch {
		case s.Z < SpikeZ:
			s.Why = "statistical"
		case s.Excess < floor:
			s.Why = "absolute"
		// Vacuous at a zero median, which is deliberate: "twice nothing" is not
		// a claim the headline gate has any opinion about, and the absolute gate
		// is what carries that case.
		case s.Median > 0 && p.X < SpikeRatio*s.Median:
			s.Why = "headline"
		default:
			s.Fired, s.Why = true, "fire"
		}
		out = append(out, s)
	}
	return out
}

// Fired is the days that fired, which is usually all a caller wants.
func Fired(verdicts []Spike) []string {
	var out []string
	for _, v := range verdicts {
		if v.Fired {
			out = append(out, v.Day)
		}
	}
	return out
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}

func meanOf(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}
