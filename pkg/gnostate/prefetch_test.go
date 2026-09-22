package gnostate

import (
	"fmt"
	"testing"
)

// roundFetcher serves a fixture and records how many rounds it was asked for,
// and how wide each one was. Those two numbers are the whole point of
// DecodePackageWith: the same data in a handful of wide rounds rather than
// hundreds of sequential ones.
type roundFetcher struct {
	f       *fixture
	rounds  int
	widths  []int
	fetched int
	failAll bool
}

func (r *roundFetcher) Objects(oids []string) (map[string][]byte, error) {
	if r.failAll {
		return nil, fmt.Errorf("transport down")
	}
	r.rounds++
	r.widths = append(r.widths, len(oids))
	out := map[string][]byte{}
	for _, o := range oids {
		if b, _ := r.f.Object(o); len(b) > 0 {
			out[o] = b
			r.fetched++
		}
	}
	return out, nil
}

func (r *roundFetcher) Types(tids []string) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, t := range tids {
		if b, _ := r.f.Type(t); len(b) > 0 {
			out[t] = b
		}
	}
	return out, nil
}

// TestPrefetchMatchesSequential is the correctness half: resolving
// breadth-first must produce exactly the tree the one-at-a-time walker does.
// A faster decode that disagrees with the slow one is not an optimization.
func TestPrefetchMatchesSequential(t *testing.T) {
	for _, name := range []string{"blog.json", "home.json"} {
		t.Run(name, func(t *testing.T) {
			f := load(t, name)
			lim := Limits{MaxDepth: 64, MaxNodes: 200000, MaxFetch: 200000}

			want, err := DecodePackage([]byte(f.Package), f, lim)
			if err != nil {
				t.Fatalf("sequential: %v", err)
			}
			rf := &roundFetcher{f: f}
			got, err := DecodePackageWith([]byte(f.Package), rf, lim)
			if err != nil {
				t.Fatalf("prefetch: %v", err)
			}
			if a, b := dump(want.Nodes, 0), dump(got.Nodes, 0); a != b {
				t.Errorf("prefetch and sequential disagree\n--- sequential ---\n%s\n--- prefetch ---\n%s", a, b)
			}
			t.Logf("%s: %d rounds, widths %v", name, rf.rounds, rf.widths)

			// The performance half. Sequential needs one round trip per
			// object; breadth-first needs one per level of the object graph.
			// If rounds ever approaches the object count, the loop has
			// degenerated back into a sequential walk and the 4m36s measured
			// on mainnet is back.
			if rf.rounds >= rf.fetched && rf.fetched > 0 {
				t.Errorf("%d rounds for %d objects: the walk is not resolving breadth-first", rf.rounds, rf.fetched)
			}
		})
	}
}

// TestPrefetchTerminatesOnMissingObjects is the loop's real failure mode: an
// object the chain cannot supply must be remembered as absent, or every round
// asks for it again and a complete tree gets reported as truncated after 64
// wasted passes.
func TestPrefetchTerminatesOnMissingObjects(t *testing.T) {
	empty := &fixture{
		Package: `{"names":["a"],"values":[
		  {"T":{"@type":"/gno.RefType","ID":"x.T"},
		   "V":{"@type":"/gno.RefValue","ObjectID":"gone:1"}}]}`,
		Objects: map[string]string{},
		Types:   map[string]string{},
	}
	rf := &roundFetcher{f: empty}
	tree, err := DecodePackageWith([]byte(empty.Package), rf, Limits{})
	if err != nil {
		t.Fatalf("DecodePackageWith: %v", err)
	}
	if rf.rounds > 2 {
		t.Errorf("%d rounds for one permanently missing object; it is being re-requested every pass", rf.rounds)
	}
	if tree.Stats.Truncated {
		t.Error("Stats.Truncated is set: a ref the chain cannot supply is not an incomplete walk")
	}
	n := find(tree.Nodes, "a")
	if n == nil || n.Kind != KindRef {
		t.Errorf("unresolvable ref rendered as %v, want a followable ref row", n)
	}
}

func TestPrefetchPropagatesTransportErrors(t *testing.T) {
	f := load(t, "blog.json")
	rf := &roundFetcher{f: f, failAll: true}
	// A transport failure must surface. Rendering a realm's state as empty
	// because the chain was briefly unreachable is the one outcome worse than
	// an error page.
	if _, err := DecodePackageWith([]byte(f.Package), rf, Limits{}); err == nil {
		t.Fatal("a failing fetcher produced no error")
	}
}
