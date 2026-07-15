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
future tuning, not a gate: sustained FPS on KMSCON and the target emulators,
terminal-side PNG decode and draw latency, the `scrollMatchTolerancePx` frame-sync
tolerance, and the advisory DOM box drift. The CEF fallback stays documented and
gated on the FPS spike. [`TODO.md`](./TODO.md) is the canonical index.

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
- **Graphics-backend damage-rectangle (sub-image) emission.** The block path diffs
  the grid and emits only changed cells; the Sixel and Kitty backends still redraw
  the full image per frame. Promote sub-image emission when sustained per-frame
  terminal decode, draw, or write latency exceeds the 16.67 ms target for 60 FPS,
  with `bytesPerFrame` as the secondary signal.
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

Later status: live Kitty now passes the CDP PNG directly, so the historical decoded
damage design does not describe its normal path. Current partial-emission work is
conditional on terminal measurements and a supported protocol route; see
[`TODO.md`](./TODO.md#2-improve-graphics-transport-where-measurements-justify-it).

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
helper for. `startScreencast` caps the stream at the active capture size and picks
its format from the consumer. At Stage 3, live Block used JPEG quality 80. Live Kitty
always used PNG. Live Sixel kept the adaptive settled-PNG,
moving-JPEG-quality-90 policy. Capture always used PNG. On each
`EventScreencastFrame` the handler puts the payload in the bounded receive path and a
worker performs any required pixel decode. The handler acks every frame on receipt,
before decode, so a slow decoder cannot consume Chromium's in-flight credit. The live
`rawFrameQueue` drops the oldest undecoded payload when full, and parallel workers
publish only the newest completed sequence.

**Later status (2026-07-15).** Live Block now uses JPEG quality 90, matching moving
Sixel. At 2560x2880, q90 retains 6.39 ms median processing headroom in the matched
headless benchmark. It improves fidelity over q80 and provides more headroom for
higher resolutions than q95.
The adaptive live decode-pool ceiling and the Sixel RGBA safety cutoff are
independent policies, both currently four. `NYALA_DECODE_WORKERS` keeps its manual 1
to 16 range. Live moving Sixel JPEG uses `jpegn` fused RGBA only when the actual
session count is four or fewer; larger manual pools keep native YCbCr. Block native
YCbCr, Kitty PNG, settled Sixel PNG, and capture PNG remain unchanged.
Capture keeps the ordered `frameSlot`, because its bounded bundle must count each
usable frame in order. A corrupt live frame is already acked and can be dropped
without stalling the stream.

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

`snapshotRuns` (`screencast.go`) runs one live in-page text walk (`walk.go`),
evaluated as a single `Runtime.evaluate` that returns the per-line runs, the DOM
scroll offset it read at, and the layout generation in one round-trip. The walk
replaces the earlier `DOMSnapshot.captureSnapshot` decode. The snapshot runs on its own
goroutine (`runTextSync`), decoupled from the frame-emit loop, so snapshot latency
never withholds acks or stalls frame emission; `composeFrame` reads the latest
published runs. The snapshot clock is decoupled from frame delivery: the first
snapshot fires off the `painted` latch (the `firstContentfulPaint` lifecycle
event), not off a screencast frame. Four triggers re-snapshot after that:
navigation (single-shot), a resize or backend switch (at once), a scroll-offset
change (so the overlay tracks the page on scroll), and a debounced
late reflow on `networkAlmostIdle` (250 ms debounce reusing `holdStalenessLimit`,
capped at one extra snapshot per second). The scroll trigger is a leading throttle
plus a trailing debounce: each scroll poke re-snapshots at once when at least
`scrollCadence` (150 ms) has elapsed since the last scroll snapshot, so the overlay
tracks the page mid-scroll at about 6-7 snapshots per second, and the same 250 ms
trailing debounce lands one final snapshot on settle. The scroll path is not held
by the once-per-second reflow cap; `scrollCadence` is its only throttle, so the
overlay never freezes mid-scroll. The published runs are sticky state
carried across frames: when text publishes and no new screencast frame is pending,
the engine re-emits the held bitmap recomposed with the new text, so a static page
shows its overlay rather than drawing bitmap-only. The pure `decodeWalkRuns` (`walk.go`)
maps the walk result to `[]render.GlyphRun` with no browser dependency, so it is
unit-testable on canned structs. The walk snaps each run's origin in the page: it
visits every text node with a `TreeWalker`, takes one client rect per visual line from
`Range.getClientRects()`, and snaps the row with `Math.round(top/cellH)` from the
line-box top, no inset and no bitmap scan. The forced `line-height` makes the divisor
equal the painted line pitch, so a browser-laid line at `N*cellH` snaps to row `N` with
no residue. A whole-node `Range.getClientRects()` call gives each visual line
fragment an ordered identity. Each word or code-unit rect maps to that identity.
Adjacent words in one fragment stay together, while separate CSS columns cannot join
even when all fragments snap to the same row. `getClientRects()` returns the on-screen rect with the scroll already
subtracted, so each run places at the walk's own scroll offset; the runs, that scroll,
and the layout generation come from one evaluate, so they stay internally consistent
and a sustained scroll shows the current text. `decodeWalkRuns` trusts the page's
integer `Col` and `Row` and never re-divides a pixel by a cell width. It keeps only the
grid right-edge clip, capping each run at the nearer of the box's snapped right edge
(`round((px+bw)/cellW)`) and the grid edge, counting cells with the Stage 2 width rule
so a width-2 cluster is never half-emitted; overflow graphemes drop because the bitmap
underneath shows them. The box width is a pure clip bound and never sizes the advance.
The parent element's computed colour feeds `GlyphRun.FG` in RGBA channel order, with
the blended text opacity folded into the alpha.

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
the reused `State`, `RenderStreamFrame`, and `writeGrid`. The steady block draw
emits cursor-home and overwrites in place; the full clear (`\x1b[2J`) is kept only
for the first draw and a resize. A previous-grid cell diff emits only the changed
cells, with SGR and cursor (CUP) coalescing, which removes the per-frame flicker and
the slow scroll refresh. Live and capture share one draw-prefix helper
(`framePrefix`) and the same differ, and queued frames coalesce to one paint per
window. It restarts the stream on
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
surface call frames arrive cropped or blank. The natural call,
`Emulation.setVisibleSize`, was removed from the pinned cdproto: the emulation
package has no `SetVisibleSize` function, only the `DontSetVisibleSize` field on
`SetDeviceMetricsOverrideParams`. So `Browser.SetContentsSize(windowID)` is the
forced choice for sizing the screencast surface, not a preference.
`enableLifecycle` calls `page.Enable`
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
so the supervisor never rebuilds on a failed URL. The paint latch fires from a
`Page.lifecycleEvent` `firstContentfulPaint` subscription (`watchPaint` in
`recovery.go`); the navigate-return load signal stays only as a bounded backstop,
guarded by `sync.Once` so the latch closes once. The supervisor is the single
owner of the session fields, so no teardown races with `Close`. `Frames()` is
created once in `Navigate` and closed exactly once, by the supervisor, on `Close`;
it survives a rebuild, so the consumer keeps reading across a recovery. `Close`
signals the supervisor to stop rather than rebuild, guarded by a `sync.Once` so a
second `Close` is a no-op. The one-shot `captureFrame` path was removed;
`SmokeStaticFrame` (live tag, `smoke_live.go`) consumed one frame from `Frames()`
and rendered it once (since removed: `SmokeStaticFrame` and `smoke_live.go` are
gone, superseded by the live test `TestLiveSyncedFrame`, which is the live path now).

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
is one `render.Frame` the engine delivers with decoded pixels, so frames the
latest-wins slot drops never count. A full capture exits 0; a partial run (the
first-draw deadline, the per-frame idle timeout, a forced-backend rejection, or the
frame channel closing early) writes what it has and exits non-zero. Tab cycling is
off in capture mode.

The later live frame contract keeps decoded images in their native layout. JPEG
remains `*image.YCbCr` through the engine/render seam, while PNG keeps the PNG
decoder's native image when pixels are needed. Block converts to its small RGBA
sample surface during downscale. Sixel converts at its encoder boundary. Capture
still forces and decodes PNG, then converts where its raster and bundle paths need
RGBA, so its usable-frame and completeness gates are unchanged. Every live Kitty
frame retains the owned CDP base64 PNG in `Frame.EncodedPNG` and sends it directly
as Kitty `f=100` data. A queued frame decodes that retained PNG if a backend switch
makes Block or Sixel the consumer before draw.

**Later status (2026-07-15).** Current live Block bypasses the small RGBA sample
surface described above. It composes the final grid directly from the full-size
decoder-native `*image.YCbCr`.

Two deadlines bound the run, not one total window: a first-draw deadline of 5
seconds after navigate succeeds, and a per-frame idle deadline of 5 seconds since
the last delivered frame, reset on every drawn frame. One resettable timer drives
both: arm it at 5 seconds after navigate, reset it to 5 seconds on each delivered
frame, and a firing always means a stall. The `cdp_frame_wait_ms` metric was
dropped, because the latest-wins slot decouples Chromium's produce rate from the
consume rate, so there is no honest per-frame wait to measure.

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
- **Ack on receipt, bounded latest-wins work.** Chrome withholds frames until acked,
  so the listener releases each frame's credit before decode. The live queue drops
  stale undecoded work and the collector rejects stale completed sequences. Capture
  keeps an ordered slot for deterministic bundles.
- **Backend-specific screencast formats at Stage 3.** Block used JPEG quality 80
  because it needed pixels and its sub-cell downsample plus colour quantisation hid
  JPEG artefacts. Kitty always requested PNG and passed Chromium's owned base64
  payload straight to Kitty framing, with no image decode or re-encode. Sixel kept
  its decoded adaptive PNG/JPEG-quality-90 policy. Capture remained lossless PNG for
  all backends.
- **Decoder-native frame seam.** `render.Frame.Img` is `image.Image`. JPEG stays
  `*image.YCbCr`, PNG keeps its decoder-native layout, and RGBA conversion happens
  only at a consumer that requires it. `Frame.EncodedPNG` carries the direct Kitty
  alternative.
- **Direct-frame benchmark.** The focused benchmark uses one tiled 2560x2880 source,
  ten iterations per sample, and the median of five samples. Direct Kitty PNG fell
  from 51.950 ms to 0.014 ms. Shared JPEG fell from 25.446 ms to 10.209 ms, saving
  about 15.2 ms. A separate real-CDP-payload benchmark measured Kitty PNG at
  45.985 ms to 0.027 ms. The PNG terminal payload was 1.68 MB against about 3.25 MB
  for JPEG quality 90 sent through the decoded Kitty encoder. The frame target is
  16.67 ms. These measurements stop before the terminal decodes and draws the PNG.
- **Snapshot off paint, not per frame or on a timer.** The first snapshot fires off
  the `firstContentfulPaint` latch, decoupled from screencast frames, then re-runs on
  navigation, resize, a scroll-offset change (a ~150 ms mid-scroll cadence plus a
  snapshot on settle), and a debounced `networkAlmostIdle` reflow. The published runs
  are sticky, so a static page that delivers no new frame still composites text by
  re-emitting the held bitmap. This fixes the prior defect where a static page drew
  with no overlay.
- **Place text by the snapshot's own scroll, not match by scroll equality.** The
  bitmap and the snapshot share no frame id, so the snapshot goroutine decodes each
  run at the snapshot's own scroll offset and `composeFrame` folds the latest runs on.
  A snapshot read failure holds the last-matched runs for a bounded time, then drops to
  bitmap-only, so crisp text never composites stale over a page it no longer matches.
- **Launch managed Chromium, not remote attach.** The browser-side flags (device
  scale factor, window size) apply only to a process Nyala starts. This decision
  pinned the device scale factor to 1 for every backend, a small viewport at DSF 1
  as the clean CDP equivalent of Carbonyl's compiled-in LoDPI hack. That DSF-1 pin
  now holds for the graphics backends only. The block backend derives a device scale
  `Z` per grid, recorded under the monospace block render below.
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
| One static end-to-end frame: navigate, snapshot, bin, composite, render | `SmokeStaticFrame` (live tag) consumed one frame from `Frames()` and rendered it once (since removed; superseded by `TestLiveSyncedFrame`) |
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
- **JPEG chroma noise read as spurious cell damage.** Resolved. The block path now
  diffs the previous grid and emits only changed cells with SGR/CUP coalescing; a
  cell whose only change is sub-visible chroma noise still re-emits, but the bounded
  byte cost is acceptable. Graphics partial emission stays measurement-gated, with
  the direct Kitty PNG constraint recorded above.
- **Frame-sync tolerance and overlay staleness unmeasured.** Resolved structurally,
  carried forward to tune. `holdStalenessLimit` (250 milliseconds) bounds the error
  hold and `scrollMatchTolerancePx` (set to 1) stays the pure compare helper; the live
  spike tunes both.
- **Live-browser CI flakiness.** Resolved. The pure cores keep `just test` and CI
  browser-free; the live test sits behind `//go:build live` and `just live`, off CI.
- **Sustained FPS and the CEF fallback.** Carried forward as accepted. Measure real
  sustained output FPS on KMSCON, Kitty, WezTerm, and Sixel terminals. CEF stays the
  documented fallback gated on that number.
- **Terminal PNG cost and graphics follow-ups.** Measure terminal-side PNG decode and
  draw latency separately from Nyala's 0.014-0.027 ms framing cost. Keep the 16.67 ms
  target. Static fixed-255 reuse is faster with identical output, but motion remains
  untested. Band research closed with no change. Long-count RLE needs full-encoder
  and compatibility validation. See
  [the Sixel hot-path record](./PERFORMANCE.md#sixel-hot-path-research-and-decisions-2026-07-15).
  Dirty-region transport stays separately gated by terminal measurements.
  Kitty needs no JPEG live path today; retain the decoded JPEG encoder only as a
  measured fallback if a future adaptive Kitty mode beats direct PNG end to end.
- **Partial-emission threshold and advisory DOM box drift.** Carried forward as
  accepted measurements that feed future tuning: the graphics terminal-latency gate
  and the sub-cell box jitter past the clip rule.

## Monospace block render (post-Stage-3 fix)

Stage 3 shipped the overlay, but the block backend placed some text in the wrong
cell. The page laid out with proportional fonts, so a run of N graphemes did not
occupy `N*cellW` pixels, and the box-to-cell mapping binned the text off its true
column. This fix forces the page onto the cell grid before capture, repairs the
mapping, and gates the corpus against placement error. It is a correction to the
Stage 3 overlay, not the Stage 4 UI. Current alignment follow-up is tracked under
[Close Block correctness gaps](./TODO.md#4-close-block-correctness-gaps).

### Document-creation font forcing

`internal/engine/stylesheet.go` builds one injection script. The engine installs
it with `Page.addScriptToEvaluateOnNewDocument`, so it runs when each new
document's JS context is created, before `documentElement` exists, and applies to
the first document and every later one. `buildStylesheetScript` embeds the
FiraCode Nerd Font Mono TTF (`font.FiraCodeTTF()`) as a base64 `data:` URL
`@font-face` under the family `FiraCode Nerd Font Mono`, then forces on every
element with `!important`:

- `font-family: "FiraCode Nerd Font Mono", monospace`, so every run resolves to
  the embedded face rather than a system fallback.
- `font-size: cellW/advanceRatio px` as the seed. `advanceRatio` is `0.6154`,
  the glyph advance over the em size, measured `1200/1950` from the embedded TTF.
  One glyph advance seeds to one cell width before calibration corrects it.
- `line-height: cellH px`, one text line per cell row.
- `font-kerning: none`, `letter-spacing: 0`, `font-variant-ligatures: none`, so
  no pair adjustment shifts a glyph off its cell.
- `text-align: left`, so justification never stretches inter-word space.
- `-webkit-text-fill-color: transparent`, with `text-shadow: none` and
  `-webkit-text-stroke: 0`, so the page paints no ordinary glyph ink into the
  screencast bitmap and leaves no orphan shadow or outline. The bitmap then carries
  backgrounds, images, and borders only, and the crisp overlay is the sole text
  layer with no painted ghost to sit against, following browsh's separate-layers
  model.

Two follow-on rules restore `-webkit-text-fill-color: currentColor` for content the
text walk does not capture, so it still paints as block graphics rather than
vanishing: one for `::before`, `::after`, and `::marker` (CSS-generated content and
list markers), the next for `input`, `textarea`, `select`, `option`, `svg`, `svg *`,
and `::placeholder` (form-control values, placeholders, options, and SVG text
labels). A button label stays hidden, because it is a real text node the walk
captures and the overlay redraws. The forcing does not de-stick `position: fixed` or
`sticky`: the in-page walk takes each run's rect from `Range.getClientRects()`,
which returns the viewport rect a fixed element paints at, so a pinned bar lands on
its painted row without forcing it into flow. The script
appends its `<style>` (id `nyala-font-forcing`) the moment `documentElement`
lands: at once if it already exists, otherwise from a `MutationObserver` on
`document` that appends and disconnects when the root element appears, so the
first layout is forced.

### Block layout scale

The Stage 3 DSF-1 pin is reopened for the block backend, with the owner's approval.
A terminal wider than the layout target used to lay the page out at the full terminal
width, so a wide grid reflowed to a wide page and the text under-filled the columns.
The block backend now launches at a continuous device scale
`Z = max(1, cols*cellW / layoutWidth)`, where `layoutWidth` is the `--layout-width`
flag with a default of `1280`. A terminal wider than the target lays out at the
target CSS width while Chromium renders at full device resolution, so the page fills
the grid rather than reflowing wide. A grid at or below the target keeps `Z` of 1 and
is unchanged. The graphics backends stay at DSF 1 and take no scale, so they render
the page at its natural width.

The launch flag `force-device-scale-factor` carries `Z` for the block backend and 1
for graphics. `SetDeviceMetricsOverride` and `Browser.SetContentsSize` take `surf/Z`
CSS pixels (DIP) at DSF `Z`, while the screencast cap stays `surf` device pixels, so
the layout viewport shrinks by `Z` and the captured surface stays full resolution.
The forcing stylesheet, the calibration, and the box-to-cell mapping all run on the
fractional CSS-pixel cell `cellW/Z`, so the cell size stays a floating-point value end
to end and rounds only at the final cell bin. A capture records `layout_scale` in
`performance.json` when `Z` exceeds 1; the field is absent at `Z` of 1 and reads as 1
downstream. `placement-score` divides the recorded `cell_px` by `layout_scale`, so its
divisor and the `text-map.json` CSS-pixel geometry share units.

### Backend-conditional forcing

The forcing and the calibration apply only to the block backend. The block path
folds the crisp text overlay onto its cell grid, so it needs the page on the
monospace grid for the box-to-cell mapping to land text true. The graphics
backends (Sixel, Kitty) draw the bitmap alone and take no overlay, so forcing
would distort a page they render faithfully. A session that starts on a graphics
backend never registers the forcing script (`launch` gates the injection on
`e.backend == "block"`), so the page keeps its own fonts and layout.

`syncFontForcing` (`internal/engine/forcing.go`) reconciles the forcing state to
the active backend, so a Tab that switches the backend toggles it live. It runs on
the snapshot goroutine, reading the desired state (block backend wants forcing) and
the active state (`fontForcingID != ""`, the registered script identifier held as
state) under the mutex, then running the CDP outside the lock, mirroring
`calibrateFonts`. The registered `page.ScriptIdentifier` is the single piece of
forcing state: a non-empty id means forcing is active, so the reconcile stores the
id only after a successful register and clears it only after a successful remove,
and a partial CDP failure never leaves a phantom or stale id.

Two transitions do work; the matched pair is a no-op:

- **Block to graphics.** `disableFontForcing` removes the on-new-document script by
  its id and evaluates `buildStyleRemovalScript`, which drops the live `<style>` by
  its `nyala-font-forcing` id. The page reflows to its natural layout without a
  reload.
- **Graphics to block.** `enableFontForcing` re-registers the on-new-document script
  and evaluates it against the live document, whose `documentElement` exists
  post-paint, so the style lands at once. The `reSnap` arm then recalibrates: a fresh
  enable resets the goroutine-local last-calibrated `(cellW, cellH)` pair to zero, so
  the calibration reruns even at an unchanged grid, since `snapshotCellPx` is
  backend-independent and would otherwise wrongly skip.

Each toggle costs one reflow flash, as the page lays out again between its natural
and forced states. A per-session teardown clears `fontForcingID`, so a crash rebuild
re-registers from the launch injection rather than carrying a stale id. Capture and
corpus force the block backend, so their forcing and calibration are unchanged.

### One-shot runtime calibration

The seed size is a font-metric estimate; the rendered advance still drifts a
fraction of a pixel per glyph. `calibrateFonts` (`internal/engine/calibrate.go`)
corrects it once per cell geometry. It runs on the snapshot goroutine, between the
paint latch and the first published snapshot, so program order is the ordering
contract and no lock is held across the `chromedp.Run`.

The pass awaits `document.fonts.ready`, appends an offscreen 20-glyph ASCII probe
styled by the injected rules, reads its box width in CSS pixels, computes the
corrected size with the pure `calibratedFontSize` (`seed * (N*cellW) /
measuredBoxW`, a single linear correction), and rewrites the injected `<style>`'s
`font-size` rule with the corrected value at the same `!important` weight. Font
advance scales linearly with size, so one correction lands every ASCII glyph on
exactly `cellW` px. The whole pass runs under a 2-second timeout
(`fontCalibrationTimeout`, mirroring `inputDispatchTimeout`); on timeout or any
error the seed stands, a `font_calibration` capture event records the miss when
the recorder is active, and the caller proceeds to publish. Calibration failure
degrades placement, never the session. A resize recalibrates through the `reSnap`
arm when the cell pair changes, guarded by a goroutine-local last-calibrated
`(cellW, cellH)` so an unchanged pair skips the pass.

### Box-to-cell mapping repair

This mapping is superseded by the live in-page walk: the origin and row snap now run
in the page against the forced CSS cell (`walk.go`), and `decodeWalkRuns` trusts the
integer `Col` and `Row` the walk returns. The round rule this fix introduced still
holds, now applied per visual line inside the walk. The original `mapBoxToCells`
(`internal/engine/snapshot.go`), recorded here for the fix history, was the pure
per-box mapping, unit tested on plain floats. It converted the CDP device-pixel bounds
to CSS pixels, subtracted the scroll offset, then:

- **Round snap on both axes.** `col = round(px/cellW)`, `row = round(py/cellH)`,
  replacing the Stage 2 floor. Page insets are arbitrary sub-cell offsets, so
  round centres the residue at zero while preserving the monotonic
  non-overlapping partition the floor rule protected. `layout-07-ua-default-floor`
  gave the evidence: its browser-default body inset pushed every run half a cell
  right, so floor binned a whole column late and round binned it true.
  `docs/STAGE-2.md` already carries this as a forward note.
- **Soft-wrap trailing-whitespace trim.** `strings.TrimRightFunc(text,
  unicode.IsSpace)` drops any whitespace the layout adds at a soft wrap, a space or
  a newline, so a wrapped line does not claim a phantom trailing cell.
- **Right-edge clip.** The run clips through `render.ClipToCells` to the nearer of
  the box's own snapped right edge (`round((px+bw)/cellW)`) and the grid's right
  edge (`cellW*cols`), replacing the old pixel-width truncation budget. The box
  width is a pure clip bound and never sizes the advance: the origin snap
  `round(px/cellW)` sets position, per-grapheme whole-cell stepping sets advance,
  and the box right edge only caps how far the run may run.
- **Symmetric edge snap.** Snapping both edges the same way, so the cap is
  `round((px+bw)/cellW)` minus `round(px/cellW)`, keeps a box a rounding epsilon
  short of N whole cells at N cells, so a font-metric residue never drops the last
  glyph, while a wrongly scaled `cellW` can only clip the run short, never push it
  wide.

### Overlay guard and background recompose

The block overlay is first-writer-wins. When two runs target the same cell, the first
run's grapheme stands and a later run never overwrites a claimed cell, so overlapping
runs no longer weld. A cell is claimed when it carries overlay text or is a width-2
trailing cell. Paint-order z-ordering stays deferred: the guard keeps the first
writer, not the front-most box.

An overlaid cell and its boundary neighbour recompose their background from the source
columns the run's box does not cover, so the block colour behind the glyph tracks the
uncovered page rather than the whole cell. The recompose reads the run's box left edge
and width in CSS pixels, quantises the covered span at the half-cell floor, and
averages only the uncovered source columns. The quadrant grid is two source columns
per cell, so the mask quantises at a half cell and a residue finer than a half cell
stays. The boundary neighbour beside the run becomes a background-only cell, because
the sub-cell residue paints into the cell the overlay never writes. A box that covers
a whole cell reproduces the earlier whole-cell average byte for byte, so an
uncovered-column recompose shifts only the boundary cells and the amd64 golden
archives stay put elsewhere.

### Capture: the textReady latch

The capture driver waits for the first text-bearing publish after paint, not just
the first painted frame. The `textReady` latch holds the first counted frame until
the engine publishes a snapshot with runs folded on, so a capture bundle's first
frame always carries its text. The latch is capture-only; production pays only the
nil checks.

### Hidden-box visibility filter

The decode drops boxes the page hides, so their text never composes phantom runs
over the overlay. `hiddenStyle` (`walk.go`) reads the parent element's computed
`visibility`, `position`, `clip`, and `clip-path` in the page and multiplies the
`opacity` up the ancestor chain, then drops the node when its visibility is `hidden`
or `collapse`, when an absolute or fixed box has a collapsed `clip` rect or an
`inset(50%)` clip-path (the screen-reader-only pattern), or when its effective opacity
falls below `hiddenOpacityEps` (0.05). A per-line rect below a quarter cell high also
drops. Absent style data reads as visible, so a node without these values is never
dropped. One recorded limitation stands: the height check is inert on forced pages,
because `line-height: cellH` gives every box a full cell of height. The real page's
hidden player uses techniques the visibility and opacity checks catch; `layout-09`
gates that class.

### Corpus placement gate

`just corpus` now gates each fixture. After each block capture it runs
`placement-score -gate -thresholds <fixture.json>` against the bundle's frame,
reading the recorded `cell_px` from `performance.json` and the grid size from the
frame's `terminal-model.json`. The default thresholds are mean right-edge error
under 0.5 cells, max under 1.0, drifted-box count 0, same-row cell collisions 0, and
off-grid box count 0. The off-grid count is horizontal and negative escapes only
(`Col < 0`, `Row < 0`, `Col >= cols`); `Row >= rows` is legitimate below-fold
content. A breach sets the failure flag, and the sweep still continues past a failure
and exits nonzero if any fixture fails. The sweep runs nine fixtures at two grids,
120x40 and 240x48.

Four fixtures established the initial fixture-specific placement evidence:

- `layout-02-rtl-bidi` loosens mean, max, and drifted for Arabic shaped-cluster
  advances that exceed one cell.
- `layout-03-cjk-vertical-ruby` loosens mean, max, and drifted and allows 2
  collisions for CJK vertical and ruby runs that fall off the embedded face.
- `layout-04-multicolumn-justified` formerly allowed 8 collisions for a wrong-column
  paragraph. Whole-node visual fragment identities fixed the row-key collision, and
  the fixture now passes all three grids with zero collisions and no override. Paint
  displacement is not applicable: separate layers remove glyph ink from the source
  screenshot, so the metric skips every run.
- `layout-08` loosens max to 1.1 and drifted to 2 for a trailing-whitespace metric
  artefact: the trim shortens two soft-wrapped runs while the box width keeps the
  trimmed whitespace, so the right edge errs exactly 1.0 cells. The render is correct;
  the raw-width reference in the metric is the artefact.

The Latin runs on the shaped-script pages still score near zero; each override
loosens only enough to pass the affected runs. `layout-09-hidden-player` gates the
visibility filter and needs no override. All nine corpus fixtures pass the gate at
both grids.

### The accepted trade

One forced font size breaks faithful proportional layout: a wide display heading
and a body paragraph both render at the cell size, so the page loses its type
scale. This is the trade every screenshot-plus-overlay terminal browser makes, and
Nyala makes it deliberately. The bitmap underneath still shows the true visual page;
the overlay's job is crisp, correctly placed text, and correct placement needs the
monospace grid.

## Post-Stage-3 correctness fixes

Three overlay and DOM-walk faults were fixed after Stage 3 shipped. The block
text-placement fix (some block-backend text landed in the wrong cell) is recorded
above under "Monospace block render (post-Stage-3 fix)". The two below are DOM-walk
and overlay fixes.

### Screen-reader-only text leaked as visible runs

Screen-reader-only text (GitHub Primer `.sr-only`, e.g. `#visual-code-0-desc`)
rendered as visible overlay runs down a narrow left column. The text was real DOM
text, not an `alt` attribute, so the walk was right to see it. The bug was a gap in
the `hiddenStyle` filter (`internal/engine/walk.go`): it matched `clip-path` only
against the literal `inset(50%)`, so Chrome's normalised `inset(0px 100% 100% 0px)`
(from `clip-path: rect(0 0 0 0)`) slipped through, and `clip` was `auto` so the legacy
rect check never fired. Fix: `hiddenStyle` now drops a collapsed `clip-path: inset(...)`
in general (a new `clipPathHidden` helper) and drops a 1px-square, overflow-hidden,
absolute or fixed box. A `//go:build live` test (`walk_live_test.go`) guards it against
a crafted page, and a CI test asserts the new predicates are in the generated walk
script.

### Overlay text leaked through opaque overlays

An opaque overlay (a cookie banner, a sticky header, a modal) showed the page text
behind it leaking through, mixed with the overlay's own text. Seen on bbc.co.uk: the
cookie banner buttons collided with sport-card text behind them. Root cause: the crisp
text overlay ignored stacking order. The text walk (`internal/engine/walk.go`) emitted
runs for text nodes that were visible in the DOM but visually covered by a higher-z
element, and the overlay is first-writer-wins by DOM order, not z-index, so behind-text
leaked through. Fix: an opaque-aware occlusion cull in the walk (the browsh model), in
a new `occluded(el, x, y)` helper. It hit-tests each run's line-box centre with
`document.elementFromPoint`; it drops the run when a different element covers the point
and that element resolves a fully opaque background (`bgOf(top).a>=255`). A null
hit-test, the run's own element chain, or a translucent cover all keep the run. One
hit-test per run, on the snapshot goroutine, not per frame. A `//go:build live` test
(`walk_live_test.go`) guards a banner-over-text page plus a translucent-scrim case; a
CI test asserts the predicates in the walk script. This one mechanism also covers
sticky headers, modals, and dropdowns.

## References

These sources sit behind the engine fixes recorded above.

- chromedp v0.15.1 source:
  - Navigate and the `responseAction` load waiter: `nav.go` lines 18 to 29, `chromedp.go` lines 566 to 702.
  - `ListenTarget` no-blocking rule: `chromedp.go` lines 791 to 836.
  - `Target.run` event goroutine and `Execute`: `target.go` lines 90 to 217.
  - Current top-frame tracking: `target.go` lines 290 to 359.
  - `KeyEvent` input dispatch: `input.go` lines 166 to 193.
  - Context model and shutdown via `Cancel`: `chromedp.go` lines 122 to 300.
- chromedp issues:
  - #920 ack-inside-listener screencast stall, the primary root cause: https://github.com/chromedp/chromedp/issues/920
  - #455 the same fault with `network.GetResponseBody`: https://github.com/chromedp/chromedp/issues/455
  - #431 the lifecycle-events and networkIdle idiom: https://github.com/chromedp/chromedp/issues/431
  - #631 the enable-lifecycle-events idiom: https://github.com/chromedp/chromedp/issues/631
  - #1393 a hand-rolled WaitForNavigation is error-prone: https://github.com/chromedp/chromedp/issues/1393
- CDP protocol docs:
  - Page domain (`startScreencast`, `screencastFrame`, `lifecycleEvent`, `setLifecycleEventsEnabled`): https://chromedevtools.github.io/devtools-protocol/tot/Page/
  - Emulation domain (`setDeviceMetricsOverride`, `setVisibleSize`): https://chromedevtools.github.io/devtools-protocol/tot/Emulation/
  - DOMSnapshot domain (`InlineTextBox.boundingBox`, `TextBoxSnapshot.bounds`, `LayoutTreeSnapshot.PaintOrders`): https://chromedevtools.github.io/devtools-protocol/tot/DOMSnapshot/
  - Fetch domain (the proxy auth challenge answer): https://chromedevtools.github.io/devtools-protocol/tot/Fetch/
- The same device-metrics-versus-surface sizing bug fixed in the wild: https://github.com/vercel-labs/agent-browser/pull/1033
- The reference screencast loop: https://github.com/mafredri/cdp/blob/master/example/screencast/main.go
- Chromium `page_handler.cc`, the `frames_in_flight_` screencast backpressure: https://chromium.googlesource.com/chromium/src/+/3d83dad2db679c3924ffe74599cfd48a03211778/content/browser/devtools/protocol/page_handler.cc
