package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPurgeSessionState(t *testing.T) {
	t.Run("removes only the Sessions directory", func(t *testing.T) {
		profile := t.TempDir()
		sessions := filepath.Join(profile, "Default", "Sessions")
		if err := os.MkdirAll(sessions, 0o700); err != nil {
			t.Fatalf("create sessions dir: %v", err)
		}
		tabs := filepath.Join(sessions, "Tabs_13432816619517769")
		if err := os.WriteFile(tabs, []byte("stale tab state"), 0o600); err != nil {
			t.Fatalf("write tabs file: %v", err)
		}
		prefs := filepath.Join(profile, "Default", "Preferences")
		if err := os.WriteFile(prefs, []byte("{}"), 0o600); err != nil {
			t.Fatalf("write preferences: %v", err)
		}
		if err := purgeSessionState(profile); err != nil {
			t.Fatalf("purgeSessionState() = %v, want nil", err)
		}
		if _, err := os.Stat(sessions); !os.IsNotExist(err) {
			t.Fatalf("sessions dir Stat error = %v, want path absent", err)
		}
		if _, err := os.Stat(prefs); err != nil {
			t.Fatalf("preferences Stat error = %v, want the file kept", err)
		}
	})

	t.Run("missing directory is a no-op", func(t *testing.T) {
		if err := purgeSessionState(t.TempDir()); err != nil {
			t.Fatalf("purgeSessionState() = %v, want nil", err)
		}
	})

	t.Run("empty profile path is a no-op", func(t *testing.T) {
		if err := purgeSessionState(""); err != nil {
			t.Fatalf("purgeSessionState(\"\") = %v, want nil", err)
		}
	})
}
