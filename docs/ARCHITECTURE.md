# Nyala Architecture

This is the durable design record: what Nyala is, how it works, and why the
load-bearing decisions went the way they did. It is also the performance
record: each decision carries the measurement that justified it. Usage and
terminal compatibility live in the [README](../README.md).

## What Nyala is

Nyala is a terminal web browser written in Go. It drives headless Chromium
over the Chrome DevTools Protocol (CDP) and renders the live page to any TTY,
including a bare Linux console under KMSCON.

Goals:

- Render modern pages (HTML5, CSS, JS) in truecolour with no display server.
- Composite crisp glyph text over the bitmap, not fuzzy rasterised text.
- Support full keyboard and mouse interaction.
- Ship one small Go binary that depends on a system-installed Chromium.
- Keep the engine pluggable behind a stable seam.

Non-goals: no bundled engine, no extension ecosystem, no desktop-parity UI,
no tabs (one instance per terminal), no home-page configuration.

Two framing decisions shape everything else. Block rendering is the permanent
floor because KMSCON speaks neither Sixel nor Kitty; Sixel and Kitty are
emulator enhancements layered above it. Crisp text is the make-or-break
differentiator; the rest of the field streams pixels only.

## The seam

`viewer.Engine` is a consumer-owned interface in `internal/viewer/stream.go`.
The viewer declares the methods it calls and never imports the engine package.
`app.engineAdapter` in `internal/app/app.go` wraps `engine.ChromiumEngine` to
satisfy the contract, so the dependency runs one way. Method signatures (doc
comments elided):

```go
type Engine interface {
	SetBackend(backend render.Backend)
	SetCellPx(w, h int)
	Resize(cols, rows int)
	Navigate(url string) error
	Frames() <-chan render.Frame
	SendKey(code string)
	SendNav(action NavAction)
	CurrentURL() string
	OpenURL(url string)
	BrowserVersion() string
	SendMouse(ev MouseEvent)
	Close() error
}
```

`Close()` is the viewer's browser teardown seam. Normal and error returns after
session setup, plus caught signals, call it. Crash recovery enters the same
engine teardown through the supervisor.

### One synced channel

`Frames()` is the only frame source. The engine places the DOM text layer by
the snapshot's own scroll offset and folds it onto `Frame.Text` before the
frame crosses the seam. Text is sticky state carried across frames, so a
static page still composites its overlay. There is no second text or geometry
channel; consumers never reconcile two cadences.

### Decoder-native pixels

`render.Frame.Img` is an `image.Image`, not a forced RGBA plane. Live JPEG
crosses the seam as the decoder's `*image.YCbCr`. On the direct Kitty
transport the frame carries the owned CDP base64 string in `Frame.EncodedPNG`
and the consumer sends it on without decoding. RGBA conversion happens only at
consumers that need it.

One invariant holds repo-wide: pixels are RGBA byte order everywhere, never
BGRA. Carbonyl's maths assumed BGRA; the port kept RGBA and never swaps
channels.

### Live formats per consumer

The format policy follows the consumer:

- **Block** requests JPEG q90 and composes cells directly from native YCbCr.
- **Sixel** requests JPEG q90; it decodes every frame, so no pass-through
  constraint applies.
- **Kitty** requests PNG on the direct transport and JPEG q90 on the
  shared-memory fast path; both transports are described under Graphics
  backends.

Measurements set this policy. Direct Kitty pass-through cut Nyala framing
from 51.95 ms to 0.014 ms per frame on the focused 2560x2880 benchmark,
and 45.99 to 0.027 ms on real CDP payloads. Composing Block cells from
native YCbCr cut matched q90 processing 42.95% (18.012 to 10.276 ms at
2560x2880), leaving 6.39 ms median headroom inside the 16.67 ms 60 fps
budget. q90 beat q80 on fidelity (PSNR 45.234 to 49.021 dB, SSIM 0.995468
to 0.997696) and beat q95 and q100 on headroom: q100 leaves 0.53 ms and
doubles the payload.

### Engines

`ChromiumEngine` is the only engine built. It streams frames via
`Page.startScreencast` and extracts the text layer through CDP DOM queries.
The seam admits others; WPE WebKit is the named candidate, wired through the
app adapter when it arrives.

CEF was rejected. It bakes roughly 670 MB of Chromium into the binary, needs
C/C++ bindings against a cgo-free pure-Go stack, and returns no text geometry,
so the crisp-text problem remains. CDP performance already exceeded
expectations, so the in-process paint hop buys nothing Nyala needs.

### Rendering pipeline

1. Receive decoder-native pixels, or a direct Kitty PNG, plus the positioned
   text layer through `viewer.Engine`.
2. Downsample pixel frames to terminal resolution and render Block cells.
3. Composite the crisp text layer over the bitmap, glyphs where text exists,
   bitmap elsewhere.
4. Diff Block cells against the previous frame. Sixel and Kitty emit a full
   image per drawn frame.

Frame assembly and the terminal write are pipelined
(`internal/viewer/stream_writer.go`): the draw loop fills one of two
ping-pong buffers with the whole DEC 2026 frame while a single writer
goroutine writes the previous one, so the paint wall is max(encode, write),
not the sum (+47% output fps on a full-motion fixture). Under it,
`out_write` times the terminal's true ingest rate.

## The engine

`internal/engine` owns Chromium. `ChromiumEngine` (`chromium.go`) is the one
concrete engine behind the consumer-owned `viewer.Engine` seam. It holds the
chromedp context, the URL, the scroll offset, the resolved cell pixels, the
active backend, and the capture size in its own state, not inside chromedp, so
it can rebuild everything after a crash. The screencast loop (`screencast.go`), the text
walk (`walk.go`, `walk.js`), the sync (`sync.go`), the launch (`launch.go`),
and the recovery (`recovery.go`) live in their own files; `chromium.go` stays
thin orchestration.

### Launch and Chromium discovery

`resolveExecutable` (`discovery.go`) gives explicit settings precedence:
`--browser`, `NYALA_BROWSER`, then
`CHROME_PATH`. An invalid explicit setting fails rather than selecting another
browser. Automatic discovery searches Chrome/Chromium first, Brave second,
and Microsoft Edge last. Each family searches `PATH`, known native installation
paths, active Nix profiles on Linux and macOS, then known packaged launchers.
Every primary-family candidate precedes every fallback-family candidate, so
Brave on `PATH` cannot override Chrome in a native installation.

The primary `PATH` names retain their order: `chromium`, `chromium-browser`,
`google-chrome`, `google-chrome-stable`, and `chrome`. Automatic candidates use
ordinary installation names and stable-channel paths. Preview channels,
portable builds, and custom installation directories need an explicit override.
Discovery checks a bounded list rather than scanning directories, package-manager
caches, the Nix store, or historical profile generations. The selected public
launcher remains the executable, including package wrappers and symlinks.

| Platform | Native locations |
|---|---|
| Linux | Public launchers in `/usr/bin` and the vendor wrappers under `/opt`. |
| macOS | Browser bundle executables under `/Applications` and `~/Applications`. |
| Windows | Vendor application paths under `ProgramFiles`, `ProgramFiles(x86)`, `LOCALAPPDATA`, and `ProgramW6432`. Empty or relative roots are ignored. |

Active Nix directories include the configured `NIX_STATE_HOME` profile, the
XDG state profile, `~/.nix-profile`, the per-user system profile, the current
system environment, and the default system profile. The resolver retains their
public wrapper paths rather than replacing them with Nix store targets.
Linux packaged candidates include the standard Snap launcher directories and
the default system and user Flatpak export directories. Custom package roots
need an explicit override.

Known Snap and Flatpak launchers use the ordinary launch path. Launch failures
include a confinement/profile hint. Discovery alone does not qualify confined
profile visibility, the debugging connection, or process cleanup. Nyala does not
change filesystem permissions or sandbox flags to accommodate them.

Selection is separate from launch failover. Missing automatic candidates allow
the next lookup, but a selected browser's launch, profile, or protocol failure
stops that launch. Recovery retains the selected executable and browser family
for the engine lifetime, so cached browser metadata cannot describe a different
browser after a crash.

Default profiles separate browser families. Chrome/Chromium keeps `profile`,
Brave uses `profile-brave`, and Edge uses `profile-edge`, under
`$XDG_DATA_HOME/nyala` or `~/.local/share/nyala`. Explicit `--profile` and
`NYALA_PROFILE` paths stay unchanged. No automatic profile migration takes place.
The `.nyala-browser` file (`profile_browser.go`) records browser identity, so a
different family cannot silently share or adopt that profile's browser.
Known browser families use
`chromium`, `brave`, or `edge` identity, allowing same-family launcher aliases.
Unknown custom launchers use their canonical absolute path. An unmarked live
legacy browser is accepted only when its process command identifies
Chrome/Chromium. Other unmarked live browsers fail with an actionable error.
An offline legacy primary-family profile remains usable. Other families and
custom launchers reject unmarked profiles that contain `Default` or `Local State`
browser data.

The app owns explicit profile policy. Persistent mode calls `SetProfileDir`
before the viewer starts; private mode bypasses that call, even when `--profile` or
`NYALA_PROFILE` is set. The engine owns the directory and its lifetime. It
resolves a persistent path once, records it without touching the file system,
then creates the directory lazily with `0700` permissions on first launch. It
opens the `0600` `.nyala.lock` file (`profile.go`) and keeps that one fd open
for the engine lifetime, across every recovery rebuild, so `flock` mode
changes happen atomically on one descriptor. A private engine keeps an empty
profile path, so it never resolves, creates, inspects, or locks the persistent
directory. `launchPrivate` keeps the `chromedp.NewExecAllocator` path through
`newAllocator`, which omits `chromedp.UserDataDir` and leaves chromedp to
create its disposable profile. That path stays because `ExecAllocator` already
removes the temporary directory, kills the child on parent death through
`Pdeathsig`, and waits for exit. A detached spawn on a temporary directory
would need Nyala to add all three for a case that shares nothing.

Remote attach is the third launch branch. The app calls
`SetRemoteDebuggingURL` before `Navigate` when `--remote-debugging-url` or
`NYALA_REMOTE_DEBUGGING_URL` is set, and `validateLaunchMode` (`app.go`)
rejects the flag together with `--browser`, `--profile`, or `--private`,
because Nyala launches nothing in this mode and a silently ignored flag would
mislead. `launch` reads the recorded URL first and takes `launchRemote`,
skipping `resolveExecutable` and the `allocatorConfig` build entirely.
`launchRemote` dials through `connectRemote`, which is a `connectFunc` beside
`connectShared`, and both share the one `connectBrowser` dial, so
`chromedp.NewRemoteAllocator` has one call site in `launch.go`.
`remoteAllocatorOptions` picks the chromedp option by URL form: a URL that
already names `/devtools/browser/` takes `NoModifyURL` and is used as given,
and any other form, such as `http://host:port`, passes no option so chromedp
fetches `/json/version` and reads the browser socket from it. The dial then
runs `newInstanceTarget` for a page target in its own window, and the
unchanged post-connect sequence follows: `GetWindowForTarget`,
`enableLifecycle`, font forcing, `applySizing`, and `enableProxyAuth`. The
branch adds no discovery, no lock, no pid, and no retry loop, because the
user owns that browser and Nyala has no profile directory to lock or port
file to read. A failure is reported once with the endpoint named. The driving
case is Nyala on a remote development machine, reached over ssh, attached
through a reverse tunnel to a headed Chrome on the user's laptop. Chrome 136
and later refuse `--remote-debugging-port` on the default profile, so that
Chrome runs on a dedicated `--user-data-dir` whose logins persist between
runs.

A persistent profile is shared: one Chromium per profile, one page target in
its own window per `nyala` instance, and a login in one instance is visible in
the others after reload. `launchShared` runs the protocol in `attachShared`.
The engine tries `LOCK_EX|LOCK_NB`. The winner is the launcher. Every other
engine polls `LOCK_SH|LOCK_NB`, bounded by `sharedLockTimeout`, reads
`DevToolsActivePort`, and connects. A launcher that dies before it downgrades
releases its lock when the kernel closes its fd, so a waiting client wakes and
retries. Any failure unlocks the guard and retries from the exclusive attempt,
`maxLaunchAttempts` times, so a client whose launcher or browser is gone
becomes the launcher itself.

The launcher (`spawnShared`) first reads the pid the previous launcher wrote
into the lock file. When that pid is alive and a connection to its
`DevToolsActivePort` address succeeds within `adoptConnectTimeout`, it adopts
that browser and spawns nothing, without purging. The lock decides liveness,
not the port file: `DevToolsActivePort` stays on disk after a clean SIGTERM
exit, so its presence proves nothing. A live pid that answers no connection
is wedged or still shutting down, so `stopBrowser` sends SIGTERM and then
SIGKILL after `browserExitTimeout`, because a fresh spawn on the same profile
would collide with Chromium's ProcessSingleton. The launcher then purges
`Default/Sessions`, removes the stale `DevToolsActivePort`, spawns Chromium,
writes the new pid into the lock file, waits for `DevToolsActivePort` within
`devToolsWaitTimeout`, connects, and downgrades to `LOCK_SH`. Chromium writes
the port file in two steps, so `waitDevToolsActivePort` polls until both
lines parse. A failure after the spawn stops the process the launcher
started.

Every signal to a pid read from the lock file is gated on
`browserOwnsProfile`. The pid can outlive its browser, and the kernel can hand
the number to an unrelated process, so Nyala reads the process command line
(`/proc/<pid>/cmdline` on Linux, `ps -o args=` elsewhere) and signals only when
it names `--user-data-dir=<profile>`. Both `stopBrowser` and the last-client
terminate in `sharedCloseOp` pass through that gate.

The purge stays with the launcher under `LOCK_EX`. Nyala drives every
navigation itself, so Chromium's tab-restore state is never wanted, and an
aborted run leaves Session and Tabs files that grow with each navigation.
Headless Chromium reloads that state at launch and respawns the recorded
tabs, which wedged the CDP handshake before the first load milestone on a
profile that had accumulated hundreds of entries. The exclusive lock makes
the deletion safe: no other client is connected, and the launcher's own
Chromium has not spawned yet. Exit cleanup cannot replace it, because the
last client out cannot know whether the next launcher will adopt or spawn.

Nyala spawns the shared Chromium itself (`spawnDetached`) instead of through
chromedp, because `ExecAllocator` uses `exec.CommandContext` and `Pdeathsig`,
so its browser cannot outlive the Go process. The detached spawn uses
`exec.Command` with `Setsid`, `/dev/null` on all three stdio streams, and no
`Pdeathsig`. A `Wait` goroutine reaps the child, so a null-signal liveness
check on the pid stays truthful after the browser exits. `sharedArgv` appends
`--user-data-dir`, `--remote-debugging-port=0`, and `--keep-alive-for-test`.
Headless Chromium 152 stays alive after every page target closes, because a
built-in extension `background_page` target keeps it alive, so the flag is
insurance for a build without that target. Both launch paths read one flag
list: `chromedp.Flag` stores into an unexported map, so `launchFlags` is the
single source, `newAllocator` converts it to allocator options and
`sharedArgv` converts it to argv in the order `ExecAllocator.Allocate` emits.

`connectShared` dials the browser websocket through
`chromedp.NewRemoteAllocator` with `NoModifyURL`, so chromedp keeps the exact
URL the port file gave instead of rewriting it through `/json/version`. It
allocates the Browser directly on a holder context, the way chromedp's
`initContextBrowser` does, instead of through `chromedp.Run`: under a remote
allocator the first `Run` on a context without a target id creates an
`about:blank` tab in some existing window, which could foreground over another
client's page. `newInstanceTarget` then creates this instance's page target
with `Target.createTarget` and `newWindow=true` and attaches a child context
with `chromedp.WithTargetID`. A separate window is required, not preferred.
`TestLiveSharedBrowserFacts` (`sharedbrowser_live_test.go`) shows that two
such targets have distinct window ids, that `Browser.setContentsSize` on one
leaves the other's bounds and `window.innerWidth` unchanged, and that
chromedp's own first target also has its own window.

`newAllocator` launches through `chromedp.NewExecAllocator`. When audio is
enabled it appends `chromedp.Flag("mute-audio", false)` with a bool value; a
bool `false` emits no flag at all, which is what unmutes Chromium, while the
string `"false"` would emit `--mute-audio=false` and keep it muted. It also
sets `--autoplay-policy=no-user-gesture-required` so audible media starts
before Nyala has input. Audio plays straight to the host sound server through
the inherited environment; owner testing found A/V sync acceptable with no
Nyala-side delay, so audio sits off the frame-rate critical path.

Headless Chromium forces SwiftShader software rendering by default. The
live launch passes `--enable-gpu` (`launch.go`), which only reverts to
normal driver selection. That flag alone leaves no software path, because
Chromium removed the automatic SwiftShader software WebGL fallback
(deprecated from 130, off by default from 144), so a host with no
usable GPU gets no WebGL context at all. A kmscon console is exactly that
host: kmscon holds the DRM card node, and headless GL autodetection on
Linux needs X11. The live launch therefore also passes
`--enable-unsafe-swiftshader`. Hardware GL still wins wherever a GPU
works, so the flag only adds a software floor. The blocklist flags were
rejected as redundant or stale, `--use-gl=angle` with
`--use-angle=swiftshader-webgl` forces software WebGL unconditionally and
loses the hardware path, and `--use-angle=vulkan` on `/dev/dri/renderD*`
render nodes is driver-dependent and untested. No Chromium flag can move
the committed goldens: `just update-golden` runs the `internal/harness`
diagnostics tests, which import no chromedp and launch no browser. The
flake puts `/run/opengl-driver/lib` on the library path because the
nixpkgs Chromium wrapper does not. The WebGL aquarium measured browser
fps 26 to 60 and ingest 60.3, proving the browser render, not the
pipeline, had been the ceiling.

### Sizing and layout scale

Two sizes drive every session. `surfaceSize` is `cols*cellW` by `rows*cellH`
device pixels: the captured surface. The per-backend `captureSize` is the
stream cap: the Block sub-cell sample grid or the graphics full-pixel grid.
Both capture helpers live in `internal/render/geometry.go` as free functions,
because the block package imports render, so render must not import block.
`BlockCaptureSize(scheme, cols, rows)` takes a local `BlockScheme` enum
(quadrant `cols*2 x rows*4`, sextant `cols*2 x rows*3`);
`GraphicsCaptureSize(cols, rows, cellW, cellH)` returns
`cols*cellW x rows*cellH`.

Each backend derives a continuous device scale Z (`frame.go`). The page lays
out at `surf/Z` CSS pixels while Chromium paints the full device surface.
Block anchors 8 CSS pixels per column (`blockCellCSSW`) and graphics anchors
16 (`graphicsCellCSSW`), each held to a 1024 px layout floor so a narrow grid
keeps a desktop layout. Block's 8 is half the graphics anchor because its
raster is 2 sub-cells per cell; Browsh's hardcoded 9 CSS px per column is the
prior art. The anchor exists because terminals disagree on the unit of the
cell size they report: at the same physical window Kitty reports a 16x33
cell (logical points) while Ghostty and WezTerm report 26x53 (device
pixels). Without the anchor the device-pixel group laid out 1.6x more CSS
pixels and drew text 1.6x smaller; with it, cell 26 gives Z = 1.625 and the
identical 2496 CSS px layout. Below the floor Block keeps cssW = surfW
rather than clamping, because shrinking its sub-cell raster costs
legibility, a deliberate asymmetry with graphics. A 2560 CSS ceiling was
dropped: on a scaled super-ultrawide it would push Z past the display scale
and break the unit normalisation. The
launch flag `force-device-scale-factor` carries Z. Running headless at DSF Z
with a CSS viewport of `surf/Z` is the clean CDP equivalent of Carbonyl's
compiled-in LoDPI hack, which is unreachable over CDP.

`applySizing` feeds one shared cssW and cssH to two calls:
`Emulation.SetDeviceMetricsOverride(cssW, cssH, Z)` for the layout viewport,
then `Browser.SetContentsSize` on the window id for the captured surface. The
second call is forced, not preferred: the override sizes only the layout
viewport, and without the surface call frames arrive cropped or blank. The
natural command, `Emulation.setVisibleSize`, no longer exists in the pinned
cdproto. Lifecycle events need `page.Enable` plus
`SetLifecycleEventsEnabled(true)` per session, or CDP emits no
`Page.lifecycleEvent` for the navigate waiter to match.

Mid-session zoom re-derives the cell pair. Every SIGWINCH re-reads
TIOCGWINSZ and derives the pair afresh (`internal/viewer/cellpx.go`),
rounding each axis rather than flooring: a fractional device-pixel cell
lost up to one pixel per column, the Ghostty and WezTerm zoom-out shrink. A
failed derivation keeps the last known-good pair. The viewer queues the
backend, grid, cell size and Kitty transport together. While a restart runs,
the next request replaces any pending request. Headless
Chromium paints at the launch device scale for the whole process, and the
live metrics override changes layout only, so after a zoom-in the frame
arrives undersized and the viewer fits it to the grid: Kitty through the
`c=`/`r=` placement keys, Sixel through the shared scaler. The accepted
cost is zoom-in softness until the next navigation. A
contents-size-at-launch-scale variant was tried and reverted: it cannot
change paint density and it broke Block's frame-spans-viewport contract.

The shared browser carries the same limit across instances, and it is
recorded rather than fixed. `--force-device-scale-factor` is a process flag,
so every client of one profile paints at the launcher's device scale. A later
instance with a different cell geometry gets its own CSS layout size through
`applySizing` but the launcher's paint density, and the viewer fits the frame
to its grid as it does after a zoom-in.

A remote attach carries the same limit one step further. The remote browser
fixed its device scale, window size, and headless or headed state at its own
start, so `applySizing` sets the CSS layout size only and the frame arrives at
the remote's paint density, which the viewer fits to the grid as after a
zoom-in. Every launch-only control is lost with it, and `RemoteLostControls`
lists them in a fixed order: the executable path (`--browser`,
`NYALA_BROWSER`, `CHROME_PATH`), the profile directory and lock,
`window-size`, `force-device-scale-factor`, `headless`, `enable-gpu` and
`enable-unsafe-swiftshader`, audio unmute and `autoplay-policy`,
`disable-frame-rate-limit`, `proxy-server` and `proxy-bypass-list`,
`disable-smooth-scrolling`, and `keep-alive-for-test`. `startSession` marks
that list under `labelRemoteAttached` in the `NYALA_DEBUG_TIMING` output, so
a user who asks why a flag had no effect can read the answer. Proxy
authentication is not on the list: `enableProxyAuth` runs after connect over
CDP `Fetch`, so it works against a remote browser too. On a headed remote
Chrome, `newInstanceTarget` opens one new window per Nyala instance on the
remote desktop, and that window is what the user sees rendered in the
terminal.

### Screencast loop

The frame model is push. Chromium emits `Page.screencastFrame` on compositor
damage and withholds further frames until each one is acked, so an unacked
stream stalls after a frame or two. Source-verified constraints shape the
loop: capture is damage-driven with a 100 fps rate cap (a 10 ms minimum
capture period), and the ack budget allows 3 frames in flight. While the
budget is exhausted Chromium drops frames rather than queueing them, so a
late ack is a gap, not latency. chromedp ships no screencast helper
(upstream declined one in chromedp issue #275), so the engine runs its own
listen, decode, ack loop.

The listener acks every frame on receipt, before decode, so a slow decoder
never starves Chromium's in-flight credit. The measurement behind that:
inline decode on the receiver goroutine capped ingest near 7 fps;
ack-on-receive with off-goroutine decode lifted it to 40 to 59 fps. The ack never runs inline on the
`ListenTarget` event goroutine: a `chromedp.Run` there is the chromedp #920
self-deadlock, because the ack command waits for a response only the parked
event goroutine can read. Instead `ackFrame` pushes the SessionID onto an
unbounded FIFO `ackQueue`, and one `ackPump` goroutine drains it and runs
`page.ScreencastFrameAck` off the event goroutine. A failed ack loses
Chromium's credit, so the pump cancels the session and lets the supervisor
rebuild it rather than risk a duplicate ack.

Delivery is latest-wins. A bounded `rawFrameQueue` drops the oldest undecoded
frame when full, never the newest, and parallel decode workers publish only
the newest completed sequence. After base64 decode, an 8-byte PNG signature
routes a production pixel frame to `internal/pngdecode`; every other payload
gets one authoritative `gen2brain/jpegn` attempt. Unsupported or corrupt JPEG
is dropped. The ingest report counts decode failures whether or not
`NYALA_FPS_TIMING` is set and retains the latest contextual error. By default,
a non-zero count emits one warning at session end. `NYALA_FPS_TIMING` instead
emits one detailed FPS summary containing the same count and latest error,
without a duplicate warning. Block and Sixel stream JPEG quality 90 in
production. Direct Kitty validates and passes its PNG through without a pixel
decode. Diagnostics capture stays lossless PNG and exercises the production
PNG route; standard-library decoders remain independent diagnostic and test
oracles, not production frame fallbacks.

The decode pool (`decodepool.go`) exists because one JPEG decode worker (28
to 31 ms per frame) capped Block output at 33 fps while ingest ran at 60.
The pool, with its bounded drop-oldest queue and monotonic sequence gate,
locked output at 60 fps drawing 99.5% of ingested frames. The default is
min(4, max(2, GOMAXPROCS/2)) workers, so decode takes at most half the
cores; `NYALA_DECODE_WORKERS` overrides it, clamped 1 to 16. Parallelism is
one frame per worker: JPEG Huffman entropy decode is serial, so a single
image cannot be split.

Three decode libraries are pinned on the hot path, all pure Go. JPEG:
`gen2brain/jpegn` (SIMD with native YCbCr output), 1.85x to 2.45x over stdlib
(a projected 56 down to 23 to 30 ms at 7 MP); native YCbCr lets Block compose
cells without an RGBA conversion. cgo libjpeg-turbo at 4.8x was rejected
because `CGO_ENABLED=1` breaks the clean cross-build. Base64:
`emmansun/base64`, because CDP wraps every frame in base64 JSON with no binary
path, 7.6x over stdlib plus a further 4.06x from the pooled zero-copy decode;
the Kitty payload encoder shares it (about 53x on encode). PNG:
`internal/pngdecode`, backed by `woozymasta/png` (SIMD unfilter, byte-identical
on 1011 of 1011 real PNGs, about 54% faster). Its direct RGBA-plane decoder
accepts Chromium's non-interlaced 8-bit truecolour forms and returns the
authoritative `ErrUnsupportedPNG` for other valid PNG shapes; malformed input
remains a distinct error. Explicit signature routing avoids a second "png"
registration that would make `image.Decode` dispatch init-order-dependent.

### The text walk

One in-page walk (`walk.js`, driven by `walk.go`) runs as a single
`Runtime.evaluate` returning the per-line glyph runs, the DOM scroll offset it
read at, and a layout generation in one round trip, so the three stay
internally consistent. The evaluate targets the execution context of the
`__nyala_world` isolated world, where the generation counter lives, so one
context still answers all three and the round trip is unchanged. A zero context
id, meaning no observer has announced itself yet, is an error rather than a
page-world evaluate, because the page world holds no generation; the snapshot
error hold absorbs it. It runs on its own goroutine (`runTextSync`), so
snapshot latency never withholds acks or stalls frame emission. The first
snapshot fires off the `firstContentfulPaint` latch, not off a screencast
frame; without the latch, text waited on `networkAlmostIdle` plus the
250 ms debounce and published 2.7 s after the first frame (2931 ms against
272 ms). The walk measures per word, not per code unit, which cut
`dom_snapshot_ms` 40 to 85% on dense pages (BBC 228 to 31 ms).

Four triggers re-snapshot after that: navigation (single-shot), a resize or
backend switch (immediate), a scroll-offset change, and a debounced
`networkAlmostIdle` reflow (250 ms debounce, capped at one extra snapshot per
second). The scroll trigger is a leading throttle at `scrollCadence` (150 ms)
plus a 250 ms trailing debounce, so the overlay tracks a sustained scroll and
one final snapshot lands on settle. Published runs are sticky: when text
publishes with no new frame pending, the engine re-emits the held bitmap
recomposed with the latest text, so a static page keeps its overlay.

A `PerformanceObserver` supplies the generation counter and pokes that reflow
trigger. The Block backend registers the `__nyalaLayoutShift` binding with
`Runtime.addBinding` scoped by execution context name, then installs the
observer through `Page.addScriptToEvaluateOnNewDocument` with the same name as
`worldName` and `runImmediately` set, so the observer, its handle, and the
counter live on the `__nyala_world` isolated world rather than on the page. A
page-world install put all three window properties where any script could
enumerate `__nyalaLayoutShift`, `__nyalaLayoutShiftObserver`, and
`__nyalaLayoutGen` and use them to identify or block Nyala. The isolated world
removes that surface and costs nothing on the read path, because the walk runs
in the same world. A binding takes one string argument, so the script encodes
its payload: `announce` once at install, and `shift:<cumulative>:<generation>`
on each qualifying shift. Only the top frame announces, so an iframe's world
cannot redirect the walk into a subframe document; the observer install stays
unconditional, so a subframe shift still reports. The engine holds the
announced execution context id, pokes reflow once when that id first arrives,
runs the disconnect script against it on a backend switch, and clears it on
teardown.

`Runtime.enable` is retained, and it is a known detection signal through its
stack getter. A name-scoped binding never reaches the isolated world without it
on Chrome 151, measured directly, so dropping the call would cost the
layout-shift trigger and the generation pairing outright. The observer state
moving out of the page world is a net reduction in what a page can see.

The walk takes one client rect per visual line from `Range.getClientRects()`,
snaps the row with `round(top/cellH)` (the forced line-height makes the
divisor equal the painted line pitch), and `decodeWalkRuns` trusts the page's
integer column and row without re-dividing pixels. The parent element's
computed colour folds into `GlyphRun.FG` with the blended opacity in the
alpha.

Two filters drop text the user cannot see. The visibility filter drops a node
when an ancestor sets `visibility: hidden` or `collapse`. It drops
screen-reader-only content, marked by a collapsed `clip` rect, an `inset(...)`
clip-path, or a 1x1 overflow-hidden box. It drops a node whose effective
opacity, multiplied up the ancestor chain, falls below `hiddenOpacityEps`
(0.05), and any per-line rect under a quarter cell high. The occlusion cull
hit-tests each run's line box at its start, centre, and end with
`document.elementFromPoint`, and drops the run only when every sample that hit
an element is covered by a different element with a fully opaque background, so
a partly covered line keeps all its text. A sample outside the viewport returns
no element and counts as no evidence rather than as uncovered. The cull
composites backgrounds only below the lowest common ancestor. An occluding
`pointer-events: none` element bleeds through, the safe polarity for a text
browser: showing text under a decorative overlay beats hiding text that is
visible.

The same climb answers two separate questions about `position: fixed` ancestors,
and keeping them apart is deliberate. The first is which runs are anchored: a run
carries a flag when the walk found a `position: fixed` element among its DOM
ancestors, which is what the compose-time shift must hold still. Only the page
can answer that. A rectangle test on the client cannot, because sitting inside a
fixed box's rect is not descending from it, and a full-viewport transparent
container (a toast root, a portal) encloses the whole page while owning one line
of text.

The second is what those elements cover, which the walk reports as
viewport-anchored boxes beside the runs. A box is a `position: fixed` ancestor of
text the walk kept, carrying its screen rect from `getBoundingClientRect` and a
flag set when its own background reaches alpha 255. The composite stops at the
element's parent on purpose: walking further would reach the opaque `body`
nearly every page carries and read a transparent bar as a cover, culling text
the reader can see. A transparent wrapper around an opaque bar therefore reads
non-opaque, so the coverage cull misses a real cover for one refresh window
instead, the safe polarity. A negative `z-index` disqualifies a box, because it
paints behind content and so can never cover, as does a degenerate rect. Such an
element still anchors its own text; only its cover is dropped.
Both answers ride the ancestor climb the visibility filter already makes, and a
cache stops that climb at the first element it has seen before, so a shared
ancestry costs one lookup. The box list is capped at 32; a page with more fixed
text containers is not one this approximation helps. Every skip here, the
negative `z-index`, the degenerate rect and the cap, drops a box and never a
tag, so it costs the cull a cover and lets text bleed through rather than letting
anchored text drift. The client re-applies
that cap when it decodes the walk JSON. The walk evaluates in the `__nyala_world`
isolated world, so the page cannot redefine the `JSON.stringify` or the
`Array.prototype` the walk uses, but it still owns the DOM the walk counts boxes
over, so the client cap is the limit and the in-page cap is the early exit. Every
box is re-tested against every
run on the frame-emit goroutine, a scan no context cancellation interrupts, so
an unbounded box count would make each frame of a scroll quadratic. The decode
truncates rather than errors: the in-page cap already drops the excess, and
rejecting would let a page suppress the whole text overlay with one box too
many. The boxes travel on
the walk result rather than on `render.GlyphRun`, so neither the render seam nor
the harness golden run-set format widens for a motion-only correction.

Each run also carries a stacking rank. Climbing from the run's element to the
root, the walk records a `(z-index, document order)` pair for every ancestor
that takes part in stacking, and an ancestor takes part in one of two ways.

It establishes a stacking context: opacity below 1, a transform, a filter, a
clip-path, a mask, `mix-blend-mode`, `isolation`, `contain`,
`content-visibility`, `view-transition-name`, `will-change` naming any of those,
a flex or grid child with a `z-index`, `position: fixed` or `sticky` whatever
their `z-index`, a `relative` or `absolute` box with an explicit `z-index`, and
the root element. The flex or grid container is found through any
`display: contents` wrapper, because such a wrapper generates no box and its
children are the items.

Or it is a `relative` or `absolute` box at `z-index: auto`, which establishes
none. CSS 2.1 Appendix E step 8 paints such a box as if it did, after the parent
context's in-flow content, but its positioned descendants and any descendant
that does establish a context belong to the parent context, not to it. The walk
gives it a pseudo level, which orders the box's own text but never traps a
descendant's `z-index` beneath it: a deeper level replaces the pseudo level
instead of nesting below it, so a pseudo level is always the last entry of the
vector that carries it. Treating the box as a full context instead inverts paint
order, because the comparison then resolves on the wrapper rather than on the
descendant that carries the `z-index`.

A member of the top layer, an open modal `<dialog>` or popover, is a third case.
It paints above the whole page, outside the page's own stacking order, so it
takes a level above any `z-index` CSS can express and roots its own vector
rather than extending the one it sits in. One `:modal, :popover-open` query per
walk collects them, and it matches nothing on a page with none open.

A `SHOW_ELEMENT` `TreeWalker` pre-pass numbers every element in document order,
so two participants at different DOM depths still compare. The vectors compare
root-first and
lexicographically: at the first level where they differ the higher z-index
paints in front, and on an equal z-index the later document order paints in
front. When one vector runs out, the shorter one is the shared context's own
in-flow content, so the longer one paints behind it only when its next level
carries a negative z-index. That is the CSS 2.1 Appendix E split between
negative-z children and everything else. A sorted copy of the run indices then
gives each run a dense integer rank, 0 being front-most, while the run array
itself stays in DOM order. The walk decode carries the rank through to
`render.GlyphRun`, and the harness golden run-set format carries it alongside
each run. The Block overlay fold sorts the runs by the rank, front-most first,
before it folds them.

One approximation: a non-positioned element that establishes a stacking context
through opacity or a transform paints in its in-flow position, but this model
treats it as positioned at z-index 0 and lets document order break the tie,
which is close enough for cell contention. Detection needs real computed style,
so the live walk tests cover the model rather than the unit gate.

### Frame sync

The bitmap and the text share no frame id, so text is placed by construction,
not matched by scroll equality. The walk decodes every run at its own scroll
offset, so the runs are internally consistent even when the overlay lags the
bitmap by a frame. The pure `holdDecision` (`sync.go`) is one bounded error
hold: a successful snapshot publishes fresh runs; a failing read holds the
last-matched runs for at most `holdStalenessLimit` (250 ms) of continuous
failure, then drops to bitmap-only, so stale text never composites over a page
it no longer matches.

`composeFrame` pairs a bitmap to the published run set by layout generation and
scroll. When both match it composes the set as-is. When the generation matches
but the bitmap has scrolled past the snapshot, `shiftRunsToBitmapScroll`
translates the runs onto the bitmap's own scroll, so the overlay stays glued to
the moving page instead of freezing until the settle walk lands. Scroll is
whole-cell, so the row shift is an exact integer.

That translation is right for document-flow text and wrong for anything anchored
to the viewport, which is why each run carries the walk's anchor flag and the
boxes travel beside them. An anchored run is a fixed element's own text: it holds
its screen position while the page scrolls beneath it, so the shift keeps it
exactly as it is, whether or not the element it descends from yielded a box.
Every other run shifts, then drops when an opaque box covers it. The order is the
only correct one. The anchor test runs first, so an opaque banner's own text is
never culled by the banner's own box, and coverage compares the post-shift box,
because a viewport-anchored box keeps its screen rect while the page moves under
it. Coverage takes the same three samples across the line's vertical centre that
the walk's own cull takes, so the two agree on the same line box and a partly
covered line keeps all its text.

Three limits are deliberate. `position: sticky` is not tracked: a stuck element
behaves viewport-fixed and an unstuck one scrolls with the page, telling them
apart needs a second layout measurement the mid-scroll path cannot afford, and
anchoring an unstuck one would cull text the reader can see. A fixed box with no
text inside it is never discovered, because collection rides the text walk. A
run culled at snapshot time stays absent while it scrolls out from under a
cover.

They do not all fail the same way. Only the undiscovered box fails towards text
bleeding through, the safe polarity for a text browser: a box nobody collected
costs a cover and never a hold. The other two hide text. A stuck sticky header's
own text is treated as flow text, so it shifts away from the header the bitmap
holds still and drops once its row leaves the grid, and a run culled under a
cover it has since scrolled out from stays missing until the next walk retests
it. Untracked sticky bleeds in the other direction too, because a stuck bar is
never collected and so never covers the body text passing under it.

Both hiding cases are accepted only because each self-corrects on the settle
walk, so the worst case is one refresh window in which the bitmap shows text the
crisp layer does not. Neither should be widened, and a new limit that hides text
needs the same bound before it lands.

This is a mid-scroll approximation and nothing more. At rest the bitmap and the
run set share a scroll, `composeFrame` composes the runs as-is, and the walk's
own hit-test is the only authority on occlusion. The approximation covers the
window between a scroll landing in a bitmap and the next walk publishing, which
`scrollCadence` (150 ms) and the settle debounce already bound.

### Input

Pointer scroll is one transport. Wheel events arrive with their own deltas and
reach `dispatchWheel`, which advances the engine's owned scroll offset and
dispatches one `Input.dispatchMouseEvent` `mouseWheel`. Key events accumulate
and forward in order rather than coalescing latest-wins: latest-wins is right
for frames but wrong for cumulative input, where a burst would collapse to one
step. `synthesizeScrollGesture` is deliberately unused: it synthesises touch and
inherits focus-dependent defaults. Mouse events arrive as terminal cells and
convert to CSS pixels at the cell centre, divided by Z so the click lands
where the frame drew the cell. The engine owns the scroll offset as the single
source of truth and re-applies it after crash recovery.

The caret keys, the four arrows plus Home, End, PageUp, and PageDown, do not
take that transport. They stroke like Space, under Keys below, because a
focused page field needs them for the caret and the viewer keeps no focus
state to tell the two cases apart with. Routing them to the wheel was right
while nothing could type: it made scrolling position-independent. It became
wrong once page text input landed, where Left and Right sent a horizontal wheel
that did nothing and Home and End jumped to the document edges instead of the
line ends. Chromium now arbitrates, exactly as it does for Space.

That hands the page the scroll offset the engine owns, so the same dispatch
reads it back. `dispatchScrollingStroke` (`input.go`) appends `scroll.js` to
the stroke's tasks, so one bounded dispatch presses the key and reports where
the page settled, and `adoptScrollLocked` takes that as the held offset. It is
the mechanism the find reveal already uses, including the cell-pitch snap that
keeps the crisp text overlay on the grid, and the shared helper is the one
place a page-side scroll enters the engine's state. `--disable-smooth-scrolling`
is load-bearing here: Chromium animates a keyboard scroll over about 150 ms by
default, so the read in the same dispatch would catch the animation part way
and the snap would freeze it there. The flag also spares the terminal a burst
of frames per key press.

A pointer wheel report carries a direction, not a magnitude, so both axes take
the same fixed 120 CSS px step per report and the deltas follow the DOM
convention CDP speaks: positive `deltaX` scrolls towards the document's right
edge, so a wheel-right report increases `scrollLeft`. A pointer wheel also
carries the report's modifier bits, so a page reading `shiftKey` or `ctrlKey`
on a wheel event sees what the user held. Keyboard scrolling and
crash-recovery replay carry no modifiers and stay position-independent. A
DevTools-injected Ctrl+wheel does not drive browser-level zoom, so the CSS
layout the renderer is sized to cannot shift underneath it; a live test watches
that assumption.

Keys and typed text are one transport too, and the engine owns the choice of
CDP event. `strokeKeys` (`input.go`) maps the named keys the viewer forwards,
Tab, Enter, Backspace, Delete, Escape, the caret keys, and the A key
select-all rides on, to a full `rawKeyDown`, optional char, `keyUp` stroke, so
the page gets the browser-native default action: Tab traverses focus with the
focus ring, Enter activates the focused element, Backspace and Delete edit
either side of the caret, an arrow moves the caret or scrolls, and Ctrl+A
selects. A stroke carries no char text unless the key produces some, which is
why the Ctrl+A stroke selects rather than typing an "a". A
typed printable ASCII character takes the opposite route. `printableStroke`
dispatches a `keyDown` carrying the character as its text plus a `keyUp`, so
Chromium generates the keypress from the text and applies its own default
action. That is what keeps Space arbitration in the browser with no focus
tracking in the viewer, a space typing into a focused field and scrolling the
page otherwise, and it means a page listening on `keydown` sees the key the
user pressed. Multi-byte and longer text, a composed character or a whole
paste, inserts atomically through `Input.insertText` instead, so text never
depends on per-byte key synthesis and a one-character paste runs no key default
action. Every dispatch is bounded by `inputDispatchTimeout`, so a wedged
browser drops the keystroke rather than parking the caller. Which keys reach
the page at all is the viewer's decision, under Key bindings and text input
below.

### Clipboard

Selection and copying are separate acts. Chromium owns the selection, which the
user makes by drag, double-click, or triple-click through the ordinary mouse
dispatch, held together by the pointer-capture rule under Rendering and input
below, or by Ctrl+A, and it paints its own selection; Nyala renders no highlight
of its own. A page may paint highlights of its own on top: a code editor that
boxes the word under the caret is doing that, not selecting it.

Ctrl+A was unbound, so the chord did nothing at all and a large page could not
be selected without a drag across it. It forwards as a key stroke rather than a
viewer command, so Chromium arbitrates it the way it does Space and the caret
keys: the focused field's text, or the whole document when nothing holds focus.
The byte keeps its readline meaning inside the URL editor and the find field,
which claim all input except registered Alt bindings before chrome mode sees
it. A Kitty key report for it normalises back to that byte at the single decode
point, so both paths behave alike.
Copying is explicit, on Alt+W or the menu's Copy item, and nothing reaches the
clipboard until then. Alt+W is the Emacs kill-ring-save chord and was unbound.
The alternatives were all taken or ambiguous: Ctrl+C quits, Ctrl+Shift+C is
indistinguishable from Ctrl+C on a legacy terminal and is the emulator's own copy
key, Alt+C cycles colour, and Ctrl+W is the URL editor's delete-word. The menu
item exists because the chord is the one page action the row cannot advertise,
and opening the menu never touches the page, so the selection survives it.

A copy reads the page through one bounded dispatch on the same worker as keys, so
a wedged browser drops it rather than freezing quit or draw. The page script reads
the document selection plus a focused text control's own selection, which is
invisible to the document selection. The write is OSC 52 to `CLIPBOARD` only,
base64, BEL-terminated, and never a read: no `p` target, no `?` query. The
payload being base64 means copied text cannot carry an escape of its own.

The bound is 74,994 bytes of text, and an oversized selection is refused rather
than truncated. The de-facto OSC 52 sequence limit is 100,000 bytes; framing takes
8 and base64 costs 4 bytes per 3, so that is the largest text that fits. A
truncated clipboard is a paste the user discovers is wrong later, where a refusal
leaves the previous clipboard intact. The bound protects the render as much as the
clipboard: a terminal that abandons an over-long OSC string prints the remainder
over the frame just drawn. The engine enforces the same number page-side, so an
oversized selection never crosses the CDP wire. The read reports the refusal
beside the empty text, because a refusal that looked like a successful copy was
the worst of both: see Copy and paste signals below.

The sequence goes out through the one writer goroutine, on the cap-1 ready
channel, exactly like a bar repaint or a resize clear. The input worker never
writes the terminal; it posts the outcome to the consumer loop, which emits it
after flushing any assembled frame. So under continuous screen updates the copy lands
whole between two frames, which is the failure other terminal browsers have
shipped. Multiplexers see plain OSC 52 with no DCS wrapping: tmux handles it
itself under `set-clipboard on` and ignores it under the default `external`, and
zellij and screen handle or drop it on their own terms. BEL terminates the string
rather than ST, matching tmux's built-in `Ms` and the ecosystem copy tools.

Shift-drag remains the terminal-native selection path and is untouched.

### Find

Find splits the work: Chromium matches, and an injected page script paints. No
CDP surface does both. `DOM.performSearch` returns node results and paints
nothing, and the browser's own find highlights are browser-level, not reachable
over CDP. So one `Runtime.evaluate` through the bounded dispatch runs a page
script where the legacy `window.find()` enumerates matches and the CSS Custom
Highlight API paints them. `window.find`'s own selection highlight does reach
the screencast, but only the current match and only in the inactive grey
selection colour, because a headless document never holds window focus; the two
custom-highlight registrations paint every match amber and the current one
orange, the desktop-browser convention. Keeping `window.find` as the matcher
keeps matching, wrapping, and cross-element and hidden-text semantics in
Chromium rather than a hand-rolled `TreeWalker`.

The costs of that choice, all deliberate:

- `window.find` is non-standard, unspecified, and Chromium and WebKit only. It
  could be removed, which would need a new matcher behind the same seam.
- It drives off the DOM selection, so the script borrows the selection and puts
  it back, including a text control's caret, which a document `Range` cannot
  round-trip.
- It walks the top document only. `searchInFrames` would leave the match in a
  subframe's selection where the top document cannot read the range, so iframe
  text is not searchable.
- The paint side needs Chromium 105 or later. Older builds still count and
  navigate; only the highlights are missing.
- One search collects at most 2000 matches.
- The highlight paints no glyph ink on Block, because the crisp overlay is the
  only text layer there, and black ink elsewhere, where the page's own ink is
  the text.

The engine owns the search state, as it owns the scroll offset and the committed
URL. The four dispatching operations, search, next, previous, and clear, run on
the viewer's serialised worker and each dispatch is bounded; `FindState` is a
separate read that takes only the mutex, so a repaint never waits on Chromium.
In flight the viewer reads `Pending` with the previous counts still
valid; on a stall it reads `Failed` after the dispatch deadline, keeping the last
counts, because the highlights that search painted are still on the page. A
main-frame commit drops the state, so a navigated page never shows a count for
highlights it no longer carries.

Clear removes both highlight registrations, the injected style, and the
page-side range list. DOM focus and the caret are untouched, so page input after
a clear reaches the element and offset the reader left. Scroll stays where the
last match was, the way closing a desktop find bar behaves, and the engine has
already adopted the page's settled offset, so the wheel transport and crash
recovery stay aligned. A revealing scroll is snapped to the cell pitch on
Block, so the crisp overlay stays on the grid.

The bar row that drives all of this, and what it draws while a dispatch is in
flight, is under Rendering and input below.

### Crash recovery and concurrency

A supervisor goroutine (`supervise` in `recovery.go`) owns the session. It
waits on `Target.targetCrashed` or a lost connection, tears the dead session
down, and rebuilds from engine-held state: re-navigate the held URL, re-apply
the held scroll offset once the load signal lands. `Frames()` survives the
rebuild, so the consumer keeps reading. `navigateAsync` drives navigation on
its own goroutine after the frame loop is live, so startup never blocks; a
failed load renders Chromium's own error page rather than tearing the session
down. Every launch uses the same engine-held profile path. Rebuilds neither
resolve another path nor replace the profile guard, so one lock fd spans every
Chromium connection in one engine lifetime. On a shared profile a dead browser
closes the websocket, `RemoteAllocator` cancels the holder context and with it
`e.ctx`, and `rebuild` reruns `launch`. That rerun takes the lock in the right
mode on the same fd, so it relaunches the browser, reconnects to a browser
another client relaunched first, or makes a new target on a browser that still
lives.

Normal close, error cleanup, and crash recovery share `teardownSession`. For a
live session it runs `closeBrowserOp` bounded by `gracefulCloseTimeout`, 500
ms. A private engine asks `chromedp.Cancel` to send `Browser.close`. A shared
profile runs `sharedCloseOp`: it closes this instance's own target, closes the
websocket, then tries `LOCK_EX|LOCK_NB`. Success means this is the last
client, so it sends SIGTERM to the pid the launcher wrote into the lock file
and waits for exit within the same deadline. Failure means another client
lives, so the browser stays. SIGTERM replaces `Browser.close` because that
command is unreachable from a chromedp remote context: `chromedp.Cancel` there
closes only a target, and chromedp refuses `browser.Close()` on the Browser
executor. The websocket closes before the terminate, so chromedp never logs a
write on a socket the dying browser reset. A close error, timeout, or
already-dead session falls back to raw cancellation, so browser or CDP failure
cannot block process exit or crash rebuild. On engine close, this teardown
completes before `Close` releases the profile guard, so the last-client
decision reads a still-held lock and Chromium finishes writing persistent
state while the next launcher still waits on `LOCK_EX`.

A remote attach runs `remoteCloseOp`: it closes this instance's own target,
then the websocket, and returns the close error. It never sends SIGTERM and
never calls `Browser.close`, because the remote Chromium belongs to the user
and no pid or lock exists to terminate through. The 500 ms
`gracefulCloseTimeout` applies here too, and a target close over a slow ssh
tunnel can miss it, so `teardownSession` falls back to the raw cancel and
leaves an `about:blank` window open on the remote browser. The timeout is
kept: a longer wait would delay every quit to protect a window the user can
close by hand. Recovery is unchanged. A dropped tunnel closes the websocket,
`RemoteAllocator` cancels the holder context, and `rebuild` reruns `launch`,
which reads the same recorded URL and reconnects to it. A tunnel that is still
down at that moment would otherwise end the supervisor, because `rebuild`
gives up after one failed `launch`, so `launchRemote` on a rebuild (marked by
a committed `currentURL`) redials every `remoteReconnectInterval` for up to
`remoteReconnectTimeout`, and stops early when Close arrives. The first launch
keeps failing fast, so a mistyped endpoint is reported at once. A tunnel drop
therefore heals without Nyala touching the remote browser. Chrome on Linux exits when its last
window closes, and the remote has no `--keep-alive-for-test`, so the user
keeps one tab of their own open or the reconnect finds no browser.

One mutex guards all shared session and geometry state. Every method locks,
copies into locals, unlocks, then runs chromedp I/O on the locals; the mutex
is never held across a `chromedp.Run` or a channel send.

`go.mod` pins `chromedp v0.16.0` and `cdproto v0.0.0-20260714215040-dc233986426f`,
both pure Go, so the module stays cgo-free.

## The crisp text overlay

The bitmap floor makes small text illegible. Measurement put the limit at
10 px: text rendered at 10 px survives the block downsample, text at 8 px does
not. Most page text sits below that floor. So Nyala hides page text from the
bitmap entirely and paints every glyph as a full crisp terminal cell. The
overlay is always legible whatever size the page asked for.

Burning glyphs into the bitmap is rejected permanently. It re-rasterises text
into pixels at whatever size the page asked for, dropping it back under the
10 px floor the overlay exists to clear.

### Font forcing

`internal/engine/stylesheet.go` builds a script installed through
`Page.addScriptToEvaluateOnNewDocument`. The script runs when each new
document's JS context is created, before `documentElement` exists, so it
applies to the first document and every later one. It appends a `<style>`
element the moment the root element lands.

The stylesheet embeds FiraCode Nerd Font Mono as a base64 `@font-face` and
forces every element with `!important`: the font family, `font-kerning: none`,
`letter-spacing: 0`, `font-variant-ligatures: none`, and `text-align: left`.
Text-bearing elements also get `font-size: cellW/advanceRatio` px and
`line-height: cellH` px, where `advanceRatio = 0.6154` (glyph advance over em,
measured 1200/1950 from the embedded TTF). One glyph advance then seeds to one
cell width. Replaced media elements (`img`, `svg`, `canvas`, `video`,
`iframe`, `embed`, `object`) are excluded from the sizing rule so the forced
line-height cannot collapse their boxes.

Text ink is removed from the bitmap: `-webkit-text-fill-color: transparent`,
`text-shadow: none`, `-webkit-text-stroke: 0`. The bitmap carries backgrounds,
images, and borders only; the overlay is the sole text layer, with no painted
ghost beneath it. Two restore rules return `currentColor` to content the text
walk cannot capture, so it shows as block graphics rather than vanishing:
`::before`/`::after`/`::marker` for generated content and list markers, and
`input`/`textarea`/`select`/`option`/`svg`/`svg *`/`::placeholder` for form
values and SVG labels.

Forcing applies to the Block backend only; graphics backends draw the
untouched bitmap. `syncFontForcing` (`internal/engine/forcing.go`) reconciles
the injected script to the active backend on a live backend switch, and
refreshes the stylesheet when the cell pair it baked goes stale. Each toggle
costs one reflow.

Calibration corrects the seed size (`internal/engine/calibrate.go`). After
awaiting the forced face through `document.fonts.load`, it measures an
offscreen 20-glyph ASCII probe and rewrites the forced size by one linear
correction, `calibratedFontSize = seed * (N*cellW) / measuredBoxW`, then
measures once more against the corrected face so repeat calibration is
idempotent. The pass runs under a 2 s timeout; on expiry the seed stands.
Failure degrades placement, never the session. A resize that changes the cell
pair recalibrates.

The accepted trade: one forced font size breaks the page's proportional type
scale. That is deliberate. The bitmap shows the true visual page; the
overlay's job is crisp, correctly placed text, which needs the monospace grid.

### The combined grid

One cell grid holds the bitmap fill and an optional grapheme. A grapheme
present means a text cell; absent means the block glyph. The grapheme
overriding the block collapses cell ownership, z-order, and block suppression
into one branch; there is no separate z-buffer. This is Browsh's indexed grid
and Carbonyl's cell model, ported.

`block.Cell` carries the overlay in two fields. `Text string` holds the
grapheme, with `""` meaning a bitmap cell. `Wide bool` marks the trailing cell
of a width-2 glyph. The zero value composes exactly as a bitmap-only cell.

Compose folds the overlay in a second pass (`applyOverlay`,
`internal/render/block/overlay.go`). The foreground is the run's DOM colour.
The background is the plain average of the bitmap pixels beneath the cell
(`cellBGAverageFrom`); with sub-cell geometry it averages only the uncovered
source columns, otherwise the whole cell. Contrast holds by construction.

The overlay is first-writer-wins, fed front-to-back. The fold reads the runs in
ascending stacking rank, front-most first ("The text walk" above holds the CSS
ordering model that assigns the rank), so the first run to reach a contested
cell is the front-most one and the claim rule keeps it there. Sorting the input
gives the z-order; the compositor itself did not change. That inverts Carbonyl's
back-to-front, last-writer-wins feed, reaching the same stacking result from the
opposite end. Later runs never overwrite a claimed cell. A wide grapheme's
trailing cell carries background only, and terminal writers emit nothing for it.
The claim mask is a uint bitmask per run, not a per-grapheme bool slice, which
removed all per-fold allocations (1,360 to 2,924 down to zero). The sort keeps
that count at zero: a frame whose runs already arrive in rank order takes a
no-copy fast path, and a reorder lands in a scratch buffer on the `block.Grid`,
which the live draw path reuses frame after frame.

A run's box can also spill a sub-cell residue into the cell beside it, which the
overlay never writes, so the fold repaints those two neighbours from their
uncovered source columns alone (`recomposeBoundaryCellsFrom`). That repaint is
first-writer-wins too, through its own per-frame bitset on the grid, so both
halves of the fold hand a contested cell to the same run. The bitset is separate
from the cell claim because the cell claim also gates the glyph write: claiming
a neighbour there would suppress legitimate text from a run behind. Like the
scratch buffer it hangs off the grid, so the reused live grid allocates it once.

The producer owns cell positions; the compositor does no binning. The in-page
walk snaps each run's origin with round, not floor: `col = round(px/cellW)`,
`row = round(py/cellH)`. Page insets are arbitrary sub-cell offsets, so
rounding centres the residue at zero while preserving a monotonic,
non-overlapping partition. Runs clip at the grid edge; soft-wrap trailing
whitespace is trimmed.

Grapheme width follows two libraries: segment with `rivo/uniseg`, measure
with `mattn/go-runewidth`, clamp to 1 or 2. A width-0 combining mark counts
as width 1. A width-2 glyph claims its lead cell plus the trailing cell, and
is skipped whole when either is already claimed, so a wide glyph is never
half-written over a prior run. At the right edge it clips rather than
half-emits (terminals disagree on clipped wide glyphs), writing a single
background space.

### Font fallback and the placement oracle

The diagnostic rasteriser resolves glyphs through `font.Atlas.Resolve`
(`internal/render/font`), which checks coverage via `sfnt.GlyphIndex` with no
runtime probe: FiraCode Nerd Font Mono first, then GNU Unifont, else the rune
is reported uncovered. Both faces ship in the binary via `go:embed`.

The placement oracle is an exact-template matcher (`internal/ocr`, pure Go,
cgo-free), not general OCR. The candidate set is the run's graphemes plus the
fixed block glyph set the compositor can emit: the ten quadrant glyphs
U+2596..U+259F, the half blocks U+2580, U+2584, U+258C, U+2590, the full
block U+2588, and space. Each cell binarises by an adaptive Otsu split and
matches by polarity-invariant Hamming distance, so dark-on-light cells match
white-on-black templates. Templates draw through the same face path as the
graded cells, so the comparison is like for like. Near-zero character error
proves placement; a block glyph recovered where a letter was expected flags a
cell the overlay failed to claim.

The background average is architecture-sensitive; the amd64-only golden rule
in Correctness architecture covers it.

## Block backend

### Glyph schemes

The Block backend draws the page as Unicode block glyphs, one glyph per cell,
with a foreground and background colour. Two schemes trade resolution against
colour fidelity:

| Scheme | Sub-cells | Capture size (cols x rows grid) | Role |
| --- | --- | --- | --- |
| Sextant | 2x3 | `cols*2` x `rows*3` | Sharpest; allow-listed terminals only |
| Quadrant | 2x2 | `cols*2` x `rows*4` | Default |

`--blocks=auto|sextant|quadrant` selects the scheme (`resolveScheme` in
`internal/viewer`). `auto` and `sextant` resolve to sextant when
`SextantSupported` matches the terminal against the allow-list (detailed
under Capability detection), and degrade to quadrant otherwise. Fallback is
silent by owner decision. Alt+B cycles quadrant and sextant live;
`CycleScheme` skips sextant on a terminal off the allow-list. Capture paths
resolve `auto` to quadrant, so diagnostic bundles stay comparable across
terminals. A half-block scheme (`--blocks=half`, `SchemeHalfBlock`, one 1x2
source column per cell drawn as U+2584) existed and was removed: no auto
route selected it, no compatibility niche was recorded for it, and it gave
no perceivable speed benefit on the parallel compose path.

A Shaded scheme (`--blocks=shaded`, `SchemeShaded`) existed and was removed
after a full evaluation. It scored the Symbols for Legacy Computing
medium-shade glyphs and, later, the U+2591..U+2593 full-cell dither
candidates against each cell's 2x4 samples, quantising candidate colours to
the active low-colour palette before scoring. The scheme was implemented in
full with a reproducible A/B harness against Quadrant. The findings:
monochrome was its only decisive win, and that colour mode was removed
first; under 256 and grey it tied Quadrant on flat fixtures and trailed
it on corpus pages and hard edges; the dither candidates, blending
lattice-bracketed palette pairs, won only anti-aliased gradients, and only
at viewing distance (a 4x box downscale before scoring); the owner observed
evened-out grey patches on video, a real but niche gain. Adaptive OSC 4
palette work (WW-236) supersedes glyph-side palette emulation, so the scheme
came out.

**Quadrant.** A cell maps to a 2x4 source region. Each of the four quadrants
box-averages a 1x2 source column into one sub-cell colour. Each sub-cell
converts to Rec.601 luma (integer weights 77, 150, 29); a bit sets when the
sub-cell's luma exceeds the cell midpoint `min + (max-min)/2`. The 4-bit mask
`TL<<3 | TR<<2 | BL<<1 | BR` keys the glyph table, so one luma mask drives both
the glyph choice and the foreground/background split: the foreground is the
average of the bright sub-cells, the background the average of the dark ones.
The two flat masks (0000 and 1111) emit U+2588 with fg equal to bg.

**Sextant.** A cell maps to a 2x3 source region, one pixel per sub-cell. The
glyphs are the Unicode 13 Symbols for Legacy Computing sextants,
U+1FB00..U+1FB3B (60 glyphs), not the Unicode 16 octants. The 6-bit mask is
row-major with bit 0 at the top-left, the opposite assembly order to the
quadrant mask, so the two builds stay separate. Four masks have no sextant code
point and reuse existing glyphs: 0 is a space, 21 is U+258C, 42 is U+2590, and
63 is U+2588. Every other mask maps through:

```
codepoint = 0x1FB00 + (mask - 1 - (mask > 21) - (mask > 42))
```

Binarisation mirrors quadrant: Rec.601 luma, midpoint split, one foreground
and one background per cell. More sub-cells sharing two colours means more
colour error; that is the shared constraint of the whole block tier. A uniform
cell yields mask 0, a space with fg equal to bg, visually identical to a full
block.

Octants are deferred: terminal support is thin, colour fidelity is lower, and
the sextant table generalises to them later. Braille was rejected because it is
monochrome and cannot sit on a coloured page background.

Compose performance follows a measured model. Compose and downscale
parallelise over whole-row bands from a persistent pool sized GOMAXPROCS-1
(`internal/rowband`) with disjoint row writes; speed-up is near-linear to
about 8 bands, then memory-bandwidth-bound (13.9 to 2.22 ms at 240x86).
notcurses' one threading attempt lost to a shared-state mutex, the
cautionary prior art. Luma uses
inline integer weights (77, 150, 29) because the multiply beat a lookup
table (-17.6% vs -15.3% geomean; three L1 loads cost more than the
multiply); the swap flipped 16 of 24,000 cells at
near-tie boundaries and the goldens were re-blessed. Colour conversion is
not the cost: `color.YCbCrToRGB` is 0.99% of CPU while composition (51.6%)
and JPEG decode (44.0%) dominate, so a SIMD converter was rejected. A 4:2:0
scalar fast path is retained (isolated sampler -26.9%, saturated throughput
448 to 531 MB/s) with a generic fallback for other ratios. Colour picking
uses mask-membership count tables (quadrant -33%, sextant -30% isolated); a
packed-RGBA exact sampler (-64%) serves PNG-derived frames only.

### Colour

`ColourTrue` passes each cell's RGB values through 24-bit SGR.
`ColourXTerm256` converts each cell colour through `block.XTerm256`, a generated
32 KiB lookup table: a
15-bit RGB key indexes the CIEDE2000-nearest canonical xterm colour in 16..255,
so configurable system colours 0..15 never appear. One indexed load per colour,
no search and no start-up work. The internal rendering harness measured the LUT
against a closed-form formula and a nearest-of-two comparison: against
nearest-two the LUT improves mean CIEDE2000 4.81%, worst-cell p95 20.09%,
and full-path p90 9.86%. PSNR and SSIM favoured nearest-two, but CIEDE2000
is the stated selection metric. The generated asset is deterministic,
exactly 32,768 bytes.

`ColourXTermGrey` converts each cell colour through `block.XTermGrey`. It uses
the integer Rec.601 key `77R + 150G + 29B` and selects the nearest canonical
xterm greyscale level, with indexes fixed to 232..255. The canonical levels are
8 through 238 in steps of 10. An exact midpoint selects the lighter grey.
Values outside that range clamp to its endpoints.

Block palette handling keeps capability, selection, and register state
separate. `paletteQueryOK` is the startup capability verdict and never changes
at runtime. `staticPaletteSelected` is the session policy, with its false zero
value selecting adaptive palettes. The live and ever-written bits in
`paletteWriteRecord` track whether a restore is due now and whether teardown
must restore again.

`adaptivePaletteActive()` combines the selected policy with the capability
verdict, the active Block backend, and either `ColourXTerm256` or
`ColourXTermGrey`. <kbd>Alt</kbd>+<kbd>P</kbd> changes the policy only when all
three gates allow it. Every unsupported context leaves the policy, terminal
output, and engine restart queue unchanged.

The policy belongs to `State`, separate from the current colour mode, glyph
scheme, backend, page, and terminal size. <kbd>Alt</kbd>+<kbd>C</kbd>,
<kbd>Alt</kbd>+<kbd>B</kbd>, <kbd>Alt</kbd>+<kbd>R</kbd>, reload, and resize keep
the policy. Nyala does not save the policy, so each process starts adaptive.
When the capability gate is closed, the static palette path remains active even
though the selected policy is adaptive.

An <kbd>Alt</kbd>+<kbd>P</kbd> transition follows the same ordered repaint path
as the Block cycles. First, `togglePalettePolicy()` changes the policy and
clears any pending palette write. A transition to static calls
`releasePalette()`. That function invalidates both adaptive engines and returns
a bare OSC 104 only when adaptive registers are live. `recomposeBlock()` then
resets the cell diff and emits the SGR reset, screen
clear, optional OSC 104, and current bar in that order. It finally queues a
same-backend restart. A transition to adaptive invalidates both engines, emits
the reset, clear, and bar without OSC 104, then restarts Block. The first new
frame fits and writes its palette before it emits cells. The ever-written bit
stays set, so teardown restores again after any adaptive write.

Precedence is `--color` flag, then `NO_COLOR`, then detection
(`resolveColour`). The flag accepts
`auto|truecolor|256|grey`. Explicit choices map to
`ColourTrue`, `ColourXTerm256`, or `ColourXTermGrey`.
A monochrome mode (`--color=none`, `ColourMono`) existed and was removed: a
web browser without colour is pointless, and every remaining mode emits SGR
colour codes. An ANSI tonal mode (`--color=ansi-tonal`, `ColourANSITonal`,
16 luma-ranked ANSI indexes) followed it out: the indexes 0..15 it wrote are
terminal-configurable, and every remaining mode builds on 256-colour
support. Under `auto`, a present `NO_COLOR` key with ANY value,
including empty and "0", per no-color.org, yields `ColourXTermGrey`, the
least colourful output Nyala renders. Every explicit non-auto mode overrides
`NO_COLOR`. The flag spells `truecolor` the American way to match the
`COLORTERM` token. Go identifiers stay British.

Detection (`detectColour`) is env plus terminfo through `colorprofile.Detect`,
hermetic and injectable, with no DCS round-trip. This is colour depth alone,
a separate question from the terminal's background polarity, which the
handshake does ask for over OSC 11 under Capability detection. A present `COLORTERM` of
`kmscon`, `truecolor`, or `24bit` is a trusted positive that short-circuits to
truecolour before `Detect` runs. The round trip was dropped because its answer
could change nothing a reader sees: the coloured modes paint every page cell,
so the terminal's default colours are not visible on the page area. The bar
row is the one exception, the row drawn in the terminal's own colours, and
its handling is tracked as WW-38. The round trip also failed on three measured platforms,
which corroborated the drop: DECRQSS is a no-op on KMSCON's libtsm, zellij
leaks the reply into stdin, and herdr stops answering after a child sets the
background. Under tmux,
detection accepts one `tmux info` exec at startup, the only reliable way to
honour tmux's truecolour override. GNU screen never sets `COLORTERM` and lacks
a Tc/RGB terminfo cap, so truecolour there is a user opt-in via `.screenrc`.

For automatic startup, the detected profile maps TrueColor to `ColourTrue`
and every other profile to `ColourXTerm256`, the floor mode: every remaining
colour mode builds on 256-colour support, and every modern terminal renders
indexed colour. The runtime cycle order is `ColourTrue`, `ColourXTerm256`,
then `ColourXTermGrey`.

An automatic True Colour Block session observes sustained terminal write
pressure when palette control is confirmed and the adaptive palette policy is
still selected. The observer selects every eighth eligible completed frame
write for timing, ignores its first four samples, and reports pressure after
four consecutive samples of at least 15 ms. A faster sample clears the slow
run. The observer has fixed state and adds no allocations, atomics, goroutines,
logging, or dependency on the FPS meter. Unselected writes do not read the
clock.

`automaticColourFallback` records that `--color=auto` resolved to
`ColourTrue`. An explicit `--color=truecolor`, another backend, an unanswered
OSC 4 probe, the static palette policy, or another colour mode closes the
observer gate. Resize, backend, render, colour-cycle, and session lifecycle
changes reset the samples and invalidate stale completions. Page navigation
keeps the session-level sample history. When an automatic session cycles back
to `ColourTrue`, the reset observer becomes eligible again.

The first pressure verdict changes the mode to `ColourXTerm256` through
`recomposeBlock()`. Queued True Colour output completes before the reset,
clear, current navigation bar, and same-backend restart. The next frame writes
its OSC 4 palette before indexed cells. The new mode makes the observer
ineligible. Nyala never promotes the mode automatically. Manual colour cycling
remains available for every flag. An automatic session can cycle back to
`ColourTrue` and arm a new pressure observation period.

`resolveColourCapability` sets the cycle's effective cap from detection and an
explicit mode. On a real TTY the cap is floored at `ColourXTerm256`, the mode
every profile now starts at or above. A non-TTY keeps its detected cap, so
piped output never gains colour modes. `ColourXTermGrey` remains available
through an explicit mode or the cycle when the effective cap permits it. An
explicit mode can raise a lower detected cap because the user asserts support.
Terminal compatibility remains authoritative and can lower the cap.
`CycleColour` skips every mode above that cap. `NO_COLOR` changes the automatic
starting mode but does not change the detected cap.

### The diff writer

Steady Block output emits a cursor home plus in-place overwrites; a full screen
clear runs only on the first draw and after a resize (`framePrefix`).
`block.GridDiff` retains the previous grid and emits only the cells whose
glyph, colours, or overlay changed, coalescing cursor moves (one CUP per
contiguous run) and SGR (one pair per same-colour run). Queued frames coalesce
latest-wins, so a burst paints once. Each frame goes out in one buffered write,
bracketed by DEC 2026 synchronised updates; a terminal without the mode ignores
the bracket, and the cursor stays hidden for the whole session.

The one-buffered-write rule is sized by measurement: a full-motion frame is
about 183 KB against the 4 KB default bufio, and mid-frame auto-flushes put
torn frames on screen; buffered whole, the write holds about 0.4 ms under
motion. `EmitTrueColour` tracks RGB state directly instead of comparing
encoded SGR bytes (isolated diff -22 to -25%), and a combined
foreground-plus-background SGR halves the SGR count (9,240 to 4,800 on a
120x40 full-motion frame), proven equal through the vt terminal emulator.
Appends are allocation-free (down from 14,440 allocations per full-motion
diff) with a precomputed [256]string decimal table (BenchmarkBlockDiff
-44.9%; hardware out_encode 2.1 to 1.7 ms). REP was tried in a
benchmark-only spike and rejected: 92 to 98% byte cuts on flat fixtures,
but a 15.7 to 24.6% p90 regression and 0% saving on photo and motion, so
the writer stays literal.

## Capability detection

### The handshake

One write batches the whole probe (`queryBytes`): the Kitty graphics query
(`a=q`), an optional Kitty shared-memory transport query, the three cell-size
queries, the XTVERSION identity query, the Kitty keyboard-protocol flags query,
the OSC 11 background-colour query, the OSC 22 pointer-shape capability query,
then DA1:

```
CSI 16t   cell size in pixels
CSI 14t   text area in pixels
CSI 18t   text area in cells
CSI > 0 q XTVERSION
CSI ? u   Kitty keyboard-protocol flags
OSC 11 ?  terminal background colour
OSC 22 ?  pointer-shape capability
CSI c     DA1
```

DA1 is the fence: terminals answer it last, so its arrival means every earlier
reply is already buffered. A bare TTY never answers DA1, so a 200 ms timeout
fails closed to Block. The read loop polls with `unix.Poll` because a tty
returns ErrNoDeadline for SetReadDeadline.

Kitty support is the APC reply carrying `;OK` for the probe's image id. Sixel
support is a DA1 attribute element exactly equal to "4", split-matched so "64"
and "14" never false-match. Footgun: the `CSI t` replies order height then
width, the opposite of GetSize.

Kitty keyboard support is the presence of a `CSI ? <flags> u` reply, since only
an implementing terminal answers. The flag value itself is ignored, because
Nyala pushes its own. The query rides inside the fence, so a silent terminal
costs nothing and a late reply is rejected by the CSI u decode rather than
leaking through as keys. The DA1 reply shares the `CSI ?` prefix, so the parse
requires the `u` final over a digit run alone and keeps scanning past a
non-match. What Nyala does with the capability is under Key bindings and text
input below.

The background reply is an OSC 11 body carrying an XParseColor spec, read as
dark when its HSL lightness falls below one half (`parseBackgroundDark`). It
accepts the `rgb:` and `rgba:` forms at every legal digit width and the `#`
form at three, six, nine, or twelve digits; an alpha component is parsed and
discarded, because a background composites over the desktop and its opacity
says nothing about the colour a reader sees. The legacy X11 colour names are
refused rather than carried as a name table. The reply holds no `CSI ?`, so it
can never terminate the DA1 read early, and no other handshake parser scans for
an OSC introducer. The query is confined to the full handshake, which runs
before the key reader goroutine exists: a mid-session query would land its
reply in `nextEvent`, which has no `ESC ]` arm and would type the body into the
page. The reduced multiplexer handshake omits it, because tmux answers from a
cache read once at attach and so describes the terminal as it was rather than
as it is. The only consumer is the bottom bar's colour, below.

Backend nomination is ceiling-first: the enabled cycle orders Kitty, Sixel,
Block, so the default backend is the best one the terminal confirmed. Block is
always present and last. Alt+R cycles the enabled backends.

### Cell geometry

The resolution ladder for the cell pixel size:

1. TIOCGWINSZ, when both pixel fields are non-zero.
2. The `CSI 16t` reply (cell size direct).
3. The `CSI 14t` text-area pixels divided by the `CSI 18t` cell counts.
4. The 8x16 px default.

A winsize-derived cell outside 4..32 px wide or 8..64 px tall is rejected as
garbage (WSL and telnet paths report zeros or nonsense like 32000x0) and falls
through. The 8x16 default has the 1:2 aspect of fbcon and errs narrow and
short, so it under-fills rather than over-draws. The `NYALA_CELL_PX` diagnostic
override beats the whole ladder.

### Multiplexers

`detectMultiplexer` uses environment variables because glyph probes fail in
multiplexers. It checks `TMUX` for tmux, `ZELLIJ` for zellij (including a value
of "0"), `HERDR_ENV=1` plus `HERDR_PANE_ID` for Herdr, and `STY` for screen.

tmux and zellij use a reduced handshake without the Kitty graphics query.
Herdr uses a reduced handshake that sends the standard Kitty graphics query,
but not the shared-memory probe. Screen skips the handshake. Every
multiplexer keeps the legacy key parser because none of these handshakes
queries the Kitty keyboard protocol. See [Multiplexer graphics](#multiplexer-graphics)
for the backend checks.

### The sextant allow-list

Sextant support is identity, not probing. A cursor-position glyph probe
false-passes on tofu (a tofu box advances one cell, the same as a real
sextant), corrupts under multiplexers, and hangs headless; reading the active
font file fails over SSH. So `internal/viewer/sextant.go` gates on a static
allow-list over terminal identity env markers, the notcurses model, name-only
with no version gating: kitty, wezterm, ghostty, foot, alacritty, konsole, vte,
contour, rio. iTerm2 is excluded: its 3.5.x builds show a vertical-gap seam
between sextant glyphs, and name-only matching cannot exclude one bad version,
so the whole terminal is out pending a version gate.

The decisive finding behind the model: allow-listed terminals self-draw block
glyphs from cell metrics, so the terminal font does not gate support. FiraCode
Nerd Font lacks U+1FB00..3B coverage and it does not matter. Identity matching
prefers terminal-specific markers (`KITTY_*`, `WEZTERM_*`, and peers) over
`TERM`, because tmux rewrites `TERM` but leaves the outer terminal's own vars
untouched.

## Terminal integration

Nyala tells the terminal what the page is doing with five OSC sequences:
OSC 52 writes the clipboard (under Clipboard above), OSC 0 sets the window
title, OSC 9;4 reports load progress, OSC 22 sets the pointer shape, and
OSC 8 marks hyperlinks in the Block output. Each viewer-emitted sequence goes
out through the one ordered writer path, exactly like a bar repaint, so it
never lands inside a frame's sync bracket; the OSC 8 bytes are part of the
Block frame itself. ST terminates every sequence except OSC 52, which keeps
BEL to match tmux's `Ms` and the ecosystem copy tools (under Clipboard). ST
is the safer terminator for page-fed payloads: a BEL is one smuggleable byte,
where ST cannot appear inside a sanitised body at all.

Every payload a page can influence is sanitised before it reaches the
terminal parser. The title strips every C0 control, DEL, and C1 control and
is capped at 128 bytes cut on a rune boundary; an empty result falls back to
the URL host, or the raw URL when parsing yields no host. A hyperlink id or
URI containing any of the same control bytes is rejected whole, as is a URI
over 2,083 bytes or an id over 250 bytes, the VTE and iTerm2 limits. Nothing
is ever truncated to fit: a shortened URI is a different, wrong target, so an
oversized link is dropped and the run renders as plain text.

### Reply consumption

The key parser consumes terminal replies as well as keys. Its `ESC ]` arm
reads an OSC body to BEL or ST, and its `ESC P` arm reads a DCS body to ST,
both bounded at 4096 bytes, so a reply arriving mid-session is consumed and
dropped rather than typed into the page as text. The handshake drains its own
replies before the key reader goroutine exists; these arms are the safety net
for a late one. The one reply Nyala solicits beyond the handshake set above
is the OSC 22 capability reply, which rides the DA1-fenced probe batch: the
query sends `?` plus the thirty kitty pointer-shape names, and the terminal
answers one flag per name, paired by position. Silence, a missing terminator,
or an unparseable body all read as no support, and like OSC 11 the reply
carries no `CSI ?`, so it can never end the DA1 read early.

### The terminal title

The title is OSC 0, set once at startup (the sanitiser's fallback shows the
launch URL host while the page loads), then refreshed on each navigation
and, when the bar's fps heartbeat runs, on its once-per-second beat; an
unchanged title costs one string compare and no write. Entry pushes the terminal's own title with the XTWINOPS
stack (`CSI 22;0t`) and teardown pops it (`CSI 23;0t`), so the page title
never outlives the session. WezTerm and Konsole parse the push and pop but do
not restore on pop, so on those terminals, matched by XTVERSION identity,
teardown follows the pop with an explicit neutral title; an empty payload
would reset to the terminal default rather than the saved title, so the
fallback payload is a fixed non-empty string. Nyala never sends the `CSI 21 t`
title query: terminals ship it disabled because a reported title lands on the
input line as if typed, the CVE-2003-0070 vector, so the push-and-pop stack is
the only reliable restore.

### Load progress

OSC 9;4 carries the load state, fed by the CDP lifecycle events: a commit
sets indeterminate, firstContentfulPaint raises determinate 50%,
DOMContentLoaded raises 75%, the load event clears, and a navigation failure
sets the error state. Progress is monotonic per navigation, so an out-of-order
lifecycle event can never move the indicator backwards. The viewer re-reads
the snapshot on every frame arrival, on navigation, and on the same
once-per-second beat as the title, writing only on change. Both fields are always present
in the sequence, because some terminals treat a missing percent field oddly.

### Pointer shapes

OSC 22 sets the pointer shape from the page's computed CSS cursor, mapped by
identity onto the kitty shape names; `auto`, `none`, and any unmapped value
fall back to `default`. The write is gated on the handshake's capability
reply: a terminal that never answered sees zero pointer-shape bytes, and a
cursor mapping to a shape the terminal's flag list refused emits nothing and
leaves the last shape standing, never a substitute. The reduced multiplexer
handshake sends no OSC 22 query, so multiplexer sessions keep the terminal's
own pointer. Over the bar row the viewer writes the empty-payload reset, so
the terminal's default shape comes back where no page sits. The engine
refreshes its cursor snapshot on hover dispatch only, so a page that changes
its cursor under a still pointer waits for the next motion; that limit is a
recorded non-goal.

### Hyperlinks

OSC 8 hyperlinks ride the crisp text overlay: each glyph run carries its
nearest ancestor anchor's resolved absolute URL, and the sanitised link
becomes a cell attribute shared by every cell of the run, wide trailing cells
included. Both Block grid writers emit it, the full-frame writer and the cell
diff; the diff treats the link as global terminal state like SGR, opens and
closes it only on transitions, compares it by value in the cell-unchanged
test because the compose path rebuilds link pointers each frame, and closes
any open link before the frame ends so none leaks into the surrounding
stream. The id groups the cells of one anchor across partial repaints and is
DOM-derived, the layout generation plus a per-anchor counter, so a repaint
keeps one anchor as one link. OSC 8 is the one sequence with no capability gate,
because none exists to ask: there is no query form, and a terminal without
support ignores the sequence and paints the cells as plain text, a harmless
failure.

### Teardown

Teardown membership is deliberate per sequence. The title pop and the OSC 9;4
clear join the unconditional restore string, beside the mouse-reporting
disable, so every exit path including a fatal signal restores the title and
removes the progress indicator. OSC 22 is exempt: the terminal keeps the
pointer shape per screen, so leaving the alternate screen on exit discards
every shape Nyala set without a write. OSC 8 needs no teardown at all,
because the writers close any open link at the end of every frame.

## Graphics backends

Both graphics backends write the bitmap out of band into the full
engine-sized grid from the top left; the bitmap never enters the cell differ.
The differ touches only chrome cells, so the two paths never contend for the
same bytes.

### Kitty

When startup selects the direct transport, the live path reuses Chromium's
work. CDP delivers each screencast frame as a base64 PNG payload, and Nyala
rechunks that payload directly as Kitty `f=100,t=d,a=T` data. There is no
decode and no re-encode; the bytes Chromium encoded are the bytes the terminal
receives.

Direct transport has a hard in-band ceiling. Kitty's post-SIMD escape
parser measures about 270 MB/s (kitty issue #7005); raw RGBA at 2560x1500
is 20.5 MB of base64 per frame, about 13 fps, and no in-band pixel format
escapes that arithmetic. Live direct PNG sat on that ceiling, with 78.9 ms
terminal-side write stalls.

At startup, capability policy may select a POSIX shared-memory transport
instead. It captures JPEG q90, decodes it in Nyala, and sends raw `f=32` RGBA
over POSIX shared memory (`f=32,t=s`). Terminal-side PNG decode dominates at
full-grid sizes, so moving the decode into Nyala and handing the terminal raw
pixels is the faster trade where shm works.

A transport benchmark picked raw over shm (same frame, 30 repetitions):
direct PNG 16.07 ms, shm PNG 16.14 ms (no gain, the terminal's PNG decode
dominates, not transport), shm raw `f=32` 10.74 ms, in-band raw 54.88 ms.
The pre-agreed sub-2x gate fired, so raw-over-shm shipped.

The transport resolves once per run (`internal/viewer/transport.go`):
override, then deny list, then probe, then allow list, then direct. The
probe is icat-style, a real 1x1 payload inside the DA1-fenced handshake.
Linux opens the POSIX objects through `/dev/shm`; Darwin binds `shm_open` and
`shm_unlink` from libSystem through purego, so the binary remains cgo-free.
Both implementations use a fresh object for each frame. Names have one leading
slash and no other slash. Creation is exclusive and read-write with mode
`0600`; each object is sized once, mapped shared and read-write, filled through
the mapping, unmapped, and closed. Darwin validates its 31-byte name limit
before the libSystem call and never writes through the descriptor.

Transport policy is common across the platforms. Seeds allow WezTerm on Linux
(it supports `t=s` but never acks the probe) and deny WezTerm on macOS (it
renders it wrongly). Each shm object carries one complete transmission,
because chunked shm does not display on Ghostty. Pacing keeps one frame in
flight, opened by the correlated current frame's OK reply or a 40 ms deadline
(the gate lives in `internal/viewer/stream.go`; the frame assembler arms it).
Startup selects direct PNG when capability policy does not select shm. Probe
replies and correlated replies for invalidated or stale transmits do not affect
live pacing, and their failures are ignored. Once startup selects shm, a
per-frame object creation, mapping, close, transmit, or terminal write error
stops the stream through its normal shutdown path, as does a rejection for a
current or unknown transmit. Nyala does not demote the Kitty stint to direct
PNG or re-arm a demotion on a later backend cycle.

JPEG q90 is the shm capture format because PNG plane decode is
content-scaled (96.9 ms on live pages) while JPEG q90 is content-stable
(17.2 ms); on the codec fixture it is 44% of the PNG payload. The aquarium
progression reads 12.7 fps direct, 24.4 shm PNG, 59.8 shm JPEG, locked to
the 60 Hz cap; an uncapped
run (82.6 in, 72.9 out) proves 60 is Chromium's cap, not the pipeline's.

Frame discipline avoids blanking:

- Outside Herdr, frames alternate two image IDs. Nyala transmits and puts the next image,
  then deletes the previous one (`a=d,d=I,i=<id>`, the capital `I` frees the
  stored data). The old image leaves the screen only after the new one is
  placed, so no blank shows between frames. The `a=f,r=1,o=z`
  animation-frame edit was tried and rejected: it rendered partial or black
  on kitty and WezTerm, so the double-buffered id swap stays.
- In Herdr, direct PNG frames keep one image ID and one placement ID. Each
  full transfer replaces that placement. Herdr can keep its file-backed
  upload path when the pane's image identity stays the same.
- Direct PNG uses `q=2` to suppress terminal replies. Shared memory uses `q=0`
  so each transmit returns the acknowledgement used for pacing or a rejection
  that stops the stream.

Placement is classic cursor-anchored (`a=T`) with `C=1` so the cursor does
not move, followed by an explicit absolute cursor position (`CSI <row>;<col> H`)
before the next frame. The default `C=0` landing position is undefined at
screen edges and differs across terminals, which is what smears a following
cell diff. Unicode placeholders (`U=1`) render only on kitty itself, so Nyala
does not use them.

### Sixel

Sixel is the compatibility backend, not a motion path: the limiter is the
terminal's serial, input-blocking decode of each payload (notcurses calls
its own Sixel unusable at double-digit fps).

Nyala owns its opaque Sixel encoder, derived from mattn/go-sixel v0.0.12. It
replaced the charmbracelet encoder, which cost about 1 s and 14.9 million
allocations per 2560x2880 frame, rebuilding its median-cut palette every
frame; the replacement measured 9.45x faster (960.4 to 101.7 ms) with
99.993% fewer allocations. 256 colours beat 192 on SSIM on every quality
image; dithering was slower, larger, and lower quality. The owner accepted
PSNR 45.4 down to 39.3 dB for the speed, and dropping alpha handling
(frames are opaque) cut a further 8.4% byte-identically. The encoder's
shape:

- An adaptive 256-colour palette with dithering off. The palette and its
  RGB555 lookup table persist across frames
  (`internal/render/sixel/palette_reuse.go`): a frame reuses them while the
  mapping mean squared error stays within max(1.15 x baseline, 12.0); on
  re-quantisation, registers within CIEDE2000 2.3 of an old entry freeze;
  geometry changes invalidate. Per-frame re-quantisation changed 202.7 of
  255 registers per frame pair with unchanged-pixel dE up to 15.99; reuse
  changed none with dE 0.000. A fixed-palette experiment showed the speed
  (warm encode 45.8% faster) but cost 7.6 to 12.2 dB PSNR, so fixed
  palettes were rejected; delta-triggered reuse keeps the quality.
- A 64K per-frame pixel sample cap feeding the quantiser, so palette cost
  does not scale with grid size: -30.39% on combined decode plus encode for
  +1.75% payload, with all 800 swept features staying above 3:1 contrast.
- RGB555 indexed colour mapping through a lookup table.
- One shared min(4, GOMAXPROCS) worker budget (`encoder.go`): adaptive
  mapping splits at 500,000 pixels, band emission activates at 120 bands,
  and LUT build, mapping, and emission never overlap, with payloads
  byte-identical to serial. The accepting sweep cut full-encode p90 28 to
  50%, to 14.55, 19.01, and 21.65 ms at the three benchmark surfaces.
- Rightmost-nonblank plane bounds per colour skip about 38% of columns
  (median -8.85% and -12.19%), because full-width plane scans were about
  76% of encoder CPU.
- An 8192 repeat ceiling per run cuts payload 17 to 21% byte-identically,
  stays under tmux's native 10000 cap, and sits inside the 32767 that DEC
  STD 070 permits; the known trade is mlterm 3.9.2 and older overrunning
  above 512.
- One ordered write per frame; the whole payload lands in one buffer first.

Sixel captures JPEG q90 by a live reversal: PNG-fed Sixel won the synthetic
byte sweep but lost live, where the 66.7 ms PNG decode capped ingest near
25 fps, so capture returned to JPEG q90 (13.5 ms decode, 60 fps ingest).

Frame discipline: home the cursor, enter DECSDM with `CSI ?80h`, write the
DCS, reset with `CSI ?80l`. DECSDM gives fixed-page, non-scrolling placement
that leaves the text cursor unchanged on current xterm, foot, and WezTerm.
DEC 2026 brackets the frame.

Sixel pixels are opaque with no alpha, so a same-size full-viewport overwrite
needs no clear. Nyala clears the region only on viewport shrink or geometry
change.

## Multiplexer graphics

### Native Sixel in tmux

tmux must report Sixel (attribute 4 in DA1) and identify as version 3.6 or
later in XTVERSION. Nyala also requires valid cell pixel geometry and checks
its terminal compatibility rules. Versions 3.4 through 3.5a have a fixed
1 MiB device control string (DCS) limit. Larger frames disappear without an
error.

Nyala uses the socket in `TMUX` for read-only queries. It matches `TMUX_PANE`
to the current terminal device before it trusts the pane ID. Every attached
client that can view the pane's window must advertise Sixel. This includes
linked sessions and clients on another window in those sessions, but excludes
control-mode clients. Nyala also needs a readable server `input-buffer-size`.
If any check fails, automatic selection falls back to Block unless the
experimental Kitty path qualifies. `--backend=sixel` reports an unsupported
backend. Nyala checks at startup, so restart it after a client change.

The reserved bottom row accounts for tmux's image height limit. See
[The bottom bar](#the-bottom-bar).

### Kitty graphics in Herdr

Nyala enables Kitty when Herdr answers the standard graphics query with
`OK` for the probe image ID and Nyala has valid cell pixel geometry. The
transport passes Chromium's PNG directly. Nyala neither probes shared memory
nor calls a Herdr-specific API. Herdr owns image placement within the pane.

### Experimental Kitty passthrough in tmux

This path uses Kitty Unicode placeholders and direct PNG, not shared
memory. It applies to passthrough on tmux 3.6 and later, including tmux 3.8.
It does not use native Kitty graphics from development versions of tmux.
tmux does not forward replies to wrapped Kitty probes, so
`NYALA_TMUX_KITTY=1` is the user's assertion that the outer terminal supports
Kitty Unicode placeholders.

Nyala checks the pane against the current terminal device, requires valid
cell pixel geometry, and rejects known nested multiplexer sessions. It
requires one attached client that can see the pane's window, with UTF-8 and
truecolour. The pane's effective `allow-passthrough` must be `on`, not `all`.
The server's `input-buffer-size` must be at least 8192 bytes to accept a
wrapped 4096-byte Kitty chunk and its framing.

Nyala checks those conditions at startup and does not monitor client changes.
Restart Nyala after reattaching or changing clients. Nested tmux and
concurrent Kitty applications are outside this experimental setup. If a
check fails, automatic selection still considers native Sixel and Block.

### zellij and screen

zellij Sixel is blocked outright, regardless of DA1 and geometry. Two
zellij-side defects, root-caused in zellij source, would present as Nyala
bugs: an image-id collision that drops every third full-replacement frame,
and bare re-emission of image chunks to the host that breaks page scrolling.
Nyala uses Block in zellij. The compatibility policy identifies zellij by
its `Zellij(<version>)` XTVERSION reply. Screen also uses Block.

### tmux Sixel pacing and quality ladder

tmux re-encodes stored images for its host terminal, so sustained full-rate
Sixel overwhelms it. The viewer survives this with tmux-only machinery
(`internal/viewer/sixelbudget.go`); native terminals and other multiplexers
keep normal behaviour.

- Hard byte limit: Nyala reads `input-buffer-size` without changing the
  server. It disables native Sixel if the value is unknown or below 1 MiB.
  The hard limit is 900,000 bytes for settings from 1 MiB to below 2 MiB.
  For larger settings, Nyala rounds down to a power of two, subtracts one
  byte and caps the result at 15 MiB. Nyala drops any image that exceeds
  the hard limit, even after the last downscale step.
- Motion target: tmux 3.6.x also caps the target at 1.5 MB to limit the
  re-encoded output rate. Over-target frames try downscale factors 1.5, 2,
  3, and 4, then scale back to the exact grid extent. At the last factor,
  Nyala can send an image above the motion target if it fits the hard limit.
- Emit pacing: a version-tuned floor (250 ms on 3.7+, 300 ms below or
  unknown) at the latest-wins coalescing seam, backing off to the last write
  duration plus 50 ms, capped at 1 s. The pty write blocks at tmux's read
  pace, so write duration measures saturation directly.
- Ladder escalation: two consecutive writes at or above the floor step the
  ladder one factor; the next two samples are skipped as old-factor backlog,
  so escalations cannot chain on stale evidence. The criterion is whether
  tmux ever idles between frames, not a multiple of the floor.
- Quality recovery: a one-step-sharper probe every 10 budgeted frames, judged
  inside a band at 60 percent of the floor and needing a flat-or-falling
  write window; a failed probe retreats and silences probing until the next
  settle. Band judgement exists because tmux's buffers absorb a failing
  sharper factor and hide it from the escalation threshold.
- Idle settle-to-sharp: after two idle pacing intervals, Nyala re-encodes a
  blurred final frame at factor 1. Nyala sends the sharper image only if it
  fits the hard limit. The next motion episode resumes at the pre-settle
  factor. A resize invalidates retained frames from the old geometry.
- No per-frame erase: a steady frame carries a bare cursor-home prefix; new
  images paint over old, and tmux's stored-image LRU (capped at 20 on 3.6b+)
  bounds the stack. An erase prefix caused visible strobe. DEC 2026 is
  suppressed under tmux on every version because tmux does not defer image
  operations under sync.
- `NYALA_DEBUG_TIMING=1` reports per-frame byte size, active factor, write
  duration, and next interval on stderr.

## The bottom bar

Nyala reserves one persistent bottom row for browser chrome, hand-rolled inside
`internal/viewer` with no TUI framework. Bubble Tea v2 was dropped by owner
decision: a framework brings PTY-ownership complexity, and the viewer already
owns raw mode, mouse reporting, DEC 2026 synchronised output, and the writer
goroutine. The bar is a set of bytes folded into the existing frame assembly,
not a second renderer.

The bottom row is the only position that costs no render-path change on any
backend. DECSDM anchors the Sixel image at the screen origin, so when the
engine grid shrinks by one row the freed row falls at the bottom; Kitty and
Block home to 1;1 and are unaffected. `engineRows` in
`internal/viewer/bar.go` reserves the row unconditionally, handing rows-1 to
the engine so the bar row sits outside every frame. The same reservation
absorbs tmux's image-crop rule: tmux rescales any Sixel image taller than
pane rows minus one, keeping the bottom and cropping the top, so a
full-pane-height frame would lose its top row.

### Layout and glyphs

The chrome row is `← │ → │ ⟳ │ ⌂ │ <transport> <url> [│ fps] │ <render> │ <colour> │ ☰`.
Button order matches the Chromium toolbar. The ☰ menu button anchors to the
bottom-right corner as a stable click target. The render glyph and colour badge
form a fixed status group beside it. The optional fps segment sits before that
group, so toggling fps never moves the status group. The URL region absorbs the
remaining width and truncates with an ellipsis. Glyph click targets pad to
three cells. The render target uses a fixed two-cell field plus the same outer
padding, so every render glyph has one explicit trailing cell.
For example, the Kitty status group is `│ 🅺  │ RGB  │ ☰`.

The render glyph is ⬒ U+2B12 for Block quadrant, ⬢ U+2B22 for Block sextant,
🅢 U+1F162 for Sixel, and 🅺 U+1F17A for Kitty. The Block glyph tracks the live
scheme.
Clicking the indicator cycles backends, mirroring Alt+R; a right-click steps
the same enabled cycle backwards, mirroring Alt+Shift+R. Clicking the colour
badge cycles colour, mirroring Alt+C; a right-click cycles backwards, mirroring
Alt+Shift+C. The indicator and colour badge read the button for these reverse
actions. No nav button has a history dropdown to open here, so elsewhere a
click is a click whichever button sent it. The URL region starts with one
fixed transport cell and one padding cell. A yellow ★ marks an HTTPS URL, a
normal-foreground ☆ marks an HTTP URL, and other schemes leave the transport
cell blank. The filled star identifies HTTPS transport only. It makes no trust
claim. A braille spinner (U+2800 block) replaces the transport glyph until the
first frame lands, so the URL does not move during loading. URL changes refresh
the glyph.

Glyph rules: never use U+FE0F or a presentation variant. Every segment position
and mouse hit span derives from `go-runewidth` measurement, so the fixed glyph
group stays aligned with the status group.

### Colour and the fps segment

The bar uses the terminal default foreground and background plus the base 16
ANSI colours only, so it follows the user's theme. Bold marks focus and press
feedback. The loading spinner uses bright white on a dark bar and black on a
light bar. The reverse-video fallback keeps its default contrast. The HTTPS
transport glyph uses ANSI yellow. Each prefix style returns to the resolved bar
style after the glyph, so the padding cell and URL keep the normal bar style.
Fixed 240-palette entries were rejected because they ignore the theme.

The four-cell colour badge is always present. True Colour and Kitty read
`RGB `, Sixel reads `256a`, Block 240 Static reads `240s`, Block 240 Adaptive
reads `240a`, 24 Shades reads `24g `, and 240 Shades reads `240g`. The Block
labels report the effective palette path, so an unavailable OSC 4 probe reads
as static even when the adaptive policy remains selected. Alt+C,
Alt+P, and the automatic true-colour fallback update the badge through the
normal bar repaint.

The bar is a grey band that sits with the terminal's own polarity, chosen once
from the handshake's OSC 11 reply (`resolveBarStyle`). A dark terminal gets
index 8 with index 7 ink, a light one index 7 with index 0 ink. Both are
theme-defined, so the bar is the user's grey rather than a pinned shade.
Reverse video holds wherever the polarity is unknown: the terminal stayed
silent, answered unreadably, or runs under a multiplexer that sends no query.
The style is a
single SGR opener resolved on every `configureBackends` path and read through
`State.barStyle`, so the chrome row, the URL editor, the find field, the menu,
and the About line share one decision, and a `State` that skipped the handshake
still draws a legible bar.

Bold over the light style is the one combination to watch: terminals that
brighten indexes 0 to 7 under SGR 1 render index 0 ink as index 8, a grey on
the index 7 band.

The fps segment is present when a meter
is armed and the menu has not hidden it: `NYALA_FPS_TIMING` arms one at startup,
and the menu's Show FPS command arms one mid-session. Hiding keeps the meter,
because the switch's quit summary reads that same one, so only the badge and its
heartbeat stop. Its heartbeat ticker is armed with the meter rather than at loop
start, so a session that never shows the badge starts no ticker and pays only a
nil check. The segment reads `↓ NN.n │ ↑ NN.n │ ≌ N`: ↓ is the
ingest rate from the engine, ↑ the rate written to the terminal, ≌ the
cumulative received total. Rates carry one decimal and read `--` when idle.

### Copy and paste signals

Three input outcomes change nothing on the page, so the bar reports them: a
completed copy with the bytes it wrote, a selection refused for breaking the
74,994-byte bound, and a paste Chromium discarded because no page field held
focus. Without them all three read exactly like success, and a reader who is
unsure presses the chord again or checks another application. Space arbitration
has the same shape and gets no signal, because the page visibly scrolls or
types.

A signal takes the URL region's text for two seconds. That is the one flexible
segment, so every button keeps its column, the ☰ anchor never moves, and the row
still measures exactly `cols` cells in every signal state, truncating with the
same ellipsis a long URL takes. Each line leads with its outcome, so a
phone-width row that truncates the tail still tells the three apart. A signal is
not a mode: keys, clicks, and frames keep flowing while it shows, and the
consumer loop wakes at the expiry, so an idle page that pushes no frames clears
it with no key press. The repaint re-reads the deadline too, so no frame can
draw a signal past its hold.

The discarded paste needed a decision, because `Input.insertText` reports
nothing back. The engine asks the page instead: one script rides the same
bounded dispatch as the insert and answers `field`, `none`, or `unknown`. Only
`none` is reported as a discard, the insert runs whatever the answer, so paste
behaviour is unchanged and only the report is new, and a dropped dispatch
reports nothing for the reason every other dropped keystroke is silent.

Two oracles answer, because live Chromium proved each one wrong on its own, and
the wrong direction is a bar line contradicting the text on screen. Chromium
commits the text through the focused frame's input-method controller, which
inserts at that frame's editing selection. `document.queryCommandEnabled`
(`insertText`) is that same question, and it catches what the DOM hides:
blurring a text field leaves the editing selection inside it, so an insert still
lands with the body as the active element. It misses the opposite case, a page
that drops the document selection while a field keeps focus, which still takes
the insert. So the script also tests the focused element itself, `none` needs
both to say no, and either one saying yes is `field`. Silence costs a reader
nothing. Focus nests, so the walk descends into a focused frame, because a
frame's field is invisible to the document above it; a cross-origin frame is
opaque and answers `unknown`. The editing query is deprecated, so a browser that
drops it throws, which also answers `unknown` rather than guessing.

A copy with nothing selected stays silent as well. The selection read cannot
tell an empty selection from a dispatch that did not land, so any line it drew
would be a guess, and the clipboard was never going to change either way.

### Rendering and input

The bar folds into the same DEC 2026 sync bracket as the page frame, so both
present atomically. It repaints after any clear and draws only when the
terminal is at least two rows tall; a one-row terminal gets the page alone,
and a bar mode that can no longer draw exits rather than swallow keys as an
invisible modal.

Mouse routing is a row check: bar-row events hit-test the segment spans of
the last repaint, every other row forwards to CDP. Side buttons 8 to 11
dispatch history back and forward and never reach the page.

A press captures the pointer for the surface under it until the release, the
way a browser does, so a drag is one whole gesture rather than a row check per
report. SGR motion (bit 32) forwards as CDP `mouseMoved`, and the button it
carries decides what Chromium reads it as. A held button goes out in both the
button name and the pressed-buttons mask, which is what makes Chromium treat
the move as a drag of that button. Buttonless motion goes out as the CDP `none`
button with an empty mask, which Chromium reads as a hover, so CSS hover state
follows the pointer. On the wire that hover carries `"button":"none"` and no
`buttons` field at all: cdproto tags the field `omitempty,omitzero`, so the
empty mask is dropped, and CDP reads the missing field as none. A drag that
started on the page keeps reaching the page once the pointer crosses the bar
row, because the engine clamps the cell to its grid; without that, sliding a
slider off the bottom of the viewport would open the URL editor mid-gesture. A
drag that started on the bar row never injects page motion, since the bar holds
no draggable control. Button-held motion with no capture is dropped, and a
release with no capture reaches no page, because forwarding a bare release
would click whatever now sits under the pointer. Buttonless motion needs no
capture, as it belongs to no gesture: off the bar row it forwards as a hover,
and on the bar row it is dropped, because the bar sits outside the engine grid
and holds no page to hover. A bar mode taking over input drops the capture,
since the mode swallows the release that would otherwise clear it. Moves leave
the click chain untouched, so neither a drag inside a double-click gesture nor
a hover between two presses can break word selection.

Mouse reporting enables button-event tracking (mode 1002) before any-event
tracking (mode 1003), then SGR encoding (mode 1006). That order is deliberate.
A terminal holds one mouse-target state, so the last target it accepts wins,
and a terminal that ignores 1003 keeps drag rather than losing mouse reporting
altogether. PuTTY before 0.79 and Chromebook hterm drop reporting entirely when
asked for 1003 alone. `exitTerminal` resets all three unconditionally.

Motion is coalesced at 16 ms, latest-wins, on the consumer loop goroutine ahead
of the shared key worker. It reads no button, so a drag collapses exactly as a
hover does and both halves of 1003's motion take one path. Most terminals report
motion only when the
pointer enters a new cell, but VTE reports every pixel and VTE drives
gnome-terminal: a pointer that never left one cell was measured producing about
thirty reports. Sixteen milliseconds is roughly one frame at 60 Hz, and a hover
is a request to repaint, so dispatching faster than the page can repaint buys
nothing. The first move of a burst goes out at once, further moves inside the
window collapse into one held report, and the loop wakes at the window edge to
send it, so the last move of a burst is never swallowed. A press, release, or
wheel flushes the held report before dispatching itself, so coalescing can
never reorder motion against a button. The coalescer sits outside `keySender`
on purpose: that worker is cumulative and drops nothing by design, which is
what a burst of caret keys needs and the opposite of what motion needs.

Nyala tracks no terminal focus, so it dispatches whatever motion the terminal
sends, focused or not. Terminals already disagree here: xterm, VTE, konsole, and
foot report motion unfocused, while iTerm2 and Windows Terminal do not. Matching
them would need focus reporting (mode 1004), a new input class, new session
state, and a new teardown pairing, for no gain. An unfocused hover only
repaints CSS hover state in a page nobody is looking at, and it cannot click,
navigate, or type. The coalescer caps the cost either way, and a still pointer
produces no motion at all.

URL edit mode opens on Ctrl+L or a URL click. The editor takes the whole bar:
reverse video, a ❯ prompt, and a hand-rolled readline subset (Ctrl+A/E/U/K/W,
arrows, Home, End, Delete, Backspace) over a rune slice with a horizontal
viewport. Enter commits; schemeless input with a dot gains `https://`, and
only http, https, and file schemes pass, so a committed `javascript:` URL can
never execute in the page origin. Escape cancels. Registered Alt bindings run
first. A focus flag routes other keys and paste to the editor instead of CDP.
A bar-row press places the caret at the clicked cell, walking the visible runes by
`go-runewidth` width from the viewport offset, so a wide or ambiguous-width
glyph counts the cells it really occupies. The cursor is a rune index
throughout, so no mapping can split UTF-8. Every cell of a wide grapheme places
the caret before it, since the trailing cell carries background only and is not
a position of its own. A click on the prompt clamps to the leftmost visible rune
and a click past the last visible rune clamps after it. `renderBarEdit` reports
the prompt width it drew and the session keeps it, so a click maps against the
row as drawn: a row too narrow for the prompt gives every cell to the text,
which a recomputed layout could get wrong by two cells. Menu mode opens via ☰
or Alt+F (F10 is avoided; terminals grab it) and offers Show FPS or Hide FPS,
About, and Quit from a command registry. The fps item names the action rather
than the state, so the label says what activating it does; both labels are four
letters wide, so the menu row and its click spans hold their columns across a
toggle. There is no key binding for it, and no config file, so the choice lives
for the session.

Find mode opens on Ctrl+F, which was unbound and is not printable, so no page
text route loses a character to it. It is the front end for the search seam
under Find above. The row is `⌕ <query> │ <count> │ ✕`, with the query field
absorbing the width the count and the right-anchored close button leave, and it
shares one field measurement and one text-editing path with the URL editor. A
row too narrow to hold the count beside a usable field degrades to the prompt
and the field alone.

Search runs on submission, not per keystroke. Each dispatch re-collects and
repaints every match in the page, and the worker queue is cumulative rather than
latest-wins, so search-as-you-type would run one whole search per character and
only the last would matter. Enter searches a changed query and steps to the next
match otherwise, so Enter after typing finds and Enter again walks the matches.
Shift+Enter steps back, read from the CDP modifier bits a Kitty key report
carried, which no legacy terminal can send, so Down and Up step next and
previous on every terminal: vertical keys walk matches, horizontal keys stay on
the caret. Those two are the only find binding a user cannot discover from the
row, which holds no hint text.

`renderFindCount` is a pure function of the state snapshot, so any number of
repaints of one state draw one row. It shows `current/total`, `0/0` for a page
with no match, an ellipsis for a fresh query still in flight (a new query zeroes
the counts, and `0/0` would claim no matches before the page answered), the
counts it steps through for a step in flight, and the last counts plus `!` when a
dispatch was dropped, because those highlights are still painted so the mark says
the keypress did not land rather than that the count is wrong.

Escape clears the search and closes the mode, so it never reaches the page; a
paste lands in the query field as one unit with newlines stripped, so it cannot
smuggle a submission. One known rough edge: a page-initiated navigation while
find is open drops the engine's search state, so the row reads `0/0` and Enter
steps nothing until the query changes. Escape and retype recovers. The viewer
does not detect it by comparing the state's query, because the dispatch is
asynchronous, so a second quick Enter would re-search instead of stepping.

Home is the launch URL, permanently; there is no home-page configuration
because a shell alias does that job and the binary stays stateless. The bar
pulls `CurrentURL` from the engine's committed navigation state on each
repaint and strips control characters at the display boundary.

### The About row

The menu's About command swaps the bar for a one-row capability summary:

```text
Nyala dev │ Chrome 150 │ Kitty 0.4.0 │ ✓ Kitty (shm) │ ✗ Sixel │ ✓ Sextant │ Colour: True Colour │ WebGL ➁  (Intel) │ ✕
```

Every field formats from one `capabilitySnapshot`, built in
`internal/viewer/capability.go` from configured state and the current palette
policy: `State.Enabled`, the resolved Kitty transport, the sextant gate, the
colour mode, the selected palette policy, the stored handshake identity, the
engine's browser version, and the engine's WebGL state. The fields show the
selected policy and effective decisions, not raw probe evidence. A backend
that the multiplexer rule or the compatibility list
blocked reads `✗` even when the terminal answered the probe. `?` is reserved
for unknown runtime state, which today is only the WebGL tier before or without
a probe result. The terminal field is omitted when the handshake gave no
XTVERSION identity, and the browser field when the engine has no version. The
`256` mode reads `240 Adaptive` or `240 Static`. The grey mode reads `240
Shades` or `24 Shades`. These labels show the effective palette path without
protocol terms. Registered Alt render bindings repaint the row while About
stays open. Alt+C and Alt+P update the colour label when their gates permit a
change. The row
keeps the bar's `│` separator rather than the ASCII `|` of the original mock-up,
because the About row shares the chrome row's framing.

The WebGL probe runs once in `startSession`, after the browser version fetch
and while the page is still `about:blank`, under a two-second timeout. Context
creation counts as support: `webgl.js` asks for `webgl2`, then `webgl`, then
`experimental-webgl`, and records the first version that answers. A software
rasteriser creates a context the same way, so the tier never claims hardware
acceleration. A tier other than unknown caches across crash rebuilds, and a
failure or timeout leaves it unknown so the next session start retries. The
vendor label is compact, whole-word, control-stripped text capped at 24 cells,
taken from the vendor string or from the vendor that an `ANGLE (...)` renderer
wraps. The viewer reads the state through the optional `webGLReporter`
interface, so the `Engine` seam and every fake stay unchanged, and a fake that
lacks it shows `?`.

Fields drop whole when the row does not fit, highest priority number first,
and one `…` field stands in for whatever dropped:

1. the close control, which never drops
2. the Nyala version
3. the browser
4. the terminal
5. Kitty and its transport
6. Sixel
7. Sextant
8. the colour mode
9. the WebGL tier
10. the WebGL vendor

The vendor is a glued field with no separator before it, so its double space
before the parenthesis survives the join, and it drops with the tier or on its
own before it. When no field fits, the row is ` … │ ✕ `, and a terminal
narrower than seven cells falls back to `closeAnchoredRow`, which keeps only
whole spans and drops the ellipsis before the close. The `✓` and `✗` glyphs are coloured green and red as a hint added
after layout, so the glyph carries the meaning alone: `ColourXTermGrey` writes
no colour SGR and the row still reads. <kbd>Esc</kbd> closes back to chrome, and
the quit keys quit. The row swallows page input and other unbound keys, but no
registered Alt binding.

### Key bindings and text input

Key bindings live in `internal/viewer/keys.go`. Every registered Alt binding
runs before the active navigation-bar mode handles other input, so each one
works in chrome, URL edit, find, menu, and About modes. Rendering:
<kbd>Alt</kbd>+<kbd>R</kbd> and <kbd>Alt</kbd>+<kbd>Shift</kbd>+<kbd>R</kbd>
cycle the backend, <kbd>Alt</kbd>+<kbd>B</kbd> and
<kbd>Alt</kbd>+<kbd>Shift</kbd>+<kbd>B</kbd> cycle the Block scheme,
<kbd>Alt</kbd>+<kbd>C</kbd> and <kbd>Alt</kbd>+<kbd>Shift</kbd>+<kbd>C</kbd>
cycle colour, and <kbd>Alt</kbd>+<kbd>P</kbd> toggles the Block palette policy.
Navigation: Alt+Left and Alt+Right step history, and Alt+Home opens the launch
URL. In chrome, Ctrl+R and F5 reload, Ctrl+L edits the URL, Ctrl+F opens find,
and Ctrl+A selects the page. Alt+F opens the menu, and Alt+W copies the page
selection. Ctrl+Q or Ctrl+C quits from every mode. Page keys: Tab, Shift+Tab,
and Enter forward for focus traversal and activation, the arrows, Home, End,
PageUp, and PageDown forward for the caret and page scrolling, and every
unbound printable key, Backspace, Delete, and Escape forward as page text input.

Render actions keep the active bar mode and repaint its row. Alt+F closes the
active mode and opens the menu. Alt+W closes a temporary mode so the chrome row
can report the copy result. Alt+Left, Alt+Right, and Alt+Home close a temporary
mode before navigation. When they close find, they clear its page highlights
first. The Block-only render cycles and the Alt+P capability checks still apply
in every bar mode.

The Kitty keyboard protocol is an enabler for find-in-page, which needs
Shift+Enter for its previous-match step; legacy terminal encoding has no signal
for that chord. The capability comes from the startup handshake, under The
handshake above. The reduced multiplexer handshake omits the query, so tmux,
zellij, screen, and herdr sessions keep the legacy parser: a multiplexer would
have to translate the reports back for its own key handling.

The legacy decoder waits briefly for the byte after Escape, even when it
arrives in a separate read. A lone Escape takes effect when the lookahead
expires. The decoder also uses bounded lookahead to tell terminal replies
from legacy Alt chords. The change fixes the decoder's handling of split
Escape sequences. It does not establish why input failed in Herdr with remote
Chromium.

Nyala pushes flag 1, disambiguate escape codes, and nothing else. That alone
delivers what Nyala binds, and it leaves plain typing on the normal UTF-8 path.
Event types (2) would double every keystroke with release reports,
report-all-keys (8) would drag typing onto the report path for no gain, and
alternate keys (4) and associated text (16) add parsing surface and per-terminal
variation with no use here. The protocol is a mode stack, so the enable pushes
one entry and `exitTerminal` pops exactly one, conditionally where its other
resets are unconditional: an unmatched pop would discard the entry an outer
application pushed. `enterTerminalModes` and `exitTerminal` are one function
either side of that pairing.

Reports normalise at the single decode point rather than at each consumer. A
report for a key with a legacy control byte becomes that byte event carrying the
report's modifiers, so Escape becomes ESC, Enter CR, Tab TAB, Backspace DEL, and
a Ctrl+Q report the Ctrl+Q byte. Without that, the three bar-mode handlers,
which bind Esc as a byte, would have left the URL editor, the menu, and the
About row uncloseable on a supporting terminal. Shift+Tab stays separate, since
it is its own binding on the legacy path. Shift+Enter carries its modifier
through as CDP bits, which is the signal find mode reads for its previous-match
step; in chrome mode it activates exactly like a legacy carriage return.

Page text input is the default for unbound keys, which is why plain q does not
quit. A printable byte or an assembled rune forwards through `SendText`, and
Backspace, Delete, and Escape forward as full key strokes; Delete and Escape
reach the page only when no bar mode claimed them, since the bar's text fields
bind Delete to their own forward delete. Text shares the serialised key
worker, so typing and page keys reach the engine in arrival order and a wedged
browser drops input instead of freezing quit or draw. Which CDP event each one
becomes is the engine's choice, under Input above.

Bracketed paste (mode 2004) is a session mode, enabled beside mouse reporting
after the startup handshake and disabled by `exitTerminal` on every teardown.
It is deliberately not scoped to a bar mode: with the mode off, a paste would
arrive as loose bytes and type character by character, so a pasted newline
would press Enter and submit a form. A paste outside a bar mode forwards
through `SendPaste`, so the whole payload lands atomically; inside the URL
editor or the find field it lands in that field instead. The parser's 1 MiB
paste bound is unchanged. `SendPaste` reports a paste the page had no field to
take, which the bar says out loud, under Copy and paste signals above.

`exitTerminal` is the sole teardown composer, and a deferred restore behind a
`sync.Once` is the only path that runs it. Go runs no deferred function for a
signal left on its default disposition, so a signal that killed Nyala would
strand raw mode, mouse reporting, and bracketed paste, and the shell would then
read SGR mouse reports as typed input. `Stream` therefore watches SIGHUP,
SIGINT, SIGQUIT, and SIGTERM on one channel. A caught signal restores through
the same `Once`, calls `Engine.Close` to request the 500 ms bounded
`Browser.close`, then exits 128 plus the signal number: 129, 130, 131, and 143.
The browser result cannot change the status or block exit past the bounded raw
cancellation fallback. SIGHUP is the closed terminal window and the dropped
SSH connection; SIGINT and SIGQUIT are a `kill` from another shell. Catching
SIGINT does not touch interactive Ctrl+C, because raw mode clears ISIG and the
byte `0x03` quits through the key path. SIGKILL is uncatchable and a dropped
connection defeats in-process cleanup, so `reset` stays the answer to both.

## Correctness architecture

`just diagnostics` is the single correctness workflow. It runs the
browser-free quality harness, the independent Block, Kitty, and Sixel
protocol gates, and the Chromium-fed corpus placement, paint, and scroll
checks. Evidence goes to gitignored `testdata/diagnostics/`. The focused
interop recipes select the same tests for development; they define no
separate workflow.

Diagnostics drive the normal engine and viewer seams. Block checks consume
`viewer.State.ComposeBlockFrame`, the same compose result production terminal
writing consumes, so no second compositor exists to drift.
`render.Frame.Diagnostics` is a sidecar carrying capture geometry and
calibration evidence; it stays nil in production.

The Chromium-backed leg of `just diagnostics` uses a fresh temporary profile.
`just cdp-interop`, `just live`, and `just internal-render-metrics` do the same.
Each recipe sets `NYALA_PROFILE` to the temporary directory and removes it on
exit. They cannot read or change a developer's persistent profile. The
browser-free protocol legs create no profile.

Package ownership: `internal/cellraster` rasterises Block grids browser-free,
`internal/renderdecode` owns the correctness-only Kitty and Sixel decoders,
`internal/metrics` owns the image, placement, paint, scroll, and fixture-gate
metrics, and `internal/harness` owns the harness and golden data.

Every threshold traces to calibration or a measured noise floor, never a
guess. The Sixel encoder is lossy and non-deterministic, so its PSNR noise
floor was measured as the worst standard deviation over 64 repeated renders,
0.2 dB; Sixel PSNR gets a 1.5 dB band against the global 0.5 dB. Goldens are
text artefacts (JSON, txtar) behind an explicit `UPDATE_GOLDEN` refresh
because they diff cleanly and survive font and Chromium bumps. Bitmap and
overlay goldens regenerate on amd64 only: arm64 contracts float operations to
FMA, shifting results below 1e-12, so their tests skip elsewhere. The overlay
run sets in `testdata/overlay/*.runs` are hand-written, one run per line as
`col row fgHex rank text`, with the rank column mandatory: an optional one
cannot be told from text that starts with an integer. `just genruns` rewrites
only the foreground colours, picking the higher-contrast one against each run's
composed background, so a hand-set rank survives regeneration.

Five of the six run sets place one run per row in rank order, where the fold
takes its no-copy fast path and no cell is ever contested, so paint order cannot
show in the result. `overlay-contended` exists to close that: two of its runs
overlap by two cells at different ranks, out of rank order in the file, straddling
the page's dark-to-light boundary so the two runs carry different foregrounds.
The contested cells then name their winner in the golden archive, in the
recovered text, and in the contrast count. A fixture whose runs contend states
the text the fold leaves visible in `OverlayFixture.Truth`, because the run that
loses a cell also loses that grapheme from what the matcher can read back.

The legibility ruler is a pure-Go exact-template glyph matcher graded by
character error rate. Its calibration measured the bitmap legibility floor
recorded in The crisp text overlay, the number that justifies the overlay.
WCAG AA contrast at 4.5:1 gates low-contrast text cells as a co-signal.

Corpus placement gates default to mean right-edge error under 0.5 cells, max
under 1.0, zero drifted boxes, zero same-row collisions, and zero off-grid
boxes. Fixtures run at 120x40 and 240x48. A fixture may loosen a threshold
only with a reason recorded in its `fixture.json`, and
`paint-baseline.json` records expected paint failures explicitly rather than
letting them pass silently. Live Chromium integration tests sit behind the
`live` build tag (`just live`) and stay off CI.

CI compiles more than the untagged suite. The `Gates` job runs the three
Chromium-free tagged suites (the diagnostics harness, and the Block and Kitty
protocol gates) on linux/amd64, and fails when a test skips or when a leg runs
no test at all, so a wrong build tag or a self-skipping test cannot read as
coverage. It is amd64-only because the golden comparisons skip by design
elsewhere. The suites that need a browser (`cdpinterop`, `live`) and the one
that needs `sixel2png` stay local, behind `just diagnostics`, `just live` and
`nix flake check`. `just live` fails when no Chromium resolves rather than
skipping every test green. What CI therefore cannot catch is a change in real
browser behaviour: the in-page walk's stacking, occlusion and anchoring answers
are pinned on CI only as script fragments in
`TestBuildTextWalkScriptContents`, and semantically only by the local live
suite.

## Performance envelope

`NYALA_FPS_TIMING` is the live instrument and the only control for all values
in its one-line quit summary. It separates In, stamped at receive before any
drop, from Out, stamped at draw. Under the pipelined writer, `out_write`
measures the buffer flush to the PTY. It ends when the PTY accepts the bytes,
not when the terminal parses, paints, or presents them. A gap between the In
and Out counts is the latest-wins coalescer working, not loss. Go benchmarks
report the median of five `-benchmem` samples on a pinned host; the controlled
backend sweep reports the median of six independent process p90s. The internal
rendering test drives the production `RenderStreamFrame` path into a counting
writer, so it measures Nyala's byte generation and excludes the terminal.

The palette and output-byte aggregates use the same post-FCP gate as the Out
statistics. They include the frame that opens the gate and exclude all earlier
work. Their rates use the post-FCP output wall time; a zero wall time gives a
zero rate. `out_bytes` counts each complete assembled frame after Nyala adds the
bar and the optional synchronised-output suffix. It therefore includes all
frame-control bytes that the writer receives.

The palette fields cover the adaptive Block `256` and `grey` paths:

| Field | Meaning |
| --- | --- |
| `palette_checks` | Frames that build a palette sample and run the reuse check. |
| `palette_reuse_failures` | Checks that continue into a refit. Each failure starts one refit. |
| `palette_length_equal_refits` | Refits whose active length equals the prior valid palette length. |
| `palette_equal_refits` | Refits whose active length and ordered emitted 8-bit RGB entries equal the prior valid palette. |
| `palette_lut_equal_refits` | Refits whose complete RGB555 LUT equals the prior valid LUT. |
| `palette_noop_refits` | Refits that have equal active length, emitted palette, and complete LUT. |
| `palette_changed_entries` | New active register positions plus active positions whose emitted RGB changed, excluding a removed old tail. |
| `palette_updates` | Refits that emit the complete current palette through OSC 4. |
| `palette_osc_entries` | Total palette entries passed to `AppendOSC4`. |
| `palette_osc_sequences` | Total portable OSC 4 sequences after applying `MaxOSC4Entries`. |
| `palette_osc_bytes` | Total bytes that `AppendOSC4` generates. |
| `palette_full_repaints` | Full repaints caused by an emitted palette update, excluding first-draw and resize clears. |
| `out_bytes` | Total bytes in the complete assembled frames. |
| `palette_updates_per_s` | `palette_updates` divided by the post-FCP output wall time. |
| `palette_osc_bytes_per_s` | `palette_osc_bytes` divided by the post-FCP output wall time. |
| `out_bytes_per_s` | `out_bytes` divided by the post-FCP output wall time. |
| `palette_check_mean_ms`, `palette_check_p90_ms`, `palette_check_max_ms` | Sample construction and reuse-check time per palette check. |
| `palette_fit_mean_ms`, `palette_fit_p90_ms`, `palette_fit_max_ms` | Quantisation, hysteresis, LUT rebuild, and fresh-baseline time per reuse failure. |
| `palette_osc_mean_ms`, `palette_osc_p90_ms`, `palette_osc_max_ms` | `AppendOSC4` time per emitted palette update. |

Duration p90 values use nearest rank, `ceil(0.9*n)-1`, and the summary sorts
samples only at quit. An exact no-op requires a valid palette before the refit,
an equal active length and emitted palette, and an equal complete LUT. The refit
still pays the complete fit cost and keeps its fresh engine baseline, bounds,
palette, and LUT. It emits no OSC 4, does not reset the cell diff, and causes no
palette full repaint.

Twice, a 5-10x stage optimisation moved the frame rate by zero because the
wall sat elsewhere. So the rule is the end-to-end budget, 16.67 ms per
frame at 60 fps, measured on hardware before optimising a stage; every
threshold traces to a measurement. The walls fell in order: the browser
render (26 fps WebGL until `--enable-gpu`), the JPEG decode (33 fps until
the parallel pool), then the terminal itself.

The internal ceilings per frame, from the internal rendering test across
1920x1080, 2560x1600, and 2560x2880: Kitty pass-through 0.008 to 0.073 ms;
Block 1.10 to 3.72 ms p90 with zero allocations. The Sixel encoder
benchmark spans the same three surfaces at 14.55 to 21.65 ms p90 after the
encoder work.

The terminal sets the delivered rate once Nyala's side clears the budget.
Block at 2560x2880 full motion holds 60 fps on kitty, Alacritty, and foot.
WezTerm is byte-bound: truecolour SGR pushes `out_write` to about 20 ms
(about 40 fps), and `--color=256` locks it to 60 at 0.4 ms. Sixel output is
set by the emulator's decode. At 2560x1600, with Nyala's side constant
(decode 13.0 to 13.7 ms, encode 28.3 to 30.3 ms): mlterm 22.9 fps, Alacritty 17.5, foot
8.1, Contour 4.0, WezTerm 1.8; Sixel has no out-of-band escape. WezTerm is
the one exception to the constant Nyala side: its parsing contends for CPU,
inflating Nyala's decode to 29.2 ms and dropping ingest to 38.3 fps. Kitty
direct is parser-bound near 13 fps; shm with JPEG q90 reaches the 60 fps
Chromium cap (59.8 measured at 2560x1600, 45.3 at 2560x2880).

The format economics: at 2560x2880 PNG decodes in about 19 ms against JPEG
q90 at about 9.2 ms, with 2.2x the payload. That ratio is why every
decode-every-frame consumer streams JPEG q90.

## Decision log

- CEF rejected: CDP already exceeds expectations (full reasons under
  Engines).
- Bubble Tea v2 dropped: PTY-ownership complexity; the viewer already owns
  the terminal.
- The DCS colour-depth round-trip dropped: the coloured modes paint every page
  cell, so the terminal's default colours do not show on the page area; the
  bar row is the exception, tracked as WW-38 (details under Colour). The OSC 11 background
  query is a separate decision:
  it rides the startup handshake, and the bar reads its answer (details under
  Capability detection).
- A theming library rejected for the bar: bubbletint and Lipgloss both pin
  colours the user's theme cannot move, and the bar owns three SGR openers
  drawn from the base 16.
- Burning glyphs into the bitmap rejected permanently: it drops text back under
  the 10 px legibility floor.
- Damage tracking, dirty tiles, and partial graphics emission are out of
  scope by owner decision; reopening needs owner sign-off,
  terminal-presented measurements, and a sustained full-frame path over
  16.67 ms.
- Sub-cell-resolution Block capture rejected by owner: a trivially cheap
  input would mask optimisation headroom and hide terminal ceilings, so
  full-resolution capture stays as the discriminating benchmark.
- Grid memoising rejected: pointer identity is not a content proxy and the
  hit rate is near zero.
- Upstream policy: send only accepted wins upstream; if integration needs a
  fork, publish a versioned fork with a module replacement; never vendor or
  patch in Nix only.
- Block screencast quality is fixed at JPEG q90; no runtime quality knob.
- Tabs are out of scope for v1; the engine seam admits child targets later.
- Remote attach (`--remote-debugging-url`) reuses the shared-profile connect
  path through `connectBrowser` and adds no discovery, lock, pid, or retry
  loop; the launch flags it cannot apply are rejected at the CLI by
  `validateLaunchMode` rather than ignored (details under Launch and Chromium
  discovery).
- No home-page or theming configuration; the binary stays stateless.
- The whole tree is cgo-free and cross-builds cleanly; that constraint has
  steered library choices repeatedly (pure-Go glyph matcher, no libsixel at
  runtime, no CEF).

## References

- chromedp issue #920: acknowledging a screencast frame inside the event
  goroutine deadlocks; the reason the engine acks through a separate pump.
- chromedp issue #275: chromedp ships no screencast helper, so Nyala owns the
  raw `Page.startScreencast` event handling.
- vercel-labs/agent-browser #1033: the same device-metrics-versus-surface
  sizing bug fixed in the wild; justifies Nyala's viewport sizing rule.
- Chromium `page_handler.cc`: the `frames_in_flight_` counter is the
  backpressure that makes timely acks mandatory.
- no-color.org: any present `NO_COLOR` value disables colour, including
  empty; the viewer checks presence directly rather than parsing.
- Unicode 13 Symbols for Legacy Computing: the source of the sextant glyphs
  the Block backend's densest scheme draws with.
- zellij issue #5125: the Sixel image-id collision that blanks every third
  frame; why Sixel is never offered under zellij.
- kitty issue #7005: the ~270 MB/s escape-parser measurement that caps
  in-band graphics and motivates the shm fast path.
- Chromium `devtools_video_consumer.cc`: the 10 ms minimum capture period
  behind the 100 fps screencast rate cap.
- DEC STD 070: the Sixel repeat-count grammar (32767 permitted) behind the
  owned encoder's 8192 ceiling.
