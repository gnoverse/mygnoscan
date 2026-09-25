package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// Account sessions: delegated signing keys, one account per grant.
//
// A session is a separate account at a derived path that signs on a master's
// behalf. The master stays the caller of every message it sends, which is the
// whole point and also why nothing else on an account page can reveal one:
// storage records the master, the signature records the key, and only the chain
// records the grant that links them.
//
// Read live, per request, from the one place the grant exists:
//
//	abci_query path="auth/accounts/<master>/sessions"
//
// There is no indexed alternative today. The tx-indexer does not define
// MsgCreateSession on any chain this serves (probed on mainnet and pearl,
// 2026-09-23, both `__type` null), so the typed fragments in pkg/indexer carry
// no data and the syncer has never seen a grant. The history that would make a
// reverse index possible is reachable through UnexpectedMessage.raw, which the
// selection set does not ask for yet.
//
// The consequence to know when reading this file: this answers "what has this
// account delegated", never "whose session is this address". The chain cannot
// answer the second one either: auth/accounts/<session_addr> returns null,
// because a session is not a plain account.

// Session is one live grant, flattened out of the chain's nested
// SessionAccount so the frontend does not have to know about
// BaseSessionAccount.BaseAccount.
type Session struct {
	Address string `json:"address"`
	Master  string `json:"master"`

	// AllowPaths is the grant itself: what this key may do, as
	// "<route>/<type>[:<path>]" entries. Prefix-matched by the chain, so an
	// entry naming a realm also covers its sub-realms.
	AllowPaths []string `json:"allow_paths,omitempty"`

	// SpendLimit and SpendUsed are coin strings ("5000000ugnot"), reported as
	// the chain spells them rather than parsed into a number: they are the
	// cap a reader checks, and a wrong denom assumption would silently
	// mis-scale it.
	SpendLimit string `json:"spend_limit,omitempty"`
	SpendUsed  string `json:"spend_used,omitempty"`

	// SpendPeriod is the rolling window in seconds. Zero is not "no limit":
	// it means the cap is for the session's whole lifetime, which is the
	// stricter of the two and must not read as the looser one.
	SpendPeriod int64 `json:"spend_period"`
	SpendReset  int64 `json:"spend_reset,omitempty"`

	// ExpiresAt is a unix timestamp. Zero means the grant never expires,
	// which the chain allows (`-expires-at none`).
	ExpiresAt int64 `json:"expires_at"`

	// Sequence is how many transactions the key has actually signed, and is
	// the only usage figure that does not depend on gas being spent.
	Sequence      int64 `json:"sequence"`
	AccountNumber int64 `json:"account_number,omitempty"`
}

// sessionAccount mirrors the chain's JSON for one entry of
// auth/accounts/<addr>/sessions. Numbers arrive as strings, the way amino's
// JSON encoding writes int64.
type sessionAccount struct {
	Base struct {
		Account struct {
			Address       string `json:"address"`
			AccountNumber string `json:"account_number"`
			Sequence      string `json:"sequence"`
		} `json:"BaseAccount"`
		MasterAddress string `json:"master_address"`
		ExpiresAt     string `json:"expires_at"`
		SpendLimit    string `json:"spend_limit"`
		SpendUsed     string `json:"spend_used"`
		SpendPeriod   string `json:"spend_period"`
		SpendReset    string `json:"spend_reset"`
	} `json:"BaseSessionAccount"`
	AllowPaths []string `json:"allow_paths"`
}

// HandleAddressSessions serves the sessions an address has granted.
//
// Kept off /api/address rather than folded into it: almost no account has a
// session, the read is a live RPC round trip, and every address page would pay
// for it. The frontend asks for this as an optional apiSWR path, so a chain
// without the feature costs one failed request and no missing page.
func (a *API) HandleAddressSessions(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	addr := r.PathValue("addr")

	// Sessions are chain state, so they exist per network and only when one is
	// selected. Merging them across chains would put two different grants under
	// one address, the same category error that keeps balance single-network.
	if network == "" {
		JSONResponse(w, map[string]any{"address": addr, "supported": false, "sessions": []Session{}})
		return
	}

	sessions, supported := fetchSessions(r.Context(), addr, a.rpcURLFor(network))
	JSONResponse(w, map[string]any{
		"address": addr,
		// Distinguishes "this chain cannot answer" from "this account has
		// granted nothing". They look identical in the payload otherwise, and
		// a page that says "no sessions" about a chain that has never heard of
		// them is stating a fact it does not have.
		"supported": supported,
		"sessions":  sessions,
	})
}

// fetchSessions reads and flattens auth/accounts/<addr>/sessions.
//
// The bool is whether the chain answered at all. Every failure collapses into
// false on purpose: an old chain replies std.UnknownRequestError ("unknown
// account sub-query: sessions"), an unreachable one replies nothing, and the
// page has the same single thing to say about both.
func fetchSessions(ctx context.Context, addr, rpcURL string) ([]Session, bool) {
	raw, err := fetchABCIQuery(ctx, rpcURL, "auth/accounts/"+addr+"/sessions", "")
	if err != nil {
		return []Session{}, false
	}

	// An account with no sessions answers the JSON literal `null`, which
	// unmarshals into a nil slice without error. That is a supported chain
	// saying "none", not a failure.
	var accounts []sessionAccount
	if err := json.Unmarshal([]byte(raw), &accounts); err != nil {
		return []Session{}, false
	}

	out := make([]Session, 0, len(accounts))
	for _, acc := range accounts {
		out = append(out, Session{
			Address:       acc.Base.Account.Address,
			Master:        acc.Base.MasterAddress,
			AllowPaths:    acc.AllowPaths,
			SpendLimit:    acc.Base.SpendLimit,
			SpendUsed:     acc.Base.SpendUsed,
			SpendPeriod:   atoi64(acc.Base.SpendPeriod),
			SpendReset:    atoi64(acc.Base.SpendReset),
			ExpiresAt:     atoi64(acc.Base.ExpiresAt),
			Sequence:      atoi64(acc.Base.Account.Sequence),
			AccountNumber: atoi64(acc.Base.Account.AccountNumber),
		})
	}

	// The chain returns them in whatever order its store iterates, which is not
	// stable between reads and makes a session appear to move around the table
	// between the cached paint and the fresh one. Soonest expiry first, because
	// that is the row a reader is looking for; a never-expiring grant sorts
	// last rather than first, which a zero would otherwise do.
	sort.SliceStable(out, func(i, j int) bool {
		ei, ej := out[i].ExpiresAt, out[j].ExpiresAt
		if ei == 0 || ej == 0 {
			return ej == 0 && ei != 0
		}
		if ei != ej {
			return ei < ej
		}
		return out[i].Address < out[j].Address
	})
	return out, true
}

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// Chain-wide sessions, from the index rather than from RPC.
//
// The per-account read above asks the chain and gets live state for one master.
// Neither half can do the other's job: RPC cannot enumerate every account, and
// the index cannot know a spend counter that moves without a transaction. So
// the pages use both, and each says which it is showing.

const (
	sessionsPageDefault = 100
	sessionsPageMax     = 500
	sessionRealmsTop    = 25
)

// HandleSessions serves the chain-wide picture: the counts, the realms
// delegated to, and the grant log.
func (a *API) HandleSessions(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > sessionsPageMax {
		limit = sessionsPageDefault
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}

	// Stated by the caller so the three counts partition the same instant.
	// Computing now() per row inside SQLite would let a grant expire between
	// two CASE arms and be counted twice or not at all.
	now := time.Now().Unix()

	stats, err := a.db.SessionStats(network, now)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	grants, total, err := a.db.SessionGrants(network, limit, offset)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	realms, err := a.db.SessionRealms(network, sessionRealmsTop)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	stats.Realms = len(realms)

	// How much of the chain this has actually looked at. A sessions page that
	// is really showing the last week of a two-year chain must say so: the
	// index fills forward from whenever the feature shipped and sweeps
	// backwards on a cursor, so "3 grants" can mean "3 so far".
	done, at, stop := a.db.SessionBackfillProgress(network)

	JSONResponse(w, map[string]any{
		"stats":   stats,
		"grants":  grants,
		"realms":  realms,
		"total":   total,
		"limit":   limit,
		"offset":  offset,
		"scanned": map[string]any{"complete": done, "at": at, "stop": stop},
	})
}

// HandleSessionIdentity answers "whose session is this address".
//
// The chain cannot: auth/accounts/<session_addr> returns null, because a
// session is not a plain account. Storage cannot either, directly, because a
// session-signed transaction records the master as its caller and the session
// address appears in no message. Only the grant transaction ties the two
// together, which is what session_grants keeps.
//
// Served separately from HandleAddressSessions because the two answer opposite
// questions about the same address, and an address can be both: a master that
// has granted keys, and itself a key granted by someone else.
func (a *API) HandleSessionIdentity(w http.ResponseWriter, r *http.Request) {
	addr := r.PathValue("addr")
	grants, err := a.db.SessionGrantsByAddress(a.networkParam(r), addr)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	// Also the grants this address has *made*, from the index rather than RPC,
	// so the history (revoked, expired) is there alongside the live read.
	granted, err := a.db.SessionGrantsByMaster(a.networkParam(r), addr)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	done, _, _ := a.db.SessionBackfillProgress(a.networkParam(r))
	JSONResponse(w, map[string]any{
		"address": addr,
		// is_session is the headline: this address signs for somebody else.
		"is_session": len(grants) > 0,
		"grants":     grants,
		"granted":    granted,
		// Without this a reader cannot tell "not a session" from "not indexed
		// yet", and the sweep can take a while on a long chain.
		"scan_complete": done,
	})
}
