# registry

Curated data that cannot be derived from a chain: what an address is called,
which token is the real one, and what an app does.

Everything here is embedded into the binary at build time and served through
`/api/labels` and `/api/registry/apps`. Adding an entry is a pull request
against one of these files, and nothing else.

## What belongs here, and what does not

**Only what cannot be derived.** Anything provable from on-chain data is
computed live instead, so it stays right as the chains change rather than
rotting in a file. Namespace ownership is the worked example: `/api/labels`
works it out from deploy history every time it is asked, and an entry here
duplicating that is a stale copy waiting to happen.

So: a person's name, a team's name, a faucet, an oracle key, a token's display
symbol, what an app is for. Not: who deployed what, how many calls a realm has,
what a realm imports.

## Provenance: four kinds, never collapsed

Every entry says where its claim comes from. This is the part that makes the
data worth trusting, and it is why there is a `kind` field rather than just a
name.

| `kind` | means | who is asserting it |
|---|---|---|
| `curated` | a human vouched for it, in a merged pull request | a contributor here |
| `derived` | provable from chain data, recomputed live | the chain, via `/api/labels` |
| `declared` | the subject said so: `r/sys/users`, a valoper moniker | the address itself |
| `inferred` | a heuristic over behaviour | this repo's reading of the chain |

`derived` is never written here; it is listed because the explorer merges the
two and a reader has to be able to tell them apart. Self-declared names are
attacker-controlled by construction: anyone can call `UpdateDescription` on
`r/gnops/valopers` and call themselves anything.

The explorer renders the four differently, and an `inferred` label carries its
reasoning in the tooltip. Guessing silently is how an explorer loses the trust
that makes it worth using.

## Adding an entry

1. Pick the file: `data/addresses.json`, `data/tokens.json` or `data/apps.json`.
2. Add one entry. **`why` is required for anything that is not `curated`**, and
   it has to be evidence rather than an assertion: "3683 sends to 953 addresses,
   never calls a realm" is evidence, "it is a faucet" is not.
3. Add `checked` (a `YYYY-MM-DD` date) for anything measured, so a reader can
   see how old the measurement is. It works in all three files. An app blurb
   needs one whenever it names a fact that can change without the entry
   changing: a minimum balance a realm asks for, which generation a front-end
   currently serves, a parameter's present value. "What this realm is for" does
   not need a date; "it asks 3000 GNOT" does, and `/apps` prints it.
4. Open a pull request. `go test ./pkg/registry/` validates the shape, and CI
   runs it.

One entry per pull request where you can. A name is a small change with a large
blast radius: it appears on every page the address does.

## Files

| file | keyed by | used by |
|---|---|---|
| `addresses.json` | bech32 address | every address link in the explorer |
| `tokens.json` | `<realm path>.<name>.<id>`, the GRC20 event key | the token views |
| `apps.json` | realm path | `/apps` |

Every file's `checked` is validated the same way and none may be dated in the
future; `TestShippedDatesAreNotInTheFuture` covers all three.

Token keys are whatever the GRC20 `Transfer` event puts in its `token`
attribute, verbatim. That is usually the full triple
(`gno.land/r/gnoland/wugnot.wugnot.0000000`) rather than a bare realm path,
because one realm can expose more than one token, but not always: two live
mainnet tokens emit a bare symbol instead. Copy the key from the asset page
rather than constructing it.
