package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAudioAvailablePulseServer(t *testing.T) {
	// PULSE_SERVER set is the short circuit: a sink is present without probing the
	// runtime dir at all.
	t.Setenv("PULSE_SERVER", "unix:/run/pulse")
	t.Setenv("XDG_RUNTIME_DIR", "")
	if !audioAvailable() {
		t.Error("audioAvailable() = false, want true when PULSE_SERVER is set")
	}
}

func TestAudioAvailableNoXDG(t *testing.T) {
	// No PULSE_SERVER and an empty XDG_RUNTIME_DIR must read as no sink, never
	// probing a bogus root path.
	t.Setenv("PULSE_SERVER", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	if audioAvailable() {
		t.Error("audioAvailable() = true, want false with no PULSE_SERVER and no XDG_RUNTIME_DIR")
	}
}

func TestAudioAvailablePulseSocket(t *testing.T) {
	// A pulse/native socket under XDG_RUNTIME_DIR marks a PulseAudio sink. The
	// stat only checks existence, so a plain file stands in for the socket.
	xdg := t.TempDir()
	t.Setenv("PULSE_SERVER", "")
	t.Setenv("XDG_RUNTIME_DIR", xdg)
	pulse := filepath.Join(xdg, "pulse")
	if err := os.MkdirAll(pulse, 0o755); err != nil {
		t.Fatalf("mkdir pulse: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pulse, "native"), nil, 0o600); err != nil {
		t.Fatalf("write pulse/native: %v", err)
	}
	if !audioAvailable() {
		t.Error("audioAvailable() = false, want true with a pulse/native socket present")
	}
}

func TestAudioAvailablePipewireSocket(t *testing.T) {
	// A pipewire-0 socket under XDG_RUNTIME_DIR marks a PipeWire sink.
	xdg := t.TempDir()
	t.Setenv("PULSE_SERVER", "")
	t.Setenv("XDG_RUNTIME_DIR", xdg)
	if err := os.WriteFile(filepath.Join(xdg, "pipewire-0"), nil, 0o600); err != nil {
		t.Fatalf("write pipewire-0: %v", err)
	}
	if !audioAvailable() {
		t.Error("audioAvailable() = false, want true with a pipewire-0 socket present")
	}
}

func TestAudioAvailableNoSink(t *testing.T) {
	// An XDG_RUNTIME_DIR with neither socket present must read as no sink.
	t.Setenv("PULSE_SERVER", "")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	if audioAvailable() {
		t.Error("audioAvailable() = true, want false with no sink socket present")
	}
}
