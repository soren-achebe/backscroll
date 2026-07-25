# backscroll tmux integration
# Append to ~/.tmux.conf:   backscroll init tmux >> ~/.tmux.conf
# (needs tmux >= 3.2 for display-popup, and fzf on your PATH)

# prefix + B — fuzzy-search every recorded command + output in a popup
bind-key B display-popup -E -w 90% -h 85% "backscroll pick --pager"

# prefix + F — same, but only failed commands
bind-key F display-popup -E -w 90% -h 85% "backscroll pick --pager --exit fail"
