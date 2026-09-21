package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/moul/mygnoscan/pkg/config"
	"github.com/moul/mygnoscan/pkg/indexer"
)

// newFeed builds a feed without starting its poll loop, so the fan-out can be
// tested without an indexer. addClientChan deliberately starts polling, which is
// why these register directly.
func newFeed() *liveFeed {
	return &liveFeed{clients: make(map[chan []byte]struct{}), networkID: "test"}
}

func (f *liveFeed) register(ch chan []byte) {
	f.mu.Lock()
	f.clients[ch] = struct{}{}
	f.mu.Unlock()
}

func TestLiveFeedBroadcast(t *testing.T) {
	t.Parallel()

	t.Run("every subscriber receives the event", func(t *testing.T) {
		t.Parallel()
		f := newFeed()
		a, b := make(chan []byte, 1), make(chan []byte, 1)
		f.register(a)
		f.register(b)

		f.broadcast([]byte("block 1"))

		for name, ch := range map[string]chan []byte{"a": a, "b": b} {
			select {
			case got := <-ch:
				if string(got) != "block 1" {
					t.Errorf("%s got %q", name, got)
				}
			default:
				t.Errorf("%s received nothing", name)
			}
		}
	})

	t.Run("a client that stopped reading does not stall the others", func(t *testing.T) {
		t.Parallel()
		f := newFeed()
		// Unbuffered and never read: a backgrounded tab or a stalled connection.
		stalled := make(chan []byte)
		healthy := make(chan []byte, 1)
		f.register(stalled)
		f.register(healthy)

		done := make(chan struct{})
		go func() {
			f.broadcast([]byte("event"))
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("broadcast blocked on a client that is not reading — one stalled browser would freeze the feed for everyone")
		}

		select {
		case got := <-healthy:
			if string(got) != "event" {
				t.Errorf("healthy client got %q", got)
			}
		default:
			t.Error("the healthy client was skipped because of the stalled one")
		}
	})

	t.Run("events are dropped, not queued, once a buffer is full", func(t *testing.T) {
		t.Parallel()
		f := newFeed()
		ch := make(chan []byte, 1)
		f.register(ch)

		f.broadcast([]byte("first"))
		f.broadcast([]byte("second")) // no room; must be dropped

		if got := <-ch; string(got) != "first" {
			t.Errorf("got %q, want the first event", got)
		}
		select {
		case got := <-ch:
			t.Errorf("got a second event %q; a full buffer should drop rather than grow", got)
		default:
		}
	})

	t.Run("a removed client stops receiving", func(t *testing.T) {
		t.Parallel()
		f := newFeed()
		ch := make(chan []byte, 1)
		f.register(ch)
		f.removeClient(ch)

		f.broadcast([]byte("event"))

		select {
		case got := <-ch:
			t.Errorf("a removed client still received %q", got)
		default:
		}
	})

	t.Run("removing an unknown client is harmless", func(t *testing.T) {
		t.Parallel()
		newFeed().removeClient(make(chan []byte, 1)) // must not panic
	})

	t.Run("broadcasting to nobody is harmless", func(t *testing.T) {
		t.Parallel()
		newFeed().broadcast([]byte("event")) // must not panic or block
	})

	t.Run("concurrent subscribe, unsubscribe and broadcast", func(t *testing.T) {
		t.Parallel()
		// Meaningful under -race: clients is shared between the poll loop and
		// every connecting browser.
		f := newFeed()
		var wg sync.WaitGroup
		for i := 0; i < 40; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ch := make(chan []byte, 4)
				f.register(ch)
				f.broadcast([]byte("event"))
				f.removeClient(ch)
			}()
		}
		wg.Wait()

		f.mu.RLock()
		remaining := len(f.clients)
		f.mu.RUnlock()
		if remaining != 0 {
			t.Errorf("%d clients left registered after every one unsubscribed", remaining)
		}
	})
}

// A network configured without an indexer client must get no feed rather than
// one that panics: pollLoop dereferences f.indexer from a goroutine, so a nil
// would take the process down when a browser subscribed.
func TestInitLiveFeedsSkipsNetworksWithoutAClient(t *testing.T) {
	original := liveFeeds
	t.Cleanup(func() { liveFeeds = original })
	liveFeeds = map[string]*liveFeed{}

	InitLiveFeeds(
		[]config.NetworkConfig{{ID: "withclient"}, {ID: "noclient"}},
		map[string]*indexer.Client{"withclient": {}},
	)

	if _, ok := liveFeeds["withclient"]; !ok {
		t.Error("a network with a client got no feed")
	}
	if f, ok := liveFeeds["noclient"]; ok {
		t.Errorf("a network with no client got a feed with indexer %v — pollLoop would panic on it", f.indexer)
	}
	for id, f := range liveFeeds {
		if f.indexer == nil {
			t.Errorf("feed %q has a nil indexer", id)
		}
	}
}

// A client must never end up subscribed to a feed that nobody is polling.
//
// pollLoop used to read the client count, release the lock, then take it again
// to clear the running flag. A client arriving in that window registered itself,
// called ensureRunning, saw running still true and started nothing — and then
// the loop cleared the flag and exited. The connection stayed open and silently
// delivered no events until some other client happened to restart the loop.
//
// Driving the two functions directly rather than waiting on a real pollLoop is
// deliberate: the loop only reaches its idle check once every three seconds, so
// a test that waits for it would almost never land inside the window and would
// pass against the bug. This hits the same two critical sections back to back.
func TestFeedKeepsPollingWhenAClientArrivesAsTheLastOneLeaves(t *testing.T) {
	fake, client := indexer.NewFake(t)
	fake.SeedChain(1, 3)

	for attempt := 0; attempt < 200; attempt++ {
		f := &liveFeed{clients: map[chan []byte]struct{}{}, indexer: client, networkID: "race"}
		f.running = true // as if a loop were already polling

		var wg sync.WaitGroup
		wg.Add(2)

		// One side is the poll loop deciding whether to exit.
		var stopped bool
		go func() { defer wg.Done(); stopped = f.stopIfIdle() }()

		// The other is a browser connecting at that exact moment.
		ch := make(chan []byte, 4)
		go func() { defer wg.Done(); f.addClientChan(ch) }()

		wg.Wait()

		// Whichever order they landed in, a subscribed client must have a loop.
		// If stopIfIdle won, running is false and ensureRunning must have seen
		// that and started one; if the client won, stopIfIdle must have seen it
		// and declined to stop.
		f.mu.RLock()
		clients, running := len(f.clients), f.running
		f.mu.RUnlock()

		if clients > 0 && !running && stopped {
			t.Fatalf("attempt %d: client subscribed to a feed that stopped polling", attempt)
		}
		f.removeClient(ch)
	}
}

// The invariant stated directly: the flag is cleared only when nobody is left.
func TestStopIfIdle(t *testing.T) {
	f := newFeed()
	f.running = true

	if f.stopIfIdle() != true {
		t.Error("an empty feed did not stop")
	}
	if f.running {
		t.Error("running was left set on a stopped feed")
	}

	f.running = true
	ch := make(chan []byte, 1)
	f.clients[ch] = struct{}{}

	if f.stopIfIdle() != false {
		t.Error("a feed with a subscriber stopped anyway")
	}
	if !f.running {
		t.Error("running was cleared while a client was still subscribed")
	}
}

// stubSource is a liveSource whose two reads of the chain tip can disagree, the
// way a real indexer's do: pollStep reads the height, then GetRecentBlocks
// reads it again a round trip later and can see a newer one.
type stubSource struct {
	height    int
	heightErr error
	// ahead is how far past `height` GetRecentBlocks' own tip read lands.
	ahead     int
	blocksErr error
	txHeights []int
	txErr     error
	blockReqs []int
}

func (s *stubSource) LatestBlockHeight(context.Context) (int, error) {
	return s.height, s.heightErr
}

func (s *stubSource) GetRecentBlocks(_ context.Context, limit int) ([]indexer.Block, error) {
	s.blockReqs = append(s.blockReqs, limit)
	if s.blocksErr != nil {
		return nil, s.blocksErr
	}
	// Mirrors the real query: fromHeight = latest-limit, `height > fromHeight`,
	// order DESC.
	latest := s.height + s.ahead
	from := max(latest-limit, 0)
	var out []indexer.Block
	for h := latest; h > from; h-- {
		out = append(out, indexer.Block{Height: h})
	}
	return out, nil
}

func (s *stubSource) GetRecentTransactions(context.Context, int) ([]indexer.Transaction, error) {
	if s.txErr != nil {
		return nil, s.txErr
	}
	out := make([]indexer.Transaction, 0, len(s.txHeights))
	for _, h := range s.txHeights { // the query orders heightAndIndex DESC
		out = append(out, indexer.Transaction{BlockHeight: h, Hash: fmt.Sprintf("tx%d", h)})
	}
	return out, nil
}

// drainHeights reads everything buffered and returns the block heights in the
// order a browser would receive them.
func drainHeights(t *testing.T, ch chan []byte) []int {
	t.Helper()
	var out []int
	for {
		select {
		case data := <-ch:
			var ev struct {
				Type    string `json:"type"`
				Payload struct {
					Data struct {
						Blocks indexer.Block `json:"getBlocks"`
					} `json:"data"`
				} `json:"payload"`
			}
			if err := json.Unmarshal(data, &ev); err != nil {
				t.Fatalf("undecodable event %q: %v", data, err)
			}
			if ev.Type == "block" {
				out = append(out, ev.Payload.Data.Blocks.Height)
			}
		default:
			return out
		}
	}
}

func feedWith(src liveSource) (*liveFeed, chan []byte) {
	f := &liveFeed{clients: map[chan []byte]struct{}{}, indexer: src, networkID: "test"}
	ch := make(chan []byte, 256)
	f.clients[ch] = struct{}{}
	return f, ch
}

func TestPollStep(t *testing.T) {
	t.Parallel()

	t.Run("a batch goes out oldest first", func(t *testing.T) {
		t.Parallel()
		// The indexer answers height DESC and browsers prepend each event to
		// the top of the list, so forwarding the batch in that order rendered
		// every tick upside down: 215270..215273 came out as 215273 at the
		// bottom of the group. This is the bug from the /blocks screenshot.
		s := &stubSource{height: 104}
		f, ch := feedWith(s)
		f.lastBlock = 100

		f.pollStep(context.Background())

		got := drainHeights(t, ch)
		want := []int{101, 102, 103, 104}
		if !slices.Equal(got, want) {
			t.Errorf("got %v, want %v; a prepending client renders this reversed", got, want)
		}
	})

	t.Run("a block above the observed height is not sent twice", func(t *testing.T) {
		t.Parallel()
		// GetRecentBlocks re-reads the tip itself, so its batch can run past
		// the height pollStep saw. Recording that stale height as the new tip
		// left the extras unrecorded and the next tick resent them; caught live
		// on pearl, where 595929 arrived in two consecutive ticks.
		s := &stubSource{height: 102, ahead: 1}
		f, ch := feedWith(s)
		f.lastBlock = 100

		f.pollStep(context.Background())
		if got, want := drainHeights(t, ch), []int{101, 102, 103}; !slices.Equal(got, want) {
			t.Fatalf("first tick got %v, want %v", got, want)
		}

		s.height, s.ahead = 104, 0
		f.pollStep(context.Background())
		if got, want := drainHeights(t, ch), []int{104}; !slices.Equal(got, want) {
			t.Errorf("second tick got %v, want %v; 103 went out twice", got, want)
		}
	})

	t.Run("the first poll starts at the tip", func(t *testing.T) {
		t.Parallel()
		// Whatever is at the tip is already on the page from the initial load.
		s := &stubSource{height: 500}
		f, ch := feedWith(s)

		f.pollStep(context.Background())

		if got := drainHeights(t, ch); len(got) != 0 {
			t.Errorf("first poll replayed %v", got)
		}
		if f.lastBlock != 500 {
			t.Errorf("lastBlock = %d, want 500", f.lastBlock)
		}
	})

	t.Run("a long idle gap is clamped", func(t *testing.T) {
		t.Parallel()
		// pollLoop exits when the last client leaves but keeps lastBlock, so
		// the next browser restarts it against a stale tip. Unclamped that is
		// one query for every block since, ~26k after a day idle.
		s := &stubSource{height: 30000}
		f, ch := feedWith(s)
		f.lastBlock = 1

		f.pollStep(context.Background())

		got := drainHeights(t, ch)
		if len(got) != maxLiveCatchup {
			t.Errorf("replayed %d blocks, want the %d-block cap", len(got), maxLiveCatchup)
		}
		if len(got) > 0 && got[len(got)-1] != 30000 {
			t.Errorf("last event %d, want the tip 30000", got[len(got)-1])
		}
		for _, limit := range s.blockReqs {
			if limit > maxLiveCatchup+1 {
				t.Errorf("asked the indexer for %d blocks, past the cap", limit)
			}
		}
	})

	t.Run("a chain reset re-arms the feed instead of wedging it", func(t *testing.T) {
		t.Parallel()
		// height > lastBlock can never hold again once the chain rewinds, so
		// the feed used to go silent until the process restarted. Same class of
		// bug as the edge rollups in #223.
		s := &stubSource{height: 5}
		f, ch := feedWith(s)
		f.lastBlock = 9000

		f.pollStep(context.Background())
		if f.lastBlock != 5 {
			t.Fatalf("lastBlock = %d after a reset to 5", f.lastBlock)
		}

		s.height = 7
		f.pollStep(context.Background())
		if got, want := drainHeights(t, ch), []int{6, 7}; !slices.Equal(got, want) {
			t.Errorf("got %v, want %v; the feed stayed wedged on the old chain", got, want)
		}
	})

	t.Run("a failed block fetch retries the span rather than skipping it", func(t *testing.T) {
		t.Parallel()
		s := &stubSource{height: 103, blocksErr: errors.New("indexer down")}
		f, ch := feedWith(s)
		f.lastBlock = 100

		f.pollStep(context.Background())
		if f.lastBlock != 100 {
			t.Fatalf("lastBlock advanced to %d through a failed fetch, losing those blocks", f.lastBlock)
		}

		s.blocksErr = nil
		f.pollStep(context.Background())
		if got, want := drainHeights(t, ch), []int{101, 102, 103}; !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("txs are bounded by the blocks the tick announced", func(t *testing.T) {
		t.Parallel()
		// GetRecentTransactions is a third, even fresher read. A tx from a
		// block the feed has not reached yet must wait for that block, or it
		// goes out now and again when the block arrives.
		s := &stubSource{height: 102, txHeights: []int{104, 102, 101, 100}}
		f, ch := feedWith(s)
		f.lastBlock = 100

		f.pollStep(context.Background())

		var hashes []string
		for len(ch) > 0 {
			var ev struct {
				Type    string `json:"type"`
				Payload struct {
					Data struct {
						Tx indexer.Transaction `json:"getTransactions"`
					} `json:"data"`
				} `json:"payload"`
			}
			if err := json.Unmarshal(<-ch, &ev); err != nil {
				t.Fatal(err)
			}
			if ev.Type == "tx" {
				hashes = append(hashes, ev.Payload.Data.Tx.Hash)
			}
		}
		// 100 is not new, 104 is past the announced tip, and the pair that is
		// left comes out oldest first like the blocks do.
		if want := []string{"tx101", "tx102"}; !slices.Equal(hashes, want) {
			t.Errorf("got %v, want %v", hashes, want)
		}
	})
}
