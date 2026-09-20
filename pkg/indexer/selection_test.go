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

func selectionSets() []selectionSet {
	return []selectionSet{
		{"light", txFieldsLight, false, false},
		{"sync", txFieldsSync, true, false},
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

			if got := strings.Contains(tc.fields, "files { name body }"); got != tc.fileBodies {
				t.Errorf("file bodies present = %v, want %v", got, tc.fileBodies)
			}

			if !tc.fileBodies && !strings.Contains(tc.fields, "files { name }") {
				t.Error("a set without file bodies must still select file names")
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
		})
	}
}

func TestTrimFields_StripsBothGroupsFromEverySet(t *testing.T) {
	for _, tc := range selectionSets() {
		t.Run(tc.name, func(t *testing.T) {
			// A chain that defines neither optional type
			c := &Client{typeSupport: map[string]bool{
				inertProbeType:   false,
				sessionProbeType: false,
			}}

			trimmed := c.trimFields(context.Background(), tc.fields)

			if trimmed == tc.fields {
				t.Fatal("nothing was trimmed")
			}

			for _, unwanted := range []string{
				inertFragments,
				sessionFragments,
				"MsgEnablePackage",
				"MsgCreateSession",
			} {
				if strings.Contains(trimmed, unwanted) {
					t.Errorf("trimmed set still contains %q", unwanted)
				}
			}
		})
	}
}
