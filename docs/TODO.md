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
- [ ] Validate the adopted SIMD decode paths on real arm64 hardware. Confirm that the
  base64 and `jpegn` NEON paths improve performance, and rerun byte-identity tests
  before treating the amd64 results as portable evidence. Source:
  [Decode optimisation](./PERFORMANCE.md#decode-optimization).
- [ ] Re-test Kitty cursor anchoring, classic placement, Unicode placeholder support,
  repeated replacement, and image cleanup on the exact Kitty, WezTerm, and Ghostty
  releases supported by Nyala. Source:
  [TUI decisions](./OVERVIEW.md#decisions-tui-research).
- [ ] Measure Sixel full-viewport FPS, bytes per frame, palette quality on a
  photo-heavy fixture, and mode-2026 tearing on foot, WezTerm, xterm, and mlterm.
  Source: [TUI decisions](./OVERVIEW.md#decisions-tui-research).

The CEF decision depends on the first checkbox. Do not reopen CEF for a limit in
Nyala's renderer or the terminal.

## 2. Improve graphics transport where measurements justify it

- [ ] Remove the Sixel quantisation wall. Persist the encoder and palette, buffer the
  writer, compare the documented faster quantisers or encoder, then add dirty-band
  emission if the terminal matrix shows a gain. Gate quality with the photo-heavy A/B.
  Sources: [Performance recommendations](./PERFORMANCE.md#top-recommendations-ranked-by-impact-vs-effort),
  recommendations 6 and 8, and `OVERVIEW.md`, "Sixel in Go".
- [ ] Tighten Sixel output after palette reuse: use aggressive RLE, omit empty bands,
  set `P2=0` for opaque frames, and keep dither off. Require smaller output, lower
  terminal decode time, protocol-conformant output, and no fixture regression. Source:
  `PERFORMANCE.md`, "Sixel/Kitty library verdict".
- [ ] Parallelise Sixel band encoding only after palette caching and tighter output
  land, and only if a benchmark with per-worker buffers beats the serial encoder on
  target frame sizes. Source: `PERFORMANCE.md`, "Sixel/Kitty library verdict".
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
- [ ] Add a JPEG Kitty fallback only if a real-terminal end-to-end benchmark beats
  direct PNG after image quality, terminal decode/draw latency, and bytes per frame
  are included. Nyala's 0.014 to 0.027 ms PNG framing cost is not the gate. Sources:
  [Performance Theme 7](./PERFORMANCE.md#theme-7---screencast-image-format) and
  `STAGE-3.md`, "Terminal PNG cost and graphics follow-ups".
- [ ] Build the off-screen super-viewport scroll buffer after the transport work. Gate
  it on fewer CDP round trips and smoother scrolling without stale overlay geometry or
  excess memory. Source: [Performance recommendation 12](./PERFORMANCE.md#top-recommendations-ranked-by-impact-vs-effort).
- [ ] Validate JPEG q95 on real terminals and delivered-frame measurements as a later
  quality option. q90 is now the default because it improves fidelity over q80,
  matches moving Sixel, and retains 6.39 ms median processing headroom in the matched
  2560x2880 headless benchmark. It provides more headroom for higher resolutions than
  q95, whose worst-sample processing headroom is 4.56 ms. q100 is rejected: its payload
  grows 59.76 percent over q95 and its worst processing headroom is only 0.48 ms.
  Include flat, text, photo, and motion pages in the terminal pass. Source:
  [Direct native-YCbCr Block grid](./PERFORMANCE.md#direct-native-ycbcr-block-grid-2026-07-15).
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
- [ ] Validate Stage 4 scheduling on KMSCON and graphics terminals. Chrome-only diffs
  plus any post-repaint viewport re-blit must finish within one frame budget. Test
  resize, alt-screen behaviour, cursor restoration, and concurrent frame arrival.
  Source: [TUI decisions](./OVERVIEW.md#decisions-tui-research).
- [ ] Add direct interaction: address entry, keyboard input, mouse clicks, buttons,
  links, and focus mapped to CDP events. Scrolling already uses the Stage 3 engine
  transport. Source: `OVERVIEW.md`, "Input mapping".
- [ ] Add back/forward history and Vimium-style link hints. Add a separate text or
  geometry channel only when link hints need a cadence independent of rendered
  frames. Sources: `OVERVIEW.md`, "v1 interaction scope", and
  [Stage 3 deferred scope](./STAGE-3.md#objective-and-scope).
- [ ] Decide the graphics-backend overlay contract during the link-hint work. Block
  keeps the crisp DOM overlay; Sixel and Kitty currently render the page bitmap alone.
  Do not burn crisp text back into graphics pixels. Sources: `STAGE-2.md`, "Out of
  scope", and `STAGE-3.md`, "Out of scope, deferred".
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
- [ ] Fix `layout-04-multicolumn-justified` wrong-column mapping, remove its expected
  collision allowance, and turn the paint-displacement expected failure green.
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

- [ ] Open a CEF engine spike only if the real-terminal Stage 3 matrix shows CDP itself
  cannot sustain the accepted frame rate. Account for cgo, bundled Chromium, BGRA
  conversion, and loss of CDP text geometry. Sources:
  [Overview risks](./OVERVIEW.md#known-constraints--risks) and
  [Stage 3 deferred scope](./STAGE-3.md#objective-and-scope).
- [ ] Add remote Chromium attach only when a locked-down or shared host needs it.
  Document which launch-only viewport and device-scale controls cannot be applied.
  Source: [Stage 3 deferred scope](./STAGE-3.md#objective-and-scope).
- [ ] Add A/V synchronisation only if real media use shows audio leading the terminal
  video path. Audio playback itself is complete. Source: `OVERVIEW.md`, "Audio".
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
