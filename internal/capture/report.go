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
	fmt.Fprintf(&b, "- Captured: %d\n\n", m.FramesCaptured)

	fmt.Fprintf(&b, "## Manifest\n\n")
	fmt.Fprintf(&b, "See [manifest.json](manifest.json) for the machine-readable record.\n\n")

	fmt.Fprintf(&b, "## Notes\n\n")

	return b.Bytes()
}
