package indexer

import (
	"context"
	"strings"
	"testing"
)

// The three transaction selection sets and what each is expected to carry.
type selectionSet struct {
	name       string
	fields     string
	fileBodies bool
	contentRaw bool
}

// fileSelections is how many message fragments select package files:
// MsgAddPackage and MsgRun.
const fileSelections = 2

func selectionSets() []selectionSet {
	return []selectionSet{
		{"light", txFieldsLight, false, false},
		{"bodies", txFieldsBodies, true, false},
		{"full", txFields, true, true},
	}
}

// content_raw is the encoded signed transaction, so it carries every source
// file a second time. Only SignerAddress reads it, on the single-transaction
// path. The sync stores file bodies and never touches it, so selecting it there
// was 64.6% of the package sync payload fetched for nothing.
func TestSelectionSets_ContentRawOnlyOnTheSingleTransactionPath(t *testing.T) {
	for _, tc := range selectionSets() {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.Contains(tc.fields, "content_raw"); got != tc.contentRaw {
				t.Errorf("content_raw present = %v, want %v", got, tc.contentRaw)
			}

			// Both file selections, MsgAddPackage's and MsgRun's, or a set
			// that grew bodies on one of them would pass a Contains check
			// while fetching half of what its caller reads.
			want := 0
			if tc.fileBodies {
				want = fileSelections
			}
			if got := strings.Count(tc.fields, "files { name body }"); got != want {
				t.Errorf("file body selections = %d, want %d", got, want)
			}

			if !tc.fileBodies && strings.Count(tc.fields, "files { name }") != fileSelections {
				t.Errorf("a set without file bodies must still select all %d file name sets", fileSelections)
			}
		})
	}
}

// trimFields drops the optional fragment groups by literal substring, so every
// set has to carry them byte for byte. Without this guard a set could drift out
// of the trimmer's reach and start 422ing on chains that lack those types.
func TestSelectionSets_CarryTheOptionalFragmentsVerbatim(t *testing.T) {
	for _, tc := range selectionSets() {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.fields, inertFragments) {
				t.Error("set does not carry the inert lifecycle fragments verbatim")
			}

			if !strings.Contains(tc.fields, sessionFragments) {
				t.Error("set does not carry the session fragments verbatim")
			}

			// The one that takes a whole chain down when it drifts: pearl's
			// indexer does not define TransferEvent (probed 2026-09-23), so a
			// set the trimmer cannot reach here 422s every transaction query
			// on that network, not merely the view that wanted coins.
			if !strings.Contains(tc.fields, transferFragments) {
				t.Error("set does not carry the transfer fragments verbatim")
			}
		})
	}
}

func TestTrimFields_StripsEveryOptionalGroupFromEverySet(t *testing.T) {
	for _, tc := range selectionSets() {
		t.Run(tc.name, func(t *testing.T) {
			// A chain that defines none of the optional types
			// Keyed through typeSupportKey, not by bare type name: support is
			// cached per endpoint, because a pool's members can run different
			// schemas. With no URL configured the endpoint is the empty string.
			c := &Client{typeSupport: map[string]bool{
				typeSupportKey("", inertProbeType):    false,
				typeSupportKey("", sessionProbeType):  false,
				typeSupportKey("", transferProbeType): false,
			}}

			trimmed := c.trimFields(context.Background(), tc.fields)

			if trimmed == tc.fields {
				t.Fatal("nothing was trimmed")
			}

			for _, unwanted := range []string{
				inertFragments,
				sessionFragments,
				transferFragments,
				"MsgEnablePackage",
				"MsgCreateSession",
				"TransferEvent",
			} {
				if strings.Contains(trimmed, unwanted) {
					t.Errorf("trimmed set still contains %q", unwanted)
				}
			}
		})
	}
}

// The sets render through fmt.Sprintf. go vet catches a stray verb in the
// template while it is a constant, but not an escaped one: %% renders to a
// literal percent, which is not valid GraphQL and fails at runtime on every
// call, with an indexer error that says nothing about a format string.
func TestSelectionSets_RenderWithoutALeftoverVerb(t *testing.T) {
	for _, tc := range selectionSets() {
		t.Run(tc.name, func(t *testing.T) {
			i := strings.Index(tc.fields, "%")
			if i < 0 {
				return
			}

			end := min(i+24, len(tc.fields))
			t.Errorf("rendered set carries a percent at offset %d: %q", i, tc.fields[i:end])
		})
	}
}

// A pool's members can run different schemas, and mainnet's two do:
// indexer.gno.land does not define MsgCreateSession, indexer.onbloc.xyz does.
//
// Caching one answer for the whole pool means the field set gets trimmed for
// one member and sent to the other, and that failure is silent rather than
// loud: the message comes back with its real __typename and no fields at all,
// because the fragment that would have selected them was stripped and the
// UnexpectedMessage fragment no longer matches. It dropped ten session grants
// on mainnet on 2026-09-25 while every log line said the sweep was healthy.
func TestTypeSupportIsCachedPerEndpointNotPerPool(t *testing.T) {
	const (
		bare  = "https://indexer.bare.example/graphql/query"
		rich  = "https://indexer.rich.example/graphql/query"
		aType = "MsgCreateSession"
	)
	c := &Client{
		urls: []string{bare, rich},
		typeSupport: map[string]bool{
			typeSupportKey(bare, aType): false,
			typeSupportKey(rich, aType): true,
		},
	}

	// Selecting the bare endpoint: the fragments must go.
	c.active = 0
	if c.supportsType(context.Background(), aType) {
		t.Error("the bare endpoint reported support it does not have")
	}

	// Rotating to the rich one must change the answer, not reuse the first.
	c.active = 1
	if !c.supportsType(context.Background(), aType) {
		t.Error("the rich endpoint inherited the bare endpoint's cached answer, " +
			"so its typed fragments would be stripped and its grants arrive empty")
	}
}
