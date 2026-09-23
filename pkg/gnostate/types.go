// Package gnostate decodes the Amino JSON that gno.land's VM queries return
// into a named value tree a reader can follow.
//
// The chain answers `vm/qpkg_json` with a realm's package block exactly as the
// VM stores it: parallel `names` and `values` arrays, where a single string
// constant arrives as a PointerValue to a RefValue to a HeapItemValue to a
// StructValue whose one field holds the string. That representation is correct
// and unreadable, which is the whole reason this package exists.
//
// Two deliberate constraints shape it:
//
//   - **No gnovm dependency.** The `@type` discriminators in the payload carry
//     everything needed to walk it structurally, so this package reads gno's
//     JSON without importing gno. mygnoscan ships as one binary with one
//     non-stdlib direct dependency, and importing the GnoVM to decode its own
//     wire format would trade that for a type switch we can write here.
//
//   - **Every walk is bounded.** A realm is attacker-controlled data: its
//     author chooses the variable names, the nesting depth and whether the
//     object graph has a cycle. Limits are not a tuning knob, they are what
//     makes decoding someone else's state safe to do on a request path.
package gnostate

// Kind classifies a node into the shape a renderer needs, which is coarser
// than the VM's own type system on purpose: a reader cares whether a row is a
// value, a container, or a link to follow, not whether the VM called it a
// DeclaredType over a StructType.
type Kind string

const (
	KindPrimitive Kind = "primitive" // rendered inline: string, number, bool
	KindStruct    Kind = "struct"
	KindSlice     Kind = "slice"
	KindArray     Kind = "array"
	KindMap       Kind = "map"
	KindFunc      Kind = "func"
	KindType      Kind = "type" // a type declaration held as a value
	KindPackage   Kind = "package"
	KindNil       Kind = "nil"
	KindRef       Kind = "ref"       // a stored object we chose not to follow
	KindCycle     Kind = "cycle"     // already on the path above this node
	KindTruncated Kind = "truncated" // a limit stopped the walk here
)

// Node is one row in the decoded tree.
//
// Name is relative to the parent (`Title`, `[0]`, `posts`); a renderer joins
// the chain itself rather than every node carrying a redundant full path.
type Node struct {
	Name     string `json:"name,omitempty"`
	Type     string `json:"type,omitempty"` // the gno-level type: "string", "avl.Tree", "*Post"
	Kind     Kind   `json:"kind"`
	Value    string `json:"value,omitempty"` // leaves only
	Children []Node `json:"children,omitempty"`

	// ObjectID survives ref unwrapping so the object graph and the storage
	// view can still link a node the walker flattened for readability.
	ObjectID string `json:"object_id,omitempty"`
	OwnerID  string `json:"owner_id,omitempty"`
	Hash     string `json:"hash,omitempty"`
	// TypeID is the fully-qualified type this node was declared with
	// (`gno.land/p/nt/avl/v0.Tree`), where Type is the shortened display name
	// (`Tree`). It is here because `vm/qobject_json` returns a stored object
	// with no type attached: expanding a collapsed branch correctly needs the
	// type its parent knew, and without this the caller has nowhere to get it.
	TypeID string `json:"type_id,omitempty"`
	// Size is the object's LastObjectSize as the chain reports it, which is
	// what the storage deposit was charged against.
	Size int64 `json:"size,omitempty"`

	// Refs is the object's RefCount: how many stored references point at it.
	// 1 for an ordinary field, higher for something genuinely shared.
	Refs int64 `json:"refs,omitempty"`

	// Rev is the object's ModTime, and the name is deliberate: ModTime is not
	// a time. It is the owning realm's own logical counter at the moment the
	// object was last written (gnovm: `oo.SetIsDirty(true, rlm.Time)`), so it
	// orders changes within one realm and means nothing across realms, and it
	// cannot be converted to a block height, a date or an age.
	//
	// What it does give, honestly, is "which parts of this realm changed most
	// recently". Calling it a timestamp anywhere in the UI would be inventing
	// a fact the chain does not have.
	Rev int64 `json:"rev,omitempty"`

	// File and Line locate a stored function's declaration in the realm's own
	// source. A FuncValue carries its Source.Location on the wire, so a
	// function held in state can be linked precisely to the line that declares
	// it rather than rendered as an opaque "func".
	File string `json:"file,omitempty"`
	Line int    `json:"line,omitempty"`

	// fromHeapItem records that this node is the contents of a heap item the
	// walker unwrapped. It is unexported, so it never reaches the API; it
	// exists only so a PointerValue can tell "index 0 of a heap item", where
	// the unwrap already selected the slot, from "index 0 of a struct", where
	// it still has to select field 0.
	fromHeapItem bool
}

// Limits bound a single decode. Zero means "use the default", so a caller can
// set one field without silently disabling the rest.
type Limits struct {
	MaxDepth  int // nesting levels below a top-level variable
	MaxNodes  int // total nodes emitted across the whole tree
	MaxFetch  int // resolver calls: each one is an RPC round trip or a cache hit
	MaxString int // bytes of any single rendered leaf value
}

// DefaultLimits are sized for one realm rendered into one HTTP response.
//
// MaxFetch is the one that matters for latency: an unresolved ref renders as a
// followable link, so stopping early degrades the page rather than breaking
// it. That is why the walk prefers breadth over depth when it runs out.
var DefaultLimits = Limits{MaxDepth: 12, MaxNodes: 4000, MaxFetch: 512, MaxString: 4096}

func (l Limits) withDefaults() Limits {
	if l.MaxDepth <= 0 {
		l.MaxDepth = DefaultLimits.MaxDepth
	}
	if l.MaxNodes <= 0 {
		l.MaxNodes = DefaultLimits.MaxNodes
	}
	if l.MaxFetch <= 0 {
		l.MaxFetch = DefaultLimits.MaxFetch
	}
	if l.MaxString <= 0 {
		l.MaxString = DefaultLimits.MaxString
	}
	return l
}

// Resolver fetches the raw Amino JSON the walker needs to follow a reference.
//
// Both methods may return (nil, nil) for "not available": a missing object or
// an unknown type is an expected outcome (a stale ref, a stdlib type we did
// not crawl), and it must degrade to an unexpanded node rather than fail the
// whole decode. Only a transport error deserves a non-nil error.
type Resolver interface {
	// Object returns the body of `vm/qobject_json` for an ObjectID.
	Object(oid string) ([]byte, error)
	// Type returns the body of `vm/qtype_json` for a TypeID. It is what turns
	// a struct's positional fields into named ones, so a nil Resolver.Type
	// costs readability but never correctness.
	Type(tid string) ([]byte, error)
}

// Stats reports what a decode actually cost, so a handler can surface "this
// tree is incomplete" instead of presenting a truncated walk as the whole
// state.
//
// Truncated and Clipped are separate because they mean different things to a
// reader. Truncated says the walk did not reach every value, so the page is
// showing less than the realm holds. Clipped says every value is here and one
// of them was too long to print in full, which is not a gap. Measured on
// mainnet's r/gnoland/blog, where a post body runs past the default 4 KB cap:
// conflating the two would badge a fully loaded realm as incomplete.
type Stats struct {
	Nodes     int  `json:"nodes"`
	Fetches   int  `json:"fetches"`
	Truncated bool `json:"truncated"`
	Clipped   bool `json:"clipped,omitempty"`
}

// Tree is a decoded package block: one node per named top-level variable.
type Tree struct {
	Nodes []Node `json:"nodes"`
	Stats Stats  `json:"stats"`
}
