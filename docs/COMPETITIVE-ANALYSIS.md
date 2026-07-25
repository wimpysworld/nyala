# Competitive Analysis

Status: provisional, 18 July 2026.

There is no honest overall leaderboard yet. The projects use different browser
engines, update models, resolutions, terminals, page workloads, and definitions of
FPS. This document records what the available evidence supports, then defines the
direct benchmark needed before launch.

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

Nyala's current measurements are project-owned results, not a cross-project
benchmark. On the stated aquarium run, Block output reached the browser's 60 FPS
arrival rate after parallel JPEG decode. At 2560x2880, kitty, Alacritty, and foot
held 60 FPS while WezTerm and Ghostty saturated sooner
([Nyala live measurements](./PERFORMANCE.md#hardware-outcome-2026-07-06-the-decode-was-the-wall),
[terminal result](./PERFORMANCE.md#terminal-ceiling-found-2026-07-07)). The terminal,
colour mode, workload, and frame entropy therefore remain part of every result.

The latest controlled p90 measurements on a Ryzen AI MAX+ 395 used Go 1.26.5,
Chromium 150.0.7871.114, `GOMAXPROCS=4`, four warm-ups, and 30 retained samples in
each of six rotated processes. At 1920x1080, full Sixel encode took 14.5515 ms and
PNG decode plus encode took 27.7895 ms. These imply internal compute ceilings of
68.7 and 36.0 FPS. They exclude terminal write, decode, and draw. Kitty's 0.0080 ms
State result measures framing after direct CDP PNG pass-through, not browser capture
or terminal PNG decode and draw
([Nyala controlled sweep](./PERFORMANCE.md#capture-removal-and-combined-sixel-sweep-2026-07-18)).

## Engine and backend matrix

| Project | Browser and frame source | Terminal backends | Update model | Evidence status |
|---|---|---|---|---|
| Nyala | Headless Chromium over CDP. Block receives JPEG. Kitty and Sixel receive PNG. | Block, Kitty, Sixel | Full accepted frames. Block adds a first-writer-wins text overlay. Kitty passes the CDP PNG through. | Current project code and controlled measurements ([architecture](./ARCHITECTURE.md), [measurements](./PERFORMANCE.md#capture-removal-and-combined-sixel-sweep-2026-07-18)). |
| Carbonyl, original | Patched Chromium sends shared-memory BGRA pixels and Chromium damage rectangles to the renderer. | Unicode quadrant cells and a release-era bitmap mode | Damage rectangles constrain pixel-to-cell work. A cell double buffer emits changed cells, with cursor and SGR state reuse. | Archived upstream at Chromium 111.0.5511.1. The implementation is visible in its [renderer](https://github.com/fathyb/carbonyl/blob/ab80a27/src/output/renderer.rs), [painter](https://github.com/fathyb/carbonyl/blob/ab80a27/src/output/painter.rs), and [render thread](https://github.com/fathyb/carbonyl/blob/ab80a27/src/output/render_thread.rs). |
| Carbonyl, jmagly fork | Maintained patched Chromium fork. Its bridge supplies BGRA frames to terminal image encoders. | Unicode quadrant cells, Sixel, Kitty, iTerm2 | The cell path retains damage-aware incremental output. The added image paths encode full live frames. | Image backends were added after the latest tagged alpha in commit [`026485b`](https://github.com/jmagly/carbonyl/commit/026485b); inspect the pinned [terminal image](https://github.com/jmagly/carbonyl/blob/74e028ee3800b33eb93bd80d996bef7c9852e375/src/output/terminal_image.rs), [Sixel](https://github.com/jmagly/carbonyl/blob/74e028ee3800b33eb93bd80d996bef7c9852e375/src/output/sixel.rs), and [bridge](https://github.com/jmagly/carbonyl/blob/74e028ee3800b33eb93bd80d996bef7c9852e375/src/browser/bridge.rs) sources. |
| Carboxyl, Servo branch | Servo 0.4 software rendering. Each paint reads the full viewport as RGBA. | Unicode quadrant cells through Ratatui. Sixel and Kitty remain TODO items. | A full terminal grid is rebuilt from each paint. Servo, input, and UI use separate threads. | Early Servo release candidate. See the [release](https://github.com/carboxyl-rs/carboxyl/releases/tag/v0.1.0-servo-rc.1), [Servo thread](https://github.com/carboxyl-rs/carboxyl/blob/4cf88581b3e50f109ec504069fdb92e6516f4f31/src/browser/servo/servo_thread.rs), [widget](https://github.com/carboxyl-rs/carboxyl/blob/4cf88581b3e50f109ec504069fdb92e6516f4f31/src/output/browser_widget.rs), and [session](https://github.com/carboxyl-rs/carboxyl/blob/4cf88581b3e50f109ec504069fdb92e6516f4f31/src/browser/servo/session.rs). An alternative display-list text path is an [unmerged pull request](https://github.com/carboxyl-rs/carboxyl/pull/21). |
| Casty | Chrome Headless Shell over CDP. A quarter-resolution JPEG quality 30 screencast detects change, then a separate full-resolution screenshot supplies output. | Kitty | A serial capture guard drops screencast events while capture is active. Active output uses JPEG quality 85, then switches to PNG after 500 ms idle. | Version 1.2.2 implementation, with no reproducible achieved FPS result in the [README](https://github.com/sanohiro/casty/blob/v1.2.2/README.md), [browser source](https://github.com/sanohiro/casty/blob/v1.2.2/lib/browser.js), or [Kitty source](https://github.com/sanohiro/casty/blob/v1.2.2/lib/kitty.js). |
| Brow6el | CEF windowless rendering supplies full-resolution BGRA pixels and dirty rectangles. | Sixel, Kitty | Sixel maps dirty rectangles to roughly 250-pixel, cell-aligned tiles and can compare against the prior BGRA frame. Kitty sends full-frame RGB in double-buffered image IDs. | Current pinned [README](https://codeberg.org/janantos/brow6el/src/commit/771c650e49136de9dd2c4a686d521191bc2e7edd/README.md), [Sixel renderer](https://codeberg.org/janantos/brow6el/src/commit/771c650e49136de9dd2c4a686d521191bc2e7edd/src/sixel_renderer.cpp), and [Kitty renderer](https://codeberg.org/janantos/brow6el/src/commit/771c650e49136de9dd2c4a686d521191bc2e7edd/src/kitty_renderer.cpp). |
| Browsh | Firefox WebExtension `drawWindow` capture sends visible RGB data as JSON over WebSocket. Go combines vertical pixel pairs into half-block cells. | Unicode half-block cells | Requested captures cover the visible terminal-sized viewport. The Go frame builder constructs the cell frame. | Current pinned [graphics builder](https://github.com/browsh-org/browsh/blob/499ef386d45cd1e2b5457dd04887c017f77b7e27/webext/src/dom/graphics_builder.js), [frame builder](https://github.com/browsh-org/browsh/blob/499ef386d45cd1e2b5457dd04887c017f77b7e27/interfacer/src/browsh/frame_builder.go), and [README](https://github.com/browsh-org/browsh/blob/499ef386d45cd1e2b5457dd04887c017f77b7e27/README.md). |

## Published performance claims

The available headline numbers are not comparable results.

| Project | Public or configured number | Classification and limit |
|---|---:|---|
| Carbonyl, original | README says 60 FPS and 0% CPU at idle. Version 0.0.3 reports about 40 FPS for bitmap mode at 1080p in Alacritty on an unspecified M1. | The 60 FPS value also matches the default maximum frame-rate setting, so it is not a complete benchmark. The 40 FPS report lacks the exact M1 model, page workload, and measurement method ([architecture article](https://fathy.fr/carbonyl), [v0.0.3 release](https://github.com/fathyb/carbonyl/releases/tag/v0.0.3)). |
| Carbonyl, original prototype | About 5 FPS at 400% Carbonyl CPU plus about 200% iTerm2 CPU, later around 15% CPU while scrolling and 0% idle. | Historical development observations. Hardware, resolution, workload, and measurement method are absent, so the numbers show direction only ([architecture article](https://fathy.fr/carbonyl)). |
| Carbonyl, jmagly fork | No new reproducible achieved FPS result found for the image backends. | The fork contains useful fixed-fixture payload measurements, including 1,135,440 bytes for Kitty and 133,180 bytes for Sixel at 1280x800, but not encode time, CPU, or FPS ([measurements](https://github.com/jmagly/carbonyl/blob/74e028ee3800b33eb93bd80d996bef7c9852e375/test/repro/issue-241/MEASUREMENTS.md)). |
| Carboxyl, Servo branch | 60 FPS default and 1% idle are branch claims. | No stated hardware, resolution, page workload, terminal, or achieved-frame method accompanies them. Treat 60 as a configured or advertised target until reproduced ([Servo branch](https://github.com/carboxyl-rs/carboxyl/tree/servo-dev)). |
| Casty | 50 ms minimum capture interval. | At most 20 capture attempts per second before capture and output costs. The serial guard can drop intervening events. This is an internal ceiling, not achieved terminal FPS ([browser source](https://github.com/sanohiro/casty/blob/v1.2.2/lib/browser.js)). |
| Brow6el | CEF `windowless_frame_rate=30`. | Requested browser cadence only. It does not measure encode, stdout, terminal decode, or draw ([main source](https://codeberg.org/janantos/brow6el/src/commit/771c650e49136de9dd2c4a686d521191bc2e7edd/src/main.cpp)). |
| Browsh | `small_pixel_frame_rate=250 ms`. | Roughly four request ticks per second before capture, JSON transport, cell conversion, and terminal draw. It is not achieved FPS ([sample configuration](https://github.com/browsh-org/browsh/blob/499ef386d45cd1e2b5457dd04887c017f77b7e27/interfacer/src/browsh/config_sample.go#L55-L60)). |

One third-party comparison said Carbonyl felt faster and more responsive than Browsh.
It published no timings, so it remains subjective evidence
([xkyle.com comparison](https://xkyle.com/Modern-Text-Console-Browsers-in-2023/)).

## Implementation optimisation comparison

| Project and path | Work avoided | Work retained | Expected workload bias |
|---|---|---|---|
| Nyala Block | Parallel decode prevents one JPEG decoder from setting the arrival ceiling. Fused YCbCr averaging avoids a full RGBA conversion and allocation before cell composition. | Accepted frames still cover the viewport. Terminal cell output can become byte-bound. | Strong when decode and composition fit the frame budget. Terminal throughput matters under high-entropy motion ([Nyala Block results](./PERFORMANCE.md#large-surface-decode-cut-no-downscaling-2026-07-06)). |
| Nyala Kitty | Direct CDP PNG pass-through removes base64 decode, PNG decode, pixel packing, zlib, and base64 re-encode inside Nyala. | Full PNG frames still cross the terminal protocol and must be decoded and drawn by the terminal. | Strong full-frame path. The terminal PNG implementation can set the end-to-end ceiling ([closed Kitty work](./PERFORMANCE.md#top-recommendations-ranked-by-impact-vs-effort)). |
| Nyala Sixel | Bounded parallel mapping and band emission, a capped colour sample, long RLE counts, and plane bounds reduce CPU and bytes without changing the accepted output of the measured optimisation pair. | Full accepted frames are decoded, quantised, encoded, written, decoded, and drawn. | Strong full-frame Sixel path. Sparse changes still pay full-frame costs ([controlled sweep](./PERFORMANCE.md#capture-removal-and-combined-sixel-sweep-2026-07-18)). |
| Carbonyl cell path | Chromium damage rectangles bound conversion work. Cell double buffering avoids rewriting unchanged cells. Cursor and SGR reuse cut control bytes. | Changed character cells still require colour mapping and terminal output. | Strong for sparse page damage and character-cell output ([renderer](https://github.com/fathyb/carbonyl/blob/ab80a27/src/output/renderer.rs), [painter](https://github.com/fathyb/carbonyl/blob/ab80a27/src/output/painter.rs)). |
| jmagly image paths | Sixel skips inactive planes and uses RLE. Its palette is exact at 256 colours or fewer, then uses RGB332. | Live Kitty, iTerm2, and Sixel image paths encode full frames. The pinned implementation is scalar and does not show parallel encoding. | Useful full-frame image support, with no current achieved-rate comparison ([Sixel source](https://github.com/jmagly/carbonyl/blob/74e028ee3800b33eb93bd80d996bef7c9852e375/src/output/sixel.rs), [terminal image source](https://github.com/jmagly/carbonyl/blob/74e028ee3800b33eb93bd80d996bef7c9852e375/src/output/terminal_image.rs)). |
| Brow6el Sixel | CEF dirty rectangles restrict candidate tiles. A prior-frame comparison can reject unchanged tile data. | Each changed tile creates a libsixel palette and is encoded serially to stdout. | Strong for sparse, local changes. Large or full-screen motion can erase the dirty-tile advantage ([Sixel renderer](https://codeberg.org/janantos/brow6el/src/commit/771c650e49136de9dd2c4a686d521191bc2e7edd/src/sixel_renderer.cpp)). |
| Brow6el Kitty | Double-buffered image IDs reduce visible replacement artefacts. | Each update converts and transmits a full RGB frame as uncompressed base64 in 16 KiB chunks. | Full-motion cost grows with surface size and terminal transport ([Kitty renderer](https://codeberg.org/janantos/brow6el/src/commit/771c650e49136de9dd2c4a686d521191bc2e7edd/src/kitty_renderer.cpp)). |
| Casty Kitty | Low-resolution, low-quality JPEG screencast frames act only as change signals. Idle output moves to lossless PNG. | Each accepted signal triggers a separate full-resolution screenshot and full-viewport Kitty transfer. | Favours pages with idle periods. Serial capture can discard rapid updates ([browser source](https://github.com/sanohiro/casty/blob/v1.2.2/lib/browser.js), [Kitty source](https://github.com/sanohiro/casty/blob/v1.2.2/lib/kitty.js)). |
| Browsh | Capture is sized around terminal output rather than preserving the browser's full source resolution. Half-block cells compress two vertical pixels into one character. | Full visible RGB JSON crosses the WebSocket, then Go rebuilds the character frame. | Favours low-resolution character output, not like-for-like full-fidelity graphics ([graphics builder](https://github.com/browsh-org/browsh/blob/499ef386d45cd1e2b5457dd04887c017f77b7e27/webext/src/dom/graphics_builder.js), [frame builder](https://github.com/browsh-org/browsh/blob/499ef386d45cd1e2b5457dd04887c017f77b7e27/interfacer/src/browsh/frame_builder.go)). |
| Carboxyl Servo | Separate Servo, input, and UI threads isolate responsibilities. | Every paint reads the full software-rendered viewport and rebuilds the full quadrant grid. | Current evidence does not establish a sparse-update advantage ([Servo thread](https://github.com/carboxyl-rs/carboxyl/blob/4cf88581b3e50f109ec504069fdb92e6516f4f31/src/browser/servo/servo_thread.rs), [widget](https://github.com/carboxyl-rs/carboxyl/blob/4cf88581b3e50f109ec504069fdb92e6516f4f31/src/output/browser_widget.rs)). |

## Provisional workload-specific positions

These are engineering judgements from implementation shape and current evidence.
They are hypotheses for the direct benchmark, not launch claims.

- **Full-frame Kitty:** Nyala appears strongest. Direct CDP PNG pass-through removes
  almost all application-side frame work. End-to-end results still depend on the
  terminal's PNG decode and draw path.
- **Full-frame Sixel:** Nyala appears strongest. Its measured parallel mapper and
  band emitter have current, controlled CPU and correctness evidence. No direct
  competitor run yet proves the end-to-end ranking.
- **Damage-aware character cells:** Carbonyl appears strongest. Chromium damage
  rectangles, cell comparison, and incremental terminal output align well with
  sparse page changes.
- **Sparse Sixel:** Brow6el appears strongest. CEF dirty rectangles and tile-level
  rejection can avoid encoding most of a quiet frame.

Nyala will not pursue damage tracking merely to chase these positions. Damage
tracking, dirty tiles, and partial Sixel output remain outside the accepted roadmap.
Reopening them needs an explicit owner decision.

## The coward's lever

Our deliberately spicy house view: aggressive downscaling for headline numbers is
the coward’s lever. 😁

Nyala fixes resolution and fidelity during performance work because that exposes
the true bottleneck. Downscaling can hide browser encoding, pixel mapping,
allocation, protocol transport, terminal composition, decode, and draw costs. It
would also have hidden the stacked micro-optimisations that accumulated over the
last two weeks: direct Kitty pass-through, fused Block processing, parallel decode,
bounded Sixel mapping and band output, colour sampling, plane bounds, and longer RLE
runs ([Nyala performance record](./PERFORMANCE.md)).

Downscaling is the last option. It requires specific, explicit owner approval and
must remain outside the active roadmap until that approval exists. It must never enter a
benchmark silently or support a like-for-like claim.

Competitor-native tuning and reduced-fidelity tests still have value, but they are
separate labelled tracks:

- **Tuned competitor:** documented settings that preserve the fixed output surface
  and fidelity contract.
- **Fidelity reduced:** lower resolution, lower colour depth, lossy quality changes,
  or another weaker output contract. Results cannot headline the like-for-like
  table.

## Direct benchmark protocol before launch

### Fixed environment

Use one machine, one OS image, one terminal build, one shell environment, and fixed
CPU power settings. Record CPU model, core count, memory, GPU, terminal, browser
engine revision, project commit, compiler, and every flag. Run each project directly
in the same terminal session type. Do not compare a multiplexer run with a direct
run.

Test 1920x1080 and 2560x1600 browser surfaces at a fixed device-pixel ratio. Match
terminal rows, columns, cell pixels, viewport pixels, and page zoom. Preserve each
backend's full output resolution on the like-for-like track.

### Workloads

Use local, versioned fixtures with deterministic assets and scripts:

1. Static documentation page, followed by idle time.
2. Smooth vertical text scroll at a fixed distance and duration.
3. High-entropy full-screen animation.
4. Image-heavy page with fixed decoded assets.
5. Sparse local mutation, such as a clock or small animated region.
6. Interactive typing, focus movement, and link activation.

Record correctness captures for every project and workload. Reject a run with
missing content, wrong dimensions, stale regions, unreadable text, or visible frame
assembly faults before comparing speed.

### Measurement

Measure achieved frames at the last observable application and terminal boundary,
not only requested capture cadence. Record:

- browser or engine frame arrival rate;
- accepted and dropped frames;
- application output FPS and terminal-presented FPS where the terminal exposes it;
- median, p90, p95, and p99 frame interval and input-to-visible latency;
- application and terminal CPU, RSS, and total process-tree CPU;
- payload bytes per frame and bytes per second;
- capture, decode, map, quantise, encode, write, terminal decode, and draw time where
  instrumentation permits;
- output resolution, colour count, image format, quality setting, and visual error.

Keep stage-only compute ceilings in a separate table. Never label them FPS without
the `internal compute ceiling` qualifier.

Use at least six fresh processes per arm. Rotate project order to reduce thermal and
cache bias. Use declared warm-ups, a fixed retained sample count, and the same page
timeline. Publish raw data, commands, fixtures, rejected-run reasons, and the script
that builds the tables.

### Result tracks

Publish four separate tables:

1. **Like-for-like full fidelity:** fixed surface, comparable colour and quality,
   project defaults only where they meet that contract.
2. **Native default:** each project's documented default, with resolution and quality
   printed beside every result.
3. **Tuned competitor:** declared settings, fixed full-fidelity contract, and a reason
   for every change.
4. **Fidelity reduced:** every downscale or quality concession named in the table
   title and each row.

Do not merge these tracks into one ranking. A project can lead one workload and lose
another.

## Decisions and risks

Decisions:

- Publish no overall winner before the direct benchmark.
- Make every launch statement backend-specific, workload-specific, resolution-specific,
  and terminal-specific.
- Keep achieved FPS, configured ceilings, and internal compute ceilings separate.
- Keep damage tracking, dirty-tile output, and partial Sixel outside Nyala's roadmap
  unless the owner reopens them.
- Keep graphics downscaling closed. Any exception needs specific owner approval and a
  fidelity-reduced label.
- Report competitor-native and tuned results even when they win. Never use a Nyala-only
  optimisation as the definition of fairness.

Risks:

- Terminal PNG, Sixel, and character-cell implementations can reverse a result.
- Sparse and full-frame workloads reward different update models.
- Browser engine revisions can dominate a project-level comparison.
- FPS can conceal latency, dropped frames, tearing, stale regions, or reduced quality.
- Application CPU alone can move work into the terminal or browser process.
- Public claims can be stale, configured targets can be mistaken for achieved output,
  and subjective reports can be mistaken for measurements.
- A fixed palette, lossy image setting, reduced colour mode, or downscale can trade
  fidelity for speed without making that trade visible in the headline.

## Primary source set

- Nyala: [performance record](./PERFORMANCE.md) and
  [architecture](./ARCHITECTURE.md).
- Carbonyl original: [v0.0.3 release](https://github.com/fathyb/carbonyl/releases/tag/v0.0.3),
  [architecture article](https://fathy.fr/carbonyl),
  [renderer](https://github.com/fathyb/carbonyl/blob/ab80a27/src/output/renderer.rs),
  [painter](https://github.com/fathyb/carbonyl/blob/ab80a27/src/output/painter.rs), and
  [render thread](https://github.com/fathyb/carbonyl/blob/ab80a27/src/output/render_thread.rs).
- Maintained Carbonyl fork: [repository](https://github.com/jmagly/carbonyl),
  [latest tagged alpha](https://github.com/jmagly/carbonyl/releases/tag/v0.2.0-alpha.18),
  [image-backend commit](https://github.com/jmagly/carbonyl/commit/026485b),
  [terminal image source](https://github.com/jmagly/carbonyl/blob/74e028ee3800b33eb93bd80d996bef7c9852e375/src/output/terminal_image.rs),
  [Sixel source](https://github.com/jmagly/carbonyl/blob/74e028ee3800b33eb93bd80d996bef7c9852e375/src/output/sixel.rs), and
  [bridge](https://github.com/jmagly/carbonyl/blob/74e028ee3800b33eb93bd80d996bef7c9852e375/src/browser/bridge.rs).
- Carboxyl: [Servo branch](https://github.com/carboxyl-rs/carboxyl/tree/servo-dev),
  [release candidate](https://github.com/carboxyl-rs/carboxyl/releases/tag/v0.1.0-servo-rc.1),
  [Servo thread](https://github.com/carboxyl-rs/carboxyl/blob/4cf88581b3e50f109ec504069fdb92e6516f4f31/src/browser/servo/servo_thread.rs),
  [widget](https://github.com/carboxyl-rs/carboxyl/blob/4cf88581b3e50f109ec504069fdb92e6516f4f31/src/output/browser_widget.rs), and
  [session](https://github.com/carboxyl-rs/carboxyl/blob/4cf88581b3e50f109ec504069fdb92e6516f4f31/src/browser/servo/session.rs).
- Casty: [README](https://github.com/sanohiro/casty/blob/v1.2.2/README.md),
  [browser source](https://github.com/sanohiro/casty/blob/v1.2.2/lib/browser.js), and
  [Kitty source](https://github.com/sanohiro/casty/blob/v1.2.2/lib/kitty.js).
- Brow6el: [README](https://codeberg.org/janantos/brow6el/src/commit/771c650e49136de9dd2c4a686d521191bc2e7edd/README.md),
  [Sixel renderer](https://codeberg.org/janantos/brow6el/src/commit/771c650e49136de9dd2c4a686d521191bc2e7edd/src/sixel_renderer.cpp), and
  [Kitty renderer](https://codeberg.org/janantos/brow6el/src/commit/771c650e49136de9dd2c4a686d521191bc2e7edd/src/kitty_renderer.cpp).
- Browsh: [README](https://github.com/browsh-org/browsh/blob/499ef386d45cd1e2b5457dd04887c017f77b7e27/README.md),
  [configuration](https://github.com/browsh-org/browsh/blob/499ef386d45cd1e2b5457dd04887c017f77b7e27/interfacer/src/browsh/config_sample.go#L55-L60),
  [graphics builder](https://github.com/browsh-org/browsh/blob/499ef386d45cd1e2b5457dd04887c017f77b7e27/webext/src/dom/graphics_builder.js), and
  [frame builder](https://github.com/browsh-org/browsh/blob/499ef386d45cd1e2b5457dd04887c017f77b7e27/interfacer/src/browsh/frame_builder.go).
- Third-party context: [xkyle.com console browser comparison](https://xkyle.com/Modern-Text-Console-Browsers-in-2023/).
