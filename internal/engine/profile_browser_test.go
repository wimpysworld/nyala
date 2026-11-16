package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProfileBrowserMismatchDoesNotAdoptOrPurge(t *testing.T) {
	for _, exclusive := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared", true: "exclusive"}[exclusive], func(t *testing.T) {
			dir := t.TempDir()
			guard := &profileGuard{file: openLockFD(t, dir)}
			if won, err := guard.tryExclusive(); err != nil || !won {
				t.Fatalf("exclusive lock = %v, %v", won, err)
			}
			cfg := allocatorConfig{execPath: filepath.Join(t.TempDir(), "chromium"), profileDir: dir}
			if err := checkProfileBrowser(guard, cfg, true); err != nil {
				t.Fatal(err)
			}
			if !exclusive {
				if err := guard.downgrade(); err != nil {
					t.Fatal(err)
				}
			}
			sessions := filepath.Join(dir, "Default", "Sessions")
			if err := os.MkdirAll(sessions, 0o700); err != nil {
				t.Fatal(err)
			}
			keep := filepath.Join(sessions, "keep")
			if err := os.WriteFile(keep, []byte("unchanged"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg.execPath = filepath.Join(t.TempDir(), "brave")
			connected := false
			connect := func(context.Context, string, time.Duration) (context.Context, context.CancelFunc, error) {
				connected = true
				return context.Background(), func() {}, nil
			}
			var err error
			if exclusive {
				_, _, err = spawnShared(guard, cfg, connect)
			} else {
				client := &profileGuard{file: openLockFD(t, dir)}
				_, _, err = attachShared(client, cfg, connect)
			}
			if err == nil || !strings.Contains(err.Error(), "separate --profile") {
				t.Fatalf("mismatch error = %v", err)
			}
			if connected {
				t.Fatal("connected to an incompatible browser")
			}
			if content, err := os.ReadFile(keep); err != nil || string(content) != "unchanged" {
				t.Fatalf("session changed: %q, %v", content, err)
			}
			if marker, err := os.ReadFile(filepath.Join(dir, profileBrowserName)); err != nil || string(marker) != "chromium" {
				t.Fatalf("identity changed: %q, %v", marker, err)
			}
		})
	}
}

func TestProfileBrowserLegacyOfflinePolicy(t *testing.T) {
	for _, tc := range []struct {
		name, browser        string
		populated, wantError bool
	}{
		{"primary retains existing profile", "chrome", true, false},
		{"Brave keeps new profile", "brave", false, false},
		{"Brave refuses unidentified data", "brave", true, true},
		{"Edge refuses unidentified data", "msedge", true, true},
		{"custom refuses unidentified data", "custom", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			guard := &profileGuard{file: openLockFD(t, dir)}
			if won, err := guard.tryExclusive(); err != nil || !won {
				t.Fatalf("exclusive lock = %v, %v", won, err)
			}
			if tc.populated {
				if err := os.WriteFile(filepath.Join(dir, "Local State"), []byte("existing"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg := allocatorConfig{execPath: filepath.Join(t.TempDir(), tc.browser), profileDir: dir}
			err := checkProfileBrowser(guard, cfg, true)
			if (err != nil) != tc.wantError {
				t.Fatalf("legacy identity error = %v, want error %v", err, tc.wantError)
			}
			marker, err := os.ReadFile(filepath.Join(dir, profileBrowserName))
			if tc.wantError {
				if !os.IsNotExist(err) {
					t.Fatalf("rejected legacy profile gained marker %q, %v", marker, err)
				}
			} else if err != nil || string(marker) != browserIdentity(cfg.execPath) {
				t.Fatalf("marker = %q, %v", marker, err)
			}
		})
	}
}

func TestProfileBrowserRejectsUnidentifiedLiveOwner(t *testing.T) {
	dir := t.TempDir()
	guard := &profileGuard{file: openLockFD(t, dir)}
	if won, err := guard.tryExclusive(); err != nil || !won {
		t.Fatalf("exclusive lock = %v, %v", won, err)
	}
	if err := guard.writePID(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"chromium", "brave", "msedge"} {
		cfg := allocatorConfig{execPath: filepath.Join(t.TempDir(), name), profileDir: dir}
		if err := checkProfileBrowser(guard, cfg, true); err == nil {
			t.Fatalf("unidentified live owner accepted for %s", name)
		}
	}
}
