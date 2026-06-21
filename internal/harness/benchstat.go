package harness

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// MinRepeats is the smallest number of repeats benchstat-format output must
// carry for A/B comparison. The sweep measures each metric at least this many
// times so benchstat has enough samples to estimate a confidence interval.
const MinRepeats = 10

// benchPrefix is the leading token every Go benchmark line carries. benchstat
// and the wider tool family only read lines that start with this token followed
// by an uppercase letter, so each metric name is capitalised before emission.
const benchPrefix = "Benchmark"

// MetricSample is one repeat of one named metric for one (page, mode). The
// sweep collects MinRepeats or more of these per metric so the emitted lines
// feed straight into benchstat. Unit is the custom benchmark-format unit that
// lets one tool rank both quality and cost, for example "deltaE/op" or
// "B/frame"; lower is always better once the sign convention is fixed by the
// unit name.
type MetricSample struct {
	// Page is the fixture name, for example "linuxmatters".
	Page string
	// Mode is the render backend, one of "block", "sixel", or "kitty".
	Mode string
	// Metric is the unqualified metric name, for example "SSIM" or
	// "BytesPerFrame". It is capitalised in the emitted benchmark name.
	Metric string
	// Unit is the custom benchmark-format unit for this metric's value, for
	// example "ssim/op" or "B/frame". A unit must not contain whitespace.
	Unit string
	// Value is one measured sample of the metric.
	Value float64
}

// WriteBenchstat writes the samples as Go benchmark-format lines to w, one line
// per sample, grouped so every repeat of a given (metric, page, mode) shares the
// same benchmark name. The output parses with golang.org/x/perf/benchfmt and
// compares with the benchstat tool. Samples are emitted in a stable order:
// metric, then page, then mode, preserving input order within a group so repeat
// order is the caller's. WriteBenchstat returns an error if any sample carries a
// blank field or a unit with whitespace, since such a line would not parse.
//
// Each line has the form:
//
//	BenchmarkMetric/page/mode-1<TAB>1<TAB>value unit
//
// The "-1" suffix pins the reported GOMAXPROCS to 1 so runs compare like for
// like, matching the per-backend benchmarks. The iteration count is fixed at 1
// because each line carries one already-measured sample, not a timed loop.
func WriteBenchstat(w io.Writer, samples []MetricSample) error {
	for i := range samples {
		if err := samples[i].validate(); err != nil {
			return fmt.Errorf("sample %d: %w", i, err)
		}
	}

	ordered := make([]int, len(samples))
	for i := range ordered {
		ordered[i] = i
	}
	sort.SliceStable(ordered, func(a, b int) bool {
		x, y := samples[ordered[a]], samples[ordered[b]]
		if x.Metric != y.Metric {
			return x.Metric < y.Metric
		}
		if x.Page != y.Page {
			return x.Page < y.Page
		}
		return x.Mode < y.Mode
	})

	for _, i := range ordered {
		s := samples[i]
		if _, err := fmt.Fprintf(w, "%s\t1\t%s %s\n", s.benchName(), formatValue(s.Value), s.Unit); err != nil {
			return err
		}
	}
	return nil
}

// benchName builds the full benchmark name for a sample: the Benchmark prefix,
// the capitalised metric, the page and mode sub-name segments, and the "-1"
// GOMAXPROCS suffix.
func (s MetricSample) benchName() string {
	return fmt.Sprintf("%s%s/%s/%s-1", benchPrefix, capitalise(s.Metric), s.Page, s.Mode)
}

// validate reports whether the sample can produce a parseable benchmark line.
// Blank required fields or whitespace in a name or unit would break the format.
func (s MetricSample) validate() error {
	switch {
	case s.Metric == "":
		return fmt.Errorf("empty metric")
	case s.Unit == "":
		return fmt.Errorf("empty unit")
	case strings.ContainsAny(s.Metric, " \t"):
		return fmt.Errorf("metric %q contains whitespace", s.Metric)
	case strings.ContainsAny(s.Page, " \t"):
		return fmt.Errorf("page %q contains whitespace", s.Page)
	case strings.ContainsAny(s.Mode, " \t"):
		return fmt.Errorf("mode %q contains whitespace", s.Mode)
	case strings.ContainsAny(s.Unit, " \t"):
		return fmt.Errorf("unit %q contains whitespace", s.Unit)
	}
	return nil
}

// capitalise upper-cases the first rune of name so the benchmark line starts
// with the Benchmark prefix followed by an uppercase letter, which the
// benchmark format requires. Names that already start uppercase pass through.
func capitalise(name string) string {
	if name == "" {
		return name
	}
	r := []rune(name)
	if r[0] >= 'a' && r[0] <= 'z' {
		r[0] -= 'a' - 'A'
	}
	return string(r)
}

// formatValue renders a metric value with enough precision for the quality
// metrics (deltaE, SSIM) without trailing-zero noise. It uses %g, which drops
// insignificant trailing zeros and falls back to exponent form only for very
// large or very small magnitudes.
func formatValue(v float64) string {
	return fmt.Sprintf("%g", v)
}
