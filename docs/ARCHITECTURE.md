# Nyala Architecture

This is the durable design record: what Nyala is, how it works, and why the
load-bearing decisions went the way they did. Measurements and their history
live in [PERFORMANCE.md](./PERFORMANCE.md). Usage and terminal compatibility
live in the [README](../README.md).

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

`resolveExecutable` (`launch.go`) tries, in order: the `--browser` flag,
`NYALA_BROWSER`, `CHROME_PATH`, then a PATH lookup over `chromium`,
`chromium-browser`, `google-chrome`, and `google-chrome-stable`. It fails fast
with an error naming that order.

`newAllocator` launches through `chromedp.NewExecAllocator`. When audio is
enabled it appends `chromedp.Flag("mute-audio", false)` with a bool value; a
bool `false` emits no flag at all, which is what unmutes Chromium, while the
string `"false"` would emit `--mute-audio=false` and keep it muted. It also
sets `--autoplay-policy=no-user-gesture-required` so audible media starts
before Nyala has input. Audio plays straight to the host sound server through
the inherited environment; owner testing found A/V sync acceptable with no
Nyala-side delay, so audio sits off the frame-rate critical path.

### Sizing and layout scale

Two sizes drive every session. `surfaceSize` is `cols*cellW` by `rows*cellH`
device pixels: the captured surface. The per-backend `captureSize` is the
stream cap: the Block sub-cell sample grid or the graphics full-pixel grid.
Both capture helpers live in `internal/render/geometry.go` as free functions,
because the block package imports render, so render must not import block.
`BlockCaptureSize(scheme, cols, rows)` takes a local `BlockScheme` enum
(quadrant `cols*2 x rows*4`, half-block `cols x rows*2`, sextant
`cols*2 x rows*3`); `GraphicsCaptureSize(cols, rows, cellW, cellH)` returns
`cols*cellW x rows*cellH`.

Each backend derives a continuous device scale Z (`frame.go`). The page lays
out at `surf/Z` CSS pixels while Chromium paints the full device surface.
Block anchors 8 CSS pixels per column (`blockCellCSSW`) and graphics anchors
16 (`graphicsCellCSSW`), each held to a 1024 px layout floor so a narrow grid
keeps a desktop layout. Anchoring density per cell makes the laid-out page
identical whether a terminal reports logical-point or device-pixel cells. The
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

### Screencast loop

The frame model is push. Chromium emits `Page.screencastFrame` on compositor
damage and withholds further frames until each one is acked, so an unacked
stream stalls after a frame or two. chromedp ships no screencast helper
(upstream declined one in chromedp issue #275), so the engine runs its own
listen, decode, ack loop.

The listener acks every frame on receipt, before decode, so a slow decoder
never starves Chromium's in-flight credit. The ack never runs inline on the
`ListenTarget` event goroutine: a `chromedp.Run` there is the chromedp #920
self-deadlock, because the ack command waits for a response only the parked
event goroutine can read. Instead `ackFrame` pushes the SessionID onto an
unbounded FIFO `ackQueue`, and one `ackPump` goroutine drains it and runs
`page.ScreencastFrameAck` off the event goroutine. A failed ack loses
Chromium's credit, so the pump cancels the session and lets the supervisor
rebuild it rather than risk a duplicate ack.

Delivery is latest-wins. A bounded `rawFrameQueue` drops the oldest undecoded
frame when full, never the newest, and parallel decode workers publish only
the newest completed sequence. Block and Sixel stream JPEG quality 90 in
production; Kitty follows its transport (see Graphics backends); diagnostics
capture stays lossless PNG.

### The text walk

One in-page walk (`walk.js`, driven by `walk.go`) runs as a single
`Runtime.evaluate` returning the per-line glyph runs, the DOM scroll offset it
read at, and a layout generation in one round trip, so the three stay
internally consistent. It runs on its own goroutine (`runTextSync`), so
snapshot latency never withholds acks or stalls frame emission. The first
snapshot fires off the `firstContentfulPaint` latch, not off a screencast
frame.

Four triggers re-snapshot after that: navigation (single-shot), a resize or
backend switch (immediate), a scroll-offset change, and a debounced
`networkAlmostIdle` reflow (250 ms debounce, capped at one extra snapshot per
second). The scroll trigger is a leading throttle at `scrollCadence` (150 ms)
plus a 250 ms trailing debounce, so the overlay tracks a sustained scroll and
one final snapshot lands on settle. Published runs are sticky: when text
publishes with no new frame pending, the engine re-emits the held bitmap
recomposed with the latest text, so a static page keeps its overlay.

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
(0.05), and any per-line rect under a quarter cell high. The occlusion cull hit-tests each run's line-box centre with
`document.elementFromPoint` and drops the run when a different element covers
it with a fully opaque background, compositing backgrounds only below the
lowest common ancestor. An occluding `pointer-events: none` element bleeds
through, the safe polarity for a text browser: showing text under a decorative
overlay beats hiding text that is visible.

### Frame sync

The bitmap and the text share no frame id, so text is placed by construction,
not matched by scroll equality. The walk decodes every run at its own scroll
offset, so the runs are internally consistent even when the overlay lags the
bitmap by a frame. The pure `holdDecision` (`sync.go`) is one bounded error
hold: a successful snapshot publishes fresh runs; a failing read holds the
last-matched runs for at most `holdStalenessLimit` (250 ms) of continuous
failure, then drops to bitmap-only, so stale text never composites over a page
it no longer matches.

### Input

Scroll is one transport. `scrollKeyDelta` (`input.go`) maps arrows to 40 CSS
px steps and Page keys to one viewport height; wheel events arrive with their
own deltas. Both reach `dispatchWheel`, which advances the engine's owned
scroll offset and dispatches one `Input.dispatchMouseEvent` `mouseWheel`.
`synthesizeScrollGesture` is deliberately unused: it synthesises touch and
inherits focus-dependent defaults. Mouse events arrive as terminal cells and
convert to CSS pixels at the cell centre, divided by Z so the click lands
where the frame drew the cell. The engine owns the scroll offset as the single
source of truth and re-applies it after crash recovery.

### Crash recovery and concurrency

A supervisor goroutine (`supervise` in `recovery.go`) owns the session. It
waits on `Target.targetCrashed` or a lost connection, tears the dead session
down, and rebuilds from engine-held state: re-navigate the held URL, re-apply
the held scroll offset once the load signal lands. `Frames()` survives the
rebuild, so the consumer keeps reading. `navigateAsync` drives navigation on
its own goroutine after the frame loop is live, so startup never blocks; a
failed load renders Chromium's own error page rather than tearing the session
down.

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
into fuzzy pixels, the Browsh failure mode Nyala exists to avoid.

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

The overlay is first-writer-wins. The first run's grapheme keeps the cell;
later runs never overwrite a claimed cell. A wide grapheme's trailing cell
carries background only, and terminal writers emit nothing for it.

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
is reported uncovered. Both faces ship in the binary via `go:embed`. The
half-block floor remains the design floor for terminals whose fonts cover
neither face's block set.

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
with a foreground and background colour. Three schemes trade resolution against
colour fidelity:

| Scheme | Sub-cells | Capture size (cols x rows grid) | Role |
| --- | --- | --- | --- |
| Sextant | 2x3 | `cols*2` x `rows*3` | Sharpest; allow-listed terminals only |
| Quadrant | 2x2 | `cols*2` x `rows*4` | Default |
| Half-block | 1x2 | `cols` x `rows*2` | The floor; explicit only |

`--blocks=auto|sextant|quadrant|half` selects the scheme (`resolveScheme` in
`internal/viewer`). `auto` and `sextant` resolve to sextant when
`SextantSupported` matches the terminal against the allow-list (detailed
under Capability detection), and degrade to quadrant otherwise. The fallback ladder is sextant to quadrant only; half stays
reachable through an explicit `--blocks=half`. Fallback is silent by owner
decision. Alt+B cycles half, quadrant, sextant live; `CycleScheme` skips
sextant on a terminal off the allow-list. Capture paths resolve `auto` to
quadrant, so diagnostic bundles stay comparable across terminals.

**Quadrant.** A cell maps to a 2x4 source region. Each of the four quadrants
box-averages a 1x2 source column into one sub-cell colour. Each sub-cell
converts to Rec.601 luma (integer weights 77, 150, 29); a bit sets when the
sub-cell's luma exceeds the cell midpoint `min + (max-min)/2`. The 4-bit mask
`TL<<3 | TR<<2 | BL<<1 | BR` keys the glyph table, so one luma mask drives both
the glyph choice and the foreground/background split: the foreground is the
average of the bright sub-cells, the background the average of the dark ones.
The two flat masks (0000 and 1111) emit U+2588 with fg equal to bg.

**Half-block.** A cell maps to a 1x2 source column: the top pixel becomes the
background, the bottom pixel the foreground, and every glyph is U+2584.

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

### Colour

Truecolour SGR passthrough is the default. The 256-colour fallback converts
each cell colour through `block.XTerm256`, a generated 32 KiB lookup table: a
15-bit RGB key indexes the CIEDE2000-nearest canonical xterm colour in 16..255,
so configurable system colours 0..15 never appear. One indexed load per colour,
no search and no start-up work. The internal rendering harness measured the LUT
against a closed-form formula and a nearest-of-two comparison; the LUT
won on mean CIEDE2000, worst-cell error, and full-path p90.

`ColourMono` is a third colour mode, not a backend. It keeps the luma-selected
glyph and suppresses all colour SGR, a 1-bit silhouette on the terminal's
default colours. It lives in the viewer's SGR binding (`fgSGRFunc` and
`bgSGRFunc` return no-op appenders); the block renderer itself stays
truecolour-only.

Precedence is `--color` flag, then `NO_COLOR`, then detection
(`resolveColour`). `--color=auto|truecolor|256|none` maps an explicit choice to
ColourTrue, ColourXTerm256, or ColourMono. Under `auto`, a present `NO_COLOR`
key yields ColourMono with ANY value, including empty and "0", per
no-color.org. The flag spells `truecolor` the American way to match the
`COLORTERM` token; Go identifiers stay British.

Detection (`detectColour`) is env plus terminfo through `colorprofile.Detect`,
hermetic and injectable, with no DCS round-trip. A present `COLORTERM` of
`kmscon`, `truecolor`, or `24bit` is a trusted positive that short-circuits to
truecolour before `Detect` runs. The round-trip was dropped for three measured
failures: DECRQSS is a no-op on KMSCON's libtsm, zellij leaks the reply into
stdin, and herdr stops answering after a child sets the background. Under tmux,
detection accepts one `tmux info` exec at startup, the only reliable way to
honour tmux's truecolour override. GNU screen never sets `COLORTERM` and lacks
a Tc/RGB terminfo cap, so truecolour there is a user opt-in via `.screenrc`.

### The diff writer

Steady Block output emits a cursor home plus in-place overwrites; a full screen
clear runs only on the first draw and after a resize (`framePrefix`).
`block.GridDiff` retains the previous grid and emits only the cells whose
glyph, colours, or overlay changed, coalescing cursor moves (one CUP per
contiguous run) and SGR (one pair per same-colour run). Queued frames coalesce
latest-wins, so a burst paints once. Each frame goes out in one buffered write,
bracketed by DEC 2026 synchronised updates; a terminal without the mode ignores
the bracket, and the cursor stays hidden for the whole session.

## Capability detection

### The handshake

One write batches the whole probe (`queryBytes`): the Kitty graphics query
(`a=q`), an optional Kitty shared-memory transport query, the three cell-size
queries, the XTVERSION identity query, then DA1:

```
CSI 16t   cell size in pixels
CSI 14t   text area in pixels
CSI 18t   text area in cells
CSI > 0 q XTVERSION
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

Detection is env identity, which survives a multiplexer where glyph probing
does not (`detectMultiplexer`): tmux is `TMUX` set; zellij is `ZELLIJ` present
with any value, including "0", so the check tests presence; herdr is
`HERDR_ENV=1` plus `HERDR_PANE_ID`; screen is `STY` set. Under tmux and zellij
a reduced handshake runs with no Kitty APC probe, because both consume APC
without passthrough and the probe would vanish unanswered. Screen and herdr get
no handshake and stay Block. Kitty is never offered under a multiplexer; tmux
3.6 and later may offer Sixel, gated in Multiplexer graphics below.

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

## Graphics backends

Both graphics backends write the bitmap out of band into the full
engine-sized grid from the top left; the bitmap never enters the cell differ.
The differ touches only chrome cells, so the two paths never contend for the
same bytes.

### Kitty

The live path reuses Chromium's work. CDP delivers each screencast frame as a
base64 PNG payload, and Nyala rechunks that payload directly as Kitty
`f=100,t=d,a=T` data. There is no decode and no re-encode; the bytes Chromium
encoded are the bytes the terminal receives.

A runtime-detected POSIX shared-memory fast path sits above the direct PNG
fallback. It captures JPEG q90 instead, decodes it in Nyala, and sends raw
`f=32` RGBA over POSIX shared memory (`f=32,t=s`). Terminal-side PNG decode
dominates at full-grid sizes, so moving the decode into Nyala and handing the
terminal raw pixels is the faster trade where shm works.

Frame discipline avoids blanking:

- Frames alternate two image IDs. Nyala transmits and puts the next image,
  then deletes the previous one (`a=d,d=I,i=<id>`, the capital `I` frees the
  stored data). The old image leaves the screen only after the new one is
  placed, so no blank shows between frames.
- `q=2` suppresses terminal replies in the loop; queries run only at startup.

Placement is classic cursor-anchored (`a=T`) with `C=1` so the cursor does
not move, followed by an explicit absolute cursor position (`CSI <row>;<col> H`)
before the next frame. The default `C=0` landing position is undefined at
screen edges and differs across terminals, which is what smears a following
cell diff. Unicode placeholders (`U=1`) render only on kitty itself, so Nyala
does not use them.

### Sixel

Nyala owns its opaque Sixel encoder, derived from mattn/go-sixel v0.0.12. Its
shape:

- An adaptive 256-colour palette with dithering off, rebuilt only when a
  sampled drift test says the retained palette no longer fits the frame.
- A 64K per-frame pixel sample cap feeding the quantiser, so palette cost
  does not scale with grid size.
- RGB555 indexed colour mapping through a lookup table.
- Bounded parallel band emission for large images, right-edge plane bounds
  per colour, and an 8192 repeat ceiling per run.
- One ordered write per frame; the whole payload lands in one buffer first.

Frame discipline: home the cursor, enter DECSDM with `CSI ?80h`, write the
DCS, reset with `CSI ?80l`. DECSDM gives fixed-page, non-scrolling placement
that leaves the text cursor unchanged on current xterm, foot, and WezTerm.
DEC 2026 brackets the frame.

Sixel pixels are opaque with no alpha, so a same-size full-viewport overwrite
needs no clear. Nyala clears the region only on viewport shrink or geometry
change.

## Multiplexer graphics

Multiplexer detection and the reduced handshake live in Capability detection
above. Graphics gating under each multiplexer:

tmux Sixel is gated three ways:

- DA1 must advertise attribute 4. tmux reports it only when both its build
  and its outer terminal support Sixel, so the reply is honest.
- Real cell pixel geometry must be present, or the surface would be sized
  from the 8x16 fallback and render at the wrong physical scale.
- XTVERSION must name tmux 3.6 or later. Versions 3.4 through 3.5a compile
  in a 1 MiB DCS input cap with no option to raise it, so any busy frame over
  1 MiB is silently discarded as a black screen. Those versions get no Sixel
  at all; `--backend=sixel` fails with the standard unsupported error.

The bar's unconditional bottom-row reservation absorbs tmux's image-crop
rule; the mechanism lives in The bottom bar.

zellij Sixel is blocked outright, regardless of DA1 and geometry. Two
zellij-side defects, root-caused in zellij source, would present as Nyala
bugs: an image-id collision that drops every third full-replacement frame,
and bare re-emission of image chunks to the host that breaks page scrolling.
No Nyala-side workaround exists, so Block is the zellij backend. The block
lifts through the compatibility policy, keyed on the `Zellij(<version>)`
XTVERSION identity, once upstream fixes ship.

### tmux Sixel pacing and quality ladder

tmux re-encodes stored images for its host terminal, so sustained full-rate
Sixel overwhelms it. The viewer survives this with tmux-only machinery
(`internal/viewer/sixelbudget.go`); native terminals and other multiplexers
keep normal behaviour.

- Byte budget: 15 MiB per frame on tmux 3.7+, 1.5 MB on 3.6.x (a byte-rate
  cap on 3.6's re-encoded output stage), 900 KB fallback that stays under
  tmux's silent 1 MiB DCS discard. An over-budget frame downscales by a
  ladder factor (1.5, 2, 3, 4) and upscales back to the exact grid extent,
  trading blur for delivery; if even the floor factor does not fit, the
  smallest encode is sent anyway.
- Input-buffer raise: on a gated version Nyala runs
  `tmux set -s input-buffer-size 16777216` once at startup. Success carries
  the version-tuned budget, failure keeps the fallback; the option persists
  for the tmux server lifetime.
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
- Idle settle-to-sharp: after two idle pacing intervals a blurred final frame
  re-emits once at factor 1, so a settled page always reads crisp. The next
  motion episode resumes at the remembered pre-settle factor.
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

The chrome row is `← │ → │ ⟳ │ ⌂ │ <indicator> <url> │ [fps │] ☰`. Button
order matches the Chromium toolbar. The ☰ menu button anchors to the
bottom-right corner as a stable click target; the optional fps segment sits
immediately left of it, so toggling fps never moves the menu. The URL region
absorbs the remaining width and truncates with an ellipsis. Glyph click
targets pad to three cells.

The backend indicator sits before the URL: ⬒ U+2B12 Block half, ⬔ U+2B14
Block quadrant, ▦ U+25A6 Block sextant, ⬣ U+2B23 Sixel, ✪ U+272A Kitty. The
Block glyphs echo their scheme's subcell shape and track the live scheme.
Clicking the indicator cycles backends, mirroring Alt+R. A braille spinner
(U+2800 block) fills the slot until the first frame lands.

Glyph rules: Basic Multilingual Plane only, never U+FE0F, and no
emoji-presentation lookalikes (← U+2190, not ⬅ U+2B05). Several chosen glyphs
are East-Asian-ambiguous width, so every segment position and mouse hit span
derives from `go-runewidth` measurement; a wide rendering shifts the layout
without breaking hit-testing.

### Colour and the fps segment

The bar uses the terminal default foreground and background plus the base 16
ANSI colours only, so it follows the user's theme. Reverse video is the base
style; bold marks focus and press feedback. Fixed 240-palette entries were
rejected because they ignore the theme. The fps segment, present exactly when
`NYALA_FPS_TIMING` arms the meter, reads `↓ NN.n │ ↑ NN.n │ ≌ N`: ↓ is the
ingest rate from the engine, ↑ the rate written to the terminal, ≌ the
cumulative received total. Rates carry one decimal and read `--` when idle.

### Rendering and input

The bar folds into the same DEC 2026 sync bracket as the page frame, so both
present atomically. It repaints after any clear and draws only when the
terminal is at least two rows tall; a one-row terminal gets the page alone,
and a bar mode that can no longer draw exits rather than swallow keys as an
invisible modal.

Mouse routing is a row check: bar-row events hit-test the segment spans of
the last repaint, every other row forwards to CDP. Side buttons 8 to 11
dispatch history back and forward and never reach the page.

URL edit mode opens on Ctrl+L or a URL click. The editor takes the whole bar:
reverse video, a ❯ prompt, and a hand-rolled readline subset (Ctrl+A/E/U/K/W,
arrows, Home, End, Delete, Backspace) over a rune slice with a horizontal
viewport. Enter commits; schemeless input with a dot gains `https://`, and
only http, https, and file schemes pass, so a committed `javascript:` URL can
never execute in the page origin. Esc cancels; a lone Esc is told apart from
an escape sequence by the buffered-byte check on the reader. A focus flag
routes every key to the editor and none to CDP while editing, and bracketed
paste is enabled only in edit mode. Menu mode opens via ☰ or Alt+F (F10 is
avoided; terminals grab it) and offers About and Quit from a command registry.

Chrome-mode key bindings live in `internal/viewer/keys.go`. Rendering: Alt+R
and Alt+Shift+R cycle the backend, Alt+B and Alt+Shift+B cycle the Block
scheme, Alt+C and Alt+Shift+C cycle colour. Navigation: Alt+Left and
Alt+Right step history, Alt+Home opens the launch URL, Ctrl+R and F5 reload.
Chrome: Ctrl+L edits the URL, Alt+F opens the menu, Ctrl+Q or Ctrl+C quits.
Tab, Shift+Tab, and Enter forward to the page for focus traversal and
activation.

Home is the launch URL, permanently; there is no home-page configuration
because a shell alias does that job and the binary stays stateless. The bar
pulls `CurrentURL` from the engine's committed navigation state on each
repaint and strips control characters at the display boundary.

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
FMA, shifting results below 1e-12, so their tests skip elsewhere.

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

## Decision log

- CEF rejected: CDP already exceeds expectations (full reasons under
  Engines).
- Bubble Tea v2 dropped: PTY-ownership complexity; the viewer already owns
  the terminal.
- The DCS/OSC colour round-trip dropped: it fails on three target platforms
  (details under Colour).
- Burning glyphs into the bitmap rejected permanently: it recreates Browsh's
  fuzzy text.
- Damage tracking, dirty tiles, and partial graphics emission are out of
  scope by owner decision; reopening needs owner sign-off and
  terminal-presented measurements.
- Block screencast quality is fixed at JPEG q90; no runtime quality knob.
- Tabs are out of scope for v1; the engine seam admits child targets later.
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
