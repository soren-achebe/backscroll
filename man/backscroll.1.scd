backscroll(1)

# NAME

backscroll - never lose a command's output again

# SYNOPSIS

*backscroll run*++
*backscroll exec* _command_ [_args_...]++
*backscroll* _query-command_ [_options_...]

# DESCRIPTION

*backscroll* records your interactive shell sessions *per command*: for every
command you run it stores the command line, its full output, exit code,
working directory, and duration in a local SQLite database with full-text
search. When your terminal scrollback is gone - cleared, scrolled away, in a
closed tmux pane, or from yesterday - the outputs are still there.

It works by spawning your normal shell on a pseudo-terminal and passing every
byte through untouched, while a small shell-integration snippet emits OSC 133
semantic-prompt marks that let the recorder segment the stream into commands.
Everything stays on your machine; there is no daemon, no cloud, and no
telemetry.

# SETUP

Add the shell integration to your rc file, then start recorded sessions with
*backscroll run* (or make it your terminal's login command):

	bash:	echo 'eval "$(backscroll init bash)"' >> ~/.bashrc++
zsh:	echo 'eval "$(backscroll init zsh)"' >> ~/.zshrc++
fish:	echo 'backscroll init fish | source' >> ~/.config/fish/config.fish++
pwsh:	Add-Content $PROFILE 'backscroll init pwsh | Out-String | Invoke-Expression'

Run *backscroll doctor* to verify the setup.

fish ≥ 4.0 emits OSC 133 marks (with the command line attached) natively,
so *backscroll run* records it with no snippet at all; the snippet still
adds the picker binding and tab completion, and both together are fine.
nushell is fully zero-config as well — nu ships with OSC 133 integration
on by default, and backscroll reconstructs the command text from the
terminal echo (nu reports Ctrl-C during a command as exit 1, its own
convention).

# COMMANDS

*run* [*--head-cap* _N_] [*--tail-cap* _N_] [*-l*|*--login*]
	Start a recorded shell session ($SHELL on a PTY). Outputs larger than
	the caps keep the first _head-cap_ (default 256 KiB) and last
	_tail-cap_ (default 1 MiB) bytes. The shell is a plain interactive
	shell so that bash reads _~/.bashrc_; pass *--login* for login-shell
	semantics (note that a login bash does not read _~/.bashrc_).

*exec* [*--quiet*] [*--head-cap* _N_] [*--tail-cap* _N_] _command_ [_args_...]
	Run one command and record its output, exit code, cwd and timing —
	no recorded session needed. Built for cron jobs, CI steps and long
	builds. stdin/stdout/stderr pass straight through (tee-style;
	*--quiet* records without echoing) and the child's exit code is
	mirrored, including 128+_n_ for signal deaths, so it drops into
	pipelines and crontabs without changing behavior. A recording
	problem never stops or fails the command itself. Flags after
	_command_ belong to the command; use *backscroll exec sh -c '...'*
	for pipes and redirection. The command runs without a terminal
	(output is piped), so most tools disable colors, exactly as under
	*tee*; ignore patterns apply as usual.

*init* <*bash*|*zsh*|*fish*|*pwsh*|*tmux*|*zellij*|*screen*>
	Print the shell-integration snippet (OSC 133 marks, cwd reporting,
	tab completion, and a *Ctrl-X Ctrl-P* binding that opens *pick* and
	inserts the selected command at the prompt — set
	_$BACKSCROLL_NO_BIND_ to skip the binding) for the given shell, or a
	picker key-binding snippet for your multiplexer: tmux popup binds
	for _~/.tmux.conf_, zellij floating-pane keybinds for
	_~/.config/zellij/config.kdl_ (including *Alt r*, which types the
	picked command back at your prompt — any shell, no rc snippet
	needed), or GNU screen window binds for _~/.screenrc_.

*list* [*-n* _N_] [_filters_]
	List recent commands. Aliased as *last*.

*show* [_id_ | *-N*] [*--raw*] [*--redact*]
	Print the full stored output of a command. With no argument, the last
	command; *-2* is the one before last, and so on. *--raw* replays with
	colors; the default strips ANSI escapes.

*search* _query_ [_filters_] [*-C* _n_] [*-A* _n_] [*-B* _n_] [*--redact*]
	Full-text search (SQLite FTS5, case-insensitive substring) over both
	command lines and outputs.

	*-C* _n_ shows every matching output line with _n_ lines of context
	before and after, line-numbered, like *grep -C* (*-A*/*-B* set
	after/before independently; *-C 0* shows matching lines only).
	Without these flags each result shows a short snippet instead.

*pick* [_query_] [_filters_] [*--pager*] [*--raw*] [*--print-id*] [*--print-cmd*] [*--redact*]
	Interactive fuzzy picker over recorded commands (requires *fzf*(1))
	with a live preview of each command's stored output. Enter prints the
	selected output; *--pager* views it in _$PAGER_ (default *less -R*) -
	the right mode for a tmux popup (see *init tmux*). *--print-id* /
	*--print-cmd* print only the id / command line, for scripting.

*diff* _a_ [_b_] [*-U* _N_]
	Line-diff two stored outputs (ids or -N offsets). With a single
	argument, diffs against the previous run of the *same* command -
	"what changed since last time?". Exit codes follow diff(1).

*export* [_id_|*-N* ...] [_filters_] [*--format* *md*|*cast*|*json*|*html*] [*--details*] [*--redact*] [*-o* _FILE_]
	Share commands and their outputs: GitHub-flavored markdown (default),
	asciinema v2 cast, JSON, or a self-contained HTML page (dark theme,
	full ANSI color, no JavaScript — attach it to a ticket or open it
	locally). Pick commands by explicit ids / *-N*
	offsets, or with the shared filters (see _FILTERS_): *export --exit
	fail --since 1d* renders today's failures oldest-first (*-n* caps how
	many, default 20 newest matches).

*import* <*atuin*|*zsh*|*bash*|*fish*|*nu*|*pwsh*> [_path_] [*--dry-run*] [*--host* _NAME_] [*-n* _N_]
	Seed the database from history you already have, so *list*,
	*search*, *pick* and *stats* are useful before your first recorded
	session. Sources: *atuin* (_~/.local/share/atuin/history.db_ —
	timestamps, exit codes, cwd, hostnames), *zsh* (_$HISTFILE_ or
	_~/.zsh_history_; timestamps and durations when EXTENDED_HISTORY
	was set), *bash* (_$HISTFILE_ or _~/.bash_history_; timestamps when
	HISTTIMEFORMAT was set), *fish* (_fish_history_), *nu*
	(nushell's _history.sqlite3_ — timestamps, exit codes, cwd,
	hostnames — or plaintext _history.txt_, detected by content),
	*pwsh* (PSReadLine's _ConsoleHost_history.txt_). Imported entries
	have no recorded output — nobody was recording at the time — and
	entries without timestamps show their age as unknown and are
	deduplicated by command text. Re-running an import only adds new
	entries. *--dry-run* parses and reports without writing; *--host*
	labels file-sourced entries with an origin host (e.g. a server's
	history file you copied over); *-n* keeps only the newest N.
	Not to be confused with *sync import* below.

*sync* <*init* _dir_|*export*|*import*|*status*>
	Cross-machine history sync through a shared directory (Syncthing,
	Dropbox, rsync — any file sync works; no server involved). Each
	machine appends its own end-to-end-encrypted log (XChaCha20-Poly1305;
	shared key file _~/.config/backscroll/sync.key_ that you copy to each
	machine) and imports the others'. *init* _dir_ sets it up, *export*
	writes new local history, *import* pulls in other machines' history,
	*status* shows per-machine progress and the key fingerprint. Redact
	and ignore patterns are applied *before* anything leaves the machine;
	imported entries appear in *list*/*search*/*pick* tagged with their
	host (filter with *--host*). Set _$BACKSCROLL_HOST_ to override this
	machine's advertised hostname. Design: _docs/sync-design.md_.

*stats* [*--by* _cmd_|_cwd_|_exit_|_host_|_day_] [*-n* _rows_]
	Without flags: database statistics (counts, sizes, paths). With
	*--by*, a breakdown of your history along one dimension — count,
	failure rate and total wall time per group. *--by cmd* groups by
	normalized command head (wrappers like _sudo_/_env_/_time_ and
	VAR=val assignments are skipped; multi-tool CLIs keep their first
	subcommand: "git push", not "git"), *--by cwd* by directory,
	*--by exit* by exit code (signal names labeled), *--by host* by
	origin machine (synced history), *--by day* as a per-day histogram.
	The shared *list* filters scope the breakdown: *backscroll stats --by
	cmd --exit fail --since 1w* is "what failed most this week". *-n*
	caps the rows shown (default 15; 0 = all). Each row also carries an
	_activity_ sparkline — 12 time buckets across the filtered range
	showing _when_ that group ran (entries imported from shell history
	without timestamps are left out).

*note* [_id_|*-N*] [_text_ ...] | [*--rm*]
	Attach a note to a stored command ("this is the one that fixed it").
	Defaults to the last command; give an id or a *-N* offset to target
	an older one. Notes appear in *list*, *show*, *search* results, the
	web UI and exports, and are searchable (*search* matches notes as
	well as commands and outputs). Without text the current note is
	printed; *--rm* removes it. Notes stay local: they are not carried
	along by *sync* exports.

*prune* *--older* _30d_
	Delete entries older than the given age.

*delete* _id_
	Delete one entry.

*redact* [_id_|*-N* ...] [*--dry-run*]
	Permanently mask secrets (tokens, keys, passwords) in stored entries,
	including the search index. Idempotent.

*mcp* [*--no-redact*]
	Run a Model Context Protocol server on stdin/stdout (JSON-RPC, MCP
	stdio transport), so AI coding agents can query your history: tools
	_search_output_, _list_commands_, _get_output_, _diff_output_.
	Secrets are masked by default in everything handed to the client
	(same patterns as *redact*); *--no-redact* disables that. Register
	with your client as command *backscroll*, args *mcp*.

*serve* [*--addr* _host:port_] [*--redact*] [*--open*]
	Serve a local, read-only web UI for browsing and searching history:
	search-as-you-type over commands and outputs (with grep-style
	context lines around matches), ANSI colors rendered, one-click diff
	against the previous run of the same command, stats breakdowns
	(by command, directory, exit, host or day) scoped by the active
	filters, and per-command _#id_ permalinks that open one command
	full-page. A download button saves any command as the same
	self-contained page *export --format html* writes. Binds
	to loopback only by default (_127.0.0.1:4133_) and rejects non-local
	Host headers (DNS-rebinding guard). *--redact* masks secrets in
	everything served (colors are lost on redacted output). *--open*
	opens the UI in your default browser once listening.

*off* / *on*
	Pause / resume recording in the current session (in-band; no IPC).

*doctor*
	Check that shell integration, database, and environment are set up
	correctly. On an empty database it also lists importable history
	(atuin, nu, zsh, bash, fish, pwsh) found on this machine, with entry counts.

*doctor --reindex*
	Rebuild the full-text search index from the stored outputs. Use if
	search results ever look incomplete (e.g. after a crash mid-write or
	after older backscroll versions wrote to the same database).

# FILTERS

*list*, *search*, *pick*, and *export* accept:

*--cwd* _DIR_
	Only commands launched in _DIR_ or beneath it.

*--exit* _CODE_|*fail*
	Only commands with the given exit code, or any non-zero exit.

*--since* _30m_|_2h_|_3d_|_1w_|_YYYY-MM-DD[ HH:MM]_
	Only commands newer than the given age or date (local time).

*--until* _30m_|_2h_|_3d_|_1w_|_YYYY-MM-DD[ HH:MM]_
	Only commands older than the given age or date (exclusive bound,
	local time). Combine with *--since* to select a window:
	*--since 2026-07-26 --until 2026-07-27* is exactly that day.

*--session* _ID_
	Only commands from the given recording session.

*--host* _NAME_|*local*
	Only commands recorded on the given synced machine, or (*local*) on
	this machine.

# FILES

_~/.local/share/backscroll/backscroll.db_
	SQLite database with all recorded commands (override with
	*$BACKSCROLL_DB*).

_~/.config/backscroll/ignore_
	One Go regexp per line; matching commands are never stored.

_~/.config/backscroll/redact_
	Extra secret-masking patterns (one Go regexp per line) applied by
	*redact* and *--redact*.

_~/.config/backscroll/sync.key_
	Shared symmetric key for *sync* (64 hex chars). Copy it to every
	machine; anyone holding it can read your synced history.

_~/.config/backscroll/sync.json_
	Local sync configuration (sync directory, this machine's id).

# PRIVACY

Recording everything your terminal prints is the point - and a
responsibility. Recording is local-only and opt-in per session; alt-screen
applications (editors, pagers, TUIs) are excluded; ignore patterns prevent
storage, *off*/*on* pauses it, and *redact* scrubs after the fact. See the
README's privacy section for details.

# AUTHORS

Built and maintained by Soren Achebe, an AI agent.
Project page: https://github.com/soren-achebe/backscroll
