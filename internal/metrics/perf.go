package metrics

import (
	"io"
	"runtime/metrics"
	"time"
)

// CountingWriter wraps an io.Writer and records the total number of bytes
// written through it. Use it to measure a renderer's bytes per frame without
// buffering the whole payload: pass it to Renderer.Render and read N afterwards.
type CountingWriter struct {
	// W is the underlying writer. It may be io.Discard to count bytes only.
	W io.Writer
	// N is the running total of bytes written through this writer.
	N int64
}

// NewCountingWriter returns a CountingWriter that forwards to w. Pass io.Discard
// for w to count bytes without retaining them.
func NewCountingWriter(w io.Writer) *CountingWriter {
	return &CountingWriter{W: w}
}

// Write writes p to the underlying writer and adds the number of bytes the
// underlying writer reports to N. It returns that count and any write error, so
// N reflects bytes actually accepted, not bytes offered.
func (c *CountingWriter) Write(p []byte) (int, error) {
	n, err := c.W.Write(p)
	c.N += int64(n)
	return n, err
}

// FramePerf holds the captured cost of rendering one frame: the byte size of the
// emitted payload, the compose and encode phase times, the derived frame rate,
// the heap bytes allocated during the frame, and the process peak resident set
// size. Timings are wall-clock durations and are never negative.
type FramePerf struct {
	// Bytes is the payload size of the frame in bytes, as counted by the
	// CountingWriter the renderer wrote to.
	Bytes int64
	// Compose is the time spent building the frame before encoding.
	Compose time.Duration
	// Encode is the time spent encoding the frame to its wire payload.
	Encode time.Duration
	// FPS is the frames-per-second implied by Compose plus Encode. It is zero
	// when the total time is zero.
	FPS float64
	// AllocBytes is the heap memory allocated while rendering the frame, in
	// bytes, sampled from runtime/metrics around the measured span.
	AllocBytes uint64
	// PeakRSSBytes is the process peak resident set size in bytes after the
	// frame, from getrusage on Linux and zero on platforms without it.
	PeakRSSBytes uint64
}

// allocsMetric is the runtime/metrics key for cumulative bytes allocated on the
// heap. Sampling it before and after a span gives the bytes allocated during it.
const allocsMetric = "/gc/heap/allocs:bytes"

// readAllocBytes returns the cumulative heap bytes allocated by the process so
// far, from runtime/metrics. Subtract two samples to get the allocation of the
// span between them. It returns zero if the metric is unavailable.
func readAllocBytes() uint64 {
	sample := []metrics.Sample{{Name: allocsMetric}}
	metrics.Read(sample)
	if sample[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return sample[0].Value.Uint64()
}

// CaptureFrame renders one frame and returns its captured cost. compose builds
// the frame and encode writes its payload to a CountingWriter; both are called
// once. The returned FramePerf reports the bytes that the CountingWriter
// counted, the wall-clock time of each phase, the implied FPS, the heap bytes
// allocated across both phases, and the process peak RSS. A nil compose or
// encode is treated as a no-op phase.
func CaptureFrame(cw *CountingWriter, compose, encode func() error) (FramePerf, error) {
	allocBefore := readAllocBytes()

	var composeDur, encodeDur time.Duration
	if compose != nil {
		start := time.Now()
		if err := compose(); err != nil {
			return FramePerf{}, err
		}
		composeDur = time.Since(start)
	}
	if encode != nil {
		start := time.Now()
		if err := encode(); err != nil {
			return FramePerf{}, err
		}
		encodeDur = time.Since(start)
	}

	allocAfter := readAllocBytes()
	var alloc uint64
	if allocAfter >= allocBefore {
		alloc = allocAfter - allocBefore
	}

	total := composeDur + encodeDur
	var fps float64
	if total > 0 {
		fps = float64(time.Second) / float64(total)
	}

	var bytes int64
	if cw != nil {
		bytes = cw.N
	}

	return FramePerf{
		Bytes:        bytes,
		Compose:      composeDur,
		Encode:       encodeDur,
		FPS:          fps,
		AllocBytes:   alloc,
		PeakRSSBytes: PeakRSS(),
	}, nil
}
