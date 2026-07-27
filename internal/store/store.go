package store

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/soren-achebe/backscroll/internal/ansi"
	sqlite "modernc.org/sqlite"
)

// bks_z / bks_unz are zstd compress/decompress SQL functions. The FTS
// index is an external-content fts5 table whose content is the
// commands_plain VIEW, which decompresses commands.plain_z on demand via
// bks_unz — so the ANSI-stripped plain text is stored exactly once
// (compressed) instead of living uncompressed inside the FTS content
// table. bks_z exists for the one-time migration and for debugging.
//
// NOTE: this means the commands_plain view (and therefore FTS queries)
// only work from binaries that register these functions — i.e.
// backscroll >= 0.4.0. Plain sqlite3 CLI access to the commands table
// itself (cmd, cwd, timing, raw output blob) is unaffected.
var (
	sqlEnc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	sqlDec, _ = zstd.NewReader(nil)
)

func init() {
	sqlite.MustRegisterDeterministicScalarFunction("bks_unz", 1,
		func(ctx *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			b, ok := args[0].([]byte)
			if !ok || len(b) == 0 {
				return "", nil
			}
			out, err := sqlDec.DecodeAll(b, nil)
			if err != nil {
				return nil, fmt.Errorf("bks_unz: %w", err)
			}
			return string(out), nil
		})
	sqlite.MustRegisterDeterministicScalarFunction("bks_z", 1,
		func(ctx *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			var in []byte
			switch v := args[0].(type) {
			case string:
				in = []byte(v)
			case []byte:
				in = v
			case nil:
				in = nil
			default:
				return nil, fmt.Errorf("bks_z: unsupported type %T", v)
			}
			return sqlEnc.EncodeAll(in, nil), nil
		})
}

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
	// v2: sync export state (single kv table; today just the high-water
	// local command id already written to our own sync log).
	{
		`CREATE TABLE IF NOT EXISTS sync_state(
		   k TEXT PRIMARY KEY,
		   v INTEGER NOT NULL
		 )`,
	},
	// v3: stop storing plain text uncompressed inside FTS. The old
	// commands_fts was a normal (content-storing) fts5 table, so every
	// command's ANSI-stripped output lived in the DB twice: zstd'd in
	// commands.output and uncompressed in commands_fts_content — the
	// latter dominated DB size (measured: 67 of 151 MB on a 5k-command
	// soak DB). Now plain text is zstd'd into commands.plain_z and
	// commands_fts is an external-content table over the commands_plain
	// view, which decompresses on demand (bks_unz). snippet()/highlight()
	// keep working — fts5 pulls text through the view when it needs it.
	// All index writes must go through the fts5 special commands from
	// here on (see AddCommand/Delete/Prune/UpdateOutput).
	{
		`ALTER TABLE commands ADD COLUMN plain_z BLOB`,
		`UPDATE commands SET plain_z = bks_z(coalesce(
		   (SELECT f.output FROM commands_fts f WHERE f.rowid = commands.id), ''))`,
		`DROP TABLE commands_fts`,
		`CREATE VIEW commands_plain(id, cmd, output) AS
		   SELECT id, cmd, bks_unz(plain_z) FROM commands`,
		`CREATE VIRTUAL TABLE commands_fts USING fts5(
		   cmd, output, content='commands_plain', content_rowid='id',
		   tokenize='trigram')`,
		`INSERT INTO commands_fts(commands_fts) VALUES('rebuild')`,
	},
}

func migrate(db *sql.DB) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	startV := v
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
	// v3 dropped the (large) old FTS content table; reclaim the space
	// once, outside any transaction.
	if startV < 3 && v >= 3 {
		if _, err := db.Exec(`VACUUM`); err != nil {
			return fmt.Errorf("post-migration vacuum: %w", err)
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
	if len(plainText) > ftsCap {
		plainText = plainText[:ftsCap]
	}
	res, err := s.db.Exec(
		`INSERT INTO commands(session_id, cmd, cwd, exit_code, started_at, ended_at, output, output_bytes, truncated, plain_z)
		 VALUES(?,?,?,?,?,?,?,?,?,?)`,
		sessionID, cmd, cwd, ec, startedAt.UnixMilli(), endedAt.UnixMilli(),
		blob, len(rawOutput), boolInt(truncated), s.enc.EncodeAll([]byte(plainText), nil))
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
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
	fts := plainText
	if len(fts) > ftsCap {
		fts = fts[:ftsCap]
	}
	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO commands(session_id, cmd, cwd, exit_code, started_at, ended_at,
		   output, output_bytes, truncated, machine, host, origin_seq, plain_z)
		 VALUES(0,?,?,?,?,?,?,?,0,?,?,?,?)`,
		cmd, cwd, ec, startedAt.UnixMilli(), endedAt.UnixMilli(),
		blob, len(plainText), machine, host, seq, s.enc.EncodeAll([]byte(fts), nil))
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil // already imported
	}
	id, _ := res.LastInsertId()
	_, err = s.db.Exec(`INSERT INTO commands_fts(rowid, cmd, output) VALUES(?,?,?)`,
		id, cmd, fts)
	return true, err
}

// SyncState reads an int64 from the sync_state kv table (0 if unset).
func (s *Store) SyncState(k string) (int64, error) {
	var v int64
	err := s.db.QueryRow(`SELECT v FROM sync_state WHERE k=?`, k).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return v, err
}

// SetSyncState writes an int64 to the sync_state kv table.
func (s *Store) SetSyncState(k string, v int64) error {
	_, err := s.db.Exec(
		`INSERT INTO sync_state(k, v) VALUES(?,?)
		 ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

// ExportEntry is a locally recorded command paired with the plain
// (ANSI-stripped) text that FTS indexed — what sync export ships.
type ExportEntry struct {
	Command
	Plain string
}

// LocalAfter returns locally recorded commands with id > afterID in id
// order (up to limit), each with its FTS plain text. Sync export uses
// the local id as the per-machine sequence number.
func (s *Store) LocalAfter(afterID int64, limit int) ([]ExportEntry, error) {
	rows, err := s.db.Query(`
		SELECT c.id, c.cmd, coalesce(c.cwd,''), c.exit_code, c.started_at, c.ended_at,
		       bks_unz(c.plain_z)
		FROM commands c
		WHERE c.machine IS NULL AND c.id > ?
		ORDER BY c.id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExportEntry
	for rows.Next() {
		var e ExportEntry
		var st, en sql.NullInt64
		if err := rows.Scan(&e.ID, &e.Cmd, &e.Cwd, &e.ExitCode, &st, &en, &e.Plain); err != nil {
			return nil, err
		}
		if st.Valid {
			e.StartedAt = time.UnixMilli(st.Int64)
		}
		if en.Valid {
			e.EndedAt = time.UnixMilli(en.Int64)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MaxLocalID returns the highest id among locally recorded commands.
func (s *Store) MaxLocalID() (int64, error) {
	var id sql.NullInt64
	err := s.db.QueryRow(`SELECT max(id) FROM commands WHERE machine IS NULL`).Scan(&id)
	return id.Int64, err
}

// CountLocalAfter returns how many locally recorded commands have
// id > afterID (i.e. how many `sync export` would ship).
func (s *Store) CountLocalAfter(afterID int64) (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT count(*) FROM commands WHERE machine IS NULL AND id > ?`, afterID).Scan(&n)
	return n, err
}

// ImportState is one row of the per-foreign-machine import high-water table.
type ImportState struct {
	Machine string
	Host    string
	LastSeq int64
}

// ImportStates returns the import high-water marks for all known
// foreign machines.
func (s *Store) ImportStates() ([]ImportState, error) {
	rows, err := s.db.Query(`SELECT machine, coalesce(host,''), last_seq FROM import_state ORDER BY machine`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ImportState
	for rows.Next() {
		var is ImportState
		if err := rows.Scan(&is.Machine, &is.Host, &is.LastSeq); err != nil {
			return nil, err
		}
		out = append(out, is)
	}
	return out, rows.Err()
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
	Until   time.Time // non-zero: only commands started strictly before this time
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
	if !f.Until.IsZero() {
		conds = append(conds, tbl+`started_at<?`)
		args = append(args, f.Until.UnixMilli())
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

// Active reports whether any narrowing filter is set (Limit alone is not a
// filter).
func (f Filter) Active() bool {
	return f.Session > 0 || f.Cwd != "" || f.ExitSet || f.Failed || !f.Since.IsZero() || !f.Until.IsZero() || f.Host != ""
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

// Plain returns the ANSI-stripped text of a command's output — the same
// text the FTS index sees (capped at ftsCap). Rows written by a pre-0.4
// binary have no stored plain text; for those it falls back to stripping
// the raw output on the fly.
func (s *Store) Plain(id int64) (string, error) {
	row := s.db.QueryRow(`SELECT coalesce(bks_unz(plain_z), ''), output FROM commands WHERE id=?`, id)
	var plain string
	var blob []byte
	if err := row.Scan(&plain, &blob); err != nil {
		return "", err
	}
	if plain == "" && len(blob) > 0 {
		raw, err := s.dec.DecodeAll(blob, nil)
		if err != nil {
			return "", fmt.Errorf("decompress: %w", err)
		}
		p := ansi.Strip(raw)
		if len(p) > ftsCap {
			p = p[:ftsCap]
		}
		plain = string(p)
	}
	return plain, nil
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
	Imported int64 // of Commands, how many came from other machines via sync
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
	if err := s.db.QueryRow(`SELECT count(*) FROM commands WHERE machine IS NOT NULL`).Scan(&st.Imported); err != nil {
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
	// External-content fts5: index rows are removed with the 'delete'
	// special command, which must be given the originally indexed text.
	if _, err := s.db.Exec(`
		INSERT INTO commands_fts(commands_fts, rowid, cmd, output)
		SELECT 'delete', id, cmd, bks_unz(plain_z) FROM commands WHERE started_at < ?`,
		cutoff); err != nil {
		return 0, err
	}
	res, err := s.db.Exec(`DELETE FROM commands WHERE started_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	// fts5 'delete' only marks rows; merge the segments so the index
	// actually sheds the deleted data, then let VACUUM return the pages.
	if _, err := s.db.Exec(`INSERT INTO commands_fts(commands_fts) VALUES('optimize')`); err != nil {
		return n, err
	}
	if _, err := s.db.Exec(`VACUUM`); err != nil {
		return n, err
	}
	return n, nil
}

// Delete removes a single command by id.
func (s *Store) Delete(id int64) error {
	if err := s.ftsDelete(id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM commands WHERE id=?`, id)
	return err
}

// ftsDelete removes id's row from the external-content FTS index (must
// run while the commands row still holds the indexed text).
func (s *Store) ftsDelete(id int64) error {
	_, err := s.db.Exec(`
		INSERT INTO commands_fts(commands_fts, rowid, cmd, output)
		SELECT 'delete', id, cmd, bks_unz(plain_z) FROM commands WHERE id=?`, id)
	return err
}

// UpdateOutput replaces a command's stored command line, raw output and
// FTS text in place — used by `backscroll redact` to scrub secrets that
// were already recorded.
func (s *Store) UpdateOutput(id int64, cmd string, rawOutput []byte, plainText string) error {
	// Drop the old index entry first — the 'delete' special command needs
	// the text as originally indexed, i.e. before the row is updated.
	if err := s.ftsDelete(id); err != nil {
		return err
	}
	if len(plainText) > ftsCap {
		plainText = plainText[:ftsCap]
	}
	blob := s.enc.EncodeAll(rawOutput, nil)
	if _, err := s.db.Exec(
		`UPDATE commands SET cmd=?, output=?, output_bytes=?, plain_z=? WHERE id=?`,
		cmd, blob, len(rawOutput), s.enc.EncodeAll([]byte(plainText), nil), id); err != nil {
		return err
	}
	_, err := s.db.Exec(`INSERT INTO commands_fts(rowid, cmd, output) VALUES(?,?,?)`,
		id, cmd, plainText)
	return err
}

// RebuildFTS discards and rebuilds the whole FTS index from the
// commands_plain view. Safety valve for index drift (e.g. a crash
// between a commands write and its index write, or a pre-0.4 binary
// having written rows without plain_z). Exposed as `doctor --reindex`.
func (s *Store) RebuildFTS() error {
	// Rows written by a pre-0.4 binary have NULL plain_z; regenerate it
	// from the stored raw output so their output stays searchable.
	rows, err := s.db.Query(`SELECT id, output FROM commands WHERE plain_z IS NULL`)
	if err != nil {
		return err
	}
	type fix struct {
		id    int64
		plain string
	}
	var fixes []fix
	for rows.Next() {
		var id int64
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			rows.Close()
			return err
		}
		var plain string
		if len(blob) > 0 {
			raw, err := s.dec.DecodeAll(blob, nil)
			if err == nil {
				p := ansi.Strip(raw)
				if len(p) > ftsCap {
					p = p[:ftsCap]
				}
				plain = string(p)
			}
		}
		fixes = append(fixes, fix{id, plain})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, f := range fixes {
		if _, err := s.db.Exec(`UPDATE commands SET plain_z=? WHERE id=?`,
			s.enc.EncodeAll([]byte(f.plain), nil), f.id); err != nil {
			return err
		}
	}
	_, err = s.db.Exec(`INSERT INTO commands_fts(commands_fts) VALUES('rebuild')`)
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
