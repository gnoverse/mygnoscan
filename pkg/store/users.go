package store

import (
	"database/sql"
	"strings"
)

// The user registry: name <-> address, as gno.land/r/sys/users records it.
//
// Every other name this explorer shows for an address is an approximation of
// this one. A namespace read off a package path is a guess about who deployed
// it, the curated file in pkg/registry is a human's note, and a valoper moniker
// is a claim the subject made about itself. The registry is the chain's own
// answer, and it is the only one of the four that a name with no packages, no
// curation and no validator ever appears in -- 67 of mainnet's 78 registrations
// on 2026-09-23.
//
// Rows arrive from the syncer, replayed from the realm's events. See the users
// table in schema.go for why the key is the name and why a deletion is a
// tombstone.

// User is one registration.
type User struct {
	Network     string `json:"network,omitempty"`
	Name        string `json:"name"`
	Address     string `json:"address"`
	TxHash      string `json:"tx_hash,omitempty"`
	BlockHeight int    `json:"block_height"`
	BlockTime   string `json:"block_time,omitempty"`
	// Alias says this name is a previous name of the address rather than its
	// current one. r/sys/users keeps old names resolvable on purpose, so they
	// belong in the index; they just must not outrank the real one.
	Alias   bool `json:"alias,omitempty"`
	Deleted bool `json:"deleted,omitempty"`
	// Packages is how many packages this address has deployed on the same
	// network. Selected with the row rather than counted per result, because
	// it is the one number that separates the project a reader was looking for
	// from a namesake, and eight round trips on a debounced keystroke is not a
	// price worth paying for it. Zero for every user that has never deployed,
	// which on mainnet is 67 of 78.
	Packages int `json:"packages"`
}

// UpsertUser records a registration.
//
// INSERT OR REPLACE rather than IGNORE: a resync replays the same events, and
// the later pass carries the same facts. A name whose row already exists is
// being re-stated, not contradicted.
func (d *DB) UpsertUser(network string, u User) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec(`
		INSERT OR REPLACE INTO users
		  (network, name, address, tx_hash, block_height, block_time, alias, deleted)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		network, u.Name, u.Address, u.TxHash, u.BlockHeight, u.BlockTime, u.Alias, u.Deleted)
	return err
}

// MarkUserDeleted tombstones every name an address holds.
//
// The event carries only the address, which is enough: r/sys/users deletes the
// user, and all of that user's names go with it. The rows stay, because the
// names stay taken -- Delete() sets deleted=true and leaves the nameStore entry
// exactly so the name cannot be revived by someone else.
func (d *DB) MarkUserDeleted(network, address string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec(
		`UPDATE users SET deleted = 1 WHERE network = ? AND address = ?`, network, address)
	return err
}

// DemoteUserAliases marks every name an address holds except one as an alias.
//
// Called when an Updated event names the new current name. The realm's own rule
// is that the newest name is what an address lookup returns and the older ones
// stay resolvable, so this is not a cleanup: it is the ranking the realm states.
func (d *DB) DemoteUserAliases(network, address, current string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec(
		`UPDATE users SET alias = 1 WHERE network = ? AND address = ? AND name != ?`,
		network, address, current)
	return err
}

// UserCount is how many live registrations a network has.
func (d *DB) UserCount(network string) (int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var n int
	err := d.db.QueryRow(`SELECT COUNT(*) FROM users WHERE deleted = 0 AND ` +
		d.networkFilter("network", network)).Scan(&n)
	return n, err
}

// UserByAddress returns the current name of an address, or nil.
func (d *DB) UserByAddress(network, address string) (*User, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	row := d.db.QueryRow(`
		SELECT network, name, address, tx_hash, block_height, block_time, alias, deleted
		  FROM users
		 WHERE address = ? AND deleted = 0 AND `+d.networkFilter("network", network)+`
		 ORDER BY alias ASC, block_height DESC LIMIT 1`, address)
	var u User
	err := row.Scan(&u.Network, &u.Name, &u.Address, &u.TxHash,
		&u.BlockHeight, &u.BlockTime, &u.Alias, &u.Deleted)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// SearchUsers matches a registration by name or by address, best first.
//
// Ranked in SQL rather than by a LIKE alone, because a plain substring match
// puts the answer in the wrong place: on mainnet, "moul" as a substring also
// hits nothing else, but "gno" hits `gno`, `gnoland`, `gnolang`, `gnoswap` and
// every nym- name containing it, and the exact registration has to come first.
// The order is exact name, then prefix, then substring; a current name always
// outranks an alias of the same shape, and a tombstoned one sorts last rather
// than disappearing, because "this name is taken and dead" is a real answer.
//
// An address is matched as a prefix, which is what a reader pasting a truncated
// g1... from somewhere else actually has.
func (d *DB) SearchUsers(network, q string, limit int) ([]User, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if limit <= 0 {
		limit = 8
	}
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return []User{}, nil
	}
	rows, err := d.db.Query(`
		SELECT u.network, u.name, u.address, u.tx_hash, u.block_height, u.block_time,
		       u.alias, u.deleted,
		       (SELECT COUNT(*) FROM packages p
		         WHERE p.network = u.network AND p.creator = u.address),
		       CASE
		         WHEN LOWER(u.name) = ?        THEN 0
		         WHEN LOWER(u.address) = ?     THEN 1
		         WHEN LOWER(u.name) LIKE ?     THEN 2
		         WHEN LOWER(u.address) LIKE ?  THEN 3
		         ELSE 4
		       END AS rank
		  FROM users u
		 WHERE (LOWER(u.name) LIKE ? OR LOWER(u.address) LIKE ?)
		   AND `+d.networkFilter("u.network", network)+`
		 ORDER BY u.deleted ASC, rank ASC, u.alias ASC, u.block_height ASC, u.name ASC
		 LIMIT ?`,
		q, q, q+"%", q+"%", "%"+q+"%", q+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []User{}
	for rows.Next() {
		var u User
		var rank int
		if err := rows.Scan(&u.Network, &u.Name, &u.Address, &u.TxHash,
			&u.BlockHeight, &u.BlockTime, &u.Alias, &u.Deleted, &u.Packages, &rank); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// RegisteredAddressLabels is the registry as address -> label, for /api/labels.
//
// Kind is "derived" rather than "declared", which is the judgement call worth
// stating: a valoper moniker is declared because anyone may call
// UpdateDescription and claim any name, whereas a registration is enforced --
// r/sys/users rejects a name that is taken, and rejects one whose canonical form
// collides with a taken one. The chain is not repeating a claim here, it is the
// thing that decided it.
//
// Aliases and tombstones are left out. A previous name still resolves, and the
// search says so, but it is not what the address is called now; a deleted user
// has no current name at all.
func (d *DB) RegisteredAddressLabels(network string) (map[string]AddressLabel, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.Query(`SELECT address, name FROM users
		 WHERE alias = 0 AND deleted = 0 AND ` + d.networkFilter("network", network))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]AddressLabel{}
	for rows.Next() {
		var addr, name string
		if err := rows.Scan(&addr, &name); err != nil {
			return nil, err
		}
		// One address, two chains, two names: in all-networks mode the first
		// answer wins rather than the two overwriting each other row by row,
		// which would make the label depend on scan order. A per-chain page
		// asks with a network and gets that chain's answer.
		if _, seen := out[addr]; seen {
			continue
		}
		out[addr] = AddressLabel{
			Label: "@" + name,
			Kind:  "derived",
			Why:   "registered in gno.land/r/sys/users",
		}
	}
	return out, rows.Err()
}
