package main

import (
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// baseGate is the flag-default gate the tests start from, mirroring the main flag
// defaults, so a threshold override is visible against a known baseline.
func baseGate() gateConfig {
	return gateConfig{on: true, meanH: 0.5, maxH: 1.0, meanV: 0.5, maxV: 1.0}
}

// writeFile writes content to a fresh file in a temp dir and returns its path.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// TestApplyThresholds checks the paint_displace_gate override merge: present keys
// override the flag defaults, absent keys keep them, a file with no paint_displace_gate
// object changes nothing, invalid JSON is an error, and a missing file is an error.
func TestApplyThresholds(t *testing.T) {
	path := writeFile(t, "fixture.json", `{"paint_displace_gate":{"mean_abs_h":0.25,"max_abs_v":0.6}}`)
	gc := baseGate()
	if err := applyThresholds(&gc, path); err != nil {
		t.Fatalf("applyThresholds: %v", err)
	}
	if gc.meanH != 0.25 {
		t.Errorf("meanH = %v, want 0.25", gc.meanH)
	}
	if gc.maxV != 0.6 {
		t.Errorf("maxV = %v, want 0.6", gc.maxV)
	}
	if gc.maxH != 1.0 || gc.meanV != 0.5 {
		t.Errorf("absent keys changed: maxH %v meanV %v, want 1.0 and 0.5", gc.maxH, gc.meanV)
	}

	none := writeFile(t, "none.json", `{"id":"x"}`)
	gc2 := baseGate()
	if err := applyThresholds(&gc2, none); err != nil {
		t.Fatalf("applyThresholds no gate: %v", err)
	}
	if gc2 != baseGate() {
		t.Errorf("no gate object changed config: %+v", gc2)
	}

	bad := writeFile(t, "bad.json", `{`)
	if err := applyThresholds(&gc2, bad); err == nil {
		t.Error("invalid JSON: want error")
	}
	if err := applyThresholds(&gc2, filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("missing file: want error")
	}
}

// writeFrame writes a minimal frame directory: a screenshot.png with a dark glyph
// block at the given device rectangle, a text-map.json with one run at Col/Row, and a
// bundle-root performance.json with the cell size. It returns the frame directory.
func writeFrame(t *testing.T, glyphX0, glyphY0, glyphX1, glyphY1, col, row int) string {
	t.Helper()
	bundle := t.TempDir()
	frameDir := filepath.Join(bundle, "frames", "frame-000000")
	if err := os.MkdirAll(frameDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 120, 80))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = 255, 255, 255, 255
	}
	for y := glyphY0; y < glyphY1; y++ {
		for x := glyphX0; x < glyphX1; x++ {
			o := img.PixOffset(x, y)
			img.Pix[o], img.Pix[o+1], img.Pix[o+2], img.Pix[o+3] = 51, 51, 51, 255
		}
	}
	f, err := os.Create(filepath.Join(frameDir, shotName))
	if err != nil {
		t.Fatalf("create screenshot: %v", err)
	}
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("encode screenshot: %v", err)
	}
	f.Close()
	tm := `[{"Col":` + itoa(col) + `,"Row":` + itoa(row) +
		`,"Text":"hi","FG":{"R":51,"G":51,"B":51,"A":255},"X":24,"Y":48,"W":16,"H":16,"CellW":8}]`
	if err := os.WriteFile(filepath.Join(frameDir, mapName), []byte(tm), 0o644); err != nil {
		t.Fatalf("write text-map: %v", err)
	}
	perf := `{"frames":[{"cell_px":[8,16]}]}`
	if err := os.WriteFile(filepath.Join(bundle, perfName), []byte(perf), 0o644); err != nil {
		t.Fatalf("write performance: %v", err)
	}
	return frameDir
}

// itoa is a tiny int-to-string for building JSON, avoiding an fmt import for one use.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// TestRunAligned checks run scores zero displacement and passes the default gate when
// the painted ink sits at the run's DOM-snapped cell.
func TestRunAligned(t *testing.T) {
	frameDir := writeFrame(t, 24, 48, 30, 56, 3, 3)
	if err := run(frameDir, baseGate()); err != nil {
		t.Errorf("run aligned: %v", err)
	}
}

// TestRunGateBreach checks run returns an error when the painted ink sits far enough
// from the DOM cell to breach the default gate. The ink is three cells right of Col.
func TestRunGateBreach(t *testing.T) {
	frameDir := writeFrame(t, 48, 48, 54, 56, 3, 3)
	err := run(frameDir, baseGate())
	if err == nil {
		t.Fatal("run gate breach: want error")
	}
	if !strings.Contains(err.Error(), "h disp") {
		t.Errorf("error = %v, want a horizontal displacement gate breach", err)
	}
}

// TestResolveFrameDir checks the argument resolution: a frame directory is used as is,
// a text-map.json path resolves to its directory, and a missing path is an error.
func TestResolveFrameDir(t *testing.T) {
	frameDir := writeFrame(t, 24, 48, 30, 56, 3, 3)
	if got, err := resolveFrameDir(frameDir); err != nil || got != frameDir {
		t.Errorf("dir = %q, %v, want %q", got, err, frameDir)
	}
	mapPath := filepath.Join(frameDir, mapName)
	if got, err := resolveFrameDir(mapPath); err != nil || got != frameDir {
		t.Errorf("file = %q, %v, want %q", got, err, frameDir)
	}
	if _, err := resolveFrameDir(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("missing path: want error")
	}
}
