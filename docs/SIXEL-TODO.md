# Sixel optimisation backlog

## Scope

This is the canonical implementation backlog for Sixel encoder CPU work. The
accepted Chafa-informed candidates are bounded parallel indexed-pixel mapping and,
only if a new profile supports it, bounded parallel six-row band emission. Complete
tasks in order. Each change must remain useful, measurable, and safe to revert on
its own.

## Current measured baseline

Chafa is comparative evidence, not a prediction for Nyala. On the matched
1920x1080 photo, Chafa encoded in 14.22 ms and Nyala in 38.68 ms, but Chafa used 24
workers and 932% CPU. Nyala spent 27.05 ms in palette selection plus indexed-pixel
mapping and 11.74 ms in band emission. Nyala already wins the small flat and web
cases, and its 1920x1080 flat and web payloads are 76.3% and 30.3% smaller. These
results support bounded experiments, not a Chafa transplant or a general speed
claim.

## Independent decoder gate

`just sixel-interop` is the offline compatibility gate for the retained encoder.
The `sixelinterop`-tagged Go test renders complete production DCS streams, decodes
each stream with both `capture.DecodeSixel` (go-sixel) and libsixel 1.10.5's
`sixel2png`, and checks framing, raster dimensions, decoded bounds, pixels, and
adaptive-image PSNR and SSIM. Its eight cases cover flat and page-like content,
photo-like content at the 500,000-pixel mapping threshold, odd height, sparse
planes, palette register 255, RLE at 3/4 and 8192/8193, and the committed page PNG
through the production parallel mapping and band thresholds. Exact primary-colour
palettes compare exactly. Adaptive decoder output allows at most two levels per
RGB channel for different integer expansion of Sixel's 0..100 colour registers.

The flake exposes this as `checks.<system>.sixel-interop` on every supported
platform. `nix flake check` and `just check` run the gate for the current system.
Libsixel is present only in the development shell and that check's native inputs.
It is absent from Nyala's package inputs and runtime closure. The gate proves that
an independent decoder
accepts and reconstructs Nyala's stream. It does not prove physical-terminal DCS
parsing, multiplexer passthrough, image anchoring, terminal colour conversion,
repaint behaviour, throughput, or visible tearing; those remain terminal-only.

## Shared invariants

- Preserve the opaque, dither-free, adaptive palette path, the 64K median-cut
  sample, RGB555 lookup table, palette and index output, exact-colour fast path,
  palette order, and byte-identical payload.
- Preserve the present byte-plane layout, right-edge bounds, generation counters,
  RLE grammar and 8192 repeat ceiling, band and colour separators, DCS framing, and
  decoded pixels. The accepted omission of `$` immediately after inter-band `-`
  is the sole payload-grammar change.
- Keep `Renderer.Render` serial under its mutex. Parallel stages must finish before
  the next stage starts and before the single ordered writer call.
- Use the existing Sixel budget from `newPaletteLUT` in
  `internal/render/sixel/encoder.go`: `min(4, runtime.GOMAXPROCS(0))`. The code has
  no named worker-count helper today. Extract this exact expression once and reuse
  it for LUT construction, mapping, and any later emission work. Do not add a flag,
  environment variable, second count, independent concurrency budget, or nested
  pool.
- Preserve the current ownership model. `viewer.State` retains one
  `sixel.Renderer` across backend switches; the renderer mutex serialises calls and
  owns one reusable `opaqueEncoder`, which owns the output and band scratch. The
  current LUT workers are bounded temporary workers inside `newPaletteLUT`. The
  separate `rowband.Pool` in viewer `frameScratch` is owned by the live Block path,
  uses `GOMAXPROCS-1`, and is not the Sixel worker-count policy.
- At most one encoder worker set may consume the shared budget at a time. Palette
  LUT construction, indexed mapping, and band emission run in sequence. Do not
  overlap them with each other.
- Keep small work serial. Activation thresholds are benchmark candidates, not
  constants to retain without evidence.

## Shared benchmark protocol

Use the focused Sixel tests and opt-in benchmark harness in
`internal/render/sixel/palette_bench_test.go`, plus the production-shaped viewer
benchmark in `internal/viewer/sixel_bench_test.go`. Extend them instead of creating
a separate harness. Measure flat, web, and photo fixtures at 1280x720, 1920x1080,
and 2560x1600. Add boundary sizes around every activation threshold.

For an acceptance run, use six fresh processes with baseline and candidate order
rotated, four discarded warm-ups, at least 30 retained samples per arm,
`GOMAXPROCS=4`, and `GOGC=100`. Report encode-only stage spans, full encode p90,
complete PNG-decode-plus-encode p90, B/op, allocs/op, payload bytes and SHA-256,
and decoded-pixel equality through `capture.DecodeSixel` or the existing
`mattn/go-sixel` oracle. Keep hashing and decoding outside timed regions.

For live checks, use the existing capture and timing instruments. Record Chrome
delivery, first contentful paint, Sixel encode and terminal write spans, delivered
FPS, CPU, frame bytes, drops, and worst frame gap. Microbenchmarks alone cannot
justify retaining either optimisation.

## Order

1. Implement and gate parallel indexed-pixel mapping.
2. Reprofile the retained encoder and decide whether emission still qualifies.
3. Implement parallel band emission only if task 2.1 opens its gate.
4. Validate the accepted stack live and independently revert any failed layer.

## Phase 1: Indexed-pixel mapping

### 1.1 - Add bounded parallel RGB555 mapping

- **Assigned Agent**: `donatello`
- **Assignment Reason**: The implementation specialist owns the bounded encoder and
  test changes from this defined plan, including exact-output preservation.
- **Dependencies**: None
- **Parallel**: No
- **Blocked By**: None
- **Scope**: `internal/render/sixel/encoder.go` (`newPaletteLUT`, `mapPaletted`,
  `palettedFromOpaquePixels`, `opaqueEncoder.encode`),
  `internal/render/sixel/sixel.go` (`Renderer` ownership if retained worker state is
  needed), `internal/render/sixel/sixel_test.go`,
  `internal/render/sixel/palette_sampling_test.go`, and
  `internal/render/sixel/palette_bench_test.go`.
- **Reuse**: Extract the existing `min(4, runtime.GOMAXPROCS(0))` expression into
  one Sixel worker-count helper. Reuse the current row-range splitting pattern and
  `mapPaletted` inner loop. Keep one bounded worker set for each active stage; do
  not create another tunable or allow LUT and mapping work to overlap.
- **Success Criteria**: Preserve the 64K median-cut sample, RGB555 LUT, palette and
  index output, exact-colour fast path, and byte-identical payload. Run the
  exact-colour scan serially; split disjoint source and destination row ranges only
  after that path returns `nil`. Begin activation trials at 500,000 pixels and
  benchmark candidate cut-offs through 1,000,000 pixels, including one pixel below,
  at, and above each candidate. Retain the serial mapper below the measured cut-off.
  Add unit tests for odd bounds, non-zero origins, stride, threshold boundaries,
  worker counts 1 and 4, deterministic repeated output, payload SHA-256 equality,
  and decoded-pixel equality. Run `go test -race ./internal/render/sixel`. Add no
  material warm allocations and do not allocate per row. At 1920x1080 photo, require
  both at least 5% lower full-encode p90 and at least 2 ms saved. Require no
  regression in the complete PNG-decode-plus-encode gate, Chrome delivery, or first
  contentful paint.
- **Complexity**: M
- **Flags**: ⚠️ High-risk, 🔍 Needs review

**Status (2026-07-18): Accepted after stacked interaction validation.** The
four-worker candidate extracted the existing
four-worker budget and split adaptive indexed mapping into disjoint row ranges at
500,000 pixels. Serial and parallel indexes, payload SHA-256 values, repeated
payloads, and decoded pixels matched across threshold boundaries, unusual bounds,
origins, strides, and worker counts 1 and 4. The race test passed.

The four-worker screen used six fresh rotated processes and 30 retained 1920x1080
photo iterations per arm with `GOMAXPROCS=4` and `GOGC=100`. Isolated mapping
median fell from 2.100 ms to 0.648 ms. The worst full-encode process aggregate
fell only from 20.278 ms to 19.434 ms, 4.16% and 0.843 ms. This missed both
retention gates. PNG-decode-plus-encode did not regress. The candidate added five
to six temporary-worker allocations per active map.

A user-authorised follow-up raised only the shared Sixel cap to six and ran six
fresh rotated process pairs with four warm-ups and 30 retained samples per arm at
`GOMAXPROCS=6` and `GOGC=100`. The median of the six full-encode p90 values fell
from 20.795 ms to 19.573 ms, 5.88%, but saved only 1.222 ms. It passed the 5% gate
and missed the unchanged 2 ms gate. PNG-decode-plus-encode p90 fell from 36.998 ms
to 35.331 ms. Active mapping added seven temporary-worker allocations. The code
and temporary tests were removed after each rejection. Live Chrome delivery,
first-contentful-paint, and contention checks did not run because the full-encode
gate had already failed. These results remain as the record of the original
isolated rejection. The user later accepted mapping after the stacked task 4.1
matrix showed a repeatable complete-path win in every workload and size.

**Combined candidate status (2026-07-18): Accepted.** At
the user's request, the rejected four-worker mapper has been restored on top of
the committed parallel-band candidate without changing either shared worker cap.
The exact-colour scan stays serial. Adaptive mapping activates at 500,000 pixels,
splits source and destination into disjoint row ranges, waits for all workers, and
then enters band emission. LUT construction, mapping, and emission therefore use
the same `sixelWorkerCount()` budget in sequence.

Six fresh rotated paired processes used four warm-ups, 30 retained samples,
`GOMAXPROCS=4`, and `GOGC=100`. On the adaptive 1920x1080 Linux Matters photo,
median process p90 fell from 17.382 ms for the committed band-only shape to 15.592
ms combined, 10.30% and 1.790 ms faster. PNG decode plus encode fell from 31.472
ms to 29.457 ms, 6.40% and 2.015 ms faster. Isolated mapping fell from 2.107 ms to
0.711 ms p90, 66.25% faster. It adds five isolated allocations and about 336
B/op; the complete path stayed at 1,306 allocations within about 0.2 KiB/op of
the baseline. CPU profiling rose from 99.9% to 352.5% across the bounded workers.

The earlier 0.843 ms mapping saving persists after band parallelism and measures
1.790 ms in this paired screen. Flat full encode improved 0.51%; web improved
1.19%. Flat PNG decode plus encode improved 0.38%, while web regressed 0.28%,
inside the 1% small-frame gate. Payload bytes and SHA-256 values match for every
fixture. Focused tests also match mapped indexes, repeated payloads, and decoded
pixels across the threshold edges, workers 1 and 4, and unusual bounds and stride.
The task 4.1 interaction matrix supersedes this preliminary screen. It showed
15.78% to 50.19% lower full-encode p90 for the combined arm than the serial
map-and-emission control, with identical payloads and decoded pixels. The mapping
layer is accepted as part of the stack. Live Chromium delivery, first contentful
paint, and terminal effects remain deferred because no supported automated Sixel
terminal was available.

## Phase 2: Reprofile gate

### 2.1 - Reprofile band emission after parallel mapping

- **Assigned Agent**: `gonzales`
- **Assignment Reason**: The decision depends on measured stage attribution and
  end-to-end cost after phase 1 changes the profile.
- **Dependencies**: 1.1
- **Parallel**: No
- **Blocked By**: 1.1 passing every acceptance gate
- **Scope**: `internal/render/sixel/palette_bench_test.go` and the existing stage
  timing seams around `opaqueEncoder.encode`, `mapPaletted`, and the six-row loop.
- **Reuse**: Use the shared benchmark protocol and current palette, mapping, and
  emission stage accounting. Do not modify production emission in this task.
- **Success Criteria**: Measure retained phase 1 code across the full fixture and
  resolution matrix. Record full encode, palette selection, mapping, band emission,
  payload, allocations, and complete PNG-decode-plus-encode results. Unblock task
  3.1 only when band emission remains at least 25% of total encode time on the
  1920x1080 photo and the larger matrix shows enough absolute time to meet task
  3.1's gate. Otherwise record the closed decision here and stop.
- **Complexity**: S
- **Flags**: 🔍 Needs review

## Phase 3: Conditional band emission

### 3.1 - Add bounded parallel six-row band emission

- **Assigned Agent**: `donatello`
- **Assignment Reason**: The implementation specialist owns the conditional encoder,
  retained-scratch, ordered-join, and exact-output test changes from this plan.
- **Dependencies**: 2.1
- **Parallel**: No
- **Blocked By**: Task 2.1 must show band emission is at least 25% of total encode
  time and has enough absolute cost to satisfy this task's acceptance gate.
- **Scope**: `internal/render/sixel/encoder.go` (`opaqueEncoder` scratch,
  six-row band loop, `appendColourSelect`, `appendRun`),
  `internal/render/sixel/sixel_test.go`, and
  `internal/render/sixel/palette_bench_test.go`.
- **Reuse**: Reuse the Sixel worker-count helper from task 1.1 and the existing
  byte-plane, generation, right-edge, and RLE code. Divide complete six-row bands
  into no more than the shared worker count of contiguous batches. Retain bounded
  per-worker scratch and output, then join batches in source order. Mapping must
  finish before emission begins. Do not add nested pools, concurrent mapping and
  emission pools, or Chafa's worst-case allocation model.
- **Success Criteria**: Preserve byte-plane layout, palette order, right-edge
  bounds, generation counters, RLE, the 8192 repeat ceiling, separators, framing,
  and byte-identical output. Base activation on present colours x width x band
  count, not pixels alone. Start trials at 1,000,000 pixels and 120 bands. Cover
  odd heights, partial final bands, absent colours, repeat boundaries, wide rows,
  non-zero bounds, worker counts 1 and 4, ordered joining, scratch reuse, writer
  errors, payload hashes, and decoded pixels. Require at least 5% lower full-encode
  p90 and at least 4 ms saved on the 1920x1080 photo. Allow no more than 1%
  regression on 1280x720 flat and web fixtures, no material warm allocations, and
  no Chrome delivery or first-contentful-paint slowdown.
- **Complexity**: M
- **Flags**: ⚠️ High-risk, 🔍 Needs review, 🧠 Context-intensive

**Status (2026-07-18): Accepted.** The user explicitly
waived task 2.1's 25% reprofile blocker and the plan's automatic revert rule. The
task 4.1 interaction matrix settles the non-live retention decision.

The implementation names the inherited budget as `sixelWorkerCount()`, still
`min(4, runtime.GOMAXPROCS(0))`, and reuses it for LUT construction and band
emission. At 120 bands and 39,000,000 present-colour-width-band units, it divides
complete bands into at most four contiguous batches. Each batch owns retained
byte-plane, generation, right-edge, and output scratch. Outputs join in band
order, then a partial final band runs serially. Smaller work remains serial.

Six fresh rotated serial/candidate processes used `GOMAXPROCS=4`, `GOGC=100`,
four untimed warm-ups in the benchmark setup, and 30 timed samples per arm. The
screen used bounded synthetic 1920x1080 photo, 1280x720 flat, and 1280x720 web
fixtures. It is implementation evidence, not the full acceptance matrix. Median
process p90 results were:

| Fixture and path | Serial | Candidate | Change |
|---|---:|---:|---:|
| 1920x1080 photo, emission | 62.779 ms | 18.919 ms | 69.86%, 43.860 ms faster |
| 1920x1080 photo, complete paletted encode | 62.747 ms | 19.836 ms | 68.39%, 42.911 ms faster |
| 1920x1080 photo, PNG decode plus paletted encode | 64.387 ms | 20.872 ms | 67.58%, 43.515 ms faster |
| 1280x720 flat, complete paletted encode | 1.848 ms | 1.887 ms | 2.11% slower |
| 1280x720 web, complete paletted encode | 5.196 ms | 4.754 ms | 8.51% faster |

The photo candidate adds five warm emission allocations, about 537 B/op after
subtracting the timing harness. CPU profile samples rose from 100% of one core to
353.7% across four workers. Serial and parallel payloads, SHA-256 values, and
decoded pixels match; complete output is 9,893,259 bytes in both arms. Tests cover
odd and partial heights, absent colours,
8192-repeat boundaries, non-zero origins and stride, threshold edges, workers 1
and 4, ordered joining, repeated output, retained scratch, writer errors, and the
race detector. The first flat p90 and allocation results missed plan gates. The
later full adaptive matrix showed a lower complete-path p90 for bands-only and
combined at every workload and size, so the user accepted the bounded band layer.
Physical terminal checks remain pending.

## Phase 4: Stacked live validation

### 4.1 - Validate and keep or revert each accepted layer

- **Assigned Agent**: `gonzales`
- **Assignment Reason**: The final decision joins controlled benchmarks with
  user-visible terminal throughput and CPU measurements.
- **Dependencies**: 1.1 and 2.1; 3.1 only when task 2.1 opens its gate
- **Parallel**: No
- **Blocked By**: Every implemented dependency must pass its own gate
- **Scope**: The accepted changes from tasks 1.1 and 3.1, existing focused Sixel
  benchmarks, live FPS instrumentation, diagnostic evidence, and this
  decision record.
- **Reuse**: Use the shared benchmark protocol, `just run`, `just diagnostics`,
  `just debug-run`, current Sixel metrics, and the existing fast and
  compatibility terminal test process. Do not add runtime configuration.
- **Success Criteria**: Test flat, web, and photo fixtures at 1280x720, 1920x1080,
  and 2560x1600. Run six fresh rotated process pairs, four warm-ups, and at least 30
  samples with `GOMAXPROCS=4` and `GOGC=100`. Run
  `go test -race ./internal/render/sixel`, `just build`, `just test`, and
  `just lint`. On one fast Sixel terminal and one compatibility Sixel terminal, record
  sustained delivered FPS, encode and write p90, process CPU, terminal bytes,
  drops, worst gap, visible tearing, and first contentful paint. Keep each
  optimisation only if its own complete-path gate still passes in the stack and
  live results do not regress. Independently revert mapping or emission when its
  gate fails. Do not retain complexity supported only by a microbenchmark.
- **Complexity**: M
- **Flags**: ⚠️ High-risk, 🔍 Needs review

**Status (2026-07-18): Non-live validation passed; both layers retained.** The
opt-in harness now selects serial map plus emission, mapping-only, bands-only, and
combined arms without changing production defaults. Palette selection and the
RGB555 LUT stay on the production adaptive path in every arm; the arm control
changes only indexed mapping and band emission. All parallel stages use
`sixelWorkerCount() = min(4, GOMAXPROCS)` in sequence. There is no second worker
count, overlapping stage, nested parallelism, or runtime control.

Six fresh rotated processes used four warm-ups, 30 retained samples per arm,
`GOMAXPROCS=4`, and `GOGC=100`. Median process p90 values were:

| Fixture | Size | Serial full | Mapping-only full | Bands-only full | Combined full | Serial PNG + encode | Combined PNG + encode |
|---|---:|---:|---:|---:|---:|---:|---:|
| Flat | 1280x720 | 9.968 ms | 9.241 ms | 8.986 ms | 8.364 ms | 16.319 ms | 14.870 ms |
| Flat | 1920x1080 | 19.115 ms | 17.654 ms | 17.139 ms | 15.268 ms | 33.496 ms | 29.527 ms |
| Flat | 2560x1600 | 16.655 ms | 13.306 ms | 12.114 ms | 8.867 ms | 44.450 ms | 36.952 ms |
| Web | 1280x720 | 24.218 ms | 23.451 ms | 19.160 ms | 18.331 ms | 29.964 ms | 24.148 ms |
| Web | 1920x1080 | 40.127 ms | 38.925 ms | 27.145 ms | 25.916 ms | 52.992 ms | 38.877 ms |
| Web | 2560x1600 | 44.383 ms | 41.567 ms | 25.415 ms | 22.108 ms | 69.350 ms | 48.680 ms |
| Photo | 1280x720 | 15.245 ms | 14.537 ms | 13.758 ms | 12.840 ms | 21.682 ms | 19.199 ms |
| Photo | 1920x1080 | 20.093 ms | 18.675 ms | 16.173 ms | 14.582 ms | 33.499 ms | 28.062 ms |
| Photo | 2560x1600 | 30.192 ms | 27.005 ms | 22.183 ms | 18.989 ms | 56.269 ms | 45.172 ms |

The combined arm lowers full-encode p90 by 15.78% to 50.19% and PNG-decode-plus-
encode p90 by 8.88% to 29.81% against the serial map-and-emission control. The
interaction varies slightly by workload: it compounds the independent p90 savings
on four cells, is additive within 2% on four cells, and interferes by 0.105 ms on
1280x720 flat. It never loses either independent win and remains faster than every
other arm in every cell.

On the representative 1920x1080 photo, median stage p90 spans were 8.057 ms
palette selection, 2.745 ms LUT construction, 2.262 ms serial or 0.890 ms parallel
mapping, and 7.204 ms serial or 3.018 ms parallel emission. The serial and
mapping-only arms used 1,045 allocations; bands-only and combined used 1,047.
Combined added about 608 B/op at this cell. Across the matrix, band parallelism
added two warm allocations, while mapping added about 299 to 410 B/op.

Every arm produced the same payload bytes and SHA-256 per fixture-size cell, and
the `mattn/go-sixel` oracle reconstructed identical pixels. Payloads ranged from
204,469 to 4,162,693 bytes. Focused tests cover arm selection, all four arm
combinations, payload identity, and decoded-pixel identity. The raw process logs
are generated under gitignored `testdata/diagnostics/sixel-task-4.1/`. Reproduce
the experiment with:

```sh
for run in 0 1 2 3 4 5; do
  GOMAXPROCS=4 GOGC=100 NYALA_COMBINED_BENCH=1 \
    NYALA_COMBINED_BENCH_RUN=$run \
    go test -run '^TestCombinedMappingBenchmark$' -count=1 -v \
    ./internal/render/sixel
done
```

No physical fast or compatibility Sixel terminal was available because the user
had phone-only access. This blocks the terminal FPS, write p90, CPU, drops, gap,
tearing, and first-contentful-paint observations, but not the requested non-live
decision. Both accepted layers stay enabled pending later physical-terminal
evidence.

## Payload grammar clean-up

**Status (2026-07-18): Accepted; non-damage optimisation backlog closed.** The
encoder no longer emits `$` immediately after inter-band `-`. Sixel `-` advances
to the next six-row band and returns the active position to the left edge, so the
following carriage return was redundant. Later colour-plane carriage returns are
unchanged.

Production-shaped 1280x720, 1920x1080, and 2560x1600 payload checks save 119,
179, and 266 bytes respectively, one byte for every band boundary. Reconstructed
legacy payloads and the retained payloads decode to exactly equal pixels. Focused
tests pin the byte saving, command structure, decoded bounds, and decoded pixels.
The independent go-sixel and libsixel interop gate accepts the retained streams.
Only the separate damage-aware decision below remains deferred.

## Deferred damage-aware Sixel decision

Production partial updates remain blocked on validated Chrome/CDP damage metadata
attached to the exact screencast frame. Nyala-side decoded-frame differencing is an
offline validation oracle only, not a production fallback. This is not an active
encoder optimisation task. The canonical decision, measurements, correctness
contract, and source links remain in [Nyala TODO, Stage 3 validation](./TODO.md#1-validate-the-stage-3-browser).

## Not planned

Do not reopen these Chafa ideas without new Nyala complete-path and live-terminal
evidence:

- PNN, PCA, or hash-based palette selection and mapping transplants.
- A work-factor mode or a lower sampling budget than the accepted 64K cap.
- 64-column banks, SWAR, or a byte-plane data-layout rewrite.
- Palette-precision deduplication without evidence that duplicate frequency repays it.
- Wholesale Chafa emitter adoption or a return to the 255 repeat ceiling.
- Dithering, an SIMD scaler, temporal palette reuse, or transparency work.
- cgo or native Chafa integration.
- A specialised decimal formatter; the complete-path rejection remains recorded in
  [the performance review](./PERFORMANCE.md#repeat-decimal-formatter-rejection-2026-07-17).
