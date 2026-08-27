package main

import (
	"sync"
	"testing"
	"time"
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

	initLiveFeeds(
		[]NetworkConfig{{ID: "withclient"}, {ID: "noclient"}},
		map[string]*IndexerClient{"withclient": {}},
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
	fake, client := newFakeIndexer(t)
	fake.seedChain(1, 3)

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
