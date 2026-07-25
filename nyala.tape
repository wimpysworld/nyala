# Nyala demo tape. Record with `just vhs`.
#
# VHS records through ttyd and xterm.js, not through your terminal. That stack
# has no Kitty graphics parser, and VHS captures only the text and cursor
# canvases, so the Sixel image layer never reaches the GIF. Block is the one
# backend VHS can record.
#
# Two rules follow from that:
#
#   1. Force `--backend block`. With its image addon loaded, xterm.js answers
#      DA1 with the Sixel attribute, so auto-detection would pick Sixel and
#      record a page with no pixels in it.
#   2. Never add `Alt+R`. Cycling backends switches to Sixel and the page
#      vanishes from the recording.
#
# `--blocks quadrant` is deliberate too: xterm.js does not custom-draw the
# sextant range, so sextants would record as tofu.

Output assets/nyala.gif

Require ./nyala

Set Shell "fish"
Set FontSize 14
Set Width 1280
Set Height 720
Set Padding 8
Set WindowBar ColorfulRight
Set WindowBarSize 32
Set TypingSpeed 27ms
Set Theme catppuccin-mocha

# Start at the coarsest settings: half blocks, 256 colours.
Type "./nyala --backend block --blocks half --color=256 https://www.bbc.co.uk" Sleep 250ms Enter
Sleep 8s

# Scroll the page, then return to the top.
PageDown
Sleep 2s
PageDown
Sleep 2s
PageUp 2
Sleep 2s

# Alt+C cycles True Colour, 256, Monochrome. From 256 that means passing
# through monochrome to reach truecolour, so move through it quickly.
Alt+C
Alt+C
Sleep 2s

# The same scroll again, now in truecolour.
PageDown
Sleep 2s
PageDown
Sleep 2s
PageUp 2
Sleep 2s

# Alt+B cycles Half, Quadrant, Sextant. Quadrant doubles the vertical
# resolution of half blocks. The tape stops here: the cycle skips sextant on a
# terminal that is not on the self-drawing allow-list, and ttyd is not on it.
Alt+B
Sleep 4s
