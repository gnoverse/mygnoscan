package syncer

import (
	"context"
	"testing"

	"github.com/moul/mygnoscan/pkg/indexer"
)

// realGrantRaw is UnexpectedMessage.raw for a real mainnet auth/create_session,
// transaction zjlVIZgqeSyqzup70aoUG8SrGZA5dV0W8FHL3ccyJ2Y= at block 272956,
// captured 2026-09-23. Verbatim, so the two shapes that are easy to get wrong
// are the real ones: session_key is a 33-byte pubkey array and not a g1
// address, and spend_limit is a coin array here while the chain's own
// auth/accounts read spells the same value "5000000ugnot".
const realGrantRaw = `{"creator":"g1manfred47kzduec920z88wfr64ylksmdcedlf5","session_key":[2,147,52,73,49,114,213,70,63,43,241,103,46,138,61,165,120,105,81,185,211,81,9,152,248,88,199,168,87,110,43,246,208],"expires_at":1792772718,"allow_paths":["vm/exec:gno.land/r/moul/x/reaper"],"spend_limit":[{"denom":"ugnot","amount":5000000}]}`

// The address the chain itself reports for that key, from
// auth/accounts/<master>/sessions. This is the assertion that matters: if the
// derivation is wrong it still yields a valid-looking g1 address, just one that
// matches no account on the chain.
const realGrantAddr = "g1rrtqvv2kcffw0nezkecxmxyqa6u9wy06e03fck"

// The two shapes a live instance actually sees, from the two interchangeable
// indexers mainnet is configured with. Handling only the first is how every
// grant went missing on 2026-09-25.
func TestDecodeSessionMsgHandlesBothIndexerShapes(t *testing.T) {
	const master = "g1manfred47kzduec920z88wfr64ylksmdcedlf5"

	tests := []struct {
		name string
		msg  indexer.TxMessage
	}{
		{
			// indexer.gno.land: does not model the type, so it arrives as
			// UnexpectedMessage with the grant as JSON and the key as bytes.
			name: "raw, from an indexer that does not model the type",
			msg: indexer.TxMessage{
				Route: "auth", TypeURL: "create_session",
				Value: indexer.MessageValue{Typename: "UnexpectedMessage", Raw: realGrantRaw},
			},
		},
		{
			// indexer.onbloc.xyz: models it, so session_key is already an
			// address and spend_limit already a coin string. Captured from it
			// on 2026-09-25 for the same transaction.
			name: "typed, from an indexer that models it",
			msg: indexer.TxMessage{
				Route: "auth", TypeURL: "create_session",
				Value: indexer.MessageValue{
					Typename:    "MsgCreateSession",
					Creator:     master,
					SessionKey:  realGrantAddr,
					ExpiresAt:   1792772718,
					AllowPaths:  []string{"vm/exec:gno.land/r/moul/x/reaper"},
					SpendLimit:  "5000000ugnot",
					SpendPeriod: 0,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, ok := decodeSessionMsg(tt.msg)
			if !ok {
				t.Fatal("decodeSessionMsg returned false on a real grant")
			}
			// Both shapes must resolve to the SAME grant: same session address,
			// same master, same scope, same limit spelling.
			if g.SessionAddr != realGrantAddr {
				t.Errorf("session address = %q, want %q", g.SessionAddr, realGrantAddr)
			}
			if g.Creator != master {
				t.Errorf("creator = %q", g.Creator)
			}
			if g.SpendLimit != "5000000ugnot" {
				t.Errorf("spend_limit = %q, want the coin-string form", g.SpendLimit)
			}
			if g.ExpiresAt != 1792772718 {
				t.Errorf("expires_at = %d", g.ExpiresAt)
			}
			if len(g.AllowPaths) != 1 || g.AllowPaths[0] != "vm/exec:gno.land/r/moul/x/reaper" {
				t.Errorf("allow_paths = %v", g.AllowPaths)
			}
			if g.SpendPeriod != 0 {
				t.Errorf("spend_period = %d, want 0 for a lifetime cap", g.SpendPeriod)
			}
		})
	}
}

func TestDecodeSessionMsgRejects(t *testing.T) {
	tests := []struct {
		name string
		msg  indexer.TxMessage
	}{
		{
			name: "no raw and no typed fields",
			msg:  indexer.TxMessage{Route: "auth", TypeURL: "create_session"},
		},
		{
			name: "raw that is not JSON",
			msg: indexer.TxMessage{Route: "auth", TypeURL: "create_session",
				Value: indexer.MessageValue{Raw: "Tx{deadbeef}"}},
		},
		{
			name: "raw with a key of no recognised length",
			msg: indexer.TxMessage{Route: "auth", TypeURL: "create_session",
				Value: indexer.MessageValue{Raw: `{"creator":"g1x","session_key":[1,2,3]}`}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := decodeSessionMsg(tt.msg); ok {
				t.Error("decoded something from a message that carries no usable grant")
			}
		})
	}
}

func TestAddressFromShortKeyIsEmpty(t *testing.T) {
	if got := indexer.AddressFromPubKey([]byte{2, 147, 52}); got != "" {
		t.Errorf("AddressFromPubKey(3 bytes) = %q, want empty", got)
	}
}

// The element cap is the dangerous failure here: the resolver returns the rows
// it had alongside the error, so a caller that treats the partial page as
// success silently skips whatever fell past the cap. For this sweep that means
// losing grants with nothing to show anything went wrong.
//
// fetchSessionRange must split instead, and the floor it reports must never
// claim coverage it does not have.
func TestFetchSessionRangeSplitsOnTheElementCap(t *testing.T) {
	s, fake, _ := newTestSyncer(t, "mainnet")
	fake.SeedChain(1, 200)

	// Small enough that a 100-block range trips it and the halves do not.
	fake.CapAt = 2

	txs, floor := s.fetchSessionRange(context.Background(), 100, 200)
	if floor > 100 {
		t.Errorf("floor = %d, want <= 100: splitting should still cover the range", floor)
	}
	if len(txs) == 0 {
		t.Error("split returned no transactions at all")
	}
}

// A range that cannot be answered at all leaves the floor at `to`, which is how
// the caller knows to leave the cursor alone and retry rather than skip ahead.
func TestFetchSessionRangeReportsNoCoverageOnFailure(t *testing.T) {
	s, fake, _ := newTestSyncer(t, "mainnet")
	fake.SeedChain(1, 200)
	fake.GQLError = "boom"

	txs, floor := s.fetchSessionRange(context.Background(), 100, 200)
	if floor != 200 {
		t.Errorf("floor = %d, want 200: a failed range must claim no coverage", floor)
	}
	if len(txs) != 0 {
		t.Errorf("got %d transactions from a failed range", len(txs))
	}
}
