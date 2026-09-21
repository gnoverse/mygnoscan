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
	// All three files, not just addresses: the field means the same thing
	// everywhere it appears, so a guard on one of them is a guard on none.
	dated := map[string]string{}
	for addr, e := range reg.Addresses {
		dated["addresses.json: "+addr] = e.Checked
	}
	for key, tok := range reg.Tokens {
		dated["tokens.json: "+key] = tok.Checked
	}
	for _, a := range reg.Apps {
		dated["apps.json: "+a.Path] = a.Checked
	}
	for subject, checked := range dated {
		if checked == "" {
			continue
		}
		when, err := time.Parse("2006-01-02", checked)
		if err != nil {
			t.Errorf("%s: %v", subject, err)
			continue
		}
		if when.After(tomorrow) {
			t.Errorf("%s was checked %s, which is in the future", subject, checked)
		}
	}
}

// An app description is mostly durable, but the volatile fact inside one (a
// minimum balance, which generation a front-end serves) is exactly what a
// reader acts on, so the entries carrying one have to say how old they are.
func TestAppValidationRejects(t *testing.T) {
	ok := App{Path: "gno.land/r/x/y", Name: "Y", Category: "content", Description: "what it is"}
	mutate := func(f func(*App)) App {
		a := ok
		f(&a)
		return a
	}
	tests := []struct {
		name string
		app  App
		want string
	}{
		{"not a path", mutate(func(a *App) { a.Path = "example.com/x" }), "not a gno.land path"},
		{"no name", mutate(func(a *App) { a.Name = " " }), "has no name"},
		{"no category", mutate(func(a *App) { a.Category = "" }), "has no category"},
		{"no description", mutate(func(a *App) { a.Description = "" }), "has no description"},
		{"a malformed date", mutate(func(a *App) { a.Checked = "yesterday" }), "want YYYY-MM-DD"},
		{"a date that is not one", mutate(func(a *App) { a.Checked = "2026-13-45" }), "not a date"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateApps([]App{tt.app})
			if err == nil {
				t.Fatalf("accepted %+v, want a rejection mentioning %q", tt.app, tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}

	if err := validateApps([]App{ok, ok}); err == nil || !strings.Contains(err.Error(), "listed twice") {
		t.Errorf("a duplicate path gave %v, want it rejected as listed twice", err)
	}
	if err := validateApps([]App{mutate(func(a *App) { a.Checked = "2026-09-21" })}); err != nil {
		t.Errorf("rejected a well-formed dated entry: %v", err)
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
