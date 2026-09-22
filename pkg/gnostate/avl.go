package gnostate

// Recognizing avl.Tree is what separates a state explorer from a picture of
// the VM's heap.
//
// Almost every gno realm keeps its real data in a `gno.land/p/nt/avl` tree,
// because gno maps do not have a deterministic iteration order. Decoded
// literally, a tree of 65 posts renders as seven levels of
// `node.leftNode.rightNode.leftNode…`, each level a struct of
// `key / value / height / size / leftNode / rightNode`. Every value the reader
// came for is a leaf at the bottom of that, and the shape above it is a
// balancing implementation detail they did not ask about.
//
// gnoweb's object explorer renders it literally, which is most of why it is
// unusable. This pass flattens a recognized tree into its key/value pairs, in
// key order, and keeps the tree's own ObjectID so the raw view is still one
// click away.

// avlNodeFields is the exact field set of `avl.Node`. The match has to be
// exact: a user type that happens to have a `key` and a `value` is not an AVL
// node, and flattening it would drop fields the reader needs.
var avlNodeFields = []string{"key", "value", "height", "size", "leftNode", "rightNode"}

// flattenTrees rewrites every recognized avl.Tree in a decoded forest.
func flattenTrees(nodes []Node) []Node {
	for i := range nodes {
		nodes[i].Children = flattenTrees(nodes[i].Children)
		if flat, ok := flattenTree(nodes[i]); ok {
			nodes[i] = flat
		}
	}
	return nodes
}

// flattenTree converts an avl.Tree struct node into a map-kinded node whose
// children are its entries. It reports false for anything that is not one.
func flattenTree(n Node) (Node, bool) {
	if n.Kind != KindStruct || len(n.Children) != 1 || n.Children[0].Name != "node" {
		return n, false
	}
	root := n.Children[0]
	// An empty tree is `node == nil`, which is a legitimate tree and must
	// render as an empty map rather than be left as a struct holding a nil.
	if root.Kind != KindNil && !isAVLNode(root) {
		return n, false
	}

	out := Node{
		Name:     n.Name,
		Type:     n.Type,
		Kind:     KindMap,
		ObjectID: n.ObjectID,
		OwnerID:  n.OwnerID,
		Hash:     n.Hash,
		Size:     n.Size,
	}
	var partial bool
	collectEntries(root, &out.Children, &partial)

	out.Value = plural(len(out.Children))
	if partial {
		// The walk ran out of budget before reaching every leaf. Saying so is
		// the whole point: a tree that silently renders 40 of its 65 entries
		// is worse than one that renders 40 and admits it.
		out.Value += ", partially loaded"
	}
	return out, true
}

// isAVLNode reports whether a decoded struct has exactly avl.Node's fields.
func isAVLNode(n Node) bool {
	if n.Kind != KindStruct || len(n.Children) != len(avlNodeFields) {
		return false
	}
	for i, want := range avlNodeFields {
		if n.Children[i].Name != want {
			return false
		}
	}
	return true
}

// collectEntries walks an avl.Node subtree in key order, appending one child
// per leaf.
//
// A node the walker could not resolve (an unfollowed ref, a cycle, a
// truncation) sets partial rather than being skipped silently.
func collectEntries(n Node, out *[]Node, partial *bool) {
	switch n.Kind {
	case KindNil:
		return
	case KindRef, KindCycle, KindTruncated:
		*partial = true
		return
	}
	if !isAVLNode(n) {
		*partial = true
		return
	}

	key, value := n.Children[0], n.Children[1]
	height, left, right := n.Children[2], n.Children[4], n.Children[5]

	// height 0 is a leaf, and in this AVL implementation only leaves carry a
	// value: an internal node repeats its subtree's smallest key with a nil
	// value, which is why filtering on "value is not nil" would also drop
	// every legitimately nil entry.
	if height.Value == "0" {
		entry := value
		entry.Name = key.Value
		if entry.ObjectID == "" {
			entry.ObjectID = n.ObjectID
		}
		*out = append(*out, entry)
		return
	}

	collectEntries(left, out, partial)
	collectEntries(right, out, partial)
}

func plural(n int) string {
	if n == 1 {
		return "1 entry"
	}
	return itoa(n) + " entries"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
