# Terminal compatibility evidence

Terminal support is a recorded fact, never an inference. Unicode assignment,
font coverage, and another scheme's allow-list are not proof that one terminal
draws a glyph set at one-cell width with clean alignment. A support claim
needs the terminal, its version, the font path, any multiplexer path, and the
rendering mode it was observed under.

## Sextant scheme

The sextant allow-list lives in `internal/viewer/sextant.go`. It proves only
the U+1FB00..U+1FB3B sextant range on the listed terminals. It is not evidence
for any other glyph range.

## Shaded scheme

The Shaded scheme draws the Symbols for Legacy Computing medium-shade glyphs
U+1FB8C..U+1FB8F, U+1FB91, U+1FB92, and U+1FB94. No
terminal evidence is recorded yet, so no allow-list exists: the scheme starts
only from an explicit `--blocks=shaded`, and automatic selection never
chooses it.

Run `just shaded-testcard` on a candidate terminal to gather evidence. A clean
card shows every glyph drawn (no blank cell, no replacement box), every `X|`
sentinel column aligned (one-cell advance for the four-byte glyphs), and shade
runs reading as an even 50 percent tone without seams.

Record the evidence here, one row per observation:

| Terminal | Version | Font path | Multiplexer | Mode | Result |
| --- | --- | --- | --- | --- | --- |
