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
| mlterm | Quadrant | Yes | No | Sixel was live-tested. Sextants depend on font coverage, so auto mode uses quadrants. In mlterm's default compact truecolour build, reuse of 240 mutable palette IDs can cause sparse incorrect Block-cell colours under high-cardinality workloads. WebGL Aquarium showed the issue, while Blender Charge did not in limited testing. This is an intermittent, workload-dependent mlterm limitation, not a Nyala SGR defect. Use `--color=256` as the manual workaround. Building mlterm with `--disable-compact-truecolor` fixed the corruption but severely slowed measured Block output to 5.3 FPS, with a 189.2 ms mean write time. |
| Rio | Sextant | No | Yes | Tested with Rio 0.4.7. Rio 0.4.x has the terminal-side limits described below; Nyala applies compatibility gates for them. |
| WezTerm | Sextant | Yes | Yes | Sixel is documented as preliminary and experimental. Nyala's Kitty placement works. |

## Graphics text size

Terminals disagree on the unit of the cell pixel size they report: Kitty, Rio,
and Contour report logical points, while Ghostty and WezTerm report scaled
device pixels (26x53 against 16x33 at the same HiDPI window). Ghostty and
WezTerm therefore used to lay out more CSS pixels on the Kitty and Sixel
backends, rendering page text about 1.6x smaller than the other terminals.
Nyala now anchors the CSS layout density per cell and carries the difference in
a fractional device scale factor, so every terminal lays out the same page at
the same text size. There is no layout-width ceiling (owner decision): the
density anchor already guarantees text size, so scaled super-ultrawide grids
stay normalised instead of over-scaling. Capture resolution still follows the
full terminal grid. See "Graphics cell-density layout normalisation
(2026-07-22)" in [PERFORMANCE.md](PERFORMANCE.md).

## Sixel performance

Measured with the live WebGL Aquarium at 2560x1600 with Sixel JPEG q90 capture
on the same machine, 2026-07-22. Full metric lines live in
[PERFORMANCE.md](PERFORMANCE.md) under "Sixel palette reuse (2026-07-22)".

| Terminal | Nyala decode mean ms | Nyala encode mean ms | Terminal write mean ms | Output fps |
|---|---|---|---|---|
| mlterm | 13.5 | 28.3 | 42.2 | 22.9 |
| Alacritty | 13.7 | 29.4 | 55.6 | 17.5 |
| foot | 13.6 | 30.0 | 123.9 | 8.1 |
| Contour | 13.0 | 30.3 | 250.3 | 4.0 |
| WezTerm | 29.2 | 70.3 | 553.7 | 1.8 |

Ingest was 60.0 fps (the CDP cap) on every terminal except WezTerm (38.3).
Nyala's side is constant across emulators: decode 13 to 14 ms, encode 28 to
30 ms. Output rate is set by the emulator's Sixel consumption, a 13x spread
from mlterm to WezTerm on identical payloads. Sixel is in-band with no
shared-memory escape hatch, so the emulator sets the ceiling. Nyala's
freshest-frame pacing degrades gracefully; slow terminals show the newest
frame at a lower rate rather than lagging behind. WezTerm is the double
outlier. Its parse and raster cost contends for CPU, which inflates Nyala's
own decode and encode and drops ingest, the only tested emulator that
degrades the Nyala side. mlterm is the fastest Sixel renderer tested.

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
Nyala rejects `--color=truecolor` for every starting backend, because Tab can
always cycle to Block at runtime. Start a new Rio
tab or process if the table was already exhausted.

Kitty graphics remain enabled. Rio 0.4.x does not draw Nyala's negative-z Kitty
image layer, so Nyala omits negative-z placement and the live FPS badge. Page
images still work, FPS sampling continues, and the final FPS timing summary is
still printed.

## Multiplexers

`Host mode` under Block means the glyph mode follows the outer terminal:
multiplexers pass the host's identity variables through, so an allow-listed
host gets sextants and an unknown host gets quadrants. `Manual` under Sixel
means the backend is enabled and selectable through Tab or `--backend=sixel`,
but auto-detection does not start on it. Notes record the gates and limits
explained in the prose below.

| Multiplexer | Block | Sixel | Kitty | Notes |
|---|---|---|---|---|
| tmux | Host mode | Yes | No | Needs tmux 3.4 or later built with `--enable-sixel`; the Debian, Ubuntu, Homebrew, and nixpkgs builds enable it. Honest DA1 gate plus real cell geometry. Auto-detection starts on Sixel. Bottom row reserved. Live-tested. |
| zellij | Host mode | Manual | No | zellij 0.31.0 or later re-encodes Sixel but drops every third full-replacement frame (image id collision), so auto-detection starts on Block. DA1 is advertised unconditionally, so real cell geometry gates non-Sixel hosts. Bottom row reserved. Live-tested. |
| GNU screen | Host mode | No | No | Blocks graphics passthrough entirely; no handshake runs. |
| herdr | Host mode | No | No | Consumes graphics sequences, reports no cell geometry, and has no passthrough. Experimental Kitty-only support is deferred. |

Nyala never offers the Kitty backend under a multiplexer. tmux and zellij
consume the Kitty APC protocol silently without passthrough, so the startup
Kitty probe is not sent there and the capability stays undetected. Under tmux
and zellij Nyala runs a reduced handshake instead: the cell-size queries,
XTVERSION, and the DA1 fence, with no Kitty APC probe.

Sixel under tmux and zellij is gated per multiplexer:

- tmux 3.4 and later stores and re-emits Sixel natively when built with
  `--enable-sixel`; the Debian, Ubuntu, Homebrew, and nixpkgs builds enable
  it. tmux advertises DA1 attribute 4 only when both its build and its outer
  terminal support Sixel, so its DA1 reply is an honest gate. tmux answers
  XTVERSION with `tmux 3.x`.
- zellij 0.31.0 and later decodes, crops per pane, and re-encodes Sixel, on
  by default. zellij answers DA1 with `?62;4c` unconditionally, even when its
  host terminal cannot render Sixel, so DA1 alone is not a sufficient gate.
  zellij synthesises pane-relative pixel geometry (the winsize pixel fields
  and the `CSI 14t`/`16t` replies) only after its own startup query to the
  host succeeded, so Nyala also requires real cell pixel geometry before
  offering Sixel: the host-chain-live signal. zellij answers XTVERSION as
  `Zellij(<version>)`.
- The geometry requirement applies under tmux too. Without real cell pixel
  geometry the Sixel surface would be sized from the 8x16 fallback and render
  at the wrong physical scale, so Nyala stays on Block.

Sixel under tmux and zellij reserves the bottom terminal row: the engine grid
runs at rows-1, so the frame is one cell row short and the last row stays
blank. Two independent bottom-row faults force this, both root-caused from
multiplexer source:

- tmux's `screen_write_sixelimage` unconditionally rescales any Sixel image
  taller than pane rows minus one, keeping the bottom rows and cropping the
  top; the last pane row is reserved for the cursor. A full-pane-height frame
  therefore always loses its top row. This is designed-in behaviour and will
  not change upstream.
- zellij honours Nyala's DECSDM wrap inside its own grid but re-serialises the
  visible image chunks to the host terminal bare, with no DECSDM and the host
  in scrolling mode. When the frame's bottom pixel row reaches the host's
  bottom cell row, the host scrolls one line per frame and zellij's absolute
  repaint snaps it back: a scroll-then-snap-back judder during page scrolling.
  This is a zellij defect that deserves an upstream report.

The reservation costs one blank row under both multiplexers. Block keeps the
full grid there, and native terminals are unaffected on every backend.

zellij additionally drops every third full-replacement Sixel frame. Its
`sixel.rs::next_image_id` allocates image ids as the current store size
instead of monotonically, so a stream that replaces the whole frame each time
(Nyala's exact pattern) collides ids every third frame, and the end-of-frame
reap in `grid.rs::read_changes` deletes the just-inserted image before
serialising it to the host. The symptom is the picture vanishing every third
frame, perceived as a rhythmic scroll bounce during page scrolling. zellij
issue #5125 reports the same id-collision pattern. No Nyala-side workaround
exists, so under zellij auto-detection starts on Block and Sixel stays
selectable through Tab or `--backend=sixel` (`multiplexerStartBackend` in
`internal/viewer/viewer.go`). Lift the demotion in the compatibility policy,
gated on the XTVERSION identity `Zellij(<version>)`, once a zellij release
ships monotonic image ids. tmux is unaffected: its auto-detection still
starts on Sixel when the gate passes.

GNU screen blocks graphics passthrough, and herdr consumes graphics sequences
and reports no cell geometry, so both stay Block only with no handshake.
herdr's experimental Kitty-only support is deferred.
