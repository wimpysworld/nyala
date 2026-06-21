package metrics

import (
	"bytes"
	"errors"
	"io"
	"runtime"
	"testing"
)

func TestCountingWriterTotal(t *testing.T) {
	var buf bytes.Buffer
	cw := NewCountingWriter(&buf)

	payload := []byte("the quick brown fox jumps over the lazy dog")
	n, err := cw.Write(payload)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("write returned %d, want %d", n, len(payload))
	}
	if cw.N != int64(len(payload)) {
		t.Fatalf("counter total = %d, want %d", cw.N, len(payload))
	}
	if buf.Len() != len(payload) {
		t.Fatalf("underlying buffer holds %d bytes, want %d", buf.Len(), len(payload))
	}
}

func TestCountingWriterMultipleWrites(t *testing.T) {
	cw := NewCountingWriter(io.Discard)
	parts := [][]byte{[]byte("abc"), []byte("defg"), {}, []byte("hi")}
	var want int64
	for _, p := range parts {
		if _, err := cw.Write(p); err != nil {
			t.Fatalf("write: %v", err)
		}
		want += int64(len(p))
	}
	if cw.N != want {
		t.Fatalf("counter total = %d, want %d", cw.N, want)
	}
}

func TestCaptureFrameBytesMatchPayload(t *testing.T) {
	var buf bytes.Buffer
	cw := NewCountingWriter(&buf)
	payload := bytes.Repeat([]byte{0x1b}, 4096)

	perf, err := CaptureFrame(
		cw,
		func() error { return nil },
		func() error {
			_, werr := cw.Write(payload)
			return werr
		},
	)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if perf.Bytes != int64(len(payload)) {
		t.Fatalf("Bytes = %d, want %d", perf.Bytes, len(payload))
	}
	if perf.Bytes != cw.N {
		t.Fatalf("Bytes = %d, counter = %d; want equal", perf.Bytes, cw.N)
	}
}

func TestCaptureFrameTimingsNonNegative(t *testing.T) {
	cw := NewCountingWriter(io.Discard)
	perf, err := CaptureFrame(
		cw,
		func() error {
			// Allocate so the heap-alloc sample moves forward.
			sink := make([]byte, 1<<16)
			_ = sink
			return nil
		},
		func() error {
			_, werr := cw.Write([]byte("x"))
			return werr
		},
	)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if perf.Compose < 0 {
		t.Fatalf("Compose negative: %v", perf.Compose)
	}
	if perf.Encode < 0 {
		t.Fatalf("Encode negative: %v", perf.Encode)
	}
	if perf.FPS < 0 {
		t.Fatalf("FPS negative: %v", perf.FPS)
	}
	if total := perf.Compose + perf.Encode; total > 0 && perf.FPS == 0 {
		t.Fatalf("FPS zero with positive total time %v", total)
	}
}

func TestCaptureFramePropagatesError(t *testing.T) {
	cw := NewCountingWriter(io.Discard)
	want := errors.New("compose failed")
	if _, err := CaptureFrame(cw, func() error { return want }, nil); !errors.Is(err, want) {
		t.Fatalf("compose error = %v, want %v", err, want)
	}
	wantEnc := errors.New("encode failed")
	if _, err := CaptureFrame(cw, nil, func() error { return wantEnc }); !errors.Is(err, wantEnc) {
		t.Fatalf("encode error = %v, want %v", err, wantEnc)
	}
}

func TestCaptureFrameNilPhases(t *testing.T) {
	cw := NewCountingWriter(io.Discard)
	perf, err := CaptureFrame(cw, nil, nil)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if perf.Bytes != 0 || perf.Compose != 0 || perf.Encode != 0 {
		t.Fatalf("nil phases produced non-zero perf: %+v", perf)
	}
}

func TestReadAllocBytesMonotonic(t *testing.T) {
	before := readAllocBytes()
	sink := make([][]byte, 256)
	for i := range sink {
		sink[i] = make([]byte, 1024)
	}
	runtime.KeepAlive(sink)
	after := readAllocBytes()
	if after < before {
		t.Fatalf("cumulative allocs went backwards: before %d, after %d", before, after)
	}
}

func TestPeakRSSPlausibleOnLinux(t *testing.T) {
	rss := PeakRSS()
	if runtime.GOOS != "linux" {
		t.Skipf("PeakRSS is zero off Linux; GOOS=%s", runtime.GOOS)
	}
	if rss == 0 {
		t.Fatal("PeakRSS returned 0 on Linux, want a plausible non-zero value")
	}
	// A running Go test process holds at least a megabyte resident.
	if rss < 1<<20 {
		t.Fatalf("PeakRSS = %d bytes, implausibly small", rss)
	}
}
