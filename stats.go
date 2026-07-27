package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/soren-achebe/backscroll/internal/store"

	"golang.org/x/term"
)

// cmdStats prints either the overview (no flags) or a breakdown of the
// recorded history along one dimension (--by cmd|cwd|exit|host|day),
// scoped by the shared list/search filters.
func cmdStats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	by := fs.String("by", "", "breakdown `dimension`: cmd, cwd, exit, host, day")
	n := fs.Int("n", 15, "max rows in a breakdown")
	mkFilter := filterFlags(fs)
	fs.Parse(args)
	if fs.NArg() > 0 {
		return fmt.Errorf("stats takes no positional arguments (did you mean --by %s?)", fs.Arg(0))
	}
	f, err := mkFilter(0)
	if err != nil {
		return err
	}
	if *by == "" {
		if f.Active() {
			return fmt.Errorf("filters need a breakdown: add --by cmd|cwd|exit|host|day")
		}
		return statsOverview()
	}
	dim := strings.TrimPrefix(*by, "--")
	switch dim {
	case "cmd", "command", "cwd", "dir", "exit", "host", "day", "date":
	default:
		return fmt.Errorf("--by: want cmd, cwd, exit, host or day, got %q", *by)
	}
	return statsBreakdown(dim, *n, f)
}

func statsOverview() error {
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	s, err := st.Stats()
	if err != nil {
		return err
	}
	if s.Imported > 0 {
		fmt.Printf("commands recorded : %d (%d synced from other machines)\n", s.Commands, s.Imported)
	} else {
		fmt.Printf("commands recorded : %d\n", s.Commands)
	}
	fmt.Printf("sessions          : %d\n", s.Sessions)
	fmt.Printf("raw output stored : %s\n", humanBytes(s.RawBytes))
	fmt.Printf("database size     : %s\n", humanBytes(s.DBBytes))
	if !s.FirstAt.IsZero() {
		fmt.Printf("oldest entry      : %s\n", s.FirstAt.Format("2006-01-02 15:04"))
	}
	fmt.Printf("database path     : %s\n", store.DefaultPath())
	fmt.Printf("\nbreakdowns        : backscroll stats --by cmd|cwd|exit|host|day\n")
	return nil
}

type statGroup struct {
	key   string
	count int
	fails int
	dur   time.Duration
	spark []int // per-time-bucket counts across the filtered range
}

// sparkBuckets is the number of time buckets behind the activity
// sparkline in breakdowns (CLI column and web UI cell).
const sparkBuckets = 12

func statsBreakdown(dim string, topN int, f store.Filter) error {
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	f.Limit = 1 << 30
	cmds, err := st.List(f)
	if err != nil {
		return err
	}
	if len(cmds) == 0 {
		if f.Active() {
			fmt.Println("no commands match those filters")
		} else {
			fmt.Println("no recorded commands yet — start a session with: backscroll run")
		}
		return nil
	}
	list := groupStats(cmds, dim)
	if dim == "day" || dim == "date" {
		if topN > 0 && len(list) > topN {
			list = list[len(list)-topN:] // most recent days
		}
		printDayTable(list)
		fmt.Printf("\n(%d commands total)\n", len(cmds))
		return nil
	} else {
		shown := list
		if topN > 0 && len(list) > topN {
			shown = list[:topN]
		}
		printStatTable(dim, shown)
		if len(shown) < len(list) {
			fmt.Printf("\n(top %d of %d — %d commands total%s; -n 0 for all)\n",
				len(shown), len(list), len(cmds), sparkSpanNote(cmds))
			return nil
		}
	}
	fmt.Printf("\n(%d commands total%s)\n", len(cmds), sparkSpanNote(cmds))
	return nil
}

// sparkSpanNote labels the time range the activity sparklines cover.
func sparkSpanNote(cmds []store.Command) string {
	first, last, ok := statTimeRange(cmds)
	if !ok {
		return ""
	}
	f, l := first.Local().Format("2006-01-02"), last.Local().Format("2006-01-02")
	if f == l {
		return ", activity on " + f
	}
	return ", activity " + f + " → " + l
}

// groupStats buckets commands along one dimension and sorts the result:
// day → chronological, everything else → count desc, key asc. Shared by
// the stats CLI and the web UI's /api/stats?by= endpoint.
func groupStats(cmds []store.Command, dim string) []*statGroup {
	first, last, spanOK := statTimeRange(cmds)
	span := last.Sub(first)
	groups := map[string]*statGroup{}
	for _, c := range cmds {
		key := statKey(dim, c)
		g := groups[key]
		if g == nil {
			g = &statGroup{key: key}
			if spanOK {
				g.spark = make([]int, sparkBuckets)
			}
			groups[key] = g
		}
		g.count++
		if c.ExitCode.Valid && c.ExitCode.Int64 != 0 {
			g.fails++
		}
		if !c.EndedAt.IsZero() && !c.StartedAt.IsZero() {
			if d := c.EndedAt.Sub(c.StartedAt); d > 0 {
				g.dur += d
			}
		}
		if spanOK && !c.StartedAt.IsZero() {
			b := sparkBuckets - 1
			if span > 0 {
				b = int(int64(c.StartedAt.Sub(first)) * sparkBuckets / (int64(span) + 1))
			}
			g.spark[b]++
		}
	}
	list := make([]*statGroup, 0, len(groups))
	for _, g := range groups {
		list = append(list, g)
	}
	if dim == "day" || dim == "date" {
		sort.Slice(list, func(i, j int) bool { return list[i].key < list[j].key })
	} else {
		sort.Slice(list, func(i, j int) bool {
			if list[i].count != list[j].count {
				return list[i].count > list[j].count
			}
			return list[i].key < list[j].key
		})
	}
	return list
}

// statTimeRange finds the first and last start time across the filtered
// commands (imported ts-less entries are skipped). ok is false when fewer
// than one timestamped command exists — then no sparklines are drawn.
func statTimeRange(cmds []store.Command) (first, last time.Time, ok bool) {
	for _, c := range cmds {
		if c.StartedAt.IsZero() {
			continue
		}
		if !ok || c.StartedAt.Before(first) {
			first = c.StartedAt
		}
		if !ok || c.StartedAt.After(last) {
			last = c.StartedAt
		}
		ok = true
	}
	return first, last, ok
}

var sparkRunes = []rune("▁▂▃▄▅▆▇█")

// sparkline renders per-bucket counts as a fixed-width run of block
// characters, scaled to the group's own busiest bucket; empty buckets
// stay blank so gaps in activity are visible.
func sparkline(buckets []int) string {
	max := 0
	for _, n := range buckets {
		if n > max {
			max = n
		}
	}
	if max == 0 {
		return strings.Repeat(" ", len(buckets))
	}
	var b strings.Builder
	for _, n := range buckets {
		if n == 0 {
			b.WriteByte(' ')
			continue
		}
		idx := (n*len(sparkRunes) - 1) / max
		if idx >= len(sparkRunes) {
			idx = len(sparkRunes) - 1
		}
		b.WriteRune(sparkRunes[idx])
	}
	return b.String()
}

// statKey computes the grouping key of one command for a dimension.
func statKey(dim string, c store.Command) string {
	switch dim {
	case "cwd", "dir":
		return c.Cwd
	case "exit":
		return exitStr(c)
	case "host":
		if c.Host == "" {
			return "local"
		}
		return c.Host
	case "day", "date":
		if c.StartedAt.IsZero() {
			return "unknown"
		}
		return c.StartedAt.Local().Format("2006-01-02")
	default: // cmd
		return cmdHead(c.Cmd)
	}
}

// wrappers are prefix commands that run another command; the interesting
// name comes after them (plus any leading VAR=val assignments and flags).
var statWrappers = map[string]bool{
	"sudo": true, "doas": true, "env": true, "command": true,
	"builtin": true, "exec": true, "time": true, "nohup": true,
	"stdbuf": true, "timeout": true, "watch": true, "xargs": true,
}

// subcmdTools are commands whose first subcommand is what you actually
// care about in a breakdown ("git push", not "git").
var statSubcmdTools = map[string]bool{
	"git": true, "docker": true, "podman": true, "kubectl": true,
	"npm": true, "pnpm": true, "yarn": true, "bun": true, "deno": true,
	"cargo": true, "go": true, "pip": true, "pip3": true, "uv": true,
	"poetry": true, "brew": true, "apt": true, "apt-get": true,
	"dnf": true, "pacman": true, "systemctl": true, "terraform": true,
	"gcloud": true, "aws": true, "az": true, "gh": true, "glab": true,
	"just": true, "make": true, "nix": true, "helm": true, "bundle": true,
	"rake": true, "mvn": true, "gradle": true, "composer": true,
	"svn": true, "hg": true, "jj": true, "backscroll": true,
}

// cmdHead normalizes a command line down to the name people scan for in a
// breakdown: wrapper prefixes (sudo, env, time, …) and VAR=val assignments
// are skipped, paths are reduced to their basename, and for multi-tool
// CLIs (git, docker, cargo, …) the first subcommand is kept.
func cmdHead(cmdline string) string {
	line := cmdline
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	fields := strings.Fields(line)
	head, lastWrapper := "", ""
	i := 0
	for ; i < len(fields); i++ {
		w := strings.TrimPrefix(fields[i], `\`) // \cmd = skip-alias spelling
		if isShellOp(w) {
			break // "env | grep": the wrapper itself was the command
		}
		if w == "" || strings.HasPrefix(w, "-") || isAssignment(w) {
			continue // flags of a wrapper / env assignments
		}
		base := w
		if strings.ContainsAny(base, "/") {
			base = filepath.Base(base)
		}
		if statWrappers[base] {
			lastWrapper = base
			continue
		}
		head = base
		break
	}
	if head == "" {
		if lastWrapper != "" {
			return lastWrapper
		}
		return "(other)"
	}
	if statSubcmdTools[head] {
		for j := i + 1; j < len(fields); j++ {
			w := fields[j]
			if isShellOp(w) {
				break // "make && git push" → make, not "make &&"
			}
			// skip flags, assignments and anything path-shaped — a
			// subcommand is a bare word ("git -C /tmp status" → status)
			if strings.HasPrefix(w, "-") || isAssignment(w) ||
				strings.ContainsAny(w, "/") || strings.HasPrefix(w, ".") || strings.HasPrefix(w, "~") {
				continue
			}
			return head + " " + w
		}
	}
	return head
}

// isShellOp reports whether a whitespace-delimited token starts a shell
// operator: pipes, logic, separators, redirections (incl. "2>", "&>").
func isShellOp(w string) bool {
	t := strings.TrimLeft(w, "0123456789")
	return t != "" && strings.IndexByte("|&;<>()", t[0]) >= 0
}

func isAssignment(w string) bool {
	i := strings.IndexByte(w, '=')
	if i <= 0 {
		return false
	}
	for _, r := range w[:i] {
		if r != '_' && !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// sigNames labels the common 128+n "killed by signal" exit codes.
var sigNames = map[string]string{
	"129": "SIGHUP", "130": "SIGINT", "131": "SIGQUIT", "134": "SIGABRT",
	"137": "SIGKILL", "139": "SIGSEGV", "141": "SIGPIPE", "143": "SIGTERM",
}

func printStatTable(dim string, list []*statGroup) {
	label := map[string]string{
		"cmd": "command", "command": "command",
		"cwd": "directory", "dir": "directory",
		"exit": "exit", "host": "host",
	}[dim]
	home, _ := os.UserHomeDir()
	withSpark := len(list) > 0 && list[0].spark != nil
	if withSpark {
		fmt.Printf("  \x1b[2m%7s  %5s  %10s  %-*s  %s\x1b[0m\n",
			"count", "fail%", "total time", sparkBuckets, "activity", label)
	} else {
		fmt.Printf("  \x1b[2m%7s  %5s  %10s  %s\x1b[0m\n", "count", "fail%", "total time", label)
	}
	for _, g := range list {
		key := statDisplayKey(dim, g.key, home)
		failPct := "-"
		if g.fails > 0 {
			failPct = fmt.Sprintf("%d%%", (g.fails*100+g.count/2)/g.count)
		}
		if withSpark {
			fmt.Printf("  %7d  %5s  %10s  \x1b[36m%s\x1b[0m  %s\n",
				g.count, failPct, humanDur(g.dur), sparkline(g.spark), key)
		} else {
			fmt.Printf("  %7d  %5s  %10s  %s\n", g.count, failPct, humanDur(g.dur), key)
		}
	}
}

// statDisplayKey renders a group key for humans: cwd is ~-shortened,
// exit codes get their signal label. Shared by CLI and web UI.
func statDisplayKey(dim, key, home string) string {
	switch dim {
	case "cwd", "dir":
		if home != "" {
			if key == home {
				return "~"
			}
			if strings.HasPrefix(key, home+"/") {
				return "~" + key[len(home):]
			}
		}
	case "exit":
		if sig := sigNames[key]; sig != "" {
			return key + " (" + sig + ")"
		}
		if key == "?" {
			return key + " (no exit recorded)"
		}
	}
	return key
}

func printDayTable(list []*statGroup) {
	maxCount := 0
	for _, g := range list {
		if g.count > maxCount {
			maxCount = g.count
		}
	}
	width := 30
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 && w-42 < width {
		width = w - 42
		if width < 8 {
			width = 8
		}
	}
	fmt.Printf("  \x1b[2m%10s  %7s  %5s\x1b[0m\n", "day", "count", "fail%")
	for _, g := range list {
		failPct := "-"
		if g.fails > 0 {
			failPct = fmt.Sprintf("%d%%", (g.fails*100+g.count/2)/g.count)
		}
		bar := strings.Repeat("█", (g.count*width+maxCount-1)/maxCount)
		fmt.Printf("  %10s  %7d  %5s  \x1b[36m%s\x1b[0m\n", g.key, g.count, failPct, bar)
	}
}

// humanDur renders a duration compactly: 0s, 4.2s, 3m41s, 1h12m, 2d3h.
func humanDur(d time.Duration) string {
	switch {
	case d == 0:
		return "-"
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}
