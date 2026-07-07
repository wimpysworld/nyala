# Nyala TODO

Open work after Stage 3, in priority order. Completed items are recorded in the
working docs: block render, graphics, and GPU performance in `PERFORMANCE.md`, the
sextant spike in `BLOCK-MODES.md`, and the block text-placement, alt/aria walk, and
opaque-overlay fixes in `STAGE-3.md`.

## 1. Make kitty and sixel usable

Not done. The graphics backends need a fit fix and a graphics performance fix, both
eyeballed on a real graphics terminal, not gated behind the capture harness. The path
sits on a black screen for a long time, scrolls slowly, and tears with big full-frame
refreshes. Scope and measure each win first, then work the engine-independent levers:
encoder, downscale, snapshot cadence, frame coalescing. Reopen CEF only with evidence
that a remaining limit is Chromium's, not our code.

### Open graphics work

- Fit fix: kitty and sixel draw into a small area of the terminal and letterbox
  heavily. Size and fit the bitmap to use the available width and height. The remaining
  Kitty follow-up is the multiplexer path, virtual placement `U=1` plus passthrough,
  gated off today; the bulk f=24 encode is done. See PERFORMANCE.md rec 5.
- Sixel encoder (recommendations 6 and 8, untouched): the per-frame quantise dominates
  the frame budget (~593ms measured in Stage 3, our code not Chromium). A cached
  palette plus dirty-band diffing removes it from steady-state frames, with the interim
  wins (persist the encoder, buffer the writer, swap the quantiser). See
  PERFORMANCE.md.
- PNG decode (Option 2, NOT started): the klauspost `compress/zlib` swap buys ~7-21%,
  but PNG decode only lands on the graphics idle frame and the capture corpus, off the
  hot JPEG motion path. Deferred until it profiles hot; see PERFORMANCE.md "Decode
  Optimization" Option 2 for the two routes (woozymasta/png drop-in, or a one-import
  fork of image/png).
- BACK-POCKET (measure first, do not build yet): the block-live JPEG quality knob.
  Lower q trims the Huffman decode only; the IDCT is quality-independent in the
  decoder, so the win is bounded and near-free on block (washes out in the sub-cell
  average). Size it with a hardware `in_decode` split before committing.
- DEFERRED TO LAST (owner's call): downscale the graphics bitmap in motion. It is the
  paint lever for media 60fps (Out ~24 against In 60, Kitty encode on the large live
  surface), but the owner wants every other perf option exhausted first.

### Open block lever: sub-cell screencast resolution [ ON HOLD ]

Rec 1: request the block screencast at sub-cell resolution (cols*2 by rows*4) and
delete the downscale at source, so the engine emits the exact bitmap Compose samples
and `renderBlock` drops `DownscaleLiveInto`. It is the highest-payoff lever left (it
deletes the 3-14 ms decode/downscale stage rather than speeding it), but ON HOLD by
owner decision, not never. WHY HELD: making Nyala's decode trivially cheap (a) masks
potential future optimisations, since a shrunk input removes the headroom those wins
would show, and (b) hides the performance gap between terminal emulators, since the
current heavy end-to-end pipeline is what stresses terminals and reveals their
throughput ceiling. The full-resolution path stays as the discriminating benchmark.
Revisit when the owner decides otherwise. See PERFORMANCE.md rec 1.

## 2. Stage 4: the UI

Build the browser UI on a correct, fast-enough foundation. Defer until item 1 lands,
so the UI is not built on a letterboxed view or poor graphics performance. First
interactivity target: clicking links and buttons for basic web use, mapping a terminal
click or key to a CDP input event at the DOM box.

## Follow-ups from the text-clarity work

The block backend hides painted text in the raster (separate-layers model), so the
bitmap is graphics only and the crisp overlay is the sole text layer. The horizontal
skew was a letterbox in the block compose, fixed by composing onto the full grid. Four
follow-ups remain.

### Restore an overlay-to-bitmap alignment gate

`paint-displacement` scores the overlay against painted ink. With page text
hidden, there is no ink to measure, so it reports high skip fractions and is
effectively dead. The `placement-score` gate compares the overlay to its own DOM
box, so it is self-consistent and cannot catch a text-versus-bitmap scale drift
like the letterbox bug that shipped. The block full-grid compose has a unit-test
guard (`TestRenderBlockComposesFullGridNoLetterbox`), but there is no end-to-end
gate that ties the overlay to the bitmap. Restore one that works with hidden
text: measure a known non-text bitmap feature against the overlay, or re-enable a
text-bearing raster path for the metric alone.

### Widen the sextant allow-list

The allow-list is name-only and conservative: it maps known terminals to
supported or not from environment variables, excludes iTerm2 (3.5.x seam), and
gives any unknown terminal quadrant. Sextant is better where it renders cleanly,
so grow the list as each terminal is eyeballed for inter-row bleed. Scope for the
mini-project: a repeatable verification step (a sextant test card, like
`just kmscon-testcard`), a per-terminal record of what was checked and on which
version, and the allow-list edits that follow. Open question carried over:
whether to add version gating so a good terminal with a bad release (iTerm2
3.5.x) can be split by version rather than excluded whole.

### In-app fallback toast

When block mode falls back (a sextant request on an unsupported terminal degrades
to quadrant), tell the user in the terminal. Keep it app-driven via Bubble Tea: a
transient status line the viewer draws and then clears, no OSC 9 / OSC 99 desktop
notification and no terminal-specific escape. Small, separate from the sextant
render work.

### Make the CJK ruby wide leg deterministic

`layout-03-cjk-vertical-ruby` at 240x48 reflows to variable states under corpus
load: box count and collision count shift run to run (isolated runs settle near
2, loaded runs reach 6 to 7). The collision threshold is pinned at 8 as a
workaround. The capture settle hold waits for the first `networkAlmostIdle`
reflow, which does not catch this later CJK shaping reflow. A stronger settle
signal (wait for a layout that stays stable across snapshots) would make the wide
leg deterministic and let the threshold tighten back to 2.
