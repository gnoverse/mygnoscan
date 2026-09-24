package syncer

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moul/mygnoscan/pkg/indexer"
	"github.com/moul/mygnoscan/pkg/store"
)

// Session grants, replayed from the auth/* messages.
//
// The indexer models none of the three (probed on mainnet and pearl,
// 2026-09-23, `__type(name:"MsgCreateSession")` null on both), so they arrive
// as UnexpectedMessage carrying only `raw`: the message's own JSON. The typed
// fragments in pkg/indexer exist for a chain whose indexer catches up; until
// one does, this decodes the raw.
//
// What makes this worth storing at all is that nothing else can answer it. A
// session signs for a master and the master stays the caller of every message,
// so calls and bank_sends record the master and the session address appears
// nowhere. The chain will not close the loop either: auth/accounts/<session>
// returns null. Only the grant transaction ties key to account.

// How hard this sweep leans on the indexer.
//
// Gentler than the shared backfillConcurrency: this runs on top of normal sync
// catch-up, which is already the traffic the indexer rate-limits, and a 403
// here costs a whole pass rather than one row.
const (
	sessionBackfillConcurrency = 4
	sessionBackfillRetryPause  = 3 * time.Second
)

// sessionRawGrant is the JSON inside UnexpectedMessage.raw for
// auth/create_session. Field names are the struct tags in
// tm2/pkg/sdk/auth/msgs.go; the shapes are what mainnet actually returns.
type sessionRawGrant struct {
	Creator string `json:"creator"`
	// The raw 33-byte compressed pubkey, not an address: crypto.PubKey
	// amino-JSON-encodes as its bytes. Deriving the address is on us.
	SessionKey []byte   `json:"session_key"`
	ExpiresAt  int64    `json:"expires_at"`
	AllowPaths []string `json:"allow_paths"`
	// A coin array here, though the chain's own auth/accounts read spells the
	// same value "5000000ugnot". Normalised to the string form so the two
	// sources agree and the frontend has one shape to render.
	SpendLimit []struct {
		Denom  string `json:"denom"`
		Amount int64  `json:"amount"`
	} `json:"spend_limit"`
	SpendPeriod int64 `json:"spend_period"`
}

func (g sessionRawGrant) limitString() string {
	parts := make([]string, 0, len(g.SpendLimit))
	for _, c := range g.SpendLimit {
		parts = append(parts, strconv.FormatInt(c.Amount, 10)+c.Denom)
	}
	return strings.Join(parts, ",")
}

// recordSessions stores the session grants and revocations in one transaction.
//
// Rides whatever walk is already holding the payload, the way
// recordTokenTransfers does, so it costs no extra query.
//
// A failed transaction is skipped: the chain rejected it, so no grant exists
// and recording one would invent a key that can sign.
func (s *Syncer) recordSessions(tx indexer.Transaction, blockTime string) int {
	if !tx.Success {
		return 0
	}
	stored := 0
	for _, msg := range tx.Messages {
		if msg.Route != "auth" {
			continue
		}
		switch msg.TypeURL {
		case "create_session":
			var g sessionRawGrant
			if !decodeSessionRaw(msg, &g) {
				continue
			}
			addr := indexer.AddressFromPubKey(g.SessionKey)
			if addr == "" || g.Creator == "" {
				// A key we cannot derive an address for is a row keyed on
				// nothing. Logged rather than stored, because a silent skip
				// here is how a grant goes missing from a page that claims to
				// list every one.
				log.Printf("[%s] session grant at %d: undecodable session_key (%d bytes)",
					s.networkID, tx.BlockHeight, len(g.SessionKey))
				continue
			}
			if err := s.db.UpsertSessionGrant(store.SessionGrant{
				Network:       s.networkID,
				SessionAddr:   addr,
				Master:        g.Creator,
				AllowPaths:    g.AllowPaths,
				SpendLimit:    g.limitString(),
				SpendPeriod:   g.SpendPeriod,
				ExpiresAt:     g.ExpiresAt,
				GrantedHeight: tx.BlockHeight,
				GrantedTime:   blockTime,
				GrantedTx:     tx.Hash,
			}); err != nil {
				log.Printf("[%s] session grant at %d: %v", s.networkID, tx.BlockHeight, err)
				continue
			}
			stored++

		case "revoke_session":
			var g sessionRawGrant
			if !decodeSessionRaw(msg, &g) {
				continue
			}
			addr := indexer.AddressFromPubKey(g.SessionKey)
			if addr == "" {
				continue
			}
			if err := s.db.RevokeSessionGrant(s.networkID, addr, tx.BlockHeight, blockTime, tx.Hash); err != nil {
				log.Printf("[%s] session revoke at %d: %v", s.networkID, tx.BlockHeight, err)
				continue
			}
			stored++

		case "revoke_all_sessions":
			var g sessionRawGrant
			if !decodeSessionRaw(msg, &g) || g.Creator == "" {
				continue
			}
			if err := s.db.RevokeAllSessionGrants(s.networkID, g.Creator, tx.BlockHeight, blockTime, tx.Hash); err != nil {
				log.Printf("[%s] session revoke-all at %d: %v", s.networkID, tx.BlockHeight, err)
				continue
			}
			stored++
		}
	}
	return stored
}

// decodeSessionRaw reads the grant out of a message, from whichever of the two
// shapes this indexer gave us.
//
// The typed fields are preferred when present, because an indexer that models
// the type has already parsed it. Today none does, so in practice this always
// takes the raw path; the typed branch is what stops this silently going blank
// on the day one ships support.
func decodeSessionRaw(msg indexer.TxMessage, g *sessionRawGrant) bool {
	if msg.Value.Raw != "" {
		return json.Unmarshal([]byte(msg.Value.Raw), g) == nil
	}
	if msg.Value.Creator == "" && msg.Value.SessionKey == "" {
		return false
	}
	// A modelled MsgCreateSession gives session_key as an address string, so
	// there is nothing to derive and nothing for AddressFromPubKey to do. The
	// caller handles that by checking Creator, so flag it here rather than
	// returning a half-filled struct.
	g.Creator = msg.Value.Creator
	g.ExpiresAt = msg.Value.ExpiresAt
	g.AllowPaths = msg.Value.AllowPaths
	g.SpendPeriod = int64(msg.Value.SpendPeriod)
	return false
}

// backfillSessions sweeps the history the forward fill never saw.
//
// Same shape as backfillTokenTransfers: the forward pass rides the sync walk
// and so starts at whatever height this feature shipped on, leaving everything
// below it unindexed. auth messages cannot be filtered for in GraphQL at all
// (MessageRoute is `vm` and `bank` only), so the only way to find them is to
// walk every block and look, which is what this does, a bounded batch per pass.
//
// Unlike backfillTokenTransfers it walks NEWEST FIRST. Sessions landed on
// mainnet around height 270,000 of 306,000, so sweeping up from genesis spends
// about a day on blocks that cannot hold a grant. See SessionBackfillRange.
func (s *Syncer) backfillSessions(ctx context.Context) {
	// Pin the boundary before the first batch, so the sweep has a fixed finish
	// line rather than chasing the tip forever. The store reads the tip itself;
	// see PinSessionBackfillStop for why it is not passed in.
	if err := s.db.PinSessionBackfillStop(s.networkID); err != nil {
		log.Printf("[%s] session backfill pin: %v", s.networkID, err)
	}

	from, to, more, err := s.db.SessionBackfillRange(s.networkID, backfillTxBatch)
	if err != nil {
		log.Printf("[%s] session backfill: %v", s.networkID, err)
		return
	}
	if !more {
		return
	}

	type blockTxs struct {
		txs []indexer.Transaction
		err error
	}
	heights := make([]int, 0, to-from)
	for h := from; h < to; h++ {
		heights = append(heights, h)
	}
	results := make([]blockTxs, len(heights))
	var wg sync.WaitGroup
	sem := make(chan struct{}, sessionBackfillConcurrency)
	for i, h := range heights {
		wg.Add(1)
		go func(i, h int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			txs, err := s.client.GetTransactionsByBlock(ctx, h)
			if err != nil {
				// One retry, after a pause. A rate-limited indexer answers 403
				// and this sweep is exactly the traffic that earns one: it adds
				// a burst of block queries on top of normal sync catch-up.
				//
				// Worth retrying rather than leaving to the next pass because
				// the batch is scanned from the top down and stops at the first
				// failure, so a single refused height at the top costs the
				// whole pass. Observed on mainnet 2026-09-25, where height
				// 306501 got a 403 and the cursor did not move at all.
				select {
				case <-ctx.Done():
					results[i] = blockTxs{err: ctx.Err()}
					return
				case <-time.After(sessionBackfillRetryPause):
				}
				txs, err = s.client.GetTransactionsByBlock(ctx, h)
			}
			results[i] = blockTxs{txs: txs, err: err}
		}(i, h)
	}
	wg.Wait()

	// The cursor only advances over heights that answered, so an unhealthy
	// indexer costs a retry rather than a permanent hole nothing comes back for.
	// Walking newest first, so the cursor records the LOWEST height that
	// answered and the scan runs down from the top of the batch. A gap stops
	// the cursor above it, so the missing heights are retried rather than
	// skipped past.
	var answered []indexer.Transaction
	done := to
	for i := len(results) - 1; i >= 0; i-- {
		r := results[i]
		if r.err != nil {
			log.Printf("[%s] session backfill at %d: %v", s.networkID, heights[i], r.err)
			break
		}
		answered = append(answered, r.txs...)
		done = heights[i]
	}
	stored := 0
	if len(answered) > 0 {
		// Block times are resolved in one pass: GetTransactionsByBlock does not
		// populate tx.BlockTime, and a grant stored with an empty time would
		// show up on the page as having happened at no particular moment.
		times := s.fetchBlockTimes(ctx, answered)
		for _, tx := range answered {
			stored += s.recordSessions(tx, times[tx.BlockHeight])
		}
	}
	if done == to {
		return // nothing answered; leave the cursor alone and retry next pass
	}
	if err := s.db.SetSessionBackfillCursor(s.networkID, done); err != nil {
		log.Printf("[%s] session backfill cursor: %v", s.networkID, err)
		return
	}
	if stored > 0 {
		log.Printf("[%s] session backfill: %d..%d, %d grant(s) recovered",
			s.networkID, done, to-1, stored)
	}
}
