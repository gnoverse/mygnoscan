package httpapi

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The storage map: the chain as a disk, and who is holding its blocks.
//
// gno.land charges a refundable deposit of `vm:p:storage_price` ugnot for every
// byte of realm state. That is not a fee metaphor, it is a hard capacity: the
// chain can never hold more bytes than the money supply can pay for, so
//
//	capacity = total_supply / storage_price
//
// The monorepo does the same sum in a comment next to the default price,
// "1.333B GNOT == 13.33TB" (gno.land/pkg/sdk/vm/params.go). Measured against
// mainnet on 2026-09-21 the figures were 1,333,000,221,686,563 ugnot at
// 100 ugnot/byte, which is 13.33 TB, and about 47 MB of it was in use.
//
// Three numbers therefore make the page, and each comes from a different place:
// the price from a live params read, the supply from the indexer, and the used
// bytes from local storage_events. Any one of them missing degrades the page
// rather than failing it. The map of who holds what is still worth drawing
// without a denominator, and the capacity is still worth stating when the
// breakdown is empty.

// storagePriceDefault is the chain's own default, used only to label the
// fallback. It is never silently substituted: a response that could not read
// the price says so in price_source, so a reader can tell a real 100 from a
// guessed one.
const storagePriceDefault = 100

// storagePriceTTL is how long a read price is trusted. The key only moves by
// GovDAO proposal, so this is short enough that a vote shows up the same
// session and long enough that the map is not one RPC round trip per visitor.
const storagePriceTTL = 10 * time.Minute

type priceCacheEntry struct {
	price   int64
	source  string
	fetched time.Time
}

var priceCache = struct {
	mu sync.Mutex
	by map[string]priceCacheEntry
}{by: map[string]priceCacheEntry{}}

// storagePriceFor reads vm:p:storage_price for a network, in ugnot per byte.
//
// Returns the source alongside the number rather than just the number: "chain"
// and "default" are the same 100 on mainnet today and mean entirely different
// things, and a capacity figure computed from a guess must not be presented as
// one read from the chain.
func (a *API) storagePriceFor(ctx context.Context, network string) (int64, string) {
	priceCache.mu.Lock()
	if e, ok := priceCache.by[network]; ok && time.Since(e.fetched) < storagePriceTTL {
		priceCache.mu.Unlock()
		return e.price, e.source
	}
	priceCache.mu.Unlock()

	price, source := int64(storagePriceDefault), "default"
	if rpcURL := a.rpcURLFor(network); rpcURL != "" {
		if raw, state, err := fetchParam(ctx, rpcURL, "vm:p:storage_price"); err == nil && state == paramStateSet {
			if p, err := parseUgnotPerByte(raw); err == nil {
				price, source = p, "chain"
			}
		}
	}

	priceCache.mu.Lock()
	priceCache.by[network] = priceCacheEntry{price: price, source: source, fetched: time.Now()}
	priceCache.mu.Unlock()
	return price, source
}

// parseUgnotPerByte turns the params response, a JSON-quoted coin like
// `"100ugnot"`, into a number.
//
// Rejects a price in any other denom instead of reading the digits off it: a
// capacity divided by a number of some other token is not a capacity, and
// falling back to the default at least labels itself as a fallback.
func parseUgnotPerByte(raw string) (int64, error) {
	var s string
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return 0, err
	}
	s = strings.TrimSpace(s)
	amount := strings.TrimSuffix(s, "ugnot")
	if amount == s {
		return 0, &strconv.NumError{Func: "parseUgnotPerByte", Num: s, Err: strconv.ErrSyntax}
	}
	return strconv.ParseInt(amount, 10, 64)
}

// StorageCapacity is the disk: how big it is, how much of it is spoken for, and
// what the numbers were derived from.
//
// The amounts are strings because ugnot at chain scale does not survive a JSON
// number: 1.333e15 is already past 2^53 once a chain mints more, and a
// capacity silently rounded in the browser is the one number on this page that
// must be exact.
type StorageCapacity struct {
	PricePerByte  int64  `json:"price_per_byte"`
	PriceSource   string `json:"price_source"`
	SupplyUgnot   string `json:"supply_ugnot,omitempty"`
	SupplyLocked  string `json:"supply_locked,omitempty"`
	SupplyHeight  int    `json:"supply_height,omitempty"`
	CapacityBytes string `json:"capacity_bytes,omitempty"`
	UsedBytes     int    `json:"used_bytes"`
	LockedUgnot   int    `json:"locked_ugnot"`
	Realms        int    `json:"realms"`
	Events        int    `json:"events"`
	// Error says why capacity_bytes is absent, so the page can explain a
	// missing denominator instead of drawing a disk of size zero.
	Error string `json:"error,omitempty"`
}

// HandleStorageMap serves the whole page in one response.
//
// One endpoint rather than three because the parts are only meaningful
// together: a breakdown with no capacity cannot say how full the chain is, and
// a capacity with no breakdown cannot say who filled it. It also means the
// group-by control costs no round trip, since every grouping is a different
// reading of the same rows.
func (a *API) HandleStorageMap(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	if network == "" {
		// Capacity is per chain, and so is the disk it describes. Summing two
		// chains' used bytes against one chain's supply would produce a
		// fullness figure that is true of neither.
		jsonError(w, "the storage map is per-chain: add ?network=", 400)
		return
	}

	limit := 2000
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 5000 {
		limit = v
	}

	total, err := a.db.StorageFootprintTotal(network)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	cells, err := a.db.StorageCells(network, limit)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	payers, err := a.db.StoragePayers(network, 500)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}

	price, source := a.storagePriceFor(r.Context(), network)
	capacity := StorageCapacity{
		PricePerByte: price,
		PriceSource:  source,
		UsedBytes:    total.Bytes,
		LockedUgnot:  total.Fee,
		Realms:       total.Realms,
		Events:       total.Events,
	}

	if client := a.clientFor(network); client != nil {
		if supply, err := client.GetSupply(r.Context(), "ugnot"); err != nil {
			capacity.Error = "supply unavailable: " + err.Error()
		} else {
			capacity.SupplyUgnot, capacity.SupplyLocked, capacity.SupplyHeight = supply.Total, supply.Locked, supply.Height
			if c, ok := capacityBytes(supply.Total, price); ok {
				capacity.CapacityBytes = c
			} else {
				capacity.Error = "supply did not parse as an amount: " + supply.Total
			}
		}
	} else {
		capacity.Error = "no indexer configured for this network"
	}

	JSONResponse(w, map[string]any{
		"capacity": capacity,
		"cells":    cells,
		"payers":   payers,
		// How much of the total the returned rows account for. The map draws
		// the difference as an explicit remainder rather than letting a
		// truncated list understate how full the chain is.
		"cells_truncated": len(cells) < total.Realms,
	})
}

// capacityBytes divides the supply by the price in big.Int.
//
// int64 would in fact hold today's 1.3e13 bytes, but the inputs are the whole
// money supply of a chain and the divisor is governance-controlled: a price
// lowered by a proposal raises this figure by the same factor, and a capacity
// that silently wrapped would read as a chain that shrank.
func capacityBytes(supply string, price int64) (string, bool) {
	if price <= 0 {
		return "", false
	}
	total, ok := new(big.Int).SetString(strings.TrimSpace(supply), 10)
	if !ok {
		return "", false
	}
	return new(big.Int).Div(total, big.NewInt(price)).String(), true
}
