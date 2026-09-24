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
	// Checked dates the description, for the same reason Entry and Token carry
	// it: an app blurb is mostly durable ("what this realm is for") but usually
	// smuggles in one fact that is not, like a minimum balance a board asks of
	// a poster, or which generation a front-end currently serves. Those go
	// stale silently, because nothing about the page says how old they are.
	Checked string `json:"checked,omitempty"`
	// Supersedes names an older generation this entry replaces.
	//
	// Two live deployments of the same idea is the normal state of a chain
	// nobody can delete from, and showing both as peers is how a directory
	// sends people to last year's version. Kourt is the worked example: v1 and
	// v3 are both live, both busy, and only one is the one to open.
	Supersedes []string `json:"supersedes,omitempty"`
	// Covers names the realms that are parts of this same app rather than apps
	// of their own, as an exact path or a `/*` prefix.
	//
	// The difference from Supersedes is what a reader is being told. A
	// superseded realm is the same app at an earlier date and the answer is
	// "open the new one"; a covered realm is a live, load-bearing piece of the
	// app on this card and the answer is "this is already what you are looking
	// at". GnoSwap is the worked example: router, gns, position, staker, gnft
	// and gov/staker are all busy, all current, and all one DEX, and a hub that
	// ranks them as peers tells a visitor there are six of it.
	//
	// A prefix is allowed because the alternative is a list that goes stale the
	// next time somebody deploys, silently and in the direction of showing more
	// cards. `gno.land/r/gnoswap/*` covers a realm nobody has written yet.
	Covers []string `json:"covers,omitempty"`
}

// Registry is the parsed whole.
type Registry struct {
	Addresses map[string]Entry `json:"addresses"`
	Tokens    map[string]Token `json:"tokens"`
	Apps      []App            `json:"apps"`
	// Awesome is the vendored snapshot of gnoverse/awesome-gno. It is the one
	// part of this package nobody here writes by hand: it is generated from
	// somebody else's list, and the whole point is that the community edits it
	// there rather than here. See awesome.go.
	Awesome *Awesome `json:"awesome"`
	// Moderation is the skip list: the one lever here that removes rather than
	// adds. See moderation.go.
	Moderation *Moderation `json:"moderation"`
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
	if reg.Awesome, err = loadAwesome(); err != nil {
		return nil, err
	}
	if reg.Moderation, err = loadModeration(); err != nil {
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
	return validateChecked("addresses.json", addr, e.Checked)
}

// validateChecked is shared by every file, because a date that is only
// validated in one of them is a date that is wrong in the other two.
func validateChecked(file, subject, checked string) error {
	if checked == "" {
		return nil
	}
	if !isoDate.MatchString(checked) {
		return fmt.Errorf("%s: %s has checked %q, want YYYY-MM-DD", file, subject, checked)
	}
	if _, err := time.Parse("2006-01-02", checked); err != nil {
		return fmt.Errorf("%s: %s has checked %q, which is not a date", file, subject, checked)
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
	if err := validateTokens(doc.Tokens); err != nil {
		return nil, err
	}
	return doc.Tokens, nil
}

func validateTokens(tokens map[string]Token) error {
	for key, tok := range tokens {
		if !realmPath.MatchString(key) {
			return fmt.Errorf("tokens.json: %q is not a gno.land path", key)
		}
		// Keys mirror whatever the Transfer event emits. That is usually the
		// triple, because one realm can expose several tokens, but two live
		// mainnet tokens emit a bare symbol instead (measured 2026-09-20), so
		// this checks the shape is plausible rather than mandating one.
		if strings.HasSuffix(key, ".") || strings.Contains(key, "..") {
			return fmt.Errorf("tokens.json: %q is not a usable token key", key)
		}
		if strings.TrimSpace(tok.Symbol) == "" {
			return fmt.Errorf("tokens.json: %s has no symbol", key)
		}
		if tok.Verified && strings.TrimSpace(tok.Why) == "" {
			return fmt.Errorf("tokens.json: %s is verified and must explain why", key)
		}
		if tok.Decimals < 0 || tok.Decimals > 30 {
			return fmt.Errorf("tokens.json: %s has implausible decimals %d", key, tok.Decimals)
		}
		if err := validateChecked("tokens.json", key, tok.Checked); err != nil {
			return err
		}
	}
	return nil
}

func loadApps() ([]App, error) {
	var doc struct {
		Apps []App `json:"apps"`
	}
	if err := readJSON("data/apps.json", &doc); err != nil {
		return nil, err
	}
	if err := validateApps(doc.Apps); err != nil {
		return nil, err
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

func validateApps(apps []App) error {
	seen := map[string]bool{}
	for _, a := range apps {
		// An entry that only asserts a relation is complete without a name, a
		// category or a sentence.
		//
		// "v3 replaces v2" is a fact about two deploys, checkable from the
		// chain. Requiring a description alongside it would force a contributor
		// to invent one about somebody else's realm in order to state it, and
		// this package's whole rule is that a default must be the project's own
		// words. Three bubblerumble generations ranked as peers on mainnet is
		// the case that found this.
		relationOnly := len(a.Supersedes) > 0 &&
			strings.TrimSpace(a.Name) == "" &&
			strings.TrimSpace(a.Category) == "" &&
			strings.TrimSpace(a.Description) == ""

		switch {
		case !realmPath.MatchString(a.Path):
			return fmt.Errorf("apps.json: %q is not a gno.land path", a.Path)
		case seen[a.Path]:
			return fmt.Errorf("apps.json: %s is listed twice", a.Path)
		case relationOnly:
			// Nothing further to check; the supersedes paths are validated below.
		case strings.TrimSpace(a.Name) == "":
			return fmt.Errorf("apps.json: %s has no name", a.Path)
		case strings.TrimSpace(a.Category) == "":
			return fmt.Errorf("apps.json: %s has no category", a.Path)
		case strings.TrimSpace(a.Description) == "":
			// A directory entry that does not say what the thing does is a link
			// list, which the realm list already is.
			return fmt.Errorf("apps.json: %s has no description", a.Path)
		}
		if err := validateChecked("apps.json", a.Path, a.Checked); err != nil {
			return err
		}
		for _, old := range a.Supersedes {
			if !realmPath.MatchString(old) {
				return fmt.Errorf("apps.json: %s supersedes %q, which is not a gno.land path", a.Path, old)
			}
			if old == a.Path {
				return fmt.Errorf("apps.json: %s supersedes itself", a.Path)
			}
		}
		for _, part := range a.Covers {
			bare := strings.TrimSuffix(part, "/*")
			switch {
			case !realmPath.MatchString(bare):
				return fmt.Errorf("apps.json: %s covers %q, which is not a gno.land path or prefix", a.Path, part)
			case part == a.Path:
				return fmt.Errorf("apps.json: %s covers itself", a.Path)
			}
		}
		seen[a.Path] = true
	}
	return nil
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
