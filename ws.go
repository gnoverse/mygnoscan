package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// SSE live feed via polling. The Go backend polls the tx-indexer for new
// blocks/txs and fans out to browser clients via Server-Sent Events.

type liveFeed struct {
	mu         sync.RWMutex
	clients    map[chan []byte]struct{}
	running    bool
	indexer    *IndexerClient
	lastBlock  int
}

var feed = &liveFeed{
	clients: make(map[chan []byte]struct{}),
}

func initLiveFeed(client *IndexerClient) {
	feed.indexer = client
}

func (f *liveFeed) addClient() chan []byte {
	ch := make(chan []byte, 32)
	f.mu.Lock()
	f.clients[ch] = struct{}{}
	f.mu.Unlock()
	f.ensureRunning()
	return ch
}

func (f *liveFeed) removeClient(ch chan []byte) {
	f.mu.Lock()
	delete(f.clients, ch)
	f.mu.Unlock()
}

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

func (f *liveFeed) pollLoop() {
	log.Println("live feed: started polling")
	for {
		f.mu.RLock()
		n := len(f.clients)
		f.mu.RUnlock()
		if n == 0 {
			f.mu.Lock()
			f.running = false
			f.mu.Unlock()
			log.Println("live feed: no clients, stopped")
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		height, err := f.indexer.LatestBlockHeight(ctx)
		cancel()
		if err != nil {
			time.Sleep(3 * time.Second)
			continue
		}

		if f.lastBlock == 0 {
			f.lastBlock = height
		}

		if height > f.lastBlock {
			// New blocks — fetch them
			ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
			blocks, err := f.indexer.GetRecentBlocks(ctx2, height-f.lastBlock+1)
			cancel2()
			if err == nil {
				for _, b := range blocks {
					if b.Height <= f.lastBlock {
						continue
					}
					data, _ := json.Marshal(map[string]any{
						"type": "block",
						"payload": map[string]any{
							"data": map[string]any{"getBlocks": b},
						},
					})
					f.broadcast(data)
				}
			}

			// Check for new txs in these blocks
			ctx3, cancel3 := context.WithTimeout(context.Background(), 10*time.Second)
			txs, err := f.indexer.GetRecentTransactions(ctx3, 20)
			cancel3()
			if err == nil {
				for _, tx := range txs {
					if tx.BlockHeight > f.lastBlock {
						data, _ := json.Marshal(map[string]any{
							"type": "tx",
							"payload": map[string]any{
								"data": map[string]any{"getTransactions": tx},
							},
						})
						f.broadcast(data)
					}
				}
			}

			f.lastBlock = height
		}

		time.Sleep(3 * time.Second)
	}
}

func sseHandler() http.HandlerFunc {
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

		ch := feed.addClient()
		defer feed.removeClient(ch)

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
