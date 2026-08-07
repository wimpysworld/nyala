//go:build darwin

package posixshm

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func testName(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("/nyala-pst-%d", os.Getpid())
}

func TestObjectLifecycle(t *testing.T) {
	name := testName(t)
	if err := Unlink(name); err != nil {
		t.Fatalf("remove stale object: %v", err)
	}
	t.Cleanup(func() { _ = Unlink(name) })

	payload := []byte("mapped shared-memory payload")
	obj, err := Create(name, len(payload))
	if err != nil {
		t.Fatalf("Create() = %v", err)
	}
	if obj.Name() != name {
		t.Errorf("Name() = %q, want %q", obj.Name(), name)
	}
	copy(obj.Data(), payload)
	if got := string(obj.Data()); got != string(payload) {
		t.Errorf("Data() = %q, want %q", got, payload)
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
	fd, err := shmOpen(name, unix.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("shmOpen(O_RDONLY) = %v", err)
	}
	reopened, mapErr := unix.Mmap(fd, 0, len(payload), unix.PROT_READ, unix.MAP_SHARED)
	closeErr := unix.Close(fd)
	if mapErr != nil {
		t.Fatalf("Mmap(reopened) = %v", mapErr)
	}
	t.Cleanup(func() { _ = unix.Munmap(reopened) })
	if closeErr != nil {
		t.Fatalf("Close(reopened) = %v", closeErr)
	}
	if got := string(reopened); got != string(payload) {
		t.Errorf("reopened payload = %q, want %q", got, payload)
	}
	if err := Unlink(name); err != nil {
		t.Fatalf("Unlink() = %v", err)
	}
	if err := Unlink(name); err != nil {
		t.Fatalf("second Unlink() = %v", err)
	}
}

func TestCreateIsExclusive(t *testing.T) {
	name := testName(t)
	if err := Unlink(name); err != nil {
		t.Fatalf("remove stale object: %v", err)
	}
	t.Cleanup(func() { _ = Unlink(name) })

	obj, err := Create(name, 1)
	if err != nil {
		t.Fatalf("Create() = %v", err)
	}
	defer obj.Close()

	if _, err := Create(name, 1); !errors.Is(err, unix.EEXIST) {
		t.Fatalf("second Create() error = %v, want EEXIST", err)
	}
}

func TestCreateRejectsInvalidInput(t *testing.T) {
	for _, name := range []string{"nyala", "//nyala", "/nyala/frame"} {
		if _, err := Create(name, 1); err == nil {
			t.Errorf("Create(%q, 1) succeeded", name)
		}
	}
	if _, err := Create(testName(t), 0); err == nil {
		t.Error("Create(valid, 0) succeeded")
	}
}
