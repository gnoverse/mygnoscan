// Package gnoaddr derives the accounts that belong to a gno package path.
//
// Every deployed package owns two accounts, and neither is stored anywhere: both
// are pure functions of the path, so the chain never needs to write them down
// and no indexer can look them up. That is why the explorer showed neither until
// now, and why a realm holding 80,846 GNOT had a page that never mentioned money.
//
//	realm banker      hash("pkgPath:" + path)                   coins it holds
//	storage deposit   hash("pkgPath:" + path + ".storageDeposit") its locked bytes
//
// The derivation mirrors gnovm/pkg/gnolang/misc.go (DerivePkgBech32Addr and
// DeriveStorageDepositBech32Addr): the first twenty bytes of the SHA-256 of the
// preimage, bech32-encoded under the "g" prefix. Reimplemented in forty lines
// rather than imported, because mygnoscan's go.mod depends on neither the gno
// monorepo nor a bech32 library and taking both on for this would be the more
// expensive half of the trade. The test pins it to three live mainnet balances,
// which is what keeps the reimplementation honest.
package gnoaddr

import (
	"crypto/sha256"
	"regexp"
	"strings"
)

// hrp is the human-readable part every gno.land address carries.
const hrp = "g"

// addressSize is crypto.AddressSize: a truncated hash, not a full one.
const addressSize = 20

// runPath matches gno.land/e/<g1...>/run, the one path shape whose address is
// not derived at all.
//
// A MsgRun executes under an ephemeral package owned by the *caller*, so the
// address is embedded in the path rather than hashed out of it (gnolang's
// IsGnoRunPath). Hashing one of these would mint a plausible-looking address
// that holds nothing and belongs to nobody, which is worse than refusing.
var runPath = regexp.MustCompile(`^[a-z0-9.\-]+/e/(g1[a-z0-9]+)/run$`)

// Derive returns the account a package's coins live in, or "" for a path with
// no account: an empty path, or a std library path with no domain.
func Derive(pkgPath string) string {
	if pkgPath == "" {
		return ""
	}
	if m := runPath.FindStringSubmatch(pkgPath); m != nil {
		return m[1]
	}
	if !strings.Contains(pkgPath, "/") {
		return ""
	}
	return bech32("pkgPath:" + pkgPath)
}

// DeriveStorageDeposit returns the account holding a package's storage deposit:
// the ugnot locked against its bytes, refunded to whoever frees them.
//
// A run path has no storage deposit account of its own, and deriving one off the
// caller's address would name an account that never existed.
func DeriveStorageDeposit(pkgPath string) string {
	if pkgPath == "" || !strings.Contains(pkgPath, "/") {
		return ""
	}
	if runPath.MatchString(pkgPath) {
		return ""
	}
	return bech32("pkgPath:" + pkgPath + ".storageDeposit")
}

func bech32(preimage string) string {
	sum := sha256.Sum256([]byte(preimage))
	return encode(hrp, sum[:addressSize])
}

// --- bech32 ------------------------------------------------------------------
//
// BIP-173, checksum constant 1. Encoding only: nothing here ever needs to read
// an address back, and a decoder is the half with the failure modes.

const charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

func encode(hrp string, data []byte) string {
	conv := convertBits(data)
	var sb strings.Builder
	sb.WriteString(hrp)
	sb.WriteByte('1')
	for _, v := range conv {
		sb.WriteByte(charset[v])
	}
	for _, v := range checksum(hrp, conv) {
		sb.WriteByte(charset[v])
	}
	return sb.String()
}

// convertBits regroups bytes into 5-bit values, zero-padding the tail. No error
// path: 20 bytes is 160 bits, which is 32 groups exactly, and the padding branch
// is dead for every address. It stays in because the function is written for
// bytes in general and a silent truncation would be the worse bug.
func convertBits(data []byte) []byte {
	out := make([]byte, 0, len(data)*8/5+1)
	acc, bits := 0, uint(0)
	for _, b := range data {
		acc = acc<<8 | int(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out = append(out, byte(acc>>bits)&31)
		}
	}
	if bits > 0 {
		out = append(out, byte(acc<<(5-bits))&31)
	}
	return out
}

func checksum(hrp string, data []byte) []byte {
	values := append(hrpExpand(hrp), data...)
	values = append(values, 0, 0, 0, 0, 0, 0)
	mod := polymod(values) ^ 1
	out := make([]byte, 6)
	for i := range out {
		out[i] = byte(mod>>uint(5*(5-i))) & 31
	}
	return out
}

func hrpExpand(hrp string) []byte {
	out := make([]byte, 0, len(hrp)*2+1)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]>>5)
	}
	out = append(out, 0)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]&31)
	}
	return out
}

func polymod(values []byte) int {
	gen := [5]int{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := 1
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ int(v)
		for i := 0; i < 5; i++ {
			if (top>>uint(i))&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}
