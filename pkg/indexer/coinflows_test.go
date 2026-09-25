package indexer

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

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
