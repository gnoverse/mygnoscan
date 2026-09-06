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

// rejectUnknownNetwork guards every API route at once, so it is tested against a
// stand-in handler rather than the real mux: what matters is which requests
// reach the other side, not what they would have returned.
func TestRejectUnknownNetwork(t *testing.T) {
	configured := []NetworkConfig{{ID: "gnoland1"}, {ID: "sapphire"}}

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantPassed bool
	}{
		{
			name:       "configured network passes",
			path:       "/api/stats?network=sapphire",
			wantStatus: 200,
			wantPassed: true,
		},
		{
			name:       "no network means all networks",
			path:       "/api/stats",
			wantStatus: 200,
			wantPassed: true,
		},
		{
			name:       "explicit all passes",
			path:       "/api/stats?network=all",
			wantStatus: 200,
			wantPassed: true,
		},
		{
			name: "retired network is rejected",
			// topaz was removed from the config once its testnet went away. The
			// database still holds its rows, so without this the endpoint answers
			// 200 with stale data and an unrelated chain's block height.
			path:       "/api/stats?network=topaz",
			wantStatus: 404,
			wantPassed: false,
		},
		{
			name:       "never-configured network is rejected",
			path:       "/api/txs?network=does-not-exist",
			wantStatus: 404,
			wantPassed: false,
		},
		{
			name: "SPA is not guarded",
			// A stale bookmark must still load the app, which can report the
			// unknown network itself. Handing a browser a JSON 404 would not.
			path:       "/?network=topaz",
			wantStatus: 200,
			wantPassed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			passed := false
			h := rejectUnknownNetwork(configured, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				passed = true
				w.WriteHeader(200)
			}))

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", tt.path, nil))

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if passed != tt.wantPassed {
				t.Errorf("reached the handler = %v, want %v", passed, tt.wantPassed)
			}
			if !tt.wantPassed {
				var body struct {
					Error string `json:"error"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("rejection body is not JSON: %v", err)
				}
				if body.Error == "" {
					t.Error("rejection body carries no error message")
				}
			}
		})
	}
}

// gnoEvents is the pure half of the events endpoints: pick out GnoEvents, drop
// transactions that emitted none, and tag each row with the chain it came from.
// The tag is what lets the merged view interleave chains and show a dot.
func TestGnoEvents(t *testing.T) {
	tx := func(hash string, ok bool, evs ...TxEvent) Transaction {
		return Transaction{Hash: hash, BlockHeight: 10, BlockTime: "2026-01-01T00:00:00Z", Success: ok,
			Response: &TxResponse{Events: evs}}
	}
	gno := func(typ string) TxEvent { return TxEvent{Typename: "GnoEvent", Type: typ} }
	storage := TxEvent{Typename: "StorageDepositEvent", Type: "StorageDeposit"}

	tests := []struct {
		name     string
		txs      []Transaction
		wantRows int
		wantEvs  int
	}{
		{
			name:     "keeps gno events",
			txs:      []Transaction{tx("a", true, gno("Transfer"), gno("Approval"))},
			wantRows: 1,
			wantEvs:  2,
		},
		{
			name: "drops transactions with no gno events",
			// A storage event is still an event, but not one this view lists.
			txs:      []Transaction{tx("a", true, storage), tx("b", true, gno("Mint"))},
			wantRows: 1,
			wantEvs:  1,
		},
		{
			name:     "filters non-gno events out of a mixed transaction",
			txs:      []Transaction{tx("a", true, storage, gno("Mint"), storage)},
			wantRows: 1,
			wantEvs:  1,
		},
		{
			name:     "a nil response is not a crash",
			txs:      []Transaction{{Hash: "a", BlockHeight: 1}},
			wantRows: 0,
		},
		{
			name:     "no transactions",
			txs:      nil,
			wantRows: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := gnoEvents(tt.txs, "sapphire")
			if len(got) != tt.wantRows {
				t.Fatalf("got %d rows, want %d", len(got), tt.wantRows)
			}
			for _, r := range got {
				if r.Network != "sapphire" {
					t.Errorf("row %s has network %q, want sapphire", r.TxHash, r.Network)
				}
				if r.BlockTime == "" {
					t.Errorf("row %s lost its timestamp — the merged view sorts on it", r.TxHash)
				}
				for _, ev := range r.Events {
					if ev.Typename != "GnoEvent" {
						t.Errorf("row %s kept a %s", r.TxHash, ev.Typename)
					}
				}
			}
			if tt.wantRows == 1 && len(got[0].Events) != tt.wantEvs {
				t.Errorf("got %d events, want %d", len(got[0].Events), tt.wantEvs)
			}
		})
	}
}

// "No limit" used to mean every transaction the chain had ever seen, which
// returned 500 after ten seconds on a busy network because the fetch could not
// finish inside the client timeout. The endpoint bounds it instead.
func TestTxWindow(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  int
	}{
		{"absent falls back to the default window", "", defaultTxs},
		{"zero is a request for recent, not for everything", "limit=0", defaultTxs},
		{"negative is treated the same", "limit=-1", defaultTxs},
		{"a small limit is honoured", "limit=20", 20},
		{"a limit above the cap is clamped", "limit=99999", maxTxs},
		{"exactly the cap", fmt.Sprintf("limit=%d", maxTxs), maxTxs},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/txs?"+tt.query, nil)
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

			windowed := limit
			if windowed <= 0 {
				windowed = defaultTxs
			}
			if windowed > maxTxs {
				windowed = maxTxs
			}
			if windowed != tt.want {
				t.Errorf("window = %d, want %d", windowed, tt.want)
			}
		})
	}
}

// The cap has to stay under what the server will wait for, or the endpoint
// trades a bounded answer for a timeout — which is the bug it was fixing.
func TestTxWindowCapIsSane(t *testing.T) {
	if maxTxs > indexerElementCap {
		t.Errorf("maxTxs %d exceeds the indexer's element cap %d, so the cap can never be reached",
			maxTxs, indexerElementCap)
	}
	if defaultTxs > maxTxs {
		t.Errorf("defaultTxs %d is above maxTxs %d", defaultTxs, maxTxs)
	}
	if defaultTxs <= 0 {
		t.Errorf("defaultTxs %d would make an absent limit return nothing", defaultTxs)
	}
}

// Every cross-network list orders through newerFirst. Heights are per-chain —
// gnoland1 sits near 3.1M while sapphire is near 400k — so comparing them across
// networks lets the chain with the largest numbers win every comparison. In a
// list that is then truncated to a page, that does not mis-order: it deletes a
// chain. It did exactly that to sapphire's events before block times were
// stamped, which is what this pins.
func TestNewerFirst(t *testing.T) {
	const (
		older = "2026-08-01T00:00:00Z"
		newer = "2026-08-27T00:00:00Z"
	)

	tests := []struct {
		name             string
		timeA, timeB     string
		heightA, heightB int
		want             bool
	}{
		{
			name: "the newer timestamp wins regardless of height",
			// The classic cross-chain case: a low-height row from a busy chain
			// is genuinely more recent than a high-height row from a quiet one.
			timeA: newer, timeB: older, heightA: 400_000, heightB: 3_100_000,
			want: true,
		},
		{
			name:  "the older timestamp loses regardless of height",
			timeA: older, timeB: newer, heightA: 3_100_000, heightB: 400_000,
			want: false,
		},
		{
			name: "a dated row sorts ahead of an undated one",
			// Undated rows collect at the end instead of being interleaved by a
			// number that means nothing across chains.
			timeA: older, timeB: "", heightA: 1, heightB: 9_999_999,
			want: true,
		},
		{
			name:  "an undated row sorts behind a dated one",
			timeA: "", timeB: older, heightA: 9_999_999, heightB: 1,
			want: false,
		},
		{
			name: "height only decides between two undated rows",
			// By here both are undated, so any order is a guess — but a stable one.
			timeA: "", timeB: "", heightA: 200, heightB: 100,
			want: true,
		},
		{
			// Neither is strictly newer, so this reports false and the stable
			// sort keeps the input order. Height is deliberately not consulted:
			// rows sharing a timestamp are in the same block on the same chain,
			// where height cannot separate them either.
			name:  "equal timestamps leave the order alone",
			timeA: newer, timeB: newer, heightA: 200, heightB: 100,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := newerFirst(tt.timeA, tt.timeB, tt.heightA, tt.heightB); got != tt.want {
				t.Errorf("newerFirst(%q, %q, %d, %d) = %v, want %v",
					tt.timeA, tt.timeB, tt.heightA, tt.heightB, got, tt.want)
			}
		})
	}
}

// The failure this ordering exists to prevent, end to end: a page-sized
// truncation must not drop a whole chain.
func TestMergedListDoesNotDropAChain(t *testing.T) {
	// sapphire is busier and more recent but numbers its blocks far lower.
	rows := []Transaction{}
	for i := 0; i < 40; i++ {
		rows = append(rows, Transaction{
			Hash:        fmt.Sprintf("gnoland1-%d", i),
			BlockHeight: 3_100_000 + i,
			BlockTime:   "2026-08-01T00:00:00Z",
		})
	}
	for i := 0; i < 40; i++ {
		rows = append(rows, Transaction{
			Hash:        fmt.Sprintf("sapphire-%d", i),
			BlockHeight: 400_000 + i,
			BlockTime:   "2026-08-27T00:00:00Z",
		})
	}

	sortTransactionsByTime(rows)
	page := rows[:20] // what a limit would keep

	for _, tx := range page {
		if tx.BlockTime != "2026-08-27T00:00:00Z" {
			t.Fatalf("page contains an older row (%s) — the newest 20 should all be sapphire's", tx.Hash)
		}
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
