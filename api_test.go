package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestGranularityForSpan(t *testing.T) {
	// The bands are expressed as target point counts (see the constants next
	// to granularityForSpan), not hardcoded day counts, so they don't need
	// re-tuning as a specific chain ages. The boundaries below are the ones
	// that arithmetic implies.
	tests := []struct {
		name string
		days int
		want string
	}{
		{"a few hours of history", 1, "hourly"},
		{"the original bug report: an 8-day chain", 8, "hourly"},
		// spanDays rounds an 8-day history up to 9; it must still be hourly.
		{"nine days, the rounded-up 8-day chain", 9, "hourly"},
		{"ten days is the last hourly band (240 points)", 10, "hourly"},
		{"eleven days switches to daily", 11, "daily"},
		{"gno.land mainnet, ~165 days", 165, "daily"},
		{"550 days is the last daily band", 550, "daily"},
		{"551 days switches to weekly", 551, "weekly"},
		{"~5 years is the last weekly band", 1820, "weekly"},
		{"integer division keeps the boundary through 1826", 1826, "weekly"},
		{"beyond ~5 years is monthly", 1827, "monthly"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := granularityForSpan(tt.days); got != tt.want {
				t.Errorf("granularityForSpan(%d) = %q, want %q", tt.days, got, tt.want)
			}
		})
	}
}

func TestResolveTimeseriesParamsSizesAllToTheData(t *testing.T) {
	db := newTestDB(t)
	api := &API{db: db}

	// Eight days of history, which is what a young chain looks like. The fixed
	// windowSpecs mapping would bucket this monthly and collapse it to a single
	// point.
	start := time.Now().UTC().AddDate(0, 0, -8).Format(time.RFC3339)
	if err := db.InsertCall("gnoland1", "TX1", 1, start, "g1a", "gno.land/r/demo/foo", "Bar", true); err != nil {
		t.Fatalf("insert call: %v", err)
	}

	tests := []struct {
		name            string
		query           string
		wantDays        int
		wantGranularity string
	}{
		{"all is sized to the real span, and is at least as fine as 7d", "window=all", 9, "hourly"},
		{"case-insensitive", "window=ALL", 9, "hourly"},
		// Back-compat: explicit values still win, exactly as in parseTimeseriesParams.
		{"explicit granularity still wins", "window=all&granularity=monthly", allWindowDays, "monthly"},
		{"explicit days still wins", "window=all&days=7", 7, "monthly"},
		// Garbage days must not opt out of sizing: parseTimeseriesParams treats
		// unparseable days as "not supplied" and falls through to its own
		// default, so resolveTimeseriesParams must fall through to sizing too.
		{"garbage days does not opt out of sizing", "window=all&days=notanumber", 9, "hourly"},
		// Other windows are untouched by the span.
		{"90d is unaffected", "window=90d", 90, "daily"},
		{"legacy params unaffected", "days=14&granularity=hourly", 14, "hourly"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/timeseries/transactions?"+tt.query, nil)
			days, granularity := api.resolveTimeseriesParams(r, "gnoland1")
			if days != tt.wantDays {
				t.Errorf("days = %d, want %d", days, tt.wantDays)
			}
			if granularity != tt.wantGranularity {
				t.Errorf("granularity = %q, want %q", granularity, tt.wantGranularity)
			}
		})
	}
}

func TestResolveTimeseriesParamsClampsClockSkew(t *testing.T) {
	// A start time in the future (clock skew, not a real negative-length span)
	// must floor to a 1-day span rather than producing something negative.
	db := newTestDB(t)
	api := &API{db: db}

	future := time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339)
	if err := db.InsertCall("gnoland1", "TX1", 1, future, "g1a", "gno.land/r/demo/foo", "Bar", true); err != nil {
		t.Fatalf("insert call: %v", err)
	}

	r := httptest.NewRequest("GET", "/api/timeseries/transactions?window=all", nil)
	days, granularity := api.resolveTimeseriesParams(r, "gnoland1")
	if days != 1 || granularity != "hourly" {
		t.Errorf("got (%d, %q), want (1, \"hourly\")", days, granularity)
	}
}

func TestResolveTimeseriesParamsClampsMalformedFarPastTimestamp(t *testing.T) {
	// A row with a year-1 timestamp is valid RFC3339 and passes
	// NetworkDataStart's parse guard, so it must be caught here: without the
	// allWindowDays clamp this produces a ~106,752-day span (time.Duration
	// saturates near 292 years), which fillBuckets would iterate one bucket at
	// a time.
	db := newTestDB(t)
	api := &API{db: db}

	if err := db.InsertCall("gnoland1", "TX1", 1, "0001-01-01T00:00:00Z", "g1a", "gno.land/r/demo/foo", "Bar", true); err != nil {
		t.Fatalf("insert call: %v", err)
	}

	r := httptest.NewRequest("GET", "/api/timeseries/transactions?window=all", nil)
	days, granularity := api.resolveTimeseriesParams(r, "gnoland1")
	if days != allWindowDays || granularity != "monthly" {
		t.Errorf("got (%d, %q), want (%d, \"monthly\")", days, granularity, allWindowDays)
	}
}

func TestResolveTimeseriesParamsFallsBackWithoutData(t *testing.T) {
	// A network with nothing indexed has no span to measure; the fixed mapping
	// is the right answer because every window returns empty anyway.
	api := &API{db: newTestDB(t)}
	r := httptest.NewRequest("GET", "/api/timeseries/transactions?window=all", nil)
	days, granularity := api.resolveTimeseriesParams(r, "gnoland1")
	if days != allWindowDays || granularity != "monthly" {
		t.Errorf("got (%d, %q), want (%d, \"monthly\")", days, granularity, allWindowDays)
	}
}

// TestHandleTimeSeriesTransactionsWindowAllOnAYoungChain is the regression
// test for the original bug report: a chain with only a few days of history
// rendered window=all as a single dot, because the fixed (allWindowDays,
// monthly) mapping bucketed the whole span into one bucket. This exercises
// the handler end to end, unlike TestResolveTimeseriesParamsSizesAllToTheData
// which only checks the (days, granularity) the resolver picks — neither
// resolveTimeseriesParams nor granularityForSpan existed when the bug was
// reported, so a test confined to them could not have caught it.
func TestHandleTimeSeriesTransactionsWindowAllOnAYoungChain(t *testing.T) {
	db := newTestDB(t)
	api := &API{db: db}

	// Eight days of hourly calls, one per hour, so a single-bucket response
	// would be obviously wrong regardless of which granularity is chosen.
	start := time.Now().UTC().AddDate(0, 0, -8)
	for i := 0; i < 8*24; i += 3 {
		ts := start.Add(time.Duration(i) * time.Hour).Format(time.RFC3339)
		txHash := "TX" + strconv.Itoa(i)
		if err := db.InsertCall("gnoland1", txHash, i, ts, "g1a", "gno.land/r/demo/foo", "Bar", true); err != nil {
			t.Fatalf("insert call: %v", err)
		}
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/timeseries/transactions?window=all&network=gnoland1", nil)
	api.HandleTimeSeriesTransactions(w, r)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var pts []TxTimePoint
	if err := json.Unmarshal(w.Body.Bytes(), &pts); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	nonEmpty := 0
	for _, p := range pts {
		if p.Total > 0 {
			nonEmpty++
		}
	}
	// The single-dot bug would produce exactly one non-empty bucket (or, with
	// the fixed monthly mapping applied to an 8-day span, exactly one bucket
	// total). A healthy response spreads the data across many buckets.
	if nonEmpty <= 1 {
		t.Fatalf("got %d non-empty buckets out of %d, want substantially more than one", nonEmpty, len(pts))
	}
}

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

// --- batch 2b ---

func TestHandleFunctionCallHeatmapRequiresARealm(t *testing.T) {
	api := &API{db: newTestDB(t)}

	w := httptest.NewRecorder()
	api.HandleFunctionCallHeatmap(w, httptest.NewRequest("GET", "/api/calls/function-heatmap", nil))
	if w.Code != 400 {
		t.Errorf("status = %d, want 400 — a heatmap with no realm has no y-axis", w.Code)
	}

	w = httptest.NewRecorder()
	api.HandleFunctionCallHeatmap(w, httptest.NewRequest("GET", "/api/calls/function-heatmap?realm=gno.land/r/nope", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 for an unknown realm", w.Code)
	}
	// Empty must serialize as [] rather than null: the frontend distinguishes
	// "no data in this window" from a load failure by the array's length.
	if got := strings.TrimSpace(w.Body.String()); got != "[]" {
		t.Errorf("body = %s, want []", got)
	}
}

func TestBatch2bEndpointsSerializeEmptyAsArrays(t *testing.T) {
	api := &API{db: newTestDB(t)}

	tests := []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request)
		path    string
		// Mode B endpoints return a fixed-size grid or bin set even when empty;
		// mode A ones return whatever buckets the window covers.
		wantNonEmpty bool
	}{
		{"activity heatmap", api.HandleActivityHeatmap, "/api/activity/heatmap?window=90d", true},
		{"gas per tx", api.HandleGasPerTxHistogram, "/api/gas/per-tx-histogram?window=90d", true},
		{"new addresses", api.HandleTimeSeriesNewAddresses, "/api/timeseries/new-addresses?window=30d", true},
		{"active rolling", api.HandleTimeSeriesActiveRolling, "/api/timeseries/active-rolling?window=30d", true},
		{"call realms", api.HandleCallRealms, "/api/calls/realms", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			tt.handler(w, httptest.NewRequest("GET", tt.path, nil))
			if w.Code != 200 {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			body := strings.TrimSpace(w.Body.String())
			if !strings.HasPrefix(body, "[") {
				t.Fatalf("body = %.60s, want a JSON array (never null)", body)
			}
			if tt.wantNonEmpty && body == "[]" {
				t.Errorf("empty database collapsed the response to []; this endpoint's shape is fixed, not data-driven")
			}
			if !tt.wantNonEmpty && body != "[]" {
				t.Errorf("body = %.60s, want []", body)
			}
		})
	}
}

// The three trailing windows are day-defined, so the endpoint must ignore
// ?granularity= rather than mislabel a 30-day count as an hour's worth.
func TestHandleTimeSeriesActiveRollingIsAlwaysDaily(t *testing.T) {
	api := &API{db: newTestDB(t)}

	w := httptest.NewRecorder()
	api.HandleTimeSeriesActiveRolling(w, httptest.NewRequest("GET", "/api/timeseries/active-rolling?days=10&granularity=hourly", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var pts []RollingActivePoint
	if err := json.Unmarshal(w.Body.Bytes(), &pts); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(pts) != 11 {
		t.Fatalf("got %d points for days=10, want 11 daily ones", len(pts))
	}
	for _, p := range pts {
		if _, err := time.Parse("2006-01-02", p.Time); err != nil {
			t.Errorf("bucket %q is not a daily label", p.Time)
		}
	}
}

// Fix 3: parseTimeseriesParams' 365-day cap is skipped for granularity=monthly,
// and this handler discards granularity entirely, so an explicit
// days=3650&granularity=monthly must not reach the query uncapped — it is
// always a daily series, so 3650 points would be 10 years of daily data.
func TestHandleTimeSeriesActiveRollingCapsExplicitDays(t *testing.T) {
	api := &API{db: newTestDB(t)}

	w := httptest.NewRecorder()
	api.HandleTimeSeriesActiveRolling(w, httptest.NewRequest("GET", "/api/timeseries/active-rolling?days=3650&granularity=monthly", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var pts []RollingActivePoint
	if err := json.Unmarshal(w.Body.Bytes(), &pts); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(pts) > 366 {
		t.Errorf("got %d points, want at most 366 (the 365-day rolling cap plus one)", len(pts))
	}
}

// Fix 3: window=all on a network with nothing indexed falls back to the fixed
// (allWindowDays, monthly) mapping, which reaches this handler as
// days=3650 too. An empty database must not turn that into a multi-thousand
// point all-zero response.
func TestHandleTimeSeriesActiveRollingCapsWindowAllOnEmptyDatabase(t *testing.T) {
	api := &API{db: newTestDB(t)}

	w := httptest.NewRecorder()
	api.HandleTimeSeriesActiveRolling(w, httptest.NewRequest("GET", "/api/timeseries/active-rolling?window=all", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var pts []RollingActivePoint
	if err := json.Unmarshal(w.Body.Bytes(), &pts); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(pts) > 366 {
		t.Errorf("got %d points for window=all on an empty database, want at most 366", len(pts))
	}
}

// Fix 7: ?limit= on the realm selector must be validated and capped, not
// passed straight through with its error discarded.
func TestHandleCallRealmsValidatesLimit(t *testing.T) {
	api := &API{db: newTestDB(t)}
	now := time.Now().UTC()
	for i := 0; i < realmsWithCallsMaxLimit+20; i++ {
		mustCall(t, api.db, "gnoland1", fmt.Sprintf("c%d", i), i+1, now.Add(-time.Hour), "g1a", fmt.Sprintf("gno.land/r/x%d", i), "F")
	}

	w := httptest.NewRecorder()
	api.HandleCallRealms(w, httptest.NewRequest("GET", fmt.Sprintf("/api/calls/realms?limit=%d", realmsWithCallsMaxLimit+20), nil))
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var paths []string
	if err := json.Unmarshal(w.Body.Bytes(), &paths); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(paths) != realmsWithCallsMaxLimit {
		t.Errorf("got %d realms, want the cap of %d", len(paths), realmsWithCallsMaxLimit)
	}

	// A garbage limit must not error or panic; it falls back to the default.
	w = httptest.NewRecorder()
	api.HandleCallRealms(w, httptest.NewRequest("GET", "/api/calls/realms?limit=notanumber", nil))
	if w.Code != 200 {
		t.Fatalf("garbage limit: status = %d, want 200", w.Code)
	}
}

func TestStorageConsumersRouteBeatsTheRealmWildcard(t *testing.T) {
	// /api/storage/{path...} is registered too; the literal must win, or the
	// consumers endpoint would be handled as a realm named "consumers".
	mux := http.NewServeMux()
	hit := ""
	mux.HandleFunc("GET /api/storage/{path...}", func(http.ResponseWriter, *http.Request) { hit = "wildcard" })
	mux.HandleFunc("GET /api/storage/consumers", func(http.ResponseWriter, *http.Request) { hit = "consumers" })

	for _, tc := range []struct{ path, want string }{
		{"/api/storage/consumers", "consumers"},
		{"/api/storage/r/demo/foo", "wildcard"},
	} {
		hit = ""
		mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", tc.path, nil))
		if hit != tc.want {
			t.Errorf("%s routed to %q, want %q", tc.path, hit, tc.want)
		}
	}
}
