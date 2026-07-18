capture := ""
backend := ""
scroll := ""
settle := ""
cell := ""

# List available recipes
default:
    @just --list

# Build Nyala
build:
    @go build -ldflags "-s -w" -o nyala ./cmd/nyala

# Build and run Nyala
[positional-arguments]
run url="https://wimpys.world" *flags: build
    #!/usr/bin/env bash
    set -euo pipefail

    capture={{ quote(capture) }}
    backend={{ quote(backend) }}
    scroll={{ quote(scroll) }}
    settle={{ quote(settle) }}
    cell={{ quote(cell) }}
    url="$1"
    shift

    flags=()
    browser=""
    raw_flags=("$@")
    for ((i = 0; i < ${#raw_flags[@]}; i++)); do
        arg="${raw_flags[$i]}"
        case "$arg" in
            capture=*)
                capture="${arg#capture=}"
                ;;
            backend=*)
                backend="${arg#backend=}"
                ;;
            scroll=*)
                scroll="${arg#scroll=}"
                ;;
            settle=*)
                settle="${arg#settle=}"
                ;;
            cell=*)
                cell="${arg#cell=}"
                ;;
            --browser=*)
                browser="${arg#--browser=}"
                ;;
            --browser)
                if ((i + 1 >= ${#raw_flags[@]})); then
                    flags+=("$arg")
                else
                    i=$((i + 1))
                    browser="${raw_flags[$i]}"
                fi
                ;;
            *)
                flags+=("$arg")
                ;;
        esac
    done

    if [ "${FENCE_SANDBOX:-0}" = "1" ] \
        && [ "${FENCE_DEBUG_CHROMIUM_NO_SANDBOX:-0}" != "1" ] \
        && [ "${NYALA_DEBUG_CHROMIUM_NO_SANDBOX:-0}" != "1" ]; then
        export FENCE_DEBUG_CHROMIUM_NO_SANDBOX=1
        printf '%s\n' 'FENCE_SANDBOX=1 detected, setting FENCE_DEBUG_CHROMIUM_NO_SANDBOX=1 for this run.' >&2
    fi

    if [ -n "$browser" ]; then
        flags+=("--browser" "$browser")
    fi

    args=()
    args+=("$url")
    if [ -n "$backend" ]; then
        args+=("--backend=$backend")
    fi
    args+=("${flags[@]}")

    cmd=(./nyala "${args[@]}")
    env_pairs=()
    # cell overrides the terminal cell pixel size (NYALA_CELL_PX="WxH"). It is a
    # diagnostic and harness knob and rides any run, capture or not, so a headless
    # pty run can pin a real terminal's cell size to reproduce device quantisation.
    if [ -n "$cell" ]; then
        env_pairs+=("NYALA_CELL_PX=$cell")
    fi
    if [ -n "$capture" ]; then
        env_pairs+=("NYALA_CAPTURE=$capture")
        # scroll rides only with capture: NYALA_CAPTURE_SCROLL is ignored off the
        # capture path, so setting it without capture would do nothing.
        if [ -n "$scroll" ]; then
            env_pairs+=("NYALA_CAPTURE_SCROLL=$scroll")
        fi
        # settle rides only with capture: NYALA_CAPTURE_SCROLL_SETTLE=0 skips the
        # scroll hook's settle wait so the pre-settle scrolled frame is captured.
        if [ -n "$settle" ]; then
            env_pairs+=("NYALA_CAPTURE_SCROLL_SETTLE=$settle")
        fi
    fi
    if [ ${#env_pairs[@]} -gt 0 ]; then
        cmd=(env "${env_pairs[@]}" "${cmd[@]}")
    fi

    if [ -t 0 ] && [ -t 1 ]; then
        "${cmd[@]}"
    else
        # No TTY (headless or fenced): wrap in script to allocate a pty so the
        # viewer has a terminal. A synthetic pty has no emulator, so graphics
        # backend probes fall back to block. stty fixes a deterministic grid
        # while still honouring caller-exported LINES/COLUMNS.
        if [ -z "$(command -v script)" ]; then
            printf '%s\n' 'script (util-linux) not found; needed for headless run' >&2
            exit 1
        fi
        wrapped="stty rows \"\${LINES:-40}\" cols \"\${COLUMNS:-120}\"; "
        for word in "${cmd[@]}"; do
            wrapped+="$(printf '%q ' "$word")"
        done
        # script runs the wrapped command via $SHELL. The wrapped string is POSIX
        # sh syntax (${LINES:-40} and %q quoting), so force a POSIX shell rather
        # than the caller's login shell, which may be fish and cannot parse it.
        SHELL=/bin/sh script -qec "$wrapped" /dev/null
    fi

# Run Nyala with load-path timing. Interactive tty streams and writes the stderr
# timing to NYALA_TIMING_FILE (default nyala-timings.log); headless runs the bounded
# capture path and print its performance.json load_timings, so it works with no tty.
[positional-arguments]
debug-run url="https://wimpys.world" *flags:
    #!/usr/bin/env bash
    set -euo pipefail

    if [ -t 0 ] && [ -t 1 ]; then
        # Interactive tty: NYALA_DEBUG_TIMING makes the engine print one stderr line
        # per load milestone; just run forwards the ambient env to nyala. The viewer
        # draws to the tty (fd 0), so redirecting stderr (fd 2) captures the timings
        # without touching the TUI. Quit nyala to end the run.
        out="${NYALA_TIMING_FILE:-nyala-timings.log}"
        rc=0
        NYALA_DEBUG_TIMING=1 just run "$@" 2>"$out" || rc=$?
        printf '\nload timings (%s):\n' "$out" >&2
        grep '^nyala timing:' "$out" >&2 || printf '  none captured\n' >&2
        exit "$rc"
    fi

    # No tty (agent, CI, fenced): the interactive viewer never self-exits and script
    # binds stderr to the pty, so run the bounded capture path, which records the load
    # milestones into performance.json by construction. The throwaway bundle is deleted
    # after its timings print; it can hold page content, so it is not kept.
    log="$(mktemp)"
    rc=0
    just capture=1 run "$@" >"$log" 2>&1 || rc=$?
    # The capture prints "capture bundle: <path>" on the pty, glued to the frame
    # escape stream and ending in a carriage return, so match just the path token.
    bundle="$(grep -aoE 'capture bundle: [^[:space:]]+' "$log" | tail -n1 || true)"
    bundle="${bundle#capture bundle: }"
    if [ -n "$bundle" ] && [ -f "$bundle/performance.json" ]; then
        printf 'load timings from %s\n' "$bundle/performance.json" >&2
        cat "$bundle/performance.json"
        rm -rf "$bundle"
    else
        printf 'debug-run: capture produced no bundle; tail of output:\n' >&2
        tail -c 2000 "$log" >&2
        # No bundle means no load_timings, the recipe's whole output. Fail so a CI or
        # agent caller does not read success from a run that produced nothing, even if
        # the capture itself exited 0.
        if [ "$rc" -eq 0 ]; then rc=1; fi
    fi
    rm -f "$log"
    exit "$rc"

# Run Nyala with the live FPS meter. Sets NYALA_FPS_TIMING so the viewer draws a
# ~1 Hz fps badge over the page and prints a summary line to stdout on quit. It
# forwards the same url and flags as just run (backend=, cell=, --browser, ...).
# Interactive tty only: the meter measures the on-screen render, so a headless run
# has nothing to draw. Quit nyala to end the run and print the summary.
[positional-arguments]
fps url="https://wimpys.world" *flags: build
    #!/usr/bin/env bash
    set -euo pipefail

    if ! { [ -t 0 ] && [ -t 1 ]; }; then
        printf 'just fps: needs an interactive terminal; the live meter measures the on-screen render.\n' >&2
        exit 1
    fi
    NYALA_FPS_TIMING=1 just run "$@"

# Capture every corpus fixture on the block backend for a text-layout sweep
corpus:
    #!/usr/bin/env bash
    set -uo pipefail
    root="$(pwd)/testdata/corpus"
    fail=0

    # capture_bundle COLS ROWS DIR CELL: capture one block bundle for the fixture
    # at DIR (the index.html directory) at the given grid, forcing CELL device px
    # per cell when CELL is non-empty. It prints the parsed bundle path on stdout
    # and returns non-zero on a capture failure (tail of the run output goes to
    # stderr). The 240-column screencast is megabytes of escape bytes, so the run
    # output goes to a temp file, not a shell variable, to stay under ARG_MAX. The
    # pty wrapper emits CRLF, so the trailing CR is stripped before use.
    capture_bundle() {
        local cols="$1" rows="$2" dir="$3" cell="$4"
        local out; out="$(mktemp)"
        local cellflag=(); [ -n "$cell" ] && cellflag=("cell=$cell")
        COLUMNS="$cols" LINES="$rows" just capture=1 run "file://${dir}index.html" backend=block "${cellflag[@]}" > "$out" 2>&1
        local status=$?
        if [ "$status" -ne 0 ]; then
            tail -n 20 "$out" >&2
            rm -f "$out"
            return 1
        fi
        local bundle; bundle="$(grep 'capture bundle:' "$out" | tail -n 1)"
        rm -f "$out"
        bundle="${bundle#*capture bundle: }"
        bundle="${bundle%$'\r'}"
        [ -n "$bundle" ] || return 1
        printf '%s' "$bundle"
    }

    # paint_gate FIXTURE_JSON FRAME LABEL: run the paint-referenced displacement
    # gate against FRAME. The gate threshold is the correctness target (mean-abs
    # below 0.5 cell, max-abs below 1.0 cell, both axes), NOT the current build's
    # baseline, so on the unfixed architecture several fixtures breach it. This is
    # the point: the board is honest. A breach is logged as an expected-fail with
    # its real numbers and does NOT set fail, so the sweep exit code stays usable
    # while the architecture is unfixed; every later phase turns these expected-
    # fails green. A run with no source glyph ink is N/A, not a pass. This mirrors
    # the report-the-number discipline the layout-13 scroll leg uses. The paint
    # metric reads screenshot.png, the source page paint, not block-render.png,
    # which folds our overlay on and would cancel. It therefore cannot self-cancel
    # the way placement-score can.
    paint_gate() {
        local fjson="$1" frame="$2" label="$3"
        local numbers measure_status
        numbers="$(go run ./tools/paint-displacement "$frame" 2>/dev/null | grep -E 'measured|skipped|disp')"
        measure_status=$?
        if [ "$measure_status" -ne 0 ]; then
            printf 'paint-displacement ERROR (measurement failed): %s\n' "$label" >&2
            fail=1
            return
        fi
        local measured; measured="$(printf '%s\n' "$numbers" | awk '$1 == "measured" && $2 == "runs:" { print $3 }')"
        if ! [[ "$measured" =~ ^[0-9]+$ ]]; then
            printf 'paint-displacement ERROR (invalid measured-run count): %s\n' "$label" >&2
            fail=1
            return
        fi
        if [ "$measured" -eq 0 ]; then
            printf 'paint-displacement N/A (no source glyph ink): %s\n' "$label"
            printf '%s\n' "$numbers" | sed 's/^/    /'
            return
        fi
        if go run ./tools/paint-displacement -gate -thresholds "$fjson" "$frame" >/dev/null 2>&1; then
            printf 'paint-displacement PASS: %s\n' "$label"
        else
            printf 'paint-displacement EXPECTED-FAIL (architecture unfixed): %s\n' "$label"
            printf '%s\n' "$numbers" | sed 's/^/    /'
        fi
    }

    # Each fixture captures and gates at three grids: the default 120x40, a wide
    # 240x48, and the owner's real terminal geometry 158x86 (cell 16x33 device px,
    # pinned so a headless pty reproduces the device quantisation). Every leg runs
    # the placement-score gate (a cheap regression guard, hard-gated as before) and
    # the paint-referenced displacement gate (the honest arbiter, expected-fail on
    # the current build). The run wrapper honours caller-exported LINES/COLUMNS.
    legs=("120x40 " "240x48 " "158x86 16x33")
    for dir in "$root"/layout-*/; do
        name="$(basename "$dir")"
        for leg in "${legs[@]}"; do
            grid="${leg%% *}"
            cell="${leg#* }"
            cols="${grid%x*}"
            rows="${grid#*x}"
            label="$name [$grid${cell:+ cell $cell}]"
            printf '== %s ==\n' "$label"
            bundle="$(capture_bundle "$cols" "$rows" "$dir" "$cell")" || {
                printf 'capture failed: %s\n' "$label" >&2
                fail=1
                continue
            }
            frame="$(printf '%s' "$bundle"/frames/frame-*/)"
            if ! go run ./tools/placement-score -gate -thresholds "${dir}fixture.json" "$frame"; then
                printf 'placement gate failed: %s\n' "$label" >&2
                fail=1
            fi
            paint_gate "${dir}fixture.json" "$frame" "$label"
        done
    done

    # layout-13 real-geometry scroll leg. It captures three frames then scrolls
    # 600 CSS px and settles, so the bundle holds consecutive snapshot generations
    # across a scroll. The scroll-dance gate hard-fails on danced 0: the metric
    # classifies the position:fixed nav as fixed (viewport-stationary on scroll)
    # and scores every other run on its own scroll-compensated document row, so a
    # fixed nav and scrolling body content each read honestly. Whole-cell scroll
    # lands the settle re-snapshot's runs on the resting bitmap's scroll and
    # composeFrame pairs the two on scroll equality, so no run hops one cell at
    # settle. Only layout-13 carries the scroll leg; the placement and paint legs
    # above cover every fixture at all three geometries.
    lm="$root/layout-13-linuxmatters/"
    if [ -d "$lm" ]; then
        printf '== layout-13-linuxmatters [158x86 cell 16x33 scroll=600] ==\n'
        out="$(mktemp)"
        COLUMNS=158 LINES=86 just capture=3 scroll=600 run "file://${lm}index.html" backend=block cell=16x33 > "$out" 2>&1
        status=$?
        if [ "$status" -ne 0 ]; then
            tail -n 20 "$out"
            rm -f "$out"
            printf 'scroll capture failed: layout-13-linuxmatters [158x86 cell 16x33 scroll=600]\n' >&2
            fail=1
        else
            bundle="$(grep 'capture bundle:' "$out" | tail -n 1)"
            rm -f "$out"
            bundle="${bundle#*capture bundle: }"
            bundle="${bundle%$'\r'}"
            if ! go run ./tools/scroll-dance -gate -thresholds "${lm}fixture.json" "$bundle"; then
                printf 'scroll-dance gate failed: layout-13-linuxmatters [158x86 cell 16x33 scroll=600]\n' >&2
                fail=1
            fi
        fi
    fi

    exit "$fail"

# Run all local checks
check:
    @just tidy-check
    @just fmt-check
    @just lint
    @just test
    @just build
    @just release-check
    @nix flake check
    @just vuln

# Run the heavy quality gate: sweep, calibration, golden track, and overlay grade
diagnostics:
    @go test -v -tags diagnostics -count=1 ./internal/harness/

# Cross-check the production Sixel encoder with libsixel's independent decoder
sixel-interop:
    #!/usr/bin/env bash
    set -euo pipefail
    if ! command -v sixel2png >/dev/null 2>&1; then
        printf '%s\n' 'sixel2png not found; enter the Nix dev shell with nix develop' >&2
        exit 1
    fi
    go test -v -run '^TestLibsixelRoundTrip$' -tags sixelinterop -count=1 ./internal/render/sixel/

# Cross-check the production Block terminal stream with an independent VT model
block-interop:
    @go test -v -run '^TestBlockTerminalRoundTrip$' -tags blockinterop -count=1 ./internal/viewer/

# Cross-check the production Kitty viewer stream with an independent terminal model
kitty-interop:
    @go test -v -run '^TestKittyTerminalRoundTrip$' -tags kittyinterop -count=1 ./internal/viewer/

# Regenerate golden archives via the golden track writer
update-golden:
    @UPDATE_GOLDEN=1 go test -run 'TestGoldenTrack|TestOverlayGoldenTrack' -tags diagnostics -count=1 ./internal/harness/

# Regenerate the overlay artefact PNGs into testdata/diagnostics/ for eyeballing
overlay:
    @OVERLAY_ARTEFACTS=1 go test -run TestOverlayMatcherRoundTrip -tags diagnostics -count=1 ./internal/harness/

# Rasterise a captured terminal-model.json (or frame dir) into block-render.png beside it
rasterise path:
    @go run ./tools/rasterise {{path}}

# Score horizontal text placement error from a captured text-map.json (or frame dir)
placement-score path:
    @go run ./tools/placement-score {{path}}

# Score paint-referenced glyph displacement from a captured frame dir against screenshot.png
paint-displacement path:
    @go run ./tools/paint-displacement {{path}}

# Score overlay re-placement (scroll dance) across a capture bundle's frame sequence
scroll-dance path:
    @go run ./tools/scroll-dance {{path}}

# Print the KMSCON quadrant test card to the TTY (owner-verified, off CI)
kmscon-testcard:
    @go run ./tools/kmscon-testcard

# Regenerate the committed testdata/overlay/*.runs fixtures
genruns:
    @go run ./tools/genruns

# Rebuild the test page PNGs from the committed PDFs at 150 DPI for reproducibility
regen-fixtures:
    #!/usr/bin/env bash
    set -euo pipefail
    for name in testpage-01-grayscale testpage-02-basic testpage-03-cmyk testpage-04-color; do
        pdftoppm -png -r 150 -singlefile "testdata/${name}.pdf" "testdata/${name}"
    done

# Format Go sources
fmt:
    @golangci-lint fmt

# Check Go formatting
fmt-check:
    @golangci-lint fmt --diff --color=never

# Run linters
lint:
    @go vet ./...
    @gocyclo -top 20 -avg -ignore '_test\.go$' .
    @ineffassign ./...
    @golangci-lint run
    @actionlint

# Check GoReleaser config
release-check:
    @goreleaser check

# Build a local snapshot release
release-snapshot:
    @goreleaser release --snapshot --clean --skip=sign

# Run tests with coverage
coverage:
    #!/usr/bin/env bash
    set -euo pipefail
    coverage_file="${TMPDIR:-/tmp}/nyala-coverage.out"
    go test -coverprofile="$coverage_file" -covermode=atomic ./...
    go tool cover -func="$coverage_file"

# Run tests
test:
    @go test ./...

# Run the live engine integration test (needs Chromium; off CI, not in `test`)
live:
    @FENCE_DEBUG_CHROMIUM_NO_SANDBOX="${FENCE_SANDBOX:-0}" go test -tags live ./internal/engine/

# Measure internal renderer-to-terminal-byte costs from production CDP frames.
internal-render-metrics:
    # The fenced Chromium wrapper owns its no-sandbox diagnostic switch.
    @FENCE_DEBUG_CHROMIUM_NO_SANDBOX="${FENCE_SANDBOX:-0}" NYALA_INTERNAL_RENDER_METRICS=1 go test -tags live -run '^TestInternalRenderingMetrics$' -v -count=1 ./internal/viewer/

# Compare Sixel palette implementations on the committed still-image matrix.
palette-bench:
    #!/usr/bin/env bash
    set -euo pipefail
    for run in 0 1 2 3 4 5; do
        NYALA_PALETTE_BENCH=1 NYALA_PALETTE_BENCH_RUN="$run" \
            go test -run '^TestPaletteBenchmark$' -v -count=1 ./internal/render/sixel/
    done

# Tidy Go modules
tidy:
    @go mod tidy

# Check Go modules are tidy
tidy-check:
    #!/usr/bin/env bash
    set -euo pipefail
    go mod tidy
    if [ -n "$(git status --porcelain -- go.mod go.sum)" ]; then
        git diff -- go.mod go.sum
        git status --short -- go.mod go.sum
        exit 1
    fi

# Run Go vulnerability checks
vuln:
    @govulncheck ./...

# Create a new release tag (requires VERSION=x.y.z)
release VERSION:
    #!/usr/bin/env bash
    set -euo pipefail

    if ! [[ "{{VERSION}}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        echo "Error: VERSION must be in format x.y.z (e.g. 0.1.0)"
        exit 1
    fi

    if [ -n "$(git status --porcelain)" ]; then
        echo "Error: Working directory is not clean"
        exit 1
    fi

    if git show-ref --tags --verify --quiet "refs/tags/v{{VERSION}}"; then
        echo "Error: Tag v{{VERSION}} already exists"
        exit 1
    fi

    echo "Creating release v{{VERSION}}..."
    git tag -a "v{{VERSION}}" -m "v{{VERSION}}"
    echo "Tag v{{VERSION}} created"
    echo ""
    echo "To publish the release:"
    echo "  git push origin v{{VERSION}}"
    echo ""
    echo "This release is configured for GoReleaser to:"
    echo "  - Cross-compile binaries for linux and darwin (amd64, arm64)"
    echo "  - Generate changelog from commits"
    echo "  - Create GitHub release assets with bare binaries, tarballs, and checksums"
    echo "  - Build native packages (deb, rpm, apk, archlinux)"
    echo "  - Update the Homebrew tap (wimpysworld/homebrew-tap)"
    echo "  - Update the AUR package (nyala-bin)"
    echo "  - Generate the Nix package (pkgs/nyala/default.nix)"
    echo "  - Build and publish Docker images (ghcr.io/wimpysworld/nyala)"
    echo "  - Sign checksums with cosign (keyless)"
    echo "  - Sign Docker images with cosign (keyless)"
