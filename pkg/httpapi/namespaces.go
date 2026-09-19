package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sync"
	"time"
)

// Namespace ownership, read from the chain rather than inferred from who
// deployed what.
//
// The inference is wrong often enough to matter. Deploy history says
// gno.land/r/onbloc belongs to the genesis deployer, because onbloc's packages
// were included in genesis rather than deployed by onbloc — and the same
// address then "owns" gnoland, aeddi, mason, sunspirit and jeronimoalbi by the
// same reasoning. Checked against the registry on mainnet, four of twelve
// namespaces resolved to a different account than their deploy history
// suggested, including one where the derived label itself disagrees.
//
// r/sys/users is the registry the chain itself uses, so it is the answer rather
// than an approximation of it.
const namespaceRealm = "gno.land/r/sys/users"

// nsOwnerTTL is how long a resolved owner is trusted. Registrations change
// rarely; this is short enough that a transfer shows up the same session.
const nsOwnerTTL = 10 * time.Minute

// nsAddress pulls the account out of qeval's rendered struct. The realm returns
// a Go-ish literal rather than JSON, and the address is the only bech32-shaped
// token in it.
var nsAddress = regexp.MustCompile(`g1[a-z0-9]{38}`)

type nsCacheEntry struct {
	owners  map[string]string
	fetched time.Time
}

var nsCache = struct {
	mu sync.Mutex
	by map[string]nsCacheEntry
}{by: map[string]nsCacheEntry{}}

// NamespaceOwners resolves each name to the account registered for it.
//
// Unregistered names are omitted rather than guessed at: a namespace can hold
// packages without a registration (leon does on mainnet), and inventing an
// owner for it is exactly the error this exists to avoid.
//
// One query per name, which is affordable because the set is small and bounded
// by what has actually been deployed — twelve on mainnet — and cached. A name
// that fails to resolve is skipped, not cached as absent, so a transient RPC
// failure does not pin it as unregistered for the whole TTL.
func (a *API) NamespaceOwners(ctx context.Context, network string, names []string) map[string]string {
	rpcURL := a.rpcURLFor(network)
	if rpcURL == "" {
		return nil
	}

	nsCache.mu.Lock()
	if e, ok := nsCache.by[network]; ok && time.Since(e.fetched) < nsOwnerTTL {
		nsCache.mu.Unlock()
		return e.owners
	}
	nsCache.mu.Unlock()

	owners := map[string]string{}
	for _, name := range names {
		expr := fmt.Sprintf(`%s.ResolveName(%q)`, namespaceRealm, name)
		out, err := fetchABCIQuery(ctx, rpcURL, "vm/qeval", expr)
		if err != nil {
			continue
		}
		if m := nsAddress.FindString(out); m != "" {
			owners[name] = m
		}
	}

	nsCache.mu.Lock()
	nsCache.by[network] = nsCacheEntry{owners: owners, fetched: time.Now()}
	nsCache.mu.Unlock()
	return owners
}

// HandleNamespaces answers name -> owning account for a network.
func (a *API) HandleNamespaces(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	if network == "" {
		// Ownership is per chain: the same name can be registered to different
		// accounts on two networks, and merging them would assert something
		// neither chain says.
		jsonError(w, "namespace ownership is per-chain: add ?network=", 400)
		return
	}

	names, err := a.db.Namespaces(network)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	owners := a.NamespaceOwners(r.Context(), network, names)
	if owners == nil {
		owners = map[string]string{}
	}
	JSONResponse(w, owners)
}
