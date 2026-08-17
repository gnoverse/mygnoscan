# Network Graph Selection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the three "network" section charts' static top-N views with a shared table+search+multi-select UI: rank addresses/realms by volume or calls, search for any one even outside the top ranking, select up to 20, and view the induced subgraph among exactly the selected set, with per-selection removal via tag/chip.

**Architecture:** Two new backend query capabilities (ranking with sent/received or caller/realm breakdown, and bounded substring search) plus a new "selected-set" mode on the existing two graph endpoints. One new frontend component, `renderEntityPicker`, shared by all three charts, replacing their current `fetch`/`opt` dispatch with a `render`-owned card that toggles between a table view and each chart's existing graph rendering.

**Tech Stack:** Go (stdlib + `modernc.org/sqlite`), vanilla JS with the repo's `el()` helper, ECharts 5 + echarts-gl (already in place). No bundler, no build step.

**Spec:** [`docs/superpowers/specs/2026-08-17-network-graph-selection-design.md`](../specs/2026-08-17-network-graph-selection-design.md)

## Global Constraints

- **Everything is network-scoped.** Every query, join and aggregate filters or groups by `network`.
- **The frontend builds DOM, never HTML strings.** Use `el()`. No `innerHTML` with interpolated data. Addresses and realm paths are attacker-controlled — table cells, tags, and tooltips must never interpolate them into HTML.
- **No build step.** No bundler, no npm, no framework, no JS test runner.
- **Search is parameterized, never string-concatenated.** `LIKE '%'||?||'%'` with the search term bound, not built into the query text.
- **Search and selection are both bounded.** Search results capped at 50 regardless of match count; selections capped at 20, with extras silently dropped (clamp, don't error — matches the existing `topN` clamping convention).
- **Go gates before any commit:** `gofmt -l .` prints nothing, `go vet ./...` passes, `go test ./...` passes.
- **Commits are conventional and single-line. No co-author or attribution trailers.**
- **Go tests are table-driven** with a real temp SQLite file, never mocks.
- **This is additive, not breaking**, to the batch-4 backend: existing `topN`/`ego`/`min_value`/`min_calls` params and response shapes are unchanged when the new `addresses`/`entities` params are absent.
- **`caller_edges` is bipartite.** A `caller`/realm-`pkg_path` pair is the only edge shape; a selected-entities graph with only callers or only realms yields zero edges, by construction, not by bug.

---

## File Structure

| File | Change | Responsibility |
|---|---|---|
| `db.go` | New queries + signature changes | `GetTransferRanking`, `GetCallerRanking`, `edgesAmongSet`, `selectedTransferGraph`, `selectedCallerGraph`; `GetTransferGraph`/`GetCallerGraph` gain a new param each |
| `db_test.go` | Tests + 5 call-site updates | New ranking/selected-set tests; existing `GetTransferGraph`/`GetCallerGraph` call sites updated for the new param |
| `api.go` | 2 new handlers + 2 updated | `HandleGraphTransfersRanking`, `HandleGraphCallersRanking`; `HandleGraphTransfers`/`HandleGraphCallers` parse the new params |
| `api_test.go` | Tests | Ranking endpoints; `addresses`/`entities` param wiring |
| `main.go` | 2 new routes | `/api/graph/transfers/ranking`, `/api/graph/callers/ranking` |
| `frontend/index.html` | New shared component + CSS; 3 charts rewired | `renderEntityPicker`; `transfer-graph`, `token-flow-sankey`, `caller-graph` switch from `fetch`/`opt` to `render`; `onNetworkChange` resets the new state shape |
| `docs/api.md` | Update | New endpoints, new params on the two existing ones |

---

## Task 1: Ranking queries

**Files:**
- Modify: `db.go` (new `// --- network graph ranking ---` group)
- Test: `db_test.go`

**Interfaces:**
- Consumes: `transfer_edges`, `caller_edges` schemas (unchanged, from batch 4).
- Produces:
  - `type TransferRankRow struct { Address string; Sent, Received, Volume int64 }` — JSON `address`, `sent`, `received`, `volume`
  - `type CallerRankRow struct { ID, Type string; Calls int }` — JSON `id`, `type`, `calls`
  - `const searchResultCap = 50`
  - `func (d *DB) GetTransferRanking(network string, days, topN int, search string) ([]TransferRankRow, error)`
  - `func (d *DB) GetCallerRanking(network string, days, topN int, search string) ([]CallerRankRow, error)`

- [ ] **Step 1: Write the failing tests**

Append to `db_test.go`:

```go
func TestGetTransferRankingSplitsSentReceived(t *testing.T) {
	db := newTestDB(t)
	today := time.Now().UTC().Format("2006-01-02")
	seedTransferEdge(t, db, "gnoland1", "g1a", "g1b", today, 1000, 1)
	seedTransferEdge(t, db, "gnoland1", "g1b", "g1a", today, 300, 1)

	rows, err := db.GetTransferRanking("gnoland1", 7, 10, "")
	if err != nil {
		t.Fatalf("ranking: %v", err)
	}
	byAddr := map[string]TransferRankRow{}
	for _, r := range rows {
		byAddr[r.Address] = r
	}
	if a := byAddr["g1a"]; a.Sent != 1000 || a.Received != 300 || a.Volume != 1300 {
		t.Errorf("g1a = %+v, want sent=1000 received=300 volume=1300", a)
	}
	if b := byAddr["g1b"]; b.Sent != 300 || b.Received != 1000 || b.Volume != 1300 {
		t.Errorf("g1b = %+v, want sent=300 received=1000 volume=1300", b)
	}
}

func TestGetTransferRankingSearchFindsOutsideTopN(t *testing.T) {
	db := newTestDB(t)
	today := time.Now().UTC().Format("2006-01-02")
	// g1a and g1b dwarf g1z in volume; a topN=2 ranking would never surface g1z.
	seedTransferEdge(t, db, "gnoland1", "g1a", "g1b", today, 100000, 1)
	seedTransferEdge(t, db, "gnoland1", "g1b", "g1a", today, 90000, 1)
	seedTransferEdge(t, db, "gnoland1", "g1z", "g1a", today, 5, 1)

	ranked, err := db.GetTransferRanking("gnoland1", 7, 2, "")
	if err != nil {
		t.Fatalf("ranking: %v", err)
	}
	for _, r := range ranked {
		if r.Address == "g1z" {
			t.Fatalf("g1z appeared in the top-2 ranking; test setup is wrong")
		}
	}

	found, err := db.GetTransferRanking("gnoland1", 7, 0, "g1z")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(found) != 1 || found[0].Address != "g1z" {
		t.Errorf("search results = %+v, want just g1z", found)
	}
}

func TestGetTransferRankingSearchIsBoundedAndParameterized(t *testing.T) {
	db := newTestDB(t)
	today := time.Now().UTC().Format("2006-01-02")
	// A search term containing a single quote must not break the query or
	// inject SQL — it must be treated as a literal, bound value.
	seedTransferEdge(t, db, "gnoland1", "g1a", "g1b", today, 100, 1)

	rows, err := db.GetTransferRanking("gnoland1", 7, 0, "g1a'; DROP TABLE transfer_edges; --")
	if err != nil {
		t.Fatalf("search with a quote in the term errored instead of treating it as a literal: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %+v, want none — the literal term does not match any address", rows)
	}
	// The table must still exist and be queryable.
	if _, err := db.GetTransferRanking("gnoland1", 7, 10, ""); err != nil {
		t.Fatalf("transfer_edges appears to have been dropped: %v", err)
	}
}

func TestGetTransferRankingSearchIsCapped(t *testing.T) {
	db := newTestDB(t)
	today := time.Now().UTC().Format("2006-01-02")
	for i := 0; i < 60; i++ {
		seedTransferEdge(t, db, "gnoland1", fmt.Sprintf("g1match%02d", i), "g1counterparty", today, 10, 1)
	}

	rows, err := db.GetTransferRanking("gnoland1", 7, 0, "g1match")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(rows) != searchResultCap {
		t.Errorf("got %d results, want the cap of %d", len(rows), searchResultCap)
	}
}

func TestGetTransferRankingIsNetworkScoped(t *testing.T) {
	db := newTestDB(t)
	today := time.Now().UTC().Format("2006-01-02")
	seedTransferEdge(t, db, "gnoland1", "g1a", "g1b", today, 100, 1)
	seedTransferEdge(t, db, "test12", "g1a", "g1b", today, 99999, 1)

	rows, err := db.GetTransferRanking("gnoland1", 7, 10, "")
	if err != nil {
		t.Fatalf("ranking: %v", err)
	}
	for _, r := range rows {
		if r.Volume >= 99999 {
			t.Errorf("row %+v looks like it includes test12's volume — network scoping leaked", r)
		}
	}
}

func TestGetCallerRankingCombinesCallersAndRealmsIndependently(t *testing.T) {
	// A realm called by many small callers should rank on its own total, not
	// be limited to appearing only alongside a single top-ranked caller.
	db := newTestDB(t)
	today := time.Now().UTC().Format("2006-01-02")
	for i := 0; i < 5; i++ {
		seedCallerEdge(t, db, "gnoland1", fmt.Sprintf("g1caller%d", i), "gno.land/r/popular", today, 100)
	}
	seedCallerEdge(t, db, "gnoland1", "g1biggestcaller", "gno.land/r/rare", today, 50)

	rows, err := db.GetCallerRanking("gnoland1", 7, 10, "")
	if err != nil {
		t.Fatalf("ranking: %v", err)
	}
	byID := map[string]CallerRankRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	popular, ok := byID["gno.land/r/popular"]
	if !ok || popular.Type != "realm" || popular.Calls != 500 {
		t.Errorf("gno.land/r/popular = %+v (ok=%v), want type=realm calls=500", popular, ok)
	}
	if popular.Calls <= byID["g1biggestcaller"].Calls {
		t.Errorf("popular realm's combined calls (%d) should exceed the single biggest caller's (%d)",
			popular.Calls, byID["g1biggestcaller"].Calls)
	}
}

func TestGetCallerRankingSearch(t *testing.T) {
	db := newTestDB(t)
	today := time.Now().UTC().Format("2006-01-02")
	seedCallerEdge(t, db, "gnoland1", "g1a", "gno.land/r/demo/needle", today, 1)

	rows, err := db.GetCallerRanking("gnoland1", 7, 0, "needle")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "gno.land/r/demo/needle" || rows[0].Type != "realm" {
		t.Errorf("rows = %+v, want just the realm", rows)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestGetTransferRanking|TestGetCallerRanking' ./... -v`
Expected: FAIL to compile — `db.GetTransferRanking undefined`, `db.GetCallerRanking undefined`, `undefined: searchResultCap`.

- [ ] **Step 3: Implement**

Append a new group to `db.go`, after the `// --- network graphs ---` group batch 4 added:

```go
// --- network graph ranking ---

// searchResultCap bounds a search query's result count regardless of how
// many rows match — a broad or empty search term must not return an
// unbounded payload.
const searchResultCap = 50

type TransferRankRow struct {
	Address  string `json:"address"`
	Sent     int64  `json:"sent"`
	Received int64  `json:"received"`
	Volume   int64  `json:"volume"` // sent + received
}

// GetTransferRanking ranks addresses by total volume (sent+received) in the
// window, splitting sent/received for the table UI. search, when non-empty,
// replaces the topN cutoff with a bounded substring match, so an address
// outside the top ranking can still be found and selected.
func (d *DB) GetTransferRanking(network string, days, topN int, search string) ([]TransferRankRow, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	start := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")

	base := `
		SELECT addr, COALESCE(SUM(sent), 0), COALESCE(SUM(received), 0) FROM (
			SELECT from_address AS addr, total_value AS sent, 0 AS received FROM transfer_edges WHERE network = ? AND day >= ?
			UNION ALL
			SELECT to_address AS addr, 0 AS sent, total_value AS received FROM transfer_edges WHERE network = ? AND day >= ?
		)`

	var rows *sql.Rows
	var err error
	if search != "" {
		rows, err = d.db.Query(base+`
			WHERE addr LIKE '%'||?||'%'
			GROUP BY addr ORDER BY SUM(sent)+SUM(received) DESC LIMIT ?`,
			network, start, network, start, search, searchResultCap)
	} else {
		if topN <= 0 {
			topN = 100
		}
		if topN > 1000 {
			topN = 1000
		}
		rows, err = d.db.Query(base+`
			GROUP BY addr ORDER BY SUM(sent)+SUM(received) DESC LIMIT ?`,
			network, start, network, start, topN)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TransferRankRow
	for rows.Next() {
		var r TransferRankRow
		if err := rows.Scan(&r.Address, &r.Sent, &r.Received); err != nil {
			return nil, err
		}
		r.Volume = r.Sent + r.Received
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if out == nil {
		out = []TransferRankRow{}
	}
	return out, nil
}

type CallerRankRow struct {
	ID    string `json:"id"`
	Type  string `json:"type"` // "caller" | "realm"
	Calls int    `json:"calls"`
}

// GetCallerRanking ranks callers and realms together by call volume. Unlike
// GetCallerGraph's topN mode (which only ranks callers, then shows whichever
// realms they happen to call), this independently ranks realms by their own
// total — a heavily-called realm surfaces even if no single caller in the
// top ranking is the one calling it.
func (d *DB) GetCallerRanking(network string, days, topN int, search string) ([]CallerRankRow, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	start := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")

	base := `
		SELECT id, type, SUM(calls) FROM (
			SELECT caller AS id, 'caller' AS type, calls FROM caller_edges WHERE network = ? AND day >= ?
			UNION ALL
			SELECT pkg_path AS id, 'realm' AS type, calls FROM caller_edges WHERE network = ? AND day >= ?
		)`

	var rows *sql.Rows
	var err error
	if search != "" {
		rows, err = d.db.Query(base+`
			WHERE id LIKE '%'||?||'%'
			GROUP BY id, type ORDER BY SUM(calls) DESC LIMIT ?`,
			network, start, network, start, search, searchResultCap)
	} else {
		if topN <= 0 {
			topN = 200
		}
		if topN > 1000 {
			topN = 1000
		}
		rows, err = d.db.Query(base+`
			GROUP BY id, type ORDER BY SUM(calls) DESC LIMIT ?`,
			network, start, network, start, topN)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CallerRankRow
	for rows.Next() {
		var r CallerRankRow
		if err := rows.Scan(&r.ID, &r.Type, &r.Calls); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if out == nil {
		out = []CallerRankRow{}
	}
	return out, nil
}
```

`seedTransferEdge`/`seedCallerEdge` already exist in `db_test.go` from batch 4 — no changes needed there. `fmt` is already imported in `db_test.go` (used elsewhere); if not, add it.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -run 'TestGetTransferRanking|TestGetCallerRanking' ./... -v`
Expected: PASS (7 tests).

- [ ] **Step 5: Run the full gate**

```bash
gofmt -l . && go vet ./... && go test ./...
```

- [ ] **Step 6: Commit**

```bash
git add db.go db_test.go
git commit -m "feat: add transfer and caller ranking queries"
```

---

## Task 2: Selected-set graph queries

**Files:**
- Modify: `db.go` — `// --- network graphs ---` group (batch 4's group; refactor + extend)
- Modify: `db_test.go` — 5 existing call sites need a new trailing argument

**Interfaces:**
- Consumes: `GraphNode`, `GraphEdge`, `TransferGraph`, `CallerGraphNode`, `CallerGraphEdge`, `CallerGraph` (batch 4, unchanged shapes).
- Produces:
  - `const maxSelectedEntities = 20`
  - `func (d *DB) edgesAmongSet(network, start string, order []string, minValue int64) ([]GraphEdge, error)` (new, extracted)
  - `func (d *DB) selectedTransferGraph(network, start string, addresses []string, minValue int64) (TransferGraph, error)` (new)
  - `func (d *DB) selectedCallerGraph(network, start string, entities []string, minCalls int) (CallerGraph, error)` (new)
  - `func (d *DB) GetTransferGraph(network string, days, topN int, minValue int64, ego string, addresses []string) (TransferGraph, error)` — **signature change**, new trailing param
  - `func (d *DB) GetCallerGraph(network string, days, topN, minCalls int, entities []string) (CallerGraph, error)` — **signature change**, new trailing param

**This is a breaking signature change to two batch-4 functions.** Every existing call site must be updated in the same commit or the build fails: `api.go`'s `HandleGraphTransfers`/`HandleGraphCallers` (Task 3 updates these) and 5 call sites in `db_test.go` (this task updates those, since they'd otherwise fail to compile before Task 3 even runs).

- [ ] **Step 1: Update the 5 existing test call sites**

In `db_test.go`, these 4 calls to `GetTransferGraph` each get a trailing `, nil`:

```go
// before: db.GetTransferGraph("gnoland1", 7, 2, 0, "")
g, err := db.GetTransferGraph("gnoland1", 7, 2, 0, "", nil)
```

Apply the same trailing `, nil` to the `GetTransferGraph` calls in `TestGetTransferGraphMinValueFiltersDustEdges`, `TestGetTransferGraphEgoModeReturnsOneHopNeighborhoodOnly`, and `TestGetTransferGraphIsNetworkScoped` (the calls with args `"gnoland1", 7, 100, 100, ""`, `"gnoland1", 7, 0, 0, "g1a"`, and `"gnoland1", 7, 10, 0, ""` respectively).

And the 1 call to `GetCallerGraph`:

```go
// before: db.GetCallerGraph("gnoland1", 7, 1, 0)
g, err := db.GetCallerGraph("gnoland1", 7, 1, 0, nil)
```

- [ ] **Step 2: Write the failing tests**

Append to `db_test.go`:

```go
func TestSelectedTransferGraphOnlyIncludesEdgesBetweenSelectedAddresses(t *testing.T) {
	db := newTestDB(t)
	today := time.Now().UTC().Format("2006-01-02")
	seedTransferEdge(t, db, "gnoland1", "g1a", "g1b", today, 100, 1)
	seedTransferEdge(t, db, "gnoland1", "g1a", "g1c", today, 999, 1) // g1c is not selected

	g, err := db.GetTransferGraph("gnoland1", 7, 0, 0, "", []string{"g1a", "g1b"})
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	if len(g.Edges) != 1 || g.Edges[0].To == "g1c" {
		t.Fatalf("edges = %+v, want just the g1a->g1b edge (g1c was not selected)", g.Edges)
	}
	if len(g.Nodes) != 2 {
		t.Errorf("nodes = %+v, want exactly the 2 selected addresses", g.Nodes)
	}
}

func TestSelectedTransferGraphCapsAtMaxSelectedEntities(t *testing.T) {
	db := newTestDB(t)
	today := time.Now().UTC().Format("2006-01-02")
	addrs := make([]string, 0, maxSelectedEntities+5)
	for i := 0; i < maxSelectedEntities+5; i++ {
		addrs = append(addrs, fmt.Sprintf("g1addr%02d", i))
	}
	seedTransferEdge(t, db, "gnoland1", addrs[0], addrs[1], today, 10, 1)

	g, err := db.GetTransferGraph("gnoland1", 7, 0, 0, "", addrs)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	if len(g.Nodes) != maxSelectedEntities {
		t.Errorf("got %d nodes, want the cap of %d (extras silently dropped)", len(g.Nodes), maxSelectedEntities)
	}
}

func TestSelectedTransferGraphEmptySelectionReturnsEmpty(t *testing.T) {
	db := newTestDB(t)
	g, err := db.GetTransferGraph("gnoland1", 7, 0, 0, "", []string{})
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	if len(g.Nodes) != 0 || len(g.Edges) != 0 {
		t.Errorf("got %+v, want empty (an empty explicit selection is not the same as topN mode)", g)
	}
}

func TestSelectedCallerGraphRequiresBothEndpointsSelected(t *testing.T) {
	// caller_edges is bipartite: a selection of callers-only must yield zero
	// edges, since no realm was selected for any edge to land on.
	db := newTestDB(t)
	today := time.Now().UTC().Format("2006-01-02")
	seedCallerEdge(t, db, "gnoland1", "g1a", "gno.land/r/demo/foo", today, 5)

	g, err := db.GetCallerGraph("gnoland1", 7, 0, 0, []string{"g1a"})
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	if len(g.Edges) != 0 {
		t.Errorf("edges = %+v, want none (only a caller was selected, no realm)", g.Edges)
	}
	if len(g.Nodes) != 1 || g.Nodes[0].Type != "caller" {
		t.Errorf("nodes = %+v, want the one caller node, typed correctly even with zero edges", g.Nodes)
	}

	g2, err := db.GetCallerGraph("gnoland1", 7, 0, 0, []string{"g1a", "gno.land/r/demo/foo"})
	if err != nil {
		t.Fatalf("graph with both selected: %v", err)
	}
	if len(g2.Edges) != 1 {
		t.Errorf("edges = %+v, want 1 now that both endpoints are selected", g2.Edges)
	}
}

func TestSelectedCallerGraphTypesEntitiesCorrectly(t *testing.T) {
	db := newTestDB(t)
	g, err := db.GetCallerGraph("gnoland1", 7, 0, 0, []string{"g1a", "gno.land/r/demo/foo"})
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	byID := map[string]CallerGraphNode{}
	for _, n := range g.Nodes {
		byID[n.ID] = n
	}
	if byID["g1a"].Type != "caller" {
		t.Errorf("g1a typed as %q, want caller", byID["g1a"].Type)
	}
	if byID["gno.land/r/demo/foo"].Type != "realm" {
		t.Errorf("gno.land/r/demo/foo typed as %q, want realm", byID["gno.land/r/demo/foo"].Type)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test -run 'TestSelectedTransferGraph|TestSelectedCallerGraph' ./... -v`
Expected: FAIL to compile — the 5 updated call sites already need the signature to exist, and the new tests reference `maxSelectedEntities` and the new trailing params, none of which exist yet.

- [ ] **Step 4: Extract `edgesAmongSet` and refactor `topNTransferGraph`**

In `db.go`, replace `topNTransferGraph`'s edge-query block (the second SQL query, from `placeholders := strings.TrimSuffix(...)` through the `edgeRows` loop) with a call to a new shared helper. The full replacement:

```go
// edgesAmongSet collapses transfer_edges into edges where BOTH endpoints are
// in order, summing same-pair edges across days in the window. This is the
// query both the top-N ranking path and the explicit-selection path run —
// the only difference between them is how order was obtained: a ranking
// query's result, or the caller's explicit selection.
func (d *DB) edgesAmongSet(network, start string, order []string, minValue int64) ([]GraphEdge, error) {
	if len(order) == 0 {
		return []GraphEdge{}, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(order)), ",")
	args := make([]any, 0, 2+2*len(order)+1)
	args = append(args, network, start)
	for _, a := range order {
		args = append(args, a)
	}
	for _, a := range order {
		args = append(args, a)
	}
	args = append(args, minValue)

	q := fmt.Sprintf(`
		SELECT from_address, to_address, SUM(total_value), SUM(tx_count)
		FROM transfer_edges
		WHERE network = ? AND day >= ? AND from_address IN (%s) AND to_address IN (%s)
		GROUP BY from_address, to_address
		HAVING SUM(total_value) >= ?
		ORDER BY SUM(total_value) DESC`, placeholders, placeholders)

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var edges []GraphEdge
	for rows.Next() {
		var e GraphEdge
		if err := rows.Scan(&e.From, &e.To, &e.Value, &e.TxCount); err != nil {
			return nil, err
		}
		edges = append(edges, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if edges == nil {
		edges = []GraphEdge{}
	}
	return edges, nil
}
```

`topNTransferGraph` becomes:

```go
func (d *DB) topNTransferGraph(network, start string, topN int, minValue int64) (TransferGraph, error) {
	addrRows, err := d.db.Query(`
		SELECT addr, SUM(vol) FROM (
			SELECT from_address AS addr, total_value AS vol FROM transfer_edges WHERE network = ? AND day >= ?
			UNION ALL
			SELECT to_address AS addr, total_value AS vol FROM transfer_edges WHERE network = ? AND day >= ?
		) GROUP BY addr ORDER BY SUM(vol) DESC LIMIT ?`,
		network, start, network, start, topN)
	if err != nil {
		return TransferGraph{}, err
	}
	nodeVol := map[string]int64{}
	var order []string
	for addrRows.Next() {
		var addr string
		var vol int64
		if err := addrRows.Scan(&addr, &vol); err != nil {
			addrRows.Close()
			return TransferGraph{}, err
		}
		nodeVol[addr] = vol
		order = append(order, addr)
	}
	if err := addrRows.Err(); err != nil {
		addrRows.Close()
		return TransferGraph{}, err
	}
	addrRows.Close()

	if len(order) == 0 {
		return TransferGraph{Nodes: []GraphNode{}, Edges: []GraphEdge{}}, nil
	}

	edges, err := d.edgesAmongSet(network, start, order, minValue)
	if err != nil {
		return TransferGraph{}, err
	}

	nodes := make([]GraphNode, 0, len(order))
	for _, a := range order {
		nodes = append(nodes, GraphNode{ID: a, Volume: nodeVol[a]})
	}
	return TransferGraph{Nodes: nodes, Edges: edges}, nil
}
```

(This is the same logic as before, just calling the extracted helper instead of duplicating the query inline. The batch-4 tests covering `topNTransferGraph`'s behavior — `TestGetTransferGraphTopNKeepsOnlyEdgesBetweenTopAddresses` etc. — must still pass unmodified after this refactor; if any fails, the refactor introduced a behavior change and needs fixing, not the test.)

- [ ] **Step 5: Add `selectedTransferGraph` and wire `GetTransferGraph`**

```go
// maxSelectedEntities bounds an explicit selection (addresses or
// caller/realm entities) — extras are silently dropped, matching the
// existing topN-clamping convention rather than a 400 error.
const maxSelectedEntities = 20

// selectedTransferGraph returns the induced subgraph over exactly the given
// addresses.
func (d *DB) selectedTransferGraph(network, start string, addresses []string, minValue int64) (TransferGraph, error) {
	if len(addresses) > maxSelectedEntities {
		addresses = addresses[:maxSelectedEntities]
	}
	if len(addresses) == 0 {
		return TransferGraph{Nodes: []GraphNode{}, Edges: []GraphEdge{}}, nil
	}

	edges, err := d.edgesAmongSet(network, start, addresses, minValue)
	if err != nil {
		return TransferGraph{}, err
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(addresses)), ",")
	args := make([]any, 0, 4+len(addresses))
	args = append(args, network, start, network, start)
	for _, a := range addresses {
		args = append(args, a)
	}

	volRows, err := d.db.Query(fmt.Sprintf(`
		SELECT addr, COALESCE(SUM(sent), 0) + COALESCE(SUM(received), 0) FROM (
			SELECT from_address AS addr, total_value AS sent, 0 AS received FROM transfer_edges WHERE network = ? AND day >= ?
			UNION ALL
			SELECT to_address AS addr, 0 AS sent, total_value AS received FROM transfer_edges WHERE network = ? AND day >= ?
		) WHERE addr IN (%s)
		GROUP BY addr`, placeholders), args...)
	if err != nil {
		return TransferGraph{}, err
	}
	defer volRows.Close()

	vol := map[string]int64{}
	for volRows.Next() {
		var addr string
		var v int64
		if err := volRows.Scan(&addr, &v); err != nil {
			return TransferGraph{}, err
		}
		vol[addr] = v
	}
	if err := volRows.Err(); err != nil {
		return TransferGraph{}, err
	}

	nodes := make([]GraphNode, 0, len(addresses))
	for _, a := range addresses {
		nodes = append(nodes, GraphNode{ID: a, Volume: vol[a]}) // 0 if no matching activity in-window
	}
	return TransferGraph{Nodes: nodes, Edges: edges}, nil
}
```

Change `GetTransferGraph`'s signature and dispatch:

```go
// GetTransferGraph returns a scoped view of the value-transfer network:
// the top-N addresses by volume (topN mode), the 1-hop neighborhood of one
// address (ego mode), or the induced subgraph over an explicit address list
// (addresses mode — checked first, since an explicit selection is the most
// specific request). All three are bounded per query regardless of chain
// size, which is what lets this be shipped to the browser at all.
func (d *DB) GetTransferGraph(network string, days, topN int, minValue int64, ego string, addresses []string) (TransferGraph, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	start := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")

	if len(addresses) > 0 {
		return d.selectedTransferGraph(network, start, addresses, minValue)
	}
	if ego != "" {
		return d.egoTransferGraph(network, start, ego, minValue)
	}
	if topN <= 0 {
		topN = 100
	}
	if topN > 1000 {
		topN = 1000
	}
	return d.topNTransferGraph(network, start, topN, minValue)
}
```

- [ ] **Step 6: Add `selectedCallerGraph` and wire `GetCallerGraph`**

```go
// selectedCallerGraph returns the induced subgraph over exactly the given
// entities (a mix of caller addresses and realm paths). Edges require BOTH
// the caller and the realm to be in the selected set — caller_edges is
// bipartite, so an all-caller or all-realm selection yields zero edges by
// construction, not by bug.
func (d *DB) selectedCallerGraph(network, start string, entities []string, minCalls int) (CallerGraph, error) {
	if len(entities) > maxSelectedEntities {
		entities = entities[:maxSelectedEntities]
	}
	if len(entities) == 0 {
		return CallerGraph{Nodes: []CallerGraphNode{}, Edges: []CallerGraphEdge{}}, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(entities)), ",")
	args := make([]any, 0, 2+2*len(entities)+1)
	args = append(args, network, start)
	for _, e := range entities {
		args = append(args, e)
	}
	for _, e := range entities {
		args = append(args, e)
	}
	args = append(args, minCalls)

	q := fmt.Sprintf(`
		SELECT caller, pkg_path, SUM(calls)
		FROM caller_edges
		WHERE network = ? AND day >= ? AND caller IN (%s) AND pkg_path IN (%s)
		GROUP BY caller, pkg_path
		HAVING SUM(calls) >= ?
		ORDER BY SUM(calls) DESC`, placeholders, placeholders)

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return CallerGraph{}, err
	}
	defer rows.Close()

	callCount := map[string]int{}
	var edges []CallerGraphEdge
	for rows.Next() {
		var e CallerGraphEdge
		if err := rows.Scan(&e.Caller, &e.PkgPath, &e.Calls); err != nil {
			return CallerGraph{}, err
		}
		edges = append(edges, e)
		callCount[e.Caller] += e.Calls
		callCount[e.PkgPath] += e.Calls
	}
	if err := rows.Err(); err != nil {
		return CallerGraph{}, err
	}

	nodes := make([]CallerGraphNode, 0, len(entities))
	for _, e := range entities {
		// pkg_path values always look like "gno.land/r/..." (they contain a
		// slash); bech32 addresses never do. This mirrors how the rest of the
		// codebase already distinguishes the two without a lookup.
		typ := "caller"
		if strings.Contains(e, "/") {
			typ = "realm"
		}
		nodes = append(nodes, CallerGraphNode{ID: e, Type: typ, Calls: callCount[e]})
	}
	if edges == nil {
		edges = []CallerGraphEdge{}
	}
	return CallerGraph{Nodes: nodes, Edges: edges}, nil
}
```

Change `GetCallerGraph`'s signature and dispatch — add the check at the top of the function, before the existing `topN` defaulting:

```go
func (d *DB) GetCallerGraph(network string, days, topN, minCalls int, entities []string) (CallerGraph, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	start := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")

	if len(entities) > 0 {
		return d.selectedCallerGraph(network, start, entities, minCalls)
	}

	if topN <= 0 {
		topN = 200
	}
	if topN > 1000 {
		topN = 1000
	}
	// (the rest of the existing function body is unchanged from here —
	// the `start :=` line already above replaces the one currently
	// inside the function, so delete the old duplicate)
	...
```

Concretely: move the existing `start := time.Now().UTC()...` line (currently a few lines into the function body) up to right after the `d.mu.RUnlock()` defer, insert the `entities` check right after it, and delete the old duplicate `start :=` line further down. Everything else in the function body (the caller-ranking query, the edge query, the node assembly) is unchanged.

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test -run 'TestGetTransferGraph|TestGetCallerGraph|TestSelectedTransferGraph|TestSelectedCallerGraph' ./... -v`
Expected: PASS (all batch-4 graph tests plus the 5 new ones in this task — 15 total).

- [ ] **Step 8: Run the full gate**

```bash
gofmt -l . && go vet ./... && go test ./...
```

Expected: `api.go` will fail to compile at this point (`GetTransferGraph`/`GetCallerGraph` call sites there still use the old arity) — that's expected and fixed in Task 3. If the gate fails only on `api.go`'s call sites, proceed to Task 3 and commit both together; otherwise fix what broke in `db.go`/`db_test.go` first.

- [ ] **Step 9: Commit**

```bash
git add db.go db_test.go
git commit -m "feat: add selected-set graph queries for explicit address/entity selection"
```

---

## Task 3: API endpoints

**Files:**
- Modify: `api.go` — update `HandleGraphTransfers`, `HandleGraphCallers`; add `HandleGraphTransfersRanking`, `HandleGraphCallersRanking`
- Modify: `main.go` — 2 new routes
- Test: `api_test.go`

**Interfaces:**
- Consumes: `GetTransferRanking`, `GetCallerRanking` (Task 1); `GetTransferGraph`, `GetCallerGraph` new signatures (Task 2).
- Produces: `GET /api/graph/transfers/ranking`, `GET /api/graph/callers/ranking`; `addresses`/`entities` params on the two existing endpoints.

- [ ] **Step 1: Write the failing tests**

Append to `api_test.go`:

```go
func TestHandleGraphTransfersRankingSerializesEmptyAsArray(t *testing.T) {
	api := &API{db: newTestDB(t)}

	w := httptest.NewRecorder()
	api.HandleGraphTransfersRanking(w, httptest.NewRequest("GET", "/api/graph/transfers/ranking?network=gnoland1&window=90d", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var rows []TransferRankRow
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rows == nil {
		t.Error("rows is nil, want [] on an empty network")
	}
}

func TestHandleGraphTransfersRankingSearchParam(t *testing.T) {
	db := newTestDB(t)
	seedTransferEdge(t, db, "gnoland1", "g1needle", "g1b", time.Now().UTC().Format("2006-01-02"), 100, 1)
	api := &API{db: db}

	w := httptest.NewRecorder()
	api.HandleGraphTransfersRanking(w, httptest.NewRequest("GET", "/api/graph/transfers/ranking?network=gnoland1&window=90d&search=needle", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var rows []TransferRankRow
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 1 || rows[0].Address != "g1needle" {
		t.Errorf("rows = %+v, want just g1needle", rows)
	}
}

func TestHandleGraphCallersRankingSerializesEmptyAsArray(t *testing.T) {
	api := &API{db: newTestDB(t)}

	w := httptest.NewRecorder()
	api.HandleGraphCallersRanking(w, httptest.NewRequest("GET", "/api/graph/callers/ranking?network=gnoland1&window=90d", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var rows []CallerRankRow
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rows == nil {
		t.Error("rows is nil, want [] on an empty network")
	}
}

func TestHandleGraphTransfersAddressesParam(t *testing.T) {
	db := newTestDB(t)
	seedTransferEdge(t, db, "gnoland1", "g1a", "g1b", time.Now().UTC().Format("2006-01-02"), 100, 1)
	seedTransferEdge(t, db, "gnoland1", "g1a", "g1c", time.Now().UTC().Format("2006-01-02"), 999, 1)
	api := &API{db: db}

	w := httptest.NewRecorder()
	api.HandleGraphTransfers(w, httptest.NewRequest("GET", "/api/graph/transfers?network=gnoland1&window=90d&addresses=g1a,g1b", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body struct {
		Edges []GraphEdge `json:"edges"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Edges) != 1 {
		t.Errorf("edges = %+v, want 1 (only the g1a-g1b pair was selected)", body.Edges)
	}
}

func TestHandleGraphCallersEntitiesParam(t *testing.T) {
	db := newTestDB(t)
	seedCallerEdge(t, db, "gnoland1", "g1a", "gno.land/r/demo/foo", time.Now().UTC().Format("2006-01-02"), 5)
	api := &API{db: db}

	w := httptest.NewRecorder()
	api.HandleGraphCallers(w, httptest.NewRequest("GET", "/api/graph/callers?network=gnoland1&window=90d&entities=g1a,gno.land/r/demo/foo", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body struct {
		Edges []CallerGraphEdge `json:"edges"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Edges) != 1 {
		t.Errorf("edges = %+v, want 1", body.Edges)
	}
}

func TestHandleGraphTransfersAddressesTakesPrecedenceOverEgoAndTopN(t *testing.T) {
	// Sanity check on param precedence: addresses must win even if topN/ego
	// are also present on the query string (a stray leftover from a prior
	// request state, say), matching GetTransferGraph's documented dispatch
	// order.
	db := newTestDB(t)
	seedTransferEdge(t, db, "gnoland1", "g1a", "g1b", time.Now().UTC().Format("2006-01-02"), 100, 1)
	api := &API{db: db}

	w := httptest.NewRecorder()
	api.HandleGraphTransfers(w, httptest.NewRequest(
		"GET", "/api/graph/transfers?network=gnoland1&window=90d&topN=5&ego=g1a&addresses=g1a,g1b", nil))
	var body struct {
		Edges []GraphEdge `json:"edges"`
	}
	json.Unmarshal(w.Body.Bytes(), &body)
	if len(body.Edges) != 1 {
		t.Errorf("edges = %+v, want the addresses-mode result", body.Edges)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestHandleGraphTransfersRanking|TestHandleGraphCallersRanking|TestHandleGraphTransfersAddresses|TestHandleGraphCallersEntities' ./... -v`
Expected: FAIL to compile — `api.HandleGraphTransfersRanking undefined`, etc.

- [ ] **Step 3: Update the two existing handlers**

In `api.go`, replace `HandleGraphTransfers` and `HandleGraphCallers`:

```go
func (a *API) HandleGraphTransfers(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, _ := a.resolveTimeseriesParams(r, network)
	topN, _ := strconv.Atoi(r.URL.Query().Get("topN"))
	minValue, _ := strconv.ParseInt(r.URL.Query().Get("min_value"), 10, 64)
	ego := r.URL.Query().Get("ego")
	addresses := splitCommaList(r.URL.Query().Get("addresses"))

	g, err := a.db.GetTransferGraph(network, days, topN, minValue, ego, addresses)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if g.Nodes == nil {
		g.Nodes = []GraphNode{}
	}
	if g.Edges == nil {
		g.Edges = []GraphEdge{}
	}
	jsonResponse(w, g)
}

func (a *API) HandleGraphCallers(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, _ := a.resolveTimeseriesParams(r, network)
	topN, _ := strconv.Atoi(r.URL.Query().Get("topN"))
	minCalls, _ := strconv.Atoi(r.URL.Query().Get("min_calls"))
	entities := splitCommaList(r.URL.Query().Get("entities"))

	g, err := a.db.GetCallerGraph(network, days, topN, minCalls, entities)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if g.Nodes == nil {
		g.Nodes = []CallerGraphNode{}
	}
	if g.Edges == nil {
		g.Edges = []CallerGraphEdge{}
	}
	jsonResponse(w, g)
}

// splitCommaList splits a comma-separated query param into a trimmed,
// non-empty slice. An empty input string returns nil (not an empty slice),
// which callers rely on: GetTransferGraph/GetCallerGraph both treat a nil
// selection as "no explicit selection", falling through to ego/topN mode.
func splitCommaList(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
```

- [ ] **Step 4: Add the two ranking handlers**

After `HandleGraphCallers`:

```go
func (a *API) HandleGraphTransfersRanking(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, _ := a.resolveTimeseriesParams(r, network)
	topN, _ := strconv.Atoi(r.URL.Query().Get("topN"))
	search := strings.TrimSpace(r.URL.Query().Get("search"))

	rows, err := a.db.GetTransferRanking(network, days, topN, search)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if rows == nil {
		rows = []TransferRankRow{}
	}
	jsonResponse(w, rows)
}

func (a *API) HandleGraphCallersRanking(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, _ := a.resolveTimeseriesParams(r, network)
	topN, _ := strconv.Atoi(r.URL.Query().Get("topN"))
	search := strings.TrimSpace(r.URL.Query().Get("search"))

	rows, err := a.db.GetCallerRanking(network, days, topN, search)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if rows == nil {
		rows = []CallerRankRow{}
	}
	jsonResponse(w, rows)
}
```

Check `api.go` already imports `strings` (it does — used in `resolveTimeseriesParams`).

- [ ] **Step 5: Register the routes**

In `main.go`, beside the existing graph routes:

```go
	mux.HandleFunc("GET /api/graph/transfers/ranking", api.HandleGraphTransfersRanking)
	mux.HandleFunc("GET /api/graph/callers/ranking", api.HandleGraphCallersRanking)
```

Neither collides with a wildcard route (there is no `/api/graph/{path...}` registered), so no route-precedence test is needed.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test -run 'TestHandleGraphTransfersRanking|TestHandleGraphCallersRanking|TestHandleGraphTransfersAddresses|TestHandleGraphCallersEntities' ./... -v`
Expected: PASS (6 tests).

- [ ] **Step 7: Run the full gate**

```bash
gofmt -l . && go vet ./... && go test ./...
```

- [ ] **Step 8: Verify against real data**

```bash
go build -o /tmp/mygnoscan-sel . && cd /tmp && /tmp/mygnoscan-sel -listen :8899 -db /tmp/sel.db
```

Let it sync for a minute or two, then:

```bash
curl -s 'localhost:8899/api/graph/transfers/ranking?network=gnoland1&window=all&topN=20' | head -c 400
curl -s 'localhost:8899/api/graph/callers/ranking?network=gnoland1&window=all&topN=20' | head -c 400
```

Grab two addresses from the ranking response and re-query with an explicit selection:

```bash
curl -s 'localhost:8899/api/graph/transfers?network=gnoland1&window=all&addresses=<addr1>,<addr2>' | head -c 400
```

Expected: ranking calls return `[{address/id, ...}]` arrays; the selected-set call returns `{nodes, edges}` scoped to just the two given addresses. Stop the server.

- [ ] **Step 9: Commit**

```bash
git add api.go main.go api_test.go
git commit -m "feat: add ranking endpoints and explicit-selection graph params"
```

---

## Task 4: Shared `renderEntityPicker` component

**Files:**
- Modify: `frontend/index.html` — CSS additions; new `renderEntityPicker` function and a `debounce` helper

**Interfaces:**
- Consumes: `el()`, `dashTooltipNode`, `dashCompactNumber`, `DASH_PAL`, `dashApi`/`api` (all pre-existing).
- Produces: `function debounce(fn, ms)`; `function renderEntityPicker(host, opts)` where `opts` is:
  ```js
  {
    state,                              // { view: 'table'|'graph', selected: Set<string>, searchQuery: string }
    fetchRank: (topN) => Promise<row[]>,
    fetchSearch: (query) => Promise<row[]>,
    fetchGraph: (ids) => Promise<graphData>,
    columns: [{ key, label, format? }], // format?: (row) => string
    idOf: (row) => string,
    renderGraph: (canvasHost, graphData, selectedIds, onDeselect) => void,
    rerender: () => void,               // re-invokes renderEntityPicker itself; wired by the caller
  }
  ```
  This task builds and unit-tests-by-hand the component in isolation; Task 5 and Task 6 wire it into the three
  actual charts.

- [ ] **Step 1: Add CSS**

In `frontend/index.html`'s `<style>` block, beside the existing `.dash-seg`/`.dash-msg` rules:

```css
.entity-picker { display: flex; flex-direction: column; gap: 8px; }
.entity-search { width: 100%; max-width: 320px; background: var(--bg2); color: var(--fg); border: 1px solid var(--border); border-radius: 4px; padding: 5px 8px; font-family: var(--mono); font-size: 12px; }
.entity-tags { display: flex; flex-wrap: wrap; gap: 6px; min-height: 22px; }
.entity-tag { display: inline-flex; align-items: center; gap: 4px; background: var(--bg2); border: 1px solid var(--border); border-radius: 12px; padding: 2px 4px 2px 10px; font-size: 11px; }
.entity-tag button { background: none; border: none; color: var(--fg2); cursor: pointer; font-size: 13px; line-height: 1; padding: 2px 6px; }
.entity-tag button:hover { color: var(--fg); }
.entity-table-wrap { max-height: 320px; overflow-y: auto; border: 1px solid var(--border); border-radius: 4px; }
.entity-table { width: 100%; border-collapse: collapse; font-size: 12px; }
.entity-table th { position: sticky; top: 0; background: var(--bg2); text-align: left; padding: 6px 8px; border-bottom: 1px solid var(--border); }
.entity-table td { padding: 5px 8px; border-bottom: 1px solid var(--border); cursor: pointer; }
.entity-table tr.selected td { background: rgba(78,205,196,0.12); }
.entity-table tr:hover td { background: var(--bg2); }
.entity-actions { display: flex; justify-content: space-between; align-items: center; }
.entity-actions button { background: var(--accent); color: var(--bg); border: none; border-radius: 4px; padding: 6px 14px; font-family: var(--mono); font-size: 12px; font-weight: bold; cursor: pointer; }
.entity-actions button:disabled { background: var(--bg2); color: var(--fg2); cursor: not-allowed; }
.entity-actions .back { background: var(--bg2); color: var(--fg); }
```

- [ ] **Step 2: Add the `debounce` helper**

Near `dashCompactNumber` in `frontend/index.html`:

```js
function debounce(fn, ms) {
  let t;
  return (...args) => {
    clearTimeout(t);
    t = setTimeout(() => fn(...args), ms);
  };
}
```

- [ ] **Step 3: Implement `renderEntityPicker`**

Near `renderDashChart`:

```js
// renderEntityPicker drives the table+search+selection+"see graph" flow
// shared by the network section's three charts. It owns `host`'s entire
// DOM subtree and switches between a table view and each chart's own graph
// rendering (via opts.renderGraph, which is that chart's pre-existing
// ECharts-option-building code, unchanged).
async function renderEntityPicker(host, opts) {
  const st = opts.state;
  host.textContent = '';

  const tagsRow = el('div', { className: 'entity-tags' });
  st.selected.forEach(id => {
    const tag = el('span', { className: 'entity-tag' }, el('span', {}, id));
    const x = el('button', { type: 'button', 'aria-label': 'remove ' + id }, '×');
    x.addEventListener('click', () => {
      st.selected.delete(id);
      if (st.view === 'graph' && st.selected.size === 0) st.view = 'table';
      opts.rerender();
    });
    tag.appendChild(x);
    tagsRow.appendChild(tag);
  });
  host.appendChild(tagsRow);

  if (st.view === 'graph') {
    renderGraphView(host, opts);
    return;
  }
  renderTableView(host, opts);
}

async function renderTableView(host, opts) {
  const st = opts.state;

  const search = el('input', {
    className: 'entity-search',
    type: 'text',
    placeholder: 'search address or realm…',
    value: st.searchQuery || '',
  });
  const debouncedSearch = debounce(q => { st.searchQuery = q; opts.rerender(); }, 300);
  search.addEventListener('input', () => debouncedSearch(search.value.trim()));
  host.appendChild(search);

  const tableWrap = el('div', { className: 'entity-table-wrap' });
  const msg = el('div', { className: 'dash-msg' }, 'loading…');
  tableWrap.appendChild(msg);
  host.appendChild(tableWrap);

  const actions = el('div', { className: 'entity-actions' });
  const seeGraphBtn = el('button', { type: 'button' }, 'see graph (' + st.selected.size + ' selected)');
  seeGraphBtn.disabled = st.selected.size === 0;
  seeGraphBtn.addEventListener('click', () => { st.view = 'graph'; opts.rerender(); });
  actions.appendChild(seeGraphBtn);
  host.appendChild(actions);

  let rows;
  try {
    rows = st.searchQuery ? await opts.fetchSearch(st.searchQuery) : await opts.fetchRank(100);
  } catch (err) {
    tableWrap.textContent = '';
    tableWrap.appendChild(el('div', { className: 'dash-msg' }, 'could not load this table'));
    return;
  }

  tableWrap.textContent = '';
  if (!rows || rows.length === 0) {
    tableWrap.appendChild(el('div', { className: 'dash-msg' }, st.searchQuery ? 'no matches' : 'no data in this window'));
    return;
  }

  const table = el('table', { className: 'entity-table' });
  const thead = el('thead', {}, el('tr', {}, ...opts.columns.map(c => el('th', {}, c.label))));
  table.appendChild(thead);
  const tbody = el('tbody');
  rows.forEach(row => {
    const id = opts.idOf(row);
    const tr = el('tr', { className: st.selected.has(id) ? 'selected' : '' },
      ...opts.columns.map(c => el('td', {}, c.format ? c.format(row) : String(row[c.key]))));
    tr.addEventListener('click', () => {
      if (st.selected.has(id)) {
        st.selected.delete(id);
      } else if (st.selected.size < 20) {
        st.selected.add(id);
      }
      opts.rerender();
    });
    tbody.appendChild(tr);
  });
  table.appendChild(tbody);
  tableWrap.appendChild(table);
}

async function renderGraphView(host, opts) {
  const st = opts.state;

  const actions = el('div', { className: 'entity-actions' });
  const backBtn = el('button', { type: 'button', className: 'back' }, '← back to table');
  backBtn.addEventListener('click', () => { st.view = 'table'; opts.rerender(); });
  actions.appendChild(backBtn);
  host.appendChild(actions);

  const canvasHost = el('div', { className: 'dash-chart', style: { minHeight: '360px' } });
  host.appendChild(canvasHost);

  let graphData;
  try {
    graphData = await opts.fetchGraph(Array.from(st.selected));
  } catch (err) {
    canvasHost.appendChild(el('div', { className: 'dash-msg' }, 'could not load this chart'));
    return;
  }
  opts.renderGraph(canvasHost, graphData, st.selected, id => {
    st.selected.delete(id);
    if (st.selected.size === 0) st.view = 'table';
    opts.rerender();
  });
}
```

`el()`'s `attrs` handling (see its definition earlier in this file) calls `e.setAttribute(k, v)` for any key it doesn't special-case. `setAttribute('disabled', null)` sets the attribute to the literal string `"null"`, which browsers treat as present (truthy) — a disabled-when-null attempt passed through `el()`'s attrs would render the button permanently disabled. Setting `.disabled` as a boolean property after construction, as `seeGraphBtn.disabled = st.selected.size === 0` does above, avoids that trap entirely.

- [ ] **Step 4: Run the full gate**

This step is frontend-only; there is no Go code to test, but confirm nothing else broke:

```bash
gofmt -l . && go vet ./... && go test ./...
```

- [ ] **Step 5: Commit**

```bash
git add frontend/index.html
git commit -m "feat: add shared entity-picker table/search/selection component"
```

---

## Task 5: Wire `transfer-graph` and `token-flow-sankey`

**Files:**
- Modify: `frontend/index.html` — replace both charts' `fetch`/`opt` with `render`; remove the now-dead `transfer-graph` click handler in `renderDashChart`; update `onNetworkChange`

**Interfaces:**
- Consumes: `renderEntityPicker` (Task 4); `/api/graph/transfers/ranking`, `/api/graph/transfers?addresses=` (Tasks 1 & 3).
- Produces: nothing later tasks rely on (Task 6 is independent, for `caller-graph`).

- [ ] **Step 1: Replace `transfer-graph`'s config**

Replace the entire `transfer-graph` chart object (from `id: 'transfer-graph'` through its closing `},`) with:

```js
      {
        id: 'transfer-graph',
        title: 'value-transfer network',
        wide: true,
        networkScoped: true,
        state: { view: 'table', selected: new Set(), searchQuery: '' },
        why: 'Who sends GNOT to whom. Search or browse the ranking below, select up to 20 addresses (click a row, or the × on a tag to remove one), then "see graph" for the flows between exactly your selection. Node size is total volume in the window; edge width is the value moved between that pair.',
        render: function (host, controlsBar, state) {
          const chart = this;
          renderEntityPicker(host, {
            state,
            fetchRank: topN => dashApi('graph/transfers/ranking?topN=' + topN, chart.window),
            fetchSearch: q => dashApi('graph/transfers/ranking?search=' + encodeURIComponent(q), chart.window),
            fetchGraph: ids => dashApi('graph/transfers?addresses=' + ids.map(encodeURIComponent).join(','), chart.window),
            columns: [
              { key: 'address', label: 'address' },
              { key: 'sent', label: 'sent', format: r => dashCompactNumber(r.sent) },
              { key: 'received', label: 'received', format: r => dashCompactNumber(r.received) },
              { key: 'volume', label: 'volume', format: r => dashCompactNumber(r.volume) },
            ],
            idOf: r => r.address,
            renderGraph: (canvasHost, graph, selectedIds, onDeselect) => renderTransferForceGraph(canvasHost, graph, selectedIds, onDeselect),
            rerender: () => renderDashChart(chart, _dashGen),
          });
        },
      },
```

`dashApi(path, chartWindow)` appends `chart.window || _dashWindow` as the `window=` query param itself (see its definition earlier in this file) — passing `chart.window` here is exactly the pattern every other network-section fetch already uses, so the ranking calls stay consistent with `fetchGraph` above rather than needing their own window-resolution logic.

- [ ] **Step 2: Add `renderTransferForceGraph`**

This is `transfer-graph`'s old `opt` function, adapted to take the already-selected ID set (for highlighting) and an `onDeselect` callback (for the graph-view tag removal), and to call `echarts.init`/`setOption` itself since it's no longer running through `renderDashChart`'s pipeline:

```js
function renderTransferForceGraph(canvasHost, graph, selectedIds, onDeselect) {
  if (!graph.nodes || graph.nodes.length === 0) {
    canvasHost.appendChild(el('div', { className: 'dash-msg' }, 'no data for this selection'));
    return;
  }
  const maxVol = graph.nodes.reduce((m, n) => Math.max(m, n.volume), 1);
  const maxVal = graph.edges.reduce((m, e) => Math.max(m, e.value), 1);

  const existing = echarts.getInstanceByDom(canvasHost);
  if (existing) existing.dispose();
  const inst = echarts.init(canvasHost);
  try {
    inst.setOption(dashBase({
      tooltip: {
        formatter: p => dashTooltipNode(
          p.dataType === 'edge'
            ? [p.data.source + ' → ' + p.data.target, String(p.data.value) + ' ugnot']
            : [p.data.name, 'volume ' + p.data.value]
        ),
      },
      series: [{
        type: 'graph',
        layout: 'force',
        roam: true,
        draggable: true,
        force: { repulsion: 120, edgeLength: [40, 160] },
        label: { show: false },
        data: graph.nodes.map(n => ({
          id: n.id, name: n.id, value: n.volume,
          symbolSize: 8 + 24 * (n.volume / maxVol),
          itemStyle: { color: DASH_PAL[0] },
        })),
        edges: graph.edges.map(e => ({
          source: e.from, target: e.to, value: e.value,
          lineStyle: { width: 1 + 5 * (e.value / maxVal), color: '#3a3a3a', curveness: 0.1 },
        })),
      }],
    }), true);
  } catch (err) {
    console.error('setOption failed for transfer-graph', err);
    inst.dispose();
    canvasHost.appendChild(el('div', { className: 'dash-msg' }, 'could not load this chart'));
    return;
  }
  _dashCharts['transfer-graph'] = inst;
  inst.on('click', params => {
    if (params.dataType === 'node') onDeselect(params.data.id);
  });
}
```

Clicking a node now *removes* it from the selection (rather than the old ego-drill-down behavior) — a natural way to prune the graph without going back to the table, consistent with the tag row's `×`.

- [ ] **Step 3: Remove the now-dead click handler in `renderDashChart`**

`transfer-graph` no longer goes through `renderDashChart`'s `fetch`/`opt` path (it has `render` instead), so the hardcoded block in `renderDashChart`:

```js
  if (chart.id === 'transfer-graph') {
    inst.on('click', params => {
      if (params.dataType !== 'node') return;
      chart.state = chart.state || {};
      chart.state.ego = params.data.id;
      renderDashChart(chart, _dashGen);
    });
  }
```

is now unreachable dead code (verify this: `renderDashChart`'s `fetch`/`opt`/`setOption` branch never runs for a chart with `chart.render` set, per Task 4's step 5 change to `renderDashChart` — wait, that change is actually made in this step, see below). Delete this block entirely.

- [ ] **Step 4: Add the `chart.render` dispatch branch to `renderDashChart`**

At the very top of `renderDashChart`, right after the `if (!host) return;` line, add this branch. It must come before the existing `fetch`/`opt` path entirely (a `render`-based chart never runs that path), and it needs its own copy of the `networkScoped`/`all`-networks guard, since `renderEntityPicker` itself doesn't know about that convention:

```js
  if (chart.render) {
    if (chart.networkScoped && getNetwork() === 'all') {
      host.textContent = '';
      host.appendChild(el('div', { className: 'dash-msg' }, 'select a specific network to see this chart'));
      return;
    }
    const controlsBar = document.getElementById('dash-controls-' + chart.id);
    chart.state = chart.state || {};
    chart.render(host, controlsBar, chart.state);
    return;
  }
```

- [ ] **Step 5: Replace `token-flow-sankey`'s config**

Same shape as `transfer-graph`, sharing its ranking/selection data but rendering a sankey instead of a force graph:

```js
      {
        id: 'token-flow-sankey',
        title: 'token flow (top senders → receivers)',
        wide: true,
        networkScoped: true,
        state: { view: 'table', selected: new Set(), searchQuery: '' },
        why: 'Search or browse the ranking below, select up to 20 addresses, then "see graph" for a sankey of value moved between exactly your selection. Link width is value moved from sender to receiver.',
        render: function (host, controlsBar, state) {
          const chart = this;
          renderEntityPicker(host, {
            state,
            fetchRank: topN => dashApi('graph/transfers/ranking?topN=' + topN, chart.window),
            fetchSearch: q => dashApi('graph/transfers/ranking?search=' + encodeURIComponent(q), chart.window),
            fetchGraph: ids => dashApi('graph/transfers?addresses=' + ids.map(encodeURIComponent).join(','), chart.window),
            columns: [
              { key: 'address', label: 'address' },
              { key: 'sent', label: 'sent', format: r => dashCompactNumber(r.sent) },
              { key: 'received', label: 'received', format: r => dashCompactNumber(r.received) },
              { key: 'volume', label: 'volume', format: r => dashCompactNumber(r.volume) },
            ],
            idOf: r => r.address,
            renderGraph: (canvasHost, graph, selectedIds, onDeselect) => renderTokenFlowSankey(canvasHost, graph, selectedIds, onDeselect),
            rerender: () => renderDashChart(chart, _dashGen),
          });
        },
      },
```

- [ ] **Step 6: Add `renderTokenFlowSankey`**

The old `token-flow-sankey` `opt` function, adapted the same way as the force graph — self-contained `echarts.init`/`setOption`, plus a click handler on sankey nodes for tag-style deselection:

```js
function renderTokenFlowSankey(canvasHost, graph, selectedIds, onDeselect) {
  if (!graph.edges || graph.edges.length === 0) {
    canvasHost.appendChild(el('div', { className: 'dash-msg' }, 'no flow between this selection'));
    return;
  }
  const ids = new Set();
  graph.edges.forEach(e => { ids.add(e.from); ids.add(e.to); });
  // ECharts' sankey rejects any graph with a cycle — not just simple A<->B
  // mutual-send pairs, but longer cycles and self-loops (real chain data can
  // contain self-transfers). Dropping self-loops and orienting every
  // remaining edge along a fixed total order over addresses guarantees a DAG
  // regardless of how many edges or how long a cycle they'd otherwise form.
  const seen = new Map();
  graph.edges.forEach(e => {
    if (e.from === e.to) return;
    const [lo, hi] = [e.from, e.to].sort();
    const key = lo + '|' + hi;
    const cur = seen.get(key) || { from: lo, to: hi, value: 0 };
    cur.value += e.value;
    seen.set(key, cur);
  });
  if (seen.size === 0) {
    canvasHost.appendChild(el('div', { className: 'dash-msg' }, 'no flow between this selection'));
    return;
  }

  const existing = echarts.getInstanceByDom(canvasHost);
  if (existing) existing.dispose();
  const inst = echarts.init(canvasHost);
  try {
    inst.setOption(dashBase({
      tooltip: {
        formatter: p => dashTooltipNode([p.data.source + ' → ' + p.data.target, String(p.data.value) + ' ugnot']),
      },
      series: [{
        type: 'sankey',
        emphasis: { focus: 'adjacency' },
        data: Array.from(ids).map(id => ({ name: id })),
        links: Array.from(seen.values()).map(e => ({ source: e.from, target: e.to, value: e.value })),
        lineStyle: { color: 'gradient', curveness: 0.5 },
        itemStyle: { color: DASH_PAL[0], borderColor: '#2a2a2a' },
        label: { color: '#888', fontSize: 10 },
      }],
    }), true);
  } catch (err) {
    console.error('setOption failed for token-flow-sankey', err);
    inst.dispose();
    canvasHost.appendChild(el('div', { className: 'dash-msg' }, 'could not load this chart'));
    return;
  }
  _dashCharts['token-flow-sankey'] = inst;
  inst.on('click', params => {
    if (params.dataType === 'node') onDeselect(params.data.name);
  });
}
```

- [ ] **Step 7: Update `onNetworkChange`**

Replace the existing block:

```js
  // transfer-graph's drilled-into ego address lives on the module-level
  // DASHBOARDS array, so it survives a network switch even though it's
  // meaningless there (an address from network A almost certainly has no
  // edges on network B). Persisting it across a *window* change is correct
  // and intentional — only a network switch should reset it.
  const transferGraph = DASHBOARDS
    .flatMap(s => s.charts)
    .find(c => c.id === 'transfer-graph');
  if (transferGraph && transferGraph.state) transferGraph.state.ego = null;
```

with:

```js
  // The three network-section charts hold selection/view state on the
  // module-level DASHBOARDS array, so it survives a network switch even
  // though a selection from network A is almost certainly meaningless on
  // network B. Persisting it across a *window* change is correct and
  // intentional — only a network switch should reset it.
  ['transfer-graph', 'token-flow-sankey', 'caller-graph'].forEach(id => {
    const chart = DASHBOARDS.flatMap(s => s.charts).find(c => c.id === id);
    if (chart && chart.state) {
      chart.state.view = 'table';
      chart.state.selected = new Set();
      chart.state.searchQuery = '';
    }
  });
```

(`caller-graph` is included here even though it isn't rewired until Task 6 — this reset logic is generic across all three charts' identical state shape, and writing it once now avoids touching `onNetworkChange` again in Task 6.)

- [ ] **Step 8: Verify in the browser**

Rebuild, serve a database with bank sends, open `/dashboards?section=network` with a specific network selected.

1. Both `transfer-graph` and `token-flow-sankey` cards show a table with address/sent/received/volume columns, sorted by volume.
2. Typing in the search box (e.g. a partial address) narrows the table after a brief debounce; clearing it returns to the top-100 ranking.
3. Clicking a row selects it (visually highlighted) and adds a tag above the table with a `×`; clicking a selected row again deselects it. Selecting 21 distinct rows leaves the 21st unselected (the 20-cap).
4. "see graph" is disabled at zero selections; with 2+ selected, click it — the card switches to the force graph / sankey respectively, showing only the selected addresses and their interconnections.
5. In graph view, clicking a node (force graph) or a sankey node removes it from selection and, if that empties the selection, returns to the table view automatically. The `×` on a tag does the same from either view.
6. "back to table" returns to the table view without losing the selection — the tags and highlighted rows still reflect it.
7. Switch the network selector: both charts reset to the table view with an empty selection.
8. Console clean throughout.

- [ ] **Step 9: Commit**

```bash
git add frontend/index.html
git commit -m "feat: replace transfer-graph and token-flow-sankey with selectable entity picker"
```

---

## Task 6: Wire `caller-graph`; update docs

**Files:**
- Modify: `frontend/index.html` — replace `caller-graph`'s config
- Modify: `docs/api.md`

**Interfaces:**
- Consumes: `renderEntityPicker` (Task 4); `/api/graph/callers/ranking`, `/api/graph/callers?entities=` (Tasks 1 & 3).
- Produces: nothing later tasks rely on (final task).

- [ ] **Step 1: Replace `caller-graph`'s config**

Replace the entire `caller-graph` chart object with:

```js
      {
        id: 'caller-graph',
        title: 'caller → realm graph',
        wide: true,
        networkScoped: true,
        state: { view: 'table', selected: new Set(), searchQuery: '' },
        why: 'Search or browse callers and realms together, ranked by call volume. Select up to 20 (mix callers and realms — a selection of only one type shows no edges, since a caller only ever connects to a realm, never to another caller) then "see graph" for the calls between exactly your selection.',
        render: function (host, controlsBar, state) {
          const chart = this;
          renderEntityPicker(host, {
            state,
            fetchRank: topN => dashApi('graph/callers/ranking?topN=' + topN, chart.window),
            fetchSearch: q => dashApi('graph/callers/ranking?search=' + encodeURIComponent(q), chart.window),
            fetchGraph: ids => dashApi('graph/callers?entities=' + ids.map(encodeURIComponent).join(','), chart.window),
            columns: [
              { key: 'id', label: 'address / realm' },
              { key: 'type', label: 'type' },
              { key: 'calls', label: 'calls', format: r => dashCompactNumber(r.calls) },
            ],
            idOf: r => r.id,
            renderGraph: (canvasHost, graph, selectedIds, onDeselect) => renderCallerGraph(canvasHost, graph, selectedIds, onDeselect),
            rerender: () => renderDashChart(chart, _dashGen),
          });
        },
      },
```

- [ ] **Step 2: Add `renderCallerGraph`**

The old `caller-graph` `opt` function, adapted the same way as Task 5's two graphs:

```js
function renderCallerGraph(canvasHost, graph, selectedIds, onDeselect) {
  if (!graph.nodes || graph.nodes.length === 0) {
    canvasHost.appendChild(el('div', { className: 'dash-msg' }, 'no data for this selection'));
    return;
  }
  if (graph.edges.length === 0) {
    // caller_edges is bipartite: an all-caller or all-realm selection is a
    // legitimate zero-edge result, not a load failure — say so plainly
    // rather than rendering an empty WebGL canvas or the generic error.
    canvasHost.appendChild(el('div', { className: 'dash-msg' },
      'no calls between this selection — pick at least one caller and one realm'));
    return;
  }
  const maxN = graph.nodes.reduce((m, n) => Math.max(m, n.calls), 1);
  const maxE = graph.edges.reduce((m, e) => Math.max(m, e.calls), 1);

  const existing = echarts.getInstanceByDom(canvasHost);
  if (existing) existing.dispose();
  const inst = echarts.init(canvasHost);
  try {
    inst.setOption(dashBase({
      tooltip: {
        formatter: p => dashTooltipNode(
          p.dataType === 'edge'
            ? [p.data.source + ' → ' + p.data.target, p.data.value + ' calls']
            : [p.data.name, p.data.value + ' calls']
        ),
      },
      series: [{
        type: 'graphGL',
        layout: 'forceAtlas2',
        forceAtlas2: { steps: 5, stopThreshold: 1 },
        label: { show: false },
        data: graph.nodes.map(n => ({
          id: n.id, name: n.id, value: n.calls,
          symbolSize: 6 + 18 * (n.calls / maxN),
          itemStyle: { color: n.type === 'realm' ? DASH_PAL[0] : DASH_PAL[5] },
        })),
        edges: graph.edges.map(e => ({
          source: e.caller, target: e.pkg_path, value: e.calls,
          lineStyle: { width: 1 + 3 * (e.calls / maxE), color: '#3a3a3a' },
        })),
      }],
    }), true);
  } catch (err) {
    console.error('setOption failed for caller-graph', err);
    inst.dispose();
    canvasHost.appendChild(el('div', { className: 'dash-msg' }, 'could not load this chart'));
    return;
  }
  _dashCharts['caller-graph'] = inst;
  inst.on('click', params => {
    if (params.dataType === 'node') onDeselect(params.data.id);
  });
}
```

- [ ] **Step 3: Verify in the browser**

Rebuild, serve a database with calls, open `/dashboards?section=network`.

1. `caller-graph`'s table shows id/type/calls, mixing caller addresses and realm paths in one ranking.
2. Selecting only callers (no realm) and clicking "see graph" shows the explicit "pick at least one caller and one realm" message, not a blank canvas or a generic error.
3. Selecting at least one caller and one realm that have actually called each other renders the WebGL graph scoped to the selection.
4. Switching away from and back to the network section disposes the old WebGL instance (repeat ~10x, check devtools for a "too many active contexts" warning — none expected).
5. Console clean.

- [ ] **Step 4: Update `docs/api.md`**

Add/update entries:

- `GET /api/graph/transfers/ranking?network=&window=&topN=&search=` — ranks addresses by total volume (sent+received), split into `sent`/`received`/`volume`. `topN` defaults to 100, capped at 1000; ignored when `search` is set. `search` does a bounded (50-result) substring match against addresses outside the top ranking. Returns `[{address, sent, received, volume}]`.
- `GET /api/graph/callers/ranking?network=&window=&topN=&search=` — ranks callers and realms together by call volume (independently — a realm's rank reflects its own total, not just calls from top-ranked callers). Same `topN`/`search` semantics. Returns `[{id, type, calls}]` where `type` is `"caller"` or `"realm"`.
- `GET /api/graph/transfers` — new `addresses` param (comma-separated, capped at 20): when present, returns the induced subgraph over exactly that address list instead of top-N/ego mode. Takes precedence over `topN`/`ego` if all three are somehow present.
- `GET /api/graph/callers` — new `entities` param (comma-separated, capped at 20, mixing caller addresses and realm paths): same induced-subgraph behavior. Because `caller_edges` is bipartite, an all-caller or all-realm selection returns zero edges by design.

- [ ] **Step 5: Commit**

```bash
git add frontend/index.html docs/api.md
git commit -m "feat: replace caller-graph with selectable entity picker; update docs"
```

---

## Done when

- All three network-section charts default to a searchable, sortable-by-volume ranking table with row selection, a removable tag row, and a capped-at-20 selection
- "see graph" renders each chart's existing graph type (force graph, sankey, WebGL graph) scoped to exactly the selected entities, and "back to table" returns without losing the selection
- Search finds an address/realm outside the default top-100/top-200 ranking
- A network switch resets all three charts' view and selection; a window-picker change does not
- The caller-graph's bipartite zero-edge case shows an explicit message, not a blank canvas
- `gofmt -l .` is empty, `go vet ./...` and `go test ./...` pass
- `docs/api.md` reflects the two new ranking endpoints and the two new selection params
