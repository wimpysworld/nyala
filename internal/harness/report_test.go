package harness

import (
	"encoding/json"
	"testing"
)

// f64 returns a pointer to a float64 literal.
func f64(v float64) *float64 { return &v }

// pint returns a pointer to an int literal.
func pint(v int) *int { return &v }

// pstr returns a pointer to a string literal.
func pstr(v string) *string { return &v }

// fullReport returns a populated block-mode report exercising every field.
func fullReport() Report {
	return Report{
		SchemaVersion: SchemaVersion,
		Page:          "wikipedia-article",
		Mode:          "block",
		Viewport:      Viewport{Cols: 120, Rows: 40},
		NyalaCommit:   "abc1234",
		Quality: Quality{
			MSSSIM:         f64(0.94),
			SSIM:           f64(0.91),
			PSNR:           f64(32.7),
			DeltaE2000Mean: f64(2.1),
			DeltaE2000P95:  f64(6.8),
			DeltaEText:     f64(1.4),
			DeltaEBitmap:   f64(2.9),
			HistDivergence: f64(0.07),
			ContrastMin:    f64(4.7),
			Sharpness:      f64(184.2),
		},
		QualityRelative: QualityRelative{
			MSSSIM:         f64(0.99),
			SSIM:           f64(0.98),
			PSNR:           f64(44.1),
			MSE:            f64(2.5),
			GMSD:           f64(0.01),
			DeltaE2000Mean: f64(0.4),
			DeltaE2000P95:  f64(1.2),
			HistDivergence: f64(0.01),
		},
		Perf: Perf{
			RenderMs:       f64(1.8),
			EncodeMs:       f64(0.6),
			BytesPerFrame:  pint(18432),
			FPS:            f64(58.0),
			AllocsPerFrame: pint(12),
			PeakRSSBytes:   pint(41943040),
		},
		CellStats: CellStats{Total: 4800, TextCells: 1320, BitmapCells: 3480, RegressedCells: 11},
		Pass:      true,
		Artefacts: Artefacts{
			Composite: "composite.png",
			Heatmap:   "diff.png",
			CellMap:   "cells.png",
			QuantMap:  pstr("quant.png"),
			Histogram: "hist.png",
		},
	}
}

// TestSchemaVersionPinned pins the schema version to 2.
func TestSchemaVersionPinned(t *testing.T) {
	if SchemaVersion != 2 {
		t.Fatalf("SchemaVersion = %d, want 2", SchemaVersion)
	}
	if got := fullReport().SchemaVersion; got != 2 {
		t.Fatalf("report schemaVersion = %d, want 2", got)
	}
}

// TestMarshalShape checks a populated report marshals to the stable flat-key
// shape.
func TestMarshalShape(t *testing.T) {
	got := marshalToMap(t, fullReport())

	want := map[string]any{
		"schemaVersion": float64(2),
		"page":          "wikipedia-article",
		"mode":          "block",
		"viewport":      map[string]any{"cols": float64(120), "rows": float64(40)},
		"nyalaCommit":   "abc1234",
		"quality": map[string]any{
			"msssim":         0.94,
			"ssim":           0.91,
			"psnr":           32.7,
			"deltaE2000Mean": 2.1,
			"deltaE2000P95":  6.8,
			"deltaEText":     1.4,
			"deltaEBitmap":   2.9,
			"histDivergence": 0.07,
			"contrastMin":    4.7,
			"sharpness":      184.2,
		},
		"qualityRelative": map[string]any{
			"msssim":         0.99,
			"ssim":           0.98,
			"psnr":           44.1,
			"mse":            2.5,
			"gmsd":           0.01,
			"deltaE2000Mean": 0.4,
			"deltaE2000P95":  1.2,
			"histDivergence": 0.01,
		},
		"perf": map[string]any{
			"renderMs":       1.8,
			"encodeMs":       0.6,
			"bytesPerFrame":  float64(18432),
			"fps":            58.0,
			"allocsPerFrame": float64(12),
			"peakRSSBytes":   float64(41943040),
		},
		"cellStats": map[string]any{
			"total":          float64(4800),
			"textCells":      float64(1320),
			"bitmapCells":    float64(3480),
			"regressedCells": float64(11),
		},
		"pass": true,
		"artefacts": map[string]any{
			"composite": "composite.png",
			"heatmap":   "diff.png",
			"cellMap":   "cells.png",
			"quantMap":  "quant.png",
			"histogram": "hist.png",
		},
	}

	assertKeys(t, "", want, got)
}

// TestNotApplicableSerialisesNull checks a not-applicable metric serialises as
// JSON null, not zero. The kitty mode emits no quant-error map, so that artefact
// is null, and an unmeasured quality metric is null too.
func TestNotApplicableSerialisesNull(t *testing.T) {
	r := fullReport()
	r.Mode = "kitty"
	r.Quality.MSSSIM = nil       // unmeasured metric
	r.Quality.DeltaEBitmap = nil // unmeasured metric
	r.Artefacts.QuantMap = nil   // kitty emits no quant map

	got := marshalToMap(t, r)
	quality := got["quality"].(map[string]any)
	artefacts := got["artefacts"].(map[string]any)

	assertNull(t, "quality.msssim", quality, "msssim")
	assertNull(t, "quality.deltaEBitmap", quality, "deltaEBitmap")
	assertNull(t, "artefacts.quantMap", artefacts, "quantMap")
}

// TestShapeUniformAcrossModes checks the top-level key set is identical across
// block, sixel, and kitty even when metrics are absent.
func TestShapeUniformAcrossModes(t *testing.T) {
	block := keySet(marshalToMap(t, fullReport()))

	sixel := fullReport()
	sixel.Mode = "sixel"
	sixel.Quality.PSNR = nil
	sixelKeys := keySet(marshalToMap(t, sixel))

	kitty := fullReport()
	kitty.Mode = "kitty"
	kitty.Quality.MSSSIM = nil
	kitty.Artefacts.QuantMap = nil
	kittyKeys := keySet(marshalToMap(t, kitty))

	for _, name := range []string{"sixel", "kitty"} {
		other := sixelKeys
		if name == "kitty" {
			other = kittyKeys
		}
		if len(other) != len(block) {
			t.Fatalf("%s key count = %d, want %d", name, len(other), len(block))
		}
		for k := range block {
			if _, ok := other[k]; !ok {
				t.Errorf("%s missing top-level key %q", name, k)
			}
		}
	}
}

// marshalToMap marshals a report and unmarshals it into a generic map so tests
// can inspect the raw JSON shape and null fields.
func marshalToMap(t *testing.T, r Report) map[string]any {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	return m
}

// keySet returns the set of top-level keys of a marshalled report.
func keySet(m map[string]any) map[string]struct{} {
	out := make(map[string]struct{}, len(m))
	for k := range m {
		out[k] = struct{}{}
	}
	return out
}

// assertNull fails if key is missing or its value is not JSON null.
func assertNull(t *testing.T, path string, m map[string]any, key string) {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Errorf("%s: key absent, want present and null", path)
		return
	}
	if v != nil {
		t.Errorf("%s = %v, want null", path, v)
	}
}

// assertKeys checks every key in want is present in got with an equal value,
// recursing into nested maps.
func assertKeys(t *testing.T, prefix string, want, got map[string]any) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: key count = %d, want %d (got %v)", prefix, len(got), len(want), keyNames(got))
	}
	for k, wv := range want {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		gv, ok := got[k]
		if !ok {
			t.Errorf("%s: missing", path)
			continue
		}
		wm, wIsMap := wv.(map[string]any)
		gm, gIsMap := gv.(map[string]any)
		if wIsMap || gIsMap {
			if !wIsMap || !gIsMap {
				t.Errorf("%s: map mismatch, want map=%v got map=%v", path, wIsMap, gIsMap)
				continue
			}
			assertKeys(t, path, wm, gm)
			continue
		}
		if gv != wv {
			t.Errorf("%s = %v, want %v", path, gv, wv)
		}
	}
}

// keyNames returns the keys for error messages, in no particular order.
func keyNames(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
