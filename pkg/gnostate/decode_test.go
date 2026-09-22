package gnostate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture is a captured realm: the qpkg_json body plus every qobject_json and
// qtype_json body needed to resolve it. Captured from mainnet so the tests
// exercise the shapes the chain actually emits, and replayed from disk so they
// never touch the network.
type fixture struct {
	Path    string            `json:"path"`
	Package string            `json:"package"`
	Objects map[string]string `json:"objects"`
	Types   map[string]string `json:"types"`
}

func (f *fixture) Object(oid string) ([]byte, error) { return []byte(f.Objects[oid]), nil }
func (f *fixture) Type(tid string) ([]byte, error)   { return []byte(f.Types[tid]), nil }

func load(t *testing.T, name string) *fixture {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var f fixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return &f
}

// find walks the tree for the first node at the given dotted path, where a
// path element matches a node Name.
func find(nodes []Node, path string) *Node {
	parts := strings.Split(path, ".")
	cur := nodes
	for i, p := range parts {
		var hit *Node
		for j := range cur {
			if cur[j].Name == p {
				hit = &cur[j]
				break
			}
		}
		if hit == nil {
			return nil
		}
		if i == len(parts)-1 {
			return hit
		}
		cur = hit.Children
	}
	return nil
}

func TestDecodePackage(t *testing.T) {
	tests := []struct {
		name     string
		file     string
		wantVar  string
		wantKey  string // a dotted path that must resolve
		wantVal  string // and the value it must carry, "" to only require presence
		wantKind Kind
	}{
		{
			// The motivating case: on the wire this is a PointerValue to a
			// RefValue to a HeapItemValue to a StructValue whose single field
			// holds the string. A reader must see the string.
			name: "error string resolves through four layers of indirection",
			file: "blog.json", wantVar: "errNotCommenter",
			wantKey: "errNotCommenter.s", wantVal: "access restricted: not commenter",
			wantKind: KindPrimitive,
		},
		{
			name: "address-typed string renders as its value",
			file: "home.json", wantVar: "Admin",
			wantKey: "Admin", wantVal: "g1lnkytfqcjwllws63gvf0mv9yt04aswy4y9amhm",
			wantKind: KindPrimitive,
		},
		{
			// N-encoded numbers: 8 little-endian bytes, base64. Getting the
			// endianness wrong yields a plausible-looking wrong number, which
			// is why this is pinned to an exact value.
			name: "untyped string constant",
			file: "home.json", wantVar: "LayoutSlug",
			wantKey: "LayoutSlug", wantVal: "layout",
			wantKind: KindPrimitive,
		},
		{
			name: "bigint constant",
			file: "home.json", wantVar: "maxSlugLen",
			wantKey: "maxSlugLen", wantVal: "64",
			wantKind: KindPrimitive,
		},
		{
			name: "declared type held as a value",
			file: "home.json", wantVar: "Theme",
			wantKey: "Theme", wantVal: "Theme",
			wantKind: KindType,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := load(t, tt.file)
			tree, err := DecodePackage([]byte(f.Package), f, Limits{})
			if err != nil {
				t.Fatalf("DecodePackage: %v", err)
			}
			if find(tree.Nodes, tt.wantVar) == nil {
				t.Fatalf("variable %q missing from tree", tt.wantVar)
			}
			n := find(tree.Nodes, tt.wantKey)
			if n == nil {
				t.Fatalf("path %q did not resolve; tree was:\n%s", tt.wantKey, dump(tree.Nodes, 0))
			}
			if n.Kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", n.Kind, tt.wantKind)
			}
			if tt.wantVal != "" && n.Value != tt.wantVal {
				t.Errorf("value = %q, want %q", n.Value, tt.wantVal)
			}
		})
	}
}

// TestDecodePackageNumbers pins the N decoding against a value read off
// mainnet. `rev` is an int and `lastHeight` an int64, both carried as 8
// little-endian bytes; a big-endian read of lastHeight would give
// 14200705646462468096 rather than a block height, and would look like a
// deliberate uint64 to anyone reviewing the page.
func TestDecodePackageNumbers(t *testing.T) {
	f := load(t, "home.json")
	tree, err := DecodePackage([]byte(f.Package), f, Limits{})
	if err != nil {
		t.Fatalf("DecodePackage: %v", err)
	}
	for _, tc := range []struct{ name, typ string }{
		{"rev", "int"},
		{"lastHeight", "int64"},
	} {
		n := find(tree.Nodes, tc.name)
		if n == nil {
			t.Fatalf("%s missing", tc.name)
		}
		if n.Kind != KindPrimitive {
			t.Errorf("%s kind = %q, want primitive", tc.name, n.Kind)
		}
		if n.Type != tc.typ {
			t.Errorf("%s type = %q, want %q", tc.name, n.Type, tc.typ)
		}
		// Both are small non-negative counters on mainnet. A byte-order or
		// sign error puts them outside this range immediately.
		var v int64
		if _, err := fmt.Sscanf(n.Value, "%d", &v); err != nil {
			t.Fatalf("%s value %q is not a number", tc.name, n.Value)
		}
		if v <= 0 || v > 1<<40 {
			t.Errorf("%s = %d, outside the plausible range: byte order or sign is wrong", tc.name, v)
		}
	}
}

// TestDecodeNoResolver is the degraded mode: with the chain unreachable the
// tree must still decode, one level deep, with every stored object rendered as
// a followable ref rather than as an error.
func TestDecodeNoResolver(t *testing.T) {
	f := load(t, "blog.json")
	tree, err := DecodePackage([]byte(f.Package), nil, Limits{})
	if err != nil {
		t.Fatalf("DecodePackage: %v", err)
	}
	if len(tree.Nodes) == 0 {
		t.Fatal("no nodes decoded without a resolver")
	}
	if tree.Stats.Fetches != 0 {
		t.Errorf("fetches = %d, want 0 with a nil resolver", tree.Stats.Fetches)
	}
	var refs int
	for _, n := range tree.Nodes {
		if n.Kind == KindRef {
			refs++
			if n.ObjectID == "" {
				t.Errorf("ref node %q carries no ObjectID, so the UI cannot offer to open it", n.Name)
			}
		}
	}
	if refs == 0 {
		t.Error("expected unresolved refs with a nil resolver")
	}
}

// cyclicResolver serves two objects that point at each other. A realm author
// can build exactly this, so the walk must terminate on its own rather than
// relying on the depth limit to save it.
type cyclicResolver struct{ calls int }

func (c *cyclicResolver) Object(oid string) ([]byte, error) {
	c.calls++
	if c.calls > 1000 {
		return nil, fmt.Errorf("runaway: the cycle guard did not hold")
	}
	other := "aa:2"
	if oid == "aa:2" {
		other = "aa:1"
	}
	return []byte(fmt.Sprintf(`{"objectid":%q,"value":{"@type":"/gno.StructValue",
	  "ObjectInfo":{"ID":%q,"LastObjectSize":"10"},
	  "Fields":[{"T":{"@type":"/gno.RefType","ID":"x.Loop"},
	             "V":{"@type":"/gno.RefValue","ObjectID":%q}}]}}`, oid, oid, other)), nil
}
func (c *cyclicResolver) Type(string) ([]byte, error) { return nil, nil }

func TestDecodeCycleTerminates(t *testing.T) {
	pkg := `{"names":["loop"],"values":[
	  {"T":{"@type":"/gno.RefType","ID":"x.Loop"},
	   "V":{"@type":"/gno.RefValue","ObjectID":"aa:1"}}]}`
	r := &cyclicResolver{}
	tree, err := DecodePackage([]byte(pkg), r, Limits{})
	if err != nil {
		t.Fatalf("DecodePackage: %v", err)
	}
	var cycles int
	var walk func([]Node)
	walk = func(ns []Node) {
		for _, n := range ns {
			if n.Kind == KindCycle {
				cycles++
			}
			walk(n.Children)
		}
	}
	walk(tree.Nodes)
	if cycles == 0 {
		t.Error("a self-referential object graph produced no cycle marker")
	}
	// The guard, not the depth cap, is what must stop this: with MaxDepth 12 a
	// two-object loop would otherwise emit a dozen identical levels first.
	if tree.Stats.Fetches > 4 {
		t.Errorf("fetches = %d; the cycle guard should stop after the second object", tree.Stats.Fetches)
	}
}

func TestDecodeLimits(t *testing.T) {
	f := load(t, "blog.json")
	tests := []struct {
		name string
		lim  Limits
	}{
		{"node cap", Limits{MaxNodes: 5}},
		{"depth cap", Limits{MaxDepth: 1}},
		{"fetch cap", Limits{MaxFetch: 1}},
		{"string cap", Limits{MaxString: 8}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tree, err := DecodePackage([]byte(f.Package), f, tt.lim)
			if err != nil {
				t.Fatalf("DecodePackage: %v", err)
			}
			if !tree.Stats.Truncated {
				t.Error("Stats.Truncated is false, so the page would present a clipped tree as complete")
			}
			if tt.lim.MaxNodes > 0 && tree.Stats.Nodes > tt.lim.MaxNodes+len(tree.Nodes) {
				t.Errorf("nodes = %d, well past the cap of %d", tree.Stats.Nodes, tt.lim.MaxNodes)
			}
			if tt.lim.MaxFetch > 0 && tree.Stats.Fetches > tt.lim.MaxFetch {
				t.Errorf("fetches = %d, past the cap of %d", tree.Stats.Fetches, tt.lim.MaxFetch)
			}
		})
	}
}

// TestDecodeDropsFunctions keeps the state view from becoming a second symbol
// table: a realm's functions are its API, and the docs tab already owns them.
func TestDecodeDropsFunctions(t *testing.T) {
	f := load(t, "blog.json")
	tree, err := DecodePackage([]byte(f.Package), f, Limits{})
	if err != nil {
		t.Fatalf("DecodePackage: %v", err)
	}
	for _, n := range tree.Nodes {
		if n.Kind == KindFunc {
			t.Errorf("function %q reached the state tree", n.Name)
		}
	}
	// Render is declared by the blog realm, so if it survived the filter this
	// test would be passing for the wrong reason.
	if find(tree.Nodes, "Render") != nil {
		t.Error("Render is in the state tree; it is API, not state")
	}
}

func TestDecodeMalformed(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"not json", `not json at all`},
		{"names without values", `{"names":["a","b"],"values":[]}`},
		{"null value", `{"names":["a"],"values":[{"T":null,"V":null}]}`},
		{"unknown discriminator", `{"names":["a"],"values":[{"T":null,"V":{"@type":"/gno.MartianValue"}}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The contract is "never panic": a malformed payload either errors
			// or decodes to something honest. The explorer reads whatever a
			// chain hands it, including one it does not control.
			tree, err := DecodePackage([]byte(tt.raw), nil, Limits{})
			if err != nil {
				return
			}
			if tree == nil {
				t.Fatal("nil tree with nil error")
			}
		})
	}
}

// TestDecodeObject covers the lazy-expansion path the State tab uses when a
// reader opens a collapsed branch.
func TestDecodeObject(t *testing.T) {
	f := load(t, "blog.json")
	var oid string
	var body string
	for k, v := range f.Objects {
		if strings.Contains(v, "/gno.StructValue") {
			oid, body = k, v
			break
		}
	}
	if oid == "" {
		t.Skip("no struct object in fixture")
	}
	n, err := DecodeObject([]byte(body), f, Limits{})
	if err != nil {
		t.Fatalf("DecodeObject: %v", err)
	}
	if n.ObjectID == "" {
		t.Error("decoded object carries no ObjectID")
	}
}

func dump(ns []Node, d int) string {
	var b strings.Builder
	for _, n := range ns {
		fmt.Fprintf(&b, "%s%s (%s) %q\n", strings.Repeat("  ", d), n.Name, n.Kind, n.Value)
		b.WriteString(dump(n.Children, d+1))
	}
	return b.String()
}
