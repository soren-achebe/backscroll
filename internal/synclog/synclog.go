// Package synclog implements cross-machine sync as per-machine
// append-only encrypted logs in a user-synced directory. See
// docs/sync-design.md for the design.
//
// Layout: <dir>/<machine-id>/meta.json + NNNNNNNNNNNN-NNNNNNNNNNNN.seg
// files. Each segment holds a run of records (one per history entry),
// JSONL-encoded, zstd-compressed, sealed with XChaCha20-Poly1305 using
// a shared 32-byte key file the user copies between machines.
//
// A machine only ever appends to its own log, which makes the scheme
// conflict-free: syncing twice, partially, or out of order can never
// corrupt anything. The per-machine sequence number is the local
// command id, so re-exporting after a crash rewrites identical records
// and importers (INSERT OR IGNORE on (machine, seq)) are unaffected.
package synclog

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/crypto/chacha20poly1305"

	"github.com/soren-achebe/backscroll/internal/record"
	"github.com/soren-achebe/backscroll/internal/redact"
	"github.com/soren-achebe/backscroll/internal/store"
)

const (
	magic          = "bkseg1\n"
	segCap         = 4 << 20 // plaintext bytes per segment before sealing
	exportBatch    = 500     // rows fetched from the DB at a time
	stateKey       = "export_local_id"
	keyFileComment = "# backscroll sync key — copy this file to ~/.config/backscroll/sync.key on each machine.\n# Anyone with this key can read your synced history. Keep it out of the sync dir!\n"
)

// hostname returns the name other machines will see for this one.
// BACKSCROLL_HOST overrides os.Hostname for containers or machines
// with generic hostnames ("localhost").
func hostname() string {
	if h := os.Getenv("BACKSCROLL_HOST"); h != "" {
		return h
	}
	h, _ := os.Hostname()
	return h
}

// ConfigDir returns the backscroll config directory.
func ConfigDir() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "backscroll")
}

func configPath() string { return filepath.Join(ConfigDir(), "sync.json") }

// KeyPath returns the sync key file path.
func KeyPath() string { return filepath.Join(ConfigDir(), "sync.key") }

// Config is the local sync configuration (~/.config/backscroll/sync.json).
type Config struct {
	Dir     string `json:"dir"`     // the shared sync directory
	Machine string `json:"machine"` // this machine's random id (32 hex chars)
}

// LoadConfig reads sync.json. Returns os.ErrNotExist if sync was never
// initialized on this machine.
func LoadConfig() (*Config, error) {
	b, err := os.ReadFile(configPath())
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", configPath(), err)
	}
	if c.Dir == "" || c.Machine == "" {
		return nil, fmt.Errorf("%s: missing dir or machine", configPath())
	}
	return &c, nil
}

func saveConfig(c *Config) error {
	if err := os.MkdirAll(ConfigDir(), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return atomicWrite(configPath(), append(b, '\n'), 0o600)
}

// LoadKey reads the 32-byte sync key. If create is true and no key file
// exists, a fresh random key is minted and written (0600).
func LoadKey(create bool) ([]byte, error) {
	b, err := os.ReadFile(KeyPath())
	if errors.Is(err, os.ErrNotExist) && create {
		key := make([]byte, chacha20poly1305.KeySize)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(ConfigDir(), 0o700); err != nil {
			return nil, err
		}
		data := keyFileComment + hex.EncodeToString(key) + "\n"
		if err := atomicWrite(KeyPath(), []byte(data), 0o600); err != nil {
			return nil, err
		}
		return key, nil
	}
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, err := hex.DecodeString(line)
		if err != nil || len(key) != chacha20poly1305.KeySize {
			return nil, fmt.Errorf("%s: want 64 hex chars, got %q", KeyPath(), line)
		}
		return key, nil
	}
	return nil, fmt.Errorf("%s: no key found", KeyPath())
}

// KeyFingerprint returns a short human-checkable digest of the key, so
// users can confirm two machines hold the same key.
func KeyFingerprint(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:4])
}

// Init sets up sync on this machine: mints a machine id, creates the
// sync dir + our log dir, and creates the key file if absent. Calling
// it again with the same dir is a no-op; with a different dir it
// repoints the config (the machine id is kept).
func Init(dir string) (*Config, []byte, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := LoadConfig()
	if errors.Is(err, os.ErrNotExist) {
		id := make([]byte, 16)
		if _, err := rand.Read(id); err != nil {
			return nil, nil, err
		}
		cfg = &Config{Machine: hex.EncodeToString(id)}
	} else if err != nil {
		return nil, nil, err
	}
	cfg.Dir = abs
	key, err := LoadKey(true)
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(filepath.Join(abs, cfg.Machine), 0o700); err != nil {
		return nil, nil, err
	}
	if err := saveConfig(cfg); err != nil {
		return nil, nil, err
	}
	host := hostname()
	if err := writeMeta(abs, cfg.Machine, host, 0, true); err != nil {
		return nil, nil, err
	}
	return cfg, key, nil
}

// Meta is the per-machine metadata file (plaintext; holds no history).
type Meta struct {
	Host    string `json:"host"`
	Created int64  `json:"created,omitempty"` // unix ms
	LastSeq int64  `json:"last_seq"`
}

func metaPath(dir, machine string) string { return filepath.Join(dir, machine, "meta.json") }

func readMeta(dir, machine string) (Meta, error) {
	var m Meta
	b, err := os.ReadFile(metaPath(dir, machine))
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(b, &m)
	return m, err
}

// writeMeta updates meta.json; keepSeq preserves an existing last_seq
// (used by Init so re-init doesn't clobber progress).
func writeMeta(dir, machine, host string, lastSeq int64, keepSeq bool) error {
	m, err := readMeta(dir, machine)
	if err != nil {
		m = Meta{Created: time.Now().UnixMilli()}
	}
	m.Host = host
	if !keepSeq || lastSeq > m.LastSeq {
		m.LastSeq = lastSeq
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return atomicWrite(metaPath(dir, machine), append(b, '\n'), 0o600)
}

// Record is one history entry as it appears in a segment.
type Record struct {
	Seq   int64  `json:"seq"`
	Host  string `json:"host"`
	Cmd   string `json:"cmd"`
	Cwd   string `json:"cwd,omitempty"`
	Exit  *int64 `json:"exit,omitempty"`
	Start int64  `json:"start"` // unix ms
	End   int64  `json:"end"`   // unix ms
	Out   string `json:"out,omitempty"`
}

func segName(first, last int64) string { return fmt.Sprintf("%012d-%012d.seg", first, last) }

var segRe = regexp.MustCompile(`^(\d{12})-(\d{12})\.seg$`)

// seal encrypts a plaintext segment. The machine id and filename are
// bound in as AAD so segments can't be renamed or moved between logs.
func seal(key []byte, machine, name string, plain []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(magic)+len(nonce)+len(plain)+aead.Overhead())
	out = append(out, magic...)
	out = append(out, nonce...)
	return aead.Seal(out, nonce, plain, []byte(machine+"/"+name)), nil
}

func open(key []byte, machine, name string, sealed []byte) ([]byte, error) {
	if len(sealed) < len(magic)+chacha20poly1305.NonceSizeX || string(sealed[:len(magic)]) != magic {
		return nil, fmt.Errorf("%s: not a backscroll segment", name)
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	nonce := sealed[len(magic) : len(magic)+aead.NonceSize()]
	plain, err := aead.Open(nil, nonce, sealed[len(magic)+aead.NonceSize():], []byte(machine+"/"+name))
	if err != nil {
		return nil, fmt.Errorf("%s: decrypt failed (wrong sync key? run `backscroll sync status` and compare key fingerprints)", name)
	}
	return plain, nil
}

func encodeRecords(recs []Record) ([]byte, error) {
	var b strings.Builder
	for _, r := range recs {
		line, err := json.Marshal(r)
		if err != nil {
			return nil, err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	defer enc.Close()
	return enc.EncodeAll([]byte(b.String()), nil), nil
}

func decodeRecords(compressed []byte) ([]Record, error) {
	dec, _ := zstd.NewReader(nil)
	defer dec.Close()
	plain, err := dec.DecodeAll(compressed, nil)
	if err != nil {
		return nil, err
	}
	var recs []Record
	for _, line := range strings.Split(string(plain), "\n") {
		if line == "" {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, err
		}
		recs = append(recs, r)
	}
	return recs, nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ExportResult reports what Export did.
type ExportResult struct {
	Exported int // records written
	Skipped  int // matched an ignore pattern
	Segments int
}

// Export appends local history recorded since the last export to our
// log. Redact patterns (built-in + user extras) are applied to every
// command line and output before anything leaves the machine; commands
// matching ignore patterns are skipped entirely. The per-machine seq is
// the local command id, so a partial/crashed export re-runs cleanly.
func Export(st *store.Store, cfg *Config, key []byte) (ExportResult, error) {
	var res ExportResult
	host := hostname()
	extras := redact.LoadExtra()
	ignores := record.LoadIgnore()
	logDir := filepath.Join(cfg.Dir, cfg.Machine)
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return res, err
	}
	hwm, err := st.SyncState(stateKey)
	if err != nil {
		return res, err
	}
	var pending []Record
	var pendingBytes int
	lastSeq := hwm
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		name := segName(pending[0].Seq, pending[len(pending)-1].Seq)
		blob, err := encodeRecords(pending)
		if err != nil {
			return err
		}
		sealed, err := seal(key, cfg.Machine, name, blob)
		if err != nil {
			return err
		}
		if err := atomicWrite(filepath.Join(logDir, name), sealed, 0o600); err != nil {
			return err
		}
		res.Exported += len(pending)
		res.Segments++
		pending = pending[:0]
		pendingBytes = 0
		return nil
	}
	for {
		entries, err := st.LocalAfter(lastSeq, exportBatch)
		if err != nil {
			return res, err
		}
		if len(entries) == 0 {
			break
		}
		for _, e := range entries {
			lastSeq = e.ID
			if record.Ignored(ignores, e.Cmd) {
				res.Skipped++
				continue
			}
			cmd, _ := redact.String(e.Cmd, extras)
			out, _ := redact.String(e.Plain, extras)
			r := Record{
				Seq: e.ID, Host: host, Cmd: cmd, Cwd: e.Cwd,
				Start: e.StartedAt.UnixMilli(), End: e.EndedAt.UnixMilli(),
				Out: out,
			}
			if e.ExitCode.Valid {
				v := e.ExitCode.Int64
				r.Exit = &v
			}
			pending = append(pending, r)
			pendingBytes += len(r.Cmd) + len(r.Out) + 64
			if pendingBytes >= segCap {
				if err := flush(); err != nil {
					return res, err
				}
			}
		}
	}
	if err := flush(); err != nil {
		return res, err
	}
	if lastSeq > hwm {
		if err := st.SetSyncState(stateKey, lastSeq); err != nil {
			return res, err
		}
		if err := writeMeta(cfg.Dir, cfg.Machine, host, lastSeq, true); err != nil {
			return res, err
		}
	}
	return res, nil
}

// MachineImport reports what Import pulled from one foreign machine.
type MachineImport struct {
	Machine  string
	Host     string
	Imported int
	LastSeq  int64
}

// Import scans the sync dir for foreign machines' logs and inserts any
// records past our per-machine high-water mark. Re-importing is a
// no-op. Each segment commits its high-water mark, so a partial import
// resumes where it stopped.
func Import(st *store.Store, cfg *Config, key []byte) ([]MachineImport, error) {
	entries, err := os.ReadDir(cfg.Dir)
	if err != nil {
		return nil, err
	}
	var out []MachineImport
	for _, ent := range entries {
		machine := ent.Name()
		if !ent.IsDir() || machine == cfg.Machine {
			continue
		}
		meta, err := readMeta(cfg.Dir, machine)
		if err != nil {
			continue // not a machine log dir
		}
		mi := MachineImport{Machine: machine, Host: meta.Host}
		hwm, err := st.ImportedSeq(machine)
		if err != nil {
			return out, err
		}
		mi.LastSeq = hwm
		segs, err := listSegments(filepath.Join(cfg.Dir, machine))
		if err != nil {
			return out, err
		}
		for _, sg := range segs {
			if sg.last <= hwm {
				continue
			}
			sealed, err := os.ReadFile(filepath.Join(cfg.Dir, machine, sg.name))
			if err != nil {
				return out, err
			}
			blob, err := open(key, machine, sg.name, sealed)
			if err != nil {
				return out, fmt.Errorf("%s: %w", machine, err)
			}
			recs, err := decodeRecords(blob)
			if err != nil {
				return out, err
			}
			for _, r := range recs {
				if r.Seq <= hwm {
					continue
				}
				rhost := r.Host
				if rhost == "" {
					rhost = meta.Host
				}
				added, err := st.AddImported(machine, rhost, r.Seq, r.Cmd, r.Cwd,
					int(val(r.Exit)), r.Exit != nil,
					time.UnixMilli(r.Start), time.UnixMilli(r.End), r.Out)
				if err != nil {
					return out, err
				}
				if added {
					mi.Imported++
				}
				if r.Seq > mi.LastSeq {
					mi.LastSeq = r.Seq
				}
			}
			hwm = sg.last
			if hwm > mi.LastSeq {
				mi.LastSeq = hwm
			}
			if err := st.SetImportedSeq(machine, meta.Host, mi.LastSeq); err != nil {
				return out, err
			}
		}
		out = append(out, mi)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out, nil
}

func val(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

type segment struct {
	name        string
	first, last int64
}

func listSegments(dir string) ([]segment, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var segs []segment
	for _, e := range ents {
		m := segRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		var s segment
		s.name = e.Name()
		fmt.Sscanf(m[1], "%d", &s.first)
		fmt.Sscanf(m[2], "%d", &s.last)
		segs = append(segs, s)
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].first < segs[j].first })
	return segs, nil
}

// MachineStatus is one machine's row in `sync status`.
type MachineStatus struct {
	Machine   string
	Host      string
	Local     bool
	LastSeq   int64 // newest seq available in the log dir (from meta.json)
	Imported  int64 // our high-water mark (foreign machines)
	Segments  int
	DiskBytes int64
}

// Status summarizes the sync dir and our import/export progress.
func Status(st *store.Store, cfg *Config) ([]MachineStatus, error) {
	ents, err := os.ReadDir(cfg.Dir)
	if err != nil {
		return nil, err
	}
	imported := map[string]int64{}
	states, err := st.ImportStates()
	if err != nil {
		return nil, err
	}
	for _, s := range states {
		imported[s.Machine] = s.LastSeq
	}
	var out []MachineStatus
	for _, ent := range ents {
		machine := ent.Name()
		if !ent.IsDir() {
			continue
		}
		meta, err := readMeta(cfg.Dir, machine)
		if err != nil {
			continue
		}
		ms := MachineStatus{
			Machine: machine, Host: meta.Host,
			Local: machine == cfg.Machine, LastSeq: meta.LastSeq,
			Imported: imported[machine],
		}
		segs, _ := listSegments(filepath.Join(cfg.Dir, machine))
		ms.Segments = len(segs)
		for _, sg := range segs {
			if fi, err := os.Stat(filepath.Join(cfg.Dir, machine, sg.name)); err == nil {
				ms.DiskBytes += fi.Size()
			}
		}
		out = append(out, ms)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Local != out[j].Local {
			return out[i].Local
		}
		return out[i].Host < out[j].Host
	})
	return out, nil
}
