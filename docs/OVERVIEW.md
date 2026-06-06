# Terminal Browser - Project Overview & Tech Stack

## Summary

A modern, text-based web browser that renders real web pages to a TTY. Spiritual
successor to browsh, built in Go, driving headless Chromium via the Chrome
DevTools Protocol (CDP). Targets a fully terminal-native experience, including on
a bare Linux console via KMSCON.

## Goals

- Render modern web pages (HTML5, CSS, JS) to a terminal using truecolour
- Run in a real TTY with no display server present (headless Chromium needs none)
- Crisp text via a composited glyph layer, not just fuzzy half-block pixels
- Full keyboard and mouse interaction
- Single small Go binary; depends on a system-installed Chromium/Chrome
- Pluggable rendering engine behind a stable interface (see Architecture)

## Non-Goals (initial)

- Bundling the browser engine (nice-to-have, not required)
- Bitmap graphics protocols (Sixel/Kitty) - target terminals (KMSCON) lack them
- Browser-extension support
- Feature parity with a desktop browser

## Target Environment

- Primary: KMSCON (active fork `kmscon/kmscon`) in a Linux TTY. As of libtsm
  late-2025 it supports 24-bit truecolour, scalable Unicode fonts, and mouse.
- Also: any truecolour-capable terminal emulator (xterm, foot, kitty, wezterm).
- Truecolour detection MUST use a DCS query/response, not `COLORTERM`. KMSCON does
  not reliably set `COLORTERM`, so palette sniffing gives false negatives.

## Architecture (key decision)

Decouple the browser engine from everything else via an `Engine` seam. The bulk of
the codebase (rendering, input, TUI, config) is engine-agnostic and reusable.

```go
// engine/engine.go
type Engine interface {
    Navigate(url string) error
    Resize(cols, rows int)            // logical terminal cell size
    Frames() <-chan Frame             // RGBA bitmap + damage region
    TextLayer() <-chan []GlyphRun      // positioned Unicode for crisp text
    SendKey(k Key)
    SendMouse(m MouseEvent)
    Close() error
}
```

- **Engine #1: `ChromiumEngine` (chromedp).** Frames via `Page.startScreencast`;
  text layer via CDP DOM/accessibility queries. This is the only engine to build now.
- **Future engines** (e.g. WPE WebKit) implement the same interface. No other code
  changes. Do not build these yet; the seam keeps the option open.

### Rendering pipeline (engine-agnostic)

1. Receive bitmap frames + a positioned text layer from the active `Engine`.
2. Downsample bitmap to terminal resolution; render with half-block characters
   (`▄`): background = top pixel, foreground = bottom pixel (two square pixels/cell).
3. Composite the text layer over the bitmap so glyphs render as real Unicode where
   text exists, bitmap elsewhere. This is the primary quality differentiator.
4. Diff against the previous frame; emit only changed cells.

### Known constraints / risks

- **Frame rate**: `startScreencast` streams JPEG/PNG over the CDP socket. Benchmarks
  cite low FPS, but those are full-resolution; at terminal resolution the bitmap is
  tiny, so cost should be far lower. Measure early against KMSCON before optimising.
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
| Terminal I/O | `tcell` (or `bubbletea` if a richer TUI is wanted) | raw mode, resize, truecolour, mouse |
| Frame decode | `image/jpeg`, `image/png` (stdlib) | screencast frames |
| Truecolour detect | Custom DCS query/parse | not `COLORTERM` |
| Config | TBD (e.g. `koanf`/TOML) | architecture phase |

## Open Questions (for the architecture phase)

- Frame model: push (engine emits on damage) vs pull (renderer requests)? Damage
  granularity?
- Text-layer extraction: DOM traversal vs accessibility tree vs CDP `DOMSnapshot`?
  Cost and coverage trade-offs.
- How to force a low device-scale-factor so the captured bitmap maps cleanly to
  terminal cells (Carbonyl uses a 1/7 scale; via CDP this is `Emulation` overrides).
- Input mapping: terminal escape sequences (mouse, keys) → CDP `Input` events.
- Scrolling, tabs, navigation history, link hinting - scope for v1?
- Process lifecycle: launch vs attach; crash recovery; multiple pages.

## Prior Art (reference, not dependencies)

- **Carbonyl** (Chromium fork, Rust): the quality benchmark. Steal its half-block
  rendering, RGB→xterm-256 conversion, and DCS truecolour-detection techniques.
- **browsh** (Firefox + Marionette, Go): the thing we're improving on. Stagnant;
  its weakness is the Firefox dependency and fuzzy text.