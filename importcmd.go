package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/soren-achebe/backscroll/internal/histimport"
	"github.com/soren-achebe/backscroll/internal/store"
)

// cmdImport seeds the DB from an existing shell-history store so
// list/search/pick/stats are useful before a single recorded session.
// Not to be confused with `backscroll sync import`, which imports
// encrypted sync segments from your other machines.
func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "parse and report, write nothing")
	hostFlag := fs.String("host", "", "label entries with this origin host (file sources)")
	nCap := fs.Int("n", 0, "import at most the newest N entries (0 = all)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: backscroll import <atuin|zsh|bash|fish> [path] [flags]

Seed backscroll from history you already have. Sources:
  atuin   ~/.local/share/atuin/history.db   (times, exits, cwd, host)
  zsh     $HISTFILE / ~/.zsh_history        (times+durations with EXTENDED_HISTORY)
  bash    $HISTFILE / ~/.bash_history       (times if HISTTIMEFORMAT was set)
  fish    ~/.local/share/fish/fish_history  (times)

Imported entries have no recorded output; re-running an import only adds
new entries. Not the same as "backscroll sync import" (machine sync).

Flags:
`)
		fs.PrintDefaults()
	}
	flags, pos := splitFlags(fs, args)
	fs.Parse(flags)
	if len(pos) < 1 {
		fs.Usage()
		return fmt.Errorf("missing source")
	}
	src := pos[0]
	path := ""
	if len(pos) > 1 {
		path = pos[1]
	}

	var entries []histimport.Entry
	var err error
	switch src {
	case "atuin":
		if path == "" {
			path = firstExisting(
				filepath.Join(xdgData(), "atuin", "history.db"),
			)
		}
		if path == "" {
			return fmt.Errorf("no atuin history.db found — pass the path explicitly")
		}
		entries, err = histimport.ReadAtuin(path)
		if err != nil {
			return err
		}
	case "zsh", "bash", "fish":
		if path == "" {
			path = defaultHistFile(src)
		}
		if path == "" {
			return fmt.Errorf("no %s history file found — pass the path explicitly", src)
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		switch src {
		case "zsh":
			entries = histimport.ParseZsh(data)
		case "bash":
			entries = histimport.ParseBash(data)
		case "fish":
			entries = histimport.ParseFish(data)
		}
	default:
		fs.Usage()
		return fmt.Errorf("unknown source %q", src)
	}

	if *nCap > 0 && len(entries) > *nCap {
		entries = entries[len(entries)-*nCap:]
	}
	if *hostFlag != "" {
		for i := range entries {
			entries[i].Host = *hostFlag
		}
	} else if src == "atuin" {
		// entries recorded on this machine shouldn't wear a host tag
		if hn, _ := os.Hostname(); hn != "" {
			for i := range entries {
				if entries[i].Host == hn {
					entries[i].Host = ""
				}
			}
		}
	}

	noTime := 0
	for _, e := range entries {
		if e.Started.IsZero() {
			noTime++
		}
	}
	if *dry {
		fmt.Printf("would import %d entries from %s (%s)\n", len(entries), src, path)
		if noTime > 0 {
			fmt.Printf("%d entries have no timestamps (their age will show as unknown)\n", noTime)
		}
		return nil
	}

	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	he := make([]store.HistEntry, len(entries))
	for i, e := range entries {
		he[i] = store.HistEntry{
			Seq: e.Seq, Cmd: e.Cmd, Cwd: e.Cwd, Host: e.Host,
			Exit: e.Exit, HasExit: e.HasExit, Started: e.Started, Ended: e.Ended,
		}
	}
	added, err := st.AddHistory("hist:"+src, he)
	if err != nil {
		return err
	}
	skipped := len(entries) - added
	fmt.Printf("imported %d entries from %s (%s)", added, src, path)
	if skipped > 0 {
		fmt.Printf(", %d already present or duplicate", skipped)
	}
	fmt.Println()
	if noTime > 0 {
		fmt.Printf("note: %d entries had no timestamps — their age shows as unknown\n", noTime)
	}
	return nil
}

func firstExisting(paths ...string) string {
	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func xdgData() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share")
}

func defaultHistFile(shell string) string {
	home, _ := os.UserHomeDir()
	switch shell {
	case "zsh":
		return firstExisting(os.Getenv("HISTFILE"),
			filepath.Join(home, ".zsh_history"), filepath.Join(home, ".histfile"))
	case "bash":
		return firstExisting(os.Getenv("HISTFILE"), filepath.Join(home, ".bash_history"))
	case "fish":
		return firstExisting(filepath.Join(xdgData(), "fish", "fish_history"))
	}
	return ""
}
