# backscroll zsh integration — https://github.com/soren-achebe/backscroll
# Emits OSC 133 shell-integration marks (plus command text) so the
# backscroll recorder can segment output per command. No-op outside a
# backscroll session; safe to keep in .zshrc permanently.
if [[ -n "$BACKSCROLL_ACTIVE" && -z "$BACKSCROLL_HOOKED" ]]; then
  export BACKSCROLL_HOOKED=1

  __bks_preexec() {
    printf '\033]6973;cmd=%s\007' "$(printf '%s' "$1" | base64 | tr -d '\n')"
    printf '\033]133;C\007'
  }

  __bks_precmd() {
    local ec=$?
    printf '\033]133;D;%s\007' "$ec"
    printf '\033]7;file://%s%s\007' "${HOST:-localhost}" "$PWD"
    printf '\033]133;A\007'
  }

  autoload -Uz add-zsh-hook
  add-zsh-hook preexec __bks_preexec
  add-zsh-hook precmd  __bks_precmd
fi

# Tab completion (active in and out of recorded sessions).
if (( $+functions[compdef] )); then
  _backscroll() {
    if (( CURRENT == 2 )); then
      compadd run init list last show search pick diff export stats prune delete redact off on doctor version help
    elif [[ $words[2] == init ]]; then
      compadd bash zsh fish tmux
    elif [[ $words[2] == export && $words[CURRENT-1] == (--format|-format) ]]; then
      compadd md cast json
    elif [[ $words[2] == (list|last|search|pick) ]]; then
      if [[ $words[CURRENT-1] == (--exit|-exit) ]]; then
        compadd fail 0 1 2
      elif [[ $words[CURRENT-1] == (--cwd|-cwd) ]]; then
        _files -/
      elif [[ $words[CURRENT] == -* ]]; then
        local -a _bs_flags=(-n --session --cwd --exit --since)
        [[ $words[2] == pick ]] && _bs_flags+=(--pager --raw --print-id --print-cmd --redact)
        compadd -- $_bs_flags
      fi
    fi
  }
  compdef _backscroll backscroll
fi
