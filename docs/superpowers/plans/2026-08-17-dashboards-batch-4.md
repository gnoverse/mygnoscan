# Dashboards Batch 4 — Network Graphs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Persist day-collapsed `transfer_edges` / `caller_edges` rollup tables from data the syncer already has locally, add two scope-aware graph endpoints doing window/top-N/ego/edge-collapse in SQL, and ship a new "network" dashboard section with a value-transfer force graph (click-to-focus ego drill-down), a token-flow sankey, and a caller→realm WebGL graph.

**Architecture:** Two new rollup tables filled by two new sync passes that read `bank_sends`/`calls` locally (no indexer fetch — both source tables are already fully synced). Two endpoints do top-N ranking / ego neighborhood / min-value filtering in SQL against the rollups. Three ECharts-based cards join a new "network" section.

**Tech Stack:** Go (stdlib + `modernc.org/sqlite`), vanilla JS with the repo's `el()` helper, ECharts 5 + echarts-gl from CDN. No bundler, no build step.

**Spec:** [`docs/superpowers/specs/2026-08-17-dashboards-batch-4-network-design.md`](../specs/2026-08-17-dashboards-batch-4-network-design.md)

## Global Constraints

- **Everything is network-scoped.** Every query, join and aggregate filters or groups by `network`. `networkParam` maps a missing `network` **and** `network=all` to `""`; a query that hardcodes `WHERE network = ?` returns nothing in that state.
- **The frontend builds DOM, never HTML strings.** Use `el()`. No `innerHTML` with interpolated data. `from_address`/`to_address`/`caller`/`pkg_path` are all attacker-controlled (any address or realm path a signer chooses).
- **No build step.** No bundler, no npm, no framework, no JS test runner.
- **Idempotent, additive upserts** — `INSERT ... ON CONFLICT` against declared keys, accumulating `total_value`/`tx_count`/`calls` rather than overwriting, since a rollup row can be revisited as new source rows land in an already-touched `(pair, day)` bucket.
- **Cursors derive from stored data** — `MAX(last_height)` already rolled into the table for that network, not separate `sync_state`.
- **Errors go up from query paths.** Only the sync loop logs and continues; these two passes are local aggregate queries, not indexer walks, so a failure returns an error rather than being swallowed.
- **Go gates before any commit:** `gofmt -l .` prints nothing, `go vet ./...` passes, `go test ./...` passes.
- **Commits are conventional and single-line. No co-author or attribution trailers.**
- **Go tests are table-driven** with a real temp SQLite file, never mocks.
- **Only successful transfers count.** The rollup filters `bank_sends` to `success = 1` — a failed `BankMsgSend` moved no value on chain, so including it in a value-transfer graph would misrepresent real fund flow.
- **`bank_sends.amount` is a decorated string** (e.g. `"1000000ugnot"`), not a bare integer. Parse it once, at rollup-write time, with the same expression `amountExpr` uses elsewhere in `db.go`: `CAST(REPLACE(REPLACE(amount, 'ugnot', ''), '"', '') AS INTEGER)`. `transfer_edges.total_value` then stores a plain integer.

---

## File Structure

| File | Change | Responsibility |
|---|---|---|
| `db.go` | Schema + queries | `transfer_edges`/`caller_edges` DDL; upserts + cursors; local rollup readers; graph queries |
| `db_test.go` | Tests | Accumulation, network isolation, cursor, malformed-day skip, top-N/ego/min-value graph shaping |
| `syncer.go` | Add `syncTransferEdges`, `syncCallerEdges` | Cursor, local read, upsert; wire into `SyncAll` |
| `syncer_test.go` | Tests | End-to-end rollup from seeded `bank_sends`/`calls`, cursor resume |
| `api.go` | Add 2 handlers | `/api/graph/transfers`, `/api/graph/callers` |
| `api_test.go` | Tests | Param parsing, empty-array serialization |
| `main.go` | Add 2 routes | |
| `frontend/index.html` | New "network" section, 3 charts, echarts-gl CDN tag | Force graph + ego drill-down, sankey, WebGL caller graph |
| `docs/api.md` | Update | New endpoint shapes |
| `docs/superpowers/specs/2026-08-13-chain-analytics-dashboards-design.md` | Update | Tick batch 4, resolve §9 renderer question |

---

## Task 1: Schema and rollup writers

**Files:**
- Modify: `db.go` (DDL in `initSchema`; new query group)
- Test: `db_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `type TransferEdgeRow struct { FromAddress, ToAddress, Day string; TotalValue int64; TxCount, LastHeight int }`
  - `type CallerEdgeRow struct { Caller, PkgPath, Day string; Calls, LastHeight int }`
  - `func (d *DB) UpsertTransferEdges(network string, rows []TransferEdgeRow) error`
  - `func (d *DB) UpsertCallerEdges(network string, rows []CallerEdgeRow) error`
  - `func (d *DB) TransferEdgesLastHeight(network string) (int, bool, error)`
  - `func (d *DB) CallerEdgesLastHeight(network string) (int, bool, error)`

- [ ] **Step 1: Write the failing tests**

Append to `db_test.go`:

```go
func TestUpsertTransferEdgesAccumulatesAcrossRuns(t *testing.T) {
	// The same (from, to, day) bucket can be revisited when a later sync pass
	// finds more bank_sends rows for a day it already partially rolled up.
	// Re-upserting must add, not overwrite.
	db := newTestDB(t)

	if err := db.UpsertTransferEdges("gnoland1", []TransferEdgeRow{
		{FromAddress: "g1a", ToAddress: "g1b", Day: "2026-08-10", TotalValue: 100, TxCount: 1, LastHeight: 10},
	}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := db.UpsertTransferEdges("gnoland1", []TransferEdgeRow{
		{FromAddress: "g1a", ToAddress: "g1b", Day: "2026-08-10", TotalValue: 50, TxCount: 1, LastHeight: 20},
	}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	var total, count, height int
	err := db.db.QueryRow(
		`SELECT total_value, tx_count, last_height FROM transfer_edges WHERE network = ? AND from_address = ? AND to_address = ? AND day = ?`,
		"gnoland1", "g1a", "g1b", "2026-08-10",
	).Scan(&total, &count, &height)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if total != 150 {
		t.Errorf("total_value = %d, want 150 (accumulated, not overwritten)", total)
	}
	if count != 2 {
		t.Errorf("tx_count = %d, want 2", count)
	}
	if height != 20 {
		t.Errorf("last_height = %d, want 20 (the max of the two runs)", height)
	}
}

func TestTransferEdgesLastHeightIsNetworkScoped(t *testing.T) {
	db := newTestDB(t)

	if _, ok, err := db.TransferEdgesLastHeight("gnoland1"); err != nil || ok {
		t.Fatalf("empty: ok = %v, err = %v; want ok=false, err=nil", ok, err)
	}

	if err := db.UpsertTransferEdges("gnoland1", []TransferEdgeRow{
		{FromAddress: "g1a", ToAddress: "g1b", Day: "2026-08-10", TotalValue: 1, TxCount: 1, LastHeight: 42},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Another network's higher height must not move this network's cursor.
	if err := db.UpsertTransferEdges("test12", []TransferEdgeRow{
		{FromAddress: "g1x", ToAddress: "g1y", Day: "2026-08-10", TotalValue: 1, TxCount: 1, LastHeight: 9999},
	}); err != nil {
		t.Fatalf("upsert other network: %v", err)
	}

	h, ok, err := db.TransferEdgesLastHeight("gnoland1")
	if err != nil || !ok {
		t.Fatalf("ok = %v, err = %v; want ok=true, err=nil", ok, err)
	}
	if h != 42 {
		t.Errorf("last height = %d, want 42 — another network's rows leaked in", h)
	}
}

func TestUpsertCallerEdgesAccumulatesAcrossRuns(t *testing.T) {
	db := newTestDB(t)

	if err := db.UpsertCallerEdges("gnoland1", []CallerEdgeRow{
		{Caller: "g1a", PkgPath: "gno.land/r/demo/foo", Day: "2026-08-10", Calls: 3, LastHeight: 10},
	}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := db.UpsertCallerEdges("gnoland1", []CallerEdgeRow{
		{Caller: "g1a", PkgPath: "gno.land/r/demo/foo", Day: "2026-08-10", Calls: 2, LastHeight: 20},
	}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	var calls, height int
	err := db.db.QueryRow(
		`SELECT calls, last_height FROM caller_edges WHERE network = ? AND caller = ? AND pkg_path = ? AND day = ?`,
		"gnoland1", "g1a", "gno.land/r/demo/foo", "2026-08-10",
	).Scan(&calls, &height)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if calls != 5 {
		t.Errorf("calls = %d, want 5 (accumulated, not overwritten)", calls)
	}
	if height != 20 {
		t.Errorf("last_height = %d, want 20", height)
	}
}

func TestCallerEdgesLastHeightIsNetworkScoped(t *testing.T) {
	db := newTestDB(t)

	if _, ok, err := db.CallerEdgesLastHeight("gnoland1"); err != nil || ok {
		t.Fatalf("empty: ok = %v, err = %v; want ok=false, err=nil", ok, err)
	}
	if err := db.UpsertCallerEdges("gnoland1", []CallerEdgeRow{
		{Caller: "g1a", PkgPath: "gno.land/r/demo/foo", Day: "2026-08-10", Calls: 1, LastHeight: 7},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := db.UpsertCallerEdges("test12", []CallerEdgeRow{
		{Caller: "g1x", PkgPath: "gno.land/r/demo/bar", Day: "2026-08-10", Calls: 1, LastHeight: 9999},
	}); err != nil {
		t.Fatalf("upsert other network: %v", err)
	}

	h, ok, err := db.CallerEdgesLastHeight("gnoland1")
	if err != nil || !ok {
		t.Fatalf("ok = %v, err = %v; want ok=true, err=nil", ok, err)
	}
	if h != 7 {
		t.Errorf("last height = %d, want 7 — another network's rows leaked in", h)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestUpsertTransferEdges|TestTransferEdgesLastHeight|TestUpsertCallerEdges|TestCallerEdgesLastHeight' ./... -v`

Expected: FAIL to compile — `undefined: TransferEdgeRow`, `db.UpsertTransferEdges undefined`, etc.

- [ ] **Step 3: Add the schema**

In `db.go`, inside `initSchema`'s DDL, after the `storage_events` table and before the `CREATE INDEX` statements:

```sql
		CREATE TABLE IF NOT EXISTS transfer_edges (
			network      TEXT NOT NULL DEFAULT 'gnoland1',
			from_address TEXT NOT NULL,
			to_address   TEXT NOT NULL,
			day          TEXT NOT NULL,
			total_value  INTEGER NOT NULL DEFAULT 0,
			tx_count     INTEGER NOT NULL DEFAULT 0,
			last_height  INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (network, from_address, to_address, day)
		);

		CREATE TABLE IF NOT EXISTS caller_edges (
			network     TEXT NOT NULL DEFAULT 'gnoland1',
			caller      TEXT NOT NULL,
			pkg_path    TEXT NOT NULL,
			day         TEXT NOT NULL,
			calls       INTEGER NOT NULL DEFAULT 0,
			last_height INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (network, caller, pkg_path, day)
		);
```

And with the other indexes:

```sql
		CREATE INDEX IF NOT EXISTS idx_transfer_edges_day  ON transfer_edges(network, day, total_value);
		CREATE INDEX IF NOT EXISTS idx_transfer_edges_from ON transfer_edges(network, from_address);
		CREATE INDEX IF NOT EXISTS idx_transfer_edges_to   ON transfer_edges(network, to_address);
		CREATE INDEX IF NOT EXISTS idx_caller_edges_day    ON caller_edges(network, day, calls);
		CREATE INDEX IF NOT EXISTS idx_caller_edges_pkg    ON caller_edges(network, pkg_path);
```

- [ ] **Step 4: Add the row types and writers**

In `db.go`, in a new contiguous block:

```go
// --- network graph rollups ---

// TransferEdgeRow is one day's worth of value transferred between one pair of
// addresses. LastHeight is the highest bank_sends.block_height folded into
// this row so far; the syncer derives its cursor from MAX(last_height) rather
// than separate state.
type TransferEdgeRow struct {
	FromAddress string
	ToAddress   string
	Day         string // 'YYYY-MM-DD'
	TotalValue  int64  // ugnot, already parsed out of bank_sends' decorated string
	TxCount     int
	LastHeight  int
}

// CallerEdgeRow is one day's worth of calls from one caller into one realm.
type CallerEdgeRow struct {
	Caller     string
	PkgPath    string
	Day        string
	Calls      int
	LastHeight int
}

// UpsertTransferEdges accumulates rows into transfer_edges. Additive, not
// overwriting: a (from, to, day) bucket can be revisited by a later pass that
// finds more bank_sends rows for a day it already partially rolled up.
func (d *DB) UpsertTransferEdges(network string, rows []TransferEdgeRow) error {
	if len(rows) == 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO transfer_edges (network, from_address, to_address, day, total_value, tx_count, last_height)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (network, from_address, to_address, day) DO UPDATE SET
			total_value = total_value + excluded.total_value,
			tx_count    = tx_count + excluded.tx_count,
			last_height = MAX(last_height, excluded.last_height)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, r := range rows {
		if _, err := stmt.Exec(network, r.FromAddress, r.ToAddress, r.Day, r.TotalValue, r.TxCount, r.LastHeight); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// TransferEdgesLastHeight is the highest bank_sends height already rolled
// into transfer_edges for this network; ok is false when the network has none.
func (d *DB) TransferEdgesLastHeight(network string) (int, bool, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var h sql.NullInt64
	err := d.db.QueryRow(
		`SELECT MAX(last_height) FROM transfer_edges WHERE network = ?`, network,
	).Scan(&h)
	if err != nil {
		return 0, false, err
	}
	if !h.Valid {
		return 0, false, nil
	}
	return int(h.Int64), true, nil
}

// UpsertCallerEdges accumulates rows into caller_edges. Additive, matching
// UpsertTransferEdges.
func (d *DB) UpsertCallerEdges(network string, rows []CallerEdgeRow) error {
	if len(rows) == 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO caller_edges (network, caller, pkg_path, day, calls, last_height)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (network, caller, pkg_path, day) DO UPDATE SET
			calls       = calls + excluded.calls,
			last_height = MAX(last_height, excluded.last_height)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, r := range rows {
		if _, err := stmt.Exec(network, r.Caller, r.PkgPath, r.Day, r.Calls, r.LastHeight); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CallerEdgesLastHeight is the highest calls height already rolled into
// caller_edges for this network; ok is false when the network has none.
func (d *DB) CallerEdgesLastHeight(network string) (int, bool, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var h sql.NullInt64
	err := d.db.QueryRow(
		`SELECT MAX(last_height) FROM caller_edges WHERE network = ?`, network,
	).Scan(&h)
	if err != nil {
		return 0, false, err
	}
	if !h.Valid {
		return 0, false, nil
	}
	return int(h.Int64), true, nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -run 'TestUpsertTransferEdges|TestTransferEdgesLastHeight|TestUpsertCallerEdges|TestCallerEdgesLastHeight' ./... -v`
Expected: PASS (4 tests).

- [ ] **Step 6: Run the full gate**

```bash
gofmt -l . && go vet ./... && go test ./...
```

- [ ] **Step 7: Commit**

```bash
git add db.go db_test.go
git commit -m "feat: add transfer_edges and caller_edges rollup tables"
```

---

## Task 2: Local rollup readers

**Files:**
- Modify: `db.go` — same `// --- network graph rollups ---` group
- Test: `db_test.go`

**Interfaces:**
- Consumes: `TransferEdgeRow`, `CallerEdgeRow` from Task 1; existing `bank_sends`/`calls` schemas.
- Produces:
  - `func (d *DB) RollupBankSendsSince(network string, sinceHeight int) ([]TransferEdgeRow, error)`
  - `func (d *DB) RollupCallsSince(network string, sinceHeight int) ([]CallerEdgeRow, error)`

- [ ] **Step 1: Write the failing tests**

Append to `db_test.go`:

```go
func TestRollupBankSendsSinceCollapsesByDayAndFiltersFailures(t *testing.T) {
	db := newTestDB(t)

	// Two sends same day/pair -> one bucket. One send a different day -> a
	// second bucket. One failed send -> excluded entirely (no value moved).
	if err := db.InsertBankSend("gnoland1", "TX1", 10, "2026-08-10T00:00:00Z", "g1a", "g1b", "100ugnot", true); err != nil {
		t.Fatalf("send 1: %v", err)
	}
	if err := db.InsertBankSend("gnoland1", "TX2", 11, "2026-08-10T05:00:00Z", "g1a", "g1b", "50ugnot", true); err != nil {
		t.Fatalf("send 2: %v", err)
	}
	if err := db.InsertBankSend("gnoland1", "TX3", 12, "2026-08-11T00:00:00Z", "g1a", "g1b", "25ugnot", true); err != nil {
		t.Fatalf("send 3: %v", err)
	}
	if err := db.InsertBankSend("gnoland1", "TX4", 13, "2026-08-11T01:00:00Z", "g1a", "g1b", "999ugnot", false); err != nil {
		t.Fatalf("failed send: %v", err)
	}

	rows, err := db.RollupBankSendsSince("gnoland1", 0)
	if err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rollup rows, want 2 (one per day)", len(rows))
	}
	byDay := map[string]TransferEdgeRow{}
	for _, r := range rows {
		byDay[r.Day] = r
	}
	if r := byDay["2026-08-10"]; r.TotalValue != 150 || r.TxCount != 2 || r.LastHeight != 11 {
		t.Errorf("2026-08-10 row = %+v, want total=150 count=2 height=11", r)
	}
	if r := byDay["2026-08-11"]; r.TotalValue != 25 || r.TxCount != 1 || r.LastHeight != 12 {
		t.Errorf("2026-08-11 row = %+v, want total=25 count=1 height=12 (failed send excluded)", r)
	}
}

func TestRollupBankSendsSinceRespectsCursor(t *testing.T) {
	db := newTestDB(t)
	if err := db.InsertBankSend("gnoland1", "TX1", 10, "2026-08-10T00:00:00Z", "g1a", "g1b", "100ugnot", true); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := db.InsertBankSend("gnoland1", "TX2", 20, "2026-08-11T00:00:00Z", "g1a", "g1b", "200ugnot", true); err != nil {
		t.Fatalf("send: %v", err)
	}

	rows, err := db.RollupBankSendsSince("gnoland1", 10)
	if err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if len(rows) != 1 || rows[0].TotalValue != 200 {
		t.Errorf("rows = %+v, want just the height-20 send (height > 10)", rows)
	}
}

func TestRollupBankSendsSinceSkipsMalformedBlockTime(t *testing.T) {
	// block_time is nullable TEXT compared as a string; a malformed value
	// yields a NULL day from date(), which must be skipped rather than
	// crashing the pass or corrupting a real day's bucket.
	db := newTestDB(t)
	if err := db.InsertBankSend("gnoland1", "TX1", 10, "2026-08-10T00:00:00Z", "g1a", "g1b", "100ugnot", true); err != nil {
		t.Fatalf("good send: %v", err)
	}
	if err := db.InsertBankSend("gnoland1", "TX2", 11, "not-a-timestamp", "g1a", "g1b", "999ugnot", true); err != nil {
		t.Fatalf("bad send: %v", err)
	}

	rows, err := db.RollupBankSendsSince("gnoland1", 0)
	if err != nil {
		t.Fatalf("rollup returned an error instead of skipping the bad row: %v", err)
	}
	if len(rows) != 1 || rows[0].TotalValue != 100 {
		t.Errorf("rows = %+v, want just the good row", rows)
	}
}

func TestRollupCallsSinceCollapsesByDay(t *testing.T) {
	db := newTestDB(t)
	if err := db.InsertCall("gnoland1", "TX1", 10, "2026-08-10T00:00:00Z", "g1a", "gno.land/r/demo/foo", "Post", true); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	if err := db.InsertCall("gnoland1", "TX2", 11, "2026-08-10T05:00:00Z", "g1a", "gno.land/r/demo/foo", "Edit", true); err != nil {
		t.Fatalf("call 2: %v", err)
	}
	if err := db.InsertCall("gnoland1", "TX3", 12, "2026-08-10T06:00:00Z", "g1a", "gno.land/r/demo/foo", "Post", false); err != nil {
		t.Fatalf("failed call: %v", err)
	}

	rows, err := db.RollupCallsSince("gnoland1", 0)
	if err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].Calls != 2 || rows[0].LastHeight != 11 {
		t.Errorf("row = %+v, want calls=2 height=11 (failed call excluded)", rows[0])
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestRollupBankSendsSince|TestRollupCallsSince' ./... -v`
Expected: FAIL to compile — `db.RollupBankSendsSince undefined`, `db.RollupCallsSince undefined`.

- [ ] **Step 3: Implement**

Append to the `// --- network graph rollups ---` group in `db.go`:

```go
// RollupBankSendsSince aggregates bank_sends rows with block_height >
// sinceHeight into day-collapsed transfer edges, for the syncer to fold into
// transfer_edges. Only successful sends count: a failed BankMsgSend moved no
// value on chain.
func (d *DB) RollupBankSendsSince(network string, sinceHeight int) ([]TransferEdgeRow, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.Query(`
		SELECT from_address, to_address, date(block_time) AS day,
		       COALESCE(SUM(CAST(REPLACE(REPLACE(amount, 'ugnot', ''), '"', '') AS INTEGER)), 0),
		       COUNT(*), MAX(block_height)
		FROM bank_sends
		WHERE network = ? AND block_height > ? AND success = 1
		GROUP BY from_address, to_address, day`, network, sinceHeight)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TransferEdgeRow
	for rows.Next() {
		var r TransferEdgeRow
		var day sql.NullString
		if err := rows.Scan(&r.FromAddress, &r.ToAddress, &day, &r.TotalValue, &r.TxCount, &r.LastHeight); err != nil {
			return nil, err
		}
		// A row whose block_time will not parse yields a NULL day from date();
		// skip it rather than corrupting a real bucket or failing the pass.
		if !day.Valid || day.String == "" {
			continue
		}
		r.Day = day.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// RollupCallsSince aggregates calls rows with block_height > sinceHeight into
// day-collapsed caller edges, for the syncer to fold into caller_edges.
func (d *DB) RollupCallsSince(network string, sinceHeight int) ([]CallerEdgeRow, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.Query(`
		SELECT caller, pkg_path, date(block_time) AS day, COUNT(*), MAX(block_height)
		FROM calls
		WHERE network = ? AND block_height > ? AND success = 1
		GROUP BY caller, pkg_path, day`, network, sinceHeight)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CallerEdgeRow
	for rows.Next() {
		var r CallerEdgeRow
		var day sql.NullString
		if err := rows.Scan(&r.Caller, &r.PkgPath, &day, &r.Calls, &r.LastHeight); err != nil {
			return nil, err
		}
		if !day.Valid || day.String == "" {
			continue
		}
		r.Day = day.String
		out = append(out, r)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -run 'TestRollupBankSendsSince|TestRollupCallsSince' ./... -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Run the full gate**

```bash
gofmt -l . && go vet ./... && go test ./...
```

- [ ] **Step 6: Commit**

```bash
git add db.go db_test.go
git commit -m "feat: add local bank_sends/calls rollup readers"
```

---

## Task 3: `syncTransferEdges` and `syncCallerEdges`

**Files:**
- Modify: `syncer.go` (two new functions; call from `SyncAll`)
- Test: `syncer_test.go`

**Interfaces:**
- Consumes: `RollupBankSendsSince`, `RollupCallsSince`, `UpsertTransferEdges`, `UpsertCallerEdges`, `TransferEdgesLastHeight`, `CallerEdgesLastHeight` from Tasks 1–2.
- Produces: `func (s *Syncer) syncTransferEdges(ctx context.Context) error`, `func (s *Syncer) syncCallerEdges(ctx context.Context) error`.

Unlike every prior sync pass, these do not touch the indexer or `walkTransactions` — they read local SQLite directly, since `bank_sends`/`calls` are already fully populated by `syncCalls`.

- [ ] **Step 1: Write the failing tests**

Append to `syncer_test.go`:

```go
func TestSyncTransferEdgesRollsUpAndResumes(t *testing.T) {
	s, _, db := newTestSyncer(t, "gnoland1")

	if err := db.InsertBankSend("gnoland1", "TX1", 10, "2026-08-10T00:00:00Z", "g1a", "g1b", "100ugnot", true); err != nil {
		t.Fatalf("seed send: %v", err)
	}
	if err := s.syncTransferEdges(context.Background()); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	h1, ok, err := db.TransferEdgesLastHeight("gnoland1")
	if err != nil || !ok || h1 != 10 {
		t.Fatalf("cursor after first pass = %d, ok=%v, err=%v; want 10, true, nil", h1, ok, err)
	}
	var total int64
	if err := db.db.QueryRow(`SELECT total_value FROM transfer_edges WHERE network = 'gnoland1' AND day = '2026-08-10'`).Scan(&total); err != nil {
		t.Fatalf("query: %v", err)
	}
	if total != 100 {
		t.Errorf("total_value = %d, want 100", total)
	}

	// A second pass with no new sends must be a no-op, not an error, and must
	// not move the cursor backward or double-count.
	if err := s.syncTransferEdges(context.Background()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	h2, _, _ := db.TransferEdgesLastHeight("gnoland1")
	if h2 != h1 {
		t.Errorf("cursor moved from %d to %d on an empty pass", h1, h2)
	}
	db.db.QueryRow(`SELECT total_value FROM transfer_edges WHERE network = 'gnoland1' AND day = '2026-08-10'`).Scan(&total)
	if total != 100 {
		t.Errorf("total_value = %d after empty second pass, want unchanged 100", total)
	}

	// A third send lands after the cursor: it must be picked up, and the
	// existing bucket must accumulate rather than reset.
	if err := db.InsertBankSend("gnoland1", "TX2", 15, "2026-08-10T06:00:00Z", "g1a", "g1b", "50ugnot", true); err != nil {
		t.Fatalf("seed send 2: %v", err)
	}
	if err := s.syncTransferEdges(context.Background()); err != nil {
		t.Fatalf("third pass: %v", err)
	}
	h3, _, _ := db.TransferEdgesLastHeight("gnoland1")
	if h3 != 15 {
		t.Errorf("cursor after third pass = %d, want 15", h3)
	}
	db.db.QueryRow(`SELECT total_value FROM transfer_edges WHERE network = 'gnoland1' AND day = '2026-08-10'`).Scan(&total)
	if total != 150 {
		t.Errorf("total_value = %d after third pass, want 150", total)
	}
}

func TestSyncCallerEdgesRollsUpAndResumes(t *testing.T) {
	s, _, db := newTestSyncer(t, "gnoland1")

	if err := db.InsertCall("gnoland1", "TX1", 10, "2026-08-10T00:00:00Z", "g1a", "gno.land/r/demo/foo", "Post", true); err != nil {
		t.Fatalf("seed call: %v", err)
	}
	if err := s.syncCallerEdges(context.Background()); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	h1, ok, err := db.CallerEdgesLastHeight("gnoland1")
	if err != nil || !ok || h1 != 10 {
		t.Fatalf("cursor = %d, ok=%v, err=%v; want 10, true, nil", h1, ok, err)
	}

	if err := s.syncCallerEdges(context.Background()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	h2, _, _ := db.CallerEdgesLastHeight("gnoland1")
	if h2 != h1 {
		t.Errorf("cursor moved from %d to %d on an empty pass", h1, h2)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestSyncTransferEdges|TestSyncCallerEdges' ./... -v`
Expected: FAIL to compile — `s.syncTransferEdges undefined`, `s.syncCallerEdges undefined`.

- [ ] **Step 3: Implement**

In `syncer.go`:

```go
// syncTransferEdges folds newly-synced bank_sends rows into transfer_edges.
//
// Unlike every other sync pass, this reads local SQLite rather than walking
// the indexer: bank_sends is already fully populated by syncCalls, so there
// is no fetch to make, only a local GROUP BY.
func (s *Syncer) syncTransferEdges(ctx context.Context) error {
	last, ok, err := s.db.TransferEdgesLastHeight(s.networkID)
	if err != nil {
		return fmt.Errorf("transfer edges cursor: %w", err)
	}
	from := 0
	if ok {
		from = last
	}

	rows, err := s.db.RollupBankSendsSince(s.networkID, from)
	if err != nil {
		return fmt.Errorf("rollup bank sends: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	if err := s.db.UpsertTransferEdges(s.networkID, rows); err != nil {
		return fmt.Errorf("upsert transfer edges: %w", err)
	}
	log.Printf("[%s] syncTransferEdges: rolled up %d edges", s.networkID, len(rows))
	return nil
}

// syncCallerEdges folds newly-synced calls rows into caller_edges. Same
// local-read shape as syncTransferEdges.
func (s *Syncer) syncCallerEdges(ctx context.Context) error {
	last, ok, err := s.db.CallerEdgesLastHeight(s.networkID)
	if err != nil {
		return fmt.Errorf("caller edges cursor: %w", err)
	}
	from := 0
	if ok {
		from = last
	}

	rows, err := s.db.RollupCallsSince(s.networkID, from)
	if err != nil {
		return fmt.Errorf("rollup calls: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	if err := s.db.UpsertCallerEdges(s.networkID, rows); err != nil {
		return fmt.Errorf("upsert caller edges: %w", err)
	}
	log.Printf("[%s] syncCallerEdges: rolled up %d edges", s.networkID, len(rows))
	return nil
}
```

`ctx` is unused in both bodies (the read is local, not against `s.client`) but kept in the signature for consistency with every other `func (s *Syncer) sync*(ctx context.Context) error` and so a future indexer-backed rewrite would not need to change callers.

- [ ] **Step 4: Call both from `SyncAll`**

In `SyncAll`, after the `syncStorageEvents` call and before the final `syncMsgRuns` call:

```go
	if err := s.syncStorageEvents(ctx); err != nil {
		return err
	}
	if err := s.syncTransferEdges(ctx); err != nil {
		return fmt.Errorf("syncTransferEdges error: %w", err)
	}
	if err := s.syncCallerEdges(ctx); err != nil {
		return fmt.Errorf("syncCallerEdges error: %w", err)
	}
	return s.syncMsgRuns(ctx)
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -run 'TestSyncTransferEdges|TestSyncCallerEdges' ./... -v`
Expected: PASS (2 tests).

- [ ] **Step 6: Run the full gate**

```bash
gofmt -l . && go vet ./... && go test ./...
```

- [ ] **Step 7: Commit**

```bash
git add syncer.go syncer_test.go
git commit -m "feat: roll up bank sends and calls into edge tables"
```

---

## Task 4: Graph queries

**Files:**
- Modify: `db.go` — new `// --- network graphs ---` group
- Test: `db_test.go`

**Interfaces:**
- Consumes: `transfer_edges`/`caller_edges` schema from Task 1.
- Produces:
  - `type GraphNode struct { ID string; Volume int64 }` — JSON `id`, `volume`
  - `type GraphEdge struct { From, To string; Value int64; TxCount int }` — JSON `from`, `to`, `value`, `tx_count`
  - `type TransferGraph struct { Nodes []GraphNode; Edges []GraphEdge }` — JSON `nodes`, `edges`
  - `type CallerGraphNode struct { ID, Type string; Calls int }` — JSON `id`, `type`, `calls`
  - `type CallerGraphEdge struct { Caller, PkgPath string; Calls int }` — JSON `caller`, `pkg_path`, `calls`
  - `type CallerGraph struct { Nodes []CallerGraphNode; Edges []CallerGraphEdge }` — JSON `nodes`, `edges`
  - `func (d *DB) GetTransferGraph(network string, days, topN int, minValue int64, ego string) (TransferGraph, error)`
  - `func (d *DB) GetCallerGraph(network string, days, topN, minCalls int) (CallerGraph, error)`

- [ ] **Step 1: Write the failing tests**

Append to `db_test.go`:

```go
func seedTransferEdge(t *testing.T, db *DB, network, from, to, day string, value int64, txCount int) {
	t.Helper()
	if err := db.UpsertTransferEdges(network, []TransferEdgeRow{
		{FromAddress: from, ToAddress: to, Day: day, TotalValue: value, TxCount: txCount, LastHeight: 1},
	}); err != nil {
		t.Fatalf("seed transfer edge: %v", err)
	}
}

func TestGetTransferGraphTopNKeepsOnlyEdgesBetweenTopAddresses(t *testing.T) {
	db := newTestDB(t)
	today := time.Now().UTC().Format("2006-01-02")

	// g1a and g1b are the two biggest by volume; g1c is small and trades only
	// with g1a. With topN=2, the g1a<->g1c edge must be dropped because g1c
	// did not make the top-N set, even though g1a did.
	seedTransferEdge(t, db, "gnoland1", "g1a", "g1b", today, 1000, 1)
	seedTransferEdge(t, db, "gnoland1", "g1b", "g1a", today, 900, 1)
	seedTransferEdge(t, db, "gnoland1", "g1a", "g1c", today, 5, 1)

	g, err := db.GetTransferGraph("gnoland1", 7, 2, 0, "")
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	if len(g.Nodes) != 2 {
		t.Fatalf("got %d nodes, want 2 (top-2 by volume)", len(g.Nodes))
	}
	if len(g.Edges) != 2 {
		t.Fatalf("got %d edges, want 2 (both directions between g1a/g1b only)", len(g.Edges))
	}
	for _, e := range g.Edges {
		if e.To == "g1c" || e.From == "g1c" {
			t.Errorf("edge %+v touches g1c, which is not in the top-N set", e)
		}
	}
}

func TestGetTransferGraphMinValueFiltersDustEdges(t *testing.T) {
	db := newTestDB(t)
	today := time.Now().UTC().Format("2006-01-02")
	seedTransferEdge(t, db, "gnoland1", "g1a", "g1b", today, 1000, 1)
	seedTransferEdge(t, db, "gnoland1", "g1a", "g1c", today, 5, 1)

	g, err := db.GetTransferGraph("gnoland1", 7, 100, 100, "")
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	if len(g.Edges) != 1 || g.Edges[0].Value != 1000 {
		t.Errorf("edges = %+v, want just the 1000-value edge (min_value=100 drops the 5-value one)", g.Edges)
	}
}

func TestGetTransferGraphEgoModeReturnsOneHopNeighborhoodOnly(t *testing.T) {
	db := newTestDB(t)
	today := time.Now().UTC().Format("2006-01-02")
	seedTransferEdge(t, db, "gnoland1", "g1a", "g1b", today, 100, 1) // direct neighbor
	seedTransferEdge(t, db, "gnoland1", "g1b", "g1c", today, 999, 1) // 2 hops from g1a — must not appear

	g, err := db.GetTransferGraph("gnoland1", 7, 0, 0, "g1a")
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	if len(g.Edges) != 1 {
		t.Fatalf("got %d edges, want 1 (only g1a's direct edge)", len(g.Edges))
	}
	if g.Edges[0].From != "g1a" && g.Edges[0].To != "g1a" {
		t.Errorf("edge %+v does not touch the ego address", g.Edges[0])
	}
	for _, e := range g.Edges {
		if e.From == "g1c" || e.To == "g1c" {
			t.Errorf("2-hop neighbor g1c leaked into a 1-hop ego view: %+v", e)
		}
	}
}

func TestGetTransferGraphIsNetworkScoped(t *testing.T) {
	db := newTestDB(t)
	today := time.Now().UTC().Format("2006-01-02")
	seedTransferEdge(t, db, "gnoland1", "g1a", "g1b", today, 100, 1)
	seedTransferEdge(t, db, "test12", "g1a", "g1b", today, 99999, 1)

	g, err := db.GetTransferGraph("gnoland1", 7, 10, 0, "")
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	if len(g.Edges) != 1 || g.Edges[0].Value != 100 {
		t.Errorf("edges = %+v, want just gnoland1's edge — test12 leaked in", g.Edges)
	}
}

func seedCallerEdge(t *testing.T, db *DB, network, caller, pkgPath, day string, calls int) {
	t.Helper()
	if err := db.UpsertCallerEdges(network, []CallerEdgeRow{
		{Caller: caller, PkgPath: pkgPath, Day: day, Calls: calls, LastHeight: 1},
	}); err != nil {
		t.Fatalf("seed caller edge: %v", err)
	}
}

func TestGetCallerGraphTopNAndNodeTypes(t *testing.T) {
	db := newTestDB(t)
	today := time.Now().UTC().Format("2006-01-02")
	seedCallerEdge(t, db, "gnoland1", "g1a", "gno.land/r/demo/foo", today, 50)
	seedCallerEdge(t, db, "gnoland1", "g1b", "gno.land/r/demo/foo", today, 1)

	g, err := db.GetCallerGraph("gnoland1", 7, 1, 0)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	if len(g.Edges) != 1 || g.Edges[0].Caller != "g1a" {
		t.Fatalf("edges = %+v, want just g1a's edge (top-1 caller)", g.Edges)
	}
	var sawCaller, sawRealm bool
	for _, n := range g.Nodes {
		if n.Type == "caller" {
			sawCaller = true
		}
		if n.Type == "realm" {
			sawRealm = true
		}
	}
	if !sawCaller || !sawRealm {
		t.Errorf("nodes = %+v, want at least one caller node and one realm node", g.Nodes)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestGetTransferGraph|TestGetCallerGraph' ./... -v`
Expected: FAIL to compile — `db.GetTransferGraph undefined`, `db.GetCallerGraph undefined`.

- [ ] **Step 3: Implement**

Append a new group to `db.go`:

```go
// --- network graphs ---

type GraphNode struct {
	ID     string `json:"id"`
	Volume int64  `json:"volume"`
}

type GraphEdge struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Value   int64  `json:"value"`
	TxCount int    `json:"tx_count"`
}

type TransferGraph struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

// GetTransferGraph returns a scoped view of the value-transfer network: the
// top-N addresses by volume in the window (ego == ""), or the 1-hop
// neighborhood of one address (ego set). Both are bounded per query
// regardless of chain size, which is what lets this be shipped to the
// browser at all — see the design's §7-derived scoping rule.
func (d *DB) GetTransferGraph(network string, days, topN int, minValue int64, ego string) (TransferGraph, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	start := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")

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

	// Parallel-edge collapse: same-pair edges across multiple days in the
	// window sum at read time. Both endpoints must be in the top-N set, or a
	// high-volume node would drag in every low-volume address it ever touched.
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

	edgeRows, err := d.db.Query(q, args...)
	if err != nil {
		return TransferGraph{}, err
	}
	defer edgeRows.Close()

	var edges []GraphEdge
	for edgeRows.Next() {
		var e GraphEdge
		if err := edgeRows.Scan(&e.From, &e.To, &e.Value, &e.TxCount); err != nil {
			return TransferGraph{}, err
		}
		edges = append(edges, e)
	}
	if err := edgeRows.Err(); err != nil {
		return TransferGraph{}, err
	}

	nodes := make([]GraphNode, 0, len(order))
	for _, a := range order {
		nodes = append(nodes, GraphNode{ID: a, Volume: nodeVol[a]})
	}
	if edges == nil {
		edges = []GraphEdge{}
	}
	return TransferGraph{Nodes: nodes, Edges: edges}, nil
}

func (d *DB) egoTransferGraph(network, start, ego string, minValue int64) (TransferGraph, error) {
	rows, err := d.db.Query(`
		SELECT from_address, to_address, SUM(total_value), SUM(tx_count)
		FROM transfer_edges
		WHERE network = ? AND day >= ? AND (from_address = ? OR to_address = ?)
		GROUP BY from_address, to_address
		HAVING SUM(total_value) >= ?
		ORDER BY SUM(total_value) DESC`, network, start, ego, ego, minValue)
	if err != nil {
		return TransferGraph{}, err
	}
	defer rows.Close()

	nodeVol := map[string]int64{}
	var edges []GraphEdge
	for rows.Next() {
		var e GraphEdge
		if err := rows.Scan(&e.From, &e.To, &e.Value, &e.TxCount); err != nil {
			return TransferGraph{}, err
		}
		edges = append(edges, e)
		other := e.To
		if e.From != ego {
			other = e.From
		}
		nodeVol[other] += e.Value
		nodeVol[ego] += e.Value
	}
	if err := rows.Err(); err != nil {
		return TransferGraph{}, err
	}

	nodes := make([]GraphNode, 0, len(nodeVol)+1)
	if _, ok := nodeVol[ego]; !ok {
		nodeVol[ego] = 0 // the ego node still renders even with zero matching edges
	}
	nodes = append(nodes, GraphNode{ID: ego, Volume: nodeVol[ego]})
	for addr, vol := range nodeVol {
		if addr == ego {
			continue
		}
		nodes = append(nodes, GraphNode{ID: addr, Volume: vol})
	}
	if edges == nil {
		edges = []GraphEdge{}
	}
	return TransferGraph{Nodes: nodes, Edges: edges}, nil
}

type CallerGraphNode struct {
	ID    string `json:"id"`
	Type  string `json:"type"` // "caller" | "realm"
	Calls int    `json:"calls"`
}

type CallerGraphEdge struct {
	Caller  string `json:"caller"`
	PkgPath string `json:"pkg_path"`
	Calls   int    `json:"calls"`
}

type CallerGraph struct {
	Nodes []CallerGraphNode `json:"nodes"`
	Edges []CallerGraphEdge `json:"edges"`
}

// GetCallerGraph returns the top-N callers by call volume in the window and
// the realms they called, with edges collapsed across days. No ego mode in
// this batch — extending it later is a straightforward addition behind the
// same shape, not required for the first ship.
func (d *DB) GetCallerGraph(network string, days, topN, minCalls int) (CallerGraph, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if topN <= 0 {
		topN = 200
	}
	if topN > 1000 {
		topN = 1000
	}
	start := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")

	callerRows, err := d.db.Query(`
		SELECT caller, SUM(calls) FROM caller_edges WHERE network = ? AND day >= ?
		GROUP BY caller ORDER BY SUM(calls) DESC LIMIT ?`, network, start, topN)
	if err != nil {
		return CallerGraph{}, err
	}
	callerCalls := map[string]int{}
	var callers []string
	for callerRows.Next() {
		var c string
		var n int
		if err := callerRows.Scan(&c, &n); err != nil {
			callerRows.Close()
			return CallerGraph{}, err
		}
		callerCalls[c] = n
		callers = append(callers, c)
	}
	if err := callerRows.Err(); err != nil {
		callerRows.Close()
		return CallerGraph{}, err
	}
	callerRows.Close()

	if len(callers) == 0 {
		return CallerGraph{Nodes: []CallerGraphNode{}, Edges: []CallerGraphEdge{}}, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(callers)), ",")
	args := make([]any, 0, 2+len(callers)+1)
	args = append(args, network, start)
	for _, c := range callers {
		args = append(args, c)
	}
	args = append(args, minCalls)

	q := fmt.Sprintf(`
		SELECT caller, pkg_path, SUM(calls)
		FROM caller_edges
		WHERE network = ? AND day >= ? AND caller IN (%s)
		GROUP BY caller, pkg_path
		HAVING SUM(calls) >= ?
		ORDER BY SUM(calls) DESC`, placeholders)

	edgeRows, err := d.db.Query(q, args...)
	if err != nil {
		return CallerGraph{}, err
	}
	defer edgeRows.Close()

	realmCalls := map[string]int{}
	var edges []CallerGraphEdge
	for edgeRows.Next() {
		var e CallerGraphEdge
		if err := edgeRows.Scan(&e.Caller, &e.PkgPath, &e.Calls); err != nil {
			return CallerGraph{}, err
		}
		edges = append(edges, e)
		realmCalls[e.PkgPath] += e.Calls
	}
	if err := edgeRows.Err(); err != nil {
		return CallerGraph{}, err
	}

	nodes := make([]CallerGraphNode, 0, len(callers)+len(realmCalls))
	for _, c := range callers {
		nodes = append(nodes, CallerGraphNode{ID: c, Type: "caller", Calls: callerCalls[c]})
	}
	for pkg, n := range realmCalls {
		nodes = append(nodes, CallerGraphNode{ID: pkg, Type: "realm", Calls: n})
	}
	if edges == nil {
		edges = []CallerGraphEdge{}
	}
	return CallerGraph{Nodes: nodes, Edges: edges}, nil
}
```

Check the top of `db.go` already imports `strings`; if not, add it to the `import` block.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -run 'TestGetTransferGraph|TestGetCallerGraph' ./... -v`
Expected: PASS (6 tests).

- [ ] **Step 5: Run the full gate**

```bash
gofmt -l . && go vet ./... && go test ./...
```

- [ ] **Step 6: Commit**

```bash
git add db.go db_test.go
git commit -m "feat: add transfer and caller graph queries"
```

---

## Task 5: Endpoints and routes

**Files:**
- Modify: `api.go` — add `HandleGraphTransfers`, `HandleGraphCallers`
- Modify: `main.go` — two routes
- Test: `api_test.go`

**Interfaces:**
- Consumes: `GetTransferGraph`, `GetCallerGraph` from Task 4; `a.networkParam(r)`, `a.resolveTimeseriesParams(r, network)`, `jsonResponse`, `jsonError`.
- Produces: `GET /api/graph/transfers`, `GET /api/graph/callers`.

- [ ] **Step 1: Write the failing tests**

Append to `api_test.go`:

```go
func TestHandleGraphTransfersSerializesEmptyAsArrays(t *testing.T) {
	api := &API{db: newTestDB(t)}

	w := httptest.NewRecorder()
	api.HandleGraphTransfers(w, httptest.NewRequest("GET", "/api/graph/transfers?network=gnoland1&window=90d", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body struct {
		Nodes []GraphNode `json:"nodes"`
		Edges []GraphEdge `json:"edges"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Nodes == nil || body.Edges == nil {
		t.Errorf("nodes/edges = %v / %v, want [] not null on an empty network", body.Nodes, body.Edges)
	}
}

func TestHandleGraphTransfersEgoParam(t *testing.T) {
	db := newTestDB(t)
	seedTransferEdge(t, db, "gnoland1", "g1a", "g1b", time.Now().UTC().Format("2006-01-02"), 100, 1)
	api := &API{db: db}

	w := httptest.NewRecorder()
	api.HandleGraphTransfers(w, httptest.NewRequest("GET", "/api/graph/transfers?network=gnoland1&window=90d&ego=g1a", nil))
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
		t.Errorf("edges = %+v, want 1 (g1a's ego neighborhood)", body.Edges)
	}
}

func TestHandleGraphCallersSerializesEmptyAsArrays(t *testing.T) {
	api := &API{db: newTestDB(t)}

	w := httptest.NewRecorder()
	api.HandleGraphCallers(w, httptest.NewRequest("GET", "/api/graph/callers?network=gnoland1&window=90d", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body struct {
		Nodes []CallerGraphNode `json:"nodes"`
		Edges []CallerGraphEdge `json:"edges"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Nodes == nil || body.Edges == nil {
		t.Errorf("nodes/edges = %v / %v, want [] not null on an empty network", body.Nodes, body.Edges)
	}
}
```

Check `api_test.go`'s existing imports already include `encoding/json` and `time`; add them if not.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestHandleGraphTransfers|TestHandleGraphCallers' ./... -v`
Expected: FAIL to compile — `api.HandleGraphTransfers undefined`, `api.HandleGraphCallers undefined`.

- [ ] **Step 3: Implement the handlers**

In `api.go`, near the other graph/storage handlers:

```go
func (a *API) HandleGraphTransfers(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, _ := a.resolveTimeseriesParams(r, network)
	topN, _ := strconv.Atoi(r.URL.Query().Get("topN"))
	minValue, _ := strconv.ParseInt(r.URL.Query().Get("min_value"), 10, 64)
	ego := r.URL.Query().Get("ego")

	g, err := a.db.GetTransferGraph(network, days, topN, minValue, ego)
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

	g, err := a.db.GetCallerGraph(network, days, topN, minCalls)
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
```

- [ ] **Step 4: Register the routes**

In `main.go`, beside the other graph-adjacent routes:

```go
	mux.HandleFunc("GET /api/graph/transfers", api.HandleGraphTransfers)
	mux.HandleFunc("GET /api/graph/callers", api.HandleGraphCallers)
```

Neither path collides with an existing wildcard route (`/api/storage/{path...}` and `/api/events/{path...}` are the only wildcards registered, and `/api/graph/...` doesn't fall under either), so no route-precedence test is needed here — unlike batch 3's `/api/storage/consumers`.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -run 'TestHandleGraphTransfers|TestHandleGraphCallers' ./... -v`
Expected: PASS (3 tests).

- [ ] **Step 6: Run the full gate**

```bash
gofmt -l . && go vet ./... && go test ./...
```

- [ ] **Step 7: Verify against real data**

```bash
go build -o /tmp/mygnoscan-b4 . && cd /tmp && /tmp/mygnoscan-b4 -listen :8899 -db /tmp/b4.db
```

Let it sync for a minute or two (enough for at least one bank send and one call to land), then:

```bash
curl -s 'localhost:8899/api/graph/transfers?network=gnoland1&window=all&topN=20' | head -c 400
curl -s 'localhost:8899/api/graph/callers?network=gnoland1&window=all&topN=20' | head -c 400
```

Grab one address from the transfers response's `nodes` and re-query in ego mode:

```bash
curl -s 'localhost:8899/api/graph/transfers?network=gnoland1&window=all&ego=<address>' | head -c 400
```

Expected: both top-N calls return `{nodes:[...], edges:[...]}` with numeric `value`/`volume`/`calls`; the ego call returns only edges touching that address. Stop the server.

- [ ] **Step 8: Commit**

```bash
git add api.go main.go api_test.go
git commit -m "feat: add transfer and caller graph endpoints"
```

---

## Task 6: Value-transfer force graph and token-flow sankey

**Files:**
- Modify: `frontend/index.html` — CDN script tag; new `network` section with two charts

**Interfaces:**
- Consumes: `/api/graph/transfers` from Task 5; `dashApi`, `dashBase`, `DASH_PAL`, `el`, `DASHBOARDS`, `renderDashChart`, `_dashCharts`, `_dashGen`.
- Produces: chart ids `transfer-graph`, `token-flow-sankey`; a fourth entry in `DASHBOARDS` with `id: 'network'`.

- [ ] **Step 1: Add the `echarts-gl` CDN tag**

In `frontend/index.html`, beside the existing `echarts@5` script tag (needed now so Task 7's `graphGL` series type is registered; loading both together avoids a second deploy touching this line):

```html
<script src="https://cdn.jsdelivr.net/npm/echarts@5/dist/echarts.min.js"></script>
<script src="https://cdn.jsdelivr.net/npm/echarts-gl@2/dist/echarts-gl.min.js"></script>
```

- [ ] **Step 2: Add the `network` section with the force graph**

After the `realms` section's closing `},` and before the `DASHBOARDS` array's closing `];`:

```js
  {
    id: 'network',
    title: 'network',
    charts: [
      {
        id: 'transfer-graph',
        title: 'value-transfer network',
        wide: true,
        mode: 'B',
        networkScoped: true,
        state: { ego: null },
        why: 'Who sends GNOT to whom. Node size is total volume in the window; edge width is the value moved between that pair. Starts on the top 100 addresses by volume — click a node to drill into its direct counterparties only (1 hop), and use "back to overview" to return.',
        controls: (bar, rerender, state) => {
          if (!state.ego) return;
          const btn = el('button', { type: 'button' }, '← back to overview');
          btn.addEventListener('click', () => { state.ego = null; rerender(); });
          bar.appendChild(btn);
        },
        fetch: function (w) {
          this.state = this.state || {};
          const q = this.state.ego
            ? 'ego=' + encodeURIComponent(this.state.ego)
            : 'topN=100';
          return dashApi('graph/transfers?' + q, w);
        },
        opt: function (graph) {
          const maxVol = graph.nodes.reduce((m, n) => Math.max(m, n.volume), 1);
          const maxVal = graph.edges.reduce((m, e) => Math.max(m, e.value), 1);
          const chart = this;
          return dashBase({
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
              // Node size scales 8-32px by volume share; a flat radius would
              // make every address look equally significant.
              data: graph.nodes.map(n => ({
                id: n.id, name: n.id, value: n.volume,
                symbolSize: 8 + 24 * (n.volume / maxVol),
                itemStyle: { color: n.id === chart.state.ego ? DASH_PAL[1] : DASH_PAL[0] },
              })),
              edges: graph.edges.map(e => ({
                source: e.from, target: e.to, value: e.value,
                lineStyle: { width: 1 + 5 * (e.value / maxVal), color: '#3a3a3a', curveness: 0.1 },
              })),
            }],
          });
        },
      },
      {
        id: 'token-flow-sankey',
        title: 'token flow (top senders → receivers)',
        wide: true,
        mode: 'B',
        networkScoped: true,
        why: 'The same top-100-by-volume subgraph as the network graph above, reshaped as a sankey: link width is value moved from sender to receiver. Reads best for spotting where GNOT concentrates — hubs show up as wide bands.',
        fetch: w => dashApi('graph/transfers?topN=100', w),
        opt: graph => {
          const ids = new Set();
          graph.edges.forEach(e => { ids.add(e.from); ids.add(e.to); });
          // ECharts' sankey rejects a graph with a cycle. address A -> B and
          // B -> A both existing is common (mutual sends) and would otherwise
          // throw; keep only the heavier direction of each pair.
          const seen = new Map();
          graph.edges.forEach(e => {
            const key = [e.from, e.to].sort().join('|');
            const cur = seen.get(key);
            if (!cur || e.value > cur.value) seen.set(key, e);
          });
          return dashBase({
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
          });
        },
      },
    ],
  },
];
```

All three network-section charts set `mode: 'B'`. `renderDashChart` calls `trimLeadingEmptyRows(rows)` — which
calls `rows.findIndex(...)` — on anything that isn't `mode: 'B'`. These charts' `fetch` resolves to a
`{nodes, edges}` object rather than an array of time-series points, so without `mode: 'B'` the very first render
throws `rows.findIndex is not a function`. This is the same reason the activity heatmap and function-call
heatmap (both non-time-series shapes) are `mode: 'B'` already. One side effect, matching those existing mode-B
charts: `renderDashChart`'s generic `!rows || rows.length === 0` empty-check can't see into an object's contents
either (`rows.length` is `undefined`, so the check never fires), so an empty `{nodes: [], edges: []}` result
renders as a blank graph/sankey canvas rather than the "no data in this window" message — acceptable, and
consistent with how the existing mode-B charts already handle emptiness.

- [ ] **Step 3: Wire the force graph's click-to-focus**

ECharts graph series emit a `click` event through the chart instance, not through `opt`. In `renderDashChart`, right after `_dashCharts[chart.id] = inst;` (the line that registers a freshly-created instance — see the `echarts.init` call from Task-adjacent code), add a chart-specific hook:

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

This mirrors the section's existing per-chart special-casing rather than adding a generic `onClick` field to every chart config, since no other chart in the codebase needs a click handler yet — see `AGENTS.md`'s YAGNI guidance against speculative generality.

- [ ] **Step 4: Verify in the browser**

Rebuild, serve a database with bank sends, open `/dashboards?section=network` with a specific network selected.

1. Both cards render. The force graph shows up to 100 nodes; the sankey shows the same edge set as flowing bands.
2. Clicking a node in the force graph re-renders it scoped to that node's direct counterparties, shows a "back to overview" button, and highlights the ego node in a different color.
3. Clicking "back to overview" returns to the top-100 view and the button disappears.
4. On the "all" network selector, both cards show the select-a-network message.
5. **XSS probe**, since `from_address`/`to_address` are attacker-controlled strings: insert a fake send whose address is a payload, into a `/tmp` copy of the database (never the repo's own):

```bash
sqlite3 /tmp/b4.db "INSERT INTO bank_sends (network, tx_hash, block_height, block_time, from_address, to_address, amount, success) VALUES ('gnoland1','TXXSS',999999,strftime('%Y-%m-%dT%H:%M:%SZ','now'),'<img src=x onerror=\"window.__XSS_FIRED=1\">','g1b','1000ugnot',1);"
sqlite3 /tmp/b4.db "DELETE FROM transfer_edges WHERE network='gnoland1';"  # force a re-roll on next sync
```

Restart the server against `/tmp/b4.db`, let it sync, reload `/dashboards?section=network`, hover both the force-graph node and the sankey band for that address, and assert `window.__XSS_FIRED` is `undefined`, `document.images.length` is unchanged, and the payload renders as literal text in both tooltips (both go through `dashTooltipNode`, per the global constraint).
6. Console clean.

- [ ] **Step 5: Commit**

```bash
git add frontend/index.html
git commit -m "feat: add value-transfer force graph and token-flow sankey"
```

---

## Task 7: Caller→realm WebGL graph, docs, and roadmap

**Files:**
- Modify: `frontend/index.html` — third chart in the `network` section
- Modify: `docs/api.md`
- Modify: `docs/superpowers/specs/2026-08-13-chain-analytics-dashboards-design.md`

**Interfaces:**
- Consumes: `/api/graph/callers` from Task 5; `echarts-gl`'s `graphGL` series (CDN tag from Task 6); `dashApi`, `dashBase`, `DASH_PAL`.
- Produces: chart id `caller-graph`.

- [ ] **Step 1: Append the chart**

In the `network` section's `charts` array, after `token-flow-sankey`:

```js
      {
        id: 'caller-graph',
        title: 'caller → realm graph',
        wide: true,
        mode: 'B',
        networkScoped: true,
        why: 'Who calls into which realms. Blue nodes are calling addresses, teal nodes are realms; edge width is call volume. Rendered in WebGL (no ego drill-down yet) since this graph can have more nodes than the force-directed canvas view above comfortably lays out.',
        fetch: w => dashApi('graph/callers?topN=300', w),
        opt: graph => {
          const maxN = graph.nodes.reduce((m, n) => Math.max(m, n.calls), 1);
          const maxE = graph.edges.reduce((m, e) => Math.max(m, e.calls), 1);
          return dashBase({
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
          });
        },
      },
```

- [ ] **Step 2: Verify the WebGL instance disposes on section leave**

`destroyDashCharts()` already calls `.dispose()` on every entry in `_dashCharts` regardless of series type, and `echarts.init(host)` works the same way whether or not `graphGL` is registered — no `graphGL`-specific lifecycle code is needed as long as the CDN tag from Task 6 loaded before this chart renders. Confirm this live: open `/dashboards?section=network`, then switch to `/dashboards?section=pulse` and back.

1. On first load of the `network` section, `caller-graph` renders and `_dashCharts['caller-graph'].isDisposed()` is `false`.
2. After switching away and back, the old instance is disposed (no leaked WebGL context — check the browser's dev tools for a "too many active WebGL contexts" warning after repeating the switch ~10 times) and a fresh instance renders.
3. Console clean; no `typeof echarts === 'undefined'` fallback message shown (the CDN script loaded).

- [ ] **Step 3: Update `docs/api.md`**

Add entries for the two new endpoints:

- `GET /api/graph/transfers?network=&window=&topN=&min_value=&ego=&hops=` — top-N addresses by transfer volume in the window (or the 1-hop neighborhood of `ego`, which ignores `topN`). Returns `{nodes: [{id, volume}], edges: [{from, to, value, tx_count}]}`. `topN` defaults to 100, capped at 1000. `hops` is accepted but only `1` has an effect in this batch.
- `GET /api/graph/callers?network=&window=&topN=&min_calls=` — top-N callers by call volume and the realms they called. Returns `{nodes: [{id, type, calls}], edges: [{caller, pkg_path, calls}]}` where `type` is `"caller"` or `"realm"`. `topN` defaults to 200, capped at 1000. No `ego` support yet.

- [ ] **Step 4: Tick the roadmap and resolve the open renderer question**

In `docs/superpowers/specs/2026-08-13-chain-analytics-dashboards-design.md` §5, tick batch 4:

```markdown
- [x] **Batch 4 — `transfer_edges` / `caller_edges` rollups.**
```

In §9, resolve the batch-4 renderer open question:

```markdown
- ~~**Batch 4 — renderer for the caller graph.**~~ **Resolved: echarts-gl `graphGL`.** No new dependency;
  WebGL force layout covers realistic per-network caller-graph sizes well within its ~100k-node ceiling, and
  this batch's top-N/ego-style scoping keeps requests far below that regardless. See
  [`2026-08-17-dashboards-batch-4-network-design.md`](2026-08-17-dashboards-batch-4-network-design.md) §5.
```

- [ ] **Step 5: Commit**

```bash
git add frontend/index.html docs/api.md docs/superpowers/specs/2026-08-13-chain-analytics-dashboards-design.md
git commit -m "feat: add caller-realm WebGL graph and update docs"
```

---

## Done when

- `/dashboards?section=network` shows three cards on a specific network — force graph, sankey, WebGL caller graph — and all three show the select-a-network message on "all"
- Clicking a node in the force graph drills into its 1-hop neighborhood and "back to overview" returns cleanly
- The force graph and sankey render an attacker-controlled address as literal text in their tooltips, verified with a live payload
- Switching away from and back to the network section disposes the old WebGL instance rather than leaking contexts
- `transfer_edges`/`caller_edges` accumulate additively across repeated syncs rather than overwriting, and each network's cursor is independent of the others'
- `gofmt -l .` is empty, `go vet ./...` and `go test ./...` pass
- Batch 4 is ticked in the roadmap and its §9 renderer question is resolved
