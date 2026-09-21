package httpapi

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The named-constructor table in govdao_audit.go is a transcription of
// constants that live in someone else's repository, so it is the one thing
// in this package that can become wrong without anything here changing.
//
// Two layers guard it, because neither alone is enough:
//
//   - At runtime, a params request constructor that decodes to nothing
//     raises the `request-not-decoded` signal (see buildProposalSignals), so
//     a reader looking at an undecodable proposal is told so rather than
//     shown a page with a section quietly missing. That covers a renamed or
//     newly added factory.
//   - Here, when a gno monorepo checkout is available, the table is diffed
//     against r/sys/params itself. That covers the worse case: a factory
//     that still exists under the same name but now writes a different key,
//     where the runtime guard sees nothing wrong and the page shows a
//     confidently wrong diff.
//
// The second layer needs a checkout, so it skips when there is none. Point
// GNO_MONOREPO at one to run it:
//
//	GNO_MONOREPO=~/p/gh/gnolang/gno go test ./pkg/httpapi/ -run ParamTable

// Invariants that hold with no checkout at all.
func TestNamedParamRequestsWellFormed(t *testing.T) {
	for fn, specs := range namedParamRequests {
		if len(specs) == 0 {
			t.Errorf("%s: no specs", fn)
		}
		for i, spec := range specs {
			if !paramKeyRe.MatchString(spec.key) {
				t.Errorf("%s[%d]: key %q would be refused by fetchChainParam", fn, i, spec.key)
			}
			switch spec.verb {
			case "set", "add", "remove":
			default:
				t.Errorf("%s[%d]: verb %q is not one applyParamVerb handles", fn, i, spec.verb)
			}
			if spec.argIndex == 0 && spec.constVal == nil {
				t.Errorf("%s[%d]: argIndex 0 means a constant value, but constVal is nil", fn, i)
			}
			if spec.argIndex != 0 && spec.constVal != nil {
				t.Errorf("%s[%d]: constVal is set but argIndex %d says to read an argument", fn, i, spec.argIndex)
			}
		}
	}
}

// keyConstRe finds `name = "value"` in a const block, which is how
// r/sys/params spells every parameter name and module prefix.
var keyConstRe = regexp.MustCompile(`(?m)^\s*(\w+)\s*=\s*"([^"]*)"`)

// setterCallRe finds the prms.Set/UpdateSysParam* call inside a factory,
// capturing its first three arguments: module, submodule, name.
var setterCallRe = regexp.MustCompile(`prms\.(?:Set|Update)SysParam\w+\(\s*([\w"]+)\s*,\s*([\w"]+)\s*,\s*([\w"]+)\s*[,)]`)

// funcStartRe finds a factory's declaration.
var funcStartRe = regexp.MustCompile(`(?m)^func\s+(\w+)\(cur realm`)

func TestNamedParamRequestsMatchRealmSource(t *testing.T) {
	root := os.Getenv("GNO_MONOREPO")
	if root == "" {
		t.Skip("GNO_MONOREPO not set; see this file's comment. The runtime `request-not-decoded` signal covers the renamed-factory case without a checkout.")
	}
	dir := filepath.Join(root, "examples", "gno.land", "r", "sys", "params")
	files, err := filepath.Glob(filepath.Join(dir, "*.gno"))
	if err != nil || len(files) == 0 {
		t.Skipf("no r/sys/params sources under %s", dir)
	}

	consts := map[string]string{}
	var all strings.Builder
	for _, f := range files {
		if strings.HasSuffix(f, "_test.gno") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		all.Write(b)
		all.WriteString("\n")
		for _, m := range keyConstRe.FindAllStringSubmatch(string(b), -1) {
			consts[m[1]] = m[2]
		}
	}
	src := all.String()

	// resolve turns an argument as written into its value: a quoted literal
	// is itself, an identifier is looked up in the const map.
	resolve := func(arg string) (string, bool) {
		arg = strings.TrimSpace(arg)
		if strings.HasPrefix(arg, `"`) {
			return strings.Trim(arg, `"`), true
		}
		v, ok := consts[arg]
		return v, ok
	}

	// Which factories exist in the source at all.
	existing := map[string]bool{}
	for _, m := range funcStartRe.FindAllStringSubmatch(src, -1) {
		existing[m[1]] = true
	}

	for fn, specs := range namedParamRequests {
		if !existing[fn] {
			t.Errorf("namedParamRequests has %s, which no longer exists in r/sys/params; the table is stale", fn)
			continue
		}
		// Slice the factory's body: from its declaration to the next one.
		start := strings.Index(src, "func "+fn+"(cur realm")
		if start < 0 {
			continue
		}
		body := src[start:]
		if next := strings.Index(body[1:], "\nfunc "); next >= 0 {
			body = body[:next+1]
		}
		var keys []string
		for _, m := range setterCallRe.FindAllStringSubmatch(body, -1) {
			mod, ok1 := resolve(m[1])
			sub, ok2 := resolve(m[2])
			name, ok3 := resolve(m[3])
			if !ok1 || !ok2 || !ok3 {
				continue
			}
			keys = append(keys, mod+":"+sub+":"+name)
		}
		// A factory that delegates to a generic one (ProposeLockTransferRequest
		// calls NewSysParamStringsPropRequestWithTitle) has no prms. call of
		// its own; the generic path is checked by decodeSysParamRequest's own
		// tests, so nothing to compare here.
		if len(keys) == 0 {
			continue
		}
		if len(keys) != len(specs) {
			t.Errorf("%s writes %d key(s) in r/sys/params (%v) but the table lists %d; the table is stale",
				fn, len(keys), keys, len(specs))
			continue
		}
		for i, key := range keys {
			if specs[i].key != key {
				t.Errorf("%s key[%d]: r/sys/params writes %q, the table says %q; the table is stale", fn, i, key, specs[i].key)
			}
		}
	}
}
