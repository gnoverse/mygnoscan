package indexer

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

// The walk is the whole point of this file: before it, a realm with more than
// ElementCap matching transactions got its newest 10,000 and a `truncated` flag,
// and the derived balance built on top silently stopped matching the chain.
func TestCoinFlowsWalksPastTheElementCap(t *testing.T) {
	tests := []struct {
		name          string
		tip           int
		wantTxs       int
		wantTruncated bool
		wantMinPages  int
	}{
		{
			// One page, nothing capped, no second query.
			name: "inside one page", tip: 4000,
			wantTxs: 4000, wantTruncated: false, wantMinPages: 1,
		},
		{
			// Three pages. The cap trims the trailing height of each capped
			// page, so the pages do not line up on round numbers, and the total
			// still has to be every transaction and each of them once.
			name: "three pages", tip: 25000,
			wantTxs: 25000, wantTruncated: false, wantMinPages: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &TruncatingServer{Tip: tt.tip, TxsPerBlock: 1}
			srv := httptest.NewServer(fake)
			defer srv.Close()

			txs, truncated, err := NewClient(srv.URL).CoinFlows(
				context.Background(), []string{"g1abc", ""})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if truncated != tt.wantTruncated {
				t.Errorf("truncated = %v, want %v", truncated, tt.wantTruncated)
			}
			if len(txs) != tt.wantTxs {
				t.Fatalf("got %d transactions, want %d", len(txs), tt.wantTxs)
			}

			// Newest first, and every height exactly once: a cursor that
			// resumed one height too far would lose a block, and one that
			// resumed one too short would repeat it. Neither shows up in a
			// count alone.
			seen := map[int]bool{}
			for i, tx := range txs {
				if seen[tx.BlockHeight] {
					t.Fatalf("height %d returned twice", tx.BlockHeight)
				}
				seen[tx.BlockHeight] = true
				if i > 0 && txs[i-1].BlockHeight < tx.BlockHeight {
					t.Fatalf("not newest-first at %d: %d after %d",
						i, tx.BlockHeight, txs[i-1].BlockHeight)
				}
			}
			for h := 1; h <= tt.tip; h++ {
				if !seen[h] {
					t.Fatalf("height %d is missing from the walk", h)
				}
			}

			fake.Mu.Lock()
			defer fake.Mu.Unlock()
			if len(fake.TxQueries) < tt.wantMinPages {
				t.Errorf("got %d queries, want at least %d", len(fake.TxQueries), tt.wantMinPages)
			}
			// The filter has to survive being merged with the cursor bound on
			// every page, not just the first: a resumed query that dropped the
			// address clause would return the whole chain and the sum would be
			// nonsense rather than short.
			for i, q := range fake.TxQueries {
				// The queries are recorded as the JSON request body, so the
				// address quotes arrive escaped.
				for _, want := range []string{"TransferEvent", `to: { eq: \"g1abc\" }`, "success: { eq: true }", "ASC"} {
					if !strings.Contains(q, want) {
						t.Errorf("query %d is missing %q: %s", i, want, q)
					}
				}
				if strings.Contains(q, `eq: \"\"`) {
					t.Errorf("query %d filtered on the empty address: %s", i, q)
				}
			}
		})
	}
}

func TestCoinFlowsWithNoAddressesAsksNothing(t *testing.T) {
	fake := &TruncatingServer{Tip: 10, TxsPerBlock: 1}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	txs, truncated, err := NewClient(srv.URL).CoinFlows(context.Background(), []string{"", ""})
	if err != nil || txs != nil || truncated {
		t.Fatalf("got (%v, %v, %v), want (nil, false, nil)", txs, truncated, err)
	}
	fake.Mu.Lock()
	defer fake.Mu.Unlock()
	if len(fake.TxQueries) != 0 {
		t.Errorf("asked %d queries for no addresses, want 0", len(fake.TxQueries))
	}
}

// A selection set that filters on success but does not *select* it hands back
// `success: false` on every row, because the field is simply absent from the
// payload and Go zeroes it. The consumer then drops everything it was given,
// silently and with no error anywhere: the first live backfill run walked 10,000
// real mainnet transactions and stored 0 legs for exactly this reason.
func TestTransferQueriesSelectSuccessAndNotOnlyFilterOnIt(t *testing.T) {
	tests := []struct {
		name string
		call func(*Client) error
	}{
		{"CoinFlows", func(c *Client) error {
			_, _, err := c.CoinFlows(context.Background(), []string{"g1abc"})
			return err
		}},
		{"CoinTransferWindow", func(c *Client) error {
			_, _, err := c.CoinTransferWindow(context.Background(), 0, 100)
			return err
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &TruncatingServer{Tip: 3, TxsPerBlock: 1}
			srv := httptest.NewServer(fake)
			defer srv.Close()

			if err := tt.call(NewClient(srv.URL)); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			fake.Mu.Lock()
			defer fake.Mu.Unlock()
			if len(fake.TxQueries) == 0 {
				t.Fatal("no query was sent")
			}
			q := fake.TxQueries[0]
			// Counted rather than Contains'd, because the *filter* already says
			// `success: { eq: true }` and a Contains check is satisfied by that
			// alone. Two occurrences means the where clause and the selection
			// set; one means the bug.
			if n := strings.Count(q, "success"); n < 2 {
				t.Errorf("query mentions success %d time(s), so it filters on it without selecting it: %s", n, q)
			}
		})
	}
}
