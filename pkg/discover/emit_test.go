package discover

import (
	"encoding/json"
	"testing"
)

// Every emitter must reproduce its golden file exactly.
//
// The goldens were written by a person obeying the grounding rule, so they are
// the standard rather than a snapshot of whatever the code happened to print.
// Generating them from the emitters would make this test assert that the code
// equals itself.
func TestEmittersReproduceTheirGoldens(t *testing.T) {
	goldens := loadGolden(t)

	cases := []struct {
		kind   string
		facts  Facts
		layers Layers
	}{}

	f, l := PackageDeployed{
		Path: "gno.land/r/moul/hello", Creator: "g1manfred47kzduec920z88wfr64ylksmdcedlf5",
		ActorLabel: "@moul", Height: 173108, IsRealm: true, NumFiles: 3, FirstEver: false,
	}.Emit()
	cases = append(cases, struct {
		kind   string
		facts  Facts
		layers Layers
	}{"package.deployed", f, l})

	f, l = DeployerFirst{
		Address: "g1r6luttvrkksxh9h4asjq2qd8zsd5nlkrzvyjur", ActorLabel: "",
		PackageName: "pixelgnomes", IsRealm: true, NetworkLabel: "mainnet",
		DistinctDeployers: 10,
	}.Emit()
	cases = append(cases, struct {
		kind   string
		facts  Facts
		layers Layers
	}{"deployer.first", f, l})

	f, l = ChainSpike{
		Day: "2026-09-18", NetworkLabel: "mainnet",
		NewAddresses: 603, BaselineMedian: 46, Ratio: 13.1,
	}.Emit()
	cases = append(cases, struct {
		kind   string
		facts  Facts
		layers Layers
	}{"chain.spike", f, l})

	f, l = PackageEnabled{
		Path: "gno.land/r/nym-thegnomic001/gnomic_airdrop", Height: 173108,
		WaitBlocks: 4, WaitSeconds: 13.2, IsRealm: true,
	}.Emit()
	cases = append(cases, struct {
		kind   string
		facts  Facts
		layers Layers
	}{"package.enabled", f, l})

	for _, c := range cases {
		g, ok := goldens[c.kind]
		if !ok {
			t.Errorf("%s: no golden file", c.kind)
			continue
		}
		if got, want := c.layers, g.Layers; got != want {
			t.Errorf("%s layers differ:\n  what    got %q\n          want %q\n"+
				"  means   got %q\n          want %q\n  matters got %q\n          want %q",
				c.kind, got.What.Text, want.What.Text, got.Means.Text, want.Means.Text,
				got.Matters.Text, want.Matters.Text)
		}
		if !sameFacts(t, c.facts, g.Facts) {
			gotJSON, _ := json.Marshal(c.facts)
			wantJSON, _ := json.Marshal(g.Facts)
			t.Errorf("%s facts differ:\n  got  %s\n  want %s", c.kind, gotJSON, wantJSON)
		}
	}
}

// sameFacts compares through JSON, because the golden is decoded with
// json.Number and the emitters produce Go ints and floats. Comparing the
// rendered forms is the comparison that matters anyway: it is what the API
// serializes.
func sameFacts(t *testing.T, got, want Facts) bool {
	t.Helper()
	normalize := func(f Facts) map[string]string {
		out := map[string]string{}
		for k, v := range f {
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("marshal fact %s: %v", k, err)
			}
			out[k] = string(b)
		}
		return out
	}
	a, b := normalize(got), normalize(want)
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		if b[k] != av {
			return false
		}
	}
	return true
}

// The emitters must satisfy the gate, not merely match a fixture. A golden file
// and a template can be wrong together; Ground is the independent check.
func TestEmittersPassTheGroundingGate(t *testing.T) {
	type emitted struct {
		kind   string
		facts  Facts
		layers Layers
	}
	var all []emitted

	add := func(kind string, f Facts, l Layers) { all = append(all, emitted{kind, f, l}) }

	f, l := PackageDeployed{
		Path: "gno.land/r/moul/hello", Creator: "g1manfred47kzduec920z88wfr64ylksmdcedlf5",
		ActorLabel: "@moul", Height: 173108, IsRealm: true, NumFiles: 3,
	}.Emit()
	add("package.deployed", f, l)

	// The unnamed-author path, which is the common one: most addresses have no
	// registered name, and the template has to read as a sentence without one.
	f, l = PackageDeployed{
		Path: "gno.land/p/someone/avl", Creator: "g1abc", Height: 10, NumFiles: 1,
	}.Emit()
	add("package.deployed (no label, pure package, one file)", f, l)

	f, l = DeployerFirst{
		Address: "g1r6luttvrkksxh9h4asjq2qd8zsd5nlkrzvyjur", PackageName: "pixelgnomes",
		IsRealm: true, NetworkLabel: "mainnet", DistinctDeployers: 10,
	}.Emit()
	add("deployer.first", f, l)

	f, l = ChainSpike{Day: "2026-09-18", NetworkLabel: "mainnet",
		NewAddresses: 603, BaselineMedian: 46, Ratio: 13.1}.Emit()
	add("chain.spike", f, l)

	f, l = PackageEnabled{Path: "gno.land/r/x/y", Height: 1, WaitBlocks: 1,
		WaitSeconds: 1.4, IsRealm: true}.Emit()
	add("package.enabled (one block, one second)", f, l)

	for _, e := range all {
		if v := Ground(e.facts, e.layers, nil); len(v) > 0 {
			for _, one := range v {
				t.Errorf("%s: %s", e.kind, one.Error())
			}
		}
	}
}

// splitPath is the only place a namespace is derived, and getting it wrong
// would label every package on the chain "gno.land".
func TestSplitPath(t *testing.T) {
	cases := []struct{ in, ns, name string }{
		{"gno.land/r/moul/hello", "moul", "hello"},
		{"gno.land/p/demo/avl", "demo", "avl"},
		{"gno.land/r/moul/x/vm/bf", "moul", "bf"},
		{"gno.land/r/g1abc/kourt", "g1abc", "kourt"},
		{"weird", "", "weird"},
	}
	for _, c := range cases {
		ns, name := splitPath(c.in)
		if ns != c.ns || name != c.name {
			t.Errorf("splitPath(%q) = (%q, %q), want (%q, %q)", c.in, ns, name, c.ns, c.name)
		}
	}
}

// A spelled number is checked by the gate exactly as a digit is, so the
// spelling has to match the gate's vocabulary and fall back to digits beyond it
// rather than inventing a word Ground cannot verify.
func TestSpellSmallStaysInsideTheGatesVocabulary(t *testing.T) {
	for n := 0; n <= 19; n++ {
		word := spellSmall(n)
		v, ok := smallWords[lower(word)]
		if !ok || int(v) != n {
			t.Errorf("spellSmall(%d) = %q, which the gate does not read as %d", n, word, n)
		}
	}
	if got := spellSmall(20); got != "20" {
		t.Errorf("spellSmall(20) = %q, want the digits: the gate has no word for it", got)
	}
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
