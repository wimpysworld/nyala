# The Evolution of Nyala's Renderers

Nyala is a terminal web browser. It drives headless Chromium over CDP and
paints pages through three backends: Block (Unicode cells), Sixel, and Kitty
graphics. Two projects shaped it: Browsh proved that browser text in a
terminal must be drawn as crisp glyphs, not pixels, and Carbonyl proved the
cell model that maps page pixels onto terminal cells. This document records
the milestones in evolution order, without dates: what changed, why, and
the measured impact, or the plain outcome where no measurement exists.

## Foundations

**The frame seam came before the browser.** The `render.Frame` seam, all
three backends, and a browser-free diagnostics harness with OCR and PSNR
gates existed before any Chromium code. The point was to prove the
terminal-rendering half in isolation, so every later engine change
validates against a fixed seam that is still the correctness gate today.

**A screencast loop with latest-wins backpressure.** `ChromiumEngine`
launches headless Chromium, streams `Page.startScreencast`, decodes DOM
snapshots into glyph runs, and syncs text to bitmap by scroll offset.
Latest-wins beat queuing: slow terminals drop frames, not build latency.

**The ack pump, or how chromedp deadlocks itself.** Chrome froze after
exactly one frame. An inline screencast ack is the chromedp #920
self-deadlock: the ack waits for a response that only the parked event
goroutine can read. Acks moved onto a queue drained by a dedicated `ackPump`
goroutine, later hardened to an unbounded FIFO so slow CDP commands cannot
drop or reorder acks.

**Text arrives at first paint.** The background painted in about 272 ms
while text waited 2931 ms, a 2.7 second lag, because the text snapshot
armed on `networkAlmostIdle` plus a 250 ms debounce; it now arms on
`firstContentfulPaint`. Walking the DOM per word instead of per code unit
cut snapshot time on BBC from 228 to 31 ms, Wikipedia from 451 to 259 ms.
Text is placed by the snapshot's own scroll offset, held at most 250 ms on
failure, then dropped rather than shown wrong.

**The measurement discipline.** `NYALA_FPS_TIMING` arrived early and splits
In (ingest, stamped before any drop) from Out (paint), plus `in_decode`,
`out_encode`, `out_write`, and `ack_rtt`, so every wall is attributable:
browser, decode, our encode, or the terminal. It later caught WezTerm
byte-bound at ~20 ms `out_write`, fixed by `--color=256` with no code
change. This instrument supplies most numbers from here on.

**Acks decoupled from decode.** Inline PNG decode stalled acks, so
Chromium's frame credit waited on Nyala's decode. The receiver now acks
immediately and hands payloads to a decode worker. Ingest rose from about
7 fps to 40 to 59 fps. A leak fixed alongside: each backend switch stacked
another listener, so frames were decoded and acked N+1 times.

**GPU for headless Chromium, one flag.** Headless Chromium forces
SwiftShader software WebGL by default. The fix was `--enable-gpu` on the
live path plus `/run/opengl-driver/lib` in the Nix wrapper so Chromium
finds Mesa; five folklore flags were rejected as redundant or stale. The
WebGL aquarium went from 26 to 60 browser fps and ingest reached 60.3 fps.

**The decode was the wall.** Two byte-identical render parallelisations cut
render CPU five to ten times and moved the frame rate not at all. An
end-to-end split showed why: JPEG decode took 28 to 31 ms on one worker and
capped output at 33 fps. A decode-worker pool with a drop-oldest queue and a
monotonic sequence gate locked 60 fps while drawing 99.5% of ingested
frames. The lesson: measure the end-to-end per-frame budget on hardware
before optimising a stage.

**SIMD codecs on the byte path.** An interim fused YCbCr average had
already cut large-surface JPEG decode from 64 to 56 ms; `gen2brain/jpegn`
(pure-Go SIMD, native YCbCr) then took `in_decode` from 56 to 29 ms and
decode workers from three to two. Base64 moved to `emmansun/base64`, 7.6x
faster, then 4.06x more with pooled zero-copy decode; CDP wraps every frame
in base64 JSON with no binary path. PNG moved to `woozymasta/png`, about
54% faster on amd64 and byte-identical on 1011 of 1011 corpus PNGs. cgo
libjpeg-turbo at 4.8x was rejected because `CGO_ENABLED=1` breaks the clean
cross-build.

**The frame seam went decoder-native.** `Frame.Img` became `image.Image`:
JPEG stays `*image.YCbCr` and Kitty carries the undecoded CDP payload. The
full-resolution RGBA convert had allocated 28 MB per frame. Shared JPEG
processing fell from 25.446 to 10.209 ms and Kitty PNG framing from 51.950
to 0.014 ms.

**One wire format per consumer, and q90 over q80.** A fixed format per
backend replaced an adaptive PNG/JPEG state machine: Block and Sixel stream
JPEG q90, Kitty passes PNG through or captures JPEG for shared memory. q90
won a matched sweep over q80: PSNR 45.234 to 49.021 dB, SSIM 0.995468 to
0.997696. The budget held: 6.39 ms median headroom remained in the 16.67 ms
frame budget, while q100 doubles the payload and leaves 0.53 ms.

**Diagnostics drive the production seams.** Bounded capture, bundle
tooling, and every capture-only render path were deleted;
`ComposeBlockFrame` is the single compositor for diagnostics and production
alike. A second pipeline meant diagnostics validated code production never
ran; the sweep showed no regression (Block p90 -0.09% to -2.59%).

**Sizing, in three eras.** Sizing passed through a full-grid surface at
device scale 1, then a fixed 1280 CSS layout width, then a per-cell CSS
density anchor: graphics lay out at `max(cols*16, 1024)` CSS px, Block at
`max(cols*8, 1024)`, with `Z = surfW / cssW`. Terminals disagree on the
cell-size unit (Kitty reports 16x33 logical points, Ghostty and WezTerm
26x53 device pixels), so the fixed width drew text 1.6x smaller on the
device-pixel group; the anchor converges both and makes the scale a
per-terminal constant. Browsh's measured 9 CSS px per column is prior art.

**Mid-session zoom without a relaunch.** SIGWINCH re-derives the cell pair
and ships it in the same one-slot, drop-on-full control closure as the
grid restart. The viewer scales frames to the grid because headless
Chromium paints at its launch device scale for the whole process; the
exact-density relaunch was dropped and slight zoom-in softness accepted.

**Crash recovery, supervised then hardened.** A supervisor rebuilds the
session on `Target.targetCrashed` and re-navigates to the held URL;
`Frames()` survives and a renderer crash no longer ends the session.
Hardening followed: a never-committing navigation no longer parks the
rebuild, and the recovered scroll replays after first contentful paint,
because at commit the document has no height and Chromium clamps the
scroll to zero.

## Block

**A quadrant renderer with a half-block floor.** One cell maps to a 2x4
source region; a Rec.601 luma mask at the cell midpoint picks one of 16
quadrant glyphs and partitions sub-cells into foreground and background
averages; half-block `U+2584` is the floor. An early per-mask colour switch
assigned bright and dark backwards. The single-rule mask partition fixed it,
raised block SSIM to 0.66 to 0.70, and halved mean colour error.

**A colour-mode ladder.** Block gained truecolour SGR, a closed-form
xterm-256 fallback, and monochrome; `--color` and `NO_COLOR` are honoured
and Alt+C cycles live. `--color=256` later became the lever for byte-bound
terminals.

**Crisp text, justified by a legibility floor.** `Cell.Text` and
`Cell.Wide` joined the grid; positioned glyph runs composite over the
bitmap, the grapheme wins the cell, and wide glyphs claim two cells and
clip rather than half-emit. The bitmap alone renders text as fuzzy blocks,
the Browsh failure mode; OCR calibration set the floor at 10 px legible,
8 px not (character error rate 0.23), and body text sits below it. The
overlay is gated by an exact-template matcher at CER 0 plus a WCAG AA
4.5:1 contrast count.

**The grid diff writer.** A previous-grid cell diff with SGR and
cursor-move coalescing replaced full-screen repaints; the screen clears only
on first draw and resize. It killed per-frame flicker, and later measured at
0.133 ms per diff at 120x40.

**Text lands where the browser laid it out.** Font forcing pins layout:
embedded FiraCode, font size `cellW / advanceRatio` (0.6154, measured from
the TTF), line height `cellH`, and a transparent text fill so the overlay
is the sole text layer. A 20-glyph runtime calibration probe corrects the
seed, and cell snapping moved from floor to round after a half-cell page
inset binned a whole column late. The corpus gate: mean right-edge error
under 0.5 cells, zero drifted boxes, zero collisions, nine fixtures at two
grid sizes.

**Occlusion cull and hidden-text filters.** Screen-reader-only text
(collapsed clip rects, 1 px boxes) is dropped, and one `elementFromPoint`
hit-test per run drops text covered by an opaque overlay; cookie banners on
bbc.co.uk had leaked through before. LCA-bounded background compositing
later stopped false culls over `pointer-events: none` images.

**Sextant mode behind an allow-list.** A 2x3 scheme (Unicode 13, `U+1FB00`
to `U+1FB3B`) gives 50% more vertical resolution. Auto mode resolves
through an environment-identity allow-list, never a glyph probe, because a
probe cannot tell tofu from a sextant; quadrant stays the floor for unknown
terminals and iTerm2 is excluded for a vertical-gap seam. Chosen by eye.

**The first optimisation batch found the downscale, not the compose.** The
dominant render cost was the full-surface downscale: a persistent row-band
pool parallelised it byte-identically, 3.17 to 0.56 ms at 120x40 and 13.9
to 1.40 ms at 240x86. Integer luma weights (77/150/29) beat a LUT (-13%
quadrant and -21.5% sextant compose; 16 of 24,000 cells flipped, goldens
re-blessed), a `[256]string` SGR table cut the diff by 44.9%, and the
overlay mask became a zero-alloc uint bitmask. None of it moved the frame
rate; the wall was the decode (see Foundations), and once the decode pool
landed Block went from 33 to a locked 60 fps.

**YCbCr-native compose paid for q90.** Live frames now stay decoder-native
YCbCr end to end, and the compositor averages source regions straight into
the grid. The post-fuse profile put the RGBA convert at about 50% of CPU;
deleting the intermediate beat optimising it. Matched q80 processing fell
from 16.725 to 8.889 ms (-46.85%), and the freed headroom paid for q90
fidelity (PSNR 49.021 dB, SSIM 0.997696).

**Colour by lookup, judged by CIEDE2000.** An offline-generated
32,768-byte perceptual LUT, built by exhaustive CIEDE2000 search, replaced
the arithmetic xterm-256 mapping; each colour costs one indexed load. A
three-arm benchmark decided it: the LUT won every perceptual measure (mean
dE -4.81% against nearest-two, worst p95 -20.09%) and was the fastest arm.

**Fewer bytes for byte-bound terminals.** `EmitTrueColour` tracks RGB state
directly (isolated diff -22.6% and -24.1%), a combined fg/bg SGR halves the
SGR count (9,240 to 4,800 at 120x40), and CUP scratch removed the last
allocations, all proven equivalent against a terminal emulator.
Output-identical micro-kernels followed: mask-membership tables cut colour
picking (quadrant -32.8%, sextant -29.6%) and exact packed-RGBA sampling
cut its kernel by 63.7%.

**`REP`, measured and rejected.** ECMA-48 `REP` for runs of three or more
equal cells saved 92 to 98% of bytes on flat fixtures, but regressed p90 by
15.7 to 24.6% with 0% saving on photo and motion content. The production
writer stays literal; a win on the flattering benchmark is not a win.

## Sixel

**A seam with an honest noise floor.** `sixel.Renderer` wrapped
`charmbracelet/x/ansi/sixel` and the harness decoded payloads back to
pixels to gate quality. The encoder is non-deterministic, so a measured
0.2 dB PSNR noise floor earned Sixel a widened 1.5 dB threshold band.

**Why Sixel was slow.** The charm path hashed every pixel into a map, ran
median-cut per frame, and allocated everything fresh. Live frames cost
about 593 ms each; the matched State benchmark put the median at 960.4 ms,
or 1.04 frames per second. libsixel stayed reference-only (the tree is
cgo-free and cross-builds cleanly) and survives as an interop test decoder.

**A 9.45x encoder swap.** `mattn/go-sixel` with an adaptive palette, 256
colours, and dithering off: 256 beat 192 on SSIM on all five quality images,
and dithering was slower, larger, and lower quality. The same State
benchmark median fell from 960.4 to 101.7 ms, allocations by 99.993%,
payload by 18.47%. The PSNR drop from 45.4 to 39.3 dB was accepted: Sixel
is the compatibility backend.

**Forking in-tree.** The dependency became a derived, opaque-frame-only
encoder owning its scratch buffers and the full DCS; alpha handling was
dropped because frames are opaque (a further -8.4%, byte-identical). Owning
the code is the platform for everything after.

**The throughput ladder.** Five rungs stacked up. A 64K palette sample cap
cut combined time by 30.39%; parallel RGB555 LUT build and mapping engage
above 500,000 px; six-row bands emit in parallel with an in-order join. The
repeat ceiling rose to 8192, byte-identical and under tmux's native 10000
cap, cutting payload 17.17% and 21.28%. Rightmost-nonblank plane bounds
skip about 38% of columns; plane scans had been about 76% of encoder CPU.
The accepting sweep put full-encode p90 down 28.22% to 49.63%, landing at
14.55, 19.01, and 21.65 ms across the fixture grids.

**DECSDM on every frame.** Every frame is wrapped in set-mode `?80`
(fixed-page, cursor-neutral); without it a full-height payload hits the
bottom margin and scrolls the viewport. A Foot carve-out omits DEC 2026
only.

**Palette reuse killed the shimmer.** Per-frame re-quantisation changed
202.7 of 255 registers per frame pair, pushing unchanged-pixel dE up to
15.99 (visible shimmer); reuse changed zero (dE 0.000). The encoder retains
palette and LUT while mapping MSE stays within `max(1.15 x baseline, 12.0)`,
and on re-quantisation registers within CIEDE2000 2.3 of an old entry
freeze. A fixed-palette experiment showed the speed (warm encode 45.8%
faster), but fixed palettes cost 7.6 to 12.2 dB PSNR against the adaptive
one; delta-triggered reuse keeps both. Live mlterm played a film at its
native ~24 fps, 6477 of 6478 frames, no shimmer.

**Capture in JPEG, not PNG.** PNG won the synthetic byte sweep and lost
live: a 66.7 ms PNG decode capped ingest at about 25 fps, while JPEG q90
decodes in 13.5 ms and ingest hits the 60 fps CDP cap. The cost: JPEG noise
fires the exact-colour fast path less often.

**The wall moved to the terminal.** Five live runs used identical payloads.
On four emulators Nyala's side was constant (decode 13.0 to 13.7 ms, encode
28.3 to 30.3 ms) and the emulator set the output: mlterm 22.9, Alacritty
17.5, foot 8.1, Contour 4.0, WezTerm 1.8 fps, a 13x spread. WezTerm's
parsing also contends with Nyala for CPU, inflating decode to 29.2 ms and
dropping ingest to 38.3 fps. Sixel has no shared-memory escape, so the
emulator's parser is the wall; freshest-frame pacing degrades gracefully.

**Honest capability gates.** Rio advertises Sixel via DA1 but does not
render it, so auto-selection skips it there. Under tmux and zellij, Sixel
gates on DA1 attribute 4 plus real geometry; the bottom row is reserved
against tmux's top-crop.

**The tmux survival kit.** Sustained 10 fps Sixel had permanently killed
the pane's image rendering. The kit stacks five defences: a byte budget
with a downscale-blur ladder (factors 1.5 to 4), a pacing floor with
latest-wins coalescing, a one-shot input-buffer raise to 16 MiB, no
per-frame erase (the erase was the strobe), and DEC 2026 suppressed (tmux
does not defer image operations under sync).

**The tmux adaptive machinery.** Version gates come first: below tmux 3.6
there is no Sixel at all, because the unraisable 1 MiB DCS cap discards
busy frames as black screens. Byte budgets are 15 MiB on 3.7+, 1.5 MB on
3.6.x, and 900 KB as the fallback. Pacing floors are 250 and 300 ms, with
back-off to `lastWrite + 50 ms` capped at 1 s and escalation on two
floor-level writes. Sharper probes run every 10 budgeted frames, judged at
60% of the floor, and idle settles to sharp by re-emitting the final frame
at factor 1. Each mechanism removed a named failure: black screen, stall,
permanent blur, oscillation.

**zellij runs Block.** Two root-caused zellij defects (an image-id
collision that drops every third full-replacement frame, zellij #5125, and
bare chunk re-emission that breaks scrolling) would present as Nyala bugs.
Block is the zellij backend until upstream fixes land, liftable on the
Zellij `XTVERSION` identity.

## Kitty

**The first backend, lossless by construction.** The first path sent
zlib-compressed raw RGBA via `charmbracelet/x/ansi/kitty`, base64-encoded
in an APC envelope, with every frame decoded and re-encoded. The lossless
round trip (MSE 0) anchored the quality metrics.

**Chunking and the black-screen fix.** Payloads split at the APC chunk
limit, the action moved to transmit-and-put (`a=T`), `q=2` suppressed
replies, and delete-all runs before each redraw because a screen clear
erases text, not images.

**No Kitty under a multiplexer.** tmux and zellij consume Kitty APC
silently with no passthrough, so the probe is never sent there and Kitty is
never offered under a multiplexer. Unicode placeholders (`U=1`) render only
on kitty itself, so no workaround exists.

**Ping-pong ids ended the per-frame flash.** The delete-all blanked the
screen between frames; alternating image ids place the new image before
freeing the old (`a=d,d=I`). The animation-frame edit (`a=f,r=1,o=z`) was
tried and reverted: partial-then-black on kitty, black on WezTerm. When
full-height placements later moved the cursor and smeared the terminal,
`C=1` plus an explicit cursor position fixed it.

**Squeezing the old encoder, 4.5 to 8.5 fps.** Three cuts landed: bulk
pixel reads with `f=24` RGB packing (25% fewer bytes), klauspost zlib
BestSpeed on the live path, and the encode pipelined with the tty write
through ping-pong buffers, so paint cost became `max(encode, write)` (+47%
throughput). Headless encode fell from 67.7 to 12.2 ms and allocations from
4,147,504 to 13.

**Deleting the encoder beat improving it.** Direct PNG pass-through
rechunks Chromium's own base64 PNG straight into `f=100,t=d` frames: zero
decode, zero re-encode, 1.3 ms per live frame. Framing fell from 51.950 to
0.014 ms and the 82% encoder cut above became obsolete. The biggest Kitty
win was deleting code.

**The in-band ceiling, then the transport benchmark.** Direct PNG capped at
12.7 fps at 2560x1500. The 78.9 ms `out_write` was the terminal decoding
base64 and PNG on its main thread, about 21 MB/s of ingest; even Kitty's
post-SIMD parser at ~270 MB/s yields only ~13 fps for raw RGBA. No pixel
format escapes the in-band channel; the designed exit is POSIX shared
memory (`t=s`), field-proven by mpv. The benchmark clocked direct PNG at
16.07 ms, shm PNG at 16.14, shm raw `f=32` at 10.74, and in-band raw at
54.88. shm PNG gained nothing because at 1 MB the terminal's PNG
decode dominates, not transport; the pre-agreed sub-2x gate on shm PNG
fired, so raw-over-shm shipped.

**The shm fast path.** An icat-style probe sends a real 1x1 payload in
`/dev/shm` inside the DA1-fenced handshake. The engine decodes the CDP PNG
into pooled planes, the writer copies into a per-frame shm object and emits
one escape of about 100 bytes; one frame is in flight, opened by the OK
reply or a 40 ms deadline, and any failure demotes the stint to direct.
Aquarium went from 12.7 to 24.4 fps (+92%) and `out_write` from 78.9 to
0.0 ms. The trade: Nyala now pays the PNG decode, 96.9 ms, pipelined.

**JPEG q90 into shm, and 60 fps.** With shm active, capture switches to
JPEG q90: content-stable decode, about 4x cheaper on real pages, 44% of the
payload. Plane decode fell from 96.9 to 17.2 ms and output rose from 24.4
to 59.8 fps, locked to Chromium's 60 fps screencast cap and painting 1635
of 1637 frames; an uncapped run reached 72.9 fps. The progression: 12.7 fps
direct, 24.4 shm PNG, 59.8 shm JPEG (4.7x), and the 2560x2880 torture
surface holds 45.3 fps, ten times the original 4.5.

## The numbers that matter

The wall moved three times. It sat first in the browser's render, fixed
with one GPU flag. It moved to the decode, found only when end-to-end
measurement showed two big render wins moving nothing. It now sits in the
terminal: Sixel's 13x per-emulator spread, and Kitty's in-band channel
escaped through shared memory.

| Measure | Before | After |
| --- | --- | --- |
| Text visible after background paint | 2.7 s later | at first contentful paint |
| DOM snapshot, BBC | 228 ms | 31 ms |
| CDP ingest | ~7 fps | 60.3 fps |
| WebGL aquarium in headless Chromium | 26 fps | 60 fps (one flag) |
| Block output | 33 fps | locked 60 fps, 99.5% of frames |
| Block frame processing, matched input | 16.7 ms | 8.9 ms |
| Stream fidelity, q80 to q90 | 45.2 dB PSNR | 49.0 dB PSNR |
| Sixel internal encode (benchmark) | 960.4 ms median | 14.55 to 21.65 ms p90 |
| Sixel live encode | ~593 ms | 28.3 to 30.3 ms |
| Sixel encoder allocations | baseline | -99.993% |
| Sixel palette churn per frame pair | 202.7 registers | 0 |
| Kitty PNG framing | 51.95 ms | 0.014 ms |
| Kitty aquarium | 12.7 fps | 59.8 fps |
| Kitty torture surface, 2560x2880 | 4.5 fps | 45.3 fps |
