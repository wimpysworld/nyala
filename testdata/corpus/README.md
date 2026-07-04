# Text-layout corpus

Self-contained web pages that stress text layout, used to measure how accurately
the block backend places crisp text on the terminal cell grid. Each page is one
fixture. Each fixture isolates one stressor, so a placement fault points at one
cause.

## Licence

All pages are original content under the repository's Blue Oak Model License 1.0.0.
The two `adapted` fixtures draw facts about the Ubuntu Podcast from its Wikipedia
page, but the prose and markup are written fresh for this corpus, so no third-party
text is copied. Embedded fonts are subset Noto faces under the SIL Open Font License
1.1; each fixture records its fonts in `fixture.json`.

## Fixtures

| Directory | Stressor |
|---|---|
| `layout-01-dense-prose` | Dense paragraph prose, heading levels, references, search form |
| `layout-02-rtl-bidi` | Right-to-left scripts (Arabic, Hebrew), bidi reorder with embedded LTR |
| `layout-03-cjk-vertical-ruby` | CJK horizontal and vertical writing modes, ruby annotations |
| `layout-04-multicolumn-justified` | Multi-column layout, justified text, mixed heading sizes |
| `layout-05-mixed-sizes-floor` | Font sizes from a display heading down to the 10px legibility floor |
| `layout-06-overlap-abspos` | Absolutely-positioned overlapping text, z-index, non-cell-aligned offsets |
| `layout-07-ua-default-floor` | Browser default styles, single-column prose, baseline legibility |
| `layout-08-table-infobox-form` | Data table, infobox sidebar, labelled form inputs |
| `layout-09-hidden-player` | Hidden UI that must emit no runs: `sr-only` clip, zero opacity, visibility hidden, offscreen clip and clip-path |
| `layout-10-centred-boxes` | Sub-cell-inset alignment from flex, grid, and margin-auto box centring |
| `layout-11-advance-skew` | Rightward advance skew that grows with x, sampled by right-anchored runs and a left-to-right ruler |
| `layout-12-brandmark-overlay` | Ink anchoring: a transparent brand-hue numeric badge anchored to its ink over a raster banner carrying the same hue elsewhere, plus a control label |
| `layout-13-linuxmatters` | A real-world Bootstrap-grid podcast homepage (frozen from linuxmatters.sh): fixed nav, banner, subscribe badges, a multi-column episode card grid, and pagination; tall enough to scroll, for the scroll-dance leg |

## Page rules

Every page is deterministic and offline:

- All CSS inline; fonts base64 in `@font-face`; no external URLs, no script, no
  `@import`.
- `lang` and `dir` set explicitly.
- Text sized in absolute px with explicit line-height.
- Laid out for a 960x640 viewport (the capture grid is 120 cols by 40 rows at an
  8x16 cell), so content fills the top region.

## Each fixture

- `index.html` - the self-contained page.
- `fixture.json` - metadata: `id`, `title`, `stressor`, `origin`
  (`original` or `adapted`), `source_url`, `license`, `attribution`, `fonts`,
  `self_contained`, `notes`, and an optional `placement_gate` object
  (`mean`, `max`, `drifted`, `reason`) that overrides the corpus placement gate's
  default thresholds for shaped-script fixtures whose advances cannot match the
  forced monospace cell.

## Capture and render

The `just run` harness owns the launch; do not set `FENCE_SANDBOX` or wrap in
`script` yourself. A block capture writes the source `screenshot.png` and the
auto-rasterised `block-render.png` per frame, so the two compare directly at
960x640.

```
just capture=1 run file:///abs/path/testdata/corpus/layout-01-dense-prose/index.html backend=block
```

Use `capture=1`. The capture driver waits for the page to paint, then a text-ready
latch holds the first counted frame until the text layer is folded on, so
`capture=1` lands the painted, text-bearing frame deterministically. A static file page emits few screencast
frames then idles; the gate keeps the blank pre-paint frame out, so `n>1` is no
longer needed. To re-render an existing bundle, run `just rasterise <frame-dir>`.

`just placement-score <frame-dir>` reports per-box horizontal placement error from
the captured geometry, for before/after measurement of the block renderer. It reads
the cell size from the bundle's `performance.json` (`cell_px`).

`just corpus` captures every fixture then gates each one: it runs
`placement-score -gate -thresholds <fixture.json>` against the bundle frame and
fails the fixture when mean right-edge error reaches 0.5 cells, max reaches 1.0, or
any box drifts. A fixture whose shaped-script advances cannot match the forced
monospace cell carries a `placement_gate` override in `fixture.json` (see below)
that loosens these thresholds. The sweep continues past a failure and exits
non-zero if any fixture fails.

`layout-13-linuxmatters` also runs two real-geometry legs after the placement
sweep, pinned to the owner's terminal (158x86 cells, 16x33 device px per cell via
`cell=16x33`). The first is a placement leg gated like the others. The second is a
scroll leg: it captures three frames, then scrolls 600 CSS px and settles
(`capture=3 scroll=600`), so the bundle holds consecutive snapshot generations
across a scroll. `just scroll-dance <bundle>` scores overlay re-placement over that
frame sequence: it matches each on-page run frame to frame, classifies a run that
holds its viewport cell as the page scrolls as fixed (a `position:fixed` or sticky
run, never danced), and counts every other run as danced when its scroll-compensated
document row (round((Y+scrollY)/cellH)) or its column takes a second value. The
`scroll_dance_gate` object in `fixture.json` sets the thresholds (`danced`,
`replacements`), both zero. This leg is a hard gate: it counts against the sweep
exit code. On the current build it scores danced 0, re-placements 0 of 65 matched
runs (13 fixed nav, 52 glued body).

## Committed artefacts

Resolved: the corpus commits pre-rendered reference artefacts per fixture,
regenerated deliberately, not regenerated from the HTML on every test run. The
diagnostics harness is browser-free, so it grades committed inputs, never the live
engine.

Per fixture, regenerated like `just update-golden`:

- the source bitmap PNG (the screencast frame),
- the text layer as a `.runs` file (from the captured DOM text-map),
- the golden `.txtar` grid.

`block-render.png` stays a regenerated diagnostic artefact via `just rasterise`, not a
committed golden.
