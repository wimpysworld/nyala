package ocr

// Matcher is the bespoke nearest-glyph matcher. It is a Stage 2 feature and
// ships no code in Stage 0.
//
// The matcher reads text Nyala itself rasterised by matching each cell against
// Nyala's own font atlas. Its near-zero-CER strength needs crisp glyph cells,
// which only the Stage 2 text overlay provides. Stage 0 renders all ladder text
// as fuzzy block pixels, so the matcher has no valid input here and Stage 0 uses
// gosseract alone (see tesseract.go and docs/STAGE-0.md "OCR options").
//
// This stub fixes the package seam without committing an implementation. Build
// the matcher for real in Stage 2 against the crisp overlay.
type Matcher struct{}
