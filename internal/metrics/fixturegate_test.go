package metrics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadFixtureGatesPartialOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.json")
	raw := []byte(`{"placement_gate":{"mean":0.25,"collisions":2},"paint_displace_gate":{"max_abs_v":1.5},"scroll_dance_gate":{"danced":3}}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFixtureGates(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Placement.Mean != 0.25 || got.Placement.Max != 1 || got.Placement.Collisions != 2 {
		t.Fatalf("placement gates = %+v", got.Placement)
	}
	if got.Paint.MeanH != 0.5 || got.Paint.MaxV != 1.5 {
		t.Fatalf("paint gates = %+v", got.Paint)
	}
	if got.Scroll.Danced != 3 || got.Scroll.Replacements != 0 {
		t.Fatalf("scroll gates = %+v", got.Scroll)
	}
}

func TestFixtureGateBoundarySemantics(t *testing.T) {
	if CheckPlacementGate(PlacementStats{MeanAbsRightEdgeErr: 0.5}, DefaultFixtureGates().Placement) == nil {
		t.Fatal("placement equality must breach")
	}
	if CheckPaintGate(PaintDisplaceStats{MaxAbsV: 1}, DefaultFixtureGates().Paint) == nil {
		t.Fatal("paint equality must breach")
	}
	if CheckScrollGate(ScrollDanceStats{Runs: 1, Placements: 1}, DefaultFixtureGates().Scroll) != nil {
		t.Fatal("one initial placement is not a re-placement")
	}
}

// TestFixtureGateLegacyCompatibility compares the shared loader with an
// independent copy of the retired tools' pointer-per-field parsing contract on
// every committed fixture.
func TestFixtureGateLegacyCompatibility(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "testdata", "corpus", "layout-*", "fixture.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("discover fixtures: %v", err)
	}
	for _, path := range paths {
		got, err := LoadFixtureGates(path)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		want := legacyFixtureGates(raw)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s gates = %+v, legacy %+v", filepath.Base(filepath.Dir(path)), got, want)
		}
	}
}

func legacyFixtureGates(raw []byte) FixtureGates {
	g := DefaultFixtureGates()
	var doc struct {
		Placement *struct {
			Mean       *float64 `json:"mean"`
			Max        *float64 `json:"max"`
			Drifted    *int     `json:"drifted"`
			Collisions *int     `json:"collisions"`
			OffGrid    *int     `json:"offgrid"`
		} `json:"placement_gate"`
		Paint *struct {
			MeanH *float64 `json:"mean_abs_h"`
			MaxH  *float64 `json:"max_abs_h"`
			MeanV *float64 `json:"mean_abs_v"`
			MaxV  *float64 `json:"max_abs_v"`
		} `json:"paint_displace_gate"`
		Scroll *struct {
			Danced       *int `json:"danced"`
			Replacements *int `json:"replacements"`
		} `json:"scroll_dance_gate"`
	}
	_ = json.Unmarshal(raw, &doc)
	if p := doc.Placement; p != nil {
		if p.Mean != nil {
			g.Placement.Mean = *p.Mean
		}
		if p.Max != nil {
			g.Placement.Max = *p.Max
		}
		if p.Drifted != nil {
			g.Placement.Drifted = *p.Drifted
		}
		if p.Collisions != nil {
			g.Placement.Collisions = *p.Collisions
		}
		if p.OffGrid != nil {
			g.Placement.OffGrid = *p.OffGrid
		}
	}
	if p := doc.Paint; p != nil {
		if p.MeanH != nil {
			g.Paint.MeanH = *p.MeanH
		}
		if p.MaxH != nil {
			g.Paint.MaxH = *p.MaxH
		}
		if p.MeanV != nil {
			g.Paint.MeanV = *p.MeanV
		}
		if p.MaxV != nil {
			g.Paint.MaxV = *p.MaxV
		}
	}
	if s := doc.Scroll; s != nil {
		if s.Danced != nil {
			g.Scroll.Danced = *s.Danced
		}
		if s.Replacements != nil {
			g.Scroll.Replacements = *s.Replacements
		}
	}
	return g
}
