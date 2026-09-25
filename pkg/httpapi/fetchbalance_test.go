package httpapi

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// abciNode answers one canned abci_query body, so each case below can be the
// exact shape a real node produces rather than a paraphrase of it.
func abciNode(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// The bug this covers: an account that holds nothing and an account that could
// not be read both came back as "", so the defi tab announced that the chain
// was unreadable for every realm whose money is all in GRC20.
func TestFetchBalanceSeparatesEmptyFromUnreadable(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    string
		wantErr bool
	}{
		{
			name: "a funded account",
			body: `{"result":{"response":{"ResponseBase":{"Error":null,"Data":"` + b64(`"754954090ugnot"`) + `"}}}}`,
			want: "754954090ugnot",
		},
		{
			// g1em9s40nfrwd2aqn9ypjv7d9x9z9c8uk5uxrza9 on mainnet: the node
			// answers, Error is None, Data is empty. This is a zero balance.
			name: "an account that holds nothing",
			body: `{"result":{"response":{"ResponseBase":{"Error":null,"Data":""}}}}`,
			want: "",
		},
		{
			// A failed query ALSO has empty Data, which is why Error has to be
			// read: without it this case would render as "0 GNOT", which is the
			// same lie in the other direction.
			name:    "a query the node refused",
			body:    `{"result":{"response":{"ResponseBase":{"Error":{"@type":"/std.UnknownRequestError"},"Data":""}}}}`,
			wantErr: true,
		},
		{
			name:    "a body that is not the shape we expect",
			body:    `not json at all`,
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := fetchBalanceErr(t.Context(), "g1whatever", abciNode(t, c.body))
			if c.wantErr {
				if err == nil {
					t.Fatalf("want an error, got balance %q and nil", got)
				}
				if got != "" {
					t.Fatalf("a failed read must carry no balance, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("want no error, got %v", err)
			}
			if got != c.want {
				t.Fatalf("want %q, got %q", c.want, got)
			}
		})
	}
}

// A network with no RPC configured is a read that did not happen, not an empty
// account. The e2e harness runs in exactly this state, which is why its
// assertions expect "unknown".
func TestFetchBalanceWithoutAnEndpointIsAnError(t *testing.T) {
	got, err := fetchBalanceErr(t.Context(), "g1whatever", "")
	if err == nil {
		t.Fatalf("want an error for an unconfigured endpoint, got %q", got)
	}
	if !strings.Contains(err.Error(), "no rpc endpoint") {
		t.Fatalf("the error should say what is missing, got %v", err)
	}
}

// The string-only wrapper still exists for callers with nowhere to put a
// failure, and it must keep collapsing both cases to "" so they are unchanged.
func TestFetchBalanceStringFormIsUnchanged(t *testing.T) {
	ok := abciNode(t, `{"result":{"response":{"ResponseBase":{"Error":null,"Data":"`+b64(`"5ugnot"`)+`"}}}}`)
	if got := fetchBalance(t.Context(), "g1whatever", ok); got != "5ugnot" {
		t.Fatalf("want 5ugnot, got %q", got)
	}
	bad := abciNode(t, `{"result":{"response":{"ResponseBase":{"Error":{"@type":"/std.UnknownRequestError"},"Data":""}}}}`)
	if got := fetchBalance(t.Context(), "g1whatever", bad); got != "" {
		t.Fatalf("a failed read must still collapse to empty here, got %q", got)
	}
}
