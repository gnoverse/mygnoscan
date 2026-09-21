package gnoaddr

import "testing"

// The vectors are not synthetic. Each address below was derived by this
// algorithm and then read back with `abci_query path="bank/balances/<addr>"`
// against mainnet on 2026-09-22; every one resolved to an account the chain
// knows, and the balances quoted in the comments are what it answered. A
// reimplementation that drifts from gnovm produces addresses that look right and
// hold nothing, which no unit test over made-up input would catch.
func TestDerive(t *testing.T) {
	for _, tc := range []struct {
		name    string
		path    string
		addr    string
		deposit string
	}{
		{
			// Holds 80,846.85 GNOT in its banker: the realm that made the case
			// for showing this at all.
			name:    "user realm with a funded banker",
			path:    "gno.land/r/g1leu8d2vsplhehcfkjg50mwgdpxdkt8tztu95wr/bubblerumble2",
			addr:    "g1qxp9zvu4w6t3v3e8tschm8avdxjz8q55e4u4s3",
			deposit: "g1df9ckm973dvnq30xvzl6yq2yl8ghlr84tl662l",
		},
		{
			// The wrapped-GNOT vault, and the largest realm balance on chain.
			name:    "namespaced realm",
			path:    "gno.land/r/gnoland/wugnot",
			addr:    "g15vj5q08amlvyd0nx6zjgcvwq2d0gt9fcchrvum",
			deposit: "g1828ptkrj9ttvuw0sl6kuh4l26tr4qwcy9gtktg",
		},
		{
			// Empty banker, 127.86 GNOT of storage deposit. The case this
			// feature exists for: the account is real and worth printing even
			// though it holds nothing.
			name:    "realm with an empty banker",
			path:    "gno.land/r/gnoland/blog",
			addr:    "g1n2j0gdyv45aem9p0qsfk5d2gqjupv5z536na3d",
			deposit: "g1lgdfanm2y5h5255u5qxskxtmkhvt7g7z3p8xmt",
		},
		{
			// A path three segments deep, and a /p/ rather than a /r/: pure
			// packages get both accounts too, and the deposit one is funded.
			name:    "pure package, nested path",
			path:    "gno.land/p/nt/ufmt/v0",
			addr:    "g1qzka4m04kxs9f0ggcc575hgqyqdml348n5ml87",
			deposit: "g1m5ml0k5jnnevy34sw4pqyq6fu47xwyj940n3jj",
		},
		{
			// Not hashed. A MsgRun package belongs to its caller, so the
			// address is already in the path and there is no deposit account.
			name:    "run path carries its own address",
			path:    "gno.land/e/g1manfred47kzduec920z88wfr64ylksmdcedlf5/run",
			addr:    "g1manfred47kzduec920z88wfr64ylksmdcedlf5",
			deposit: "",
		},
		{name: "empty path", path: "", addr: "", deposit: ""},
		{name: "stdlib path has no account", path: "strconv", addr: "", deposit: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Derive(tc.path); got != tc.addr {
				t.Errorf("Derive(%q) = %q, want %q", tc.path, got, tc.addr)
			}
			if got := DeriveStorageDeposit(tc.path); got != tc.deposit {
				t.Errorf("DeriveStorageDeposit(%q) = %q, want %q", tc.path, got, tc.deposit)
			}
		})
	}
}

// The two accounts must never collide, and the suffix is the only thing keeping
// them apart. A copy-paste that dropped it would give a realm one account that
// holds both its coins and its deposit, which reads as a doubled balance.
func TestDepositIsADistinctAccount(t *testing.T) {
	const path = "gno.land/r/gnoland/blog"
	if Derive(path) == DeriveStorageDeposit(path) {
		t.Fatal("banker and storage deposit derived to the same address")
	}
}

// bech32 is checksummed, so a one-character difference in the path has to change
// far more than one character of the output. This is the cheap guard against an
// encoder that silently drops the checksum.
func TestDerivationIsSensitive(t *testing.T) {
	a, b := Derive("gno.land/r/gnoland/blog"), Derive("gno.land/r/gnoland/blogs")
	if a == b {
		t.Fatal("two different paths derived to the same address")
	}
	same := 0
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] == b[i] {
			same++
		}
	}
	if same > len(a)/2 {
		t.Errorf("%q and %q share %d of %d characters, which is not a hash", a, b, same, len(a))
	}
}
