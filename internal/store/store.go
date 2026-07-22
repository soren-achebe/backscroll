package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
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
}

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

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func scanCmd(rows *sql.Rows) (Command, error) {
	var c Command
	var st, en sql.NullInt64
	err := rows.Scan(&c.ID, &c.SessionID, &c.Cmd, &c.Cwd, &c.ExitCode, &st, &en, &c.OutputLen, &c.Truncated)
	if st.Valid {
		c.StartedAt = time.UnixMilli(st.Int64)
	}
	if en.Valid {
		c.EndedAt = time.UnixMilli(en.Int64)
	}
	return c, err
}

const cmdCols = `id, session_id, cmd, coalesce(cwd,''), exit_code, started_at, ended_at, output_bytes, truncated`

func (s *Store) List(limit int, session int64) ([]Command, error) {
	q := `SELECT ` + cmdCols + ` FROM commands`
	var args []any
	if session > 0 {
		q += ` WHERE session_id=?`
		args = append(args, session)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
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
	var st, en sql.NullInt64
	var blob []byte
	err := row.Scan(&c.ID, &c.SessionID, &c.Cmd, &c.Cwd, &c.ExitCode, &st, &en, &c.OutputLen, &c.Truncated, &blob)
	if err != nil {
		return nil, err
	}
	if st.Valid {
		c.StartedAt = time.UnixMilli(st.Int64)
	}
	if en.Valid {
		c.EndedAt = time.UnixMilli(en.Int64)
	}
	if len(blob) > 0 {
		c.Output, err = s.dec.DecodeAll(blob, nil)
		if err != nil {
			return nil, fmt.Errorf("decompress: %w", err)
		}
	}
	return &c, nil
}

func (s *Store) Search(query string, limit int) ([]Command, error) {
	rows, err := s.db.Query(`
		SELECT c.id, c.session_id, c.cmd, coalesce(c.cwd,''), c.exit_code, c.started_at, c.ended_at,
		       c.output_bytes, c.truncated,
		       snippet(commands_fts, 1, char(27)||'[1;31m', char(27)||'[0m', '…', 12)
		FROM commands_fts JOIN commands c ON c.id = commands_fts.rowid
		WHERE commands_fts MATCH ?
		ORDER BY c.id DESC LIMIT ?`, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Command
	for rows.Next() {
		var c Command
		var st, en sql.NullInt64
		if err := rows.Scan(&c.ID, &c.SessionID, &c.Cmd, &c.Cwd, &c.ExitCode, &st, &en,
			&c.OutputLen, &c.Truncated, &c.Snippet); err != nil {
			return nil, err
		}
		if st.Valid {
			c.StartedAt = time.UnixMilli(st.Int64)
		}
		if en.Valid {
			c.EndedAt = time.UnixMilli(en.Int64)
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
