package engine

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/wimpysworld/nyala/internal/render"
)

// fakeExecutable writes an executable file named name into a fresh temp dir and
// returns the dir and the full path. The file is mode 0o755 so exec.LookPath
// accepts it on the test PATH, keeping the lookup hermetic with no real browser.
func fakeExecutable(t *testing.T, name string) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake executable: %v", err)
	}
	return dir, path
}

func TestResolveExecutablePrecedence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH lookup semantics differ on windows")
	}
	// A fake chromium on a temp dir prepended to PATH is the lowest-precedence
	// source. Each higher source must beat it.
	pathDir, pathExec := fakeExecutable(t, "chromium")

	cases := []struct {
		name         string
		flag         string
		nyalaBrowser string
		chromePath   string
		want         string
	}{
		{"flag wins over all", "/from/flag", "/from/nyala", "/from/chrome", "/from/flag"},
		{"nyala wins over chrome and path", "", "/from/nyala", "/from/chrome", "/from/nyala"},
		{"chrome wins over path", "", "", "/from/chrome", "/from/chrome"},
		{"path lookup is the fallback", "", "", "", pathExec},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("PATH", pathDir)
			t.Setenv("NYALA_BROWSER", c.nyalaBrowser)
			t.Setenv("CHROME_PATH", c.chromePath)

			got, err := resolveExecutable(c.flag)
			if err != nil {
				t.Fatalf("resolveExecutable(%q) error: %v", c.flag, err)
			}
			if got != c.want {
				t.Errorf("resolveExecutable(%q) = %q, want %q", c.flag, got, c.want)
			}
		})
	}
}

func TestResolveExecutableNoneResolves(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH lookup semantics differ on windows")
	}
	// Empty flag, unset envs, and an empty PATH leave nothing to find, so the
	// call must report a clear error rather than return a stray path.
	t.Setenv("PATH", t.TempDir())
	t.Setenv("NYALA_BROWSER", "")
	t.Setenv("CHROME_PATH", "")

	got, err := resolveExecutable("")
	if err == nil {
		t.Fatalf("resolveExecutable(\"\") = %q, want error", got)
	}
}

func TestCaptureSizeBlock(t *testing.T) {
	const cols, rows = 80, 24
	e := &ChromiumEngine{backend: "block"}

	gotW, gotH := e.captureSize(cols, rows)
	wantW, wantH := render.BlockCaptureSize(true, cols, rows)
	if gotW != wantW || gotH != wantH {
		t.Errorf("captureSize block = (%d, %d), want (%d, %d)", gotW, gotH, wantW, wantH)
	}
	// Pin the quadrant scheme explicitly so a scheme regression is obvious.
	if wantW != 160 || wantH != 96 {
		t.Fatalf("quadrant scheme drifted: BlockCaptureSize(80x24) = (%d, %d), want (160, 96)", wantW, wantH)
	}
}

func TestCaptureSizeGraphics(t *testing.T) {
	const cols, rows = 80, 24
	const cellW, cellH = 9, 18
	for _, backend := range []string{"sixel", "kitty"} {
		t.Run(backend, func(t *testing.T) {
			e := &ChromiumEngine{backend: backend, cellW: cellW, cellH: cellH}

			gotW, gotH := e.captureSize(cols, rows)
			wantW, wantH := render.GraphicsCaptureSize(cols, rows, cellW, cellH)
			if gotW != wantW || gotH != wantH {
				t.Errorf("captureSize %s = (%d, %d), want (%d, %d)", backend, gotW, gotH, wantW, wantH)
			}
			// Graphics capture is cell-pixel: cols*cellW by rows*cellH.
			if wantW != cols*cellW || wantH != rows*cellH {
				t.Fatalf("graphics size not cell-pixel: got (%d, %d), want (%d, %d)",
					wantW, wantH, cols*cellW, rows*cellH)
			}
		})
	}
}
