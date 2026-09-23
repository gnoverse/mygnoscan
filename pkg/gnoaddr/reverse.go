package gnoaddr

// Reverse answers the question Derive cannot: given a bare g1… account, which
// package owns it.
//
// There is no lookup for this and there cannot be one. The address is the first
// twenty bytes of a SHA-256, so the mapping is one-way, and the chain never
// stores the preimage because it never needs to. The only way back is to derive
// every path forward and keep the answers — cheap at explorer scale (two hashes
// per package, a few hundred packages per chain) and exact, because a hit is the
// preimage itself rather than a heuristic.
//
// This is what turns "g1f4v2vrs8yny4ru6vcn2y0f8pnm4ycsvxfvkqwm sent 4,000 GNOT"
// into "r/gnoswap/pool sent 4,000 GNOT". Without it a transfer list is a wall of
// addresses, and the reader cannot tell a payout from a person.
type Reverse struct {
	m map[string]Owner
}

// Owner names the package an address belongs to, and which of its two accounts
// the address is.
//
// A package owns exactly two: the banker, which holds the coins it is handed,
// and the storage deposit account, which holds the ugnot locked against its
// bytes. They are different accounts with different balances, and showing a
// deposit refund as "the realm received money" is wrong in the way that matters.
type Owner struct {
	// Path is the full package path, e.g. gno.land/r/gnoswap/pool.
	Path string
	// Deposit is true when the address is the storage deposit account rather
	// than the banker.
	Deposit bool
}

// NewReverse builds the index over the paths given. Passing the same path twice
// is harmless; passing a path with no account of its own (a bare std library
// name) skips it.
//
// Run paths are skipped rather than indexed. Derive resolves gno.land/e/<g1…>/run
// to the *caller's* own address, which is a person, so indexing one would put a
// realm's name on a human's account — the single worst answer this index could
// give, because it is confidently wrong rather than absent.
func NewReverse(paths []string) *Reverse {
	r := &Reverse{m: make(map[string]Owner, len(paths)*2)}
	for _, p := range paths {
		if p == "" || runPath.MatchString(p) {
			continue
		}
		if addr := Derive(p); addr != "" {
			r.m[addr] = Owner{Path: p}
		}
		if addr := DeriveStorageDeposit(p); addr != "" {
			r.m[addr] = Owner{Path: p, Deposit: true}
		}
	}
	return r
}

// Lookup returns the package owning addr. The boolean is false for an address
// that belongs to no known package, which on a synced chain means a human (or a
// package this explorer has not seen).
func (r *Reverse) Lookup(addr string) (Owner, bool) {
	if r == nil || addr == "" {
		return Owner{}, false
	}
	o, ok := r.m[addr]
	return o, ok
}

// Len is how many accounts the index resolves, for callers that want to say so.
func (r *Reverse) Len() int {
	if r == nil {
		return 0
	}
	return len(r.m)
}
