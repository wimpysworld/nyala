//go:build darwin

package viewer

import (
	"bytes"
	"io"
	"os"
	"testing"

	"github.com/wimpysworld/nyala/internal/posixshm"
)

func TestDarwinProbeSHMPayload(t *testing.T) {
	name := probeSHMName(uint32(os.Getpid()), probeSHMSeq.Add(1))
	if err := posixshm.Unlink(name); err != nil {
		t.Fatalf("remove stale probe: %v", err)
	}
	t.Cleanup(func() { _ = posixshm.Unlink(name) })

	obj, err := createProbeSHMObject(name)
	if err != nil {
		t.Fatalf("createProbeSHMObject: %v", err)
	}
	if got := obj.Data(); !bytes.Equal(got, []byte{0, 0, 0}) {
		t.Errorf("probe payload = %v, want one black RGB pixel", got)
	}
	if err := obj.Close(); err != nil {
		t.Fatalf("close probe: %v", err)
	}
}

func TestDarwinProbeSHMCleanup(t *testing.T) {
	for _, test := range []struct {
		name     string
		consumed bool
	}{
		{name: "ignored by terminal"},
		{name: "consumed by terminal", consumed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			name, err := createProbeSHM()
			if err != nil {
				t.Fatalf("createProbeSHM: %v", err)
			}
			t.Cleanup(func() { _ = posixshm.Unlink(name) })
			if test.consumed {
				if err := posixshm.Unlink(name); err != nil {
					t.Fatalf("simulate terminal unlink: %v", err)
				}
			}
			unlinkProbeSHM(name)

			obj, err := posixshm.Create(name, 1)
			if err != nil {
				t.Fatalf("probe name remains after cleanup: %v", err)
			}
			if err := obj.Close(); err != nil {
				t.Fatalf("close replacement object: %v", err)
			}
			if err := posixshm.Unlink(name); err != nil {
				t.Fatalf("unlink replacement object: %v", err)
			}
		})
	}
}

func TestDarwinProbeSHMCleanupReporting(t *testing.T) {
	capture := func(name string) []byte {
		t.Helper()
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatalf("capture stderr: %v", err)
		}
		originalStderr := os.Stderr
		os.Stderr = writer
		unlinkProbeSHM(name)
		os.Stderr = originalStderr
		if err := writer.Close(); err != nil {
			t.Fatalf("close captured stderr: %v", err)
		}
		got, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read captured stderr: %v", err)
		}
		if err := reader.Close(); err != nil {
			t.Fatalf("close stderr reader: %v", err)
		}
		return got
	}

	missing := probeSHMName(uint32(os.Getpid()), probeSHMSeq.Add(1))
	if err := posixshm.Unlink(missing); err != nil {
		t.Fatalf("remove stale probe: %v", err)
	}
	if got := capture(missing); len(got) != 0 {
		t.Errorf("missing probe output = %q, want silence", got)
	}
	got := capture("invalid")
	if !bytes.Contains(got, []byte("nyala: remove shm probe object:")) {
		t.Fatalf("unlink failure output = %q, want cleanup error", got)
	}
}

func TestDarwinProbeSHMCreateFailure(t *testing.T) {
	name := probeSHMName(uint32(os.Getpid()), probeSHMSeq.Load()+1)
	obj, err := posixshm.Create(name, 1)
	if err != nil {
		t.Fatalf("create collision object %q: %v", name, err)
	}
	t.Cleanup(func() { _ = posixshm.Unlink(name) })
	if err := obj.Close(); err != nil {
		t.Fatalf("close collision object %q: %v", name, err)
	}

	if _, err := createProbeSHMWithAttempts(1); err == nil {
		t.Fatal("createProbeSHMWithAttempts succeeded after a name collision")
	}
	if query := queryBytes(""); bytes.Contains(query, []byte("t=s")) {
		t.Fatalf("failed probe query contains t=s: %q", query)
	}
	if got := resolveKittyTransport("auto", "Ghostty 1.1.3", false, "darwin"); got != KittyTransportDirect {
		t.Fatalf("failed probe transport = %q, want %q", got, KittyTransportDirect)
	}
}
