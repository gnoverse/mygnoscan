package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The read-path query functions, against a fake indexer over real HTTP.
// None of these had a test: every one was only ever exercised in production.

func TestGetRecentTransactions(t *testing.T) {
	f, c := newFakeIndexer(t)
	f.seedChain(100, 30)

	tests := []struct {
		name       string
		maxResults int
		want       int
	}{
		{"a cap smaller than the result set truncates", 10, 10},
		{"a cap larger than the result set keeps everything", 500, 30},
		{"zero means no cap", 0, 30},
		{"negative means no cap", -1, 30},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			txs, err := c.GetRecentTransactions(context.Background(), tt.maxResults)
			if err != nil {
				t.Fatalf("GetRecentTransactions: %v", err)
			}
			if len(txs) != tt.want {
				t.Fatalf("got %d transactions, want %d", len(txs), tt.want)
			}
			// Truncation must keep the newest rows. Taking the tail would serve
			// the oldest transactions on a page labelled "recent".
			if len(txs) > 0 && txs[0].BlockHeight != 129 {
				t.Errorf("first row is height %d, want the tip at 129", txs[0].BlockHeight)
			}
		})
	}
}

func TestGetRecentBlocks(t *testing.T) {
	f, c := newFakeIndexer(t)
	f.seedChain(1, 200)

	blocks, err := c.GetRecentBlocks(context.Background(), 20)
	if err != nil {
		t.Fatalf("GetRecentBlocks: %v", err)
	}
	if len(blocks) == 0 {
		t.Fatal("no blocks")
	}
	if blocks[0].Height != 200 {
		t.Errorf("first block is %d, want the tip at 200", blocks[0].Height)
	}
	for i := 1; i < len(blocks); i++ {
		if blocks[i].Height >= blocks[i-1].Height {
			t.Fatalf("blocks are not descending at %d: %d then %d",
				i, blocks[i-1].Height, blocks[i].Height)
		}
	}
}

// A limit of zero used to mean "every block the chain has", which is a query
// that never finishes on a real chain.
func TestGetRecentBlocksDefaultsItsLimit(t *testing.T) {
	f, c := newFakeIndexer(t)
	f.seedChain(1, 500)

	blocks, err := c.GetRecentBlocks(context.Background(), 0)
	if err != nil {
		t.Fatalf("GetRecentBlocks: %v", err)
	}
	if len(blocks) > 60 {
		t.Errorf("an absent limit returned %d blocks; it should fall back to a window, not the chain", len(blocks))
	}
}

func TestGetTransactionByHash(t *testing.T) {
	f, c := newFakeIndexer(t)
	f.seedChain(100, 5)

	t.Run("a known hash comes back", func(t *testing.T) {
		tx, err := c.GetTransactionByHash(context.Background(), "tx-call-102")
		if err != nil {
			t.Fatalf("GetTransactionByHash: %v", err)
		}
		if tx == nil {
			t.Fatal("got nil for a hash that exists")
		}
		if tx.BlockHeight != 102 {
			t.Errorf("height = %d, want 102", tx.BlockHeight)
		}
	})

	t.Run("an unknown hash is a not-found error", func(t *testing.T) {
		// A miss is reported as an error carrying the hash, which HandleTx turns
		// into a 404 rather than a 500. The distinction matters to the caller:
		// a typo in the search box is not an outage.
		tx, err := c.GetTransactionByHash(context.Background(), "nope")
		if err == nil {
			t.Fatalf("a hash that does not exist came back as %+v", tx)
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("error does not read as a miss: %v", err)
		}
	})
}

func TestGetTransactionsByBlock(t *testing.T) {
	f, c := newFakeIndexer(t)
	f.seedChain(100, 10)

	txs, err := c.GetTransactionsByBlock(context.Background(), 105)
	if err != nil {
		t.Fatalf("GetTransactionsByBlock: %v", err)
	}
	if len(txs) != 1 {
		t.Fatalf("got %d transactions for block 105, want 1", len(txs))
	}
	if txs[0].BlockHeight != 105 {
		t.Errorf("block %d leaked into a query for 105", txs[0].BlockHeight)
	}
}

func TestGetTransactionsByAddress(t *testing.T) {
	f, c := newFakeIndexer(t)
	when := "2026-08-01T00:00:00Z"
	f.add(
		fakeCall(10, when, "g1alice", "gno.land/r/demo/boards", "Post"),
		fakeCall(11, when, "g1bob", "gno.land/r/demo/boards", "Post"),
		fakeSend(12, when, "g1alice", "g1bob", "1000ugnot"),
	)

	txs, err := c.GetTransactionsByAddress(context.Background(), "g1alice")
	if err != nil {
		t.Fatalf("GetTransactionsByAddress: %v", err)
	}
	if len(txs) == 0 {
		t.Fatal("no transactions for an address that has some")
	}
	// The address appears as a caller in one and a sender in another; both are
	// its activity, and a page that shows only calls is missing half the story.
	for _, tx := range txs {
		if !mentions(tx, "g1alice") {
			t.Errorf("tx %s does not mention g1alice", tx.Hash)
		}
	}
}

func mentions(tx Transaction, addr string) bool {
	for _, m := range tx.Messages {
		if m.Value.Caller == addr || m.Value.Creator == addr ||
			m.Value.FromAddress == addr || m.Value.ToAddress == addr {
			return true
		}
	}
	return false
}

func TestGetRecentTransactionsWithEvents(t *testing.T) {
	f, c := newFakeIndexer(t)
	f.seedChain(100, 10)

	txs, err := c.GetRecentTransactionsWithEvents(context.Background(), 5)
	if err != nil {
		t.Fatalf("GetRecentTransactionsWithEvents: %v", err)
	}
	if len(txs) == 0 {
		t.Fatal("no transactions")
	}
	for _, tx := range txs {
		if tx.Response == nil || len(tx.Response.Events) == 0 {
			t.Errorf("tx %s came back without its events — the field set is wrong", tx.Hash)
		}
	}
}

func TestGetGasUsageForRealm(t *testing.T) {
	f, c := newFakeIndexer(t)
	when := "2026-08-01T00:00:00Z"
	f.add(
		fakeCall(10, when, "g1alice", "gno.land/r/demo/boards", "Post"),
		fakeCall(11, when, "g1bob", "gno.land/r/demo/users", "Register"),
	)

	txs, err := c.GetGasUsageForRealm(context.Background(), "gno.land/r/demo/boards")
	if err != nil {
		t.Fatalf("GetGasUsageForRealm: %v", err)
	}
	for _, tx := range txs {
		for _, m := range tx.Messages {
			if m.Value.PkgPath != "" && !strings.Contains(m.Value.PkgPath, "boards") {
				t.Errorf("realm %q leaked into a query for boards", m.Value.PkgPath)
			}
		}
	}
}

// The failure modes, which are what actually broke in production.
func TestQueryFailureModes(t *testing.T) {
	t.Run("a non-200 names the status", func(t *testing.T) {
		f, c := newFakeIndexer(t)
		f.seedChain(1, 5)
		f.status = 403

		_, err := c.GetRecentTransactions(context.Background(), 10)
		if err == nil {
			t.Fatal("a 403 was reported as success")
		}
		// The indexer answers with an HTML page, which used to surface as
		// "invalid character '<'" and sent the reader hunting for a bad query.
		if !strings.Contains(err.Error(), "403") {
			t.Errorf("error does not mention the status: %v", err)
		}
	})

	t.Run("a graphql error is reported", func(t *testing.T) {
		f, c := newFakeIndexer(t)
		f.gqlError = "field does not exist"

		if _, err := c.GetRecentTransactions(context.Background(), 10); err == nil {
			t.Fatal("a GraphQL error was reported as success")
		}
	})

	t.Run("the element cap returns rows alongside its error", func(t *testing.T) {
		f, c := newFakeIndexer(t)
		f.seedChain(1, 100)
		f.capAt = 20

		// LatestBlockHeight is unaffected, so this exercises the transaction
		// path specifically.
		_, err := c.GetRecentTransactions(context.Background(), 0)
		if !errors.Is(err, errQueryTooLarge) {
			t.Fatalf("error = %v, want errQueryTooLarge", err)
		}
	})

	t.Run("a capped query does not count against the breaker", func(t *testing.T) {
		f, c := newFakeIndexer(t)
		f.seedChain(1, 100)
		f.capAt = 20

		// The cap describes the query, not the indexer's health. Tripping the
		// breaker on it would take a working indexer offline for a cooldown
		// because someone asked for too much.
		for i := 0; i < clientBreakerThreshold+2; i++ {
			_, _ = c.GetRecentTransactions(context.Background(), 0)
		}
		if c.breakerOpen() {
			t.Error("the breaker opened on capped queries; the indexer is healthy")
		}
	})

	t.Run("a cancelled context does not count against the breaker", func(t *testing.T) {
		f, c := newFakeIndexer(t)
		f.seedChain(1, 5)
		f.delay = 200 * time.Millisecond

		// A browser that navigates away cancels its request. That says nothing
		// about the indexer, and counting it would let a few impatient users
		// take a healthy network offline for a cooldown.
		for i := 0; i < clientBreakerThreshold+2; i++ {
			ctx, cancel := context.WithCancel(context.Background())
			go func() { time.Sleep(10 * time.Millisecond); cancel() }()
			_, _ = c.GetRecentTransactions(ctx, 10)
			cancel()
		}
		if c.breakerOpen() {
			t.Error("the breaker opened on caller-side cancellation, which says nothing about the indexer")
		}
	})

	t.Run("a deadline does count against the breaker", func(t *testing.T) {
		f, c := newFakeIndexer(t)
		f.seedChain(1, 5)
		f.delay = 200 * time.Millisecond

		// The mirror image, and the reason cancellation is treated separately
		// rather than every context error being waved through: a deadline is
		// the caller saying the indexer was too slow, which is exactly the
		// signal the breaker exists to act on. fanOut sets one per network.
		for i := 0; i < clientBreakerThreshold; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			_, _ = c.GetRecentTransactions(ctx, 10)
			cancel()
		}
		if !c.breakerOpen() {
			t.Error("repeated deadlines left the breaker closed; a slow indexer keeps being waited on")
		}
	})

	t.Run("real failures do open the breaker", func(t *testing.T) {
		f, c := newFakeIndexer(t)
		f.status = 500

		for i := 0; i < clientBreakerThreshold; i++ {
			_, _ = c.GetRecentTransactions(context.Background(), 10)
		}
		if !c.breakerOpen() {
			t.Error("the breaker stayed closed after repeated 500s")
		}

		// And once open, it stops sending requests rather than waiting out the
		// timeout on every page load.
		before := len(f.askedQueries())
		if _, err := c.GetRecentTransactions(context.Background(), 10); !errors.Is(err, errIndexerUnavailable) {
			t.Errorf("error = %v, want errIndexerUnavailable", err)
		}
		if after := len(f.askedQueries()); after != before {
			t.Errorf("%d request(s) went out with the breaker open", after-before)
		}
	})
}

// A successful query clears the failure count, so a blip does not leave the
// breaker one failure from opening forever.
func TestBreakerResetsOnSuccess(t *testing.T) {
	f, c := newFakeIndexer(t)
	f.seedChain(1, 5)

	f.status = 500
	_, _ = c.GetRecentTransactions(context.Background(), 10)

	f.status = 0
	if _, err := c.GetRecentTransactions(context.Background(), 10); err != nil {
		t.Fatalf("recovery query failed: %v", err)
	}

	f.status = 500
	_, _ = c.GetRecentTransactions(context.Background(), 10)
	if c.breakerOpen() {
		t.Error("the breaker opened after one failure; the success in between should have reset the count")
	}
}
