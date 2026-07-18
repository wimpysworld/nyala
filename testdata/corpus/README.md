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
- Laid out for a 960x640 viewport (the base diagnostic grid is 120 cols by 40 rows at an
  8x16 cell), so content fills the top region.

## Each fixture

- `index.html` - the self-contained page.
- `fixture.json` - metadata: `id`, `title`, `stressor`, `origin`
  (`original` or `adapted`), `source_url`, `license`, `attribution`, `fonts`,
  `self_contained`, `notes`, and an optional `placement_gate` object
  (`mean`, `max`, `drifted`, `reason`) that overrides the corpus placement gate's
  default thresholds for shaped-script fixtures whose advances cannot match the
  forced monospace cell.

## Diagnostics

Run `just diagnostics` for the whole corpus. The CDP leg drives every fixture
through the normal Block engine at 120x40 with 8x16 cells, 240x48 with 8x16
cells, and 158x86 with 16x33 cells. It applies each fixture's placement and
paint thresholds and writes gitignored evidence below
`testdata/diagnostics/cdp-corpus/`.

`paint-baseline.json` approves each fixture and geometry's state, measured-run
floor, skipped-run ceiling, and horizontal and vertical displacement ceilings.
Measured floors are 75% of the initial supported Nix/amd64 observation, rounded
up. The committed ceilings include the observed bounded frame-sampling variance.
Checks are directional: more measured runs, fewer skipped runs, and lower
displacement pass. State changes and worse values require an explicit baseline
review.

`layout-13-linuxmatters` also runs a 600 CSS-pixel scroll at the real geometry.
The test scores the first advanced frame and the settled frame, so both
compose-time scroll compensation and final snapshot placement remain covered.
The fixture's `scroll_dance_gate` requires zero danced and zero replaced runs.

Use `just cdp-interop` while developing this leg. Do not launch Chromium or set
sandbox flags by hand.

## Committed artefacts

The corpus commits pre-rendered reference artefacts per fixture for the
browser-free harness. The CDP leg grades the normal live engine separately.

Per fixture, regenerated deliberately:

- the source bitmap PNG,
- the text layer as a `.runs` file,
- the golden `.txtar` grid.

Live CDP evidence is not committed.
