# Block modes

Nyala supports four schemes for the Block backend. The `--blocks` flag accepts `auto`, `sextant`, `quadrant`, `half`, or `shaded`.

| Scheme | Flag value | Source pixels per cell | Selection |
| --- | --- | --- | --- |
| Quadrant | `quadrant` | 2x4, reduced to a 2x2 mask | Explicit choice and fallback |
| Half-block | `half` | 1x2 | Explicit choice |
| Sextant | `sextant` | 2x3 | Used when the existing sextant check passes, otherwise Quadrant |
| Shaded | `shaded` | 2x4, scored against shade glyph candidates | Explicit choice only, experimental, low-colour only |

`auto` uses Sextant when the existing sextant check passes. It uses Quadrant otherwise. `auto` never selects Shaded. The interactive Block-mode cycle contains Half-block, Quadrant, Sextant, and, only on a session started with `--blocks=shaded` and a compatible colour mode, Shaded.

## Shaded scheme (experimental)

Shaded targets low-colour output. It samples the same 2x4 source block as Quadrant, then scores a fixed candidate set against the eight samples: the solid block glyphs (space, full, and the four half blocks) plus the Symbols for Legacy Computing rectangular medium-shade glyphs U+1FB8C..U+1FB8F, U+1FB91, U+1FB92, U+1FB94. The triangular medium shades U+1FB9C..U+1FB9F are excluded, because their half-solid texture reads as visual noise. A medium shade displays half the foreground over the background, so mixing two palette colours crosses tone gaps a solid glyph cannot.

For each candidate the compose estimates a foreground from the covered samples and a background from the uncovered samples, quantises both to the active output palette, then scores the displayed result against the samples. Monochrome scores glyph coverage against sample luma, so coverage carries tone. An exactly flat cell under a colour palette emits a full block with foreground equal to background, never shade texture.

Shaded supports `--color=256`, `--color=ansi-grey`, `--color=ansi-tonal`, and `--color=none`. `--blocks=shaded --color=truecolor` fails before the first paint. `--blocks=shaded --color=auto` degrades a truecolour resolution to `256`. <kbd>Alt</kbd>+<kbd>C</kbd> under Shaded cycles only the compatible modes and skips `truecolor`; <kbd>Alt</kbd>+<kbd>B</kbd> skips Shaded under `truecolor` and on any session not started with `--blocks=shaded`, because an explicit start is the only recorded evidence that the terminal draws the shade glyphs. Terminal support is a separate recorded fact from Unicode support: see `docs/TERMINAL-COMPATIBILITY.md` and run `just shaded-testcard` to gather evidence.

## Colour modes

The `--color` flag accepts `auto`, `truecolor`, `256`, `ansi-grey`, `ansi-tonal`, or `none`. These modes affect only the Block backend.

Colour mode precedence is an explicit non-`auto` `--color` value, then `NO_COLOR` for `auto`, then detection. Detection treats `COLORTERM=kmscon`, `COLORTERM=truecolor`, or `COLORTERM=24bit` as `truecolor`, even without a TTY. Without one of these values, a non-TTY selects `none`.

| Mode | Output | `auto` selection |
| --- | --- | --- |
| `truecolor` | Exact 24-bit foreground and background colours | A truecolour profile or recognised truecolour `COLORTERM` value |
| `256` | Canonical xterm indexes 16..255 | A 256-colour profile |
| `ansi-grey` | Fixed xterm greyscale indexes 232..255 | Never |
| `ansi-tonal` | Basic ANSI indexes 0..15 | Never |
| `none` | No foreground or background colour sequences | `NO_COLOR`, ANSI, ASCII, unknown, or non-TTY output without a recognised truecolour `COLORTERM` value |

Automatic selection maps a detected ANSI profile to `none`, and it never selects `ansi-tonal` or `ansi-grey` at startup. On a real TTY the stored capability is floored at `256`, so the cycle can raise colour on an ANSI or unknown terminal after user action. A non-TTY keeps its detected capability.

<kbd>Alt</kbd>+<kbd>C</kbd> cycles `truecolor` -> `256` -> `ansi-grey` -> `ansi-tonal` -> `none`, then wraps. The cycle skips modes above the effective capability. From an automatically selected `none` on an ANSI profile, the first forward cycle reaches `256`. This user action is separate from automatic startup. `ansi-grey` remains available through `--color=ansi-grey` or the cycle when the effective capability permits it. An explicit mode asserts support for that mode and can raise a lower detected capability. Terminal safety restrictions remain hard caps. `NO_COLOR` selects the automatic starting mode only, so the cycle can still use detected colour modes.

The `256` mode maps each RGB colour to the CIEDE2000-nearest canonical xterm entry in indexes 16..255. Both degraded modes use the Rec.601 key `77R + 150G + 29B`. The `ansi-grey` mode maps that key to the nearest fixed xterm grey in indexes 232..255. Its reference levels are 8, 18, through 238 in steps of 10. An exact midpoint selects the lighter grey.

The `ansi-tonal` mode maps Rec.601 luma to the nearest canonical xterm ANSI colour. From dark to bright, the reference index order is `0, 4, 12, 1, 5, 2, 9, 6, 13, 3, 8, 10, 14, 7, 11, 15`. An exact tie selects the darker reference entry.

Terminals can redefine ANSI indexes 0..15. The visible `ansi-tonal` order can differ from the reference xterm order. Nyala does not query or replace the user's terminal palette.

## Monochrome output

`--color=none` selects monochrome directly. With `--color=auto`, a present `NO_COLOR` key selects monochrome regardless of its value. Any explicit non-auto `--color` value overrides `NO_COLOR`.

Monochrome keeps each composed Block glyph and suppresses foreground and background colour sequences, so the page renders as a 1-bit silhouette in the terminal's default colours. The crisp text overlay stays. Kitty and Sixel ignore the mode.
