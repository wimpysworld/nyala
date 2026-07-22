# Nyala Performance

> Historical note: bounded recording commands and bundle metrics in this
> research record were retired by the diagnostics consolidation. Use
> `just diagnostics` for current correctness validation and `just fps` for live
> throughput measurement.

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
2. **Graphics letterbox** came from fitting a bitmap that was already sized to
   the full grid through a retired 1:2-aspect path that shrank it.
3. **Original slow, tearing graphics** came from a full re-encode and full re-transmit every
   frame, an unbuffered writer (~1800 syscalls/frame), no synchronised-output
   fencing, and, for Sixel, a per-frame palette rebuild that dominates the
   ~593 ms cost.

The reference projects agree on three durable ideas: coalesce frames, reuse buffers,
and avoid redundant terminal bytes. Nyala now passes each live Kitty CDP PNG straight
to terminal framing, so the old transmit-once animation-frame plan does not apply.
Live Sixel now uses a retained opaque Nyala encoder derived from `mattn/go-sixel`
v0.0.12, with its adaptive 256-colour policy and dithering off. It receives PNG for
every live frame. The former moving-JPEG fused-RGBA path measured 103.014 ms matched
sequential processing and 88.637 ms State processing before terminal work. Static
fixed-255 reuse later cut warm State time by 45.8% with identical output, but a
per-frame palette rebuild erased the gain. A later still-image matrix found a
stateless alternative: a 64K median-cut sample cap cuts combined q90 decode plus
encode by 30.39% against the prior parallel-LUT production path. A deterministic
phase sweep records colour-category shifts, but
every feature remains visible and structurally intact, so the performance win is
retained. Parallel construction of the unchanged 15-bit LUT cuts the same path by
6.21% with byte-identical output. The later combined RGB555 mapping and bounded band
emitter cut production-shaped full-encode p90 by 28.22% to 49.63% and are retained.
Long-count RLE is retained with an 8192 repeat ceiling.
Kitty backend optimisation is complete. Partial Kitty transport remains closed unless
the sustained real-terminal matrix exceeds 16.67 ms per frame in write, transport,
terminal decode, or draw and a supported protocol path exists; see
[`TODO.md`](./TODO.md#2-improve-graphics-transport-where-measurements-justify-it).

The full headless `pprof` and five-sample `go test -bench` pass completed on
2026-07-14. Its measured rankings, limits, and profile-gated decisions are in
[the dated record below](#full-headless-profile-and-benchmark-pass-2026-07-14).
The accepted direct native-YCbCr Block follow-up and its matched q80-q100 sweep
are in [the same record](#direct-native-ycbcr-block-grid-2026-07-15).
The colour-conversion follow-up and the resulting Block and Sixel benchmark order
are in [the 2026-07-15 research record](#colour-conversion-research-2026-07-15).
Real-terminal decode, draw, write, FPS, and tearing measurements remain separate
pending work in [`TODO.md`](./TODO.md#1-validate-the-stage-3-browser).

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

Current status: direct CDP PNG pass-through, protocol chunking, reusable framing
buffers, single-write assembly, double-buffered IDs, and SIMD base64 are complete.
The historical fallback RGB/zlib encoder has been removed. Framing p90 is 0.008 ms at 1920x1080 and 0.013 ms at
2560x1600, with 10 allocations and unchanged payload and allocation classes. These
are framing measurements, not terminal decode or draw measurements. The findings
below record the older decoded encoder and the work that replaced it.

- KEY (the letterbox / item 6 "best fit"). The removed graphics placement path
  assumed a 1:2 cell aspect. But the engine already sizes the bitmap to
  `cols*cellW x rows*cellH`
  (`internal/engine/launch.go:73-78`, `internal/render/geometry.go:42-44`), so it
  maps one-to-one onto the grid. On any non-1:2 cell (e.g. 16x33), the former fit
  shrank and centred the columns, which letterboxed the view. The viewer now
  encodes the engine-sized graphics bitmap directly from the top-left.
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

### Theme 4 - Sixel renderer (~593 ms/frame, historical)

This profile predates the 2026-07-15 encoder adoption. The live path then used
`charmbracelet/x/ansi/sixel`. Production now uses Nyala's opaque encoder derived
from `mattn/go-sixel`; see the
[matched adoption record](#sixel-encoder-adoption-2026-07-15).
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
  for Kitty, Sixel, and every capture. Block uses JPEG quality 90 because its
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

The wire format now follows the backend. Live Block requests JPEG quality 90. The
direct native-YCbCr path retains measured processing
headroom while improving fidelity over the former q80 default. Live Kitty always
requests PNG and passes Chromium's owned base64 payload straight to Kitty
`f=100,t=d,a=T` framing. Live Sixel decodes PNG for every frame. This supersedes the
former settled-PNG/moving-JPEG state machine. Diagnostic capture remains lossless PNG
on every backend.

The frame seam no longer expands every decode to a full RGBA plane. `Frame.Img` is
`image.Image`: JPEG stays as the decoder's `*image.YCbCr`, and PNG keeps its native
decoder layout. Block now averages the native YCbCr planes directly into its final
cell grid, with no intermediate RGBA sample surface. Sixel converts at the encoder
boundary. Capture converts where its raster and bundle paths require RGBA. Live Kitty
carries no decoded image on the direct path; `EncodedPNG` holds the CDP string. A
backend switch decodes that retained PNG if the queued frame reaches Block or Sixel.

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

**Kitty.** Live frames use only direct PNG. Nyala rechunks Chromium's
owned base64 PNG as `f=100,t=d,a=T`, alternates image IDs, and deletes the previous
ID after placement. The earlier `a=f,r=1,o=z` edit was tried and reverted because it
rendered partially or black on target terminals. The decoded RGB live encoder and
its retained scratch have been removed. A live JPEG fallback is not required: the owner's
measurement put direct PNG pass-through at about 1 ms. Reconsider it
only if terminal-side PNG decoding or drawing becomes a measured bottleneck. Any
future partial update must use a supported protocol path and beat direct PNG end to
end.

**Sixel.** Production uses a pure-Go MIT encoder copied from `mattn/go-sixel`
v0.0.12 and narrowed to Nyala's opaque CDP frames. A retained encoder owns its
scratch and complete parameterised DCS, uses the same adaptive 256-colour policy,
and disables dithering.
The matched q90 State benchmark is 9.45x faster than the former
`charmbracelet/x/ansi/sixel` path and cuts allocations by 99.993%.

The former moving-q90 Sixel path asked `jpegn` for fused RGBA when the session used
four decode workers or fewer. Its matched sequential path fell 10.43%, but the
PNG-only production decision removed that JPEG-specific branch and worker cutoff.
The historical result remains in the
[four-worker policy](#bounded-sixel-rgba-decode-and-four-worker-policy-2026-07-15).

This supersedes the earlier prediction that an encoder swap would be only a
partial win. The five-image quality set chose 256 colours over 192 because SSIM
was higher on every image and grayscale quality was safer. Dithering was slower,
larger, and lower quality. The later fixed-palette track proved a static reuse gain,
but temporal quality and refresh cost remain open. Sixel is a
compatibility backend, so the authorised speed and colour-quality trade-off favours
the adopted encoder. The full evidence is in the
[dated adoption record](#sixel-encoder-adoption-2026-07-15).

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
4b. **[DONE - 67936b1]** **Add the lossless PNG screencast path.** PNG is the
   capture, Sixel, and direct Kitty format. The backend policy at that completion
   point kept JPEG quality 80 for Block and later added moving q90 Sixel JPEG. The
   2026-07-15 decision raised Block to q90; the 2026-07-17 decision returned all
   Sixel frames to PNG. See Theme 7.

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

5. **[DONE - ee99ab7 and direct PNG follow-ups]** **Kitty flash fix + transmit
   optimise.** The per-frame
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
   general-purpose browser, so BestSpeed. This cut the historical paint wall but did
   not remove it: the encode remained the wall at 115 ms of the 172 ms budget, with
   the write about a third (57 ms). At that point damage/dirty-rect and motion downscaling were candidate
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
   [DONE - current] Live Kitty now uses only direct PNG: every Kitty stream requests
   PNG and rechunks the CDP base64 payload as `f=100`. The historical decoded
   encoder and its fallback path have been removed. The owner's measurement put
   direct PNG pass-through at about 1 ms, so a JPEG Kitty fallback is
   not required. Reconsider it only if terminal-side PNG decoding or drawing becomes
   a measured bottleneck. Backend optimisation is complete. Sustained Kitty,
   WezTerm, and KMSCON measurements and Kitty-through-multiplexer placement remain
   validation and correctness work, not unfinished encoder optimisation.
6. **[DONE - staged] Sixel 64K per-frame palette sampling.** A ten-cell still-image
   matrix cuts mean combined q90 decode plus encode by 30.39% against the prior
   parallel-LUT production path, but output quality is
   mixed and mean payload rises 1.75%. A deterministic phase sweep records colour
   shifts on thin lines, badges, blocks, icons, and antialias shades, while all 800
   features remain above 3:1 and preserve structure. Retain the cap. Reopen only if
   content disappears, merges with its background, becomes unreadable, or loses
   structure. The former temporal palette-reuse motion fixture is not required.
7. **[DONE - 3861ccc]** **Accumulate scroll keys** instead of latest-wins-drop
   (`internal/viewer/stream.go`). `keySender` now accumulates every scroll key and
   forwards it in order, so the engine's `dispatchWheel` sums the deltas; a burst no
   longer collapses to one step (down, up, page, and horizontal, sign preserved).
   Direct fix for sluggish scroll. Impact 7, effort medium.
8. **[DONE - 2026-07-16] Sixel encoder adoption.** State retains Nyala's opaque
   encoder derived from `mattn/go-sixel` and switches its destination through a writer proxy.
   Adaptive 256-colour output with dithering off cuts the matched median from
   960.437 to 101.670 ms and payload by 18.47%. See the
   [dated adoption record](#sixel-encoder-adoption-2026-07-15). The bounded
   `jpegn` RGBA follow-up then cuts matched sequential processing from 115.005 to
   103.014 ms; see the
   [four-worker policy](#bounded-sixel-rgba-decode-and-four-worker-policy-2026-07-15).
9. **[DONE - 9c091c6]** **Cheap viewer allocs:** drop per-cell `fmt.Sprintf` (build
   with `strconv.AppendInt` into reused buffers, byte-compare against a cache so run
   coalescing stays exact in every colour mode) and `string(glyph)` (use
   `utf8.AppendRune`). `BenchmarkGridDiff` on a full-motion 120x40 truecolour grid:
   14440 -> 40 allocs/op, 230861 -> 678 B/op, ~3.3x faster; output byte-identical
   (golden track green). Impact 6, effort low.
15. **[DONE, Sixel policy superseded] Adaptive screencast quality.** The earlier
    policy requested lossy JPEG while the page moved and lossless PNG after settle.
    The aquarium
    capture measured PNG at ~1.6 MB and 14-23 ms/frame on photographic full-motion
    content, where the damage-diff gives nothing; PNG stays best for typical and idle
    pages. MEASURED again (bbc, kitty, live): `in_decode_mean_ms ~34`, over the 16.6 ms
    60fps budget, so PNG decode was the ingest ceiling once the paint wall (5) cleared.
    The current policy is simpler: Block uses JPEG quality 90, while Sixel, Kitty,
    and capture use PNG for every frame. The Sixel state machine and its JPEG decode
    policy were removed after the sustained PNG/JPEG sweep. See Theme 7.

### Tier 3 - larger and strategic

10. **Conditional partial graphics emission.** Damage tracking, dirty tiles, and
    partial Sixel output are excluded. For Kitty, first measure terminal decode, draw,
    write, transport, and bytes per frame. Reopen partial Kitty research only if the
    sustained full-frame path exceeds 16.67 ms and a supported protocol design can
    prove an end-to-end win. Direct Kitty PNG has no decoded bitmap, the tested `a=f`
    edit was not portable, and no supported design exists. Do not decode every PNG to
    recreate the old plan.
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
ceiling. At that point, paint (Out ~24) was the wall: the Kitty encode on the large
live surface.
The klauspost zlib BestSpeed swap (recommendation 5) cut that encode, measured 22%
off encode-plus-write, and the encode/write pipeline (recommendation 5) hid the write
behind the encode, measured `5.8 -> 8.5` out fps. Neither removed the wall: two co-equal
~115 ms limits then bounded paint, our encode CPU (zlib of the 22 MB frame) and the
terminal ingest. Direct PNG later removed Nyala's decoded Kitty encode. The former
JPEG-in-motion recommendation is superseded by Sixel's PNG-only decision. The next task
is to measure the terminal's PNG decode and draw; graphics downscaling is outside the
active roadmap.

### Idea 2 - backend-specific screencast

Built for the pixel consumers. Block uses JPEG quality 90. Sixel stays PNG for all
motion and idle frames. Kitty also stays PNG because it
passes the CDP payload to the terminal without a Nyala decode. The owner's measurement
put that pass-through at about 1 ms, so a JPEG Kitty path
is not required. Reconsider it only if terminal-side PNG decoding or drawing becomes
a measured bottleneck.

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

Baseline, before any GPU work: `just record https://webglsamples.org/aquarium/
aquarium.html frames=3 backend=block` measured **5.9 sustained fps** (3 usable frames over
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

Field semantics, for reading a summary line correctly. `in_*` is stamped at
receive on the listener goroutine, before any queue or drop; `out_*` is stamped
per assembled paint after first contentful paint. Each `*_mean` is
whole-session, `(frames-1)/span`. `*_min`, `*_max`, and `*_median` are
instantaneous, from single inter-frame gaps: one worst gap sets the min, one
back-to-back pair sets the max, so neither is a sustained rate. `out_encode`
is Nyala's framing cost. Under the pipelined writer, `out_write` measures the
terminal's true ingest rate, not Nyala work (recommendation 5's nuance above).
A gap between in and out frame counts is the latest-wins coalescer working as
designed: a frame superseded before assembly is never painted. The later
`ack_rtt_mean_ms` and `ack_rtt_max_ms` fields time the screencast ack round
trip in the engine's ack pump; they matter only when the rtt approaches half
the frame interval.

MEASURED (bbc.co.uk, kitty, pgup/pgdn): In median ~40 fps, peak ~59; Out 2 to 6 fps.
At that point, paint was the binding constraint on screen, not ingest. The historical
paint split was decisive:
`out_encode_mean_ms ~191` (our Kitty escape build) against `out_write_mean_ms ~8` (the
terminal). Our encode was the wall, not the terminal or tmux. `in_decode_mean_ms ~34`
was the ingest ceiling, over the 16.6 ms 60fps budget. The two levers were, in order:
cut the Kitty encode (recommendation 5, bulk `f=24` and buffer reuse, plus the
klauspost zlib BestSpeed swap on the live path, measured 22% off encode-plus-write on
a ~22 MB frame), then the now-superseded JPEG-in-motion experiment (recommendation 15).
Direct CDP PNG pass-through supersedes this decoded-encoder bottleneck record.

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
2. [DONE, superseded] JPEG-in-motion was removed from Sixel after the sustained
   PNG/JPEG sweep. Block stays JPEG quality 90. Sixel and Kitty stay PNG.
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

### Core-adaptive decode-worker default (2026-07-07)

At this date, `defaultDecodeWorkers()` returned
`max(2, runtime.GOMAXPROCS(0)/2)`. The current bounded default is
`min(4, max(2, runtime.GOMAXPROCS(0)/2))`; it resolves to 2 on four cores and 4
from eight cores upwards. `NYALA_DECODE_WORKERS` remains a manual 1 to 16 override.
The cap bounds live decode concurrency; Sixel's PNG format does not depend on it.

### Terminal ceiling found (2026-07-07)

On the block backend Nyala emits a frame in ~1.7 ms (`out_encode`), so the wall on a
slow terminal is the terminal, not Nyala. WezTerm is byte-bound: truecolour SGR on a
full-motion 2560x2880 grid pushed `out_write` to ~20 ms (Out ~40), while `--color=256`
(fewer SGR bytes) dropped `out_write` to 0.4 ms and locked Out at 60. Lower resolution
does the same (2560x1440 holds 60 at sextant + truecolour). So `just fps` now measures
each terminal's bytes-per-frame ceiling: kitty, Alacritty, foot hold 60 at 2560x2880;
WezTerm and Ghostty saturate sooner. `--color=256` is the documented lever for
byte-bound terminals; no code change needed. The Sixel counterpart, emulator
write ceilings across mlterm, Alacritty, and Contour on the same payload, is
recorded under "Sixel palette reuse (2026-07-22)".

### Full headless profile and benchmark pass (2026-07-14)

The pass ran at commit `edbc6f5` on Go 1.26.4, linux/amd64, Linux 7.1.2,
Chromium 149.0.7827.200, and an AMD Ryzen AI Max+ 395 with 16 cores, 32 threads,
and 64 MiB L3. Each suite ran serially against the others. Tables report the
median of five samples with `-benchmem`.

Exact benchmark commands:

```text
go test -run '^$' -bench '^BenchmarkRenderers$' -benchmem -benchtime=1s -count=5 ./internal/metrics
go test -run '^$' -bench '^BenchmarkDownscale' -benchmem -benchtime=1s -count=5 ./internal/metrics
go test -run '^$' -bench '.' -benchmem -benchtime=1s -count=5 ./internal/render/block
CGO_ENABLED=0 go test -run '^$' -bench '^BenchmarkDecodePoolThroughput$' -benchmem -benchtime=1s -count=5 ./internal/engine
CGO_ENABLED=0 go test -run '^$' -bench '^BenchmarkRenderStreamFrame' -benchmem -benchtime=1s -count=5 ./internal/viewer
CGO_ENABLED=0 go test -tags live -run '^$' -bench '^BenchmarkCDPScreencastDecode$' -benchmem -benchtime=2s -count=5 ./internal/engine
CGO_ENABLED=0 go test -tags live -run '^$' -bench '^BenchmarkCDPNativeFrame$' -benchmem -benchtime=1s -count=5 ./internal/engine
```

Pinned 240x160 renderer comparison:

| Renderer | ms/op | B/op | allocs/op | bytes/frame |
|---|---:|---:|---:|---:|
| Block | 0.923771 | 948,408 | 41,734 | 182,116 |
| Sixel | 18.472358 | 7,543,307 | 79,114 | 23,004 |
| Kitty | 2.514828 | 1,750,077 | 76,882 | 164,349 |

`BenchmarkRenderers` pins `GOMAXPROCS=1` and drives the generic decoded-image
`Renderer` seam. Its results are not comparable with the parallel live Block path
or direct-PNG live Kitty.

Block, downscale, and viewer paths:

| Benchmark | ms/op | B/op | allocs/op |
|---|---:|---:|---:|
| Downscale serial 120x40 | 3.206531 | 0 | 0 |
| Downscale parallel 120x40 | 0.491850 | 0 | 0 |
| Downscale serial 240x86 | 14.059426 | 0 | 0 |
| Downscale parallel 240x86 | 1.449950 | 0 | 0 |
| Compose quadrant 120x40 | 0.220458 | 0 | 0 |
| Compose parallel quadrant 120x40 | 0.054401 | 0 | 0 |
| Compose quadrant 240x86 | 0.943217 | 0 | 0 |
| Compose parallel quadrant 240x86 | 0.137701 | 0 | 0 |
| Compose sextant 120x40 | 0.180430 | 0 | 0 |
| Compose parallel sextant 120x40 | 0.046671 | 0 | 0 |
| Compose sextant 240x86 | 0.785666 | 0 | 0 |
| Compose parallel sextant 240x86 | 0.115816 | 0 | 0 |
| Diff 120x40 | 0.132771 | 639 | 39 |
| Diff 240x86 | 0.583436 | 1,375 | 85 |
| Overlay 120x40 | 0.122987 | 0 | 0 |
| Overlay 240x86 | 0.275170 | 0 | 0 |
| Viewer unchanged | 0.325986 | 0 | 0 |
| Viewer changed full repaint | 0.485812 | 197,518 | 41 |

The extra Block compatibility benchmarks returned `ComposeQuadrant` 0.220285 ms,
`ComposeSextant` 0.181007 ms, cached overlay 0.340626 ms, uncached overlay
0.416589 ms, and `GridDiff` 0.132514 ms. The production parallel Compose maximum
above is 0.137701 ms, 0.83 percent of the 16.67 ms frame budget. The changed viewer
benchmark resets the differ on every iteration; its allocations describe forced
full repaint, while the unchanged steady-state path allocates nothing.

The synthetic fixed-5-ms decode-pool benchmark returned 5.109203, 2.561417,
1.702833, and 1.279973 ms/op for one through four workers. It confirms parallel
delivery of independent jobs. It does not measure JPEG latency or a real delivered
frame interval.

Real-CDP 2560x2880 format results:

| Format | payload bytes | native decode ms | current decode ms | Block ms | Block B/op | Block allocs/op | CDP p50 ms |
|---|---:|---:|---:|---:|---:|---:|---:|
| PNG | 1,255,170 | 18.925303 | 19.062252 | 21.558624 | 31,858,702 | 379 | 62.250 |
| JPEG q80 | 425,401 | 7.668980 | 7.782389 | 16.201964 | 11,879,251 | 18 | 56.330 |
| JPEG q85 | 477,761 | 8.200747 | 8.391783 | 16.818776 | 11,936,666 | 18 | 57.490 |
| JPEG q90 | 563,559 | 9.047259 | 9.217286 | 17.790463 | 12,018,446 | 18 | 58.770 |
| JPEG q95 | 730,950 | 10.760010 | 10.904083 | 19.431912 | 12,190,518 | 18 | 60.910 |
| JPEG q100 | 1,167,773 | 14.883941 | 15.051112 | 23.498926 | 12,626,449 | 20 | 70.820 |

The q80 Block arm covers base64 decode, JPEG decode, and fused 320x270 sampling.
Its serial 16.201964 ms cost uses 97.19 percent of the budget and leaves 0.468 ms.
The CDP p50 covers Chromium encoding, base64 creation, transport, and scheduling;
it is neither Nyala decode latency nor FPS. Each timed operation reuses one captured
payload, so the decode figures are warm-cache throughput.

`BenchmarkCDPNativeFrame` passed every format. Its production direct-PNG Kitty arm
measured 0.027523 ms, 320 B/op, 12 allocs/op, 1,255,170 input bytes, and 1,677,273
terminal bytes. Its `current-rgba` and `proposed-native` labels are historical: the
current decode already keeps native layouts. The Sixel current and proposed rows are
headless encode arms. They remained near 1.0 second/op with about 14.8 million
allocations/op, and exclude terminal decode, draw, and tearing.

Exact profile commands:

```text
# Corrected CPU-only q80 Block profile
CGO_ENABLED=0 go test -tags live -run '^$' -bench '^BenchmarkCDPScreencastDecode$/^jpeg-q80$/^block-decode-320x270$' -benchmem -benchtime=5s -count=1 -cpuprofile /tmp/nyala-perf-edbc6f5/q80-block-clean.cpu.pprof ./internal/engine

# Original allocation-profile capture; its CPU samples are not used
CGO_ENABLED=0 go test -tags live -run '^$' -bench '^BenchmarkCDPScreencastDecode$/^jpeg-q80$/^block-decode-320x270$' -benchmem -benchtime=5s -count=1 -cpuprofile /tmp/nyala-perf-edbc6f5/q80-block.cpu.pprof -memprofile /tmp/nyala-perf-edbc6f5/q80-block.mem.pprof -memprofilerate=1 ./internal/engine

# Other representative profiles
CGO_ENABLED=0 go test -run '^$' -bench '^BenchmarkRenderStreamFrameChanged$' -benchmem -benchtime=10s -count=1 -cpuprofile /tmp/nyala-perf-edbc6f5/viewer-changed.cpu.pprof -memprofile /tmp/nyala-perf-edbc6f5/viewer-changed.mem.pprof -memprofilerate=1 ./internal/viewer
go test -run '^$' -bench '^BenchmarkBlockCompose$/^quadrant$/^240x86$' -benchmem -benchtime=10s -count=1 -cpuprofile /tmp/nyala-perf-edbc6f5/compose-quadrant.cpu.pprof -memprofile /tmp/nyala-perf-edbc6f5/compose-quadrant.mem.pprof -memprofilerate=1 ./internal/render/block
go test -run '^$' -bench '^BenchmarkBlockCompose$/^sextant$/^240x86$' -benchmem -benchtime=10s -count=1 -cpuprofile /tmp/nyala-perf-edbc6f5/compose-sextant.cpu.pprof -memprofile /tmp/nyala-perf-edbc6f5/compose-sextant.mem.pprof -memprofilerate=1 ./internal/render/block
```

The corrected q80 Block CPU-only profile measured 16.164015 ms/op and put
`averageYCbCrToRGBA` first at 49.81 percent flat and 50.94 percent cumulative.
The next leaders were `jpegn.idctAVX2` at 11.99 percent flat, `jpegn.getVLC` at
11.24 percent flat and 19.98 percent cumulative, and `jpegn.decodeBlock` at 8.36
percent flat and 41.70 percent cumulative. GC background marking was 2.00 percent
cumulative CPU.

The retained allocation profile is read only for allocation-space evidence.
Allocation space was `jpegn.decodeSOF` 89.21 percent, `jpegn.readAllData` 3.44
percent, `image.NewRGBA` 2.86 percent, and `bytes.growSlice` 2.28 percent.

At that commit, the remaining fused output allocation was one 320x270 RGBA result,
345,600 bytes per frame. The direct native-YCbCr follow-up below removes it. The old
full-resolution RGBA surface was already gone, so a reusable RGBA ring had no
material GC case and recommendation 4 remains closed.

The viewer changed-frame profile put `areaAverageBand` at 56.71 percent flat and
59.28 percent cumulative, but that synthetic benchmark feeds a full 960x640 RGBA
surface. At that commit, production q80 frames arrived at sub-cell RGBA size and
skipped this viewer downscale. Current Block frames arrive as full-size native
YCbCr and compose directly into the final grid, so they also bypass this RGBA
downscale. Changed-frame allocation space was 99.42 percent `GridDiff.retainPrev`;
96.86 percent of allocation objects came from `writeCUP`.

The 2026-07-16 follow-up replaced `writeCUP`'s local `[16]byte` with one
`GridDiff`-owned `[16]byte` scratch array. The emitted stream is byte-identical.
A focused comparison covered every row and column from 0 through 999 against
the former encoder. Six fresh benchmark processes ran in rotated
baseline/candidate order with `GOMAXPROCS=4`, `GOGC=100`, four discarded warm-ups,
and 30 timed one-second samples per variant. At 120x40, median time moved from
131,533 to 130,668 ns/op
(-0.66 percent), p90 from 133,400 to 132,160 ns/op (-0.93 percent), and
allocations from 39 and 639-640 B/op to zero. At 240x86, median moved from
578,443 to 577,406 ns/op (-0.18 percent), p90 from 583,425 to 585,538 ns/op
(+0.36 percent), and allocations from 85 and 1,375 B/op to zero. The small p90
movement at 240x86 is within run variation; the row-scaled CUP allocations are
gone without a latency regression. The fixed array also keeps the first CUP
allocation-free. These p90 values measure sustained in-process diff encoding,
not terminal presentation or end-to-end per-frame tail latency.

The 2026-07-16 True Colour follow-up keeps `GridDiff.Emit` fully generic for
xterm-256, monochrome, wrappers, and other callers. The viewer explicitly selects
`GridDiff.EmitTrueColour` only for `ColourTrue`. That path calls the concrete
appenders directly and tracks RGB state without rebuilding, comparing, and
copying encoded SGR bytes. Alpha remains absent from terminal colour state, so
alpha-only differences do not add an SGR change after RGB state is seeded. A
generic-wrapper comparison covers unchanged, foreground-only, background-only,
both-colour, alpha-only, sparse, row-separated, full-motion, overlay, wide,
non-ASCII, reset, resize, and error transitions byte-for-byte.

Each benchmark series used six fresh processes in rotated baseline/candidate order
with `GOMAXPROCS=4`, `GOGC=100`, four discarded warm-ups, and 30 timed one-second
samples per variant. Production-shaped changed-frame median/p90 moved from
668,816/678,684 to 643,955/655,490 ns at 120x40 (-3.72/-3.42 percent), and from
2,803,482/2,859,927 to 2,705,675/2,751,554 ns at 240x86 (-3.49/-3.79 percent).
The unchanged paths moved -0.02/-0.16 percent and -0.34/-0.47 percent. Isolated
`BenchmarkBlockDiff` median/p90 improved -22.61/-22.91 percent at 120x40 and
-24.07/-25.41 percent at 240x86. There were zero allocs/op in all samples; B/op
was zero except one 2 B/op baseline sample. Both arms also match at 9,240 SGRs,
178,579 payload bytes, and one outer-writer call for 120x40, and 40,506 SGRs,
782,829 bytes, and one call for
240x86 changed frames; unchanged frames report zero for all three counters.

A bounded follow-up combines foreground and background parameters into one SGR
when both RGB states change. Foreground-only and background-only sequences stay
unchanged. The `charmbracelet/x/vt` emulator parsed separate and combined streams
at different byte boundaries and produced equal cells, RGB rendition, cursor
positions, text, and wide-cell state across the transition corpus. Exact sequence
and writer-error tests also pass.

Six fresh processes per arm ran in rotated order without another benchmark load,
using the same environment, warm-up, and sample counts above. Changed-frame
median/p90 moved from 642,497/654,780 to 631,157/646,725 ns at 120x40
(-1.76/-1.23 percent), and from 2,715,011/2,751,996 to
2,645,464/2,708,548 ns at 240x86 (-2.56/-1.58 percent). The three retained
candidate process medians were below every baseline process median at both sizes.
Unchanged median/p90 moved -0.34/+0.77 percent and -0.36/-0.29 percent, with
overlapping process ranges. Isolated diff median/p90 moved -2.19/-2.72 percent
and -1.46/-0.43 percent; its process-median ranges were also fully separated.
All samples had zero allocs/op; B/op was zero except three 2 B/op samples. The
120x40 changed frame fell from 9,240 SGRs and 178,579 bytes to 4,800 SGRs and
169,699 bytes; 240x86
fell from 40,506 SGRs and 782,829 bytes to 20,640 SGRs and 743,097 bytes. The
outer writer stayed at one call per changed frame. The repeated range separation,
smaller payload, and equal terminal state support retaining the combined SGR.

The next bounded Block pass targeted the already-known colour-selection hot
path. Fresh 240x86 profiles placed `quadrantColours` at 23.44 percent flat and
39.04 percent cumulative CPU, and `sextantColours` at 25.79 percent flat and
46.19 percent cumulative, so both cleared the 10 percent profile gate. The
adopted implementation stores mask membership and bright/dark counts in 16-entry
quadrant and 64-entry sextant tables, then uses exact count-specific averaging.
Half-block composition is unchanged. Exhaustive sum/count, all-mask, boundary,
random-frame, complete-grid, overlay, and terminal-byte comparisons matched the
former loop and division algorithm exactly.

Six fresh, non-overlapping processes ran per comparison in rotated order with
`GOMAXPROCS=4`, `GOGC=100`, four discarded warm-ups, and 30 retained 300 ms
samples per arm. Every target's three candidate process-median ranges sat below
every baseline range. Isolated quadrant median/p90 fell from 17.25/17.30 to
11.59/11.63 ns (-32.81/-32.77 percent), and sextant from 18.33/18.44 to
12.91/12.96 ns (-29.57/-29.72 percent).

| Compose path | 120x40 median/p90 before -> after | Change | 240x86 median/p90 before -> after | Change |
|---|---:|---:|---:|---:|
| Quadrant serial | 217,637/219,668 -> 210,779/211,958 ns | -3.15/-3.51% | 937,365/943,765 -> 903,222/907,962 ns | -3.64/-3.79% |
| Sextant serial | 180,573/181,491 -> 157,474/158,270 ns | -12.79/-12.79% | 773,581/777,401 -> 680,241/683,104 ns | -12.07/-12.13% |
| Quadrant parallel | 81,727/84,219 -> 78,209/80,405 ns | -4.30/-4.53% | 336,700/347,087 -> 323,087/329,812 ns | -4.04/-4.98% |
| Sextant parallel | 67,642/69,749 -> 60,100/61,657 ns | -11.15/-11.60% | 279,116/283,825 -> 245,297/252,417 ns | -12.12/-11.07% |
| Half-block serial | 34,646/34,817 -> 34,603/34,744 ns | -0.12/-0.21% | 149,280/150,385 -> 149,110/150,497 ns | -0.11/+0.07% |

The production-shaped quadrant path also retained separated process-median
ranges. Changed-frame median/p90 moved from 631,491/640,480 to
621,458/641,959 ns at 120x40 (-1.59/+0.23 percent), and from
2,651,767/2,766,768 to 2,602,373/2,702,522 ns at 240x86
(-1.86/-2.32 percent). Unchanged frames moved -2.77/-3.84 percent and
-1.58/-2.07 percent. Retained samples stayed at zero B/op and zero allocs/op.
Payload, SGR count, and outer-writer calls were identical in both arms. The mask
candidate post-profile, before the source-sampling pass below, placed
`quadrantColours` at 25.86 percent flat and 27.73 percent cumulative, and
`sextantColours` at 33.12 percent flat and 39.06 percent cumulative. Colour
selection remained on top, but the repeated whole-path gains and exact-output
oracle closed this pass without a second refinement.

The next Quadrant-only profile put `avgBlock` at 20.99 percent flat and 40.95
percent cumulative CPU, `pixel` at 12.58/18.43 percent, and `sampleQuadrant` at
4.25/45.19 percent. The adopted exact-source branch requires a zero-origin,
packed RGBA image of exactly `2*cols` by `4*rows`, with the expected stride and
backing length. It reads row offsets directly and averages each vertical byte
pair through an overflow-safe unsigned sum and floor divide. Nil, empty,
undersized, oversized, odd, subimage, non-zero-origin, padded-stride, and rejected
malformed layouts retain the generic sampler.

The same six-process rotated method retained 30 timed samples per arm. All
isolated, serial, and parallel Quadrant process-median ranges were separated.
Exact Quadrant sampling fell from 23.75/23.91 to 8.62/8.64 ns median/p90
(-63.71/-63.86 percent).

| Quadrant path | 120x40 median/p90 before -> after | Change | 240x86 median/p90 before -> after | Change |
|---|---:|---:|---:|---:|
| Serial compose | 210,722/212,179 -> 113,405/113,864 ns | -46.18/-46.34% | 903,727/909,022 -> 488,345/491,275 ns | -45.96/-45.96% |
| Parallel compose | 78,264/81,074 -> 43,838/45,794 ns | -43.99/-43.52% | 323,219/329,873 -> 176,833/182,628 ns | -45.29/-44.64% |
| Production-shaped RGBA viewer, changed | 625,869/662,058 -> 596,592/636,164 ns | -4.68/-3.91% | 2,633,852/2,795,049 -> 2,550,455/2,697,161 ns | -3.17/-3.50% |
| Production-shaped RGBA viewer, unchanged | 508,573/528,470 -> 479,583/493,612 ns | -5.70/-6.60% | 2,090,011/2,175,214 -> 1,961,860/2,013,553 ns | -6.13/-7.43% |

The viewer rows use exact-size packed RGBA input. They do not measure Nyala's
default native-YCbCr q90 JPEG path, which composes YCbCr directly and bypasses
this sampler. The fast path benefits Quadrant traffic that reaches
`Renderer.Compose` as exact-size packed RGBA, including RGBA/PNG-derived and
fallback frames.

Sextant and Half-block control ranges overlapped; serial median movements stayed
within 0.3 percent. All retained samples had zero allocs/op; B/op was zero except
one 6 B/op candidate sample. Payload, SGR count, writer calls, complete grids,
and terminal bytes were unchanged. Exhaustive comparison covered all 65,536
input byte pairs plus exact
and fallback boundary/random images, overlays, alpha, and parallel reads. A
post-profile removed `avgBlock`, `pixel`, and generic `sampleQuadrant` from the
exact-source hot path; `sampleQuadrantExact` accounts for 5.30 percent flat and
19.59 percent cumulative CPU.

Historical pre-mask 240x86 Compose profiles were allocation-free. Before the
mask-metadata change above, Quadrant CPU was led by `quadrantColours` at 24.02
percent flat and 40.17 percent cumulative; Sextant was led by `sextantColours`
at 27.59 percent flat and 49.41 percent cumulative. Their
production parallel wall times are too small to justify more splitting, so the
residual RGBA Compose task is closed. The bounded q80 sampler follow-up is now
complete in [the direct native-YCbCr record below](#direct-native-ycbcr-block-grid-2026-07-15).
It lowers matched processing time, preserves exact final grids, and improves
saturated compute throughput.

These measurements do not form a matched end-to-end Block benchmark. Do not add
decode, Compose, diff, viewer, CDP p50, or renderer numbers across mismatched inputs
and concurrency models. The decode workers overlap serial per-frame work, while a
real delivered interval also depends on Chromium and the terminal. No bounded
capture, `just fps`, terminal eyeballing, or real-terminal result was used. The
terminal validation tasks remain open.

#### Direct native-YCbCr Block grid (2026-07-15)

The bounded sampler follow-up is complete and accepted. Live Block JPEG frames now
remain as decoder-native `*image.YCbCr` values through decode. The Block compositor
averages their source regions directly into the final `block.Grid`; it creates no
320x270 RGBA intermediate. The sampler prepares destination source spans, luma-row
offsets, chroma-row offsets, and chroma-column mappings once, then reuses their
backing slices. Its final grouped-chroma pass weights each unique chroma sample by
the number of luma pixels that map to it. This preserves the former repeat-weighted
average while reducing repeated Cb and Cr reads.

The live frame-parallel decode workers remain in place. Direct composition still
uses the viewer's row-band pool, after text synchronisation, so the engine/render
seam and overlay ordering stay unchanged. Kitty and Sixel keep their graphics paths.
Capture keeps its lossless and bundle path. PNG, RGBA, non-downscale inputs, and
backend-switch fallbacks keep the established generic compose path.

The final pass ran on the same host as the 2026-07-14 pass: Go 1.26.4,
linux/amd64, Linux 7.1.2, Chromium 149.0.7827.200, and an AMD Ryzen AI Max+ 395
with 16 cores, 32 threads, and 64 MiB L3. Each benchmark suite ran separately.
The live fixture was the static `testdata/linuxmatters.png` page captured at
2560x2880. Each table reports the median of five 2-second samples with `-benchmem`;
parentheses show the minimum and maximum.

Exact commands:

```text
CGO_ENABLED=0 go test -tags live -run '^$' -bench '^BenchmarkCDPBlockComposeMatched$' -benchmem -benchtime=2s -count=5 ./internal/engine
CGO_ENABLED=0 go test -tags live -run '^$' -bench '^BenchmarkBlockDecodePoolSaturatedCompute$' -benchmem -benchtime=2s -count=5 ./internal/engine
CGO_ENABLED=0 go test -tags live -run '^$' -bench '^BenchmarkBlockDecodePoolSaturatedCompute$/^jpeg-q80$/^direct-workers\+ycbcr-grid$' -benchmem -benchtime=5s -count=1 -cpuprofile /tmp/nyala-perf-direct-ycbcr/final/direct-pool.cpu.pprof ./internal/engine
CGO_ENABLED=0 go test -tags live -run '^TestCDPScreencastJPEGFidelity$' -count=1 -v ./internal/engine
```

The matched arms both decode the same captured payload and finish the same 160x90
sextant grid. The legacy arm runs the removed optimised YCbCr-to-RGBA average before
RGBA composition. The direct arm composes from native YCbCr. This comparison does
not add CDP delivery, terminal write, terminal decode, or terminal draw time.

| JPEG | Legacy ms/op | Direct ms/op | Time saved | Speed-up | Direct median headroom |
|---|---:|---:|---:|---:|---:|
| q80 | 16.725 (16.522-16.932) | 8.889 (8.767-9.111) | 7.836 ms, 46.85% | 1.882x | 7.78 ms |
| q85 | 17.310 (16.985-17.849) | 9.361 (9.328-9.407) | 7.949 ms, 45.92% | 1.849x | 7.31 ms |
| q90 | 18.012 (17.985-18.544) | 10.276 (10.253-10.390) | 7.736 ms, 42.95% | 1.753x | 6.39 ms |
| q95 | 20.168 (19.899-20.239) | 12.031 (11.961-12.102) | 8.137 ms, 40.35% | 1.676x | 4.64 ms |
| q100 | 23.987 (23.805-24.248) | 16.137 (16.089-16.187) | 7.850 ms, 32.73% | 1.486x | 0.53 ms |

All q95 samples stay within the 16.67 ms processing budget; the worst leaves
4.56 ms. Every q100 sample also passes, but the worst leaves only 0.48 ms.

| JPEG | Legacy B/op | Direct B/op | Legacy allocs/op | Direct allocs/op |
|---|---:|---:|---:|---:|
| q80 | 12,248,664 | 11,870,982 | 31 | 21 |
| q85 | 12,352,438 | 11,973,833 | 30 | 21 |
| q90 | 12,500,886 | 12,125,319 | 30 | 21 |
| q95 | 12,818,318 | 12,455,814 | 30 | 21 |
| q100 | 13,617,107 | 13,224,043 | 30 | 21 |

Direct composition saves 2.83 to 3.08 percent of bytes per operation and 9 to 10
allocations per operation across the five qualities.

The saturated q80 benchmark used the then-current 16 decode workers on
this 32-thread host. It feeds workers continuously and counts completed final grids.

| Path | Completed-grid interval | Completed grids/s | B/op | allocs/op |
|---|---:|---:|---:|---:|
| Legacy | 1.391 ms (1.381-1.435) | 718.74 (696.93-724.32) | 12,085,061 | 23 |
| Direct | 1.257 ms (1.253-1.258) | 795.58 (794.68-797.90) | 11,637,983 | 12 |

Direct saturated throughput improves by 10.69 percent. This is a compute-throughput
benchmark, not FPS or delivered-frame latency. Its blocking feed does not model CDP
acknowledgement, `rawFrameQueue.put` drops, monotonic delivery, display cadence, or
terminal work.

The clean q80 direct profile completed 4,490 grids. Direct composition used
13.86 aggregate CPU-ms/grid, 12.7 percent above the old averaging-plus-compose
target of 12.30. `pixelAt` used 12.70 CPU-ms/grid after grouped chroma, down from
15.13. The change is accepted because the matched end-to-end processing time falls
46.85 percent at q80 and saturated compute throughput rises 10.69 percent. The CPU
number remains a profile reference, not a reopening trigger by itself.

Fidelity compares each captured JPEG with the PNG captured from the same unchanged
page. PSNR and SSIM use the project's BT.601-luma metrics.

| JPEG | Payload bytes | PSNR | SSIM |
|---|---:|---:|---:|
| q80 | 425,401 | 45.234 dB | 0.995468 |
| q85 | 477,761 | 46.806 dB | 0.996597 |
| q90 | 563,559 | 49.021 dB | 0.997696 |
| q95 | 730,950 | 52.682 dB | 0.998927 |
| q100 | 1,167,773 | 58.282 dB | 0.999864 |

The equality gate compares both timed kernels with an independent intermediate-RGBA
oracle. Unit coverage includes all six YCbCr subsampling ratios, quadrant, half-block,
and sextant schemes, odd and non-zero bounds, overlays, parallel reuse, resize and
scheme transitions, and RGBA fallback. Every q80-q100 live preflight produced the
same final grid.

JPEG q90 is now the Block default. Against q80, it raises PSNR from 45.234 dB to
49.021 dB and SSIM from 0.995468 to 0.997696; payload grows from 425,401 to 563,559
bytes. Its 10.276 ms matched median at 2560x2880 leaves 6.39 ms median processing
headroom and provides more headroom for higher resolutions than q95.
Block JPEG quality remains fixed at q90 as the accepted fidelity and performance
balance. No runtime quality control or extra configuration will be added.
q100 is rejected as a candidate: it adds 59.76 percent payload over q95 and leaves too
little processing headroom. These headless figures exclude terminal decode and draw time.

#### JPEG quality rerun (2026-07-17)

The current tree reran the existing 2560x2880 live-CDP harness against the same
static `testdata/linuxmatters.png` fixture. No Justfile recipe exists for this sweep
in the current file or its Git history. The durable recipe is the exact Go command
recorded above. The Block run retained that command unchanged and the results below
select its production `direct-decode+ycbcr-grid` arm. The Sixel run selected the
harness's existing production `sixel/current-rgba` arm for the same five JPEG
payloads. That historical label now calls the current `render/sixel.Renderer`.

```text
CGO_ENABLED=0 go test -tags live -run '^$' -bench '^BenchmarkCDPBlockComposeMatched$' -benchmem -benchtime=2s -count=5 ./internal/engine
CGO_ENABLED=0 go test -tags live -run '^$' -bench '^BenchmarkCDPNativeFrame$/^jpeg-q(80|85|90|95|100)$/^sixel$/^current-rgba$' -benchmem -benchtime=2s -count=5 ./internal/engine
CGO_ENABLED=0 go test -tags live -run '^TestCDPScreencastJPEGFidelity$' -count=1 -v ./internal/engine
```

Each p90 below is the nearest-rank p90 of five independent two-second benchmark
samples, which is the maximum sample for a set of five. The delta compares each
quality with the current q90 p90 for that backend. FPS is processing capacity only.
It excludes CDP delivery and terminal write, decode, draw, and refresh.

| Backend | JPEG | Processing p90 | Delta from q90 | Projected internal FPS | JPEG payload bytes | PSNR | SSIM |
|---|---:|---:|---:|---:|---:|---:|---:|
| Block | q80 | 8.641409 ms | -1.468979 ms | 115.72 | 425,401 | 45.234 dB | 0.995468 |
| Block | q85 | 9.238345 ms | -0.872043 ms | 108.24 | 477,761 | 46.806 dB | 0.996597 |
| Block | q90 | 10.110388 ms | 0 | 98.91 | 563,559 | 49.021 dB | 0.997696 |
| Block | q95 | 11.891159 ms | +1.780771 ms | 84.10 | 730,950 | 52.682 dB | 0.998927 |
| Block | q100 | 15.967314 ms | +5.856926 ms | 62.63 | 1,167,773 | 58.282 dB | 0.999864 |
| Sixel | q80 | 72.871305 ms | +0.193141 ms | 13.72 | 425,401 | 45.234 dB | 0.995468 |
| Sixel | q85 | 71.860366 ms | -0.817798 ms | 13.92 | 477,761 | 46.806 dB | 0.996597 |
| Sixel | q90 | 72.678164 ms | 0 | 13.76 | 563,559 | 49.021 dB | 0.997696 |
| Sixel | q95 | 71.417698 ms | -1.260466 ms | 14.00 | 730,950 | 52.682 dB | 0.998927 |
| Sixel | q100 | 72.780056 ms | +0.101892 ms | 13.74 | 1,167,773 | 58.282 dB | 0.999864 |

The complete sorted p90 source samples in milliseconds are:

| Backend | JPEG | Five samples |
|---|---:|---|
| Block | q80 | 8.510791, 8.547109, 8.598929, 8.623327, 8.641409 |
| Block | q85 | 9.059109, 9.099007, 9.178857, 9.191994, 9.238345 |
| Block | q90 | 10.021056, 10.043404, 10.079098, 10.080294, 10.110388 |
| Block | q95 | 11.652069, 11.797957, 11.850965, 11.887974, 11.891159 |
| Block | q100 | 15.829116, 15.870771, 15.886052, 15.931514, 15.967314 |
| Sixel | q80 | 72.498017, 72.613538, 72.778532, 72.792896, 72.871305 |
| Sixel | q85 | 71.274537, 71.519218, 71.817373, 71.836355, 71.860366 |
| Sixel | q90 | 71.789372, 72.003334, 72.426034, 72.486270, 72.678164 |
| Sixel | q95 | 70.206729, 70.900889, 71.004175, 71.037695, 71.417698 |
| Sixel | q100 | 71.780684, 71.976316, 72.135756, 72.242218, 72.780056 |

The Block medians are 8.598929, 9.178857, 10.079098, 11.850965, and
15.886052 ms from q80 through q100. Against the historical medians above, they
improve by 3.26, 1.95, 1.92, 1.50, and 1.56 percent. Historical raw samples were
not retained, so a matched historical p90 comparison is not possible. The payload
sizes and fidelity scores reproduce the historical values exactly.

For Block, q95 still passes the 16.67 ms processing rule with 4.78 ms of p90
headroom. q100 passes with only 0.70 ms. For Sixel, none of the five qualities
passes that processing rule at this surface, and the small non-monotonic changes
show no material quality-dependent processing trend. Sixel terminal payloads were
1,861,958, 1,828,055, 1,768,045, 1,555,398, and 1,222,400 bytes from q80 through
q100, but terminal handling remains outside this benchmark.

Keep q90 as the default for both consumers. q95 adds 29.70 percent JPEG payload
over q90 for a fidelity gain on an already high-quality source, while q100 adds
107.21 percent and leaves Block too close to its processing limit. The headless
results do not establish enough user-visible gain to accept those payload and
headroom costs, especially without terminal-side measurements.

#### Block REP benchmark-only spike (2026-07-19)

A benchmark-only Block writer added REP for runs of three or more equal cells. Six
fresh processes used rotated arm order, four warm-ups, 30 retained samples,
`GOMAXPROCS=4`, and `GOGC=100`.

| Fixture class | Output-byte saving | REP writer p90 |
|---|---:|---|
| Flat | 97.19% to 97.90% | Improved |
| Scroll | 92.33% to 94.15% | Improved |
| Web and text | 16.49% to 16.76% | Improved |
| Photo and motion | 0% | Regressed by 15.7% to 24.6% |

REP preserved zero allocations and the existing buffer budgets. Logical screen and
cursor equivalence passed across rows, colours, overlays, multi-rune text, wide
cells, unchanged gaps, CUP, and SGR. Focused tests and race checks, formatting,
build, unit tests, lint, and Block interoperability passed.

The benchmark identifies high byte savings on compressible output, but the fixed
threshold pays a material detection cost when nothing compresses. It is not accepted
for production. The benchmark-only harness remains in
`internal/render/block/rep_bench_test.go`; the production writer in `diff.go` is
unchanged. A production spike still needs cheaper or adaptive detection, a capability
and literal-fallback policy, physical checks on KMSCON, foot, tmux, Zellij, herdr,
and GNU Screen, and terminal-presented byte, latency, and frame-rate evidence. GNU
Screen requires literal output fallback.

### Summary

This is the historical pre-fuse summary that led to the row-band and fused paths.
The 2026-07-14 record above supersedes its current rankings. At this point, the
dominant block-render cost was the source downscale, not the colour maths.
`renderBlock` reduced the full screencast surface to the sub-cell
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
with cell count. A separate historical cost sat off this path: the engine allocated
a fresh full-resolution `image.RGBA` per decoded frame, the destination of the
JPEG-to-RGBA convert, at about 73 percent of decode bytes. The live Block fuse later
removed it; the dated record above measures the remaining small result allocation.

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

1. **CLOSED by owner decision.** Request the block screencast at
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
   path as the discriminating benchmark. Reopen only with specific, explicit owner
   approval.
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
   variant and the residual Compose split were not needed for the win. The 2026-07-14
   profile later put only `averageYCbCrToRGBA` back on top. The bounded sampler
   follow-up replaced it with direct native-YCbCr grid composition; the residual
   RGBA Compose split stays closed. See the
   [direct native-YCbCr record](#direct-native-ycbcr-block-grid-2026-07-15).
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
4. **[CLOSED - profile gate, strengthened 2026-07-15]** Reusable RGBA ring on the
   decode seam. The 2026-07-14 path materialised one 320x270 result whose
   `image.NewRGBA` was 345,600 bytes, 2.86 percent of allocation space. Direct
   native-YCbCr composition has removed that result too. The old full-resolution
   `draw.Draw` destination and its claimed 73 percent share are stale; no live Block
   RGBA intermediate remains to pool.
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

## Kitty in-band ceiling research (2026-07-21)

Why the direct `t=d` path capped at 12.7 fps on the live aquarium, and what the
terminal and Chromium can each sustain. This research motivated the transport
benchmark and the shm transport in the next two sections.

### Live direct baseline (aquarium)

`NYALA_FPS_TIMING=1 just debug-run 'https://webglsamples.org/aquarium/aquarium.html'
--backend=kitty`, Kitty maximised on a 2560x1600 display:

```
in_mean=23.4 in_median=39.4 in_worst_gap_ms=119.9 in_decode_mean_ms=0.6
out_mean=12.7 out_encode_mean_ms=1.3 out_write_mean_ms=78.9
```

Verdict: Nyala was not the bottleneck; both walls were external. Nyala's
framing cost 1.3 ms per frame. The 78.9 ms write was pty backpressure while
Kitty parsed, base64-decoded, and PNG-decoded each frame on its main thread.
Ingest was Chromium-side frame production: ~39 fps steady state with tail
stalls to 120 ms dragging the mean to 23.4. For comparison, puppeteer users
report ~25-31 fps from its JPEG default-viewport screencast; a 39 fps median
at four times the pixels in PNG is in line. The 683-frame in/out gap (1484
against 801) died at the viewer's latest-wins coalescing by design: frames
arrived every 25-43 ms while a paint took 79 ms, so roughly every second frame
was superseded before assembly.

Arithmetic on the write: a ~1.25 MB PNG at 2560x1500, times 4/3 for base64, is
~1.67 MB per frame; 1.67 MB over 78.9 ms is ~21 MB/s terminal ingest. Chunk
envelope overhead is ~0.2%, negligible. 801 frames at 78.9 ms matches the
63.1 s wall clock, so the writer was busy the whole session and the pipeline
fully exposed the terminal as the wall.

### Kitty's in-band cost model and hard ceiling

Kitty's IO thread reads the pty into a 1 MiB parse buffer; the main thread
then SIMD-scans escapes, SIMD base64-decodes chunks, accumulates them, and
runs libpng inflate before texture upload (`vt-parser.c`, `graphics.c`). When
the parse buffer nears full, the IO thread stops reading, the ~64 KiB kernel
pty buffer fills, and the client's `write()` blocks, so a slow terminal decode
stalls the sender directly. At 3.84 Mpx, single-threaded PNG inflate dominates
at an estimated 40-70 ms, with base64 and chunk parsing adding 10-15 ms; 79 ms
per frame was the expected cost, not a fault.

The hard ceiling: Kitty's own `kitten __benchmark__` measured the post-SIMD
parser at ~270 MB/s on its Images workload. Raw RGBA at 2560x1500 is 20.5 MB
of base64 per frame; 270 MB/s gives ~13 fps. The in-band channel tops out
where the direct path already sat, and no pixel-format tweak escapes it.

### What Kitty is capable of

- In-band, measured by others: tpix sustains 60 fps truecolour at 720p and
  30 fps at 1280p; mpv manages 24 fps at 1080p over direct transmission.
  Nobody publishes high fps at 2560x1500 in-band; the parser arithmetic above
  says nobody will.
- Out-of-band, `t=s` POSIX shm is the designed fast path: the client writes
  pixels to an shm object and sends a tiny escape; Kitty mmap-reads and
  unlinks the object. No base64, no chunk parse, no pty backpressure on
  megabytes. Field proof: mpv dropped frames at high resolution over direct
  transmission and stopped dropping with `--vo-kitty-use-shm=yes`. Same-host
  only. PNG over shm still pays libpng inflate on Kitty's main thread, so shm
  alone removes only the transport share, not the decode wall.
- Atomic same-id replacement: retransmitting with the same image id replaces
  data and placements without flicker (`a=T`, fixed `i`, `q=2`). Nyala keeps
  the double-buffered id swap instead. The animation edit path (`a=f,r=1`)
  stays rejected as non-portable (black frames on kitty and WezTerm).
- notcurses cut bandwidth 99.9% with the animation protocol, evidence of the
  headroom that avoiding full-frame retransmission buys where it is portable.
- Config caps: `repaint_delay` (10 ms) and `input_delay` (3 ms) are irrelevant
  below ~60-100 fps and are user-config only; `sync_to_monitor` caps at the
  monitor refresh rate.

### Chromium screencast facts (source-verified)

- Capture is damage-driven: frames arrive only on visual change.
- The rate cap on current Chromium is 100 fps (10 ms minimum capture period);
  the old 10 fps hard cap is historical.
- The ack budget allows up to 3 frames between capture and acknowledgement.
  While the budget is exhausted, frames are dropped, not queued, so a late ack
  becomes a gap rather than latency.
- Screencast PNG takes a fast-encode path on a worker thread. The DevTools
  front end itself screencasts as JPEG quality 80.
- `maxWidth`/`maxHeight` scale GPU-side in the capturer, before readback.

### GPU verification (2026-07-21)

`just debug-run 'https://webglreport.com/?v=2' --backend=kitty` confirmed the
GPU active inside Nyala's Chromium: unmasked renderer ANGLE (AMD Radeon 8060S
Graphics, radeonsi, OpenGL ES 3.2). A silent SwiftShader fallback is ruled out
as the ingest-tail cause. The remaining tail suspects are PNG encode spikes on
busy frames and the 3-frame ack window.

### Decision trail

The operator first chose PNG-over-shm for simplicity, with raw-over-shm
deferred, and kept the raw benchmark variants as the escalation baseline: a
sub-2x shm-PNG gain would reopen the payload question. The benchmark below
fired exactly that gate, so raw `f=32` over shm shipped instead, with direct
PNG unchanged as the universal fallback.

### Sources

- Kitty graphics protocol (mediums, formats, chunking, animation): https://sw.kovidgoyal.net/kitty/graphics-protocol/
- Kitty performance and config (`repaint_delay`, `input_delay`, `sync_to_monitor`): https://sw.kovidgoyal.net/kitty/performance/ and https://sw.kovidgoyal.net/kitty/conf/
- Parser ~270 MB/s on the Images workload after the SIMD rewrite: https://github.com/kovidgoyal/kitty/issues/7005
- Main-thread parse and PNG decode path: https://github.com/kovidgoyal/kitty/blob/master/kitty/graphics.c and https://github.com/kovidgoyal/kitty/blob/f13c8cd4/kitty/vt-parser.c
- Same-id atomic replacement recipe: https://github.com/kovidgoyal/kitty/issues/8701
- tpix 60 fps at 720p, 30 fps at 1280p: https://github.com/kovidgoyal/kitty/discussions/5660
- mpv shm fix for high-resolution drops: https://github.com/mpv-player/mpv/issues/13285 and https://github.com/mpv-player/mpv/blob/1ac687d7/video/out/vo_kitty.c
- mpv 2 to 24 fps at 1080p direct after the strlen fix: https://github.com/mpv-player/mpv/pull/15766
- notcurses animation protocol cut bandwidth 99.9%: https://github.com/dankamongmen/notcurses/discussions/1898
- Chromium ack window, drop-not-queue, fast PNG encode: https://chromium.googlesource.com/chromium/src/+/refs/heads/main/content/browser/devtools/protocol/page_handler.cc
- Chromium 10 ms minimum capture period: https://chromium.googlesource.com/chromium/src/+/refs/heads/main/content/browser/devtools/devtools_video_consumer.cc
- Damage-driven capture oracle and GPU-side scaling: https://chromium.googlesource.com/chromium/src/+/refs/heads/main/components/viz/service/frame_sinks/video_capture/frame_sink_video_capturer_impl.h
- DevTools front end screencasts as JPEG q80: https://chromium.googlesource.com/devtools/devtools-frontend/+/a6e6344c2ad021c594e2962d61db87621a2f8987/front_end/screencast/ScreencastView.ts
- Puppeteer screencast ~25-31 fps reports: https://github.com/puppeteer/puppeteer/issues/15112

---

## Kitty transport benchmark (2026-07-21)

`just kitty-transport-bench` sent the same 2560x1500 fixture frame to a live,
maximised Kitty terminal (2560x1600 display, AMD Radeon 8060S) 30 times per
variant. The PNG payload was 1.09 MB and the raw `f=32` payload 15.36 MB. Direct
variants time the tty write alone; file and shm variants time the write plus the
terminal's acknowledgement.

| Variant | Payload | Timed span | Mean ms | Median ms | Min ms | Max ms |
|---|---:|---|---:|---:|---:|---:|
| `t=d` PNG | 1.09 MB | write | 16.07 | 16.22 | 2.66 | 20.03 |
| `t=f` PNG | 1.09 MB | write+ack | 16.54 | 16.13 | 15.06 | 27.33 |
| `t=s` PNG | 1.09 MB | write+ack | 16.14 | 15.71 | 15.23 | 19.82 |
| `t=s` raw `f=32` | 15.36 MB | write+ack | 10.74 | 10.57 | 9.44 | 12.64 |
| `t=d` raw `f=32` | 15.36 MB | write | 54.88 | 54.55 | 41.86 | 76.37 |

shm-PNG shows no gain over direct (16.14 against 16.07 ms mean). At 1 MB the
transport share is negligible and Kitty's PNG decode dominates the span, so
moving the same PNG bytes through shared memory buys nothing. The plan's
escalation gate (an shm-PNG improvement under 2x reopens the payload question)
fired. Raw `f=32` over shm is the only variant that beats direct, 10.74 against
16.07 ms, because it removes the terminal's PNG decode as well as the in-band
transport. In-band raw at 54.88 ms for 15.36 MB confirms the escape-parser
throughput wall for large payloads. Verdict: raw-over-shm is promoted to the
fast path shipped in the next section; the direct fallback stays PNG.

Caveat: the gradient-plus-noise fixture writes in 16 ms over direct, far below
the 78.9 ms live aquarium baseline in [the ceiling research
above](#kitty-in-band-ceiling-research-2026-07-21). Real high-entropy content
decodes slower, so the fixture understates terminal-side PNG decode, and the
live before-and-after aquarium comparison remains the definitive gain
measurement.

---

## Kitty shm transport (2026-07-21)

The raw-over-shm promotion from [the transport benchmark
above](#kitty-transport-benchmark-2026-07-21) shipped as a runtime-detected
fast path. The direct `t=d` PNG pass-through is unchanged and remains the
default for Konsole, SSH, sandboxes, probe failure, and runtime demotion.

**Detection.** The DA1-fenced startup handshake in `internal/viewer/detect.go`
gained an icat-style shm probe: an `a=q` query carrying a real 1x1 `f=24`
payload in a real `/dev/shm` object, so it adds no round trip and self-handles
SSH, private `/dev/shm`, and terminals without shm support. Terminal identity
comes from an XTVERSION query in the same batch.

**Decision order** (`internal/viewer/transport.go`, resolved once per run):
user override (`--kitty-transport=auto|direct|shm`) > deny list > probe result
> allow list > direct fallback. The allow list covers terminals that support
shm but stay silent on the probe; the deny list covers terminals whose shm
path is broken. Seeds: allow WezTerm on Linux (supports `t=s` but never acks
the probe), deny WezTerm on macOS (renders `t=s` wrongly). Live terminal
testing populates both lists. Operator scoping decision (2026-07-21):
terminal-emulator validation runs on Kitty only for now, as the golden
reference implementation; the broader matrix (Ghostty, WezTerm, Konsole,
st-graphics) follows once Kitty is satisfactory.

**Fast path.** The engine decodes each CDP PNG frame into a pooled RGBA plane
(`internal/pngdecode.DecodeInto`, zero per-frame plane allocations). The
viewer's writer goroutine copies the plane into a per-frame POSIX shm object
(`internal/render/kitty.SHMWriter`) and emits one ~100-byte `f=32,t=s` escape
with double-buffered image ids. The terminal mmap-reads and unlinks the
objects it consumes; the writer unlinks the rest on close. Each shm object
carries one complete transmission, never chunked: `m=1` shm updates do not
display on Ghostty. The engine-decode-then-viewer-copy split is a recorded
deviation from the promotion design notes, which had the decode land directly
in the shm object: decoding into pooled engine-owned planes and copying the
plane into the shm mapping at write time costs one ~15 MB memcpy (1-2 ms, on
the writer goroutine) and keeps the engine-viewer seams clean.

**Codecs.** Hot paths pin the optimised third-party codecs already in the
module: base64 via `github.com/emmansun/base64`, PNG decode via
`internal/pngdecode` wrapping `github.com/woozymasta/png`, JPEG via
`github.com/gen2brain/jpegn`. Stdlib `encoding/base64` and `image/png` are
acceptable only in tests and goldens.

**Pacing.** The tiny escape removes the pty backpressure that paces the direct
writer, so the shm path bounds frames in flight to one: the next frame waits
for the terminal's OK reply (sent with `q=0`) or a 40 ms deadline, whichever
comes first, so a lost reply cannot stall the stream.

**Demotion.** Any shm failure, from object creation through a terminal error
reply, demotes the current Kitty stint to the direct PNG path. A terminal
error reply counts only while the session is still on the Kitty backend: after
Tab cycles away, the switch itself unlinks unconsumed objects, so a busy
terminal's late failure reply is self-inflicted and ignored. Tab back to Kitty
re-arms the probe-proven transport, so a demotion never outlives its stint.

The fixture benchmark above predicts the per-frame write falling from 16.07 to
10.74 ms and understates the live gain (see its caveat). The live
before-and-after aquarium measurement ran on the same maximised 2560x1600
Kitty with the shm transport active:

```
warmup_ms=938 in_frames=713 in_mean=24.6 in_min=9.4 in_max=272.0 in_median=40.4 in_worst_gap_ms=106.7 in_decode_mean_ms=96.9 in_decode_max_ms=165.5 out_frames=705 wall=28.87s out_mean=24.4 out_median=39.0 out_min=7.7 out_max=89.6 out_worst_gap_ms=129.6 out_encode_mean_ms=4.6 out_encode_max_ms=6.5 out_write_mean_ms=0.0 out_write_max_ms=0.0 ack_rtt_mean_ms=11.7 ack_rtt_max_ms=46.1
```

| Metric | Direct baseline | shm | Change |
|---|---:|---:|---|
| `out_mean` fps | 12.7 | 24.4 | +92% |
| `out_write_mean_ms` | 78.9 | 0.0 | eliminated |
| `in_mean` fps | 23.4 | 24.6 | unchanged |
| `in_median` fps | 39.4 | 40.4 | unchanged |

The terminal write wall is gone and output now tracks ingest: 705 of 713
frames painted, `out_median` 39.0 against `in_median` 40.4. The remaining
ceiling is Chromium frame production, median ~40 fps with tail gaps to
~107 ms from PNG encode spikes on busy frames. `ack_rtt_mean_ms` at 11.7
confirms the ack path is not the cap. The new cost is the engine-side PNG
plane decode, `in_decode_mean_ms` 96.9. Pooled workers pipeline it, so it
does not cap throughput, but it adds ~100 ms of latency.

Open follow-ups:

- Upstream `DecodeInto` to `woozymasta/png` as a pull request; it is carried
  as a minimal in-package fork of the row loop in `internal/pngdecode`.
- Populate the transport allow and deny lists from live terminal testing,
  under the Kitty-first scoping decision above.

### JPEG q90 shm capture (2026-07-21)

Operator decision (2026-07-21): JPEG q90 is the screencast capture format
whenever the shm fast path is active, with no CLI flag. This closes the JPEG
q90 follow-up from the shm promotion.

Shipped behaviour (`internal/engine/screencast.go`,
`internal/engine/decodepool.go`, `internal/engine/frame.go`): the engine
requests JPEG at Block's q90 when the Kitty backend is active and shm decode
is on, and PNG otherwise. A toggle that changes the effective format restarts
a running screencast through the same control seam resize and backend changes
use. The plane decode sniffs the payload signature: PNG keeps the pooled
zero-allocation `DecodeInto` path, and JPEG routes through jpegn with
`ToRGBA`, accepting one plane allocation per decode (the same per-frame cost
production Block pays) and a nil `Release`. On demotion the capture returns
to PNG, and in-flight JPEG frames drop at the direct writer because
`EncodedPNG` is empty.

Codec benchmark (2026-07-21, 2560x1500 fixture, 30 iterations):

| Codec | Payload bytes | Payload-to-plane mean |
|---|---:|---:|
| PNG | 1,091,682 | 15.53 ms |
| JPEG q90 | 480,812 (44% of PNG) | 23.38 ms upper bound |

The JPEG figure is an upper bound from an unoptimised test conversion;
production decodes through jpegn `ToRGBA`. The fixture flatters PNG: the live
aquarium measurement above put the PNG plane path at 96.9 ms
(`in_decode_mean_ms`), while JPEG decode cost is content-stable, so JPEG wins
about 4x on real pages. It also halves websocket bytes, relieving Chromium
encode, the current ingest ceiling. Live verification on Kitty is done,
below.

Live verification (2026-07-21, same maximised 2560x1600 Kitty, aquarium
WebGL page):

```
warmup_ms=3102 in_frames=1637 in_mean=59.9 in_min=22.2 in_max=508.7 in_median=60.0 in_worst_gap_ms=45.0 in_decode_mean_ms=17.2 in_decode_max_ms=25.8 out_frames=1635 wall=27.32s out_mean=59.8 out_median=62.3 out_min=21.4 out_max=92.2 out_worst_gap_ms=46.7 out_encode_mean_ms=4.8 out_encode_max_ms=6.8 out_write_mean_ms=0.0 out_write_max_ms=0.2 ack_rtt_mean_ms=0.6 ack_rtt_max_ms=8.0
```

Load timings: process spawned +184.3 ms, screencast started +3053.9 ms,
first frame emitted +3094.4 ms.

Ingest and output both lock to 60 fps, the monitor refresh ceiling:
`in_mean` 59.9, `out_mean` 59.8, 1635 of 1637 frames painted. Against the
shm PNG run above, plane decode fell from 96.9 to 17.2 ms and the worst
ingest gap from 106.7 to 45.0 ms. `ack_rtt_mean_ms` at 0.6 shows the ack
path idle. Progression on this page: 12.7 fps out direct, 24.4 shm PNG,
59.8 shm JPEG, a 4.7x overall gain.

Startup note (closed 2026-07-21): the 3054 ms screencast start in this run
was load variance, not a code regression. Investigation confirmed no
PNG-to-JPEG restart exists at startup: the viewer resolves the transport
and sets the engine decode mode before `Navigate`, so the single initial
screencast start already selects JPEG q90.
`TestKittySHMPreStartResolveStartsOnce` pins the one-start behaviour.
Later runs on identical code started at 1286 ms and 926 ms.

Uncapped datapoint (2026-07-21, same setup, Chromium started with
`--disable-frame-rate-limit`):

```
warmup_ms=1328 in_frames=3062 in_mean=74.6 in_min=9.5 in_max=3006.4 in_median=82.6 in_worst_gap_ms=104.7 in_decode_mean_ms=20.3 in_decode_max_ms=30.4 out_frames=2699 wall=41.01s out_mean=65.8 out_median=72.9 out_min=9.4 out_max=90.3 out_worst_gap_ms=106.6 out_encode_mean_ms=5.5 out_encode_max_ms=9.9 out_write_mean_ms=0.0 out_write_max_ms=0.6 ack_rtt_mean_ms=1.0 ack_rtt_max_ms=13.5
```

Load timings: process spawned +165.4 ms, screencast started +1286.2 ms,
first frame emitted +1319.6 ms.

The 60 fps above is Chromium's frame-rate cap, not a pipeline limit:
uncapped, ingest median reaches 82.6 and output median 72.9. The output
shortfall against ingest is the one-frame-in-flight pacing gate waiting on
Kitty's per-frame raw upload (~10-13 ms, `ack_rtt_max_ms` 13.5). The 60 cap
stays the default: frames beyond the display refresh waste browser CPU and
GPU, the instantaneous minimum fell to 9.5, and the worst gaps did not
improve. A pass-through for extra Chromium flags is a possible future
convenience. Screencast start was 1286 ms here against 3054 ms above,
confirming the earlier figure was load variance (see the closed startup
note above; no startup restart exists).

Torture test (2026-07-21, 2560x2880 Kitty, aquarium WebGL page, shm JPEG
path):

```
warmup_ms=1347 in_frames=2682 in_mean=49.2 in_min=9.4 in_max=1131.5 in_median=56.6 in_worst_gap_ms=106.0 in_decode_mean_ms=32.9 in_decode_max_ms=47.9 out_frames=2471 wall=54.48s out_mean=45.3 out_median=47.2 out_min=9.0 out_max=59.8 out_worst_gap_ms=110.8 out_encode_mean_ms=9.8 out_encode_max_ms=13.6 out_write_mean_ms=0.0 out_write_max_ms=0.7 ack_rtt_mean_ms=0.8 ack_rtt_max_ms=16.7
```

Load timings: process spawned +209.4 ms, screencast started +925.8 ms,
first frame emitted +1335.2 ms.

Output holds 45.3 fps at the size where the direct decoded path measured
its `4.5 -> 5.8 -> 8.5` fps arc (2568x2880, above): 5.3x the best prior
figure and 10x the original. Costs scale linearly with the 1.92x pixel
count against the 2560x1600 run: plane decode `17.2 -> 32.9 ms`,
plane-to-shm encode `4.8 -> 9.8 ms`. The output ceiling is the
one-frame-in-flight pacing gate waiting on Kitty's raw upload of each
~29.5 MB frame, ~17-21 ms (`ack_rtt_max_ms` 16.7), as designed. Screencast
start at 926 ms confirms the earlier 3054 ms figure was load variance.

### Terminal support for raw-RGB-over-shm (research, 2026-07-21)

`t=s` is safe on Kitty, Ghostty, WezTerm (Linux and Windows), and current
st-graphics. Konsole is direct-only by design. The `t=d` PNG path stays the
universal fallback.

| Terminal | `t=s` shm | `t=f`/`t=t` file | Raw `f=24`/`f=32` | `f=100` PNG | Notes |
|---|---|---|---|---|---|
| Kitty | Yes | Yes | Yes | Yes | Error reply (`EBADF`/`EINVAL`) on failure |
| Ghostty | Yes (2024-08, PR #2064) | Yes | Yes | Yes | Chunked (`m=1`) shm updates do not display; send one complete transmission per object |
| WezTerm | Yes on Linux/Windows (2022-04, PR #1810); broken on macOS (#7631) | Yes | Yes | Yes | Sends no APC ack for `t=s`, so detection relies on the DA1 timeout |
| Konsole | No | No | Yes (direct) | Yes | Direct-only by deliberate design (MR 594) |
| st (kitty-graphics patch) | Yes (2025-12-30 diff) | Yes | Yes | Yes | The earlier 2024 diff lacked shm |
| wayst / Warp | Unverified | Unverified | Unverified | Presumed yes | Protocol support announced, mediums undocumented |
| tmux | Pass-through | Pass-through | Pass-through | Pass-through | Native relay since 2026-03 (`ENABLE_KITTY`); works when client and outer terminal share the host |

Detection is defined by the protocol and proven by Kitty's own icat kitten
(`kittens/icat/detect.go`): send an `a=q` probe per medium, each with a real
1x1 payload (a real shm object for `t=s`), then `CSI c` (DA1). An `OK` reply
for the probe's image id marks the medium supported; an error or silence
before the DA1 answer means fallback. A short timeout covers WezTerm's
missing ack.

Remote and sandbox behaviour: `t=s` and `t=f` fail whenever the client and
terminal do not share a filesystem or shm namespace (SSH, containers with a
private `/dev/shm`). The probe handles this automatically because the
terminal-side open fails. Kitty's docs state file and memory modes will not
work over remote sessions.

Matrix sources: https://github.com/ghostty-org/ghostty/pull/2064,
https://github.com/wezterm/wezterm/pull/1810,
https://github.com/wezterm/wezterm/issues/7631,
https://invent.kde.org/utilities/konsole/-/merge_requests/594,
https://st.suckless.org/patches/kitty-graphics-protocol/,
https://github.com/kovidgoyal/kitty/blob/master/kittens/icat/detect.go,
https://github.com/tmux/tmux/commit/cf6cbe430cb18eabeb39a1cac807dece48eac33a.

---

## Internal rendering test (2026-07-16)

This is an **internal rendering test**, not a delivered-frame or terminal
performance test. It measures each production viewer path from a retained
production-shaped frame to terminal bytes in a counting writer. It excludes
terminal transport, terminal protocol decode, terminal draw, display refresh,
and the CDP capture and preparation span reported separately below.

The source was the committed offline
`testdata/corpus/layout-13-linuxmatters/index.html` page. Separate live
`ChromiumEngine` sessions loaded it through CDP for each backend and size. The
fixture contains prose, navigation, cards, borders, fixed content, and coloured
SVG graphics without network input. Each session captured the settled top and
half-viewport-scrolled states. The timed loop alternated those two retained
frames through `viewer.State.RenderStreamFrame`, including terminal-byte
generation. Block therefore ran its q90 native-YCbCr decode hand-off, quadrant
compose, crisp DOM-text overlay, grid diff, SGR generation, and buffered byte
write. Kitty validated and framed the owned CDP base64 PNG directly. Sixel used
the production PNG hand-off, adaptive 256-colour palette, retained encoder state,
and full Sixel encoding. The q90 policy applies only to Block here.

Both exact device surfaces used a 10x20 pixel terminal cell: 192x54 cells for
1920x1080 and 256x80 cells for 2560x1600. Block used the broadly supported
quadrant scheme and the default 1280 CSS-pixel layout width. Thirty timed batches
produced each distribution. A batch held 10 Block frames, 1000 Kitty frames, or
one Sixel frame; each reported sample is normalised to one frame. Estimated FPS
is `1000 / p90_ms`. `testing.AllocsPerRun(5, ...)` measured the warm production
path separately from the latency batches.

| Backend | Device surface | Grid | Mean ms | p90 ms | Estimated internal FPS from p90 | Mean terminal bytes | Allocations/frame |
|---|---:|---:|---:|---:|---:|---:|---:|
| Block | 1920x1080 | 192x54 | 0.659 | 0.730 | 1370.76 | 37,937 | 59 |
| Kitty | 1920x1080 | 192x54 | 0.008 | 0.008 | 120,094.69 | 175,735 | 10 |
| Sixel | 1920x1080 | 192x54 | 38.060 | 39.246 | 25.48 | 306,362 | 1,302 |
| Block | 2560x1600 | 256x80 | 0.988 | 1.058 | 945.13 | 63,886 | 122 |
| Kitty | 2560x1600 | 256x80 | 0.012 | 0.013 | 77,234.16 | 269,574 | 10 |
| Sixel | 2560x1600 | 256x80 | 50.486 | 52.220 | 19.15 | 480,010 | 1,303 |

CDP preparation was timed outside the renderer samples. Each value covers one
Chromium launch, local-page load, first settled frame, injected half-viewport
scroll, and second settled frame. It is a two-frame fixture preparation span,
not a per-frame screencast latency.

| Backend | Device surface | CDP preparation ms |
|---|---:|---:|
| Block | 1920x1080 | 2106.3 |
| Kitty | 1920x1080 | 1445.3 |
| Sixel | 1920x1080 | 1447.2 |
| Block | 2560x1600 | 2142.4 |
| Kitty | 2560x1600 | 1453.8 |
| Sixel | 2560x1600 | 1470.4 |

The test ran on Linux 7.1.2, amd64, an AMD Ryzen AI MAX+ 395 with 32 logical
CPUs, and Go 1.26.4, from base commit `27f1fe3`. Run it again with:

```sh
just internal-render-metrics
```

### Progression rerun (2026-07-17)

This comparison uses runnable baseline commit `254f318`, whose durable record names
`27f1fe3` as the measured production base. The commits differ only by the benchmark
harness, recipe, and documentation. Current commit `9e151fc` used the unchanged
harness, fixture, and `just internal-render-metrics` recipe. Each side ran as six
fresh, interleaved processes with `CGO_ENABLED=0` in the same environment. Each
process retained the same CDP fixture, two settled frames, q90 Block policy,
production backends, grids, warm-up, batches, and 30 measured samples.

The table reports the median of the six printed per-process p90 values. Projected FPS
is internal byte-generation capacity, not delivered terminal FPS.

| Surface | Backend | Baseline median p90 | Current median p90 | Delta | Improvement | Projected internal FPS baseline -> current |
|---|---|---:|---:|---:|---:|---:|
| 1920x1080 | Block | 0.7170 ms | 0.6960 ms | -0.0210 ms | 2.93% | 1394.70 -> 1436.78 (+42.08) |
| 1920x1080 | Kitty | ~0.009 ms | ~0.009 ms | 0.000 ms | 0% | ~111111 -> ~111111 (+0) |
| 1920x1080 | Sixel | 39.2310 ms | 15.3755 ms | -23.8555 ms | 60.81% | 25.49 -> 65.04 (+39.55) |
| 2560x1600 | Block | 1.0700 ms | 1.0580 ms | -0.0120 ms | 1.12% | 934.58 -> 945.18 (+10.60) |
| 2560x1600 | Kitty | ~0.013 ms | ~0.013 ms | 0.000 ms | 0% | ~76923 -> ~76923 (+0) |
| 2560x1600 | Sixel | 52.4900 ms | 22.8110 ms | -29.6790 ms | 56.54% | 19.05 -> 43.84 (+24.79) |

The complete sorted per-process p90 audit trail is:

| Surface | Backend | Baseline p90 values (ms) | Current p90 values (ms) |
|---|---|---|---|
| 1920x1080 | Block | 0.662, 0.680, 0.710, 0.724, 0.731, 0.754 | 0.657, 0.670, 0.682, 0.710, 0.712, 0.744 |
| 1920x1080 | Kitty | 0.008, 0.009, 0.009, 0.009, 0.009, 0.010 | 0.008, 0.008, 0.008, 0.009, 0.009, 0.010 |
| 1920x1080 | Sixel | 38.669, 38.996, 39.063, 39.399, 39.439, 39.521 | 15.026, 15.143, 15.231, 15.520, 15.702, 15.763 |
| 2560x1600 | Block | 1.027, 1.046, 1.059, 1.081, 1.095, 1.126 | 1.032, 1.051, 1.057, 1.059, 1.067, 1.105 |
| 2560x1600 | Kitty | 0.012, 0.013, 0.013, 0.013, 0.013, 0.013 | 0.012, 0.013, 0.013, 0.013, 0.013, 0.013 |
| 2560x1600 | Sixel | 51.673, 52.330, 52.445, 52.535, 52.723, 53.877 | 22.599, 22.642, 22.755, 22.867, 23.053, 23.065 |

At 1920x1080, the literal Kitty medians are 0.0090 and 0.0085 ms. The
millisecond output precision cannot resolve that half-microsecond difference as a
material change, so the progression table records zero. The 2560x1600 medians are
both 0.0130 ms. Kitty mean terminal bytes were also unchanged at 175,735 and
269,574. Sixel mean bytes changed from 306,362 to 227,116 and from 480,010 to
317,909. Block mean bytes varied by at most 14 bytes within a run set. The harness
does not emit output hashes.

Repeated full batched runs kept Sixel p90 within 2.1 ms at 1920x1080 and 2.2 ms
at 2560x1600. Block stayed below 1.1 ms p90 and Kitty below 0.014 ms p90 in the
selected run. The very high Block and Kitty FPS calculations are computation
ceilings only. They do not predict terminal throughput. Sixel is the only
internal encoder that falls below 30 FPS on this fixture before any terminal
cost is added.

Limits: this is one static local page at two scroll positions, not a motion,
animation, photographic, or multi-page corpus. The counting writer never blocks
and does not model tty copies or syscalls. The test does not measure terminal
parsing, decompression, palette handling, raster draw, refresh cadence, tearing,
or delivered FPS. Those remain part of the real-terminal matrix.

---

## Colour conversion research (2026-07-15)

This follow-up tested whether a general or SIMD YCbCr-to-RGBA converter should
replace either production path. Block keeps its fused compositor and its retained
4:2:0 scalar fast path. Sixel should target its encoder before conversion because
palette, pixel-map, bitset, and output generation dominate its measured path.

### Block measurements

The production benchmark used JPEG q90 at 2560x2880. The CPU figures are
aggregate profile shares and derived CPU time per completed grid.

| Measure | Result |
|---|---:|
| Production q90 median | 10.283 ms |
| Direct Block composition | 51.6% cumulative CPU |
| JPEG decode | 44.0% cumulative CPU |
| `color.YCbCrToRGB` | 0.99% aggregate CPU, about 0.23 CPU-ms/grid |
| Three average divisions | About 0.46 CPU-ms/grid |
| Warm sampler preparation | 12.827 us |
| Serial sampling plus conversion | 4.379 ms |

Block already averages the source Y, Cb, and Cr planes, performs one RGB
conversion per terminal sub-cell, binarises the result, and writes the final grid
inside persistent row-band workers. No reviewed module provides Nyala's weighted
region averages across all six YCbCr subsampling ratios. A full-frame converter
would add an intermediate surface and memory pass. A second worker pool would
duplicate the existing row-band pool.

#### Retained 4:2:0 scalar fast path

The completed follow-up specialises the common 4:2:0 chroma sampling path and
unrolls the common eight-pixel luma span. Other ratios and span widths keep the
generic fallback. The change adds no workers and allocates nothing during warm
sampling.

The matched benchmark used one fixed q90 2560x2880 payload, a 160x90 sextant
grid, `GOMAXPROCS=32`, and a fixed 16-worker benchmark pool. Fifteen end-to-end pairs ran
in interleaved baseline and candidate order. The candidate won 14 pairs, run order
did not explain the result, and the paired test reported p=0.000 (p < 0.001).
The 10.283 ms figure above is earlier profile context; the paired baseline below
is the acceptance comparison.

| End-to-end statistic | Baseline | Candidate | Time saved | Improvement |
|---|---:|---:|---:|---:|
| Median | 10.202621 ms | 10.022665 ms | 0.179956 ms | 1.76% |
| p90 | 10.311689 ms | 10.084548 ms | 0.227141 ms | 2.20% |
| p95 | 10.341147 ms | 10.110773 ms | 0.230374 ms | 2.23% |
| Maximum | 10.369346 ms | 10.163974 ms | 0.205372 ms | 1.98% |

| Kernel measure | Baseline | Candidate | Improvement |
|---|---:|---:|---:|
| Isolated sampler | 4.390641 ms | 3.209646 ms | 26.90% |
| `pixelAt` | 52.65 ns | 37.89 ns | 28.03% |

The ten-pair saturated run used the same fixed 16 workers. The candidate won every pair
and reported p=0.000 (p < 0.001).

| Saturated measure | Baseline | Candidate | Improvement |
|---|---:|---:|---:|
| Median completed-grid interval | 1.257890 ms | 1.062180 ms | 15.56% |
| Throughput | 448.02 MB/s | 530.57 MB/s | 18.43% |

End-to-end CPU samples fell 11.61%, isolated samples fell 27.52%, and cumulative
`pixelAt` CPU per grid fell 28.15%. The maximum saturated interval improved 15.76%.
Warm sampling remained at 0 B/op and 0 allocs/op. Saturated allocations fell from
12 to 10 per operation. The independent oracle and focused tests passed across
every YCbCr ratio, scheme, overlay, fallback, parallel-parity case, and adversarial
bound. `go test -race ./internal/render/block`, `just fmt-check`, `just build`,
`just test`, and `just lint` also passed.

Retain the fast path. Its 1.76% end-to-end median gain is below the former 5% gate,
but the paired result is statistically sound, exact, uses the existing cores, and
improves p90, p95, maximum latency, CPU use, and saturated throughput. Small gains
remain acceptable when those conditions hold. The fixed-payload benchmark excludes
CDP delivery, terminal work, other production pages, and other architectures.

### Block ANSI 256-colour conversion (internal rendering test, 2026-07-16)

The bounded comparison measured three RGB-to-xterm-256 arms: the former
cube-or-exact-grey formula, a corrected nearest cube-versus-grey formula using
squared RGB distance, and an offline-generated 32 KiB perceptual LUT. The LUT has
one byte for each 5-bit RGB key and searches only canonical xterm indices 16..255
when generated. Production performs one indexed load. It does no search, start-up
generation, dithering, content sampling, or configurable-palette access.

The five committed PNG fixtures and overlay run sets ran at 120x40 and 240x86.
Each source was scaled to the quadrant sample footprint, composed with its crisp
text overlay, then full-repainted through `GridDiff.Emit` in ANSI 256-colour mode.
Quality used a truecolour raster of that composed grid as its reference. Before
raster comparison, both colours in every candidate cell were replaced by the
canonical RGB value of the emitted xterm index. This emulates the actual fixed
palette display rather than scoring the unconverted RGBA grid.

Six fresh processes ran with arm and fixture order rotated, `GOMAXPROCS=4`, GC at
100%, four warmups, and 21 timed samples per arm and fixture-size cell. Each arm
therefore has 1,260 full-path samples. Mapper time covers both colours of every
cell and amortises each sample over 64 passes. Full path covers compose plus a
complete indexed diff emit to a reused buffer. The p90 is taken from each arm's
combined 1,260-sample distribution, not from per-cell summaries. FPS is its
reciprocal. Allocations were identical within each geometry: 41 at 120x40 and 87
at 240x86, or 64 averaged across the matrix. The LUT arm calls the production
`block.XTerm256` accessor, including its embedded-string table load.

| Arm | Mapper mean | Mapper p90 | Full mean | Full p90 | Internal FPS | Run p90 range | Allocs | Mean payload |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| Former formula | 53.939 us | 98.355 us | 0.816360 ms | 1.399837 ms | 714.37 | 1.363707..1.431656 ms | 64.0 | 84,888 B |
| Nearest-two | 260.900 us | 441.023 us | 0.873034 ms | 1.516894 ms | 659.24 | 1.511947..1.523398 ms | 64.0 | 87,745 B |
| Perceptual LUT | 41.198 us | 67.032 us | 0.813130 ms | 1.367396 ms | 731.32 | 1.329855..1.459407 ms | 64.0 | 85,917 B |

| Arm | PSNR mean / worst | SSIM mean / worst | dE mean | Mean cell-p95 | Worst mean / p95 | Low-contrast text cells |
|---|---:|---:|---:|---:|---:|---:|
| Former formula | 31.527955 / 27.882311 dB | 0.946861107 / 0.873665341 | 1.735178 | 7.912803 | 3.199048 / 13.024775 | 18 |
| Nearest-two | 36.678401 / 33.076967 dB | 0.984075344 / 0.968215506 | 1.358837 | 6.210657 | 2.655186 / 11.177667 | 18 |
| Perceptual LUT | 34.388752 / 30.978571 dB | 0.976917333 / 0.954109397 | 1.293465 | 5.624615 | 2.298853 / 8.932386 | 16 |

Adopt the perceptual LUT. Against nearest-two it improves mean CIEDE2000 by
4.81%, mean cell-p95 by 9.44%, worst mean by 13.42%, and worst p95 by 20.09%.
It adds no allocations or low-contrast text cells and improves aggregate full-path
p90 by 9.86%. It also improves p90 by 2.32% against the former formula. PSNR and
SSIM favour nearest-two because they measure channel and structural error rather
than perceptual colour distance; the stated decision gives colour correctness
priority and uses CIEDE2000 for selection. The generated asset is deterministic,
exactly 32,768 bytes, and a focused exhaustive test pins every output to indices
16..255. The command is:

```console
for run in 0 1 2 3 4 5; do
  NYALA_COLOUR256_BENCH=1 NYALA_COLOUR256_BENCH_RUN="$run" \
    go test ./internal/render/block -run TestColour256Benchmark -count=1 -v
done
```

This is an internal rendering test. It excludes CDP delivery, terminal parsing and
draw time, terminal palette reconfiguration, and manual terminal inspection. The
estimated FPS is processing capacity, not observed display FPS.

### Sixel measurements

During motion, q90 JPEG frames decoded to decoder-native YCbCr. The former Kitty
consumer conversion helper then materialised a reusable full-frame RGBA buffer before
`x/ansi/sixel` encodes it.

| Production q90 State path | Median | Range | B/op | allocs/op |
|---|---:|---:|---:|---:|
| Base64 and JPEG decode, conversion, Sixel encode | 1.039 s | 1.009 to 1.055 s | 791,275,808 | 14,859,761 |
| Isolated warm Kitty consumer conversion | 14.321 ms | 14.292 to 14.342 ms | 0 | 0 |

The profile puts `imageutil.DrawYCbCr` at 1.50% flat CPU and
`sixel.Encoder.Encode` at 95.17% cumulative CPU. Removing conversion entirely has
a 14.321 ms, or 1.38%, wall-time ceiling on the measured production path.

The matched conversion experiment reused persistent workers and split the RGBA
destination into disjoint row bands. Every parallel result was byte-equal to the
serial result, warm, and allocation-free.

| Conversion path | Median | Saving from serial | Share of 1.039 s path saved |
|---|---:|---:|---:|
| Serial | 14.321 ms | 0 | 0% |
| 4 workers | 3.830 ms | 10.491 ms | 1.01% |
| 8 workers | 2.171 ms | 12.150 ms | 1.17% |
| 16 workers | 1.730 ms | 12.591 ms | 1.21% |
| 31 workers | 1.343 ms | 12.978 ms | 1.25% |

The 10.66x isolated speed-up at 31 workers does not justify production complexity
while it saves only 1.25% end to end. Benchmark Sixel palette construction,
pixel-map and bitset generation, and output first. Keep conversion parallelism as
a secondary candidate if the profile shifts after those costs fall.

### Sixel encoder adoption (2026-07-15)

Production switched from `charmbracelet/x/ansi/sixel` to an encoder derived from
the pure-Go MIT `mattn/go-sixel` v0.0.12. The renderer retains one encoder, redirects
each call through a swappable writer proxy, and serialises concurrent calls with a
mutex. The encoder owns the complete parameterised DCS and its scratch buffers.
`State` retains the renderer across frames and backend switches. The capture
decoder accepts both the legacy `ESC P q` introducer and parameterised
`ESC P ... q` form.

The production policy uses an adaptive palette with `Colors=256` and
`Dither=false`. YCbCr, RGBA, PNG, Kitty, capture, and backend-switch paths retain
their contracts.

#### Matched fixed-frame result

The matched headless benchmark used one fixed 2560x2880 q90 4:2:0 frame, five
samples, and `GOMAXPROCS=1`. Each State reused its buffers and renderer.

| Measure | Former `x/ansi` | Adopted `mattn` | Change |
|---|---:|---:|---:|
| State median | 960.437 ms | 101.670 ms | 89.41% lower, 9.45x faster |
| State range | 958.698 to 989.485 ms | 101.345 to 102.375 ms | Lower and tighter |
| B/op | 797,933,112 | 39,828,000 | 95.01% lower |
| allocs/op | 14,857,669 | 1,042 | 99.993% lower |
| Payload median | 2,546,731 B | 2,076,375 B | 18.47% lower |
| State processing capacity | 1.04 frames/s | 9.84 frames/s | 8.80 frames/s higher |

Capacity is Nyala State processing, not terminal FPS. Sequential base64 decode,
JPEG decode, and State processing take 110.931 ms, or about 9.01 processing
frames/s. A 15 frames/s budget is 66.67 ms. State alone remains 35.00 ms over
that budget and needs a further 34.4% reduction.

#### Authorised quality trade-off

The adopted encoder reduces colour accuracy on the fixed frame. The owner accepts
this trade-off because Sixel is the compatibility backend and the speed gain is
large. Output remains 2560x2880, opaque, and protocol-valid.

| Quality measure | Former `x/ansi` | Adopted `mattn` |
|---|---:|---:|
| PSNR | 45.395 dB | 39.268 dB |
| SSIM | 0.9974399 | 0.9894787 |
| RGB absolute p50 / p95 / p99 / maximum | 1 / 3 / 8 / 85 | 2 / 5 / 11 / 98 |
| Delta E 2000 mean / p95 | 0.5118 / 2.3953 | 0.9989 / 3.2267 |

The five-image quality set selected 256 colours over 192. The 256-colour arm produced
higher SSIM on all five images and safer grayscale output.

| Corpus measure | 192 colours | 256 colours |
|---|---:|---:|
| Median PSNR | 33.294 dB | 33.466 dB |
| Median SSIM | 0.9930730 | 0.9938816 |
| Worst SSIM | 0.9699998 | 0.9741557 |
| Median encode | 73.837 ms | 82.877 ms |

For 256 colours, worst PSNR was 32.930 dB, worst encode was 108.618 ms, median
payload was 2,076,375 B, and the largest payload was 3,712,566 B. Dithering was
slower, larger, and lower quality. This arm did not test fixed palettes, so their
same-frame build cost and stale-palette quality remain open.

#### Pre-ToRGBA profile and limit

The adopted path spends 15.05% of CPU in YCbCr-to-RGBA conversion, 30.10% in
palette sampling, 6.96% in LUT construction, 7.93% in pixel mapping, and about
31% in band generation. Allocation space is 74.73% RGBA, 17.94% paletted image,
and 4.72% quantisation. These percentages are pre-ToRGBA CPU profile shares, not
per-frame wall times. In particular, 15.05% does not mean 15.05 ms.

This profile motivated the bounded fused-RGBA follow-up below. Headless capacity
does not close the real-terminal FPS, decode, draw, byte, or tearing tasks.

#### Bounded Sixel RGBA decode and four-worker policy (2026-07-15)

**Superseded 2026-07-17.** Sixel now uses PNG for every CDP frame, so production no
longer contains this JPEG-specific branch or worker cutoff. The measurement remains
as historical evidence.

The follow-up used `jpegn.Options{ToRGBA: true}` only for live moving
Sixel JPEG when the actual session had one to four decode workers. This Sixel RGBA
safety cutoff was independent of the adaptive default,
`min(4, max(2, GOMAXPROCS/2))`. `NYALA_DECODE_WORKERS` accepted manual values from
1 to 16; values above 4 kept decoder-native YCbCr for Sixel. The engine recorded the
actual count at session start and cleared it at teardown. Backend switches read the
active backend and count together, so Block remained native YCbCr and returning to
Sixel restored the policy.

Kitty direct PNG, capture PNG, settled Sixel PNG, Block composition, and the generic
decode fallback were unchanged by that experiment.

The independent matched benchmark used the fixed 2560x2880 q90 4:2:0 frame from
baseline commit `2560db7`, with five samples per arm.

| Sequential measure | Native YCbCr | Fused RGBA | Change |
|---|---:|---:|---:|
| JPEG decode | 9.108 ms | 13.435 ms | 4.327 ms slower |
| Sixel State | 105.184 ms | 88.637 ms | 16.547 ms faster |
| Decode plus State | 115.005 ms | 103.014 ms | 11.990 ms, 10.43% faster |
| Allocation | 52.27 MB/op | 74.31 MB/op | 42.2% higher |

The fused decode costs 4.327 ms more, but it avoids the encoder's later full-frame
conversion and wins end to end. At 88.637 ms, State alone remains 21.97 ms over
the 66.67 ms budget for 15 frames/s and needs a further 24.8% reduction. These are
processing figures, not terminal FPS.

| Supply and memory measure | Native YCbCr | Fused RGBA | Change |
|---|---:|---:|---:|
| Saturated supply | 986 frames/s | 210 frames/s | Lower, still above encoder demand |
| Saturated CPU | 1285% | 378% | 70.6% lower |
| Peak RSS | 423,120 KiB | 625,760 KiB | 47.9% higher |
| Per-frame allocation | 11.63 MB | 55.86 MB | 44.23 MB higher |
| Retained live plus State bound | 458.6 MiB | 463.6 MiB | 5.0 MiB, 1.1% higher |

Peak RSS rises because Go retains the larger transient heap after saturation; it
does not represent the steady retained live-frame bound. Four current Block workers
supply 372 frames/s. Applying the documented 29 ms slow-decode estimate still gives
134 completed grids/s, 2.24x the 60 frames/s arrival target. Two workers were
rejected because that slow-path margin was too small.

A manual five-worker Sixel session stays native YCbCr and measures 114.568 ms
sequentially with 438.2 frames/s saturated supply. Block decode plus a 160x90 grid
moves from 10.042 to 9.963 ms, which is noise rather than a claimed Block gain.

| Quality measure | Native YCbCr | Fused RGBA | Change |
|---|---:|---:|---:|
| Sixel payload | 2,142,324 B | 2,168,998 B | 1.25% larger |
| PSNR | 39.1808 dB | 39.1575 dB | 0.023 dB lower |
| SSIM | 0.9897798 | 0.9901145 | Higher |
| Maximum channel error | 143 | 142 | One level lower |

The raw decoded images differ by at most one channel level, with PSNR 63.129 dB
and SSIM 0.99989759. Retain the fused path and four-worker cap: the matched
sequential gain is 10.43%, quality is stable, saturated supply exceeds demand, and
the retained bound rises only 1.1%. The public fixed-palette sweep below became the
next narrow Sixel target.

##### Direct native-YCbCr Sixel follow-up (2026-07-17)

A later candidate removed the fused full-frame RGBA result and sampled and mapped
decoder-native YCbCr planes inside Nyala's opaque encoder. Focused tests matched a
standard materialised-RGBA oracle across all six YCbCr subsampling ratios, odd
dimensions, non-zero bounds, and padded strides. A bounded refinement cached the
chroma conversion terms for each shared sample. This removes full-frame RGBA
materialisation, not the YCbCr-to-RGB maths needed by palette sampling and mapping.
No candidate code was retained.

The matched gate used commit `52ac91e` as the fused-RGBA baseline. Six rotated
sequential process pairs used `GOMAXPROCS=4`, `GOGC=100`, four warm-ups, and 30
retained samples per arm. Timings include q90 JPEG decode and complete Sixel byte
generation. They do not include terminal parsing or drawing.

| Resolution | Fused RGBA median / p90 | Direct YCbCr median / p90 | Median change | Process-median ranges | B/op change |
|---|---:|---:|---:|---:|---:|
| 1920x1080 | 29.495 / 29.829 ms | 32.909 / 33.207 ms | 11.57% slower | 29.162-29.829 / 32.696-33.207 ms | 21.17 -> 8.72 MB, 58.82% lower |
| 2560x1600, refined | 42.810 / 43.146 ms | 47.686 / 47.980 ms | 11.39% slower | 42.487-43.127 / 47.533-47.875 ms | 40.63 -> 16.06 MB, 60.49% lower |
| 2560x2880 | 71.653 / 72.239 ms | 70.787 / 71.125 ms | 1.21% faster | 70.927-72.228 / 70.514-71.021 ms | 49.88 -> 27.74 MB, 44.39% lower |

The 2560x2880 timing ranges overlap. The two normal target sizes regress by more
than 11% with fully separated ranges. The candidate changes payload by 0.245% at
1920x1080 and 0.069% at 2560x1600 because `jpegn` fused conversion and Go's YCbCr
conversion can round a channel one level differently; the 2560x2880 payload is
byte-identical. Allocation counts stay flat at the two target sizes and rise at
2560x2880.

The decision at this point retained fused RGBA for moving Sixel JPEG because direct
YCbCr did not lower processing time at the normal target sizes. The later PNG-only
decision supersedes both JPEG layouts. Do not add a resolution threshold or another
scalar layout without new profile evidence.

##### Sustained moving PNG versus q90 JPEG (2026-07-17)

The temporary live benchmark kept one Chromium screencast active per format and
resolution. It drove 110 predetermined full-surface translations of
the Linux Matters page fixture. An eight-bit marker identifies every state, and
the gate rejects a wrong marker, wrong dimensions, a repeated adjacent frame hash,
an invalid Sixel payload, or a JPEG frame that bypasses the then-production
fused-RGBA decode policy. Each arm discards ten warm-ups and retains 100 frames.

Three fresh processes counterbalance resolution and format order with
`GOMAXPROCS=4` and `GOGC=100`, giving 300 retained frames per arm. Each retained
frame measures mutation-to-CDP-event delivery, `decodeLiveFrame`, one reused Sixel
renderer, and their serial sum. Service p90 is the directly measured decode plus
encode span. No stage p90 is reported. Projected internal FPS is `1000 / service p90`.
The serial synthetic result does not exercise the production decode queue,
worker goroutines, collector, latest-wins drops, or terminal parse and draw.
Aggregate medians and p90 values pool all 300 retained frames per arm. They are not
derived from the three process summaries; the per-process p90 table below remains
the drift check.

The counterbalanced order was:

| Process | Resolution order | Format order within each resolution |
|---|---|---|
| 0 | 1920x1080, 2560x1600, 2560x2880 | PNG/JPEG, JPEG/PNG, PNG/JPEG |
| 1 | 2560x1600, 2560x2880, 1920x1080 | JPEG/PNG, PNG/JPEG, JPEG/PNG |
| 2 | 2560x2880, 1920x1080, 2560x1600 | PNG/JPEG, JPEG/PNG, PNG/JPEG |

Aggregated results:

| Resolution | Format | Delivery median | Decode median | Encode median | Service median / p90 | Serial synthetic median / p90 | Projected internal FPS | CDP payload median | Sixel payload median |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|
| 1920x1080 | PNG | 36.415 ms | 6.124 ms | 18.264 ms | 24.525 / 25.494 ms | 61.087 / 67.348 ms | 39.22 | 482,111 B | 391,493 B |
| 1920x1080 | JPEG q90 | 36.996 ms | 5.352 ms | 23.500 ms | 29.074 / 30.301 ms | 65.959 / 68.793 ms | 33.00 | 271,328 B | 781,830 B |
| 2560x1600 | PNG | 43.764 ms | 10.966 ms | 27.368 ms | 38.498 / 39.888 ms | 82.546 / 86.147 ms | 25.07 | 817,780 B | 668,119 B |
| 2560x1600 | JPEG q90 | 42.753 ms | 8.628 ms | 34.011 ms | 42.789 / 44.434 ms | 85.772 / 89.482 ms | 22.51 | 408,004 B | 1,240,905 B |
| 2560x2880 | PNG | 70.874 ms | 18.845 ms | 39.313 ms | 58.576 / 62.228 ms | 129.723 / 135.129 ms | 16.07 | 1,265,148 B | 1,086,704 B |
| 2560x2880 | JPEG q90 | 68.516 ms | 13.390 ms | 46.703 ms | 60.273 / 62.658 ms | 128.906 / 133.447 ms | 15.96 | 564,824 B | 1,765,363 B |

PNG cuts service p90 by 15.86% at 1920x1080 and 10.23% at 2560x1600. It is
0.69% faster at 2560x2880, within run variation. After CDP delivery, PNG's serial
synthetic p90 is 2.10% faster, 3.73% faster, and 1.26% slower respectively. PNG
raises the CDP payload by 77.69%, 100.43%, and 123.99%, but cuts generated Sixel
bytes by 49.93%, 46.16%, and 38.44%.

Per-process p90 values show the run variation:

| Resolution | PNG service p90 | JPEG service p90 | PNG serial p90 | JPEG serial p90 |
|---|---|---|---|---|
| 1920x1080 | 25.723, 25.131, 25.494 ms | 30.366, 30.687, 29.629 ms | 69.050, 65.608, 65.334 ms | 68.548, 68.830, 68.675 ms |
| 2560x1600 | 40.449, 39.589, 39.698 ms | 45.684, 44.286, 43.830 ms | 84.210, 87.239, 83.964 ms | 88.313, 90.366, 86.950 ms |
| 2560x2880 | 61.653, 59.315, 62.982 ms | 61.492, 61.488, 64.747 ms | 134.958, 133.861, 136.117 ms | 132.569, 132.473, 134.735 ms |

The resource figures are medians of the three warm per-process allocation probes:

| Resolution | Format | B/op | allocs/op |
|---|---|---:|---:|
| 1920x1080 | PNG | 14,344,539 | 1,600 |
| 1920x1080 | JPEG q90 | 21,193,091 | 1,333 |
| 2560x1600 | PNG | 26,993,932 | 1,755 |
| 2560x1600 | JPEG q90 | 40,762,856 | 1,338 |
| 2560x2880 | PNG | 47,586,793 | 1,720 |
| 2560x2880 | JPEG q90 | 71,937,814 | 1,333 |

PNG lowers warm B/op by about 32% to 35%, while raising allocation counts.
Its Sixel output scores 39.69/0.99456, 39.88/0.99325, and 39.22/0.99235
PSNR/SSIM against the PNG source. JPEG q90 scores 36.98/0.98847,
37.37/0.98909, and 37.22/0.98930. Source JPEG quality remains at least 45.03 dB
PSNR and 0.99522 SSIM.

The initial recommendation was to keep q90 JPEG for moving Sixel until terminal
testing. The product decision below supersedes it. The temporary harness was removed
after the sweep rather than retain 505 lines of benchmark-only code.

##### PNG-only Sixel production decision (2026-07-17)

Sixel now requests PNG from CDP for every live frame. This reverses the former
settled-PNG/moving-q90-JPEG state machine and removes its motion controller,
Sixel-specific JPEG quality, fused-RGBA JPEG worker cutoff, and associated tests.
Block retains q90 JPEG and native YCbCr because its separate quality and performance
sweep supports that path. Kitty remains direct PNG. Capture remains PNG.

The owner accepts PNG's 2560x2880 serial synthetic p90 regression of 1.26%, about
0.09 projected FPS, for its higher fidelity, 38.44% smaller Sixel output at that
size, and one Sixel input format. At 1920x1080 and 2560x1600, PNG improves serial
p90 by 2.10% and 3.73%, about 0.31 and 0.43 projected FPS, while cutting Sixel
output by 49.93% and 46.16%. Do not add a resolution threshold.

This is a synthetic internal-rendering decision, not terminal validation. PNG raises
the CDP payload by 78% to 124%, while the terminal receives 38% to 50% fewer Sixel
bytes. Real-terminal write, parse, draw, FPS, and tearing measurements must confirm
the expected transport benefit when the owner can test at a terminal.

#### Sixel hot-path research and decisions (2026-07-15)

`Encoder.Encode` accepts an `image.Image`; it does not decode JPEG, and the encoder
implementation does not import `jpegn`. Nyala decodes each CDP JPEG before `State`,
and bounded live Sixel pools already request packed RGBA through
`jpegn.Options{ToRGBA: true}`. The earlier 14.321 ms figure measured isolated warm
YCbCr-to-RGBA wall time in the former `x/ansi` State path. The 15.05% figure above
is a pre-ToRGBA CPU share, not a duration. The matched 2560x2880 q90 table above is
the acceptance evidence: the extra decode time is offset by a larger State saving,
which cuts sequential decode plus State by 11.990 ms.

An independent post-ToRGBA CPU profile attributed about 39.4% to the band loop and
RLE, 36.0% to palette sampling, 7.8% to LUT construction, and 9.6% to pixel mapping.
These shares are directional profile evidence, not acceptance data.

The public seams are `Encoder.Palette`, which caches a fixed palette and its LUT,
`Colors`, `Dither`, `Transparent`, and direct `*image.Paletted` input. Quantisation,
mapping, band construction, RLE, and scratch are private. Private-stage tests belong
in a temporary upstream worktree. Keep stage-only, encoder, and combined decode plus
State results separate. Do not combine medians from different fixtures or harnesses.

##### Opaque-frame alpha-only experiment (2026-07-16)

Nyala's current CDP page frames are composited and opaque. A temporary copy of
`go-sixel` v0.0.12 removed only its post-map transparent-pixel remap scan, palette
alpha scan, opaque scratch, and per-pixel opaque branch. Source pixels, adaptive
palette selection, 256-colour budget, mapping, dithering policy, band construction,
RLE, retained encoder state, and output writes stayed unchanged. No dependency or
production file changed.

The fixture was the first settled 2560x1600 Sixel PNG frame captured through CDP
from `layout-13-linuxmatters`. An exhaustive scan confirmed all 4,096,000 source
pixels had alpha 255. Eight alternating process pairs measured 50 warm encodes per
arm after four warm-ups. The table aggregates those runs; the mean range is the
range of the eight per-run means, and the within-run standard deviation is averaged
across the eight runs.

| Arm | Mean | Mean range | p90 | Estimated FPS from p90 | Average within-run standard deviation | allocs/op | Payload |
|---|---:|---:|---:|---:|---:|---:|---:|
| Current alpha handling | 46.832 ms | 46.593 to 47.155 ms | 47.843 ms | 20.902 | 0.775 ms | 1,303 | 461,879 B |
| Opaque-only candidate | 42.891 ms | 42.588 to 43.537 ms | 43.843 ms | 22.809 | 0.736 ms | 1,303 | 461,879 B |

Removing those alpha operations reduced the mean by 3.941 ms (8.416%) and p90 by
4.000 ms (8.361%). All sixteen outputs had SHA-256
`0acefb7d583330053463e7b7a5e3eab5ec93f7c399d768a0ef5a95f127abfccb`. The paired
mean reduction ranged from 3.296 to 4.363 ms, with a 0.325 ms standard deviation and
a paired t statistic of 34.35. The paired p90 reduction had a 0.554 ms standard
deviation and a t statistic of 20.42. The gain is above measured noise.

This result does not accept a palette algorithm. Nyala now owns the minimum encoder
subset needed by its opaque CDP frames. The copied code keeps palette selection,
the 256-colour policy, mapping, band construction, the 255-count RLE limit, framing,
retained scratch, and output writes unchanged. The upstream decoder remains the
round-trip test oracle.

The production implementation was measured again on an opaque 2560x1600 RGBA image
scaled from the committed Linux Matters fixture. Eight process runs performed 20
warm encodes per arm. P90 is the highest of the eight run means. Both arms emitted
the same 934,772-byte payload. Strict tests also require byte identity for opaque
RGBA, NRGBA, and YCbCr inputs.

| Arm | Mean | Run-mean range | Run-mean standard deviation | p90 | Estimated FPS from p90 | allocs/op |
|---|---:|---:|---:|---:|---:|---:|
| `go-sixel` v0.0.12 alpha path | 67.119 ms | 66.715 to 67.828 ms | 0.420 ms | 67.828 ms | 14.743 | 1,301 |
| Nyala opaque encoder | 63.190 ms | 62.868 to 63.634 ms | 0.298 ms | 63.634 ms | 15.715 | 1,301 |

The production path reduces mean processing by 3.929 ms (5.853%) and p90 by
4.194 ms (6.183%). Fixture and machine differences make this result separate from
the CDP experiment above. Both measurements show a repeatable total-path gain.

##### Palette track, benchmark A

Benchmark A used commit `5598f343`, Go 1.26.4, Linux amd64, `GOMAXPROCS=1`, five
samples, and one 2560x2880 q90 frame decoded to RGBA by `jpegn`. Fixed palettes use
the `go-sixel` sample budget and median quantiser. Warm reuses the palette, LUT, and
encoder scratch. Cold replaces the palette and rebuilds the LUT. Palette plus cold
adds external palette construction. Decode plus palette plus cold adds JPEG decode.
All values are medians from this benchmark only.

| Colours | Warm State | Cold State | Palette plus cold | Decode plus palette plus cold | Pre-palettised output control |
|---:|---:|---:|---:|---:|---:|
| Adaptive | 91.667 ms | Not measured | Not measured | 105.163 ms | Not applicable |
| 64 | 36.884 ms | 38.909 ms | 67.666 ms | 82.807 ms | 29.118 ms |
| 128 | 41.859 ms | 45.809 ms | 77.589 ms | 91.748 ms | 40.237 ms |
| 216 | 48.435 ms | 54.118 ms | 87.331 ms | 102.478 ms | 55.144 ms |
| 255 | 49.665 ms | 56.768 ms | 90.045 ms | 105.142 ms | 60.847 ms |

Fixed 255 is 45.8% faster than adaptive when warm. Its same-frame output is byte
identical to adaptive, but rebuilding the palette for every frame leaves combined
decode plus State unchanged within 0.021 ms. The pre-palettised arm is an output-path
control, not a band-only result. It keeps palette-register output, opacity scanning,
bitsets, DCS framing, scratch reuse, RLE, and the final write. It excludes conversion,
adaptive quantisation, LUT construction, mapping, and alpha remapping.

| Palette | Median PSNR | Worst PSNR | Median SSIM | Worst SSIM |
|---|---:|---:|---:|---:|
| Adaptive | 34.944 dB | 33.490 dB | 0.995019 | 0.973668 |
| Fixed 255 | 34.944 dB | 33.490 dB | 0.995019 | 0.973668 |
| Fixed 216 | 34.916 dB | Not recorded | 0.994726 | Not recorded |

The quality set is `linuxmatters.png` plus `testpage-01-grayscale.png` through
`testpage-04-color.png`. Fixed 64 and 128 lost more quality than fixed 216. Static
reuse is not production evidence. The later stateless 64K sample-cap matrix
supersedes the proposed temporal motion fixture.

##### Palette track, benchmark B

Benchmark B used another canonical frame and harness. Its medians cannot be compared
with benchmark A. It tests aggressive palette modes and reports incomplete quality
data.

| Arm | Warm encoder | Payload | PSNR | SSIM |
|---|---:|---:|---:|---:|
| Adaptive | 88.24 ms | 2,118,201 B | 37.31 dB | Not recorded |
| Direct Plan9 256 | 27.81 ms | 1,009,015 B | 29.67 dB | 0.996700 global Y |
| WebSafe 216 | 27.35 ms | Not recorded | 25.09 dB | Not recorded |
| Reused current median | 42.76 ms | Not recorded | Exact static output | Not recorded |
| `go-quantize` | 37.43 ms | Not recorded | 30.62 dB | Not recorded |

Direct Plan9 and WebSafe 216 lose too much quality. The later matrix below measures
`octreequant` directly; do not infer results for any other algorithm from these arms.

##### Internal Sixel palette matrix (2026-07-16)

This is an internal rendering test, not terminal FPS. It measures complete Sixel
byte generation and combined q90 JPEG decode plus byte generation. It excludes tty
transport and terminal decode, draw, and refresh.

The opt-in `just palette-bench` harness uses the five committed quality images at
1920x1080 and 2560x1600. Each source is composited opaque, scaled to the target
width, and top-cropped or bottom-padded to the target height before timing. Every
arm keeps the exact-colour fast path, 255 usable adaptive colours, current band and
RLE code, framing, and final write. Each of six independent processes rotates the
arm and fixture order, runs four warm-ups and three timed samples per cell, and pins
`GOMAXPROCS=4`, `GOGC=100`. The machine was Linux amd64, Go 1.26.4, on an AMD Ryzen
AI Max+ 395. Baseline commit was `1ad80b2`.

The table averages the ten cell means and cell p90s across the six processes. The
range and standard deviation are over process means. FPS is `1000 / p90`. Payload
is the ten-cell mean and does not include terminal transport wrappers.

| Arm | Full mean, range, SD | Full p90 / FPS | Decode plus encode mean | Decode plus encode p90 / FPS | allocs/op | mean payload |
|---|---:|---:|---:|---:|---:|---:|
| Original `go-sixel` v0.0.12 | 52.830 ms, 52.644 to 53.026, 0.135 | 53.574 ms / 18.67 | 62.980 ms | 64.102 ms / 15.60 | 1,296 | 558,357 B |
| Nyala serial-LUT baseline | 51.196 ms, 51.002 to 51.420, 0.156 | 51.861 ms / 19.28 | 61.946 ms | 63.168 ms / 15.83 | 1,296 | 558,357 B |
| Nyala parallel 15-bit LUT | 48.359 ms, 47.666 to 49.764, 0.798 | 49.439 ms / 20.23 | 58.101 ms | 59.301 ms / 16.86 | 1,304 | 558,357 B |
| Inherited 64K sample cap | 31.572 ms, 31.348 to 31.902, 0.195 | 31.972 ms / 31.28 | 40.443 ms | 41.312 ms / 24.21 | 1,286 | 568,106 B |
| `ericpauley/go-quantize` | 44.351 ms, 43.684 to 45.173, 0.571 | 45.760 ms / 21.85 | 54.844 ms | 57.472 ms / 17.40 | 525 | 515,687 B |
| `delthas/octreequant` | 171.350 ms, 170.332 to 172.546, 0.987 | 174.306 ms / 5.74 | 186.957 ms | 190.516 ms / 5.25 | 6,339,928 | 466,110 B |

Original, serial Nyala, and parallel Nyala emit 246 to 255 colour registers across
the cells; 64K emits 232 to 255, `go-quantize` emits 255, and `octreequant` emits
249 to 255. Both decoders return the requested dimensions in every cell, and the
production decoder returns opaque output.

Parallel construction changes only how the existing 32,768-entry nearest-colour
LUT is filled. Each worker owns a disjoint range. Palette selection, lookup values,
mapping, band generation, RLE, and framing are unchanged. All ten payloads match
the serial baseline byte for byte. Full encode falls 5.54%; combined decode plus
encode falls 6.21%. Every process mean improves. Adopt this bounded change.

The 64K cap changes only the median-cut sample budget from `2^18` to `2^16`. Against
the serial baseline, full encode falls 38.33% and combined decode plus encode falls
34.71%. Against the prior parallel-LUT production path, the reductions are 34.71%
and 30.39%. Mean payload rises 1.75%. It changes every non-trivial payload. The
phase gate below decides it.
Signed quality deltas below are candidate minus current Nyala. Positive PSNR and
SSIM are better; negative dE mean is better.

| Fixture and size | Payload | PSNR | SSIM | dE mean | dE p95 result |
|---|---:|---:|---:|---:|---|
| Linux Matters 1920x1080 | +7,177 B | -0.728 dB | +0.003628 | +0.0452 | improves |
| Linux Matters 2560x1600 | +18,107 B | +2.340 dB | -0.001580 | -0.0917 | improves |
| Grayscale 1920x1080 | -834 B | -0.004 dB | -0.000002 | +0.0004 | unchanged |
| Grayscale 2560x1600 | -7,065 B | -0.005 dB | -0.000003 | +0.0005 | unchanged |
| Basic 1920x1080 | +2,606 B | +0.014 dB | +0.000031 | +0.0002 | unchanged |
| Basic 2560x1600 | +4,059 B | +1.298 dB | +0.000148 | +0.4548 | worsens |
| CMYK 1920x1080 | +18,225 B | +0.030 dB | -0.000044 | +0.0029 | unchanged |
| CMYK 2560x1600 | +2,001 B | +0.049 dB | +0.000330 | +0.0224 | worsens |
| Colour 1920x1080 | +10,805 B | +0.580 dB | -0.000275 | +0.5332 | improves |
| Colour 2560x1600 | +42,404 B | +0.290 dB | +0.000123 | -0.3238 | slightly worsens |

The deterministic rare-colour gate generates one opaque fixture at each target
size. It covers every regular-grid sampling phase with 1 px and 2 px lines, badges,
blocks, icons, antialias shades, and rare saturated colours. `go-sixel` must decode
deterministic payloads at the requested dimensions, and the result must remain opaque.
WCAG 3:1 defines feature visibility.
CIEDE2000 2.3 and colour-category changes are recorded as quality evidence. Only a
feature disappearing, merging with its background, becoming unreadable, or losing
structure rejects the policy.

| Size | 64K stride | Features | Category changes | dE increases >= 2.3 | Current-to-64K dE >= 2.3 | Maximum output change |
|---|---:|---:|---:|---:|---:|---:|
| 1920x1080 | 6 | 288 | 13 | 18 | 20 | 7.767 dE, antialias shade phase (2,3) |
| 2560x1600 | 8 | 512 | 28 | 39 | 72 | 16.155 dE, antialias shade phase (4,1) |

Every feature stays above 3:1 and preserves its structure. Category changes repeat
byte for byte across runs. Examples include cyan badges becoming blue, blue 1 px
lines becoming magenta, red 2 px lines becoming yellow, yellow badges becoming red,
and magenta icons becoming blue. Accept this measured quality trade-off and retain
the 64K production cap. Reopen the decision only if later evidence shows content
disappearing, merging with its background, becoming unreadable, or losing structure.
The small exact-colour path stays byte-identical.

`go-quantize` is BSD-3-Clause, dependency-free, and last changed in 2020. It is
13.37% faster than the serial baseline on full encode, but slower than the inherited
64K arm and changes quality. Keep it test-only. `octreequant` is MIT,
dependency-free, and last changed in 2024. Its direct indexed output is 3.35 times
slower than the serial baseline with about 6.34 million allocations per operation;
reject it. The GPL `color-thief` candidate was excluded. `pixelize` was not added
because it is a quality reference without published speed evidence and would widen
the dependency and run-time cost.

The `go-sixel` decoder accepts every payload and returns the requested dimensions.
Quality and opacity use that decoder. Charm x/ansi v0.11.7 is no longer a Sixel
acceptance oracle because its DECGRI token-boundary defect can over-consume following
data bytes, including after a legal repeat of 255. PSNR is BT.601 luma, SSIM is the
validated 11x11 windowed luma implementation, and CIEDE2000 mean and p95 supply the
colour check. These quality calculations run outside the timed region.

##### RLE track

`go-sixel` v0.0.12 already delays each run decision, uses threshold 4, appends into
a reused output buffer with `strconv.AppendInt`, and performs one final write. It
splits repeat counts at 255 as a compatibility policy, not a Sixel grammar limit;
DEC STD 070 permits 32767. Nyala's retained opaque copy also packs each six-row
colour plane into one byte per x-coordinate, uses generation counters to skip absent
colours, detects and emits runs during the plane scan, omits a trailing zero run from
the payload, and reuses its output, plane, and presence buffers.

###### Source compatibility audit (2026-07-16)

This source audit records the linked snapshots and version boundaries. It replaces
manual repeat-parser confirmation only for those sources; it does not make unsafe
older versions compatible or confirm terminal presentation. A 32767 encoder ceiling
also does not make Nyala emit runs of 32767 pixels: `appendRun` receives a run from
one image row, so the largest useful count is the actual frame width, 1920 or 2560
in the target measurements.

| Implementation | Source verdict | Relevant cap or version | Nyala impact |
|---|---|---|---|
| [xterm](https://invisible-island.net/xterm/xterm.log.html#xterm_393) | Parses 32767; current output clips safely | Patch 371 bounded allocation; patch 393 fixed out-of-bounds wrapping; graphics width defaults to 1000 | Current xterm normally clips these target widths. Do not use patch 371 alone as the output-safety boundary. |
| [foot](https://codeberg.org/dnkl/foot/src/branch/master/sixel.c) | Accepts and clips | Active width, default maximum 10000 | Both target widths fit the default. |
| WezTerm [parser](https://github.com/wezterm/wezterm/blob/76b606ec597a3c0263fa60321548637451c0a547/wezterm-escape-parser/src/parser/sixel.rs) and [renderer](https://github.com/wezterm/wezterm/blob/76b606ec597a3c0263fa60321548637451c0a547/term/src/terminalstate/sixel.rs) | Accepts as `u32` | Commit `76b606ec`; full expansion subject to image allocation | Both target widths are accepted; allocation follows expanded pixels. |
| [mlterm](https://github.com/arakiken/mlterm) | Accepts in 3.9.3 onward | 3.9.2 and several earlier releases can overrun above 512 | DA1 cannot distinguish the fixed and unsafe versions, so a default above 512 would knowingly expose old mlterm. |
| [Contour](https://github.com/contour-terminal/contour) | Accepts and clips | Pixels beyond the canvas are discarded | Both target widths are bounded by the canvas. |
| [Windows Terminal](https://github.com/microsoft/terminal) | Accepts | Sixel support from 1.22; parser cap 65535 | Both target widths are accepted. |
| [Konsole](https://invent.kde.org/utilities/konsole) | Accepts and clips | Maximum width 16384 | Both target widths are accepted. |
| tmux [native parser](https://github.com/tmux/tmux/blob/e50545f4516bebb2efb027d077895bd8ca9727a3/image-sixel.c) and [passthrough](https://github.com/tmux/tmux/blob/e50545f4516bebb2efb027d077895bd8ca9727a3/input.c) | Native parser rejects above 10000; passthrough preserves | Commit `e50545f4`; 10000 native cap | Nyala's 1920 and 2560 maximum useful runs stay below the native cap. |
| [Zellij](https://github.com/zellij-org/zellij) | Accepts 32767 | Expands then re-encodes | Compatible, with memory cost proportional to expanded pixels. |
| [herdr](https://github.com/ogulcancelik/herdr/blob/4a3302d19a2baa3a33e910e0088c24e780905b90/vendor/libghostty-vt/src/terminal/stream_terminal.zig#L301-L306) | Discards every Sixel DCS | Current source | No repeat ceiling can make Sixel work through herdr. |
| [GNU Screen](https://sources.debian.org/src/screen/4.9.1-3/ansi.c/) | No usable Sixel path | `MAXSTR` is 768, leaving a 767-byte DCS payload; audited GNU commit `9d8b0ff3` | Frames are corrupted regardless of repeat ceiling. |
| [Chafa](https://github.com/hpjansson/chafa/blob/61a9ae47415fd9c26c22e86854217b6e66c130e2/chafa/internal/chafa-sixel-renderer.c#L201-L235) | Encoder splits at 255; no Sixel image decoder | Policy unchanged since 2019 | Evidence for conservative output only, not decoder acceptance. |
| [`mattn/go-sixel`](https://github.com/mattn/go-sixel/blob/v0.0.12/sixel.go) | Encoder splits at 255; decoder accepts 32767 | v0.0.12 | Confirms Nyala's current policy and long-count decoding, not terminal support. |
| [libsixel](https://github.com/saitoha/libsixel/blob/v1.8.7-r2/src/tosixel.c#L150-L180) | High-level encoder is unlimited; decoder accepts through 65535; low-level API and `-R` split at 255 | Maintained v1.8.7-r2 | Direct precedent for both modern and compatibility policies. |
| [brow6el](https://codeberg.org/janantos/brow6el) | Splits at 255 through the libsixel low-level default | Current source | Browser prior art retains the conservative policy. |

The audited current Sixel-capable parsers accept counts sufficient for Nyala's real
widths, sometimes by clipping to an implementation or canvas limit. This excludes
herdr and GNU Screen, which provide no usable Sixel path, and does not claim xterm
displays the full target width. Runs above 512 can crash old mlterm, and Nyala cannot
infer the mlterm version from DA1. That is a known legacy trade-off, not evidence that
255 is the Sixel grammar limit. Nyala now uses 8192: it covers both current frame
widths and single runs up to 8192 without splitting and stays below tmux's native
10000 cap.

###### Repeat-ceiling benchmark (2026-07-16)

This is an internal byte-generation measurement, not delivered terminal FPS. A
temporary harness scaled the representative committed Linux Matters frame to each
target size, encoded it as q90 JPEG, and decoded it through Nyala's `jpegn` RGBA path.
The complete arm then ran the production-shaped palette, mapping, band, and RLE path.
The isolated arm reused the resulting paletted frame. Six fresh rotated processes
used four warm-ups and 30 retained samples for each distinct arm. Hashing, payload
copies, and decoder checks stayed outside the timed regions. Median and p90 below are
the medians of the six process statistics; the range is the six process-median range.
Estimated internal FPS is `1000 / p90`.

| Size | Ceiling | Complete median / p90 | Process median range | FPS | Allocs/op | B/op | Payload | Payload vs 255 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| 1920x1080 | 255 | 24.500 / 25.138 ms | 24.300-24.795 ms | 39.78 | 1,306 | 5,100,135 | 731,023 B | baseline |
| 1920x1080 | 512 | 24.420 / 25.094 ms | 24.301-24.711 ms | 39.85 | 1,306 | 5,100,254 | 644,272 B | -11.87% |
| 1920x1080 | 1024 | 24.290 / 24.833 ms | 24.169-24.493 ms | 40.27 | 1,306 | 5,100,399 | 615,239 B | -15.84% |
| 1920x1080 | 2048 | 24.248 / 24.929 ms | 24.082-24.554 ms | 40.11 | 1,306 | 5,100,341 | 605,517 B | -17.17% |
| 2560x1600 | 255 | 38.783 / 39.538 ms | 38.414-39.039 ms | 25.29 | 1,306 | 9,212,497 | 1,274,116 B | baseline |
| 2560x1600 | 512 | 38.447 / 39.215 ms | 38.346-38.563 ms | 25.50 | 1,306 | 9,212,549 | 1,101,472 B | -13.55% |
| 2560x1600 | 1024 | 38.339 / 39.199 ms | 38.084-38.754 ms | 25.51 | 1,306 | 9,212,570 | 1,028,324 B | -19.29% |
| 2560x1600 | 2048 | 38.275 / 39.459 ms | 37.945-38.573 ms | 25.34 | 1,306 | 9,212,593 | 1,003,227 B | -21.26% |
| 2560x1600 | 4096 | 38.263 / 39.312 ms | 37.999-38.468 ms | 25.44 | 1,306 | 9,212,538 | 1,002,987 B | -21.28% |

The complete-path process ranges overlap baseline. Treat the median reductions of
0.33%, 0.86%, and 1.03% at 1920x1080, and 0.87%, 1.14%, 1.31%, and 1.34% at
2560x1600, as directional rather than proven latency gains. Allocations are unchanged.
The deterministic payload reduction is the strong result.

| Size | Ceiling | Isolated median / p90 | Process median range | FPS | Allocs/op | B/op |
|---|---:|---:|---:|---:|---:|---:|
| 1920x1080 | 255 | 11.168 / 11.218 ms | 11.044-11.216 ms | 89.15 | 0 | 0 |
| 1920x1080 | 512 | 11.017 / 11.098 ms | 10.840-11.100 ms | 90.11 | 0 | <1 |
| 1920x1080 | 1024 | 10.976 / 11.105 ms | 10.900-11.032 ms | 90.05 | 0 | <1 |
| 1920x1080 | 2048 | 10.945 / 11.065 ms | 10.718-11.022 ms | 90.38 | 0 | <1 |
| 2560x1600 | 255 | 21.339 / 21.517 ms | 21.299-21.372 ms | 46.47 | 0 | 0 |
| 2560x1600 | 512 | 21.106 / 21.210 ms | 20.827-21.223 ms | 47.15 | 0 | <1 |
| 2560x1600 | 1024 | 20.939 / 21.058 ms | 20.595-21.045 ms | 47.49 | 0 | <1 |
| 2560x1600 | 2048 | 20.954 / 21.125 ms | 20.679-21.048 ms | 47.34 | 0 | <1 |
| 2560x1600 | 4096 | 20.854 / 21.081 ms | 20.846-21.043 ms | 47.44 | 0 | <1 |

The largest logical runs were the frame widths, 1920 and 2560. At 1920x1080,
ceilings 2048, 4096, 8192, and 32767 produced byte-identical output. At 2560x1600,
4096, 8192, and 32767 were byte-identical. The distinct payload hashes were:

| Size | Ceiling or equivalence group | SHA-256 |
|---|---|---|
| 1920x1080 | 255 | `c512adfd90cd90861566ccd933d1516ae6ca0084cf96ebfb988b65d989260526` |
| 1920x1080 | 512 | `295ae8108969bc42c48866932ccb21c6bda0a3185d6941a55cbbd7b8223d5639` |
| 1920x1080 | 1024 | `2de24d2ccd9d002f85a2fb368a966be452cce11cc2ad2cff2ce006497f4e20b9` |
| 1920x1080 | 2048, 4096, 8192, 32767 | `08b716a9328bdf0ecd577536f541090795579300e32b54642cb49d4c03608eda` |
| 2560x1600 | 255 | `1396a9af384f944ae26a379f7f7fe10631fb7f11c8f47705c7cd80a0e365eb6e` |
| 2560x1600 | 512 | `6b8ea4c7b65ecd3234b678f058ef51abc2eb8bf8face0ac3262a443f3994631d` |
| 2560x1600 | 1024 | `eb723024a5b20d9225baffbb18cbb623040ab5ebe59e3b0cae8ea6d8184117c3` |
| 2560x1600 | 2048 | `ef8d232c057c9aabe3da879f5d934e96fe513bc0879d83e5cc5fd142e8a29b35` |
| 2560x1600 | 4096, 8192, 32767 | `6442cf416516a3be710fdb4b8723007987e2f5be665e97120e953111be9cd976` |

`mattn/go-sixel` decoded every distinct output to pixels identical to the 255 arm.
Charm x/ansi v0.11.7 did not. Its decoder has a token-boundary defect that can
over-consume Sixel data bytes following any DECGRI token, including a legal repeat
of 255. Charm was removed from Sixel benchmark acceptance checks; its result is not
evidence that long repeats are incompatible.

Nyala uses 8192. At the measured widths it produces the same bytes as 4096, so it
inherits the exact 17.17% and 21.28% payload reductions above. It also supports single
runs up to 8192 without splitting and remains below tmux's native 10000 per-repeat
cap. The benchmark itself changed no production code; the later focused change
implemented the selected ceiling.

###### Pre-change internal encoder profile (2026-07-16)

This is an internal byte-generation measurement, not delivered terminal FPS. A
temporary untracked harness scaled the committed Linux Matters fixture to both target
sizes. The pre-palettised arm isolates band construction, plane scanning, RLE,
framing, and the final write; it is not RLE alone. Three fresh processes supplied 15
samples per cell with `GOMAXPROCS=4` and `GOGC=100`.

| Path | 1920x1080 median / p90 | 2560x1600 median / p90 | Warm allocation |
|---|---:|---:|---:|
| Full pre-change RGBA encode | 20.109 / 20.238 ms | 30.034 / 30.166 ms | 5.15 / 9.36 MB, 1,309 allocs |
| Pre-palettised band and RLE | 7.295 / 7.364 ms | 13.941 / 14.002 ms | 0 B, 0 allocs |

In the isolated 2560x1600 CPU profile, full-width plane scanning and run detection
account for about 76% of encoder CPU, six-row scatter for about 14%, and `appendRun`
for about 7%. `appendRun` is about 3% of the full RGBA profile. The full-path
allocations belong to palette selection and indexed-image construction, not warm RLE.

###### Rightmost nonblank plane bound (2026-07-17)

One focused post-8192 retry records each present colour plane's rightmost nonblank
column during the existing six-row scatter, then scans only through that column. It
adds one retained 256-entry integer scratch slice. It does not add a left bound,
filter, packed layout, parallelism, SIMD, or temporal state.

The comparison used the representative q90 frames and exact commit `18bba495` as the
baseline. Six fresh rotated process pairs used four warm-ups and 30 retained samples
per arm with `GOMAXPROCS=4` and `GOGC=100`. Median and p90 are medians of the six
process results. Ranges are the six process-median ranges.

| Size and path | Baseline median / p90 | Bound median / p90 | Baseline range | Bound range | Median gain | Bound allocs/op and B/op |
|---|---:|---:|---:|---:|---:|---:|
| 1920x1080 complete | 24.527 / 25.410 ms | 22.356 / 23.173 ms | 23.975-24.718 ms | 22.047-23.183 ms | 8.85% | 1,306; 5,100,485 B |
| 1920x1080 isolated | 10.745 / 10.763 ms | 8.448 / 8.572 ms | 10.576-10.851 ms | 8.396-8.494 ms | 21.38% | 0; effectively 0 B |
| 2560x1600 complete | 38.556 / 39.410 ms | 33.857 / 35.079 ms | 37.801-39.278 ms | 33.360-34.245 ms | 12.19% | 1,306; 9,212,917 B |
| 2560x1600 isolated | 20.515 / 21.007 ms | 16.061 / 16.617 ms | 20.014-20.804 ms | 15.963-16.456 ms | 21.71% | 0; 0 B |

Both complete-path ranges separate from baseline. The p90 gains are 8.81% and
10.99%. The bound skips 7,053,850 of 18,195,840 plane columns (38.7663%) at
1920x1080 and 13,669,605 of 35,970,560 (38.0022%) at 2560x1600. Warm allocation
counts are unchanged; the small full-path B/op variation against 5,100,371 and
9,212,692 baseline bytes is measurement noise within the palette path. Payloads,
hashes, and maximum repeats remain exactly 605,517 bytes,
`08b716a9328bdf0ecd577536f541090795579300e32b54642cb49d4c03608eda`, and 1920;
and 1,002,987 bytes,
`6442cf416516a3be710fdb4b8723007987e2f5be665e97120e953111be9cd976`, and 2560.
Retain the bound.

###### Repeat decimal formatter rejection (2026-07-17)

One direct branch-based formatter for positive repeat counts 4 through 8192 replaced
only the hot-path `strconv.AppendInt` call. An exhaustive test matched `strconv` for
every value. The benchmark used the same q90 frames, exact commit `a943eac` as the
baseline, and six fresh rotated process pairs with four warm-ups and 30 retained
samples per arm at `GOMAXPROCS=4` and `GOGC=100`.

| Size and path | Baseline median / p90 | Formatter median / p90 | Baseline range | Formatter range | Median change |
|---|---:|---:|---:|---:|---:|
| 1920x1080 complete | 22.132 / 22.553 ms | 21.855 / 22.661 ms | 21.871-22.322 ms | 21.622-22.067 ms | 1.25% faster |
| 1920x1080 isolated | 8.404 / 8.516 ms | 8.139 / 8.242 ms | 8.364-8.462 ms | 8.014-8.224 ms | 3.16% faster |
| 2560x1600 complete | 33.517 / 34.405 ms | 33.142 / 33.782 ms | 33.110-34.039 ms | 32.580-33.300 ms | 1.12% faster |
| 2560x1600 isolated | 15.900 / 16.107 ms | 15.489 / 15.664 ms | 15.829-16.059 ms | 15.308-15.706 ms | 2.59% faster |

The formatter micro-path falls from a 7.188 ns median to 3.716 ns, 48.30%. Both
isolated ranges separate, but both complete-path ranges overlap baseline; 1080p p90
also regresses 0.48%. Allocation counts remain 1,306 complete and zero isolated.
Payload sizes, hashes, and maximum repeats remain identical at both sizes. Reject the
formatter and retain `strconv.AppendInt`; the complete-path gain does not clear run
variance. The temporary code and tests were removed.

###### PNG-fed repeat formatter recheck (2026-07-17)

The rejected branch formatter was recreated without changing any other RLE work.
PNG does not increase its scope: the formatter runs once per emitted repeat command,
not once per source byte or pixel. A lossless frame can change the number and lengths
of spatial runs, but PNG's smaller Sixel payload does not mean decimal formatting is a
larger share of the full decode and encode path.

The screen scaled the committed Linux Matters fixture to each size, PNG-encoded it,
then timed production `decodeLiveFrame` plus one reused Sixel renderer. Each arm ran
in one fresh process with five retained 700 ms benchmark samples at
`GOMAXPROCS=4` and `GOGC=100`. The p90 is nearest-rank. All single-process sample
ranges overlap, so the agreed gate stopped before six rotated process pairs.
Projected FPS is `1000 / p90`.

| Resolution | Baseline median / p90 | Formatter median / p90 | Baseline range | Formatter range | Median change | Projected FPS change |
|---|---:|---:|---:|---:|---:|---:|
| 1920x1080 | 32.770 / 33.260 ms | 32.682 / 33.192 ms | 32.370-33.260 ms | 32.117-33.192 ms | 0.27% faster | 30.07 -> 30.13 |
| 2560x1600 | 50.990 / 51.369 ms | 50.580 / 52.245 ms | 50.554-51.369 ms | 50.510-52.245 ms | 0.80% faster | 19.47 -> 19.14 |
| 2560x2880 | 74.880 / 75.690 ms | 74.807 / 75.837 ms | 73.965-75.690 ms | 74.379-75.837 ms | 0.10% faster | 13.21 -> 13.19 |

The 1600p and 2880p p90 values regress by 1.71% and 0.19%. Median B/op and
allocations remain equivalent within benchmark noise: 14.09 MB/1,877,
26.64-26.76 MB/1,877-1,879, and 46.86-46.97 MB/1,865-1,867. The formatter itself
allocates nothing.

Baseline and candidate output match exactly. The three payloads contain 76,683,
117,788, and 175,808 repeat commands and are 494,251, 754,913, and 1,129,741 bytes.
Their SHA-256 values are respectively
`68492d77752b28d9a341c3a7be335c65604befd9721a68bc5cb859baf9c6b13e`,
`731092516a369c4b694c457aed329d847d2e7d8305c4f349a84041ad11accced`, and
`7941da46af53bb877ca2bc104b90ab327cea8f7eb5fd411d22e06bc1dce7b241`.
An exhaustive check also matched `strconv` for every count from 4 through 8192.

The existing moving-frame comparison remains the format-policy evidence. PNG beats
q90 JPEG service p90 at all three sizes there, but its serial synthetic p90 loses by
1.26% at 2560x2880. This formatter does not move that decision: its full-path gain is
inside run variation and two p90 values regress. Retain `strconv.AppendInt`; the
separate PNG-only product decision governs Sixel input. Keep all temporary code removed.

###### Bounded indexed-pixel mapping rejection (2026-07-17)

A temporary candidate extracted the inherited `min(4, GOMAXPROCS)` Sixel budget
and used it to split adaptive RGB555 mapping into disjoint row ranges at 500,000
pixels. The exact-colour scan, palette selection, LUT construction, mapping, and
band emission remained sequential stages. Boundary trials from 500,000 through
1,000,000 pixels supported the lowest cut-off. At 500,000 pixels, isolated mapping
fell from 0.530-0.540 ms serial to 0.155-0.186 ms with four workers.

The initial screen used six fresh rotated processes and 30 retained iterations per
arm on the 1920x1080 Linux Matters photo with `GOMAXPROCS=4` and `GOGC=100`.

| Path | Serial process aggregate | Four-worker process aggregate | Worst-process change | Allocation change |
|---|---:|---:|---:|---:|
| Isolated mapping | 2.100 / 2.109 ms | 0.648 / 0.754 ms | 64.27% faster | 0 to 5-6 allocs/op |
| Full encode | 19.918 / 20.278 ms | 18.546 / 19.434 ms | 4.16%, 0.843 ms faster | 1,309-1,310 to 1,314-1,315 allocs/op |
| PNG decode plus encode | 33.321 / 34.108 ms | 31.896 / 32.890 ms | 3.57%, 1.218 ms faster | 2,529 to 2,533-2,535 allocs/op |

Focused tests matched serial and parallel palette indexes, repeated output,
payload SHA-256, and decoded pixels across unusual bounds, non-zero origins,
strides, threshold boundaries, and worker counts 1 and 4. The race test passed.
The worst full-encode process aggregate misses both required gates, at least 5%
and at least 2 ms. Reject this cap.

The user-authorised six-worker retry changed only the shared Sixel cap and ran the
same serial and row-split mapper at `GOMAXPROCS=6`. Six fresh rotated process pairs
used four warm-ups and 30 retained samples per arm. The table reports the median
of the six process medians and p90 values.

| Path | Serial median / p90 | Six-worker median / p90 | P90 change | Allocation change |
|---|---:|---:|---:|---:|
| Isolated mapping | 2.110 / 2.148 ms | 0.376 / 0.470 ms | 78.12% faster | 0 to 7 allocs/op |
| Full encode | 20.281 / 20.795 ms | 18.656 / 19.573 ms | 5.88%, 1.222 ms faster | 1,306 to 1,313 allocs/op |
| PNG decode plus encode | 36.138 / 36.998 ms | 34.299 / 35.331 ms | 4.51%, 1.667 ms faster | 2,726 to 2,733 allocs/op |

Six workers pass the 5% full-encode p90 gate but miss the required 2 ms saving.
The complete PNG path does not regress. Reject the retry and remove its temporary
code and harness. Live Chrome delivery, first-contentful-paint, and contention
checks stop here because the full-encode gate failed. Keep the production mapper
serial and leave the inherited four-worker LUT policy unchanged.

The supplied optimisation advice maps onto the current encoder as follows:

| Advice | Current assessment |
|---|---|
| Packed six-pixel columns | Already present as one six-bit byte per colour and x-coordinate; the earlier packed-bank rewrite was slower. |
| Colour-presence filters | Already present through generation counters; only colours seen in a band are scanned. Extra filters duplicate this work. |
| Fused generation and RLE | Run detection and emission are fused in the plane scan. Pixel scatter and plane scan remain separate; merging them needs a broad layout rewrite and is closed. |
| Specialised emission | Threshold-4 literals, retained output, direct appends, and one final write already exist. A direct decimal formatter improved its micro-path but failed the complete-path range gate, so retain `strconv.AppendInt`. |
| Trailing blank omission | Trailing zero runs are not emitted. The retained rightmost bound now skips their scan columns with byte-identical output. |
| Branch reduction | The run-change branch is visible inside the dominant scan, but no separate exact candidate avoids the surrounding work. |
| Buffer ownership and pooling | The renderer owns and reuses all RLE scratch; the isolated warm path is zero-allocation. |
| Band parallelism | Prior production-shaped variants did not win; keep it closed. |
| SIMD | Reject for this compatibility path unless a future profile and portable upstream implementation change the case. |

Generic RLE libraries do not model Sixel delimiters and repeat syntax. Transparency
has already been removed from Nyala's opaque path. Temporal or motion state, another
packed-bank design, a broad fusion rewrite, extra pooling, and a huge benchmark
matrix are not required unless a future profile changes the decision. Chafa is
LGPLv3+ prior art only; no Chafa code is copied.

The temporary exact-payload harness processed 36.5 million band bytes across 22,003
planes. These are stage-only results and do not measure a complete candidate path.

| RLE arm | Stage result | Payload result |
|---|---:|---:|
| Current 255 split | 24.662 ms | 2,244,229 B |
| Legal 32767 split, direct digits | 23.080 ms, 6.4% faster | 1,819,461 B, 18.9% smaller |
| 255 split, decimal table | 5.1% faster | Byte identical to current |

The 1.582 ms stage saving is an arithmetic ceiling of about 1.55% against the local
102.227 ms full baseline, not a measured full-path gain. The complete benchmark above
supersedes that estimate. The focused production change raises only the repeat ceiling
from 255 to 8192. Tests cover repeat transitions, multi-chunk counts, mandatory data
bytes, delimiters, odd heights, decoded pixels, and warm allocations. `mattn/go-sixel`
is the decoded-pixel oracle. Charm x/ansi v0.11.7 is not an RLE correctness oracle
because its token-boundary defect also affects 255 repeats.

The encoder can emit a harmless redundant `$` immediately after an inter-band `-`.
Removing it is a separate optional byte-level micro-experiment, not a performance
priority and not part of the steps above.

##### Band track

The current builder uses one-pass scatter, row masks, generation counters, sparse
clearing, reused buffers, and the retained rightmost nonblank bound. The earlier
production-shaped full encoder with
Plan9-palettised input measured 17.24 ms, zero allocations, and a 1,009,015-byte
payload. Active-bitset and transposed variants were slower. Packed banks cut scratch
96.7% but made band plus RLE 31% slower than the instrumented baseline. Earlier broad
bounded-plane variants looked faster only in an inflated dual-mode harness. The
single post-8192 rightmost bound above clears the production-shaped retention gate.

Every band variant produced identical bytes and decoded through both decoders across
canonical, solid, sparse, dense, noisy, transparency, subimage and stride, width
override, and odd-height inputs. Close this track with no code or fork. Parallel
bands were deferred at this stage; the user-requested candidate below later
superseded this decision.

##### User-requested parallel-band candidate (2026-07-17)

The user overrode the later plan's 25% reprofile blocker and automatic revert rule.
A new candidate therefore superseded the closed implementation decision for review.
It shares the inherited
`min(4, runtime.GOMAXPROCS(0))` LUT budget, activates at 120 bands and 39,000,000
present-colour-width-band units, splits complete bands into contiguous batches,
retains isolated worker scratch and output, joins in order, and emits any partial
last band serially.

Six fresh rotated processes used `GOMAXPROCS=4`, `GOGC=100`, four setup warm-ups,
and 30 retained samples per arm. This bounded screen used synthetic paletted
fixtures, so it does not replace the required adaptive-photo and production PNG
matrix.

| Fixture and path | Serial median / p90 | Candidate median / p90 | P90 change | Allocation change |
|---|---:|---:|---:|---:|
| 1920x1080 photo emission | 62.037 / 62.779 ms | 18.276 / 18.919 ms | 69.86%, 43.860 ms faster | 0 to 5 allocs/op, about 537 B/op |
| 1920x1080 complete paletted encode | 61.942 / 62.747 ms | 18.464 / 19.836 ms | 68.39%, 42.911 ms faster | 2 to 7 allocs/op |
| 1920x1080 PNG decode plus paletted encode | 63.577 / 64.387 ms | 19.952 / 20.872 ms | 67.58%, 43.515 ms faster | 298 to 303 allocs/op |
| 1280x720 flat complete | 1.892 / 1.848 ms | 1.878 / 1.887 ms | 2.11% slower | unchanged at 2 allocs/op |
| 1280x720 web complete | 4.687 / 5.196 ms | 4.155 / 4.754 ms | 8.51% faster | unchanged at 2 allocs/op |

The photo CPU profiles sampled 2.31 CPU seconds over 0.653 seconds, 353.7% CPU,
against 2.07 CPU seconds over 2.07 seconds for serial. Exact bytes, SHA-256 values,
and decoded pixels match across the focused correctness matrix. Complete payloads
match at 9,893,259 bytes for photo, 615,787 bytes for flat, and 1,930,251 bytes for
web. The flat p90 and warm allocation gates failed. This first screen did not accept
the candidate. The broader production-shaped sweep below supplied the missing
adaptive 1920x1080, 2560x1600, 2560x2880, PNG-decode-plus-encode, and interop
evidence.

##### Combined mapping plus band candidate (2026-07-17)

The user requested the four-worker RGB555 mapper on top of the committed band
candidate and waived automatic reversion. The implementation reuses
`sixelWorkerCount()` unchanged. The exact-colour scan remains serial. Only an
adaptive image at or above 500,000 pixels splits mapping into disjoint row ranges.
LUT construction waits before mapping, mapping waits before emission, and the
parallel stages never overlap.

Six fresh paired processes rotated arm and fixture order with `GOMAXPROCS=4`,
`GOGC=100`, four warm-ups, and 30 retained samples. The photo is the scaled
1920x1080 Linux Matters fixture and takes the adaptive path. The 1280x720 flat and
web fixtures take the exact-colour path and screen small-frame overhead. Values
below are medians of the six process p90 values.

| Fixture and path | Band-only | Combined | Change |
|---|---:|---:|---:|
| 1920x1080 photo isolated mapping | 2.107 ms | 0.711 ms | 66.25%, 1.396 ms faster |
| 1920x1080 photo full encode | 17.382 ms | 15.592 ms | 10.30%, 1.790 ms faster |
| 1920x1080 photo PNG decode plus encode | 31.472 ms | 29.457 ms | 6.40%, 2.015 ms faster |
| 1280x720 flat full encode | 6.000 ms | 5.969 ms | 0.51% faster |
| 1280x720 flat PNG decode plus encode | 8.601 ms | 8.568 ms | 0.38% faster |
| 1280x720 web full encode | 6.976 ms | 6.893 ms | 1.19% faster |
| 1280x720 web PNG decode plus encode | 9.946 ms | 9.974 ms | 0.28% slower |

The earlier pre-band four-worker saving of 0.843 ms persists and measures 1.790 ms
in this combined screen. The result passes the former 5% percentage gate but saves
less than the former 2 ms full-encode gate. Isolated mapping moves from zero
allocations to five and about 336 B/op. Full photo encode remains at 1,306
allocations; median B/op differs by about 0.2 KiB, within measurement noise. CPU
profiles measured 99.9% for serial mapping and 352.5% for the combined mapper.

All three fixture payload sizes and SHA-256 values match between arms: 411,048 B
and `74b1b0400bf1e69f40df4f65007463570078556e8afc3f002b1106c662e53e48`
for photo, 1,244 B and
`ec43ec817ff15e79396b33f47e9d5e426e2233809c8864de927c41201b30224c` for
flat, and 49,187 B and
`adda562534e381a5bd38efa908ed389ba0361639419ab40a670950c8a77a154a` for web.
Focused tests preserve indexes, deterministic bytes, decoded pixels, non-zero
bounds, unusual stride, threshold edges, and worker counts 1 and 4. The following
production-shaped sweep accepted the combined path.

##### Capture-removal and combined Sixel sweep (2026-07-18)

Six fresh processes rotated arm order with four warm-ups and 30 retained samples per
arm at `GOMAXPROCS=4` and `GOGC=100`. Values are medians of process p90s on a Ryzen AI
MAX+ 395 with Go 1.26.5 and Chromium 150.0.7871.114. Block and Kitty compare current
`8cdf973a` with the capture baseline `fae43acc`. Their starred measurements use the
production `State` seam with alternating committed-photo frames. Sixel compares the
current retained serial arm with the combined mapper and band path, using controlled
PNG decode and the production encoder. Commit `fae43acc` already contained the
parallel Sixel work, so it is not the Sixel CPU baseline.

| Backend and path | Size | Baseline or serial p90 | Current or combined p90 | Change |
|---|---:|---:|---:|---:|
| Block State* | 1920x1080 | 1.1025 ms | 1.1015 ms | -0.09% |
| Block State* | 2560x1600 | 2.0045 ms | 1.9525 ms | -2.59% |
| Block State*, production-shaped fallback | 2560x2880 | 3.7560 ms | 3.7230 ms | -0.88% |
| Kitty State* | 1920x1080 | 0.0080 ms | 0.0080 ms | unchanged |
| Kitty State* | 2560x1600 | 0.0130 ms | 0.0130 ms | unchanged |
| Kitty State*, production-shaped fallback | 2560x2880 | 0.0730 ms | 0.0730 ms | unchanged |
| Sixel full encode | 1920x1080 | 20.2710 ms | 14.5515 ms | -28.22% |
| Sixel full encode | 2560x1600 | 29.8815 ms | 19.0115 ms | -36.38% |
| Sixel full encode | 2560x2880 | 42.9870 ms | 21.6535 ms | -49.63% |
| Sixel PNG decode plus encode | 1920x1080 | 33.8495 ms | 27.7895 ms | -17.90% |
| Sixel PNG decode plus encode | 2560x1600 | 54.7920 ms | 43.3995 ms | -20.79% |
| Sixel PNG decode plus encode | 2560x2880 | 85.8895 ms | 64.2290 ms | -25.22% |

The Block process ranges overlap, so the sweep establishes no regression but does
not claim an improvement. Kitty keeps identical payload and allocation classes at
10 allocations; Block remains at zero allocations. Capture removal therefore did
not regress either production seam.

| Sixel stage | 1920x1080 serial to combined | 2560x1600 serial to combined | 2560x2880 serial to combined |
|---|---:|---:|---:|
| Indexed-pixel mapping | 2.3740 to 0.8830 ms | 4.4140 to 1.6805 ms | 7.9710 to 2.4965 ms |
| Band emission | 7.2775 to 2.9230 ms | 13.7990 to 5.6890 ms | 23.6100 to 8.0745 ms |

Sixel allocations move from 1,045 to 1,047. B/op rises by 589, 608, and 603 bytes,
under 0.02% at each size. Serial and combined payloads are identical at 410,869,
658,457, and 1,135,208 bytes, and decode to identical pixels. Omitting the redundant
inter-band separator saves 179, 266, and 479 bytes. `just sixel-interop` passed, so
the combined mapper and bounded band emitter are accepted. Live CDP at 2560x2880
timed out while awaiting a settled PNG for Kitty and Sixel; this does not alter the
controlled CPU or protocol conclusions. Raw sweep logs remain local, ephemeral
diagnostic output under `testdata/diagnostics/performance-20260718`.

##### Cross-track decision

Keep adaptive 256 with the stateless 64K sample cap and the 8192 repeat ceiling.
Retain the accepted combined mapping and band path. Send only accepted wins upstream.
If short-lived integration needs a fork, publish a versioned fork and use a temporary
module replacement. Do not vendor, use `go:linkname`, or add a Nix-only patch.

### Sixel palette reuse (2026-07-22)

Sixel now reuses the adaptive palette across frames through delta-triggered
reuse with hysteresis (`internal/render/sixel/palette_reuse.go`). This revisits
the "stateless" wording in the cross-track decision above: the temporal-state
deferral named a future profile as its trigger, and palette shimmer on motion
content was that trigger. The 64K sample cap, the median-cut quantiser, the
no-dither mapping, and the 8192 repeat ceiling are unchanged. The palette
history above still stands: benchmark A measured warm palette reuse as 45.8%
faster with byte-identical static output, benchmark B rejected fixed palettes
on quality, and the sample-cap matrix fixed the 64K budget.

The encoder retains the palette and the RGB555 LUT between frames. Every frame
still builds the 64K palette sample (the `1<<16` budget is untouched).
`reusePalette` maps that sample through the retained LUT and measures the mean
squared RGB error, summed over the three channels. The frame reuses the
retained palette while that error stays within `max(1.15 x baseline, 12.0)`,
where the baseline is the mapping error recorded at the last re-quantisation,
1.15 is the relative trigger, and 12.0 is the absolute floor. Above the
threshold, `requantisePalette` rebuilds the palette and
`freezeSubJNDRegisters` keeps an old register value wherever the new entry's
nearest old entry sits within CIEDE2000 2.3. Nearest matching uses squared RGB
distance; dE runs only for the 255 final judgements. Geometry changes and
exact-path interludes invalidate the retained state, so the next adaptive
frame re-quantises from scratch without hysteresis. Registers are still
emitted every frame. No dithering was introduced. Determinism is preserved:
the same input sequence produces the same output sequence.

`TestPaletteShimmerBenchmark`, opt-in through `just palette-bench`, is the
shimmer measurement. It encodes a deterministic 20-frame motion sequence at
1280x720, with static page chrome over the top 60% and a photo-like field
translating 3 px per frame across the bottom 40%, through one production
renderer session. It decodes every payload and scores decoded pixel pairs
whose source pixels are byte-identical between consecutive frames.

| Arm | Unchanged-pixel dE mean | Unchanged-pixel dE max, pair mean | Registers changed, mean of 255 |
|---|---:|---:|---:|
| Per-frame re-quantisation | 0.056360 | 15.99 | 202.7 |
| Palette reuse | 0.000000 in all 19 pairs | 0.000000 | 0 |

Unit tests pin the surrounding behaviour: static sequences quantise once,
sub-threshold motion reuses, a scene change (full inversion) re-quantises
immediately, geometry changes and exact-path interludes invalidate, hysteresis
freezes sub-JND registers, and repeated sequences stay byte-identical.

Benchmark interpretation note: the existing palette-bench arms re-encode one
fixture through one encoder, so from iteration 2 onward they now measure the
reuse path, not per-frame re-quantisation. Read their per-frame means with
that in mind.

The 32K sample-cap question is answered: no. Shimmer comes from per-frame
global re-quantisation, not sample size; a halved sample is a noisier
estimate, so palette jitter rises by about sqrt(2). The expected speed gain
was about 2-3 ms because selection is no longer the dominant cost at 64K.

Live verification (2026-07-22, mlterm, noted by the operator as the fastest
Sixel renderer, aquarium WebGL page):

```
warmup_ms=3135 in_frames=1868 in_mean=25.3 in_min=9.2 in_max=213.4 in_median=31.1 in_worst_gap_ms=108.8 in_decode_mean_ms=66.7 in_decode_max_ms=113.8 out_frames=1814 wall=73.77s out_mean=24.6 out_median=25.0 out_min=7.6 out_max=115.8 out_worst_gap_ms=131.5 out_encode_mean_ms=17.0 out_encode_max_ms=42.0 out_write_mean_ms=37.1 out_write_max_ms=64.7 ack_rtt_mean_ms=12.5 ack_rtt_max_ms=67.5
```

Load timings: process spawned +185.3 ms, screencast started +3096.4 ms,
first frame emitted +3123.0 ms.

No shimmer observed. Output tracks ingest (`out_mean` 24.6 against `in_mean`
25.3), so the Sixel encoder is no longer the bound. The remaining costs are
the per-frame PNG decode (66.7 ms mean) and mlterm's in-band consumption
(`out_write_mean_ms` 37.1, a ~27 fps ceiling on this content). Encode at
17.0 ms mean is mapping and band emission, with palette selection skipped
under reuse.

Live verification (2026-07-22, same mlterm, Blender Charge video):

```
warmup_ms=869 in_frames=6478 in_mean=24.4 in_min=1.8 in_max=171.9 in_median=27.3 in_worst_gap_ms=570.9 in_decode_mean_ms=18.3 in_decode_max_ms=40.9 out_frames=6477 wall=265.20s out_mean=24.4 out_median=26.4 out_min=1.7 out_max=166.7 out_worst_gap_ms=571.7 out_encode_mean_ms=9.2 out_encode_max_ms=32.4 out_write_mean_ms=2.0 out_write_max_ms=20.6 ack_rtt_mean_ms=0.3 ack_rtt_max_ms=3.2
```

Load timings: process spawned +183.3 ms, screencast started +832.9 ms,
first frame emitted +861.3 ms.

No shimmer observed; the operator called the result impressive. 6477 of
6478 frames painted at the video's native ~24 fps over 265 s. The 571 ms
worst gaps are damage-driven idle during still scenes, not stalls.

Open items:

- Closed 2026-07-22: the Sixel path now captures JPEG q90. Sixel decodes every
  frame, so no pass-through constraint applies, and the `jpegn` decode cuts the
  66.7 ms PNG decode towards ~20 ms while relieving Chromium encode; see the
  Kitty shm JPEG q90 capture decision above for the payload and decode
  analysis. JPEG noise means the encoder's exact-colour fast path fires less
  often and flat pages take the adaptive path; this is accepted. Live
  verification is recorded below.
- Omitting palette-register emission on reused frames would cut payload bytes,
  but register redefinition behaviour varies across emulators. Deferred as
  future work.

Live verification of the JPEG q90 capture (2026-07-22, mlterm, aquarium WebGL
page, 2560x1600 on the office machine; the PNG baseline above ran at 2560x1440
at home, so this run pushes 11% more pixels, a confounder in any direct
comparison):

```
warmup_ms=2129 in_frames=4559 in_mean=60.0 in_min=43.5 in_max=233.2 in_median=60.1 in_worst_gap_ms=23.0 in_decode_mean_ms=13.5 in_decode_max_ms=19.6 out_frames=1742 wall=75.86s out_mean=22.9 out_median=21.3 out_min=13.3 out_max=62.6 out_worst_gap_ms=75.2 out_encode_mean_ms=28.3 out_encode_max_ms=60.9 out_write_mean_ms=42.2 out_write_max_ms=70.5 ack_rtt_mean_ms=0.3 ack_rtt_max_ms=3.1
```

Load timings: process spawned +234.1 ms, screencast started +1285.2 ms,
first frame emitted +2047.6 ms.

The ingest wall is gone. Ingest runs at 60.0 fps, the CDP cap, against the PNG
baseline's 25.3, and decode falls from 66.7 ms to 13.5 ms mean. Output is now
terminal-bound: the 28.3 ms encode overlaps the 42.2 ms write, so mlterm's
in-band payload consumption sets a ~24 fps ceiling for full-screen motion at
this size. This is Sixel's equivalent of the Kitty parser wall, and the
protocol has no shm escape hatch. 1742 of 4559 frames painted is the
freshest-frame pacing working as designed. Payload grew against the PNG run
partly because JPEG noise compresses worse under RLE and partly because of the
resolution change.

Two further live runs (2026-07-22) repeat the same setup, the aquarium WebGL
page at 2560x1600 with JPEG q90 capture on the same machine, on Alacritty and
Contour.

Alacritty:

```
warmup_ms=1080 in_frames=4342 in_mean=60.0 in_min=29.9 in_max=287.6 in_median=59.8 in_worst_gap_ms=33.5 in_decode_mean_ms=13.7 in_decode_max_ms=19.2 out_frames=1266 wall=72.26s out_mean=17.5 out_median=16.7 out_min=11.2 out_max=69.2 out_worst_gap_ms=89.5 out_encode_mean_ms=29.4 out_encode_max_ms=63.7 out_write_mean_ms=55.6 out_write_max_ms=74.4 ack_rtt_mean_ms=0.3 ack_rtt_max_ms=6.5
```

Load timings: process spawned +213.9 ms, screencast started +624.7 ms,
first frame emitted +1012.1 ms.

Contour:

```
warmup_ms=1218 in_frames=2814 in_mean=60.0 in_min=42.6 in_max=113.7 in_worst_gap_ms=23.5 in_median=60.0 in_decode_mean_ms=13.0 in_decode_max_ms=19.5 out_frames=187 wall=46.34s out_mean=4.0 out_median=3.6 out_min=2.9 out_max=84.9 out_worst_gap_ms=340.8 out_encode_mean_ms=30.3 out_encode_max_ms=57.4 out_write_mean_ms=250.3 out_write_max_ms=319.7 ack_rtt_mean_ms=0.2 ack_rtt_max_ms=5.3
```

Load timings: process spawned +221.7 ms, screencast started +1163.2 ms,
first frame emitted +1194.9 ms.

Two more live runs (2026-07-22) repeat the identical setup on foot and
WezTerm.

foot:

```
warmup_ms=879 in_frames=7663 in_mean=60.0 in_min=40.3 in_max=142.3 in_median=60.0 in_worst_gap_ms=24.8 in_decode_mean_ms=13.6 in_decode_max_ms=19.9 out_frames=1030 wall=127.44s out_mean=8.1 out_median=7.9 out_min=6.2 out_max=47.6 out_worst_gap_ms=161.8 out_encode_mean_ms=30.0 out_encode_max_ms=60.2 out_write_mean_ms=123.9 out_write_max_ms=146.3 ack_rtt_mean_ms=0.2 ack_rtt_max_ms=6.9
```

Load timings: process spawned +186.3 ms, screencast started +581.4 ms,
first frame emitted +811.7 ms.

WezTerm:

```
warmup_ms=1421 in_frames=24383 in_mean=38.3 in_min=13.4 in_max=691.2 in_median=47.2 in_worst_gap_ms=74.7 in_decode_mean_ms=29.2 in_decode_max_ms=43.8 out_frames=1150 wall=636.20s out_mean=1.8 out_median=1.8 out_min=1.5 out_max=24.2 out_worst_gap_ms=646.3 out_encode_mean_ms=70.3 out_encode_max_ms=103.5 out_write_mean_ms=553.7 out_write_max_ms=627.6 ack_rtt_mean_ms=1.7 ack_rtt_max_ms=27.4
```

Load timings: process spawned +216.3 ms, screencast started +1298.8 ms,
first frame emitted +1370.3 ms.

On mlterm, Alacritty, Contour, and foot the Nyala side is constant: ingest
holds 60.0 fps at the CDP cap, decode stays at 13.0-13.7 ms mean, and encode
stays at 28.3-30.3 ms mean. For those four, the output spread is entirely the
emulator's Sixel consumption:

| Emulator | `out_write_mean_ms` | Output fps |
|---|---:|---:|
| mlterm | 42.2 | 22.9 |
| Alacritty | 55.6 | 17.5 |
| foot | 123.9 | 8.1 |
| Contour | 250.3 | 4.0 |
| WezTerm | 553.7 | 1.8 |

WezTerm is the outlier twice over. It is the slowest consumer at 553.7 ms per
frame, thirteen times mlterm. Its Sixel parsing and rasterisation also contend
with Nyala for CPU, inflating decode to 29.2 ms and encode to 70.3 ms and
dropping ingest to 38.3 fps. It is the only terminal tested whose consumption
degrades the Nyala side.

Contour consumes the same payload six times slower than mlterm. This is the
same shape as the Kitty in-band parser wall, and Sixel has no shm escape
hatch, so the emulator sets the ceiling. Freshest-frame pacing degrades
gracefully: Contour paints the newest frame at 4 fps, and WezTerm at 1.8 fps,
rather than falling behind. The operator's ranking is confirmed: mlterm is the
fastest Sixel renderer tested.

Remaining Sixel levers, recorded as ideas, not commitments: omit register
emission on reused frames (the deferred item above), and emit only dirty bands
for partial-motion content.

### Option assessment

| Option | Assessment | Decision |
|---|---|---|
| Current fused Block compositor | Exact weighted region average across six ratios, one conversion per terminal sub-cell, existing persistent workers | Keep |
| Ratio-specialised Block plane sums | The retained 4:2:0 path removes common mapping and loop work without a full RGBA surface; generic fallback preserves every other case | Retained after exactness, paired, CPU, throughput, and tail gates |
| `jpegn` v0.4.4 `Options.ToRGBA` | MIT, cgo-free packed RGBA; historical matched sequential Sixel processing fell 10.43% with stable quality and a 1.1% retained-bound increase | Superseded; Sixel is PNG-only and the JPEG-specific branch is removed |
| Parallel inherited 15-bit Sixel LUT | Ten-cell full encode falls 5.54% and combined q90 decode plus encode falls 6.21%; all payloads are byte-identical | Retained at up to four workers |
| 64K median-cut sample cap | Combined q90 decode plus encode falls 30.39% against prior production; colour-category shifts are accepted because all 800 phase-swept features remain visible and structurally intact | Retained |
| `mattn/go-sixel` fixed 255 palette | Warm State falls 45.8% with byte-identical static output; per-frame rebuild removes the gain | Superseded by the stateless 64K sample-cap decision |
| Long-count Sixel RLE | Complete six-process benchmark cuts payload 17.17% at 1920x1080 and 21.28% at 2560x1600; timing ranges overlap; 8192 is byte-identical to 4096 at both widths; `mattn/go-sixel` pixels are identical | Retained at 8192, below tmux's native 10000 cap |
| Rightmost Sixel plane bound | Skips 38.77% and 38.00% of plane columns; complete median falls 8.85% and 12.19% with separated ranges, identical bytes, and unchanged allocation counts | Retained |
| Bounded parallel Sixel bands and RGB555 mapping | Full-encode p90 falls 28.22% to 49.63% and PNG decode plus encode falls 17.90% to 25.22%; bytes and decoded pixels are identical | Retained at up to four workers |
| Other private Sixel band changes | Active-bitset, transposed, and packed-bank variants do not beat production | Closed |
| Current decoder plus row-parallel `draw.Draw` | Uses the existing viewer pool, preserves current pixels, and measured 0 B/op and 0 allocs/op warm | Closed; Sixel's production input is PNG |
| Standard library `imageutil.DrawYCbCr` | Internal, full-frame, and specialised for four ratios | Use only through the public `draw.Draw` boundary |
| Go 1.26 `archsimd` | Experimental, `GOEXPERIMENT`-gated, amd64-only | Reject for production |
| `go-cv-simd` | Stale, BGRA-oriented, and supports fewer input formats | Reject |
| Jive row workers | Persistent disjoint-row scalar conversion | Prior art only; Block already uses the pattern and the Sixel experiment measured it |

Nyala still pins `jpegn` v0.4.4 for Block's live JPEG decode. Sixel's PNG input has no
JPEG activation boundary or worker cutoff. The next Sixel work follows the
cross-track order above.

### Primary sources

- `mattn/go-sixel` v0.0.12 encoder, fixed-palette cache, paletted fast path, and
  private pipeline:
  https://github.com/mattn/go-sixel/blob/v0.0.12/sixel.go
- DEC STD 070 Sixel grammar and repeat limit:
  https://archive.org/details/bitsavers_decstandar0VideoSystemsReferenceManualDec91_74264381
- VT330 and VT340 Sixel grammar:
  https://vt100.net/docs/vt3xx-gp/chapter14.html
- Independent pure-Go picture-body decoder:
  https://github.com/charmbracelet/x/blob/ansi/v0.11.7/ansi/sixel/decoder.go
- `benchstat` sample and comparison guidance:
  https://pkg.go.dev/golang.org/x/perf/cmd/benchstat
- `mattn/go-sixel` v0.0.12 package record:
  https://pkg.go.dev/github.com/mattn/go-sixel@v0.0.12
- Upstream June and July encoder performance work:
  https://github.com/mattn/go-sixel/pull/23 and
  https://github.com/mattn/go-sixel/commit/44d338f95d08e64b3c61598c3c6b309d954f8130
- `mattn/go-sixel` uses the MIT licence:
  https://github.com/mattn/go-sixel/blob/v0.0.12/LICENSE
- `jpegn` v0.4.4 uses the MIT licence:
  https://github.com/gen2brain/jpegn/blob/v0.4.4/LICENSE
- `jpegn` exposes the decode options and packed RGBA result:
  https://github.com/gen2brain/jpegn/blob/v0.4.4/jpegn.go
- `jpegn` implements its decoder and colour-conversion paths here:
  https://github.com/gen2brain/jpegn/blob/v0.4.4/decoder.go
- The v0.4.4 tree contains the amd64, arm64, and scalar implementations:
  https://github.com/gen2brain/jpegn/tree/v0.4.4
- `jpegn` equivalence tests allow a per-channel difference of 2:
  https://github.com/gen2brain/jpegn/blob/v0.4.4/jpegn_test.go
- `x/ansi/sixel` builds its palette and colour map in this path:
  https://github.com/charmbracelet/x/blob/ansi/v0.11.7/ansi/sixel/palette.go
- Go 1.26.4 keeps `DrawYCbCr` internal and specialises four ratios:
  https://github.com/golang/go/blob/go1.26.4/src/image/internal/imageutil/impl.go
- Go 1.26 `archsimd` is an amd64-only experiment behind `GOEXPERIMENT`:
  https://github.com/golang/go/blob/go1.26.4/src/simd/archsimd/doc.go
- Jive converts persistent disjoint row bands in scalar Go:
  https://github.com/linuxmatters/jive-visualiser/blob/c939ee9e443c588b01a2a9491b1c1b9a99325ae9/internal/encoder/frame.go
- `go-cv-simd` provides the stale BGRA-oriented comparison:
  https://github.com/fwessels/go-cv-simd
- Go documents a published fork plus versioned `replace` and vendoring here:
  https://go.dev/doc/modules/managing-dependencies#external_fork and
  https://go.dev/ref/mod#vendoring
- `libsixel` and `rasterm` provide external band-generation comparisons:
  https://github.com/libsixel/libsixel and
  https://github.com/BourgeoisBear/rasterm/blob/v1.1.2/sixel.go

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

- RESOLVED: screencast format is backend-specific. Block uses JPEG quality 90.
  Kitty, Sixel, and capture always use PNG. Theme 7 and the PNG-only decision record
  the policy and measurements.
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
native `image.YCbCr`, which Block composes directly into the final grid without an
intermediate RGBA surface. Real-arm64 performance validation is not required because
the upstream projects publish their SIMD implementation details, as tracked in
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
record the pre-adoption test. Real-arm64 performance validation is not required
because the upstream projects publish their SIMD implementation details, as recorded in
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
