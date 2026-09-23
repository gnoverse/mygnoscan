package syncer

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/moul/mygnoscan/pkg/analyzer"
	"github.com/moul/mygnoscan/pkg/indexer"
	"github.com/moul/mygnoscan/pkg/store"
)

const (
	coinRealm  = "g1realm000000000000000000000000000000"
	coinFunder = "g1funder00000000000000000000000000000"
)

func transferEvent(from, to, coins string) indexer.TxEvent {
	return indexer.TxEvent{Typename: "TransferEvent", From: from, To: to, Coins: coins}
}

// What the recorder keeps and what it drops. Getting either wrong is invisible
// in a row count and shows up much later as a balance that will not reconcile.
func TestRecordCoinTransfers(t *testing.T) {
	tests := []struct {
		name      string
		tx        indexer.Transaction
		want      int
		wantUgnot int64
	}{
		{
			name: "every transfer leg in a successful transaction",
			tx: indexer.Transaction{
				Hash: "tx-ok", BlockHeight: 10, Success: true,
				Response: &indexer.TxResponse{Events: []indexer.TxEvent{
					transferEvent(coinFunder, coinRealm, "5000000ugnot"),
					transferEvent(coinRealm, coinFunder, "1000000ugnot"),
				}},
			},
			want: 2, wantUgnot: 5000000,
		},
		{
			// A reverted transaction still reports its events. Counting them
			// would invent transfers that never settled, which is the same rule
			// the GRC20 ledger and CoinFlows both apply.
			name: "nothing at all from a reverted transaction",
			tx: indexer.Transaction{
				Hash: "tx-fail", BlockHeight: 11, Success: false,
				Response: &indexer.TxResponse{Events: []indexer.TxEvent{
					transferEvent(coinFunder, coinRealm, "5000000ugnot"),
				}},
			},
			want: 0,
		},
		{
			// One end empty is the chain itself: a fee collector, a genesis
			// allocation. That is a fact worth storing, not a missing value.
			name: "a leg with one end unnamed is still a leg",
			tx: indexer.Transaction{
				Hash: "tx-chain", BlockHeight: 12, Success: true,
				Response: &indexer.TxResponse{Events: []indexer.TxEvent{
					transferEvent("", coinRealm, "42ugnot"),
				}},
			},
			want: 1, wantUgnot: 42,
		},
		{
			// Neither end named is not attributable to anyone, and a row keyed
			// on two empty strings pools into an address that does not exist.
			name: "a leg with neither end named is dropped",
			tx: indexer.Transaction{
				Hash: "tx-void", BlockHeight: 13, Success: true,
				Response: &indexer.TxResponse{Events: []indexer.TxEvent{
					transferEvent("", "", "42ugnot"),
				}},
			},
			want: 0,
		},
		{
			name: "other event types are not transfers",
			tx: indexer.Transaction{
				Hash: "tx-other", BlockHeight: 14, Success: true,
				Response: &indexer.TxResponse{Events: []indexer.TxEvent{
					{Typename: "GnoEvent", Type: "Transfer"},
					{Typename: "StorageDepositEvent"},
				}},
			},
			want: 0,
		},
		{
			name: "a transaction with no response is not a crash",
			tx:   indexer.Transaction{Hash: "tx-nil", BlockHeight: 15, Success: true},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _, db := newTestSyncer(t, "alpha")
			got := s.recordCoinTransfers(tt.tx, "2026-09-01T00:00:00Z")
			if got != tt.want {
				t.Fatalf("stored %d legs, want %d", got, tt.want)
			}
			stats, err := db.CoinLedgerStatsFor("alpha", coinRealm)
			if err != nil {
				t.Fatal(err)
			}
			if stats.Received != tt.wantUgnot {
				t.Errorf("realm received %d ugnot, want %d", stats.Received, tt.wantUgnot)
			}
		})
	}
}

// The event index has to be the index over *all* events, not over transfer
// events only: it is half the primary key, and renumbering it whenever the
// event list changes shape would let one leg be stored twice.
func TestRecordCoinTransfersKeysOnTheWholeEventIndex(t *testing.T) {
	s, _, db := newTestSyncer(t, "alpha")
	tx := indexer.Transaction{
		Hash: "tx-mixed", BlockHeight: 20, Success: true,
		Response: &indexer.TxResponse{Events: []indexer.TxEvent{
			{Typename: "StorageDepositEvent"},
			transferEvent(coinFunder, coinRealm, "100ugnot"),
			{Typename: "GnoEvent", Type: "Transfer"},
			transferEvent(coinFunder, coinRealm, "200ugnot"),
		}},
	}
	if got := s.recordCoinTransfers(tx, "2026-09-01T00:00:00Z"); got != 2 {
		t.Fatalf("stored %d legs, want 2", got)
	}

	var idxs []int
	rows, err := db.SQL().Query(
		`SELECT event_idx FROM coin_transfers WHERE network = 'alpha' ORDER BY event_idx`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var i int
		if err := rows.Scan(&i); err != nil {
			t.Fatal(err)
		}
		idxs = append(idxs, i)
	}
	if len(idxs) != 2 || idxs[0] != 1 || idxs[1] != 3 {
		t.Errorf("event indexes = %v, want [1 3], the positions in the whole event list", idxs)
	}
}

// A chain whose indexer has no TransferEvent type must stop being asked, rather
// than logging a validation error every thirty seconds forever. Pearl is that
// chain today (probed 2026-09-23).
func TestBackfillCoinTransfersStopsOnAChainThatCannotAnswer(t *testing.T) {
	s, fake, db := newTestSyncer(t, "alpha")
	fake.NoTransferEvents = true

	s.backfillCoinTransfers(context.Background())

	done, err := db.GetSyncState(store.CoinBackfillDoneKey("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if done != "1" {
		t.Fatalf("done marker = %q, want %q: an unanswerable chain must stop being asked", done, "1")
	}

	// And the marker is honoured: a second pass makes no request at all.
	before := len(fake.AskedQueries())
	s.backfillCoinTransfers(context.Background())
	if after := len(fake.AskedQueries()); after != before {
		t.Errorf("a finished backfill asked %d more queries, want 0", after-before)
	}
}

// windowServer answers just enough for the backfill walk: a tip, the schema
// probe, and a windowed transfer query that reports which window it was asked
// for. It serves no rows; the walk's arithmetic is what is under test, and rows
// would only make the assertions about it harder to read.
type windowServer struct {
	tip int

	mu      sync.Mutex
	windows [][2]int
}

func (w *windowServer) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	q := string(body)
	rw.Header().Set("Content-Type", "application/json")

	switch {
	case strings.Contains(q, "latestBlockHeight"):
		fmt.Fprintf(rw, `{"data":{"latestBlockHeight":%d}}`, w.tip)
	case strings.Contains(q, `__type(name:`):
		fmt.Fprint(rw, `{"data":{"__type":{"name":"TransferEvent"}}}`)
	case strings.Contains(q, "getTransactions"):
		var from, to int
		if i := strings.Index(q, "gt: "); i >= 0 {
			fmt.Sscanf(q[i+4:], "%d", &from)
		}
		if i := strings.Index(q, "lt: "); i >= 0 {
			fmt.Sscanf(q[i+4:], "%d", &to)
		}
		w.mu.Lock()
		w.windows = append(w.windows, [2]int{from, to})
		w.mu.Unlock()
		fmt.Fprint(rw, `{"data":{"getTransactions":[]}}`)
	default:
		fmt.Fprint(rw, `{"data":{}}`)
	}
}

func (w *windowServer) asked() [][2]int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([][2]int(nil), w.windows...)
}

// The walk has to advance by whole windows even across stretches of chain where
// nobody moved a coin, and it has to stop at the tip. Deriving the next cursor
// from the rows instead would stop dead on the first empty window, and on
// mainnet the first 200,000 blocks are mostly empty of transfers.
func TestBackfillCoinTransfersWalksWholeWindowsAndStopsAtTheTip(t *testing.T) {
	srv := httptest.NewServer(&windowServer{tip: 50000})
	defer srv.Close()

	db := store.NewTestDB(t)
	s := NewSyncer(indexer.NewClient(srv.URL), db, analyzer.NewAnalyzer(db), "alpha")

	// One pass: coinBackfillWindowsPerPass windows of coinBackfillWindowBlocks.
	s.backfillCoinTransfers(context.Background())

	fake := srv.Config.Handler.(*windowServer)
	got := fake.asked()
	want := [][2]int{
		{0, 20001},
		{20000, 40001},
		{40000, 50001}, // clipped to the tip rather than overshooting it
	}
	if len(got) != len(want) {
		t.Fatalf("asked for %d windows %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("window %d = %v, want %v", i, got[i], want[i])
		}
	}

	done, err := db.GetSyncState(store.CoinBackfillDoneKey("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if done != "1" {
		t.Errorf("done = %q, want %q: the walk reached the tip", done, "1")
	}

	// And the cursor survives, so a resumed walk does not restart at zero.
	cur, err := db.GetSyncState(store.CoinBackfillCursorKey("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if cur != "50000" {
		t.Errorf("cursor = %q, want %q", cur, "50000")
	}
}
