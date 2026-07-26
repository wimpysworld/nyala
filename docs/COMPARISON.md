# Comparison

This document holds the evidence behind Nyala's claim to be the fastest and
highest fidelity terminal browser. It records what was measured, what was read
from source, and what remains unproven.

Terminal browsers are a small field working on a hard problem, and the projects here
solve it in genuinely different ways: patched Chromium, CEF windowless rendering,
Servo, a Firefox extension, and headless Chrome over CDP. Reading their source has
been the best education available on this problem. Nyala's combined cell grid is
Browsh's indexed grid and Carbonyl's cell model ported, and Browsh's hardcoded 9 CSS
px per column is the prior art behind Nyala's layout anchor, both recorded in
[`ARCHITECTURE.md`](./ARCHITECTURE.md). Several techniques below are better than
Nyala's. This document holds Nyala's claims to evidence; it does not diminish the work
that made Nyala possible.

## Status and evidence limits

Performance numbers have four distinct meanings here:

- **Achieved FPS** is observed output under a stated workload, resolution, terminal,
  and machine. It is useful only inside that scope.
- **Configured ceiling** is a timer, rate limit, or requested browser frame rate. It
  is not proof that frames were produced, transported, decoded, and drawn at that
  rate.
- **Internal compute ceiling** is `1000 / stage time in milliseconds`. It excludes
  every unmeasured stage and is not achieved FPS.
- **Subjective evidence** describes perceived responsiveness. It does not establish
  throughput or latency.

Demo videos are excluded from FPS evidence. Frame cadence cannot be recovered
reliably after capture, editing, encoding, hosting, and playback.

Where a project carries no instrumentation, an observed bound is recorded with its
method stated. It is firmer than subjective evidence and it is never presented as
achieved FPS.

Two projects have measured output here, Nyala and Carboxyl. Casty has an observed
bound. The rest have source-verified configured ceilings and source-verified fidelity
reductions. That asymmetry is stated in every table that mixes them.

## Engine and backend matrix

| Project | Browser and frame source | Terminal backends | Update model | Evidence status |
|---|---|---|---|---|
| Nyala | Headless Chromium over CDP. Block and Sixel receive JPEG q90. Shared-memory Kitty captures JPEG q90 and sends raw RGBA. Direct Kitty passes the CDP PNG through. | Block, Kitty, Sixel | Full accepted frames. Block adds a first-writer-wins text overlay. | Measured output, this document. |
| Carbonyl, original | Patched Chromium sends shared-memory BGRA pixels and Chromium damage rectangles to the renderer. | Unicode quadrant cells and a release-era bitmap mode | Damage rectangles constrain pixel-to-cell work. A cell double buffer emits changed cells, with cursor and SGR state reuse. | Archived upstream at Chromium 111.0.5511.1. Source audit at [`ab80a27`](https://github.com/fathyb/carbonyl/blob/ab80a27/src/output/renderer.rs). |
| Carbonyl, jmagly fork | Maintained patched Chromium fork. Its bridge supplies BGRA frames to terminal image encoders. | Unicode quadrant cells, Sixel, Kitty, iTerm2 | The cell path retains damage-aware incremental output. The added image paths encode full live frames. | Image backends added after the latest tagged alpha in [`026485b`](https://github.com/jmagly/carbonyl/commit/026485b). Source audit at [`74e028e`](https://github.com/jmagly/carbonyl/blob/74e028ee3800b33eb93bd80d996bef7c9852e375/src/output/terminal_image.rs). |
| Carboxyl, Servo branch | Servo 0.4 software rendering. Each paint reads the full viewport as RGBA. | Unicode quadrant cells through Ratatui. Sixel and Kitty remain TODO items. | Ratatui diffs cells before flushing, so unchanged cells cost no terminal bytes. Servo, input, and UI use separate threads. | Measured output, plus source audit at [`4cf8858`](https://github.com/carboxyl-rs/carboxyl/blob/4cf88581b3e50f109ec504069fdb92e6516f4f31/src/output/browser_widget.rs). |
| Casty | Chrome Headless Shell over CDP. A quarter-resolution JPEG q30 screencast detects change, then a separate full-resolution screenshot supplies output. | Kitty | A serial capture guard drops screencast events while capture is active. Output is lossless PNG on kitty, Ghostty, and WezTerm, because the terminal detection that would select JPEG fails there. | Source audit at [v1.2.2](https://github.com/sanohiro/casty/blob/v1.2.2/lib/browser.js). Observed bound only; no FPS instrumentation exists. |
| Brow6el | CEF windowless rendering supplies full-resolution BGRA pixels and dirty rectangles. | Sixel, Kitty | Kitty sends full-frame lossless RGB in double-buffered image IDs. Sixel defaults to full-frame monolithic output; tiling exists but is off by default. | Source audit at [`771c650`](https://codeberg.org/janantos/brow6el/src/commit/771c650e49136de9dd2c4a686d521191bc2e7edd/src/kitty_renderer.cpp). |
| Browsh | Firefox WebExtension `drawWindow` capture sends visible RGB data as JSON over WebSocket. Go combines vertical pixel pairs into half-block cells. | Unicode half-block cells | Text is blanked before capture and every glyph is redrawn as a real terminal character, keeping graphics and text separate. | Source audit at [`499ef38`](https://github.com/browsh-org/browsh/blob/499ef386d45cd1e2b5457dd04887c017f77b7e27/webext/src/dom/graphics_builder.js). |

## Measured results

WebGL Aquarium at 2560x1440. Figures are frames drawn on screen, averaged across
the run.

| Browser | Backend | Terminal | Cap | In | Out | Frames drawn |
|---|---|---|---|---|---|---|
| Nyala | Block, sextant truecolour | kitty 0.47.0 | uncapped | 85.8 | 85.5 | 3364 of 3374, 99.7% |
| Nyala | Block | Ghostty 1.3.1 | uncapped | | 84.5 | 3219 of 3232, 99.6% |
| Nyala | Kitty graphics | kitty 0.47.0 | uncapped | 81.3 | 71.5 | 2214 of 2520, 87.9% |
| Nyala | Sixel | Alacritty 0.17.0 | default 60 fps | 60.0 | 19.4 | 1002 of 3107, 32.3% |
| Carboxyl | Unicode quadrants | Alacritty 0.17.0 | uncapped | | 422.5 | 12960 over 30.68 s |

Carboxyl's median was 345.5. It renders about a quarter of the pixels Nyala does,
for the reasons in its section below, and reaching this rate needed a local patch.

Brow6el is absent from this table because the animated content never draws. It loads
and renders the Aquarium page correctly, but its own launch flags leave Chromium with
no GL implementation, so the WebGL canvas fails to initialise. Every other figure here
times that animation, so no comparable measurement exists for Brow6el. The mechanism
is in its section below.

Nyala's Sixel encode cost 25.5 ms per frame against a 51.1 ms terminal write, so
the terminal sets that rate. Uncapping Sixel made it slower, not faster: Alacritty
fell from 19.4 fps to 16.9, and mlterm 3.9.4 from 23.2 to 20.7, with encode rising
to 33.7 ms and write to 58.9 ms. More frames arrive, contend, and are dropped. The
60 fps cap never bound Sixel, because the write sets a wall near 19.6 fps, so
lifting it only added contention. For Block and Kitty the cap does bind, which is
why those rows are uncapped.

## What each browser renders

This table records each project's render resolution, text handling, and colour
depth.

| Browser | Renders at | Text layer | Colour |
|---|---|---|---|
| **Nyala** | `cols*cellW` x `rows*cellH`, the full terminal surface | Crisp glyphs on Block; real page typography on Kitty and Sixel | Truecolour, detected |
| Brow6el | Full surface, less one row | None, pixels only | 24-bit RGB, lossless on Kitty |
| Casty | Full surface, less one row, page zoomed `cellW/8` | None, pixels only | Lossless PNG |
| Carboxyl | `cols*6.5` x `rows*13`, about 325% zoom | None, pixels only | 216-colour cube |
| Carbonyl | `cols*2` x `(rows-1)*4`, about 152% zoom | Glyphs, but all page typography forced to one monospace size | Truecolour, detected |
| Browsh | `cols` x `rows*2` | Glyphs, but every element forced to 15 px | Truecolour, forced |

At a 200 by 50 terminal with 13 by 27 pixel cells, those formulas give roughly 3.5
million pixels per frame for Nyala, 3.4 million for Brow6el and Casty, 845 thousand
for Carboxyl, 78 thousand for Carbonyl, and 20 thousand for Browsh. That geometry is
illustrative.

### Where Nyala loses

Two projects encode pixels more faithfully than Nyala. Brow6el's Kitty path is
lossless uncompressed 24-bit RGB, where Nyala's shared-memory Kitty captures JPEG q90.
Casty outputs lossless PNG on kitty, Ghostty, and WezTerm, though only because its
terminal detection fails, as its section records. Neither project reduces resolution.

Nyala also forces a single font size, on Block, and
[`ARCHITECTURE.md`](./ARCHITECTURE.md) records it as a deliberate trade. Carbonyl and
Browsh force one everywhere.

Nyala is the only project here with both a full-resolution graphics path and a
crisp-text cell path.

## Published claims that are not results

These circulate as performance figures. None is an achieved result under a stated
scope, so none is used above.

| Project | Claim | Classification |
|---|---|---|
| Carbonyl, original | README says 60 FPS and 0% CPU at idle. | Configured ceiling. The 60 matches the default `--fps` value, which the section below shows is an enforced output gate. |
| Carbonyl, original | v0.0.3 reports about 40 FPS for bitmap mode at 1080p on an unspecified M1. | Achieved FPS, scope not stated. No M1 model, page workload, or method. |
| Carbonyl, original prototype | About 5 FPS at 400% Carbonyl CPU, later around 15% while scrolling. | Historical development observations. Direction only. |
| Carbonyl, jmagly fork | No reproducible achieved FPS for the image backends. Fixed-fixture payloads only: 1,135,440 bytes Kitty and 133,180 bytes Sixel at 1280x800. | Payload sizes, not rates. |
| Carboxyl | 60 FPS and 1% idle, stated on the branch. | Configured ceiling, with no hardware, resolution, workload, terminal, or method. Superseded by the measured result above. |
| Third party | Carbonyl felt faster and more responsive than Browsh. | Subjective evidence. No timings. |

## Per-browser findings

### Carboxyl

Servo, `servo-dev` at [`4cf8858`](https://github.com/carboxyl-rs/carboxyl/tree/servo-dev).

- **Magnifies by default.** The Servo viewport is `cols * 6.5` by `rows * 13.0`
  pixels (`src/output/window.rs:38-42`), a constant the source documents as
  reproducing a 325% zoom. On the Aquarium's 1024x1024 canvas the viewport is
  roughly 650 px tall, so the canvas is clipped rather than fitted.
- **Point-samples four corners per cell** with no filtering
  (`src/output/browser_widget.rs:142`). Nyala box-averages.
- **Truecolour is unreachable.** `src/browser/servo.rs:69` reads
  `true_color: u32::from(available_color_count()) >= TRUE_COLOR_COUNT`. The function
  returns a `u16`, maximum 65535, and the constant is `1<<24`, or 16777216. The test
  is false on every platform, so `COLORTERM=truecolor` has no effect and output falls
  back to the xterm 6x6x6 cube of 216 colours by integer divide. The truecolour code
  path is complete and correct, and unreachable. A one-line fix is
  `available_color_count() == u16::MAX`.
- **Default cap is `--fps 60`**, with the budget derived as `1000/fps` in whole
  milliseconds, so 16 ms gives a real ceiling of 62.5. `--fps 1001` yields a zero
  budget and removes the cap. That follows from the integer division and is not
  documented.
- **Uncapped it starves its own input handling.** The terminal saturates
  that the quit key stops responding.
- Ratatui diffs cells before flushing, so Carboxyl does not pay a full-frame cost in
  terminal bytes.

### Carbonyl

fathyb at [`ab80a27`](https://github.com/fathyb/carbonyl/blob/ab80a27/src/output/renderer.rs),
and the jmagly fork at [`74e028e`](https://github.com/jmagly/carbonyl/blob/74e028ee3800b33eb93bd80d996bef7c9852e375/src/output/terminal_image.rs).

- **Fixed 2 by 4 device pixels per terminal cell.** The framebuffer is `cols * 2` by
  `(rows - 1) * 4` whatever the terminal's real cell size, because `auto_scale` is
  false and the cell size is hardcoded to 8 by 16 (`src/output/window.rs:80-97`).
- **About 152% zoom.** A device scale factor of 0.38 comes from a hardcoded 1.5 zoom,
  giving a CSS viewport of `cols * 5.26` by `(rows - 1) * 10.53`. The LoDPI patch is
  `chromium/patches/chromium/0006-Setup-display-DPI.patch`, which removes Chromium's
  0.5 device-scale-factor floor.
- **60 FPS is an enforced output gate, not a browser request.** It is the default
  `--fps` value (`src/cli/cli.rs:40`), applied by a `frame_duration` timer in the
  render thread that limits terminal writes (`src/output/frame_sync.rs:13`,
  `src/output/render_thread.rs:61-103`).
- **Sub-cell sampling is a box average** of 8 pixels per cell
  (`src/output/renderer.rs:157-173`), not a point sample, then binarised to two
  colours per cell.
- **Forces every element to a fixed-size monospace font** and disables glyph
  rasterisation (`chromium/patches/chromium/0010`, `patches/skia/0001`). Text
  decorations are compiled out unconditionally (`0007`).
- Colour is detected: truecolour on `COLORTERM`, else a DECRQSS probe, else xterm-256.
- The jmagly fork keeps the same 2 by 4 framebuffer, font override, and 60 fps gate,
  but sets the device scale factor to 1.0, so its CSS viewport is `cols * 2` by
  `(rows - 1) * 4`, a larger magnification than the original.

### Brow6el

At [`771c650`](https://codeberg.org/janantos/brow6el/src/commit/771c650e49136de9dd2c4a686d521191bc2e7edd/src/kitty_renderer.cpp).

- **Reduces nothing.** It queries the terminal's real pixel size and renders a CEF
  viewport of exactly that, less one text row, at device scale factor 1.0 and page
  zoom 1.0 (`src/terminal_detector.cpp:19-27`, `src/browser_client.cpp:108-113`).
- **Its Kitty path is lossless** 24-bit RGB uncompressed, `f=24`
  (`src/kitty_renderer.cpp:257-303`), where Nyala's shared-memory Kitty captures
  JPEG q90.
- The Sixel path quantises to a 256-colour adaptive palette with Floyd-Steinberg
  dithering, which the protocol requires of Nyala too.
- **Tiled rendering defaults to false** (`profile_config.h:72`), so the default Sixel
  path is full-frame monolithic.
- CEF `windowless_frame_rate` is hardcoded to 30 (`src/main.cpp:459`), with no flag or
  config key. CEF documents it as an enforced maximum on paint delivery. That 30 is a
  configured ceiling, not a result on the benchmark page used here, whose animation
  never draws.
- Its README sample config disagrees with the code defaults: zoom 2.5 against 1.0,
  tiling true against false.
- **WebGL contexts fail to initialise**, through its own launch configuration. The
  page renders, background, chrome, and text all draw, and only the canvas fails,
  with no crash and no blank screen. `createDefaultCefFlagsConfig` writes
  `disable-gpu` and `disable-gpu-compositing` into `~/.brow6el/cef_flags.conf` on
  first run (`src/config.h:152-155`), commented "required for terminal rendering",
  and they reach the command line every run (`src/browser_app.h:24-31`). With
  `--disable-gpu` the pinned Chromium 148 (`download_cef.sh:6`) leaves only
  `GpuMode::DISPLAY_COMPOSITOR`
  (`content/browser/gpu/gpu_data_manager_impl_private.cc:508-534`), and the
  SwiftShader fallback that would supply software WebGL is off in Chromium itself
  (`BASE_FEATURE(kAllowSwiftShaderFallback, base::FEATURE_DISABLED_BY_DEFAULT)`,
  `ui/gl/gl_features.cc:293`). Brow6el passes neither `--enable-unsafe-swiftshader`
  nor `--use-angle=swiftshader`, so the process has no GL implementation. This is a
  configurable default, not a limit of CEF windowless rendering: the CEF commit
  introducing the current offscreen renderer states WebGL and 3D CSS work there where
  GPU support exists. A user can comment the flag out, but config version bumps
  overwrite the file. Scope is WebGL, WebGL2, WebGPU, and hardware video decode;
  Canvas 2D and CSS transforms and animations still work on the CPU. Nyala made the
  opposite one-line choice in the same Chromium era, 150 against 148:
  [`ARCHITECTURE.md`](./ARCHITECTURE.md) records `--enable-gpu` taking the WebGL
  aquarium from 26 to 60 fps. Nyala also passes `--enable-unsafe-swiftshader`, so a
  host with no usable GPU still gets software WebGL rather than none. Two launch
  configurations, not an engine advantage.
- **Observed on the Aquarium**, the page's own fallback appears, not a Brow6el message
  and not a Chrome error page, consistent with Brow6el having no WebGL handling in
  source. `tdl.js` prints "It does not appear your computer supports WebGL", then the
  sample prints Chromium's diagnostic from the `webglcontextcreationerror` event:

  ```
  Status: Could not create a WebGL context, VENDOR = 0xffff, DEVICE = 0xffff,
  GL_VENDOR = Disabled, GL_RENDERER = Disabled, Sandboxed = no, Optimus = no,
  AMD switchable = no, Reset notification strategy = 0x0000,
  ErrorMessage = BindToCurrentSequence failed: .
  ```

  `GL_VENDOR = Disabled` and `GL_RENDERER = Disabled` corroborate the `--disable-gpu`
  chain from the runtime side, so the mechanism is observed, not only inferred.

### Casty

At [v1.2.2](https://github.com/sanohiro/casty/blob/v1.2.2/lib/browser.js).

- **Does not downscale.** It renders exactly the terminal's pixel grid, one image
  pixel per terminal pixel.
- The quarter-resolution JPEG q30 screencast is a change detector only and is never
  displayed. Its `data` field is destructured and discarded
  (`lib/browser.js:370-383`). Every displayed frame is a separate full-resolution
  `Page.captureScreenshot`.
- **Applies a page zoom of `cellWidth / 8`**, so the CSS layout viewport is always
  `cols * 8` pixels wide regardless of font size (`bin/casty.js:74-81`,
  `lib/browser.js:257-265`). Less page is visible than in Nyala at the same pixel
  count, but no pixels are lost.
- **Output is lossless PNG** on kitty, Ghostty, and WezTerm, through a code path that
  diverges from the documented behaviour. Casty's README says kitty takes file
  transport, which selects JPEG 85
  in motion with a PNG refresh 500 ms after activity stops. The transport check reads
  only `TERM_PROGRAM` (`lib/kitty.js:47-56`), and kitty does not set that variable, so
  the match fails and the inline path is taken instead. Inline forces PNG, because the
  Kitty graphics protocol has no JPEG format code, and the refine step never runs
  (`lib/browser.js:323-325`, `:339-340`). Confirmed on the maintainer's kitty, where
  `TERM_PROGRAM` is empty.
- A hardcoded 50 ms minimum capture interval allows 20 attempts per second
  (`lib/browser.js:31`) and is not configurable. The project's own notes record about
  12 fps in practice.
- **Observed bound**, on kitty 0.47.0 at 2560x1440 running the WebGL Aquarium: the
  page's own in-page overlay reports about 30 fps, while delivered terminal output is
  visibly 10 fps or lower. Casty carries no FPS instrumentation, so this is a visual
  estimate and an upper bound, not achieved FPS. It sits below both the 20 attempts
  per second the capture interval allows and the project's own note of about 12 fps.
- **The output strobes** persistently through that run. This is a rendering-quality
  observation, separate from the rate above. Casty reuses a single Kitty image ID,
  `i=1`, hardcoded at all three emit sites (`lib/kitty.js:113`, `:127`, `:137`), with
  no placement ID and no alternation. It issues no per-frame delete or clear either;
  the only per-frame control sequence is a bare cursor move, `\x1b[2;1H`
  (`lib/kitty.js:60`). The teardown is kitty's, a consequence of the ID reuse rather
  than a delete: `handle_add_command` frees the existing image's placements when the
  first 4096-byte chunk of a same-ID replacement arrives and marks the screen dirty
  (`kitty/graphics.c`, verified in v0.40.1, v0.42.2, v0.43.1, and master), and the new
  placement is created only once the final chunk lands, because the put is gated on
  the transfer completing. Casty sends no DEC 2026 synchronised update anywhere, so
  nothing holds the repaint across the gap, and the page is off screen for the length
  of every inline transfer.
- Limits on that finding: the blank window's duration is not measured, which would
  need a terminal-side trace, and whether a given payload strobes visibly depends on
  its size, so the mechanism explains the observation rather than predicting it
  universally. The file transport (`t=f`) does not have the problem, because add and
  put complete in one parse pass, but the `TERM_PROGRAM` match noted above means kitty
  takes the inline path.
- Nyala alternates two Kitty image IDs, transmitting and placing the next image before
  deleting the previous one, and brackets each frame in DEC 2026 in a single write
  ([`ARCHITECTURE.md`](./ARCHITECTURE.md), under Graphics backends and The diff
  writer), so the previous image leaves the screen only after its replacement is
  placed.

### Browsh

At [`499ef38`](https://github.com/browsh-org/browsh/blob/499ef386d45cd1e2b5457dd04887c017f77b7e27/webext/src/dom/graphics_builder.js).

- **Output raster is `cols` by `rows * 2` pixels.** The Firefox window is `cols * 9`
  by `(rows + 1) * 20` CSS px and the capture is downscaled by 1/9 horizontally and
  1/10 vertically, so 90 CSS pixels collapse into each output pixel. It never renders
  at display resolution, and no device-pixel-ratio handling exists.
- The 9 CSS px per column is hardcoded (`webext/src/dom/dimensions.js`), exactly
  0.6 em at a forced 15 px font size. It is not measured.
- **It does not burn glyphs into the bitmap.** It forces every element to a
  block-glyph font, blanks text before capture, and redraws every glyph as a real
  terminal character. That is the same architectural choice Nyala makes.
- Every element is forced to 15 px with no weight or style
  variation (`webext/assets/styles.css`), so headings and body copy render
  identically and `white-space: pre` formatting is lost.
- Gecko filters the downscale, so sub-cell sampling is filtered rather than
  point-sampled.
- Colour is forced to 24-bit by setting `TERM=xterm-truecolor`.
- `small_pixel_frame_rate = 250` ms is the shipped default, not a sample value. The
  sample config is written verbatim as the user's config and loaded as the base layer
  either way.

## The coward's lever

Our house view: aggressive downscaling for headline numbers is the coward’s lever. 😁

The audits above make that view concrete. Three projects cut the raster itself, by
design. Carboxyl magnifies to about 325%. Carbonyl renders a fixed 2 by 4 pixels per
cell at about 152% zoom. Browsh collapses 90 CSS pixels into each output pixel. None
of the three can be configured out of it. Casty is a milder case: it zooms the layout
viewport, so less page is visible, but it loses no pixels.

Brow6el queries the terminal's real pixel size and renders exactly that, at device
scale factor 1.0 and page zoom 1.0. The reduction the other three make is therefore a
choice rather than a necessity.

Nyala fixes resolution and fidelity during performance work because that exposes the
true bottleneck. Downscaling can hide browser encoding, pixel mapping, allocation,
protocol transport, terminal composition, decode, and draw costs. It would also have
hidden the stacked micro-optimisations: direct Kitty pass-through, fused Block
processing, parallel decode, bounded Sixel mapping and band output, colour sampling,
plane bounds, and longer RLE runs ([Nyala renderer evolution](./EVOLUTION.md)).

So downscaling is the last option. It needs explicit owner approval, stays off the
roadmap until that exists, and must never enter a benchmark silently or support a
like-for-like claim. Competitor-native tuning and reduced-fidelity tests keep their
value in two labelled tracks: **tuned competitor**, documented settings that preserve
the output surface and fidelity contract, and **fidelity reduced**, any weaker output
contract, whose results cannot headline the like-for-like table.

## Direct benchmark protocol

The measured results above cover two projects on one page. This protocol defines the
full cross-project run.

**Environment.** One machine, OS image, terminal build, and shell, at fixed CPU power
settings. Record CPU, memory, GPU, terminal, engine revision, project commit, compiler,
and every flag. Run each project directly in the same session type; never compare a
multiplexer run with a direct one. Test 1920x1080 and 2560x1600 at a fixed
device-pixel ratio, matching rows, columns, cell pixels, viewport pixels, and page
zoom, preserving each backend's full output resolution on the like-for-like track.

**Workloads.** Local, versioned fixtures: a static page then idle; a fixed vertical
scroll; a high-entropy full-screen animation; an image-heavy page; a sparse local
mutation such as a clock; and interactive typing, focus movement, and link activation.
Capture correctness evidence for every run, and reject any with missing content, wrong
dimensions, stale regions, unreadable text, or visible frame assembly faults before
comparing speed.

**Measurement.** Measure achieved frames at the last observable application and
terminal boundary, not requested capture cadence. Record arrival rate, accepted and
dropped frames, output and terminal-presented FPS, frame-interval and latency
percentiles, CPU and RSS, payload bytes, per-stage timings where instrumentation
permits, and each run's output resolution, colour count, format, and quality. Keep
stage-only compute ceilings in a separate table, never labelled FPS without the
`internal compute ceiling` qualifier. Use at least six fresh processes per arm, rotate
project order against thermal and cache bias, and publish raw data, commands, fixtures,
rejected-run reasons, and the table-building script.

**Result tracks.** Publish four, unmerged, because a project can lead one workload and
lose another: **like-for-like full fidelity**, fixed surface and comparable quality;
**native default**, each project's own defaults with resolution and quality printed
beside every result; **tuned competitor**, declared settings against the full-fidelity
contract; and **fidelity reduced**, every concession named in the table title and each
row.

## Decisions and risks

Decisions:

- Claim "fastest and highest fidelity terminal browser". It rests on measured Nyala
  output, measured Carboxyl output, source-verified configured ceilings for the rest,
  and source-verified fidelity reductions in four of the five other projects. The claim stands
  until a cross-project benchmark under the protocol above contradicts it.
- State what the claim does not cover. Brow6el and Casty beat Nyala on pixel encoding,
  and no other project has been measured on the full workload set.
- Make every launch statement backend-specific, workload-specific, resolution-specific,
  and terminal-specific, and keep achieved FPS, configured ceilings, and internal
  compute ceilings separate.
- Keep graphics downscaling closed, and damage tracking, dirty-tile output, and partial
  Sixel off the roadmap, unless the owner reopens them. Any exception needs approval and
  a fidelity-reduced label.
- Report competitor-native and tuned results even when they win. Never use a Nyala-only
  optimisation as the definition of fairness.

Risks:

- The headline claim rests on one page and one machine. A different workload class
  could change the order.
- Terminal PNG, Sixel, and character-cell implementations can reverse a result.
- FPS can conceal latency, dropped frames, tearing, stale regions, or reduced quality.
- A fixed palette, lossy image setting, reduced colour mode, or downscale can trade
  fidelity for speed without making that trade visible in the headline.
- Public claims can be stale, configured targets can be mistaken for achieved output,
  and subjective reports can be mistaken for measurements.

## Primary source set

Sources not already linked inline above.

- Nyala: [performance envelope](./ARCHITECTURE.md#performance-envelope).
- Carbonyl original: [v0.0.3 release](https://github.com/fathyb/carbonyl/releases/tag/v0.0.3), [architecture article](https://fathy.fr/carbonyl), [painter](https://github.com/fathyb/carbonyl/blob/ab80a27/src/output/painter.rs), [render thread](https://github.com/fathyb/carbonyl/blob/ab80a27/src/output/render_thread.rs).
- Carbonyl fork: [repository](https://github.com/jmagly/carbonyl), [tagged alpha](https://github.com/jmagly/carbonyl/releases/tag/v0.2.0-alpha.18), [Sixel](https://github.com/jmagly/carbonyl/blob/74e028ee3800b33eb93bd80d996bef7c9852e375/src/output/sixel.rs), [bridge](https://github.com/jmagly/carbonyl/blob/74e028ee3800b33eb93bd80d996bef7c9852e375/src/browser/bridge.rs).
- Carboxyl: [release candidate](https://github.com/carboxyl-rs/carboxyl/releases/tag/v0.1.0-servo-rc.1), [Servo thread](https://github.com/carboxyl-rs/carboxyl/blob/4cf88581b3e50f109ec504069fdb92e6516f4f31/src/browser/servo/servo_thread.rs), [session](https://github.com/carboxyl-rs/carboxyl/blob/4cf88581b3e50f109ec504069fdb92e6516f4f31/src/browser/servo/session.rs).
- Casty: [README](https://github.com/sanohiro/casty/blob/v1.2.2/README.md), [Kitty source](https://github.com/sanohiro/casty/blob/v1.2.2/lib/kitty.js).
- Brow6el: [README](https://codeberg.org/janantos/brow6el/src/commit/771c650e49136de9dd2c4a686d521191bc2e7edd/README.md), [Sixel renderer](https://codeberg.org/janantos/brow6el/src/commit/771c650e49136de9dd2c4a686d521191bc2e7edd/src/sixel_renderer.cpp).
- Browsh: [README](https://github.com/browsh-org/browsh/blob/499ef386d45cd1e2b5457dd04887c017f77b7e27/README.md), [configuration](https://github.com/browsh-org/browsh/blob/499ef386d45cd1e2b5457dd04887c017f77b7e27/interfacer/src/browsh/config_sample.go#L55-L60), [frame builder](https://github.com/browsh-org/browsh/blob/499ef386d45cd1e2b5457dd04887c017f77b7e27/interfacer/src/browsh/frame_builder.go).
- Third-party context: [xkyle.com console browser comparison](https://xkyle.com/Modern-Text-Console-Browsers-in-2023/).
