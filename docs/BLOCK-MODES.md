# Block modes

Nyala supports three schemes for the Block backend. The `--blocks` flag accepts `auto`, `sextant`, `quadrant`, or `shaded`.

| Scheme | Flag value | Source pixels per cell | Selection |
| --- | --- | --- | --- |
| Quadrant | `quadrant` | 2x4, reduced to a 2x2 mask | Explicit choice and fallback |
| Sextant | `sextant` | 2x3 | Used when the existing sextant check passes, otherwise Quadrant |
| Shaded | `shaded` | 2x4, scored against shade glyph candidates | Explicit choice only, experimental, low-colour only |

`auto` uses Sextant when the existing sextant check passes. It uses Quadrant otherwise. `auto` never selects Shaded. The interactive Block-mode cycle contains Quadrant, Sextant, and, whenever the current colour mode is `256` or `ansi-grey`, Shaded, on every session however it started.

## Shaded scheme (experimental)

Shaded targets low-colour output. It samples the same 2x4 source block as Quadrant, then scores a fixed candidate set against the eight samples: the solid block glyphs (space, full, and the four half blocks) plus the Symbols for Legacy Computing rectangular medium-shade glyphs U+1FB8C..U+1FB8F, U+1FB91, U+1FB92, U+1FB94. The triangular medium shades U+1FB9C..U+1FB9F are excluded, because their half-solid texture reads as visual noise. A medium shade displays half the foreground over the background, so mixing two palette colours crosses tone gaps a solid glyph cannot.

For each candidate the compose estimates a foreground from the covered samples and a background from the uncovered samples, quantises both to the active output palette, then scores the displayed result against the samples. An exactly flat cell emits a full block with foreground equal to background, never shade texture.

Shaded supports `--color=256` and `--color=ansi-grey`. `--blocks=shaded --color=truecolor` fails before the first paint. `--blocks=shaded --color=auto` degrades a truecolour resolution to `256`. <kbd>Alt</kbd>+<kbd>C</kbd> under Shaded cycles only the compatible modes and skips `truecolor`; <kbd>Alt</kbd>+<kbd>B</kbd> skips Shaded while the colour mode is `truecolor` and offers it again as soon as the mode drops to `256` or `ansi-grey`, on every session however it started. Neither toggle changes the other setting: <kbd>Alt</kbd>+<kbd>B</kbd> never changes the colour mode and <kbd>Alt</kbd>+<kbd>C</kbd> never changes the scheme. Terminal support is a separate recorded fact from Unicode support: see `docs/TERMINAL-COMPATIBILITY.md` and run `just shaded-testcard` to gather evidence.

## Colour modes

The `--color` flag accepts `auto`, `truecolor`, `256`, or `ansi-grey`. These modes affect only the Block backend.

Colour mode precedence is an explicit non-`auto` `--color` value, then `NO_COLOR` for `auto`, then detection. Detection treats `COLORTERM=kmscon`, `COLORTERM=truecolor`, or `COLORTERM=24bit` as `truecolor`, even without a TTY. Every other profile maps to `256`.

| Mode | Output | `auto` selection |
| --- | --- | --- |
| `truecolor` | Exact 24-bit foreground and background colours | A truecolour profile or recognised truecolour `COLORTERM` value |
| `256` | Canonical xterm indexes 16..255 | A 256-colour profile |
| `ansi-grey` | Fixed xterm greyscale indexes 232..255 | `NO_COLOR` |

Automatic selection maps every profile below truecolour to `256`, the floor mode: every remaining colour mode builds on 256-colour support, and every modern terminal renders indexed colour. A present `NO_COLOR` key selects `ansi-grey`; greyscale output still emits SGR colour codes. On a real TTY the stored capability is floored at `256`. A non-TTY keeps its detected capability.

<kbd>Alt</kbd>+<kbd>C</kbd> cycles `truecolor` -> `256` -> `ansi-grey`, then wraps. The cycle skips modes above the effective capability. An explicit mode asserts support for that mode and can raise a lower detected capability. Terminal safety restrictions remain hard caps. `NO_COLOR` selects the automatic starting mode only, so the cycle can still use detected colour modes.

The `256` mode maps each RGB colour to the CIEDE2000-nearest canonical xterm entry in indexes 16..255. Both degraded modes use the Rec.601 key `77R + 150G + 29B`. The `ansi-grey` mode maps that key to the nearest fixed xterm grey in indexes 232..255. Its reference levels are 8, 18, through 238 in steps of 10. An exact midpoint selects the lighter grey.

## NO_COLOR

With `--color=auto`, a present `NO_COLOR` key selects `ansi-grey` output regardless of its value. Any explicit non-auto `--color` value overrides `NO_COLOR`. Greyscale output still emits SGR colour codes; no colourless mode exists. Kitty and Sixel ignore the colour mode.
