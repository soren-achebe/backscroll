package store

import (
	"time"
)

// HistEntry is one shell-history entry to seed the DB with (see
// internal/histimport). No output exists for these; they are stored
// like sync-imported rows (machine "hist:<source>") so (machine, seq)
// makes re-import a no-op and sync export never ships them.
type HistEntry struct {
	Seq     int64
	Cmd     string
	Cwd     string
	Host    string
	Exit    int
	HasExit bool
	Started time.Time // zero = source had no timestamps (stored as 0)
	Ended   time.Time
}

// AddHistory bulk-inserts history entries in one transaction (a plain
// per-row AddImported round-trip is ~1ms of fsync each — far too slow
// for a 50k-line history file). Returns how many rows were actually
// added; rows whose (machine, seq) already exist are skipped.
func (s *Store) AddHistory(machine string, entries []HistEntry) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	ins, err := tx.Prepare(`INSERT OR IGNORE INTO commands
		(session_id, cmd, cwd, exit_code, started_at, ended_at,
		 output, output_bytes, truncated, machine, host, origin_seq, plain_z)
		VALUES(0,?,?,?,?,?,?,0,0,?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer ins.Close()
	fts, err := tx.Prepare(`INSERT INTO commands_fts(rowid, cmd, output) VALUES(?,?,'')`)
	if err != nil {
		return 0, err
	}
	defer fts.Close()
	empty := s.enc.EncodeAll(nil, nil)
	added := 0
	for _, e := range entries {
		var ec any
		if e.HasExit {
			ec = e.Exit
		}
		var cwd any
		if e.Cwd != "" {
			cwd = e.Cwd
		}
		var host any
		if e.Host != "" {
			host = e.Host
		}
		var started int64
		if !e.Started.IsZero() {
			started = e.Started.UnixMilli()
		}
		var ended any // NULL when the source has no duration info
		if !e.Ended.IsZero() && e.Ended.After(e.Started) {
			ended = e.Ended.UnixMilli()
		}
		res, err := ins.Exec(e.Cmd, cwd, ec, started, ended, empty, machine, host, e.Seq, empty)
		if err != nil {
			return added, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		id, _ := res.LastInsertId()
		if _, err := fts.Exec(id, e.Cmd); err != nil {
			return added, err
		}
		added++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return added, nil
}
