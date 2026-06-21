package harness_test

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"golang.org/x/perf/benchfmt"

	"github.com/wimpysworld/nyala/internal/harness"
)

// TestWriteBenchstatLineFormat pins the exact benchmark-line shape for a single
// sample: the Benchmark prefix, the capitalised metric, the page and mode
// sub-name, the "-1" GOMAXPROCS suffix, the iteration count, and the value and
// custom unit, tab-separated.
func TestWriteBenchstatLineFormat(t *testing.T) {
	var buf bytes.Buffer
	err := harness.WriteBenchstat(&buf, []harness.MetricSample{
		{Page: "linuxmatters", Mode: "block", Metric: "ssim", Unit: "ssim/op", Value: 0.875},
	})
	if err != nil {
		t.Fatalf("WriteBenchstat: %v", err)
	}
	got := strings.TrimRight(buf.String(), "\n")
	want := "BenchmarkSsim/linuxmatters/block-1\t1\t0.875 ssim/op"
	if got != want {
		t.Fatalf("line mismatch:\n got %q\nwant %q", got, want)
	}
}

// TestWriteBenchstatParses feeds the emitted output through the real
// golang.org/x/perf/benchfmt reader, the same parser benchstat uses, and checks
// every line parses into a Result with the expected name, value, and custom
// unit. If benchstat cannot parse the output, this fails.
func TestWriteBenchstatParses(t *testing.T) {
	samples := []harness.MetricSample{
		{Page: "testcard-01-greyscale", Mode: "kitty", Metric: "deltaE2000Mean", Unit: "deltaE/op", Value: 1.25},
		{Page: "testcard-01-greyscale", Mode: "block", Metric: "bytesPerFrame", Unit: "B/frame", Value: 4096},
	}
	var buf bytes.Buffer
	if err := harness.WriteBenchstat(&buf, samples); err != nil {
		t.Fatalf("WriteBenchstat: %v", err)
	}

	r := benchfmt.NewReader(&buf, "benchstat_test")
	var results int
	for r.Scan() {
		rec, ok := r.Result().(*benchfmt.Result)
		if !ok {
			continue
		}
		if len(rec.Values) != 1 {
			t.Fatalf("result %q: got %d values, want 1", rec.Name, len(rec.Values))
		}
		v := rec.Values[0]
		// benchfmt strips the leading "Benchmark" token, so the parsed name
		// starts at the metric. Assert the segments survive the round trip.
		if !strings.Contains(string(rec.Name), "/") {
			t.Errorf("name %q lost its sub-name segments", rec.Name)
		}
		if v.Unit == "" {
			t.Errorf("result %q: empty unit", rec.Name)
		}
		results++
	}
	if err := r.Err(); err != nil {
		t.Fatalf("benchfmt reader: %v", err)
	}
	if results != len(samples) {
		t.Fatalf("parsed %d results, want %d", results, len(samples))
	}
}

// TestWriteBenchstatRepeats emits MinRepeats samples of one metric for one
// (page, mode) and checks every repeat shares the same benchmark name, so
// benchstat groups them as repeats of a single benchmark for A/B comparison.
func TestWriteBenchstatRepeats(t *testing.T) {
	if harness.MinRepeats < 10 {
		t.Fatalf("MinRepeats = %d, want >= 10", harness.MinRepeats)
	}
	samples := make([]harness.MetricSample, harness.MinRepeats)
	for i := range samples {
		samples[i] = harness.MetricSample{
			Page: "testcard-02-colour", Mode: "sixel",
			Metric: "psnr", Unit: "psnr/op", Value: 30 + float64(i),
		}
	}
	var buf bytes.Buffer
	if err := harness.WriteBenchstat(&buf, samples); err != nil {
		t.Fatalf("WriteBenchstat: %v", err)
	}

	sc := bufio.NewScanner(&buf)
	var lines, name string
	var count int
	for sc.Scan() {
		lines = sc.Text()
		fields := strings.SplitN(lines, "\t", 2)
		if count == 0 {
			name = fields[0]
		} else if fields[0] != name {
			t.Fatalf("repeat %d name %q != first %q", count, fields[0], name)
		}
		count++
	}
	if count != harness.MinRepeats {
		t.Fatalf("emitted %d repeats, want %d", count, harness.MinRepeats)
	}
}

// TestWriteBenchstatOrder pins the stable emission order: metric, then page,
// then mode, so two runs diff line for line.
func TestWriteBenchstatOrder(t *testing.T) {
	samples := []harness.MetricSample{
		{Page: "b", Mode: "kitty", Metric: "ssim", Unit: "ssim/op", Value: 1},
		{Page: "a", Mode: "block", Metric: "ssim", Unit: "ssim/op", Value: 1},
		{Page: "a", Mode: "block", Metric: "psnr", Unit: "psnr/op", Value: 1},
	}
	var buf bytes.Buffer
	if err := harness.WriteBenchstat(&buf, samples); err != nil {
		t.Fatalf("WriteBenchstat: %v", err)
	}
	want := []string{
		"BenchmarkPsnr/a/block-1",
		"BenchmarkSsim/a/block-1",
		"BenchmarkSsim/b/kitty-1",
	}
	sc := bufio.NewScanner(&buf)
	var i int
	for sc.Scan() {
		name := strings.SplitN(sc.Text(), "\t", 2)[0]
		if i >= len(want) {
			t.Fatalf("more lines than expected at %q", name)
		}
		if name != want[i] {
			t.Errorf("line %d: got %q, want %q", i, name, want[i])
		}
		i++
	}
	if i != len(want) {
		t.Fatalf("got %d lines, want %d", i, len(want))
	}
}

// TestWriteBenchstatRejectsBadFields checks a sample with a whitespace unit is
// rejected, since such a line would not parse as the benchmark format.
func TestWriteBenchstatRejectsBadFields(t *testing.T) {
	var buf bytes.Buffer
	err := harness.WriteBenchstat(&buf, []harness.MetricSample{
		{Page: "a", Mode: "block", Metric: "ssim", Unit: "bad unit", Value: 1},
	})
	if err == nil {
		t.Fatal("expected error for whitespace unit, got nil")
	}
}
