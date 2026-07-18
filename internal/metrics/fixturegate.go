package metrics

import (
	"encoding/json"
	"fmt"
	"os"
)

// PlacementGate holds the corpus placement thresholds. A score breaches mean
// and max at equality, matching the retired placement-score command.
type PlacementGate struct {
	Mean       float64
	Max        float64
	Drifted    int
	Collisions int
	OffGrid    int
}

// PaintGate holds the paint-displacement correctness thresholds.
type PaintGate struct {
	MeanH float64
	MaxH  float64
	MeanV float64
	MaxV  float64
}

// ScrollGate holds the scroll-dance maximum counts.
type ScrollGate struct {
	Danced       int
	Replacements int
}

// FixtureGates is the complete threshold set loaded from one fixture.json.
type FixtureGates struct {
	Placement PlacementGate
	Paint     PaintGate
	Scroll    ScrollGate
}

// DefaultFixtureGates returns the retired tools' command-line defaults.
func DefaultFixtureGates() FixtureGates {
	return FixtureGates{
		Placement: PlacementGate{Mean: 0.5, Max: 1, Drifted: 0, Collisions: 0, OffGrid: 0},
		Paint:     PaintGate{MeanH: 0.5, MaxH: 1, MeanV: 0.5, MaxV: 1},
		Scroll:    ScrollGate{Danced: 0, Replacements: 0},
	}
}

// LoadFixtureGates reads fixture.json and applies only present override fields.
func LoadFixtureGates(path string) (FixtureGates, error) {
	gates := DefaultFixtureGates()
	raw, err := os.ReadFile(path)
	if err != nil {
		return gates, err
	}
	var doc struct {
		Placement *struct {
			Mean, Max           *float64
			Drifted, Collisions *int
			OffGrid             *int `json:"offgrid"`
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
	if err := json.Unmarshal(raw, &doc); err != nil {
		return gates, fmt.Errorf("decode %s: %w", path, err)
	}
	if p := doc.Placement; p != nil {
		if p.Mean != nil {
			gates.Placement.Mean = *p.Mean
		}
		if p.Max != nil {
			gates.Placement.Max = *p.Max
		}
		if p.Drifted != nil {
			gates.Placement.Drifted = *p.Drifted
		}
		if p.Collisions != nil {
			gates.Placement.Collisions = *p.Collisions
		}
		if p.OffGrid != nil {
			gates.Placement.OffGrid = *p.OffGrid
		}
	}
	if p := doc.Paint; p != nil {
		if p.MeanH != nil {
			gates.Paint.MeanH = *p.MeanH
		}
		if p.MaxH != nil {
			gates.Paint.MaxH = *p.MaxH
		}
		if p.MeanV != nil {
			gates.Paint.MeanV = *p.MeanV
		}
		if p.MaxV != nil {
			gates.Paint.MaxV = *p.MaxV
		}
	}
	if s := doc.Scroll; s != nil {
		if s.Danced != nil {
			gates.Scroll.Danced = *s.Danced
		}
		if s.Replacements != nil {
			gates.Scroll.Replacements = *s.Replacements
		}
	}
	return gates, nil
}

// CheckPlacementGate returns the first placement threshold breach.
func CheckPlacementGate(s PlacementStats, g PlacementGate) error {
	switch {
	case s.MeanAbsRightEdgeErr >= g.Mean:
		return fmt.Errorf("mean right-edge err %.3f >= %.3f cells", s.MeanAbsRightEdgeErr, g.Mean)
	case s.MaxAbsRightEdgeErr >= g.Max:
		return fmt.Errorf("max right-edge err %.3f >= %.3f cells", s.MaxAbsRightEdgeErr, g.Max)
	case s.DriftedBoxes > g.Drifted:
		return fmt.Errorf("drifted boxes %d > %d", s.DriftedBoxes, g.Drifted)
	case s.Collisions > g.Collisions:
		return fmt.Errorf("collisions %d > %d", s.Collisions, g.Collisions)
	case s.OffGrid > g.OffGrid:
		return fmt.Errorf("off-grid boxes %d > %d", s.OffGrid, g.OffGrid)
	}
	return nil
}

// CheckPaintGate returns the first paint-displacement threshold breach.
func CheckPaintGate(s PaintDisplaceStats, g PaintGate) error {
	switch {
	case s.MeanAbsH >= g.MeanH:
		return fmt.Errorf("mean abs h disp %.3f >= %.3f cells", s.MeanAbsH, g.MeanH)
	case s.MaxAbsH >= g.MaxH:
		return fmt.Errorf("max abs h disp %.3f >= %.3f cells", s.MaxAbsH, g.MaxH)
	case s.MeanAbsV >= g.MeanV:
		return fmt.Errorf("mean abs v disp %.3f >= %.3f cells", s.MeanAbsV, g.MeanV)
	case s.MaxAbsV >= g.MaxV:
		return fmt.Errorf("max abs v disp %.3f >= %.3f cells", s.MaxAbsV, g.MaxV)
	}
	return nil
}

// CheckScrollGate returns the first scroll-dance threshold breach.
func CheckScrollGate(s ScrollDanceStats, g ScrollGate) error {
	if s.DancedRuns > g.Danced {
		return fmt.Errorf("danced runs %d > %d", s.DancedRuns, g.Danced)
	}
	replacements := s.Placements - (s.Runs - s.FixedRuns)
	if replacements > g.Replacements {
		return fmt.Errorf("re-placements %d > %d", replacements, g.Replacements)
	}
	return nil
}
