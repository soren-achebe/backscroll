package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
		fmt.Fprintf(os.Stderr, `usage: backscroll import <atuin|zsh|bash|fish|nu|pwsh> [path] [flags]

Seed backscroll from history you already have. Sources:
  atuin   ~/.local/share/atuin/history.db   (times, exits, cwd, host)
  zsh     $HISTFILE / ~/.zsh_history        (times+durations with EXTENDED_HISTORY)
  bash    $HISTFILE / ~/.bash_history       (times if HISTTIMEFORMAT was set)
  fish    ~/.local/share/fish/fish_history  (times)
  nu      ~/.config/nushell/history.sqlite3 (times, exits, cwd, host)
          or history.txt, whichever the history.file_format setting wrote
  pwsh    PSReadLine ConsoleHost_history.txt

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
	case "nu":
		if path == "" {
			path = defaultNuHistory()
		}
		if path == "" {
			return fmt.Errorf("no nushell history found — pass the path explicitly")
		}
		if isSQLite(path) {
			entries, err = histimport.ReadNuSqlite(path)
			if err != nil {
				return err
			}
		} else {
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			entries = histimport.ParseNuPlain(data)
		}
	case "zsh", "bash", "fish", "pwsh":
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
		case "pwsh":
			entries = histimport.ParsePwsh(data)
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
	} else if src == "atuin" || src == "nu" {
		// entries recorded on this machine shouldn't wear a host tag
		// (nu stores the FQDN; compare short names on both sides)
		if hn, _ := os.Hostname(); hn != "" {
			if i := strings.IndexByte(hn, '.'); i > 0 {
				hn = hn[:i]
			}
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

// histSource is an importable shell-history store found on this machine.
type histSource struct {
	Name    string // import subcommand: atuin, zsh, bash, fish
	Path    string
	Entries int // parsed entry count; -1 if unreadable
}

// detectHistSources probes the default locations `backscroll import`
// would use and counts what each parser can actually extract, so doctor
// can suggest seeding an empty DB. Order = data richness: atuin carries
// exits/cwd/durations, the plain files only text (+timestamps if lucky).
func detectHistSources() []histSource {
	var out []histSource
	if p := firstExisting(filepath.Join(xdgData(), "atuin", "history.db")); p != "" {
		n := -1
		if entries, err := histimport.ReadAtuin(p); err == nil {
			n = len(entries)
		}
		if n != 0 { // unreadable (-1) is still worth surfacing
			out = append(out, histSource{"atuin", p, n})
		}
	}
	if p := firstExisting(nuHistCandidates()...); p != "" {
		n := -1
		if isSQLite(p) {
			if entries, err := histimport.ReadNuSqlite(p); err == nil {
				n = len(entries)
			}
		} else if data, err := os.ReadFile(p); err == nil {
			n = len(histimport.ParseNuPlain(data))
		}
		if n != 0 {
			out = append(out, histSource{"nu", p, n})
		}
	}
	home, _ := os.UserHomeDir()
	canonical := map[string][]string{
		// deliberately NOT $HISTFILE: an exported HISTFILE from the
		// invoking shell would mis-attribute its file to every shell here
		"zsh":  {filepath.Join(home, ".zsh_history"), filepath.Join(home, ".histfile")},
		"bash": {filepath.Join(home, ".bash_history")},
		"fish": {filepath.Join(xdgData(), "fish", "fish_history")},
		"pwsh": pwshHistCandidates(),
	}
	for _, sh := range []string{"zsh", "bash", "fish", "pwsh"} {
		p := firstExisting(canonical[sh]...)
		if p == "" {
			continue
		}
		n := -1
		if data, err := os.ReadFile(p); err == nil {
			switch sh {
			case "zsh":
				n = len(histimport.ParseZsh(data))
			case "bash":
				n = len(histimport.ParseBash(data))
			case "fish":
				n = len(histimport.ParseFish(data))
			case "pwsh":
				n = len(histimport.ParsePwsh(data))
			}
		}
		if n == 0 {
			continue // present but empty — nothing to suggest
		}
		out = append(out, histSource{sh, p, n})
	}
	return out
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
	case "pwsh":
		return firstExisting(pwshHistCandidates()...)
	}
	return ""
}

// pwshHistCandidates returns PSReadLine's default HistorySavePath
// locations: $XDG_DATA_HOME-aware ~/.local/share/powershell on
// unix (verified against pwsh 7.5), %APPDATA% on Windows.
func pwshHistCandidates() []string {
	var out []string
	out = append(out,
		filepath.Join(xdgData(), "powershell", "PSReadLine", "ConsoleHost_history.txt"))
	if appdata := os.Getenv("APPDATA"); appdata != "" {
		out = append(out,
			filepath.Join(appdata, "Microsoft", "Windows", "PowerShell", "PSReadLine", "ConsoleHost_history.txt"))
	}
	return out
}

// nuHistCandidates returns nushell history locations, richest first:
// the SQLite backend, then plaintext, across the config dirs nu uses
// ($XDG_CONFIG_HOME/nushell or ~/.config/nushell on unix — nu honors
// XDG on every OS — plus %APPDATA%\nushell and the macOS
// Application Support dir for stock setups).
func nuHistCandidates() []string {
	home, _ := os.UserHomeDir()
	var dirs []string
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		dirs = append(dirs, filepath.Join(d, "nushell"))
	}
	dirs = append(dirs, filepath.Join(home, ".config", "nushell"))
	if appdata := os.Getenv("APPDATA"); appdata != "" {
		dirs = append(dirs, filepath.Join(appdata, "nushell"))
	}
	dirs = append(dirs, filepath.Join(home, "Library", "Application Support", "nushell"))
	var out []string
	for _, d := range dirs {
		out = append(out, filepath.Join(d, "history.sqlite3"))
	}
	for _, d := range dirs {
		out = append(out, filepath.Join(d, "history.txt"))
	}
	return out
}

func defaultNuHistory() string {
	return firstExisting(nuHistCandidates()...)
}

// isSQLite sniffs the 16-byte SQLite magic so an explicit `import nu
// <path>` picks the right parser regardless of the file's name.
func isSQLite(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 16)
	n, _ := f.Read(buf)
	return string(buf[:n]) == "SQLite format 3\x00"
}
