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
	// recent is the tail of what was broadcast, replayed to each new client.
	recent [][]byte
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
	f.replay(ch)
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
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recent = append(f.recent, data)
	if n := len(f.recent) - liveReplay; n > 0 {
		f.recent = append(f.recent[:0], f.recent[n:]...)
	}
	for ch := range f.clients {
		select {
		case ch <- data:
		default:
		}
	}
}

// replay hands a newly subscribed client the tail of what the feed has already
// sent, so switching live mode on does not start with a hole above the first
// live row. Only that client receives it, and only what fits: a full buffer is
// better trimmed than blocking a subscribe.
func (f *liveFeed) replay(ch chan []byte) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	// The tip first, so the header shows a height before any block arrives.
	// A cold feed has none yet and says nothing rather than claiming zero.
	if f.lastBlock > 0 {
		select {
		case ch <- tipEvent(f.networkID, f.lastBlock):
		default:
			return
		}
	}
	for _, data := range f.recent {
		select {
		case ch <- data:
		default:
			return
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

// livePollInterval is the pause between polls, not the period. The period is
// this plus the round trips, which is why the queries below are the bounded
// ones: a poll built out of GetRecentBlocks and GetRecentTransactions measured
// an 11.5-second period against production, so blocks arrived four at a time
// after eleven seconds of nothing. Range-bounded, a poll is ~0.8s of round
// trips, and a block reaches the browser about two seconds after it is minted.
const livePollInterval = time.Second

// liveReplay is how many recent events a newly subscribed browser is handed.
//
// Live mode is switched on some time after the page loaded, and the blocks
// minted in between belong to neither: the static load is older and the feed
// only sends what comes next, so clicking live skipped a few blocks and left a
// hole above the first live row. Replaying the tail closes it. Duplicates are
// not a concern because every row carries a key and the client drops what it
// already has.
const liveReplay = 16

// liveSource is the slice of indexer.Client a poll needs. Narrowing it is what
// makes pollStep testable without a live indexer behind it.
//
// Both queries are bounded by height and come back ascending, which is the
// order the feed sends in. The "recent" helpers are the wrong tool here twice
// over: GetRecentTransactions is the unbounded `where: {}` query, 1.7s and
// 839KB against mainnet to find the handful of txs in three new blocks, and
// GetRecentBlocks re-reads the chain tip itself, so its window could overshoot
// the height the caller had just read and resend those blocks on the next tick.
type liveSource interface {
	LatestBlockHeight(ctx context.Context) (int, error)
	GetBlocksInRange(ctx context.Context, fromHeight, toHeight int) ([]indexer.Block, error)
	GetTransactionsFromHeight(ctx context.Context, lastHeight *int) ([]indexer.Transaction, bool, error)
}

func liveEvent(kind, networkID, field string, payload any) []byte {
	data, _ := json.Marshal(map[string]any{
		"type":       kind,
		"network_id": networkID,
		"payload":    map[string]any{"data": map[string]any{field: payload}},
	})
	return data
}

// tipEvent announces the height the feed is currently at, without carrying a
// block with it.
//
// The HUD in the header shows the chain tip on every page, and its only source
// is this feed. Without a tip event a browser that subscribes to a cold feed
// shows a dash until the next block is minted, which on mainnet is up to 3.3s
// of a header that reads as broken. The event is regenerated per subscriber
// rather than stored, so it never enters the replay ring and can never be
// mistaken for a block that was skipped.
func tipEvent(networkID string, height int) []byte {
	data, _ := json.Marshal(map[string]any{
		"type":       "tip",
		"network_id": networkID,
		"payload":    map[string]any{"height": height},
	})
	return data
}

// broadcastTip fans a tip out without recording it in the replay ring.
func (f *liveFeed) broadcastTip(height int) {
	data := tipEvent(f.networkID, height)
	f.mu.RLock()
	defer f.mu.RUnlock()
	for ch := range f.clients {
		select {
		case ch <- data:
		default:
		}
	}
}

func (f *liveFeed) pollLoop() {
	log.Printf("[%s] live feed: started polling", f.networkID)
	for {
		if f.stopIfIdle() {
			log.Printf("[%s] live feed: no clients, stopped", f.networkID)
			return
		}
		f.pollStep(context.Background())
		time.Sleep(livePollInterval)
	}
}

// pollStep broadcasts everything that appeared since the last one.
//
// Three properties the subscribers depend on, all of which this used to break:
//
//   - Events go out oldest first. Browsers prepend each event to the top of
//     the list, and the "recent" indexer queries this used to read order height
//     DESC, so forwarding a batch as it arrived rendered every tick upside
//     down: a tick carrying four blocks put the newest of the four at the
//     bottom of its group.
//   - Nothing is sent twice. Asking for a bounded range rather than "the most
//     recent N" is what guarantees it: the old GetRecentBlocks re-read the tip
//     itself and could return a block above the height this had just read,
//     which then went out again on the following tick.
//   - A tick is cheap, so it can be frequent. See livePollInterval.
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
		// Whoever subscribed to this cold feed got no tip on replay, and the
		// next block is up to a block time away. Tell them where the chain is
		// now. On a reset this is also how a browser learns the height went
		// backwards instead of holding the old chain's number.
		f.broadcastTip(height)
	case height-f.lastBlock > maxLiveCatchup:
		f.lastBlock = height - maxLiveCatchup
	}

	if height <= f.lastBlock {
		return
	}

	// from is the tip as it stood before this poll. It stays fixed while
	// lastBlock advances, so both queries below cover the same span.
	from := f.lastBlock

	ctx2, cancel2 := context.WithTimeout(parent, liveCallTimeout)
	blocks, err := f.indexer.GetBlocksInRange(ctx2, from+1, height)
	cancel2()
	if err != nil {
		// lastBlock stays where it was, so the next tick retries this span
		// instead of skipping it. maxLiveCatchup keeps the retry bounded.
		return
	}
	for _, b := range blocks { // already ascending
		if b.Height <= from || b.Height > height {
			continue
		}
		f.broadcast(liveEvent("block", f.networkID, "getBlocks", b))
		if b.Height > f.lastBlock {
			f.lastBlock = b.Height
		}
	}

	ctx3, cancel3 := context.WithTimeout(parent, liveCallTimeout)
	txs, _, err := f.indexer.GetTransactionsFromHeight(ctx3, &from)
	cancel3()
	if err != nil {
		return
	}
	for _, tx := range txs { // ascending by height and index
		// Bounded above by the blocks this tick actually announced. This is a
		// third and even fresher read, so without the ceiling a tx from a block
		// the feed has not reached yet goes out now and goes out again when its
		// block finally arrives.
		if h := tx.BlockHeight; h > from && h <= f.lastBlock {
			f.broadcast(liveEvent("tx", f.networkID, "getTransactions", tx))
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

		// Deep enough to take a replay from every feed at once (all-networks
		// mode subscribes to each) without dropping the tail on the floor.
		ch := make(chan []byte, 256)

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
