package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/moul/mygnoscan/pkg/config"
	"github.com/moul/mygnoscan/pkg/indexer"
)

// SSE live feed via polling. The Go backend polls the tx-indexer for new
// blocks/txs and fans out to browser clients via Server-Sent Events.

type liveFeed struct {
	mu        sync.RWMutex
	clients   map[chan []byte]struct{}
	running   bool
	indexer   liveSource
	networkID string
	lastBlock int
}

var liveFeeds = map[string]*liveFeed{}

func InitLiveFeeds(networks []config.NetworkConfig, clients map[string]*indexer.Client) {
	for _, n := range networks {
		// A network without a client gets no feed rather than a feed that
		// panics: pollLoop dereferences f.indexer, so a nil one would take the
		// process down from a goroutine the moment a browser subscribed.
		c := clients[n.ID]
		if c == nil {
			log.Printf("[%s] live feed: no indexer client, feed disabled", n.ID)
			continue
		}
		liveFeeds[n.ID] = &liveFeed{
			clients:   make(map[chan []byte]struct{}),
			indexer:   c,
			networkID: n.ID,
		}
	}
}

func (f *liveFeed) addClientChan(ch chan []byte) {
	f.mu.Lock()
	f.clients[ch] = struct{}{}
	f.mu.Unlock()
	f.ensureRunning()
}

func (f *liveFeed) removeClient(ch chan []byte) {
	f.mu.Lock()
	delete(f.clients, ch)
	f.mu.Unlock()
}

// broadcast sends to every subscriber, skipping any whose buffer is full.
//
// The default case is load-bearing: without it one browser that has stopped
// reading — a backgrounded tab, a stalled connection — would block the poll loop
// and stall the feed for everyone else. A slow client loses events instead.
func (f *liveFeed) broadcast(data []byte) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	for ch := range f.clients {
		select {
		case ch <- data:
		default:
		}
	}
}

func (f *liveFeed) ensureRunning() {
	f.mu.Lock()
	if f.running {
		f.mu.Unlock()
		return
	}
	f.running = true
	f.mu.Unlock()
	go f.pollLoop()
}

// stopIfIdle clears the running flag and reports true when the last client has
// gone, so pollLoop can exit.
//
// The check and the clear have to happen under one lock. Reading the client
// count, releasing, then taking the lock again to clear the flag leaves a window
// where a client arrives in between: addClientChan registers it and calls
// ensureRunning, which sees running still true and starts nothing, and this loop
// then clears the flag and exits. The client stays subscribed to a feed with
// nobody polling it and silently receives no events until some other connection
// happens to restart the loop.
func (f *liveFeed) stopIfIdle() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.clients) > 0 {
		return false
	}
	f.running = false
	return true
}

// maxLiveCatchup bounds how far back one poll will replay.
//
// pollLoop exits when the last client leaves but keeps lastBlock, so the next
// browser to open live mode restarts it against the height from whenever the
// previous one disconnected. Unclamped, that first tick pulls every block since
// then in a single GraphQL query and fans out one SSE frame each: measured at
// 37 blocks after two minutes idle on mainnet, which is ~1100 after an hour and
// ~26k after a day. Someone turning live mode on wants what happens next, not
// the backlog.
const maxLiveCatchup = 20

// liveCallTimeout caps each indexer call a poll makes, individually: three
// sequential calls sharing one budget would let a slow first call starve the
// other two.
const liveCallTimeout = 10 * time.Second

// liveSource is the slice of indexer.Client a poll needs. Narrowing it is what
// makes pollStep testable without a live indexer behind it.
type liveSource interface {
	LatestBlockHeight(ctx context.Context) (int, error)
	GetRecentBlocks(ctx context.Context, limit int) ([]indexer.Block, error)
	GetRecentTransactions(ctx context.Context, maxResults int) ([]indexer.Transaction, error)
}

func liveEvent(kind, networkID, field string, payload any) []byte {
	data, _ := json.Marshal(map[string]any{
		"type":       kind,
		"network_id": networkID,
		"payload":    map[string]any{"data": map[string]any{field: payload}},
	})
	return data
}

func (f *liveFeed) pollLoop() {
	log.Printf("[%s] live feed: started polling", f.networkID)
	for {
		if f.stopIfIdle() {
			log.Printf("[%s] live feed: no clients, stopped", f.networkID)
			return
		}
		f.pollStep(context.Background())
		time.Sleep(3 * time.Second)
	}
}

// pollStep broadcasts everything that appeared since the last one.
//
// Two invariants the subscribers depend on, both of which this used to break:
//
//   - Events go out oldest first. Browsers prepend each event to the top of the
//     list, and the indexer queries this reads order height DESC, so forwarding
//     the batch as it arrives rendered every tick upside down: a tick carrying
//     four blocks put the newest of the four at the bottom of its group.
//   - Nothing is sent twice. GetRecentBlocks re-reads the latest height itself,
//     so the batch routinely contains a block above the height read a moment
//     earlier. Recording the earlier height as the new tip left those extras
//     unrecorded and the next tick sent them again.
func (f *liveFeed) pollStep(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, liveCallTimeout)
	height, err := f.indexer.LatestBlockHeight(ctx)
	cancel()
	if err != nil {
		return
	}

	switch {
	case f.lastBlock == 0 || height < f.lastBlock:
		// First poll, or the chain reset under us. Start from the tip: the
		// blocks already there are the ones the page painted on load. Before
		// the reset arm existed a rewound chain wedged the feed for good, since
		// height could never again exceed a lastBlock from the old chain.
		f.lastBlock = height
	case height-f.lastBlock > maxLiveCatchup:
		f.lastBlock = height - maxLiveCatchup
	}

	if height <= f.lastBlock {
		return
	}

	// from is the tip as it stood before this poll. It stays fixed while
	// lastBlock advances, so both loops below filter against the same edge.
	from := f.lastBlock

	ctx2, cancel2 := context.WithTimeout(parent, liveCallTimeout)
	blocks, err := f.indexer.GetRecentBlocks(ctx2, height-from+1)
	cancel2()
	if err != nil {
		// lastBlock stays where it was, so the next tick retries this span
		// instead of skipping it. maxLiveCatchup keeps the retry bounded.
		return
	}
	for i := len(blocks) - 1; i >= 0; i-- { // DESC query, walked oldest first
		b := blocks[i]
		if b.Height <= from {
			continue
		}
		f.broadcast(liveEvent("block", f.networkID, "getBlocks", b))
		if b.Height > f.lastBlock {
			f.lastBlock = b.Height
		}
	}

	ctx3, cancel3 := context.WithTimeout(parent, liveCallTimeout)
	txs, err := f.indexer.GetRecentTransactions(ctx3, 20)
	cancel3()
	if err != nil {
		return
	}
	for i := len(txs) - 1; i >= 0; i-- { // heightAndIndex DESC, same reversal
		// Bounded above by the blocks this tick actually announced. This is a
		// third and even fresher read, so without the ceiling a tx from a block
		// the feed has not reached yet goes out now and goes out again when its
		// block finally arrives.
		if h := txs[i].BlockHeight; h > from && h <= f.lastBlock {
			f.broadcast(liveEvent("tx", f.networkID, "getTransactions", txs[i]))
		}
	}
}

func LiveFeedHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", 500)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		network := r.URL.Query().Get("network")

		ch := make(chan []byte, 64)

		// Register in appropriate feeds
		if network == "" || network == "all" {
			for _, f := range liveFeeds {
				f.addClientChan(ch)
			}
			defer func() {
				for _, f := range liveFeeds {
					f.removeClient(ch)
				}
			}()
		} else if f, ok := liveFeeds[network]; ok {
			f.addClientChan(ch)
			defer f.removeClient(ch)
		}

		fmt.Fprintf(w, ": connected\n\n")
		flusher.Flush()

		ctx := r.Context()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case data := <-ch:
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			case <-ticker.C:
				fmt.Fprintf(w, ": keepalive\n\n")
				flusher.Flush()
			}
		}
	}
}
