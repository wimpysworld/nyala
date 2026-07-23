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
host gets sextants and an unknown host gets quadrants. Notes record the gates
and limits explained in the prose below.

| Multiplexer | Block | Sixel | Kitty | Notes |
|---|---|---|---|---|
| tmux | Host mode | Yes | No | Needs tmux 3.6 or later built with `--enable-sixel`; the Debian, Ubuntu, Homebrew, and nixpkgs builds enable it. 3.6b or newer is recommended. Honest DA1 gate plus real cell geometry plus the XTVERSION 3.6+ version gate. Auto-detection starts on Sixel. Bottom row reserved. Live-tested. |
| zellij | Host mode | No | No | Sixel is blocked outright: zellij 0.31.0 or later re-encodes Sixel but drops every third full-replacement frame (image id collision) and breaks page scrolling (scroll-then-snap-back). Both are zellij defects. Block is the zellij backend. Live-tested. |
| GNU screen | Host mode | No | No | Blocks graphics passthrough entirely; no handshake runs. |
| herdr | Host mode | No | No | Consumes graphics sequences, reports no cell geometry, and has no passthrough. Experimental Kitty-only support is deferred. |

Nyala never offers the Kitty backend under a multiplexer. tmux and zellij
consume the Kitty APC protocol silently without passthrough, so the startup
Kitty probe is not sent there and the capability stays undetected. Under tmux
and zellij Nyala runs a reduced handshake instead: the cell-size queries,
XTVERSION, and the DA1 fence, with no Kitty APC probe.

Sixel under tmux is gated three ways:

- tmux stores and re-emits Sixel natively when built with `--enable-sixel`;
  the Debian, Ubuntu, Homebrew, and nixpkgs builds enable it. tmux advertises
  DA1 attribute 4 only when both its build and its outer terminal support
  Sixel, so its DA1 reply is an honest gate. tmux answers XTVERSION with
  `tmux 3.x`, and Nyala requires that version to be 3.6 or later; an unknown
  or unparsed identity fails the gate. tmux 3.4 through 3.5a compile in a
  1 MiB DCS input cap with no `input-buffer-size` option to raise it, so any
  busy frame over 1 MiB is silently discarded: the original invisible
  black-screen failure. The 900 KB fallback budget cannot guarantee that cap
  on large grids (the downscale ladder floor at factor 4 encodes ~1.45 MB
  and the smallest-anyway rule would feed tmux frames it throws away with no
  signal), those versions are untested, and the ecosystem answer for them is
  shrink or refuse, so Nyala refuses: tmux below 3.6 gets no Sixel and stays
  on Block. Sixel then leaves the backend cycle entirely, so `--backend=sixel`
  fails with the standard "not supported by this terminal" error. Prefer 3.6b
  or newer over 3.6 and 3.6a: 3.6b fixes an image-eviction use-after-free in
  the stored-image LRU.
- Real cell pixel geometry must be present. Without it the Sixel surface
  would be sized from the 8x16 fallback and render at the wrong physical
  scale, so Nyala stays on Block.

Sixel under zellij is blocked outright, regardless of DA1 and geometry. Live
testing surfaced two zellij-side defects, both root-caused from zellij
source, that would present as Nyala bugs:

- zellij's `sixel.rs::next_image_id` allocates image ids as the current store
  size instead of monotonically, so a stream that replaces the whole frame
  each time (Nyala's exact pattern) collides ids every third frame, and the
  end-of-frame reap in `grid.rs::read_changes` deletes the just-inserted
  image before serialising it to the host. The picture vanishes every third
  frame. zellij issue #5125 reports the same id-collision pattern.
- zellij honours Nyala's DECSDM wrap inside its own grid but re-serialises the
  visible image chunks to the host terminal bare, with no DECSDM and the host
  in scrolling mode. Page scrolling does not work: the host scrolls, then
  zellij's absolute repaint snaps it back.

No Nyala-side workaround exists for either, so `multiplexerBackends` never
offers Sixel under zellij: auto-detection and Tab stay on Block, and
`--backend=sixel` fails with the standard "not supported by this terminal"
error, the same shape as the tmux pre-3.6 refusal. Kitty stays blocked as
under every multiplexer, so Block is the zellij backend. Lift the block in
the compatibility policy, gated on the XTVERSION identity `Zellij(<version>)`,
once a zellij release fixes both defects. (zellij answers DA1 with `?62;4c`
unconditionally and XTVERSION as `Zellij(<version>)`; neither is consulted
while the block stands.)

Sixel under tmux reserves the bottom terminal row: the engine grid runs at
rows-1, so the frame is one cell row short and the last row stays blank.
tmux's `screen_write_sixelimage` unconditionally rescales any Sixel image
taller than pane rows minus one, keeping the bottom rows and cropping the
top; the last pane row is reserved for the cursor. A full-pane-height frame
therefore always loses its top row. This is designed-in behaviour and will
not change upstream. The reservation costs one blank row; Block keeps the
full grid there, and native terminals are unaffected on every backend.

tmux Sixel additionally survives high-frame-rate animation through tmux-only
measures; zellij, screen, herdr, and native terminals keep their normal
behaviour:

- Byte budget with adaptive downscale. On tmux 3.7 and later rate pacing is
  all tmux needs: live 3.7b runs with no per-frame erase sustained ~4 MB
  full-quality frames indefinitely at the paced rate, tmux consuming each
  frame in about 230 ms. So once the input-buffer-size raise succeeds, the
  per-frame budget on 3.7+ is 15 MiB and frames pass at full quality. On
  3.6.x a successful raise carries a 1.5 MB budget instead, an output-stage
  rate cap of ~5 MB/s at the 300 ms legacy floor: live 3.6a with the ladder
  settled at factor 2 (2.5 MB frames, 300 ms pacing, ~8.3 MB/s) kept its
  pty writes fully drained at ~21 ms, yet quitting replayed a queued
  backlog, and an earlier factor-3 run at the 250 ms floor (~8.8 MB/s)
  burst identically. The bottleneck is 3.6's re-encoded output toward the
  host terminal, invisible to every write-duration signal, and factor 3
  trims only ~10 percent off factor 2 on such content, so the cure is a
  byte-rate cap, not a deeper fixed factor. Where the set fails the budget
  falls back to 900 KB, keeping a margin under tmux's default 1 MiB input
  buffer, whose overflow discards the DCS silently; that parser cap is then
  the tighter bound. tmux below 3.6 never reaches the fallback: the version
  gate refuses Sixel there outright. An
  over-budget frame is downscaled by a ladder factor (1.5, 2, 3, 4) and
  upscaled back to the exact grid pixel extent before encoding, so the DCS
  raster keeps covering the full grid while the blur shortens the encoded
  output; the ladder carries the quality trade. If even the floor factor
  does not fit, the smallest encode is sent anyway. The ladder also
  escalates on measured write latency, with no version gate: when the
  effective pacing interval (write duration plus the 50 ms margin) reaches
  the resolved pacing floor (a write of 200 ms or more at the 250 ms floor,
  250 ms or more at the legacy 300 ms floor) for two consecutive frames,
  the ladder steps one factor, trading one mild blur step for frame rate.
  After any escalation the next two write samples are skipped: they measure
  backlog encoded at the old factor (frames in flight through the writer
  handoff plus tmux's own buffered input), and counting them chained the
  ladder 1.5 -> 4 within four frames on live 3.6a before the new factor's
  true cost was ever observed. Only a fresh slow streak measured after the
  skip escalates again.
  The criterion is whether tmux ever idles between frames, not whether a
  write exceeds some multiple of the floor: a healthy drained pipe shows
  tiny writes (tmux 3.7b at the floor: 33 ms mean, 40 ms max), while
  sustained writes near the floor mean tmux runs at exact capacity with
  zero slack, so backlog built during earlier bursts never drains and
  replays on quit. Measured live under an earlier 1.4x-floor threshold,
  tmux 3.6a held ~215 ms writes at factor 2 (under that threshold, so
  escalation stopped) and never drained its backlog. The working factor is
  remembered across frames, and quality recovery runs per motion episode: an
  episode spans from the first budgeted frame after a settle emit (or
  session start) to the next settle. Within an episode the ladder probes one
  step weaker every 10 budgeted frames (about three seconds at the pacing
  rate). A probe is a band-judged trial: while it is pending, any recorded
  write reaching the probe band (60 percent of the resolved floor: 150 ms at
  250 ms, 180 ms at the legacy 300 ms) fails it
  at once, the ladder retreats straight to the pre-probe factor, and
  probing is silenced for the rest of the episode: the ladder just watched
  that sharper factor fail, so retrying it during sustained motion only
  buys one brief saturation ramp per retry (live 3.6a at factor 2, just
  over capacity, breathed quality on a ~15 s period under timed retries).
  The trial survives only when the full 10-frame cadence has elapsed AND
  the last three recorded writes are flat or falling with every one inside
  the band; a window still rising at the cadence keeps the trial pending
  until it either stabilises (survival) or crosses the band (retreat), so
  no trial-length cap is needed. Survival keeps probing enabled,
  so the ladder may step sharper again after the cool-down. Judging by the
  band, not the escalation threshold, is what live 3.6a demanded: tmux's
  buffers absorb a failing sharper factor, so writes ramped 20 -> 107 ms
  across the cadence without ever reaching the 200 ms escalation threshold,
  the trial was judged survived, and the next, sharper probe tipped the
  accumulated queue (255-341 ms writes) and took the blame, hunting forever
  with a lag spike each cycle. The stability requirement is what a later
  3.6a run at the 300 ms floor demanded: the saturation ramp outlives the
  trial window, with writes climbing 30 -> 146 ms across the ten-frame
  cadence while staying inside the 180 ms band, so a cadence-only trial
  declared survival, the writes then climbed to 250-296 ms, the escalation
  backstop fired, and the ladder hunted on a cool-down cycle. A sharper probe also fires only when the
  last three recorded writes all sit inside the band with no monotonic
  rise: steady jitter (18, 20, 19 ms) passes, while a ramp (69, 83, 90 ms)
  means tmux is buffering content the pipe cannot sustain, so probing
  sharper mid-ramp would tip that queue. After a latency escalation the
  probe stays suppressed for the cool-down (40 budgeted frames, about ten
  seconds), because the freshly drained pipe writes tiny frames that would
  otherwise re-sharpen it within the normal cadence and flap the ladder.
  Full recovery belongs to the settle: the idle settle emit always shows
  the page sharp at factor 1, and the next episode resumes at the sustained
  pre-settle factor behind the same cool-down instead of re-running the
  discovery cascade (live 3.6a resumed at factor 1 after every settle and
  re-walked 1 -> 1.5 -> 2 -> 3 with 400-535 ms writes each time motion
  returned). The 200 ms two-write escalation streak stays unchanged as the
  hard backstop for content that gets heavier mid-episode; it never touches
  the probing state. Byte-budget escalations keep the plain cadence.
  The 256-colour palette is untouched. With `NYALA_DEBUG_TIMING=1` each
  frame reports its encoded byte size and active factor on stderr.
- Input-buffer raise. tmux 3.6 and later expose the `input-buffer-size`
  server option (3.5a and earlier do not, which is one reason the version
  gate refuses them Sixel entirely). When the XTVERSION reply names such a
  version, Nyala runs `tmux set -s input-buffer-size 16777216` once at
  startup; a successful set carries the version-tuned budget (15 MiB on
  3.7+, 1.5 MB on 3.6.x, where the raise still protects ladder-floor frames
  that overshoot the cap), and a failure keeps the 900 KB fallback. When the
  version gate fails, no set runs at all: the tmux server is not mutated for
  a session that will never send Sixel. The option is not restored on exit:
  it persists for the tmux server lifetime.
- Idle settle-to-sharp. When the Sixel stream goes idle for two effective
  pacing intervals and the last emitted frame was blurred (ladder factor above 1),
  the viewer re-encodes that frame at factor 1 and emits it once, so a
  settled page always shows crisp text. The settle emit counts as a normal
  paced handoff, ends the motion episode (the next episode resumes at the
  remembered pre-settle factor with probing re-enabled), and disarms until
  a new budgeted frame goes out.
- No per-frame erase. A steady tmux Sixel frame carries the bare cursor-home
  prefix, the same as native: each new full-frame image paints over the old
  one with no blank state in between. tmux stores images on the active screen
  list, where LRU eviction (capped at 20 images on 3.6b and later) is
  correct, so the stacked store stays bounded without an erase. Live testing
  on tmux 3.7b showed an earlier home-plus-erase prefix was the flicker
  source: the erase painted a visible blank between frames, so animation
  strobed draw-hold-clear-pause and ordinary page scrolling blanked before
  each paint. First-draw, resize, and backend-switch clears are unchanged.
- DEC 2026 suppressed on every version. tmux does not defer image operations
  under an application synchronised update, so the bracket cannot hide an
  erase or make an image swap atomic (live-verified on tmux 3.7b). Sixel
  frames under tmux carry no DEC 2026 markers on any version. Block under
  tmux keeps the normal fence.
- Emit pacing. tmux re-encodes stored images for its host terminal, and live
  testing on tmux 3.7b showed sustained Sixel at ten frames per second kills
  the pane's image rendering permanently, so Sixel frames are paced by a
  version-tuned floor: 250 ms on tmux 3.7 and later, 300 ms below 3.7 or
  when the version is unknown. The 250 ms value was bisected live on 3.7b
  with ~4 MB full-quality frames, which tmux consumes in about 230 ms each.
  A large frame leaves Nyala only as fast as tmux reads it from the pty, so
  the frame write duration is a direct saturation signal: at 200 ms pacing
  the pty write blocked for the full consumption time, a downstream backlog
  built, and the display lagged live content, while at 250 ms writes stay at
  ~33 ms and tmux drains fully between frames. The write signal is blind to
  one stage, though: live tmux 3.6a at a settled factor kept its pty writes
  fully drained (~20 ms) at the 250 ms floor yet showed periodic display
  stalls followed by a rapid release of several frames, so the backlog sat
  in 3.6a's output scheduler toward the host terminal, past the pty. The
  same content at 300 ms showed no bursts, so older and unknown tmux pay the
  wider floor. The escalation threshold, probe band, and settle delay all
  derive from the resolved floor. Because the consumption time varies with
  machine, grid, and content, the pacing backs off adaptively above the
  floor: the next interval is the last measured write duration plus a 50 ms
  margin whenever that exceeds the floor, capped at 1 s so one pathological
  write cannot freeze the stream. The pacing sits at the frame-coalescing
  seam: held-back frames are replaced latest-wins, so no queue grows. With
  `NYALA_DEBUG_TIMING=1` each write reports its duration and the next
  effective interval on stderr.

GNU screen blocks graphics passthrough, and herdr consumes graphics sequences
and reports no cell geometry, so both stay Block only with no handshake.
herdr's experimental Kitty-only support is deferred.
