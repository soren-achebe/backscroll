# backscroll bash integration — https://github.com/soren-achebe/backscroll
# Emits OSC 133 shell-integration marks (plus command text) so the
# backscroll recorder can segment output per command. No-op outside a
# backscroll session; safe to keep in .bashrc permanently.
if [[ -n "$BACKSCROLL_ACTIVE" && -z "$BACKSCROLL_HOOKED" ]]; then
  export BACKSCROLL_HOOKED=1

  __bks_preexec() {
    # $1 = command text when invoked as a bash-preexec preexec function;
    # empty when invoked directly as a DEBUG trap.
    # Only fire for the first invocation after a prompt, and not for
    # completion or PROMPT_COMMAND itself.
    [[ -n "$COMP_LINE" ]] && return
    # Skip bind -x widgets (fzf's Ctrl-R, etc.): bash runs the DEBUG trap
    # for them too, which would emit a phantom mark labeled with the
    # previous command AND leave the real next command unmarked. bash sets
    # READLINE_LINE exactly while a bind -x command runs (bash >= 4.0) and
    # unsets it afterwards, so its presence identifies widget execution.
    [[ -n "${READLINE_LINE+x}" ]] && return
    [[ -z "$__bks_at_prompt" ]] && return
    if ((BASH_VERSINFO[0] < 4)); then
      # READLINE_LINE doesn't exist before bash 4.0 (macOS /bin/bash is
      # 3.2), so the guard above is inert there. Fall back on a 3.x
      # quirk: bind -x runs its handler while the tty is still in
      # readline's raw mode (icanon off), whereas accepted commands run
      # after readline restores canonical mode. Verified on 3.2.57;
      # HISTCMD is frozen inside the trap and LINENO advances for widget
      # bodies too, so neither distinguishes the cases — and comparing
      # `history 1` misfires under HISTCONTROL=ignoredups.
      case $(stty -a </dev/tty 2>/dev/null) in
        *-icanon*) return ;;
      esac
    fi
    local cmd
    if [[ -n "${1:-}" ]]; then
      cmd=$1
    elif (( ${#FUNCNAME[@]} > 1 )); then
      # DEBUG trap fired while inside another function. Two cases:
      # (a) bash-preexec was sourced *after* us and chained our old trap
      #     as __bp_original_debug_trap — a real user command; take it
      #     from history (BASH_COMMAND is bp's internals here).
      # (b) anything else (e.g. bash-preexec's `declare -ft` installers,
      #     whose internals fire DEBUG during hook installation) — not a
      #     user command; skip, or we'd record `local lastexit=$? ...`.
      [[ "${FUNCNAME[1]}" == "__bp_original_debug_trap" ]] || return
      cmd=$(HISTTIMEFORMAT= builtin history 1 2>/dev/null | sed 's/^ *[0-9]* *//')
      [[ -z "$cmd" ]] && return
    else
      # ignore our own bind -x widgets (e.g. the Ctrl-X Ctrl-P picker)
      # and bash-preexec internals (its installer runs from PROMPT_COMMAND)
      [[ "$BASH_COMMAND" == __bks_* || "$BASH_COMMAND" == __bp_* ]] && return
      # ignore fragments of PROMPT_COMMAND itself ([*] — it may be an
      # array on bash >= 5.1, and others may have appended elements).
      # Match against both the raw text and a bash-normalized rendering
      # (__bks_pc_norm, built in __bks_precmd): BASH_COMMAND is bash's
      # pretty-printed form, so e.g. Ghostty's `__ghostty_hook 2>/dev/null`
      # fires the trap as `__ghostty_hook 2> /dev/null` — a raw substring
      # test alone misses it and we'd record the hook as a command.
      [[ -n "$BASH_COMMAND" && "${PROMPT_COMMAND[*]}$__bks_pc_norm" == *"$BASH_COMMAND"* ]] && return
      cmd=$(HISTTIMEFORMAT= builtin history 1 2>/dev/null | sed 's/^ *[0-9]* *//')
      [[ -z "$cmd" ]] && cmd="$BASH_COMMAND"
    fi
    unset __bks_at_prompt
    __bks_ran_command=1
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
    # Rebuild the normalized PROMPT_COMMAND cache when it changes (other
    # integrations append their hooks lazily). Defining a throwaway
    # function around the fragments and printing it back with `declare -f`
    # yields bash's canonical formatting — the same formatting BASH_COMMAND
    # uses inside the DEBUG trap (redirections gain a space, etc.), which
    # the preexec fragment guard needs for its substring test.
    if [[ "${PROMPT_COMMAND[*]}" != "$__bks_pc_src" ]]; then
      __bks_pc_src="${PROMPT_COMMAND[*]}"
      __bks_pc_norm=""
      if eval "__bks_pc_probe() {
${PROMPT_COMMAND[*]}
}" 2>/dev/null; then
        __bks_pc_norm=$(declare -f __bks_pc_probe 2>/dev/null)
        unset -f __bks_pc_probe 2>/dev/null
      fi
    fi
    __bks_at_prompt=1
  }

  if [[ -n "${bash_preexec_imported:-}${__bp_imported:-}" ]]; then
    # bash-preexec is loaded (atuin, hishtory, etc. use it). Register into
    # its hook arrays instead of competing for the DEBUG trap — it hands us
    # the exact command text and guarantees once-per-command semantics.
    preexec_functions+=(__bks_preexec)
    precmd_functions+=(__bks_precmd)
  else
    trap '__bks_preexec' DEBUG
    PROMPT_COMMAND="__bks_precmd${PROMPT_COMMAND:+;$PROMPT_COMMAND}"
  fi
fi

# Ctrl-X Ctrl-P: fuzzy-pick a past command (with output preview) and insert
# it at the prompt. Active in and out of recorded sessions; set
# BACKSCROLL_NO_BIND=1 before sourcing to skip. Needs fzf, and bash >= 4.0
# (insertion works via READLINE_LINE, which bash 3.x doesn't have; 3.x
# bind -x also leaves the tty raw, which would garble fzf's UI).
if [[ -z "$BACKSCROLL_NO_BIND" ]] && ((BASH_VERSINFO[0] >= 4)); then
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
    COMPREPLY=($(compgen -W "run init list last show search pick diff export sync stats prune delete redact mcp off on doctor version help" -- "$cur"))
  elif [ "${COMP_WORDS[1]}" = "init" ]; then
    COMPREPLY=($(compgen -W "bash zsh fish tmux" -- "$cur"))
  elif [ "${COMP_WORDS[1]}" = "export" ] && { [ "$prev" = "--format" ] || [ "$prev" = "-format" ]; }; then
    COMPREPLY=($(compgen -W "md cast json" -- "$cur"))
  elif [ "${COMP_WORDS[1]}" = "sync" ] && [ "$COMP_CWORD" -eq 2 ]; then
    COMPREPLY=($(compgen -W "init export import status" -- "$cur"))
  elif [ "${COMP_WORDS[1]}" = "doctor" ]; then
    COMPREPLY=($(compgen -W "--reindex" -- "$cur"))
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
