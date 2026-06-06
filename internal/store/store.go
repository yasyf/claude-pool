// Package store is cc-pool's sole state layer: a modernc.org/sqlite
// (pure-Go) database holding accounts, usage samples, sessions, and the
// refresh log. It stores NO secrets — the Keychain is the only secret store.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Store wraps the sqlite connection.
type Store struct {
	db *sql.DB
}

const schemaVersion = 1

const schema = `
CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS accounts (
  id               INTEGER PRIMARY KEY,
  config_dir       TEXT NOT NULL UNIQUE,
  keychain_service TEXT NOT NULL,
  keychain_account TEXT NOT NULL,
  label            TEXT NOT NULL DEFAULT '',
  overlay_kind     TEXT NOT NULL DEFAULT 'symlink',
  created_at       INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS usage_samples (
  account_id     INTEGER NOT NULL,
  ts             INTEGER NOT NULL,
  util_5h        REAL,
  util_7d        REAL,
  util_7d_opus   REAL,
  resets_5h      INTEGER,
  resets_7d      INTEGER,
  resets_7d_opus INTEGER,
  rate_limited   INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (account_id, ts)
);
CREATE INDEX IF NOT EXISTS idx_usage_acct_ts ON usage_samples(account_id, ts DESC);
CREATE TABLE IF NOT EXISTS sessions (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id INTEGER NOT NULL,
  pid        INTEGER,
  config_dir TEXT,
  started_at INTEGER NOT NULL,
  ended_at   INTEGER
);
CREATE INDEX IF NOT EXISTS idx_sessions_active ON sessions(account_id) WHERE ended_at IS NULL;
CREATE TABLE IF NOT EXISTS refresh_log (
  account_id INTEGER NOT NULL,
  ts         INTEGER NOT NULL,
  ok         INTEGER NOT NULL,
  err        TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (account_id, ts)
);
CREATE TABLE IF NOT EXISTS sticky (
  cwd         TEXT PRIMARY KEY,
  account_id  INTEGER NOT NULL,
  selected_at INTEGER NOT NULL
);
`

// Open opens (creating if needed) the database at path and applies the schema.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // serialize writes; sqlite + WAL is fine for our load
	s := &Store{db: db}
	if err := s.applySchema(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) applySchema() error {
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	_, err := s.db.Exec(
		`INSERT INTO meta(key,value) VALUES('schema_version',?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		fmt.Sprint(schemaVersion))
	return err
}

// GetMeta returns the meta value for key, ok=false if absent.
func (s *Store) GetMeta(key string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get meta %q: %w", key, err)
	}
	return v, true, nil
}

// SetMeta upserts a meta key.
func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO meta(key,value) VALUES(?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("set meta %q: %w", key, err)
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// UpsertAccount inserts or replaces an account row by id.
func (s *Store) UpsertAccount(a Account) error {
	created := a.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	_, err := s.db.Exec(
		`INSERT INTO accounts(id,config_dir,keychain_service,keychain_account,label,overlay_kind,created_at)
		 VALUES(?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   config_dir=excluded.config_dir,
		   keychain_service=excluded.keychain_service,
		   keychain_account=excluded.keychain_account,
		   label=excluded.label,
		   overlay_kind=excluded.overlay_kind`,
		a.ID, a.ConfigDir, a.KeychainService, a.KeychainAccount, a.Label, a.OverlayKind, created.Unix())
	if err != nil {
		return fmt.Errorf("upsert account %d: %w", a.ID, err)
	}
	return nil
}

// scanAccount decodes one account row; the parameter is satisfied by both
// *sql.Row and *sql.Rows.
func scanAccount(rows interface{ Scan(...any) error }) (Account, error) {
	var a Account
	var created int64
	if err := rows.Scan(&a.ID, &a.ConfigDir, &a.KeychainService, &a.KeychainAccount,
		&a.Label, &a.OverlayKind, &created); err != nil {
		return a, err
	}
	a.CreatedAt = time.Unix(created, 0)
	return a, nil
}

const accountCols = `id,config_dir,keychain_service,keychain_account,label,overlay_kind,created_at`

// ListAccounts returns all accounts ordered by id.
func (s *Store) ListAccounts() ([]Account, error) {
	rows, err := s.db.Query(`SELECT ` + accountCols + ` FROM accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAccount returns one account by id.
func (s *Store) GetAccount(id int) (Account, error) {
	row := s.db.QueryRow(`SELECT `+accountCols+` FROM accounts WHERE id=?`, id)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return a, fmt.Errorf("account %d not found", id)
	}
	return a, err
}

// DeleteAccount removes an account and its dependent rows.
func (s *Store) DeleteAccount(id int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM usage_samples WHERE account_id=?`,
		`DELETE FROM sessions WHERE account_id=?`,
		`DELETE FROM refresh_log WHERE account_id=?`,
		`DELETE FROM sticky WHERE account_id=?`,
		`DELETE FROM accounts WHERE id=?`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// NextAccountIndex returns the smallest unused account index >= 1.
func (s *Store) NextAccountIndex() (int, error) {
	rows, err := s.db.Query(`SELECT id FROM accounts ORDER BY id`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	used := map[int]bool{}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		used[id] = true
	}
	for n := 1; ; n++ {
		if !used[n] {
			return n, nil
		}
	}
}

func tsOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Unix()
}

// InsertUsageSample records one usage poll.
func (s *Store) InsertUsageSample(u UsageSample) error {
	rl := 0
	if u.RateLimited {
		rl = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO usage_samples(account_id,ts,util_5h,util_7d,util_7d_opus,resets_5h,resets_7d,resets_7d_opus,rate_limited)
		 VALUES(?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(account_id,ts) DO NOTHING`,
		u.AccountID, u.TS.Unix(), u.Util5h, u.Util7d, u.Util7dOpus,
		tsOrNil(u.Resets5h), tsOrNil(u.Resets7d), tsOrNil(u.Resets7dOpus), rl)
	return err
}

// LatestUsageSample returns the most recent sample for an account, or ok=false.
func (s *Store) LatestUsageSample(accountID int) (UsageSample, bool, error) {
	row := s.db.QueryRow(
		`SELECT account_id,ts,util_5h,util_7d,util_7d_opus,resets_5h,resets_7d,resets_7d_opus,rate_limited
		 FROM usage_samples WHERE account_id=? ORDER BY ts DESC LIMIT 1`, accountID)
	var u UsageSample
	var ts int64
	var u5, u7, u7o sql.NullFloat64
	var r5, r7, r7o sql.NullInt64
	var rl int
	if err := row.Scan(&u.AccountID, &ts, &u5, &u7, &u7o, &r5, &r7, &r7o, &rl); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return u, false, nil
		}
		return u, false, err
	}
	u.TS = time.Unix(ts, 0)
	u.Util5h, u.Util7d, u.Util7dOpus = u5.Float64, u7.Float64, u7o.Float64
	if r5.Valid {
		u.Resets5h = time.Unix(r5.Int64, 0)
	}
	if r7.Valid {
		u.Resets7d = time.Unix(r7.Int64, 0)
	}
	if r7o.Valid {
		u.Resets7dOpus = time.Unix(r7o.Int64, 0)
	}
	u.RateLimited = rl != 0
	return u, true, nil
}

// RecentUsageSamples returns up to limit of an account's most recent samples,
// newest first. Used to estimate the utilization burn rate.
func (s *Store) RecentUsageSamples(accountID, limit int) ([]UsageSample, error) {
	rows, err := s.db.Query(
		`SELECT account_id,ts,util_5h,util_7d,util_7d_opus,resets_5h,resets_7d,resets_7d_opus,rate_limited
		 FROM usage_samples WHERE account_id=? ORDER BY ts DESC LIMIT ?`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageSample
	for rows.Next() {
		var u UsageSample
		var ts int64
		var u5, u7, u7o sql.NullFloat64
		var r5, r7, r7o sql.NullInt64
		var rl int
		if err := rows.Scan(&u.AccountID, &ts, &u5, &u7, &u7o, &r5, &r7, &r7o, &rl); err != nil {
			return nil, err
		}
		u.TS = time.Unix(ts, 0)
		u.Util5h, u.Util7d, u.Util7dOpus = u5.Float64, u7.Float64, u7o.Float64
		if r5.Valid {
			u.Resets5h = time.Unix(r5.Int64, 0)
		}
		if r7.Valid {
			u.Resets7d = time.Unix(r7.Int64, 0)
		}
		if r7o.Valid {
			u.Resets7dOpus = time.Unix(r7o.Int64, 0)
		}
		u.RateLimited = rl != 0
		out = append(out, u)
	}
	return out, rows.Err()
}

// OpenSession records a new checkout and returns its id.
func (s *Store) OpenSession(accountID, pid int, configDir string) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO sessions(account_id,pid,config_dir,started_at) VALUES(?,?,?,?)`,
		accountID, pid, configDir, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// CloseSession marks a session ended by id.
func (s *Store) CloseSession(id int64) error {
	_, err := s.db.Exec(`UPDATE sessions SET ended_at=? WHERE id=? AND ended_at IS NULL`,
		time.Now().Unix(), id)
	return err
}

// ActiveSessionCount returns the number of live sessions for an account.
func (s *Store) ActiveSessionCount(accountID int) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM sessions WHERE account_id=? AND ended_at IS NULL`, accountID).Scan(&n)
	return n, err
}

// ListActiveSessions returns all live sessions across accounts.
func (s *Store) ListActiveSessions() ([]Session, error) {
	rows, err := s.db.Query(
		`SELECT id,account_id,pid,config_dir,started_at FROM sessions WHERE ended_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var se Session
		var started int64
		var pid sql.NullInt64
		var cd sql.NullString
		if err := rows.Scan(&se.ID, &se.AccountID, &pid, &cd, &started); err != nil {
			return nil, err
		}
		se.PID = int(pid.Int64)
		se.ConfigDir = cd.String
		se.StartedAt = time.Unix(started, 0)
		out = append(out, se)
	}
	return out, rows.Err()
}

// CloseDeadSessions ends every active session whose pid is not in alive.
func (s *Store) CloseDeadSessions(alive map[int]bool) (int, error) {
	sessions, err := s.ListActiveSessions()
	if err != nil {
		return 0, err
	}
	closed := 0
	for _, se := range sessions {
		if se.PID > 0 && !alive[se.PID] {
			if err := s.CloseSession(se.ID); err != nil {
				return closed, err
			}
			closed++
		}
	}
	return closed, nil
}

// UpsertSticky records (or refreshes) the account last selected for cwd.
func (s *Store) UpsertSticky(cwd string, accountID int, at time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO sticky(cwd,account_id,selected_at) VALUES(?,?,?)
		 ON CONFLICT(cwd) DO UPDATE SET
		   account_id=excluded.account_id,
		   selected_at=excluded.selected_at`,
		cwd, accountID, at.Unix())
	if err != nil {
		return fmt.Errorf("upsert sticky for %s: %w", cwd, err)
	}
	return nil
}

// GetSticky returns the sticky record for cwd, ok=false if none exists.
func (s *Store) GetSticky(cwd string) (Sticky, bool, error) {
	row := s.db.QueryRow(`SELECT cwd,account_id,selected_at FROM sticky WHERE cwd=?`, cwd)
	var st Sticky
	var at int64
	if err := row.Scan(&st.Cwd, &st.AccountID, &at); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return st, false, nil
		}
		return st, false, err
	}
	st.SelectedAt = time.Unix(at, 0)
	return st, true, nil
}

// PruneSticky deletes sticky rows last refreshed before cutoff, returning the
// number deleted.
func (s *Store) PruneSticky(cutoff time.Time) (int, error) {
	res, err := s.db.Exec(`DELETE FROM sticky WHERE selected_at < ?`, cutoff.Unix())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// LogRefresh records a refresh attempt outcome.
func (s *Store) LogRefresh(accountID int, ok bool, errMsg string) error {
	v := 0
	if ok {
		v = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO refresh_log(account_id,ts,ok,err) VALUES(?,?,?,?)
		 ON CONFLICT(account_id,ts) DO NOTHING`,
		accountID, time.Now().Unix(), v, errMsg)
	return err
}

// LastRefresh returns the most recent refresh attempt for an account, ok=false
// if none.
func (s *Store) LastRefresh(accountID int) (RefreshEntry, bool, error) {
	row := s.db.QueryRow(
		`SELECT account_id,ts,ok,err FROM refresh_log WHERE account_id=? ORDER BY ts DESC LIMIT 1`, accountID)
	var e RefreshEntry
	var ts int64
	var ok int
	if err := row.Scan(&e.AccountID, &ts, &ok, &e.Err); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return e, false, nil
		}
		return e, false, err
	}
	e.TS = time.Unix(ts, 0)
	e.OK = ok != 0
	return e, true, nil
}
