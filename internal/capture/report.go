package capture

import (
	"bytes"
	"fmt"
)

// renderReport builds the report.md body from the manifest view m. It reads only
// the manifest fields, never live page content, so no page text reaches the
// report. The Command field is the secret-stripped command line the manifest
// already carries, so no raw proxy env value or credential appears. The Notes
// section is an empty placeholder the user fills with their own observations. It
// returns the rendered markdown bytes.
func renderReport(m manifest) []byte {
	var b bytes.Buffer
	fence := "```"
	fmt.Fprintf(&b, "# Nyala capture report\n\n")

	fmt.Fprintf(&b, "## Command and environment\n\n")
	fmt.Fprintf(&b, "%s\n%s\n%s\n\n", fence, m.Command, fence)

	fmt.Fprintf(&b, "## Backend and terminal capability\n\n")
	fmt.Fprintf(&b, "- Backend: %s\n", m.Backend)
	fmt.Fprintf(&b, "- Terminal capability: %s\n", m.TerminalCapability)
	if m.Note != "" {
		fmt.Fprintf(&b, "- Note: %s\n", m.Note)
	}
	b.WriteByte('\n')

	fmt.Fprintf(&b, "## Navigation\n\n")
	fmt.Fprintf(&b, "- Result: %s\n", m.NavigationResult)
	fmt.Fprintf(&b, "- URL: %s\n", m.NavigationURL)
	if m.FinalURL != "" {
		fmt.Fprintf(&b, "- Final URL: %s\n", m.FinalURL)
	}
	if m.Partial {
		fmt.Fprintf(&b, "- Partial bundle: %s\n", m.FailureReason)
	}
	b.WriteByte('\n')

	fmt.Fprintf(&b, "## Frames\n\n")
	fmt.Fprintf(&b, "- Requested: %d\n", m.FramesRequested)
	fmt.Fprintf(&b, "- Captured: %d\n", m.FramesCaptured)
	if m.RequestedScrollPx > 0 {
		fmt.Fprintf(&b, "- Requested scroll: %d CSS px (last frame is the scrolled one)\n", m.RequestedScrollPx)
	}
	b.WriteByte('\n')

	fmt.Fprintf(&b, "## Artefacts\n\n")
	fmt.Fprintf(&b, "Root:\n\n")
	fmt.Fprintf(&b, "- `manifest.json` - machine-readable record.\n")
	fmt.Fprintf(&b, "- `report.md` - this file.\n")
	fmt.Fprintf(&b, "- `performance.json` - per-frame timings and counts.\n")
	fmt.Fprintf(&b, "- `terminal-stream.raw` - raw terminal bytes.\n")
	fmt.Fprintf(&b, "- `cdp-session.json` - CDP session, proxy credentials stripped.\n")
	fmt.Fprintf(&b, "- `cdp-events.ndjson` - whitelisted CDP events, no image bytes.\n\n")
	fmt.Fprintf(&b, "Per frame, under `frames/frame-NNNNNN/`:\n\n")
	fmt.Fprintf(&b, "- `screenshot.png` - source bitmap, the screencast frame, the layout ground truth.\n")
	fmt.Fprintf(&b, "- `text.txt` - the page text layer as plain text.\n")
	fmt.Fprintf(&b, "- `text-map.json` - positioned text runs, schema below.\n")
	fmt.Fprintf(&b, "- `cdp-payload.json` - viewport metadata: page scale, device size, scroll offset.\n")
	fmt.Fprintf(&b, "- `terminal-model.json` - composed block cell grid, block backend only.\n")
	fmt.Fprintf(&b, "- `block-render.png` - terminal-model rasterised to PNG beside it, block backend only.\n\n")
	fmt.Fprintf(&b, "Kitty and Sixel omit the last two, because a graphics backend has no cell grid.\n\n")

	fmt.Fprintf(&b, "## text-map.json schema\n\n")
	fmt.Fprintf(&b, "Each `text-map.json` is a JSON array of text-run objects with these fields:\n\n")
	fmt.Fprintf(&b, "| Field | Type | Meaning |\n")
	fmt.Fprintf(&b, "|---|---|---|\n")
	fmt.Fprintf(&b, "| `Col`, `Row` | int | binned start cell: `floor(X/cellW)`, `floor(Y/cellH)` |\n")
	fmt.Fprintf(&b, "| `Text` | string | the run text, packed one grapheme per cell from `Col` |\n")
	fmt.Fprintf(&b, "| `FG` | {R,G,B,A} | run colour, RGBA 0-255 |\n")
	fmt.Fprintf(&b, "| `X`, `Y` | float | box left, top in CSS pixels, on-screen (scroll subtracted) |\n")
	fmt.Fprintf(&b, "| `W`, `H` | float | box width, height in CSS pixels |\n\n")
	fmt.Fprintf(&b, "The cell is 8x16 px (also in `performance.json` `cell_px`). ")
	fmt.Fprintf(&b, "`just placement-score <frame-dir>` scores horizontal placement error from this geometry.\n\n")

	fmt.Fprintf(&b, "## Manifest\n\n")
	fmt.Fprintf(&b, "See [manifest.json](manifest.json) for the machine-readable record.\n\n")

	fmt.Fprintf(&b, "## Notes\n\n")

	return b.Bytes()
}
