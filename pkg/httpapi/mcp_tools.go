package httpapi

import (
	"fmt"
	"sort"
	"strconv"
)

// The tool table.
//
// One rule decided what is here: a tool exists where this explorer knows
// something a node will not tell you. Source is stored on chain and no node
// can grep it. A symbol's declaration site is derivable but nobody derives it
// per query. A transaction's decoded messages, events and storage deltas are
// ours. Everything else an agent can get with curl against an RPC, and wrapping
// it in a tool would only add a layer that can be stale.
//
// Each tool resolves to one internal GET against the route table, so a tool's
// answer is the REST endpoint's answer and cannot drift from it.

type mcpTool struct {
	Name        string
	Title       string
	Description string
	Schema      map[string]any
	// build validates arguments and returns the request to run. Errors here
	// come back to the agent as tool errors it can act on, not as protocol
	// failures its client swallows.
	build func(mcpArgs) (mcpTarget, error)
}

// Caps, applied server-side whatever the agent asks for. A tool's limit
// parameter is a request, not a promise.
const (
	mcpCodeHits    = 25
	mcpCodeHitsMax = 100
	mcpSymbolHits  = 25
	mcpSymbolMax   = 100
	mcpListRows    = 50
	mcpListRowsMax = 200
)

// networkProp is the argument every tool takes, described once.
func networkProp() map[string]any {
	return map[string]any{
		"type": "string",
		"description": "Chain to query, e.g. \"mainnet\". Omit to search every chain this " +
			"explorer indexes; each result then carries the network it came from.",
	}
}

func schema(required []string, props map[string]any) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	// Refuse unknown arguments rather than ignoring them: a model that
	// misremembers a parameter name should be told, not silently given the
	// unfiltered answer it will then present as filtered.
	s["additionalProperties"] = false
	return s
}

var mcpTools = map[string]mcpTool{
	"search_code": {
		Name:  "search_code",
		Title: "Search every source file on chain",
		Description: "Full-text search across every .gno file and README the chain stores. " +
			"This is the one thing no gno.land node can answer: source is on chain, but " +
			"there is no query that reads it, so the alternative is cloning tx-exports and " +
			"grepping. Supports phrase search (\"func Render\"), prefix (Iterate*) and " +
			"boolean operators. Returns a ranked list of files with a highlighted snippet, " +
			"not whole files: follow a hit with get_package.",
		Schema: schema([]string{"query"}, map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "FTS5 query. A bare word, a \"quoted phrase\", a prefix*, or AND/OR/NOT between them.",
			},
			"kind": map[string]any{
				"type":        "string",
				"enum":        []string{"realm", "package"},
				"description": "Narrow to realms (gno.land/r/) or pure packages (gno.land/p/). Omit for both.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("Hits to return, default %d, capped at %d.", mcpCodeHits, mcpCodeHitsMax),
			},
			"network": networkProp(),
		}),
		build: func(a mcpArgs) (mcpTarget, error) {
			q, err := a.required("query")
			if err != nil {
				return mcpTarget{}, err
			}
			kind, err := a.oneOf("kind", "", "realm", "package")
			if err != nil {
				return mcpTarget{}, err
			}
			t := target("/api/code/search")
			t.query.Set("q", q)
			if kind != "" {
				t.query.Set("kind", kind)
			}
			t.query.Set("limit", strconv.Itoa(a.intIn("limit", mcpCodeHits, mcpCodeHitsMax)))
			return t, nil
		},
	},

	"search_symbols": {
		Name:  "search_symbols",
		Title: "Find a declaration by name",
		Description: "Search the symbol index: which package declares a given function, type, " +
			"method or constant, with its signature, doc comment and the file and line it is " +
			"declared on. Answers \"has anyone already written this\" and \"where does this " +
			"name come from\", neither of which a node can answer. Use search_code instead to " +
			"find uses rather than declarations.",
		Schema: schema([]string{"query"}, map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "A symbol name or prefix, e.g. IterateByOffset, Render, Banker.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("Results to return, default %d, capped at %d.", mcpSymbolHits, mcpSymbolMax),
			},
			"network": networkProp(),
		}),
		build: func(a mcpArgs) (mcpTarget, error) {
			q, err := a.required("query")
			if err != nil {
				return mcpTarget{}, err
			}
			t := target("/api/symbols/search")
			t.query.Set("q", q)
			t.query.Set("limit", strconv.Itoa(a.intIn("limit", mcpSymbolHits, mcpSymbolMax)))
			return t, nil
		},
	},

	"get_package": {
		Name:  "get_package",
		Title: "Read a package: source, metadata and symbols",
		Description: "Everything about one deployed package or realm in a single call: every " +
			"source file, who deployed it and in which transaction and block, its imports and " +
			"importers, its exported functions and full symbol table, and the addresses the " +
			"chain derives for it. Reading this from a node means one vm/qfile per file with " +
			"no file list to start from.",
		Schema: schema([]string{"path"}, map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Package path, with or without the gno.land/ prefix, e.g. gno.land/r/gnoland/home.",
			},
			"network": networkProp(),
		}),
		build: func(a mcpArgs) (mcpTarget, error) {
			raw, err := a.required("path")
			if err != nil {
				return mcpTarget{}, err
			}
			p, err := pkgPath(raw)
			if err != nil {
				return mcpTarget{}, err
			}
			return target("/api/realm/" + escapePathValue(p)), nil
		},
	},

	"get_realm_state": {
		Name:  "get_realm_state",
		Title: "Read a realm's live state, decoded",
		Description: "What a realm currently holds, as a named tree of decoded values with its " +
			"object graph resolved. A node answers this as raw Amino JSON with unresolved " +
			"object references, which is unreadable without following each one by hand. " +
			"Read live from a node, so unlike every other tool here it is the chain's tip " +
			"rather than the index. Large realms come back partial and say so; ask again with " +
			"full=true for a wider read.",
		Schema: schema([]string{"path"}, map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Realm path, with or without the gno.land/ prefix, e.g. gno.land/r/gnoland/home.",
			},
			"full": map[string]any{
				"type": "boolean",
				"description": "Spend a longer, wider read. Use only after a partial answer: " +
					"this runs against a public node nobody is funding.",
			},
			"network": networkProp(),
		}),
		build: func(a mcpArgs) (mcpTarget, error) {
			raw, err := a.required("path")
			if err != nil {
				return mcpTarget{}, err
			}
			p, err := pkgPath(raw)
			if err != nil {
				return mcpTarget{}, err
			}
			t := target("/api/state/" + escapePathValue(p))
			if a.boolean("full") {
				t.query.Set("full", "1")
			}
			return t, nil
		},
	},

	"find_realms": {
		Name:  "find_realms",
		Title: "List packages, faceted and ranked",
		Description: "Browse what is deployed: filter by kind (realm or pure package) and by " +
			"namespace, and rank by what the chain has done with each one, calls received, " +
			"unique users, gas burned, storage held, importers, or symbols declared. The " +
			"chain has no such listing at all: it can tell you a path exists if you already " +
			"know the path.",
		Schema: schema(nil, map[string]any{
			"kind": map[string]any{
				"type":        "string",
				"enum":        []string{"all", "realm", "pure"},
				"description": "realm is gno.land/r/, pure is gno.land/p/. Default all.",
			},
			"namespace": map[string]any{
				"type":        "string",
				"description": "The element after r/ or p/, e.g. \"gnoland\" for gno.land/r/gnoland/home.",
			},
			"sort": map[string]any{
				"type": "string",
				"enum": []string{"newest", "oldest", "name", "calls", "users", "gas",
					"storage", "importers", "imports", "symbols", "last_call"},
				"description": "Ranking. Default newest. Use symbols to find libraries worth reading: " +
					"a pure package burns no gas and receives no calls, so every other ranking puts all of p/ at zero.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("Rows to return, default %d, capped at %d.", mcpListRows, mcpListRowsMax),
			},
			"offset":  map[string]any{"type": "integer", "description": "Rows to skip, for paging."},
			"network": networkProp(),
		}),
		build: func(a mcpArgs) (mcpTarget, error) {
			kind, err := a.oneOf("kind", "all", "all", "realm", "pure")
			if err != nil {
				return mcpTarget{}, err
			}
			sortBy, err := a.oneOf("sort", "", "newest", "oldest", "name", "calls", "users",
				"gas", "storage", "importers", "imports", "symbols", "last_call")
			if err != nil {
				return mcpTarget{}, err
			}
			t := target("/api/packages")
			// Always explicit: /api/packages with neither kind nor namespace
			// means pure packages only, which is not what "find_realms with no
			// arguments" should return.
			t.query.Set("kind", kind)
			if ns := a.str("namespace"); ns != "" {
				t.query.Set("namespace", ns)
			}
			if sortBy != "" {
				t.query.Set("sort", sortBy)
			}
			t.query.Set("limit", strconv.Itoa(a.intIn("limit", mcpListRows, mcpListRowsMax)))
			if off := a.intIn("offset", 0, 100000); off > 0 {
				t.query.Set("offset", strconv.Itoa(off))
			}
			return t, nil
		},
	},

	"explain_tx": {
		Name:  "explain_tx",
		Title: "Decode a transaction",
		Description: "One transaction, decoded: its messages with arguments, the events it " +
			"emitted, gas wanted against gas used, what it cost, the storage it locked or " +
			"released, and the block and time it landed in. A node serves the raw Amino " +
			"encoding of the same thing, and a public RPC usually runs with no transaction " +
			"index at all, so it cannot find a transaction by hash.",
		Schema: schema([]string{"hash"}, map[string]any{
			"hash": map[string]any{
				"type":        "string",
				"description": "Transaction hash as the explorer and the indexer print it.",
			},
			"network": networkProp(),
		}),
		build: func(a mcpArgs) (mcpTarget, error) {
			h, err := a.required("hash")
			if err != nil {
				return mcpTarget{}, err
			}
			return target("/api/tx/" + escapeSegment(h)), nil
		},
	},

	"get_address": {
		Name:  "get_address",
		Title: "Everything on chain about one address",
		Description: "An account's history and holdings in one call: balance, the realms it " +
			"deployed, the calls it made, transfers in and out, GRC20 positions, and the " +
			"name behind it when there is one. A node can only answer the current balance, " +
			"and only for an address you already know about. Works for a realm's derived " +
			"address as well as a user's.",
		Schema: schema([]string{"address"}, map[string]any{
			"address": map[string]any{
				"type":        "string",
				"description": "A bech32 gno address, g1...",
			},
			"network": networkProp(),
		}),
		build: func(a mcpArgs) (mcpTarget, error) {
			addr, err := a.required("address")
			if err != nil {
				return mcpTarget{}, err
			}
			return target("/api/address/" + escapeSegment(addr)), nil
		},
	},
}

// mcpToolList renders the table for tools/list, in a stable order.
//
// Sorted because a map's iteration order is random and the list goes into a
// model's context: an identical server that describes itself differently on
// every connection is one that cannot be cached and is tedious to diff.
func mcpToolList() []map[string]any {
	names := make([]string, 0, len(mcpTools))
	for n := range mcpTools {
		names = append(names, n)
	}
	sort.Strings(names)

	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		t := mcpTools[n]
		out = append(out, map[string]any{
			"name":        t.Name,
			"title":       t.Title,
			"description": t.Description,
			"inputSchema": t.Schema,
			// Read-only and non-destructive, declared rather than implied, so
			// a client that gates writes behind approval knows it does not
			// need to gate these.
			"annotations": map[string]any{
				"title":           t.Title,
				"readOnlyHint":    true,
				"destructiveHint": false,
				"idempotentHint":  true,
				"openWorldHint":   true,
			},
		})
	}
	return out
}

// MCPToolCount is how many tools the endpoint serves, for the startup log.
func MCPToolCount() int { return len(mcpTools) }
