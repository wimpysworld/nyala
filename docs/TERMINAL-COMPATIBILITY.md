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

## Quadrant scheme

The quadrant scheme draws the U+2596..U+259F quadrant glyphs, the half blocks
U+2580, U+2584, U+258C, U+2590, the full block U+2588, and space.

| Terminal | Version | Font path | Multiplexer | Mode | Result |
| --- | --- | --- | --- | --- | --- |
| KMSCON | unrecorded | default | none | Quadrant scheme | Renders correctly; owner-tested 2026-09-02 |
