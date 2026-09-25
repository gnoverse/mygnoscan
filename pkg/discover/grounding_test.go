package discover

import (
	"strings"
	"testing"
)

// The glossary headwords, as of docs/glossary.md. Passed in rather than read,
// so this package's tests do not move every time a term is added.
var headwords = []string{
	"address", "block", "call", "directory", "gas", "GPAO", "inert", "mainnet",
	"MsgAddPackage", "namespace", "package", "parked", "proposal", "realm",
	"render", "storage deposit", "transaction", "unique callers", "validator",
}

func checks(vs []Violation) string {
	var out []string
	for _, v := range vs {
		out = append(out, v.Error())
	}
	return strings.Join(out, "; ")
}

// The four worked examples from the spec, hand-written by a person trying to
// obey the rule. If the checks reject these the checks are wrong, not the
// prose, so this is the test that keeps the gate honest rather than merely
// strict: a gate nobody can pass gets switched off.
func TestTheSpecsOwnExamplesPass(t *testing.T) {
	tests := []struct {
		kind   string
		facts  Facts
		layers Layers
	}{
		{
			kind: "package.deployed",
			facts: Facts{
				"package_name": "hello", "namespace": "moul", "actor_label": "@moul",
				"is_realm": true, "num_files": 3, "first_ever": false,
			},
			layers: Layers{
				What:    Layer{"MsgAddPackage published gno.land/r/moul/hello, 3 files, by g1manfred47kzduec920z88wfr64ylksmdcedlf5, at block 173108."},
				Means:   Layer{"@moul put a new app on the chain, called hello."},
				Matters: Layer{"Anyone can look at it or use it now, and nobody can change it except @moul. It is 3 files of code."},
			},
		},
		{
			kind: "deployer.first",
			// distinct_deployers is in this set and is not in the spec's own
			// fact list, though the spec's note beside the example requires it:
			// "layer 3 can say ten people have ever published only because
			// facts.distinct_deployers = 10 is in the fact set". The note is
			// the rule; the list under it is short one entry.
			facts: Facts{
				"actor_address": "g1r6luttvrkksxh9h4asjq2qd8zsd5nlkrzvyjur", "actor_label": "",
				"package_name": "pixelgnomes", "first_ever": true, "distinct_deployers": 10,
			},
			layers: Layers{
				What:    Layer{"g1r6luttvrkksxh9h4asjq2qd8zsd5nlkrzvyjur has no earlier successful MsgAddPackage on mainnet."},
				Means:   Layer{"Somebody published on the chain for the first time, an app called pixelgnomes."},
				Matters: Layer{"A new builder arrived. Ten people have ever published on this chain."},
			},
		},
		{
			kind: "chain.spike",
			facts: Facts{
				"new_addresses": 603, "baseline_median": 46, "excess": 557,
				"ratio": 13.1, "day": "2026-09-18",
			},
			layers: Layers{
				What:    Layer{"603 addresses appeared on mainnet for the first time on 2026-09-18, against a 7-day median of 46."},
				Means:   Layer{"603 new wallets showed up on the chain in one day."},
				Matters: Layer{"A normal day is about 46, so that is 13 times normal. Something brought a lot of people at once."},
			},
		},
		{
			kind:  "package.enabled",
			facts: Facts{"package_name": "gnomic_airdrop", "wait_blocks": 4, "wait_seconds": 13.2},
			layers: Layers{
				What:    Layer{"MsgEnablePackage enabled gno.land/r/nym-thegnomic001/gnomic_airdrop at block 173108, 4 blocks after submission."},
				Means:   Layer{"An app called gnomic_airdrop was switched on."},
				Matters: Layer{"Anyone can use it now. It waited 13 seconds to be checked and approved."},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			if vs := Ground(tt.facts, tt.layers, headwords); len(vs) > 0 {
				t.Errorf("the spec's own example fails: %s", checks(vs))
			}
		})
	}
}

// One table for the five rules, each case a sentence somebody could plausibly
// write and each one wrong in exactly one way.
func TestEachCheckCatchesItsFailure(t *testing.T) {
	base := Facts{"package_name": "hello", "actor_label": "@moul", "num_files": 3}
	ok := "An app called hello was published."

	tests := []struct {
		name  string
		facts Facts
		l     Layers
		check string
		want  string
	}{
		{
			name:  "G1 a number no fact supports",
			facts: base,
			l:     Layers{What: Layer{"x"}, Means: Layer{ok}, Matters: Layer{"It is 47 files of code."}},
			check: "G1", want: "the number 47",
		},
		{
			name:  "G1 a figure spelled as a word",
			facts: base,
			l:     Layers{What: Layer{"x"}, Means: Layer{ok}, Matters: Layer{"Seven people have published."}},
			check: "G1", want: `"seven"`,
		},
		{
			name:  "G1 a block count converted without a block rate",
			facts: Facts{"wait_blocks": 4},
			l:     Layers{What: Layer{"x"}, Means: Layer{ok}, Matters: Layer{"It waited 8 seconds."}},
			check: "G1", want: "the number 8",
		},
		{
			name:  "G2 a package path from another row",
			facts: base,
			l:     Layers{What: Layer{"x"}, Means: Layer{ok}, Matters: Layer{"It imports gno.land/p/demo/avl to work."}},
			check: "G2", want: "a package path",
		},
		{
			name:  "G2 a handle nobody supplied",
			facts: base,
			l:     Layers{What: Layer{"x"}, Means: Layer{ok}, Matters: Layer{"It was reviewed by @onbloc."}},
			check: "G2", want: "a handle",
		},
		{
			name:  "G2 a proper noun the chain never said",
			facts: base,
			l:     Layers{What: Layer{"x"}, Means: Layer{ok}, Matters: Layer{"It runs on the Gno Chain."}},
			check: "G2", want: "a proper noun",
		},
		{
			name:  "G3 the named failure mode",
			facts: base,
			l:     Layers{What: Layer{"x"}, Means: Layer{ok}, Matters: Layer{"This is the first lending protocol here."}},
			check: "G3", want: `"first"`,
		},
		{
			name:  "G3 a superlative nothing can license",
			facts: base,
			l:     Layers{What: Layer{"x"}, Means: Layer{ok}, Matters: Layer{"It is the most popular app around."}},
			check: "G3", want: "nothing licenses it",
		},
		{
			name:  "G4 a motive",
			facts: base,
			l:     Layers{What: Layer{"x"}, Means: Layer{ok}, Matters: Layer{"The team plans to add trading."}},
			check: "G4", want: "plans to",
		},
		{
			name:  "G4 a promise",
			facts: base,
			l:     Layers{What: Layer{"x"}, Means: Layer{ok}, Matters: Layer{"It will be useful to builders."}},
			check: "G4", want: "looks forward",
		},
		{
			name:  "G5 jargon in the one line a skimmer reads",
			facts: base,
			l:     Layers{What: Layer{"x"}, Means: Layer{"A new realm was published."}, Matters: Layer{"Anyone can use it."}},
			check: "G5", want: `"realm"`,
		},
		{
			name:  "G5 a two-word headword",
			facts: base,
			l:     Layers{What: Layer{"x"}, Means: Layer{"It locked a storage deposit."}, Matters: Layer{"Anyone can use it."}},
			check: "G5", want: "storage deposit",
		},
		{
			name:  "G7 a headline that would truncate on the card",
			facts: base,
			l: Layers{What: Layer{"x"}, Means: Layer{"An app called hello was published by somebody, and it is worth a look if you have a spare moment."},
				Matters: Layer{"Anyone can use it."}},
			check: "G7", want: "the budget is 90",
		},
		{
			name:  "G7 a second sentence in layer 2",
			facts: base,
			l:     Layers{What: Layer{"x"}, Means: Layer{"An app was published. It is called hello."}, Matters: Layer{"Anyone can use it."}},
			check: "G7", want: "layer 2 is exactly one",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vs := Ground(tt.facts, tt.l, headwords)
			if len(vs) == 0 {
				t.Fatalf("passed, want a %s violation mentioning %q", tt.check, tt.want)
			}
			for _, v := range vs {
				if v.Check == tt.check && strings.Contains(v.Msg, tt.want) {
					return
				}
			}
			t.Fatalf("got %s, want a %s violation mentioning %q", checks(vs), tt.check, tt.want)
		})
	}
}

// The rule the spec states in one line beside its own example: the sentence is
// legal because a fact licenses it, and illegal the moment that fact is gone.
// Same prose, two fact sets, two answers. Nothing else in this package proves
// the checks are reading the facts rather than the words.
func TestTheSameSentenceIsLegalOnlyWhileAFactLicensesIt(t *testing.T) {
	l := Layers{
		What:    Layer{"g1r6lu has no earlier successful MsgAddPackage on mainnet."},
		Means:   Layer{"Somebody published on the chain for the first time."},
		Matters: Layer{"A new builder arrived. Ten people have ever published on this chain."},
	}

	with := Facts{"first_ever": true, "distinct_deployers": 10}
	if vs := Ground(with, l, headwords); len(vs) > 0 {
		t.Fatalf("with the licensing facts it should pass: %s", checks(vs))
	}

	without := Facts{"first_ever": true}
	vs := Ground(without, l, headwords)
	if len(vs) == 0 {
		t.Fatal("without distinct_deployers, 'ten people have ever published' has nothing behind it and must fail")
	}

	// And the first half stays legal, because first_ever still licenses it.
	onlyFirst := Layers{What: l.What, Means: l.Means, Matters: Layer{"A new builder arrived."}}
	if vs := Ground(Facts{"first_ever": true}, onlyFirst, headwords); len(vs) > 0 {
		t.Fatalf("first_ever licenses 'for the first time': %s", checks(vs))
	}
}

// first_ever: false must not license "first". A boolean read as "the key is
// present" rather than "the key is true" would pass every deploy event on a
// chain, which is the loudest possible version of this failure.
func TestAFalseLicenceIsNotALicence(t *testing.T) {
	l := Layers{
		What:    Layer{"MsgAddPackage published gno.land/r/moul/hello."},
		Means:   Layer{"Somebody published an app."},
		Matters: Layer{"It is their first one."},
	}
	if vs := Ground(Facts{"first_ever": false}, l, headwords); len(vs) == 0 {
		t.Fatal("first_ever=false licensed 'first'")
	}
	if vs := Ground(Facts{"first_ever": true}, l, headwords); len(vs) > 0 {
		t.Fatalf("first_ever=true should license it: %s", checks(vs))
	}
}

// The declared conversions, and the one that is declared as needing a fact.
func TestDeclaredConversions(t *testing.T) {
	tests := []struct {
		name  string
		facts Facts
		text  string
		ok    bool
	}{
		{"ugnot to GNOT", Facts{"amount_ugnot": 1500000000}, "It locked 1500 GNOT.", true},
		{"ugnot to GNOT, one decimal", Facts{"amount_ugnot": 1550000}, "It locked 1.6 GNOT.", true},
		{"ugnot read as GNOT", Facts{"amount_ugnot": 1500000000}, "It locked 1500000000 GNOT.", true},
		{"a rate that is not the fact", Facts{"amount_ugnot": 1500000000}, "It locked 15 GNOT.", false},
		{"seconds to minutes", Facts{"wait_seconds": 120}, "It waited 2 minutes.", true},
		{"blocks to seconds, with the rate", Facts{"wait_blocks": 4, "block_seconds": 3.3}, "It waited 13 seconds.", true},
		{"blocks to seconds, without the rate", Facts{"wait_blocks": 4}, "It waited 13 seconds.", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := Layers{What: Layer{"x"}, Means: Layer{"An app was published."}, Matters: Layer{tt.text}}
			vs := Ground(tt.facts, l, headwords)
			if tt.ok && len(vs) > 0 {
				t.Fatalf("rejected a declared conversion: %s", checks(vs))
			}
			if !tt.ok && len(vs) == 0 {
				t.Fatal("accepted a number the conversion table does not produce")
			}
		})
	}
}

// A name with digits in it is an entity, not a figure.
//
// Mainnet has nym-polux007 and nym-thegnomic001 today, and before
// maskNamedEntities the scanner pulled "007" out of the middle of the word and
// reported "the number 007 is in the text and in no fact", because
// groundedNumbers only ever collects numeric fact values. That false positive
// blocked the namespace.registered kind outright: every rendering of it names
// the name.
func TestANameWithDigitsIsNotAFigure(t *testing.T) {
	facts := Facts{"name": "nym-polux007", "address": "g1l0tdzdyxsayestm8lu95xehgtc0s825pxu583g"}
	l := Layers{
		What:    Layer{"r/sys/namereg recorded the name nym-polux007."},
		Means:   Layer{"@nym-polux007 claimed their name on chain."},
		Matters: Layer{"Packages published by that address can live under nym-polux007 from now on."},
	}
	if v := Ground(facts, l, nil); len(v) > 0 {
		for _, one := range v {
			t.Errorf("a name the facts contain verbatim was rejected: %s", one.Error())
		}
	}
}

// The masking must not become a way to smuggle a figure in. A string fact whose
// whole value is digits is a number written as a string, and masking it would
// let a template state any figure it liked by putting it in a string field
// first.
func TestMaskingDoesNotLaunderAFigure(t *testing.T) {
	facts := Facts{"looks_like_a_name": "999"}
	l := Layers{
		What:    Layer{"something happened."},
		Means:   Layer{"999 things happened."},
		Matters: Layer{"That is a lot of things."},
	}
	v := Ground(facts, l, nil)
	found := false
	for _, one := range v {
		if one.Check == "G1" {
			found = true
		}
	}
	if !found {
		t.Error("a digits-only string fact laundered a number past G1")
	}
}
