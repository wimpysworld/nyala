# Terminal Browser - Project Overview & Tech Stack

## Summary

A modern, text-based web browser that renders real web pages to a TTY. Spiritual
successor to browsh, built in Go, driving headless Chromium via the Chrome
DevTools Protocol (CDP). Targets a fully terminal-native experience, including on
a bare Linux console via KMSCON.

Stage 0, Stage 1, Stage 2, and Stage 3 are built. Three swappable rendering backends sit behind one
`Renderer` seam (quadrant or half-block, Sixel, Kitty), with a measurement harness
and automatic capability plus colour detection that nominate the best backend and
colour depth per terminal. Stage 2 adds the crisp text overlay on the block path, and
Stage 3 makes the binary web-first: `nyala <url>` launches headless Chromium, streams the
screencast, and renders the live page. See
[`STAGE-0.md`](./STAGE-0.md), [`STAGE-1.md`](./STAGE-1.md),
[`STAGE-2.md`](./STAGE-2.md), and [`STAGE-3.md`](./STAGE-3.md). Stage 4 is the remaining design goal; the rest
of this document is the target architecture and the research behind the unbuilt stage.

## Goals

- Render modern web pages (HTML5, CSS, JS) to a terminal using truecolour
- Run in a real TTY with no display server present (headless Chromium needs none)
- Crisp text via a composited glyph layer, not just fuzzy half-block pixels
- Initial render objective without Sixel/Kitty: match the Carbonyl gold standard,
  quadrant blocks for the bitmap with real Unicode text composited on top, truecolour
  where present
- Full keyboard and mouse interaction
- Single small Go binary; depends on a system-installed Chromium/Chrome
- Pluggable rendering engine behind a stable interface (see Architecture)

## Non-Goals (initial)

- Bundling the browser engine (nice-to-have, not required)
- Browser-extension support
- Feature parity with a desktop browser

One initial non-goal was dropped. Bitmap graphics protocols were first ruled out
because the primary target, KMSCON, speaks neither Sixel nor Kitty. Stage 0 instead
built Sixel and Kitty as emulator-only enhancements above the mandatory block floor,
and Stage 1 detects and nominates them per terminal. KMSCON still lacks both, so the
quadrant or half-block backend stays the permanent floor.

## Target Environment

- Primary: KMSCON (active fork `kmscon/kmscon`) in a Linux TTY. As of libtsm
  late-2025 it supports 24-bit truecolour, scalable Unicode fonts, and mouse.
- Also: any truecolour-capable terminal emulator (xterm, foot, kitty, wezterm).
- Truecolour detection uses env plus terminfo via `colorprofile`. A present
  `COLORTERM` is a trusted positive signal; its absence is never read as "no
  truecolour". No DCS query/response is used. KMSCON now sets `COLORTERM=kmscon`
  or `truecolor`, and the DCS round-trip is a no-op on KMSCON's libtsm. See
  `docs/STAGE-1.md` for the evidence.

## Architecture (key decision)

Decouple the browser engine from everything else via an `Engine` seam. The bulk of
the codebase (rendering, input, TUI, config) is engine-agnostic and reusable.

```go
// engine/engine.go
type Engine interface {
    Navigate(url string) error
    Resize(cols, rows int)            // logical terminal cell size
    Frames() <-chan Frame             // RGBA bitmap + synced Text
    SendKey(k Key)
    SendMouse(m MouseEvent)
    Close() error
}
```

Stage 3 resolves the text layer to one synced channel. The engine places the text by
the snapshot's own scroll offset and folds it onto `Frame.Text`, so consumers receive
bitmap and crisp text on one channel, never two cadences to reconcile. The text is
sticky state carried across frames, so a static page still composites its overlay. A
separate text channel may return in Stage 4 for link-hint geometry. See
`docs/STAGE-3.md`.

- **Engine #1: `ChromiumEngine` (chromedp).** Frames via `Page.startScreencast`;
  text layer via CDP DOM queries, placed by the snapshot's scroll and folded into
  `Frame.Text`. This is the only engine to build now.
- **Future engines** (e.g. WPE WebKit, CEF) implement the same interface. No other code
  changes. Do not build these yet; the seam keeps the option open.
- **CEF (gated future engine).** CEF embeds Chromium in-process and hands back BGRA
  frames from `OnPaint`, with no CDP screencast hop. It is a candidate fallback if the
  Stage 3 FPS spike shows the CDP screencast cannot hold an acceptable frame rate at
  LoDPI on the target terminals. It is not the default: it bakes Chromium into the
  binary (a ~670MB build dependency), needs C/C++ bindings against the pure-Go cgo-free
  stack, and gives no text geometry, so the crisp-text problem stays. See the Brow6el
  internals section under "Half-Block Rendering Research" for the worked reference.

### Rendering pipeline (engine-agnostic)

1. Receive bitmap frames + a positioned text layer from the active `Engine`.
2. Downsample bitmap to terminal resolution; render block cells, quadrant (2x2) by
   default with half-block (`▄`) as the floor (the half-block cell sets background =
   top pixel, foreground = bottom pixel, two square pixels per cell). On graphics
   terminals the Sixel or Kitty backend renders the same bitmap instead.
3. Composite the text layer over the bitmap so glyphs render as real Unicode where
   text exists, bitmap elsewhere. This is the primary quality differentiator.
4. Diff against the previous frame; emit only changed cells.

### Known constraints / risks

- **Frame rate**: `startScreencast` streams JPEG/PNG over the CDP socket. Benchmarks
  cite low FPS, but those are full-resolution; at terminal resolution the bitmap is
  tiny, so cost should be far lower. Measure early against KMSCON before optimising.
  This is the explicit CDP-vs-CEF gate: if the Stage 3 spike shows CDP holds sustained
  full-viewport FPS at LoDPI on the target terminals, ship CDP; if it fails, open a CEF
  engine spike behind the existing `Engine` seam.
- **Crisp text is the make-or-break feature.** Prototype the text-layer compositing
  first; it separates "readable" from "browsh-grade fuzzy".
- **Dependency**: a compatible Chromium/Chrome must be installed (via chromedp's
  `ExecAllocator`, or `RemoteAllocator` to attach to a running instance). Accepted.

### chromedp Screencast Support

Screencast works on chromedp, but chromedp ships no helper for it. The protocol
commands and the frame event are fully implemented in `cdproto/page`; the
high-level `chromedp` package just never wrapped them. The `ChromiumEngine`
implements the listen-decode-ack loop itself. Budget ~20 lines, not a feature.

#### What's actually there

These are real, callable, generated symbols in
[`github.com/chromedp/cdproto/page`](https://pkg.go.dev/github.com/chromedp/cdproto/page),
each with a working `Do(ctx)` executor - not stubs:

- `page.StartScreencast()` → `Page.startScreencast`, with `WithFormat`,
  `WithQuality`, `WithMaxWidth`, `WithMaxHeight`, `WithEveryNthFrame`
- `page.StopScreencast()` → `Page.stopScreencast`
- `page.ScreencastFrameAck(sessionID int64)` → `Page.screencastFrameAck`
- `page.EventScreencastFrame` - the frame event (image data + `SessionID`)
- `page.ScreencastFrameMetadata`, `page.ScreencastFormat`

#### What's missing

There is no `chromedp.Screencast()` convenience action (unlike
`chromedp.CaptureScreenshot()`), and chromedp's event loop does **not** consume
or ack frames for you. The maintainers declined to build a packaged feature
(see [chromedp/chromedp#275](https://github.com/chromedp/chromedp/issues/275)).
This is an ergonomics gap, not an implementation gap.

#### The gotcha that makes it look broken

⚠️ Chrome sends frames only as fast as you **acknowledge** them. If you don't
call `ScreencastFrameAck` for each frame, the stream stalls after one or two
frames. The frames aren't lost - they're withheld pending an ack that chromedp
never sends on its own. This is almost certainly why some reports claim the
events are "discarded/not handled".

#### Minimal working pattern

```go
// 1. Start streaming.
if err := chromedp.Run(ctx, page.StartScreencast().
    WithFormat(page.ScreencastFormatJpeg).
    WithMaxWidth(800).
    WithMaxHeight(600).
    WithEveryNthFrame(1),
); err != nil {
    return err
}

// 2. Pump frames yourself - chromedp won't.
chromedp.ListenTarget(ctx, func(ev interface{}) {
    if f, ok := ev.(*page.EventScreencastFrame); ok {
        // f carries the JPEG/PNG frame data and f.SessionID.
        // Hand the image to the renderer here.

        // 3. MUST ack every frame or the stream stalls.
        go func() {
            _ = chromedp.Run(ctx, page.ScreencastFrameAck(f.SessionID))
        }()
    }
})
```

## Tech Stack

| Concern | Choice | Notes |
|---|---|---|
| Language | Go | |
| Browser engine | Headless Chromium | System-installed; no display server needed |
| Engine driver | `chromedp` | De-facto CDP client, pure Go, no external deps |
| TUI runtime + chrome | Bubble Tea v2 + Bubbles v2 + Lipgloss v2 (`charm.land/*`) | Event loop, address bar, status line, layout; GA 2026-02-24. Do NOT route the bitmap viewport through its cell renderer |
| Animation (optional) | Harmonica | Smooth chrome scroll/load polish; defer until chrome exists |
| Frame decode | `image/jpeg`, `image/png` (stdlib) | screencast frames |
| Resize/composite | `golang.org/x/image/draw` | In-place scale into a reused RGBA buffer |
| Half-block fallback | Custom compositor (port Carbonyl quad maths; reference `charmbracelet/x/mosaic`) | The permanent floor for KMSCON. Combined cell grid (bitmap + optional grapheme); quadrant (2x2) default, half-block (`U+2584`) floor; text overwrites cells. `rivo/uniseg` + `mattn/go-runewidth` for glyph width |
| Sixel encode | `charmbracelet/x/ansi/sixel` | Fastest Go encoder; same stack as Bubble Tea v2 |
| Kitty graphics | `charmbracelet/x/ansi/kitty` | Full protocol; unicode-placeholder support |
| Image quantisation | `ericpauley/go-quantize` + `makeworld/dither` (paletted Sixel only) | Cache palettes across frames; ordered/Bayer dither for speed |
| Colour degradation | Direct truecolour SGR (default); hardcoded RGB→xterm-256 formula (Carbonyl) fallback | Truecolour passthrough where present; closed-form per pixel (6x6x6 cube + 24-step grey ramp) otherwise. Stage 1 detects colour from env plus terminfo via `colorprofile.Detect`; a present `COLORTERM` (`kmscon`, `truecolor`, `24bit`) is a trusted positive that short-circuits to truecolour; no DCS round-trip. See `docs/STAGE-1.md` |
| Capability detect | DA1-fenced startup handshake on the tty (Kitty `a=q`, Sixel via DA1, cell pixel size via `CSI 16t`/`14t`/`18t`), failing closed to block in 200 ms | Built in Stage 1, with the colour detector above; never trust `$TERM`. See `docs/STAGE-1.md` |
| Config | `koanf` v2 + TOML | Pure Go, no cgo; layered env, file, and flag sources merged by load order; opt-in parsers keep the binary small. Chosen over viper (heavier dep set, larger binary) and stdlib (hand-rolled merge) |

## Architecture Decisions

Resolved in the architecture phase. Each decision sits behind the `Engine` seam and
is reversible.

- **Frame model: push, with Nyala-side damage.** CDP `Page.screencastFrame` events
  are emitted by the engine on compositor damage, so the model is inherently push;
  Nyala acks each frame with `Page.screencastFrameAck(sessionID)` or the stream
  stalls. CDP sends whole frames with no dirty-rect metadata, so damage is computed
  Nyala-side by diffing the decoded bitmap against the previous frame at
  terminal-cell granularity. Evidence: Chromium `page_handler.cc` emits one
  `ScreencastFrame` per encoded frame with no rect field, gated by `frames_in_flight_`.
- **Text-layer extraction: `DOMSnapshot.captureSnapshot`.** A single `captureSnapshot`
  call returns the flattened node tree, the post-layout inline text boxes (`textBoxes`,
  absolute bounds plus character ranges), and a whitelist of computed styles (request
  `color`) in one round-trip, so geometry and colour arrive together. The accessibility
  tree exposes no pixel boxes and drops decorative or non-semantic text; manual DOM
  traversal needs one `DOM.getBoxModel` per node, an O(n) round-trip cost that does not
  hold up per frame. Capture off the first paint (`firstContentfulPaint`), then
  re-snapshot on navigation, resize, and a debounced `networkAlmostIdle` reflow,
  decoupled from the screencast frame, not on a timer, so cost tracks real visual
  change. Caveats: snapshot bounds are device
  pixels (divide by the device pixel ratio), ignore scroll offset (add `scrollOffsetX/Y`
  back), and the layout box layout is explicitly unstable between versions, so treat
  exact boxes as advisory. Source: CDP DOMSnapshot domain spec and
  `github.com/chromedp/cdproto/domsnapshot`. No prior art validates this path: Carbonyl
  forks Chromium and browsh uses a Firefox WebExtension, so both bypass CDP; the closest
  CDP precedent is browser-use's `captureSnapshot` usage.
- **Device-scale-factor: DSF 1 plus a small viewport.** Set
  `Emulation.setDeviceMetricsOverride(width, height, deviceScaleFactor=1, mobile=false)`
  and cap the stream with `Page.startScreencast` `maxWidth`/`maxHeight` set to the target
  pixel grid. Carbonyl's 1/7 ratio is a compiled-in Blink `GetForcedDeviceScaleFactor`
  hack and is not reachable over CDP; DSF 1 with a small viewport is the clean equivalent
  and keeps captured pixels equal to CSS pixels. Note: `setDeviceMetricsOverride` resizes
  only the CSS viewport; the screencast captures the content area, so also set launch
  `--window-size`, and on runtime resize restart the screencast and resize the content
  area, else frames arrive at the default 1280x720.
- **Input mapping: translate Bubble Tea v2 messages to CDP `Input` events.** The TUI
  decodes the wire protocols (Kitty keyboard, SGR 1006 mouse, bracketed paste) into typed
  messages; a single translator maps `tea.KeyPressMsg`/`KeyReleaseMsg` to
  `Input.dispatchKeyEvent` (`keyDown`/`rawKeyDown` then `char` for printable keys, `keyUp`
  on release), folding `Key.Mod` into the CDP modifier bits (Alt=1, Ctrl=2, Meta=4,
  Shift=8) and setting `text`/`code`/`windowsVirtualKeyCode` from `Key.Text`/`Code`/`BaseCode`.
  Mouse messages map to `Input.dispatchMouseEvent` (`mousePressed`/`mouseReleased`/
  `mouseWheel`/`mouseMoved`) with the same modifier bits, a pressed-buttons mask (Left=1,
  Right=2, Middle=4, Back=8, Forward=16), and wheel deltas in CSS pixels. Cell coordinates
  convert to CSS pixels via the Stage 0 capture geometry: `x = (cellX + 0.5) * cellWidthPx`,
  `y = (cellY + 0.5) * cellHeightPx` at the LoDPI viewport. Pasted and IME text routes
  through `Input.insertText` so multi-byte input inserts atomically. Evidence: CDP Input
  domain and `chromedp/cdproto/input`; Bubble Tea v2 `key.go`/`mouse.go`.
- **v1 interaction scope (recommendation, owner may override):** ship scrolling,
  back/forward navigation history, and Vimium-style link hinting; defer tabs to post-v1.
  Scrolling and history are baseline reading and browsing primitives; link hinting is core
  to keyboard-only link following and should enumerate real anchor geometry from the text
  layer, avoiding Carbonyl's cell-buffer heuristic, which it used for lack of DOM access.
  Tabs are deferred to keep v1 small; the single-page `Engine` model adds them later via a
  child `NewContext` bound with `WithTargetID`, so the deferral is reversible. Evidence:
  Carbonyl PR #219 (vimium-style scroll, history, `f` link hints); browsh keybindings.
- **Process lifecycle: launch by default, attach optional, single page for v1.** Launch
  via chromedp `NewExecAllocator` (managed headless Chromium; chromedp owns the process and
  force-kills it on exit), with attach via `NewRemoteAllocator` (`ws://host:port`) as an
  opt-in flag for locked-down or shared hosts. Launch is the default because the
  browser-side flags Nyala needs (low device scale, window size) only apply to a process it
  starts. Crash recovery: listen for `Target.targetCrashed` and lost-connection context
  cancellation, then rebuild the allocator and context and re-navigate to the URL held in
  `Engine` state (hold current URL and scroll offset in the `Engine`, not in chromedp). v1
  drives a single page/target; the seam keeps the page behind an interface so tabs add later
  with no allocator change. Evidence: chromedp `ExecAllocator`/`RemoteAllocator` and CDP
  Target domain; chromedp issues #1457 and #1007.

## TUI Library Research

This section records the library evaluation behind the Tech Stack choices. Each
dimension scores its tools 1-10 for Nyala specifically. Scores weigh maturity,
maintenance, and fit for a TTY browser that blits a per-frame bitmap viewport and
composites crisp Unicode text over it. Dates verified mid-June 2026.

The one hard problem runs through every dimension: a full-screen TUI framework owns
the cell grid and will overwrite raw graphics escape sequences it did not emit. No
Go framework solves "full-screen TUI plus per-frame Sixel/Kitty graphics" natively.
Graphics must be written out-of-band into a reserved region the cell renderer leaves
blank. KMSCON speaks neither Sixel nor Kitty, so a half-block renderer must always
exist as the floor.

### Bubble Tea v2 core (TUI runtime / event loop)

| Tool | Score | Maturity | Maintenance | Suitability for Nyala |
|---|---|---|---|---|
| Bubble Tea v2 | 7/10 | v2.0.0 GA 2026-02-24; v2.0.7 2026-06-01; API stable | Very active; ~43.2k stars; Charm-backed; 18k+ deps | Strong for chrome and event loop; weak for the bitmap viewport (cell-only renderer, frame-capped at a default 60 FPS and a 120 FPS hard cap, clobbers inline graphics) |

- Pros: GA and stable; declarative `tea.View` struct; ncurses-style diff renderer with synchronized output (mode 2026); strong input story (Kitty keyboard, split key/mouse interfaces, bracketed paste, OSC52); built-in colour downsampling; real escape hatches (`WithFPS`, `WithoutRenderer`, `WithOutput`, `ReleaseTerminal`/`RestoreTerminal`); huge battle-tested ecosystem.
- Cons: no native graphics (ultraviolet `Cell` holds only grapheme + style + width); renderer overwrites inline graphics it did not emit (issue #163 open since 2021); frame-capped; not a video pipeline; v1 to v2 migration is breaking; depends on ultraviolet (still 0.x, ~357 stars); no built-in region-locking primitive.
- Sources: `github.com/charmbracelet/bubbletea/releases/tag/v2.0.0`, `UPGRADE_GUIDE_V2.md`, `pkg.go.dev/charm.land/bubbletea/v2`, `github.com/charmbracelet/bubbletea/issues/163`, `github.com/charmbracelet/ultraviolet` (`cell.go`).

Verdict: adopt for the event loop and chrome UI, but do NOT route the per-frame
bitmap through its managed renderer. Drive graphics via `WithoutRenderer` and
compose by hand, or reserve a renderer-blanked region and write graphics into it via
the program output writer.

### Lipgloss v2 (styling/layout) + Bubbles v2 (components)

| Tool | Score | Maturity | Maintenance | Suitability for Nyala |
|---|---|---|---|---|
| Lipgloss v2 | 9/10 | v2.0.0 GA 2026-02-24; v2.0.3 2026-04-13 | Very active; ~11k stars; Charm-backed | Right layout and text-overlay engine: borders, joins, Place, and a Canvas/Layer/Compositor for z-ordered glyph overlays. Cannot blit bitmaps |
| Bubbles v2 | 8/10 | v2.0.0 GA 2026-02-24; v2.1.0 2026-03-25 | Very active; ~8-9k stars; used in Crush | Supplies chrome widgets: `textinput` (address bar), `viewport` (page rect), `help`+`key` (status line), `spinner` (load). No button, no graphics-aware widget |

- Lipgloss pros: truecolour first-class (`Color()` returns `color.Color`); full layout primitives; Canvas/Layer/Compositor for exact `(X,Y,Z)` placement; pure value-type styles (no global renderer); shares the ultraviolet cell model with Bubble Tea v2.
- Lipgloss cons: Canvas composites cells not raster images; v2 is breaking (Color is a function, Renderer removed, AdaptiveColor moved); light/dark detection now manual; solid-colour cells are the only Lipgloss route to half-block.
- Bubbles pros: covers every chrome need out of the box; real terminal cursor in v2; consistent getter/setter API; viewport adds soft-wrap, gutter, regex highlights; production-proven; released in lockstep with Bubble Tea v2.
- Bubbles cons: no widget reserves or emits bitmap graphics; no button widget (build from styled text plus `key.Binding` hit-testing); v2 breaking changes across nearly every component; viewport assumes text content; light/dark needs the manual handshake.
- Sources: `lipgloss/releases/tag/v2.0.0`, `lipgloss/releases/tag/v2.0.3`, `lipgloss/.../canvas.go`, `bubbles/releases/tag/v2.0.0`, `pkg.go.dev/charm.land/bubbles/v2`, `bubbles/pull/996`.

Verdict: adopt both for the chrome. Neither composites bitmaps; the per-frame
Sixel/Kitty blit is out-of-band into a reserved page rectangle.

### Harmonica (animation)

| Tool | Score | Maturity | Maintenance | Suitability for Nyala |
|---|---|---|---|---|
| Harmonica | 5/10 | v0.2.0, 2022-04-15; tiny stable API; no v2 | Maintenance mode; ~1.48k stars; last push 2025-06-11 | Narrow nice-to-have. Eases chrome scroll and animates a loading indicator. Zero contribution to bitmap, graphics, or text compositing |

- Pros: tiny, dependency-free, MIT; framework-agnostic; correct damped-spring physics; cheap per-frame; maintenance-mode acceptable because the maths is fixed.
- Cons: pure nice-to-have; stale release cadence (last tag April 2022); renders nothing and has no scheduler; applicability limited to chrome animation; animating the continuously-repainting viewport is pointless.
- Sources: `github.com/charmbracelet/harmonica`, `harmonica/releases`, `pkg.go.dev/github.com/charmbracelet/harmonica`, `ryanjuckett.com/damped-springs`.

Verdict: optional polish only. Adopt after chrome and scrolling exist, or vendor a
dozen lines of spring maths. Do not let it shape the architecture.

### bubbletint (theming)

[`lrstanley/bubbletint`](https://github.com/lrstanley/bubbletint) is the intended
theming layer for the chrome. It ships a large registry of named terminal colour
schemes and maps each to Lipgloss colours, so the address bar, status line, and
borders can switch theme at runtime from one tint.

Use it for chrome theming only. It colours styled cells; it does not touch the
bitmap viewport or the Sixel/Kitty blit. Adopt it once the Bubble Tea chrome
exists (Stage 4), not before. Captured here so the choice is not lost.

Future idea: drive the page's dark/light mode from the active tint. The web
feature is the CSS `prefers-color-scheme` media query. Chromium emulates it over
CDP with `Emulation.setEmulatedMedia` (`features: [{name: "prefers-color-scheme",
value: "dark"|"light"}]`), which chromedp can call. Classify the active bubbletint
theme as dark or light, then set the emulated media to match, so sites that honour
`prefers-color-scheme` render in the same mode as the terminal chrome. Needs the
browser layer (Stage 3) and the tint (Stage 4) first.

### Sixel in Go

| Tool | Score | Maturity | Maintenance | Suitability for Nyala |
|---|---|---|---|---|
| `charmbracelet/x/ansi/sixel` | 8/10 | Merged 2026-02-21; in x/ansi v0.11.7; experimental module | Active; Charm-backed; charmbracelet/x ~300 stars | Best fit: ~5x faster encode than mattn, native to the v2 stack, shares the Kitty code. Higher allocations; pool buffers |
| `mattn/go-sixel` | 7/10 | Mature (2014); v0.0.9 2026-04-13 | Active; single maintainer (mattn); 174 stars | Dependency-light fallback and reference; ships `gosvideo` (ffmpeg Sixel video). Slower; per-frame median-cut is the bottleneck |
| `BourgeoisBear/rasterm` | 7/10 | v1.1.2, 2025-12-21 | Maintained; 108 stars; multi-contributor | One library for Sixel + Kitty + iTerm2 plus capability detection. Sixel needs pre-quantised `*image.Paletted` (suits a streaming pipeline) |
| `blacktop/go-termimg` | 6/10 | Pre-1.0; "needs love"; created 2024-09 | Single author; 59 stars | All three protocols plus half-block, auto-detect, Bubble Tea widget. Sixel ~90ms/render is too slow; sub-view clipping incomplete |
| `zellij-org/sixel-image` | 4/10 | Production in Zellij (Rust) | Maintained within Zellij | Reference only (Rust). Proven design for Sixel-inside-TUI: decode to ARGB, track occupied cells, redraw on resize |

- Sources: `charmbracelet/x/pull/352`, `/pull/380`, `mattn/go-sixel/issues/19`, `BourgeoisBear/rasterm`, `blacktop/go-termimg`, `zellij-org/zellij/issues/371`, `arewesixelyet.com`.
- KMSCON exclusion confirmed: KMSCON does NOT support Sixel; the maintainer said (March 2026) that adding it needs a near-total libtsm rewrite. Half-block only there.

Verdict: pick `charmbracelet/x/ansi/sixel`. Wrap it with a reused buffer and a stable
palette across frames. Write Sixel out-of-band at an absolute cursor position, never
through the Lipgloss/Bubble Tea View string. Gate behind capability detection.

### Kitty graphics protocol in Go

| Tool | Score | Maturity | Maintenance | Suitability for Nyala |
|---|---|---|---|---|
| `charmbracelet/x/ansi` + `ansi/kitty` | 9/10 | x/ansi v0.11.7; experimental module; cut through 2026-02 | Active; Charm-backed | Best by a wide margin: full protocol, every Options key, image Encoder (RGBA/RGB/PNG + zlib), unicode-placeholder primitives. Already in the v2 stack |
| `charmbracelet/x/mosaic` | 7/10 | Pseudo-version v0.0.0-20260607; Go 1.25 | Active (2026-06-07); Charm | In-stack half/quarter/complex block fallback with truecolour; the KMSCON path. Per-frame CPU scales with cell count |
| `BourgeoisBear/rasterm` | 6/10 | v1.1.2, 2025-12-21 | Maintained; 108 stars | Real semver, Kitty + iTerm2 + Sixel encode plus DA1/DA2 tables. No unicode-placeholder, no placement/delete control; outside the v2 stack |
| kitty Go tools (`kovidgoyal/kitty`) | 4/10 | Tracks the kitty release line | Very active (Kovid Goyal) | Authoritative spec reference and cross-check only. Not a consumable module; would drag in the whole kitty repo |

- Sources: `pkg.go.dev/github.com/charmbracelet/x/ansi`, `x/main/ansi/kitty/graphics.go`, `/options.go`, `/encoder.go`, `pkg.go.dev/.../mosaic`, `BourgeoisBear/rasterm`, `sw.kovidgoyal.net/kitty/graphics-protocol`.
- Precedent: `awrit` (Chromium into a Kitty-graphics terminal, ~1.3k stars) proves per-frame browser blitting over this protocol.

Verdict: use `charmbracelet/x/ansi/kitty`, paired with `x/mosaic` for the half-block
fallback. Build a thin Nyala layer that owns capability detection, a fixed image ID
with transmit-and-display replacement, explicit delete-by-ID, shared-memory transmit
when local, and the unicode-placeholder path so images sit in tracked cells.

### tcell and TUI foundation alternatives

| Tool | Score | Maturity | Maintenance | Suitability for Nyala |
|---|---|---|---|---|
| vaxis (Go) | 8/10 | Pre-1.0; v0.16.0; created 2023-07 | Active; one primary author; sr.ht-hosted | Strongest native fit: Sixel, Kitty and half-block built in, query-based detection, render loop profiled for speed. Single-maintainer bus factor; diverges from Charm |
| tcell | 7/10 | v3.1.2 2026-01-26; v2 still maintained | Healthy; 5.1k stars; 110 contributors | Clean cell buffer, truecolour, mouse, pure Go, runs on KMSCON. No native graphics; diff renderer clobbers out-of-band escapes |
| `charmbracelet/x` cellbuf + ansi | 6/10 | Experimental 0.0.x | Very active; Charm | The Bubble Tea v2 substrate. x/ansi ships Sixel/Kitty/iTerm2 encoders. Used standalone, same cell-diff limits as tcell |
| Bubble Tea v2 (for contrast) | 5/10 | v2 GA; 42k stars | Very active; Charm flagship | Good for chrome only. Maintainers decline official graphics support; renderer and Lipgloss fight raw escapes |
| tview | 4/10 | v0.42.0 2025-08-27; ~8.5 yrs | Active; 13.7k stars | Poor fit for the engine. Widget toolkit, not a per-frame compositor; Image widget is block/braille only |

- Sources: `git.sr.ht/~rockorager/vaxis`, `github.com/rockorager/vaxis`, `github.com/gdamore/tcell`, `charmbracelet/x/cellbuf`, `bubbletea/issues/163`, `charmbracelet/crush/.../image.go`, `github.com/rivo/tview`.

Verdict: vaxis is the best single fit for the hard problem if you accept its bus
factor; tcell is the safer institutional bet at the cost of engineering graphics
coexistence yourself. Bubble Tea v2 stays the choice for chrome. Charm maintainers
state on record (issue #163) that per-frame images are ill-suited to TUIs and there
are no plans for official graphics support, so graphics are always a bypass.

### Image rendering and quantisation

| Tool | Score | Maturity | Maintenance | Suitability for Nyala |
|---|---|---|---|---|
| `golang.org/x/image/draw` | 9/10 | v0.41.0 2026-05-21; stable | Go team; unlimited bus factor | The resizer. In-place draw into a reused RGBA buffer; multiple quality scalers; also composites |
| `BourgeoisBear/rasterm` | 8/10 | v1.1.2 2025-12-21 | Maintained; 108 stars | Clean Kitty + iTerm2 + Sixel encode-to-bytes; caller owns I/O; terminal-detection tables. No half-block |
| `mattn/go-sixel` | 8/10 | Continuous; Go 1.24+ | Very active in 2026 (real-time video work) | Best dedicated Sixel encoder for motion: RGBA fast path, tunable colours/dither. Sixel only; no tags |
| `muesli/termenv` | 8/10 | v0.16.0 2025-02-21 | Healthy; ~2k stars | Capability detection and chrome styling. WARNING: per-colour search is too slow per pixel; do not quantise with it |
| `makeworld/dither` v2 | 8/10 | v2.4.0 2023-12-20 | Stable; 450 stars | Best-quality paletted dithering (linearised, luminance-weighted). Use ordered/Bayer for speed; paletted path only |
| `ericpauley/go-quantize` | 7/10 | Settled API; created 2016 | Quiet; 25 stars; algorithm fixed | Fastest Go median-cut palette (~17-23x faster). Cache palettes across frames; paletted Sixel only |
| `charmbracelet/x/mosaic` | 6/10 | Pre-1.0; landed 2025-03 | Maintained; feature-static | Charm-stack half-block, but returns a finished string not a composable cell grid. Reference, expect to replace |
| `blacktop/go-termimg` | 6/10 | Pre-1.0; no tags; Go 1.26 | Active; single maintainer; 59 stars | Closest all-in-one (4 protocols, auto-detect, Bubble Tea widget). Hides the per-frame control Nyala needs |
| `disintegration/imaging` | 6/10 | v1.6.2 2019; stable | Unmaintained since 2019 | Convenient resize/adjust helpers, but allocates per call. Keep for favicons/thumbnails, not the frame loop |
| `fathyb/carbonyl` (ref) | 7/10 | v0.0.3 2023; Rust | Dormant (last push 2024-07) | The canonical reference for half-block + text-overlay. LoDPI native-resolution trick; closed-form RGB→xterm-256 |
| `Zebbeni/ansipx` | 5/10 | Created 2026-04; pre-1.0 | Single author; 6 stars | Reference for the half-block recipe (resize + dither + colour). Too new; depends on archived nfnt/resize |
| `qeesung/image2ascii` | 3/10 | Stable, feature-frozen | Dormant; 978 stars | Poor fit. ASCII art is lower fidelity than half-blocks; wrong model for a browser |
| `nfnt/resize` | 3/10 | ARCHIVED 2022-04 | Abandoned | Avoid. Superseded by `golang.org/x/image/draw` |

- Sources: `pkg.go.dev/golang.org/x/image/draw`, `BourgeoisBear/rasterm`, `mattn/go-sixel`, `muesli/termenv`, `makeworld/dither`, `ericpauley/go-quantize`, `charmbracelet/x/.../mosaic`, `blacktop/go-termimg`, `disintegration/imaging`, `fathyb/carbonyl` + `fathy.fr/carbonyl`, `Zebbeni/ansipx`, `qeesung/image2ascii`, `nfnt/resize`.

Verdict: do not adopt one all-in-one library. Write Nyala's own thin half-block
compositor that emits a cell grid (`U+2584`, bg=top pixel, fg=bottom pixel) so glyphs
overwrite cells. Use `x/image/draw` for in-place resize, `rasterm`/`go-sixel` for the
graphics encoders, `dither`/`go-quantize` on the paletted path only, and the Carbonyl
closed-form RGB→xterm-256 formula per pixel. The biggest win is Carbonyl's LoDPI
trick: render Chromium at terminal resolution so per-frame downsampling is cheap.

### Capability detection and coexistence

| Tool | Score | Maturity | Maintenance | Suitability for Nyala |
|---|---|---|---|---|
| Bubble Tea v2 | 9/10 | v2.0.0 GA 2026-02-24 | Very active; Charm | The runtime-probe layer: `tea.Raw` plus DA1/DECRPM/XTVERSION/XTGETTCAP query commands and colorprofile truecolour. godoc shows Sixel detection |
| `colorprofile` | 8/10 | Shipped in Bubble Tea v2 | Active; Charm; ~110 stars | Right tool for the truecolour half. DCS-backed when fed XTGETTCAP responses. No graphics detection |
| `charmbracelet/x` term + ansi | 8/10 | x/term v0.2.2; ansi active | Very active; Charm | The construction kit: raw-mode `/dev/tty` control plus correct DA1/DECRQM/Kitty/Sixel builders |
| Lipgloss v2 Canvas/Layer | 8/10 | v2.0.0 GA; v2.0.4 | Very active; Charm | The cell compositor TUIOS uses: z-ordered chrome + text + half-block layers. Leave the graphics region blank |
| TUIOS (prior art) | 8/10 | ~2.5k stars; Bubble Tea v2 | Active; single primary maintainer | The blueprint. `kitty_passthrough.go` shows save-cursor + CUP + APC + restore wrapped in synchronized-output; `capabilities.go` shows the layered probe |
| `blacktop/go-termimg` | 7/10 | v0.1.26; Go 1.26 | Active; single maintainer; 59 stars | Best ready-made detector to copy/vendor: env short-circuits, parallel probes with timeouts, Kitty `a=q`, Sixel XTSMGRAPHICS, CSI 14t/16t cell size |
| `charmbracelet/x/mosaic` | 6/10 | Pre-1.0 | Active; Charm | The KMSCON-floor renderer. Pure-cell half-block, composes into a Canvas layer. Quarter-resolution vs real pixels |
| `termenv` | 4/10 | v0.16.0 2025-02-21 | Maintenance mode | Colour profile and theme only. NO graphics detection; env-based truecolour. Superseded by colorprofile |
| Harmonica | 6/10 | v0.2.0 2022 | Stable; low activity | Tangential. Smooth scroll maths; no terminal I/O, so no conflict with graphics |

- Sources: `bubbletea/releases/tag/v2.0.0`, `bubbletea/blob/main/raw.go`, `/termcap.go`, `colorprofile/.../env.go`, `pkg.go.dev/github.com/charmbracelet/x/term`, `lipgloss/releases/tag/v2.0.0`, `Gaurav-Gosain/tuios/.../kitty_passthrough.go`, `/capabilities.go`, `blacktop/go-termimg/.../detect.go`.

Verdict: build a layered runtime probe, not termenv. Env short-circuits first
(`KITTY_WINDOW_ID`, `TERM_PROGRAM`, `GHOSTTY_RESOURCES_DIR`, `COLORTERM`), then APC
`a=q` Kitty and DA1 Sixel probes on `/dev/tty` with a sentinel and a ~100-300ms
timeout, plus CSI 14t/16t for pixel cell size. Coexistence is solved in production:
let the cell renderer own chrome, text, and a reserved blank viewport rectangle;
emit Sixel/Kitty frames as save-cursor + CUP + APC/DCS + restore writes via `tea.Raw`,
wrapped in synchronized output (mode 2026). TUIOS does exactly this on the same stack.

### Recommended Stack

The Charmbracelet v2 stack IS the right core for the chrome and event loop. It is GA,
stable, and the best-supported Go TUI runtime. It is NOT the bitmap pipeline: route
the per-frame viewport around its renderer.

Coexistence verdict: feasible without leaving Bubble Tea v2, but only via a positioned
graphics side-channel, never a native widget. Let the cursed renderer own the cells
(chrome, text layer, a reserved blank viewport rectangle), and write Sixel/Kitty
frames out-of-band with save-cursor + CUP + APC/DCS + restore, wrapped in synchronized
output. TUIOS proves this in production on the same stack.

KMSCON tension: KMSCON speaks neither Sixel nor Kitty. A half-block renderer is
mandatory as the permanent floor, not optional. Sixel and Kitty are emulator-only
enhancements gated behind capability detection.

| Role | Pick | Score | One-line reason |
|---|---|---|---|
| Core runtime | Bubble Tea v2 | 7/10 | GA, stable, best-supported Go TUI; chrome only, bypass it for graphics |
| Styling/layout | Lipgloss v2 | 9/10 | Right layout and text-overlay engine; Canvas/Layer places glyphs over the bitmap |
| Components | Bubbles v2 | 8/10 | Address bar, status line, page rect, spinner out of the box |
| Animation | Harmonica | 5/10 | Optional chrome polish; defer or vendor |
| Sixel | `charmbracelet/x/ansi/sixel` | 8/10 | Fastest Go encoder, native to the v2 stack |
| Kitty | `charmbracelet/x/ansi/kitty` | 9/10 | Full protocol with unicode-placeholder support |
| Half-block fallback | Custom (ref `x/mosaic`) | 6/10 | Mandatory KMSCON floor; composable cell grid for text overlay |
| Resize/composite | `golang.org/x/image/draw` | 9/10 | In-place scale into a reused buffer; stdlib-grade |
| Quantise/dither | `go-quantize` + `dither` | 7-8/10 | Paletted Sixel only; cache palettes across frames |
| Colour detect | `colorprofile` (+ hardcoded RGB→256) | 8/10 | Env plus terminfo via `colorprofile.Detect`; trusted `COLORTERM`; closed-form formula per pixel. Built in Stage 1 |
| Capability detect | DA1-fenced tty handshake (`x/ansi` + `x/term`) | 9/10 | Kitty `a=q`, Sixel via DA1, cell size via `CSI 16t`/`14t`/`18t`; fails closed to block; never trust `$TERM`. Built in Stage 1 |
| Viewport substrate alt | vaxis | 8/10 | Runner-up if Charm graphics coexistence proves intractable |

### Decisions (TUI research)

- Reserved viewport region vs `WithoutRenderer`? Resolved: keep the standard renderer for chrome and reserve a blank viewport rectangle; do NOT use `WithoutRenderer`. The ultraviolet diff is incremental: `RenderBuffer.SetCell` touches a line only when a cell changes, and `Render` emits zero bytes when no line is touched, so an unchanged viewport rectangle is never visited and gets no cursor move or erase. Two hazards, both controllable: a line whose trailing cells go blank is erased to end of line (`EL0`/`DCH`), so the viewport must never share a row with chrome cells that can shrink; and a full repaint, resize, or alt-screen toggle sets the renderer's clear flag and emits `ED2` over the whole screen, so Nyala must re-blit the viewport after any repaint or resize and must not toggle alt-screen mid-session. Blit via `tea.Raw` or a separate writer so the renderer never sees the graphics bytes. TUIOS proves this on the same stack. Verified at bubbletea v2.0.7 / ultraviolet `terminal_renderer.go`.
- Ultraviolet throughput vs bypass? Resolved as an architecture decision: the bitmap never enters the diff. Route every per-frame bitmap byte out-of-band (`tea.Raw` or a direct writer), never through `tea.View`. Ultraviolet diffs only chrome cells (tens per tick), which is cheap; the bitmap goes out-of-band as one Sixel/Kitty payload, so the renderer's large-region speed does not gate the design. Default FPS is 60, hard cap 120 (`tea.WithFPS`). Stage 4 benchmark: confirm chrome-only diffs and the post-repaint re-blit stay within one frame budget on KMSCON.
- Does KMSCON honour synchronized output (mode 2026) and mode 2027, and tolerate the renderer's cursor-positioning sequences without artefacts? Resolved: the colour half closed in Stage 1; the mode half closes here. KMSCON/libtsm ignores mode 2026 and mode 2027 harmlessly: an unknown DEC private mode hits the parser `default:` branch (debug-log and `continue`), consuming the whole sequence with no grid leak, verified in libtsm v4.4.2 (the version kmscon ships) and main. CUP/HVP, DECSC/DECRC, and SGR truecolour are all supported without artefacts. `DECRQM` (`CSI ?2026$p`) is a stub on KMSCON, so do not probe for 2026 at runtime; assume 2026/2027 absent. Fallback for tear reduction on KMSCON: bracket each frame with cursor hide/show (`CSI ?25l` ... `CSI ?25h`, DECTCEM mode 25), which KMSCON honours, and write each whole frame in one syscall with absolute CUP per row. Send `CSI ?2026h/l` anyway for Kitty/foot/Ghostty, since KMSCON drops it cleanly.
- On graphics-capable emulators, after writing a Kitty/Sixel image, where does the cursor land, and can Nyala reposition so the next diff does not smear the image? Resolved for Kitty: place with `C=1` (do not move the cursor), then issue an explicit absolute `CSI <row>;<col> H` before the next frame; the default `C=0` position is undefined at screen edges and differs across terminals (kitty vs wezterm), which is what smears a following diff. Position via explicit CUP, not DECSC/DECRC, so it does not fight the Bubble Tea chrome cursor. For Sixel, wrap each frame `ESC 7` (DECSC), `CSI <row>;<col> H`, sixel, `ESC 8` (DECRC). Stage 3 spike: confirm `C=1` is honoured on the shipped wezterm/ghostty; if ignored, the explicit per-frame CUP already covers it.
- Per-frame Kitty strategy: one fixed image ID with transmit-and-display, or ping-pong double-buffering with explicit deletes? Resolved: reuse one fixed image id `i=` and one placement id `p=`; re-transmit RGBA each frame with `a=t` then re-place with `a=p,C=1` (or fold into `a=T`), wrapped in DEC mode 2026 and written once per frame. A persistent re-place is not possible: the viewport changes every frame, so pixels must be re-sent; `a=p` only redisplays unchanged stored pixels. Same `(i=, p=)` re-placement is the flicker-free swap, and re-transmit to a live id auto-deletes the old image, so no per-frame `a=d` ping-pong is needed. Use `q=2` to suppress all replies in the loop; query with `q=0` only at startup. Stage 3 spike: confirm same-id replace does not blank-flash on ghostty/wezterm (else fall back to two-id place-then-delete), and whether a repeated `a=T` loop leaks images on wezterm (issue #986; if so inject periodic `a=d,d=I,i=`).
- Best local transmission medium: direct base64 RGBA, zlib RGBA, PNG, or POSIX shared memory? Resolved: default `t=s` (POSIX shared memory, `f=32` raw RGBA), fallback `t=d,f=32,o=z` (direct base64, zlib RGBA, chunked at 4096 with `m=`). Shared memory minimises both CPU and wire bytes (memcpy raw RGBA, escape carries only the object name, no base64 inflation, no per-frame encode); PNG `f=100` has the worst encode CPU; plain base64 has the worst byte count. The `t=d,o=z` fallback is conservative: it needs no shared filesystem and is the only path that works over SSH. Optional middle tier `t=t` (temp file in `/dev/shm`, path containing `tty-graphics-protocol`). Probe order: `t=s` local, `t=t` local, `t=d,o=z` remote. Shared memory is supported on kitty, ghostty, and wezterm; assume `t=d` elsewhere. Stage 3 spike: measure sustained full-viewport FPS and per-frame `shm_open`/mmap cost vs a reused mapped object on each target terminal.
- Confirm which Kitty placement modes render on current wezterm and ghostty; unicode placeholders are unsupported in several terminals. Resolved: use classic cursor-anchored placement (`a=T`/`a=p`); it renders on current kitty, ghostty, and the wezterm GUI. Unicode placeholders (`U=1`, `U+10EEEE`) render only on kitty (and Terminal.app) as of mid-2026; ghostty 1.3.1 and iTerm2 do not render them, and wezterm has them only in an unmerged fork (issue #986). Reserve placeholders for kitty-only tmux passthrough. Stage 3 spike: re-test placeholder render status on the exact ghostty and wezterm builds shipped.
- Cell pixel geometry: query CSI 16t for cell width/height to map the CDP pixel viewport to cells without distortion. Define the fallback when terminals omit it (per-terminal defaults). (Resolved in Stage 1 (see `docs/STAGE-1.md`): resolve via `TIOCGWINSZ`, then `CSI 16t`, then `14t`/`18t`, else default to 8x16 px (aspect 1:2), with a winsize sanity bound rejecting derived cell px outside ~4..32 wide or 8..64 tall.)
- Will Nyala drive the screencast viewport to terminal resolution (Carbonyl LoDPI) so downsampling is cheap, or downsample full-resolution frames every frame? Resolved: drive the viewport small (LoDPI). Size the emulated viewport and `maxWidth`/`maxHeight` so one captured pixel maps to one sub-cell (2 px per cell half-block, 2x2 quadrant); an 80x24 quadrant grid needs a 160x48 px capture. The engine rasterises at terminal resolution once, avoiding a full-frame downscale in Go on every frame. Evidence: Carbonyl reports LoDPI is about 49x faster than native-resolution renderers. Caveat: tiny viewports change page layout, so treat viewport size as a layout knob and expose a zoom override for sites that break.
- For a constantly-changing viewport, is per-frame full-screen Sixel/Kitty bandwidth acceptable, or does Nyala need damage-rectangle partial updates? Resolved: default to full-frame redraw per changed frame; add cell-diff damage and partial emission only when measured. At LoDPI the frame is small, so full redraw is the simple, correct baseline, and the per-cell diff already gates whether anything changed. Escalate to damage rects when the diagnostics harness shows pty write throughput or frame latency breaching threshold on a busy viewport (scrolling, video). Evidence: Carbonyl runs 60 FPS full-frame at LoDPI without rect updates; bandwidth pressure appears only in full-resolution bitmap mode, which is not the default. Stage 3 sets the escalation threshold from harness numbers.
- Does a fixed/stable palette (skipping per-frame median-cut) give acceptable quality for arbitrary web pages, or will photo banding force per-frame quantisation? Resolved: stable/cached palette default; per-frame median-cut only when photo banding forces it. A fixed palette skips per-frame quantisation, which protects the moving-viewport frame budget, and Sixel terminals broadly handle a stable shared palette. Stage 3 spike: benchmark cached vs per-frame palette on a photo-heavy fixture for banding and FPS before enabling per-frame quantisation.
- Sixel lacks a Kitty-style ID and scrolls with text; it needs its own placement/erase discipline and tearing test. Resolved: anchor every frame with `ESC 7` (DECSC), `CSI <row>;<col> H` (CUP to a fixed origin), sixel `ESC P q ... ESC \`, `ESC 8` (DECRC), all wrapped in DEC mode 2026 and written once to stop tearing; prefer `ESC 7`/`ESC 8` over `CSI s`/`CSI u`. Leave DECSDM (mode 80, `CSI ?80h/l`) alone: its set/reset sense is inverted across terminals, and the save/CUP/restore wrap anchors the frame regardless. Erase: a same-size full-viewport overwrite needs no clear; clear the region first (background fill or `CSI 2 J`) only on viewport shrink or geometry change, because sixel leaves opaque pixels with no alpha. Sixel reaches xterm, foot, wezterm, mlterm, contour, Windows Terminal, Konsole, and tmux (`--enable-sixel`), but not ghostty, kitty, or Alacritty, so it stays the broad-compatibility fallback below Kitty. Stage 3 spike: measure full-viewport FPS, per-frame encoded bytes, and whether mode 2026 removes visible tearing on foot/wezterm/xterm/mlterm at target resolution.
- Truecolour over SSH/Docker is unreliable via `COLORTERM`. Will Nyala implement Carbonyl's DCS background-colour round-trip handshake, and does it interact cleanly with Bubble Tea v2 setup? (Resolved in Stage 1 (see `docs/STAGE-1.md`): no. The DCS round-trip is dropped and not planned. Nyala detects colour from env plus terminfo via `colorprofile.Detect`, and treats a present `COLORTERM` as a trusted positive.)
- tmux/SSH: Kitty and Sixel need DCS passthrough inside tmux and AcceptEnv over SSH. Target these or document as unsupported? Resolved: tmux is block-only for v1. Nyala detects tmux (`$TMUX` set) and falls back to the block backend, which renders everywhere. Graphics through tmux need DCS passthrough (`allow-passthrough on`, app-side sequence wrapping, pane-offset maths, unicode-placeholder redraw), and the result is fragile and about to be obsoleted: tmux merged native Kitty image support into mainline in March 2026 (behind `--enable-kitty-images`), unreleased and post-3.5. Revisit graphics-in-tmux once a tagged release ships it. Over SSH, graphics ride the same stdout stream as text and work with no server-side env plumbing; do not gate graphics on `AcceptEnv`/`SendEnv` or `COLORTERM` (env-only forwarding). Detect capability at runtime by querying the terminal (Kitty `a=q`, Sixel via DA1), the only reliable method over SSH and inside tmux; treat `COLORTERM=truecolor` as an advisory colour hint.
- Is ultraviolet (0.x, ~357 stars) stable enough to depend on transitively for a long-lived project? Is Charm's promised first-class Bubble Tea image support on a timeline Nyala could wait for? Resolved: accept ultraviolet transitively; it is unavoidable under Bubble Tea v2, which sits on it. Ultraviolet ships only `v0.0.0-...` pseudo-versions and warns its API may change, but the risk is bounded: Nyala depends on the stable Bubble Tea v2 surface (v2.0.0, Feb 2026), which pins the ultraviolet version it needs, and the bus factor is Charm (an active company), not a lone maintainer. Do not wait for Charm first-class image support: Charm states it has no plans, citing low terminal adoption, so Nyala owns its graphics pipeline (block, Sixel, Kitty) and will continue to.
- Bus-factor risk: is depending on vaxis (single maintainer, sr.ht, 0.x) acceptable, and is vendoring or a maintained fork an acceptable mitigation? Resolved: vaxis stays a documented escape hatch, not a dependency. It is single-maintainer on sr.ht (GitHub mirror, Apache-2.0/MIT, v0.16.0, actively maintained as of mid-2026). The single-maintainer bus factor rules it out as a live dependency, but if Charm graphics coexistence proves intractable, vendoring or a maintained fork is a sound mitigation given the permissive licence and mirror.
- Will the team accept owning ~20-40 lines of raw terminal control (release/restore, region blanking, sixel/kitty encode) as the price of Bubble Tea, or split the stack? Resolved: accept the owned layer; keep Bubble Tea v2 for chrome, do not split the stack. The surface is bounded and TUIOS-proven: per-frame `DECSC`, `CUP`, APC/DCS payload, `DECRC`, wrapped once per batch in `CSI ?2026h/l` on emulators or cursor hide/show on KMSCON, plus re-blit-after-repaint and the reserved-rectangle invariant (no chrome and viewport on a shared row). The real cost is holding that invariant, not the line count. vaxis stays the documented fallback only if the invariants prove unholdable.
- Under mode 2026, must chrome and viewport compose into one synchronized frame per tick, or can they bracket independently? Resolved: bracket independently. Chrome and viewport never share cells (chrome leaves the rectangle blank, the out-of-band blit fills it), so there is no seam to tear. Bubble Tea emits and brackets its own text frame; Nyala wraps the graphics batch in its own `CSI ?2026h ... CSI ?2026l`, sequenced inside `View()` to land right after the text frame so graphics never precede the cells that reserve them. Order matters, cross-frame atomicity does not. On KMSCON mode 2026 is a no-op, so this degrades to one-syscall-per-frame plus cursor hide/show. Single-frame chrome plus graphics atomicity is a later refinement, not a requirement; TUIOS ships without it.

## Half-Block Rendering Research

This section records the research behind the half-block fallback path. This path is
the permanent floor. It must always work on terminals with no Sixel and no Kitty:
KMSCON, basic emulators, and many SSH/tmux sessions. It maps the RGBA viewport to a
terminal cell grid using Unicode block glyphs and terminal colour, then composites
crisp real Unicode text over the bitmap. The crisp-text overlay is the make-or-break
feature that separates Nyala from browsh-grade fuzz. Browsh (Go) and Carbonyl (Rust)
are the two reference implementations; the techniques below port their algorithms to
Go. Sixel and Kitty stay emulator-only enhancements and are out of scope here. Dates
verified mid-June 2026.

### Browsh internals

Browsh splits cleanly: a Firefox WebExtension emits two JSON streams (a pixel frame
and a positioned text frame), and a Go interfacer composites them through tcell.

Techniques:

- **Half-block two-pixels-per-cell (`U+2584`)**: each cell draws `▄`, bg = top pixel,
  fg = bottom pixel, giving two stacked square pixels. The browser pre-scales the
  screenshot so 1 source pixel maps to half a cell. Trade-off: doubles vertical
  resolution with zero extra escapes and works on a bare TTY, but only 2 colours per
  cell and horizontal resolution stays at one pixel per cell.
- **Separate text frame over pixel frame**: pixels, text runes, and text colours share
  one cell grid keyed by index. A real glyph wins its cell; otherwise the half-block
  fills it. Trade-off: crisp glyphs over a bitmap backdrop, but the text layer must sit
  on the exact same grid or text floats off its background.
- **Dual-screenshot font hack (do NOT port)**: Browsh forces a solid-block font and
  takes two screenshots to sample per-glyph colour and detect occlusion. Nyala gets
  text colour and visibility from CDP directly, so skip this and its per-site CSS
  fixes.
- **DOM-rect to cell snapping (do NOT port)**: Browsh steps across DOM boxes one
  char-width at a time to place glyphs. This is the documented source of its fuzzy,
  misaligned text. Nyala should use CDP-reported text geometry instead.
- **Hatched placeholder**: alternating grey `▄` cells mark un-loaded regions during a
  streaming frame. Cheap first-paint cue worth copying.
- **Monochrome mode**: force fg=white, bg=black, replace `▄` with a space for a
  text-only readout on low-colour terminals.

| Tool | Score (n/10) | Maturity | Maintenance | Suitability |
|---|---|---|---|---|
| `gdamore/tcell` | 8 | v3.1.2 2026-01-26; v2 line maintained; Browsh pins dead v1.4.0 | Very active; 5.1k stars; last push 2026-04-07 | Exact-fit primitives (rune+fg+bg, truecolour, 256/8 fallback, mouse, cell diff), but owns the whole screen and clashes with the chosen Bubble Tea v2 / Lipgloss v2 renderer |
| `gorilla/websocket` | 5 | v1.5.1; stable | Revived after 2022 archival; active | Irrelevant to Nyala; CDP replaces the WebExtension transport |
| `lucasb-eyer/go-colorful` | 6 | v1.2.0; stable | Mature, low churn | Useful only if Nyala does its own perceptual 256-colour quantise |

- Pros: tcell does the exact half-block primitives Browsh needs and diffs cells for
  free; pure Go; runs on a bare TTY.
- Cons: tcell owns the screen and fights the Charm v2 stack; gorilla and go-colorful do
  not transfer to the CDP architecture.
- Sources: `github.com/browsh-org/browsh` (`interfacer/src/browsh/frame_builder.go`,
  `tty.go`), `github.com/gdamore/tcell`.

Verdict: copy Browsh's compositing model (one shared grid, glyph-wins-cell, `y+=2`
pixel walk); skip its font hack and DOM-rect stepping; do not adopt tcell as the
renderer, mine it only for colour-downsample and cell-diff logic.

### Carbonyl internals

Carbonyl is a Chromium fork that renders into a terminal. Its shipped source goes past
the blog's half-block to a 2x2 quadrant cell, a closed-form colour conversion, and a
per-cell diff. The algorithms port to Go cleanly.

Techniques:

- **Quadrant cell (2x2 colours, 8 glyphs)**: each cell holds four colours.
  `binarize_quadrant` converts the four to Rec.601 luma, splits at the luma midpoint,
  builds a 4-bit mask, and maps it to one of 8 block glyphs with two averaged colours.
  Trade-off: doubles perceived resolution over plain half-block for a small luma
  computation and a 16-way branch, but a cell still holds only 2 colours.
- **Bitmap to quadrant downsample**: `avg_with` box-averages source pixels into each
  cell quadrant. Size the CDP viewport so one cell maps to 2x4 (or 2x2) source pixels.
  Watch byte order: CDP decodes to RGBA, Carbonyl reads BGRA.
- **Closed-form RGB to xterm-256**: no nearest-neighbour search. A grayscale branch maps
  near-grey pixels to the 24-step ramp; the colour branch projects onto the non-linear
  6x6x6 cube (levels 0,95,135,175,215,255) via a fused multiply-add. O(1) per pixel.
- **Per-cell diff with SGR coalescing**: keep (previous, current) cells; skip unchanged
  cells; emit a cursor move only when not contiguous and an SGR colour code only when
  the colour changed. Buffer one write per frame.
- **Crisp text overlay**: each cell carries an optional grapheme that overrides the
  block; bg from the bitmap under the glyph, fg from the text colour. Nyala feeds this
  from the CDP text layer, so no Skia or Mojo fork is needed.
- **Truecolour via DCS query (not COLORTERM)**: Carbonyl sets an RGB background, sends
  a DCS query, and enables truecolour only if the terminal echoes the stored SGR.
  Nyala diverged: it detects colour from env plus terminfo and trusts a present
  `COLORTERM`, with no DCS round-trip. The probe is a no-op on KMSCON's libtsm and
  `COLORTERM` now covers the targets (see `docs/STAGE-1.md`).
- **LoDPI / forced monospace (do NOT port)**: a Chromium fork hack. Nyala gets the
  equivalent by sizing the CDP viewport to the cell grid; the separate text layer
  removes the need for forced-monospace layout.

| Tool | Score (n/10) | Maturity | Maintenance | Suitability |
|---|---|---|---|---|
| `rivo/uniseg` | 9 | v0.4.x; tracks Unicode 15+ | Active; de-facto Go grapheme/width lib | Direct fit for placing crisp glyphs (CJK/emoji width, ZWJ clusters); matches the Charm stack |
| `charmbracelet/x/ansi` | 8 | Pre-1.0; built with Bubble Tea v2 | Charm; very active | Native SGR/cursor/DCS byte construction; Bubble Tea v2 supplies key + SGR 1006 mouse |
| Custom quadrant compositor | 9 | To be written; algorithm proven in Carbonyl | Owned by Nyala; ~300-500 LoC | The heart of the path: port `quad.rs`, `cell.rs`, `painter.rs`, `xterm.rs` |
| `muesli/termenv` | 6 | v0.16.0 2025-02-21; mature | Maintained, lower velocity | First-guess profile only; its COLORTERM detection is wrong on the primary targets |

- Pros: the quadrant algorithm doubles resolution and is small and testable; uniseg and
  x/ansi fit the chosen stack.
- Cons: you must write and test the compositor; termenv mis-detects KMSCON; mind the
  RGBA/BGRA byte order.
- Sources: `github.com/fathyb/carbonyl` (`src/output/{quad,cell,painter,xterm,renderer}.rs`),
  `fathy.fr/carbonyl`, `github.com/rivo/uniseg`, `github.com/charmbracelet/x`.

Verdict: port Carbonyl's quadrant compositor and closed-form colour maths as an owned
Nyala package; lean on uniseg and x/ansi from the Charm stack; lock the colour and
quadrant logic with golden-frame tests because Carbonyl is dormant.

#### Active Carbonyl forks

Upstream `fathyb/carbonyl` is dormant, so two community forks are tracked as live
references. Both keep Carbonyl's quadrant compositor, Rec.601 luma split, closed-form
RGB to xterm-256, and per-cell diff unchanged, and both place text by cell origin
with a one-cell advance per grapheme (no per-glyph geometry), so they confirm the
lineage relies on Carbonyl's forced-monospace layout, not measured glyph x. Both read
BGRA, the mirror of Nyala's RGBA, so their channel indexing must not be copied. Full
review in `BLOCK-RESEARCH.md`.

- **`carboxyl-rs/carboxyl`** (BSD-3-Clause; last commit 2026-06-14): a rebrand only.
  The `src/output/` and `src/gfx/` trees are byte-identical to Carbonyl 0.0.3; the
  only `src/` change is the ASCII logo. Nothing fork-specific to port.
- **`jmagly/carbonyl`** (BSD-3-Clause; last commit 2026-06-23): an active fork, ported
  from Chromium M111 to M147, the renderer god-module split into per-concern files.
  Two ideas worth porting: a `UNIFORM_THRESHOLD` guard that collapses a near-uniform
  (sub-JND) cell to a single full block, killing antialiasing mottle in the quadrant
  binariser, and a colour-inversion toggle applied at SGR-emit time for an optional
  high-contrast mode.

### Brow6el internals

Brow6el is the third reference: a full terminal browser that embeds Chromium directly
through CEF (Chromium Embedded Framework) rather than driving it over CDP. It renders
the whole viewport as a Sixel or Kitty image and never composites text or block glyphs.
It is the closest live example of the CEF-embed path Nyala is weighing against its
chromedp/CDP design, so the trade-offs below feed that decision.

Techniques:

- **CEF off-screen rendering (the engine contrast)**: brow6el links the CEF library and
  runs Chromium windowless. CEF calls `OnPaint` with the rendered viewport as a BGRA
  buffer each frame. Trade-off: no separate browser process to launch or speak CDP to,
  and the bitmap arrives in-process with no screenshot round-trip, but the binary embeds
  Chromium and pulls a ~670MB CEF distribution at build time. Nyala's CDP path keeps
  Chromium as an external process and trades the in-process buffer for a
  `Page.captureScreenshot` (or `screencast`) hop.
- **BGRA buffer to image pipeline**: the CEF buffer feeds two paths. Sixel goes BGRA →
  libsixel → Sixel escapes. Kitty goes BGRA → RGBA → base64 → Kitty escapes. Mind the
  byte order: like Carbonyl, CEF hands back BGRA, not the RGBA Nyala's pipeline assumes.
- **Image-only output, no text overlay (the make-or-break gap)**: brow6el emits the
  page purely as a graphics-protocol image. It has no half-block fallback and no crisp
  Unicode text layer, so all text is whatever the Sixel or Kitty bitmap shows and reads
  as fuzzy at terminal scale. This is the exact weakness Nyala's CDP text layer exists
  to fix; the CEF embed gives no text geometry of its own without extra Chromium plumbing.
- **Tiled Sixel diffing**: an optional mode redraws only changed screen regions as Sixel
  tiles to cut flicker and data volume; monolithic mode redraws the whole screen. Kitty
  always redraws monolithically, with double-buffered image IDs to prevent flicker and
  the image placed below the text layer (`z=-1`). A per-frame diff worth noting for
  Nyala's Sixel and Kitty enhancement paths, though Nyala diffs cells, not image tiles.
- **Cell-grid sizing by zoom, not a Chromium fork**: brow6el auto-detects terminal cell
  dimensions and sizes the render by a zoom factor (`zoom_level`, DPI-aware `auto`
  behaviour). It needs no LoDPI/forced-monospace fork because it ships an image, not a
  cell grid. Nyala sizes the CDP viewport to its grid instead.
- **Modal vim input and mouse emulation**: STANDARD/INSERT/MOUSE/VISUAL modes,
  keyboard-driven mouse with a grid-jump overlay, hint-mode link labels, and a CDP-style
  element inspector. A reference for terminal input mapping, independent of the engine
  choice.

| Tool | Score (n/10) | Maturity | Maintenance | Suitability |
|---|---|---|---|---|
| CEF (Chromium Embedded Framework) | 6 | CEF 148 / Chromium 148 beta pinned | Tracks Chromium; mature upstream | The engine-embed alternative to CDP: in-process BGRA frames, no external browser, but ~670MB build dependency, C/C++ binding, and Chromium baked into the binary |
| `libsixel` | 7 | Mature; MIT | Maintained | Proven RGBA/BGRA-to-Sixel encoder; relevant only to Nyala's Sixel enhancement path, not the half-block floor |
| Custom Kitty encoder | 6 | In brow6el; BGRA → RGBA → base64 | Owned by brow6el | Confirms the Kitty pipeline shape; Nyala gets the same from `charmbracelet/x/ansi/kitty` |

- Pros: proves the CEF-embed path end to end; in-process frames skip the CDP screenshot
  hop; shows a complete terminal input and chrome model to mine.
- Cons: image-only, so text is fuzzy with no overlay, the gap Nyala must close; C/C++ and
  a ~670MB CEF build dependency against Nyala's pure-Go chromedp stack; self-described
  POC quality; no half-block floor, so it needs a Sixel- or Kitty-capable terminal.
- Sources: `codeberg.org/janantos/brow6el` (`README.md`, `download_cef.sh`),
  `www.brow6el.dev`, `aur.archlinux.org/packages/brow6el-git`. Licence MIT (project),
  CEF BSD 3-Clause, libsixel MIT; active, v0.3.4, last source commit 2026-05-20, CI
  AppImage builds for amd64 and arm64. The exact CEF `OnPaint`/BGRA call sites were not
  read directly (source tree fetch timed out); the pipeline is taken from the project's
  own "How It Works" description and is marked accordingly.

Verdict: treat brow6el as the CEF-embed reference point, not a port target. It validates
that embedding Chromium yields in-process frames with no CDP hop, but it ships fuzzy
image-only text with no half-block floor, which is the weakness Nyala's CDP text overlay
removes. Carry its tiled-diff and modal-input ideas forward; carry the CEF-vs-CDP
trade-off (build weight and language against in-process frames) into the engine decision.

### Sub-cell glyph density

A cell carries one fg and one bg, so denser glyphs trade colour fidelity for spatial
resolution. Every step up forces more sub-pixels into two colours. Font coverage is
the hard gate.

Techniques:

- **Half-block (1x2, `U+2580`/`U+2584`)**: two true colours per cell, universal font
  support. The safe floor; zero colour compromise; lowest detail.
- **Quadrant (2x2)**: luma-split binarisation picks 1 of 16 glyphs plus two averaged
  colours. Unicode 1.1/3.2, universal. Best general balance for photographic content.
- **Sextant (2x3)**: Unicode 13 (2020). More vertical detail, solid glyphs, but needs a
  Unicode-13 font and can show inter-row background bleed from tight glyph boxes.
- **Octant (2x4)**: Unicode 16 (2024). Highest solid-block resolution, but drawn by
  very few terminals today and effectively unavailable on font-driven KMSCON.
- **Braille (2x4 dots)**: portable everywhere, but renders as gapped dots, wrong for a
  colour viewport. Good for line art and accessibility.
- **Glyph-correlation (chafa)**: scores a large glyph set per cell for best fit.
  Highest quality, too heavy for a live moving viewport; an offline/paused mode at best.

| Tool | Score (n/10) | Maturity | Maintenance | Suitability |
|---|---|---|---|---|
| `charmbracelet/x/mosaic` | 6 | Pre-1.0; experimental x package | Active; Charm-backed | In-stack half/quadrant renderer; no sextant/octant adaptivity; read or extend it |
| `hpjansson/chafa` | 7 | Mature; stable C API | Very active; single principal author | Gold-standard glyph selection, but C; reference and quality benchmark only |
| `queelius/dapple` | 4 | New (2026); not Go | Young, single author | Renderer-taxonomy and auto-select template only; cannot import |
| Custom Nyala compositor | 8 | To be written; algorithms published | Owned by Nyala | Best fit; quadrant default, half-block floor, octant only where proven |

- Pros: a custom compositor gives exact cell ownership for text overlay and runtime
  scheme switching; mosaic shares the stack as a reference.
- Cons: you write and test the quantiser and glyph tables; higher tiers need font
  capability detection.
- Sources: `github.com/fathyb/carbonyl` (`src/output/quad.rs`),
  `github.com/browsh-org/browsh` (`interfacer/src/browsh/tty.go`),
  `github.com/ObscuritySRL/bun-win32` (`packages/terminal/glyphs.ts`),
  `github.com/hpjansson/chafa`, `github.com/queelius/dapple`.

Verdict: quadrant (2x2) default, half-block (1x2) floor, adaptive promotion to octants
only where the terminal and font prove Unicode 16 support; sextant is an opt-in tier,
braille an explicit line-art/accessibility mode.

Sextant (2x3) research spike (unscheduled): add sextant as a font-gated tier above
quadrant and measure it before committing. It raises vertical resolution by 50%, the
scarce axis, since cells are about 1:2. The catch: six sub-cells still share one
foreground and one background, so colour quantisation error per cell rises as shape
detail rises. Unicode 13 (2020) font support is narrower than quadrant, so it can never
replace the floor; GNU Unifont covers the range, so the existing Unifont fallback tier
may reach it. The generalisation is mechanical: a `SchemeSextant`, a `sampleSextant`
and `binarizeSextant` (direct extensions of the quadrant pair), a coverage check like
`QuadrantSupport`, and a 60-entry pattern-to-rune table. The value band is narrow,
terminals with sextant glyphs but no Sixel or Kitty, so gate the spike on the
diagnostics harness: score quality, cost, and legibility against quadrant on the
committed fixtures, and keep it only if the gain clears the added complexity. Octant
(2x4) is the next rung behind the same tiering, newer and less supported.

### Go half-block / cell-art modules

No published Go module delivers the full path: a composable cell grid, text overlay,
truecolour-or-256 colour, and frame diffing. Every library returns a finished ANSI
string and does no overlay or diff. Own the compositor.

Techniques:

- **Indexed cell grid with glyph overwrite (Browsh)**: cells indexed by `(y*width+x)`,
  each `{rune, fg, bg}`; the bitmap fills `▄` cells, the text layer overwrites the same
  cells. The model that lets text and half-block coexist; no Go library exposes it.
- **Cell-buffer frame diff**: hold previous and current grids; emit only changed cells;
  track cursor and active style to skip redundant SGR and cursor moves. Essential for a
  moving viewport; no image library does it.
- **Quadrant vs half-block, closed-form xterm-256**: the same Carbonyl maths above. A
  fixed-128 threshold (as in mosaic) biases flat dark or bright blocks, so prefer the
  adaptive luma midpoint.

| Tool | Score (n/10) | Maturity | Maintenance | Suitability |
|---|---|---|---|---|
| `charmbracelet/x/mosaic` | 6 | Pre-1.0; pseudo-version; feature-static | Charm; dependabot only | Best in-stack algorithm reference, but returns a string, no overlay, no diff, fixed-128 threshold |
| `Zebbeni/ansipx` | 5 | v1.0.0; created 2026-04 | Single author; 6 stars | Richest dither/alpha options as a recipe; string output; pulls archived `nfnt/resize` |
| `blacktop/go-termimg` | 5 | Pre-1.0; Go 1.26 | Single author; 59 stars | Half-block path wraps mosaic; adds nothing; better for capability detection |
| `qeesung/image2ascii` | 3 | Stable, feature-frozen | Dormant; 978 stars | ASCII ramp, lower fidelity, clashes with crisp text; `CharPixel` shape worth a read |
| `eliukblau/pixterm` | 4 | ansimage frozen at 2019 pseudo-version | Low; last push 2024-09 | Textbook `U+2584` reference; renders to a writer, no grid, no diff |
| Browsh (reference) | 7 | Mature product; GPL | Maintained, renderer stagnant | Proves the indexed-grid + glyph-overwrite model in production Go; port the idea, not the GPL code |
| Carbonyl (reference) | 7 | v0.0.3 2023; Rust | Dormant; ~19k stars | The maths to port: quadrant binarisation + closed-form xterm-256 |

- Pros: the reference implementations prove the model; mosaic is a readable in-stack
  guide.
- Cons: all libraries fail the overlay and diff requirements; several pull unmaintained
  resize deps.
- Sources: `pkg.go.dev/github.com/charmbracelet/x/mosaic`, `github.com/Zebbeni/ansipx`,
  `github.com/blacktop/go-termimg`, `github.com/qeesung/image2ascii`,
  `github.com/eliukblau/pixterm`, `github.com/browsh-org/browsh`,
  `github.com/fathyb/carbonyl`.

Verdict: own a custom cell-grid compositor; port Carbonyl's quadrant + xterm-256 maths
onto Browsh's indexed-grid model; use `x/image/draw` for resize; treat mosaic as a
reference, not a dependency.

### Colour pipeline

Colour conversion is the hottest loop: roughly cols x rows x 2 conversions per frame,
many times a second. Split by terminal capability.

Techniques:

- **Direct truecolour SGR passthrough (preferred)**: where truecolour is supported, skip
  conversion. Emit one combined `48;2;.../38;2;...` SGR then `▄`. No maths, exact colour;
  highest byte cost, so the diff matters more here.
- **Carbonyl closed-form RGB to xterm-256**: grey branch onto the 24-step ramp,
  otherwise project onto the non-linear 6x6x6 cube (the 95-gap detail most online
  formulas get wrong). A handful of float ops per pixel, no search, no allocation.
- **tmux/colette nearest-of-two**: compares the cube candidate and the grey candidate by
  squared RGB distance. More accurate than Carbonyl's single threshold, two distance
  computations per pixel. A drop-in if 256-colour fidelity matters.
- **KMSCON-aware detection**: KMSCON sets `TERM=linux` and `COLORTERM=kmscon`, and its
  truecolour landed in libtsm on 2025-11-28. Off-the-shelf detectors map `TERM=linux`
  to 16 colours and ignore `COLORTERM=kmscon`. Stage 1 special-cases it: a present
  `COLORTERM` in `{kmscon, truecolor, 24bit}` is a trusted positive that short-circuits
  to truecolour. No DCS round-trip is used; on KMSCON's libtsm it is a no-op and would
  give a false negative (see `docs/STAGE-1.md`).
- **Optional ordered Bayer dither (256 path only)**: stateless per pixel and
  frame-stable, unlike Floyd-Steinberg which flickers on motion. Off by default.

| Tool | Score (n/10) | Maturity | Maintenance | Suitability |
|---|---|---|---|---|
| Custom closed-form converter (port of Carbonyl `xterm.rs`) | 9 | Own code; algorithm proven | Owned by Nyala; ~30 lines | Exact fit for the hot loop: uint8 in/out, no boxing, no lock, no search |
| `charmbracelet/colorprofile` | 7 | v0.4.3 + pseudo-versions; created 2024-09 | Charm; very active | Best for detection and chrome colours; `Convert` boxes and locks, so not for the per-pixel loop; misses `COLORTERM=kmscon` |
| `muesli/termenv` | 6 | v0.16.0 2025-02-21 | Maintained, maintenance mode | Detection and chrome only; per-pixel HSLuv distance is too slow |
| `makeworld/dither` v2 | 5 | v2.x; correctness-focused | Quiet; single maintainer | Bayer matrices as a reference for the optional 256-path dither; whole-image API is awkward per cell |
| `ericpauley/go-quantize` | 3 | Dormant since 2020; pre-modules | ~25 stars; inactive | Wrong tool; the xterm-256 palette is fixed, no adaptive palette needed |

- Pros: the owned closed-form converter is the fastest possible path; colorprofile fits
  the stack for detection and chrome.
- Cons: every library boxes colour through interfaces, too slow per pixel; detectors
  miss KMSCON.
- Sources: `github.com/fathyb/carbonyl` (`src/output/xterm.rs`),
  `github.com/charmbracelet/colorprofile`, `github.com/muesli/termenv`,
  `github.com/makeworld-the-better-one/dither`, `github.com/ericpauley/go-quantize`.

Verdict: own a tiny closed-form RGB to xterm-256 converter for the per-pixel loop;
emit direct truecolour SGR where supported; use colorprofile for detection and chrome
only, with a trusted `COLORTERM` positive and no DCS probe (Stage 1 dropped the
round-trip, see `docs/STAGE-1.md`); keep dither off by default.

### Frame diffing and minimal output

For a viewport changing every frame, per-frame byte cost is dominated by how many
cells re-emit, how many SGR escapes per run, and how many cursor moves. All references
converge on a previous-frame buffer plus cached last-style. Inside a fully-dirty
viewport, clever line transforms buy almost nothing.

Techniques:

- **Cell-level damage diff**: hold current and previous grids; emit only differing
  cells. Free insurance for static chrome and unchanged text cells; bounded inside a
  moving viewport.
- **Run-length SGR coalescing**: cache last fg/bg/style; emit the colour escape only on
  change. The single biggest byte saver; turns a flat run of N cells from ~19N bytes
  into ~19+3N. Use a combined fg+bg SGR when both change.
- **Cursor minimisation**: tier 1 (Carbonyl) emits an absolute CUP only when the cursor
  is not already where the next cell sits; near-optimal for dense damage. Tier 2
  (tcell/ultraviolet) ICH/DCH/scroll transforms gain little on a solid bitmap, though
  REP can collapse runs of an identical cell.
- **Synchronized output (DEC mode 2026)**: wrap each frame's bytes to paint atomically
  and stop tearing. KMSCON does not support it, so fall back to cursor hide/show
  there. Gate behind detection and disable inside multiplexers.
- **Double-buffer, one write per frame**: render all escape bytes into one reused buffer
  and write once. Removes per-cell syscall overhead and lets mode 2026 wrap the frame.

| Tool | Score (n/10) | Maturity | Maintenance | Suitability |
|---|---|---|---|---|
| `charmbracelet/ultraviolet` | 7 | Pre-1.0; no tags; backs Bubble Tea v2 | Charm; very active (2026 fixes) | Already a transitive dep; diffs the chrome well; line transforms wasted on the viewport; `Cell` cannot carry graphics |
| `gdamore/tcell` v3 | 7 | v3.4.0 2026-05-17; v2 maintained | Very active; healthy | Clearest readable reference for the diff loop and mode 2026; second renderer if adopted beside Bubble Tea |
| Carbonyl renderer (port) | 6 | v0.0.3 2023; Rust | Dormant | Best single source: half-block plus crisp text in one diff loop; ports almost line-for-line; add mode 2026 on top |
| notcurses (design ref) | 4 | Mature C library | Active; cgo only | Architecture reference for render/rasterize + damage map; not a Go dependency |

- Pros: the references show the exact technique; ultraviolet handles the chrome already.
- Cons: ultraviolet and tcell line transforms are wasted on a fully-dirty viewport;
  Carbonyl predates mode 2026.
- Sources: `github.com/charmbracelet/ultraviolet`, `github.com/gdamore/tcell`,
  `github.com/fathyb/carbonyl` (`src/output/{renderer,painter}.rs`),
  `notcurses.com`.

Verdict: own a small viewport writer modelled on Carbonyl's painter (previous-frame
grid, SGR coalescing, absolute-CUP-on-move, one buffered write); wrap each write in
mode 2026 where supported, hide/show cursor on KMSCON; let Bubble Tea v2 keep diffing
the chrome.

### Text-over-bitmap compositing

The make-or-break decision is per cell: does this cell carry page text or only bitmap?
Both references resolve it the same way: one cell grid where every cell holds the
bitmap fill plus an optional grapheme, and the grapheme wins at paint time.

Techniques:

- **Single combined cell grid**: each cell stores the bitmap quad/half-block AND an
  optional grapheme + colour. A grapheme present means a text cell; otherwise emit the
  block. No separate z-buffer. This collapses cell ownership, z-order, and block
  suppression into one branch.
- **Text-cell colour**: fg = the glyph's DOM colour; bg = the average of the cell's
  underlying bitmap colours, so the glyph sits on the local page background and contrast
  is preserved by construction. An optional luma-distance nudge toward black/white can
  rescue thin anti-aliased glyphs on busy backgrounds.
- **Alignment to the grid**: map CDP pixel boxes to cells by dividing by a known
  cell width/height. Sub-cell positions need a chosen rounding rule and a stable
  cellW/cellH (CSI 16t with a per-terminal fallback) or text drifts off by one.
- **Wide glyphs (CJK/emoji)**: measure each grapheme width with uniseg, claim N cells,
  mark trailing cells occupied so the block-fill never paints into them; clip (do not
  half-emit) a wide glyph at the viewport edge.

| Tool | Score (n/10) | Maturity | Maintenance | Suitability |
|---|---|---|---|---|
| Lipgloss v2 Canvas/Layer | 4 | v2.0.0 GA 2026-02-24 | Charm; very active | Wrong for the viewport composite: layers merge whitespace as opaque, so gaps do not fall through. Use it for chrome overlays only |
| `charmbracelet/x/mosaic` | 4 | Pre-1.0; feature-static | Charm | Bitmap-half reference only; string output gives no per-cell text hook |
| `rivo/uniseg` + `mattn/go-runewidth` | 9 | uniseg v0.4.x; runewidth mature | Both very active; in the tree already | Mandatory for correct grapheme segmentation and wide-glyph width/index |
| Carbonyl (reference) | 7 | v0.0.3 2023; Rust | Dormant; ~19k stars | The canonical end-to-end model: `Cell{quadrant, grapheme}`, text-wins, fg=glyph/bg=averaged |
| Browsh (reference) | 6 | Stable; stagnant | Firefox-bound | Go reference for the `buildCell` text-vs-block branch; reject its CSS-snap text source |

- Pros: uniseg/runewidth give exact widths; the references solve every sub-problem in
  readable code.
- Cons: Lipgloss layers cannot express transparent text-over-bitmap; the text source
  (CDP geometry, sub-cell alignment) is the real risk and is out of dimension here.
- Sources: `github.com/charmbracelet/lipgloss` (`_autodocs/compositing.md`),
  `github.com/fathyb/carbonyl` (`src/output/{painter,cell}.rs`),
  `github.com/browsh-org/browsh` (`interfacer/src/browsh/frame_builder.go`),
  `github.com/rivo/uniseg`, `github.com/mattn/go-runewidth`.

Verdict: build a single combined cell grid Nyala owns (bitmap fill plus optional
grapheme), grapheme wins the cell with fg=glyph colour and bg=averaged bitmap; use
uniseg + runewidth for widths; reserve Lipgloss layers for chrome, never the viewport.

### Recommended Half-Block Pipeline

This pipeline is the initial render objective for any terminal without Sixel or Kitty.
The target is the Carbonyl gold standard: quadrant blocks for the bitmap, real Unicode
text composited on top, and truecolour where the terminal supports it. Nyala matches it
without forking a browser by taking text geometry from CDP instead of Chromium internals.

End-to-end, in order. Each step names the pick and its score.

1. **Resize**: `golang.org/x/image/draw` (9/10). In-place scale the RGBA frame into a
   reused buffer; size the CDP viewport so one cell maps to 2x4 (quadrant) or 1x2
   (half-block) source pixels, which removes Carbonyl's Chromium-fork LoDPI hack.
2. **Glyph choice**: custom compositor (8-9/10). Quadrant (2x2) default for detail,
   half-block (`U+2584`, 1x2) floor for safety, octant (2x4) only where the terminal
   and font prove Unicode 16. Default scheme: quadrant, falling back to half-block.
3. **Colour conversion**: custom closed-form converter (9/10), ported from Carbonyl
   `xterm.rs`. Default: emit direct truecolour SGR where present; closed-form xterm-256
   (6x6x6 cube + 24-step grey ramp) as the fallback. Stage 1 detects with `colorprofile`
   (7/10) over env plus terminfo, trusting a present `COLORTERM`; no DCS round-trip (see
   `docs/STAGE-1.md`).
4. **Text overlay**: custom combined cell grid (9/10) on Browsh's indexed-grid model.
   A grapheme overrides the block; fg = glyph colour, bg = averaged bitmap behind it.
   `rivo/uniseg` + `mattn/go-runewidth` (9/10) measure grapheme width and index.
5. **Diff**: custom viewport writer (built on Carbonyl's painter, references tcell v3
   7/10 and ultraviolet 7/10). Previous-frame grid, per-cell skip, SGR coalescing,
   absolute-CUP-on-move, REP for flat runs.
6. **Output**: one reused byte buffer, one write per frame, wrapped in DEC mode 2026
   where supported, cursor hide/show on KMSCON. Drive it under Bubble Tea v2
   `WithoutRenderer` for the viewport region so it never fights the chrome renderer.

How this relates to the Tech Stack rows. It reuses the existing choices and sharpens
two. `x/image/draw`, `colorprofile` plus the closed-form RGB to xterm-256 formula, and
the layered capability probe are unchanged. The "Half-block fallback" row now states the
default glyph scheme (quadrant with a half-block floor) and the combined cell grid with
text overlay. The "Colour degradation" row now records that truecolour SGR passthrough
is the default and the closed-form xterm-256 formula is the fallback. New supporting
modules are `rivo/uniseg` and `mattn/go-runewidth` for grapheme width in the text
overlay; both already enter the tree via the Charm stack. The compositor and the
viewport diff writer are custom, owned in `internal/`, and locked with golden-frame
tests.

### Decisions (half-block)

- Default glyph density: is quadrant (2x2) worth the per-cell luma split and the full
  quadrant-glyph font dependency over plain half-block for a moving viewport? Resolved:
  quadrant default, half-block `U+2584` floor. Quadrant renders four sub-pixels per cell
  for one luma split, double the resolution of half-block; Carbonyl ships this at 60 FPS,
  so the cost is proven for a moving viewport. The only added cost is a ten-codepoint font
  dependency (`U+2596..U+259F`), covered by DejaVu and Unifont.
- Does KMSCON render the quadrant glyphs `U+2596..U+259F` (and any sextant/octant tier)
  correctly in its scalable font without inter-row background bleed? Resolved: quadrant
  default, gated by a runtime render test, with the half-block `U+2584` floor (Unifont
  quadrant fallback first). KMSCON's default freetype engine resolves a Fontconfig
  monospace font, so cell-fill coverage of `U+2596..U+259F` is environment-dependent and
  must be probed. Font-renderer block glyphs can leave inter-row background bleed (see
  microsoft/terminal #19841, kovidgoyal/kitty #9849); the probe prints a known quadrant
  glyph, captures the cell, and checks glyph presence and no bleed against a contrasting
  background. No sextant or octant tier: not in default fonts and the worst bleed
  offenders. Stage 2 spike: implement the render test and the Unifont-then-half-block
  fallback chain.
- KMSCON truecolour landed in libtsm on 2025-11-28. What is the real install base of
  truecolour-capable KMSCON in mid-2026, and does KMSCON answer the OSC 11 / DCS colour
  query fast enough to rely on, or must Nyala trust `COLORTERM=kmscon`? Needs a bare-TTY
  test. (Resolved in Stage 1 (see `docs/STAGE-1.md`): trust `COLORTERM=kmscon`. The DCS
  round-trip is a no-op on KMSCON's libtsm, so Nyala detects from env plus terminfo and
  treats a present `COLORTERM` as a trusted positive; no live colour query is sent. The
  install-base question stays open.)
- Carbonyl's single-threshold grey-vs-cube branch versus tmux's nearest-of-two: is the
  accuracy gap visible at half-block resolution on real pages, and is the extra distance
  computation per pixel affordable at target FPS? Resolved: Carbonyl closed-form
  single-threshold default (grey ramp when colour spread < 8, else the 6x6x6 cube via a
  fused mul-add dot product, no search); nearest-of-two stays an opt-in. At half-block or
  quadrant resolution the cube step is already coarse and the viewport moves, so the
  accuracy gap sits below per-cell visibility while the cost doubles in the hot loop.
  Stage 2 spike: A/B both converters on text and photo fixtures via the harness before
  promoting nearest-of-two beyond opt-in.
- On the 256-colour path, is ordered Bayer dither a net win on text-heavy pages or does
  it muddy small glyphs? Resolved: dither off by default. Dither noise muddies small
  glyphs and hurts legibility more than banding hurts text; it earns its place on photo
  gradients, not body text. Stage 2 spike: benchmark dither on/off on a text fixture (OCR
  score) and a photo fixture (banding) before enabling per-region.
- What CDP source gives accurate per-glyph pixel boxes cheaply per frame: `DOMSnapshot`
  (batched), the accessibility tree, or DOM traversal? Resolved: `DOMSnapshot.captureSnapshot`,
  captured off the first paint then on navigation, resize, and a debounced reflow rather
  than on a fixed clock or per screencast frame. It is the only candidate
  returning per-text-run bounding boxes and computed colour in a single batched call:
  `textBoxes` carries absolute box bounds with character start and length, `layout.styles`
  carries the requested `color`, and `includePaintOrder` resolves overlap. Coverage is
  whole-document and post-layout, so off-screen and clipped text are included (filter by
  viewport after). The accessibility tree has no pixel boxes; DOM traversal plus
  `getBoxModel` costs one round-trip per node and cannot keep up. Drive the capture off
  the paint and reflow triggers above so cost tracks real change. Caveats: divide bounds by the device
  pixel ratio, re-add scroll offset, treat boxes as advisory (spec calls them unstable
  across versions). Sources: CDP DOMSnapshot spec, `chromedp/cdproto/domsnapshot`, the
  browser-use `enhanced_snapshot.py` DPR-scaling pattern. See the matching Architecture
  Decision.
- Sub-cell alignment: floor versus round on `px/cellW`, and how to get a stable
  cellW/cellH (CSI 16t with a per-terminal/KMSCON fallback) so text does not drift.
  Resolved in Stage 1 (see `docs/STAGE-1.md`): the stable cellW/cellH half resolves via
  `TIOCGWINSZ`, then `CSI 16t`, then `14t`/`18t`, else 8x16 px (1:2), with a sanity bound.
  The rounding rule resolves here: floor the cell index (`floor(px/cellW)` by
  `floor(py/cellH)`) and snap the glyph to that cell's top-left, not round. Floor keeps
  text and the quadrant bitmap on one monotonic, non-overlapping partition, so no glyph
  drifts into a neighbour cell; round pushes a glyph past a cell mid-point into the next
  cell and makes the map non-monotonic across a sweep. Pinned by golden frames.
- Is bg = average-of-bitmap good enough for contrast, or does Nyala need the
  luma-distance nudge (or sampling the DOM element's own background) for thin glyphs on
  busy backgrounds? Resolved: bg = average-of-bitmap default; luma-distance nudge (or DOM
  element background sample) opt-in for thin glyphs. Averaging holds contrast in the
  common case at near-zero cost; thin glyphs on busy backgrounds are the failure mode the
  nudge targets, and it adds per-cell work plus a DOM dependency not yet wired. Stage 2
  spike: measure contrast on a thin-glyph-on-photo fixture to decide whether the nudge
  becomes default.
- Frame sync: CDP screencast and the DOM text layer arrive on different cadences. What
  ordering keeps text from floating off a stale bitmap? Resolved: tag each screencast
  bitmap with its CDP `screencastFrame` metadata (sequence/timestamp), buffer the DOM text
  layer keyed to the same frame, and composite the overlay only when its key matches the
  current bitmap; otherwise hold the last matched overlay and skip the stale text. A
  one-frame-late but self-consistent cell is correct; crisp text over a moved bitmap is
  visibly wrong. This mirrors Carbonyl's separate bitmap and text channels merged into one
  diffed cell grid. Stage 3 spike: tune the buffer depth and the sequence-versus-timestamp
  tolerance against the live screencast.
- One-writer rule under mode 2026: must the chrome and the viewport compose into one
  synchronized frame per tick, or can they bracket independently without tearing at the
  seam? Resolved: bracket independently. Chrome and viewport never share cells, so there
  is no seam to tear; Bubble Tea brackets its own text frame and Nyala wraps the graphics
  batch in its own `CSI ?2026h ... CSI ?2026l`, sequenced inside `View()` right after the
  text frame so graphics never precede the cells that reserve them. See the matching TUI
  research decision.
- Does Bubble Tea v2 forward or swallow the DCS truecolour reply? Nyala must read that
  one reply itself; verify a raw/unhandled-sequence hook exists. (Resolved in Stage 1
  (see `docs/STAGE-1.md`): moot. Nyala sends no DCS truecolour reply; colour comes from
  env plus terminfo, so there is no reply to forward or swallow.)
- Does REP (`\x1b[<n>b`) for flat-colour runs shrink output on KMSCON, foot, and basic
  emulators, or add parser risk for little gain? Resolved: emit REP for flat-colour runs
  of about 4+ identical cells, with a plain repeated-write fallback. On a bare terminal,
  gate on the Stage 1 handshake's `rep` capability probe (terminfo `rep` via XTGETTCAP). On
  a detected multiplexer the handshake is skipped, so gate by env instead. tmux, zellij, and
  herdr honour REP, so trust it under `TMUX`, `ZELLIJ`, or `HERDR_ENV`. The `TMUX` env var
  carries no version, so the gate trusts any tmux session. A very old tmux predating REP
  support does not honour the sequence, but it no-ops and under-fills the row rather than
  corrupts it, so bare `TMUX` degrades safely and needs no version probe. GNU screen does not
  support REP, so never send it under `STY`, emit literal repeated characters there. KMSCON (libtsm `case 'b'`, maintained with libtsm at
  github.com/kmscon) and foot both support REP, as do xterm, kitty, alacritty, wezterm,
  ghostty, konsole, vte, Windows Terminal 1.18+, iTerm2, and Terminal.app. A non-supporting
  terminal silently no-ops REP and under-fills the row (it does not corrupt it), so the gate
  matters, do not emit blind. The terminfo `rep` cap is an unreliable sole gate: tmux,
  zellij, herdr, konsole, and the Linux console all honour REP yet do not advertise `rep`
  reliably (tmux and zellij omit the cap; herdr answers XTGETTCAP only for a fixed allowlist
  that excludes `rep`), which is why the supported multiplexers are trusted by env, not by
  probe. Stage 2 spike: measure the byte saving and confirm the gate on KMSCON, foot, tmux,
  zellij, and herdr against the diagnostics fixtures.
- Wide glyph at the viewport right edge or a damage-rect boundary: drop, space-fill, or
  extend the rect? Resolved: never half-emit. When a 2-cell glyph would cross the viewport
  right edge or a damage-rect right boundary, write a single background-coloured space in
  the last cell instead; an interior damage boundary may extend the rect by one cell to
  keep the whole glyph in one repaint. Terminals disagree on a clipped wide glyph (wrap,
  overflow, or drop), so half-emitting is undefined and corrupts the grid, while a space is
  always exactly one cell. Width comes from `mattn/go-runewidth` over `rivo/uniseg`
  grapheme clusters, the same width the compositor placed the glyph with.
- RTL/bidi: does Nyala need bidi reordering for Arabic/Hebrew pages, or accept
  visual-order glyphs as positioned by CDP? Resolved: accept CDP visual-order positions
  for v1; do not run a bidi pass. Blink resolves bidi and reorders runs to visual order
  (line-left to line-right) during LayoutNG (ICU `ubidi_reorderVisual`), and the per-glyph
  geometry CDP exposes reflects that order, so Arabic and Hebrew glyphs arrive positioned
  left-to-right and the floor map places them directly; a second reorder would
  double-apply the algorithm and break the layout. Shaping-aware cell width and contextual
  joining fidelity are deferred and reversible. Stage 2/3 spike: check how cleanly
  joined-Arabic visual x positions bin into cells.

## Proof of Concept (rendering)

Prove the rendering options before wiring in the browser. Build a renderer harness
behind one `Frame` (RGBA bitmap + optional positioned text runs) and one `Renderer`
interface, with three implementations (quadrant/half-block, Sixel, Kitty). Feed it a
static image first; add the live Chromium screencast later. Each stage kills one risk.

| Stage | What it does | Risk it kills | Status |
|---|---|---|---|
| 0 | Load a PNG, render it via all three backends, hotkey to cycle them | The three output paths work and are swappable behind one seam | Built; see [`STAGE-0.md`](./STAGE-0.md) |
| 1 | Capability probe picks the default backend; manual override stays | Detection is correct on the real target terminals | Built; see [`STAGE-1.md`](./STAGE-1.md) |
| 2 | Composite hardcoded glyph runs over the bitmap | The crisp-text overlay, the gold-standard differentiator | Built; see [`STAGE-2.md`](./STAGE-2.md) |
| 3 | Replace the PNG with a chromedp screencast loop; the binary becomes web-first (`nyala <url>`), dropping the static-PNG viewer | The frame ack-pump loop and real FPS | Built; see [`STAGE-3.md`](./STAGE-3.md) |
| 4 | Run under Bubble Tea v2 with a URL bar and a viewport region | Coexistence: the chrome renderer does not clobber graphics | Remaining |

Pass/fail for the PoC: on a no-graphics terminal (KMSCON), the quadrant plus text-overlay
path renders a real webpage with images as blocks and text as crisp glyphs; on foot and
kitty the same `Frame` renders via Sixel and Kitty with no change above the `Renderer`
seam. Measure correctness (golden frame), bytes per frame, and FPS on each target.

Stage 0 is the image-based harness, and it is built. The record of the work done and the decisions made is in [`STAGE-0.md`](./STAGE-0.md). Stage 1 is the capability and colour detection, and it is built; its record is in [`STAGE-1.md`](./STAGE-1.md). Stage 2 is the crisp text overlay, and it is built; its record is in [`STAGE-2.md`](./STAGE-2.md). Stage 3 is the live browser integration, and it is built; its record is in [`STAGE-3.md`](./STAGE-3.md). Stage 4 remains.

## Prior Art (reference, not dependencies)

- **Carbonyl** (Chromium fork, Rust): the quality benchmark. Steal its half-block
  rendering and RGB→xterm-256 conversion. Nyala diverged on truecolour detection:
  env plus terminfo with a trusted `COLORTERM`, not Carbonyl's DCS round-trip (see
  `docs/STAGE-1.md`).
- **browsh** (Firefox + Marionette, Go): the thing we're improving on. Stagnant;
  its weakness is the Firefox dependency and fuzzy text.
- **awrit** (CEF Chromium, C++): off-screen renders the viewport and blits it via the
  Kitty graphics protocol. No half-block floor and no crisp text layer, so all text is
  whatever the Kitty bitmap shows. ~1k stars, stalled.
- **casty** (CDP screencast): streams a raw CDP screencast plus a hi-res
  `Page.captureScreenshot`, drawn via Kitty graphics. Its hybrid low-res-detect then
  hi-res-grab capture pacing is a useful note for the screencast loop. Pixel only, no
  text on cells.
- **kitweb** (Xvfb + ffmpeg): captures the page with `ffmpeg x11grab` over Xvfb and
  draws native-resolution Kitty frames. Pixel only, no text on cells.
- **termsurf** (embedded Chromium Content API): composites the page with Metal and
  draws it as an image. Pixel only, no text on cells.

The 2025-2026 trend (awrit, casty, kitweb, termsurf) is to drop the cell grid and
stream pixels over a graphics protocol. None composite crisp Unicode text on cells, so
Nyala's block-plus-text path stays the differentiator. See `BLOCK-RESEARCH.md`.