package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
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
