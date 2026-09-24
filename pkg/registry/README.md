# registry

Curated data that cannot be derived from a chain: what an address is called,
which token is the real one, and what an app does.

Everything here is embedded into the binary at build time and served through
`/api/labels`, `/api/registry/apps` and `/api/registry/awesome`. Adding an entry
is a pull request against one of these files, and nothing else. The exception is
`awesome.json`, which is generated from another repo's README: see below.

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

## One app is one card: `supersedes` and `covers`

A chain shows an app as the several realms it was deployed as, and `/apps` ranks
realms. Left alone it draws GnoSwap six times, once per realm, five of them
named after their paths. Two fields fix that, and they are not the same claim:

| field | means | the card says |
|---|---|---|
| `supersedes` | the same app, at an earlier date | `replaces …` |
| `covers` | a live part of the app on this card | `includes …` |

`covers` takes an exact path or a `/*` prefix (`gno.land/r/gnoswap/*`), and a
prefix is the better default because a list of paths goes stale the next time
somebody deploys, silently and in the direction of showing more cards. A card
that covers is never itself covered, and the first claim on a path wins, so two
overlapping prefixes are stable rather than dependent on map order.

Both fold rather than drop: the parts and the older generations are carried on
the card, each with its own link. They are real realms with real state, and
somebody came here looking for one of them.

A folded card's figures are re-read over the whole family rather than summed.
Calls add up; callers do not, because the same people use the router and the
staker, and adding those counts would print a reach the app does not have.

## Files

| file | keyed by | used by |
|---|---|---|
| `addresses.json` | bech32 address | every address link in the explorer |
| `tokens.json` | `<realm path>.<name>.<id>`, the GRC20 event key | the token views |
| `apps.json` | realm path | `/apps` |
| `awesome.json` | **generated, never hand-edited** | `/apps` |
| `moderation.toml` | realm path | `/apps`, as a removal |

Every file's `checked` is validated the same way and none may be dated in the
future; `TestShippedDatesAreNotInTheFuture` covers all three.

## The directory discovers itself, so these files only correct it

`/apps` no longer reads `apps.json` as its list. It ranks every realm people
actually call, describes each one with its own package doc comment, and
photographs it. These files are the three levers over that:

- **List**: an entry in `apps.json` is included whatever the chain says, and its
  name, sentence, website and category override the derived ones. Being listed
  is the vouch; there is no separate flag for it.
- **Relate**: `supersedes` folds an older generation into the one that replaces
  it, because two live deployments of the same idea is the normal state of a
  chain nobody can delete from, and ranking them as peers sends people to last
  year's version. An entry may carry **only** a path and `supersedes`: that is a
  fact about two deploys, and requiring a description alongside it would force
  you to invent one about somebody else's realm.
- **Skip**: `moderation.toml` removes, and every entry must say why. Usage is
  evidence of activity, not of worth.

A `description` here is still the best one available and still wins over the
realm's own doc comment and its README, so writing one is worth doing. It is shown as one line;
`checked` now travels in the card's provenance tooltip rather than in the dense
table that used to print it.

## `awesome.json` is somebody else's list, and it is generated

[gnoverse/awesome-gno](https://github.com/gnoverse/awesome-gno) is the
community's own answer to "what is being built on gno.land", and it holds the
half this explorer is structurally blind to: a wallet, a VS Code extension, a
language server, an SDK and a workshop are not realms, so no amount of indexing
will ever surface them.

It is the one file here nobody writes by hand. `make awesome` reads the README
at a named commit, parses it, and writes the snapshot; `make awesome-check` says
whether the committed copy is behind without writing one. **An edit here is
lost on the next regeneration, and it is also the wrong place: the point is that
the community maintains that list in their repo.** The right move when something
is missing is a pull request there, and `/apps?view=ecosystem` exists partly to
make that ask specific: it ranks this directory's entries that the list does not
name, with the bullet line ready to paste.

`make awesome` does more than copy the list. For every entry in an app section
it resolves the page you would actually open, following a repository's declared
homepage when the list links a repository, and it **drops anything that does not
answer 200 with HTML**. That check is what keeps a confident screenshot of a
dead host out of the grid. It also prints the host list gnoshot needs on its
`-allow-site` flag, because that is configuration on another box.

A snapshot rather than a live fetch, for the same reason everything else here is
embedded: a page that read GitHub on every load would be down when GitHub is and
different for two readers a minute apart. The cost is staleness, so the file
carries the `commit` it came from and the day it was `synced`, and the page
prints both rather than implying it is live. Neither target runs in CI: a red
build because somebody else edited their README is a build nobody here can fix.

Token keys are whatever the GRC20 `Transfer` event puts in its `token`
attribute, verbatim. That is usually the full triple
(`gno.land/r/gnoland/wugnot.wugnot.0000000`) rather than a bare realm path,
because one realm can expose more than one token, but not always: two live
mainnet tokens emit a bare symbol instead. Copy the key from the asset page
rather than constructing it.
