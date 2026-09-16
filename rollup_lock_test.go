package main

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

// Reads must proceed while the rollup rebuilds.
//
// They did not. Every read path took the process-wide RWMutex for reading and
// RefreshRollups held it exclusively for the whole rebuild — ~32 seconds out of
// every 300 on production, so roughly one page load in nine was multi-second
// for no reason visible to the reader: 8.14s against 0.06–0.16s for the same
// endpoint outside the window. The database opens in WAL mode precisely so
// readers proceed alongside a writer; the serialization was imposed above
// SQLite and undid it.
//
// This asserts the property rather than the timing. A duration assertion would
// be flaky on a loaded CI box, and would pass for the wrong reason whenever the
// rebuild happened to be fast. What matters is that a reader *can* complete
// while a rebuild is in flight.
func TestRollupDoesNotBlockOnReaders(t *testing.T) {
	db := newTestDB(t)
	db.SetConfiguredNetworks([]NetworkConfig{{ID: "alpha"}})

	const when = "2026-08-01T00:00:00Z"
	for i := 0; i < 50; i++ {
		hash := "tx" + strconv.Itoa(i)
		if err := db.UpsertTransaction("alpha", hash, 100+i, when, 1000, 2000, 10, true); err != nil {
			t.Fatalf("UpsertTransaction: %v", err)
		}
		if err := db.InsertCall("alpha", hash, 100+i, 0, when, "g1caller", "gno.land/r/demo/boards", "Post", true); err != nil {
			t.Fatalf("InsertCall: %v", err)
		}
	}

	// Hold the read lock the way an in-flight page load does, then run a real
	// refresh and require it to finish anyway.
	//
	// Stated from this side on purpose, because it is the direction that can be
	// made deterministic. Asserting "a read finishes during a rebuild" depends
	// on the rebuild still being in flight when the read arrives, which on a
	// seeded test database it is not — that version passed against the old code
	// simply because the rebuild outran it.
	//
	// This cannot pass against the old code: a rebuild that takes the exclusive
	// lock has to wait for every reader to leave, so with one held open it
	// never returns.
	db.mu.RLock()
	defer db.mu.RUnlock()

	done := make(chan error, 1)
	go func() { done <- db.RefreshRollups() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RefreshRollups: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the rollup blocked behind an open reader; the rebuild must not share a lock with reads")
	}
}

// Writers must still be serialized against each other. Unblocking readers by
// removing the lock entirely would leave the syncer's inserts racing the
// rollup's transaction for SQLite's single writer slot, where the loser waits
// out busy_timeout or fails outright.
func TestWritersRemainSerialized(t *testing.T) {
	db := newTestDB(t)
	db.SetConfiguredNetworks([]NetworkConfig{{ID: "alpha"}})

	db.writeMu.Lock()

	started := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		close(started)
		_ = db.UpsertTransaction("alpha", "tx1", 1, "2026-08-01T00:00:00Z", 1, 2, 3, true)
		close(finished)
	}()

	<-started
	select {
	case <-finished:
		db.writeMu.Unlock()
		t.Fatal("a write completed while another writer held the lock; writers must stay serialized")
	case <-time.After(150 * time.Millisecond):
		// Correctly blocked.
	}

	db.writeMu.Unlock()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the write never completed after the lock was released")
	}
}

// The backoff must not be served while holding the write lock: a contended
// refresh would otherwise stall writers for the rebuild plus six seconds.
func TestRefreshBackoffDoesNotHoldTheWriteLock(t *testing.T) {
	db := newTestDB(t)
	db.SetConfiguredNetworks([]NetworkConfig{{ID: "alpha"}})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := db.RefreshRollups(); err != nil {
			t.Errorf("RefreshRollups: %v", err)
		}
	}()

	// A write issued while the refresh runs must complete rather than wait out
	// the whole retry schedule.
	done := make(chan error, 1)
	go func() {
		done <- db.UpsertTransaction("alpha", "tx-during", 1, "2026-08-01T00:00:00Z", 1, 2, 3, true)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("write during refresh: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a write waited out the refresh's full retry schedule")
	}
	wg.Wait()
}
