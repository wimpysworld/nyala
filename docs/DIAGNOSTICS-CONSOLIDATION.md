# Diagnostics Consolidation Proposal

## Objective

Make `just diagnostics` Nyala's single correctness and diagnostic entrypoint. It must combine the existing browser-free quality harness, independent terminal-protocol checks, and Chromium-fed layout checks without exposing a general bounded recorder. Remove `just record`, `capture=` on `just run`, all `NYALA_CAPTURE*` controls, `viewer.Capture`, bundle production, and the capture-only Block PNG path. Keep reusable codecs and metrics after moving them out of the legacy `internal/capture` namespace. The result must test production paths directly, retain useful gitignored evidence, and fail as one command when any required leg fails.

## Approach

### One entrypoint, explicit legs

📌 KEY: `just diagnostics` owns the complete required gate and runs these legs in order:

1. **Browser-free quality** runs the current diagnostics-tagged harness in `internal/harness`. It keeps the image corpus sweep, baseline and threshold gate, calibration, grid goldens, overlay goldens, WCAG count, pure-Go OCR matcher, and report generation.
2. **Browser-free protocol interop** runs the current synthetic Block, Kitty, and Sixel tests. These tests exercise production encoders and independent decoders without Chromium or a physical terminal.
3. **CDP-fed correctness** launches Chromium against local `testdata/corpus` pages. It drives production frames through the viewer and independent protocol decoders. It absorbs the current `cdp-interop` matrix and the current corpus placement, paint-displacement, and scroll-dance gates.

Each leg writes under `testdata/diagnostics/`, which remains gitignored. The recipe clears only its known output subdirectories before their owning leg starts. It prints a heading for each leg, continues far enough to report independent failures, and exits non-zero if any required leg fails. Missing Chromium or `sixel2png` is a failed prerequisite, not a skip or pass.

The default command runs every correctness gate, including Chromium-fed placement, paint, and scroll checks. Baseline and golden refresh remain explicit maintenance operations.

The focused `block-interop`, `kitty-interop`, `sixel-interop`, and `cdp-interop` recipes remain as selectors for one leg. They are not alternative end-to-end workflows and must call the same tagged tests and helpers as `just diagnostics`. `sixel-interop` remains necessary as the focused `flake.nix` check with `libsixel` in `nativeCheckInputs`. No focused recipe may produce a second artefact schema.

### Replace bundle transport with in-process diagnostic inputs

The current corpus shell workflow serialises live state to a capture bundle, then reopens `text-map.json`, `performance.json`, `cdp-payload.json`, `terminal-model.json`, and `screenshot.png` through command-line tools. The consolidated CDP test must pass the same values directly:

- Convert `render.Frame.Text` into `metrics.PlacementBox` and `metrics.PaintRun` values in test helpers.
- Pass the frame image and the known viewer cell geometry directly to `metrics.PaintDisplacement`.
- Pass ordered frame runs and `render.ScreencastMeta.ScrollOffsetY` directly to `metrics.ScrollDance`.
- Compose the expected Block grid once, send the production terminal bytes through `blockinterop.Decode`, and compare the two `block.Grid` values exactly.
- Read thresholds from each fixture's `fixture.json` through shared Go test helpers. Do not retain the tools' bundle-reading JSON adapters.

This removes the bundle as an internal API. A diagnostic failure writes a compact JSON result with the fixture, backend, grid, measured values, thresholds, and error. CDP corpus legs retain `source-cdp.png` for the production transparent-text pass and `source-paint-cdp.png` for independent paint evidence. Kitty or Sixel legs retain decoded terminal images and Sixel diffs. The Block leg retains grid text or a stable grid archive plus result JSON, not a rasterised Block PNG.

### Remove bounded capture rather than deprecate it

Delete the public and internal capability in one change. Do not keep aliases, hidden environment parsing, deprecated wrappers, or bundle readers for compatibility. There is no current external build, test, or Nix consumer that requires `just record`, `capture=` on `just run`, or `NYALA_CAPTURE*`; the repository's current consumers are the `corpus` and headless `debug-run` recipes, and both receive replacements below.

Removing capture also removes capture-dependent runtime policy. Block resumes its normal live JPEG policy in all production runs; Kitty and Sixel retain their production PNG policies. Remove the `captureOn` branches from screencast format selection, decode timing, frame composition, initial snapshot settling, and lifecycle handling. Preserve live behaviour, then make diagnostic tests wait for valid painted and text-bearing frames outside the engine instead of installing capture-only latches.

### Block PNG boundary

📌 KEY: the **capture Block PNG path is fully removable**, but PNG is not removable from all Nyala diagnostics or production backends.

The removable path is:

- `viewer.State.captureGrid` and `viewer.State.SetCaptureGrid` in `internal/viewer/viewer.go`.
- `drawCaptureFrame` and `capturePerf` in `internal/viewer/capture.go`.
- `Bundle.WriteFrame`'s `terminal-model.json` and `block-render.png` output in `internal/capture/bundle.go`.
- `capture.Rasteriser` as invoked only to turn a captured Block grid into `block-render.png`.
- `tools/rasterise/main.go` and the `rasterise` recipe.
- The capture-only `captureOn` choice that forces Chromium PNG for Block in `internal/engine/screencast.go` and `internal/engine/decodepool.go`.

The reusable Route A rasteriser in `internal/capture/routea.go` is not capture-only today. `internal/harness/sweep.go`, `internal/harness/overlay_grade_test.go`, `internal/ocr/matcher.go`, `internal/viewer/block_interop_test.go`, `internal/viewer/cdp_interop_test.go`, and Block colour benchmarks use it. Move it to a correctness-owned package such as `internal/cellraster`; update OCR and harness imports; remove its redundant use from Block interop, which already has exact grid equality. Keep Route A PNG output only for the browser-free image-quality and OCR legs. Likewise, production Kitty's direct CDP PNG path and Sixel's PNG source path are outside this removal.

Replace `SetCaptureGrid` with a reusable Block compose seam that returns the grid used by the terminal writer. `State.RenderStreamFrame` and diagnostic tests must call the same compose implementation. This lets `blockinterop.Decode` compare production output with the exact composed grid without a mutable test sink on `viewer.State`.

### Workflow decisions after capture removal

| Existing need | Final handling |
|---|---|
| `debug-run` | Keep the interactive `NYALA_DEBUG_TIMING=1` path and `NYALA_TIMING_FILE`. Remove its headless `just record` branch. A non-TTY invocation fails with a direct message. CDP load-path health is covered by the CDP diagnostics leg; it does not create a timing bundle. |
| Corpus | Remove the public `corpus` recipe. Move all fixture, three-geometry placement, paint-displacement, and `layout-13` scroll cases into the CDP diagnostics test. |
| Scroll correctness | Drive `engine.SendScroll` from the CDP test, retain pre-scroll and settled frames in memory, and gate `metrics.ScrollDance` at the fixture thresholds. Include one pre-settle case to keep `composeFrame` scroll compensation covered without `NYALA_CAPTURE_SCROLL_SETTLE`. |
| Placement correctness | Run `metrics.Placement` directly against `render.Frame.Text`, the fixture geometry, and the known layout scale. Keep every committed `placement_gate` override. |
| Paint correctness | Run `metrics.PaintDisplacement` against the CDP frame image and `render.Frame.Text`. Keep the current measured and skipped counts. Convert today's expected failures into explicit result states until the architecture is fixed; measurement errors remain hard failures. |
| Arbitrary-URL forensic evidence | Do not replace the recorder with another general page dump. Use interactive `just run`, `just debug-run`, `just fps`, Chromium DevTools, and normal logs for one-off investigation. Reproduce lasting correctness bugs as sanitised local corpus fixtures, then run `just diagnostics`. This avoids a second public diagnostic workflow and removes the page-content disclosure risk of bundles. |
| FPS | Keep `just fps` as an interactive performance instrument. Its `NYALA_FPS_TIMING` badge and summary in `internal/viewer/stream.go` are independent of bounded capture. Do not make physical-terminal FPS a correctness gate. |

## Reuse Audit

| Area | Status | Source | Decision |
|---|---|---|---|
| Browser-free sweep | **Reuse** | `internal/harness.RunSweep` in `internal/harness/sweep.go`; `sharedSweep` in `internal/harness/shared_diagnostics_test.go` | Keep the existing reports, metrics, artefacts, baseline gate, and once-guarded execution. |
| Calibration | **Extend** | `MeasureCalibration` in `internal/harness/calibration.go`; `sharedCalibration` in `internal/harness/shared_diagnostics_test.go` | Keep the correctness checks in the default diagnostics run. |
| Grid goldens | **Reuse** | `TestGoldenTrack` and `CompareGolden` in `internal/harness/golden_test.go` and `internal/harness/golden.go` | Keep byte-exact grid archives and architecture guards. |
| Overlay grade | **Reuse** | `composeOverlay`, `TestOverlayGoldenTrack`, `TestOverlayMatcherRoundTrip`, and `TestOverlayWCAGGate` in `internal/harness/overlay_grade_test.go` | Keep grid correctness, OCR CER zero, WCAG, and optional visual overlay artefacts. |
| Route A raster | **Extend** | `Rasteriser`, `Rasterise`, `RenderGlyphCell`, and `EncodePNG` in `internal/capture/routea.go` | Move to `internal/cellraster`. Retain only browser-free quality, OCR, and benchmark uses. Remove bundle and Block interop uses. |
| Sixel correctness decoder | **Extend** | `DecodeSixel` in `internal/capture/decode.go` | Move to a correctness-codec package. Reuse from the harness, synthetic Sixel interop, and CDP Sixel interop. |
| Kitty correctness decoder | **Extend** | `DecodeKitty` in `internal/capture/decode.go`; independent `kittyinterop.Decode` in `internal/kittyinterop/decode.go` | Move the harness round-trip decoder with the Sixel decoder. Keep `kittyinterop.Decode` as the independent terminal oracle. |
| Block terminal oracle | **Reuse** | `blockinterop.Decode` in `internal/blockinterop`; `TestBlockTerminalRoundTrip` in `internal/viewer/block_interop_test.go` | Compare cell models directly. Drop Route A PNG equality, which adds no signal after exact grid equality. |
| Kitty terminal oracle | **Reuse** | `kittyinterop.Decode` in `internal/kittyinterop/decode.go`; `TestKittyTerminalRoundTrip` in `internal/viewer/kitty_interop_test.go` | Keep exact command, payload, image, ID, and delete checks. |
| Sixel independent oracle | **Reuse** | `TestLibsixelRoundTrip` and `decodeLibsixel` in `internal/render/sixel/interop_test.go` | Keep go-sixel against libsixel agreement and the Nix check dependency. |
| CDP fixture driver | **Extend** | `TestCDPInteropMatrix`, `captureCDPInteropFrames`, `waitCDPInteropFrame`, and `runCDPInteropCase` in `internal/viewer/cdp_interop_test.go` | Rename capture-labelled helpers, share them with corpus correctness, and retain source and decoded graphics artefacts. |
| Frame validity | **Reuse** | `validCDPInteropFrame`, `paintedCDPInteropSource`, and `materialiseCDPInteropFrame` in `internal/viewer/cdp_interop_test.go` | Use these bounded waits in tests. They do not expose capture mode or write bundles. |
| Placement metric | **Reuse** | `metrics.Placement` and `metrics.PlacementBox` in `internal/metrics/placement.go` | Feed in-memory glyph runs. Move threshold loading out of `tools/placement-score`. |
| Paint metric | **Reuse** | `metrics.PaintDisplacement` and `metrics.PaintRun` in `internal/metrics/paintdisplace.go` | Feed the frame image and runs directly. Keep source-paint PNGs as evidence. |
| Scroll metric | **Reuse** | `metrics.ScrollDance` and `metrics.ScrollRun` in `internal/metrics/scrolldance.go` | Feed ordered in-memory frames and scroll offsets. Move threshold loading out of `tools/scroll-dance`. |
| Fixture thresholds | **Extend** | parsers and gate checks in `tools/placement-score/main.go`, `tools/paint-displacement/main.go`, and `tools/scroll-dance/main.go`; `testdata/corpus/*/fixture.json` | Move reusable parsing and gate logic beside the metrics or diagnostic tests, then delete bundle-facing CLIs. Do not duplicate threshold rules. |
| Load timings | **Extend** | `newLoadTimer` in `internal/engine/timing.go`; `NYALA_DEBUG_TIMING` handling in `justfile` | Keep stderr timing for interactive `debug-run`. Remove `sessionTimings`, `render.LoadTimings`, and performance JSON transport when no non-capture consumer remains. |
| Live FPS | **Reuse** | `newFPSMeter` and FPS rendering in `internal/viewer/stream.go`; `fps` in `justfile` | Keep outside diagnostics because it measures a physical on-screen render. |
| Bounded driver | **Delete** | `viewer.Capture`, `runCapture`, and `captureLoop` in `internal/viewer/capture.go` | No current consumer remains after corpus and headless debug migration. |
| Bundle writer | **Delete** | `Bundle`, `NewBundle`, `WriteFrame`, and finalisers in `internal/capture/bundle.go`; `internal/capture/performance.go`; `internal/capture/report.go` | Remove all manifest, report, terminal-stream, CDP-session, event, text-map, terminal-model, and per-frame bundle code. |
| Capture environment | **Delete** | `ParseCaptureEnv` in `internal/app/capture_env.go`; dispatch in `internal/app/app.go`; `record` and `run` parsing in `justfile` | Remove without compatibility shims. `NYALA_CELL_PX` is not a `NYALA_CAPTURE*` variable and remains a live diagnostic geometry override. |
| Engine recorder | **Delete** | `NewCaptureRecorder`, `Recorder`, and event types in `internal/engine/capture_record.go`; `ChromiumEngine.EnableCapture` in `internal/engine/chromium.go` | CDP tests use normal engine output and test-owned evidence. Remove recorder call sites and capture-only latches. |
| Per-frame capture timings | **Delete** | `render.FrameStats` in `internal/render/render.go`; population in `internal/engine/screencast.go`; `capturePerf` in `internal/viewer/capture.go` | Browser-free `metrics.CaptureFrame` and interactive FPS remain the supported performance measures. |
| OCR scoring | **Extend** | `ocr.Matcher` and `ocr.NewMatcher` in `internal/ocr/matcher.go`; `ocr.CER` and `ocr.WER` in `internal/ocr/ocr.go` | Change the rasteriser import to its new package. Keep the existing pure-Go correctness contract. |
| Diagnostic artefacts | **Extend** | `harness.Emit` in `internal/harness/artefacts.go`; `prepareCDPInteropDir`, `writeCDPInteropPNG`, and `writeCDPInteropResult` in `internal/viewer/cdp_interop_test.go` | Standardise output beneath `testdata/diagnostics`; do not recreate bundle manifests. |

No new measurement algorithm is required. The only new structural seam is a reusable Block compose result for the terminal writer and tests. Existing `block.Renderer.Compose` and viewer grid-writing code cover the behaviour; the implementation must extract and share it rather than introduce another compositor.

## Scope

| File or area | Intent |
|---|---|
| `justfile` | Make `diagnostics` orchestrate all required legs and optional heavy work. Remove `record`, `corpus`, `rasterise`, `placement-score`, `paint-displacement`, and `scroll-dance`. Remove `capture=`, `scroll=`, and `settle=` handling from `run`. Make headless `debug-run` fail directly. Keep focused interop selectors and interactive `fps`. |
| `internal/app/app.go`, `internal/app/capture_env.go`, tests | Remove environment parsing, capture dispatch, adapter recorder wiring, and their tests. `app.Run` always enters `viewer.Stream`. |
| `internal/viewer/capture.go`, `internal/viewer/capture_test.go` | Delete the bounded driver and its lifecycle, timing, scroll, partial-bundle, and blank-frame tests. |
| `internal/viewer/viewer.go`, `internal/viewer/capture_grid_test.go` | Remove `captureGrid` and `SetCaptureGrid`. Extract a shared Block compose result used by terminal rendering and diagnostics. Update full-grid and stream tests to the new seam. |
| `internal/viewer/cdp_interop_test.go` | Extend the CDP matrix with all corpus geometries, placement, paint, and scroll gates. Rename capture-labelled helpers. Keep graphics PNG evidence, source-paint evidence, and JSON results. Replace Block raster PNG comparison with exact grid comparison and a stable grid artefact. |
| `internal/viewer/block_interop_test.go` | Keep independent VT decoding and exact cell-model comparison. Remove Route A PNG comparison. |
| `internal/engine/capture_record.go`, tests | Delete capture events, session JSON recording, recorder latches, and tests. |
| `internal/engine/chromium.go`, `screencast.go`, `decodepool.go`, `recovery.go`, `calibrate.go`, `input.go`, tests | Remove `captureOn`, recorder fields and branches, capture-only paint/text/scroll latches, forced-PNG policy, event emission, and per-frame capture timings. Preserve and test the live branch that remains. |
| `internal/engine/timing.go` | Keep `NYALA_DEBUG_TIMING` stderr milestones. Remove capture span storage and exported transport types if no remaining user exists. |
| `internal/render/render.go`, tests | Remove `FrameStats` and `LoadTimings` after their capture consumers are gone. Keep `Frame`, `ScreencastMeta`, and production direct Kitty PNG fields. |
| `internal/capture/` | Delete bundle, performance, report, and related tests. Move Route A raster and graphics correctness decoders to purpose-named packages, then remove the empty legacy package. |
| `internal/cellraster/` | Moved Route A rasteriser, glyph-cell renderer, PNG encoder, and tests. This is correctness infrastructure, not runtime capture. |
| correctness-codec package | Moved `DecodeSixel`, `DecodeKitty`, and tests for browser-free round trips. Choose one purpose-named package during planning; do not retain the name `capture`. |
| `internal/harness/`, `internal/ocr/`, Block benchmarks | Update imports to the moved rasteriser and decoders. Keep existing browser-free gates and artefacts. Split optional repeated measurement from required correctness only where runtime warrants it. |
| `internal/metrics/` | Keep algorithms. Add shared fixture-threshold parsing or small gate helpers only where this avoids copying logic from the deleted tools. |
| `tools/rasterise`, `tools/placement-score`, `tools/paint-displacement`, `tools/scroll-dance` | Delete bundle-facing commands and tests after their parsers and gates move to reusable Go code. |
| `testdata/corpus/README.md` | Replace recorder and tool instructions with the diagnostics contract and artefact locations. Keep fixture provenance and thresholds. |
| `README.md`, `AGENTS.md` | Document one diagnostics workflow, focused selectors, interactive timing and FPS, retained artefacts, and removal of record mode. Remove bundle and capture environment guidance. |
| `docs/STAGE-0.md`, `docs/STAGE-2.md`, `docs/STAGE-3.md`, `docs/PERFORMANCE.md`, `docs/SIXEL-TODO.md` | Preserve dated and stage history. Add a short retirement note where an old command reads as current guidance, pointing to this proposal or the active README. Do not rewrite historical decisions as if capture never existed. |
| `flake.nix` | Keep the focused Sixel interop check and make its command match the shared selector used by diagnostics. Add no Chromium requirement to the package build unless the existing check policy explicitly adopts the CDP leg. |

## Acceptance Criteria

### Public surface removal

- `just --list` contains no `record`, `corpus`, `rasterise`, `placement-score`, `paint-displacement`, or `scroll-dance` recipe.
- `just run` rejects or forwards no `capture=`, `scroll=`, or `settle=` pseudo-argument. No compatibility alias exists.
- `rg 'NYALA_CAPTURE|NYALA_CAPTURE_SCROLL|NYALA_CAPTURE_SCROLL_SETTLE' --glob '!docs/STAGE-*.md' --glob '!docs/PERFORMANCE.md'` finds no active code, tests, recipes, README, AGENTS, or corpus instructions.
- `rg 'viewer\.Capture|func Capture\(|EnableCapture|CaptureSession|captureOn' internal` finds no production capture seam.
- `internal/viewer/capture.go`, `internal/app/capture_env.go`, `internal/engine/capture_record.go`, bundle-only files under `internal/capture`, and the four bundle-facing tool directories no longer exist.
- No active code writes `manifest.json`, `report.md`, `performance.json`, `terminal-stream.raw`, `cdp-session.json`, `cdp-events.ndjson`, `text-map.json`, `terminal-model.json`, or `block-render.png` as a capture bundle.

### Required diagnostics

- `just diagnostics` runs the browser-free harness, all three synthetic protocol interop gates, and the CDP-fed matrix. A failure in any leg makes the recipe exit non-zero.
- The browser-free leg still gates committed baseline thresholds, calibration invariants, bitmap and overlay grid goldens, overlay `CER == 0`, and WCAG low-contrast counts.
- The Block synthetic and CDP legs compare independently decoded terminal cell models with the production composed `block.Grid` exactly, including wide trailing cells and successive-frame diff output.
- Kitty synthetic and CDP legs check reconstructed PNG payload and exact pixels. Sixel synthetic and CDP legs check go-sixel against libsixel within the existing decoder tolerance and source quality floors.
- Every corpus fixture runs placement at 120x40, 240x48, and 158x86 with 16x33 device cells. All committed `placement_gate` overrides apply.
- Paint displacement reports measured, skipped, horizontal, and vertical values for every corpus leg. A measurement error fails. Known architectural breaches remain labelled expected failures until a separate fix changes their approved gate state.
- `layout-13-linuxmatters` runs the 600 CSS-pixel scroll case. The settled gate requires zero danced runs and zero re-placements. A pre-settle case proves `composeFrame` compensation without a capture settle switch.
- Plain `just diagnostics` is the complete correctness gate. It has no heavy-mode requirement.

### Artefacts and focused runs

- Browser-free `report.json`, `report.csv`, `summary.md`, and existing quality PNGs appear under `testdata/diagnostics/`.
- Each CDP case writes result JSON. Corpus cases retain `source-cdp.png` and `source-paint-cdp.png`; Kitty and Sixel cases retain decoded terminal PNGs; Sixel retains its diff PNG. Block cases write no expected or decoded Block PNG.
- Artefacts contain no proxy credentials. The diagnostics output is fixture-bound; no arbitrary URL or page-content bundle is produced.
- `just block-interop`, `just kitty-interop`, `just sixel-interop`, and `just cdp-interop` run the same focused tests used by `just diagnostics`.
- `nix build .#checks.<system>.sixel-interop` still passes on a supported system.

### Build and documentation

- `just build`, `just test`, and `just lint` pass.
- `just diagnostics` and `just cdp-interop` pass in the Nix development shell with Chromium and `sixel2png` available.
- `just fmt-check` passes.
- `git diff --check` passes.
- README, AGENTS, and corpus instructions describe only the consolidated current workflow. Stage and dated performance documents keep their historical record and clearly label retired commands when they could be mistaken for current guidance.

## Risks

- **Coverage loss while removing serialization:** the JSON tools currently perform threshold parsing, layout-scale conversion, and grid-size fallback. Moving to in-memory values can change units without a compiler error. Port their tests before deleting the tools, and compare old and new metric results on committed fixtures during implementation.
- **Production and diagnostic compose drift:** removing `SetCaptureGrid` can lead to a second Block compositor. The shared compose-result seam must be the one production terminal writing uses.
- **Paint-source semantics:** Block forcing hides glyph ink in normal screenshots, while paint displacement expects source glyph ink. The CDP test must preserve the current diagnostic source-paint setup or state which browser pass supplies painted text. A zero measured count is N/A, not a pass.
- **Snapshot timing:** capture-only paint and text latches currently make `frames=1` deterministic. CDP tests must identify validity from frame content and bounded waits, then fail with the last observed state on timeout. Do not reintroduce those latches as a hidden recorder.
- **Runtime increase:** all corpus geometries plus Chromium protocol cases make the canonical gate slower. Keep fixture launches shared where isolation permits and retain focused selectors. Do not drop required cases to make the command faster.
- **Platform dependencies:** libsixel is available in the Nix shell and focused flake check, while Chromium availability varies by platform. The recipe must report a prerequisite failure. CI or flake adoption of the CDP leg is a separate policy choice.
- **Package naming:** leaving reusable raster and decoder code under `internal/capture` would preserve a dead architectural boundary and invite new bundle coupling. Move the code before deleting the package.
- **Historical references:** broad search-and-replace would damage the stage and performance record. Update active guidance and add retirement notes; leave dated measurements and decisions intact.
- **Arbitrary-page debugging:** removing bundles reduces post-mortem evidence for unreproducible remote pages. The accepted trade-off is interactive observation and conversion of lasting defects into local fixtures, avoiding sensitive page dumps and a second workflow.

## Out of Scope

- Fixing the architecture that causes current paint-displacement expected failures.
- Changing renderer quality thresholds, committed baselines, golden archives, fixture HTML, or fixture provenance.
- Replacing the pure-Go OCR matcher or changing CER and WER algorithms.
- Changing production Kitty direct-PNG, Sixel PNG, or live Block JPEG behaviour beyond removal of capture-only policy branches.
- Automating physical-terminal font, glyph-width, passthrough, colour-profile, inter-row bleed, anchoring, repaint timing, or sustained-FPS checks.
- Adding remote-URL crawling, a new forensic bundle format, screenshots on demand, trace archives, or a compatibility wrapper for record mode.
- Making `just check`, Nix package builds, or release builds depend on Chromium-fed diagnostics without a separate CI and platform decision.
- Rewriting Stage 0 through Stage 3 or dated performance documents as current architecture descriptions.

## Implementation notes

- `ScreencastMeta` remains on `render.Frame` because the normal CDP scroll test
  needs the bitmap scroll offset. Capture-only frame statistics and load-timing
  transport were removed.
- The proposed optional `heavy=1` mode was not added because the harness has no
  optional repeated statistical leg. This is an implemented deviation from the
  original approach and acceptance wording. Every required case runs in the
  default diagnostics workflow.
- Normal Block screenshots contain transparent DOM text by design. Paint metrics
  use a second normal-engine Block pass that retains source glyph ink. Its
  per-fixture and geometry state, minimum measured coverage, maximum skipped
  coverage, and displacement maxima are pinned in
  `testdata/corpus/paint-baseline.json`. Known threshold breaches remain
  `expected_failure`. Placement and scroll remain hard gates.
