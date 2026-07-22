# Terminal compatibility

Block shows Nyala's default auto-selected glyph mode; `--blocks` can override it.
`Yes` under Sixel or Kitty means the terminal implements the protocol, or for
Alacritty that the protocol passed in the current test environment. It does not
mean every Nyala path was live-tested. `No` can also mean Nyala disables a
protocol to avoid a terminal-side limit. Notes record incomplete compatibility.

| Terminal | Block | Sixel | Kitty | Notes |
|---|---|---|---|---|
| Alacritty | Sextant | Yes | No | Sixel works in the current test environment, but stock upstream support differs. Built-in sextants can show a one-pixel seam. |
| Contour | Sextant | Yes | Yes | Kitty graphics support is version-dependent and requires Contour 0.7 or newer. Sixel was live-tested. |
| Foot | Sextant | Yes | No | Nyala omits DEC 2026 for Foot Sixel because Foot 1.27 can cancel pending refreshes. DECSDM remains enabled. Live-tested on Foot 1.27.0 under Hyprland 0.55.4. |
| Ghostty | Sextant | No | Yes | Native Kitty graphics; no Sixel support. |
| Kitty | Sextant | No | Yes | Native protocol. Nyala uses the POSIX shared-memory fast path where available, with direct PNG as the fallback. |
| mlterm | Quadrant | Yes | No | Sixel was live-tested. Sextants depend on font coverage, so auto mode uses quadrants. |
| Rio | Sextant | No | Yes | Tested with Rio 0.4.7. Rio 0.4.x has the terminal-side limits described below; Nyala applies compatibility gates for them. |
| WezTerm | Sextant | Yes | Yes | Sixel is documented as preliminary and experimental. Nyala's Kitty placement works. |

## Rio 0.4.x

The problems below are incompatibilities and implementation limits in Rio
0.4.x, not defects or protocol incompatibilities in Nyala. Nyala's gates avoid
these terminal-side limits.

The gates apply to XTVERSION releases from 0.4.0 up to, but not including,
0.5.0. When XTVERSION is unavailable, `TERM_PROGRAM=rio` or `TERM=rio` enables
them conservatively, including under a multiplexer. Retest and narrow the range
when a later Rio release changes these behaviours.

Rio 0.4.7 advertises Sixel through DA1 but does not render Sixel images. Nyala
therefore excludes Sixel from auto backend selection. An explicit
`--backend=sixel` request fails with an error that suggests Kitty or Block.

Rio stores terminal styles in an append-only table capped at 65,535 entries.
Truecolour Block rendering can exhaust that table, so `--color=auto` uses the
xterm-256 palette. Explicit `--color=256` and `--color=none` remain available.
Nyala rejects `--color=truecolor` when Block can be selected. Start a new Rio
tab or process if the table was already exhausted.

Kitty graphics remain enabled. Rio 0.4.x does not draw Nyala's negative-z Kitty
image layer, so Nyala omits negative-z placement and the live FPS badge. Page
images still work, FPS sampling continues, and the final FPS timing summary is
still printed.
