//go:build linux

package kitty

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// shmPath converts a POSIX object name into its /dev/shm file path.
func shmPath(name string) string {
	return shmDir + strings.TrimPrefix(name, "/")
}

// listSHMEntries returns the /dev/shm entries created by this test process.
func listSHMEntries(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob(fmt.Sprintf("%snyala-%d-*", shmDir, os.Getpid()))
	if err != nil {
		t.Fatalf("glob /dev/shm: %v", err)
	}
	return matches
}

// cleanSHMEntries removes any leftover objects so one failing test cannot
// pollute the next.
func cleanSHMEntries(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		for _, path := range listSHMEntries(t) {
			os.Remove(path)
		}
	})
}

// TestSHMWriterRoundTrip checks that WriteFrame stores the payload byte for
// byte under the returned name and that Create exposes a writable mapping the
// caller can fill directly.
func TestSHMWriterRoundTrip(t *testing.T) {
	cleanSHMEntries(t)
	w := NewSHMWriter()
	defer w.Close()

	payload := []byte("kitty t=s frame payload")
	name, err := w.WriteFrame(1, payload)
	if err != nil {
		t.Fatalf("WriteFrame() = %v", err)
	}
	if !strings.HasPrefix(name, "/") {
		t.Errorf("name %q missing the leading slash shm_open expects", name)
	}
	got, err := os.ReadFile(shmPath(name))
	if err != nil {
		t.Fatalf("read shm object: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("object contents = %q, want %q", got, payload)
	}

	obj, err := w.Create(2, len(payload))
	if err != nil {
		t.Fatalf("Create() = %v", err)
	}
	copy(obj.Data(), payload)
	if err := obj.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if obj.Data() != nil {
		t.Error("Data() non-nil after Close")
	}
	if err := obj.Close(); err != nil {
		t.Fatalf("second Close() = %v", err)
	}
	got, err = os.ReadFile(shmPath(obj.Name()))
	if err != nil {
		t.Fatalf("read mapped shm object: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("mapped object contents = %q, want %q", got, payload)
	}
}

// TestSHMWriterNoLeaks checks that no /dev/shm entries survive the writer in
// either lifecycle: the terminal consumed and unlinked the object, or the
// terminal ignored it and the writer must unlink it on replacement or Close.
func TestSHMWriterNoLeaks(t *testing.T) {
	cleanSHMEntries(t)
	w := NewSHMWriter()

	// Terminal-consumed: Kitty unlinks the object after reading it.
	consumed, err := w.WriteFrame(1, []byte("consumed"))
	if err != nil {
		t.Fatalf("WriteFrame() = %v", err)
	}
	if err := os.Remove(shmPath(consumed)); err != nil {
		t.Fatalf("simulate terminal unlink: %v", err)
	}

	// Terminal-ignored, replaced: the next frame under the same double-buffer
	// id must unlink the unconsumed predecessor.
	first, err := w.WriteFrame(2, []byte("ignored, replaced"))
	if err != nil {
		t.Fatalf("WriteFrame() = %v", err)
	}
	if _, err := w.WriteFrame(2, []byte("replacement")); err != nil {
		t.Fatalf("WriteFrame() = %v", err)
	}
	if _, err := os.Stat(shmPath(first)); !os.IsNotExist(err) {
		t.Errorf("replaced object %s still present, stat err %v", first, err)
	}

	// Terminal-ignored, left at shutdown: Close unlinks the survivors and
	// skips the consumed entry without error.
	if err := w.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if entries := listSHMEntries(t); len(entries) != 0 {
		t.Errorf("orphaned /dev/shm entries after Close: %v", entries)
	}
}

// TestSHMWriterNameCollision checks that two writer instances creating
// objects concurrently never produce the same name, the collision-safety the
// pid-plus-process-wide-sequence scheme guarantees within one process.
func TestSHMWriterNameCollision(t *testing.T) {
	cleanSHMEntries(t)
	const perWriter = 64
	writers := []*SHMWriter{NewSHMWriter(), NewSHMWriter()}
	names := make([][]string, len(writers))

	var wg sync.WaitGroup
	for i, w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWriter {
				// A fresh id per frame keeps every object alive, so a name
				// reuse would surface as an EEXIST creation failure too.
				name, err := w.WriteFrame(len(names[i])+1, []byte{0})
				if err != nil {
					t.Errorf("WriteFrame() = %v", err)
					return
				}
				names[i] = append(names[i], name)
			}
		}()
	}
	wg.Wait()

	seen := make(map[string]bool, 2*perWriter)
	for _, ws := range names {
		for _, name := range ws {
			if seen[name] {
				t.Errorf("duplicate object name %s across writers", name)
			}
			seen[name] = true
		}
	}
	if len(seen) != 2*perWriter {
		t.Errorf("unique names = %d, want %d", len(seen), 2*perWriter)
	}
	for _, w := range writers {
		if err := w.Close(); err != nil {
			t.Errorf("Close() = %v", err)
		}
	}
	if entries := listSHMEntries(t); len(entries) != 0 {
		t.Errorf("orphaned /dev/shm entries after Close: %v", entries)
	}
}

// TestSHMWriterRejectsEmptyPayload checks that a zero-size object is refused
// before any /dev/shm entry is created, because mmap cannot map zero bytes.
func TestSHMWriterRejectsEmptyPayload(t *testing.T) {
	cleanSHMEntries(t)
	w := NewSHMWriter()
	defer w.Close()
	if _, err := w.WriteFrame(1, nil); err == nil {
		t.Fatal("WriteFrame(nil) succeeded, want error")
	}
	if entries := listSHMEntries(t); len(entries) != 0 {
		t.Errorf("entries created for rejected payload: %v", entries)
	}
}
