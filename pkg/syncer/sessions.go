package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strconv"
	"strings"

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

// sessionBackfillBatch is how many blocks one pass sweeps.
//
// Twenty times backfillTxBatch, because the cost model changed: that constant
// sizes a per-height fan-out, where the batch is also the request count. This
// sweep issues ONE range query per batch, so the batch size no longer sets the
// load, and 100 blocks per 30-second pass meant 25 hours to cross mainnet.
//
// Sized against the element cap rather than under it. A range this wide will
// sometimes exceed the resolver's row limit; fetchSessionRange halves until it
// fits, which costs a handful of extra requests and self-tunes to whatever the
// chain's density actually is, instead of guessing a number that is wrong on
// both a quiet chain and a busy one.
const sessionBackfillBatch = 2000

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
		g, ok := decodeSessionMsg(msg)
		if !ok {
			// Logged, never skipped quietly. A silent continue here is exactly
			// how the typed-indexer case stayed invisible: the sweep looked
			// healthy and the index stayed empty.
			if msg.TypeURL == "create_session" || msg.TypeURL == "revoke_session" ||
				msg.TypeURL == "revoke_all_sessions" {
				log.Printf("[%s] session %s at %d: could not decode (raw=%dB typename=%q)",
					s.networkID, msg.TypeURL, tx.BlockHeight, len(msg.Value.Raw), msg.Value.Typename)
			}
			continue
		}

		switch msg.TypeURL {
		case "create_session":
			if g.SessionAddr == "" || g.Creator == "" {
				log.Printf("[%s] session grant at %d: no session address or creator",
					s.networkID, tx.BlockHeight)
				continue
			}
			if err := s.db.UpsertSessionGrant(store.SessionGrant{
				Network:       s.networkID,
				SessionAddr:   g.SessionAddr,
				Master:        g.Creator,
				AllowPaths:    g.AllowPaths,
				SpendLimit:    g.SpendLimit,
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
			if g.SessionAddr == "" {
				continue
			}
			if err := s.db.RevokeSessionGrant(s.networkID, g.SessionAddr, tx.BlockHeight, blockTime, tx.Hash); err != nil {
				log.Printf("[%s] session revoke at %d: %v", s.networkID, tx.BlockHeight, err)
				continue
			}
			stored++

		case "revoke_all_sessions":
			if g.Creator == "" {
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

// sessionGrantMsg is a grant with everything already resolved, whichever of the
// two shapes the indexer gave us.
type sessionGrantMsg struct {
	Creator     string
	SessionAddr string
	ExpiresAt   int64
	AllowPaths  []string
	SpendLimit  string
	SpendPeriod int64
}

// decodeSessionMsg reads a grant out of a message, from either shape.
//
// Both are live simultaneously and a single instance sees both, which is the
// thing that makes this subtle. mygnoscan's mainnet is configured with two
// interchangeable indexers and rotates to the second whenever the first rate
// limits it:
//
//   - indexer.gno.land does NOT model MsgCreateSession, so the message arrives
//     as UnexpectedMessage and the grant is JSON in `raw`, with session_key a
//     raw public key that has to be hashed into an address.
//   - indexer.onbloc.xyz DOES model it, so the message arrives typed, with
//     session_key already a g1 address and spend_limit already "5000000ugnot".
//
// Handling only the first is how every grant went missing on 2026-09-25: the
// sweep covered the blocks, the typed branch fell through to a bare `return
// false`, and recordSessions skipped each one without a word. The page reported
// a healthy sweep and an empty chain.
func decodeSessionMsg(msg indexer.TxMessage) (sessionGrantMsg, bool) {
	if raw := msg.Value.Raw; raw != "" {
		var g sessionRawGrant
		if err := json.Unmarshal([]byte(raw), &g); err != nil {
			return sessionGrantMsg{}, false
		}
		// session_key here is the raw key, not an address: crypto.PubKey
		// amino-JSON-encodes as its bytes.
		addr := indexer.AddressFromPubKey(g.SessionKey)
		if addr == "" {
			return sessionGrantMsg{}, false
		}
		return sessionGrantMsg{
			Creator:     g.Creator,
			SessionAddr: addr,
			ExpiresAt:   g.ExpiresAt,
			AllowPaths:  g.AllowPaths,
			SpendLimit:  g.limitString(),
			SpendPeriod: g.SpendPeriod,
		}, true
	}

	// Typed. Nothing to derive and nothing to normalise: an indexer that models
	// the type has already done both.
	v := msg.Value
	if v.Creator == "" && v.SessionKey == "" {
		return sessionGrantMsg{}, false
	}
	return sessionGrantMsg{
		Creator:     v.Creator,
		SessionAddr: v.SessionKey,
		ExpiresAt:   v.ExpiresAt,
		AllowPaths:  v.AllowPaths,
		SpendLimit:  v.SpendLimit,
		SpendPeriod: int64(v.SpendPeriod),
	}, true
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
// fetchSessionRange reads [from, to) and reports the lowest height it can
// honestly claim to have covered.
//
// Returns `covered == to` when it covered nothing, which the caller reads as
// "leave the cursor alone and retry". Anything lower is a real floor: every
// block from there up to `to` has been seen.
//
// Splits on ErrQueryTooLarge rather than accepting the partial page. The
// resolver caps its row count and hands back what it had alongside the error,
// so trusting it would silently skip whatever fell past the cap. For this
// caller that means losing grants with nothing to show anything went wrong,
// which is the failure mode worth spending an extra request to avoid.
func (s *Syncer) fetchSessionRange(ctx context.Context, from, to int) ([]indexer.Transaction, int) {
	txs, err := s.client.GetTransactionsInRange(ctx, from, to)
	if err == nil {
		return txs, from
	}

	if errors.Is(err, indexer.ErrQueryTooLarge) && to-from > 1 {
		// Halve and take both sides. The upper half is attempted first so a
		// failure in the lower half still yields a usable floor: the sweep
		// walks downward, so covering the top of the range is progress even
		// when the bottom has to wait for the next pass.
		mid := from + (to-from)/2
		upper, upperFloor := s.fetchSessionRange(ctx, mid, to)
		if upperFloor > mid {
			return upper, upperFloor // the upper half itself did not complete
		}
		lower, lowerFloor := s.fetchSessionRange(ctx, from, mid)
		return append(upper, lower...), lowerFloor
	}

	// A single block that will not answer, or a refusal this pass cannot get
	// past. One retry already happened inside the client-facing path below;
	// beyond that the cursor stays put and the next pass tries again, which is
	// what keeps a bad height from becoming a hole.
	log.Printf("[%s] session backfill %d..%d: %v", s.networkID, from, to-1, err)
	return nil, to
}

func (s *Syncer) backfillSessions(ctx context.Context) {
	// Pin the boundary before the first batch, so the sweep has a fixed finish
	// line rather than chasing the tip forever. The store reads the tip itself;
	// see PinSessionBackfillStop for why it is not passed in.
	if err := s.db.PinSessionBackfillStop(s.networkID); err != nil {
		log.Printf("[%s] session backfill pin: %v", s.networkID, err)
	}

	from, to, more, err := s.db.SessionBackfillRange(s.networkID, sessionBackfillBatch)
	if err != nil {
		log.Printf("[%s] session backfill: %v", s.networkID, err)
		return
	}
	if !more {
		return
	}

	// One range query for the whole batch, not one per height. The per-height
	// version spent most passes being rate-limited: 100 requests per batch on
	// top of sync catch-up is exactly what earns a 403, and a refusal at the
	// top of the batch moved the cursor not at all. Measured on mainnet
	// 2026-09-25 at 303 blocks swept of 306,501 in fifteen minutes.
	answered, covered := s.fetchSessionRange(ctx, from, to)
	done := covered

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
	if done >= to {
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
