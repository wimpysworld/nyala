# Nyala demo tape. Record with `just record`.
#
# ntrecord renders through libghostty-vt, so it captures Kitty graphics and
# Block Unicode output in the same session.

Output pages/images/nyala.mp4

Set Shell "bash"
Set Width 1280
Set Height 720
Set FontSize 16
Set Framerate 30

Hide
Type "unset TMUX STY ZELLIJ HERDR_ENV HERDR_PANE_ID && ./nyala https://www.bbc.co.uk --backend kitty --kitty-transport direct --private"
Enter
Wait+Screen@30s /RGB/
Sleep 7s
Tab
Sleep 200ms
Tab
Sleep 200ms
Tab
Sleep 200ms
Enter
Sleep 1.5s
Show

# Phase 1: Kitty at top
Sleep 1.5s

# Phase 2: Scroll down in Kitty
Down@120ms 14
Sleep 0.6s
PageDown
Sleep 0.8s
PageDown
Sleep 1.0s

# Phase 3: Switch live to Block at scrolled-down position
Alt+r
Sleep 1.8s

# Phase 4: Scroll back up to top in Block
PageUp
Sleep 0.8s
PageUp
Sleep 0.6s
Up@100ms 24
Home
Sleep 1.5s

# Phase 5: Switch live back to Kitty at top
Alt+r
Sleep 2.2s

Hide
Ctrl+Q
Sleep 1s
