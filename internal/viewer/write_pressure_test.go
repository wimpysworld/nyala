package viewer

import (
	"bytes"
	"io"
	"testing"
	"time"
)

func TestWritePressureObserverSamplesEveryEighthEligibleWrite(t *testing.T) {
	var observer writePressureObserver
	for eligible := 1; eligible <= 3*writePressureSampleStride; eligible++ {
		if observer.sample(false) {
			t.Fatalf("ineligible write %d selected for sampling", eligible)
		}
		got := observer.sample(true)
		want := eligible%writePressureSampleStride == 0
		if got != want {
			t.Fatalf("eligible write %d sample = %t, want %t", eligible, got, want)
		}
	}
}

func TestWritePressureObserverRequiresSustainedPressureAfterStartup(t *testing.T) {
	var observer writePressureObserver
	for sample := 1; sample <= writePressureStartupSamples; sample++ {
		if observer.observe(17 * time.Millisecond) {
			t.Fatalf("startup sample %d reported pressure", sample)
		}
	}
	if observer.observe(100 * time.Millisecond) {
		t.Fatal("one isolated slow sample reported pressure")
	}
	if observer.observe(7 * time.Millisecond) {
		t.Fatal("fast sample reported pressure")
	}
	for sample := 1; sample < writePressureSustainedSamples; sample++ {
		if observer.observe(17 * time.Millisecond) {
			t.Fatalf("slow sample %d after the reset reported pressure", sample)
		}
	}
	if observer.observe(7 * time.Millisecond) {
		t.Fatal("fast sample after a short slow run reported pressure")
	}
}

func TestWritePressureObserverRejectsAdaptiveWriteTimes(t *testing.T) {
	var observer writePressureObserver
	for sample := range 4 * (writePressureStartupSamples + writePressureSustainedSamples) {
		if observer.observe(7200 * time.Microsecond) {
			t.Fatalf("adaptive sample %d reported pressure", sample+1)
		}
	}
}

func TestWritePressureObserverReportsGhosttyPressureOnce(t *testing.T) {
	var observer writePressureObserver
	for range writePressureStartupSamples {
		if observer.observe(16700 * time.Microsecond) {
			t.Fatal("startup work reported pressure")
		}
	}
	for sample := 1; sample <= writePressureSustainedSamples; sample++ {
		got := observer.observe(16700 * time.Microsecond)
		want := sample == writePressureSustainedSamples
		if got != want {
			t.Fatalf("sustained sample %d verdict = %t, want %t", sample, got, want)
		}
	}
	if observer.observe(16700 * time.Microsecond) {
		t.Fatal("observer reported a second verdict")
	}
	if observer.sample(true) {
		t.Fatal("observer sampled after its verdict")
	}
}

func TestStreamWriterClocksOnlySelectedPressureSamples(t *testing.T) {
	var observer writePressureObserver
	var clockReads int
	base := time.Unix(0, 0)
	w := newStreamWriter(io.Discard)
	w.clock = func() time.Time {
		now := base.Add(time.Duration(clockReads) * time.Millisecond)
		clockReads++
		return now
	}

	buf := bytes.NewBufferString("frame")
	const writes = 2 * writePressureSampleStride
	for eligible := 1; eligible <= writes; eligible++ {
		sampled := observer.sample(true)
		w.send(writeItem{buf: buf, pressureSample: sampled})
		free := <-w.free
		if free.pressureSample != sampled {
			t.Fatalf("eligible write %d returned pressureSample = %t, want %t", eligible, free.pressureSample, sampled)
		}
		if sampled && free.writeDur != time.Millisecond {
			t.Fatalf("eligible write %d duration = %v, want %v", eligible, free.writeDur, time.Millisecond)
		}
		if !sampled && free.writeDur != 0 {
			t.Fatalf("eligible write %d unsampled duration = %v, want 0", eligible, free.writeDur)
		}
	}
	w.stop()

	wantReads := 2 * (writes / writePressureSampleStride)
	if clockReads != wantReads {
		t.Fatalf("clock reads = %d, want %d for sampled writes only", clockReads, wantReads)
	}
}

func TestWritePressureObserverAllocatesNothing(t *testing.T) {
	allocs := testing.AllocsPerRun(1000, func() {
		var observer writePressureObserver
		for range 64 {
			if observer.sample(true) {
				observer.observe(7 * time.Millisecond)
			}
		}
	})
	if allocs != 0 {
		t.Fatalf("observer allocations = %v, want 0", allocs)
	}
}

var benchmarkPressureVerdict bool

func BenchmarkWritePressureObserver(b *testing.B) {
	b.Run("Ineligible", func(b *testing.B) {
		var observer writePressureObserver
		b.ReportAllocs()
		for range b.N {
			benchmarkPressureVerdict = observer.sample(false)
		}
	})
	b.Run("Eligible", func(b *testing.B) {
		var observer writePressureObserver
		b.ReportAllocs()
		for range b.N {
			if observer.sample(true) {
				benchmarkPressureVerdict = observer.observe(7 * time.Millisecond)
			}
		}
	})
}
