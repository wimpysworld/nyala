package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wimpysworld/nyala/internal/metrics"
)

// baseGate is the flag-default gate the tests start from, mirroring the main flag
// defaults, so a threshold override is visible against a known baseline.
func baseGate() gateConfig {
	return gateConfig{on: true, danced: 0, replacements: 0}
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

// TestApplyThresholds checks the scroll_dance_gate override merge: present keys
// override the flag defaults, absent keys keep them, a file with no
// scroll_dance_gate object changes nothing, invalid JSON is an error, and a
// missing file is an error.
func TestApplyThresholds(t *testing.T) {
	tests := []struct {
		name    string
		start   gateConfig
		content string
		want    gateConfig
	}{
		{
			name:    "all keys override",
			start:   baseGate(),
			content: `{"scroll_dance_gate":{"danced":3,"replacements":5}}`,
			want:    gateConfig{on: true, danced: 3, replacements: 5},
		},
		{
			name:    "present key overrides, absent key keeps default",
			start:   baseGate(),
			content: `{"scroll_dance_gate":{"danced":7}}`,
			want:    gateConfig{on: true, danced: 7, replacements: 0},
		},
		{
			name:    "no scroll_dance_gate object changes nothing",
			start:   baseGate(),
			content: `{"other":"field"}`,
			want:    baseGate(),
		},
		{
			name:    "present zero overrides a nonzero default",
			start:   gateConfig{on: true, danced: 1, replacements: 9},
			content: `{"scroll_dance_gate":{"replacements":0}}`,
			want:    gateConfig{on: true, danced: 1, replacements: 0},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gc := tt.start
			path := writeFile(t, "fixture.json", tt.content)
			if err := applyThresholds(&gc, path); err != nil {
				t.Fatalf("applyThresholds: unexpected error %v", err)
			}
			if gc != tt.want {
				t.Errorf("gate = %+v, want %+v", gc, tt.want)
			}
		})
	}

	t.Run("missing file is an error", func(t *testing.T) {
		gc := baseGate()
		missing := filepath.Join(t.TempDir(), "absent.json")
		if err := applyThresholds(&gc, missing); err == nil {
			t.Fatal("applyThresholds: want error for missing file, got nil")
		}
	})

	t.Run("invalid JSON is an error", func(t *testing.T) {
		gc := baseGate()
		path := writeFile(t, "bad.json", "{not json")
		if err := applyThresholds(&gc, path); err == nil {
			t.Fatal("applyThresholds: want error for invalid JSON, got nil")
		}
	})
}

// TestCheckGate checks each threshold breaching produces its named message, and a
// within-gate stats set passes. The gate uses thresholds of danced 0,
// replacements 0.
func TestCheckGate(t *testing.T) {
	gc := baseGate()
	tests := []struct {
		name    string
		stats   metrics.ScrollDanceStats
		wantSub string
	}{
		{
			name:    "danced breach",
			stats:   metrics.ScrollDanceStats{Runs: 3, DancedRuns: 1, Placements: 4},
			wantSub: "danced runs",
		},
		{
			name:    "replacements breach only",
			stats:   metrics.ScrollDanceStats{Runs: 3, DancedRuns: 0, Placements: 5},
			wantSub: "re-placements",
		},
		{
			name:    "within gate passes",
			stats:   metrics.ScrollDanceStats{Runs: 3, DancedRuns: 0, Placements: 3},
			wantSub: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkGate(tt.stats, gc)
			if tt.wantSub == "" {
				if err != nil {
					t.Fatalf("checkGate: want nil, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkGate: want error containing %q, got nil", tt.wantSub)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("checkGate error = %q, want substring %q", err.Error(), tt.wantSub)
			}
		})
	}
}

// TestReadCellCSS checks the cell read: a present cell_px returns as the bucket and
// cell height, a layout_scale divides both to CSS pixels, and an absent
// performance.json falls back to 8x16.
func TestReadCellCSS(t *testing.T) {
	t.Run("present cell_px returns bucket and cell height", func(t *testing.T) {
		bundleRoot := t.TempDir()
		if err := os.WriteFile(filepath.Join(bundleRoot, perfName), []byte(`{"frames":[{"cell_px":[16,33]}]}`), 0o644); err != nil {
			t.Fatalf("write perf: %v", err)
		}
		mapPath := filepath.Join(bundleRoot, "frames", "frame-000000", mapName)
		bucket, cellH := readCellCSS(mapPath)
		if bucket != 16 || cellH != 33 {
			t.Errorf("readCellCSS = (%v, %v), want (16, 33)", bucket, cellH)
		}
	})

	t.Run("layout_scale divides to CSS pixels", func(t *testing.T) {
		bundleRoot := t.TempDir()
		if err := os.WriteFile(filepath.Join(bundleRoot, perfName), []byte(`{"frames":[{"cell_px":[16,33],"layout_scale":2}]}`), 0o644); err != nil {
			t.Fatalf("write perf: %v", err)
		}
		mapPath := filepath.Join(bundleRoot, "frames", "frame-000000", mapName)
		bucket, cellH := readCellCSS(mapPath)
		if bucket != 8 || cellH != 16.5 {
			t.Errorf("readCellCSS = (%v, %v), want (8, 16.5)", bucket, cellH)
		}
	})

	t.Run("absent performance.json falls back to 8x16", func(t *testing.T) {
		bundleRoot := t.TempDir()
		mapPath := filepath.Join(bundleRoot, "frames", "frame-000000", mapName)
		bucket, cellH := readCellCSS(mapPath)
		if bucket != fallbackBucketPx || cellH != fallbackCellH {
			t.Errorf("readCellCSS = (%v, %v), want (%v, %v)", bucket, cellH, float64(fallbackBucketPx), float64(fallbackCellH))
		}
	})
}

// TestReadScrollY checks the per-frame scroll read: a present scroll_offset_y
// returns, and an absent cdp-payload.json reads as 0.
func TestReadScrollY(t *testing.T) {
	t.Run("present scroll_offset_y returns", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "frames", "frame-000001")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, payloadName), []byte(`{"scroll_offset_y":522.5}`), 0o644); err != nil {
			t.Fatalf("write payload: %v", err)
		}
		if got := readScrollY(filepath.Join(dir, mapName)); got != 522.5 {
			t.Errorf("readScrollY = %v, want 522.5", got)
		}
	})

	t.Run("absent payload reads as 0", func(t *testing.T) {
		mapPath := filepath.Join(t.TempDir(), "frames", "frame-000001", mapName)
		if got := readScrollY(mapPath); got != 0 {
			t.Errorf("readScrollY = %v, want 0", got)
		}
	})
}

// TestResolveFrames checks the frame glob: a bundle directory with two frame text
// maps returns both in order, a directory with no frame maps is an error, and a
// missing path is an error.
func TestResolveFrames(t *testing.T) {
	t.Run("two frames returned in order", func(t *testing.T) {
		bundle := t.TempDir()
		for _, n := range []string{"frame-000001", "frame-000000"} {
			dir := filepath.Join(bundle, "frames", n)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dir, mapName), []byte("[]"), 0o644); err != nil {
				t.Fatalf("write map: %v", err)
			}
		}
		got, err := resolveFrames(bundle)
		if err != nil {
			t.Fatalf("resolveFrames: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("resolveFrames returned %d, want 2", len(got))
		}
		if !strings.Contains(got[0], "frame-000000") || !strings.Contains(got[1], "frame-000001") {
			t.Errorf("resolveFrames order = %v, want frame-000000 then frame-000001", got)
		}
	})

	t.Run("no frames is an error", func(t *testing.T) {
		empty := t.TempDir()
		if _, err := resolveFrames(empty); err == nil {
			t.Fatal("resolveFrames: want error for no frames, got nil")
		}
	})

	t.Run("missing path is an error", func(t *testing.T) {
		if _, err := resolveFrames(filepath.Join(t.TempDir(), "absent")); err == nil {
			t.Fatal("resolveFrames: want error for missing path, got nil")
		}
	})
}
