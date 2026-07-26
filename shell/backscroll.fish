# backscroll fish integration — https://github.com/soren-achebe/backscroll
# Emits OSC 133 shell-integration marks (plus command text) so the
# backscroll recorder can segment output per command. No-op outside a
# backscroll session; safe to keep in config.fish permanently.
if test -n "$BACKSCROLL_ACTIVE"; and test -z "$BACKSCROLL_HOOKED"
    set -gx BACKSCROLL_HOOKED 1

    function __bks_preexec --on-event fish_preexec
        printf '\033]6973;cmd=%s\007' (printf '%s' "$argv[1]" | base64 | tr -d '\n')
        printf '\033]133;C\007'
    end

    function __bks_postexec --on-event fish_postexec
        set -l ec $status
        printf '\033]133;D;%s\007' $ec
    end

    function __bks_prompt --on-event fish_prompt
        printf '\033]7;file://%s%s\007' (hostname) "$PWD"
        printf '\033]133;A\007'
    end
end

# Ctrl-X Ctrl-P: fuzzy-pick a past command (with output preview) and insert
# it at the prompt. Active in and out of recorded sessions; set
# BACKSCROLL_NO_BIND=1 before sourcing to skip. Needs fzf.
if test -z "$BACKSCROLL_NO_BIND"
    function __bks_pick_insert
        set -l sel (backscroll pick --print-cmd -- (commandline))
        if test -n "$sel"
            commandline -r -- $sel
        end
        commandline -f repaint
    end
    bind \cx\cp __bks_pick_insert
    if functions -q fish_vi_key_bindings
        bind -M insert \cx\cp __bks_pick_insert 2>/dev/null
        bind -M default \cx\cp __bks_pick_insert 2>/dev/null
    end
end

# Tab completion (active in and out of recorded sessions).
complete -c backscroll -f
complete -c backscroll -n __fish_use_subcommand -a "run init list last show search pick diff export sync stats prune delete redact mcp serve off on doctor version help"
complete -c backscroll -n "__fish_seen_subcommand_from init" -a "bash zsh fish tmux"
complete -c backscroll -n "__fish_seen_subcommand_from export" -l format -a "md cast json"
complete -c backscroll -n "__fish_seen_subcommand_from list last search pick" -s n -d "max results" -x
complete -c backscroll -n "__fish_seen_subcommand_from list last search pick" -l session -d "only this session id" -x
complete -c backscroll -n "__fish_seen_subcommand_from list last search pick" -l cwd -d "only this dir (or beneath)" -r -a "(__fish_complete_directories)"
complete -c backscroll -n "__fish_seen_subcommand_from list last search pick" -l exit -d "only this exit code" -x -a "fail 0 1 2"
complete -c backscroll -n "__fish_seen_subcommand_from list last search pick" -l since -d "only newer than (2h, 3d, 1w, date)" -x
complete -c backscroll -n "__fish_seen_subcommand_from list last search pick" -l host -d "only this synced host (local = this machine)" -x
complete -c backscroll -n "__fish_seen_subcommand_from sync" -a "init export import status"
complete -c backscroll -n "__fish_seen_subcommand_from doctor" -l reindex -d "rebuild the full-text search index"
complete -c backscroll -n "__fish_seen_subcommand_from pick" -l pager -d "view output in a pager"
complete -c backscroll -n "__fish_seen_subcommand_from pick" -l print-id -d "print only the selected id"
complete -c backscroll -n "__fish_seen_subcommand_from pick" -l print-cmd -d "print only the command line"
