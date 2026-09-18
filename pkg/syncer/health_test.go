package syncer

import (
	"errors"
	"testing"
	"time"
)

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestRegistryRecord(t *testing.T) {
	base := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name                string
		errs                []error
		wantHealthy         bool
		wantConsecutive     int
		wantFailures        int
		wantPasses          int
		wantLastError       string
		wantSuccessRecorded bool
	}{
		{
			name: "a network with no passes is not reported as broken",
			// Sync can be switched off entirely (-sync=false), and a process
			// that has not finished its first pass should not raise an alarm.
			errs: nil, wantHealthy: false, wantPasses: 0,
		},
		{
			name: "one success", errs: []error{nil},
			wantHealthy: true, wantPasses: 1, wantSuccessRecorded: true,
		},
		{
			name: "one failure", errs: []error{errors.New("422")},
			wantHealthy: false, wantConsecutive: 1, wantFailures: 1, wantPasses: 1,
			wantLastError: "422",
		},
		{
			name: "failures accumulate", errs: []error{errors.New("a"), errors.New("b")},
			wantHealthy: false, wantConsecutive: 2, wantFailures: 2, wantPasses: 2,
			wantLastError: "b",
		},
		{
			name: "a success clears the streak but keeps the history",
			errs: []error{errors.New("boom"), nil},
			// The message survives on purpose: a network alternating between
			// success and failure is the case most worth seeing, and wiping
			// the text on every success hides it.
			wantHealthy: true, wantConsecutive: 0, wantFailures: 1, wantPasses: 2,
			wantLastError: "boom", wantSuccessRecorded: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRegistry()
			r.now = fixedClock(base)
			for _, err := range tt.errs {
				r.Record("mainnet", err)
			}

			h := r.Snapshot()["mainnet"]
			if h.Healthy != tt.wantHealthy {
				t.Errorf("healthy = %v, want %v", h.Healthy, tt.wantHealthy)
			}
			if h.ConsecutiveFailures != tt.wantConsecutive {
				t.Errorf("consecutive = %d, want %d", h.ConsecutiveFailures, tt.wantConsecutive)
			}
			if h.Failures != tt.wantFailures {
				t.Errorf("failures = %d, want %d", h.Failures, tt.wantFailures)
			}
			if h.Passes != tt.wantPasses {
				t.Errorf("passes = %d, want %d", h.Passes, tt.wantPasses)
			}
			if h.LastError != tt.wantLastError {
				t.Errorf("last error = %q, want %q", h.LastError, tt.wantLastError)
			}
			if (h.LastSuccessAt != "") != tt.wantSuccessRecorded {
				t.Errorf("last success = %q, want recorded=%v", h.LastSuccessAt, tt.wantSuccessRecorded)
			}
		})
	}
}

func TestRegistryKeepsNetworksApart(t *testing.T) {
	r := NewRegistry()
	r.Record("mainnet", errors.New("mainnet is down"))
	r.Record("pearl", nil)

	snap := r.Snapshot()
	if snap["mainnet"].Healthy {
		t.Error("mainnet should not be healthy")
	}
	if !snap["pearl"].Healthy {
		t.Error("pearl should be healthy")
	}
	if snap["pearl"].LastError != "" {
		t.Errorf("pearl inherited mainnet's error: %q", snap["pearl"].LastError)
	}
}

// The handler serialises the snapshot while the sync goroutines keep writing,
// so it has to be a copy.
func TestSnapshotIsACopy(t *testing.T) {
	r := NewRegistry()
	r.Record("mainnet", nil)

	snap := r.Snapshot()
	r.Record("mainnet", errors.New("later failure"))

	if snap["mainnet"].LastError != "" {
		t.Error("a later Record mutated an already-returned snapshot")
	}
	if !r.Snapshot()["mainnet"].Healthy == false {
		t.Error("the registry itself should have recorded the failure")
	}
}

// The registry is reached through a field that is nil in every test and tool
// that does not run a sync loop.
func TestNilRegistryIsInert(t *testing.T) {
	var r *Registry
	r.Record("mainnet", errors.New("boom"))
	if got := r.Snapshot(); got != nil {
		t.Errorf("snapshot of a nil registry = %v, want nil", got)
	}
}
