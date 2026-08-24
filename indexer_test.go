package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingIndexer serves block queries and records how many requests it took,
// which is the whole point of the density heuristic under test.
type countingIndexer struct {
	mu       sync.Mutex
	requests int
	blocks   int // total block records served
}

func (f *countingIndexer) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests, f.blocks
}

func (f *countingIndexer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	query := string(body)

	f.mu.Lock()
	f.requests++
	f.mu.Unlock()

	var lo, hi int
	switch {
	case strings.Contains(query, "eq:"):
		fmt.Sscanf(query[strings.Index(query, "eq:"):], "eq: %d", &hi)
		if hi == 0 {
			fmt.Sscanf(query[strings.Index(query, "eq:"):], "eq:%d", &hi)
		}
		lo = hi
	default:
		// Range form: gt: X, lt: Y — serve everything strictly between.
		gt := strings.Index(query, "gt:")
		lt := strings.Index(query, "lt:")
		if gt < 0 || lt < 0 {
			http.Error(w, "unexpected query", 400)
			return
		}
		fmt.Sscanf(strings.TrimSpace(query[gt+3:]), "%d", &lo)
		fmt.Sscanf(strings.TrimSpace(query[lt+3:]), "%d", &hi)
		lo, hi = lo+1, hi-1
	}

	type blk struct {
		Hash    string `json:"hash"`
		Height  int    `json:"height"`
		ChainID string `json:"chain_id"`
		Time    string `json:"time"`
	}
	var blocks []blk
	for h := lo; h <= hi; h++ {
		blocks = append(blocks, blk{
			Hash:    fmt.Sprintf("hash-%d", h),
			Height:  h,
			ChainID: "testchain",
			Time:    fmt.Sprintf("2026-01-01T00:%02d:00Z", h%60),
		})
	}

	f.mu.Lock()
	f.blocks += len(blocks)
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"data": map[string]any{"getBlocks": blocks},
	})
}

func newCountingIndexer(t *testing.T) (*IndexerClient, *countingIndexer) {
	t.Helper()
	fake := &countingIndexer{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return NewIndexerClient(srv.URL), fake
}

func TestGetBlockTimesForHeights(t *testing.T) {
	ctx := context.Background()

	t.Run("dense heights use a single range query", func(t *testing.T) {
		client, fake := newCountingIndexer(t)

		heights := []int{100, 101, 102, 103, 104}
		times, err := client.GetBlockTimesForHeights(ctx, heights)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, h := range heights {
			if times[h] == "" {
				t.Errorf("height %d has no time", h)
			}
		}
		if reqs, _ := fake.counts(); reqs != 1 {
			t.Errorf("made %d requests for 5 consecutive heights, want 1", reqs)
		}
	})

	t.Run("sparse heights are fetched individually", func(t *testing.T) {
		client, fake := newCountingIndexer(t)

		// 5 transactions scattered over 20k blocks. As a range this would pull
		// every block in between — the regression this heuristic exists to stop.
		heights := []int{450000, 455000, 460000, 465000, 470000}
		times, err := client.GetBlockTimesForHeights(ctx, heights)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, h := range heights {
			if times[h] == "" {
				t.Errorf("height %d has no time", h)
			}
		}
		reqs, blocks := fake.counts()
		if reqs != len(heights) {
			t.Errorf("made %d requests, want one per height (%d)", reqs, len(heights))
		}
		// The point of the heuristic: only the wanted blocks are transferred.
		if blocks != len(heights) {
			t.Errorf("served %d block records for %d heights — the range path was taken", blocks, len(heights))
		}
	})

	t.Run("no heights makes no requests", func(t *testing.T) {
		client, fake := newCountingIndexer(t)
		if _, err := client.GetBlockTimesForHeights(ctx, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if reqs, _ := fake.counts(); reqs != 0 {
			t.Errorf("made %d requests for an empty height list, want 0", reqs)
		}
	})
}

func TestGetRecentTransactionsPageWidensWindow(t *testing.T) {
	// The indexer has no limit argument, so the page fetch bounds by height and
	// widens until it has enough. Verify it widens rather than giving up, and
	// that it stops instead of looping once it reaches genesis.
	var mu sync.Mutex
	queries := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		query := string(body)
		w.Header().Set("Content-Type", "application/json")

		if strings.Contains(query, "latestBlockHeight") {
			fmt.Fprint(w, `{"data":{"latestBlockHeight":1000000}}`)
			return
		}

		mu.Lock()
		queries++
		n := queries
		mu.Unlock()

		// Nothing in the first window; results only once it has widened.
		if n == 1 {
			fmt.Fprint(w, `{"data":{"getTransactions":[]}}`)
			return
		}
		fmt.Fprint(w, `{"data":{"getTransactions":[
			{"hash":"A","block_height":999999},
			{"hash":"B","block_height":999998},
			{"hash":"C","block_height":999997}
		]}}`)
	}))
	defer srv.Close()

	txs, err := NewIndexerClient(srv.URL).GetRecentTransactionsPage(context.Background(), 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(txs) != 3 {
		t.Errorf("got %d transactions, want 3", len(txs))
	}
	mu.Lock()
	defer mu.Unlock()
	if queries < 2 {
		t.Errorf("made %d transaction queries; expected the window to widen at least once", queries)
	}
}

func TestClientBreakerShortCircuitsDeadIndexer(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()

	client := NewIndexerClient(srv.URL)
	ctx := context.Background()

	// Every path goes through query(), so this protects single-network pages,
	// merged views and the sync loop alike.
	for i := 0; i < clientBreakerThreshold+3; i++ {
		if _, err := client.LatestBlockHeight(ctx); err == nil {
			t.Fatalf("call %d unexpectedly succeeded against a failing indexer", i)
		}
	}
	if got := attempts.Load(); got != int32(clientBreakerThreshold) {
		t.Errorf("indexer was contacted %d times, want %d — the breaker should stop retrying",
			got, clientBreakerThreshold)
	}
	if !errors.Is(mustErr(client.LatestBlockHeight(ctx)), errIndexerUnavailable) {
		t.Error("expected errIndexerUnavailable once the breaker is open")
	}

	// A recovered indexer must be picked up without a restart.
	client.mu.Lock()
	client.skipUntil = time.Now().Add(-time.Second)
	client.mu.Unlock()
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"latestBlockHeight":42}}`)
	})
	h, err := client.LatestBlockHeight(ctx)
	if err != nil {
		t.Fatalf("after recovery: %v", err)
	}
	if h != 42 {
		t.Errorf("height = %d, want 42", h)
	}
	if client.breakerOpen() {
		t.Error("breaker should be closed after a success")
	}
}

func mustErr(_ int, err error) error { return err }

// truncatingTxIndexer imitates the tx-indexer resolver: it iterates in the
// requested order, stops at the element cap, and returns the rows it already has
// *alongside* the error rather than refusing the query. txsPerBlock transactions
// per block, heights 1..tip.
type truncatingTxIndexer struct {
	tip         int
	txsPerBlock int

	mu        sync.Mutex
	txQueries []string
}

func (f *truncatingTxIndexer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	query := string(body)
	w.Header().Set("Content-Type", "application/json")

	if strings.Contains(query, "latestBlockHeight") {
		fmt.Fprintf(w, `{"data":{"latestBlockHeight":%d}}`, f.tip)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.txQueries = append(f.txQueries, query)

	from, to := 0, f.tip
	if gt := strings.Index(query, "gt:"); gt >= 0 {
		fmt.Sscanf(strings.TrimSpace(query[gt+3:]), "%d", &from)
	}
	if lt := strings.Index(query, "lt:"); lt >= 0 {
		var v int
		fmt.Sscanf(strings.TrimSpace(query[lt+3:]), "%d", &v)
		to = min(v-1, f.tip)
	}

	var rows []string
	emit := func(h int) {
		for i := 0; i < f.txsPerBlock; i++ {
			rows = append(rows, fmt.Sprintf(`{"hash":"tx-%d-%d","block_height":%d}`, h, i, h))
		}
	}
	if strings.Contains(query, "DESC") {
		for h := to; h > from && len(rows) < indexerElementCap; h-- {
			emit(h)
		}
	} else {
		for h := from + 1; h <= to && len(rows) < indexerElementCap; h++ {
			emit(h)
		}
	}

	// The resolver checks its counter before appending the next row, so a result
	// set of exactly the cap is reported as truncated too.
	truncated := len(rows) >= indexerElementCap
	if truncated {
		rows = rows[:indexerElementCap]
	}

	data := fmt.Sprintf(`"data":{"getTransactions":[%s]}`, strings.Join(rows, ","))
	if truncated {
		fmt.Fprintf(w, `{%s,"errors":[{"message":"max elements per query reached (%d)"}]}`,
			data, indexerElementCap)
		return
	}
	fmt.Fprintf(w, `{%s}`, data)
}

// blockOf groups a page by block height, so tests can assert no block came back
// half-populated.
func blockOf(txs []Transaction) map[int]int {
	per := make(map[int]int)
	for _, tx := range txs {
		per[tx.BlockHeight]++
	}
	return per
}

func TestTransactionsFromHeightPageIsWholeBlocksOnly(t *testing.T) {
	// 3 transactions per block does not divide the cap, so truncation lands inside
	// a block. Handing that block back half-populated would lose its remaining
	// rows for good: the caller resumes at the last row's height with an exclusive
	// `gt`, and the sync cursor never looks back.
	fake := &truncatingTxIndexer{tip: 20000, txsPerBlock: 3}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	txs, truncated, err := NewIndexerClient(srv.URL).GetTransactionsFromHeight(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !truncated {
		t.Fatal("page was not reported as truncated")
	}
	if len(txs) == 0 || len(txs) > indexerElementCap {
		t.Fatalf("got %d rows, want between 1 and the cap of %d", len(txs), indexerElementCap)
	}

	for h, n := range blockOf(txs) {
		if n != fake.txsPerBlock {
			t.Errorf("block %d came back with %d of its %d transactions", h, n, fake.txsPerBlock)
		}
	}
	// The cap is 10000 and blocks hold 3, so a whole-blocks page stops at 9999.
	if len(txs)%fake.txsPerBlock != 0 {
		t.Errorf("page of %d rows is not a whole number of blocks", len(txs))
	}
}

func TestTransactionsFromHeightAscendsFromTheCursor(t *testing.T) {
	// ASC is load-bearing: truncation keeps the rows the resolver saw first, so
	// only ascending order yields the contiguous page above the cursor. DESC would
	// return the newest rows and orphan everything between them and the cursor.
	fake := &truncatingTxIndexer{tip: 200, txsPerBlock: 1}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	cursor := 50
	txs, truncated, err := NewIndexerClient(srv.URL).GetTransactionsFromHeight(context.Background(), &cursor)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if truncated {
		t.Fatal("200 blocks should fit under the cap")
	}
	if len(txs) != fake.tip-cursor {
		t.Fatalf("got %d transactions, want %d", len(txs), fake.tip-cursor)
	}
	if txs[0].BlockHeight != cursor+1 {
		t.Errorf("page starts at height %d, want %d — not anchored to the cursor", txs[0].BlockHeight, cursor+1)
	}
	for i := 1; i < len(txs); i++ {
		if txs[i].BlockHeight < txs[i-1].BlockHeight {
			t.Fatalf("height went backwards at %d: %d after %d", i, txs[i].BlockHeight, txs[i-1].BlockHeight)
		}
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if q := fake.txQueries[0]; !strings.Contains(q, "ASC") {
		t.Errorf("query is not ascending: %s", q)
	}
}

func TestTransactionsFromHeightAtTip(t *testing.T) {
	// Caught up: one query, nothing above the cursor, nothing more to ask for.
	fake := &truncatingTxIndexer{tip: 500, txsPerBlock: 1}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	tip := fake.tip
	txs, truncated, err := NewIndexerClient(srv.URL).GetTransactionsFromHeight(context.Background(), &tip)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(txs) != 0 || truncated {
		t.Errorf("got %d transactions (truncated=%v) at the tip, want 0 and false", len(txs), truncated)
	}
}

func TestTransactionsFromHeightRejectsAnOversizedBlock(t *testing.T) {
	// Every row shares one height, so trimming the partial trailing block leaves
	// nothing and there is no cursor to advance to. That has to surface as an
	// error, not as a silently empty page that ends the walk.
	fake := &truncatingTxIndexer{tip: 1, txsPerBlock: indexerElementCap + 10}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	_, _, err := NewIndexerClient(srv.URL).GetTransactionsFromHeight(context.Background(), nil)
	if !errors.Is(err, errQueryTooLarge) {
		t.Fatalf("got %v, want errQueryTooLarge", err)
	}
}

func TestSyncFetchersKeepTheirFilterAndOrder(t *testing.T) {
	// The message filter has to survive being merged with the cursor's
	// block_height bound, and the order has to stay ascending.
	tests := []struct {
		name       string
		wantFilter string
		fetch      func(*IndexerClient, context.Context) ([]Transaction, bool, error)
	}{
		{"GetAllPackages", "MsgAddPackage", func(c *IndexerClient, ctx context.Context) ([]Transaction, bool, error) {
			h := 10
			return c.GetAllPackages(ctx, &h)
		}},
		{"GetMsgRunTransactions", "MsgRun", func(c *IndexerClient, ctx context.Context) ([]Transaction, bool, error) {
			h := 10
			return c.GetMsgRunTransactions(ctx, &h)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &truncatingTxIndexer{tip: 100, txsPerBlock: 1}
			srv := httptest.NewServer(fake)
			defer srv.Close()

			if _, _, err := tt.fetch(NewIndexerClient(srv.URL), context.Background()); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			fake.mu.Lock()
			defer fake.mu.Unlock()
			q := fake.txQueries[0]
			for _, want := range []string{tt.wantFilter, "block_height", "gt: 10", "ASC"} {
				if !strings.Contains(q, want) {
					t.Errorf("query is missing %q: %s", want, q)
				}
			}
		})
	}
}

// sparseTxIndexer models a chain whose transactions are all old: nothing has
// happened for the last `tip - lastTxHeight` blocks. gno.land looks like this —
// it returns no rows at all for its most recent 20,000 blocks — so the widening
// loop has to walk back before it finds anything.
type sparseTxIndexer struct {
	tip          int
	lastTxHeight int

	mu      sync.Mutex
	windows []int // `from` bound of each transaction query, in order
}

func (f *sparseTxIndexer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	query := string(body)
	w.Header().Set("Content-Type", "application/json")

	if strings.Contains(query, "latestBlockHeight") {
		fmt.Fprintf(w, `{"data":{"latestBlockHeight":%d}}`, f.tip)
		return
	}

	from := 0
	if gt := strings.Index(query, "gt:"); gt >= 0 {
		fmt.Sscanf(strings.TrimSpace(query[gt+3:]), "%d", &from)
	}

	f.mu.Lock()
	f.windows = append(f.windows, from)
	f.mu.Unlock()

	var rows []string
	for h := f.lastTxHeight; h > from; h-- {
		rows = append(rows, fmt.Sprintf(`{"hash":"tx-%d","block_height":%d}`, h, h))
	}
	fmt.Fprintf(w, `{"data":{"getTransactions":[%s]}}`, strings.Join(rows, ","))
}

func TestRecentPageWidensUntilItFindsRows(t *testing.T) {
	// The initial window is deliberately small, so a chain with no recent
	// activity must widen several times rather than give up and report nothing.
	fake := &sparseTxIndexer{tip: 100000, lastTxHeight: 500}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	txs, err := NewIndexerClient(srv.URL).GetRecentTransactionsPage(context.Background(), 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(txs) < 20 {
		t.Fatalf("got %d transactions, want at least the 20 asked for", len(txs))
	}
	if txs[0].BlockHeight != fake.lastTxHeight {
		t.Errorf("newest row is height %d, want %d", txs[0].BlockHeight, fake.lastTxHeight)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.windows) < 2 {
		t.Fatalf("only %d query issued, so the window never widened", len(fake.windows))
	}
	// Each widening must reach strictly further back than the last.
	for i := 1; i < len(fake.windows); i++ {
		if fake.windows[i] >= fake.windows[i-1] {
			t.Errorf("window %d starts at %d, no further back than %d", i, fake.windows[i], fake.windows[i-1])
		}
	}
}

func TestRecentPageServesACappedWindow(t *testing.T) {
	// A dense chain overflows the element cap inside the very first window. The
	// query is DESC, so the rows the resolver kept are the newest ones — exactly
	// what a "recent" view wants. Returning them beats failing the request, which
	// is what used to happen and what took /api/txs down on sapphire.
	fake := &truncatingTxIndexer{tip: 20000, txsPerBlock: 200}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	txs, err := NewIndexerClient(srv.URL).GetRecentTransactionsPage(context.Background(), 20)
	if err != nil {
		t.Fatalf("capped window should serve rows, not fail: %v", err)
	}
	if len(txs) != indexerElementCap {
		t.Fatalf("got %d rows, want the full capped page of %d", len(txs), indexerElementCap)
	}
	if txs[0].BlockHeight != fake.tip {
		t.Errorf("newest row is height %d, want the tip at %d", txs[0].BlockHeight, fake.tip)
	}
}

func TestRecentPageStillFailsWhenCappedPageIsTooSmall(t *testing.T) {
	// The cap only answers the question when it holds at least what was asked
	// for. Beyond that the page is genuinely short and must not pass as complete.
	fake := &truncatingTxIndexer{tip: 20000, txsPerBlock: 200}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	_, err := NewIndexerClient(srv.URL).GetRecentTransactionsPage(context.Background(), indexerElementCap+1)
	if !errors.Is(err, errQueryTooLarge) {
		t.Fatalf("got %v, want errQueryTooLarge", err)
	}
}

func TestClientTimeoutsAreSeparate(t *testing.T) {
	// A page has a browser waiting on it; a sync is catching up on history and is
	// measured against the size of the chain. Sharing one budget is what made a
	// cold sync of sapphire impossible.
	serve := NewIndexerClient("http://example.invalid/graphql")
	sync := NewSyncIndexerClient("http://example.invalid/graphql")

	if serve.client.Timeout != serveClientTimeout {
		t.Errorf("serve timeout = %v, want %v", serve.client.Timeout, serveClientTimeout)
	}
	if sync.client.Timeout != syncClientTimeout {
		t.Errorf("sync timeout = %v, want %v", sync.client.Timeout, syncClientTimeout)
	}
	if sync.client.Timeout <= serve.client.Timeout {
		t.Errorf("sync budget %v does not exceed the serve budget %v", sync.client.Timeout, serve.client.Timeout)
	}
	// URL normalisation must survive the split.
	if serve.url != sync.url || serve.url != "http://example.invalid/graphql/query" {
		t.Errorf("urls diverged: serve=%q sync=%q", serve.url, sync.url)
	}
}

func TestRefusedRequestNamesTheStatus(t *testing.T) {
	// A rate-limited indexer answers with an HTML error page. Decoding it as
	// GraphQL reports "invalid character '<'", which points at the query instead
	// of at the status code — this cost real debugging time against sapphire.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "<html>\n<head><title>403 Forbidden</title></head>\n</html>")
	}))
	defer srv.Close()

	_, err := NewIndexerClient(srv.URL).LatestBlockHeight(context.Background())
	if err == nil {
		t.Fatal("a 403 should be an error")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error does not name the status: %v", err)
	}
	if strings.Contains(err.Error(), "invalid character") {
		t.Errorf("error still reads as a decode failure: %v", err)
	}
}
