# Nyala Performance

This is the durable superset of three former working docs: the block and graphics
performance research (was `PERF-RESEARCH.md`), the GPU acceleration research (was
`docs/GPU-ACCEL-RESEARCH.md`, folded in as the "GPU acceleration for headless
Chromium" section below), and the JPEG decoder research (was
`docs/JPEG-DECODER-RESEARCH.md`, folded in as the "JPEG decoder research" section
below). Those three files are superseded by this document.

Research depth: Thorough. Method: 15 sub-agents. Five studied the reference
projects (browsh, brow6el, casty, carbonyl, carboxyl), five hunted bottlenecks in
Nyala's engine and render paths, five researched Sixel/Kitty protocol and Go
libraries. Every claim below traces to a Nyala `file:line` or an external source
listed at the end.

## Summary

Nyala's three reported problems each have a clear, code-level root cause.

1. **Black screen at load** is not the browser being slow. The
   `firstContentfulPaint` gate that would show text early is **capture-only and
   dead in production**, so the background paints fast but text waits for
   `networkAlmostIdle` plus a 250 ms debounce, trailing paint by seconds.
2. **Graphics letterbox** is one wrong call: the graphics bitmap is already sized
   to the full grid, then run through a 1:2-aspect `FitImage` that shrinks it.
3. **Slow, tearing graphics** come from a full re-encode and full re-transmit every
   frame, an unbuffered writer (~1800 syscalls/frame), no synchronised-output
   fencing, and, for Sixel, a per-frame palette rebuild that dominates the
   ~593 ms cost.

The reference projects agree on three durable ideas: coalesce frames, reuse buffers,
and avoid redundant terminal bytes. Nyala now passes each live Kitty CDP PNG straight
to terminal framing, so the old transmit-once animation-frame plan does not apply.
Sixel still benefits from a cached palette, tighter output, and measured dirty bands.
Partial Kitty transport remains conditional on terminal measurements and a supported
protocol path; see
[`TODO.md`](./TODO.md#2-improve-graphics-transport-where-measurements-justify-it).

CAVEAT: the per-frame magnitudes are allocation-count and round-trip reasoning from
source, not a profiler run. One measured capture (the WebGL aquarium, 100 frames,
block backend, 120x40) is now folded into Themes 1, 2, and 7 and tagged MEASURED; a
full `pprof` and `go test -bench` pass is still pending and is indexed in
[`TODO.md`](./TODO.md#1-validate-the-stage-3-browser).

---

## Findings

### Theme 1 - Load time (the black screen at load)

KEY. The `firstContentfulPaint` path is observed then discarded in production.
`EnableCapture` is the only writer of `e.painted` (`internal/engine/chromium.go:223`);
`markPainted` returns at once when `painted` is nil
(`internal/engine/chromium.go:287-292`), and `watchPaint`'s FCP arm calls only
`markPainted` (`internal/engine/recovery.go:300-303`). So in a normal run the
target page's first paint fires no text snapshot. Text lands only when
`networkAlmostIdle` or a layout-shift binding pokes a reflow
(`internal/engine/recovery.go:304-311`), funnelled through the 250 ms
`holdStalenessLimit` debounce (`internal/engine/sync.go:19`,
`internal/engine/screencast.go:475`). On a heavy page `networkAlmostIdle` trails
first paint by seconds. This is the block-specific "background but no text".

Secondary load costs:
- `document.fonts.ready` is awaited before the first text publish and before every
  reflow re-snapshot (`internal/engine/calibrate.go:85`,
  `internal/engine/screencast.go:387-397,550-553`). It does not block the first
  bitmap, but it serialises ahead of every text update, bounded by a 2 s timeout
  (`calibrate.go:18`). A slow page web-font can hold the overlay.
- ~9 serial CDP round-trips in `launch` before the screencast starts
  (`internal/engine/launch.go:304-334`), including three separate `Run` calls for
  `enableLayoutShift` (`internal/engine/forcing.go:93-114`). Individually cheap
  locally, but they stack before any frame flows.
- No production load-path timing exists; `FrameStats` is populated only under
  capture (`internal/engine/screencast.go:865-872`).

MEASURED (aquarium, 100 frames): first text published at 2931 ms against first frame
at 272 ms, a 2.7 s text lag on a heavy page. This is the capture path, so it isolates
the snapshot cost itself (`fonts.ready` plus the text walk), which the live FCP fix
(recommendation 1) cannot shorten. It points straight at recommendation 13 (pre-warm
the embedded font, narrow the `fonts.ready` wait).

### Theme 2 - Viewer draw loop

The consume loop is already sound: `drainFrames` coalesces bursts and draws only
the newest (`internal/viewer/stream.go:352-387`), the engine channel is buffered
and back-pressures the screencast to draw speed (`internal/engine/frame.go:22`,
`internal/engine/screencast.go:786-789`), and `GridDiff` emits only changed cells
(`internal/render/block/diff.go:53`). Leave that design alone. The costs are in
what it writes and how:

- KEY (tearing). No synchronised output (DEC 2026) anywhere, and the tty writer
  is the default 4 KB `bufio` (`internal/viewer/viewer.go:141`). A full or scroll
  frame is ~150 KB, so it auto-flushes many times mid-write, putting partial frames
  on screen. Wrap each frame in `ESC[?2026h` … `ESC[?2026l` and size the buffer to
  one whole frame so it flushes once.
- KEY (slow scroll). Scroll keys route through a latest-wins-drop coalescer
  (`internal/viewer/stream.go:210-215,514-519`). Latest-wins is right for frames,
  wrong for cumulative scroll deltas: a burst of `ArrowDown` collapses to one step.
  Accumulate the deltas into one `SendScroll(sum)` (`stream.go:69`).
- Per-cell SGR built with `fmt.Sprintf` (`internal/render/block/colour.go:66-74`),
  called for every emitted cell in the differ (`internal/render/block/diff.go:113-124`),
  ~10k `Sprintf` per scroll frame. Compare `color.RGBA` and build with
  `strconv.AppendInt` (as `sgr256` already does, `internal/viewer/viewer.go:496-502`).
- Per-cell `string(c.Glyph)` allocation (`internal/render/block/diff.go:137`,
  `block.go:162`); use `utf8.AppendRune` into the byte buffer.

MEASURED (aquarium, 100 frames): a full-motion frame wrote ~183 KB to the terminal.
The whole scene changes, so `GridDiff` emits nearly all 4800 cells, which is ~10k
per-cell `fmt.Sprintf` SGR builds per frame - direct support for the alloc wins
above. `terminal_write_ms` stayed ~0.4 ms, so the DEC 2026 one-flush buffer holds
under motion.

LANDED (beyond the ranked list, d761368): a screencast-listener leak found while
tracing Tab backend cycling. `startScreencast` registered a fresh chromedp
`ListenTarget` handler on every call, and `restartStream` re-issues it on every
backend switch and resize, so listeners piled up and each frame decoded and acked
N+1 times. The listener now registers once per session. Tab cycling itself was
correct; the perceived "Tab does nothing" traced to a forced `--backend` (which pins
the cycle) or the kitty-to-sixel step looking identical.

### Theme 3 - Kitty renderer and the fit bug

- KEY (the letterbox / item 6 "best fit"). `RenderStreamFrame` runs the graphics
  bitmap through `block.FitImage` (`internal/viewer/viewer.go:341`, `:226-227`),
  which assumes a 1:2 cell aspect (`internal/render/block/fit.go:6-9`). But the
  engine already sizes that bitmap to `cols*cellW x rows*cellH`
  (`internal/engine/launch.go:73-78`, `internal/render/geometry.go:42-44`), so it
  maps one-to-one onto the grid. On any non-1:2 cell (e.g. 16x33) `FitImage`
  shrinks the columns and centres, letterboxing. Fix: give the graphics path an
  identity fit (`OffsetCol=0, OffsetRow=0, FitCols=Cols, FitRows=Rows`) or drop the
  fit call for graphics.
- Per-pixel encode: `enc.Encode` loops `m.At(x,y).RGBA()` with a fresh 4-byte slice
  write per pixel (charmbracelet `kitty/encoder.go:42-52`), ~960k interface calls
  and allocs for a 1200x800 bitmap. `f.Img` is `*image.RGBA`
  (`internal/render/render.go:130`); bulk-write `img.Pix` row-by-row into the zlib
  writer instead. Byte-identical, zero per-pixel allocs.
- KEY (flash/tear). The old Kitty path deleted each image before transmitting its
  replacement, which exposed a blank frame. The shipped direct-CDP-PNG path alternates
  two image IDs: transmit and place the next image, then delete the previous image.
  The tested animation-frame edit (`a=f`) was not portable and is rejected (Theme 5).
- Graphics escapes are written unbuffered to the raw tty in many syscalls
  (`internal/viewer/viewer.go:404-413`, chunked at 4096 in `kitty.go:103-118`).
- Per-frame allocations: `bytes.Buffer`, payload slice, a new `zlib.NewWriter` each
  frame (`kitty.go:77-88`, `kitty/encoder.go:30`). Pool them.

### Theme 4 - Sixel renderer (~593 ms/frame)

The live Sixel path uses `charmbracelet/x/ansi/sixel` (`internal/render/sixel/sixel.go:14,47`).
KEY: **quantisation dominates, and it is rebuilt from scratch every frame.**
`newSixelPalette` hashes every pixel into a `map[sixelColor]uint64`, median-cuts
all unique colours, then runs an O(uniqueColours x 256) nearest-colour double loop
(charmbracelet `sixel/palette.go:295-345`). The image is read through the
`image.At` interface twice, and a bit index up to
`bandHeight*width*6*paletteSize` allocates a ~30 MB sparse bitset per frame
(`sixel/encoder.go:138,169`). Everything is allocated fresh per `Encode` and a new
`&sixel.Renderer{}` is created per frame (`internal/viewer/viewer.go:237`),
blocking scratch reuse. Charm's own benchmark puts both mainstream Go encoders near
300 ms/op on one image because both use `soniakeys/quant`.

Two extra Nyala-specific costs:
- The graphics writer is unbuffered, so the ~256-entry palette plus body land as
  ~1800 small `write` syscalls per frame (block path buffers via
  `internal/viewer/viewer.go:383`; graphics does not).
- No delta at all: full image re-encoded and re-blitted every frame, the direct
  cause of slow scroll and tearing on the Sixel path.

KEY (strategy). Sixel is structurally unfit for full-screen motion. notcurses'
maintainer calls its own Sixel "unacceptable for even double-digit FPS"; mpv caps
Sixel at 320x240. The limiter is the serial, input-blocking terminal decode of a
large payload. Treat Sixel as the SSH/tmux compatibility fallback at modest size
and rate; make Kitty and block the primary motion paths.

### Theme 5 - What the reference projects teach

The fast terminal browsers do not use graphics protocols at all. **Carbonyl and
browsh render blocks with cell diffing**; only brow6el (Sixel + Kitty) and casty
(Kitty) ship graphics-protocol browser code. The shared, transferable lessons:

- **Damage tracking end to end.** Carbonyl reads the compositor's software
  framebuffer from shared memory *with a damage rect attached* and re-samples only
  changed cells (`host_display_client.cc`, `renderer.rs draw_background`). brow6el
  maps CEF dirty rects to cell-and-band-aligned Sixel tiles and re-encodes only
  those (`sixel_renderer.cpp:31-54,128-167`). notcurses' TAM wipes single cells
  rather than re-emitting the bitmap. Nyala's CDP screencast carries no damage rect.
  Sixel can recover bands from decoded pixels. Direct Kitty PNG has no normal decoded
  bitmap, so any partial Kitty path must preserve pass-through or prove that decoding
  pays back end to end. This work is gated on real terminal bytes and latency.
- **Cell double-buffer diff + SGR/cursor elision.** Carbonyl and carboxyl keep
  `(prev, cur)` grids, skip unchanged cells, move the cursor only on a
  discontinuity, and cache the last-emitted fg/bg SGR (`painter.rs`). Nyala has the
  grid diff already; it lacks the SGR-string elision (Theme 2).
- **Frame coalescing on a fixed-FPS deadline.** Carbonyl drains all pending updates
  until a `1/fps` deadline and renders once, blocking at 0% CPU when idle
  (`render_thread.rs`, `frame_sync.rs`). Nyala's `drainFrames` already does the
  coalescing half.
- **Lossless where pixels must stay exact.** Carbonyl requests PNG so JPEG ringing
  does not create phantom cell changes that defeat a frame diff. Nyala now uses PNG
  for Kitty, settled Sixel, and every capture. Block uses JPEG quality 80 because its
  quantised output does not preserve source pixels for a lossless diff.
- **Kitty specifics from brow6el:** double-buffered alternating image IDs for
  flicker-free swap, `f=24` RGB to drop alpha (~25% fewer bytes), 16 KB chunks,
  `z=-1` to layer the page under terminal text, `Z_BEST_SPEED` zlib.
- **Off-screen super-viewport (browsh).** browsh serialises a region 6x the viewport
  so ordinary scrolls redraw locally with no browser round-trip
  (`dimensions.js`, `commands_mixin.js`). Nyala round-trips CDP on scroll; a buffered
  band above and below the viewport would cut scroll latency toward zero.
- **Adaptive quality (casty).** Fast/lossy encode while the page moves, one lossless
  refinement ~500 ms after it settles. Applies to Nyala's graphics backends.

### Theme 6 - Compose / overlay hot path

The engine-side `composeFrame` zero-scroll fast path is clean: it aliases the run
slice with no copy (`internal/engine/screencast.go:839`,
`shiftRunsToBitmapScroll` returns untouched on `dy==0`, `:894`). The costs are
downstream in the block renderer, and they matter only after the load-time and
graphics wins:
- `quadrantColours` and its sextant twin heap-allocate two `[]color.RGBA` per
  non-flat cell (`internal/render/block/block.go:411`, `sextant.go:128`) - the
  largest GC pressure on the compose path. Accumulate channel sums in fixed-size
  stack arrays instead.
- Grapheme runs are re-segmented every frame and twice per run
  (`internal/render/block/overlay.go:41,139`), though runs change only at snapshot
  time. Cache per-run display width at decode (`internal/engine/walk.go:288`).
- The whole grid is re-binarised every frame even when the bitmap is byte-identical
  (`block.go:233`, `viewer.go:376`). Memoise the composed grid when `Frame.Img` and
  the runs slice are both unchanged (static page, repeated scroll frame).

### Theme 7 - Screencast image format

The wire format now follows the backend. Live Block requests JPEG quality 80. The
block path needs pixels, and its sub-cell downsample plus colour quantisation hide
JPEG artefacts. Live Kitty always requests PNG and passes Chromium's owned base64
payload straight to Kitty `f=100,t=d,a=T` framing. Live Sixel keeps the existing
adaptive policy: decoded PNG while settled, decoded JPEG quality 90 while moving,
then PNG after settle. Diagnostic capture remains lossless PNG on every backend.

The frame seam no longer expands every decode to a full RGBA plane. `Frame.Img` is
`image.Image`: JPEG stays as the decoder's `*image.YCbCr`, and PNG keeps its native
decoder layout. Block converts while producing its small RGBA sample surface. Sixel
converts at the encoder boundary. Capture converts where its raster and bundle paths
require RGBA. Live Kitty carries no decoded image on the direct path; `EncodedPNG`
holds the CDP string. A backend switch decodes that retained PNG if the queued frame
reaches Block or Sixel.

The focused benchmark tiled the same source to 2560x2880, ran ten iterations per
sample, and reports the median of five samples. The real-CDP benchmark used one
2560x2880 Chromium screencast payload. Timings cover Nyala's frame processing and
Kitty framing only. They stop before the terminal decodes or draws the image.

| Path | Before | After | Result |
|---|---:|---:|---|
| Focused Kitty PNG | 51.950 ms | 0.014 ms | Decode and re-encode removed |
| Real CDP Kitty PNG | 45.985 ms | 0.027 ms | Direct payload framing |
| Focused shared JPEG | 25.446 ms | 10.209 ms | 15.237 ms saved |
| Kitty terminal bytes | JPEG q90: about 3.25 MB | PNG: 1.68 MB | PNG was smaller for this page |

The target is 16.67 ms per frame for 60 FPS. Direct PNG clears Nyala's side of that
budget, and decoder-native JPEG saves about 15.2 ms. The next measurement must cover
terminal-side PNG decode and draw latency plus sustained real-terminal FPS. A smaller
Nyala framing number does not prove a higher terminal frame rate.

---

## Sixel/Kitty library verdict

**Kitty.** Live frames now bypass the decoded encoder. Nyala rechunks Chromium's
owned base64 PNG as `f=100,t=d,a=T`, alternates image IDs, and deletes the previous
ID after placement. The earlier `a=f,r=1,o=z` edit was tried and reverted because it
rendered partially or black on target terminals. Keep the decoded RGB encoder for
tests and a measured fallback, not as the normal optimisation path. Any future
partial update must use a supported protocol path and beat direct PNG end to end.

**Sixel.** CONFLICT across agents, resolved by the benchmark evidence:
- Two agents that read the source say swap the live encoder to `mattn/go-sixel`
  (already a dep, used only for decode at `internal/capture/decode.go:13,38`): it
  has an `*image.RGBA` fast path, subsamples before quantising, a 15-bit
  RGB->index LUT instead of the O(C x 256) loop, direct `Pix` access, and reused
  scratch buffers.
- One agent says do not swap: it is comparable and older, so vendor and extend
  charmbracelet instead.
- The library-benchmark agent settles it: **quantisation dominates, and both
  encoders use the slow `soniakeys/quant`**, so the encoder swap is a partial win at
  best. The durable win is to stop re-quantising every frame.

Resulting Sixel plan, in order of payoff:
1. **Cache the palette across frames; re-quantise only on scene change** (notcurses'
   model). With Nyala's separate-layers design the Sixel bitmap carries only
   backgrounds/images/borders and changes slowly, so a cached palette plus
   dirty-band diffing removes per-frame quantisation from the steady state
   entirely. This is the biggest win and needs vendoring the encoder to own its
   state.
2. **Replace `soniakeys/quant` with `carbocation/go-quantize`** (~23x faster on the
   same image, `draw.Quantizer`-compatible) for the frames that must re-quantise.
3. **Interim quick win:** persist a `mattn/go-sixel` `*Encoder` on the renderer
   (reuse scratch) and buffer the writer, before the caching engine lands. Drop
   Nyala's hand-rolled DCS envelope if you do, since mattn emits its own.
4. **Emit tighter output:** aggressive RLE (`!count`), drop empty bands, set `P2=0`
   when opaque, dither off. This cuts both encode time and the terminal's decode
   CPU (measured 10x decode-cost swings from RLE quality).
5. **Parallelise band encoding** across goroutines only after 1-4, with per-worker
   buffers (naive threading made notcurses slower).

Do **not** take a cgo dependency on libsixel; a pure-Go stack of cached palette +
`go-quantize` + tight emission reaches the same tens-of-ms class and keeps the
cgo-free build. `mattn/go-sixel` stays as the capture decoder regardless (not dead
weight). `BourgeoisBear/rasterm` is a clean Go reference but is paletted-input-only,
so it pushes quantisation back onto the caller.

---

## Top recommendations (ranked by impact vs effort)

Status legend: **[DONE]** landed with the commit noted, **[IN PROGRESS]** partially
landed, unmarked items are open.

### Tier 1 - quick and high impact [ ALL DONE ]

1. **[DONE - 67936b1]** **Poke a re-snapshot on `firstContentfulPaint` in
   production.** Kills the black-screen-at-load text delay: text at paint instead of
   at `networkAlmostIdle`+250 ms. One arm in `watchPaint`
   (`internal/engine/recovery.go:300-305`). Impact 9, effort low.
2. **[DONE - 67936b1]** **Remove the graphics letterbox** with an identity fit for
   the graphics path (`internal/viewer/viewer.go:341`, guarded by
   `graphics_fit_test.go`). Fixes item 6 "best fit". Impact 8, effort small.
3. **[DONE - 67936b1, cb5aaf3]** **Synchronised output + one-flush buffer.** Wrap
   each frame in DEC 2026 and size the tty buffer to a whole frame; buffer the
   graphics writer too. Kills tearing on full refresh and scroll, cuts syscalls from
   dozens/~1800 to one. Impact 8, effort small.
4. **[DONE - cb5aaf3]** **Add load-path and frame timing behind a debug flag**
   (spawn-return, screencast-start, first-frame, first-text). `NYALA_DEBUG_TIMING`
   plus the `just debug-run` recipe and the capture bundle's `load_timings`. Impact
   medium (enables the rest), effort low.
4b. **[DONE - 67936b1]** **Add the lossless PNG screencast path.** PNG remains the
   capture format, the settled Sixel format, and the direct Kitty format. The later
   backend policy keeps JPEG quality 80 for Block and quality 90 for moving Sixel
   frames. See Theme 7.

Also landed on the load-time theme, beyond the ranked list:
- **[DONE - 8d288d5]** Publish the overlay at first paint with near-calibrated text,
  then recalibrate and re-publish. Calibrate only the forced FiraCode face rather
  than `document.fonts.ready`, so an in-flight page web font no longer stalls the
  calibration base (this is part of recommendation 13). Memoise `getComputedStyle`
  per element in the walk.
- **[DONE - 29c0bd0]** Measure the DOM text walk per word, not per code unit, cutting
  `dom_snapshot_ms` by roughly 40-85% on dense pages (BBC 228->31 ms, Wikipedia
  451->259 ms). This is the text-walk half of the measured 2.7 s text lag in Theme 1.

### Tier 2 - moderate effort, high value

5. **[PARTIAL - ee99ab7]** **Kitty flash fix + transmit optimise.** The per-frame
   flash is fixed: the `a=d,d=A` delete-all is replaced by a double-buffered
   alternating-id swap (place the next id, then free the previous), so the screen is
   never blank. Verified by eye on kitty and WezTerm. Note: the `a=f,r=1,o=z`
   animation-frame edit from the original plan proved patchily supported (partial-then
   -black on kitty, black on WezTerm), so it was tried and reverted; the double-buffer
   is the durable path. MEASURED (bbc, kitty): our Kitty encode was `out_encode_mean_ms
   ~191` while the tty write is ~8 ms, so the escape build was the paint wall, not the
   terminal (see the FPS instrument above). [DONE - staged] The bulk `f=24` RGB encode
   landed: a reused `kitty.LiveEncoder` reads `img.Pix` in bulk (no per-pixel `At()`),
   packs 24-bit RGB (drops the opaque alpha, 25% fewer bytes), zlib BestSpeed into
   reused buffers, and assembles chunks with no per-chunk copy. Headless benchmark on a
   1920x1080 frame: **67.7 ms -> 12.2 ms (82% cut, 5.5x)**, allocs 4,147,504 -> 13,
   payload 11% smaller. The stateless capture `Renderer` is untouched, so corpus stays
   byte-identical. Owner verifies the visual on kitty hardware (the f=24 switch assumes
   the bitmap is fully opaque). [DONE - perf] The live encoder's zlib swapped stdlib
   `compress/zlib` for `github.com/klauspost/compress/zlib` at BestSpeed (level
   unchanged, already a dependency via woozymasta, now direct). Live-path only: the
   capture `Renderer` (`kitty.go`) stays on stdlib zlib because the corpus and golden
   checks need byte-exact output, and klauspost emits different (valid, non-byte-
   identical) compressed bytes. The output is a valid zlib stream, round-trip through
   stdlib confirmed, so terminals decode it unchanged. Motivation, real hardware: the
   Aquarium WebGL benchmark on Kitty at 2568x2880 measured the paint path as the wall,
   `out_encode_mean_ms 164.6` plus `out_write_mean_ms 58.2` at ~4.5 out fps, the encode
   dominated by zlib of a ~22 MB f=24 RGB frame. Measured on that same fixture, before
   and after the swap on Kitty hardware: encode `164.6 -> 115.3 ms` (a 30% cut), write
   `58.2 -> 57.5 ms` (flat; the ~4.7% larger payload cost nothing measurable), paint
   `4.5 -> 5.8` out fps (+29%). Encode-plus-write fell `222.8 -> 172.8 ms` per frame, a
   22% cut, beating the earlier modelled ~18%. Rejected: klauspost HuffmanOnly was ~38%
   off encode-plus-write on this incompressible frame, but it disables LZ matching, so it
   inflates flat/text pages and shifts cost to the write; wrong default for a
   general-purpose browser, so BestSpeed. This cuts the paint wall but does not remove
   it: the encode stays the wall at 115 ms of the 172 ms budget, the write now about a
   third (57 ms). At that point damage/dirty-rect and motion downscaling were candidate
   levers. Direct PNG later removed the decoded Kitty encode path, and graphics
   downscaling is now outside the active roadmap. [DONE - measured] The draw loop now pipelines the
   escape encode with the tty write. A producer goroutine (the draw loop) assembles the
   whole mode-2026 frame (sync-open, prefix, render, fps badge, sync-close) into one of
   two ping-pong buffers, and a single writer goroutine does the `tty` write, so the write
   of frame N overlaps the encode of frame N+1. Strict frame order holds (single writer,
   cap-1 handoff), and the latest-wins frame-drop policy survives the handoff, so a slow
   writer drops stale frames rather than queueing; teardown is clean with no goroutine
   leak, verified `-race` clean. It is backend-agnostic: block rides it too with
   byte-identical output (the handoff moves bytes, not their content, and the golden track
   validates the bytes), while the `Capture` driver stays on the serial path so capture
   bundles stay deterministic. The paint wall drops from `encode + write` to
   `max(encode, write)`. MEASURED on the Aquarium fixture above (Kitty, 2568x2880): out
   fps `5.8 -> 8.5` (+47%). The overlap is confirmed: `out_encode_mean_ms 114.8` and
   `out_write_mean_ms 116.1`, and 8.5 fps is 117.6 ms/frame, which is `max(encode, write)`
   not the sum; serial would be 231 ms, 4.3 fps. Nuance on the write figure: `out_write`
   rose ~57 -> ~116 ms, and this is backpressure, not a regression. Serial, the ~57 ms
   only measured filling the kernel buffer and the terminal drained the bytes later behind
   the next encode; with the writer pushing continuously its `Write` now blocks until the
   terminal accepts the frame, so `out_write` now measures the terminal's true ingest rate
   (~116 ms to decode and paint the large Kitty graphic). The pipeline exposed a
   terminal-side limit that was always present. Two co-equal walls now bound paint: our
   encode CPU ~115 ms (zlib of the 22 MB frame) and the terminal ingest ~116 ms. They are
   balanced, so more speed on this full-motion fixture needs fewer pixels or fewer bytes,
   which cuts both walls. This result predates direct PNG. Graphics downscaling is now
   outside the active roadmap, while partial emission remains conditional and gives
   little on full motion because the whole frame changes each tick.
   Cumulative aquarium arc this session, all lossless and no downscale: 4.5 (stdlib zlib)
   -> 5.8 (klauspost) -> 8.5 fps (pipeline).
   [DONE - current] Live Kitty now bypasses this decoded `f=24` path: every Kitty
   stream requests PNG and rechunks the CDP base64 payload as `f=100`. Keep the
   decoded encoder for explicit fallback and tests. REMAINING: measure terminal PNG
   decode and draw latency, sustained Kitty FPS, and Kitty-through-multiplexer
   placement. Reconsider JPEG Kitty only if an end-to-end terminal benchmark beats
   direct PNG. Impact 8, effort medium.
6. **Sixel cached palette + dirty-band diffing.** Vendor and extend the charmbracelet
   encoder: hold the quantised palette, `memcmp` 6-px bands (or reuse scroll/CDP
   dirty info), re-emit only changed bands via cursor positioning. Removes the
   ~593 ms quantise from steady-state frames. Impact 8, effort medium-large.
7. **[DONE - 3861ccc]** **Accumulate scroll keys** instead of latest-wins-drop
   (`internal/viewer/stream.go`). `keySender` now accumulates every scroll key and
   forwards it in order, so the engine's `dispatchWheel` sums the deltas; a burst no
   longer collapses to one step (down, up, page, and horizontal, sign preserved).
   Direct fix for sluggish scroll. Impact 7, effort medium.
8. **Sixel interim wins** while 6 is built: persist the encoder, buffer the writer,
   swap `soniakeys/quant` -> `carbocation/go-quantize` or the encoder ->
   `mattn/go-sixel`. Impact 6, effort small-medium.
9. **[DONE - 9c091c6]** **Cheap viewer allocs:** drop per-cell `fmt.Sprintf` (build
   with `strconv.AppendInt` into reused buffers, byte-compare against a cache so run
   coalescing stays exact in every colour mode) and `string(glyph)` (use
   `utf8.AppendRune`). `BenchmarkGridDiff` on a full-motion 120x40 truecolour grid:
   14440 -> 40 allocs/op, 230861 -> 678 B/op, ~3.3x faster; output byte-identical
   (golden track green). Impact 6, effort low.
15. **[DONE - backend-specific] Adaptive screencast quality.** Request lossy (JPEG)
    while the page is in motion
    and a lossless (PNG) refinement when it settles - casty's model. The aquarium
    capture measured PNG at ~1.6 MB and 14-23 ms/frame on photographic full-motion
    content, where the damage-diff gives nothing; PNG stays best for typical and idle
    pages. MEASURED again (bbc, kitty, live): `in_decode_mean_ms ~34`, over the 16.6 ms
    60fps budget, so PNG decode was the ingest ceiling once the paint wall (5) cleared.
    The final policy is consumer-specific. Block uses JPEG quality 80 without a state
    machine. Sixel runs the SETTLED(PNG)/MOVING(JPEG-quality-90) machine, so static
    pages stay crisp and motion avoids the full PNG cost. Kitty is excluded from the
    state machine and always requests PNG for zero-decode direct framing. Capture
    forces PNG on every backend, so the corpus stays byte-identical. Onset is two
    short-gap (<200 ms) frames within 600 ms; settle is 400 ms idle. Impact 6 (motion
    pages), effort medium. See Theme 7.

### Tier 3 - larger and strategic

10. **Conditional partial graphics emission.** First measure terminal decode, draw,
    write, and bytes per frame. If full-frame cost exceeds 16.67 ms, add dirty-band
    Sixel from its decoded pixels. Direct Kitty PNG has no decoded bitmap, and the
    tested `a=f` edit was not portable, so do not decode every PNG to recreate the old
    plan. Design a Kitty path only from protocol support and an end-to-end benchmark.
    See [`TODO.md`](./TODO.md#2-improve-graphics-transport-where-measurements-justify-it).
    Impact conditional, effort large.
11. **[CLOSED - 1fd21a0]** **Compose hot-path cleanup.** Landed: scalar quadrant
    /sextant colour sums (~8-12% faster) and per-run display width cached at decode so
    the overlay stops re-segmenting graphemes each frame (~17% faster on overlay
    compose). Byte-identical (golden and overlay-golden tracks pass). Note: the
    per-cell `[]RGBA` "GC pressure" premise was wrong; Go 1.26 escape analysis already
    stack-allocated those slices, so the win is CPU time, not allocations. REJECTED:
    memoise the grid on unchanged `(Img, runs)`. The engine allocates a fresh
    `image.RGBA` almost every frame, so an `Img`-pointer cache would rarely hit, and
    pointer identity is not a safe content proxy (a reused buffer behind a stable
    pointer would show a stale frame). Near-zero benefit, real correctness trap.
    Closed; do not pursue.
12. **Off-screen super-viewport buffer** (browsh's 6x) to cut CDP round-trips on
    scroll. Track the implementation gate in
    [`TODO.md`](./TODO.md#2-improve-graphics-transport-where-measurements-justify-it).
    Impact 7, effort large.
13. **CLOSED (2026-07-07): font pre-warm done, launch-Run batching skipped.** The rec had
    two halves. Half B (pre-warm the embedded font so `fonts.ready` does not serialise text
    publish) is DONE and code-verified: calibration awaits only its own forced face via
    `fonts.load`, not `document.fonts.ready` (`calibrate.go:88`); no production text-publish
    path gates on `fonts.ready` (grep finds only comments and test-only awaits); and the font
    is a base64 `data:` URL `@font-face` injected at document creation
    (`stylesheet.go:66,74`, registered in `launch.go:266`). That was the valuable half.
    Half A (batch the ~9 serial launch CDP Runs, `launch.go:318,329,337,346,348`, plus the
    three `enableLayoutShift` Runs at `forcing.go:94,102,113`) is SKIPPED by decision, not
    parked. Two reasons. First, no user-visible win: the launch Runs finish before the first
    frame (`screencast_started` ~191 ms, dominated by Chromium's own launch), and Nyala drives
    a local headless Chromium, so each CDP round-trip is sub-millisecond; nine of them is a
    handful of ms no one sees. Second, batching is not what it sounds: `chromedp.Run(ctx,
    a, b, c)` runs actions serially, each awaiting its CDP response, so merging call sites cuts
    zero round-trips. A real cut needs the independent commands fired concurrently (an
    errgroup), which adds ordering risk (the font and layout-shift scripts must register before
    navigation, `runtime.Enable` before the live eval, device metrics before content size), for
    an invisible saving. Revisit only if a remote-CDP path (real network latency) ever lands.
14. **Position Sixel as the SSH/compat fallback**, Kitty and block as the motion
    paths. A framing decision, not code.

---

## Media browsing (GPU accel, hybrid screencast)

Owner side-quest, July 2026. Two ideas, one thread: make Nyala a media browser and
find the true screencast frame ceiling. Do the GPU work first. It is the measurement
gate: with the page rendering in software, the screencast ceiling stays hidden.

### Root cause: WebGL renders in software (corrected by research)

The first draft here blamed old `--headless`. Research corrected it (see the "GPU
acceleration for headless Chromium" section below, 18 sources). chromedp `v0.15.1` passes bare `--headless`,
which is already new headless on Chrome 132+, and sets no `--disable-gpu`. The real
cause: headless Chromium forces SwiftShader (software) for WebGL by default. The one
switch that turns that off is `--enable-gpu` (Chromium `headless/public/switches.h`).
Nyala never set it, so WebGL ran on SwiftShader (~26 fps aquarium).

### Idea 1 - GPU and video acceleration [DONE - staged, hardware-verified]

The five flags first guessed here (`--headless=new`, `--use-gl=angle`,
`--use-angle=vulkan`, `--ignore-gpu-blocklist`, `--enable-features=VaapiVideoDecoder`)
were training-data guesses: research scored them 0 verified, 3 rejected as
redundant/stale, 2 needing a hardware test. The owner flagged them, and the
evidence-based change is much smaller:

- `launch.go`: add `--enable-gpu`, on the live path only (capture stays on software
  SwiftShader so the corpus/golden pixels stay deterministic).
- `flake.nix`: add `/run/opengl-driver/lib` to the library path (wrapper plus dev
  shell) so Chromium finds the NixOS Mesa driver. Mirrors the jive-visualiser flake.

No Stage B (`--use-angle`), no Vulkan ICD, no video-decode flag: `--enable-gpu` alone
reached the GPU. Verified on hardware (AMD Radeon 8060S, radeonsi): WebGL Report shows
the AMD unmasked renderer with no performance caveat, and aquarium browser fps went
26 -> 60. The `NYALA_FPS_TIMING` badge read `In: 60.3 / Out: 24.6`, so the pipeline now
ingests the full 60 fps. The browser render, not our pipeline, had been the ingest
ceiling. Paint (Out ~24) is now the wall: the Kitty encode on the large live surface.
The klauspost zlib BestSpeed swap (recommendation 5) cuts that encode, measured 22%
off encode-plus-write, and the encode/write pipeline (recommendation 5) hides the write
behind the encode, measured `5.8 -> 8.5` out fps. Neither removes the wall: two co-equal
~115 ms limits then bounded paint, our encode CPU (zlib of the 22 MB frame) and the
terminal ingest. Direct PNG later removed Nyala's decoded Kitty encode. The next task
is to measure the terminal's PNG decode and draw; graphics downscaling is outside the
active roadmap.

### Idea 2 - PNG/JPEG hybrid screencast

Built for the pixel consumers. Block uses JPEG quality 80. Sixel switches to JPEG
quality 90 in motion and back to PNG after settle. Kitty always stays PNG because it
passes the CDP payload to the terminal without a Nyala decode. The remaining question
is end-to-end: measure whether terminal PNG decode and draw stays inside 16.67 ms, and
only restore a JPEG Kitty path if that measurement beats direct PNG.

### Audio (moved)

Audio is a built feature, not an fps lever. It lives in `docs/OVERVIEW.md`
(Architecture Decisions), as a live-path Chromium launch-flag behaviour.

### Companion: sustained-fps measure [DONE - 9002348 and the FPS instrument below]

Add a sustained screencast-fps number (frames delivered per second over a window).
Nyala has per-frame stats and load timings, but no sustained-FPS figure, and AGENTS
lists sustained FPS as a pending Stage 3 validation. This metric turns "feels smoother"
into a number and reads the ceiling idea 1 exposes.

Landed as two paths. Bundle metric: the bounded-capture path writes `sustained_fps`
and an auditable `frame_window` (`usable_frames`, `span_ms`) into `performance.json`,
beside `load_timings`, computed from monotonic wall-clock deltas over the drawn usable
frames. `sustained_fps = (usable_frames - 1) / (span_ms / 1000)`; n=1 emits 0 with a
zero span, matching the `load_timings` convention. Live counter: `NYALA_DEBUG_TIMING`
gates a rolling usable-fps badge the Stream path draws as a save/restore overlay after
the grid, so it never touches the golden-gated block bytes; unset, production pays one
nil check.

Baseline, before any GPU work: `just capture=3 run https://webglsamples.org/aquarium/
aquarium.html backend=block` measured **5.9 sustained fps** (3 usable frames over
338 ms), software SwiftShader GL. This is the number idea 1 must beat.

### Live FPS instrument [DONE - 9002348, cc7878a, 8a87773]

The `NYALA_FPS_TIMING` switch grew into the measurement tool the rest of this thread
runs on. It is live-only (no capture burden, which starves the draw loop), gated to
zero cost when off, and prints one greppable summary line on quit plus a ~1 Hz badge.
It separates two rates: **In** (ingest, counted at `handleScreencastFrame` before any
drop) and **Out** (paint, counted at draw). The badge peak-holds a scroll burst for
~3 s and shows `--` when idle. The summary reports, for each stream, frames, mean,
min, max, median, and worst gap, plus `in_decode_*_ms` (PNG decode on the worker) and
`out_encode_*_ms` / `out_write_*_ms` (our terminal encode vs the tty flush). `just fps
<url>` wires it.

MEASURED (bbc.co.uk, kitty, pgup/pgdn): In median ~40 fps, peak ~59; Out 2 to 6 fps.
So paint is the binding constraint on screen, not ingest. The paint split is decisive:
`out_encode_mean_ms ~191` (our Kitty escape build) against `out_write_mean_ms ~8` (the
terminal). Our encode is the wall, not the terminal or tmux. `in_decode_mean_ms ~34`
is the ingest ceiling, over the 16.6 ms 60fps budget. So the two levers are, in order:
cut the Kitty encode (recommendation 5, bulk `f=24` and buffer reuse, plus the
klauspost zlib BestSpeed swap on the live path, measured 22% off encode-plus-write on
a ~22 MB frame), then JPEG-in-motion for ingest (recommendation 15).

### Screencast ack decouple + off-goroutine decode [DONE - 7a246f4]

A suspected scroll wedge (In and Out both dropped to 0 after a scroll on a real
terminal) traced to the inline PNG decode: it ran on the single chromedp receiver
goroutine, so a large surface's slow decode stalled acks and command responses. The
casty study (Theme 5, ack-on-receive plus zero decode on the event loop) confirmed the
fix. Now the receiver only counts ingest, acks immediately (fire-and-forget, so
Chrome's 2-in-flight credit never waits on downstream work), and slots the raw frame; a
worker goroutine decodes off the receiver and composes. A 5 s watchdog re-acks once on
a detected stall without firing on a legitimately idle page. This lifted ingest from
~7 fps to the ~40 to 59 fps measured above, so the inline decode, not Chromium, was the
old ceiling.

### Realistic ceiling

GPU removes the render bottleneck. Then PNG decode and transport is the next wall, and
Chromium throttles the screencast on top. So 60 fps is a stretch. The point is to get
the real ceiling and see which wall is next.

### Sequence

1. [DONE] Idea 1 (GPU accel: `--enable-gpu` plus the flake driver path). Owner verified
   hardware WebGL and aquarium 26 -> 60 fps.
2. [DONE] JPEG-in-motion (recommendation 15) is active for Sixel. Block stays JPEG
   quality 80. Kitty stays direct PNG.
3. NEXT: measure sustained terminal FPS and split Kitty terminal PNG decode/draw from
   Nyala framing. Then address Sixel quantisation and dirty bands. Graphics downscaling
   is outside the active roadmap.

---

## Optimising Block Rendering

### Hardware outcome (2026-07-06): the decode was the wall

Landed: three block changes, and a lesson. Rec 2 (downscale region-split) and the
Compose region-split both landed byte-identical (goldens unchanged) and cut render
CPU 5-10x, but neither moved the frame rate. The render analysis below measured the
render stages in isolation and missed the real wall. The live end-to-end fps split
(`just fps backend=block`, aquarium) showed it: the wall was the JPEG decode,
`in_decode ~28-31 ms` on a single worker, against `out_encode 2.5 ms` and
`out_write 0.6 ms`. One decode worker caps throughput near 33 fps, so Out sat at 33
while In was 60. Parallelising the decode across a live worker pool
(`internal/engine/decodepool.go`, default 3 workers, `NYALA_DECODE_WORKERS`, bounded
drop-oldest queue, monotonic seq-gate, capture path unchanged) took block Out from 33
to a locked **60**, drawing 99.5% of ingested frames. The block backend now tracks
the 60 fps browser arrival; the ceiling is the arrival rate (vsync), not our CPU.
Lesson: measure the end-to-end per-frame budget on hardware before optimising a
stage. The decode ran on a separate goroutine the render benchmark never saw.

### Large-surface decode cut, no downscaling (2026-07-06)

At a large terminal the decode grew to ~64 ms/frame and capped Out. Two changes
landed on the live block JPEG path only, both near-identical (not byte-identical, but
no golden/corpus gate covers the JPEG path; the sub-cell average washes out a 2-3
level delta). First, a YCbCr fuse (`frame.go averageYCbCrToRGBA`): average the
decoder's YCbCr planes to the sub-cell footprint and convert only that small result,
skipping the full-res RGBA convert and its 28 MB/frame alloc and absorbing the viewer
downscale pass; decode 64 -> 56 ms, Out 48 -> 59. Second, jpegn
(`github.com/gen2brain/jpegn` v0.4.4, pure-Go SIMD, cgo-free, arm64 NEON): a ~2.5-4x
faster decode gated into `decodeLiveFrame` with a stdlib fallback on `ErrUnsupported`;
on a high-entropy aquarium frame `in_decode` 56 -> 29 ms (the Huffman decode and the
~9 ms fuse average and ~2 ms base64 are the non-SIMD remainder). Net: two decode
workers now lock 60 where four were needed, so `defaultDecodeWorkers` dropped 3 -> 2.

### Core-adaptive decode-worker default (2026-07-07, committed on perf)

`defaultDecodeWorkers()` now returns `max(2, runtime.GOMAXPROCS(0)/2)`, read once at
pool build (Go 1.26 GOMAXPROCS is cgroup-aware). So decode workers take about half the
cores, leaving the rest for the render, encode, engine goroutines, and Chromium; the
floor of 2 is the low-core heuristic. Resolves to 2 (4 cores), 4 (8), 8 (16), 16 (32).
The `NYALA_DECODE_WORKERS` env override and its hard [1,16] clamp are unchanged.
`internal/engine/decodepool.go` plus a `decodepool_test.go` clamp test.

### Terminal ceiling found (2026-07-07)

On the block backend Nyala emits a frame in ~1.7 ms (`out_encode`), so the wall on a
slow terminal is the terminal, not Nyala. WezTerm is byte-bound: truecolour SGR on a
full-motion 2560x2880 grid pushed `out_write` to ~20 ms (Out ~40), while `--color=256`
(fewer SGR bytes) dropped `out_write` to 0.4 ms and locked Out at 60. Lower resolution
does the same (2560x1440 holds 60 at sextant + truecolour). So `just fps` now measures
each terminal's bytes-per-frame ceiling: kitty, Alacritty, foot hold 60 at 2560x2880;
WezTerm and Ghostty saturate sooner. `--color=256` is the documented lever for
byte-bound terminals; no code change needed.

### Summary

Two conclusions. First, the dominant block-render cost is the source downscale, not
the colour maths. `renderBlock` reduces the full screencast surface to the sub-cell
grid before Compose runs (`internal/viewer/viewer.go:419`,
`metrics.DownscaleLiveInto` over `areaAverage` in `internal/metrics/align.go:64`).
Measured at a 16x33 cell it costs 3.13 ms at 120x40 and 13.9 ms at 240x86, which is
80 to 90 percent of block CPU. The `bench:block` harness named Compose as the
leader, but it fed an already-downsized source and so excluded this upstream
downscale; its Compose figure of 0.26 to 1.1 ms is the second cost. I checked the
code to settle the conflict: the engine emits `frame.Img` at the device surface
(`surfaceSize` = cols*cellW by rows*cellH terminal pixels, `launch.go:114`), and the
only reduction to the sub-cell footprint is the viewer downscale at `viewer.go:419`.
So the downscale is the wall and Compose is next. Second, region-split parallelism on
goroutines helps, decisively, at every Nyala grid size. Split the fused
downscale-plus-binarise into whole-row bands, one goroutine per band from a
persistent pool sized to `GOMAXPROCS-1`. Expect near-linear speedup to about 8
bands, then memory-bandwidth-bound, so roughly 3x to 5x on the 4 to 8 core boxes
Nyala targets while headless Chromium shares the cores. The diff and the overlay
emit stay sequential.

### Measured cost breakdown

`bench:block` benchmark (go1.26.4, linux/amd64, AMD Ryzen AI Max+ 395, `-benchmem`).
These measure the render-side stages on an already-downsized source; they exclude
the `viewer.go:419` downscale.

| Benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| BlockCompose/quadrant/120x40 | 261,098 | 0 | 0 |
| BlockCompose/quadrant/240x86 | 1,102,999 | 0 | 0 |
| BlockCompose/sextant/120x40 | 230,462 | 0 | 0 |
| BlockCompose/sextant/240x86 | 985,333 | 0 | 0 |
| BlockDiff/120x40 (full-motion) | 235,338 | 639 | 39 |
| BlockDiff/240x86 (full-motion) | 1,018,588 | 1,374 | 85 |
| BlockOverlay/120x40 (fold only) | 140,698 | 2,720 | 1,360 |
| BlockOverlay/240x86 (fold only) | 310,901 | 5,848 | 2,924 |

Render-side per-frame split (compose + overlay fold + full-motion diff):
- 120x40 (4,800 cells): ~0.64 ms. Compose 41 percent, diff 37 percent, overlay 22
  percent.
- 240x86 (20,640 cells): ~2.43 ms. Compose 45 percent, diff 42 percent, overlay 13
  percent.

The `code:concurrency-feasibility` measurement adds the missing downscale stage at a
16x33 cell, which dwarfs all three render-side stages above:

| grid | source px | downscale (`align.go:64`) | compose |
|---|---|---|---|
| 120x40 | 1920x1320 (2.53M) | 3.13 ms | 0.35 ms |
| 240x86 | 3840x2838 (10.9M) | 13.9 ms | 1.27 ms |

The downscale scales with source device pixels (cols*cellW by rows*cellH), so its
cost falls on a small terminal cell and rises on a large one; Compose and diff scale
with cell count. A separate cost sits off this path: the engine allocates a fresh
`image.RGBA` per decoded frame (`internal/engine/frame.go:116`), the destination of
the JPEG-to-RGBA convert, ~73 percent of decode bytes.

pprof flat leaders (`bench:block`, combined run, 9.58 s; equal wall time per
sub-benchmark, so read leaf hotspots here and per-frame share from the table above):

```
 7.52%  runtime.memmove            (diff retainPrev copy + SGR/glyph byte appends)
 6.99%  block.binarizeSextant      cum 18.06%
 6.47%  block.clampByte (inline)
 5.95%  block.pixel                cum 8.98%
 5.95%  block.quadrantColours      cum 9.60%
 4.49%  uniseg.propertySearch      (overlay grapheme segmentation)
 4.49%  block.avgBlock             cum 8.46%
 4.28%  block.sextantColours       cum 8.35%
 3.65%  block.luma (inline)
 3.55%  block.(*GridDiff).writeColour  cum 19.83%
 3.55%  internal/strconv.AppendUint    cum 10.96%
 3.44%  block.binarizeQuadrant     cum 14.09%
 2.40%  block.composeSextantInto   cum 24.95%
 2.19%  block.avgChannels (inline)
```

Per-stage share tied to Nyala file:line:
- Downscale: `metrics.DownscaleLiveInto` (`internal/metrics/align.go:41`) over
  `areaAverage` (`align.go:64`), called from `renderBlock` (`viewer.go:419`). The
  per-frame leader.
- Compose sample and binarise: `composeQuadrantInto` (`block.go:235`),
  `composeSextantInto` (`sextant.go:166`), `binarizeQuadrant` (`block.go:383`),
  `binarizeSextant` (`sextant.go:96`), `luma` (`block.go:466`), `quadrantColours`
  (`block.go:407`), `sextantColours` (`sextant.go:122`). Alloc-free.
- Diff and SGR: `GridDiff.Emit` (`internal/render/block/diff.go:59`), `writeColour`
  (`diff.go:125`), truecolour digit formatting `appendTrueColor` (`colour.go:70`).
  The `strconv.AppendUint` digit build is ~11 percent of the diff; `retainPrev`
  (`diff.go:236`) is an O(cells) memcpy every frame.
- Overlay: `applyOverlay` (`overlay.go:31`); the alloc source is
  `uniseg.NewGraphemes` per run (`overlay.go:40`) plus the `make([]bool, pxW)` mask
  per grapheme cell (`overlay.go:264`), 1,360 allocs at 40 runs.

### Parallelism verdict

Region-split the downscale-plus-binarise. It is worth it at every Nyala size. The
crossover is far below Nyala's workload: `art:go-parallel-image` puts a 2-band
break-even near 50 us of work, and the downscale is 3 to 14 ms, 60x to 280x above
break-even. `code:concurrency-feasibility` prototyped the split and confirms the win:

| grid | 1 band | 2 | 4 | 8 | 16 |
|---|---|---|---|---|---|
| 120x40 | 3.16 ms | 1.82 (1.7x) | 1.00 (3.1x) | 0.59 (5.3x) | 0.40 (7.9x) |
| 240x86 | 13.9 ms | 7.19 (1.9x) | 3.78 (3.7x) | 2.22 (6.3x) | 1.66 (8.4x) |

Speedup is near-linear to about 8 bands, then memory-bandwidth-bound, because the
downscale streams the whole source plane once. On the dev box (32 cores) 16 bands
gives 8x, not 16x. Nyala targets 4 to 8 core boxes that also run headless Chromium,
so expect ~3x at 4 bands and ~5x at 8 bands there.

The shape:
- Fuse downscale and binarise into one band worker. Split by destination grid rows.
  Each worker area-averages its rows then binarises them into `Grid.Cells`. The
  sub-cell pixels are still hot in cache from the average that just wrote them.
- Use a persistent worker pool, not per-frame goroutines. The draw loop is one
  long-lived goroutine; a pool sized once keeps the path alloc-free and avoids
  per-frame stack re-grow. Drive it with a reused `sync.WaitGroup`, not `errgroup`
  (per-call alloc).
- Band count: clamp to `GOMAXPROCS-1`, never more than dstRows/16, so bands stay
  fat and one or two cores are left for the decode worker
  (`internal/engine/screencast.go:941`) and Chromium's compositor. Read
  `runtime.GOMAXPROCS(0)` once at pool build; Go 1.26 makes it cgroup-aware.
- False sharing is not a concern here. Each cell is written once, and bands align to
  whole rows, so a band boundary shares at most one 64-byte line per frame. Write
  straight into the shared `dst.Pix` and `Grid.Cells` at disjoint row ranges. No
  padding, no per-worker scratch grids, no merge step beyond the barrier.

What stays sequential: `GridDiff.Emit` (`diff.go:59`) is an ordered escape stream
with a cursor and an fg/bg colour cache carried across cells, so it must stay serial;
its cost is small, so no loss. `applyOverlay` (`overlay.go:31`) is first-writer-wins
across runs; runs are single-row so it could ride the same row-bands, but it is a
slice of the cheap tail, so run it as a sequential pass after the band join.
Splitting the diff is clean but pays only ~0.3 ms best case and sacrifices cross-band
colour coalescing, so `code:diff-sgr` recommends against it.

Reconciliation: `code:concurrency-feasibility` and `art:go-parallel-image` agree on
the shape (static whole-row bands, `GOMAXPROCS` sized workers, disjoint slice writes,
no false-sharing mitigation, diff sequential). They differ only on which stage is the
target: the code agent points at the downscale it measured, the prior-art agent and
`bench:block` point at Compose. The evidence-based call is the downscale, since the
code shows it runs at `viewer.go:419` on the full device surface. Fuse both stages
under one band worker and the distinction disappears. `art:notcurses` adds the one
cautionary datum: the notcurses maintainer's single internal threading attempt lost
to a shared-state mutex on a 9 to 20 percent phase. The row-band model avoids both
faults by construction: no shared mutable state, and it targets the dominant stage.

### Prior-art techniques worth adopting

- Row-batch data parallelism (chafa). chafa splits work into row batches over a
  thread pool via `chafa_process_batches`, aligns each batch boundary to a whole
  symbol-row (`batch_unit`), and caps threads at `min(max, n_batches)`. This is the
  row-band model to copy for the downscale-plus-binarise. Source:
  https://raw.githubusercontent.com/hpjansson/chafa/master/chafa/internal/chafa-batch.c
- Integer luma instead of float64 (fast-quantise, notcurses). Replace the per-sub-cell
  `0.299R + 0.587G + 0.114B` float `luma` (`block.go:466`) with `77R + 150G + 29B`,
  or three 256-entry lookup tables. The split needs only luma ordering, which the
  integer weights preserve, so the masks stay byte-identical. `code:sextant` measured
  a ~23 percent Compose cut from this one swap. Reference:
  https://www.sjbrown.co.uk/posts/dxt-compression-techniques/
- Dominant-channel extrema for the two-colour pick (chafa, tiv). Pick the fg/bg pair
  by one linear min/max-per-channel scan over the sub-cell samples, taking the two
  ends of the widest-range channel, rather than averaging or sorting. chafa measured
  -27 percent of total conversion from this change (PR #334). tiv frames the same
  idea as split-on-max-range-channel then fg = mean(above), bg = mean(below). Sources:
  https://github.com/hpjansson/chafa/pull/334 and
  https://github.com/stefanhaustein/TerminalImageViewer
- Flat-cell and isoluminant fast paths (notcurses). Short-circuit a cell whose
  samples all fall within an epsilon to a solid block, skipping the sub-cell binarise
  (notcurses' `qtrans_check` and `totaldiff == 0` break). For the rare isoluminant
  cell where the luma split is unreliable, fall back to an RGB closest-pair cluster
  (`rgb_4diff`, `quadrant_drivers`). Source:
  https://notcurses.com/html/blit_8c_source.html
- Occupancy-keyed glyph LUT (notcurses). notcurses indexes a flat `sextrans[64]`
  table by the 6-bit occupancy pattern, no conditional tree. Nyala already does this
  with `quadrantGlyphs`/`sextantGlyphs`, so it is confirmation, not a gap.
- Goroutine-per-region in Go (pixterm). pixterm parallelises its per-cell conversion
  across all CPUs, the direct Go precedent for the region split. Source:
  https://github.com/eliukblau/pixterm
- Ordered or blue-noise dither only, never Floyd-Steinberg (chafa). If Nyala ever
  dithers the block bitmap, FS diffusion is serial and defeats the row-band split;
  ordered and blue-noise are a cheap parallel-safe pre-pass. Source:
  https://github.com/hpjansson/chafa/issues/238
- Faster median-cut quantiser (fast-quantise). Not on the block path, but for the
  Sixel backend `carbocation/go-quantize` replaces `soniakeys/quant` at ~23x. Already
  captured in the Sixel plan above.

### Ranked recommendations (impact vs effort)

1. **ON HOLD by owner decision (2026-07-07), not never.** Request the block screencast at
   the sub-cell resolution and delete the downscale. Size the block backend's device surface
   to the sub-cell grid (cols*2 by rows*4) via a low device scale factor while keeping the
   CSS layout width at target, so the engine emits the exact bitmap Compose samples and
   `renderBlock` drops `DownscaleLiveInto` (`viewer.go:419`; sizing at `launch.go:114`,`:134`).
   The crisp text overlay is a separate whole-cell layer, unaffected by bitmap resolution.
   Effort medium-large (engine capture-sizing, layout care). Impact very high (removes the 3
   to 14 ms dominant stage outright and makes the region split unnecessary for block).
   Verdict: highest payoff, deletes the wall rather than splitting it. WHY HELD: it makes
   Nyala's decode trivially cheap, which (a) masks potential future optimisations, since a
   shrunk input removes the headroom those wins would show, and (b) hides the performance gap
   between terminal emulators, since the current heavy end-to-end pipeline is what stresses
   the terminals and reveals their throughput ceiling. The owner keeps the full-resolution
   path as the discriminating benchmark. Revisit when the owner decides otherwise.
2. **[DONE - staged]** Region-split the downscale (owner's parallel region render).
   Landed in `internal/metrics/align.go` (a persistent `DownscalePool`: whole-row
   bands, workers sized `GOMAXPROCS-1` read once, reused `sync.WaitGroup` barrier,
   disjoint dst-row writes, band count clamped to `dstRows/16`, serial below that) and
   `internal/viewer/viewer.go` (the pool lives on `frameScratch`, built once, driven by
   `renderBlock`'s diff branch). Byte-identical: a parallel-vs-serial equality test over
   uneven splits passes, `-race` clean, `TestGoldenTrack` green with the golden
   unchanged. Measured on a 32-core box at cell 16x33: **120x40 3.17 ms -> 0.56 ms
   (5.7x), 240x86 13.9 ms -> 1.40 ms (10x)**, 0 allocs. The downscale is no longer the
   dominant stage; the block stages are now roughly level. The fused downscale+binarise
   variant and the residual Compose split were not needed for the win. Reopen either
   only if a new profile puts those stages back on top; this gate is indexed in
   [`TODO.md`](./TODO.md#2-improve-graphics-transport-where-measurements-justify-it).
   Effort medium, impact high (delivered).
3. **[DONE - staged]** Integer luma (owner's faster two-colour quantise). Swapped float
   `luma` (`block.go`) for inline integer weights `77R + 150G + 29B`, feeding
   `binarizeQuadrant` and `binarizeSextant`. Benched three forms: inline integer multiply
   (-17.6% geomean) beat the integer LUT (-15.3%) and the byte-identical float LUT
   (-2.8%); inline won because the multiply is cheaper than three L1 loads plus load-use
   latency. Measured BlockCompose: quadrant -13.1/-13.3%, sextant -21.5/-22.0% (120x40,
   240x86). NOT byte-identical: the integer midpoint crosses differently at near-tie
   high-luma boundaries, so 16/24000 cells (0.067%) flip one or two levels (3+3+2+4+4
   across the five corpus pages). The owner approved the re-bless: goldens regenerated
   (`just update-golden`, block plus overlay, amd64-only), and `calibration.json`
   regenerated (its SSIM/PSNR/dE ranges shift microscopically because calibration composes
   the block grid). `baseline.json` stayed within tolerance (unchanged). Full diagnostics
   green. This frees compose CPU, which helps a paint-bound terminal (WezTerm ~40 fps),
   not the kitty 60 fps path.
4. Reusable RGBA ring on the decode seam. Round-robin the `draw.Draw` destination
   into a dimension-keyed ring of 5 to 6 buffers owned by the frame loop
   (`internal/engine/frame.go:116`), invalidated on resize. Effort low-medium. Impact
   low (GC pressure only, ~73 percent of decode bytes, no CPU cut on the 6.8 ms
   decode). Profile before building it; the gate is indexed in
   [`TODO.md`](./TODO.md#2-improve-graphics-transport-where-measurements-justify-it).
5. **[DONE - staged]** Faster SGR build in the diff. Replaced `strconv.AppendInt` with a
   precomputed `decByte [256]string` decimal table in `appendTrueColor` (`colour.go`),
   appended per component. Byte-identical (golden green). Measured BenchmarkBlockDiff
   -44.9% (237k to 131k ns at 120x40, -44.5% at 240x86). On hardware `out_encode` dropped
   ~2.1 to ~1.7 ms on the block backend.
6. **[DONE - staged]** Alloc-free overlay. Replaced the per-grapheme `make([]bool, pxW)`
   mask (`overlay.go`) with a `uint` bitmask. Byte-identical (golden green). Measured
   BenchmarkBlockOverlay allocations 1,360 and 2,924 per fold to zero, ~8% wall. The
   optional grapheme-segmentation cache was not needed; the mask cut took allocs to zero.
7. Content-hash memoise of the composed grid (owner's idea). Verdict: do not pursue.
   `code:compose-decode-seam` shows the hit rate is near zero (the screencast
   delivers a frame only on visual change), the whole-`img.Pix` hash costs 0.28 ms on
   a normal grid and ~2 ms on the 158x86 geometry (a large fraction of the binarise it
   would save), and the reEmit path re-pushes an identical `img.Pix` with changed
   overlay text, so a pixel-only key would skip a needed text update while a combined
   key never hits. This confirms the recommendation 11 deferral.

### Sources

- chafa row-batch pool: https://raw.githubusercontent.com/hpjansson/chafa/master/chafa/internal/chafa-batch.c
- chafa symbol renderer and colour pick: https://raw.githubusercontent.com/hpjansson/chafa/master/chafa/internal/chafa-symbol-renderer.c
- chafa contrasting-pair min/max scan (PR #334): https://github.com/hpjansson/chafa/pull/334
- chafa features and threads API: https://hpjansson.org/chafa/ref/chafa-Features.html
- chafa dither cost: https://github.com/hpjansson/chafa/issues/238
- chafa-syms-rs (pure-Rust port, row-parallel Phase 7): https://docs.rs/chafa-syms-rs/latest/chafa_syms_rs/ and https://github.com/mevanlc/chafa-syms-rs
- notcurses blitter source: https://notcurses.com/html/blit_8c_source.html
- notcurses threading regression (issue #2573): https://github.com/dankamongmen/notcurses/issues/2573
- notcurses threading contract: https://notcurses.com/notcurses.3.html
- TerminalImageViewer (tiv) algorithm: https://github.com/stefanhaustein/TerminalImageViewer
- pixterm (Go goroutine-per-region): https://github.com/eliukblau/pixterm
- timg (half and quadrant, upper-block bleed dodge): https://github.com/hzeller/timg
- termimg (Go tiv port): https://git.sr.ht/~shabbyrobe/termimg
- golang.design software rendering (row-band split, SIMD as inner layer): https://golang.design/under-the-hood/en/part6hetero/ch19graphics/software/
- pierrre/imageutil Parallel1D (static bands): https://github.com/pierrre/imageutil/blob/v1.0.0/parallel.go
- disintegration/imaging parallel (row-index channel): https://github.com/disintegration/imaging/blob/master/utils.go
- golang-nuts granularity thread (Vyukov): https://groups.google.com/g/golang-nuts/c/CZVymHx3LNM/m/esYkA_YoB-MJ
- 100 Go Mistakes #56 (parallel threshold): https://100go.co/56-concurrency-faster/
- Ardan Labs scheduling part 3 (CPU-bound parallel gains): https://www.ardanlabs.com/blog/2018/12/scheduling-in-go-part3.html
- Go 1.26 archsimd (amd64-only experiment): https://pkg.go.dev/simd/archsimd
- goperf false sharing and field alignment: https://goperf.dev/01-common-patterns/fields-alignment/
- DXT range-fit vs cluster-fit (two-colour block pick): https://www.sjbrown.co.uk/posts/dxt-compression-techniques/
- BC1 endpoint refinement: https://www.ludicon.com/castano/blog/2022/11/bc1-compression-revisited/

---

## Decode Optimization

Research depth: 10 sub-agents, 2026-07-06. Five hunted the fastest Go base64
decoder, five the fastest Go PNG decoder, both under Nyala's hard constraints:
cgo-free, clean `CGO_ENABLED=0` cross-build to linux/amd64 AND linux/arm64, must
beat the Go 1.26 stdlib DECODE, output byte-identical to stdlib. Two options came
back worth doing in turn. Base64 is the real win; PNG is small and off the hot
path.

### Option 1 - base64 decode: swap to `emmansun/base64` (the real win) [DONE - committed, perf]

LANDED and committed on the perf branch (2026-07-07). Spiked GO and wired
`github.com/emmansun/base64 v0.10.0` into both `frame.go` decode sites. Measured on the
dev amd64 box (AVX2/AVX512) on a 245 KB screencast blob: stdlib DecodeString 83 us
(2.94 GB/s) against emmansun 11 us (22.5 GB/s), **7.6x**. Byte-identical to stdlib across
the blob, empty, all padding tails, and a CRLF-wrapped blob. Cross-build clean to amd64
and arm64 at `CGO_ENABLED=0`. The pooled follow-up also landed: `decodeBase64Pooled`
reuses the output buffer from a `sync.Pool` and uses the SIMD `Decode(dst, src)` path
(read-only zero-copy `unsafe.Slice` for src, as emmansun's own DecodeString does),
**4.06x** over the emmansun DecodeString form (10097 -> 2487 ns/op) with the 188 KB/frame
output alloc gone, race-clean under the decode pool; the orphaned `decodeBase64Fast` was
then removed. `just build`/`just test`/`just lint` pass; the capture corpus is unaffected
(`decodeScreencastImage` output stays byte-identical). Hardware-verified: block locks 60
with tighter frame pacing (fewer GC pauses). Arm64 NEON speed is checkable only on real
arm64 silicon. The detail below is the pre-landing research.

The live screencast decode runs `base64.StdEncoding.DecodeString` on every frame
before the image decode, at two sites in `internal/engine/frame.go`
(`decodeScreencastImage:96`, `decodeLiveFrame:169`). It sits inside the measured
`in_decode`. After jpegn shrank the JPEG decode, base64 is a larger share (~2 ms on
a large frame).

Pick: `github.com/emmansun/base64` (BSD-3, Go plus Plan9 asm). Two agents converged
on it independently, both verifying the DECODE path uses SIMD in the source, not the
README headline.
- SIMD decode on amd64 (SSE / AVX2 / AVX512-VBMI) AND arm64 (NEON), runtime CPU
  dispatch, generic Go fallback on other CPUs. Also ppc64x, s390x, loong64, riscv64.
- Cgo-free. An agent cross-built it clean to amd64 and arm64 at `CGO_ENABLED=0`.
- Drop-in for `encoding/base64`: same `StdEncoding`/`URLEncoding`/`Raw*` API, RFC 4648
  semantics, stdlib-matching `\r`/`\n` handling and `CorruptInputError` offsets.
- Speed (third-party, amd64 Zen 5, GB/s): stdlib ~2.8, emmansun ~24 at 1 KB rising to
  ~32 at 64 KB, so ~8-11x. For a 3-4 MB frame that cuts base64 from ~1.4-2.7 ms to
  ~0.1-0.2 ms.
- Caveats: 16 stars, no self-published benchmarks, arm64 NEON decode was still being
  tuned in late 2024. So the spike must prove byte-identity and the arm64 win on
  hardware, exactly as jpegn did.

Reality check: at two decode workers the block backend is already arrival-limited at
60 fps, so this is CPU-cost headroom, not more frames. A real ~1-2 ms/frame saving.

Free companion win, independent of the decoder: base64 is unavoidable on CDP (always
base64 in the JSON event, cdproto hands the raw string, no double-decode, no binary
transport, and `captureScreenshot`/`beginFrame` do not dodge it). But the live sites
call `DecodeString`, which allocates a fresh buffer per frame. Fuse base64 into the
image parse with `base64.NewDecoder(base64.StdEncoding, strings.NewReader(data))` fed
straight to `jpegn.Decode`/`image.Decode`, so no intermediate `[]byte` is
materialised; wrap in a `bufio.Reader` if the decoder does tiny reads. This kills the
per-frame decode-buffer alloc for free. It trims allocations, not the CPU; the
decoder swap is what cuts the CPU.

Not shippable yet: `dans-stuff/simdenc` (Go 1.26 `simd/archsimd`) hits ~80 GB/s decode
but is amd64-only and needs `GOEXPERIMENT=simd`. Revisit when `archsimd` ships
un-gated (targeted Go 1.28) and grows an arm64 path. cgo aklomp/base64 (~6-14 GB/s
AVX2) sets a lower ceiling than the pure-Go SIMD route and breaks the clean
cross-build, so it stays excluded. `gen2brain/base64` is encode-only (prior finding,
confirmed 2026-07-06: its decode is an older stdlib scalar fork, ~25% slower than
Go 1.26 stdlib, so it was rejected).

Spike (mirror jpegn): benchmark emmansun decode vs stdlib on a real screencast blob on
both amd64 and arm64, prove byte-identical output, confirm the arm64 win. Go criterion:
beats stdlib by enough to move `in_decode`, cgo-free, arm64 builds and wins,
byte-identical. If GO, wire a `decodeBase64Fast` helper with a stdlib fallback (gated
like `decodeJPEGFast`) into the two `frame.go` sites, and fold in the `NewDecoder` fuse
at the same time. Owner verifies the `in_decode` drop on hardware (`just fps
backend=block`).

Kitty payload base64 encode: base64 ENCODES the Kitty payload, in both the live
encoder (`internal/render/kitty/live.go`) and the stateless capture `Renderer`
(`internal/render/kitty/kitty.go`). [DONE - perf] Both now encode through
`emmansun/base64` in place of stdlib `encoding/base64`, mirroring the decode swap
above: byte-identical (RFC 4648), zero allocations, cgo-free, both arches, already a
direct dependency. Benchmarked ~53x faster on the payload (130.7 us -> 2.47 us on a
300 KB buffer). Small absolute win: base64 is a small share of the Kitty encode, since
it runs on the already-zlib-compressed payload.

### Option 2 - PNG decode: adopt `github.com/woozymasta/png` (SIMD unfilter, off the hot path) [DONE - committed, perf]

LANDED and committed on the perf branch (2026-07-07). Nyala now decodes PNG frames
through `github.com/woozymasta/png`, a lossless, byte-identical drop-in for stdlib
`image/png`, promoted to a direct dependency. It is wired in `internal/engine/frame.go`
by explicit PNG-signature dispatch: `decodeFrameImage` decodes via woozymasta when the
bytes start with the PNG magic, else falls through to stdlib `image.Decode` for JPEG,
which keeps the `jpegn` SIMD fast path and the stdlib registry. It deliberately does NOT
use woozymasta's `image.RegisterFormat` self-registration, because `internal/capture`
imports stdlib `image/png` for encoding and two `"png"` registrations would make
`image.Decode` dispatch init-order-dependent and ambiguous.

Correctness gate: 1011 of 1011 real PNGs byte-identical, every `testdata` PNG plus all
12 captured corpus fixtures, decoded through both stdlib and woozymasta. Lossless, no
golden change.

Performance: PNG decode is ~54% faster on amd64 (mean ~1.7 ms to ~0.8 ms on block
frames). This corrects the pre-landing estimate below of ~7-21% from a
`klauspost/compress` zlib swap. The measured win is almost entirely woozymasta's amd64
SSE2 SIMD unfilter, NOT the zlib inflate. A zlib-only swap (the surgical Route B fork)
measured ~0.3% on these small, highly compressible flat block frames, so Route B was not
pursued; Route A (full woozymasta) was taken.

arm64: cross-compiles clean, but native arm64 could not be run in the dev environment. A
portable (non-build-tagged) equivalence test `TestWoozymastaPNGByteIdenticalToStdlib` in
`internal/engine` runs in arm64 CI and gates byte-identity. The arm64 speed win is
unquantified and likely small (the SIMD unfilter is amd64-only; the zlib swap gives ~0
on this content), but no regression is expected because decode is integer-exact and
byte-identical.

Scope and placement: this is off the hot JPEG motion path. PNG decodes only static,
idle, and flat block frames, so it does not move the graphics-backend FPS walls (encode
and Sixel quantise); it speeds static-page and block-reading decode. It is not a
motion-fps lever.

The pre-landing research, for the record. No pure-Go SIMD PNG decoder exists (gen2brain
has no PNG member; the SIMD leaders Wuffs and Rust zune-png are cgo/non-Go). PNG decode
is dominated by DEFLATE inflate, which is inherently serial (one zlib stream, no restart
markers), so it cannot be block-parallel-decoded like JPEG. The one cgo-free inflate
lever is `github.com/klauspost/compress/zlib` (pure Go plus Plan9 asm, cgo-free,
arm64-clean, API-identical), estimated at ~7-21% total decode on amd64. Stdlib
`image/png` hardwires `zlib.NewReader` with no injection seam, so the win needed a fork.
Two routes were scoped:
- Route A (taken): adopt `github.com/woozymasta/png` (v1.1.0, BSD-3, drop-in
  `image/png`). Adds the klauspost zlib swap plus amd64 SIMD unfilter (SSE2 Up filter,
  SSSE3 RGB<->RGBA) with a pure-Go arm64 fallback.
- Route B (rejected): vendor a one-import fork of stdlib `image/png` into an internal
  package (copy `reader.go`+`paeth.go`, change `import "compress/zlib"` to `import zlib
  "github.com/klauspost/compress/zlib"`). Surgical and minimal, but it wins only the
  inflate share, which measured ~0.3% on these frames.

Follow-up, unbuilt: the codec study found that on flat/text block frames PNG beats JPEG
on bytes, quality, and decode, which argues for a future content-aware PNG/JPEG switch,
not motion-only. That is a separate, unbuilt item.

### Sources (decode research)

- emmansun/base64 (SIMD decode, both arches, cgo-free): https://github.com/emmansun/base64 and https://pkg.go.dev/github.com/emmansun/base64 and the arm64 decode-tuning issue https://github.com/emmansun/base64/issues/21
- dans-stuff/simdenc (Go 1.26 archsimd, amd64-only, decode-vs-stdlib table): https://github.com/dans-stuff/simdenc
- stdlib base64 SIMD rejected, pure-Go tuning only: https://github.com/golang/go/issues/19636 and encode CL https://github.com/golang/go/pull/78344
- aklomp/base64 (cgo ceiling) and the Muła/Lemire memory-copy-speed paper: https://github.com/aklomp/base64 and https://arxiv.org/abs/1910.05109
- go-base64-simd (cgo aklomp wrapper, whitespace-prepass cost): https://github.com/valpackett/go-base64-simd
- CDP screencastFrame is base64 in JSON, no binary transport: https://chromedevtools.github.io/devtools-protocol/tot/Page/#event-screencastFrame
- cdproto screencast field is a plain string (no double-decode); captureScreenshot vs startScreencast round-trip cost: https://github.com/vercel-labs/agent-browser/pull/1200
- klauspost/compress inflate speedup (~10-15% inflate, ~50% headline), cgo-free arm64: https://github.com/klauspost/compress and PR https://github.com/klauspost/compress/pull/483
- woozymasta/png (drop-in, klauspost zlib + amd64 SIMD unfilter; adopted, measured ~54% amd64 decode): https://pkg.go.dev/github.com/woozymasta/png
- stdlib image/png hardwires zlib.NewReader (no injection seam): https://go.dev/src/image/png/reader.go
- Wuffs PNG decoder (SIMD ceiling, transpiled C, not cgo-free Go): https://nigeltao.github.io/blog/2021/fastest-safest-png-decoder.html

---

## Open questions

- RESOLVED: screencast format is backend-specific. Block uses JPEG quality 80, Kitty
  always uses direct PNG, Sixel adapts between PNG and JPEG quality 90, and capture
  always uses PNG. Theme 7 records the policy and measurements.
- RESOLVED for current code: a detected tmux, herdr, Zellij, or screen session uses
  the Block backend. Direct Kitty PNG is the standalone-terminal path. Graphics
  passthrough remains conditional work; do not prescribe `U=1` until placeholder
  support is re-tested on the exact target releases. See
  [`TODO.md`](./TODO.md#2-improve-graphics-transport-where-measurements-justify-it).
- RESOLVED: the load-time split (Chromium spawn, navigation-to-first-paint, Nyala's
  setup round-trips) is answered by the recommendation 4 timing (`screencast_started`
  ~191 ms, dominated by Chromium's own launch). Recommendation 13 is now closed on that
  evidence: font pre-warm done, launch-Run batching skipped as no user-visible win.

---

## Sources

### Reference project source (read directly)
- browsh - https://github.com/browsh-org/browsh - Go/Firefox, half-block separate layers, 6x off-screen buffer, mutation-driven text cadence
- brow6el - https://codeberg.org/janantos/brow6el - C++/CEF, libsixel dirty-tile Sixel, Kitty double-buffer + f=24 RGB
- casty - https://github.com/sanohiro/casty - Node/CDP, Kitty-only image bridge, hybrid screencast-trigger + captureScreenshot, adaptive JPEG/PNG
- carbonyl - https://github.com/fathyb/carbonyl (fork https://github.com/jmagly/carbonyl) - patched Chromium, shared-mem frames + damage rect, cell double-buffer diff, fixed-FPS coalescing
- carboxyl - https://github.com/carboxyl-rs/carboxyl - Carbonyl fork, same rendering core

### Protocol and library sources
- Kitty graphics protocol spec - https://sw.kovidgoyal.net/kitty/graphics-protocol/ - animation frames, placements, compression, chunking, shared memory
- kitty Go GraphicsCommand - https://github.com/kovidgoyal/kitty/blob/master/tools/tui/graphics/command.go - reference animation-frame model
- ConsoleEx Kitty video sink - https://github.com/nickprotop/ConsoleEx/commit/2a8227084d81bc132880984cadcbdaaa04eb1077 - a=f,r=1,o=z in-place root-frame edit, "same id retransmit deletes placements"
- Xkitty - https://github.com/kay-ws/xserver-kitty - dirty-rect transmit, zlib, shared memory, 60fps cap
- notcurses Sixel - https://github.com/dankamongmen/notcurses/blob/master/src/lib/sixel.c and issues #1380, #1391, #2573 - threaded bands, palette cache, TAM delta, RLE decode-cost evidence
- chafa fast Sixel - https://github.com/hpjansson/chafa/issues/27, #174, #261 - PCA + hash-table palette mapping, benchmark numbers
- mattn/go-sixel - https://github.com/mattn/go-sixel - RGBA fast path, 15-bit LUT, scratch reuse
- charmbracelet x/ansi sixel+kitty benchmark - https://github.com/charmbracelet/x/pull/381 and #352 - ~300 ms/op, both on soniakeys/quant
- carbocation/go-quantize - https://pkg.go.dev/github.com/carbocation/go-quantize - ~23x faster median cut, draw.Quantizer
- soniakeys/quant - https://pkg.go.dev/github.com/soniakeys/quant/median - Palette() reuse across frames
- BourgeoisBear/rasterm - https://github.com/BourgeoisBear/rasterm - clean Go Sixel/Kitty, paletted-input-only
- libsixel - https://github.com/saitoha/libsixel - cachetable nearest-colour, quality modes, encode policy
- a-sixel quantiser benchmarks - https://docs.rs/a-sixel/latest/a_sixel/ - bit-bucket vs median-cut vs k-means cost
- mpv vo_sixel - https://github.com/mpv-player/mpv/commit/19913921eb9ee4c35de230c62c46d1591cf3326e - fixed palette, skip-frame, 320x240 default
- Sixel format - https://terminfo.dev/sixel and https://nick-black.com/dankwiki/index.php?title=Sixel - band structure, RLE, single-unit emission

### Nyala code (bottleneck evidence)
- `internal/engine/` - chromium.go, recovery.go, screencast.go, calibrate.go, launch.go, forcing.go, sync.go
- `internal/render/sixel/sixel.go`, `internal/render/kitty/kitty.go`
- `internal/render/block/` - block.go, overlay.go, sextant.go, colour.go, diff.go, fit.go
- `internal/render/render.go`, `internal/render/geometry.go`
- `internal/viewer/viewer.go`, `internal/viewer/stream.go`
- `internal/capture/decode.go`


---

## GPU acceleration for headless Chromium (research + proposal)

Status: DONE. Stage A applied (staged) and verified on hardware. Every flag, env
var, and library below traces to a cited source.

### Result (2026-07-06, owner hardware)

Stage A alone reached the GPU. No Stage B needed.

- `launch.go`: `--enable-gpu` on the live path only (capture stays software for the
  deterministic corpus). `flake.nix`: `/run/opengl-driver/lib` on the library path
  (wrapper plus dev shell).
- WebGL Report unmasked renderer:
  `ANGLE (AMD, AMD Radeon 8060S Graphics (radeonsi strix_halo ACO), OpenGL 4.6)`,
  Major Performance Caveat: No. Hardware, not SwiftShader or llvmpipe. WebGL 1 and 2
  both hardware-accelerated.
- Aquarium: browser fps 26 (software) -> 60 (GPU). The `NYALA_FPS_TIMING` badge read
  `In: 60.3 / Out: 24.6 fps`, so the screencast now ingests the full 60. Paint (Out
  ~24) is the next wall, set by the Kitty encode on the large live surface.

The five hypothesis flags stayed rejected: `--enable-gpu` alone was enough, so none
of `--use-angle=gl/vulkan`, `--ignore-gpu-blocklist`, `--headless=new`, or the
video-decode flags were applied. Stage B is unused. The rest of this document is the
original research that led here, kept for the evidence trail.

Target machine (measured on this host, 2026-07-06):

- GPU: AMD Strix Halo, Radeon 8060S (RDNA 3.5 iGPU), PCI `1002:1586` (`lspci`).
- Mesa 26.1.3; `radeonsi` DRI driver and `radeon_icd.x86_64.json` Vulkan ICD both
  present under `/run/opengl-driver` (directory listing on this host).
- Chromium 149.0.7827.200 (`chromium --version`), the pinned nixpkgs build.
- chromedp `v0.15.1`, cdproto `v0.0.0-20260427013145-5737772c319b` (`go.mod`).

### Core finding

Headless Chromium forces SwiftShader (CPU) by default, so WebGL is software unless
you turn that forcing off. The switch that turns it off is `--enable-gpu`. It does
not itself guarantee hardware. It only reverts to normal driver selection, which
then depends on the process reaching a GPU driver. Chromium's own headless switch
header states this verbatim:

> "Enable hardware GPU support. Headless uses swiftshader by default for
> consistency across headless environments. This flag just turns forcing of
> swiftshader off and lets us revert to regular driver selection logic.
> Alternatively, specific drivers may be forced with --use-gl or --use-angle.
> Neither approach guarantees that hardware GPU support will be enabled, as this
> is still conditional on headless having access to X display etc."
> from `headless/public/switches.h`, `kEnableGPU` [S1]

Chromium's headless GPU guide repeats it: pass `--enable-gpu` to stop forcing
software. Default auto-detection on Linux historically wanted an X display, but
forcing an ANGLE backend has been found to work without X on some Linux
configurations [S2]. New headless (default since Chrome 132) needs no `DISPLAY`
for the GPU path [S3][S9].

So the enabling chain on this machine is:

1. `--enable-gpu` to stop the SwiftShader force. [S1][S2]
2. An ANGLE backend that reaches Mesa without X: ANGLE-over-GL (system Mesa EGL) or
   ANGLE-over-Vulkan (radeon ICD). Which one lands on hardware is a hardware test.
   [S2][S6][S8]
3. NixOS making the Mesa driver discoverable to the Chromium process
   (`/run/opengl-driver`, and for the Vulkan route the radeon ICD). [S10][S11][S12]

chromedp already passes bare `--headless`, which is new headless on Chrome 132+, so
that part is done for us (see below). chromedp does not pass `--disable-gpu`, so
there is nothing to remove. The change is additive.

### What chromedp already sets (audit of `DefaultExecAllocatorOptions`)

`internal/engine/launch.go:162` builds the allocator as
`append(chromedp.DefaultExecAllocatorOptions[:], ...)`. In chromedp `v0.15.1` that
array is [S4]:

```
NoFirstRun, NoDefaultBrowserCheck, Headless,
Flag("disable-background-networking", true),
Flag("enable-features", "NetworkService,NetworkServiceInProcess"),
Flag("disable-background-timer-throttling", true),
Flag("disable-backgrounding-occluded-windows", true),
Flag("disable-breakpad", true),
Flag("disable-client-side-phishing-detection", true),
Flag("disable-default-apps", true),
Flag("disable-dev-shm-usage", true),
Flag("disable-extensions", true),
Flag("disable-features", "site-per-process,Translate,BlinkGenPropertyTrees"),
Flag("disable-hang-monitor", true),
Flag("disable-ipc-flooding-protection", true),
Flag("disable-popup-blocking", true),
Flag("disable-prompt-on-repost", true),
Flag("disable-renderer-backgrounding", true),
Flag("disable-sync", true),
Flag("force-color-profile", "srgb"),
Flag("metrics-recording-only", true),
Flag("safebrowsing-disable-auto-update", true),
Flag("enable-automation", true),
Flag("password-store", "basic"),
Flag("use-mock-keychain", true),
```

Findings:

- No `--disable-gpu`. Nothing to remove. Good starting point.
- No `--disable-software-rasterizer`, no `--use-gl`, no `--use-angle`, no
  `--enable-gpu`. All GPU selection stays at Chromium's default, which for headless
  is the SwiftShader force [S1].
- `Headless` maps to the bare `--headless` switch. On Chrome 132+ bare `--headless`
  selects new headless. Old headless was removed from the binary and lives only as
  the standalone `chrome-headless-shell` [S3][S9]. This host runs Chrome 149, so
  `--headless` is already new headless. There is no reason to add `--headless=new`.
- One caveat to watch: chromedp sets `enable-features` and `disable-features` as
  single-valued flags. Chromium takes the last `--enable-features` on the line and
  merges the comma list inside it. A second `--enable-features` can shadow the
  first. If we ever add a feature, extend chromedp's existing value rather than
  append a second `--enable-features` flag. Verify against the running build's
  `chrome://version` "Command Line" line.

### Verdict on each hypothesis flag

| Flag | Verdict | Evidence |
| --- | --- | --- |
| `--headless=new` | Reject (redundant) | Bare `--headless` is already new headless on Chrome 132+, and chromedp passes it. Old headless is gone from the binary. [S3][S9] Adding `=new` is a no-op on Chrome 149. |
| `--enable-gpu` | Verify on hardware (this is the lever) | Turns off the headless SwiftShader force and reverts to normal driver selection. [S1][S2] Not in chromedp defaults, so must be added. Alone it can still land on software if the process cannot reach the driver, hence the hardware test. |
| `--use-gl=angle` | Reject as written (redundant); keep the idea via `--use-angle` | ANGLE is already Chromium's default GL implementation, so `--use-gl=angle` selects what is already selected. The real lever is the ANGLE backend chosen with `--use-angle=...`. [S6][S8] |
| `--use-angle=vulkan` | Needs hardware test (plausible for AMD) | Real switch; selects the ANGLE Vulkan backend. Chromium's VA-API/Vulkan Linux recipe uses it for AMD/Vulkan [S7], and the headless guide notes forcing Vulkan works without X on some configs [S2]. Caveat: nixpkgs chromium once rejected `--use-angle=vulkan` ("vulkan is not an allowed implementation") until the upstream allow-patch landed around M127 [S8]; Chrome 149 should accept it, confirm at runtime. Requires the radeon Vulkan ICD discoverable by Chromium's bundled loader (see flake section). |
| `--ignore-gpu-blocklist` | Optional, conditionally useful | Real switch; forces GPU features on despite the blocklist [S5][S7]. A well-supported AMD Mesa stack may not need it, but it is the standard unblock lever if `chrome://gpu` shows a feature "disabled" for blocklist reasons. Add only if verification shows a blocklisted feature. |
| `--enable-features=VaapiVideoDecoder` | Reject as stale for Chrome 149 | Superseded on Linux. Since Chrome 131 the VA-API decode lever is `AcceleratedVideoDecodeLinuxGL` (plus `AcceleratedVideoDecodeLinuxZeroCopyGL`). `VaapiVideoDecoder` is on by default and no longer the switch that enables decode [S13][S14][S15]. Use the current feature name if we pursue hardware video decode. |

### Minimal proposed change

Two edits, staged so we prove the WebGL lever first and add complexity only if a
hardware check demands it. Do not apply yet.

#### Edit 1: `internal/engine/launch.go` (add flags in `newAllocator`)

Add to the `append(...)` at `launch.go:162`, after the existing options. Gate them
so they ride only on the live GPU path if you want capture and corpus to stay
byte-deterministic (capture forces the block backend and compares pixels; a hardware
GPU can shift antialiasing). Recommended: apply GPU flags on the live viewer path
only, keep capture and corpus on the current software path. Decide this with the
owner.

Stage A, proven-minimal (one flag, the lever):

```go
chromedp.Flag("enable-gpu", true), // stop headless forcing SwiftShader; revert to
                                    // normal driver selection [S1][S2]
```

Stage B, only if Stage A verification (below) still shows SwiftShader or llvmpipe.
Pick one ANGLE backend, in this order of least wiring:

```go
// B1: ANGLE over system Mesa GL/EGL. Needs no Vulkan ICD wiring, only
// /run/opengl-driver on the library path. [S2][S6]
chromedp.Flag("use-angle", "gl"),

// B2 (alternative): ANGLE over Vulkan (radeon ICD). Needs the ICD env in the
// flake. AMD-oriented recipe. [S7][S8]
chromedp.Flag("use-angle", "vulkan"),
```

Do not add both. Do not add `--use-gl=angle` (redundant, S6). Do not add
`--disable-gpu` (chromedp does not set it; adding it would defeat the change).

#### Edit 2: `flake.nix` (make the GPU driver discoverable)

The nixpkgs chromium wrapper adds `libva`, `pipewire`, `wayland`, `gtk`, `libkrb5`
to `LD_LIBRARY_PATH`, but not `/run/opengl-driver/lib`, and it does not wire the
system Vulkan ICD directory (Chromium ships its own vulkan-loader and only sees
NixOS ICDs if told) [S11][S12][S10]. nyala sets `CHROME_PATH` and launches Chromium
as a child, so environment set on the `nyala` process reaches Chromium.

For Stage A / B1 (GL/EGL route), add `/run/opengl-driver/lib` to the wrapped
binary's library path. This mirrors the jive-visualiser flake, which prepends
`/run/opengl-driver/lib` for the same reason (GPU drivers live there on NixOS)
[S10].

`postFixup` wrapProgram (packaged binary):

```nix
postFixup = pkgs.lib.optionalString onLinux ''
  wrapProgram $out/bin/nyala \
    --set-default CHROME_PATH "${pkgs.chromium}/bin/chromium" \
    --prefix LD_LIBRARY_PATH : "/run/opengl-driver/lib"
'';
```

Dev shell (so `just run` picks up the driver too), add a `shellHook` like jive's:

```nix
shellHook = pkgs.lib.optionalString pkgs.stdenv.hostPlatform.isLinux ''
  if [ -d /run/opengl-driver/lib ]; then
    export LD_LIBRARY_PATH="/run/opengl-driver/lib''${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
  fi
'';
```

For Stage B2 (Vulkan route) only, also point Chromium's bundled loader at the radeon
ICD. On this AMD-only host the file is
`/run/opengl-driver/share/vulkan/icd.d/radeon_icd.x86_64.json` (present on this
host). Two options:

- Dev shell, multi-GPU-safe (jive's pattern): glob every ICD into `VK_DRIVER_FILES`
  in the `shellHook` [S10]:
  ```sh
  export VK_DRIVER_FILES=$(find /run/opengl-driver/share/vulkan/icd.d -name '*.json' \
    2>/dev/null | tr '\n' ':' | sed 's/:$//')
  ```
- Packaged binary: `wrapProgram ... --set-default VK_ICD_FILENAMES
  "/run/opengl-driver/share/vulkan/icd.d/radeon_icd.x86_64.json"`. Pinning one ICD
  is fine on this single-GPU host but wrong on a multi-GPU machine; prefer the glob
  for anything shipped.

`VK_ICD_FILENAMES` is the loader's ICD override. NixOS keeps ICDs under
`/run/opengl-driver/share/vulkan/icd.d/`, and Chromium needs them named because it
carries its own loader [S12][S10][S11].

Proven vs test-only summary:

- Proven from sources: `--enable-gpu` is the SwiftShader-force switch; bare
  `--headless` is already new headless; chromedp sets no `--disable-gpu`;
  `/run/opengl-driver` is where NixOS Mesa/Vulkan drivers live and Chromium does not
  search it by default.
- Hardware test only: whether `--enable-gpu` alone reaches radeonsi on this host, or
  needs `--use-angle=gl` or `--use-angle=vulkan`; whether `--ignore-gpu-blocklist`
  is needed; whether the Vulkan route beats the GL route on this iGPU.

### Verification procedure (owner runs on the target)

Objective checks, in order. Run them against the live path (a real Chromium, not the
synthetic-pty capture path).

1. `chrome://gpu` fields. Launch Chromium with the candidate flags and read:
   - "Graphics Feature Status": WebGL and WebGL2 should read "Hardware accelerated"
     (not "Software only", "unavailable", or SwiftShader). [S3][S16]
   - "Driver information" / `GL_RENDERER`: hardware shows the AMD device (e.g.
     `AMD Radeon ... radeonsi`), software shows `SwiftShader` or `llvmpipe`.
     [S3][S16]
   - Programmatic capture over CDP: navigate to `chrome://gpu` and print to PDF (the
     puppeteer/CDP trick), so you can read it headless. [S16]
2. WebGL renderer probe over CDP. In nyala's engine you already run
   `Runtime.evaluate`. Evaluate this and log the result:
   ```js
   (() => {
     const c = document.createElement('canvas');
     const gl = c.getContext('webgl2') || c.getContext('webgl');
     if (!gl) return 'no-webgl';
     const dbg = gl.getExtension('WEBGL_debug_renderer_info');
     return JSON.stringify({
       vendor: gl.getParameter(gl.VENDOR),
       renderer: gl.getParameter(gl.RENDERER),
       unmaskedVendor: dbg && gl.getParameter(dbg.UNMASKED_VENDOR_WEBGL),
       unmaskedRenderer: dbg && gl.getParameter(dbg.UNMASKED_RENDERER_WEBGL),
     });
   })()
   ```
   Hardware: `unmaskedRenderer` names the AMD GPU. Software: it reads
   `ANGLE (... SwiftShader ...)` or `llvmpipe`. This is the definitive per-context
   check; the Chrome team used exactly `GL_RENDERER`/`UNMASKED_RENDERER` to tell
   hardware from SwiftShader [S3].
3. Aquarium fps delta. Load WebGL Aquarium
   (`https://webglsamples.org/aquarium/aquarium.html`), set a fixed fish count, read
   the on-page fps. Hardware on this iGPU sits far above the SwiftShader baseline.
   Record both numbers (software run with flags removed, hardware run with flags) so
   the delta is the evidence. [S3]
4. Video decode (only if we pursue it). `chrome://gpu` "Video Acceleration
   Information" should list codecs, and `chrome://media-internals` should show a
   hardware decoder (not "FFmpegVideoDecoder") while a video plays. If decode stays
   software, add `--enable-features=AcceleratedVideoDecodeLinuxGL` (current name, not
   `VaapiVideoDecoder`) and set `LIBVA_DRIVER_NAME=radeonsi`. [S13][S14][S17]
   `vainfo` (`nix shell nixpkgs#libva-utils -c vainfo`) confirms the radeonsi VA-API
   driver loads at all before you blame Chromium. [S17]

Read the launched command line from `chrome://version` ("Command Line") to confirm
the flags reached Chromium and nothing shadowed the `--enable-features` list.

### Do-not-add list (folklore, redundant, or harmful here)

- `--disable-gpu`: the opposite of the goal; forces software. chromedp does not set
  it, keep it that way. [S1]
- `--headless=new`: no-op on Chrome 132+; bare `--headless` is already new headless.
  [S3][S9]
- `--use-gl=angle`: redundant; ANGLE is the default GL implementation. The lever is
  `--use-angle=<backend>`, not this. [S6]
- `--enable-features=VaapiVideoDecoder`: stale on Chrome 131+; use
  `AcceleratedVideoDecodeLinuxGL` if you want VA-API decode. [S13][S14]
- `--enable-unsafe-swiftshader` / `--use-angle=swiftshader`: these keep software
  WebGL alive, the opposite of enabling hardware. Only for GPU-less CI. [S5][S6]
- `--ignore-gpu-blocklist` unconditionally: do not add blind. Add only if
  `chrome://gpu` shows a feature disabled for blocklist reasons; otherwise it masks
  real driver problems. [S5][S7]
- `--no-sandbox`: not a GPU flag; only needed when running as root, and nyala's
  launch already gates sandbox handling via `FENCE_SANDBOX` and chromedp's root
  check. Do not bundle it into the GPU change. [S4][S18]

### Inconclusive points (stated, not padded)

- Whether `--enable-gpu` alone reaches radeonsi on this host under new headless, or
  needs an explicit `--use-angle` backend, cannot be settled from docs. The headless
  guide only says auto-detection "doesn't seem to work without X11" while forcing a
  backend "have been found to work at least on some Linux configurations" [S2]. This
  is a per-machine hardware test.
- GL route vs Vulkan route performance on this specific RDNA 3.5 iGPU is untested.
  Pick by the aquarium fps and `chrome://gpu` output, not by guesswork.

### Sources

- [S1] Chromium `headless/public/switches.h`, `kEnableGPU` comment.
  https://chromium.googlesource.com/chromium/src/+/HEAD/headless/public/switches.h
- [S2] Chromium docs, "Using GPU Hardware in Headless Chrome".
  https://chromium.googlesource.com/chromium/src/+/refs/heads/main/docs/gpu/using-gpu-hardware-in-headless-chrome.md
- [S3] Chrome for Developers, "Supercharge Web AI testing" (new headless GPU flags,
  `chrome://gpu`, `GL_RENDERER` vs SwiftShader), 2024-01-16.
  https://developer.chrome.com/blog/supercharge-web-ai-testing
- [S4] chromedp `v0.15.1` `allocate.go`, `DefaultExecAllocatorOptions`.
  https://github.com/chromedp/chromedp/blob/v0.15.1/allocate.go
- [S5] Chromium docs, "Using Chromium with SwiftShader" (fallback deprecated,
  `--enable-unsafe-swiftshader`).
  https://chromium.googlesource.com/chromium/src/+/main/docs/gpu/swiftshader.md
- [S6] botbrowser.io, "Linux GPU Backend Selection" (ANGLE GL vs Vulkan vs
  SwiftShader flag matrix; no DISPLAY needed for default headless).
  https://botbrowser.io/docs/deployment/linux-gpu-backend/
- [S7] Chromium docs, "VA-API" (AMD Vulkan recipe: `--use-angle=vulkan`,
  `--ignore-gpu-blocklist`, VA-API feature switches).
  https://chromium.googlesource.com/chromium/src/+/refs/heads/main/docs/gpu/vaapi.md
- [S8] nixpkgs issue #320030, "chromium: vulkan is not an allowed implementation"
  (allow-patch for `--use-angle=vulkan`, landed around M127).
  https://github.com/NixOS/nixpkgs/issues/320030
- [S9] Chrome for Developers, "Removing --headless=old from Chrome" (Chrome 132) and
  the `--headless` default-to-new commit.
  https://developer.chrome.com/blog/removing-headless-old-from-chrome ;
  https://chromium.googlesource.com/chromium/src/+/b9b39a430f71c710d16aafcc67278ef77440c18d
- [S10] jive-visualiser `flake.nix` (owner's reference: prepend
  `/run/opengl-driver/lib`, `VK_DRIVER_FILES` from the ICD dir, `LIBVA_*`).
  https://github.com/linuxmatters/jive-visualiser/blob/main/flake.nix
- [S11] nixpkgs chromium `default.nix` wrapper (`LD_LIBRARY_PATH` includes libva,
  pipewire, wayland, gtk, libkrb5; does not add `/run/opengl-driver`).
  https://github.com/NixOS/nixpkgs/blob/master/pkgs/applications/networking/browsers/chromium/default.nix
- [S12] nixpkgs commit 007af34, "google-chrome: add
  /run/opengl-driver/share/vulkan/icd.d/ to path" (Chromium ships its own
  vulkan-loader and will not search NixOS ICDs without help).
  https://github.com/NixOS/nixpkgs/commit/007af34263d102667f077363b3958e6bf4b05ea8
- [S13] nvidia-vaapi-driver issue #420 (2026-01): current lever is
  `AcceleratedVideoDecodeLinuxGL`; `VaapiVideoDecoder` on by default.
  https://github.com/elFarto/nvidia-vaapi-driver/issues/420
- [S14] cromite issue #2056 (2025): `AcceleratedVideoDecodeLinuxGL` and
  `AcceleratedVideoEncoder` enable VA-API, disabled by default.
  https://github.com/uazo/cromite/issues/2056
- [S15] dev.to, "Chrome Flags' Latest 2024 Update ... Linux" (flag change from
  Chrome/Chromium 131).
  https://dev.to/archerallstars/chrome-flags-latest-2024-update-web-browser-video-hardware-acceleration-on-linux-34k1
- [S16] Musixmatch, "GPU-Accelerated Headless Chromium on Kubernetes" (2025-04:
  read `chrome://gpu`, print to PDF over Puppeteer/CDP, SwiftShader fallback
  detection). https://medium.com/musixmatch-blog/gpu-accelerated-headless-chromium-on-kubernetes-a-practical-guide-b4171c72e87e
- [S17] NixOS wiki, "Graphics" and "Chromium" (`/run/opengl-driver/lib/dri` VA-API
  drivers, `LIBVA_DRIVER_NAME`, `vainfo`, `vulkaninfo`).
  https://wiki.nixos.org/wiki/Graphics ; https://wiki.nixos.org/wiki/Chromium
- [S18] Chrome for Developers headless docs (`--no-sandbox` only for root).
  https://developer.chrome.com/docs/chromium/headless


---

## JPEG decoder research

### Verdict

`gen2brain/jpegn` was benchmarked and adopted. The amd64 speed result and corpus
byte identity are established. It fits Nyala's constraints: cgo-free (pure Go plus
Plan9 assembly the Go toolchain assembles), MIT-licensed, with SIMD IDCT and
upsample paths for both release targets (amd64 AVX2, arm64 NEON). It returns a
native `image.YCbCr`, so Nyala's fused YCbCr to RGBA downscale still applies. The
only active follow-up is real-arm64 performance validation, tracked in
[`TODO.md`](./TODO.md#1-validate-the-stage-3-browser).
The only genuinely faster option is a cgo binding to libjpeg-turbo (about 4.8x),
but that flips Nyala from a clean `GOOS`/`GOARCH` cross-build to a per-target C
toolchain, so its speed does not pay for its cost here. The WASM route
(`gen2brain/jpegli`) is a dead end for decode latency: it runs at stdlib speed.

### Candidate comparison (ranked)

| Rank | Decoder | Speed vs stdlib (baseline 4:2:0) | Build model | Cross-compile impact | YCbCr planes | Licence | Maintenance |
|---|---|---|---|---|---|---|---|
| 1 | `gen2brain/jpegn` | ~1.85x to 2.45x faster | cgo-free (Go + Plan9 asm) | None; toolchain assembles the `.s` files, `noasm` fallback elsewhere | Yes, native `image.YCbCr` | MIT | Young (v0.3.1, 2025), single maintainer, ~5 stars |
| 2 | `pixiv/go-libjpeg` + libjpeg-turbo | ~4.8x faster (9.56 ms vs 45.8 ms on the pixiv corpus) | cgo | Breaks clean cross-build; needs C cross-toolchain and libjpeg-turbo per target | Yes, `*image.YCbCr` (padded 16px strides) | BSD-3-Clause (binding), IJG/BSD (library) | Library active; binding dormant since 2019 |
| 3 | stdlib + romshark unroll patch (Go PR 71618) | ~1.11x to 1.16x faster | cgo-free | None | Yes | BSD | Merged/tracked upstream |
| 4 | Go stdlib `image/jpeg` | 1.0x baseline | cgo-free | None | Yes | BSD | Go core; SIMD rejected by policy |
| 5 | `gen2brain/jpegli` (WASM/wazero) | ~stdlib speed (tie, within ~1%) | cgo-free (WASM under wazero) | None | Yes, `image.YCbCr` | BSD-3-Clause | Maintained but thin, ~55 stars |

Notes on the numbers. The jpegn and jpegli ns/op figures come from the projects'
own benchmarks (jpegn on a baseline 4:2:0 image, jpegli on a 512x512 image), so
read them as ratios, not as 7 MP absolutes. The pixiv/go-libjpeg figures were
measured on the pixiv corpus (around 1080p). The romshark patch figures are
measured on large real images (6 MP to 100+ MP).

### The tradeoff, stated plainly

Three routes, three costs.

**Pure-Go SIMD (`gen2brain/jpegn`).** Cross-compiles with no change to the Nix
flake or GoReleaser. Faster than stdlib but not near-native: the win is about
1.85x to 2.45x, driven by the SIMD IDCT (74% faster in isolation) and SIMD
upsample. On a CPU without AVX2/NEON, or with the `noasm` tag, it falls back to a
pure-Go IDCT that is level with stdlib. This is the low-risk route.

**cgo libjpeg-turbo (`pixiv/go-libjpeg`).** The fastest decoder, about 4.8x over
stdlib, and the YCbCr fit is clean. The cost is structural, not incidental:
`CGO_ENABLED=1` makes `GOARCH=arm64 go build` on an amd64 host stop working, so
you add a C cross-toolchain per target (Zig cc or Nix `pkgsCross`), build
libjpeg-turbo for each arch, add a musl/static track to keep the single-binary
property, and rework the GoReleaser matrix. You also inherit a binding untouched
since 2019. The decoder quality is not the problem; the build tax is.

**WASM/jpegli (`gen2brain/jpegli`).** Cgo-free and near-native in theory, but not
in practice for decode. Two reasons stack. First, jpegli's decoder is not faster
than libjpeg-turbo; Google recommends libjpeg-turbo for decompression and claims
only encode parity for jpegli. Second, the wazero WASM boundary adds runtime
overhead that cancels any native SIMD gain, plus a per-call module instantiation
and a full-frame memory copy in and out of linear memory. It measures at stdlib
speed on a 512x512 image, and the copy cost grows linearly toward 7 MP. No IDCT
win.

#### Expected 7 MP decode time

Anchored to stdlib at about 56 ms per 7 MP frame:

- **stdlib**: ~56 ms (baseline).
- **`gen2brain/jpegn`**: ~23 ms to ~30 ms (from the 1.85x to 2.45x range).
- **`pixiv/go-libjpeg` + libjpeg-turbo**: ~12 ms (from 4.8x; the SIMD advantage
  holds or grows with pixel count as per-call cgo overhead amortises).
- **`gen2brain/jpegli` (WASM)**: ~56 ms (stdlib-equivalent, plus per-frame WASM
  overhead that worsens toward 7 MP).
- **jpegli float decoder (reference only)**: ~153 ms (from 47 MP/s), far worse;
  wrong tool for a decode-latency workload.

### Adoption spike [DONE]

The amd64 spike completed and `gen2brain/jpegn` was adopted. The criteria below
record the pre-adoption test. Only real-arm64 performance validation remains, in
[`TODO.md`](./TODO.md#1-validate-the-stage-3-browser).

Historical criteria:

- **Input**: a real 7 MP Chromium screencast JPEG pulled from a capture bundle,
  decoded on both amd64 and arm64.
- **What to measure**: wall-clock decode latency versus stdlib on the same frame,
  and a correctness check that the decoded `image.YCbCr` matches stdlib output
  across the corpus (jpegn already falls back to stdlib on unsupported inputs).
- **Go criterion**: jpegn beats stdlib's ~56 ms per frame by enough to matter,
  meaning at least the ~1.85x floor (~30 ms or better) on both arches, with the
  build staying cgo-free and correctness matching stdlib on the corpus.
- **No-go criterion**: the measured win drops toward 1.0x (for example the SIMD
  path does not engage, or the frames decode via the `noasm` fallback), or a
  corpus frame decodes wrong. If jpegn no-goes and the latency wall still
  matters, weigh the cgo libjpeg-turbo route deliberately, accepting the
  cross-compile and packaging cost as the price of ~4.8x.
- **Cheap parallel win**: the romshark unzig/shift-clamp unroll (Go PR 71618)
  gives about 11% to 16% on large real images with no asm, no cgo, and no
  allocation change. Vendor it or await upstream while the jpegn spike runs.

One structural note. Multi-threaded decode of a single JPEG is not on the table:
Huffman entropy decode is serial, and per-interval threading needs restart
markers that most JPEGs lack. Nyala already runs one frame per worker, so the
available parallelism is captured.

### Sources

- Go stdlib decoder favours simplicity over speed; SIMD IDCT rejected by policy;
  hot-function profile (`DrawYCbCr`, `reconstructBlock`, `idct`, `decodeHuffman`):
  https://github.com/golang/go/issues/24499 and
  https://go-review.googlesource.com/c/go/+/167417 and
  https://go.dev/src/image/jpeg/dct.go
- stdlib unzig/shift-clamp unroll patch (~11% to 16% on large images):
  https://github.com/golang/go/pull/71618 and https://github.com/romshark/jpegbench
- `gen2brain/jpegn` repo, benchmarks (baseline 420 ~1.85x, IDCT -74%, upsample),
  SIMD `.s` files, `image.YCbCr` return, MIT licence:
  https://github.com/gen2brain/jpegn and
  https://github.com/gen2brain/jpegn/tree/main and
  https://github.com/gen2brain/jpegn/blob/main/idct_amd64.go and
  https://github.com/gen2brain/jpegn/blob/main/jpegn.go and
  https://libraries.io/go/github.com%2Fgen2brain%2Fjpegn and
  https://pkg.go.dev/github.com/gen2brain/jpegn
- `m8rge/go-scaled-jpeg` scaled-IDCT decoder, mixed MIT/IJG licence:
  https://github.com/m8rge/go-scaled-jpeg
- libjpeg-turbo performance (2x to 6x libjpeg via SIMD), licence:
  https://libjpeg-turbo.org/ and https://libjpeg-turbo.org/About/Performance and
  https://doi.org/10.48550/arxiv.1311.5304
- `pixiv/go-libjpeg` speed (1.9x libjpeg, 4.8x turbo; 9.56 ms vs 45.8 ms), YCbCr
  planes and 16px alignment, BSD-3-Clause, dormant since 2019:
  https://github.com/pixiv/go-libjpeg and
  https://pkg.go.dev/github.com/pixiv/go-libjpeg/jpeg and
  https://github.com/pixiv/go-libjpeg/blob/master/jpeg/decompress.go
- `antonini/golibjpegturbo` (~6x, RGBA only, stale since 2016):
  https://github.com/antonini/golibjpegturbo
- cgo cross-compile cost, Zig cc, Nix pkgsCross, static/musl, GoReleaser:
  https://github.com/zengxs/cross-cgo and
  https://johncodes.com/archive/2026/02-11-cross-compiling-cgo/ and
  https://github.com/joncfoo/cross-compile-go and
  https://dev.to/kristoff/zig-makes-go-cross-compilation-just-work-29ho and
  https://github.com/goreleaser/example-zig-cgo and
  https://wiki.nixos.org/wiki/Cross_Compiling and
  https://github.com/NixOS/nixpkgs/blob/master/doc/stdenv/cross-compilation.chapter.md and
  https://github.com/NixOS/nixpkgs/blob/release-24.11/pkgs/by-name/li/libjpeg_turbo/package.nix
- `gen2brain/jpegli` WASM decoder at stdlib speed, wazero overhead, per-call
  instantiation, YCbCr return, BSD-3-Clause:
  https://github.com/gen2brain/jpegli and
  https://github.com/gen2brain/jpegli/blob/main/jpegli_wazero.go and
  https://pkg.go.dev/github.com/gen2brain/jpegli and
  https://github.com/gen2brain/webp and https://wazero.io/docs/how_the_optimizing_compiler_works/
- jpegli decode not faster than libjpeg-turbo; Google recommends libjpeg-turbo
  for decompression; float decoder ~47 MP/s:
  https://www.mail-archive.com/libjpeg-turbo-users@googlegroups.com/msg00174.html and
  https://opensource.googleblog.com/2024/04/introducing-jpegli-new-jpeg-coding-library.html and
  https://crates.io/crates/jpegli-rs/0.2.2 and https://github.com/google/jpegli
- Single-image parallel decode limited by serial Huffman; restart markers rare:
  https://github.com/libjpeg-turbo/libjpeg-turbo/issues/147 and
  https://stackoverflow.com/questions/8748671/jpeg-restart-markers and
  https://scispace.com/papers/design-implementation-and-evaluation-of-a-task-parallel-jpeg-4k0eed5nkj
- Per-byte decode scaling on larger images:
  http://blog.loopbio.com/video-io-2-jpeg-decoding.html
