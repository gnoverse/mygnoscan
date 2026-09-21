package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/moul/mygnoscan/pkg/config"
	"github.com/moul/mygnoscan/pkg/store"
)

// balanceNode answers bank/balances for a fixed set of addresses, and /status
// so the sweeper can stamp a height.
func balanceNode(t *testing.T, balances map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status" {
			io.WriteString(w, `{"result":{"node_info":{"network":"alpha-1"},"sync_info":{"latest_block_height":"4242"}}}`)
			return
		}
		// fetchBalance asks with the address inside the query path.
		path := r.URL.Query().Get("path")
		var amount string
		for addr, a := range balances {
			if len(path) >= len(addr) && path[len(path)-len(addr)-1:len(path)-1] == addr {
				amount = a
				break
			}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{"response": map[string]any{
				"ResponseBase": map[string]any{"Data": base64.StdEncoding.EncodeToString([]byte(amount))},
			}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSweepBalancesFillsTheCache(t *testing.T) {
	api, db := newTestAPI(t)
	now := time.Now().UTC()
	store.MustCall(t, db, "alpha", "TX1", 1, now, "g1rich", "gno.land/r/x/y", "Fn")
	store.MustCall(t, db, "alpha", "TX2", 2, now, "g1poor", "gno.land/r/x/y", "Fn")

	srv := balanceNode(t, map[string]string{"g1rich": "900ugnot", "g1poor": "5ugnot"})
	api.networks = []config.NetworkConfig{{ID: "alpha", RPCURL: srv.URL}}
	// The sweeper only asks verified endpoints: an unverified RPC might serve
	// another chain's balances, and a wrong number about money is invisible.
	api.setRPCVerified("alpha", srv.URL)

	api.SweepBalances(t.Context())

	cached, err := db.BalancesFor("alpha", []string{"g1rich", "g1poor"})
	if err != nil {
		t.Fatal(err)
	}
	if cached["g1rich"].Ugnot != 900 || cached["g1poor"].Ugnot != 5 {
		t.Errorf("cache = %+v, want both balances", cached)
	}
	// The height dates the figure. Without it a balance is a number with no
	// claim attached.
	if cached["g1rich"].Height != 4242 {
		t.Errorf("height = %d, want the tip at sweep time", cached["g1rich"].Height)
	}
}

// An unverified RPC is skipped entirely rather than read anyway. Withholding a
// balance costs a blank; trusting a mismatched one costs a figure from another
// chain that nobody can see is wrong.
func TestSweepBalancesSkipsUnverifiedNetworks(t *testing.T) {
	api, db := newTestAPI(t)
	store.MustCall(t, db, "alpha", "TX1", 1, time.Now().UTC(), "g1rich", "gno.land/r/x/y", "Fn")

	srv := balanceNode(t, map[string]string{"g1rich": "900ugnot"})
	api.networks = []config.NetworkConfig{{ID: "alpha", RPCURL: srv.URL}}

	api.SweepBalances(t.Context())

	cov, err := db.BalanceCoverage("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if cov.Swept != 0 {
		t.Errorf("swept = %d, want nothing read from an unverified endpoint", cov.Swept)
	}
}

func TestHandleRichList(t *testing.T) {
	api, db := newTestAPI(t)
	if err := db.UpsertBalances("alpha", []store.BalanceRow{
		{Address: "g1top", Amount: "900ugnot", Height: 10},
		{Address: "g1mid", Amount: "500ugnot", Height: 10},
	}); err != nil {
		t.Fatal(err)
	}

	var resp richListResponse
	getJSON(t, api.HandleRichList, "/api/accounts/rich?network=alpha", &resp)

	if len(resp.Entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(resp.Entries))
	}
	if resp.Entries[0].Address != "g1top" || resp.Entries[0].Rank != 1 {
		t.Errorf("first = %+v, want g1top at rank 1", resp.Entries[0])
	}
	// Coverage always travels with the ranking, because the ranking is over
	// what has been swept and the page has to say so.
	if resp.Coverage.Swept != 2 {
		t.Errorf("coverage = %+v, want 2 swept", resp.Coverage)
	}
}

// Ranks continue across pages: a second page numbered from 1 again would read
// as a second set of top holders.
func TestHandleRichListRanksAcrossPages(t *testing.T) {
	api, db := newTestAPI(t)
	if err := db.UpsertBalances("alpha", []store.BalanceRow{
		{Address: "g1a", Amount: "900ugnot"},
		{Address: "g1b", Amount: "500ugnot"},
		{Address: "g1c", Amount: "100ugnot"},
	}); err != nil {
		t.Fatal(err)
	}

	var resp richListResponse
	getJSON(t, api.HandleRichList, "/api/accounts/rich?network=alpha&limit=1&offset=3", &resp)

	if len(resp.Entries) != 1 || resp.Entries[0].Rank != 3 || resp.Entries[0].Address != "g1c" {
		t.Errorf("third page = %+v, want g1c at rank 3", resp.Entries)
	}
}

// The rich list never blends networks, so an absent network resolves to one and
// the response names it.
func TestHandleRichListNamesItsNetwork(t *testing.T) {
	api, db := newTestAPI(t)
	if err := db.UpsertBalances("alpha", []store.BalanceRow{{Address: "g1a", Amount: "1ugnot"}}); err != nil {
		t.Fatal(err)
	}
	var resp richListResponse
	getJSON(t, api.HandleRichList, "/api/accounts/rich", &resp)
	if resp.Network != "alpha" {
		t.Errorf("network = %q, want the resolved network named", resp.Network)
	}
}

// /api/accounts reads the cache instead of fanning out one RPC call per row.
func TestAccountsReadBalancesFromTheCache(t *testing.T) {
	api, db := newTestAPI(t)
	store.MustCall(t, db, "alpha", "TX1", 1, time.Now().UTC(), "g1rich", "gno.land/r/x/y", "Fn")
	if err := db.UpsertBalances("alpha", []store.BalanceRow{{Address: "g1rich", Amount: "900ugnot"}}); err != nil {
		t.Fatal(err)
	}
	// No RPC configured at all: if the read path still called out, this would
	// come back blank.
	var accounts []store.AccountInfo
	getJSON(t, api.HandleAccounts, "/api/accounts?network=alpha", &accounts)

	var found bool
	for _, a := range accounts {
		if a.Address == "g1rich" {
			found = true
			if a.Balance != "900ugnot" {
				t.Errorf("balance = %q, want the cached figure", a.Balance)
			}
		}
	}
	if !found {
		t.Fatalf("g1rich missing from %+v", accounts)
	}
}

func TestHandleAccountPopulation(t *testing.T) {
	api, db := newTestAPI(t)
	now := time.Now().UTC()
	store.MustCall(t, db, "alpha", "TX1", 1, now, "g1one", "gno.land/r/x/y", "Fn")
	store.MustCall(t, db, "alpha", "TX2", 2, now, "g1two", "gno.land/r/x/y", "Fn")

	var pop store.AccountPopulation
	getJSON(t, api.HandleAccountPopulation, "/api/accounts/population?network=alpha", &pop)

	if pop.Known < 2 || pop.Daily < 2 {
		t.Errorf("population = %+v, want both addresses counted", pop)
	}
}
