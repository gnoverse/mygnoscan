package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mainnetSessionsJSON is one real response from
// abci_query path="auth/accounts/g1manfred…/sessions" on gnoland-1, captured
// 2026-09-23 and trimmed to three of its six entries. Kept verbatim rather
// than hand-written so the nesting (BaseSessionAccount.BaseAccount), the
// string-encoded int64s and the fields the chain *omits* are the real ones:
// spend_period is absent on a lifetime cap, and spend_used is absent on a key
// that has never spent.
const mainnetSessionsJSON = `[
  {
    "@type": "/gno.SessionAccount",
    "BaseSessionAccount": {
      "BaseAccount": {
        "address": "g1u2k64tpesfxpmyxulrafxvlynz5cd38l02n86r",
        "coins": "",
        "account_number": "3262847",
        "sequence": "0"
      },
      "master_address": "g1manfred47kzduec920z88wfr64ylksmdcedlf5",
      "expires_at": "1797427613",
      "spend_limit": "100000000ugnot",
      "spend_reset": "1789651451"
    },
    "allow_paths": ["vm/exec:gno.land/r/g1ecsuj0q572jr0dhu29q9njtnmw03hyu7tyyvv6/kourt"]
  },
  {
    "@type": "/gno.SessionAccount",
    "BaseSessionAccount": {
      "BaseAccount": {
        "address": "g1caccz22wv8uyxnjfgtr30c879ksgxx4dscne8d",
        "coins": "",
        "account_number": "3263113",
        "sequence": "1"
      },
      "master_address": "g1manfred47kzduec920z88wfr64ylksmdcedlf5",
      "expires_at": "1805641155",
      "spend_limit": "100000000ugnot",
      "spend_period": "2592000",
      "spend_used": "356972ugnot",
      "spend_reset": "1790089155"
    },
    "allow_paths": ["vm/exec:gno.land/r/moul/faucet"]
  },
  {
    "@type": "/gno.SessionAccount",
    "BaseSessionAccount": {
      "BaseAccount": {
        "address": "g1rrtqvv2kcffw0nezkecxmxyqa6u9wy06e03fck",
        "coins": "",
        "account_number": "3263223",
        "sequence": "1"
      },
      "master_address": "g1manfred47kzduec920z88wfr64ylksmdcedlf5",
      "expires_at": "1792772718",
      "spend_limit": "5000000ugnot",
      "spend_used": "9350ugnot",
      "spend_reset": "1790180735"
    },
    "allow_paths": ["vm/exec:gno.land/r/moul/x/reaper"]
  }
]`

// fakeSessionNode answers auth/accounts/<addr>/sessions the way a node does:
// a base64 Data blob on success, an Error object on a chain that does not know
// the sub-query, and the JSON literal `null` for an account with no grants.
type fakeSessionNode struct {
	// data maps an address to the raw JSON the chain would return for it.
	data map[string]string
	// abciErr makes every query answer with std.UnknownRequestError, which is
	// what a chain predating the session feature replies.
	abciErr bool
	// gotPaths records the query paths asked for, so a test can assert the
	// handler builds the path itself rather than trusting a caller's.
	gotPaths *[]string
}

func (f fakeSessionNode) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Params struct{ Path string } `json:"params"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decode abci request: %v", err)
		}
		if f.gotPaths != nil {
			*f.gotPaths = append(*f.gotPaths, req.Params.Path)
		}
		base := map[string]any{}
		switch {
		case f.abciErr:
			base["Error"] = map[string]any{"@type": "/std.UnknownRequestError"}
		default:
			addr := strings.TrimSuffix(strings.TrimPrefix(req.Params.Path, "auth/accounts/"), "/sessions")
			raw, ok := f.data[addr]
			if !ok {
				raw = "null"
			}
			base["Data"] = base64.StdEncoding.EncodeToString([]byte(raw))
		}
		json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{"response": map[string]any{"ResponseBase": base}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchSessions(t *testing.T) {
	const master = "g1manfred47kzduec920z88wfr64ylksmdcedlf5"

	tests := []struct {
		name string
		node fakeSessionNode
		addr string
		// noRPC drops the endpoint entirely, the "all networks selected" and
		// "no verified RPC" cases.
		noRPC         bool
		wantSupported bool
		wantCount     int
	}{
		{
			name:          "a master's grants come back flattened",
			node:          fakeSessionNode{data: map[string]string{master: mainnetSessionsJSON}},
			addr:          master,
			wantSupported: true,
			wantCount:     3,
		},
		{
			name: "an account with no grants is supported and empty",
			node: fakeSessionNode{data: map[string]string{master: mainnetSessionsJSON}},
			// `null` is what the chain answers here, and it must not read as a
			// decode failure: the difference is "none" versus "cannot say".
			addr:          "g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5",
			wantSupported: true,
			wantCount:     0,
		},
		{
			name:          "a chain without the sub-query is unsupported",
			node:          fakeSessionNode{abciErr: true},
			addr:          master,
			wantSupported: false,
			wantCount:     0,
		},
		{
			name:          "no RPC endpoint is unsupported, not an empty account",
			noRPC:         true,
			addr:          master,
			wantSupported: false,
			wantCount:     0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rpc := ""
			if !tt.noRPC {
				rpc = tt.node.server(t).URL
			}
			got, supported := fetchSessions(context.Background(), tt.addr, rpc)
			if supported != tt.wantSupported {
				t.Errorf("supported = %v, want %v", supported, tt.wantSupported)
			}
			if len(got) != tt.wantCount {
				t.Fatalf("got %d sessions, want %d", len(got), tt.wantCount)
			}
			// Never nil: the frontend iterates this, and a JSON null there
			// would be a second empty case to handle for no reason.
			if got == nil {
				t.Error("sessions is nil, want an empty slice")
			}
		})
	}
}

func TestFetchSessionsFields(t *testing.T) {
	const master = "g1manfred47kzduec920z88wfr64ylksmdcedlf5"
	var paths []string
	srv := fakeSessionNode{data: map[string]string{master: mainnetSessionsJSON}, gotPaths: &paths}.server(t)

	got, supported := fetchSessions(context.Background(), master, srv.URL)
	if !supported || len(got) != 3 {
		t.Fatalf("fetchSessions = %d sessions, supported %v", len(got), supported)
	}
	if len(paths) != 1 || paths[0] != "auth/accounts/"+master+"/sessions" {
		t.Errorf("query paths = %v, want one auth/accounts/<addr>/sessions", paths)
	}

	// Soonest expiry first, so the row a reader is checking is at the top and
	// the order does not change between the cached and the fresh paint.
	wantOrder := []string{
		"g1rrtqvv2kcffw0nezkecxmxyqa6u9wy06e03fck", // 1792772718
		"g1u2k64tpesfxpmyxulrafxvlynz5cd38l02n86r", // 1797427613
		"g1caccz22wv8uyxnjfgtr30c879ksgxx4dscne8d", // 1805641155
	}
	for i, want := range wantOrder {
		if got[i].Address != want {
			t.Errorf("session %d = %s, want %s", i, got[i].Address, want)
		}
	}

	reaper := got[0]
	if reaper.Master != master {
		t.Errorf("master = %q, want %q", reaper.Master, master)
	}
	if len(reaper.AllowPaths) != 1 || reaper.AllowPaths[0] != "vm/exec:gno.land/r/moul/x/reaper" {
		t.Errorf("allow_paths = %v", reaper.AllowPaths)
	}
	if reaper.SpendLimit != "5000000ugnot" || reaper.SpendUsed != "9350ugnot" {
		t.Errorf("spend = %q of %q", reaper.SpendUsed, reaper.SpendLimit)
	}
	// Absent spend_period is a lifetime cap, the stricter reading. A zero that
	// meant "unlimited" would be the dangerous way to get this wrong.
	if reaper.SpendPeriod != 0 {
		t.Errorf("spend_period = %d, want 0 for a lifetime cap", reaper.SpendPeriod)
	}
	if reaper.ExpiresAt != 1792772718 {
		t.Errorf("expires_at = %d", reaper.ExpiresAt)
	}
	if reaper.Sequence != 1 || reaper.AccountNumber != 3263223 {
		t.Errorf("sequence = %d, account_number = %d", reaper.Sequence, reaper.AccountNumber)
	}

	// A key that has never spent reports no spend_used at all, and the rolling
	// window is carried through as seconds.
	kourt := got[1]
	if kourt.SpendUsed != "" {
		t.Errorf("spend_used = %q, want empty for an unused key", kourt.SpendUsed)
	}
	faucet := got[2]
	if faucet.SpendPeriod != 2592000 {
		t.Errorf("spend_period = %d, want 2592000", faucet.SpendPeriod)
	}
}
