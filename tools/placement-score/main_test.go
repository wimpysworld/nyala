package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wimpysworld/nyala/internal/metrics"
)

// baseGate is the flag-default gate the tests start from, mirroring the main
// flag defaults, so a threshold override is visible against a known baseline.
func baseGate() gateConfig {
	return gateConfig{on: true, mean: 0.5, max: 1.0, drifted: 0, collisions: 0, offgrid: 0}
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

// TestApplyThresholds checks the placement_gate override merge: present keys
// override the flag defaults, absent keys keep them, a file with no
// placement_gate object changes nothing, invalid JSON is an error, and a missing
// file is an error.
func TestApplyThresholds(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    gateConfig
	}{
		{
			name:    "all keys override",
			content: `{"placement_gate":{"mean":0.9,"max":2.0,"drifted":3,"collisions":4,"offgrid":5}}`,
			want:    gateConfig{on: true, mean: 0.9, max: 2.0, drifted: 3, collisions: 4, offgrid: 5},
		},
		{
			name:    "present keys override, absent keys keep defaults",
			content: `{"placement_gate":{"mean":0.25,"drifted":7}}`,
			want:    gateConfig{on: true, mean: 0.25, max: 1.0, drifted: 7, collisions: 0, offgrid: 0},
		},
		{
			name:    "no placement_gate object changes nothing",
			content: `{"other":"field"}`,
			want:    baseGate(),
		},
		{
			name:    "present zero overrides a nonzero default",
			content: `{"placement_gate":{"max":0}}`,
			want:    gateConfig{on: true, mean: 0.5, max: 0, drifted: 0, collisions: 0, offgrid: 0},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gc := baseGate()
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
		if gc != baseGate() {
			t.Errorf("gate mutated on error: %+v", gc)
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

// TestReadGrid checks the grid read degrades to 0,0 rather than fatal: an absent
// terminal-model.json returns 0,0, a valid file returns its cols and rows, and
// invalid JSON returns 0,0.
func TestReadGrid(t *testing.T) {
	t.Run("absent file returns 0,0", func(t *testing.T) {
		dir := t.TempDir() // no terminal-model.json inside
		if cols, rows := readGrid(dir); cols != 0 || rows != 0 {
			t.Errorf("readGrid = %d,%d, want 0,0", cols, rows)
		}
	})

	t.Run("valid file returns cols and rows", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, modelName), []byte(`{"cols":120,"rows":40}`), 0o644); err != nil {
			t.Fatalf("write model: %v", err)
		}
		if cols, rows := readGrid(dir); cols != 120 || rows != 40 {
			t.Errorf("readGrid = %d,%d, want 120,40", cols, rows)
		}
	})

	t.Run("invalid JSON returns 0,0", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, modelName), []byte("{not json"), 0o644); err != nil {
			t.Fatalf("write model: %v", err)
		}
		if cols, rows := readGrid(dir); cols != 0 || rows != 0 {
			t.Errorf("readGrid = %d,%d, want 0,0", cols, rows)
		}
	})
}

// TestCheckGate checks each of the five terms breaching produces its named
// message, and a within-gate stats set passes. The gate uses thresholds of
// mean 0.5, max 1.0, drifted 0, collisions 0, offgrid 0.
func TestCheckGate(t *testing.T) {
	gc := baseGate()

	tests := []struct {
		name    string
		stats   metrics.PlacementStats
		wantSub string // substring the error must contain; "" means no error
	}{
		{
			name:    "mean breach",
			stats:   metrics.PlacementStats{MeanAbsRightEdgeErr: 0.5},
			wantSub: "mean right-edge err",
		},
		{
			name:    "max breach",
			stats:   metrics.PlacementStats{MaxAbsRightEdgeErr: 1.0},
			wantSub: "max right-edge err",
		},
		{
			name:    "drifted breach",
			stats:   metrics.PlacementStats{DriftedBoxes: 1},
			wantSub: "drifted boxes",
		},
		{
			name:    "collisions breach",
			stats:   metrics.PlacementStats{Collisions: 1},
			wantSub: "collisions",
		},
		{
			name:    "offgrid breach",
			stats:   metrics.PlacementStats{OffGrid: 1},
			wantSub: "off-grid boxes",
		},
		{
			name:    "within gate passes",
			stats:   metrics.PlacementStats{MeanAbsRightEdgeErr: 0.4, MaxAbsRightEdgeErr: 0.9},
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
