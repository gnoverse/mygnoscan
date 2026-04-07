package main

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

type DB struct {
	db *sql.DB
	mu sync.RWMutex
}

func NewDB(path string) (*DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}

	// Drop old tables without unique constraints (it's just a cache, rebuilds fast)
	db.Exec(`DROP TABLE IF EXISTS calls`)
	db.Exec(`DROP TABLE IF EXISTS msg_runs`)
	db.Exec(`DROP TABLE IF EXISTS bank_sends`)

	if err := initSchema(db); err != nil {
		db.Close()
		return nil, err
	}

	return &DB{db: db}, nil
}

func initSchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS packages (
			path TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			creator TEXT NOT NULL,
			block_height INTEGER NOT NULL,
			tx_hash TEXT NOT NULL,
			is_realm BOOLEAN NOT NULL,
			num_files INTEGER NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS package_files (
			package_path TEXT NOT NULL,
			file_name TEXT NOT NULL,
			body TEXT NOT NULL,
			PRIMARY KEY (package_path, file_name),
			FOREIGN KEY (package_path) REFERENCES packages(path)
		);

		CREATE TABLE IF NOT EXISTS dependencies (
			package_path TEXT NOT NULL,
			import_path TEXT NOT NULL,
			PRIMARY KEY (package_path, import_path)
		);

		CREATE TABLE IF NOT EXISTS calls (
			tx_hash TEXT NOT NULL,
			block_height INTEGER NOT NULL,
			caller TEXT NOT NULL,
			pkg_path TEXT NOT NULL,
			func_name TEXT NOT NULL,
			success BOOLEAN NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(tx_hash, pkg_path, func_name)
		);

		CREATE TABLE IF NOT EXISTS msg_runs (
			tx_hash TEXT NOT NULL,
			block_height INTEGER NOT NULL,
			caller TEXT NOT NULL,
			source TEXT NOT NULL,
			success BOOLEAN NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(tx_hash, caller)
		);

		CREATE TABLE IF NOT EXISTS bank_sends (
			tx_hash TEXT NOT NULL,
			block_height INTEGER NOT NULL,
			from_address TEXT NOT NULL,
			to_address TEXT NOT NULL,
			amount TEXT NOT NULL,
			success BOOLEAN NOT NULL,
			UNIQUE(tx_hash, from_address, to_address)
		);

		CREATE TABLE IF NOT EXISTS sync_state (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);

		CREATE INDEX IF NOT EXISTS idx_calls_pkg ON calls(pkg_path);
		CREATE INDEX IF NOT EXISTS idx_calls_caller ON calls(caller);
		CREATE INDEX IF NOT EXISTS idx_deps_import ON dependencies(import_path);
		CREATE INDEX IF NOT EXISTS idx_packages_creator ON packages(creator);
		CREATE INDEX IF NOT EXISTS idx_packages_realm ON packages(is_realm);
		CREATE INDEX IF NOT EXISTS idx_msg_runs_caller ON msg_runs(caller);
		CREATE INDEX IF NOT EXISTS idx_bank_from ON bank_sends(from_address);
		CREATE INDEX IF NOT EXISTS idx_bank_to ON bank_sends(to_address);
	`)
	return err
}

func (d *DB) Close() error {
	return d.db.Close()
}

// UpsertPackage inserts or updates a package.
func (d *DB) UpsertPackage(path, name, creator, txHash string, blockHeight int, isRealm bool, numFiles int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec(`
		INSERT OR REPLACE INTO packages (path, name, creator, tx_hash, block_height, is_realm, num_files)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, path, name, creator, txHash, blockHeight, isRealm, numFiles)
	return err
}

// UpsertPackageFile inserts or updates a package file.
func (d *DB) UpsertPackageFile(pkgPath, fileName, body string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec(`
		INSERT OR REPLACE INTO package_files (package_path, file_name, body)
		VALUES (?, ?, ?)
	`, pkgPath, fileName, body)
	return err
}

// SetDependencies replaces all dependencies for a package.
func (d *DB) SetDependencies(pkgPath string, imports []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM dependencies WHERE package_path = ?`, pkgPath); err != nil {
		return err
	}

	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO dependencies (package_path, import_path) VALUES (?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, imp := range imports {
		if _, err := stmt.Exec(pkgPath, imp); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// InsertCall records a MsgCall.
func (d *DB) InsertCall(txHash string, blockHeight int, caller, pkgPath, funcName string, success bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec(`
		INSERT OR IGNORE INTO calls (tx_hash, block_height, caller, pkg_path, func_name, success)
		VALUES (?, ?, ?, ?, ?, ?)
	`, txHash, blockHeight, caller, pkgPath, funcName, success)
	return err
}

// InsertMsgRun records a MsgRun transaction with its source.
func (d *DB) InsertMsgRun(txHash string, blockHeight int, caller, source string, success bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec(`
		INSERT OR IGNORE INTO msg_runs (tx_hash, block_height, caller, source, success)
		VALUES (?, ?, ?, ?, ?)
	`, txHash, blockHeight, caller, source, success)
	return err
}

func (d *DB) InsertBankSend(txHash string, blockHeight int, from, to, amount string, success bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec(`INSERT OR IGNORE INTO bank_sends (tx_hash, block_height, from_address, to_address, amount, success) VALUES (?, ?, ?, ?, ?, ?)`,
		txHash, blockHeight, from, to, amount, success)
	return err
}

// GetSyncState reads a sync state value.
func (d *DB) GetSyncState(key string) (string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var val string
	err := d.db.QueryRow(`SELECT value FROM sync_state WHERE key = ?`, key).Scan(&val)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return val, err
}

// SetSyncState writes a sync state value.
func (d *DB) SetSyncState(key, value string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec(`
		INSERT OR REPLACE INTO sync_state (key, value) VALUES (?, ?)
	`, key, value)
	return err
}

type PackageInfo struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	Creator     string `json:"creator"`
	BlockHeight int    `json:"block_height"`
	TxHash      string `json:"tx_hash"`
	IsRealm     bool   `json:"is_realm"`
	NumFiles    int    `json:"num_files"`
}

type PackageDetail struct {
	PackageInfo
	Files       []FileInfo    `json:"files"`
	Imports     []string      `json:"imports"`
	Dependents  []string      `json:"dependents"`
	Callers     []CallInfo    `json:"recent_calls"`
	MsgRunRefs  []MsgRunInfo  `json:"msgrun_refs"`
	CallCount   int           `json:"call_count"`
}

type FileInfo struct {
	Name string `json:"name"`
	Body string `json:"body"`
}

type CallInfo struct {
	TxHash      string `json:"tx_hash"`
	BlockHeight int    `json:"block_height"`
	Caller      string `json:"caller"`
	FuncName    string `json:"func_name"`
	Success     bool   `json:"success"`
}

type MsgRunInfo struct {
	TxHash      string `json:"tx_hash"`
	BlockHeight int    `json:"block_height"`
	Caller      string `json:"caller"`
	Success     bool   `json:"success"`
}

func (d *DB) CountPackages(realmOnly bool) (int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	q := `SELECT COUNT(*) FROM packages`
	if realmOnly {
		q += ` WHERE is_realm = 1`
	} else {
		q += ` WHERE is_realm = 0`
	}
	var count int
	err := d.db.QueryRow(q).Scan(&count)
	return count, err
}

// ListPackages returns all packages, optionally filtered.
func (d *DB) ListPackages(realmOnly bool, limit, offset int) ([]PackageInfo, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	q := `SELECT path, name, creator, block_height, tx_hash, is_realm, num_files FROM packages`
	var args []any
	if realmOnly {
		q += ` WHERE is_realm = 1`
	} else {
		q += ` WHERE is_realm = 0`
	}
	q += ` ORDER BY block_height DESC`
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT %d OFFSET %d`, limit, offset)
	}

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pkgs []PackageInfo
	for rows.Next() {
		var p PackageInfo
		if err := rows.Scan(&p.Path, &p.Name, &p.Creator, &p.BlockHeight, &p.TxHash, &p.IsRealm, &p.NumFiles); err != nil {
			return nil, err
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, rows.Err()
}

// GetPackageDetail returns full details for a package.
func (d *DB) GetPackageDetail(path string) (*PackageDetail, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var p PackageDetail
	err := d.db.QueryRow(`
		SELECT path, name, creator, block_height, tx_hash, is_realm, num_files
		FROM packages WHERE path = ?
	`, path).Scan(&p.Path, &p.Name, &p.Creator, &p.BlockHeight, &p.TxHash, &p.IsRealm, &p.NumFiles)
	if err != nil {
		return nil, err
	}

	// Files
	rows, err := d.db.Query(`SELECT file_name, body FROM package_files WHERE package_path = ?`, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f FileInfo
		if err := rows.Scan(&f.Name, &f.Body); err != nil {
			return nil, err
		}
		p.Files = append(p.Files, f)
	}

	// Imports (dependencies)
	impRows, err := d.db.Query(`SELECT import_path FROM dependencies WHERE package_path = ?`, path)
	if err != nil {
		return nil, err
	}
	defer impRows.Close()
	for impRows.Next() {
		var imp string
		if err := impRows.Scan(&imp); err != nil {
			return nil, err
		}
		p.Imports = append(p.Imports, imp)
	}

	// Dependents (who imports this)
	depRows, err := d.db.Query(`SELECT package_path FROM dependencies WHERE import_path = ?`, path)
	if err != nil {
		return nil, err
	}
	defer depRows.Close()
	for depRows.Next() {
		var dep string
		if err := depRows.Scan(&dep); err != nil {
			return nil, err
		}
		p.Dependents = append(p.Dependents, dep)
	}

	// Recent calls
	callRows, err := d.db.Query(`
		SELECT tx_hash, block_height, caller, func_name, success
		FROM calls WHERE pkg_path = ?
		ORDER BY block_height DESC LIMIT 50
	`, path)
	if err != nil {
		return nil, err
	}
	defer callRows.Close()
	for callRows.Next() {
		var c CallInfo
		if err := callRows.Scan(&c.TxHash, &c.BlockHeight, &c.Caller, &c.FuncName, &c.Success); err != nil {
			return nil, err
		}
		p.Callers = append(p.Callers, c)
	}

	// Call count
	d.db.QueryRow(`SELECT COUNT(*) FROM calls WHERE pkg_path = ?`, path).Scan(&p.CallCount)

	// MsgRun references (where source contains import of this path)
	runRows, err := d.db.Query(`
		SELECT tx_hash, block_height, caller, success
		FROM msg_runs WHERE source LIKE ?
		ORDER BY block_height DESC LIMIT 50
	`, "%"+path+"%")
	if err != nil {
		return nil, err
	}
	defer runRows.Close()
	for runRows.Next() {
		var r MsgRunInfo
		if err := runRows.Scan(&r.TxHash, &r.BlockHeight, &r.Caller, &r.Success); err != nil {
			return nil, err
		}
		p.MsgRunRefs = append(p.MsgRunRefs, r)
	}

	return &p, nil
}

// GetDependencyGraph returns the full dependency graph for a package (recursive).
func (d *DB) GetDependencyGraph(path string) (map[string][]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	graph := make(map[string][]string)
	visited := make(map[string]bool)

	var walk func(p string) error
	walk = func(p string) error {
		if visited[p] {
			return nil
		}
		visited[p] = true

		rows, err := d.db.Query(`SELECT import_path FROM dependencies WHERE package_path = ?`, p)
		if err != nil {
			return err
		}
		defer rows.Close()

		var deps []string
		for rows.Next() {
			var dep string
			if err := rows.Scan(&dep); err != nil {
				return err
			}
			deps = append(deps, dep)
		}
		graph[p] = deps

		for _, dep := range deps {
			if strings.HasPrefix(dep, "gno.land/") {
				if err := walk(dep); err != nil {
					return err
				}
			}
		}
		return nil
	}

	if err := walk(path); err != nil {
		return nil, err
	}
	return graph, nil
}

// GetReverseGraph returns all packages that depend on path (recursive).
func (d *DB) GetReverseGraph(path string) (map[string][]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	graph := make(map[string][]string)
	visited := make(map[string]bool)

	var walk func(p string) error
	walk = func(p string) error {
		if visited[p] {
			return nil
		}
		visited[p] = true

		rows, err := d.db.Query(`SELECT package_path FROM dependencies WHERE import_path = ?`, p)
		if err != nil {
			return err
		}
		defer rows.Close()

		var deps []string
		for rows.Next() {
			var dep string
			if err := rows.Scan(&dep); err != nil {
				return err
			}
			deps = append(deps, dep)
		}
		graph[p] = deps

		for _, dep := range deps {
			if err := walk(dep); err != nil {
				return err
			}
		}
		return nil
	}

	if err := walk(path); err != nil {
		return nil, err
	}
	return graph, nil
}

type Stats struct {
	TotalTxs       int `json:"total_txs"`
	TotalCalls     int `json:"total_calls"`
	TotalDeploys   int `json:"total_deploys"`
	TotalMsgRuns   int `json:"total_msg_runs"`
	TotalSends     int `json:"total_sends"`
	TotalRealms    int `json:"total_realms"`
	TotalPackages  int `json:"total_packages"`
	UniqueCallers  int `json:"unique_callers"`
	LatestBlock    int `json:"latest_block"`
}

// GetStats returns aggregate statistics.
func (d *DB) GetStats() (*Stats, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var s Stats
	d.db.QueryRow(`SELECT COUNT(*) FROM calls`).Scan(&s.TotalCalls)
	d.db.QueryRow(`SELECT COUNT(*) FROM packages`).Scan(&s.TotalDeploys)
	d.db.QueryRow(`SELECT COUNT(*) FROM packages WHERE is_realm = 1`).Scan(&s.TotalRealms)
	s.TotalPackages = s.TotalDeploys - s.TotalRealms
	d.db.QueryRow(`SELECT COUNT(*) FROM msg_runs`).Scan(&s.TotalMsgRuns)
	d.db.QueryRow(`SELECT COUNT(*) FROM bank_sends`).Scan(&s.TotalSends)
	s.TotalTxs = s.TotalCalls + s.TotalDeploys + s.TotalMsgRuns + s.TotalSends
	d.db.QueryRow(`SELECT COUNT(DISTINCT caller) FROM calls`).Scan(&s.UniqueCallers)
	d.db.QueryRow(`SELECT COALESCE(MAX(block_height), 0) FROM packages`).Scan(&s.LatestBlock)
	return &s, nil
}

// Search searches across packages and callers.
func (d *DB) Search(q string) ([]PackageInfo, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.Query(`
		SELECT path, name, creator, block_height, tx_hash, is_realm, num_files
		FROM packages
		WHERE path LIKE ? OR name LIKE ? OR creator LIKE ?
		ORDER BY block_height DESC
		LIMIT 20
	`, "%"+q+"%", "%"+q+"%", "%"+q+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pkgs []PackageInfo
	for rows.Next() {
		var p PackageInfo
		if err := rows.Scan(&p.Path, &p.Name, &p.Creator, &p.BlockHeight, &p.TxHash, &p.IsRealm, &p.NumFiles); err != nil {
			return nil, err
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, rows.Err()
}

type TokenInfo struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Creator   string `json:"creator"`
	CallCount int    `json:"call_count"`
}

func (d *DB) GetTokenPackages() ([]TokenInfo, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	rows, err := d.db.Query(`
		SELECT DISTINCT p.path, p.name, p.creator
		FROM packages p
		JOIN dependencies dep ON dep.package_path = p.path
		WHERE dep.import_path LIKE '%grc20%'
		ORDER BY p.block_height DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tokens []TokenInfo
	for rows.Next() {
		var t TokenInfo
		if err := rows.Scan(&t.Path, &t.Name, &t.Creator); err != nil {
			return nil, err
		}
		d.db.QueryRow(`SELECT COUNT(*) FROM calls WHERE pkg_path = ?`, t.Path).Scan(&t.CallCount)
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

type AccountInfo struct {
	Address     string `json:"address"`
	CallCount   int    `json:"call_count"`
	DeployCount int    `json:"deploy_count"`
	MsgRunCount int    `json:"msgrun_count"`
	SendCount   int    `json:"send_count"`
}

func (d *DB) GetActiveAccounts() ([]AccountInfo, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	rows, err := d.db.Query(`
		SELECT address, SUM(call_count), SUM(deploy_count), SUM(run_count), SUM(send_count)
		FROM (
			SELECT caller as address, COUNT(*) as call_count, 0 as deploy_count, 0 as run_count, 0 as send_count FROM calls GROUP BY caller
			UNION ALL
			SELECT creator as address, 0, COUNT(*), 0, 0 FROM packages GROUP BY creator
			UNION ALL
			SELECT caller as address, 0, 0, COUNT(*), 0 FROM msg_runs GROUP BY caller
			UNION ALL
			SELECT from_address as address, 0, 0, 0, COUNT(*) FROM bank_sends GROUP BY from_address
			UNION ALL
			SELECT to_address as address, 0, 0, 0, COUNT(*) FROM bank_sends GROUP BY to_address
		)
		GROUP BY address
		ORDER BY (SUM(call_count) + SUM(deploy_count) + SUM(run_count) + SUM(send_count)) DESC
		LIMIT 100
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var accounts []AccountInfo
	for rows.Next() {
		var a AccountInfo
		if err := rows.Scan(&a.Address, &a.CallCount, &a.DeployCount, &a.MsgRunCount, &a.SendCount); err != nil {
			return nil, err
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}
