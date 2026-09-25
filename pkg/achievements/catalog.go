// Package achievements is the catalog of things an address can have done on
// chain, and the SQL that decides who has done them.
//
// The point is not gamification for its own sake. gno.land has a long list of
// capabilities most people never find out exist — that you can wrap ugnot, that
// a realm can import another realm's package, that an account can delegate a
// scoped signing key and revoke it later — and none of them are discoverable by
// reading a block explorer's tables. An achievement names the capability, says
// what it is for, and tells a reader the one command that earns it. The badge is
// the hook; the `How` line is the actual product.
//
// Two rules hold this together:
//
//   - An achievement is a *fact from the index*, never a judgement. Every
//     definition below is one query over tables this instance already fills from
//     the chain, and it answers "when did this address first do this" with a
//     block height and a transaction hash. Nothing is awarded by hand, nothing is
//     inferred from a heuristic, and a claim nobody can click through to is a
//     claim this package will not make.
//   - The catalog is append-only in spirit. Slugs are the identity of a badge
//     and they leak into URLs (/directory/people?has=first-realm) and into the
//     achievements table's primary key, so renaming one silently unawards it for
//     everybody. Add definitions; do not repurpose them.
//
// Adding one is a single entry in Catalog below plus a row in the catalog test.
// The rollup picks it up on its next pass and backfills every address that ever
// qualified, because every query is over all of history rather than a window.
package achievements

// Group buckets the catalog for display. The order of the constants is the
// order the groups are shown in, which is roughly the order a newcomer meets
// them: you send coins before you deploy a realm, and you deploy a realm before
// you delegate a key to it.
type Group string

const (
	GroupStart    Group = "start"
	GroupBuild    Group = "build"
	GroupIdentity Group = "identity"
	GroupMoney    Group = "money"
	GroupKeys     Group = "keys"
	GroupGovern   Group = "govern"
)

// GroupOrder is the display order, and the only place it is defined.
var GroupOrder = []Group{GroupStart, GroupBuild, GroupIdentity, GroupMoney, GroupKeys, GroupGovern}

// GroupLabel is what a reader sees above each bucket.
var GroupLabel = map[Group]string{
	GroupStart:    "getting started",
	GroupBuild:    "building",
	GroupIdentity: "identity",
	GroupMoney:    "money",
	GroupKeys:     "keys and sessions",
	GroupGovern:   "governance",
}

// Def is one achievement.
type Def struct {
	// Slug identifies the badge forever. Kebab-case, stable, never reused.
	Slug string `json:"slug"`

	// Name is the badge as a reader sees it, phrased as the deed rather than
	// the reward: "Published a realm", not "Realm Master". A badge that names
	// the deed teaches the deed.
	Name string `json:"name"`

	// Emoji is the whole icon. No sprite entry, no asset: the catalog is data
	// that ships in JSON and gets rendered as text, so a new badge costs
	// nothing in the frontend.
	Emoji string `json:"emoji"`

	Group Group `json:"group"`

	// What says, in one line, which on-chain fact unlocked this. It is the
	// honest description of the query below it, so that a reader who disagrees
	// with their badge can tell exactly what was measured.
	What string `json:"what"`

	// How is the line that makes this package worth building: what somebody
	// who does *not* have the badge would do to earn it. Written as an
	// instruction, not a description, and naming the real command or realm
	// wherever one exists.
	How string `json:"how"`

	// SQL yields one row per address that has ever unlocked this, as
	// (address, block_height, block_time, tx_hash), where the three trailing
	// columns describe the *first* time it happened.
	//
	// The network is bound as the named parameter @net, which SQLite binds to
	// every occurrence from a single sql.Named. That is deliberate: every
	// table here is network-scoped (see AGENTS.md's first invariant) and a
	// query that forgets the filter silently merges two chains' histories into
	// one address's timeline.
	//
	// The first-occurrence columns rely on SQLite's documented bare-column
	// rule: in a query whose only aggregate is a single MIN(), the bare columns
	// are taken from the row that produced the minimum. That is what makes
	// "the height, time and hash of the first one" a plain GROUP BY rather than
	// a window function over the whole table.
	//
	// Not exported as JSON. A reader is owed What and How; the SQL is an
	// implementation detail and putting it in the API would freeze it.
	SQL string `json:"-"`

	// Live marks a badge the index cannot decide, only a live chain read can.
	// There is exactly one today (session-used) and the reason is in its
	// comment. A live badge is awarded on an address's own page and is absent
	// from the directory, which would otherwise have to make one RPC call per
	// row; the API says which it is so the UI can explain itself rather than
	// look broken.
	Live bool `json:"live,omitempty"`
}

// Catalog is every achievement, in display order within its group.
//
// Kept as one slice rather than a map so the order is the file's order: a badge
// grid that reshuffles between loads is unreadable, and Go map iteration would
// do exactly that.
var Catalog = []Def{
	// --- getting started ---------------------------------------------------
	{
		Slug: "first-tx", Name: "First transaction", Emoji: "🌱", Group: GroupStart,
		What: "signed anything at all: a call, a send, a script or a deploy",
		How:  "any transaction counts. `gnokey maketx send` to a friend is the shortest one.",
		SQL: `SELECT address, MIN(block_height) AS block_height, block_time, tx_hash FROM (
			SELECT caller AS address, block_height, block_time, tx_hash FROM calls WHERE network = @net AND success = 1
			UNION ALL
			SELECT from_address, block_height, block_time, tx_hash FROM bank_sends WHERE network = @net AND success = 1 AND from_address <> ''
			UNION ALL
			SELECT caller, block_height, block_time, tx_hash FROM msg_runs WHERE network = @net AND success = 1
			UNION ALL
			SELECT creator, block_height, block_time, tx_hash FROM package_submissions WHERE network = @net AND success = 1
		) GROUP BY address`,
	},
	{
		Slug: "first-gnot-sent", Name: "Sent GNOT", Emoji: "💸", Group: GroupStart,
		What: "sent native ugnot to another address with a BankMsgSend",
		How:  "`gnokey maketx send -to <address> -send 1000000ugnot -gas-fee 1000000ugnot -gas-wanted 200000 <key>`",
		SQL: `SELECT from_address AS address, MIN(block_height) AS block_height, block_time, tx_hash
			FROM bank_sends
			WHERE network = @net AND success = 1 AND COALESCE(ugnot_amount, 0) > 0 AND from_address <> ''
			GROUP BY from_address`,
	},
	{
		Slug: "first-gnot-received", Name: "Received GNOT", Emoji: "📥", Group: GroupStart,
		What: "was on the receiving end of a BankMsgSend carrying ugnot",
		How:  "ask someone to send you some, or use a faucet. This one is not something you do to yourself.",
		SQL: `SELECT to_address AS address, MIN(block_height) AS block_height, block_time, tx_hash
			FROM bank_sends
			WHERE network = @net AND success = 1 AND COALESCE(ugnot_amount, 0) > 0 AND to_address <> ''
			GROUP BY to_address`,
	},
	{
		Slug: "first-call", Name: "Called a realm", Emoji: "📞", Group: GroupStart,
		What: "ran an exported function on a realm with MsgCall",
		How:  "`gnokey maketx call -pkgpath gno.land/r/demo/userbook -func SignUp -gas-fee 1000000ugnot -gas-wanted 2000000 <key>`",
		SQL: `SELECT caller AS address, MIN(block_height) AS block_height, block_time, tx_hash
			FROM calls WHERE network = @net AND success = 1 GROUP BY caller`,
	},
	{
		Slug: "first-run", Name: "Ran a script", Emoji: "📜", Group: GroupStart,
		What: "executed gno source directly on chain with MsgRun",
		How:  "write a `main()` that imports the realms you want, then `gnokey maketx run -gas-fee 1000000ugnot -gas-wanted 5000000 <key> script.gno`. One transaction, several realms, no deploy.",
		SQL: `SELECT caller AS address, MIN(block_height) AS block_height, block_time, tx_hash
			FROM msg_runs WHERE network = @net AND success = 1 GROUP BY caller`,
	},

	// --- building ----------------------------------------------------------
	{
		Slug: "first-package", Name: "Published a package", Emoji: "📦", Group: GroupBuild,
		What: "deployed a pure package (a /p/ path: library code, no state)",
		How:  "`gnokey maketx addpkg -pkgpath gno.land/p/<your-namespace>/<name> -pkgdir . -gas-fee 1000000ugnot -gas-wanted 20000000 <key>`. Register the namespace first.",
		SQL: `SELECT creator AS address, MIN(block_height) AS block_height, block_time, tx_hash
			FROM package_submissions
			WHERE network = @net AND success = 1 AND is_realm = 0 AND creator <> ''
			GROUP BY creator`,
	},
	{
		Slug: "first-realm", Name: "Published a realm", Emoji: "🏛", Group: GroupBuild,
		What: "deployed a realm (an /r/ path: keeps state between calls, renders a page)",
		How:  "same addpkg, a /r/ path, and an exported `Render(path string) string` so it has a page on gnoweb.",
		SQL: `SELECT creator AS address, MIN(block_height) AS block_height, block_time, tx_hash
			FROM package_submissions
			WHERE network = @net AND success = 1 AND is_realm = 1 AND creator <> ''
			GROUP BY creator`,
	},
	{
		Slug: "home-realm", Name: "Made a home realm", Emoji: "🏠", Group: GroupBuild,
		What: "deployed a realm at <namespace>/home, the page gno.land treats as yours",
		How:  "deploy anything renderable to `gno.land/r/<your-namespace>/home`. It is the closest thing the chain has to a personal site.",
		SQL: `SELECT creator AS address, MIN(block_height) AS block_height, block_time, tx_hash
			FROM package_submissions
			WHERE network = @net AND success = 1 AND is_realm = 1 AND creator <> '' AND path LIKE 'gno.land/r/%/home'
			GROUP BY creator`,
	},
	{
		Slug: "first-import", Name: "Imported someone's code", Emoji: "🔗", Group: GroupBuild,
		What: "published a package that imports a gno.land path you did not deploy",
		How:  "`import \"gno.land/p/demo/avl\"` and use its Tree instead of a map. Composability is the reason the chain stores source.",
		SQL: `SELECT p.creator AS address, MIN(p.block_height) AS block_height, p.block_time, p.tx_hash
			FROM packages p
			JOIN dependencies d ON d.network = p.network AND d.package_path = p.path
			WHERE p.network = @net AND p.creator <> '' AND d.import_path LIKE 'gno.land/%'
			  AND NOT EXISTS (
			    SELECT 1 FROM packages mine
			    WHERE mine.network = p.network AND mine.path = d.import_path AND mine.creator = p.creator)
			GROUP BY p.creator`,
	},
	{
		Slug: "imported-by-other", Name: "Someone imported you", Emoji: "🌟", Group: GroupBuild,
		What: "a package you deployed is imported by a package somebody else deployed",
		How:  "not something you can do alone. Publish something small and genuinely reusable under /p/, and document it.",
		SQL: `SELECT mine.creator AS address, MIN(other.block_height) AS block_height, other.block_time, other.tx_hash
			FROM packages mine
			JOIN dependencies d ON d.network = mine.network AND d.import_path = mine.path
			JOIN packages other ON other.network = mine.network AND other.path = d.package_path
			WHERE mine.network = @net AND mine.creator <> '' AND other.creator <> mine.creator
			GROUP BY mine.creator`,
	},
	{
		Slug: "own-realm-call", Name: "Used your own realm", Emoji: "🪞", Group: GroupBuild,
		What: "called a function on a realm you deployed yourself",
		How:  "deploy a realm, then call it. Being your own first user is how you find out the signature is wrong.",
		SQL: `SELECT c.caller AS address, MIN(c.block_height) AS block_height, c.block_time, c.tx_hash
			FROM calls c
			JOIN packages p ON p.network = c.network AND p.path = c.pkg_path
			WHERE c.network = @net AND c.success = 1 AND p.creator = c.caller
			GROUP BY c.caller`,
	},
	{
		Slug: "called-by-other", Name: "Someone used your realm", Emoji: "👥", Group: GroupBuild,
		What: "somebody other than you called a realm you deployed",
		How:  "also not something you can do alone, and the one that actually means the realm works for a stranger.",
		SQL: `SELECT p.creator AS address, MIN(c.block_height) AS block_height, c.block_time, c.tx_hash
			FROM calls c
			JOIN packages p ON p.network = c.network AND p.path = c.pkg_path
			WHERE c.network = @net AND c.success = 1 AND p.creator <> '' AND p.creator <> c.caller
			GROUP BY p.creator`,
	},

	// --- identity ----------------------------------------------------------
	{
		Slug: "username", Name: "Registered a username", Emoji: "🪪", Group: GroupIdentity,
		What: "holds a name in the chain's user registry, r/sys/users",
		How:  "register through `gno.land/r/gnoland/users/v1`. The name becomes your namespace, so /r/<name>/… is yours to deploy under.",
		SQL: `SELECT address, MIN(block_height) AS block_height, block_time, tx_hash
			FROM users WHERE network = @net AND deleted = 0 AND address <> '' GROUP BY address`,
	},
	{
		Slug: "profile", Name: "Customized a profile", Emoji: "✨", Group: GroupIdentity,
		What: "set a field on a profile realm (a Set… call on a …/profile path)",
		How:  "`gnokey maketx call -pkgpath gno.land/r/demo/profile -func SetStringField -args DisplayName -args \"your name\" …`. Avatar and bio are the same call with a different field.",
		SQL: `SELECT caller AS address, MIN(block_height) AS block_height, block_time, tx_hash
			FROM calls
			WHERE network = @net AND success = 1 AND pkg_path LIKE '%/profile' AND func_name LIKE 'Set%'
			GROUP BY caller`,
	},

	// --- money -------------------------------------------------------------
	{
		Slug: "wrap-wugnot", Name: "Wrapped GNOT", Emoji: "🎁", Group: GroupMoney,
		What: "called Deposit on a wugnot realm, turning native coin into a GRC20 balance",
		How:  "`gnokey maketx call -pkgpath gno.land/r/gnoland/wugnot -func Deposit -send 1000000ugnot …`. Wrapped ugnot is what a GRC20-only contract can accept.",
		SQL: `SELECT caller AS address, MIN(block_height) AS block_height, block_time, tx_hash
			FROM calls
			WHERE network = @net AND success = 1 AND pkg_path LIKE '%/wugnot' AND func_name = 'Deposit'
			GROUP BY caller`,
	},
	{
		Slug: "unwrap-wugnot", Name: "Unwrapped GNOT", Emoji: "🔓", Group: GroupMoney,
		What: "called Withdraw on a wugnot realm, turning the GRC20 balance back into coin",
		How:  "`… -func Withdraw -args <amount>`. Worth doing once, so you know your coin is not stuck in there.",
		SQL: `SELECT caller AS address, MIN(block_height) AS block_height, block_time, tx_hash
			FROM calls
			WHERE network = @net AND success = 1 AND pkg_path LIKE '%/wugnot' AND func_name = 'Withdraw'
			GROUP BY caller`,
	},
	{
		Slug: "grc20-sent", Name: "Sent a token", Emoji: "🪙", Group: GroupMoney,
		What: "a GRC20 Transfer event moved tokens out of this address",
		How:  "`gnokey maketx call -pkgpath <token realm> -func Transfer -args <to> -args <amount> …`. Different from a coin send: tokens live in a realm's ledger, not the bank.",
		SQL: `SELECT from_addr AS address, MIN(block_height) AS block_height, block_time, tx_hash
			FROM token_transfers WHERE network = @net AND from_addr <> '' AND value > 0 GROUP BY from_addr`,
	},
	{
		Slug: "grc20-received", Name: "Received a token", Emoji: "💰", Group: GroupMoney,
		What: "a GRC20 Transfer event moved tokens into this address",
		How:  "hold any GRC20. Wrapping ugnot is the shortest path and earns two badges at once.",
		SQL: `SELECT to_addr AS address, MIN(block_height) AS block_height, block_time, tx_hash
			FROM token_transfers WHERE network = @net AND to_addr <> '' AND value > 0 GROUP BY to_addr`,
	},
	{
		Slug: "token-issuer", Name: "Issued a token", Emoji: "🏭", Group: GroupMoney,
		What: "a GRC20 token from a package you deployed changed hands",
		How:  "deploy a realm that instantiates `grc20.NewToken`, then mint some. The badge lands the first time the token actually moves.",
		SQL: `SELECT p.creator AS address, MIN(t.block_height) AS block_height, t.block_time, t.tx_hash
			FROM token_transfers t
			JOIN packages p ON p.network = t.network AND p.path = t.pkg_path
			WHERE t.network = @net AND p.creator <> '' GROUP BY p.creator`,
	},

	// --- keys and sessions -------------------------------------------------
	{
		Slug: "session-created", Name: "Created a session key", Emoji: "🔑", Group: GroupKeys,
		What: "granted a delegated signing key with auth/create_session",
		How:  "mint a key, then `gnokey maketx auth create_session` with `-allow-path` and a spend limit. The key signs for you, scoped, without ever holding your mnemonic.",
		SQL: `SELECT master AS address, MIN(granted_height) AS block_height, granted_time AS block_time, granted_tx AS tx_hash
			FROM session_grants WHERE network = @net AND master <> '' GROUP BY master`,
	},
	{
		Slug: "session-used", Name: "Signed with a session key", Emoji: "🖋", Group: GroupKeys, Live: true,
		What: "one of this account's live session keys has signed at least one transaction",
		How:  "use the delegated key instead of your master key for the realm you scoped it to. That is the whole point of granting one.",
		// No SQL, and this is the one gap in the catalog rather than an
		// oversight. A session signs *as its master*: the calls, msg_runs and
		// bank_sends tables all record the master's address, and the chain
		// offers no reverse lookup (auth/accounts/<session_addr> returns null,
		// because a session is not a plain account). The only surviving trace
		// of use is the Sequence on the live grant, which auth/accounts/
		// <master>/sessions returns and nothing indexes. So this is awarded
		// from that live read, on an address's own page, and only while the
		// grant still exists: a key that was used and then revoked leaves no
		// evidence anywhere. Indexing it needs MsgCreateSession modelled in the
		// tx-indexer, or the signer recorded alongside the caller.
	},
	{
		Slug: "session-revoked", Name: "Revoked a session key", Emoji: "♻️", Group: GroupKeys,
		What: "ended a grant with auth/revoke_session or auth/revoke_all_sessions",
		How:  "`gnokey maketx auth revoke_session -session-key <address>`. Granting is half the skill; being able to take it back is the other half.",
		SQL: `SELECT master AS address, MIN(revoked_height) AS block_height, revoked_time AS block_time, revoked_tx AS tx_hash
			FROM session_grants WHERE network = @net AND master <> '' AND revoked_height IS NOT NULL GROUP BY master`,
	},
	{
		Slug: "session-key", Name: "Is a session key", Emoji: "🗝", Group: GroupKeys,
		What: "this address is itself a delegated key, granted by another account",
		How:  "not earned by an account: this marks the delegated address, so a page that looks empty says why rather than reading as an unused account.",
		SQL: `SELECT session_addr AS address, MIN(granted_height) AS block_height, granted_time AS block_time, granted_tx AS tx_hash
			FROM session_grants WHERE network = @net AND session_addr <> '' GROUP BY session_addr`,
	},

	// --- governance --------------------------------------------------------
	{
		Slug: "govdao-vote", Name: "Voted in GovDAO", Emoji: "🗳", Group: GroupGovern,
		What: "cast a vote on a proposal in r/gov/dao",
		How:  "GovDAO membership comes first, and it is granted by proposal. Members vote with `MustVoteOnProposalSimple`.",
		SQL: `SELECT caller AS address, MIN(block_height) AS block_height, block_time, tx_hash
			FROM calls
			WHERE network = @net AND success = 1 AND pkg_path LIKE '%/r/gov/dao' AND func_name LIKE '%Vote%'
			GROUP BY caller`,
	},
	{
		Slug: "govdao-execute", Name: "Executed a proposal", Emoji: "⚙️", Group: GroupGovern,
		What: "called ExecuteProposal on r/gov/dao, applying a passed proposal",
		How:  "anyone may execute a proposal that has passed. It is the step people forget, and nothing happens until somebody pays for it.",
		SQL: `SELECT caller AS address, MIN(block_height) AS block_height, block_time, tx_hash
			FROM calls
			WHERE network = @net AND success = 1 AND pkg_path LIKE '%/r/gov/dao' AND func_name = 'ExecuteProposal'
			GROUP BY caller`,
	},
	{
		Slug: "validator", Name: "Registered a validator", Emoji: "🛡", Group: GroupGovern,
		What: "appears in the valopers registry as a registered validator",
		How:  "run a node, then register it in `r/gnoland/valopers`. Registration is public; being in the valset is a separate GovDAO decision.",
		SQL: `SELECT address, MIN(block_height) AS block_height, block_time, tx_hash
			FROM valoper_registrations
			WHERE network = @net AND success = 1 AND address <> '' GROUP BY address`,
	},
}

// bySlug indexes the catalog once, at init, so lookups are not a linear scan
// through a slice that only grows.
var bySlug = func() map[string]*Def {
	m := make(map[string]*Def, len(Catalog))
	for i := range Catalog {
		m[Catalog[i].Slug] = &Catalog[i]
	}
	return m
}()

// Lookup returns the definition for a slug, or nil.
func Lookup(slug string) *Def { return bySlug[slug] }

// Indexed returns the definitions the rollup can compute, which is every one
// that carries SQL. The rollup iterates this rather than Catalog so a live-only
// badge cannot silently become "nobody has it".
func Indexed() []Def {
	out := make([]Def, 0, len(Catalog))
	for _, d := range Catalog {
		if d.SQL != "" {
			out = append(out, d)
		}
	}
	return out
}
