package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/moul/mygnoscan/pkg/config"
)

// stateNode stands in for a chain node. answers maps a query payload (a package
// path, an ObjectID or a TypeID) to the body the node returns; a missing key is
// answered the way a real node answers an unknown path, with an abci error, so
// the "not a live package" branch is exercised against the real shape rather
// than against a convenient one.
func stateNode(t *testing.T, answers map[string]string, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Params struct {
				Path string `json:"path"`
				Data string `json:"data"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		raw, _ := base64.StdEncoding.DecodeString(req.Params.Data)
		if calls != nil {
			calls.Add(1)
		}
		answer, ok := answers[string(raw)]
		if !ok {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result": map[string]any{"response": map[string]any{
					"Log": "package not found",
					"ResponseBase": map[string]any{
						"Error": map[string]any{"@type": "/vm.InvalidPkgPathError"},
					},
				}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{"response": map[string]any{
				"ResponseBase": map[string]any{"Data": base64.StdEncoding.EncodeToString([]byte(answer))},
			}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func stateReq(t *testing.T, api *API, path, query string) (*httptest.ResponseRecorder, StateResponse) {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/state/"+path+query, nil)
	r.SetPathValue("path", path)
	w := httptest.NewRecorder()
	api.HandleState(w, r)
	var got StateResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	return w, got
}

// A realm holding one string, reachable through the same heap-item indirection
// the chain really uses.
const stateOneVarPkg = `{"names":["greeting"],"values":[
  {"T":{"@type":"/gno.PrimitiveType","value":"16"},
   "V":{"@type":"/gno.StringValue","value":"hello"}}]}`

func TestHandleState(t *testing.T) {
	tests := []struct {
		name    string
		answers map[string]string
		path    string
		status  int
		check   func(*testing.T, StateResponse)
	}{
		{
			name:    "a realm's variables come back named and decoded",
			answers: map[string]string{"gno.land/r/x/y": stateOneVarPkg},
			path:    "r/x/y",
			status:  200,
			check: func(t *testing.T, got StateResponse) {
				if len(got.Nodes) != 1 || got.Nodes[0].Name != "greeting" || got.Nodes[0].Value != "hello" {
					t.Errorf("nodes = %+v, want one `greeting` = hello", got.Nodes)
				}
				if got.Partial {
					t.Error("a fully resolved realm reported itself partial")
				}
			},
		},
		{
			// The message a reader sees for a path the chain does not know has
			// to be about the chain, not about our decoder. The raw abci error
			// surfaces as `map[@type:/vm.InvalidPkgPathError] ()`, which reads
			// like a bug in the explorer.
			name:    "an unknown path is a plain 404, not a raw abci error",
			answers: map[string]string{},
			path:    "r/does/not/exist",
			status:  404,
			check:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api, _ := newTestAPI(t)
			srv := stateNode(t, tt.answers, nil)
			api.networks = []config.NetworkConfig{{ID: "alpha", RPCURL: srv.URL}}
			api.setRPCVerified("alpha", srv.URL)

			w, got := stateReq(t, api, tt.path, "?network=alpha")
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tt.status, w.Body.String())
			}
			if tt.status == 404 {
				if strings.Contains(w.Body.String(), "InvalidPkgPath") {
					t.Errorf("the raw abci error reached the reader: %s", w.Body.String())
				}
				return
			}
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

// An unverified RPC must not be read. Same reasoning as the balance sweeper:
// another chain's state rendered under this chain's realm is wrong in a way
// nobody can see.
func TestHandleStateRefusesUnverifiedRPC(t *testing.T) {
	api, _ := newTestAPI(t)
	srv := stateNode(t, map[string]string{"gno.land/r/x/y": stateOneVarPkg}, nil)
	api.networks = []config.NetworkConfig{{ID: "alpha", RPCURL: srv.URL}}
	// deliberately not setRPCVerified

	w, _ := stateReq(t, api, "r/x/y", "?network=alpha")
	if w.Code != 503 {
		t.Errorf("status = %d, want 503 for an unverified endpoint", w.Code)
	}
}

// The state view is the app's most attacker-facing surface: a realm's author
// chooses its variable names and values. They must survive as data, not as
// markup, all the way through the JSON.
func TestHandleStateKeepsHostileValuesAsData(t *testing.T) {
	hostile := `<img src=x onerror=alert(1)>`
	pkg := fmt.Sprintf(`{"names":["evil"],"values":[
	  {"T":{"@type":"/gno.PrimitiveType","value":"16"},
	   "V":{"@type":"/gno.StringValue","value":%q}}]}`, hostile)

	api, _ := newTestAPI(t)
	srv := stateNode(t, map[string]string{"gno.land/r/x/y": pkg}, nil)
	api.networks = []config.NetworkConfig{{ID: "alpha", RPCURL: srv.URL}}
	api.setRPCVerified("alpha", srv.URL)

	w, got := stateReq(t, api, "r/x/y", "?network=alpha")
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if len(got.Nodes) != 1 || got.Nodes[0].Value != hostile {
		t.Fatalf("value = %+v, want it preserved verbatim", got.Nodes)
	}
	// Preserved verbatim in the value, and escaped on the wire: the raw
	// `<img` must not appear unescaped in the JSON body.
	if strings.Contains(w.Body.String(), "<img") {
		t.Errorf("hostile markup is unescaped in the response body: %s", w.Body.String())
	}
}

// ?full=1 buys a wider read, and the flag that offers it must not be set when
// there is nothing wider left to try. Otherwise the UI shows a "load the full
// state" link that reloads the same partial answer.
func TestHandleStateDoesNotOfferFullTwice(t *testing.T) {
	api, _ := newTestAPI(t)
	srv := stateNode(t, map[string]string{"gno.land/r/x/y": stateOneVarPkg}, nil)
	api.networks = []config.NetworkConfig{{ID: "alpha", RPCURL: srv.URL}}
	api.setRPCVerified("alpha", srv.URL)

	_, got := stateReq(t, api, "r/x/y", "?network=alpha&full=1")
	if got.CanRetryFull {
		t.Error("a ?full=1 response offered a full retry, which would loop the reader")
	}
}
