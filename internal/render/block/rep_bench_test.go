package block

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image/color"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const (
	repMeasureEnv  = "NYALA_REP_MEASURE"
	repProcessEnv  = "NYALA_REP_PROCESS"
	repWarmups     = 4
	repSamples     = 30
	repBufferSlack = 256
	repBytesCell   = 48
)

var repThresholds = []int{3, 4, 6, 8}

type repStats struct {
	EligibleCells int `json:"eligible_cells"`
	EligibleRuns  int `json:"eligible_runs"`
	REPCount      int `json:"rep_count"`
}

// repPrototype is a test-only copy of the production truecolour diff traversal
// with REP run coalescing added. Production output never calls this type.
type repPrototype struct {
	diff   GridDiff
	minRun int
	repBuf [24]byte
}

func (p *repPrototype) emit(w io.Writer, grid Grid) (repStats, error) {
	d := &p.diff
	full := len(d.prev.Cells) == 0 || d.prev.Cols != grid.Cols || d.prev.Rows != grid.Rows
	d.lastFG, d.lastBG = d.lastFG[:0], d.lastBG[:0]
	d.fgCol, d.bgCol = color.RGBA{}, color.RGBA{}
	d.fgSet, d.bgSet = false, false
	curRow, curCol := -1, -1
	var stats repStats

	for row := range grid.Rows {
		for col := 0; col < grid.Cols; {
			c := grid.At(col, row)
			if c.Wide {
				col++
				continue
			}
			if !full && cellUnchanged(c, d.prev.At(col, row)) {
				col++
				continue
			}
			if curRow != row || curCol != col {
				if err := d.writeCUP(w, row, col); err != nil {
					return stats, err
				}
				curRow, curCol = row, col
			}

			run := 1
			if repEligible(c) {
				for nextCol := col + 1; nextCol < grid.Cols; nextCol++ {
					next := grid.At(nextCol, row)
					if next.Wide || (!full && cellUnchanged(next, d.prev.At(nextCol, row))) || !sameREPCell(c, next) {
						break
					}
					run++
				}
				if run > 1 {
					stats.EligibleCells += run
					stats.EligibleRuns++
				}
			}

			if err := d.writeTrueColour(w, c); err != nil {
				return stats, err
			}
			if err := d.writeCellGlyph(w, c); err != nil {
				return stats, err
			}
			if run >= p.minRun && repSavesBytes(c.Glyph, run) {
				if err := p.writeREP(w, run-1); err != nil {
					return stats, err
				}
				stats.REPCount++
				curCol += run
				col += run
				continue
			}
			if run > 1 {
				for offset := 1; offset < run; offset++ {
					if err := d.writeCellGlyph(w, grid.At(col+offset, row)); err != nil {
						return stats, err
					}
				}
				curCol += run
				col += run
				continue
			}

			adv := cellAdvance(c, grid, col, row)
			curCol += adv
			if overlayWidthUntrusted(c, adv) {
				curCol = -1
			}
			col++
		}
	}

	d.retainPrev(grid)
	return stats, nil
}

func (p *repPrototype) writeREP(w io.Writer, count int) error {
	b := p.repBuf[:0]
	b = append(b, '\x1b', '[')
	b = appendInt(b, count)
	b = append(b, 'b')
	_, err := w.Write(b)
	return err
}

func repEligible(c *Cell) bool {
	return c.Text == "" && !c.Wide && utf8.RuneLen(c.Glyph) > 0
}

func sameREPCell(a, b *Cell) bool {
	return repEligible(b) && a.Glyph == b.Glyph && a.FG == b.FG && a.BG == b.BG
}

func repSavesBytes(glyph rune, run int) bool {
	repeated := run - 1
	literalBytes := repeated * utf8.RuneLen(glyph)
	repBytes := 3 + decimalDigits(repeated)
	return literalBytes > repBytes
}

func decimalDigits(v int) int {
	digits := 1
	for v >= 10 {
		v /= 10
		digits++
	}
	return digits
}

func TestREPPrototypeByteBreakEven(t *testing.T) {
	for _, tc := range []struct {
		name  string
		glyph rune
		run   int
		want  bool
	}{
		{"block-two", '▄', 2, false},
		{"block-three", '▄', 3, true},
		{"ascii-four", 'x', 4, false},
		{"ascii-five", 'x', 5, false},
		{"ascii-six", 'x', 6, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := repSavesBytes(tc.glyph, tc.run); got != tc.want {
				t.Fatalf("repSavesBytes(%q, %d) = %v, want %v", tc.glyph, tc.run, got, tc.want)
			}
		})
	}
}

// TestREPPrototypeLogicalEquivalence compares literal and REP streams through the
// existing VT model across the boundaries the prototype is forbidden to cross.
func TestREPPrototypeLogicalEquivalence(t *testing.T) {
	base := gridOf(12, 3, Cell{
		Glyph: '▄',
		FG:    color.RGBA{R: 30, G: 60, B: 90, A: 0xff},
		BG:    color.RGBA{R: 12, G: 24, B: 36, A: 0xff},
	})
	frames := []Grid{cloneDiffGrid(base)}

	oddAndRowEnd := cloneDiffGrid(base)
	for _, col := range []int{0, 1, 2, 7, 8, 9, 10, 11} {
		oddAndRowEnd.At(col, 0).Glyph = '▀'
	}
	frames = append(frames, oddAndRowEnd)

	colours := cloneDiffGrid(oddAndRowEnd)
	for col := range colours.Cols {
		colours.At(col, 1).Glyph = '█'
		if col >= 5 {
			colours.At(col, 1).FG.R++
		}
	}
	frames = append(frames, colours)

	overlayWide := cloneDiffGrid(colours)
	overlayWide.At(0, 2).Text = "A"
	overlayWide.At(1, 2).Text = "界"
	overlayWide.At(2, 2).Wide = true
	overlayWide.At(3, 2).Text = "é"
	for col := 4; col < overlayWide.Cols; col++ {
		overlayWide.At(col, 2).Glyph = '▓'
	}
	frames = append(frames, overlayWide)

	gap := cloneDiffGrid(overlayWide)
	for _, col := range []int{0, 1, 2, 4, 5, 6} {
		gap.At(col, 0).Glyph = '▌'
	}
	frames = append(frames, gap)

	var literal GridDiff
	rep := repPrototype{minRun: 3}
	var literalStream, repStream bytes.Buffer
	for i, frame := range frames {
		before := cloneDiffGrid(frame)
		var literalFrame, repFrame bytes.Buffer
		if err := literal.EmitTrueColour(&literalFrame, frame); err != nil {
			t.Fatalf("frame %d literal: %v", i, err)
		}
		if _, err := rep.emit(&repFrame, frame); err != nil {
			t.Fatalf("frame %d REP: %v", i, err)
		}
		if !equalGrid(frame, before) {
			t.Fatalf("frame %d source grid changed", i)
		}
		literalStream.Write(literalFrame.Bytes())
		repStream.Write(repFrame.Bytes())
		expanded, err := expandREP(repStream.Bytes())
		if err != nil {
			t.Fatalf("frame %d expand REP: %v", i, err)
		}
		assertVTEquivalent(t, i, expanded, literalStream.Bytes())
	}
	if !bytes.Contains(repStream.Bytes(), []byte("\x1b[2b")) {
		t.Fatal("REP stream did not encode the exact three-cell Block break-even run")
	}
}

// expandREP is the narrow logical-terminal decoder for the prototype tests. It
// expands CSI Ps b into Ps copies of the preceding graphic character and leaves
// every CUP and SGR byte untouched for the existing VT model to validate.
func expandREP(stream []byte) ([]byte, error) {
	out := make([]byte, 0, len(stream))
	var previous []byte
	for len(stream) > 0 {
		if stream[0] == '\x1b' && len(stream) >= 2 && stream[1] == '[' {
			end := 2
			for end < len(stream) && (stream[end] < 0x40 || stream[end] > 0x7e) {
				end++
			}
			if end == len(stream) {
				return nil, fmt.Errorf("unterminated CSI")
			}
			seq := stream[:end+1]
			if stream[end] == 'b' {
				if len(previous) == 0 {
					return nil, fmt.Errorf("REP without preceding graphic character")
				}
				count, err := strconv.Atoi(string(stream[2:end]))
				if err != nil || count <= 0 {
					return nil, fmt.Errorf("invalid REP count %q", stream[2:end])
				}
				for range count {
					out = append(out, previous...)
				}
			} else {
				out = append(out, seq...)
			}
			stream = stream[end+1:]
			continue
		}
		r, size := utf8.DecodeRune(stream)
		if r == utf8.RuneError && size == 1 {
			return nil, fmt.Errorf("invalid UTF-8 graphic byte 0x%02x", stream[0])
		}
		previous = append(previous[:0], stream[:size]...)
		out = append(out, stream[:size]...)
		stream = stream[size:]
	}
	return out, nil
}

func equalGrid(a, b Grid) bool {
	return a.Cols == b.Cols && a.Rows == b.Rows && slices.Equal(a.Cells, b.Cells)
}

type repGeometry struct {
	name       string
	cols, rows int
}

var repGeometries = []repGeometry{
	{"1920x1080-192x54", 192, 54},
	{"2560x1600-256x80", 256, 80},
	{"2560x2880-256x144", 256, 144},
}

var repWorkloads = []string{"flat", "web", "photo", "scroll", "motion"}

func repFrames(workload string, cols, rows int) [2]Grid {
	var frames [2]Grid
	for phase := range 2 {
		g := Grid{Cols: cols, Rows: rows, Cells: make([]Cell, cols*rows)}
		for row := range rows {
			for col := range cols {
				g.Cells[row*cols+col] = repFixtureCell(workload, col, row, phase)
			}
		}
		frames[phase] = g
	}
	return frames
}

func repFixtureCell(workload string, col, row, phase int) Cell {
	opaque := uint8(0xff)
	switch workload {
	case "flat":
		return Cell{
			Glyph: []rune{'▄', '▀'}[phase],
			FG:    color.RGBA{R: [2]uint8{40, 41}[phase], G: 90, B: 140, A: opaque},
			BG:    color.RGBA{R: 12, G: [2]uint8{24, 25}[phase], B: 36, A: opaque},
		}
	case "web":
		span := 3 + row%6
		period := span + 2
		section := col / period
		v := uint8((row*11 + section*29 + phase*7) & 0xff)
		c := Cell{
			Glyph: []rune{'▄', '▀'}[(section+phase)&1],
			FG:    color.RGBA{R: v, G: v ^ 0x33, B: v + 21, A: opaque},
			BG:    color.RGBA{R: v / 3, G: v / 2, B: v / 4, A: opaque},
		}
		if col%period >= span {
			c.Text = string(rune('A' + (row+col+phase)%26))
		}
		return c
	case "photo":
		v := (col*73 ^ row*193 ^ phase*83) & 0xff
		// #nosec G115 -- v is masked to the range of one byte above.
		vb := uint8(v)
		return Cell{
			Glyph: []rune{'▄', '▀', '▌', '▐'}[v&3],
			FG:    color.RGBA{R: vb, G: vb*29 + 17, B: vb ^ 0xa5, A: opaque},
			BG:    color.RGBA{R: vb*13 + 3, G: vb ^ 0x5a, B: vb*7 + 11, A: opaque},
		}
	case "scroll":
		line := row + phase
		v := uint8(line * 17 & 0xff)
		return Cell{
			Glyph: []rune{'▄', '▀'}[line&1],
			FG:    color.RGBA{R: v, G: v + 30, B: v ^ 0x55, A: opaque},
			BG:    color.RGBA{R: v / 4, G: v / 5, B: v / 3, A: opaque},
		}
	case "motion":
		v := uint8((col + row + phase) * 7 & 0xff)
		return Cell{
			Glyph: '█',
			FG:    color.RGBA{R: v, G: v ^ 0x55, B: v + 17, A: opaque},
			BG:    color.RGBA{R: v + 3, G: v, B: v ^ 0x22, A: opaque},
		}
	default:
		panic("unknown REP workload " + workload)
	}
}

type repArmResult struct {
	Process        int     `json:"process"`
	Geometry       string  `json:"geometry"`
	Workload       string  `json:"workload"`
	Arm            string  `json:"arm"`
	Threshold      int     `json:"threshold"`
	MedianNS       float64 `json:"median_ns"`
	P90NS          float64 `json:"p90_ns"`
	BytesPerFrame  float64 `json:"bytes_per_frame"`
	EligibleCells  float64 `json:"eligible_cells"`
	EligibleRuns   float64 `json:"eligible_runs"`
	REPCount       float64 `json:"rep_count"`
	AllocsPerFrame float64 `json:"allocs_per_frame"`
	MaxPayload     int     `json:"max_payload"`
	BufferBudget   int     `json:"buffer_budget"`
}

type repArm struct {
	literal bool
	diff    GridDiff
	rep     repPrototype
	out     bytes.Buffer
	frame   int
}

func newREPArm(literal bool, threshold, budget int, frames [2]Grid) *repArm {
	a := &repArm{literal: literal, rep: repPrototype{minRun: threshold}}
	a.out.Grow(budget)
	if literal {
		_ = a.diff.EmitTrueColour(io.Discard, frames[0])
	} else {
		_, _ = a.rep.emit(io.Discard, frames[0])
	}
	return a
}

func (a *repArm) emit(frames [2]Grid) (int, repStats, error) {
	a.frame++
	a.out.Reset()
	if a.literal {
		err := a.diff.EmitTrueColour(&a.out, frames[a.frame&1])
		return a.out.Len(), repStats{}, err
	}
	stats, err := a.rep.emit(&a.out, frames[a.frame&1])
	return a.out.Len(), stats, err
}

func measureREPArm(t *testing.T, process int, geometry repGeometry, workload string, threshold int, literal bool) repArmResult {
	t.Helper()
	frames := repFrames(workload, geometry.cols, geometry.rows)
	budget := geometry.cols*geometry.rows*repBytesCell + repBufferSlack
	a := newREPArm(literal, threshold, budget, frames)
	for range repWarmups {
		if _, _, err := a.emit(frames); err != nil {
			t.Fatal(err)
		}
	}

	times := make([]float64, 0, repSamples)
	var bytesTotal, eligibleCells, eligibleRuns, repCount, maxPayload int
	for range repSamples {
		start := time.Now()
		payload, stats, err := a.emit(frames)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		times = append(times, float64(elapsed.Nanoseconds()))
		bytesTotal += payload
		eligibleCells += stats.EligibleCells
		eligibleRuns += stats.EligibleRuns
		repCount += stats.REPCount
		maxPayload = max(maxPayload, payload)
	}
	slices.Sort(times)

	allocArm := newREPArm(literal, threshold, budget, frames)
	var allocErr error
	allocs := testing.AllocsPerRun(10, func() {
		_, _, allocErr = allocArm.emit(frames)
	})
	if allocErr != nil {
		t.Fatal(allocErr)
	}
	armName := "rep"
	if literal {
		armName = "literal"
	}
	return repArmResult{
		Process:        process,
		Geometry:       geometry.name,
		Workload:       workload,
		Arm:            armName,
		Threshold:      threshold,
		MedianNS:       quantile(times, 0.5),
		P90NS:          quantile(times, 0.9),
		BytesPerFrame:  float64(bytesTotal) / repSamples,
		EligibleCells:  float64(eligibleCells) / repSamples,
		EligibleRuns:   float64(eligibleRuns) / repSamples,
		REPCount:       float64(repCount) / repSamples,
		AllocsPerFrame: allocs,
		MaxPayload:     maxPayload,
		BufferBudget:   budget,
	}
}

func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	index := int(math.Ceil(q*float64(len(sorted)))) - 1
	return sorted[max(0, min(index, len(sorted)-1))]
}

// TestREPMeasurement is opt-in because it creates the retained performance
// evidence. Each invocation is one fresh process with rotated arm order.
func TestREPMeasurement(t *testing.T) {
	if os.Getenv(repMeasureEnv) != "1" {
		t.Skip("set " + repMeasureEnv + "=1 to run REP measurements")
	}
	process, err := strconv.Atoi(os.Getenv(repProcessEnv))
	if err != nil || process < 0 {
		t.Fatalf("%s must be a non-negative integer", repProcessEnv)
	}
	var results []repArmResult
	comparison := 0
	for _, geometry := range repGeometries {
		for _, workload := range repWorkloads {
			for _, threshold := range repThresholds {
				literalFirst := (process+comparison)&1 == 0
				for _, literal := range []bool{literalFirst, !literalFirst} {
					results = append(results, measureREPArm(t, process, geometry, workload, threshold, literal))
				}
				comparison++
			}
		}
	}
	p := filepath.Join("..", "..", "..", "testdata", "diagnostics", "rep-20260718", fmt.Sprintf("process-%d.json", process))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkGridDiffREPPrototype(b *testing.B) {
	geometry := repGeometries[0]
	for _, workload := range repWorkloads {
		frames := repFrames(workload, geometry.cols, geometry.rows)
		for _, threshold := range repThresholds {
			b.Run(fmt.Sprintf("%s/threshold-%d", workload, threshold), func(b *testing.B) {
				for _, literal := range []bool{true, false} {
					name := "rep"
					if literal {
						name = "literal"
					}
					b.Run(name, func(b *testing.B) {
						a := newREPArm(literal, threshold, geometry.cols*geometry.rows*repBytesCell+repBufferSlack, frames)
						var bytesTotal int64
						b.ReportAllocs()
						for b.Loop() {
							n, _, err := a.emit(frames)
							if err != nil {
								b.Fatal(err)
							}
							bytesTotal += int64(n)
						}
						if b.N > 0 {
							b.ReportMetric(float64(bytesTotal)/float64(b.N), "bytes/frame")
						}
					})
				}
			})
		}
	}
}

func TestREPPrototypeNoProductionHook(t *testing.T) {
	data, err := os.ReadFile("diff.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "writeREP") || strings.Contains(string(data), "repPrototype") {
		t.Fatal("production diff writer contains a REP prototype hook")
	}
}
