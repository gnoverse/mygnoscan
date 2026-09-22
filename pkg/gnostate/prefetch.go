package gnostate

import (
	"math"
	"sort"
)

// Resolving a realm one object at a time is too slow to be a design.
//
// Measured 2026-09-22 against mainnet: decoding `gno.land/r/gnoland/blog` (65
// posts) takes 2,575 `vm/qobject_json` calls and 4m36s when the walker asks
// for objects one at a time, because it can only ask for the next one after
// the previous answer told it that object exists.
//
// Two things were measured and one of them is a dead end. JSON-RPC batching is
// accepted by the node and buys nothing: 10 objects in 2,017ms, 200 in
// 26,591ms, a flat ~135ms each, because a batch is served serially. Plain
// concurrent requests do scale: the same 40 objects take 15,458ms at one
// worker and 1,505ms at sixteen, with per-request latency unchanged.
//
// So the fix is not a faster fetch, it is a wider one. The object graph is
// shallow relative to its size (an avl.Tree of 65 entries is ~7 levels but
// ~130 nodes), so resolving it breadth-first turns hundreds of sequential
// round trips into a handful of parallel rounds: walk the tree, collect every
// ObjectID the walk wanted and did not have, fetch that whole level at once,
// walk again with a warmer cache. Repeat until a walk asks for nothing new.

// Fetcher retrieves many objects or types at once. Implementations are
// expected to parallelise; this package does not choose the worker count,
// because politeness towards someone else's public RPC is the caller's policy.
//
// A missing key in the returned map is "not available", which the walk renders
// as an unexpanded ref. Only a transport failure deserves an error.
type Fetcher interface {
	Objects(oids []string) (map[string][]byte, error)
	Types(tids []string) (map[string][]byte, error)
}

// maxRounds bounds the breadth-first passes. Each round resolves one more
// level of the object graph, so this is a depth limit in disguise and is set
// above Limits.MaxDepth for that reason: the walk should stop because the tree
// is fully resolved or because MaxDepth said so, never because the loop here
// ran out of patience without saying it did.
const maxRounds = 64

// DecodePackageWith decodes a package block, resolving references
// breadth-first through f.
//
// It returns the last tree it built, so a caller that runs out of budget still
// gets everything resolved so far, with Stats.Truncated set.
func DecodePackageWith(raw []byte, f Fetcher, lim Limits) (*Tree, error) {
	lim = lim.withDefaults()
	c := &prefetchCache{
		fetcher: f,
		budget:  lim.MaxFetch,
		objects: map[string][]byte{},
		types:   map[string][]byte{},
	}

	// The walk's own fetch counter has to be taken out of the loop, and this
	// is the subtle part of resolving in rounds.
	//
	// Every round re-walks the whole tree from the root, so by the last round
	// the walker makes one Resolver call per already-cached object. Those
	// calls are free, but the walker cannot tell them apart from a real round
	// trip, so leaving MaxFetch in place would have a realm needing more
	// objects than the cap stop dead against its own cache and report a fully
	// resolved tree as truncated. Measured: r/gnoland/blog touches 1,259
	// unique objects against a default cap of 512.
	//
	// The cap still applies, it just moves to where the cost actually is:
	// prefetchCache enforces it against *distinct objects fetched*, which is
	// what a round trip is.
	walkLim := lim
	walkLim.MaxFetch = math.MaxInt

	var tree *Tree
	for round := 0; round < maxRounds; round++ {
		c.wantObjects, c.wantTypes = nil, nil

		var err error
		tree, err = DecodePackage(raw, c, walkLim)
		if err != nil {
			return nil, err
		}
		if c.exhausted {
			// The caller's fetch budget ran out, so refs remain unresolved and
			// the tree must say so. The walker did not set this, because its
			// own counter was lifted above.
			tree.Stats.Truncated = true
			return tree, nil
		}
		// Nothing new was asked for, so another round would walk the same tree
		// and reach the same refs.
		if len(c.wantObjects) == 0 && len(c.wantTypes) == 0 {
			return tree, nil
		}
		if err := c.fill(); err != nil {
			return nil, err
		}
	}
	// maxRounds is not a budget the caller set, so a tree that hits it is
	// incomplete for a reason the Stats must carry.
	tree.Stats.Truncated = true
	return tree, nil
}

// prefetchCache is a Resolver that never blocks: a miss is recorded and
// answered with "not available", and the next round has it.
type prefetchCache struct {
	fetcher Fetcher
	// budget is the caller's MaxFetch, counted down in distinct objects
	// actually requested from the chain rather than in walker calls.
	budget      int
	exhausted   bool
	objects     map[string][]byte
	types       map[string][]byte
	wantObjects []string
	wantTypes   []string
}

func (c *prefetchCache) Object(oid string) ([]byte, error) {
	if b, ok := c.objects[oid]; ok {
		return b, nil
	}
	c.wantObjects = append(c.wantObjects, oid)
	return nil, nil
}

func (c *prefetchCache) Type(tid string) ([]byte, error) {
	if b, ok := c.types[tid]; ok {
		return b, nil
	}
	c.wantTypes = append(c.wantTypes, tid)
	return nil, nil
}

// fill fetches one round's worth of misses.
//
// Every requested key is written back, including the ones the fetcher could
// not supply, recorded as nil. Without that a permanently missing object is
// re-requested every round, and the loop only terminates when maxRounds runs
// out, reporting a complete tree as truncated.
func (c *prefetchCache) fill() error {
	if oids := dedupe(c.wantObjects); len(oids) > 0 {
		// Truncate the round rather than the object: a partial level still
		// leaves every ref it did not reach rendered as a followable link.
		if len(oids) > c.budget {
			oids = oids[:c.budget]
			c.exhausted = true
		}
		c.budget -= len(oids)
		if len(oids) == 0 {
			return nil
		}
		got, err := c.fetcher.Objects(oids)
		if err != nil {
			return err
		}
		for _, oid := range oids {
			c.objects[oid] = got[oid]
		}
	}
	if tids := dedupe(c.wantTypes); len(tids) > 0 {
		got, err := c.fetcher.Types(tids)
		if err != nil {
			return err
		}
		for _, tid := range tids {
			c.types[tid] = got[tid]
		}
	}
	return nil
}

// dedupe sorts and uniques a round's requests. Sorted because a stable request
// order makes a slow round reproducible when someone goes looking for which
// object is costing the time.
func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	sort.Strings(in)
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}
