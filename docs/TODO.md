# Nyala TODO

This is the canonical index of open work named in the tracked documentation.
Checked entries are closed, rejected, or out of scope. Open entries state their gate
when the work must wait for evidence. Historical detail remains in the linked stage
and research documents.

## 1. Validate the Stage 3 browser

- [ ] Run the sustained real-terminal performance matrix on KMSCON, Kitty, and
  WezTerm. Record input FPS, output FPS, frame bytes,
  Nyala encode/write time, terminal decode/draw time, and visible tearing. The Kitty
  target is 16.67 ms per frame. `just sixel-interop` is the accepted terminal-testing
  proxy for Sixel, so no physical Sixel terminal run remains. Sources:
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
- [x] Keep damage tracking, dirty-tile output, and partial Sixel updates out of
  scope. This is an explicit owner decision, not deferred work. Do not add an
  offline damage oracle, Chrome/CDP damage prototype, client-side frame diff, or
  partial-update transport. Reopen only by an explicit owner decision.
- [x] Handle mid-session terminal zoom (font-size change). The viewer re-derives
  the cell pixel pair on SIGWINCH and lands it on the engine before the resize
  restart. Headless Chromium paints at the launch device scale factor only, so the
  viewer scales graphics to the cell grid (Kitty `c=`/`r=` placement keys, Sixel
  grid upscale) and Block recomposes exactly. Live verified across the reference
  terminals in Block, Kitty, and Sixel, both zoom directions. Commit `9716a2c`.
  The exact-density relaunch option remains open in section 5. Source:
  `PERFORMANCE.md`, "Mid-session zoom re-derives the cell pair".

## 2. Improve graphics transport where measurements justify it

- [x] Confirm that diagnostics capture removal did not regress Block or Kitty.
  Six fresh rotated processes at the three target sizes found overlapping Block
  ranges and unchanged Kitty p90. Block remains allocation-free; Kitty remains at
  10 allocations with unchanged payload and allocation classes. Source:
  [2026-07-18 backend sweep](./PERFORMANCE.md#capture-removal-and-combined-sixel-sweep-2026-07-18).
- [x] Retain the ratio-specialised Block scalar fast path. The 4:2:0 chroma path and
  common eight-pixel luma span are specialised, with the generic fallback preserved.
  Fifteen interleaved q90 pairs improve median end-to-end time from 10.202621 to
  10.022665 ms (1.76%, p < 0.001), while p90, p95, and maximum latency also improve.
  Ten saturated pairs improve median completed-grid interval by 15.56% and throughput
  by 18.43%. Exactness, race, allocation, build, test, and lint gates pass with the
  fixed 16-worker benchmark pool. Source:
  [Colour conversion research](./PERFORMANCE.md#colour-conversion-research-2026-07-15).
- [x] Adopt the deterministic retained encoder derived from `mattn/go-sixel` v0.0.12
  with an adaptive
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
  Commit `e8418bc`, with the owned opaque encoder in `415fbc6` and palette work in
  `cea0684`. Source:
  [Sixel encoder adoption](./PERFORMANCE.md#sixel-encoder-adoption-2026-07-15).
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
  longer a Sixel acceptance oracle. Commit `68ef64c`. Source:
  [Sixel repeat-ceiling benchmark](./PERFORMANCE.md#repeat-ceiling-benchmark-2026-07-16).
- [ ] Send accepted palette or RLE patches upstream. Use a temporary published fork
  plus versioned replacement only for short-lived integration. Do not vendor. This
  contribution task does not reopen Nyala's encoder optimisation.
- [x] Retain one rightmost-nonblank bound per present Sixel colour plane. Against
  commit `18bba495`, six fresh rotated process pairs cut complete median encode time
  by 8.85% at 1920x1080 and 12.19% at 2560x1600, with both process-median ranges
  separated. The isolated path falls 21.38% and 21.71%. It skips 38.7663% and
  38.0022% of plane columns with identical payloads, hashes, decoded pixels, repeat
  maxima, and allocation counts. Commit `c02fd7f`. Source:
  [Sixel rightmost bound](./PERFORMANCE.md#rightmost-nonblank-plane-bound-2026-07-17).
- [x] Reject a specialised Sixel repeat decimal formatter. It cuts the formatter
  micro-path by 48.30% and isolated encode by 3.16% and 2.59%, but complete median
  gains are only 1.25% and 1.12%; both complete-path process ranges overlap baseline,
  and 1080p p90 regresses 0.48%. Output and allocations remain unchanged. Retain
  `strconv.AppendInt`. A PNG-fed production decode and render recheck also rejects it:
  median gains are 0.10-0.80%, every range overlaps, and two of three p90 values
  regress. Source:
  [Sixel decimal formatter rejection](./PERFORMANCE.md#repeat-decimal-formatter-rejection-2026-07-17).
- [x] Retain bounded parallel six-row band emission (`7148f61`) and RGB555 indexed
  mapping for images of at least 500,000 pixels (`249e8c3`). Both use
  `sixelWorkerCount() = min(4, GOMAXPROCS)` in sequence with LUT construction. They
  never overlap, nest pools, add a second budget, or change runtime configuration.
  Mapping keeps the exact-colour scan serial. Band workers own scratch, join complete
  bands in source order, and leave a partial final band serial. Four-arm validation
  (`9b5b9a3`) compared serial, mapping-only, bands-only, and combined paths across
  flat, web, and photo fixtures at 1280x720, 1920x1080, and 2560x1600. The combined
  path cut full-encode p90 by 15.78% to 50.19% and PNG-decode-plus-encode p90 by
  8.88% to 29.81%, with identical payloads, hashes, and decoded pixels. The final
  production-shaped sweep cut full-encode p90 by 28.22% to 49.63% and complete
  PNG-decode-plus-encode p90 by 17.90% to 25.22% at the three target sizes. Payloads
  and decoded pixels remain identical; the two extra allocations and less than 0.02%
  B/op increase are accepted. `just sixel-interop` passed. Source:
  [2026-07-18 backend sweep](./PERFORMANCE.md#capture-removal-and-combined-sixel-sweep-2026-07-18).
- [x] Keep `just sixel-interop` as the Sixel terminal-testing proxy. Commit `04d1703`
  sends complete production DCS streams through both go-sixel and libsixel, then
  checks framing, dimensions, bounds, pixels, and adaptive-image quality. Exact
  primary colours must match. Adaptive colour-register expansion may differ by at
  most two levels per RGB channel. The focused flake check and `just check` run this
  gate; libsixel remains a development and test dependency, not a runtime dependency.
- [x] Omit the redundant `$` after each inter-band `-` (`5ae5ff3`). Sixel `-` already
  returns to the left edge of the next six-row band. The change saves one byte per
  boundary, preserves later colour-plane carriage returns, and reconstructs pixels
  identical to the former stream through both accepted decoders.
- [x] Close Nyala's Sixel encoder optimisation. Preserve RGBA input, the opaque
  dither-free adaptive palette, the 64K sample cap, RGB555 lookup, exact-colour fast
  path, palette order, byte-plane layout, right-edge bounds, 8192 repeat ceiling,
  DCS framing, and ordered single write. Any future candidate must use six fresh processes
  with rotated arm order, four warm-ups, at least 30 retained samples per arm,
  `GOMAXPROCS=4`, and `GOGC=100`. Report stage spans, full-encode and complete
  PNG-decode-plus-encode p90, B/op, allocs/op, payload bytes and SHA-256, and decoded
  pixel equality outside timed regions. Reopen only for a reproducible production
  regression, an independent interop failure, a new platform constraint, or an
  explicit owner decision. Sources:
  [Sixel hot-path research](./PERFORMANCE.md#sixel-hot-path-research-and-decisions-2026-07-15)
  and [option assessment](./PERFORMANCE.md#option-assessment).
- [x] Keep rejected Sixel experiments closed. Active-bitset, transposed, packed-bank,
  and other private band layouts did not beat the production encoder. Do not revive
  PNN, PCA, hash-based palette transplants, lower sampling budgets, fixed or temporal
  palettes, 64-column banks, SWAR rewrites, dithering, transparency work, cgo/native
  integration, wholesale emitter replacement, the 255 repeat ceiling, or a specialised
  decimal formatter without the reopening evidence above.
- [ ] Add graphics support through tmux, Zellij, and herdr. The Sixel leg is
  implemented pending live validation: tmux and zellij run a reduced handshake
  (no Kitty APC probe); tmux offers Sixel when DA1 advertises attribute 4,
  real cell pixel geometry is present, and the XTVERSION identity is 3.6 or
  later (3.4-3.5a compile
  in an unraisable 1 MiB DCS input cap that silently discards busy frames,
  and the 900 KB fallback cannot guarantee that cap on large grids, so those
  versions get no Sixel at all); screen
  and herdr stay block only with no handshake. Sixel under tmux
  reserves the bottom terminal row (engine grid at rows-1) to avoid tmux's
  designed-in top-crop of full-height images. zellij Sixel is blocked
  outright: live testing showed zellij drops every third full-replacement
  frame (image id collision) and breaks page scrolling (its bare host
  re-emission scrolls then snaps back), both zellij-side defects
  (see [TERMINAL-COMPATIBILITY.md](./TERMINAL-COMPATIBILITY.md),
  "Multiplexers"), so Nyala blocks Sixel there to keep them from presenting
  as Nyala bugs, `--backend=sixel` fails the standard unsupported-backend
  check, and Block is the zellij backend until the upstream fixes ship;
  tmux Sixel remains auto. tmux Sixel additionally handles high-frame-rate
  animation: emission is paced at the latest-wins coalescing seam with a
  version-tuned floor, 250 ms on tmux 3.7+ (bisected live: 200 ms saturates
  tmux's ~230 ms per-frame consumption and builds display lag) and 300 ms
  below 3.7 or on an unknown version (live 3.6a at 250 ms kept its pty
  writes drained at ~20 ms yet stalled the display periodically and released
  several frames in a burst, so the backlog sat in its output scheduler
  toward the host terminal, invisible to the write signal; 300 ms showed no
  bursts), plus an adaptive back-off to the last
  frame write duration plus 50 ms, capped at 1 s, because the pty write
  blocks at tmux's own read pace and so measures saturation directly on any
  machine, grid, and content; the escalation threshold, probe band, and
  settle delay derive from the resolved floor. On tmux 3.7+ rate pacing is
  all the parse and re-encode pipeline needs (live 3.7b runs sustained
  ~4 MB full-quality frames indefinitely at that pacing), so a successful
  raise of the server's `input-buffer-size` to 16 MiB (persisting for the
  server lifetime) carries a raised 15 MiB per-frame budget and frames pass
  at full quality. On 3.6.x a successful raise carries a 1.5 MB budget
  instead, an output-stage rate cap of ~5 MB/s at the legacy floor: live
  3.6a settled at factor 2 (2.5 MB frames, ~8.3 MB/s) kept its pty writes
  drained at ~21 ms yet replayed a queued backlog on quit, a factor-3 run
  at the 250 ms floor (~8.8 MB/s) burst identically, and factor 3 trims
  only ~10 percent off factor 2, so the cure is a byte-rate cap on 3.6's
  invisible re-encoded output stage, not a deeper fixed factor. A failed
  raise falls back to a 900 KB budget under tmux's silent 1 MiB DCS discard,
  with the downscale-blur ladder carrying the quality trade; tmux below 3.6
  never reaches the fallback because the version gate refuses it Sixel. The ladder also escalates one step on measured
  write latency with no version gate: an effective interval reaching the
  floor (a write of 200 ms or more) for two consecutive frames trades one
  mild blur step for frame rate, because the criterion is whether tmux ever
  idles: a drained pipe writes tiny frames (3.7b at the floor: 33 ms mean),
  while 3.6a under an earlier 1.4x threshold held ~215 ms writes at factor 2
  and never drained its backlog, replaying it on quit. The two write samples
  after any escalation are skipped as old-factor backlog, so they cannot
  chain further steps (live 3.6a chained 1.5 -> 4 within four frames). The
  one-step-weaker probe every 10 budgeted frames is a band-judged trial
  scoped to the motion episode (settle to settle): it fires only when the
  last three writes sit inside the 60-percent-of-floor band (150 ms) with
  no monotonic rise, any in-trial write reaching that band retreats the
  ladder straight to the pre-probe factor and silences probing until the
  next settle (retrying a factor the ladder watched fail buys one
  saturation ramp per retry), and survival needs both the elapsed cadence
  and a flat-or-falling window inside the band, because live 3.6a showed
  the saturation ramp outliving the cadence (writes 30 -> 146 ms inside the
  band, then past it); a still-rising trial stays pending until it
  stabilises or breaches the band (byte-budget escalations keep the plain
  cadence). Band judgement is what live 3.6a demanded: tmux's buffers
  absorbed a failing sharper factor while writes ramped 20 -> 107 ms
  without ever reaching the 200 ms escalation threshold, so the next,
  sharper probe tipped the queue and took the blame; the 200 ms two-write
  streak stays as the saturation backstop at a settled factor. A freshly
  drained or saturated system therefore does not oscillate between adjacent
  factors, and an idle stream re-emits the last blurred frame once at
  factor 1 (settle-to-sharp), with the next episode resuming at the
  sustained pre-settle factor instead of re-running the discovery cascade
  (live 3.6a re-walked 1 -> 1.5 -> 2 -> 3 with 400-535 ms writes after
  every settle). The steady prefix is the bare cursor home with
  no per-frame erase (dropped after live tmux 3.7b testing showed the erase
  caused the animation strobe and scroll blanking; new frames paint over the
  old, and tmux's stored-image LRU, capped at 20 on 3.6b+, bounds the
  stack), and DEC 2026 is suppressed for tmux Sixel on every version (tmux
  does not defer image operations under sync, so the bracket cannot hide an
  erase). Live-validate placement,
  resize and zoom, scroll, and quit cleanup inside tmux and zellij before
  ticking this leg. The Kitty legs stay open: tmux native Kitty image support is in review
  upstream, so re-test against a tagged tmux release before choosing the final
  Kitty path; zellij passthrough is deferred; herdr's experimental Kitty flag
  is documented but carries no cell geometry. Kitty placement depends on the
  terminal-validation task in section 1, so do not assume `U=1` before that
  result. Sources:
  `STAGE-1.md`, "Focus multiplexers",
  [TUI decisions](./OVERVIEW.md#decisions-tui-research), and
  [TERMINAL-COMPATIBILITY.md](./TERMINAL-COMPATIBILITY.md), "Multiplexers".
- [x] Keep partial Kitty emission closed unless the sustained real-terminal matrix
  exceeds 16.67 ms per frame in full-frame write, transport, terminal decode, or draw.
  Reopening requires a supported direct-CDP-PNG protocol design and an end-to-end win;
  none exists today. Do not revive the failed decoded-full-frame plus Kitty `a=f`
  plan. Sources:
  [Stage 3 deferred scope](./STAGE-3.md#objective-and-scope)
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
- [x] Keep Block sub-cell screencast resolution and graphics downscaling closed. Reopen
  only with specific, explicit owner approval because either change removes the
  full-resolution benchmark that exposes renderer and terminal differences. Source:
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
- [x] Keep Block JPEG quality fixed at q90 as the accepted fidelity and performance
  balance. Do not add runtime quality control or extra configuration. Source:
  [Direct native-YCbCr Block grid](./PERFORMANCE.md#direct-native-ycbcr-block-grid-2026-07-15).
- [ ] Validate Stage 4 scheduling on KMSCON and graphics terminals. Chrome-only diffs
  plus any post-repaint viewport re-blit must finish within one frame budget. Test
  resize, alt-screen behaviour, cursor restoration, and concurrent frame arrival.
  Source: [TUI decisions](./OVERVIEW.md#decisions-tui-research).
- [ ] Add direct interaction: address entry, keyboard text input, buttons,
  hover, and drag selection mapped to CDP events. Wheel scrolling at the pointer
  and mouse clicks (press, release, modifiers, synthesised clickCount) are done
  through the viewer's SGR 1002+1006 decode and the engine `SendMouse` seam;
  keyboard scrolling already used the Stage 3 engine transport. Focus traversal
  and activation are done: Tab, Shift+Tab, and Enter forward as native CDP key
  strokes (rawKeyDown/char/keyUp), giving Chromium's own focus ring and default
  actions; plain printable keys stay free for the coming form input. Source:
  `OVERVIEW.md`, "Input mapping".
- [x] Add mouse wheel and click support. The viewer enables SGR 1002+1006 on every
  terminal with no per-terminal cases and disables them unconditionally on exit and
  SIGTERM. Wheel dispatches at the pointer, three lines per event, accumulable;
  clicks carry left/right/middle buttons, modifier bits, and synthesised clickCount
  through the engine `SendMouse` seam with Z-aware cell-centre mapping. Live
  verified on Kitty (Block and Kitty backends), Alacritty (Sixel), tmux, and
  zellij. Hover (1003) and drag selection stay in the open direct-interaction item
  above. Source: `OVERVIEW.md`, "Input mapping".
- [ ] Add clickable Back, Forward, and Reload controls to the Stage 4 chrome. The
  keyboard half is done: Ctrl+R and F5 reload, Alt+Left and Alt+Right step
  Chromium's navigation history, and Alt+Home reopens the launch URL, all through
  the engine `SendNav` seam. Back and Forward use Chromium's existing navigation
  state; Nyala does not own or persist a navigation-history model. Bookmarks and
  other full-browser features remain out of scope. Sources: `OVERVIEW.md`, "v1
  interaction scope", and [Stage 3 deferred scope](./STAGE-3.md#objective-and-scope).
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
  This is run-versus-run ordering; the occlusion cull's text-over-image false positive
  is already fixed by LCA-bounded compositing (`STAGE-3.md`, 2026-07-22 note).
  Source: `STAGE-3.md`, "Overlay guard and background recompose".
- [ ] Improve shaped-script placement and joining where corpus evidence still exceeds
  the default gates, including Arabic advances and CJK vertical/ruby runs. Tighten each
  fixture override only after its render fault is fixed. Sources: `STAGE-3.md`,
  "Corpus placement gate", and `OVERVIEW.md`, shaped-script research.
- [ ] Widen the sextant allow-list only after a repeatable real-terminal test card
  shows no inter-row bleed. Record terminal name and version, and add version gates
  where one release is bad. Source:
  [Block mode follow-ups](./BLOCK-MODES.md#open-follow-ups).
- [ ] Keep REP as an open Block production spike. The retained benchmark-only harness
  shows large byte savings on compressible fixtures, but its fixed threshold regresses
  photo and motion writer p90 by 15.7% to 24.6%; production `diff.go` remains
  unchanged. Require cheaper or adaptive detection, a capability and literal-fallback
  policy, physical checks on KMSCON, foot, tmux, Zellij, herdr, and GNU Screen, and
  terminal-presented byte, latency, and frame-rate evidence before acceptance. GNU
  Screen requires literal fallback. Sources:
  [Block REP benchmark](./PERFORMANCE.md#block-rep-benchmark-only-spike-2026-07-19),
  `STAGE-2.md`, "Out of scope", and `OVERVIEW.md`, REP research.
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
- [ ] Exact-density zoom: relaunch Chromium with the new device scale factor when a
  zoom moves `layoutScale` away from the launch `--force-device-scale-factor`, gated
  in `transitionScreencastLocked`. Headless Chromium paints at the launch scale for
  the whole process, so the current viewer-side grid scaling leaves a zoomed-in page
  slightly soft until the next navigation; a relaunch restores full density at about
  1 s per zoom step. `Emulation.SetDeviceMetricsOverride` `WithScale` was measured
  and rejected: above the launch scale it pinch-zoom crops instead of repainting
  densely. Source: `PERFORMANCE.md`, "Mid-session zoom re-derives the cell pair
  (2026-07-22)".

## Closed or stale roadmap entries

- [x] Pass live Kitty CDP PNG frames through without base64 decode, PNG decode, pixel
  packing, zlib, or base64 re-encode. `Frame.EncodedPNG` and `kitty.LiveEncoder` now
  provide the direct path. Sources: `README.md`, `OVERVIEW.md`, and `STAGE-3.md`.
- [x] Remove graphics letterboxing with identity fit. Kitty and Sixel now use the full
  engine-sized grid from the top-left; tests cover non-1:2 terminal cells. Source:
  [Performance recommendation 2](./PERFORMANCE.md#top-recommendations-ranked-by-impact-vs-effort).
- [x] Remove decoded-pixel Kitty live encoding. Direct PNG is the only accepted live
  Kitty representation; stale pixel frames are dropped during backend transitions.
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
