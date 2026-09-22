package gnostate

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Amino type discriminators. Written out rather than matched by suffix because
// `/gno.heapItemType` is lower-cased where every other one is not, and a
// suffix match would also happily accept a discriminator gno never emits.
const (
	tPrimitive = "/gno.PrimitiveType"
	tRef       = "/gno.RefType"
	tPointer   = "/gno.PointerType"
	tSlice     = "/gno.SliceType"
	tArray     = "/gno.ArrayType"
	tMap       = "/gno.MapType"
	tStruct    = "/gno.StructType"
	tDeclared  = "/gno.DeclaredType"
	tFunc      = "/gno.FuncType"
	tInterface = "/gno.InterfaceType"
	tType      = "/gno.TypeType"
	tPackage   = "/gno.PackageType"
	tHeapItem  = "/gno.heapItemType"

	vString   = "/gno.StringValue"
	vBigint   = "/gno.BigintValue"
	vBigdec   = "/gno.BigdecValue"
	vPointer  = "/gno.PointerValue"
	vRef      = "/gno.RefValue"
	vHeapItem = "/gno.HeapItemValue"
	vStruct   = "/gno.StructValue"
	vSlice    = "/gno.SliceValue"
	vArray    = "/gno.ArrayValue"
	vMap      = "/gno.MapValue"
	vFunc     = "/gno.FuncValue"
	vType     = "/gno.TypeValue"
	vBound    = "/gno.BoundMethodValue"
	vPackage  = "/gno.PackageValue"
	vBlock    = "/gno.Block"
	vRefNode  = "/gno.RefNode"
)

// primitiveNames maps gno's PrimitiveType bit flags to their source spelling.
//
// The constants are `1 << iota` (gnovm/pkg/gnolang/types.go), so these are
// powers of two and not an ordinal: 16 is StringType, not the 16th type. That
// is exactly the kind of off-by-one that reads as plausible in a UI forever,
// so the values are spelled out rather than computed from an index.
var primitiveNames = map[int64]string{
	1: "invalid", 2: "untyped bool", 4: "bool", 8: "untyped string",
	16: "string", 32: "int", 64: "int8", 128: "int16", 256: "untyped rune",
	512: "int32", 1024: "int64", 2048: "uint", 4096: "uint8", 8192: "databyte",
	16384: "uint16", 32768: "uint32", 65536: "uint64", 131072: "float32",
	262144: "float64", 524288: "untyped bigint", 1048576: "untyped bigdec",
}

// signedPrimitives are the primitive codes whose N payload is two's complement.
// Everything else numeric is read unsigned, so a large uint64 does not render
// as a negative number.
var signedPrimitives = map[int64]bool{
	32: true, 64: true, 128: true, 256: true, 512: true, 1024: true, 524288: true,
}

// typedValue is gno's TypedValue on the wire. N carries primitive numbers as
// 8 little-endian bytes, base64-encoded; V carries everything else.
type typedValue struct {
	T json.RawMessage `json:"T"`
	V json.RawMessage `json:"V"`
	N string          `json:"N"`
}

type objectInfo struct {
	ID             string `json:"ID"`
	Hash           string `json:"Hash"`
	OwnerID        string `json:"OwnerID"`
	ModTime        string `json:"ModTime"`
	RefCount       string `json:"RefCount"`
	LastObjectSize string `json:"LastObjectSize"`
}

type pkgBlock struct {
	Names  []string     `json:"names"`
	Values []typedValue `json:"values"`
}

// walker carries the per-decode budget and the cycle set. One walker is used
// for exactly one decode and is not safe for concurrent use.
type walker struct {
	res   Resolver
	lim   Limits
	stats Stats
	// onPath is the set of ObjectIDs between the root and the current node.
	// Keyed on ObjectID rather than on the JSON body because two distinct
	// objects can serialize identically (two empty structs), and treating
	// those as a cycle would hide real data.
	onPath map[string]bool
	// types caches decoded type declarations for the life of the decode. A
	// realm with 500 posts asks for the same struct type 500 times.
	types map[string]*typeInfo
}

// typeInfo is the part of a type declaration the walker needs: what to call
// it, what to ask the chain for, and what its fields are named.
//
// name and id are deliberately separate. name is shortened for display
// (`Post`, not `gno.land/p/gnoland/blog/v0.Post`) because a full type ID in a
// table column pushes the value off the page; id is what `vm/qtype_json`
// answers to. Using one for the other silently loses every field name, which
// is the difference between `Title` and `[0]`.
type typeInfo struct {
	name   string
	id     string
	fields []string
}

// DecodePackage decodes the body of `vm/qpkg_json` into one node per named
// top-level variable.
//
// res may be nil, in which case every stored object renders as KindRef and the
// tree is one level deep. That is the honest degraded mode and it is what the
// caller gets when the chain is unreachable, so it is worth keeping working.
func DecodePackage(raw []byte, res Resolver, lim Limits) (*Tree, error) {
	var blk pkgBlock
	if err := json.Unmarshal(raw, &blk); err != nil {
		return nil, fmt.Errorf("parse package block: %w", err)
	}
	w := newWalker(res, lim)
	out := make([]Node, 0, len(blk.Names))
	for i, name := range blk.Names {
		if i >= len(blk.Values) {
			// names and values are parallel arrays built by the same loop in
			// the VM, so a length mismatch means the payload is not what we
			// think it is. Stop rather than pair a name with a neighbour's
			// value, which would be confidently wrong.
			break
		}
		if name == "" || name == "_" {
			continue
		}
		n := w.value(name, blk.Values[i], 0)
		// Functions and types are the package's API, not its state, and
		// `vm/qfuncs` plus the docs tab already answer for them. Dropping them
		// here is what makes the difference between a state view and a second
		// symbol table.
		if n.Kind == KindFunc {
			continue
		}
		out = append(out, n)
	}
	// Flatten after the whole block is decoded, not during: a tree's entries
	// can only be collected once its subtree has been walked, and doing it as
	// a pass keeps the walker free of any knowledge of avl.
	return &Tree{Nodes: flattenTrees(out), Stats: w.stats}, nil
}

// objectEnvelope is what `vm/qobject_json` wraps its value in.
type objectEnvelope struct {
	ObjectID string          `json:"objectid"`
	Value    json.RawMessage `json:"value"`
}

// DecodeObject decodes the body of `vm/qobject_json` into a single node, for
// expanding one branch without re-reading the whole realm.
func DecodeObject(raw []byte, res Resolver, lim Limits) (*Node, error) {
	var env objectEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse object envelope: %w", err)
	}
	if len(env.Value) == 0 {
		return nil, fmt.Errorf("object %q has no value", env.ObjectID)
	}
	w := newWalker(res, lim)
	n := w.rawValue("", nil, env.Value, 0)
	if n.ObjectID == "" {
		n.ObjectID = env.ObjectID
	}
	return &n, nil
}

func newWalker(res Resolver, lim Limits) *walker {
	return &walker{
		res:    res,
		lim:    lim.withDefaults(),
		onPath: map[string]bool{},
		types:  map[string]*typeInfo{},
	}
}

// budget reports whether another node may be emitted, and records the
// truncation the first time the answer is no.
func (w *walker) budget(depth int) bool {
	if w.stats.Nodes >= w.lim.MaxNodes || depth > w.lim.MaxDepth {
		w.stats.Truncated = true
		return false
	}
	return true
}

// value decodes one TypedValue: the T side names the type, the N or V side
// carries the data.
func (w *walker) value(name string, tv typedValue, depth int) Node {
	if !w.budget(depth) {
		return Node{Name: name, Kind: KindTruncated}
	}
	w.stats.Nodes++

	ti := w.typeOf(tv.T)

	// A primitive number arrives in N, with T naming which one. V is absent.
	if tv.N != "" {
		return Node{Name: name, Type: tname(ti), Kind: KindPrimitive, Value: w.number(tv.T, tv.N)}
	}
	if len(tv.V) == 0 || string(tv.V) == "null" {
		// A primitive with neither N nor V is its zero value, not nil: amino
		// omits the payload for `false`, `0` and `""`. Measured on mainnet,
		// where r/gnoland/blog's `inPause` arrives as a bare
		// {"T":{"@type":"/gno.PrimitiveType","value":"4"}}. Rendering that as
		// nil would report a paused-or-not flag as unset on every realm that
		// left it false, which is most of them.
		if code := primitiveCode(tv.T); code != 0 {
			return Node{Name: name, Type: tname(ti), Kind: KindPrimitive, Value: zeroValue(code)}
		}
		// A nil V with a pointer or interface type is a real nil. With no type
		// at all it is an uninitialised slot, which reads the same way.
		return Node{Name: name, Type: tname(ti), Kind: KindNil, Value: "nil"}
	}
	return w.rawValue(name, ti, tv.V, depth)
}

// rawValue decodes the V side of a TypedValue, or a bare value from an object
// envelope. ti is what the T side said, which is usually a better name for the
// value than anything the value itself carries.
func (w *walker) rawValue(name string, ti *typeInfo, raw json.RawMessage, depth int) Node {
	var head struct {
		Type string `json:"@type"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return Node{Name: name, Type: tname(ti), Kind: KindTruncated, Value: "undecodable"}
	}

	switch head.Type {
	case vString:
		var v struct {
			Value string `json:"value"`
		}
		_ = json.Unmarshal(raw, &v)
		return Node{Name: name, Type: tname(ti), Kind: KindPrimitive, Value: w.clip(v.Value)}

	case vBigint, vBigdec:
		var v struct {
			Value string `json:"value"`
		}
		_ = json.Unmarshal(raw, &v)
		return Node{Name: name, Type: tname(ti), Kind: KindPrimitive, Value: w.clip(v.Value)}

	case vHeapItem:
		// A heap item is a storage cell, not something the realm's author
		// wrote. Unwrap to the value inside, keeping the cell's identity so
		// the graph can still draw it.
		var v struct {
			ObjectInfo objectInfo `json:"ObjectInfo"`
			Value      typedValue `json:"Value"`
		}
		_ = json.Unmarshal(raw, &v)
		inner := w.value(name, v.Value, depth)
		stamp(&inner, v.ObjectInfo)
		inner.fromHeapItem = true
		return inner

	case vPointer:
		return w.pointer(name, ti, raw, depth)

	case vRef:
		return w.ref(name, ti, raw, depth)

	case vStruct:
		return w.structValue(name, ti, raw, depth)

	case vSlice:
		return w.slice(name, ti, raw, depth)

	case vArray:
		return w.array(name, ti, raw, depth)

	case vMap:
		return w.mapValue(name, ti, raw, depth)

	case vType:
		var v struct {
			Type json.RawMessage `json:"Type"`
		}
		_ = json.Unmarshal(raw, &v)
		declared := w.typeOf(v.Type)
		n := Node{Name: name, Kind: KindType, Type: "type"}
		if declared != nil {
			n.Value = declared.name
		}
		return n

	case vFunc, vBound:
		return Node{Name: name, Type: tname(ti), Kind: KindFunc, Value: tname(ti)}

	case vPackage:
		return Node{Name: name, Type: tname(ti), Kind: KindPackage}

	case vBlock, vRefNode:
		// Execution scaffolding: a closure's captured block, or a lazy
		// reference to an AST node. Neither is realm state, and rendering
		// either one is how gnoweb's explorer fills a page with machinery.
		return Node{Name: name, Type: tname(ti), Kind: KindFunc}
	}

	return Node{Name: name, Type: tname(ti), Kind: KindTruncated, Value: head.Type}
}

// pointer follows a PointerValue to the value it addresses.
//
// TV is set when the pointee travelled inline; otherwise Base names the object
// holding it and Index selects within that object.
func (w *walker) pointer(name string, ti *typeInfo, raw json.RawMessage, depth int) Node {
	var v struct {
		TV    *typedValue     `json:"TV"`
		Base  json.RawMessage `json:"Base"`
		Index string          `json:"Index"`
	}
	_ = json.Unmarshal(raw, &v)

	if v.TV != nil {
		return w.value(name, *v.TV, depth)
	}
	if len(v.Base) == 0 || string(v.Base) == "null" {
		return Node{Name: name, Type: tname(ti), Kind: KindNil, Value: "nil"}
	}

	base := w.rawValue(name, ti, v.Base, depth)

	// Index is a position into Base, which gnovm documents as "array/struct/
	// block, or heapitem" (gnovm/pkg/gnolang/values.go, PointerValue). A heap
	// item holds one slot and the unwrap above already selected it, so
	// indexing again would descend a level too far. For a struct or an array
	// the index is still unspent and selects the field or element.
	//
	// Negative indices are the two sentinels gnovm defines,
	// PointerIndexBlockBlank (-1) and PointerIndexMap (-2); neither names a
	// child here, so both fall through to the base.
	if base.fromHeapItem {
		return base
	}
	if idx, err := strconv.Atoi(v.Index); err == nil && idx >= 0 && idx < len(base.Children) {
		sel := base.Children[idx]
		sel.Name = name
		return sel
	}
	return base
}

// ref follows a RefValue by asking the resolver for the stored object.
//
// Every failure path here degrades to KindRef with the ObjectID intact, so the
// UI can offer "open this object" rather than showing a dead row.
func (w *walker) ref(name string, ti *typeInfo, raw json.RawMessage, depth int) Node {
	var v struct {
		ObjectID string `json:"ObjectID"`
		Hash     string `json:"Hash"`
		PkgPath  string `json:"PkgPath"`
	}
	_ = json.Unmarshal(raw, &v)

	unresolved := Node{Name: name, Type: tname(ti), Kind: KindRef, ObjectID: v.ObjectID, Hash: v.Hash}
	if v.PkgPath != "" {
		// A ref to a package, not to an object: there is no object to fetch.
		return Node{Name: name, Type: tname(ti), Kind: KindPackage, Value: v.PkgPath}
	}
	if v.ObjectID == "" || w.res == nil {
		return unresolved
	}
	if w.onPath[v.ObjectID] {
		return Node{Name: name, Type: tname(ti), Kind: KindCycle, ObjectID: v.ObjectID}
	}
	if w.stats.Fetches >= w.lim.MaxFetch || !w.budget(depth) {
		w.stats.Truncated = true
		return unresolved
	}

	w.stats.Fetches++
	body, err := w.res.Object(v.ObjectID)
	if err != nil || len(body) == 0 {
		return unresolved
	}
	var env objectEnvelope
	if json.Unmarshal(body, &env) != nil || len(env.Value) == 0 {
		return unresolved
	}

	w.onPath[v.ObjectID] = true
	n := w.rawValue(name, ti, env.Value, depth)
	delete(w.onPath, v.ObjectID)

	if n.ObjectID == "" {
		n.ObjectID = v.ObjectID
	}
	return n
}

func (w *walker) structValue(name string, ti *typeInfo, raw json.RawMessage, depth int) Node {
	var v struct {
		ObjectInfo objectInfo   `json:"ObjectInfo"`
		Fields     []typedValue `json:"Fields"`
	}
	_ = json.Unmarshal(raw, &v)

	n := Node{Name: name, Type: tname(ti), Kind: KindStruct}
	stamp(&n, v.ObjectInfo)

	// Field names come from the type declaration, fetched once per type. With
	// no resolver the fields are still all there, addressed positionally.
	names := w.fieldNames(ti, len(v.Fields))
	for i, f := range v.Fields {
		fname := fmt.Sprintf("[%d]", i)
		if i < len(names) && names[i] != "" {
			fname = names[i]
		}
		n.Children = append(n.Children, w.value(fname, f, depth+1))
	}
	return n
}

func (w *walker) slice(name string, ti *typeInfo, raw json.RawMessage, depth int) Node {
	var v struct {
		Base   json.RawMessage `json:"Base"`
		Offset string          `json:"Offset"`
		Length string          `json:"Length"`
	}
	_ = json.Unmarshal(raw, &v)

	n := Node{Name: name, Type: tname(ti), Kind: KindSlice}
	if len(v.Base) == 0 || string(v.Base) == "null" {
		n.Kind = KindNil
		n.Value = "nil"
		return n
	}
	base := w.rawValue(name, ti, v.Base, depth)
	n.ObjectID = base.ObjectID
	n.Hash = base.Hash
	n.Size = base.Size

	// A slice is a window onto its backing array. Showing the whole array
	// would report elements the slice cannot reach, which is a different
	// value from the one the realm holds.
	off, _ := strconv.Atoi(v.Offset)
	length, _ := strconv.Atoi(v.Length)
	if off < 0 {
		off = 0
	}
	for i := off; i < off+length && i < len(base.Children); i++ {
		child := base.Children[i]
		child.Name = fmt.Sprintf("[%d]", i-off)
		n.Children = append(n.Children, child)
	}
	n.Value = fmt.Sprintf("len %d", length)
	return n
}

func (w *walker) array(name string, ti *typeInfo, raw json.RawMessage, depth int) Node {
	var v struct {
		ObjectInfo objectInfo   `json:"ObjectInfo"`
		List       []typedValue `json:"List"`
		Data       string       `json:"Data"`
	}
	_ = json.Unmarshal(raw, &v)

	n := Node{Name: name, Type: tname(ti), Kind: KindArray}
	stamp(&n, v.ObjectInfo)

	// A []byte travels as base64 Data rather than as a List of elements.
	// Rendering it as 4,000 numbered rows is technically faithful and useless.
	if v.Data != "" {
		b, err := base64.StdEncoding.DecodeString(v.Data)
		if err != nil {
			n.Value = "undecodable bytes"
			return n
		}
		n.Value = fmt.Sprintf("%d bytes", len(b))
		if s, ok := printableASCII(b); ok {
			n.Value = w.clip(s)
		}
		return n
	}
	for i, e := range v.List {
		n.Children = append(n.Children, w.value(fmt.Sprintf("[%d]", i), e, depth+1))
	}
	n.Value = fmt.Sprintf("len %d", len(v.List))
	return n
}

func (w *walker) mapValue(name string, ti *typeInfo, raw json.RawMessage, depth int) Node {
	var v struct {
		ObjectInfo objectInfo `json:"ObjectInfo"`
		List       []struct {
			Key   typedValue `json:"Key"`
			Value typedValue `json:"Value"`
		} `json:"List"`
	}
	_ = json.Unmarshal(raw, &v)

	n := Node{Name: name, Type: tname(ti), Kind: KindMap}
	stamp(&n, v.ObjectInfo)
	for i, e := range v.List {
		k := w.value("", e.Key, depth+1)
		key := k.Value
		if key == "" {
			key = fmt.Sprintf("[%d]", i)
		}
		n.Children = append(n.Children, w.value(key, e.Value, depth+1))
	}
	n.Value = fmt.Sprintf("len %d", len(v.List))
	return n
}

// number renders the N payload: 8 little-endian bytes, base64-encoded, whose
// interpretation depends on the primitive type in T.
func (w *walker) number(t json.RawMessage, n string) string {
	b, err := base64.StdEncoding.DecodeString(n)
	if err != nil || len(b) < 8 {
		return n
	}
	u := binary.LittleEndian.Uint64(b)

	code := primitiveCode(t)
	switch code {
	case 4, 2: // bool, untyped bool
		if u != 0 {
			return "true"
		}
		return "false"
	case 131072: // float32
		return strconv.FormatFloat(float64(math.Float32frombits(uint32(u))), 'g', -1, 32)
	case 262144: // float64
		return strconv.FormatFloat(math.Float64frombits(u), 'g', -1, 64)
	}
	if signedPrimitives[code] {
		return strconv.FormatInt(int64(u), 10)
	}
	return strconv.FormatUint(u, 10)
}

// typeOf names a type from its JSON, following DeclaredType to its name and
// composites to their element types.
func (w *walker) typeOf(raw json.RawMessage) *typeInfo {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var head struct {
		Type    string          `json:"@type"`
		ID      string          `json:"ID"`
		Name    string          `json:"Name"`
		PkgPath string          `json:"PkgPath"`
		Elt     json.RawMessage `json:"Elt"`
		Base    json.RawMessage `json:"Base"`
		Key     json.RawMessage `json:"Key"`
		Value   json.RawMessage `json:"value"`
		Len     string          `json:"Len"`
		Fields  []struct {
			Name string `json:"Name"`
		} `json:"Fields"`
	}
	if json.Unmarshal(raw, &head) != nil {
		return nil
	}

	switch head.Type {
	case tPrimitive:
		var v struct {
			Value string `json:"value"`
		}
		_ = json.Unmarshal(raw, &v)
		code, _ := strconv.ParseInt(v.Value, 10, 64)
		if nm, ok := primitiveNames[code]; ok {
			return &typeInfo{name: nm}
		}
		return &typeInfo{name: "primitive(" + v.Value + ")"}

	case tRef:
		// A RefType is a bare type ID: the declaration it names lives
		// elsewhere and is fetched only if something asks for field names.
		return &typeInfo{name: shortTypeID(head.ID), id: head.ID}

	case tDeclared:
		// A DeclaredType carries its Base inline, so its field names are
		// already here and cost no fetch. This is the shape qtype_json
		// answers with, which is why resolving a type once is enough.
		ti := &typeInfo{name: head.Name, id: head.PkgPath + "." + head.Name}
		if head.Name == "" {
			ti.name = shortTypeID(head.PkgPath)
			ti.id = head.PkgPath
		}
		if base := w.typeOf(head.Base); base != nil {
			ti.fields = base.fields
		}
		return ti

	case tStruct:
		ti := &typeInfo{name: "struct"}
		for _, f := range head.Fields {
			ti.fields = append(ti.fields, f.Name)
		}
		return ti

	case tPointer:
		if elt := w.typeOf(head.Elt); elt != nil {
			return &typeInfo{name: "*" + elt.name, id: elt.id, fields: elt.fields}
		}
		return &typeInfo{name: "pointer"}

	case tSlice:
		if elt := w.typeOf(head.Elt); elt != nil {
			return &typeInfo{name: "[]" + elt.name}
		}
		return &typeInfo{name: "slice"}

	case tArray:
		if elt := w.typeOf(head.Elt); elt != nil {
			return &typeInfo{name: "[" + head.Len + "]" + elt.name}
		}
		return &typeInfo{name: "array"}

	case tMap:
		k, v := w.typeOf(head.Key), w.typeOf(head.Value)
		if k != nil && v != nil {
			return &typeInfo{name: "map[" + k.name + "]" + v.name}
		}
		return &typeInfo{name: "map"}

	case tFunc:
		return &typeInfo{name: "func"}
	case tInterface:
		if head.Name != "" {
			return &typeInfo{name: head.Name}
		}
		return &typeInfo{name: "interface"}
	case tType:
		return &typeInfo{name: "type"}
	case tPackage:
		return &typeInfo{name: "package"}
	case tHeapItem:
		return nil
	}
	return nil
}

// fieldNames returns a struct type's field names, asking the chain for the
// type declaration only when the value did not carry it inline.
//
// want guards against a declaration that has drifted from the stored value:
// naming 3 fields of a 5-field struct would silently mislabel the last two, so
// a mismatch falls back to positional names for all of them.
func (w *walker) fieldNames(ti *typeInfo, want int) []string {
	if ti == nil || want == 0 {
		return nil
	}
	// A DeclaredType arrives with its Base inline, so most structs cost no
	// fetch at all.
	if len(ti.fields) == want {
		return ti.fields
	}
	if ti.id == "" || w.res == nil {
		return nil
	}
	if cached, ok := w.types[ti.id]; ok {
		if cached == nil || len(cached.fields) != want {
			return nil
		}
		return cached.fields
	}
	w.types[ti.id] = nil // negative-cache before the call, so a failure is not retried

	if w.stats.Fetches >= w.lim.MaxFetch {
		w.stats.Truncated = true
		return nil
	}
	w.stats.Fetches++
	body, err := w.res.Type(ti.id)
	if err != nil || len(body) == 0 {
		return nil
	}
	var env struct {
		TypeID string          `json:"typeid"`
		Type   json.RawMessage `json:"type"`
	}
	if json.Unmarshal(body, &env) != nil {
		return nil
	}
	resolved := w.typeOf(env.Type)
	w.types[ti.id] = resolved
	if resolved == nil || len(resolved.fields) != want {
		return nil
	}
	return resolved.fields
}

// tname is the display name of a possibly-absent type.
func tname(ti *typeInfo) string {
	if ti == nil {
		return ""
	}
	return ti.name
}

// zeroValue renders the omitted payload of a primitive whose value is zero.
func zeroValue(code int64) string {
	switch code {
	case 2, 4: // untyped bool, bool
		return "false"
	case 8, 16: // untyped string, string
		return ""
	case 131072, 262144: // float32, float64
		return "0"
	}
	return "0"
}

func (w *walker) clip(s string) string {
	if len(s) <= w.lim.MaxString {
		return s
	}
	w.stats.Truncated = true
	return s[:w.lim.MaxString] + "…"
}

// stamp copies a stored object's identity onto the node that replaced it, so
// unwrapping a heap item for readability does not lose the link the object
// graph and the storage view are drawn from.
func stamp(n *Node, oi objectInfo) {
	if oi.ID == "" {
		return
	}
	n.ObjectID = oi.ID
	n.OwnerID = oi.OwnerID
	n.Hash = oi.Hash
	if sz, err := strconv.ParseInt(oi.LastObjectSize, 10, 64); err == nil {
		n.Size = sz
	}
}

func primitiveCode(t json.RawMessage) int64 {
	var v struct {
		Type  string `json:"@type"`
		Value string `json:"value"`
	}
	if json.Unmarshal(t, &v) != nil || v.Type != tPrimitive {
		return 0
	}
	code, _ := strconv.ParseInt(v.Value, 10, 64)
	return code
}

// shortTypeID drops the package path from a type ID, because
// `gno.land/r/g1lnkyt…/home.Theme` in a type column pushes the value off the
// page and the path is already the page the reader is on.
func shortTypeID(id string) string {
	if i := strings.LastIndexByte(id, '.'); i >= 0 {
		return id[i+1:]
	}
	return id
}

// printableASCII reports whether b is short, printable text, so a []byte
// holding a string renders as one rather than as a byte count.
func printableASCII(b []byte) (string, bool) {
	if len(b) == 0 || len(b) > 512 {
		return "", false
	}
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return "", false
		}
	}
	return string(b), true
}
