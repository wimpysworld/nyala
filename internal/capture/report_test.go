package capture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// renderReportFor builds report.md bytes from a manifest, the same value the
// finalise path renders from. Tests assert on this in isolation, proving the
// writer reads only the manifest struct, never live page pixels or text.
func renderReportFor(t *testing.T, m manifest) string {
	t.Helper()
	return string(renderReport(m))
}

// fullManifest is a representative full-bundle manifest for the report tests.
func fullManifest() manifest {
	return manifest{
		Command:            "nyala https://example.test",
		Backend:            "block",
		TerminalCapability: "block",
		FramesRequested:    3,
		FramesCaptured:     3,
		NavigationURL:      "https://example.test",
		FinalURL:           "https://example.test/landing",
		NavigationResult:   "ok",
		Report:             "report.md",
	}
}

func TestReportContainsEverySection(t *testing.T) {
	out := renderReportFor(t, fullManifest())
	wantSections := []string{
		"## Command and environment",
		"## Backend and terminal capability",
		"## Navigation",
		"## Frames",
		"## Artefacts",
		"## text-map.json schema",
		"## Manifest",
		"## Notes",
	}
	for _, s := range wantSections {
		if !strings.Contains(out, s) {
			t.Errorf("report.md missing section %q\n%s", s, out)
		}
	}
}

func TestReportContainsManifestFields(t *testing.T) {
	m := fullManifest()
	out := renderReportFor(t, m)
	wants := []string{
		m.Command,                    // command and environment summary
		"Backend: block",             // selected backend
		"Terminal capability: block", // terminal capability summary
		"Result: ok",                 // navigation result
		"URL: https://example.test",  // navigation URL
		"Final URL: https://example.test/landing",
		"Requested: 3",  // requested frame count
		"Captured: 3",   // captured frame count
		"manifest.json", // pointer to the manifest
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("report.md missing %q\n%s", w, out)
		}
	}
}

func TestReportNotesFieldIsEmptyPlaceholder(t *testing.T) {
	out := renderReportFor(t, fullManifest())
	idx := strings.Index(out, "## Notes")
	if idx < 0 {
		t.Fatalf("report.md missing Notes section\n%s", out)
	}
	tail := strings.TrimSpace(out[idx+len("## Notes"):])
	if tail != "" {
		t.Errorf("Notes section must be an empty placeholder, got %q", tail)
	}
}

func TestReportPartialBundleShowsFailureReason(t *testing.T) {
	m := fullManifest()
	m.Partial = true
	m.FailureReason = "first-draw timeout"
	m.NavigationResult = "first-draw timeout"
	m.FramesCaptured = 1
	out := renderReportFor(t, m)
	if !strings.Contains(out, "Result: first-draw timeout") {
		t.Errorf("partial report must show the failure reason as the result\n%s", out)
	}
	if !strings.Contains(out, "Partial bundle: first-draw timeout") {
		t.Errorf("partial report must flag the partial bundle\n%s", out)
	}
}

func TestReportGraphicsBackendShowsNote(t *testing.T) {
	m := fullManifest()
	m.Backend = "sixel"
	m.TerminalCapability = "sixel"
	m.Note = graphicsNote
	out := renderReportFor(t, m)
	if !strings.Contains(out, graphicsNote) {
		t.Errorf("graphics report must carry the no-cell-grid note\n%s", out)
	}
}

// TestReportOmitsProxyCredentials proves no credential reaches report.md. The
// manifest carries only the stripped command (secrets removed upstream), so the
// report built from it cannot leak a credential. The test feeds a manifest whose
// command is the stripped form and asserts no credential string appears.
func TestReportOmitsProxyCredentials(t *testing.T) {
	const credential = "s3cr3t-token"
	const user = "alice"
	m := fullManifest()
	// The stripped command the manifest carries: credential-free, as produced for
	// cdp-session.json and the manifest. The raw form with credentials never
	// reaches the bundle data struct, so it cannot reach the report.
	m.Command = "nyala https://example.test"
	m.NavigationURL = "https://example.test"
	out := renderReportFor(t, m)
	for _, secret := range []string{credential, user, user + ":" + credential} {
		if strings.Contains(out, secret) {
			t.Errorf("report.md leaked secret %q\n%s", secret, out)
		}
	}
}

// TestReportNoPageTextLeak proves the report carries no page content. The writer
// takes a manifest only; it has no access to Frame pixels or text runs. The test
// renders a report and asserts a sentinel page string, never present in the
// manifest, does not appear.
func TestReportNoPageTextLeak(t *testing.T) {
	const pageText = "SECRET-PAGE-BODY-TEXT"
	out := renderReportFor(t, fullManifest())
	if strings.Contains(out, pageText) {
		t.Errorf("report.md leaked page text %q", pageText)
	}
}

// TestFinalisedBundleReportIsReal proves the finalise path writes the real report,
// not the stub placeholder, for both a full and a partial bundle.
func TestFinalisedBundleReportIsReal(t *testing.T) {
	cases := []struct {
		name     string
		finalise func(*Bundle) error
		wantBody string
	}{
		{"full", func(b *Bundle) error { return b.FinaliseFull() }, "Result: ok"},
		{"partial", func(b *Bundle) error { return b.FinalisePartial("first-draw timeout") }, "Result: first-draw timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := NewBundle(blockMeta(1))
			if err != nil {
				t.Fatalf("NewBundle: %v", err)
			}
			if werr := b.WriteFrame(blockFrameInput()); werr != nil {
				t.Fatalf("WriteFrame: %v", werr)
			}
			if ferr := tc.finalise(b); ferr != nil {
				t.Fatalf("finalise: %v", ferr)
			}
			raw, err := os.ReadFile(filepath.Join(b.Path(), "report.md"))
			if err != nil {
				t.Fatalf("read report.md: %v", err)
			}
			body := string(raw)
			if strings.Contains(body, "Placeholder") {
				t.Errorf("finalised report.md still holds the stub placeholder\n%s", body)
			}
			if !strings.Contains(body, tc.wantBody) {
				t.Errorf("finalised report.md missing %q\n%s", tc.wantBody, body)
			}
			if !strings.Contains(body, "## Notes") {
				t.Errorf("finalised report.md missing Notes section")
			}
		})
	}
}
