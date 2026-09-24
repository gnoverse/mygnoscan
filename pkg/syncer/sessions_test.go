package syncer

import (
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

func TestDecodeSessionRaw(t *testing.T) {
	var g sessionRawGrant
	msg := indexer.TxMessage{
		Route: "auth", TypeURL: "create_session",
		Value: indexer.MessageValue{Typename: "UnexpectedMessage", Raw: realGrantRaw},
	}
	if !decodeSessionRaw(msg, &g) {
		t.Fatal("decodeSessionRaw returned false on a real mainnet grant")
	}
	if got := indexer.AddressFromPubKey(g.SessionKey); got != realGrantAddr {
		t.Errorf("derived address = %q, want %q (the address the chain reports)", got, realGrantAddr)
	}
	if g.Creator != "g1manfred47kzduec920z88wfr64ylksmdcedlf5" {
		t.Errorf("creator = %q", g.Creator)
	}
	if g.ExpiresAt != 1792772718 {
		t.Errorf("expires_at = %d", g.ExpiresAt)
	}
	if len(g.AllowPaths) != 1 || g.AllowPaths[0] != "vm/exec:gno.land/r/moul/x/reaper" {
		t.Errorf("allow_paths = %v", g.AllowPaths)
	}
	// Normalised to the spelling the chain's own account read uses, so the two
	// sources of the same fact agree and the frontend has one shape.
	if got := g.limitString(); got != "5000000ugnot" {
		t.Errorf("limitString = %q, want %q", got, "5000000ugnot")
	}
	if g.SpendPeriod != 0 {
		t.Errorf("spend_period = %d, want 0 for a lifetime cap", g.SpendPeriod)
	}
}

func TestDecodeSessionRawRejects(t *testing.T) {
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var g sessionRawGrant
			if decodeSessionRaw(tt.msg, &g) {
				t.Error("decoded something from a message that carries no grant")
			}
		})
	}
}

// A key too short to be a compressed secp256k1 point must derive nothing rather
// than a plausible address. A wrong-length key that still produced a g1 string
// would key a row onto an account that does not exist.
func TestAddressFromShortKeyIsEmpty(t *testing.T) {
	if got := indexer.AddressFromPubKey([]byte{2, 147, 52}); got != "" {
		t.Errorf("AddressFromPubKey(3 bytes) = %q, want empty", got)
	}
}
