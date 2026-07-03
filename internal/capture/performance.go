package capture

// ViewerPerf carries the viewer-phase timing and counts the capture driver
// measures inline for one frame. The bundle writer is a pure serialiser: the
// driver fills this struct and hands it in, and the engine-phase fields arrive
// separately via render.Frame.Stats, so the two halves of the record stay with
// their owners. Timings are wall-clock milliseconds and are never negative.
type ViewerPerf struct {
	// GridCols and GridRows are the composed grid size in cells.
	GridCols int
	GridRows int
	// CellPxW and CellPxH are the terminal cell pixel size used for this frame.
	CellPxW int
	CellPxH int
	// CapturePxW and CapturePxH are the source capture extent in pixels.
	CapturePxW int
	CapturePxH int
	// DrawPxW and DrawPxH are the drawn pixel extent (the downscaled image size).
	DrawPxW int
	DrawPxH int
	// GlyphRuns is the number of overlay glyph runs in the frame.
	GlyphRuns int
	// OverlayCells is the number of grid cells the overlay covered.
	OverlayCells int
	// FitDownscaleMS is the time spent downscaling the source to the fit extent.
	FitDownscaleMS float64
	// BackendEncodeMS is the time spent encoding the frame in the active backend.
	BackendEncodeMS float64
	// BlockComposeMS is the time spent composing the block grid; zero off the
	// block path.
	BlockComposeMS float64
	// TerminalWriteMS is the time spent writing the frame's bytes to the terminal.
	TerminalWriteMS float64
	// TerminalBytes is the count of bytes written to the terminal for this frame,
	// taken from the driver's metrics.CountingWriter.
	TerminalBytes int64
	// SixelPayloadBytes is the sixel payload size; zero off the sixel path.
	SixelPayloadBytes int64
}

// FramePerformance is the per-frame entry serialised into performance.json. It
// folds the engine-phase fields (from render.Frame.Stats) with the viewer-phase
// fields (from ViewerPerf). The block-only and sixel-only fields use omitempty so
// each backend's record carries only its applicable extras.
type FramePerformance struct {
	// Backend is the render backend this frame drew with.
	Backend string `json:"backend"`
	// GridColsRows is the composed grid size as [cols, rows].
	GridColsRows [2]int `json:"grid_cols_rows"`
	// CellPx is the terminal cell pixel size as [w, h].
	CellPx [2]int `json:"cell_px"`
	// CapturePx is the source capture extent as [w, h].
	CapturePx [2]int `json:"capture_px"`
	// DrawPx is the drawn pixel extent as [w, h].
	DrawPx [2]int `json:"draw_px"`
	// GlyphRuns is the number of overlay glyph runs.
	GlyphRuns int `json:"glyph_runs"`
	// OverlayCells is the number of grid cells the overlay covered.
	OverlayCells int `json:"overlay_cells"`
	// ScreencastPayloadBytes is the screencast frame payload size in bytes.
	ScreencastPayloadBytes int64 `json:"screencast_payload_bytes"`
	// ScreencastDecodeMS is the screencast decode time in milliseconds.
	ScreencastDecodeMS float64 `json:"screencast_decode_ms"`
	// DOMSnapshotMS is the DOM snapshot time in milliseconds.
	DOMSnapshotMS float64 `json:"dom_snapshot_ms"`
	// FitDownscaleMS is the fit-downscale time in milliseconds.
	FitDownscaleMS float64 `json:"fit_downscale_ms"`
	// BackendEncodeMS is the backend encode time in milliseconds.
	BackendEncodeMS float64 `json:"backend_encode_ms"`
	// TerminalBytes is the bytes written to the terminal for this frame.
	TerminalBytes int64 `json:"terminal_bytes"`
	// TerminalWriteMS is the terminal write time in milliseconds.
	TerminalWriteMS float64 `json:"terminal_write_ms"`
	// LayoutScale is the block-only layout scale Z; present only when Z exceeds 1
	// (a block grid wider than the target layout width). An absent field means Z is
	// 1.
	LayoutScale *float64 `json:"layout_scale,omitempty"`
	// BlockComposeMS is the block compose time; present only for a block capture.
	BlockComposeMS *float64 `json:"block_compose_ms,omitempty"`
	// SixelPayloadBytes is the sixel payload size; present only for a sixel
	// capture.
	SixelPayloadBytes *int64 `json:"sixel_payload_bytes,omitempty"`
}

// performanceRecord folds the engine-phase Stats and the viewer-phase Perf of one
// frame into a FramePerformance. It adds block_compose_ms only for a block
// backend and sixel_payload_bytes only for a sixel backend, so each backend's
// record carries the fields the proposal lists for it and no others. A nil Stats
// (capture somehow off) leaves the engine-phase fields zero.
func performanceRecord(backend Backend, in FrameInput) FramePerformance {
	p := in.Perf
	rec := FramePerformance{
		Backend:         string(backend),
		GridColsRows:    [2]int{p.GridCols, p.GridRows},
		CellPx:          [2]int{p.CellPxW, p.CellPxH},
		CapturePx:       [2]int{p.CapturePxW, p.CapturePxH},
		DrawPx:          [2]int{p.DrawPxW, p.DrawPxH},
		GlyphRuns:       p.GlyphRuns,
		OverlayCells:    p.OverlayCells,
		FitDownscaleMS:  p.FitDownscaleMS,
		BackendEncodeMS: p.BackendEncodeMS,
		TerminalBytes:   p.TerminalBytes,
		TerminalWriteMS: p.TerminalWriteMS,
	}
	if s := in.Frame.Stats; s != nil {
		rec.ScreencastPayloadBytes = s.ScreencastPayloadBytes
		rec.ScreencastDecodeMS = s.ScreencastDecodeMS
		rec.DOMSnapshotMS = s.DOMSnapshotMS
		if s.LayoutScale > 1 {
			v := s.LayoutScale
			rec.LayoutScale = &v
		}
	}
	if backend == BackendBlock {
		v := p.BlockComposeMS
		rec.BlockComposeMS = &v
	}
	if backend == BackendSixel {
		v := p.SixelPayloadBytes
		rec.SixelPayloadBytes = &v
	}
	return rec
}

// performanceDoc is the JSON view written to performance.json. It always carries
// the per-frame records; for a multi-frame capture it adds a run summary with the
// requested and captured counts and the per-phase mean and max over captured
// frames. A single-frame capture carries no summary.
type performanceDoc struct {
	// Frames is the per-frame performance record, in capture order.
	Frames []FramePerformance `json:"frames"`
	// Summary is the run summary; present only for a multi-frame capture.
	Summary *runSummary `json:"summary,omitempty"`
}

// runSummary holds the multi-frame run aggregates: the requested and captured
// frame counts, and the per-phase mean and max over captured frames. There are no
// percentiles, per the proposal.
type runSummary struct {
	// FramesRequested is the requested frame count n.
	FramesRequested int `json:"frames_requested"`
	// FramesCaptured is the number of frames the run drew.
	FramesCaptured int `json:"frames_captured"`
	// Mean is the per-phase mean over captured frames.
	Mean phaseAggregate `json:"mean"`
	// Max is the per-phase max over captured frames.
	Max phaseAggregate `json:"max"`
}

// phaseAggregate holds one aggregate (mean or max) of each timed phase over the
// captured frames.
type phaseAggregate struct {
	ScreencastDecodeMS float64 `json:"screencast_decode_ms"`
	DOMSnapshotMS      float64 `json:"dom_snapshot_ms"`
	FitDownscaleMS     float64 `json:"fit_downscale_ms"`
	BackendEncodeMS    float64 `json:"backend_encode_ms"`
	TerminalWriteMS    float64 `json:"terminal_write_ms"`
}

// performanceDoc builds the JSON view from the accumulated per-frame records. It
// adds the run summary only when more than one frame was captured.
func (b *Bundle) performanceDoc() performanceDoc {
	doc := performanceDoc{Frames: b.perf}
	if len(b.perf) > 1 {
		s := summarise(b.meta.FramesRequested, b.perf)
		doc.Summary = &s
	}
	return doc
}

// summarise computes the run summary over the captured per-frame records: the
// requested and captured counts and the per-phase mean and max. With no records
// it returns zero aggregates.
func summarise(requested int, frames []FramePerformance) runSummary {
	s := runSummary{FramesRequested: requested, FramesCaptured: len(frames)}
	if len(frames) == 0 {
		return s
	}
	n := float64(len(frames))
	for _, f := range frames {
		s.Mean.ScreencastDecodeMS += f.ScreencastDecodeMS / n
		s.Mean.DOMSnapshotMS += f.DOMSnapshotMS / n
		s.Mean.FitDownscaleMS += f.FitDownscaleMS / n
		s.Mean.BackendEncodeMS += f.BackendEncodeMS / n
		s.Mean.TerminalWriteMS += f.TerminalWriteMS / n
		s.Max.ScreencastDecodeMS = maxFloat(s.Max.ScreencastDecodeMS, f.ScreencastDecodeMS)
		s.Max.DOMSnapshotMS = maxFloat(s.Max.DOMSnapshotMS, f.DOMSnapshotMS)
		s.Max.FitDownscaleMS = maxFloat(s.Max.FitDownscaleMS, f.FitDownscaleMS)
		s.Max.BackendEncodeMS = maxFloat(s.Max.BackendEncodeMS, f.BackendEncodeMS)
		s.Max.TerminalWriteMS = maxFloat(s.Max.TerminalWriteMS, f.TerminalWriteMS)
	}
	return s
}

// maxFloat returns the larger of a and b.
func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
