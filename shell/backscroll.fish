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

# Tab completion (active in and out of recorded sessions).
complete -c backscroll -f
complete -c backscroll -n __fish_use_subcommand -a "run init list last show search diff export stats prune delete off on doctor version help"
complete -c backscroll -n "__fish_seen_subcommand_from init" -a "bash zsh fish"
complete -c backscroll -n "__fish_seen_subcommand_from export" -l format -a "md cast json"
