//go:build darwin

package kitty

import (
	"bytes"
	"sync"
	"testing"

	"github.com/wimpysworld/nyala/internal/posixshm"
)

func TestDarwinSHMWriterMappedAndCopiedPayloads(t *testing.T) {
	w := NewSHMWriter()
	t.Cleanup(func() { _ = w.Close() })

	payload := []byte("kitty t=s frame payload")
	obj, err := w.Create(1, len(payload))
	if err != nil {
		t.Fatalf("Create() = %v", err)
	}
	t.Cleanup(func() { _ = obj.Close() })
	copy(obj.Data(), payload)
	if !bytes.Equal(obj.Data(), payload) {
		t.Errorf("mapped payload = %q, want %q", obj.Data(), payload)
	}
	if err := obj.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if obj.Data() != nil {
		t.Error("Data() is non-nil after Close")
	}
	if err := obj.Close(); err != nil {
		t.Fatalf("second Close() = %v", err)
	}

	var copied []byte
	backing := make([]byte, len(payload))
	fake := &SHMObject{
		name: "/ny-f-test",
		data: backing,
		close: func() error {
			copied = append(copied, backing...)
			return nil
		},
	}
	name, err := writeSHMObject(fake, payload)
	if err != nil {
		t.Fatalf("writeSHMObject() = %v", err)
	}
	if name != fake.Name() {
		t.Errorf("writeSHMObject() name = %q, want %q", name, fake.Name())
	}
	if !bytes.Equal(copied, payload) {
		t.Errorf("copied payload = %q, want %q", copied, payload)
	}
}

func TestDarwinSHMWriterNames(t *testing.T) {
	const perWriter = 32
	writers := []*SHMWriter{NewSHMWriter(), NewSHMWriter()}
	names := make([][]string, len(writers))
	for _, w := range writers {
		t.Cleanup(func() { _ = w.Close() })
	}

	var wg sync.WaitGroup
	for i, w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := 1; id <= perWriter; id++ {
				name, err := w.WriteFrame(id, []byte{byte(id)})
				if err != nil {
					t.Errorf("WriteFrame() = %v", err)
					return
				}
				if err := posixshm.ValidateName(name); err != nil {
					t.Errorf("generated name %q is invalid: %v", name, err)
					return
				}
				names[i] = append(names[i], name)
			}
		}()
	}
	wg.Wait()

	seen := make(map[string]bool, 2*perWriter)
	for _, writerNames := range names {
		for _, name := range writerNames {
			if seen[name] {
				t.Errorf("duplicate object name %s", name)
			}
			seen[name] = true
		}
	}
	if len(seen) != 2*perWriter {
		t.Errorf("unique names = %d, want %d", len(seen), 2*perWriter)
	}
}

func TestDarwinSHMWriterCleanup(t *testing.T) {
	w := NewSHMWriter()
	t.Cleanup(func() { _ = w.Close() })

	consumed, err := w.WriteFrame(1, []byte("consumed"))
	if err != nil {
		t.Fatalf("WriteFrame() = %v", err)
	}
	if err := posixshm.Unlink(consumed); err != nil {
		t.Fatalf("simulate terminal unlink: %v", err)
	}

	replaced, err := w.WriteFrame(2, []byte("ignored"))
	if err != nil {
		t.Fatalf("WriteFrame() = %v", err)
	}
	replacement, err := w.WriteFrame(2, []byte("replacement"))
	if err != nil {
		t.Fatalf("replacement WriteFrame() = %v", err)
	}
	assertDarwinSHMNameAvailable(t, replaced)

	atClose, err := w.WriteFrame(3, []byte("close"))
	if err != nil {
		t.Fatalf("WriteFrame() = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close() = %v", err)
	}
	assertDarwinSHMNameAvailable(t, atClose)
	assertDarwinSHMNameAvailable(t, replacement)
}

func TestDarwinSHMWriterRejectsZeroSize(t *testing.T) {
	w := NewSHMWriter()
	t.Cleanup(func() { _ = w.Close() })

	if _, err := w.Create(1, 0); err == nil {
		t.Fatal("Create(0) succeeded")
	}
	if _, err := w.WriteFrame(1, nil); err == nil {
		t.Fatal("WriteFrame(nil) succeeded")
	}
	if len(w.last) != 0 {
		t.Errorf("tracked objects after rejected sizes = %d, want 0", len(w.last))
	}
}

func assertDarwinSHMNameAvailable(t *testing.T, name string) {
	t.Helper()
	obj, err := posixshm.Create(name, 1)
	if err != nil {
		t.Fatalf("object %s was not unlinked: %v", name, err)
	}
	if err := obj.Close(); err != nil {
		t.Fatalf("close recreated object %s: %v", name, err)
	}
	if err := posixshm.Unlink(name); err != nil {
		t.Fatalf("unlink recreated object %s: %v", name, err)
	}
}
