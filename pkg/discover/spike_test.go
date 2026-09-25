package discover

import (
	"math"
	"reflect"
	"testing"
)

// mainnet daily new addresses, re-verified 2026-09-21 against
// /api/timeseries/new-addresses?network=mainnet&days=21. The chain's own first
// indexed day is 09-12, which is what the baseline clips to.
var newAddresses = []Point{
	{"2026-09-12", 10}, {"2026-09-13", 7}, {"2026-09-14", 29}, {"2026-09-15", 63},
	{"2026-09-16", 400}, {"2026-09-17", 87}, {"2026-09-18", 603}, {"2026-09-19", 62},
	{"2026-09-20", 76}, {"2026-09-21", 68},
}

const chainStart = "2026-09-12"

func byDay(vs []Spike) map[string]Spike {
	out := map[string]Spike{}
	for _, v := range vs {
		out[v.Day] = v
	}
	return out
}

func close(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

// Test 1: the whole calibration table, value by value.
//
// Asserting only the verdicts would pass on a detector that got the right
// answer for the wrong reason, and every one of these intermediates is a fact
// an explanation is later written from: "603, against a normal day of 46" is
// grounded only if 46 is a number the detector produced.
func TestTheCalibrationTable(t *testing.T) {
	want := []struct {
		day            string
		n              int
		median, mad, z float64
		excess, ratio  float64
		fired          bool
		why            string
	}{
		{"2026-09-12", 0, 0, 0, 0, 0, 0, false, "warm-up"},
		{"2026-09-13", 1, 0, 0, 0, 0, 0, false, "warm-up"},
		{"2026-09-14", 2, 0, 0, 0, 0, 0, false, "warm-up"},
		{"2026-09-15", 3, 10.0, 3.0, 11.92, 53.0, 6.30, false, "absolute"},
		{"2026-09-16", 4, 19.5, 11.0, 23.33, 380.5, 20.51, true, "fire"},
		{"2026-09-17", 5, 29.0, 22.0, 1.78, 58.0, 3.00, false, "statistical"},
		{"2026-09-18", 6, 46.0, 37.5, 10.02, 557.0, 13.11, true, "fire"},
		{"2026-09-19", 7, 63.0, 53.0, -0.01, -1.0, 0.98, false, "statistical"},
		{"2026-09-20", 7, 63.0, 34.0, 0.26, 13.0, 1.21, false, "statistical"},
		{"2026-09-21", 7, 76.0, 14.0, -0.39, -8.0, 0.89, false, "statistical"},
	}

	got := byDay(Detect(newAddresses, SpikeFloors["chain.spike"], chainStart))
	for _, w := range want {
		t.Run(w.day, func(t *testing.T) {
			g, ok := got[w.day]
			if !ok {
				t.Fatalf("no verdict for %s", w.day)
			}
			if g.N != w.n {
				t.Errorf("baseline days = %d, want %d", g.N, w.n)
			}
			if w.why == "warm-up" {
				if g.Why != "warm-up" || g.Fired {
					t.Errorf("got %q fired=%v, want warm-up", g.Why, g.Fired)
				}
				return
			}
			if !close(g.Median, w.median, 0.001) {
				t.Errorf("median = %v, want %v", g.Median, w.median)
			}
			if !close(g.MAD, w.mad, 0.001) {
				t.Errorf("MAD = %v, want %v", g.MAD, w.mad)
			}
			if !close(g.Z, w.z, 0.01) {
				t.Errorf("z = %.4f, want %v", g.Z, w.z)
			}
			if !close(g.Excess, w.excess, 0.001) {
				t.Errorf("excess = %v, want %v", g.Excess, w.excess)
			}
			if !close(g.Ratio, w.ratio, 0.01) {
				t.Errorf("ratio = %.4f, want %v", g.Ratio, w.ratio)
			}
			if g.Fired != w.fired || g.Why != w.why {
				t.Errorf("fired=%v why=%q, want fired=%v why=%q", g.Fired, g.Why, w.fired, w.why)
			}
		})
	}
}

// Test 2: the floor sweep, which is what separates a calibration from an
// overfit.
//
// If the right answer held only at F = 100 the number would have been chosen to
// make the fixture pass. It holds across 75 to 200, so F = 100 sits in the
// middle of a plateau rather than on a knife edge. The low end is asserted too,
// because a detector that never changed its answer would pass the plateau check
// vacuously.
func TestTheFloorIsAPlateauNotAKnifeEdge(t *testing.T) {
	for _, f := range []float64{75, 100, 150, 200} {
		got := Fired(Detect(newAddresses, f, chainStart))
		want := []string{"2026-09-16", "2026-09-18"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("F=%v fired %v, want %v", f, got, want)
		}
	}
	for _, f := range []float64{0, 25, 50} {
		got := Fired(Detect(newAddresses, f, chainStart))
		want := []string{"2026-09-15", "2026-09-16", "2026-09-18"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("F=%v fired %v, want %v: below the plateau 09-15 should also fire", f, got, want)
		}
	}
}

// Test 3: the clipping rule, on the real series that exposed it.
//
// This is the regression test for the rule most likely to be simplified away
// later, because unclipped code is shorter and looks equivalent until a realm
// goes live.
func TestClippingToTheSubjectsFirstLiveDay(t *testing.T) {
	// r/gnoland/wugnot daily calls. The package's first call is 09-16; the six
	// days before it are absence, not zeros.
	wugnot := []Point{
		{"2026-09-09", 0}, {"2026-09-10", 0}, {"2026-09-11", 0}, {"2026-09-12", 0},
		{"2026-09-13", 0}, {"2026-09-14", 0}, {"2026-09-15", 0},
		{"2026-09-16", 633}, {"2026-09-17", 714}, {"2026-09-18", 769},
		{"2026-09-19", 355}, {"2026-09-20", 570}, {"2026-09-21", 472},
	}
	floor := SpikeFloors["package.spike"]

	if got := Fired(Detect(wugnot, floor, "2026-09-16")); len(got) != 0 {
		t.Errorf("clipped fired %v, want none: one realm going live is not a spike", got)
	}
	// Unclipped, the same series produces two false spikes on consecutive days,
	// which is the bug stated as a test rather than as a comment.
	unclipped := Fired(Detect(wugnot, floor, ""))
	want := []string{"2026-09-16", "2026-09-17"}
	if !reflect.DeepEqual(unclipped, want) {
		t.Errorf("unclipped fired %v, want %v", unclipped, want)
	}

	// And the clipped verdicts are quiet on the evidence, not warm-up forever.
	clipped := byDay(Detect(wugnot, floor, "2026-09-16"))
	if v := clipped["2026-09-19"]; !close(v.Z, -4.40, 0.01) || v.Why != "statistical" {
		t.Errorf("09-19 = z %.2f why %q, want z -4.40 and a statistical verdict", v.Z, v.Why)
	}
}

// Test 4: a perfectly flat baseline. MAD is zero, and dividing by it is not a
// detector, it is a divide by zero that fires on every non-zero day forever.
func TestAFlatBaselineDoesNotFireOnTheFirstBlip(t *testing.T) {
	flat := []Point{
		{"2026-09-01", 0}, {"2026-09-02", 0}, {"2026-09-03", 0},
		{"2026-09-04", 0}, {"2026-09-05", 0}, {"2026-09-06", 0},
		{"2026-09-07", 5},
	}
	got := byDay(Detect(flat, SpikeFloors["package.spike"], ""))["2026-09-07"]
	if got.Fired {
		t.Errorf("fired on a 5 against a flat zero baseline: z=%v why=%q", got.Z, got.Why)
	}
	if !math.IsInf(got.Z, 1) {
		t.Errorf("z = %v, want +Inf: with no spread and x above the median there is no finite answer", got.Z)
	}
	if got.Why != "absolute" {
		t.Errorf("stopped by %q, want the absolute gate to be what holds this case", got.Why)
	}
}

// Test 5: the day after a spike must not fire on the spike's own shadow. This
// is the whole reason the detector is built on a median.
func TestTheDayAfterASpikeDoesNotFireOnIt(t *testing.T) {
	got := byDay(Detect(newAddresses, SpikeFloors["chain.spike"], chainStart))["2026-09-17"]
	if got.Fired {
		t.Error("09-17 fired; 87 is a normal day sitting next to a 400")
	}
	if got.Why != "statistical" {
		t.Errorf("stopped by %q, want statistical", got.Why)
	}
	// With a mean and a standard deviation instead, the baseline is poisoned by
	// the 400 it contains: mean 101.8, sd 168. The median answers 29.
	if !close(got.Median, 29, 0.001) {
		t.Errorf("baseline median = %v, want 29: the median is what absorbs the spike inside the window", got.Median)
	}
}
