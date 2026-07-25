package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/soren-achebe/backscroll/internal/synclog"
)

const syncUsage = `backscroll sync — cross-machine history sync (encrypted, serverless)

Point every machine at one shared directory (Syncthing, Dropbox, rsync…);
each machine appends its own encrypted log and imports the others.

  backscroll sync init <dir>   set up sync into <dir>; mints this machine's
                               id and a key file if you don't have one yet
  backscroll sync export       write new local history to our log
  backscroll sync import       pull in other machines' new history
  backscroll sync status       per-machine progress, sizes, key fingerprint

Setup on each additional machine:
  1. copy ~/.config/backscroll/sync.key from the first machine (same path)
  2. backscroll sync init <same shared dir>
Then use --host <name>|local with list/search/pick to filter, e.g.:
  backscroll search "connection refused" --host laptop

Docs: docs/sync-design.md in the repo.
`

func cmdSync(args []string) error {
	if len(args) == 0 {
		fmt.Print(syncUsage)
		return nil
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "init":
		return syncInit(rest)
	case "export":
		return syncExport()
	case "import":
		return syncImport()
	case "status":
		return syncStatus()
	case "help", "--help", "-h":
		fmt.Print(syncUsage)
		return nil
	default:
		return fmt.Errorf("unknown sync command %q\n\n%s", sub, syncUsage)
	}
}

func syncInit(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: backscroll sync init <shared-dir>")
	}
	cfg, key, err := synclog.Init(args[0])
	if err != nil {
		return err
	}
	fmt.Printf("sync initialized\n")
	fmt.Printf("  dir         %s\n", cfg.Dir)
	fmt.Printf("  machine id  %s\n", cfg.Machine)
	fmt.Printf("  key         %s (fingerprint %s)\n", synclog.KeyPath(), synclog.KeyFingerprint(key))
	fmt.Printf("\nOn each other machine: copy the key file to the same path, then run\n")
	fmt.Printf("`backscroll sync init <same dir>` there. `sync export` + `sync import`\n")
	fmt.Printf("move history; run `sync status` to check progress.\n")
	return nil
}

func syncLoad() (*synclog.Config, []byte, error) {
	cfg, err := synclog.LoadConfig()
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("sync not set up — run: backscroll sync init <shared-dir>")
	}
	if err != nil {
		return nil, nil, err
	}
	key, err := synclog.LoadKey(false)
	if err != nil {
		return nil, nil, err
	}
	return cfg, key, nil
}

func syncExport() error {
	cfg, key, err := syncLoad()
	if err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	res, err := synclog.Export(st, cfg, key)
	if err != nil {
		return err
	}
	switch {
	case res.Exported == 0 && res.Skipped == 0:
		fmt.Println("nothing new to export")
	default:
		fmt.Printf("exported %d entr%s in %d segment%s", res.Exported,
			plural(res.Exported, "y", "ies"), res.Segments, plural(res.Segments, "", "s"))
		if res.Skipped > 0 {
			fmt.Printf(" (%d skipped by ignore patterns)", res.Skipped)
		}
		fmt.Println()
	}
	return nil
}

func syncImport() error {
	cfg, key, err := syncLoad()
	if err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	machines, err := synclog.Import(st, cfg, key)
	if err != nil {
		return err
	}
	if len(machines) == 0 {
		fmt.Println("no other machines found in the sync dir yet")
		return nil
	}
	total := 0
	for _, m := range machines {
		fmt.Printf("  %-16s %6d new (through seq %d)\n", m.Host, m.Imported, m.LastSeq)
		total += m.Imported
	}
	if total == 0 {
		fmt.Println("everything already up to date")
	} else {
		fmt.Printf("imported %d entr%s — try: backscroll list --host <name>\n",
			total, plural(total, "y", "ies"))
	}
	return nil
}

func syncStatus() error {
	cfg, key, err := syncLoad()
	if err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	machines, err := synclog.Status(st, cfg)
	if err != nil {
		return err
	}
	fmt.Printf("sync dir  %s\n", cfg.Dir)
	fmt.Printf("key       %s (fingerprint %s — must match on every machine)\n",
		synclog.KeyPath(), synclog.KeyFingerprint(key))
	exported, err := st.SyncState("export_local_id")
	if err != nil {
		return err
	}
	pending, err := st.CountLocalAfter(exported)
	if err != nil {
		return err
	}
	fmt.Printf("\n  %-16s %-10s %9s %9s %9s\n", "HOST", "", "IN LOG", "IMPORTED", "SIZE")
	for _, m := range machines {
		who := ""
		imported := fmt.Sprintf("%d", m.Imported)
		if m.Local {
			who = "(this one)"
			imported = "—"
		}
		fmt.Printf("  %-16s %-10s %9d %9s %9s\n",
			m.Host, who, m.LastSeq, imported, humanBytes(m.DiskBytes))
	}
	if pending > 0 {
		fmt.Printf("\n%d local entr%s not exported yet — run: backscroll sync export\n",
			pending, plural(int(pending), "y", "ies"))
	}
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
