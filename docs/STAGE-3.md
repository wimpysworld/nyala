# Stage 3 - Live screencast and DOM text

Stage 3 turns Nyala into a web-first binary. `nyala <url>` launches a managed
headless Chromium, streams a live screencast over the Chrome DevTools Protocol,
extracts the page text layer from the DOM, folds crisp Unicode text onto each
bitmap frame, and pushes the frame through the existing viewer render path. It is
the first concrete implementation of the `Engine` seam, `ChromiumEngine`, and it
replaces the static PNG source from Stages 0 through 2. The make-or-break point is
crisp DOM text composited over a live screencast: the bitmap shows what the DOM
cannot (images, canvas, video, CSS visuals) and the overlay paints real glyphs
where the bitmap floor fails. This document is the durable record of the completed
work. The target architecture is in [`OVERVIEW.md`](./OVERVIEW.md); the built
foundations are in [`STAGE-0.md`](./STAGE-0.md), [`STAGE-1.md`](./STAGE-1.md), and
[`STAGE-2.md`](./STAGE-2.md).

## Status summary

Stage 3 is code-complete. The `internal/engine` package, the capture-size helper,
the viewer push consumer, the CLI rewrite, the Nix runtime browser, and the live
test are in the tree.

The gates are green. `just build` builds the cgo-free binary. `go test ./...`
passes, and `go test -race` on the changed packages (`engine`, `render`, `viewer`,
`app`) passes with the race detector on. `golangci-lint` reports zero issues, and
`gofmt` reports no diffs. The two new dependencies (`chromedp`, `cdproto`) are pure
Go, so the shipped binary stays cgo-free.

Acceptance criteria are met (see the table below). The live-tagged integration
test and the binary launch a real Chromium, which CI does not run; both stay behind
the `live` build tag and the `just live` recipe.

What remains is post-build validation, not new code. The measured spikes feed
future tuning, not a gate: sustained FPS on KMSCON and the target emulators, the
`scrollMatchTolerancePx` frame-sync tolerance, the shared-memory transmit cost, and
the advisory DOM box drift. The CEF fallback stays documented and gated on the FPS
spike.

## Objective and scope

Objective: make `nyala <url>` the only entry point, sourcing each frame from a live
Chromium screencast with the crisp DOM text layer synced on.

In scope, delivered:

- The `Engine` seam and one concrete `ChromiumEngine` over `chromedp`.
- The screencast start, ack, decode loop with a latest-wins single-frame slot.
- The DOM snapshot decode to positioned glyph runs, with colour.
- Frame sync between the bitmap and the text on the scroll offset.
- Scroll input: arrows, PageUp, PageDown, and the wheel, with the engine owning
  the offset.
- The viewer push consumer that draws each pushed frame, plus the CLI rewrite to a
  `URL` arg and the deletion of the static PNG surface.
- Managed-Chromium launch, executable discovery, and crash recovery.
- The shared capture-size helper, the Nix runtime browser, and the live test.

Out of scope, deferred, each with the bar to promote it:

- **CEF engine.** A documented fallback that embeds Chromium in-process for BGRA
  frames with no CDP hop. It bakes Chromium into the binary, needs C/C++ bindings
  against the cgo-free stack, and gives no text geometry. Promote only if the
  post-build FPS spike shows CDP cannot hold an acceptable frame rate.
- **Damage-rectangle partial emission.** Full-frame redraw is the Stage 3 baseline.
  Promote when sustained per-frame terminal write latency exceeds the frame budget
  at the target FPS (33 ms at 30 FPS as the provisional default), with
  `bytesPerFrame` as the secondary signal.
- **Remote attach (`NewRemoteAllocator`).** Launch is the only model, because the
  browser-side flags (DSF 1, window size) apply only to a process Nyala starts.
  Promote when a locked-down host needs to drive an already-running browser.
- **Tabs.** The seam keeps the page behind one interface. Promote with a child
  `NewContext` bound by `WithTargetID`, no allocator change.
- **Link hinting and a separate `TextLayer()` channel.** Stage 3 carries text on
  one synced `Frames()` channel. Promote a second channel in Stage 4 when link-hint
  geometry needs its own cadence.
- **The Sixel and Kitty overlay, full Bubble Tea chrome, history, and address
  entry.** Stage 4.

## Work as built

### The Engine seam and ChromiumEngine

`Engine` (`engine.go`) is one interface: `Navigate(url) error`, `Resize(cols,
rows)`, `Frames() <-chan render.Frame`, `SendKey(Key)`, `SendMouse(MouseEvent)`,
and `Close() error`. One synced channel delivers the bitmap with its crisp text
already folded on `Frame.Text`, so the consumer does no two-cadence reconcile. The
`Key` and `MouseEvent` types are terminal-neutral; scroll uses wheel deltas in CSS
pixels. `ChromiumEngine` (`chromium.go`) is the one concrete engine. It owns a
`chromedp` context, the URL, the scroll offset, the resolved cell px, the active
backend, the capture size, the latest-wins slot, the ack-pump channel, and the
overlay publisher. The last-matched text lives in the snapshot goroutine, not engine
state. It holds the URL and scroll offset in its own state, not inside `chromedp`, so
it rebuilds and re-navigates after a crash. The loop, the snapshot decode, the sync,
the launch, and the recovery live in their own files so `chromium.go` stays thin
orchestration.

All shared session and geometry state is guarded by one mutex. Every method that
touches it locks, copies the fields into locals or writes the updates, unlocks,
then runs the `chromedp` I/O on the locals. The mutex is never held across a
`chromedp.Run` or a channel send. This removes the data race found in validation
between the viewer goroutine and the supervisor goroutine.

### The screencast ack loop and the latest-wins slot

`screencast.go` implements the listen, decode, ack loop that `chromedp` ships no
helper for. `startScreencast` starts a JPEG stream capped at the active capture
size and registers a `ListenTarget` handler. On each `EventScreencastFrame` the
handler decodes the image and puts it in `frameSlot`, the latest-wins single-frame
backpressure slot. Chrome withholds frames until acked, so the ack is the
backpressure: `put` acks the displaced frame when it replaces an unconsumed one,
and `consume` acks the frame it takes. Every frame is acked exactly once, on
consume or on replace, and Chrome's production rate couples to the terminal draw
rate. `frameSlot` is the pure, browser-free core: `put`, `consume`, and `drain`
take an `ackFunc`, so the ack accounting is assertable without a browser. `drain`
marks the slot dropped, acks any held frame, and unblocks a parked consumer, so
`Close` leaves no frame unacked. A corrupt frame is acked and dropped so the
stream never stalls.

The ack never runs inline on the `ListenTarget` event goroutine. A `chromedp.Run`
there is the chromedp #920 self-deadlock: the ack command waits for a response that
only the parked event goroutine can read, so the event pump freezes and Chrome stops
sending frames after the first. The production `ackFunc` instead enqueues the
`SessionID` onto a buffered channel, and one `ackPump` goroutine drains it and runs
`page.ScreencastFrameAck` off the event goroutine. The enqueue is select-with-default,
so a full channel or a torn-down session drops the ack rather than parking the caller;
a dropped ack only delays one frame's credit. The pump exits on context cancel, so it
never outlives a session.

### The DOM snapshot decode

`snapshot.go` runs one `DOMSnapshot.captureSnapshot`, requesting the computed
`color` style and the paint order in one round-trip. The snapshot runs on its own
goroutine (`runTextSync`), decoupled from the frame-emit loop, so snapshot latency
never withholds acks or stalls frame emission; `composeFrame` reads the latest
published runs. The pure `decodeGlyphRuns` maps the snapshot documents and the shared
string table to `[]render.GlyphRun` with no browser dependency, so it is unit-testable
on canned structs. It walks every inline text box, converts each box's device-pixel
bounds to CSS pixels by dividing by the device pixel ratio, places each run at the
snapshot's own scroll offset, and bins the box top-left to a cell with the Stage
2 floor rule. The snapshot bounds and the scroll come from one DOM read, so they are
internally consistent and a sustained scroll shows the current text. Each box becomes
one `GlyphRun` clipped to `floor(boxWidthPx/cellW)`
display cells, counting cells with the Stage 2 width rule so a width-2 cluster is
never half-emitted; overflow graphemes are dropped because the bitmap underneath
shows them. The box's computed colour feeds `GlyphRun.FG` in RGBA channel order.
The box text is the UTF-16 substring of its layout node given by the box start and
length. DSF is pinned to 1, so the DPR divide is a no-op in production, but the pure
decoder stays general so tests pass other ratios.

### Frame sync between bitmap and text

The bitmap and the snapshot share no frame id, so the text is placed by
construction, not matched by scroll equality. The snapshot goroutine decodes each
run at the snapshot's own scroll offset, so the runs are internally consistent and a
sustained scroll shows the current text. `composeFrame` folds the latest published
runs onto the bitmap; the overlay may lag the bitmap by a frame, which the
place-by-scroll model tolerates. The pure `holdDecision` (`sync.go`) is one bounded
error hold: a successful snapshot emits the fresh runs and records them as the
last-matched; a failing read holds the last-matched runs for at most
`holdStalenessLimit` of continuous failure, then drops to a bitmap-only frame rather
than show text that no longer matches the page. No text buffer is kept: the
latest-wins slot plus the published runs collapses the two cadences into one.
`holdStalenessLimit` is a named constant set to 250 milliseconds, and
`scrollMatchTolerancePx` (set to 1, now a pure compare helper off the hot path) sits
beside it, both isolated for the post-build tuning spike.

### Scroll input

Scroll input is one transport. `SendKey` routes scroll keys through
`scrollKeyDelta` (`input.go`): arrows step 40 CSS px, Page keys step the viewport
height. `SendMouse` routes wheel deltas. Both reach `dispatchWheel`, which advances
the engine's owned scroll offset and dispatches one `input.DispatchMouseEvent`
`mouseWheel` at the viewport centre. `synthesizeScrollGesture` is deliberately not
used, to avoid touch synthesis and Chromium's focus-dependent key-to-scroll
defaults. A non-scroll key routes to `DispatchKeyEvent` for a focused element to
receive. The engine owns the offset as the single source of truth; crash recovery
re-applies the held offset after re-navigate.

### Push consumer and viewer changes

`internal/viewer/stream.go` is the push consumer. `Stream` enters raw mode, runs
`configureBackends`, hands the resolved backend and cell px to the engine, writes a
plain-text `loading <url>` cue, navigates, then draws each `Frames()` value through
the reused `State`, `RenderStreamFrame`, and `writeGrid`. It restarts the stream on
SIGWINCH at the recomputed capture size, forwards scroll keys and the wheel to the
engine, cycles the backend on Tab, and restores the terminal on exit. A dedicated
control goroutine runs the engine restarts (Resize, SetBackend), off the consumer
loop, so a slow `restartStream` cannot freeze draw, keys, and quit; the engine frame
channel is buffered so a transient restart stall does not park the producer. The viewer
declares its own local `Engine` interface, so it never imports the engine package;
the dependency runs one way. `app` supplies an `engineAdapter` that maps the
viewer's string key code to the engine's `Key`. The viewer recognises scroll keys
through `decodeCSI` and surfaces them on `Action.Scroll`; it owns no scroll state.
`LoadPNG` and the static redraw-on-event `Run` loop are deleted.

### CLI

`internal/app/app.go` takes a required `URL` arg and a `--browser` flag. `Run`
prepends `https://` when the URL has no scheme, accepts `http`, `https`, and `file`,
rejects any other scheme, then constructs the engine and runs the viewer push loop.
A load failure renders Chromium's own error page through the screencast, so Nyala
needs no custom error UI.

### Launch and Chromium discovery

`launch.go` resolves the executable in order: the `--browser` flag, then
`NYALA_BROWSER`, then `CHROME_PATH`, then a PATH lookup over the common Chromium and
Chrome names, failing fast with an actionable error naming the search order.
`resolveExecutable` is pure beyond reading the environment and PATH, so it is
hermetically testable. `newAllocator` builds a `chromedp.NewExecAllocator` with the
launch `--window-size` from the real CSS-pixel viewport (`surfaceSize`) and
`force-device-scale-factor` 1. Sizing splits into two values: `surfaceSize`
(`cols*cellW` by `rows*cellH`, the layout viewport) and the per-backend
`captureSize` (the client-side downscale target, the block sub-cell sample grid).
`launch` fetches the browser window id once via `GetWindowForTarget`, enables
lifecycle events with `enableLifecycle`, then sizes the layout and the surface
together with `applySizing`. `applySizing` sets the CSS viewport via
`SetDeviceMetricsOverride` at DSF 1, then sizes the captured content area
authoritatively with `Browser.SetContentsSize` on the window id, because the
override sizes only the layout viewport, not the captured surface; without the
surface call frames arrive cropped or blank. `enableLifecycle` calls `page.Enable`
and `SetLifecycleEventsEnabled(true)` per session, because CDP emits
`Page.lifecycleEvent` only after it, and the navigate waiter needs that event to
match a loaderID. `restartStream` re-applies the surface sizing and the screencast
cap together and restarts the screencast on a resize or a backend switch.

### Crash recovery

`recovery.go` runs a supervisor goroutine that owns the session. `supervise` waits
for a crash (`Target.targetCrashed`), a lost-connection context cancellation, or
`Close`. On a crash or lost connection it tears the dead session down and rebuilds
from held state, re-navigating to the held URL and re-applying the held scroll
offset, so frames resume where the crash left off. `navigateAsync` drives the
navigation through `chromedp.Navigate`, the full `responseAction` load waiter, on
its own goroutine after the screencast and frame loop are live, so startup stays
non-blocking and Ctrl-C stays live during launch. The waiter learns the loaderID
from the `init` lifecycle event, buffers pre-loaderID events, matches the top frame,
and finishes on the matched load signal. The held scroll offset is re-applied only
after that load signal, so the document exists before the wheel lands. A navigate
errorText renders the browser's own error page rather than tearing the session down,
so the supervisor never rebuilds on a failed URL. On the capture path the load
signal fires the paint latch. The supervisor is the single
owner of the session fields, so no teardown races with `Close`. `Frames()` is
created once in `Navigate` and closed exactly once, by the supervisor, on `Close`;
it survives a rebuild, so the consumer keeps reading across a recovery. `Close`
signals the supervisor to stop rather than rebuild, guarded by a `sync.Once` so a
second `Close` is a no-op. The one-shot `captureFrame` path was removed;
`SmokeStaticFrame` (live tag, `smoke_live.go`) consumes one frame from `Frames()`
and renders it once.

### Capture-size helper

`internal/render/geometry.go` holds two free functions so the viewer and the engine
share the maths with no import cycle. `BlockCaptureSize(quadrant bool, cols, rows)`
returns the sub-cell extent: `(cols*2, rows*4)` for quadrant, `(cols, rows*2)` for
half-block. It takes a bool, not a `block.Scheme`, because the block package imports
render, so render must not import block. `GraphicsCaptureSize(cols, rows, cellW,
cellH)` returns `(cols*cellW, rows*cellH)` for the graphics backends. The engine's
`captureSize` derives the size itself on every resize and backend switch from the
resolved cell px and the active backend, never passed in from the viewer. A
quadrant 80x24 grid captures 160x96.

### Nix and dependencies

`go.mod` pins `chromedp v0.15.1` and `cdproto v0.0.0-20260427013145-5737772c319b`,
both pure Go. `flake.nix` adds `pkgs.chromium` to the dev shell and wraps the
binary with `--set-default CHROME_PATH "${pkgs.chromium}/bin/chromium"`, both gated
behind `isLinux` because nixpkgs `chromium` is Linux-only; on Darwin the build
proceeds with no Chromium and falls through to flag, environment, and PATH
discovery. `pkgs.makeWrapper` is in the Linux `nativeBuildInputs`.

### Tests

The pure cores are unit-tested without a browser: the box decode and binning
(`snapshot_test.go`), the bounded error-hold decision (`sync_test.go`), the slot ack
accounting with a fake event source (`screencast_test.go`), executable discovery and
the per-backend capture size (`launch_test.go`), the image decode channel order
(`frame_test.go`), and the capture-size maths (`render/geometry_test.go`).
`concurrency_test.go` drives the viewer-facing methods against a supervisor
stand-in under `-race` to assert the mutex guards every shared field. The live
integration test (`live_test.go`) carries `//go:build live`, launches a real
Chromium against a self-contained `data:` URL, and asserts at least one synced frame
arrives with non-empty text. It runs via `just live`, stays off `just test` and CI,
and skips with a clear message when no Chromium resolves.

### Bounded capture (diagnostic tooling)

A later pass added bounded capture over the Stage 3 seams, so an agent loop or a
person can inspect a run that drew nothing it can see. It records what Chromium
sent, what Nyala built, and the exact bytes Nyala wrote to the terminal. It is
diagnostic capture, not benchmarking.

Two controls drive it. `--backend=auto|block|kitty|sixel` (default `auto`) forces
one renderer; `auto` keeps the detect-and-cycle behaviour. A named graphics backend
is validated after terminal detection and before navigation, so it fails fast when
the terminal cannot run it. `NYALA_CAPTURE=n` enables capture: unset or `0` is off,
`1` writes a first-frame bundle, and a positive `n` draws `n` usable frames, writes
one `nyala-capture-*` bundle directory, prints the path, then exits. A usable frame
is one `render.Frame` the engine delivers with a decoded bitmap, so frames the
latest-wins slot drops never count. A full capture exits 0; a partial run (the
first-draw deadline, the per-frame idle timeout, a forced-backend rejection, or the
frame channel closing early) writes what it has and exits non-zero. Tab cycling is
off in capture mode.

The work sits in four places, each behind the existing seams:

- **The render-measurement carrier.** `render.FrameStats` rides on `render.Frame`
  (`render.go`). The engine fills it with its own phase timings and sizes
  (screencast payload bytes, screencast decode, DOM snapshot) only when capture is
  enabled; `Frame.Stats` is nil on the production path, so live frames pay nothing.
  The viewer times its own draw phases separately.
- **The capture driver.** `internal/viewer/capture.go` adds `viewer.Capture`, a
  sibling to `viewer.Stream` that `app.Run` selects when `NYALA_CAPTURE` is set. It
  shares the setup and the per-frame `RenderStreamFrame` call but runs a bounded loop
  with no key reader and no Tab, keeping the interactive `Stream` path untouched. It
  tees the terminal writer through `io.MultiWriter(tty, rawFile)` at the one render
  call, so `terminal-stream.raw` gets the exact emitted frame bytes with no renderer
  change.
- **The CDP recorder.** `internal/engine/capture_record.go` adds
  `engine.NewCaptureRecorder`, enabled before `Navigate`. It writes a fixed whitelist
  of events to `cdp-events.ndjson` from the engine's existing listeners and call
  sites, never a firehose, and excludes image bytes; the decoded bitmap lives in
  `screenshot.png`. `app` constructs the recorder so the viewer keeps no engine
  import.
- **The bundle writer.** `internal/capture` writes the bundle: root `manifest.json`,
  `report.md`, `performance.json`, `terminal-stream.raw`, `cdp-session.json`,
  `cdp-events.ndjson`, and per-frame `frames/frame-NNNNNN/` with `screenshot.png`,
  `text.txt`, `cdp-payload.json`, `text-map.json`, and `terminal-model.json` (the
  composed block cell grid, block backend only; the graphics backends omit it). The
  default directory is a fresh `os.MkdirTemp` with the `nyala-capture-*` prefix.

The bundle records page pixels, page text, URLs, and terminal bytes with no
redaction, and launch flags strip secrets and proxy credentials. It is local
evidence that can expose private data, so it is safe to share only after the user
reviews it.

## Decisions recorded with rationale

- **One synced `Frames()` channel, not two.** The engine folds the matched text onto
  `Frame.Text` after the sync, so each frame carries the bitmap and its keyed text.
  This drops the two-cadence reconcile from the consumer. A separate text channel
  returns in Stage 4 for link-hint geometry.
- **Latest-wins slot, ack as backpressure.** Chrome withholds frames until acked, so
  acking on consume or replace couples its production rate to the draw rate and keeps
  one current frame, the single gotcha that makes the feature look broken in bug
  reports.
- **JPEG screencast at quality 90.** The bitmap downsamples into 2-colour cells, so
  JPEG loss sits below cell visibility, and the smaller payload suits the moving
  viewport. PNG stays a diagnostics-only A/B for the damage diff.
- **Capture snapshot per consumed frame, not on a timer.** The cost tracks the draw
  rate, the geometry matches the bitmap, and it keys the sync cleanly.
- **Place text by the snapshot's own scroll, not match by scroll equality.** The
  bitmap and the snapshot share no frame id, so the snapshot goroutine decodes each
  run at the snapshot's own scroll offset and `composeFrame` folds the latest runs on.
  A snapshot read failure holds the last-matched runs for a bounded time, then drops to
  bitmap-only, so crisp text never composites stale over a page it no longer matches.
- **Launch managed Chromium, not remote attach.** The browser-side flags (DSF 1,
  window size) apply only to a process Nyala starts. DSF 1 plus a small viewport is
  the clean CDP equivalent of Carbonyl's compiled-in LoDPI hack.
- **Scroll over one wheel transport, the engine owns the offset.** Keys and the wheel
  both map to `DispatchMouseEvent` `mouseWheel`, avoiding focus-dependent key-to-scroll
  defaults and the experimental `synthesizeScrollGesture`.
- **Capture size as a bool, in a neutral home.** `BlockCaptureSize` takes a quadrant
  bool, not a `block.Scheme`, because render must not import block (the block package
  imports render). The viewer and engine both call the shared functions, so the maths
  never drifts across the seam.
- **The viewer declares its own `Engine` interface; `app` adapts.** The viewer never
  imports the engine package, keeping the dependency one way and avoiding a cycle.
- **A supervisor goroutine owns the session.** It is the single owner of the session
  fields, so a teardown never races `Close`, and `Frames()` survives a rebuild because
  only the supervisor closes it.
- **Shared state under one mutex, lock-copy-unlock then I/O.** The mutex removes the
  viewer-loop-versus-supervisor data race without holding a lock across a
  `chromedp.Run` or a send.
- **Live test behind a `live` tag.** Engine integration needs Chromium and a display
  surface CI may lack, so the live test stays off CI and `just test`, mirroring the
  `ocr` single-token tag, while the pure cores keep `just test` browser-free.

## Acceptance criteria and how each was met

| Criterion | Met by |
|---|---|
| Build, test, lint, tidy pass; cgo-free | `just build`, `go test ./...`, `golangci-lint` (0 issues), `gofmt`; `chromedp`/`cdproto` are pure Go |
| `nyala host` prepends `https://`, accepts `http`/`https`/`file`, rejects others, no-arg errors | `app.normaliseURL`; `TestNormaliseURL`, `TestNormaliseURLUnsupportedScheme`, `TestParseURLRequired`, `TestRunInvalidURLReturnsSchemeError` |
| Load failure shows Chromium's own error page | screencast renders the browser error page; no custom error UI in the engine |
| One static end-to-end frame: navigate, snapshot, bin, composite, render | `SmokeStaticFrame` (live tag) consumes one frame from `Frames()` and renders it once |
| Stream does not stall; every frame acked exactly once | `frameSlot` ack-on-consume-or-replace; `TestFrameSlotInterleavedAckedExactlyOnce`, `TestFrameSlotConcurrentPutsAckedExactlyOnce` |
| Box decode: DPR divide, place at snapshot scroll, clip to box width, no half wide cluster | `decodeGlyphRuns`; `TestDecodeGlyphRunsBinning`, `TestDecodeGlyphRunsClip`, `TestDecodeGlyphRunsColour` |
| Sync: success emits fresh runs, failure holds last-matched then drops to bitmap-only past the bound | `holdDecision`; `TestHoldDecision*` |
| Discovery order and per-backend capture size | `resolveExecutable`, `captureSize`; `TestResolveExecutablePrecedence`, `TestResolveExecutableNoneResolves`, `TestCaptureSizeBlock`, `TestCaptureSizeGraphics`, `TestBlockCaptureSize`, `TestGraphicsCaptureSize` |
| Scroll changes the viewport; engine owns the offset | `dispatchWheel` advances the held offset and dispatches one `mouseWheel`; verified live |
| Resize restarts the stream at the recomputed size, no stale cells | `streamSession.resize` plus `Resize`/`restartStream`; verified live |
| Crash recovery rebuilds and re-navigates from held state | `supervise`/`rebuild`/`startSession` restore URL and scroll offset; verified live |
| Block backend renders the screencast with the overlay; Tab still cycles | `RenderStreamFrame` block path; `SetBackend` restarts at the new capture size; verified live |
| No data race across the viewer and supervisor goroutines | `TestEngineFieldsRaceFree` under `-race` |

## Risks identified and how they were resolved or carried forward

- **Engine god-object.** Resolved. The loop (`screencast.go`), the snapshot decode
  (`snapshot.go`), the sync (`sync.go`), the launch (`launch.go`), and the recovery
  (`recovery.go`) are separate files with pure, testable cores; `chromium.go` stays
  thin orchestration.
- **Capture-size logic duplicated across the seam.** Resolved. The maths lives once
  in `render.BlockCaptureSize`/`render.GraphicsCaptureSize`; the viewer and engine
  both call it.
- **The data race between the viewer loop and the supervisor.** Found in validation,
  resolved. One mutex guards every shared session and geometry field, lock-copy-unlock
  then `chromedp` I/O, asserted by `TestEngineFieldsRaceFree` under `-race`.
- **Channel and goroutine leaks on `Close`.** Resolved. `drain` acks any held frame
  and unblocks a parked consumer, the context cancel releases the send select, the
  supervisor closes `Frames()` exactly once, and `closeOnce` guards a double `Close`.
- **JPEG chroma noise read as spurious cell damage.** Resolved by scope. Full-frame
  redraw is the Stage 3 baseline, so the cell diff is not used for emission;
  damage-rect partial emission is deferred.
- **Frame-sync tolerance and overlay staleness unmeasured.** Resolved structurally,
  carried forward to tune. `holdStalenessLimit` (250 milliseconds) bounds the error
  hold and `scrollMatchTolerancePx` (set to 1) stays the pure compare helper; the live
  spike tunes both.
- **Live-browser CI flakiness.** Resolved. The pure cores keep `just test` and CI
  browser-free; the live test sits behind `//go:build live` and `just live`, off CI.
- **Sustained FPS and the CEF fallback.** Carried forward as accepted. FPS on KMSCON
  and the emulators is post-build measurement; CEF stays the documented fallback gated
  on that number.
- **Damage-rect escalation threshold, shm transmit cost, advisory DOM box drift.**
  Carried forward as accepted measurements that feed future tuning: the damage-rect
  gate on per-frame write latency (33 ms at 30 FPS provisional), the per-frame
  `shm_open`/mmap cost against a reused mapping, and the sub-cell box jitter past the
  clip rule.
