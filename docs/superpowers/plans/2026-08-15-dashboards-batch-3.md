# Dashboards Batch 3 — Storage Economics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Persist chain-state storage events and ship the four charts they unlock, retiring the source-code-bytes metric that currently masquerades as "storage".

**Architecture:** A `storage_events` table filled by a new `syncStorageEvents` pass with its own cursor, reading events the syncer already fetches. Two existing endpoints are repointed off source bytes and one is added; four charts join the Economics section, including this codebase's first treemap.

**Tech Stack:** Go (stdlib + `modernc.org/sqlite`), vanilla JS with the repo's `el()` helper, ECharts 5 from CDN. No bundler, no build step.

**Spec:** [`docs/superpowers/specs/2026-08-15-dashboards-batch-3-storage-design.md`](../specs/2026-08-15-dashboards-batch-3-storage-design.md)

## Global Constraints

- **Everything is network-scoped.** Every query, join and aggregate filters or groups by `network`. `networkParam` maps a missing `network` **and** `network=all` to `""`, which several existing readers treat as "omit the filter". A query that hardcodes `WHERE network = ?` returns nothing in that state — that bug shipped once and was caught only at final review.
- **The frontend builds DOM, never HTML strings.** Use `el()`. No `innerHTML` with interpolated data. `pkg_path` is attacker-controlled.
- **No build step.** No bundler, no npm, no framework, no JS test runner.
- **Idempotent inserts** — `INSERT ... ON CONFLICT` against declared keys; sync is re-runnable and the loop logs-and-continues per item by design.
- **Cursors derive from stored data**, not separate state — `MAX(block_height)` for that network.
- **Errors go up from query paths.** Only the sync loop logs and continues.
- **Go gates before any commit:** `gofmt -l .` prints nothing, `go vet ./...` passes, `go test ./...` passes.
- **Commits are conventional and single-line.** **No co-author or attribution trailers.**
- **Go tests are table-driven** with a real temp SQLite file, never mocks.
- **`bytes_delta` is stored signed as emitted** — deposits positive, unlocks negative. Never floor a cumulative or net figure at zero.

---

## File Structure

| File | Change | Responsibility |
|---|---|---|
| `db.go` | Schema + queries | `storage_events` DDL; `UpsertStorageEvents`; three read queries |
| `db_test.go` | Tests | Event-index key, sign convention, malformed rows, network isolation |
| `syncer.go` | Add `syncStorageEvents` | Cursor, walk, event extraction |
| `syncer_test.go` | Tests | Extraction indexing, cursor, resume |
| `api.go` | Repoint 2 handlers, add 1 | `/timeseries/storage`, `/timeseries/storage/realms`, `/storage/consumers` |
| `api_test.go` | Test | Route precedence |
| `main.go` | Add 1 route | `/api/storage/consumers` |
| `frontend/index.html` | 4 charts + migration | Economics cards; `renderStorageChart` migration |
| `docs/api.md` | Update | Repointed shapes, new endpoint |

---

## Task 1: `storage_events` schema and batched upsert

**Files:**
- Modify: `db.go` (DDL in `initSchema`; new query group)
- Test: `db_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `type StorageEventRow struct { TxHash string; EventIndex, BlockHeight int; BlockTime, PkgPath, Kind string; BytesDelta, FeeAmount int; FeeDenom string }`
  - `func (d *DB) UpsertStorageEvents(network string, rows []StorageEventRow) error`
  - `func (d *DB) StorageEventsLastHeight(network string) (int, bool, error)` — `ok` false when the network has none.

- [ ] **Step 1: Write the failing tests**

Append to `db_test.go`:

```go
func TestUpsertStorageEventsKeepsSamePkgPathEvents(t *testing.T) {
	// 13 of 201 real mainnet transactions emit two or more storage events
	// sharing BOTH kind and pkg_path. A key of (network, tx_hash, pkg_path,
	// kind) would collapse them and under-count bytes; event_index is what
	// keeps them distinct.
	db := newTestDB(t)

	rows := []StorageEventRow{
		{TxHash: "TX1", EventIndex: 0, BlockHeight: 10, BlockTime: "2026-08-10T00:00:00Z",
			PkgPath: "gno.land/r/demo/foo", Kind: "deposit", BytesDelta: 100, FeeAmount: 10000, FeeDenom: "ugnot"},
		{TxHash: "TX1", EventIndex: 1, BlockHeight: 10, BlockTime: "2026-08-10T00:00:00Z",
			PkgPath: "gno.land/r/demo/foo", Kind: "deposit", BytesDelta: 250, FeeAmount: 25000, FeeDenom: "ugnot"},
	}
	if err := db.UpsertStorageEvents("gnoland1", rows); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	var n, total int
	if err := db.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(bytes_delta), 0) FROM storage_events WHERE network = ?`, "gnoland1",
	).Scan(&n, &total); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("row count = %d, want 2 — the second event was collapsed", n)
	}
	if total != 350 {
		t.Errorf("summed bytes_delta = %d, want 350", total)
	}
}

func TestUpsertStorageEventsIsIdempotent(t *testing.T) {
	// One observed transaction emitted 58 storage events; re-syncing a page
	// must be a no-op.
	db := newTestDB(t)

	rows := make([]StorageEventRow, 0, 58)
	for i := 0; i < 58; i++ {
		rows = append(rows, StorageEventRow{
			TxHash: "TXBIG", EventIndex: i, BlockHeight: 20, BlockTime: "2026-08-11T00:00:00Z",
			PkgPath: "gno.land/r/demo/bar", Kind: "deposit", BytesDelta: 10, FeeAmount: 1000, FeeDenom: "ugnot",
		})
	}
	for i := 0; i < 3; i++ {
		if err := db.UpsertStorageEvents("gnoland1", rows); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}

	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM storage_events WHERE network = ?`, "gnoland1").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 58 {
		t.Errorf("row count = %d, want 58 after three identical upserts", n)
	}
}

func TestStorageEventsLastHeight(t *testing.T) {
	db := newTestDB(t)

	if _, ok, err := db.StorageEventsLastHeight("gnoland1"); err != nil || ok {
		t.Fatalf("empty: ok = %v, err = %v; want ok=false, err=nil", ok, err)
	}

	if err := db.UpsertStorageEvents("gnoland1", []StorageEventRow{
		{TxHash: "TX1", EventIndex: 0, BlockHeight: 5, BlockTime: "2026-08-10T00:00:00Z",
			PkgPath: "gno.land/r/demo/foo", Kind: "deposit", BytesDelta: 1},
		{TxHash: "TX2", EventIndex: 0, BlockHeight: 42, BlockTime: "2026-08-11T00:00:00Z",
			PkgPath: "gno.land/r/demo/foo", Kind: "deposit", BytesDelta: 1},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Another network's higher height must not move this network's cursor.
	if err := db.UpsertStorageEvents("test12", []StorageEventRow{
		{TxHash: "TX3", EventIndex: 0, BlockHeight: 9999, BlockTime: "2026-08-11T00:00:00Z",
			PkgPath: "gno.land/r/demo/foo", Kind: "deposit", BytesDelta: 1},
	}); err != nil {
		t.Fatalf("upsert other network: %v", err)
	}

	h, ok, err := db.StorageEventsLastHeight("gnoland1")
	if err != nil || !ok {
		t.Fatalf("ok = %v, err = %v; want ok=true, err=nil", ok, err)
	}
	if h != 42 {
		t.Errorf("last height = %d, want 42 — another network's rows leaked in", h)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestUpsertStorageEvents|TestStorageEventsLastHeight' ./... -v`

Expected: FAIL to compile — `undefined: StorageEventRow`, `db.UpsertStorageEvents undefined`, `db.StorageEventsLastHeight undefined`.

- [ ] **Step 3: Add the schema**

In `db.go`, inside `initSchema`'s DDL, after the `blocks` table and before the `CREATE INDEX` statements:

```sql
		CREATE TABLE IF NOT EXISTS storage_events (
			network      TEXT NOT NULL DEFAULT 'gnoland1',
			tx_hash      TEXT NOT NULL,
			event_index  INTEGER NOT NULL,
			block_height INTEGER NOT NULL,
			block_time   TEXT,
			pkg_path     TEXT NOT NULL,
			kind         TEXT NOT NULL,
			bytes_delta  INTEGER NOT NULL,
			fee_amount   INTEGER NOT NULL DEFAULT 0,
			fee_denom    TEXT,
			PRIMARY KEY (network, tx_hash, event_index)
		);
```

And with the other indexes:

```sql
		CREATE INDEX IF NOT EXISTS idx_storage_events_time ON storage_events(network, block_time);
		CREATE INDEX IF NOT EXISTS idx_storage_events_pkg  ON storage_events(network, pkg_path);
```

- [ ] **Step 4: Add the row type and writers**

In `db.go`, in a new contiguous block (keep all storage-event queries together):

```go
// --- storage events ---

// StorageEventRow is one StorageDepositEvent or StorageUnlockEvent.
//
// EventIndex is the event's ordinal in its transaction's FULL event list, not
// among storage events only, so it stays stable if a later batch persists
// GnoEvent rows from the same list. It is in the primary key because 13 of 201
// real mainnet transactions emit two or more events sharing both kind and
// pkg_path — a key without it silently drops events and under-counts bytes.
type StorageEventRow struct {
	TxHash      string
	EventIndex  int
	BlockHeight int
	BlockTime   string
	PkgPath     string
	Kind        string // "deposit" | "unlock"
	BytesDelta  int    // signed as the chain emits it: deposits +, unlocks -
	FeeAmount   int
	FeeDenom    string
}

// UpsertStorageEvents writes many events under a single lock and a single
// SQLite transaction, mirroring UpsertTransactions and UpsertBlocks.
//
// Not an optimisation: one observed transaction emitted 58 storage events, and
// the comment on UpsertTransactions records that per-row writes made read
// requests queue behind a backfill of a hundred rows.
func (d *DB) UpsertStorageEvents(network string, rows []StorageEventRow) error {
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
		INSERT INTO storage_events
			(network, tx_hash, event_index, block_height, block_time, pkg_path, kind, bytes_delta, fee_amount, fee_denom)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (network, tx_hash, event_index) DO UPDATE SET
			block_time  = excluded.block_time,
			bytes_delta = excluded.bytes_delta,
			fee_amount  = excluded.fee_amount,
			fee_denom   = excluded.fee_denom`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, r := range rows {
		if _, err := stmt.Exec(network, r.TxHash, r.EventIndex, r.BlockHeight, r.BlockTime,
			r.PkgPath, r.Kind, r.BytesDelta, r.FeeAmount, r.FeeDenom); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// StorageEventsLastHeight is the highest block height with a stored event for
// this network. The syncer derives its cursor from this rather than from
// separate state; ok is false when the network has none.
func (d *DB) StorageEventsLastHeight(network string) (int, bool, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var h sql.NullInt64
	err := d.db.QueryRow(
		`SELECT MAX(block_height) FROM storage_events WHERE network = ?`, network,
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

Run: `go test -run 'TestUpsertStorageEvents|TestStorageEventsLastHeight' ./... -v`
Expected: PASS (3 tests).

- [ ] **Step 6: Run the full gate**

```bash
gofmt -l . && go vet ./... && go test ./...
```

- [ ] **Step 7: Commit**

```bash
git add db.go db_test.go
git commit -m "feat: add storage_events table"
```

---

## Task 2: `syncStorageEvents`

**Files:**
- Modify: `syncer.go` (new function; call it from `SyncAll`)
- Test: `syncer_test.go`

**Interfaces:**
- Consumes: `StorageEventRow`, `db.UpsertStorageEvents`, `db.StorageEventsLastHeight` from Task 1; `walkTransactions`, `s.client.GetTransactionsFromHeight`, `s.fetchBlockTimes` already in `syncer.go`.
- Produces: `func storageEventRows(tx Transaction, blockTime string) []StorageEventRow` (package-level, testable without a syncer) and `func (s *Syncer) syncStorageEvents(ctx context.Context) error`.

- [ ] **Step 1: Write the failing tests**

Append to `syncer_test.go`:

```go
func TestStorageEventRowsIndexesAgainstTheFullEventList(t *testing.T) {
	// event_index is the ordinal in the transaction's FULL event list, not
	// among storage events only, so a later batch persisting GnoEvent rows can
	// share the numbering. A GnoEvent sitting between two storage events must
	// therefore leave a gap in the indexes we store.
	tx := Transaction{
		Hash:        "TX1",
		BlockHeight: 7,
		Response: &TxResponse{Events: []TxEvent{
			{Typename: "GnoEvent", Type: "Transfer"},
			{Typename: "StorageDepositEvent", PkgPath: "gno.land/r/demo/foo", BytesDelta: 100,
				FeeDelta: &Coin{Amount: 10000, Denom: "ugnot"}},
			{Typename: "GnoEvent", Type: "Approval"},
			{Typename: "StorageUnlockEvent", PkgPath: "gno.land/r/demo/foo", BytesDelta: -40,
				FeeRefund: &Coin{Amount: 4000, Denom: "ugnot"}},
		}},
	}

	rows := storageEventRows(tx, "2026-08-12T00:00:00Z")
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (only the storage events)", len(rows))
	}
	if rows[0].EventIndex != 1 || rows[1].EventIndex != 3 {
		t.Errorf("event indexes = %d, %d; want 1, 3 — indexed against the full list",
			rows[0].EventIndex, rows[1].EventIndex)
	}
	if rows[0].Kind != "deposit" || rows[0].BytesDelta != 100 || rows[0].FeeAmount != 10000 {
		t.Errorf("deposit row = %+v", rows[0])
	}
	// Unlocks carry fee_refund, not fee_delta, and a negative bytes_delta.
	if rows[1].Kind != "unlock" || rows[1].BytesDelta != -40 || rows[1].FeeAmount != 4000 {
		t.Errorf("unlock row = %+v", rows[1])
	}
	if rows[0].BlockTime != "2026-08-12T00:00:00Z" || rows[0].BlockHeight != 7 {
		t.Errorf("block stamp not carried: %+v", rows[0])
	}
}

func TestStorageEventRowsToleratesNilResponse(t *testing.T) {
	// Transaction.Response is a pointer and the indexer can omit it.
	if rows := storageEventRows(Transaction{Hash: "TX1"}, "2026-08-12T00:00:00Z"); len(rows) != 0 {
		t.Errorf("got %d rows from a nil Response, want 0", len(rows))
	}
}

func TestSyncStorageEventsStoresAndResumes(t *testing.T) {
	s, fake, db := newTestSyncer(t, "gnoland1")
	fake.setStorageTxs(10, 12)

	if err := s.syncStorageEvents(context.Background()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	h1, ok, err := db.StorageEventsLastHeight("gnoland1")
	if err != nil || !ok {
		t.Fatalf("after first pass: ok = %v, err = %v", ok, err)
	}
	if h1 != 12 {
		t.Errorf("cursor = %d, want 12", h1)
	}

	// A second pass with nothing new must not error and must not move backward.
	if err := s.syncStorageEvents(context.Background()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	h2, _, _ := db.StorageEventsLastHeight("gnoland1")
	if h2 != h1 {
		t.Errorf("cursor moved from %d to %d on an empty pass", h1, h2)
	}
}
```

The last test needs the fake indexer to serve transactions carrying storage events. Add to `fakeIndexer` in `syncer_test.go`:

```go
	storageLo, storageHi int // heights served with a storage-deposit event
```

```go
// setStorageTxs makes the fake serve one transaction per height in [lo, hi],
// each carrying a single StorageDepositEvent.
func (f *fakeIndexer) setStorageTxs(lo, hi int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.storageLo, f.storageHi = lo, hi
	if hi > f.latestHeight {
		f.latestHeight = hi
	}
}

// serveStorageTxs renders a getTransactions response for the configured range,
// honouring the `block_height: { gt: N }` cursor the syncer sends.
func (f *fakeIndexer) serveStorageTxs(w http.ResponseWriter, query string) {
	from := intAfter(query, "gt:") + 1
	if from < f.storageLo {
		from = f.storageLo
	}
	var parts []string
	for h := from; h <= f.storageHi; h++ {
		parts = append(parts, fmt.Sprintf(
			`{"hash":"TX%d","success":true,"block_height":%d,"messages":[],`+
				`"response":{"events":[{"__typename":"StorageDepositEvent","pkg_path":"gno.land/r/demo/foo",`+
				`"bytes_delta":100,"fee_delta":{"amount":10000,"denom":"ugnot"}}]}}`, h, h))
	}
	fmt.Fprintf(w, `{"data":{"getTransactions":[%s]}}`, strings.Join(parts, ","))
}
```

and in `ServeHTTP`, replace the `default:` arm's blanket empty response with:

```go
	default:
		if f.storageHi > 0 && strings.Contains(query, "getTransactions") {
			f.serveStorageTxs(w, query)
			return
		}
		// Any other transaction query: no results, so sync is a no-op.
		fmt.Fprint(w, `{"data":{"getTransactions":[]}}`)
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestStorageEventRows|TestSyncStorageEvents' ./... -v`

Expected: FAIL to compile — `undefined: storageEventRows`, `s.syncStorageEvents undefined`, `fake.setStorageTxs undefined`.

- [ ] **Step 3: Implement**

In `syncer.go`:

```go
// storageEventRows extracts the storage events from one transaction.
//
// The index recorded is the event's position in the transaction's FULL event
// list, so a later batch persisting GnoEvent rows can share the numbering
// rather than renumbering these.
func storageEventRows(tx Transaction, blockTime string) []StorageEventRow {
	if tx.Response == nil {
		return nil
	}
	var out []StorageEventRow
	for i, ev := range tx.Response.Events {
		var kind string
		var fee *Coin
		switch ev.Typename {
		case "StorageDepositEvent":
			kind, fee = "deposit", ev.FeeDelta
		case "StorageUnlockEvent":
			kind, fee = "unlock", ev.FeeRefund
		default:
			continue
		}
		amount, denom := 0, ""
		if fee != nil {
			amount, denom = fee.Amount, fee.Denom
		}
		out = append(out, StorageEventRow{
			TxHash:      tx.Hash,
			EventIndex:  i,
			BlockHeight: tx.BlockHeight,
			BlockTime:   blockTime,
			PkgPath:     ev.PkgPath,
			Kind:        kind,
			BytesDelta:  ev.BytesDelta,
			FeeAmount:   amount,
			FeeDenom:    denom,
		})
	}
	return out
}

// syncStorageEvents fills the storage_events table.
//
// It walks transactions from its own cursor rather than piggybacking on
// syncCalls' walk. Piggybacking would cost no extra fetching, but that walk's
// cursor comes from the calls table, so on any database already synced to the
// tip it would fetch nothing and leave storage_events permanently empty.
func (s *Syncer) syncStorageEvents(ctx context.Context) error {
	last, ok, err := s.db.StorageEventsLastHeight(s.networkID)
	if err != nil {
		return fmt.Errorf("storage events cursor: %w", err)
	}
	var from *int
	if ok {
		from = &last
	}

	count := 0
	err = walkTransactions(ctx, from, s.client.GetTransactionsFromHeight, func(txs []Transaction) {
		times := s.fetchBlockTimes(ctx, txs)
		var rows []StorageEventRow
		for _, tx := range txs {
			rows = append(rows, storageEventRows(tx, times[tx.BlockHeight])...)
		}
		if len(rows) == 0 {
			return
		}
		if err := s.db.UpsertStorageEvents(s.networkID, rows); err != nil {
			log.Printf("[%s] syncStorageEvents: upsert %d rows: %v", s.networkID, len(rows), err)
			return
		}
		count += len(rows)
	})
	if err != nil {
		return err
	}
	if count > 0 {
		log.Printf("[%s] syncStorageEvents: stored %d events", s.networkID, count)
	}
	return nil
}
```

`walkTransactions(ctx context.Context, cursor *int, fetch txPageFetcher, process func([]Transaction)) error` — the cursor is a `*int` where nil means "from the beginning", which is why `from` is built as a pointer above. `syncCalls` passes the `*int` returned by `getLastRecentTransactionBlockHeight` directly; this task's cursor comes from `StorageEventsLastHeight`, which returns `(int, bool, error)` instead, hence the conversion.

- [ ] **Step 4: Call it from `SyncAll`**

In `SyncAll`, after `syncCalls` and before `syncMsgRuns`'s return, add it to the chain so a failure is reported like the others:

```go
	if err := s.syncStorageEvents(ctx); err != nil {
		return err
	}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -run 'TestStorageEventRows|TestSyncStorageEvents' ./... -v`
Expected: PASS (3 tests).

- [ ] **Step 6: Run the full gate**

```bash
gofmt -l . && go vet ./... && go test ./...
```

- [ ] **Step 7: Commit**

```bash
git add syncer.go syncer_test.go
git commit -m "feat: persist storage deposit and unlock events"
```

---

## Task 3: Storage queries

**Files:**
- Modify: `db.go` — the `// --- storage events ---` group from Task 1; **replace** `GetStorageTimeSeries` and `GetRealmsWithStorage`
- Test: `db_test.go`

**Interfaces:**
- Consumes: the schema and `UpsertStorageEvents` from Task 1; `timeseriesFormat` and `fillBuckets` already in `db.go`.
- Produces:
  - `type StoragePoint struct { Time string; Deposited, Released, Net int }` — JSON `time`, `deposited`, `released`, `net`
  - `type StorageConsumer struct { PkgPath string; Deposited, Released, Net int }` — JSON `pkg_path`, `deposited`, `released`, `net`
  - `GetStorageTimeSeries(network, realmPath, granularity string, days int) ([]StoragePoint, error)` — same name and arity as the function it replaces, so the handler needs no signature change
  - `GetStorageConsumers(network string, days, topN int) ([]StorageConsumer, error)`
  - `GetRealmsWithStorage(network string, days int) ([]string, error)` — same signature, now sourced from events

**Delete:** the old `StorageTimePoint` type and the body of `GetStorageTimeSeries` that sums `LENGTH(pf.body)`.

- [ ] **Step 1: Write the failing tests**

Append to `db_test.go`:

```go
// seedStorage writes one event, defaulting the stamp to now so window filters
// include it.
func seedStorage(t *testing.T, db *DB, network, tx string, idx int, pkg, kind string, bytes int, at time.Time) {
	t.Helper()
	err := db.UpsertStorageEvents(network, []StorageEventRow{{
		TxHash: tx, EventIndex: idx, BlockHeight: 100 + idx,
		BlockTime: at.UTC().Format(time.RFC3339), PkgPath: pkg, Kind: kind,
		BytesDelta: bytes, FeeAmount: bytes * 100, FeeDenom: "ugnot",
	}})
	if err != nil {
		t.Fatalf("seed storage: %v", err)
	}
}

func TestGetStorageTimeSeriesNetsDepositsAgainstUnlocks(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC().Add(-2 * time.Hour)

	seedStorage(t, db, "gnoland1", "TX1", 0, "gno.land/r/demo/foo", "deposit", 1000, now)
	seedStorage(t, db, "gnoland1", "TX1", 1, "gno.land/r/demo/foo", "unlock", -400, now)

	pts, err := db.GetStorageTimeSeries("gnoland1", "", "daily", 7)
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	var dep, rel, net int
	for _, p := range pts {
		dep += p.Deposited
		rel += p.Released
		net += p.Net
	}
	if dep != 1000 {
		t.Errorf("deposited = %d, want 1000", dep)
	}
	// Released stays negative, as the chain emits it.
	if rel != -400 {
		t.Errorf("released = %d, want -400 (kept negative)", rel)
	}
	if net != 600 {
		t.Errorf("net = %d, want 600", net)
	}
}

func TestGetStorageTimeSeriesAllowsNegativeNet(t *testing.T) {
	// A window catching an unlock whose deposit predates it nets negative, and
	// that must survive rather than being floored at zero — it is the signal
	// that events are being summed against history we do not have.
	db := newTestDB(t)
	now := time.Now().UTC().Add(-2 * time.Hour)
	seedStorage(t, db, "gnoland1", "TX1", 0, "gno.land/r/demo/foo", "unlock", -8192, now)

	pts, err := db.GetStorageTimeSeries("gnoland1", "", "daily", 7)
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	net := 0
	for _, p := range pts {
		net += p.Net
	}
	if net != -8192 {
		t.Errorf("net = %d, want -8192 (not floored)", net)
	}
}

func TestGetStorageTimeSeriesSkipsMalformedBlockTime(t *testing.T) {
	// block_time is nullable TEXT compared as a string, so 'not-a-timestamp'
	// passes the window predicate and strftime yields NULL. Batch 2b shipped a
	// 500 from exactly this; the row must be skipped instead.
	db := newTestDB(t)
	now := time.Now().UTC().Add(-2 * time.Hour)
	seedStorage(t, db, "gnoland1", "TX1", 0, "gno.land/r/demo/foo", "deposit", 500, now)
	if err := db.UpsertStorageEvents("gnoland1", []StorageEventRow{{
		TxHash: "TXBAD", EventIndex: 0, BlockHeight: 999, BlockTime: "not-a-timestamp",
		PkgPath: "gno.land/r/demo/foo", Kind: "deposit", BytesDelta: 7,
	}}); err != nil {
		t.Fatalf("seed bad row: %v", err)
	}

	pts, err := db.GetStorageTimeSeries("gnoland1", "", "daily", 7)
	if err != nil {
		t.Fatalf("series returned an error instead of skipping the bad row: %v", err)
	}
	dep := 0
	for _, p := range pts {
		dep += p.Deposited
	}
	if dep != 500 {
		t.Errorf("deposited = %d, want 500 (the good row only)", dep)
	}
}

func TestGetStorageConsumersIsNetworkScoped(t *testing.T) {
	// Realm paths collide across chains, so merging two networks under one
	// label would be actively wrong.
	db := newTestDB(t)
	now := time.Now().UTC().Add(-2 * time.Hour)
	seedStorage(t, db, "gnoland1", "TX1", 0, "gno.land/r/gnoland/blog", "deposit", 1000, now)
	seedStorage(t, db, "test12", "TX2", 0, "gno.land/r/gnoland/blog", "deposit", 9999, now)

	got, err := db.GetStorageConsumers("gnoland1", 7, 10)
	if err != nil {
		t.Fatalf("consumers: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d consumers, want 1", len(got))
	}
	if got[0].PkgPath != "gno.land/r/gnoland/blog" || got[0].Net != 1000 {
		t.Errorf("consumer = %+v, want blog with net 1000 — the other network leaked in", got[0])
	}
}

func TestGetRealmsWithStorageComesFromEvents(t *testing.T) {
	// It used to list realms with package_files; it must now list realms that
	// actually have storage events.
	db := newTestDB(t)
	now := time.Now().UTC().Add(-2 * time.Hour)
	seedStorage(t, db, "gnoland1", "TX1", 0, "gno.land/r/demo/withevents", "deposit", 10, now)
	if err := db.UpsertPackage("gnoland1", "gno.land/r/demo/noevents", "noevents", "g1c", "TX9", 1,
		now.Format(time.RFC3339), true, 1); err != nil {
		t.Fatalf("upsert package: %v", err)
	}

	realms, err := db.GetRealmsWithStorage("gnoland1", 7)
	if err != nil {
		t.Fatalf("realms: %v", err)
	}
	if len(realms) != 1 || realms[0] != "gno.land/r/demo/withevents" {
		t.Errorf("realms = %v, want just the one with storage events", realms)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestGetStorage|TestGetRealmsWithStorage' ./... -v`

Expected: FAIL — `db.GetStorageConsumers undefined`, and the `GetStorageTimeSeries` tests fail on the old `StorageTimePoint` fields (`p.Deposited` undefined).

- [ ] **Step 3: Replace the two old readers and add the new one**

In `db.go`, delete `type StorageTimePoint`, the old `GetStorageTimeSeries` body, and the old `GetRealmsWithStorage` body. Add to the `// --- storage events ---` group:

```go
type StoragePoint struct {
	Time      string `json:"time"`
	Deposited int    `json:"deposited"`
	Released  int    `json:"released"` // negative, as the chain emits it
	Net       int    `json:"net"`
}

type StorageConsumer struct {
	PkgPath   string `json:"pkg_path"`
	Deposited int    `json:"deposited"`
	Released  int    `json:"released"`
	Net       int    `json:"net"`
}

// GetStorageTimeSeries buckets chain-state storage change over time.
//
// This replaces a reader of the same name that summed LENGTH(package_files.body)
// — deployed source-code bytes, which is a different quantity from the state a
// realm pays a deposit to hold. Nothing is floored: a negative net is a real
// signal, either a realm pruning state or events summed against pruned history.
func (d *DB) GetStorageTimeSeries(network, realmPath, granularity string, days int) ([]StoragePoint, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	sqlFmt, step, truncFn := timeseriesFormat(granularity)
	start := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	filter := ""
	args := []any{start}
	if network != "" {
		filter += " AND network = ?"
		args = append(args, network)
	}
	if realmPath != "" {
		filter += " AND pkg_path = ?"
		args = append(args, realmPath)
	}

	q := fmt.Sprintf(`
		SELECT strftime('%s', block_time) AS bucket,
		       COALESCE(SUM(CASE WHEN bytes_delta > 0 THEN bytes_delta ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN bytes_delta < 0 THEN bytes_delta ELSE 0 END), 0),
		       COALESCE(SUM(bytes_delta), 0)
		FROM storage_events
		WHERE block_time >= ?%s
		GROUP BY bucket ORDER BY bucket ASC`, sqlFmt, filter)

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	buckets := make(map[string]*StoragePoint)
	for rows.Next() {
		var bucket sql.NullString
		var dep, rel, net int
		if err := rows.Scan(&bucket, &dep, &rel, &net); err != nil {
			return nil, err
		}
		// A row whose block_time will not parse yields a NULL bucket: string
		// comparison lets garbage past the window filter. Skip the row rather
		// than failing the whole chart.
		if !bucket.Valid || bucket.String == "" {
			continue
		}
		p := StoragePoint{Time: bucket.String, Deposited: dep, Released: rel, Net: net}
		buckets[p.Time] = &p
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return fillBuckets(buckets, days, granularity, step, truncFn,
		func(k string) StoragePoint { return StoragePoint{Time: k} },
		func(p *StoragePoint) {}), nil
}

// GetStorageConsumers aggregates storage change per realm, biggest net first.
func (d *DB) GetStorageConsumers(network string, days, topN int) ([]StorageConsumer, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if topN <= 0 {
		topN = 20
	}
	if topN > 100 {
		topN = 100
	}
	start := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	filter := ""
	args := []any{start}
	if network != "" {
		filter = " AND network = ?"
		args = append(args, network)
	}
	args = append(args, topN)

	q := fmt.Sprintf(`
		SELECT pkg_path,
		       COALESCE(SUM(CASE WHEN bytes_delta > 0 THEN bytes_delta ELSE 0 END), 0) AS deposited,
		       COALESCE(SUM(CASE WHEN bytes_delta < 0 THEN bytes_delta ELSE 0 END), 0) AS released,
		       COALESCE(SUM(bytes_delta), 0) AS net
		FROM storage_events
		WHERE block_time >= ?%s
		GROUP BY pkg_path ORDER BY net DESC LIMIT ?`, filter)

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []StorageConsumer
	for rows.Next() {
		var c StorageConsumer
		if err := rows.Scan(&c.PkgPath, &c.Deposited, &c.Released, &c.Net); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetRealmsWithStorage lists realms that have storage events in the window.
// It used to list realms that had package_files, which is a different set.
func (d *DB) GetRealmsWithStorage(network string, days int) ([]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	start := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	filter := ""
	args := []any{start}
	if network != "" {
		filter = " AND network = ?"
		args = append(args, network)
	}

	rows, err := d.db.Query(
		`SELECT DISTINCT pkg_path FROM storage_events WHERE block_time >= ?`+filter+` ORDER BY pkg_path`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
```

Note `GetRealmsWithStorage` previously did **not** take the read lock — it does now, matching every other reader.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -run 'TestGetStorage|TestGetRealmsWithStorage' ./... -v`
Expected: PASS (6 tests).

- [ ] **Step 5: Run the full gate**

```bash
gofmt -l . && go vet ./... && go test ./...
```

The old `StorageTimePoint` is gone, so `api.go` will not compile until Task 4. If the gate fails only on that reference, proceed to Task 4 and commit both together; otherwise fix what broke.

- [ ] **Step 6: Commit**

```bash
git add db.go db_test.go
git commit -m "feat: replace source-bytes storage queries with chain-state ones"
```

---

## Task 4: Endpoints and routes

**Files:**
- Modify: `api.go` — `HandleTimeSeriesStorage`, `HandleStorageRealms`; add `HandleStorageConsumers`
- Modify: `main.go` — one route
- Test: `api_test.go`

**Interfaces:**
- Consumes: `GetStorageTimeSeries`, `GetStorageConsumers`, `GetRealmsWithStorage` from Task 3; `a.resolveTimeseriesParams(r, network)`, `a.networkParam(r)`, `jsonResponse`, `jsonError`.
- Produces: `GET /api/storage/consumers`; the two repointed endpoints keep their paths.

- [ ] **Step 1: Update the two existing handlers**

In `api.go`, `HandleTimeSeriesStorage` keeps its shape; only the slice type changes:

```go
	if pts == nil {
		pts = []StoragePoint{}
	}
```

`HandleStorageRealms` is unchanged apart from now returning event-sourced realms.

- [ ] **Step 2: Add the consumers handler**

After `HandleStorageRealms`:

```go
func (a *API) HandleStorageConsumers(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, _ := a.resolveTimeseriesParams(r, network)
	topN, _ := strconv.Atoi(r.URL.Query().Get("topN"))
	rows, err := a.db.GetStorageConsumers(network, days, topN)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if rows == nil {
		rows = []StorageConsumer{}
	}
	jsonResponse(w, rows)
}
```

- [ ] **Step 3: Register the route**

In `main.go`, beside the other storage routes:

```go
	mux.HandleFunc("GET /api/storage/consumers", api.HandleStorageConsumers)
```

- [ ] **Step 4: Write the route-precedence test**

`/api/storage/{path...}` already exists and `/api/storage/consumers` sits inside that wildcard. Go resolves it to the literal because it matches a strict subset — pin that rather than trusting it.

Append to `api_test.go`:

```go
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
```

- [ ] **Step 5: Run the gate**

```bash
gofmt -l . && go vet ./... && go test ./...
```

- [ ] **Step 6: Verify against real data**

```bash
go build -o /tmp/mygnoscan-b3 . && cd /tmp && /tmp/mygnoscan-b3 -listen :8899 -db /tmp/b3.db
```

Let it sync for a minute, then:

```bash
curl -s 'localhost:8899/api/timeseries/storage?network=gnoland1&window=all' | head -c 300
curl -s 'localhost:8899/api/storage/consumers?network=gnoland1&window=all&topN=5'
curl -s 'localhost:8899/api/timeseries/storage/realms?network=gnoland1&days=365' | head -c 200
curl -s 'localhost:8899/api/storage/r/gnoland/blog?network=gnoland1' | head -c 150
```

Expected: the series carries `deposited`/`released`/`net`; consumers returns up to 5 realms biggest-net-first; realms lists event-bearing paths; and the last call still reaches the live per-realm handler unchanged. Stop the server.

- [ ] **Step 7: Commit**

```bash
git add api.go main.go api_test.go
git commit -m "feat: add storage consumers endpoint and repoint the storage series"
```

---

## Task 5: Migrate the sanity view off source bytes

**Files:**
- Modify: `frontend/index.html` — `renderStorageChart` (~line 926)

**Interfaces:**
- Consumes: `/api/timeseries/storage` returning `{time, deposited, released, net}` from Task 4.
- Produces: nothing later tasks rely on.

- [ ] **Step 1: Update the chart's data mapping**

In `renderStorageChart`, replace the cumulative computation and the two datasets. The running sum now uses `net`, and the bar shows deposits with releases stacked below zero:

```js
  let cumulative = 0;
  const cumulativeData = data.map(p => { cumulative += p.net; return cumulative; });
```

Datasets become:

```js
      datasets: [
        {
          label: 'deposited',
          data: data.map(p => p.deposited),
          backgroundColor: 'rgba(78,205,196,0.5)',
          borderColor: '#4ecdc4',
          borderWidth: 1,
          order: 2,
        },
        {
          label: 'released',
          data: data.map(p => p.released),
          backgroundColor: 'rgba(255,107,107,0.5)',
          borderColor: '#ff6b6b',
          borderWidth: 1,
          order: 2,
        },
        {
          label: 'cumulative net',
          data: cumulativeData,
          type: 'line',
          borderColor: '#ffd43b',
          backgroundColor: 'rgba(255,212,59,0.08)',
          tension: 0.3,
          fill: true,
          pointRadius: 2,
          order: 1,
        },
      ]
```

And the title, which currently says "source storage growth (bytes)":

```js
        title: { display: true, text: 'chain-state storage (bytes)' + (realm ? ' — ' + realm : ''), color: '#888' },
```

- [ ] **Step 2: Drop the zero-floor on the y axis**

`scaleOpts.y` sets `beginAtZero: true`. Releases are negative and a net curve may go negative, so that would clip real data. In this chart's `options.scales`, override:

```js
      scales: { x: scaleOpts.x, y: { ...scaleOpts.y, beginAtZero: false } },
```

- [ ] **Step 3: Verify in the browser**

Rebuild, serve a database with storage events, and open `/sanity`. Expected:
1. The storage card renders with three series: deposited, released, cumulative net.
2. The realm selector lists realms that have storage events.
3. Selecting a realm re-renders scoped to it.
4. Console clean.

- [ ] **Step 4: Commit**

```bash
git add frontend/index.html
git commit -m "feat: migrate the sanity storage chart to chain-state bytes"
```

---

## Task 6: The two mode-A storage charts

**Files:**
- Modify: `frontend/index.html` — the `economics` section's `charts` array (~line 3869)

**Interfaces:**
- Consumes: `/api/timeseries/storage` from Task 4; `dashApi`, `dashBase`, `dashLegend`, `dashCatAxis`, `dashValAxis`, `cumulative`, `DASH_PAL` already defined.
- Produces: chart ids `storage-growth`, `storage-deposit-release`. Must not collide with existing ids: `tx-by-type`, `cumulative-tx`, `success-rate`, `active-addresses`, `blocks-per-bucket`, `block-time-histogram`, `block-proposers`, `gas-used-wanted`, `fees`, `new-addresses`, `active-rolling`, `activity-heatmap`, `gas-per-tx-histogram`, `function-heatmap`.

- [ ] **Step 1: Append the two charts**

Add to the `economics` section's `charts` array:

```js
      {
        id: 'storage-growth',
        title: 'cumulative storage growth',
        window: 'all',
        networkScoped: true,
        why: 'How much chain state realms are paying to hold, summed across all indexed history. State only grows unless a realm prunes, so the slope is the long-run cost driver. Pinned to the full range: a running total over a partial window understates the real figure. A downward slope means realms released more than they stored.',
        fetch: w => dashApi('timeseries/storage', w),
        opt: rows => dashBase({
          tooltip: { trigger: 'axis' },
          xAxis: dashCatAxis(rows.map(r => r.time)),
          yAxis: dashValAxis('bytes'),
          series: [{
            type: 'line', smooth: true, showSymbol: false,
            areaStyle: { opacity: 0.25 },
            lineStyle: { color: DASH_PAL[4] }, itemStyle: { color: DASH_PAL[4] },
            data: cumulative(rows.map(r => r.net)),
          }],
        }),
      },
      {
        id: 'storage-deposit-release',
        title: 'storage deposited vs released',
        networkScoped: true,
        why: 'Deposits lock GNOT when a realm grows chain state; releases refund it when state is pruned. Released bars sit below zero because the chain emits them as negative deltas. At 100 ugnot per byte, the bar heights are also the fee flow.',
        fetch: w => dashApi('timeseries/storage', w),
        opt: rows => dashBase({
          tooltip: { trigger: 'axis' },
          legend: dashLegend(['deposited', 'released']),
          xAxis: dashCatAxisBars(rows.map(r => r.time)),
          yAxis: dashValAxis('bytes'),
          series: [
            { name: 'deposited', type: 'bar', stack: 's', itemStyle: { color: DASH_PAL[0] }, data: rows.map(r => r.deposited) },
            { name: 'released', type: 'bar', stack: 's', itemStyle: { color: DASH_PAL[1] }, data: rows.map(r => r.released) },
          ],
        }),
      },
```

`dashCatAxisBars` is the `boundaryGap: true` variant added in batch 2b — bar series must use it or the first and last bars render half outside the clip rect.

- [ ] **Step 2: Verify in the browser**

Rebuild, serve a database with storage events, open `/dashboards?section=economics` with a specific network selected. Assert on real chart state:
1. Both cards render; Economics now has four charts (the two new ones plus gas and fees).
2. `_dashCharts['storage-growth'].getOption().series[0].data` is monotonically non-decreasing when no unlocks exist in range, and its x-axis labels do **not** change when the window picker is clicked (it pins `all`), while `storage-deposit-release` does change.
3. With the network selector on "all", both cards show the select-a-network message and no ECharts instance is created for them.
4. Screenshot the deposited-vs-released card and confirm the first and last bars are full width.
5. Console clean.

- [ ] **Step 3: Commit**

```bash
git add frontend/index.html
git commit -m "feat: add storage growth and deposit-vs-release charts"
```

---

## Task 7: The treemap and the net-delta bar

**Files:**
- Modify: `frontend/index.html` — the `economics` section's `charts` array

**Interfaces:**
- Consumes: `/api/storage/consumers` from Task 4; `dashApi`, `dashBase`, `dashTooltipNode`, `dashCatAxisBars`, `dashValAxis`, `dashCompactNumber`, `DASH_PAL`, `el`.
- Produces: chart ids `storage-consumers`, `storage-net-delta`.

**Security note, load-bearing:** `pkg_path` is attacker-controlled — a realm's path is chosen by whoever deployed it. Batch 2b shipped an XSS where a function name reached ECharts' HTML tooltip renderer. **Tooltips must return a DOM node via `dashTooltipNode`, never a string.** Treemap *labels* are drawn on canvas rather than as HTML, so a label formatter returning a string is safe — do not "fix" that one.

- [ ] **Step 1: Append the two charts**

```js
      {
        id: 'storage-consumers',
        title: 'top storage consumers',
        wide: true,
        networkScoped: true,
        state: { topN: 20 },
        why: 'Which realms hold the most chain state, by net bytes. A treemap answers "who is biggest" at a glance. Realms with a negative net (they released more than they stored) are omitted here because a treemap cannot draw a negative area — the net-delta chart below shows them.',
        controls: (bar, rerender, state) => {
          bar.appendChild(el('span', { className: 'label' }, 'top'));
          const seg = el('div', { className: 'dash-seg' });
          [10, 20, 50].forEach(n => {
            const b = el('button', { type: 'button', className: n === state.topN ? 'on' : '' }, String(n));
            b.addEventListener('click', () => { state.topN = n; rerender(); });
            seg.appendChild(b);
          });
          bar.appendChild(seg);
        },
        fetch: function (w) {
          return dashApi('storage/consumers?topN=' + (this.state && this.state.topN ? this.state.topN : 20), w);
        },
        opt: rows => dashBase({
          // pkg_path is attacker-controlled: the tooltip must be a DOM node,
          // never a string, or ECharts writes it as innerHTML.
          tooltip: {
            formatter: p => dashTooltipNode([p.name, dashCompactNumber(p.value) + ' bytes net']),
          },
          series: [{
            type: 'treemap',
            roam: false, nodeClick: false, breadcrumb: { show: false },
            // A treemap cannot represent a negative area.
            data: rows.filter(r => r.net > 0).map(r => ({ name: r.pkg_path, value: r.net })),
            // Labels are canvas-drawn, not HTML, so a string is safe here.
            label: { color: '#06231f', fontSize: 11, formatter: p => p.name },
            levels: [{ itemStyle: { borderColor: '#0a0a0a', borderWidth: 2, gapWidth: 2 } }],
          }],
        }),
      },
      {
        id: 'storage-net-delta',
        title: 'net storage change by realm',
        wide: true,
        networkScoped: true,
        state: { topN: 20 },
        why: 'Net bytes each realm added or released in the window. Bars to the left of zero are realms that pruned more state than they stored, which the treemap above cannot show. Ordered by net, so growth and cleanup sit at opposite ends.',
        controls: (bar, rerender, state) => {
          bar.appendChild(el('span', { className: 'label' }, 'top'));
          const seg = el('div', { className: 'dash-seg' });
          [10, 20, 50].forEach(n => {
            const b = el('button', { type: 'button', className: n === state.topN ? 'on' : '' }, String(n));
            b.addEventListener('click', () => { state.topN = n; rerender(); });
            seg.appendChild(b);
          });
          bar.appendChild(seg);
        },
        fetch: function (w) {
          return dashApi('storage/consumers?topN=' + (this.state && this.state.topN ? this.state.topN : 20), w);
        },
        opt: rows => {
          const ordered = rows.slice().reverse();
          return dashBase({
            grid: { left: 220, right: 20, top: 10, bottom: 28 },
            tooltip: {
              formatter: p => dashTooltipNode([p.name, dashCompactNumber(p.value) + ' bytes net']),
            },
            xAxis: dashValAxis('net bytes'),
            yAxis: dashCatAxisBars(ordered.map(r => r.pkg_path)),
            series: [{
              type: 'bar',
              data: ordered.map(r => ({
                value: r.net,
                name: r.pkg_path,
                itemStyle: { color: r.net < 0 ? DASH_PAL[1] : DASH_PAL[2] },
              })),
            }],
          });
        },
      },
```

`fetch` is a `function` rather than an arrow in both so `this.state` resolves to the chart object.

- [ ] **Step 2: Verify in the browser, including an XSS probe**

Rebuild and serve. With a specific network selected, on `/dashboards?section=economics`:

1. Both cards render; Economics now has six charts.
2. Clicking `10`/`20`/`50` on each card changes its bar or tile count, and the two cards' controls are **independent** — changing one does not change the other.
3. **Exercise a control against a failure**, per the batch 2b lesson: from the console, point one card's `fetch` at a bad path, click a top-N button and confirm the card shows its error message **with the control still present**, then restore the fetch, click again and confirm the chart renders into a live canvas — `_dashCharts['storage-consumers'].getDom()` must be the element actually in the document. A control that renders only on the success path was a real defect in batch 2b.
3. Realms with negative net appear in the net-delta chart in red, on the left of zero, and are **absent** from the treemap.
4. **XSS probe.** Against a copy of the database in `/tmp` (never the repo's own), insert a storage event whose `pkg_path` is a live payload:

```bash
sqlite3 /tmp/b3.db "INSERT INTO storage_events (network, tx_hash, event_index, block_height, block_time, pkg_path, kind, bytes_delta, fee_amount, fee_denom) VALUES ('gnoland1','TXXSS',0,999,strftime('%Y-%m-%dT%H:%M:%SZ','now'),'<img src=x onerror=\"window.__XSS_FIRED=1\">','deposit',999999,99999900,'ugnot');"
```

Reload, hover the tile and the bar for that realm, then assert **all** of:
- `window.__XSS_FIRED` is `undefined`
- `document.querySelectorAll('.dash-chart img').length` is `0` and `document.images.length` is unchanged
- the path renders as literal text in both tooltips

Report the tooltip node's `innerHTML` so the escaping is visible.

5. Console clean.

- [ ] **Step 3: Update `docs/api.md`**

Correct the storage entries: `/api/timeseries/storage` now returns `{time, deposited, released, net}` from storage events rather than source-code bytes; `/api/timeseries/storage/realms` lists realms with storage events; add `/api/storage/consumers` with its `topN` (default 20, capped at 100) and note all three are single-network in the dashboard. Note `released` is negative and that nothing is floored at zero.

- [ ] **Step 4: Tick the roadmap**

In `docs/superpowers/specs/2026-08-13-chain-analytics-dashboards-design.md` §5, tick batch 3:

```markdown
- [x] **Batch 3 — `storage_events` table.**
```

- [ ] **Step 5: Commit**

```bash
git add frontend/index.html docs/api.md docs/superpowers/specs/2026-08-13-chain-analytics-dashboards-design.md
git commit -m "feat: add storage consumers treemap and net-delta chart"
```

---

## Done when

- `/dashboards?section=economics` shows six charts on a specific network, and the four storage cards show the select-a-network message on "all"
- The treemap renders an attacker-controlled `pkg_path` as literal text, verified with a live payload
- The sanity view's storage chart reads chain-state bytes and its selector lists event-bearing realms
- A negative net survives to the chart rather than being floored
- `gofmt -l .` is empty, `go vet ./...` and `go test ./...` pass
- Batch 3 is ticked in the roadmap
