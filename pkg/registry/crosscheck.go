package registry

import (
	"regexp"
	"sort"
	"strings"
)

// Cross-checking the two curated lists against each other.
//
// There are two hand-written answers to "what is being built on gno.land" and
// they barely overlap. awesome-gno holds what a chain index physically cannot
// see: wallets, editors, a language server, SDKs, workshops, none of which is a
// realm. apps.json holds what only an indexer can qualify: a realm path, and
// the traffic behind it. Each is therefore most useful as the other's worklist,
// and nobody maintains both, which is why each has entries the other has never
// heard of.
//
// Both directions are suggestions and neither is automatic. A realm is not
// awesome because it is busy, and an awesome project does not belong in a realm
// directory unless it *is* a realm. The page ranks and links; a human still
// writes the sentence and opens the pull request.

var nameJunk = regexp.MustCompile(`[^a-z0-9]+`)

// normName is deliberately blunt: case and punctuation are noise ("GnoSwap" and
// "Gnoswap" are one project), and anything cleverer starts matching things that
// are not the same. "Kourt v3" must not match "Kourt", because awesome-gno's
// Kourt entry points at the v1 realm and the two are separate deployments with
// different economics.
func normName(s string) string { return nameJunk.ReplaceAllString(strings.ToLower(s), "") }

// AwesomeRef is where in the community list a thing was found.
type AwesomeRef struct {
	Name    string `json:"name"`
	Section string `json:"section"`
	Slug    string `json:"slug"`
	URL     string `json:"url,omitempty"`
	// Path is set when the awesome entry itself named a realm, which is what
	// makes the match provable rather than a guess about two similar names.
	Path string `json:"path,omitempty"`
}

// MissingFromAwesome returns the directory entries that awesome-gno does not
// name, in the directory's own order.
//
// This is the invitation, made specific. "Contribute to awesome-gno" is a link
// nobody clicks; "these eight realms you can see the traffic for are not on the
// community list, here is the one that opens a pull request" is a task.
func (r *Registry) MissingFromAwesome() []App {
	if r.Awesome == nil {
		return nil
	}
	byPath, byName := r.awesomeIndex()
	out := []App{}
	for _, a := range r.Apps {
		if _, ok := byPath[a.Path]; ok {
			continue
		}
		if _, ok := byName[normName(a.Name)]; ok {
			continue
		}
		out = append(out, a)
	}
	return out
}

// AwesomeInDirectory maps a directory path to the awesome-gno entry that also
// names it, so a card can say "the community list has this too" and link there.
func (r *Registry) AwesomeInDirectory() map[string]AwesomeRef {
	out := map[string]AwesomeRef{}
	if r.Awesome == nil {
		return out
	}
	byPath, byName := r.awesomeIndex()
	for _, a := range r.Apps {
		if ref, ok := byPath[a.Path]; ok {
			out[a.Path] = ref
			continue
		}
		if ref, ok := byName[normName(a.Name)]; ok {
			out[a.Path] = ref
		}
	}
	return out
}

// MissingFromDirectory returns the awesome-gno entries that name a realm this
// directory does not describe. The other direction of the same gap, and the
// cheaper one to close: somebody has already vouched for the project in public,
// so the only thing missing here is the sentence and the path.
//
// Entries from an archived section are excluded: the community has said those
// are no longer current, and importing them would be reviving something its own
// maintainers retired.
func (r *Registry) MissingFromDirectory() []AwesomeRef {
	if r.Awesome == nil {
		return nil
	}
	listed := map[string]bool{}
	for _, a := range r.Apps {
		listed[a.Path] = true
	}
	out := []AwesomeRef{}
	for _, s := range r.Awesome.Sections {
		if s.Archived {
			continue
		}
		for _, e := range s.Entries {
			if e.Path == "" || listed[e.Path] {
				continue
			}
			listed[e.Path] = true
			out = append(out, AwesomeRef{Name: e.Name, Section: s.Title, Slug: s.Slug, URL: e.URL, Path: e.Path})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// awesomeIndex builds both lookups in one pass. Archived entries are indexed
// too: a directory entry the community list mentions only in its archive is
// still mentioned, and telling someone to go add it would be wrong.
func (r *Registry) awesomeIndex() (byPath, byName map[string]AwesomeRef) {
	byPath, byName = map[string]AwesomeRef{}, map[string]AwesomeRef{}
	for _, s := range r.Awesome.Sections {
		for _, e := range s.Entries {
			ref := AwesomeRef{Name: e.Name, Section: s.Title, Slug: s.Slug, URL: e.URL, Path: e.Path}
			if e.Path != "" {
				if _, seen := byPath[e.Path]; !seen {
					byPath[e.Path] = ref
				}
			}
			if n := normName(e.Name); n != "" {
				if _, seen := byName[n]; !seen {
					byName[n] = ref
				}
			}
		}
	}
	return byPath, byName
}

// DirectoryByAwesomeName inverts AwesomeInDirectory: the community list's own
// spelling of an entry, mapped to the realm path this directory describes it
// under.
//
// The inversion is done here rather than in the browser because the match is
// made by normalising names, and a second normaliser written in JavaScript is a
// second normaliser to keep in step. Handing the page a lookup keyed by the
// exact string it is already rendering leaves it nothing to get wrong.
func (r *Registry) DirectoryByAwesomeName() map[string]string {
	out := map[string]string{}
	for path, ref := range r.AwesomeInDirectory() {
		if ref.Name != "" {
			out[ref.Name] = path
		}
	}
	return out
}
