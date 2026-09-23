# Glossary

Plain-language definitions for the words this explorer uses. One file, one source: it is
embedded in the binary and served at `GET /api/glossary`, so the page you are reading and
the tooltips in the product are the same bytes.

**It is authoritative.** Any other plain-language wording in the product, in any spec, loses
to this table. Two places inventing their own phrasing for "parked package" is how a reader
ends up being told two different things about one state.

Version: 2026-09-23

## Rules

Enforced by `pkg/glossary`, so breaking one fails CI rather than shipping.

- **Two columns, nothing else.** Adding a term is a docs change and a CI re-run, never a code
  change.
- **Present tense, three sentences at most.** Short enough that a tooltip can hold it.
- **A gloss that leans on another headword marks it in bold**, like **inert** below. Those
  bold spans are the only cross-references the parser sees, so every other word in a gloss is
  ordinary English by construction. The references must form a DAG: no cycles, and nothing may
  point at a word this table does not define.
- **The roots are ordinary English.** A definition that needs a definition that needs a
  definition is not a definition.

## Terms

| term | plain-language gloss |
|---|---|
| address | An account on the chain, written as a long `g1...` string. It is the closest thing to a username, and some of them have claimed a real name. |
| block | A batch of transactions, recorded all at once. The chain adds one every few seconds, and they are numbered in order. |
| call | Using a program that is already on the chain, as opposed to publishing a new one. |
| directory | A path that holds other things rather than being a thing itself, like a folder. It has no page of its own to show. |
| gas | What running a transaction costs, paid by whoever sent it. Bigger jobs cost more. It is spent, not returned. |
| GPAO | An automated approver that runs alongside the chain. It checks code that is waiting and switches on the code that passes. It leaves no record of why it passed on something. |
| inert | Published but not switched on. The code is stored on the chain and nobody can use it until an approver enables it. |
| mainnet | The real chain, where things count. The others are for testing. |
| MsgAddPackage | The instruction that puts new code on the chain. "Somebody published code" is the whole of it. |
| namespace | The name in front of the slash, like `moul` in `r/moul/hello`. It is claimed on the chain and only its owner can publish under it. A long `g1...` string there means nobody has claimed a name yet. |
| package | Reusable code that other programs on the chain can borrow. It remembers nothing of its own; it is a toolbox, not a machine. |
| parked | Published but waiting. The code is on the chain and nobody can use it until an approver switches it on. The plain-language rendering of **inert**, and the one the product should use. |
| proposal | A formal request to change something about the chain. A group with that power votes, and if it passes somebody runs it and the change takes effect. |
| realm | A program that lives on the chain and remembers things between uses. Everyone who uses it uses the same copy, so what one person did is still there for the next. |
| render | The page a realm draws for itself. Some realms draw one and some do not, and one that does not is not broken. |
| storage deposit | Money locked up to pay for the space that code and data take on the chain. Unlike gas it comes back if the space is freed. |
| transaction | One instruction sent to the chain by one person, which either worked or did not. |
| unique callers | How many different accounts used something, as opposed to how many times it was used. One account using it a thousand times is still one. |
| validator | One of the machines that agree on what happened and in what order. Mainnet has a small set of them and a different operator runs each one. |

## Two deliberate absences

**Deploy** is not here, because the product should not say it: "put a new app on the chain" is
the rendering, and a word you never use needs no gloss. **Token** is not here either, because
nothing in the event vocabulary emits an event about one yet, and a glossary that defines words
the product does not say is a glossary nobody trusts.
