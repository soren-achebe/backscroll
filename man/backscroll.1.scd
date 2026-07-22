backscroll(1)

# NAME

backscroll - never lose a command's output again

# SYNOPSIS

*backscroll run*++
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
fish:	echo 'backscroll init fish | source' >> ~/.config/fish/config.fish

Run *backscroll doctor* to verify the setup.

# COMMANDS

*run* [*--head-cap* _N_] [*--tail-cap* _N_]
	Start a recorded shell session ($SHELL on a PTY). Outputs larger than
	the caps keep the first _head-cap_ (default 256 KiB) and last
	_tail-cap_ (default 1 MiB) bytes.

*init* <*bash*|*zsh*|*fish*>
	Print the shell-integration snippet (OSC 133 marks, cwd reporting, and
	tab completion) for the given shell.

*list* [*-n* _N_] [_filters_]
	List recent commands. Aliased as *last*.

*show* [_id_ | *-N*] [*--raw*] [*--redact*]
	Print the full stored output of a command. With no argument, the last
	command; *-2* is the one before last, and so on. *--raw* replays with
	colors; the default strips ANSI escapes.

*search* _query_ [_filters_] [*--redact*]
	Full-text search (SQLite FTS5, case-insensitive substring) over both
	command lines and outputs.

*diff* _a_ [_b_] [*-U* _N_]
	Line-diff two stored outputs (ids or -N offsets). With a single
	argument, diffs against the previous run of the *same* command -
	"what changed since last time?". Exit codes follow diff(1).

*export* [_id_|*-N* ...] [*--format* *md*|*cast*|*json*] [*--details*] [*--redact*] [*-o* _FILE_]
	Share commands and their outputs: GitHub-flavored markdown (default),
	asciinema v2 cast, or JSON.

*stats*
	Database statistics.

*prune* *--older* _30d_
	Delete entries older than the given age.

*delete* _id_
	Delete one entry.

*redact* [_id_|*-N* ...] [*--dry-run*]
	Permanently mask secrets (tokens, keys, passwords) in stored entries,
	including the search index. Idempotent.

*off* / *on*
	Pause / resume recording in the current session (in-band; no IPC).

*doctor*
	Check that shell integration, database, and environment are set up
	correctly.

# FILTERS

*list* and *search* accept:

*--cwd* _DIR_
	Only commands launched in _DIR_ or beneath it.

*--exit* _CODE_|*fail*
	Only commands with the given exit code, or any non-zero exit.

*--since* _30m_|_2h_|_3d_|_1w_|_YYYY-MM-DD[ HH:MM]_
	Only commands newer than the given age or date (local time).

*--session* _ID_
	Only commands from the given recording session.

# FILES

_~/.local/share/backscroll/backscroll.db_
	SQLite database with all recorded commands (override with
	*$BACKSCROLL_DB*).

_~/.config/backscroll/ignore_
	One Go regexp per line; matching commands are never stored.

_~/.config/backscroll/redact_
	Extra secret-masking patterns (one Go regexp per line) applied by
	*redact* and *--redact*.

# PRIVACY

Recording everything your terminal prints is the point - and a
responsibility. Recording is local-only and opt-in per session; alt-screen
applications (editors, pagers, TUIs) are excluded; ignore patterns prevent
storage, *off*/*on* pauses it, and *redact* scrubs after the fact. See the
README's privacy section for details.

# AUTHORS

Built and maintained by Soren Achebe, an AI agent.
Project page: https://github.com/soren-achebe/backscroll
