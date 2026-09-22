package gnostate

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
)

// The AVL fixtures are built here rather than captured, because the property
// under test is structural: a tree of N entries must flatten to exactly those
// N entries in key order, whatever its balancing looks like. A captured
// mainnet tree pins one shape; a built one lets the test say what it means.

func num(n uint64) string {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], n)
	return base64.StdEncoding.EncodeToString(b[:])
}

// avlFixture serves a synthetic avl.Tree: `tree` is the Tree struct, `nN` are
// its nodes.
type avlFixture struct {
	objects map[string]string
}

func (a *avlFixture) Object(oid string) ([]byte, error) { return []byte(a.objects[oid]), nil }

func (a *avlFixture) Type(tid string) ([]byte, error) {
	switch tid {
	case "x.Tree":
		return []byte(`{"typeid":"x.Tree","type":{"@type":"/gno.DeclaredType","PkgPath":"x","Name":"Tree",
		  "Base":{"@type":"/gno.StructType","Fields":[{"Name":"node"}]}}}`), nil
	case "x.Node":
		return []byte(`{"typeid":"x.Node","type":{"@type":"/gno.DeclaredType","PkgPath":"x","Name":"Node",
		  "Base":{"@type":"/gno.StructType","Fields":[
		    {"Name":"key"},{"Name":"value"},{"Name":"height"},
		    {"Name":"size"},{"Name":"leftNode"},{"Name":"rightNode"}]}}}`), nil
	}
	return nil, nil
}

// leaf builds an avl.Node with height 0 holding a string value.
func leaf(oid, key, val string) string {
	return fmt.Sprintf(`{"objectid":%q,"value":{"@type":"/gno.StructValue",
	  "ObjectInfo":{"ID":%q,"LastObjectSize":"64"},"Fields":[
	    {"T":{"@type":"/gno.PrimitiveType","value":"16"},"V":{"@type":"/gno.StringValue","value":%q}},
	    {"T":{"@type":"/gno.PrimitiveType","value":"16"},"V":{"@type":"/gno.StringValue","value":%q}},
	    {"T":{"@type":"/gno.PrimitiveType","value":"32"}},
	    {"T":{"@type":"/gno.PrimitiveType","value":"32"},"N":%q},
	    {"T":{"@type":"/gno.PointerType","Elt":{"@type":"/gno.RefType","ID":"x.Node"}}},
	    {"T":{"@type":"/gno.PointerType","Elt":{"@type":"/gno.RefType","ID":"x.Node"}}}]}}`,
		oid, oid, key, val, num(1))
}

// inner builds an internal avl.Node pointing at two children.
func inner(oid, key string, height, size uint64, left, right string) string {
	child := func(id string) string {
		if id == "" {
			return `{"T":{"@type":"/gno.PointerType","Elt":{"@type":"/gno.RefType","ID":"x.Node"}}}`
		}
		return fmt.Sprintf(`{"T":{"@type":"/gno.PointerType","Elt":{"@type":"/gno.RefType","ID":"x.Node"}},
		  "V":{"@type":"/gno.RefValue","ObjectID":%q}}`, id)
	}
	return fmt.Sprintf(`{"objectid":%q,"value":{"@type":"/gno.StructValue",
	  "ObjectInfo":{"ID":%q,"LastObjectSize":"96"},"Fields":[
	    {"T":{"@type":"/gno.PrimitiveType","value":"16"},"V":{"@type":"/gno.StringValue","value":%q}},
	    {"T":{"@type":"/gno.PointerType","Elt":{"@type":"/gno.RefType","ID":"x.Post"}}},
	    {"T":{"@type":"/gno.PrimitiveType","value":"32"},"N":%q},
	    {"T":{"@type":"/gno.PrimitiveType","value":"32"},"N":%q},
	    %s,
	    %s]}}`, oid, oid, key, num(height), num(size), child(left), child(right))
}

// treeObj wraps a root node in the Tree struct the realm actually holds.
func treeObj(oid, root string) string {
	node := `{"T":{"@type":"/gno.PointerType","Elt":{"@type":"/gno.RefType","ID":"x.Node"}}}`
	if root != "" {
		node = fmt.Sprintf(`{"T":{"@type":"/gno.PointerType","Elt":{"@type":"/gno.RefType","ID":"x.Node"}},
		  "V":{"@type":"/gno.RefValue","ObjectID":%q}}`, root)
	}
	return fmt.Sprintf(`{"objectid":%q,"value":{"@type":"/gno.StructValue",
	  "ObjectInfo":{"ID":%q,"LastObjectSize":"396"},"Fields":[%s]}}`, oid, oid, node)
}

const avlPkg = `{"names":["posts"],"values":[
  {"T":{"@type":"/gno.RefType","ID":"x.Tree"},
   "V":{"@type":"/gno.RefValue","ObjectID":"t:0"}}]}`

func TestFlattenAVL(t *testing.T) {
	tests := []struct {
		name    string
		objects map[string]string
		want    []string // keys, in order
		wantVal []string
		partial bool
	}{
		{
			name: "three entries flatten in key order",
			objects: map[string]string{
				"t:0": treeObj("t:0", "n:1"),
				// root splits on "b"; gno's avl keeps the subtree's smallest
				// key on the internal node, with a nil value.
				"n:1": inner("n:1", "b", 1, 3, "n:2", "n:3"),
				"n:2": leaf("n:2", "a", "alpha"),
				"n:3": inner("n:3", "b", 1, 2, "n:4", "n:5"),
				"n:4": leaf("n:4", "b", "bravo"),
				"n:5": leaf("n:5", "c", "charlie"),
			},
			want:    []string{"a", "b", "c"},
			wantVal: []string{"alpha", "bravo", "charlie"},
		},
		{
			name:    "an empty tree is a tree, not a struct holding nil",
			objects: map[string]string{"t:0": treeObj("t:0", "")},
			want:    []string{},
		},
		{
			name: "a single entry",
			objects: map[string]string{
				"t:0": treeObj("t:0", "n:1"),
				"n:1": leaf("n:1", "only", "one"),
			},
			want:    []string{"only"},
			wantVal: []string{"one"},
		},
		{
			// The honesty case: a subtree the walker could not reach must make
			// the whole tree say so, not quietly report the entries it did
			// reach as the complete set.
			name: "an unreachable subtree marks the tree partial",
			objects: map[string]string{
				"t:0": treeObj("t:0", "n:1"),
				"n:1": inner("n:1", "b", 1, 3, "n:2", "n:missing"),
				"n:2": leaf("n:2", "a", "alpha"),
			},
			want:    []string{"a"},
			wantVal: []string{"alpha"},
			partial: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tree, err := DecodePackage([]byte(avlPkg), &avlFixture{objects: tt.objects}, Limits{})
			if err != nil {
				t.Fatalf("DecodePackage: %v", err)
			}
			posts := find(tree.Nodes, "posts")
			if posts == nil {
				t.Fatal("posts missing")
			}
			if posts.Kind != KindMap {
				t.Fatalf("kind = %q, want map: the avl.Tree was not recognized\n%s",
					posts.Kind, dump(tree.Nodes, 0))
			}
			var keys, vals []string
			for _, c := range posts.Children {
				keys = append(keys, c.Name)
				vals = append(vals, c.Value)
			}
			if len(keys) != len(tt.want) {
				t.Fatalf("keys = %v, want %v", keys, tt.want)
			}
			for i := range tt.want {
				if keys[i] != tt.want[i] {
					t.Errorf("key[%d] = %q, want %q", i, keys[i], tt.want[i])
				}
				if i < len(tt.wantVal) && vals[i] != tt.wantVal[i] {
					t.Errorf("value[%d] = %q, want %q", i, vals[i], tt.wantVal[i])
				}
			}
			gotPartial := strings.Contains(posts.Value, "partially loaded")
			if gotPartial != tt.partial {
				t.Errorf("partial = %v (%q), want %v", gotPartial, posts.Value, tt.partial)
			}
			// The tree's own identity has to survive flattening, or the object
			// graph loses the node and "show raw" has nothing to open.
			if posts.ObjectID == "" {
				t.Error("flattened tree carries no ObjectID")
			}
		})
	}
}

// TestFlattenAVLLeavesOtherStructsAlone guards the recognizer against a user
// type that merely resembles a tree. Flattening one would silently drop fields.
func TestFlattenAVLLeavesOtherStructsAlone(t *testing.T) {
	notATree := &avlFixture{objects: map[string]string{
		// One field named `node`, but the thing it points at is not an
		// avl.Node: it has the wrong field set.
		"t:0": treeObj("t:0", "n:1"),
		"n:1": fmt.Sprintf(`{"objectid":"n:1","value":{"@type":"/gno.StructValue",
		  "ObjectInfo":{"ID":"n:1"},"Fields":[
		    {"T":{"@type":"/gno.PrimitiveType","value":"16"},"V":{"@type":"/gno.StringValue","value":%q}}]}}`, "hello"),
	}}
	tree, err := DecodePackage([]byte(avlPkg), notATree, Limits{})
	if err != nil {
		t.Fatalf("DecodePackage: %v", err)
	}
	posts := find(tree.Nodes, "posts")
	if posts == nil {
		t.Fatal("posts missing")
	}
	if posts.Kind == KindMap && len(posts.Children) == 0 {
		t.Errorf("a struct that is not an avl.Tree was flattened to an empty map, losing its contents:\n%s",
			dump(tree.Nodes, 0))
	}
}

// TestFlattenAVLOnMainnetData runs the recognizer against a realm captured
// from mainnet, which is the only way to be sure the field ordering and the
// pointer indirection match what the chain emits rather than what this test
// file believes.
func TestFlattenAVLOnMainnetData(t *testing.T) {
	fx := load(t, "blog.json")
	tree, err := DecodePackage([]byte(fx.Package), fx, Limits{MaxDepth: 64, MaxNodes: 200000, MaxFetch: 200000})
	if err != nil {
		t.Fatalf("DecodePackage: %v", err)
	}
	// r/gnoland/blog keeps its posts in three avl.Tree fields on `b`.
	for _, name := range []string{"Posts", "PostsPublished", "PostsAlphabetical"} {
		n := find(tree.Nodes, "b."+name)
		if n == nil {
			t.Fatalf("b.%s missing from the decoded tree", name)
		}
		if n.Kind != KindMap {
			t.Errorf("b.%s kind = %q, want map: the avl.Tree was not recognized on real data", name, n.Kind)
		}
		if n.ObjectID == "" {
			t.Errorf("b.%s carries no ObjectID after flattening", name)
		}
		// Whatever the fixture's depth, the tree must never claim to be
		// complete while holding fewer entries than it reached.
		if strings.Contains(n.Value, "partially loaded") && len(n.Children) == 0 {
			continue // an honestly-empty partial read is fine
		}
		if !strings.Contains(n.Value, "entr") {
			t.Errorf("b.%s value = %q, want an entry count", name, n.Value)
		}
	}
}
