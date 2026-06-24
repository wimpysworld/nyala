package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// readPerformance writes a finalised bundle of n block frames and returns the
// decoded performance.json as a generic map for field-presence checks.
func readPerformance(t *testing.T, n int, backend Backend) map[string]any {
	t.Helper()
	meta := blockMeta(n)
	if backend != BackendBlock {
		meta = graphicsMeta(backend)
		meta.FramesRequested = n
	}
	b, err := NewBundle(meta)
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}
	for i := range n {
		in := blockFrameInput()
		if backend != BackendBlock {
			in.Grid = nil
		}
		if werr := b.WriteFrame(in); werr != nil {
			t.Fatalf("WriteFrame %d: %v", i, werr)
		}
	}
	if ferr := b.FinaliseFull(); ferr != nil {
		t.Fatalf("FinaliseFull: %v", ferr)
	}
	raw, err := os.ReadFile(filepath.Join(b.Path(), "performance.json"))
	if err != nil {
		t.Fatalf("read performance.json: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal performance.json: %v", err)
	}
	return doc
}

func TestPerformanceBlockPerFrameFields(t *testing.T) {
	doc := readPerformance(t, 1, BackendBlock)
	frames, ok := doc["frames"].([]any)
	if !ok || len(frames) != 1 {
		t.Fatalf("performance.json frames = %v, want one entry", doc["frames"])
	}
	frame := frames[0].(map[string]any)
	want := []string{
		"backend", "grid_cols_rows", "cell_px", "capture_px", "draw_px",
		"glyph_runs", "overlay_cells", "screencast_payload_bytes",
		"screencast_decode_ms", "dom_snapshot_ms", "fit_downscale_ms",
		"backend_encode_ms", "terminal_bytes", "terminal_write_ms",
		"block_compose_ms",
	}
	for _, k := range want {
		if _, ok := frame[k]; !ok {
			t.Errorf("block frame missing field %q", k)
		}
	}
	// Block must not carry sixel-only fields, nor the excluded fields.
	for _, k := range []string{"sixel_payload_bytes", "cdp_frame_wait_ms", "kitty_chunks"} {
		if _, ok := frame[k]; ok {
			t.Errorf("block frame must not carry field %q", k)
		}
	}
	if frame["backend"] != "block" {
		t.Errorf("block frame backend = %v", frame["backend"])
	}
}

func TestPerformanceSixelPerFrameFields(t *testing.T) {
	doc := readPerformance(t, 1, BackendSixel)
	frames := doc["frames"].([]any)
	frame := frames[0].(map[string]any)
	if _, ok := frame["sixel_payload_bytes"]; !ok {
		t.Errorf("sixel frame missing sixel_payload_bytes")
	}
	if _, ok := frame["block_compose_ms"]; ok {
		t.Errorf("sixel frame must not carry block_compose_ms")
	}
}

func TestPerformanceKittyOmitsExtras(t *testing.T) {
	doc := readPerformance(t, 1, BackendKitty)
	frame := doc["frames"].([]any)[0].(map[string]any)
	for _, k := range []string{"block_compose_ms", "sixel_payload_bytes", "kitty_chunks"} {
		if _, ok := frame[k]; ok {
			t.Errorf("kitty frame must not carry field %q", k)
		}
	}
}

func TestPerformanceSingleFrameNoSummary(t *testing.T) {
	doc := readPerformance(t, 1, BackendBlock)
	if _, ok := doc["summary"]; ok {
		t.Errorf("single-frame performance.json must carry no summary")
	}
}

func TestPerformanceRunSummary(t *testing.T) {
	doc := readPerformance(t, 3, BackendBlock)
	summary, ok := doc["summary"].(map[string]any)
	if !ok {
		t.Fatalf("multi-frame performance.json missing summary: %v", doc["summary"])
	}
	if summary["frames_requested"] != float64(3) {
		t.Errorf("summary frames_requested = %v, want 3", summary["frames_requested"])
	}
	if summary["frames_captured"] != float64(3) {
		t.Errorf("summary frames_captured = %v, want 3", summary["frames_captured"])
	}
	mean, ok := summary["mean"].(map[string]any)
	if !ok {
		t.Fatalf("summary missing mean: %v", summary["mean"])
	}
	max, ok := summary["max"].(map[string]any)
	if !ok {
		t.Fatalf("summary missing max: %v", summary["max"])
	}
	for _, k := range []string{
		"screencast_decode_ms", "dom_snapshot_ms", "fit_downscale_ms",
		"backend_encode_ms", "terminal_write_ms",
	} {
		if _, ok := mean[k]; !ok {
			t.Errorf("summary mean missing %q", k)
		}
		if _, ok := max[k]; !ok {
			t.Errorf("summary max missing %q", k)
		}
	}
	// No percentiles.
	for k := range summary {
		if k == "p50" || k == "p95" || k == "p99" || k == "percentiles" {
			t.Errorf("summary must carry no percentiles, found %q", k)
		}
	}
	// Identical frames: mean equals max equals the single value.
	if mean["screencast_decode_ms"] != float64(1.5) {
		t.Errorf("summary mean screencast_decode_ms = %v, want 1.5", mean["screencast_decode_ms"])
	}
	if max["dom_snapshot_ms"] != float64(2.5) {
		t.Errorf("summary max dom_snapshot_ms = %v, want 2.5", max["dom_snapshot_ms"])
	}
}
