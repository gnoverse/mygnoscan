# Dashboards Batch 2a — Blocks Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Persist gno.land blocks into SQLite and ship the three dashboard charts that need them — block-time distribution, blocks/transactions per bucket, and proposer distribution.

**Architecture:** Two additive tables (`blocks` plus a `proposers` intern table) filled by a new `syncBlocks` that head-syncs forward from the tip and backfills backward in bounded per-pass budgets. Block-time deltas are computed at query time with a SQLite window function rather than stored. Charts are entries in the existing declarative `DASHBOARDS` array, which this batch widens first to carry per-chart controls and a render context.

**Tech Stack:** Go (stdlib + `modernc.org/sqlite`, SQLite 3.51.3), vanilla JS with the repo's `el()` DOM helper, ECharts 5 from CDN. No bundler, no build step.

**Spec:** [`docs/superpowers/specs/2026-08-13-dashboards-batch-2a-blocks-design.md`](../specs/2026-08-13-dashboards-batch-2a-blocks-design.md)

## Global Constraints

Every task's requirements implicitly include these.

- **Everything is network-scoped.** Every query, join and aggregate filters or groups by `network`. Every frontend fetch goes through `api()` / `dashApi()`, which append `network=getNetwork()`. Never hand-build an `/api/` URL. Joins on a non-network key alone are how this goes wrong silently.
- **The frontend builds DOM, never HTML strings.** Use `el()`. No `innerHTML` with interpolated data anywhere — the explorer renders attacker-controlled on-chain content.
- **No build step.** No bundler, no npm, no framework, no JS test runner.
- **Idempotent inserts.** All inserts use `INSERT ... ON CONFLICT DO NOTHING/UPDATE` against the declared unique keys. Sync is incremental and re-runnable; the sync loop logs-and-continues per item by design, so partial re-runs are expected.
- **Cursors derive from stored data**, not separate state — `MAX(height)` / `MIN(height)` for that network.
- **Errors go up from query paths.** Only the sync loop logs and continues. `AGENTS.md` calls the existing zero-returning aggregate readers "a known bug, not a style to follow."
- **Go gates before any commit:** `gofmt -l .` prints nothing, `go vet ./...` passes, `go test ./...` passes.
- **Commits are conventional and single-line** (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`). **No co-author or attribution trailers.**
- **Go tests are table-driven** with a real temp SQLite file, never mocks.
- **Empty-bucket semantics are declared per endpoint** (spec §7): counts render `0`, ratios render `null`. `/api/timeseries/blocks` is counts.
- **Frontend verification asserts on data**, not just a clean console: pull `_dashCharts[id].getOption()` and check the actual series values.

---

## File Structure

| File | Change | Responsibility |
|---|---|---|
| `db.go` | Add schema + queries | `blocks`/`proposers` DDL in `initSchema`; intern helper; four read queries |
| `db_test.go` | Add tests | Interning, idempotency, cursors, binning, network isolation, coverage |
| `syncer.go` | Add `syncBlocks` | Head sync + bounded backward backfill |
| `syncer_test.go` | Add tests | Page budget, resume, termination |
| `api.go` | Add 4 handlers | `HandleTimeSeriesBlocks`, `HandleBlockTimeHistogram`, `HandleBlockProposers`, `HandleBlockCoverage` |
| `main.go` | Add 4 routes | Route registration |
| `frontend/index.html` | Widen config, add 3 charts | Config widening, chart entries, coverage note, batch 1 retitle |

`db.go` is already ~2,400 lines and `AGENTS.md` notes it mixes schema, migrations and every query with no domain grouping. This plan follows that existing structure rather than restructuring it — but keeps all block-related queries contiguous and commented as a group so a later split has a clean seam.

---

## Task 1: Config widening

Spec §8. Lands first, while there are six chart call sites rather than nine. Two changes: `opt` gains a render context, and charts gain an optional per-card control slot.

**Files:**
- Modify: `frontend/index.html` — `renderDashChart` (~:3748), `dashCard` (~:3741), CSS (~:110)

**Interfaces:**
- Consumes: existing `dashCard(chart)`, `renderDashChart(chart, gen)`, `el()`.
- Produces:
  - `opt(rows, ctx)` where `ctx = { window: string, granularity: 'hourly'|'daily'|'weekly'|'monthly'|'unknown' }`. Existing six charts ignore the second argument.
  - `dashGranularityOf(rows) -> string` — infers granularity from the first row's `time` format.
  - Chart objects may declare `state: {}` and `controls(container, rerender, state)`. `dashCard` renders `.dash-controls` when `controls` is present.

- [ ] **Step 1: Add the granularity inference helper**

In `frontend/index.html`, immediately above `renderDashChart`, add:

```js
// dashGranularityOf infers the bucket size from the shape of the server's
// `time` strings rather than re-deriving it from the window client-side, so
// it cannot drift from the server's own window->bucket mapping.
//   2026-08          -> monthly
//   2026-W33         -> weekly
//   2026-08-13       -> daily
//   2026-08-13T13    -> hourly
function dashGranularityOf(rows) {
  const t = rows && rows.length ? String(rows[0].time || '') : '';
  if (/^\d{4}-W\d{2}$/.test(t)) return 'weekly';
  if (/^\d{4}-\d{2}$/.test(t)) return 'monthly';
  if (/^\d{4}-\d{2}-\d{2}T\d{2}$/.test(t)) return 'hourly';
  if (/^\d{4}-\d{2}-\d{2}$/.test(t)) return 'daily';
  return 'unknown';
}
```

- [ ] **Step 2: Pass the context into `opt`**

In `renderDashChart`, replace the single line:

```js
  inst.setOption(chart.opt(rows), true);
```

with:

```js
  const ctx = { window: chart.window || _dashWindow, granularity: dashGranularityOf(rows) };
  inst.setOption(chart.opt(rows, ctx), true);
```

- [ ] **Step 3: Add the control slot to `dashCard`**

Replace `dashCard` entirely with:

```js
function dashCard(chart) {
  const head = el('div', { className: 'dash-head' }, el('span', { className: 'dash-title' }, chart.title));
  if (chart.why) head.appendChild(infoTip(chart.why));
  const parts = [head];
  if (chart.controls) parts.push(el('div', { className: 'dash-controls', id: 'dash-controls-' + chart.id }));
  parts.push(el('div', { className: 'dash-chart', id: 'dash-chart-' + chart.id }));
  const card = el('div', { className: 'dash-card' + (chart.wide ? ' wide' : '') });
  parts.forEach(p => card.appendChild(p));
  return card;
}
```

- [ ] **Step 4: Invoke controls after the chart renders**

In `renderDashChart`, immediately after `_dashCharts[chart.id] = inst;`, add:

```js
  if (chart.controls) {
    const bar = document.getElementById('dash-controls-' + chart.id);
    if (bar) {
      bar.textContent = '';
      chart.state = chart.state || {};
      chart.controls(bar, () => renderDashChart(chart, _dashGen), chart.state);
    }
  }
```

The rerender callback passes the *current* generation, not the captured `gen`, so a control click after a section switch still renders into the live card.

- [ ] **Step 5: Add the control-bar CSS**

In the `<style>` block, immediately after the `.dash-head` rule, add:

```css
.dash-controls { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; margin-bottom: 8px; }
```

- [ ] **Step 6: Verify in the browser**

```bash
go build -o /tmp/mygnoscan . && /tmp/mygnoscan -listen :8899 -sync=false
```

Open `http://localhost:8899/dashboards`. Expected:
1. All six existing charts still render — the widening is backward compatible.
2. In the console, `dashGranularityOf([{time:'2026-08'}])` returns `'monthly'`, `dashGranularityOf([{time:'2026-08-13T13'}])` returns `'hourly'`, `dashGranularityOf([{time:'2026-W33'}])` returns `'weekly'`, `dashGranularityOf([{time:'2026-08-13'}])` returns `'daily'`, `dashGranularityOf([])` returns `'unknown'`.
3. No `.dash-controls` div exists yet, since no chart declares `controls`.
4. Console clean.

Stop the server.

- [ ] **Step 7: Commit**

```bash
git add frontend/index.html
git commit -m "feat: widen dashboard chart config with render context and controls"
```

---

## Task 2: Blocks and proposers schema

Spec §4. Both tables are additive — `CREATE TABLE IF NOT EXISTS` in `initSchema`, never the `packages_new` rebuild path.

**Files:**
- Modify: `db.go` — `initSchema` DDL block (the statements ending ~:311)
- Test: `db_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - Tables `blocks(network, height, time, proposer_id, num_txs)` PK `(network, height)`, and `proposers(id, network, address)` with `UNIQUE(network, address)`.
  - `func (d *DB) InternProposer(network, address string) (int64, error)` — returns a stable id, creating the row on first sight.
  - `type BlockRow struct { Height int; Time string; ProposerID int64; NumTxs int }`
  - `func (d *DB) UpsertBlocks(network string, rows []BlockRow) error` — one lock, one SQLite transaction, idempotent on `(network, height)`. This is what the syncer uses.
  - `func (d *DB) UpsertBlock(network string, height int, blockTime string, proposerID int64, numTxs int) error` — single-row wrapper for tests.
  - `func (d *DB) BlockHeightBounds(network string) (minH, maxH int, ok bool, err error)` — `ok` is false when the network has no blocks.

- [ ] **Step 1: Write the failing tests**

Append to `db_test.go`:

```go
func TestInternProposer(t *testing.T) {
	db := newTestDB(t)

	a1, err := db.InternProposer("gnoland1", "g1aaa")
	if err != nil {
		t.Fatalf("intern: %v", err)
	}
	a2, err := db.InternProposer("gnoland1", "g1aaa")
	if err != nil {
		t.Fatalf("intern repeat: %v", err)
	}
	if a1 != a2 {
		t.Errorf("same address interned to different ids: %d vs %d", a1, a2)
	}

	// The same validator address on two chains must never share an id, or a
	// per-network aggregate would silently mix chains.
	b1, err := db.InternProposer("test12", "g1aaa")
	if err != nil {
		t.Fatalf("intern other network: %v", err)
	}
	if b1 == a1 {
		t.Errorf("same address on two networks shares id %d", a1)
	}
}

func TestUpsertBlockIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	pid, _ := db.InternProposer("gnoland1", "g1aaa")

	for i := 0; i < 3; i++ {
		if err := db.UpsertBlock("gnoland1", 100, "2026-08-13T13:00:00Z", pid, 2); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}

	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM blocks WHERE network = ?`, "gnoland1").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("row count = %d, want 1 after repeated upsert", n)
	}
}

func TestBlockHeightBounds(t *testing.T) {
	db := newTestDB(t)
	pid, _ := db.InternProposer("gnoland1", "g1aaa")

	if _, _, ok, err := db.BlockHeightBounds("gnoland1"); err != nil || ok {
		t.Fatalf("empty table: ok = %v, err = %v; want ok=false, err=nil", ok, err)
	}

	for _, h := range []int{50, 51, 52} {
		if err := db.UpsertBlock("gnoland1", h, "2026-08-13T13:00:00Z", pid, 0); err != nil {
			t.Fatalf("upsert %d: %v", h, err)
		}
	}
	// A different network's rows must not move this network's cursors.
	opid, _ := db.InternProposer("test12", "g1bbb")
	if err := db.UpsertBlock("test12", 9999, "2026-08-13T13:00:00Z", opid, 0); err != nil {
		t.Fatalf("upsert other network: %v", err)
	}

	minH, maxH, ok, err := db.BlockHeightBounds("gnoland1")
	if err != nil || !ok {
		t.Fatalf("ok = %v, err = %v; want ok=true, err=nil", ok, err)
	}
	if minH != 50 || maxH != 52 {
		t.Errorf("bounds = (%d, %d), want (50, 52)", minH, maxH)
	}
}
```

`db_test.go` has no shared DB helper today — each test inlines `NewDB(filepath.Join(t.TempDir(), "..."))`. Add this helper above the new tests, matching that same construction:

```go
// newTestDB opens a real temp-file database. The driver is pure Go, so this
// works everywhere including CI, and the repo prefers a real database over a
// mock in tests.
func newTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
```

`db_test.go` already imports `path/filepath` and `testing`. The new tests also need `time` — add it to the import block if it is not already there.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestInternProposer|TestUpsertBlock|TestBlockHeightBounds' ./... -v`

Expected: FAIL to compile — `db.InternProposer undefined`, `db.UpsertBlock undefined`, `db.BlockHeightBounds undefined`.

- [ ] **Step 3: Add the schema**

In `db.go`, inside `initSchema`'s DDL, immediately after the `sync_state` table and before the `CREATE INDEX` statements, add:

```sql
		CREATE TABLE IF NOT EXISTS proposers (
			id      INTEGER PRIMARY KEY,
			network TEXT NOT NULL,
			address TEXT NOT NULL,
			UNIQUE (network, address)
		);

		CREATE TABLE IF NOT EXISTS blocks (
			network     TEXT NOT NULL DEFAULT 'gnoland1',
			height      INTEGER NOT NULL,
			time        TEXT NOT NULL,
			proposer_id INTEGER,
			num_txs     INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (network, height)
		);
```

And with the other `CREATE INDEX` statements:

```sql
		CREATE INDEX IF NOT EXISTS idx_blocks_time ON blocks(network, time);
```

- [ ] **Step 4: Add the helpers**

In `db.go`, in a new contiguous block (keep every block-related query together — `AGENTS.md` notes queries are not grouped by domain yet, and a clean seam helps a later split):

```go
// --- blocks ---

// InternProposer maps a proposer address to a small integer id, creating the
// row on first sight. Storing the id rather than the 40-byte address on every
// block row saves roughly 119MB across a 3.3M-block chain.
//
// The unique key is (network, address), so the same validator address on two
// chains gets two ids and an id can never span networks.
func (d *DB) InternProposer(network, address string) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, err := d.db.Exec(
		`INSERT INTO proposers (network, address) VALUES (?, ?) ON CONFLICT DO NOTHING`,
		network, address,
	); err != nil {
		return 0, err
	}
	var id int64
	err := d.db.QueryRow(
		`SELECT id FROM proposers WHERE network = ? AND address = ?`, network, address,
	).Scan(&id)
	return id, err
}

type BlockRow struct {
	Height     int
	Time       string
	ProposerID int64
	NumTxs     int
}

// UpsertBlocks writes many blocks under a single lock and a single SQLite
// transaction, mirroring UpsertTransactions.
//
// This is not an optimisation. The comment on UpsertTransactions records that
// writing rows individually made read requests queue behind a backfill of a
// hundred rows; a block page is 5,000 rows and the full backfill is 3.3M, so
// per-row writes here would stall the API for the entire backfill.
//
// Idempotent on (network, height), so a re-synced page is a no-op.
func (d *DB) UpsertBlocks(network string, rows []BlockRow) error {
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
		INSERT INTO blocks (network, height, time, proposer_id, num_txs)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (network, height) DO UPDATE SET
			time = excluded.time,
			proposer_id = excluded.proposer_id,
			num_txs = excluded.num_txs`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, r := range rows {
		if _, err := stmt.Exec(network, r.Height, r.Time, r.ProposerID, r.NumTxs); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpsertBlock stores a single block. Convenience wrapper over UpsertBlocks for
// tests and one-off writes.
func (d *DB) UpsertBlock(network string, height int, blockTime string, proposerID int64, numTxs int) error {
	return d.UpsertBlocks(network, []BlockRow{{
		Height: height, Time: blockTime, ProposerID: proposerID, NumTxs: numTxs,
	}})
}

// BlockHeightBounds returns the lowest and highest stored height for a network.
// The syncer derives both its cursors from these rather than from separate
// state; ok is false when the network has no blocks yet.
func (d *DB) BlockHeightBounds(network string) (minH, maxH int, ok bool, err error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var lo, hi sql.NullInt64
	err = d.db.QueryRow(
		`SELECT MIN(height), MAX(height) FROM blocks WHERE network = ?`, network,
	).Scan(&lo, &hi)
	if err != nil {
		return 0, 0, false, err
	}
	if !lo.Valid || !hi.Valid {
		return 0, 0, false, nil
	}
	return int(lo.Int64), int(hi.Int64), true, nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -run 'TestInternProposer|TestUpsertBlock|TestBlockHeightBounds' ./... -v`
Expected: PASS (3 tests).

- [ ] **Step 6: Run the full gate**

```bash
gofmt -l . && go vet ./... && go test ./...
```

- [ ] **Step 7: Commit**

```bash
git add db.go db_test.go
git commit -m "feat: add blocks and proposers tables"
```

---

## Task 3: Block sync

Spec §5. Head sync forward from the tip, plus bounded backward backfill. Backward matters: filling oldest-first would leave the default 90d window empty until the backfill nearly finished.

**Files:**
- Modify: `syncer.go` — add `syncBlocks`, call it from `SyncAll` (~:27)
- Test: `syncer_test.go`

**Interfaces:**
- Consumes: `db.InternProposer`, `db.UpsertBlock`, `db.BlockHeightBounds` from Task 2; `client.GetBlocksInRange(ctx, from, to)` and `client.LatestBlockHeight(ctx)`, both already in `indexer.go`; `db.SetSyncState` / `db.GetSyncState`. Test fixtures extend the existing `fakeIndexer` and `newTestSyncer(t, network) (*Syncer, *fakeIndexer, *DB)` in `syncer_test.go` — note `NewAnalyzer` takes a `*DB` (`analyzer.go:15`), which `newTestSyncer` already handles.
- Produces: `func (s *Syncer) syncBlocks(ctx context.Context)`; the `sync_state` key `blocks_backfill_done:<network>` set to `"1"` when backfill terminates.

- [ ] **Step 1: Write the failing tests**

`syncer_test.go` already has a `fakeIndexer` httptest server and a `newTestSyncer(t, network) (*Syncer, *fakeIndexer, *DB)` helper. **Extend that fake rather than adding a second one.**

The fake's existing `getBlocks` case always returns block 1, which is what `chainFingerprint` needs. Range queries must be served separately. The two are distinguishable from the query text: `GetBlock` emits `height: { eq: N }` (`indexer.go:747`) while `GetBlocksInRange` emits `height: { gt: X, lt: Y }` (`indexer.go:662`).

First, add these fields to the `fakeIndexer` struct:

```go
	blockLo, blockHi int // the height range this fake "has"; 0,0 means none
	rangeCalls       int
```

Then replace the existing `case strings.Contains(query, "getBlocks"):` arm with:

```go
	case strings.Contains(query, "getBlocks"):
		if f.block1Failing {
			http.Error(w, "indexer unavailable", http.StatusInternalServerError)
			return
		}
		// GetBlock emits `eq:` (the fingerprint probe); GetBlocksInRange emits
		// `gt:`/`lt:`. Only the latter is a backfill page.
		if strings.Contains(query, "gt:") {
			f.rangeCalls++
			f.serveBlockRange(w, query)
			return
		}
		b, _ := json.Marshal(f.block1Hash)
		fmt.Fprintf(w, `{"data":{"getBlocks":[{"hash":%s,"height":1,"chain_id":%q,"time":"2026-01-01T00:00:00Z"}]}}`,
			b, f.chainID)
```

And add these methods:

```go
// setBlocks makes the fake serve blocks in [lo, hi] and report hi as the tip.
func (f *fakeIndexer) setBlocks(lo, hi int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blockLo, f.blockHi, f.latestHeight = lo, hi, hi
}

// serveBlockRange renders a getBlocks range response clamped to [blockLo,
// blockHi]. Heights outside that range yield an empty array — how a pruned
// indexer behaves, and what the backfill's termination check keys on.
func (f *fakeIndexer) serveBlockRange(w http.ResponseWriter, query string) {
	from := intAfter(query, "gt:") + 1
	to := intAfter(query, "lt:") - 1
	if from < f.blockLo {
		from = f.blockLo
	}
	if to > f.blockHi {
		to = f.blockHi
	}
	var parts []string
	for h := from; h <= to; h++ {
		// 5s spacing keeps times deterministic; these tests assert on counts
		// and cursors, not on deltas.
		ts := time.Unix(int64(1767225600+h*5), 0).UTC().Format(time.RFC3339)
		parts = append(parts, fmt.Sprintf(
			`{"hash":"h%d","height":%d,"chain_id":%q,"time":%q,"num_txs":0,"proposer_address_raw":"g1aaa"}`,
			h, h, f.chainID, ts))
	}
	fmt.Fprintf(w, `{"data":{"getBlocks":[%s]}}`, strings.Join(parts, ","))
}

// intAfter reads the first integer following key in s, or 0.
func intAfter(s, key string) int {
	i := strings.Index(s, key)
	if i < 0 {
		return 0
	}
	rest := strings.TrimSpace(s[i+len(key):])
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	n, _ := strconv.Atoi(rest[:end])
	return n
}
```

Add `strconv` and `time` to `syncer_test.go`'s import block.

Now the tests:

```go
func TestSyncBlocksBoundedPerPass(t *testing.T) {
	// A pass must stop at its page budget rather than draining the whole chain
	// inline, which would stall package/call/msg-run syncing behind it.
	s, fake, db := newTestSyncer(t, "gnoland1")
	fake.setBlocks(1, 1_000_000)

	s.syncBlocks(context.Background())

	minH, maxH, ok, err := db.BlockHeightBounds("gnoland1")
	if err != nil || !ok {
		t.Fatalf("bounds after one pass: ok=%v err=%v", ok, err)
	}
	stored := maxH - minH + 1
	budget := blockPageSize * blockPagesPerPass
	if stored > budget {
		t.Errorf("stored %d blocks in one pass, budget is %d", stored, budget)
	}
	if stored == 0 {
		t.Error("stored no blocks at all")
	}
	if fake.rangeCalls == 0 {
		t.Error("made no range queries")
	}
}

func TestSyncBlocksResumesBackward(t *testing.T) {
	// Successive passes must extend the range downward, not restart at the tip.
	s, fake, db := newTestSyncer(t, "gnoland1")
	fake.setBlocks(1, 1_000_000)

	s.syncBlocks(context.Background())
	min1, max1, _, _ := db.BlockHeightBounds("gnoland1")

	s.syncBlocks(context.Background())
	min2, max2, _, _ := db.BlockHeightBounds("gnoland1")

	if min2 >= min1 {
		t.Errorf("second pass did not extend backward: min went %d -> %d", min1, min2)
	}
	if max2 < max1 {
		t.Errorf("second pass lost head blocks: max went %d -> %d", max1, max2)
	}
}

func TestSyncBlocksTerminatesAtGenesis(t *testing.T) {
	// A small chain must reach the bottom and set the done flag rather than
	// retrying a range that will never return rows.
	s, fake, db := newTestSyncer(t, "gnoland1")
	fake.setBlocks(1, 20)

	for i := 0; i < 5; i++ {
		s.syncBlocks(context.Background())
	}

	done, err := db.GetSyncState(blocksBackfillDoneKey("gnoland1"))
	if err != nil {
		t.Fatalf("get sync state: %v", err)
	}
	if done != "1" {
		t.Errorf("backfill done flag = %q, want \"1\"", done)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestSyncBlocks' ./... -v`

Expected: FAIL to compile — `s.syncBlocks undefined`, `blockPageSize undefined`, `blockPagesPerPass undefined`.

- [ ] **Step 3: Implement `syncBlocks`**

First add the memo field to the `Syncer` struct (`syncer.go:12`):

```go
type Syncer struct {
	client      *IndexerClient
	db          *DB
	analyzer    *Analyzer
	networkID   string
	proposerIDs map[string]int64 // address -> interned id, memoised across passes
}
```

`NewSyncer` needs no change — the map is created lazily on first use.

Then add:

```go
// Page size measured against the live indexer: 5,000 blocks in ~500ms / 684KB.
// The per-pass budget bounds one SyncAll pass to ~100k blocks (~10s) so a full
// 3.3M-block backfill spreads over ~33 passes instead of stalling package,
// call and msg-run syncing behind a single 5-6 minute run.
const (
	blockPageSize     = 5000
	blockPagesPerPass = 20
)

func blocksBackfillDoneKey(network string) string {
	return "blocks_backfill_done:" + network
}

// syncBlocks keeps the blocks table current and backfills history.
//
// Two cursors, both derived from the table itself: head sync walks forward from
// MAX(height) to the tip, backfill walks backward from MIN(height). Because the
// table always holds a contiguous height range, neither cursor can be fooled by
// a gap — nothing may insert blocks outside that range.
//
// Backward rather than forward: filling oldest-first would leave the dashboard's
// default 90d window empty until the backfill nearly finished.
func (s *Syncer) syncBlocks(ctx context.Context) {
	tip, err := s.client.LatestBlockHeight(ctx)
	if err != nil {
		log.Printf("[%s] syncBlocks: tip: %v", s.networkID, err)
		return
	}

	minH, maxH, ok, err := s.db.BlockHeightBounds(s.networkID)
	if err != nil {
		log.Printf("[%s] syncBlocks: bounds: %v", s.networkID, err)
		return
	}
	if !ok {
		// Seed at the tip so recent windows populate immediately.
		from := tip - blockPageSize + 1
		if from < 1 {
			from = 1
		}
		if !s.fetchBlockPage(ctx, from, tip) {
			return
		}
		minH, maxH, ok, err = s.db.BlockHeightBounds(s.networkID)
		if err != nil || !ok {
			return
		}
	}

	// Head sync: catch up to the tip.
	if maxH < tip {
		to := maxH + blockPageSize
		if to > tip {
			to = tip
		}
		s.fetchBlockPage(ctx, maxH+1, to)
	}

	// Backfill: walk down until genesis or an empty page, bounded per pass.
	if done, _ := s.db.GetSyncState(blocksBackfillDoneKey(s.networkID)); done == "1" {
		return
	}
	for i := 0; i < blockPagesPerPass && minH > 1; i++ {
		to := minH - 1
		from := to - blockPageSize + 1
		if from < 1 {
			from = 1
		}
		if !s.fetchBlockPage(ctx, from, to) {
			return
		}
		newMin, _, ok, err := s.db.BlockHeightBounds(s.networkID)
		if err != nil || !ok {
			return
		}
		if newMin >= minH {
			// The page returned nothing new: the indexer prunes below here, so
			// there is no more history to fetch. Without this the backfill
			// would retry the same empty range forever.
			break
		}
		minH = newMin
	}
	if minH <= 1 {
		if err := s.db.SetSyncState(blocksBackfillDoneKey(s.networkID), "1"); err != nil {
			log.Printf("[%s] syncBlocks: mark done: %v", s.networkID, err)
		}
	}
}

// proposerID returns the interned id for an address, memoised in the syncer so
// a 5,000-block page costs one query per *distinct* proposer instead of two per
// block. gno.land runs a handful of validators, so the map stays tiny.
func (s *Syncer) proposerID(address string) (int64, error) {
	if s.proposerIDs == nil {
		s.proposerIDs = make(map[string]int64)
	}
	if id, ok := s.proposerIDs[address]; ok {
		return id, nil
	}
	id, err := s.db.InternProposer(s.networkID, address)
	if err != nil {
		return 0, err
	}
	s.proposerIDs[address] = id
	return id, nil
}

// fetchBlockPage stores one height range. Returns false when the page failed,
// so the caller stops and retries next pass rather than spinning.
//
// The whole page is written in one UpsertBlocks call: per-row writes would hold
// and release the write lock 5,000 times, and the comment on UpsertTransactions
// records that read requests already queue behind a per-row backfill of a
// hundred rows.
func (s *Syncer) fetchBlockPage(ctx context.Context, from, to int) bool {
	blocks, err := s.client.GetBlocksInRange(ctx, from, to)
	if err != nil {
		log.Printf("[%s] syncBlocks: range %d-%d: %v", s.networkID, from, to, err)
		return false
	}
	if len(blocks) == 0 {
		return true
	}

	rows := make([]BlockRow, 0, len(blocks))
	for _, b := range blocks {
		var pid int64
		if b.ProposerAddressRaw != "" {
			pid, err = s.proposerID(b.ProposerAddressRaw)
			if err != nil {
				log.Printf("[%s] syncBlocks: intern proposer: %v", s.networkID, err)
				continue
			}
		}
		rows = append(rows, BlockRow{Height: b.Height, Time: b.Time, ProposerID: pid, NumTxs: b.NumTxs})
	}
	if err := s.db.UpsertBlocks(s.networkID, rows); err != nil {
		log.Printf("[%s] syncBlocks: upsert range %d-%d: %v", s.networkID, from, to, err)
		return false
	}
	return true
}
```

Note the empty-page detection compares `BlockHeightBounds` before and after, so a page that the indexer serves as an empty array *and* one that returns only already-stored rows both terminate the backfill.

- [ ] **Step 4: Call it from `SyncAll`**

In `SyncAll`, after `s.warnOnHeightRegression(ctx)` and before `s.backfillBlockTimes(ctx)`, add:

```go
	s.syncBlocks(ctx)
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -run 'TestSyncBlocks' ./... -v`
Expected: PASS (3 tests).

- [ ] **Step 6: Run the full gate**

```bash
gofmt -l . && go vet ./... && go test ./...
```

- [ ] **Step 7: Commit**

```bash
git add syncer.go syncer_test.go
git commit -m "feat: sync blocks with bounded backward backfill"
```

---

## Task 4: Block queries

Spec §6, §7. Four read queries. The histogram is the subtle one: deltas come from a window function at query time, not a stored column.

**Files:**
- Modify: `db.go` — the `// --- blocks ---` group from Task 2
- Test: `db_test.go`

**Interfaces:**
- Consumes: the schema and `InternProposer`/`UpsertBlock` from Task 2; **`blocksBackfillDoneKey(network)` from Task 3** (defined in `syncer.go`, same package); `timeseriesFormat(granularity)` and `fillBuckets` already in `db.go`.
- Produces:
  - `type BlockTimePoint struct { Time string; Blocks int; Txs int }` — JSON `time`, `blocks`, `txs`
  - `type BlockTimeBin struct { Bin string; Blocks int }` — JSON `bin`, `blocks`
  - `type ProposerCount struct { Address string; Blocks int }` — JSON `address`, `blocks`
  - `type BlockCoverage struct { MinTime, MaxTime string; Complete bool }` — JSON `min_time`, `max_time`, `complete`
  - `GetBlockTimeSeries(network, granularity string, days int) ([]BlockTimePoint, error)`
  - `GetBlockTimeHistogram(network string, days int) ([]BlockTimeBin, error)`
  - `GetBlockProposers(network string, days, topN int) ([]ProposerCount, error)`
  - `GetBlockCoverage(network string) (BlockCoverage, error)`

- [ ] **Step 1: Write the failing tests**

Append to `db_test.go`:

```go
// seedBlocks stores blocks at fixed 1-second-multiples from a base time so
// delta bins are exactly predictable.
func seedBlocks(t *testing.T, db *DB, network string, proposer string, base time.Time, offsets []float64) {
	t.Helper()
	pid, err := db.InternProposer(network, proposer)
	if err != nil {
		t.Fatalf("intern: %v", err)
	}
	for i, off := range offsets {
		ts := base.Add(time.Duration(off * float64(time.Second))).UTC().Format("2006-01-02T15:04:05.000000000Z")
		if err := db.UpsertBlock(network, 1000+i, ts, pid, i); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
}

func TestGetBlockTimeHistogram(t *testing.T) {
	db := newTestDB(t)
	base := time.Now().UTC().Add(-2 * time.Hour)
	// Cumulative offsets producing deltas of exactly:
	//   4.2 (bin 4.0-4.5), 4.5 (bin 4.5-5.0, lower edge is inclusive),
	//   6.5 (bin 6.0-7.0), 12.0 (bin >=10.0)
	seedBlocks(t, db, "gnoland1", "g1aaa", base, []float64{0, 4.2, 8.7, 15.2, 27.2})

	bins, err := db.GetBlockTimeHistogram("gnoland1", 1)
	if err != nil {
		t.Fatalf("histogram: %v", err)
	}

	got := map[string]int{}
	total := 0
	for _, b := range bins {
		got[b.Bin] = b.Blocks
		total += b.Blocks
	}
	// 5 blocks produce 4 deltas; the first block has a NULL delta and must be
	// excluded rather than counted as a zero-second interval.
	if total != 4 {
		t.Errorf("total binned = %d, want 4 (5 blocks - 1 with no predecessor)", total)
	}
	for bin, want := range map[string]int{"4.0-4.5": 1, "4.5-5.0": 1, "6.0-7.0": 1, ">=10.0": 1} {
		if got[bin] != want {
			t.Errorf("bin %q = %d, want %d (all bins: %v)", bin, got[bin], want, got)
		}
	}
}

func TestGetBlockProposersIsNetworkScoped(t *testing.T) {
	// Two networks holding blocks at the SAME heights. AGENTS.md calls this the
	// failure mode that goes wrong silently.
	db := newTestDB(t)
	base := time.Now().UTC().Add(-2 * time.Hour)
	seedBlocks(t, db, "gnoland1", "g1aaa", base, []float64{0, 5, 10})
	seedBlocks(t, db, "test12", "g1bbb", base, []float64{0, 5, 10})

	got, err := db.GetBlockProposers("gnoland1", 1, 10)
	if err != nil {
		t.Fatalf("proposers: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d proposers, want 1 (only gnoland1's)", len(got))
	}
	if got[0].Address != "g1aaa" {
		t.Errorf("address = %q, want g1aaa", got[0].Address)
	}
	if got[0].Blocks != 3 {
		t.Errorf("blocks = %d, want 3 — other network's rows leaked in", got[0].Blocks)
	}
}

func TestGetBlockCoverage(t *testing.T) {
	db := newTestDB(t)

	cov, err := db.GetBlockCoverage("gnoland1")
	if err != nil {
		t.Fatalf("empty coverage: %v", err)
	}
	if cov.Complete || cov.MinTime != "" {
		t.Errorf("empty table: %+v, want zero value and Complete=false", cov)
	}

	base := time.Now().UTC().Add(-2 * time.Hour)
	seedBlocks(t, db, "gnoland1", "g1aaa", base, []float64{0, 5})
	if err := db.SetSyncState(blocksBackfillDoneKey("gnoland1"), "1"); err != nil {
		t.Fatalf("set state: %v", err)
	}

	cov, err = db.GetBlockCoverage("gnoland1")
	if err != nil {
		t.Fatalf("coverage: %v", err)
	}
	if !cov.Complete {
		t.Error("Complete = false after the done flag was set")
	}
	if cov.MinTime == "" || cov.MaxTime == "" {
		t.Errorf("times not populated: %+v", cov)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestGetBlock' ./... -v`

Expected: FAIL to compile — `db.GetBlockTimeHistogram undefined`, `db.GetBlockProposers undefined`, `db.GetBlockCoverage undefined`.

- [ ] **Step 3: Implement the queries**

Append to the `// --- blocks ---` group in `db.go`:

```go
type BlockTimePoint struct {
	Time   string `json:"time"`
	Blocks int    `json:"blocks"`
	Txs    int    `json:"txs"`
}

type BlockTimeBin struct {
	Bin    string `json:"bin"`
	Blocks int    `json:"blocks"`
}

type ProposerCount struct {
	Address string `json:"address"`
	Blocks  int    `json:"blocks"`
}

type BlockCoverage struct {
	MinTime  string `json:"min_time"`
	MaxTime  string `json:"max_time"`
	Complete bool   `json:"complete"`
}

// blockTimeBinExpr bins a block-time delta (seconds) into the ranges below.
// Edges come from measurement against gnoland1: median 4.34s, observed
// 3.69-10.11s, so the resolution is concentrated where the mass actually is.
// Lower edges are inclusive.
const blockTimeBinExpr = `CASE
	WHEN d <  4.0 THEN '<4.0'
	WHEN d <  4.5 THEN '4.0-4.5'
	WHEN d <  5.0 THEN '4.5-5.0'
	WHEN d <  5.5 THEN '5.0-5.5'
	WHEN d <  6.0 THEN '5.5-6.0'
	WHEN d <  7.0 THEN '6.0-7.0'
	WHEN d <  8.0 THEN '7.0-8.0'
	WHEN d < 10.0 THEN '8.0-10.0'
	ELSE '>=10.0'
END`

// BlockTimeBinOrder is the display order of the histogram's bins.
var BlockTimeBinOrder = []string{
	"<4.0", "4.0-4.5", "4.5-5.0", "5.0-5.5", "5.5-6.0", "6.0-7.0", "7.0-8.0", "8.0-10.0", ">=10.0",
}

// GetBlockTimeSeries returns blocks and transactions per bucket.
func (d *DB) GetBlockTimeSeries(network, granularity string, days int) ([]BlockTimePoint, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	sqlFmt, step, truncFn := timeseriesFormat(granularity)
	start := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	q := fmt.Sprintf(
		`SELECT strftime('%s', time) AS bucket, COUNT(*), COALESCE(SUM(num_txs), 0)
		 FROM blocks WHERE network = ? AND time >= ?
		 GROUP BY bucket ORDER BY bucket ASC`, sqlFmt)

	rows, err := d.db.Query(q, network, start)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	buckets := make(map[string]*BlockTimePoint)
	for rows.Next() {
		var p BlockTimePoint
		if err := rows.Scan(&p.Time, &p.Blocks, &p.Txs); err != nil {
			return nil, err
		}
		cp := p
		buckets[p.Time] = &cp
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return fillBuckets(buckets, days, granularity, step, truncFn,
		func(k string) BlockTimePoint { return BlockTimePoint{Time: k} },
		func(p *BlockTimePoint) {}), nil
}

// GetBlockTimeHistogram bins the interval between consecutive blocks.
//
// Deltas are computed at query time with LAG rather than stored per block: it
// needs no extra column and, crucially, no handling for the page boundaries the
// syncer fetches across, where an ingest-time computation would have no
// predecessor to subtract from. The window's first block has a NULL delta and
// is excluded.
func (d *DB) GetBlockTimeHistogram(network string, days int) ([]BlockTimeBin, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	start := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	q := fmt.Sprintf(`
		WITH deltas AS (
			SELECT (julianday(time) - julianday(LAG(time) OVER (ORDER BY height))) * 86400.0 AS d
			FROM blocks WHERE network = ? AND time >= ?
		)
		SELECT %s AS bin, COUNT(*) FROM deltas WHERE d IS NOT NULL GROUP BY bin`, blockTimeBinExpr)

	rows, err := d.db.Query(q, network, start)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var bin string
		var n int
		if err := rows.Scan(&bin, &n); err != nil {
			return nil, err
		}
		counts[bin] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]BlockTimeBin, 0, len(BlockTimeBinOrder))
	for _, bin := range BlockTimeBinOrder {
		out = append(out, BlockTimeBin{Bin: bin, Blocks: counts[bin]})
	}
	return out, nil
}

// GetBlockProposers counts blocks proposed per validator in the window.
// Addresses only — moniker resolution lives in the frontend's _valMonikers.
func (d *DB) GetBlockProposers(network string, days, topN int) ([]ProposerCount, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if topN <= 0 {
		topN = 25
	}
	start := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	// p.network is filtered as well as b.network: proposer ids are already
	// network-scoped by construction, but relying on that would make this join
	// silently wrong if the intern key ever changed.
	rows, err := d.db.Query(`
		SELECT p.address, COUNT(*) AS n
		FROM blocks b JOIN proposers p ON p.id = b.proposer_id
		WHERE b.network = ? AND p.network = ? AND b.time >= ?
		GROUP BY p.address ORDER BY n DESC LIMIT ?`,
		network, network, start, topN)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ProposerCount
	for rows.Next() {
		var c ProposerCount
		if err := rows.Scan(&c.Address, &c.Blocks); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetBlockCoverage reports the stored block range and whether backfill finished.
//
// Complete comes from the syncer's flag, not from MIN(height) <= 1: an indexer
// that prunes early history never yields height 1, and an inferred version would
// report incomplete forever.
func (d *DB) GetBlockCoverage(network string) (BlockCoverage, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var cov BlockCoverage
	var lo, hi sql.NullString
	err := d.db.QueryRow(
		`SELECT MIN(time), MAX(time) FROM blocks WHERE network = ?`, network,
	).Scan(&lo, &hi)
	if err != nil {
		return cov, err
	}
	cov.MinTime, cov.MaxTime = lo.String, hi.String

	var done string
	err = d.db.QueryRow(
		`SELECT value FROM sync_state WHERE key = ?`, blocksBackfillDoneKey(network),
	).Scan(&done)
	if err != nil && err != sql.ErrNoRows {
		return cov, err
	}
	cov.Complete = done == "1"
	return cov, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -run 'TestGetBlock' ./... -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Run the full gate**

```bash
gofmt -l . && go vet ./... && go test ./...
```

- [ ] **Step 6: Commit**

```bash
git add db.go db_test.go
git commit -m "feat: add block time-series, histogram, proposer and coverage queries"
```

---

## Task 5: Block endpoints

Spec §7. Four handlers and four routes.

**Files:**
- Modify: `api.go` — near the other time-series handlers (~:1246)
- Modify: `main.go` — routes (~:134)

**Interfaces:**
- Consumes: the four `db.GetBlock*` queries from Task 4; `parseTimeseriesParams(r)` and `a.networkParam(r)` already in `api.go`.
- Produces: routes `GET /api/timeseries/blocks`, `GET /api/blocks/time-histogram`, `GET /api/blocks/proposers`, `GET /api/blocks/coverage`.

- [ ] **Step 1: Add the handlers**

In `api.go`, after `HandleTimeSeriesActiveAddresses`, add:

```go
func (a *API) HandleTimeSeriesBlocks(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, granularity := parseTimeseriesParams(r)
	pts, err := a.db.GetBlockTimeSeries(network, granularity, days)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if pts == nil {
		pts = []BlockTimePoint{}
	}
	jsonResponse(w, pts)
}

func (a *API) HandleBlockTimeHistogram(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, _ := parseTimeseriesParams(r)
	bins, err := a.db.GetBlockTimeHistogram(network, days)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if bins == nil {
		bins = []BlockTimeBin{}
	}
	jsonResponse(w, bins)
}

func (a *API) HandleBlockProposers(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, _ := parseTimeseriesParams(r)
	topN, _ := strconv.Atoi(r.URL.Query().Get("topN"))
	props, err := a.db.GetBlockProposers(network, days, topN)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if props == nil {
		props = []ProposerCount{}
	}
	jsonResponse(w, props)
}

func (a *API) HandleBlockCoverage(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	cov, err := a.db.GetBlockCoverage(network)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonResponse(w, cov)
}
```

- [ ] **Step 2: Register the routes**

In `main.go`, after the `GET /api/timeseries/active-addresses` line, add:

```go
	mux.HandleFunc("GET /api/timeseries/blocks", api.HandleTimeSeriesBlocks)
	mux.HandleFunc("GET /api/blocks/time-histogram", api.HandleBlockTimeHistogram)
	mux.HandleFunc("GET /api/blocks/proposers", api.HandleBlockProposers)
	mux.HandleFunc("GET /api/blocks/coverage", api.HandleBlockCoverage)
```

The existing `GET /api/blocks` route is a distinct pattern and keeps working — Go 1.22's mux matches the more specific pattern first.

- [ ] **Step 3: Verify against real data**

```bash
go build -o /tmp/mygnoscan . && /tmp/mygnoscan -listen :8899
```

Let it sync for about a minute so blocks accumulate, then in a second shell:

```bash
curl -s 'localhost:8899/api/blocks/coverage?network=gnoland1'
```
Expected: JSON with `min_time`, `max_time`, and `complete: false` while backfilling.

```bash
curl -s 'localhost:8899/api/blocks/time-histogram?network=gnoland1&window=7d'
```
Expected: exactly 9 bins, most mass in `4.0-4.5` given the measured 4.34 s median.

```bash
curl -s 'localhost:8899/api/blocks/proposers?network=gnoland1&window=90d&topN=5'
```
Expected: up to 5 `{address, blocks}` entries, descending.

```bash
curl -s 'localhost:8899/api/timeseries/blocks?network=gnoland1&window=24h' | head -c 300
```
Expected: hourly buckets with `blocks` counts.

Confirm the pre-existing route still works:

```bash
curl -s 'localhost:8899/api/blocks?network=gnoland1&limit=2' | head -c 200
```
Expected: unchanged live-indexer block list.

Stop the server.

- [ ] **Step 4: Run the full gate**

```bash
gofmt -l . && go vet ./... && go test ./...
```

- [ ] **Step 5: Commit**

```bash
git add api.go main.go
git commit -m "feat: add block time-series, histogram, proposer and coverage endpoints"
```

---

## Task 6: Block charts

Spec §9. Three cards in the existing Chain Pulse section, taking it to seven.

**Files:**
- Modify: `frontend/index.html` — the `pulse` section's `charts` array

**Interfaces:**
- Consumes: `dashApi`, `dashBase`, `dashLegend`, `dashCatAxis`, `dashValAxis`, `DASH_PAL` from batch 1; `opt(rows, ctx)` and `controls(container, rerender, state)` from Task 1; the endpoints from Task 5; `loadValMonikers()`, `_valMonikers` and `truncAddr()` already in the file.
- Produces: chart ids `block-time-histogram`, `blocks-per-bucket`, `block-proposers`. These must not collide with the existing `tx-by-type`, `cumulative-tx`, `success-rate`, `active-addresses`, `gas-used-wanted`, `fees`.

- [ ] **Step 1: Append the three charts**

Add to the `pulse` section's `charts` array, after `active-addresses`:

```js
      {
        id: 'blocks-per-bucket',
        title: 'blocks and transactions per bucket',
        why: 'Throughput. Blocks per bucket tracks consensus liveness; transactions is the real on-chain transaction count, which is lower than the message count above because one transaction can carry several messages.',
        fetch: w => dashApi('timeseries/blocks', w),
        opt: rows => dashBase({
          tooltip: { trigger: 'axis' },
          legend: dashLegend(['blocks', 'transactions']),
          xAxis: dashCatAxis(rows.map(r => r.time)),
          yAxis: [dashValAxis('blocks'), Object.assign(dashValAxis('txs'), { position: 'right' })],
          series: [
            { name: 'blocks', type: 'line', showSymbol: false, areaStyle: { opacity: 0.2 }, data: rows.map(r => r.blocks) },
            { name: 'transactions', type: 'bar', yAxisIndex: 1, itemStyle: { color: DASH_PAL[3] }, data: rows.map(r => r.txs) },
          ],
        }),
      },
      {
        id: 'block-time-histogram',
        title: 'block-time distribution',
        window: '7d',
        why: 'Consensus health. A tight cluster around the target interval is healthy; a fat right tail means the chain is stalling under load. Pinned to 7 days because recent consensus behaviour is what matters here, so the window picker above does not change it.',
        fetch: w => dashApi('blocks/time-histogram', w),
        opt: rows => dashBase({
          tooltip: { trigger: 'axis' },
          xAxis: dashCatAxis(rows.map(r => r.bin)),
          yAxis: dashValAxis('blocks'),
          series: [{ type: 'bar', data: rows.map(r => r.blocks), itemStyle: { color: DASH_PAL[5] } }],
        }),
      },
      {
        id: 'block-proposers',
        title: 'block proposers',
        wide: true,
        state: { topN: 15 },
        why: 'Decentralisation. Blocks proposed per validator over the window — a heavily skewed bar means a few validators dominate block production. Names come from the r/gnops/valopers registry; validators that never registered a moniker show as truncated addresses.',
        controls: (bar, rerender, state) => {
          bar.appendChild(el('span', { className: 'label' }, 'top'));
          const seg = el('div', { className: 'dash-seg' });
          [10, 15, 25, 50].forEach(n => {
            const b = el('button', { type: 'button', className: n === state.topN ? 'on' : '' }, String(n));
            b.addEventListener('click', () => { state.topN = n; rerender(); });
            seg.appendChild(b);
          });
          bar.appendChild(seg);
        },
        fetch: async function (w) {
          // _valMonikers is only populated by the blocks view, so this chart
          // loads it itself. loadValMonikers already swallows its own errors,
          // so a registry failure degrades to truncated addresses rather than
          // failing the card.
          const [rows] = await Promise.all([
            dashApi('blocks/proposers?topN=' + (this.state && this.state.topN ? this.state.topN : 15), w),
            loadValMonikers(),
          ]);
          return rows;
        },
        opt: rows => {
          const labels = rows.map(r => _valMonikers[r.address] || truncAddr(r.address));
          return dashBase({
            grid: { left: 130, right: 20, top: 10, bottom: 28 },
            tooltip: { trigger: 'axis' },
            xAxis: dashValAxis('blocks'),
            yAxis: dashCatAxis(labels.slice().reverse()),
            series: [{ type: 'bar', data: rows.map(r => r.blocks).reverse(), itemStyle: { color: DASH_PAL[0] } }],
          });
        },
      },
```

`fetch` is a `function` rather than an arrow for the proposer chart so `this.state` resolves to the chart object.

- [ ] **Step 2: Verify in the browser**

```bash
go build -o /tmp/mygnoscan . && /tmp/mygnoscan -listen :8899
```

Let it sync for a minute, then open `http://localhost:8899/dashboards`. Verify by pulling real chart state, not screenshots alone:

1. Seven cards in Chain Pulse.
2. **Histogram is pinned:** capture `_dashCharts['block-time-histogram'].getOption().series[0].data`, click `30d` in the window picker, and confirm that series is **unchanged** while `blocks-per-bucket` changed. Confirm it has exactly 9 bins.
3. **Bins sum correctly:** the histogram's total equals the block count in a 7d window minus one (the first block has no predecessor). Cross-check against `curl 'localhost:8899/api/timeseries/blocks?network=<net>&window=7d'`.
4. **Top-N control works:** click `10`, `25`, `50` and confirm the proposer bar count changes and `getOption().yAxis[0].data.length` matches.
5. **Monikers resolve:** confirm proposer labels are monikers where the registry has them and truncated addresses otherwise — not raw 40-character addresses.
6. Console clean, and switching networks re-renders all three.

Stop the server.

- [ ] **Step 3: Commit**

```bash
git add frontend/index.html
git commit -m "feat: add block-time, throughput and proposer charts"
```

---

## Task 7: Coverage note and batch 1 retitle

Spec §9, §10. Two small user-facing corrections: signal that history is still backfilling, and disambiguate messages from transactions.

**Files:**
- Modify: `frontend/index.html` — `loadDashboards` (~:3819), the `tx-by-type` chart entry, CSS

**Interfaces:**
- Consumes: `/api/blocks/coverage` from Task 5; `loadDashboards`, `el()`, `api()`.
- Produces: `.dash-note` styling; a coverage note rendered above the section grid.

- [ ] **Step 1: Add the note CSS**

After the `.dash-msg` rule, add:

```css
.dash-note { color: var(--fg2); font-size: 11px; margin-bottom: 10px; }
```

- [ ] **Step 2: Render the coverage note**

In `loadDashboards`, immediately after `root.appendChild(grid);`, add:

```js
  // A 90d window showing three days of blocks is indistinguishable from a
  // chain that was down. Say which it is while the backfill is still running.
  if (section.charts.some(c => c.id.startsWith('block'))) {
    api('blocks/coverage').then(cov => {
      if (!cov || cov.complete || !cov.min_time) return;
      const note = el('div', { className: 'dash-note' },
        'history backfilling — block charts currently cover ' +
        cov.min_time.slice(0, 10) + ' to ' + cov.max_time.slice(0, 10));
      root.insertBefore(note, grid);
    }).catch(() => {});
  }
```

- [ ] **Step 3: Retitle batch 1's chart**

In the `tx-by-type` chart entry, change the title from `'transactions per bucket, by message type'` to:

```js
        title: 'messages per bucket, by type',
```

and replace its `why` with:

```js
        why: 'Total on-chain activity and what it is made of. A shift in the mix between calls, sends, runs and deploys shows what the chain is actually being used for. This counts messages, not transactions: one transaction carries one or more messages, so this number is always greater than or equal to the transaction count in the throughput chart below.',
```

- [ ] **Step 4: Verify in the browser**

```bash
go build -o /tmp/mygnoscan . && /tmp/mygnoscan -listen :8899
```

Open `http://localhost:8899/dashboards`. Expected:
1. While the backfill is running, a note above the grid reading "history backfilling — block charts currently cover YYYY-MM-DD to YYYY-MM-DD".
2. The note disappears on the Economics section (no block charts there).
3. The first card reads "messages per bucket, by type", and its `ⓘ` tooltip explains the message-vs-transaction relationship.
4. Console clean.

To confirm the note hides when complete, set the flag by hand and reload:

```bash
sqlite3 mygnoscan.db "INSERT OR REPLACE INTO sync_state (key, value) VALUES ('blocks_backfill_done:gnoland1', '1');"
```
Expected: the note no longer renders. Undo it afterwards by deleting that row if you intend to keep syncing.

Stop the server.

- [ ] **Step 5: Update the roadmap**

In `docs/superpowers/specs/2026-08-13-chain-analytics-dashboards-design.md` §5, tick batch 2a:

```markdown
- [x] **Batch 2a — `blocks` table.**
```

- [ ] **Step 6: Commit**

```bash
git add frontend/index.html docs/superpowers/specs/2026-08-13-chain-analytics-dashboards-design.md
git commit -m "feat: add backfill coverage note and clarify message vs transaction"
```

---

## Done when

- `/dashboards` Chain Pulse shows seven charts, three of them block-derived, all from real data
- The block-time histogram stays pinned to 7d while the window picker drives the others
- The proposer top-N control re-queries and re-renders, with monikers resolved
- A coverage note appears while backfilling and disappears when complete
- `?window=` and the network selector re-scope every chart
- `gofmt -l .` is empty, `go vet ./...` and `go test ./...` pass
- Batch 2a is ticked in the parent spec's roadmap
