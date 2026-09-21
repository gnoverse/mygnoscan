// Package registry holds the curated data that cannot be derived from a chain:
// what an address is called, which token is the real one, and what an app does.
//
// It is data rather than code on purpose. The address names used to live in a
// `const KNOWN_ADDRS` inside a 9,600-line HTML file, which meant contributing a
// name required editing the frontend. Fifteen entries after months of use was
// the symptom. Here it is three JSON files, embedded at build time, and adding
// one is a pull request against a file.
//
// See registry/README.md for the contribution rules; this file enforces them.
package registry

import (
	"embed"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

//go:embed all:data
var files embed.FS

// Provenance kinds. The split is the point of the package: a name a human
// vouched for, a fact the chain proves, a claim the subject made about itself
// and a heuristic are four different things, and an explorer that renders them
// identically is asking readers to trust the weakest of them.
const (
	// KindCurated is a human's assertion, made in a merged pull request.
	KindCurated = "curated"
	// KindDerived is proved from chain data and recomputed live. Never stored
	// here; named because the explorer merges the two sources and the frontend
	// needs the constant.
	KindDerived = "derived"
	// KindDeclared is what the subject says about itself: r/sys/users, a
	// valoper moniker. Attacker-controlled by construction.
	KindDeclared = "declared"
	// KindInferred is a heuristic over observed behaviour, and always carries
	// its evidence.
	KindInferred = "inferred"
)

// Entry is one curated address.
type Entry struct {
	Label string `json:"label"`
	Kind  string `json:"kind"`
	// Why is the evidence. Required for everything except a curated entry,
	// where the pull request itself is the evidence.
	Why string `json:"why,omitempty"`
	// Checked dates a measurement, so a reader can weigh how old it is.
	Checked string `json:"checked,omitempty"`
}

// Token is one GRC20, keyed by the full `<path>.<name>.<id>` triple that GRC20
// events emit. One realm can expose several tokens, so a bare path is not a key.
type Token struct {
	Symbol   string `json:"symbol"`
	Name     string `json:"name,omitempty"`
	Decimals int    `json:"decimals,omitempty"`
	// Verified says a human confirmed this is the token it claims to be. Anyone
	// can deploy a realm called `gns`, so this is the only thing separating the
	// real one from a lookalike.
	Verified bool   `json:"verified"`
	Why      string `json:"why,omitempty"`
	Checked  string `json:"checked,omitempty"`
}

// App is one entry in the directory.
type App struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	Category    string `json:"category"`
	Description string `json:"description"`
	URL         string `json:"url,omitempty"`
}

// Registry is the parsed whole.
type Registry struct {
	Addresses map[string]Entry `json:"addresses"`
	Tokens    map[string]Token `json:"tokens"`
	Apps      []App            `json:"apps"`
}

var (
	bech32Addr = regexp.MustCompile(`^g1[0-9a-z]{38}$`)
	isoDate    = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	realmPath  = regexp.MustCompile(`^gno\.land/[rp]/`)
)

// Load parses the embedded files and validates them.
//
// Returns an error rather than panicking so a caller can decide: the server
// treats a broken registry as fatal at startup, because shipping with labels
// silently missing is worse than not starting, and the test suite reports it
// as a test failure on the contributor's pull request.
func Load() (*Registry, error) {
	var (
		reg Registry
		err error
	)
	if reg.Addresses, err = loadAddresses(); err != nil {
		return nil, err
	}
	if reg.Tokens, err = loadTokens(); err != nil {
		return nil, err
	}
	if reg.Apps, err = loadApps(); err != nil {
		return nil, err
	}
	return &reg, nil
}

func loadAddresses() (map[string]Entry, error) {
	var doc struct {
		Addresses map[string]Entry `json:"addresses"`
	}
	if err := readJSON("data/addresses.json", &doc); err != nil {
		return nil, err
	}
	for addr, e := range doc.Addresses {
		if !bech32Addr.MatchString(addr) {
			return nil, fmt.Errorf("addresses.json: %q is not a bech32 address", addr)
		}
		if err := validateEntry(addr, e); err != nil {
			return nil, err
		}
	}
	return doc.Addresses, nil
}

func validateEntry(addr string, e Entry) error {
	if strings.TrimSpace(e.Label) == "" {
		return fmt.Errorf("addresses.json: %s has no label", addr)
	}
	switch e.Kind {
	case KindCurated, KindDeclared, KindInferred:
	case KindDerived:
		// Derived labels are computed live from deploy history. One written
		// down here is a copy that stops being true the moment the chain moves,
		// which is the exact failure this package exists to avoid.
		return fmt.Errorf("addresses.json: %s is marked %q, which is computed live and must not be stored", addr, KindDerived)
	default:
		return fmt.Errorf("addresses.json: %s has unknown kind %q", addr, e.Kind)
	}
	// The rule from the README, enforced: anything that is not a human vouching
	// in a pull request has to show its evidence.
	if e.Kind != KindCurated && strings.TrimSpace(e.Why) == "" {
		return fmt.Errorf("addresses.json: %s is %q and must explain why", addr, e.Kind)
	}
	if e.Checked != "" {
		if !isoDate.MatchString(e.Checked) {
			return fmt.Errorf("addresses.json: %s has checked %q, want YYYY-MM-DD", addr, e.Checked)
		}
		if _, err := time.Parse("2006-01-02", e.Checked); err != nil {
			return fmt.Errorf("addresses.json: %s has checked %q, which is not a date", addr, e.Checked)
		}
	}
	return nil
}

func loadTokens() (map[string]Token, error) {
	var doc struct {
		Tokens map[string]Token `json:"tokens"`
	}
	if err := readJSON("data/tokens.json", &doc); err != nil {
		return nil, err
	}
	for key, tok := range doc.Tokens {
		if !realmPath.MatchString(key) {
			return nil, fmt.Errorf("tokens.json: %q is not a gno.land path", key)
		}
		// The key is the triple GRC20 events carry, not a bare path: one realm
		// can expose several tokens and a path alone would collide.
		if strings.Count(strings.TrimPrefix(key, "gno.land/"), ".") < 2 {
			return nil, fmt.Errorf("tokens.json: %q must be <path>.<name>.<id>, the key GRC20 events emit", key)
		}
		if strings.TrimSpace(tok.Symbol) == "" {
			return nil, fmt.Errorf("tokens.json: %s has no symbol", key)
		}
		if tok.Verified && strings.TrimSpace(tok.Why) == "" {
			return nil, fmt.Errorf("tokens.json: %s is verified and must explain why", key)
		}
		if tok.Decimals < 0 || tok.Decimals > 30 {
			return nil, fmt.Errorf("tokens.json: %s has implausible decimals %d", key, tok.Decimals)
		}
	}
	return doc.Tokens, nil
}

func loadApps() ([]App, error) {
	var doc struct {
		Apps []App `json:"apps"`
	}
	if err := readJSON("data/apps.json", &doc); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, a := range doc.Apps {
		switch {
		case !realmPath.MatchString(a.Path):
			return nil, fmt.Errorf("apps.json: %q is not a gno.land path", a.Path)
		case seen[a.Path]:
			return nil, fmt.Errorf("apps.json: %s is listed twice", a.Path)
		case strings.TrimSpace(a.Name) == "":
			return nil, fmt.Errorf("apps.json: %s has no name", a.Path)
		case strings.TrimSpace(a.Category) == "":
			return nil, fmt.Errorf("apps.json: %s has no category", a.Path)
		case strings.TrimSpace(a.Description) == "":
			// A directory entry that does not say what the thing does is a link
			// list, which the realm list already is.
			return nil, fmt.Errorf("apps.json: %s has no description", a.Path)
		}
		seen[a.Path] = true
	}
	// Sorted here rather than in the file, so a contributor adding an entry
	// does not have to find the right line and a reviewer sees a one-line diff.
	sort.Slice(doc.Apps, func(i, j int) bool {
		if doc.Apps[i].Category != doc.Apps[j].Category {
			return doc.Apps[i].Category < doc.Apps[j].Category
		}
		return doc.Apps[i].Name < doc.Apps[j].Name
	})
	return doc.Apps, nil
}

func readJSON(name string, out any) error {
	b, err := files.ReadFile(name)
	if err != nil {
		return fmt.Errorf("registry: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	// Unknown fields are a typo, not an extension: a `lable` key would
	// otherwise be accepted and the entry would silently render blank.
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("registry: %s: %w", name, err)
	}
	return nil
}
