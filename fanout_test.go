package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testNetworks(ids ...string) ([]NetworkConfig, map[string]*IndexerClient) {
	nets := make([]NetworkConfig, 0, len(ids))
	clients := make(map[string]*IndexerClient, len(ids))
	for _, id := range ids {
		nets = append(nets, NetworkConfig{ID: id, IndexerURL: "http://example.invalid/graphql/query"})
		clients[id] = NewIndexerClient("http://example.invalid/graphql/query")
	}
	return nets, clients
}

func TestFanOutRunsConcurrentlyAndKeepsOrder(t *testing.T) {
	nets, clients := testNetworks("a", "b", "c")

	// Each call blocks; if they ran sequentially the total would be 3x this.
	const delay = 150 * time.Millisecond
	start := time.Now()
	got := fanOut(context.Background(), nets, clients, newHealthTracker(),
		func(ctx context.Context, n NetworkConfig, c *IndexerClient) (string, error) {
			time.Sleep(delay)
			return n.ID, nil
		})
	elapsed := time.Since(start)

	if len(got) != 3 {
		t.Fatalf("got %d results, want 3", len(got))
	}
	// Results follow configured order regardless of completion order.
	for i, want := range []string{"a", "b", "c"} {
		if got[i] != want {
			t.Errorf("result %d = %q, want %q", i, got[i], want)
		}
	}
	if elapsed > 2*delay {
		t.Errorf("took %s for 3 concurrent calls of %s — looks sequential", elapsed, delay)
	}
}

func TestFanOutSkipsFailingNetworks(t *testing.T) {
	nets, clients := testNetworks("good", "bad", "alsogood")

	got := fanOut(context.Background(), nets, clients, newHealthTracker(),
		func(ctx context.Context, n NetworkConfig, c *IndexerClient) (string, error) {
			if n.ID == "bad" {
				return "", errors.New("indexer down")
			}
			return n.ID, nil
		})

	// One bad network must not fail the others.
	if len(got) != 2 {
		t.Fatalf("got %v, want the two healthy networks", got)
	}
	if got[0] != "good" || got[1] != "alsogood" {
		t.Errorf("got %v, want [good alsogood]", got)
	}
}

func TestFanOutHonorsPerNetworkDeadline(t *testing.T) {
	nets, clients := testNetworks("slow")

	start := time.Now()
	got := fanOut(context.Background(), nets, clients, newHealthTracker(),
		func(ctx context.Context, n NetworkConfig, c *IndexerClient) (string, error) {
			// Simulate a network that never answers: respect the deadline the
			// fan-out imposes rather than hanging forever.
			<-ctx.Done()
			return "", ctx.Err()
		})
	elapsed := time.Since(start)

	if len(got) != 0 {
		t.Errorf("got %v, want no results from a timed-out network", got)
	}
	if elapsed > perNetworkDeadline+2*time.Second {
		t.Errorf("took %s, expected to be cut off near %s", elapsed, perNetworkDeadline)
	}
}

func TestCircuitBreakerStopsRetryingDeadNetwork(t *testing.T) {
	nets, clients := testNetworks("good", "dead")
	health := newHealthTracker()

	var deadCalls atomic.Int32
	call := func() []string {
		return fanOut(context.Background(), nets, clients, health,
			func(ctx context.Context, n NetworkConfig, c *IndexerClient) (string, error) {
				if n.ID == "dead" {
					deadCalls.Add(1)
					return "", errors.New("connection refused")
				}
				return n.ID, nil
			})
	}

	// Trip the breaker, then keep going.
	for i := 0; i < breakerThreshold+3; i++ {
		got := call()
		if len(got) != 1 || got[0] != "good" {
			t.Fatalf("call %d returned %v, want [good]", i, got)
		}
	}

	// Once open, the dead network is not contacted again — that is the whole
	// point: otherwise every request pays its timeout forever.
	if n := deadCalls.Load(); n != breakerThreshold {
		t.Errorf("dead network was called %d times, want %d (breaker should open after %d)",
			n, breakerThreshold, breakerThreshold)
	}
}

func TestCircuitBreakerRecoversAfterCooldown(t *testing.T) {
	health := newHealthTracker()

	for i := 0; i < breakerThreshold; i++ {
		health.record("net", errors.New("down"))
	}
	if !health.shouldSkip("net") {
		t.Fatal("breaker should be open after repeated failures")
	}

	// A network configured before launch has to start working on its own once
	// it comes up, without a restart.
	health.mu.Lock()
	health.state["net"].skipUntil = time.Now().Add(-time.Second)
	health.mu.Unlock()

	if health.shouldSkip("net") {
		t.Error("breaker should allow a retry once the cooldown has passed")
	}
	health.record("net", nil)
	if health.shouldSkip("net") {
		t.Error("breaker should be closed after a success")
	}
}

func TestHealthTrackerIsConcurrencySafe(t *testing.T) {
	health := newHealthTracker()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				health.record("net", errors.New("down"))
			} else {
				health.record("net", nil)
			}
			health.shouldSkip("net")
		}(i)
	}
	wg.Wait()
}

// The breaker's edges, which the cases above do not reach.
func TestHealthTrackerEdges(t *testing.T) {
	boom := errors.New("indexer unreachable")

	t.Run("one failure is a blip, not an outage", func(t *testing.T) {
		h := newHealthTracker()
		h.record("alpha", boom)
		if h.shouldSkip("alpha") {
			t.Error("breaker opened after a single failure")
		}
	})

	t.Run("networks are tracked independently", func(t *testing.T) {
		h := newHealthTracker()
		for i := 0; i < breakerThreshold; i++ {
			h.record("dead", boom)
		}
		if !h.shouldSkip("dead") {
			t.Error("dead network should be skipped")
		}
		if h.shouldSkip("alive") {
			t.Error("one network's failures silenced another")
		}
	})

	t.Run("a network with no history is attempted", func(t *testing.T) {
		if newHealthTracker().shouldSkip("never-seen") {
			t.Error("an unknown network should not be skipped")
		}
	})

	t.Run("a nil tracker is inert", func(t *testing.T) {
		// api.go guards for this; the guard is only useful if it works.
		var h *healthTracker
		h.record("alpha", boom)
		if h.shouldSkip("alpha") {
			t.Error("a nil tracker should skip nothing")
		}
	})

	t.Run("concurrent use is safe", func(t *testing.T) {
		// Meaningful under -race: shouldSkip and record share a map.
		h := newHealthTracker()
		var wg sync.WaitGroup
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if i%2 == 0 {
					h.record("alpha", boom)
				} else {
					h.shouldSkip("alpha")
				}
			}(i)
		}
		wg.Wait()
	})
}

func TestFanOutSkipsNetworksWithoutAClient(t *testing.T) {
	// A network can be configured without a usable client — clientFor returns
	// nil for anything not in the map. It must be skipped, not panicked on.
	nets := []NetworkConfig{{ID: "configured"}, {ID: "clientless"}}
	clients := map[string]*IndexerClient{"configured": {}}

	got := fanOut(context.Background(), nets, clients, newHealthTracker(),
		func(ctx context.Context, n NetworkConfig, c *IndexerClient) (string, error) {
			return n.ID, nil
		})

	if len(got) != 1 || got[0] != "configured" {
		t.Errorf("got %v, want only the network that has a client", got)
	}
}

func TestFanOutWithNoNetworks(t *testing.T) {
	got := fanOut(context.Background(), nil, nil, newHealthTracker(),
		func(ctx context.Context, n NetworkConfig, c *IndexerClient) (string, error) {
			t.Error("callback ran with no networks configured")
			return "", nil
		})
	if len(got) != 0 {
		t.Errorf("got %v, want nothing", got)
	}
}
