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
