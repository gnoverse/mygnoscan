package main

import (
	_ "embed"

	"github.com/moul/mygnoscan/pkg/glossary"
)

// The glossary is embedded here rather than inside pkg/glossary because
// //go:embed cannot reach a parent directory, and the file belongs in docs/:
// it is a document people read in the repo first and an API response second.
// Embedding it at the root keeps that placement and still ships one copy.
//
//go:embed docs/glossary.md
var glossaryRaw []byte

// init rather than a call in run(): every path that serves a request goes
// through this binary, so parsing at init makes "the glossary is loaded" true
// by construction instead of true as long as nobody reorders startup.
func init() { glossary.MustLoad(glossaryRaw) }
