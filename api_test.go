package main

import (
	"net/http/httptest"
	"testing"
)

func TestParseTimeseriesParams(t *testing.T) {
	tests := []struct {
		name            string
		query           string
		wantDays        int
		wantGranularity string
	}{
		{"empty falls back to the historical default", "", 30, "daily"},
		{"window 24h", "window=24h", 1, "hourly"},
		{"window 7d", "window=7d", 7, "hourly"},
		{"window 30d", "window=30d", 30, "daily"},
		{"window 90d", "window=90d", 90, "daily"},
		{"window 1y", "window=1y", 365, "weekly"},
		{"window all is monthly and exceeds the 365 cap", "window=all", allWindowDays, "monthly"},
		{"window is case-insensitive", "window=ALL", allWindowDays, "monthly"},
		{"unknown window is ignored", "window=nope", 30, "daily"},
		// Back-compat: the existing analytics/gas/sanity views pass these and
		// must keep their exact behaviour.
		{"legacy days and granularity still work", "days=14&granularity=hourly", 14, "hourly"},
		{"legacy days still capped at 365", "days=5000", 365, "daily"},
		{"legacy invalid granularity still falls back to daily", "granularity=yearly", 30, "daily"},
		// Explicit parameters win over window.
		{"explicit days overrides window", "window=all&days=7", 7, "monthly"},
		{"explicit granularity overrides window and re-applies the cap", "window=all&granularity=daily", 365, "daily"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/timeseries/transactions?"+tt.query, nil)
			days, granularity := parseTimeseriesParams(r)
			if days != tt.wantDays {
				t.Errorf("days = %d, want %d", days, tt.wantDays)
			}
			if granularity != tt.wantGranularity {
				t.Errorf("granularity = %q, want %q", granularity, tt.wantGranularity)
			}
		})
	}
}
