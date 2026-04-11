package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// SSE-based live feed. The Go backend subscribes to the tx-indexer WebSocket
// and fans out events to browser clients via Server-Sent Events.
// This avoids WebSocket upgrade issues with HTTP/2 reverse proxies (Caddy).

type liveFeed struct {
	mu         sync.RWMutex
	clients    map[chan []byte]struct{}
	running    bool
	indexerURL string
}

var feed = &liveFeed{
	clients: make(map[chan []byte]struct{}),
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
			// Client too slow, drop
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
	go f.connectLoop()
}

func (f *liveFeed) connectLoop() {
	for {
		f.mu.RLock()
		nClients := len(f.clients)
		f.mu.RUnlock()
		if nClients == 0 {
			f.mu.Lock()
			f.running = false
			f.mu.Unlock()
			return
		}

		if err := f.subscribe(); err != nil {
			log.Printf("live feed: %v, reconnecting in 5s", err)
			time.Sleep(5 * time.Second)
		}
	}
}

func (f *liveFeed) subscribe() error {
	wsURL := strings.Replace(feed.indexerURL, "https://", "wss://", 1)
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		Subprotocols:     []string{"graphql-transport-ws"},
	}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	// Init
	conn.WriteJSON(map[string]string{"type": "connection_init"})

	// Read ack
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var ack map[string]any
	if err := conn.ReadJSON(&ack); err != nil {
		return fmt.Errorf("ack: %w", err)
	}

	// Subscribe to transactions
	conn.WriteJSON(map[string]any{
		"id": "tx", "type": "subscribe",
		"payload": map[string]string{
			"query": `subscription { getTransactions(where: {}) { hash block_height success gas_used gas_fee { amount denom } messages { typeUrl route value { __typename ... on MsgCall { caller pkg_path func } ... on MsgAddPackage { creator package { name path } } ... on MsgRun { caller } ... on BankMsgSend { from_address to_address amount } } } } }`,
		},
	})

	// Subscribe to blocks
	conn.WriteJSON(map[string]any{
		"id": "block", "type": "subscribe",
		"payload": map[string]string{
			"query": `subscription { getBlocks(where: {}) { height time num_txs proposer_address_raw } }`,
		},
	})

	conn.SetReadDeadline(time.Time{}) // No deadline for streaming

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}

		var msg struct {
			ID      string          `json:"id"`
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "next":
			// Forward to SSE clients with event type
			eventType := "tx"
			if msg.ID == "block" {
				eventType = "block"
			}
			sseData, _ := json.Marshal(map[string]any{
				"type":    eventType,
				"payload": json.RawMessage(msg.Payload),
			})
			f.broadcast(sseData)
		case "ping":
			conn.WriteJSON(map[string]string{"type": "pong"})
		}
	}
}

func initLiveFeed(indexerURL string) {
	feed.indexerURL = indexerURL
}

func sseHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		ch := feed.addClient()
		defer feed.removeClient(ch)

		// Send keepalive comment immediately
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
