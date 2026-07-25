package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"
)

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
CREATE TABLE IF NOT EXISTS sessions(
  id INTEGER PRIMARY KEY,
  started_at INTEGER NOT NULL,
  hostname TEXT, shell TEXT, term TEXT
);
CREATE TABLE IF NOT EXISTS commands(
  id INTEGER PRIMARY KEY,
  session_id INTEGER NOT NULL REFERENCES sessions(id),
  cmd TEXT NOT NULL,
  cwd TEXT,
  exit_code INTEGER,
  started_at INTEGER NOT NULL,
  ended_at INTEGER,
  output BLOB,
  output_bytes INTEGER NOT NULL DEFAULT 0,
  truncated INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_commands_started ON commands(started_at);
CREATE VIRTUAL TABLE IF NOT EXISTS commands_fts USING fts5(cmd, output, tokenize='trigram');
`

// migrations are applied in order on top of the base schema; PRAGMA
// user_version tracks how many have run. Each entry is a list of
// statements executed in one transaction. Never edit an entry that has
// shipped — append a new one.
var migrations = [][]string{
	// v1: cross-machine sync groundwork (docs/sync-design.md).
	// Foreign entries carry the origin machine id + human-readable host
	// and their per-machine sequence number; (machine, origin_seq) is
	// unique so re-import is a no-op. Local entries keep NULLs.
	{
		`ALTER TABLE commands ADD COLUMN machine TEXT`,
		`ALTER TABLE commands ADD COLUMN host TEXT`,
		`ALTER TABLE commands ADD COLUMN origin_seq INTEGER`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_commands_origin
		   ON commands(machine, origin_seq) WHERE machine IS NOT NULL`,
		`CREATE TABLE IF NOT EXISTS import_state(
		   machine TEXT PRIMARY KEY,
		   host TEXT,
		   last_seq INTEGER NOT NULL,
		   updated_at INTEGER
		 )`,
	},
}

func migrate(db *sql.DB) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	for ; v < len(migrations); v++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		for _, stmt := range migrations[v] {
			if _, err := tx.Exec(stmt); err != nil {
				tx.Rollback()
				return fmt.Errorf("migration %d: %w", v+1, err)
			}
		}
		// PRAGMA cannot take a bound parameter.
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version=%d`, v+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

type Store struct {
	db  *sql.DB
	enc *zstd.Encoder
	dec *zstd.Decoder
}

func DefaultPath() string {
	if p := os.Getenv("BACKSCROLL_DB"); p != "" {
		return p
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "backscroll", "backscroll.db")
}

func Open() (*Store, error) {
	path := DefaultPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	dec, _ := zstd.NewReader(nil)
	return &Store{db: db, enc: enc, dec: dec}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) NewSession(shell, term string) (int64, error) {
	host, _ := os.Hostname()
	res, err := s.db.Exec(
		`INSERT INTO sessions(started_at, hostname, shell, term) VALUES(?,?,?,?)`,
		time.Now().UnixMilli(), host, shell, term)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

type Command struct {
	ID        int64
	SessionID int64
	Cmd       string
	Cwd       string
	ExitCode  sql.NullInt64
	StartedAt time.Time
	EndedAt   time.Time
	Output    []byte // decompressed, only populated by Get
	OutputLen int64
	Truncated bool
	Snippet   string // populated by Search
	Machine   string // origin machine id; "" = recorded locally
	Host      string // origin hostname; "" = recorded locally
	OriginSeq int64  // per-origin-machine sequence; 0 = local
}

// Local reports whether the command was recorded on this machine (as
// opposed to imported via sync).
func (c *Command) Local() bool { return c.Machine == "" }

const ftsCap = 256 << 10 // max plain text indexed per command

func (s *Store) AddCommand(sessionID int64, cmd, cwd string, exitCode int, hasExit bool,
	startedAt, endedAt time.Time, rawOutput []byte, truncated bool, plainText string) error {

	blob := s.enc.EncodeAll(rawOutput, nil)
	var ec any
	if hasExit {
		ec = exitCode
	}
	res, err := s.db.Exec(
		`INSERT INTO commands(session_id, cmd, cwd, exit_code, started_at, ended_at, output, output_bytes, truncated)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		sessionID, cmd, cwd, ec, startedAt.UnixMilli(), endedAt.UnixMilli(),
		blob, len(rawOutput), boolInt(truncated))
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	if len(plainText) > ftsCap {
		plainText = plainText[:ftsCap]
	}
	_, err = s.db.Exec(`INSERT INTO commands_fts(rowid, cmd, output) VALUES(?,?,?)`,
		id, cmd, plainText)
	return err
}

// AddImported inserts a history entry that was recorded on another
// machine (sync import). Foreign entries have no local session
// (session_id 0); output is the ANSI-stripped plain text, stored
// compressed like local output and FTS-indexed. Re-importing the same
// (machine, seq) is a no-op; the bool reports whether a row was added.
func (s *Store) AddImported(machine, host string, seq int64, cmd, cwd string,
	exitCode int, hasExit bool, startedAt, endedAt time.Time, plainText string) (bool, error) {

	blob := s.enc.EncodeAll([]byte(plainText), nil)
	var ec any
	if hasExit {
		ec = exitCode
	}
	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO commands(session_id, cmd, cwd, exit_code, started_at, ended_at,
		   output, output_bytes, truncated, machine, host, origin_seq)
		 VALUES(0,?,?,?,?,?,?,?,0,?,?,?)`,
		cmd, cwd, ec, startedAt.UnixMilli(), endedAt.UnixMilli(),
		blob, len(plainText), machine, host, seq)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil // already imported
	}
	id, _ := res.LastInsertId()
	if len(plainText) > ftsCap {
		plainText = plainText[:ftsCap]
	}
	_, err = s.db.Exec(`INSERT INTO commands_fts(rowid, cmd, output) VALUES(?,?,?)`,
		id, cmd, plainText)
	return true, err
}

// ImportedSeq returns the high-water mark for a foreign machine's log
// (0 if nothing imported yet).
func (s *Store) ImportedSeq(machine string) (int64, error) {
	var seq int64
	err := s.db.QueryRow(`SELECT last_seq FROM import_state WHERE machine=?`, machine).Scan(&seq)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return seq, err
}

// SetImportedSeq records the high-water mark after importing from a
// foreign machine's log.
func (s *Store) SetImportedSeq(machine, host string, seq int64) error {
	_, err := s.db.Exec(
		`INSERT INTO import_state(machine, host, last_seq, updated_at) VALUES(?,?,?,?)
		 ON CONFLICT(machine) DO UPDATE SET host=excluded.host,
		   last_seq=excluded.last_seq, updated_at=excluded.updated_at`,
		machine, host, seq, time.Now().UnixMilli())
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func scanCmd(rows *sql.Rows) (Command, error) {
	var c Command
	var st, en, oseq sql.NullInt64
	err := rows.Scan(&c.ID, &c.SessionID, &c.Cmd, &c.Cwd, &c.ExitCode, &st, &en, &c.OutputLen, &c.Truncated,
		&c.Machine, &c.Host, &oseq)
	if st.Valid {
		c.StartedAt = time.UnixMilli(st.Int64)
	}
	if en.Valid {
		c.EndedAt = time.UnixMilli(en.Int64)
	}
	if oseq.Valid {
		c.OriginSeq = oseq.Int64
	}
	return c, err
}

const cmdCols = `id, session_id, cmd, coalesce(cwd,''), exit_code, started_at, ended_at, output_bytes, truncated, coalesce(machine,''), coalesce(host,''), origin_seq`

// Filter narrows List/Search results. Zero values mean "no filter".
type Filter struct {
	Session int64     // >0: only this session
	Cwd     string    // non-empty: this directory or any directory beneath it
	Exit    int64     // with ExitSet: only this exit code
	ExitSet bool      // Exit is meaningful (allows filtering on 0)
	Failed  bool      // only nonzero exit codes
	Since   time.Time // non-zero: only commands started at/after this time
	Host    string    // non-empty: only this origin host; "local" = this machine
	Limit   int       // <=0: default 20
}

// where builds the WHERE clause (with leading " WHERE" or empty) and args
// for the filter, prefixing column names with tbl (e.g. "c.").
func (f Filter) where(tbl string) (string, []any) {
	var conds []string
	var args []any
	if f.Session > 0 {
		conds = append(conds, tbl+`session_id=?`)
		args = append(args, f.Session)
	}
	if f.Cwd != "" {
		conds = append(conds, `(`+tbl+`cwd=? OR `+tbl+`cwd LIKE ? ESCAPE '\')`)
		args = append(args, f.Cwd, likeEscape(f.Cwd)+`/%`)
	}
	if f.ExitSet {
		conds = append(conds, tbl+`exit_code=?`)
		args = append(args, f.Exit)
	} else if f.Failed {
		conds = append(conds, tbl+`exit_code IS NOT NULL AND `+tbl+`exit_code<>0`)
	}
	if !f.Since.IsZero() {
		conds = append(conds, tbl+`started_at>=?`)
		args = append(args, f.Since.UnixMilli())
	}
	if f.Host != "" {
		if f.Host == "local" {
			conds = append(conds, tbl+`machine IS NULL`)
		} else {
			conds = append(conds, tbl+`host=?`)
			args = append(args, f.Host)
		}
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func (f Filter) limit() int {
	if f.Limit <= 0 {
		return 20
	}
	return f.Limit
}

func (s *Store) List(f Filter) ([]Command, error) {
	where, args := f.where("")
	q := `SELECT ` + cmdCols + ` FROM commands` + where + ` ORDER BY id DESC LIMIT ?`
	args = append(args, f.limit())
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Command
	for rows.Next() {
		c, err := scanCmd(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Get fetches one command with output. n > 0 is an absolute id; n <= 0 means
// the (-n)th most recent (0 or -1 = latest, -2 = one before, ...).
func (s *Store) Get(n int64) (*Command, error) {
	var q string
	var arg any
	if n > 0 {
		q = `SELECT ` + cmdCols + `, output FROM commands WHERE id=?`
		arg = n
	} else {
		off := -n
		if off > 0 {
			off--
		}
		q = `SELECT ` + cmdCols + `, output FROM commands ORDER BY id DESC LIMIT 1 OFFSET ?`
		arg = off
	}
	row := s.db.QueryRow(q, arg)
	var c Command
	var st, en, oseq sql.NullInt64
	var blob []byte
	err := row.Scan(&c.ID, &c.SessionID, &c.Cmd, &c.Cwd, &c.ExitCode, &st, &en, &c.OutputLen, &c.Truncated,
		&c.Machine, &c.Host, &oseq, &blob)
	if err != nil {
		return nil, err
	}
	if st.Valid {
		c.StartedAt = time.UnixMilli(st.Int64)
	}
	if en.Valid {
		c.EndedAt = time.UnixMilli(en.Int64)
	}
	if oseq.Valid {
		c.OriginSeq = oseq.Int64
	}
	if len(blob) > 0 {
		c.Output, err = s.dec.DecodeAll(blob, nil)
		if err != nil {
			return nil, fmt.Errorf("decompress: %w", err)
		}
	}
	return &c, nil
}

func (s *Store) Search(query string, f Filter) ([]Command, error) {
	where, fargs := f.where("c.")
	extra := ""
	if where != "" {
		extra = " AND " + strings.TrimPrefix(where, " WHERE ")
	}
	args := append([]any{query}, fargs...)
	args = append(args, f.limit())
	rows, err := s.db.Query(`
		SELECT c.id, c.session_id, c.cmd, coalesce(c.cwd,''), c.exit_code, c.started_at, c.ended_at,
		       c.output_bytes, c.truncated, coalesce(c.machine,''), coalesce(c.host,''), c.origin_seq,
		       snippet(commands_fts, 1, char(27)||'[1;31m', char(27)||'[0m', '…', 12)
		FROM commands_fts JOIN commands c ON c.id = commands_fts.rowid
		WHERE commands_fts MATCH ?`+extra+`
		ORDER BY c.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Command
	for rows.Next() {
		var c Command
		var st, en, oseq sql.NullInt64
		if err := rows.Scan(&c.ID, &c.SessionID, &c.Cmd, &c.Cwd, &c.ExitCode, &st, &en,
			&c.OutputLen, &c.Truncated, &c.Machine, &c.Host, &oseq, &c.Snippet); err != nil {
			return nil, err
		}
		if st.Valid {
			c.StartedAt = time.UnixMilli(st.Int64)
		}
		if en.Valid {
			c.EndedAt = time.UnixMilli(en.Int64)
		}
		if oseq.Valid {
			c.OriginSeq = oseq.Int64
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type Stats struct {
	Commands int64
	Sessions int64
	RawBytes int64
	DBBytes  int64
	FirstAt  time.Time
}

func (s *Store) Stats() (Stats, error) {
	var st Stats
	var first sql.NullInt64
	err := s.db.QueryRow(`SELECT count(*), coalesce(sum(output_bytes),0), min(started_at) FROM commands`).
		Scan(&st.Commands, &st.RawBytes, &first)
	if err != nil {
		return st, err
	}
	if first.Valid {
		st.FirstAt = time.UnixMilli(first.Int64)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&st.Sessions); err != nil {
		return st, err
	}
	if fi, err := os.Stat(DefaultPath()); err == nil {
		st.DBBytes = fi.Size()
	}
	return st, nil
}

// Prune deletes commands older than the given duration.
func (s *Store) Prune(olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan).UnixMilli()
	if _, err := s.db.Exec(`DELETE FROM commands_fts WHERE rowid IN (SELECT id FROM commands WHERE started_at < ?)`, cutoff); err != nil {
		return 0, err
	}
	res, err := s.db.Exec(`DELETE FROM commands WHERE started_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := s.db.Exec(`VACUUM`); err != nil {
		return n, err
	}
	return n, nil
}

// Delete removes a single command by id.
func (s *Store) Delete(id int64) error {
	if _, err := s.db.Exec(`DELETE FROM commands_fts WHERE rowid=?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM commands WHERE id=?`, id)
	return err
}

// UpdateOutput replaces a command's stored command line, raw output and
// FTS text in place — used by `backscroll redact` to scrub secrets that
// were already recorded.
func (s *Store) UpdateOutput(id int64, cmd string, rawOutput []byte, plainText string) error {
	blob := s.enc.EncodeAll(rawOutput, nil)
	if _, err := s.db.Exec(
		`UPDATE commands SET cmd=?, output=?, output_bytes=? WHERE id=?`,
		cmd, blob, len(rawOutput), id); err != nil {
		return err
	}
	if len(plainText) > ftsCap {
		plainText = plainText[:ftsCap]
	}
	if _, err := s.db.Exec(`DELETE FROM commands_fts WHERE rowid=?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`INSERT INTO commands_fts(rowid, cmd, output) VALUES(?,?,?)`,
		id, cmd, plainText)
	return err
}

// PrevSame returns the most recent command before beforeID whose command
// line matches cmd exactly. Used by `backscroll diff <id>` to compare a
// command against its previous run.
func (s *Store) PrevSame(beforeID int64, cmd string) (*Command, error) {
	row := s.db.QueryRow(
		`SELECT id FROM commands WHERE id < ? AND cmd = ? ORDER BY id DESC LIMIT 1`,
		beforeID, cmd)
	var id int64
	if err := row.Scan(&id); err != nil {
		return nil, err
	}
	return s.Get(id)
}
