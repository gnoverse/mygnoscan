package registry

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestLoadShippedData is the gate on every registry pull request: the files in
// data/ have to parse and satisfy every rule the README states. A contributor
// adding a name sees this fail rather than shipping an entry that renders blank.
func TestLoadShippedData(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatalf("the shipped registry does not load: %v", err)
	}
	if len(reg.Addresses) == 0 || len(reg.Apps) == 0 || len(reg.Tokens) == 0 {
		t.Fatalf("registry looks empty: %d addresses, %d apps, %d tokens",
			len(reg.Addresses), len(reg.Apps), len(reg.Tokens))
	}

	// The GPAO oracle is the entry this registry was built to make possible:
	// the sole address allowed to enable a parked package, which rendered as a
	// bare bech32 string everywhere it appeared.
	oracle, ok := reg.Addresses["g1yaaa6rcp4ew5yjzdj4yms596wx2dtrj3a86704"]
	if !ok {
		t.Fatal("the gpao oracle is not labelled")
	}
	if oracle.Kind != KindCurated || !strings.Contains(oracle.Why, "pkg_approvers") {
		t.Errorf("oracle = %+v, want a curated entry citing the param that proves it", oracle)
	}
}

// Every rule in the README is enforced here rather than trusted, because a
// registry whose rules are only documented is a registry that drifts.
func TestValidationRejects(t *testing.T) {
	const addr = "g1yaaa6rcp4ew5yjzdj4yms596wx2dtrj3a86704"
	tests := []struct {
		name  string
		entry Entry
		want  string
	}{
		{"no label", Entry{Kind: KindCurated}, "no label"},
		{"unknown kind", Entry{Label: "@x", Kind: "vibes"}, "unknown kind"},
		{
			// The rule that keeps the registry from becoming a stale mirror of
			// the chain: anything provable is computed live instead.
			name:  "a derived label may not be stored",
			entry: Entry{Label: "@x", Kind: KindDerived, Why: "because"},
			want:  "must not be stored",
		},
		{"inferred without evidence", Entry{Label: "@x", Kind: KindInferred}, "must explain why"},
		{"declared without evidence", Entry{Label: "@x", Kind: KindDeclared}, "must explain why"},
		{"a malformed date", Entry{Label: "@x", Kind: KindCurated, Checked: "yesterday"}, "want YYYY-MM-DD"},
		{"a date that is not one", Entry{Label: "@x", Kind: KindCurated, Checked: "2026-13-45"}, "not a date"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateEntry(addr, tt.entry)
			if err == nil {
				t.Fatalf("accepted %+v, want a rejection mentioning %q", tt.entry, tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestValidationAccepts(t *testing.T) {
	const addr = "g1yaaa6rcp4ew5yjzdj4yms596wx2dtrj3a86704"
	for _, e := range []Entry{
		{Label: "@moul", Kind: KindCurated},
		{Label: "@faucet", Kind: KindInferred, Why: "3683 sends, never calls a realm", Checked: "2026-08-28"},
		{Label: "@someone", Kind: KindDeclared, Why: "registered in r/sys/users"},
	} {
		if err := validateEntry(addr, e); err != nil {
			t.Errorf("rejected %+v: %v", e, err)
		}
	}
}

// A typo in a key would otherwise be accepted silently and the entry would
// render blank, which is the worst of both: present in the file, absent on the
// page.
func TestUnknownFieldsAreATypo(t *testing.T) {
	var out struct {
		Addresses map[string]Entry `json:"addresses"`
	}
	dec := json.NewDecoder(strings.NewReader(`{"addresses":{"g1x":{"lable":"@x"}}}`))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err == nil {
		t.Error("a misspelled key was accepted")
	}
}

// Dates are checked for plausibility, not just shape: a registry full of
// measurements from the future is one nobody can weigh.
func TestShippedDatesAreNotInTheFuture(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	tomorrow := time.Now().UTC().AddDate(0, 0, 1)
	for addr, e := range reg.Addresses {
		if e.Checked == "" {
			continue
		}
		when, err := time.Parse("2006-01-02", e.Checked)
		if err != nil {
			t.Errorf("%s: %v", addr, err)
			continue
		}
		if when.After(tomorrow) {
			t.Errorf("%s was checked %s, which is in the future", addr, e.Checked)
		}
	}
}

// Token keys mirror whatever the Transfer event emits, verbatim.
//
// That is usually the `<path>.<name>.<id>` triple, because a realm can expose
// several tokens and the event's own pkg_path is the grc20 library for all of
// them. It is not guaranteed: two live mainnet tokens emit a bare symbol
// instead, measured 2026-09-20. So the rule is that a key has to be usable as
// a key, not that it has a particular shape.
func TestTokenKeysAreUsable(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for key := range reg.Tokens {
		if key == "" || strings.HasSuffix(key, ".") || strings.Contains(key, "..") {
			t.Errorf("token key %q is not usable", key)
		}
	}
}

func TestTokenValidationRejectsUnusableKeys(t *testing.T) {
	for _, key := range []string{"gno.land/r/x/y.", "gno.land/r/x..y", "notapath"} {
		var doc struct {
			Tokens map[string]Token `json:"tokens"`
		}
		doc.Tokens = map[string]Token{key: {Symbol: "X"}}
		if err := validateTokens(doc.Tokens); err == nil {
			t.Errorf("accepted unusable key %q", key)
		}
	}
}

// Sorting happens on load so a contributor can append to the file and still get
// a one-line diff, while the page stays ordered.
func TestAppsAreSortedOnLoad(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(reg.Apps); i++ {
		prev, cur := reg.Apps[i-1], reg.Apps[i]
		if prev.Category > cur.Category ||
			(prev.Category == cur.Category && prev.Name > cur.Name) {
			t.Errorf("apps are not sorted: %q/%q before %q/%q",
				prev.Category, prev.Name, cur.Category, cur.Name)
		}
	}
}
