package synclog

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/soren-achebe/backscroll/internal/store"
)

func TestSegmentRoundtrip(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	ex := int64(1)
	recs := []Record{
		{Seq: 1, Host: "laptop", Cmd: "echo hi", Cwd: "/tmp", Exit: &ex, Start: 100, End: 200, Out: "hi\n"},
		{Seq: 2, Host: "laptop", Cmd: "true", Start: 300, End: 400},
	}
	blob, err := encodeRecords(recs)
	if err != nil {
		t.Fatal(err)
	}
	name := segName(1, 2)
	sealed, err := seal(key, "m1", name, blob)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := open(key, "m1", name, sealed)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeRecords(plain)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Cmd != "echo hi" || *got[0].Exit != 1 || got[1].Exit != nil {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}

	// Tampering must fail.
	bad := append([]byte(nil), sealed...)
	bad[len(bad)-1] ^= 1
	if _, err := open(key, "m1", name, bad); err == nil {
		t.Fatal("tampered segment decrypted")
	}
	// Renaming (different AAD) must fail.
	if _, err := open(key, "m1", segName(3, 4), sealed); err == nil {
		t.Fatal("renamed segment decrypted")
	}
	// Foreign machine dir (different AAD) must fail.
	if _, err := open(key, "m2", name, sealed); err == nil {
		t.Fatal("segment moved between machine logs decrypted")
	}
	// Wrong key must fail.
	if _, err := open(bytes.Repeat([]byte{8}, 32), "m1", name, sealed); err == nil {
		t.Fatal("wrong key decrypted")
	}
}

// machine sets up an isolated fake machine (own config dir + DB) and
// returns its opened store. Call restore() before switching machines.
func machine(t *testing.T, root, name string) (st *store.Store, use func()) {
	t.Helper()
	home := filepath.Join(root, name)
	use = func() {
		os.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
		os.Setenv("BACKSCROLL_DB", filepath.Join(home, "backscroll.db"))
		os.Setenv("BACKSCROLL_IGNORE_FILE", filepath.Join(home, "config", "backscroll", "ignore"))
		os.Setenv("BACKSCROLL_REDACT_FILE", filepath.Join(home, "config", "backscroll", "redact"))
	}
	use()
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, use
}

func addCmd(t *testing.T, st *store.Store, sess int64, cmd, out string) {
	t.Helper()
	now := time.Now()
	if err := st.AddCommand(sess, cmd, "/tmp", 0, true, now, now.Add(time.Second),
		[]byte(out), false, out); err != nil {
		t.Fatal(err)
	}
}

func TestExportImportE2E(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared")

	stA, useA := machine(t, root, "a")
	sessA, _ := stA.NewSession("bash", "xterm")
	addCmd(t, stA, sessA, "echo one", "one\n")
	addCmd(t, stA, sessA, "curl -H 'Authorization: Bearer supersecret123' api", "ok\n")

	cfgA, keyA, err := Init(shared)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Export(stA, cfgA, keyA)
	if err != nil {
		t.Fatal(err)
	}
	if res.Exported != 2 || res.Segments != 1 {
		t.Fatalf("export: %+v", res)
	}
	// Second export: nothing new.
	res, err = Export(stA, cfgA, keyA)
	if err != nil || res.Exported != 0 {
		t.Fatalf("re-export: %+v err=%v", res, err)
	}

	// Machine B: same key file, same shared dir.
	stB, useB := machine(t, root, "b")
	if err := os.MkdirAll(filepath.Dir(KeyPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	useA()
	keyBytes, err := os.ReadFile(KeyPath())
	if err != nil {
		t.Fatal(err)
	}
	useB()
	if err := os.WriteFile(KeyPath(), keyBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	cfgB, keyB, err := Init(shared)
	if err != nil {
		t.Fatal(err)
	}
	if cfgB.Machine == cfgA.Machine {
		t.Fatal("machines share an id")
	}

	machines, err := Import(stB, cfgB, keyB)
	if err != nil {
		t.Fatal(err)
	}
	if len(machines) != 1 || machines[0].Imported != 2 {
		t.Fatalf("import: %+v", machines)
	}

	// Foreign entries visible, tagged, searchable; redaction happened
	// before export.
	hostA, _ := os.Hostname()
	got, err := stB.List(store.Filter{Host: hostA})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("list --host: %+v", got)
	}
	for _, c := range got {
		if strings.Contains(c.Cmd, "supersecret123") {
			t.Fatalf("secret left the machine: %q", c.Cmd)
		}
	}
	found, err := stB.Search(`"echo one"`, store.Filter{})
	if err != nil || len(found) != 1 {
		t.Fatalf("search imported: %v %+v", err, found)
	}
	c, err := stB.Get(found[0].ID)
	if err != nil || string(c.Output) != "one\n" {
		t.Fatalf("imported output: %v %q", err, c.Output)
	}

	// Re-import: no-op.
	machines, err = Import(stB, cfgB, keyB)
	if err != nil || machines[0].Imported != 0 {
		t.Fatalf("re-import: %+v err=%v", machines, err)
	}

	// Incremental: A records more (one ignored), B sees only the new one.
	useA()
	os.MkdirAll(filepath.Dir(os.Getenv("BACKSCROLL_IGNORE_FILE")), 0o700)
	os.WriteFile(os.Getenv("BACKSCROLL_IGNORE_FILE"), []byte("^topsecret\n"), 0o600)
	addCmd(t, stA, sessA, "echo two", "two\n")
	addCmd(t, stA, sessA, "topsecret --launch", "boom\n")
	res, err = Export(stA, cfgA, keyA)
	if err != nil || res.Exported != 1 || res.Skipped != 1 {
		t.Fatalf("incremental export: %+v err=%v", res, err)
	}
	useB()
	machines, err = Import(stB, cfgB, keyB)
	if err != nil || machines[0].Imported != 1 {
		t.Fatalf("incremental import: %+v err=%v", machines, err)
	}
	if found, _ := stB.Search(`"topsecret"`, store.Filter{}); len(found) != 0 {
		t.Fatalf("ignored command was synced: %+v", found)
	}

	// B exports too; A must not import its own log back.
	sessB, _ := stB.NewSession("zsh", "xterm")
	addCmd(t, stB, sessB, "echo from-b", "from-b\n")
	if _, err := Export(stB, cfgB, keyB); err != nil {
		t.Fatal(err)
	}
	useA()
	machines, err = Import(stA, cfgA, keyA)
	if err != nil || len(machines) != 1 || machines[0].Imported != 1 {
		t.Fatalf("import into A: %+v err=%v", machines, err)
	}
	// A's own entries stay local (machine IS NULL).
	local, err := stA.List(store.Filter{Host: "local", Limit: 10})
	if err != nil || len(local) != 4 {
		t.Fatalf("A local entries: %d err=%v", len(local), err)
	}

	// Status sees both machines.
	sts, err := Status(stA, cfgA)
	if err != nil || len(sts) != 2 {
		t.Fatalf("status: %+v err=%v", sts, err)
	}
	if !sts[0].Local {
		t.Fatalf("status[0] should be local: %+v", sts)
	}
}

func TestInitIdempotent(t *testing.T) {
	root := t.TempDir()
	_, use := machine(t, root, "solo")
	use()
	shared := filepath.Join(root, "shared")
	cfg1, key1, err := Init(shared)
	if err != nil {
		t.Fatal(err)
	}
	cfg2, key2, err := Init(shared)
	if err != nil {
		t.Fatal(err)
	}
	if cfg1.Machine != cfg2.Machine || !bytes.Equal(key1, key2) {
		t.Fatal("re-init changed identity")
	}
}
