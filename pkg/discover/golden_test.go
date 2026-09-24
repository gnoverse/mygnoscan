package discover

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// G6: the golden files.
//
// One approved rendering per kind, checked in beside the facts it came from.
// This is not a rule like G1 to G5, it is a regression net with a different
// job: a template change that shifts the voice shows up as a diff a human
// reviews, which is how twelve event kinds end up sounding like one product
// rather than twelve. The gate keeps the prose true; this keeps it consistent.
//
// The four here are the spec's own worked examples, which is the right seed:
// they were written by a person obeying the rule, so they are the standard the
// generated ones have to meet. An emitter added later drops its fixture in this
// directory and inherits every assertion below.

type goldenFile struct {
	Facts  Facts  `json:"facts"`
	Layers Layers `json:"layers"`
}

func loadGolden(t *testing.T) map[string]goldenFile {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no golden files: this test would assert nothing")
	}
	out := map[string]goldenFile{}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		var g goldenFile
		// Numbers stay json.Number so 603 does not become 603.0 and print as
		// something no template would ever write.
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.UseNumber()
		if err := dec.Decode(&g); err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		out[strings.TrimSuffix(filepath.Base(p), ".json")] = g
	}
	return out
}

func TestEveryGoldenRenderingIsGrounded(t *testing.T) {
	for kind, g := range loadGolden(t) {
		t.Run(kind, func(t *testing.T) {
			if vs := Ground(g.Facts, g.Layers, headwords); len(vs) > 0 {
				t.Errorf("approved rendering does not pass the gate: %s", checks(vs))
			}
		})
	}
}

// The flat fields are projections and nothing may set them independently. The
// assertion is cheap and the failure it prevents is not: a headline that says
// something the structure it came from does not is invisible to an auditor
// reading `layers`, which is the half of the payload that carries the evidence.
func TestFlatFieldsAreProjectionsOfTheLayers(t *testing.T) {
	for kind, g := range loadGolden(t) {
		t.Run(kind, func(t *testing.T) {
			if g.Layers.Headline() != g.Layers.Means.Text {
				t.Errorf("headline %q is not layers.means.text %q", g.Layers.Headline(), g.Layers.Means.Text)
			}
			want := g.Layers.Means.Text + "\n" + g.Layers.Matters.Text
			if g.Layers.Explanation() != want {
				t.Errorf("explanation %q is not means + newline + matters", g.Layers.Explanation())
			}
			if n := len([]rune(g.Layers.Headline())); n > MaxMeans {
				t.Errorf("headline is %d characters; the card's row is %d and a card that truncates lies by omission", n, MaxMeans)
			}
			if n := len([]rune(g.Layers.Explanation())); n > MaxExplanation {
				t.Errorf("explanation is %d characters, the budget is %d", n, MaxExplanation)
			}
		})
	}
}

// The gate has to be able to fail, and a golden file is exactly where a
// regression would land. Mutating one in memory proves the harness reads what
// it checks, rather than checking an empty set and reporting success.
func TestTheGoldenHarnessWouldCatchARegression(t *testing.T) {
	for kind, g := range loadGolden(t) {
		broken := g.Layers
		broken.Matters.Text = "It is the biggest app on the chain, and 9999 people use it."
		if vs := Ground(g.Facts, broken, headwords); len(vs) == 0 {
			t.Fatalf("%s: a superlative and an invented figure both passed", kind)
		}
		return // one is enough; the point is the harness, not the kind
	}
}
