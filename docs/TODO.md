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
- [x] Defer production Sixel dirty-tile emission pending validated Chrome/CDP damage
  metadata. The user-visible hypothesis is
  that sparse updates, such as a caret, typing, or a small animation, can reduce Sixel
  encode time, terminal bytes, and redraw latency. Scroll, video, WebGL, and other
  full-motion pages are expected to fall back to monolithic frames. This is a
  hypothesis, not a performance promise: brow6el publishes no benchmark evidence for
  its tiled mode. Production partial emission requires validated per-frame Chrome/CDP
  damage metadata and the real-terminal gate below. Without it, Nyala keeps full-frame
  Sixel output; decoded full-frame comparison is not the production fallback.

  - **Production prerequisite.** `Page.screencastFrame` in stock Chrome has no damage
    field. A usable contract must attach the rectangle atomically to the exact encoded
    screencast image, use encoded-image pixel coordinates, and conservatively cover all
    changes since the previous captured frame. It must define full-frame and
    absent/unknown semantics, carry a monotonic capture counter, and accumulate damage
    across browser-rate-limited, skipped, and Nyala-dropped frames. A sequence gap,
    first frame, size, scale, source or session change, lifecycle or navigation
    transition, capture restart, resize, backend switch, recovery, or terminal write
    failure forces a full frame and resets lineage. These signals never suppress a
    delivered frame. Add a measured periodic full refresh only as protection against
    stale terminal state.
  - **Offline diff oracle and benchmark.** Reclassify Nyala-side exact comparison as an
    offline oracle for validating browser damage and measuring the opportunity. Replay
    recorded decoded frames, time exact comparison separately, select and encode
    changed tiles, record output bytes, reconstruct them against the monolithic result,
    and derive the changed-area break-even. Compare the frame last accepted by the
    replay, not every recorded intermediate frame. Align tile edges horizontally to
    terminal cells and vertically to both terminal cells and six-pixel Sixel bands;
    start near 250 px as a tunable benchmark parameter. This spike must not alter the
    live pipeline or solve writer commit semantics.
  - **Comparison cost bounds, not measurements.** One retained 2560x1600 RGBA frame is
    16.38 MB. Comparing current and retained pixels reads up to 32.77 MB per frame, an
    arithmetic upper bound of 1.97 GB/s at 60 FPS. At 1920x1080 the read is 16.59 MB per
    frame, or 1.00 GB/s at 60 FPS. These figures establish the need to measure exact
    comparison separately; they do not show that scanning is slow. Sparse updates have
    theoretical room to repay a scan, while full-motion frames have none. Any
    area-linear saving derived from changed area is an optimistic bound until full
    encode, write, and terminal behaviour are measured.
  - **Why metadata gates production.** A client diff retains a large heap object and
    adds GC pressure; copying it costs bandwidth, while borrowing it complicates image
    lifetime and ownership. Hash-only tracking adds collision risk, and exact tiles
    still carry palette, seam, stale-pixel, replacement, and alignment complexity. The
    comparison base must be the last successfully written frame. Nyala's latest-wins
    admission and encode/write overlap therefore need transactional commit and lineage,
    damage union across dropped frames, and recovery after a write failure. Browser
    metadata avoids the full client scan, but production integration must still solve
    that output lineage and recovery contract.
  - **Rejected stock-CDP damage sources.** Do not use `LayerTree.layerPainted`, tracing
    or invalidation events, DOM observers, `HeadlessExperimental.beginFrame`, or
    clipped screenshots as a production damage source. They have incomplete
    compositor coverage, no reliable correlation with each screencast frame, false
    negatives, or no damage rectangle; a clipped screenshot still needs another
    complete source to choose its clip. They may remain diagnostics, never a gate that
    narrows or suppresses output.
  - **Chrome 150 evidence.** In the five-second probe, CSS transform produced 145
    screencast frames, 144 changed frames, and no `layerPainted` event; CSS paint
    animation produced 143/143/0, scroll 107/107/0, and canvas 94/5/0 respectively.
    Five seconds of tracing produced 183 frames and 46,067 events in 8,549,421 JSON
    bytes, about 1.71 MB/s before parsing. It contained only six `Paint` events, while
    compositor events carried no usable damage. These measurements reject LayerTree
    and tracing as completeness gates, not as diagnostic tools.
  - **Browser-assisted metadata spike.** After the offline oracle exists, prototype the
    browser contract. Chromium already computes conservative Viz
    damage, accumulates damage across rate-limited or skipped captures, maps it through
    scaling and letterboxing, and stores it in
    `VideoFrameMetadata::capture_update_rect`. Mojo and `DevToolsVideoConsumer`
    preserve it, but `PageHandler` drops it before the public Page event. Prototype an
    optional `damageRect` in `Page.ScreencastFrameMetadata`, copied from that exact
    captured frame. Define it in encoded-image pixel coordinates as a conservative
    bounding rectangle of changes since the prior captured frame; a full-frame rect is
    valid, and absence means unknown or full. Add and evaluate a monotonic capture
    counter for gap detection; do not misuse the screencast `sessionId` acknowledgement
    token as a sequence number. Validate every rectangle against the offline exact-diff
    oracle before it may narrow any work.
  - **Production metadata integration.** A validated rectangle does not remove Chrome's
    full-surface PNG encode, base64 transfer, or Nyala's full PNG decode. It can skip
    comparison outside the rectangle, encoding of unchanged Sixel tiles, and their
    terminal bytes. Carry
    the rectangle atomically with its payload and sequence through `rawFrame`, live
    `seqRawFrame`, decoded `pendingFrame`, coalescing, and `render.Frame`. Union damage
    when coalescing drops frames, and commit the lineage only after the ordered terminal
    write succeeds. Expand it to terminal-cell and six-row Sixel boundaries before tile
    selection. Keep every full-refresh rule from the prerequisite contract.
  - **Conditional patch-image follow-up.** Consider clipped patch bytes only after the
    metadata spike measures the remaining browser encode, transfer, and decode cost.
    A protocol needs patch coordinates and bytes, full-frame dimensions, a monotonic
    counter, a full/reset marker, and defined resize, scale, source, and session reset
    rules. Chromium currently captures and copies the complete surface; its source
    states that partial result selection is not implemented.
  - **Deployment and upstream boundary.** Production Nyala must keep working with the
    user's stock external Chromium. A custom Chromium build is an evidence spike and a
    possible upstream CDP contribution, not a new runtime dependency. If validated
    metadata is unavailable or an upstream proposal is rejected, keep full-frame Sixel
    output.
  - **Task sequence.** Proceed in this order: offline exact-diff oracle and benchmark;
    Chromium/CDP metadata prototype validated against that oracle; positioned Sixel
    validation on real terminals; then production integration. Consider patch-image
    transport only after the metadata path measures the remaining browser encode,
    transfer, and Nyala decode cost.
  - **Keep Nyala's current pipeline.** Retain `sixel.Renderer`'s encoder, scratch, and
    output buffers. Preserve `drainFrames` and the stream session's latest-wins
    admission, two-buffer encode/write overlap, and single writer. Batch all cursor
    moves and tile payloads into the existing single DEC-2026 synchronised update
    and one ordered terminal write. Do not copy brow6el's per-tile allocation and
    dither reconstruction, `fwrite`/`fflush` calls, synchronous CEF paint blocking, or
    direct CEF/BGRA engine design.
  - **Implementation seams.** Keep the full PNG materialisation in
    `internal/engine/frame.go` (`ChromiumEngine.decodeLiveFrame`) and the screencast
    publication path. Build the exact-diff oracle beside the focused Sixel benchmarks,
    using `internal/render/sixel/sixel.go` and `encoder.go` (`sixel.Renderer`,
    `opaqueEncoder`) without entering the live path. A later validated metadata path
    carries its rect and lineage through the engine and `render.Frame`, then puts
    partial encoding in the Sixel renderer. Assemble cursor positioning and every tile
    in `internal/viewer/viewer.go` (`State.RenderStreamFrame`) and
    `internal/viewer/stream.go` (`streamSession.assemble`, `drainFrames`,
    `streamWriter`) without adding terminal writes inside the encoder. Extend the
    existing `fpsMeter` in `internal/viewer/stream.go`, the writer measurements in
    `internal/viewer/stream_writer.go`, `internal/capture/performance.go`, and the
    focused Sixel benchmarks and harness.
  - **Correctness decisions before coding.** Decide from measurements whether tiles
    use independent palettes or one shared frame palette; test colour seams and the
    cost of rebuilding or sharing that palette. Define terminal replacement semantics
    for an opaque tile so blank pixels erase old content rather than leave stale
    pixels. Specify cursor anchoring, transparency, overlapping damage, odd image
    dimensions, partial edge tiles, cell and six-row boundaries, first-frame state,
    and recovery after an encode or write error. A tile sequence must reconstruct the
    same visible RGBA frame as the monolithic path.
  - **Measured baseline.** The review's current 2560x1600 encoder benchmark records a
    29.06 ms median, 754,913 output bytes, about 9.28 MB/op, and 1,310 allocs/op. At the
    same size Nyala's recorded PNG decode median is 10.966 ms and production PNG decode
    plus Sixel encode p90 is 39.888 ms. The recorded 1920x1080 decode-plus-encode p90 is
    25.494 ms. Keep these as measured context, not as evidence that brow6el's approach
    or client scanning will improve Nyala.
  - **Validation.** First validate any browser damage rectangle against decoded
    full-frame differences across CSS transforms, scrolling, canvas, video, WebGL,
    caret updates, filters and child surfaces, source and size changes, and Chromium
    upgrades. A false negative is a correctness failure; browser metadata may narrow
    work only after this gate passes. The oracle must also prove that every changed
    pixel lies inside the conservative browser rectangle. Replay identical recorded
    frame sequences for caret and typing, small animation, scroll, and full-motion
    content through monolithic and tiled arms. Record changed-tile coverage, full-frame
    scan, PNG decode, Sixel encode and terminal write time, terminal bytes, writer calls,
    delivered FPS, frame age, dropped frames, and worst frame gap. Decode and
    reconstruct the tile stream, then
    compare it with the monolithic result. Run real-terminal tests on foot, WezTerm,
    xterm, and mlterm, including palette quality on a photo-heavy fixture and mode 2026
    behaviour. Check for colour seams, tearing, blank tiles, and stale pixels.
  - **Acceptance.** Integrate production partial emission only when validated browser
    metadata and the positioned real-terminal matrix show a clear human-visible
    improvement or a delivered-FPS or p90 improvement, with no correctness defect and
    no material regression on full-motion frames. Choose tile size, refresh cadence,
    and the monolithic changed-area threshold from the measurements. Do not invent a
    numeric gate before that evidence exists. Disable or drop tiled emission if the
    real-terminal matrix does not validate it. Reconsider client exact comparison as a
    production fallback only if the offline spike and the same real-terminal matrix
    demonstrate that gain without a material full-motion regression.

  brow6el is prior art for the geometry and refresh cases, not a design to port:
  [tile sizing and alignment](https://codeberg.org/janantos/brow6el/src/commit/771c650e49136de9dd2c4a686d521191bc2e7edd/src/sixel_renderer.cpp#L31-L61),
  [dirty-tile selection and previous-frame comparison](https://codeberg.org/janantos/brow6el/src/commit/771c650e49136de9dd2c4a686d521191bc2e7edd/src/sixel_renderer.cpp#L74-L167),
  [per-tile allocation, dither, and writes to reject](https://codeberg.org/janantos/brow6el/src/commit/771c650e49136de9dd2c4a686d521191bc2e7edd/src/sixel_renderer.cpp#L241-L301),
  and [CEF dirty rectangles plus forced initial refreshes](https://codeberg.org/janantos/brow6el/src/commit/771c650e49136de9dd2c4a686d521191bc2e7edd/src/browser_client.cpp#L202-L260).
  Chromium sources: [DamageTracker](https://chromium.googlesource.com/chromium/src/+/60a9ea0ae553cc01a8f560433e1d1a0602adde13/cc/trees/damage_tracker.cc#32),
  [SurfaceAggregator](https://chromium.googlesource.com/chromium/src/+/60a9ea0ae553cc01a8f560433e1d1a0602adde13/components/viz/service/display/surface_aggregator.cc#1719),
  [capture and accumulated damage](https://chromium.googlesource.com/chromium/src/+/60a9ea0ae553cc01a8f560433e1d1a0602adde13/components/viz/service/frame_sinks/video_capture/frame_sink_video_capturer_impl.cc#705),
  [full-surface copy limitation](https://chromium.googlesource.com/chromium/src/+/60a9ea0ae553cc01a8f560433e1d1a0602adde13/components/viz/service/frame_sinks/video_capture/frame_sink_video_capturer_impl.cc#1015),
  [Mojo metadata](https://chromium.googlesource.com/chromium/src/+/60a9ea0ae553cc01a8f560433e1d1a0602adde13/services/viz/privileged/mojom/compositing/frame_sink_video_capture.mojom#45),
  [DevToolsVideoConsumer](https://chromium.googlesource.com/chromium/src/+/60a9ea0ae553cc01a8f560433e1d1a0602adde13/content/browser/devtools/devtools_video_consumer.cc#141),
  [PageHandler](https://chromium.googlesource.com/chromium/src/+/60a9ea0ae553cc01a8f560433e1d1a0602adde13/content/browser/devtools/protocol/page_handler.cc#1578),
  [Page protocol](https://chromium.googlesource.com/chromium/src/+/60a9ea0ae553cc01a8f560433e1d1a0602adde13/third_party/blink/public/devtools_protocol/domains/Page.pdl#1163),
  [LayerTree implementation](https://chromium.googlesource.com/chromium/src/+/60a9ea0ae553cc01a8f560433e1d1a0602adde13/third_party/blink/renderer/core/inspector/inspector_layer_tree_agent.cc#285),
  and [Headless handler](https://chromium.googlesource.com/chromium/src/+/60a9ea0ae553cc01a8f560433e1d1a0602adde13/headless/lib/browser/protocol/headless_handler.cc).
  Sources: [TUI decisions](./OVERVIEW.md#decisions-tui-research),
  [Performance damage tracking](./PERFORMANCE.md#theme-5---what-the-reference-projects-teach),
  [Performance recommendation 10](./PERFORMANCE.md#top-recommendations-ranked-by-impact-vs-effort),
  and [production Sixel measurements](./PERFORMANCE.md#sustained-moving-png-versus-q90-jpeg-2026-07-17).

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
- [x] Supersede bounded `jpegn.Options{ToRGBA:true}` for live Sixel JPEG. Its
  historical matched sequential result fell from 115.005 to 103.014 ms (10.43%),
  but Sixel now requests PNG for every frame, so the JPEG-only branch, worker cutoff,
  and tests are removed. Block retains native-YCbCr q90 JPEG. Source:
  [Bounded Sixel RGBA decode](./PERFORMANCE.md#bounded-sixel-rgba-decode-and-four-worker-policy-2026-07-15).
- [x] Reject direct native-YCbCr Sixel palette sampling and mapping. It removes
  full-frame RGBA materialisation but still performs YCbCr-to-RGB maths. It cuts
  transient B/op by 44.39% to 60.49%, but complete median processing regresses
  11.57% at 1920x1080 and 11.39% at 2560x1600. The 1.21% gain at 2560x2880 has
  overlapping process ranges. Sixel's later PNG-only decision removes this JPEG
  path, so do not add a resolution threshold. Source:
  [Direct native-YCbCr Sixel follow-up](./PERFORMANCE.md#direct-native-ycbcr-sixel-follow-up-2026-07-17).
- [x] Compare sustained distinct PNG and q90 JPEG CDP frames through production
  Sixel processing at 1920x1080, 2560x1600, and 2560x2880. PNG cuts local service
  p90 by 15.86%, 10.23%, and 0.69%, and cuts Sixel bytes by 49.93%, 46.16%, and
  38.44%. Its serial delivery-plus-service p90 changes by -2.10%, -3.73%, and
  +1.26%; the first two process ranges overlap and the torture size regresses.
  The owner accepts the 1.26% torture-size serial loss for higher fidelity, 38% to
  50% smaller Sixel output, and one input format. Sixel now uses PNG for every live
  frame. Real-terminal write, parse, draw, FPS, and tearing measurements remain the
  validation caveat. Do not add a resolution threshold. Source:
  [Sustained moving PNG versus q90 JPEG](./PERFORMANCE.md#sustained-moving-png-versus-q90-jpeg-2026-07-17).
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
- [x] Reject a specialised Sixel repeat decimal formatter. It cuts the formatter
  micro-path by 48.30% and isolated encode by 3.16% and 2.59%, but complete median
  gains are only 1.25% and 1.12%; both complete-path process ranges overlap baseline,
  and 1080p p90 regresses 0.48%. Output and allocations remain unchanged. Retain
  `strconv.AppendInt`. A PNG-fed production decode and render recheck also rejects it:
  median gains are 0.10-0.80%, every range overlaps, and two of three p90 values
  regress. Source:
  [Sixel decimal formatter rejection](./PERFORMANCE.md#repeat-decimal-formatter-rejection-2026-07-17).
- [x] Close the other Sixel band research with no code change. Active-bitset,
  transposed, and packed-bank variants did not beat the production-shaped full
  encoder. Parallel indexed-pixel mapping and conditional bounded parallel bands now
  have one canonical ordered backlog in [Sixel optimisation backlog](./SIXEL-TODO.md).
  Source:
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
- [ ] Add a user-facing JPEG q quality scale for Block only. Keep q90 as the default,
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
