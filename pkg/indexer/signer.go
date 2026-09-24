package indexer

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"golang.org/x/crypto/ripemd160" //nolint:staticcheck // the chain's address scheme uses it
)

// secp256k1TypeURL is how amino names the key type inside a signature.
const secp256k1TypeURL = "/tm.PubKeySecp256k1"

// compressedPubKeyLen is a secp256k1 point in compressed form.
const compressedPubKeyLen = 33

// SignerAddress derives the account that actually signed a transaction, from
// the public key amino-encoded in its raw content.
//
// This is the only way to tell a session-signed transaction from a self-signed
// one. A message names its *caller* — the account being acted for — which is
// the same whether the account signed for itself or a session key signed on its
// behalf. Only the signature says which key was used, and gno's session feature
// exists precisely so those can differ.
//
// Returns "" when the content carries no recognisable key, which is a normal
// answer: content_raw is only selected on single-transaction queries, so list
// views have nothing to derive from.
func SignerAddress(contentRaw string) string {
	raw := strings.TrimSuffix(strings.TrimPrefix(strings.Trim(contentRaw, `"`), "Tx{"), "}")
	b, err := hex.DecodeString(raw)
	if err != nil {
		return ""
	}

	i := strings.Index(string(b), secp256k1TypeURL)
	if i < 0 {
		return ""
	}
	// The key follows as a length-delimited field: after the type URL comes the
	// value wrapper, then 0x21 (33) and the point itself. Scanning for that
	// length byte rather than decoding the whole envelope keeps this to the one
	// field that matters, and a wrong guess fails the length check below rather
	// than producing a plausible-looking wrong address.
	rest := b[i+len(secp256k1TypeURL):]
	j := -1
	for k, c := range rest {
		if c == compressedPubKeyLen {
			j = k
			break
		}
	}
	if j < 0 || j+1+compressedPubKeyLen > len(rest) {
		return ""
	}
	pub := rest[j+1 : j+1+compressedPubKeyLen]

	return AddressFromPubKey(pub)
}

// AddressFromPubKey derives a g1 address from a 33-byte compressed secp256k1
// public key.
//
// ripemd160(sha256(pubkey)), the classic Bitcoin-style derivation gno
// inherits, *not* the truncated sha256 some other Tendermint key types use.
// Getting this wrong yields a valid-looking bech32 address that matches
// nothing, which reads as "every transaction is session-signed".
//
// Exported because the session grant in an auth/create_session carries its key
// as those raw bytes and not as an address, so the syncer has to do this same
// derivation to know which account a grant is even about.
func AddressFromPubKey(pub []byte) string {
	if len(pub) != compressedPubKeyLen {
		return ""
	}
	sum := sha256.Sum256(pub)
	h := ripemd160.New()
	h.Write(sum[:])
	return bech32Address(h.Sum(nil))
}

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

func bech32Polymod(values []byte) uint32 {
	gen := []uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := uint32(1)
	for _, v := range values {
		b := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := 0; i < 5; i++ {
			if (b>>i)&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}

// bech32Address encodes 20 address bytes with gno's "g" prefix.
func bech32Address(data []byte) string {
	conv := convertBits(data, 8, 5)
	values := append([]byte{}, hrpExpand("g")...)
	values = append(values, conv...)
	values = append(values, 0, 0, 0, 0, 0, 0)
	polymod := bech32Polymod(values) ^ 1

	var sb strings.Builder
	sb.WriteString("g1")
	for _, c := range conv {
		sb.WriteByte(bech32Charset[c])
	}
	for i := 0; i < 6; i++ {
		sb.WriteByte(bech32Charset[(polymod>>uint(5*(5-i)))&31])
	}
	return sb.String()
}

func hrpExpand(hrp string) []byte {
	out := make([]byte, 0, len(hrp)*2+1)
	for _, c := range hrp {
		out = append(out, byte(c)>>5)
	}
	out = append(out, 0)
	for _, c := range hrp {
		out = append(out, byte(c)&31)
	}
	return out
}

func convertBits(data []byte, from, to uint) []byte {
	var acc, bits uint
	maxv := byte(1<<to - 1)
	out := make([]byte, 0, len(data)*int(from)/int(to)+1)
	for _, v := range data {
		acc = acc<<from | uint(v)
		bits += from
		for bits >= to {
			bits -= to
			out = append(out, byte(acc>>bits)&maxv)
		}
	}
	if bits > 0 {
		out = append(out, byte(acc<<(to-bits))&maxv)
	}
	return out
}
