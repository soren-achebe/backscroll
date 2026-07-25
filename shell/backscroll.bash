# backscroll bash integration — https://github.com/soren-achebe/backscroll
# Emits OSC 133 shell-integration marks (plus command text) so the
# backscroll recorder can segment output per command. No-op outside a
# backscroll session; safe to keep in .bashrc permanently.
if [[ -n "$BACKSCROLL_ACTIVE" && -z "$BACKSCROLL_HOOKED" ]]; then
  export BACKSCROLL_HOOKED=1

  __bks_preexec() {
    # Only fire for the first DEBUG trap after a prompt, and not for
    # completion or PROMPT_COMMAND itself.
    [[ -n "$COMP_LINE" ]] && return
    [[ -z "$__bks_at_prompt" ]] && return
    # ignore our own bind -x widgets (e.g. the Ctrl-X Ctrl-P picker)
    [[ "$BASH_COMMAND" == __bks_* ]] && return
    # ignore fragments of PROMPT_COMMAND itself
    [[ -n "$BASH_COMMAND" && "$PROMPT_COMMAND" == *"$BASH_COMMAND"* ]] && return
    unset __bks_at_prompt
    __bks_ran_command=1
    local cmd
    cmd=$(HISTTIMEFORMAT= builtin history 1 2>/dev/null | sed 's/^ *[0-9]* *//')
    [[ -z "$cmd" ]] && cmd="$BASH_COMMAND"
    printf '\033]6973;cmd=%s\007' "$(printf '%s' "$cmd" | base64 | tr -d '\n')"
    printf '\033]133;C\007'
  }

  __bks_precmd() {
    local ec=$?
    if [[ -n "$__bks_ran_command" ]]; then
      printf '\033]133;D;%s\007' "$ec"
      unset __bks_ran_command
    fi
    printf '\033]7;file://%s%s\007' "${HOSTNAME:-localhost}" "$PWD"
    printf '\033]133;A\007'
    __bks_at_prompt=1
  }

  trap '__bks_preexec' DEBUG
  PROMPT_COMMAND="__bks_precmd${PROMPT_COMMAND:+;$PROMPT_COMMAND}"
fi

# Ctrl-X Ctrl-P: fuzzy-pick a past command (with output preview) and insert
# it at the prompt. Active in and out of recorded sessions; set
# BACKSCROLL_NO_BIND=1 before sourcing to skip. Needs fzf.
if [[ -z "$BACKSCROLL_NO_BIND" ]]; then
  __bks_pick_insert() {
    local sel
    sel=$(backscroll pick --print-cmd -- "$READLINE_LINE")
    if [[ -n "$sel" ]]; then
      READLINE_LINE=$sel
      READLINE_POINT=${#sel}
    fi
  }
  bind -m emacs -x '"\C-x\C-p": __bks_pick_insert' 2>/dev/null
  bind -m vi-insert -x '"\C-x\C-p": __bks_pick_insert' 2>/dev/null
  bind -m vi-command -x '"\C-x\C-p": __bks_pick_insert' 2>/dev/null
fi

# Tab completion (active in and out of recorded sessions).
_backscroll_complete() {
  local cur=${COMP_WORDS[COMP_CWORD]}
  local prev=${COMP_WORDS[COMP_CWORD-1]:-}
  if [ "$COMP_CWORD" -eq 1 ]; then
    COMPREPLY=($(compgen -W "run init list last show search pick diff export sync stats prune delete redact off on doctor version help" -- "$cur"))
  elif [ "${COMP_WORDS[1]}" = "init" ]; then
    COMPREPLY=($(compgen -W "bash zsh fish tmux" -- "$cur"))
  elif [ "${COMP_WORDS[1]}" = "export" ] && { [ "$prev" = "--format" ] || [ "$prev" = "-format" ]; }; then
    COMPREPLY=($(compgen -W "md cast json" -- "$cur"))
  elif [ "${COMP_WORDS[1]}" = "sync" ] && [ "$COMP_CWORD" -eq 2 ]; then
    COMPREPLY=($(compgen -W "init export import status" -- "$cur"))
  elif [ "${COMP_WORDS[1]}" = "list" ] || [ "${COMP_WORDS[1]}" = "last" ] || [ "${COMP_WORDS[1]}" = "search" ] || [ "${COMP_WORDS[1]}" = "pick" ]; then
    case "$prev" in
      --exit|-exit) COMPREPLY=($(compgen -W "fail 0 1 2" -- "$cur")) ;;
      --cwd|-cwd)   COMPREPLY=($(compgen -d -- "$cur")) ;;
      --since|-since|--session|-session|--host|-host|-n) ;;
      *) case "$cur" in
           -*) local _bs_flags="-n --session --cwd --exit --since --host"
               [ "${COMP_WORDS[1]}" = "pick" ] && _bs_flags="$_bs_flags --pager --raw --print-id --print-cmd --redact"
               COMPREPLY=($(compgen -W "$_bs_flags" -- "$cur")) ;;
         esac ;;
    esac
  fi
}
complete -F _backscroll_complete backscroll
