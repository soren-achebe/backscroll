package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// v0Schema is the schema as shipped before migrations existed (v0.2.x).
const v0Schema = `
CREATE TABLE sessions(
  id INTEGER PRIMARY KEY,
  started_at INTEGER NOT NULL,
  hostname TEXT, shell TEXT, term TEXT
);
CREATE TABLE commands(
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
CREATE INDEX idx_commands_started ON commands(started_at);
CREATE VIRTUAL TABLE commands_fts USING fts5(cmd, output, tokenize='trigram');
`

// TestMigrateFromV0 opens a database created by a pre-sync release and
// verifies the migration runs, old rows survive, and new columns work.
func TestMigrateFromV0(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	t.Setenv("BACKSCROLL_DB", path)

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(v0Schema); err != nil {
		t.Fatalf("v0 schema: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO commands(session_id, cmd, cwd, started_at, ended_at) VALUES(1,'echo hi','/tmp',1000,2000)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Open twice: migration must run once and be a no-op after.
	for i := 0; i < 2; i++ {
		st, err := Open()
		if err != nil {
			t.Fatalf("Open #%d on v0 db: %v", i+1, err)
		}
		c, err := st.Get(-1)
		if err != nil {
			t.Fatalf("Get after migrate: %v", err)
		}
		if c.Cmd != "echo hi" || !c.Local() {
			t.Fatalf("legacy row wrong: %+v", c)
		}
		st.Close()
	}

	db, _ = sql.Open("sqlite", path)
	defer db.Close()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != len(migrations) {
		t.Fatalf("user_version = %d, want %d", v, len(migrations))
	}
}

func TestAddImportedIdempotent(t *testing.T) {
	st := testStore(t)
	at := time.Now().Add(-time.Hour)

	added, err := st.AddImported("m1", "laptop", 7, "uname -a", "/home/u",
		0, true, at, at.Add(time.Second), "Linux laptop 6.1")
	if err != nil || !added {
		t.Fatalf("first import: added=%v err=%v", added, err)
	}
	added, err = st.AddImported("m1", "laptop", 7, "uname -a", "/home/u",
		0, true, at, at.Add(time.Second), "Linux laptop 6.1")
	if err != nil || added {
		t.Fatalf("re-import: added=%v err=%v, want no-op", added, err)
	}

	c, err := st.Get(-1)
	if err != nil {
		t.Fatal(err)
	}
	if c.Local() || c.Machine != "m1" || c.Host != "laptop" || c.OriginSeq != 7 {
		t.Fatalf("origin fields wrong: %+v", c)
	}
	if string(c.Output) != "Linux laptop 6.1" {
		t.Fatalf("output = %q", c.Output)
	}

	// Imported text must be searchable, and only stored once.
	res, err := st.Search(`"laptop 6.1"`, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Host != "laptop" {
		t.Fatalf("search over imported: %+v", res)
	}
}

func TestHostFilter(t *testing.T) {
	st := testStore(t)
	sess, err := st.NewSession("bash", "xterm")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour)
	add(t, st, sess, "echo local", "local out", 0, at)
	if _, err := st.AddImported("m1", "laptop", 1, "echo a", "/", 0, true, at, at, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddImported("m2", "buildbox", 1, "echo b", "/", 0, true, at, at, "b"); err != nil {
		t.Fatal(err)
	}

	for host, wantCmd := range map[string]string{
		"local":    "echo local",
		"laptop":   "echo a",
		"buildbox": "echo b",
	} {
		got, err := st.List(Filter{Host: host})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Cmd != wantCmd {
			t.Fatalf("Host=%q: got %+v, want 1 row %q", host, got, wantCmd)
		}
	}
	all, err := st.List(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("unfiltered: %d rows, want 3", len(all))
	}
}

func TestImportedSeq(t *testing.T) {
	st := testStore(t)
	if seq, err := st.ImportedSeq("nope"); err != nil || seq != 0 {
		t.Fatalf("unknown machine: seq=%d err=%v", seq, err)
	}
	if err := st.SetImportedSeq("m1", "laptop", 42); err != nil {
		t.Fatal(err)
	}
	if err := st.SetImportedSeq("m1", "laptop2", 99); err != nil {
		t.Fatal(err)
	}
	if seq, err := st.ImportedSeq("m1"); err != nil || seq != 99 {
		t.Fatalf("seq=%d err=%v, want 99", seq, err)
	}
}
