# Nyala TODO

This is the canonical index of open work named in the tracked documentation.
Checked entries are closed, rejected, or out of scope. Open entries state their gate
when the work must wait for evidence. Historical detail remains in the linked stage
and research documents.

## 1. Validate the Stage 3 browser

- [ ] Run the sustained real-terminal performance matrix on KMSCON, Kitty, WezTerm,
  and representative Sixel terminals. Record input FPS, output FPS, frame bytes,
  Nyala encode/write time, terminal decode/draw time, and visible tearing. The Kitty
  target is 16.67 ms per frame. The capture harness cannot close this task. Sources:
  [Stage 3 post-build validation](./STAGE-3.md#status-summary),
  [live FPS instrument](./PERFORMANCE.md#live-fps-instrument-done---9002348-cc7878a-8a87773), and
  [Overview risks](./OVERVIEW.md#known-constraints--risks).
- [ ] Measure `scrollMatchTolerancePx`, the 250 ms overlay hold, and advisory DOM-box
  drift during live scroll and reflow. Tune only from recorded skew and visible
  placement faults. Sources: `STAGE-3.md`, "Frame-sync tolerance and overlay
  staleness", and `AGENTS.md`, project status.
- [x] Run the full `pprof` and five-sample `go test -bench` pass. Completed on
  2026-07-14; the measured profiles closed the RGBA ring and residual Compose split.
  The bounded sampler follow-up completed on 2026-07-15. Sources:
  [Full headless profile](./PERFORMANCE.md#full-headless-profile-and-benchmark-pass-2026-07-14)
  and [direct native-YCbCr Block grid](./PERFORMANCE.md#direct-native-ycbcr-block-grid-2026-07-15).
- [x] Arm64 SIMD validation is not required. The upstream projects publish their
  SIMD implementation details, so Nyala will not duplicate that validation. Source:
  [Decode optimisation](./PERFORMANCE.md#decode-optimization).
- [ ] Re-test Kitty cursor anchoring, classic placement, Unicode placeholder support,
  repeated replacement, and image cleanup on the exact Kitty, WezTerm, and Ghostty
  releases supported by Nyala. Source:
  [TUI decisions](./OVERVIEW.md#decisions-tui-research).
- [ ] Measure Sixel full-viewport FPS, bytes per frame, palette quality on a
  photo-heavy fixture, and mode-2026 tearing on foot, WezTerm, xterm, and mlterm.
  Source: [TUI decisions](./OVERVIEW.md#decisions-tui-research).

CEF is not required because CDP performance has already exceeded the owner's
expectations. Do not reopen CEF for a limit in Nyala's renderer or the terminal.

## 2. Improve graphics transport where measurements justify it

- [x] Retain the ratio-specialised Block scalar fast path. The 4:2:0 chroma path and
  common eight-pixel luma span are specialised, with the generic fallback preserved.
  Fifteen interleaved q90 pairs improve median end-to-end time from 10.202621 to
  10.022665 ms (1.76%, p < 0.001), while p90, p95, and maximum latency also improve.
  Ten saturated pairs improve median completed-grid interval by 15.56% and throughput
  by 18.43%. Exactness, race, allocation, build, test, and lint gates pass with the
  fixed 16-worker benchmark pool. Source:
  [Colour conversion research](./PERFORMANCE.md#colour-conversion-research-2026-07-15).
- [x] Adopt the retained encoder derived from `mattn/go-sixel` v0.0.12 with an adaptive
  256-colour palette and dithering off. The matched q90 State median falls from
  960.437 to 101.670 ms (89.41%, 9.45x), allocations fall 99.993%, and payload
  falls 18.47%. The five-image quality set chooses 256 over 192 colours; the owner
  accepts the measured colour loss for this compatibility backend. The earlier
  run did not test fixed-palette build cost or stale-frame quality. The later public
  sweep completed; the stateless sample-cap result supersedes temporal reuse and is
  retained after the rare-colour phase gate. Nyala now owns the minimum opaque
  encoder subset. Removing alpha work cuts the 2560x1600 mean from 67.119 to
  63.190 ms (5.853%) with byte-identical output; palette selection and RLE remain
  unchanged. Parallel construction of the inherited 15-bit palette LUT then cuts
  the ten-cell matrix mean full encode by 5.54% and combined q90 decode plus encode
  by 6.21%, with byte-identical payloads across all fixtures.
  Source: [Sixel encoder adoption](./PERFORMANCE.md#sixel-encoder-adoption-2026-07-15).
- [x] Retain bounded `jpegn.Options{ToRGBA:true}` for live moving Sixel JPEG.
  The adaptive default and Sixel RGBA cutoff are separate policies, both currently
  four. The manual 1 to 16 override remains, with values above four keeping native
  YCbCr. Matched sequential processing falls
  from 115.005 to 103.014 ms (10.43%). Quality stays stable and the retained live
  plus State bound rises 1.1%. Block, Kitty, capture, settled Sixel PNG, and backend
  switches preserve their paths. Source:
  [Bounded Sixel RGBA decode](./PERFORMANCE.md#bounded-sixel-rgba-decode-and-four-worker-policy-2026-07-15).
- [x] Reduce the inherited per-frame median-cut sample cap from `2^18` to `2^16`.
  The internal ten-cell still-image matrix cuts mean full encode by 34.71% and
  combined q90 decode plus encode by 30.39% against the prior parallel-LUT
  production path, but changes every
  non-trivial payload, raises mean payload 1.75%, and has mixed per-fixture quality.
  A deterministic phase sweep records repeatable rare-colour category changes on
  thin lines, badges, blocks, icons, and antialias shades at both target sizes, but
  all 800 features remain above 3:1 and preserve structure. Accept the quality
  trade-off. Reopen only if content disappears, merges with its background, becomes
  unreadable, or loses structure. The exact-colour path remains byte-identical. The
  former 32-frame motion and temporal palette-reuse task is not required.
- [x] Benchmark Sixel repeat ceilings 255, 512, 1024, 2048, 4096, 8192, and 32767.
  Six fresh rotated processes show overlapping complete-path timing ranges, unchanged
  allocations, and deterministic payload reductions of 17.17% at 1920x1080 and
  21.28% at 2560x1600. Runs stop at the frame width. Ceilings from 2048 at 1920 and
  from 4096 at 2560 collapse to byte-identical groups. `mattn/go-sixel` decodes every
  distinct output to identical pixels. Charm x/ansi v0.11.7 is a known-negative
  oracle because its token-boundary defect affects any DECGRI, including 255.
  Source: [Sixel repeat-ceiling benchmark](./PERFORMANCE.md#repeat-ceiling-benchmark-2026-07-16).
- [x] Raise the Sixel repeat ceiling from 255 to 8192. Focused tests cover literals,
  the 255 and 8192 boundaries, multi-chunk counts, mandatory graphics bytes, band and
  colour delimiters, odd heights, the maximum emitted count, warm allocations, and
  pixel equality against the 255-split output through `mattn/go-sixel`. The 8192
  ceiling supports wider displays while staying below tmux's native 10000 cap. It is
  byte-identical to 4096 at both measured frame widths. Charm x/ansi v0.11.7 is no
  longer a Sixel acceptance oracle. Source:
  [Sixel repeat-ceiling benchmark](./PERFORMANCE.md#repeat-ceiling-benchmark-2026-07-16).
- [ ] Send accepted palette or RLE patches upstream. Use a temporary published fork
  plus versioned replacement only for short-lived integration. Do not vendor.
- [x] Retain one rightmost-nonblank bound per present Sixel colour plane. Against
  commit `18bba495`, six fresh rotated process pairs cut complete median encode time
  by 8.85% at 1920x1080 and 12.19% at 2560x1600, with both process-median ranges
  separated. The isolated path falls 21.38% and 21.71%. It skips 38.7663% and
  38.0022% of plane columns with identical payloads, hashes, decoded pixels, repeat
  maxima, and allocation counts. Source:
  [Sixel rightmost bound](./PERFORMANCE.md#rightmost-nonblank-plane-bound-2026-07-17).
- [x] Close the other Sixel band research with no code change. Active-bitset,
  transposed, and packed-bank variants did not beat the production-shaped full
  encoder. Defer parallel bands. Source:
  [Sixel hot-path research](./PERFORMANCE.md#sixel-hot-path-research-and-decisions-2026-07-15).
- [ ] Add graphics support through tmux, Zellij, and herdr. Keep the present block-only
  fallback until each path passes capability, placement, resize, scroll, and cleanup
  tests. Kitty needs protocol-supported placement plus passthrough wrapping. Its
  placement choice depends on the terminal-validation task in section 1, so do not
  assume `U=1` before that result. Sixel needs its own passthrough and anchoring checks.
  Re-test against a tagged tmux release with native Kitty image support before choosing
  the final Kitty path. Sources:
  `STAGE-1.md`, "Focus multiplexers", and
  [TUI decisions](./OVERVIEW.md#decisions-tui-research).
- [ ] Add partial Kitty emission only if the terminal matrix shows full-frame decode,
  draw, write, or byte cost over budget. Design from the direct CDP PNG wire format.
  Do not revive the obsolete decoded-full-frame plus Kitty `a=f` plan without a
  supported protocol path and an end-to-end win. Sixel partial emission belongs to
  the canonical Sixel task above. Sources: [Stage 3 deferred scope](./STAGE-3.md#objective-and-scope)
  and [Performance recommendation 10](./PERFORMANCE.md#top-recommendations-ranked-by-impact-vs-effort).
- [x] Do not add a JPEG Kitty fallback. The owner's measurement put
  direct PNG pass-through at about 1 ms, so a JPEG fallback has no current value.
  Reconsider it only if terminal-side PNG decoding or drawing becomes a measured
  bottleneck. Sources:
  [Performance Theme 7](./PERFORMANCE.md#theme-7---screencast-image-format) and
  `STAGE-3.md`, "Terminal PNG cost and graphics follow-ups".
- [ ] Build the off-screen super-viewport scroll buffer after the transport work. Gate
  it on fewer CDP round trips and smoother scrolling without stale overlay geometry or
  excess memory. Source: [Performance recommendation 12](./PERFORMANCE.md#top-recommendations-ranked-by-impact-vs-effort).
- [x] Keep the dimension-keyed RGBA decode ring closed as stale. The 2026-07-14 q80
  allocation profile shows `image.NewRGBA` at 2.86 percent of allocation space, and
  the clean CPU profile puts GC background marking at 2.00 percent cumulative. The
  direct native-YCbCr Block path has now removed the remaining 320x270 RGBA
  intermediate as well. Sources:
  [Full headless profile](./PERFORMANCE.md#full-headless-profile-and-benchmark-pass-2026-07-14)
  and [direct native-YCbCr Block grid](./PERFORMANCE.md#direct-native-ycbcr-block-grid-2026-07-15).
- [ ] Revisit the Block sub-cell screencast resolution only on an owner decision. It is
  on hold because it removes the full-resolution benchmark that exposes renderer and
  terminal differences. Source:
  [Block performance recommendation 1](./PERFORMANCE.md#ranked-recommendations-impact-vs-effort).
- [x] Replace `averageYCbCrToRGBA` with direct native-YCbCr Block composition. The
  sampler precomputes source and chroma mappings, groups repeated chroma samples with
  exact luma weights, and writes the final grid without an RGBA intermediate. The
  final q80 matched median falls from 16.725 ms to 8.889 ms, while 16-worker saturated
  throughput rises from 718.74 to 795.58 completed grids/s. Independent oracle and
  transition tests preserve exact output. Source:
  [Direct native-YCbCr Block grid](./PERFORMANCE.md#direct-native-ycbcr-block-grid-2026-07-15).
- [x] Keep the residual Compose split closed. Production parallel Compose measures
  0.054 ms at 120x40 and 0.138 ms at 240x86 with zero allocations, so it is neither
  dominant nor a material share of 16.67 ms. Source:
  [Full headless profile](./PERFORMANCE.md#full-headless-profile-and-benchmark-pass-2026-07-14).

## 3. Build Stage 4 UI

- [ ] Add Bubble Tea v2 chrome with an address bar, status line, load state, and a
  renderer-blank viewport. Send Block, Sixel, and Kitty viewport bytes out of band.
  Re-blit after repaint or resize, and do not let chrome erase or move the bitmap.
  Sources: [Stage 4 architecture](./OVERVIEW.md#tui-library-research) and
  [TUI decisions](./OVERVIEW.md#decisions-tui-research).
- [ ] Add a user-facing q quality scale for Block and Sixel. Keep q90 as the default,
  while allowing users on lower-spec hardware to reduce quality to reach acceptable
  performance. Validate the choices with real-terminal and delivered-frame measurements
  across flat, text, photo, and motion pages. Source:
  [Direct native-YCbCr Block grid](./PERFORMANCE.md#direct-native-ycbcr-block-grid-2026-07-15).
- [ ] Validate Stage 4 scheduling on KMSCON and graphics terminals. Chrome-only diffs
  plus any post-repaint viewport re-blit must finish within one frame budget. Test
  resize, alt-screen behaviour, cursor restoration, and concurrent frame arrival.
  Source: [TUI decisions](./OVERVIEW.md#decisions-tui-research).
- [ ] Add direct interaction: address entry, keyboard input, mouse clicks, buttons,
  links, and focus mapped to CDP events. Scrolling already uses the Stage 3 engine
  transport. Source: `OVERVIEW.md`, "Input mapping".
- [ ] Add clickable Back, Forward, and Reload controls to the Stage 4 chrome. Back
  and Forward use Chromium's existing navigation state; Nyala does not own or persist
  a navigation-history model. Bookmarks and other full-browser features remain out of
  scope. Sources: `OVERVIEW.md`, "v1 interaction scope", and [Stage 3 deferred
  scope](./STAGE-3.md#objective-and-scope).
- [ ] Add chrome theming, OSC 11 light/dark handling, per-multiplexer background
  handling, typed capability messages, and terminal-colour downsampling. Use the
  documented bubbletint choice after chrome exists. Sources: `STAGE-1.md`, "Out of
  scope", and [bubbletint](./OVERVIEW.md#bubbletint-theming).
- [ ] Add a live block-mode control through `Engine.SetScheme`. Show an in-app status
  message when a requested mode falls back, such as sextant to quadrant. Keep it
  inside the Stage 4 status line, not an OSC desktop notification. Source:
  [Block mode follow-ups](./BLOCK-MODES.md#open-follow-ups).
- [ ] Match the page's `prefers-color-scheme` to the active chrome tint after theming
  lands. Source: [bubbletint](./OVERVIEW.md#bubbletint-theming).
- [ ] Add tabs after the single-page v1 UI. Use child targets without changing the
  allocator model. Sources: `OVERVIEW.md`, "v1 interaction scope", and
  [Stage 3 deferred scope](./STAGE-3.md#objective-and-scope).

## 4. Close Block correctness gaps

- [ ] Restore an end-to-end overlay-to-bitmap alignment gate that works while page
  text is hidden from the Block bitmap. Measure a known non-text feature or provide a
  metric-only text-bearing raster. The DOM-box placement gate cannot catch a shared
  scale error. Source: `STAGE-3.md`, "Monospace block render".
- [x] Fix `layout-04-multicolumn-justified` wrong-column mapping and remove its
  collision allowance. The walk now maps word and code-unit rects to whole-node
  visual fragment identities, so separate columns can share a snapped row without
  joining. All three corpus legs record zero collisions and pass the default placement
  gate. Paint displacement is not green: separate layers remove source glyph ink, so
  the metric skips every run and is not applicable. The preceding alignment-gate item
  tracks the replacement.
  Sources: `STAGE-3.md`, "Corpus placement gate", and the fixture's `fixture.json`.
- [ ] Make the `layout-03-cjk-vertical-ruby` wide leg settle deterministically. Wait
  for layout stability across snapshots, then tighten the collision threshold from 8
  towards the isolated value near 2. Source: the fixture's `fixture.json`.
- [ ] Add paint-order z-ordering for overlapping text runs. The current
  first-writer-wins guard prevents welded text but does not select the front-most box.
  Source: `STAGE-3.md`, "Overlay guard and background recompose".
- [ ] Improve shaped-script placement and joining where corpus evidence still exceeds
  the default gates, including Arabic advances and CJK vertical/ruby runs. Tighten each
  fixture override only after its render fault is fixed. Sources: `STAGE-3.md`,
  "Corpus placement gate", and `OVERVIEW.md`, shaped-script research.
- [ ] Widen the sextant allow-list only after a repeatable real-terminal test card
  shows no inter-row bleed. Record terminal name and version, and add version gates
  where one release is bad. Source:
  [Block mode follow-ups](./BLOCK-MODES.md#open-follow-ups).
- [ ] Add REP to the Block diff writer only after measuring byte savings and verifying
  KMSCON, foot, tmux, Zellij, and herdr. The cell diff writer itself is complete.
  Sources: `STAGE-2.md`, "Out of scope", and `OVERVIEW.md`, REP research.
- [ ] Add the character-density and text-only monochrome variants as optional modes.
  Preserve the current `ColourMono` silhouette as the default monochrome path. Source:
  `STAGE-1.md`, "Monochrome mode".
- [ ] Promote luma background nudging, nearest-of-two colour conversion, or regional
  dithering only after the documented harness A/B shows a gain on its target fixture.
  Source: `STAGE-2.md`, "Out of scope, deferred".

## 5. Conditional and later work

- [x] A CEF engine spike is not required because CDP performance has already exceeded
  the owner's expectations. Sources:
  [Overview risks](./OVERVIEW.md#known-constraints--risks) and
  [Stage 3 deferred scope](./STAGE-3.md#objective-and-scope).
- [ ] Add remote Chromium attach only when a locked-down or shared host needs it.
  Document which launch-only viewport and device-scale controls cannot be applied.
  Source: [Stage 3 deferred scope](./STAGE-3.md#objective-and-scope).
- [x] A/V synchronisation needs no Nyala-side delay. The owner manually tested several
  media sources in Block and Kitty modes and found synchronisation acceptable. Audio
  playback itself is complete. Source: `OVERVIEW.md`, "Audio".
- [ ] Add Harmonica or equivalent chrome animation only after the Stage 4 UI and
  scrolling are stable. Source: [Harmonica](./OVERVIEW.md#harmonica-animation).
- [ ] Re-evaluate Go's `archsimd` base64 decoder when it is enabled by default and has
  an arm64 implementation. Keep the cgo-free cross-build and byte-identity gates.
  Source: `PERFORMANCE.md`, "Option 1 - base64 decode".

## Closed or stale roadmap entries

- [x] Pass live Kitty CDP PNG frames through without base64 decode, PNG decode, pixel
  packing, zlib, or base64 re-encode. `Frame.EncodedPNG` and `kitty.LiveEncoder` now
  provide the direct path. Sources: `README.md`, `OVERVIEW.md`, and `STAGE-3.md`.
- [x] Remove graphics letterboxing with identity fit. Kitty and Sixel now use the full
  engine-sized grid from the top-left; tests cover non-1:2 terminal cells. Source:
  [Performance recommendation 2](./PERFORMANCE.md#top-recommendations-ranked-by-impact-vs-effort).
- [x] Retire normal-path decoded-pixel Kitty encoding as an optimisation target. The
  decoded encoder remains only for fallback and tests.
- [x] Retire decoded-bitmap diff plus Kitty `a=f` as the current damage plan. Direct
  PNG has no decoded bitmap, and `a=f` support was not reliable across target
  terminals. The conditional byte and latency goal remains in section 2.
- [x] Keep graphics downscaling out of the active roadmap. Reopen it only by an owner
  decision; it is not a pending task in this index.
- [x] Keep content-hash and image-pointer grid memoisation closed. The hit rate is near
  zero and the stale-frame risk outweighs the measured saving. Source:
  [Block performance recommendation 7](./PERFORMANCE.md#ranked-recommendations-impact-vs-effort).
- [x] Keep the DCS/OSC truecolour round trip dropped. Environment and terminfo
  detection is the accepted path. Source: `STAGE-1.md`, "Colour detection".
