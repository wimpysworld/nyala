package engine

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolvedBrowserProfilePolicy(t *testing.T) {
	cases := []struct {
		name       string
		browser    string
		profile    string
		wantSuffix string
	}{
		{"Chromium retains the existing default", "chromium", "default", "profile"},
		{"Chrome retains the existing default", "google-chrome-stable", "default", "profile"},
		{"Brave separates the default", "brave-browser", "default", "profile-brave"},
		{"Edge separates the default", "microsoft-edge", "default", "profile-edge"},
		{"Brave preserves the explicit flag", "brave-browser", "flag", ""},
		{"Edge preserves the environment override", "microsoft-edge", "environment", ""},
		{"Brave leaves private mode private", "brave-browser", "private", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dataHome := t.TempDir()
			explicit := filepath.Join(t.TempDir(), "chosen-profile")
			t.Setenv("NYALA_PROFILE", "")
			t.Setenv("XDG_DATA_HOME", dataHome)
			t.Setenv("NYALA_BROWSER", "")
			t.Setenv("CHROME_PATH", "")
			name := tc.browser
			if runtime.GOOS == "windows" {
				name += ".exe"
			}
			_, browserPath := fakeExecutable(t, name, "")
			e := New("")
			wantProfile := ""
			switch tc.profile {
			case "default":
				wantProfile = filepath.Join(dataHome, "nyala", tc.wantSuffix)
				if err := e.SetProfileDir(""); err != nil {
					t.Fatal(err)
				}
			case "flag":
				wantProfile = explicit
				if err := e.SetProfileDir(explicit); err != nil {
					t.Fatal(err)
				}
			case "environment":
				wantProfile = explicit
				t.Setenv("NYALA_PROFILE", explicit)
				if err := e.SetProfileDir(""); err != nil {
					t.Fatal(err)
				}
			case "private":
				// A configured ambient profile must not enable persistence.
				t.Setenv("NYALA_PROFILE", explicit)
			}
			got, err := e.resolveBrowser(browserPath)
			if err != nil {
				t.Fatalf("resolveBrowser(%q): %v", browserPath, err)
			}
			if got != browserPath {
				t.Errorf("browser = %q, want %q", got, browserPath)
			}
			if e.profileDir != wantProfile {
				t.Errorf("profile = %q, want %q", e.profileDir, wantProfile)
			}
			if e.profileGuard != nil {
				t.Fatal("browser resolution opened the profile guard")
			}
			for _, path := range []string{explicit, filepath.Join(dataHome, "nyala")} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("profile path %q after resolution: %v, want absent", path, err)
				}
			}
		})
	}
}

func TestResolvedBrowserSelectionSurvivesEnvironmentChanges(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("NYALA_PROFILE", "")
	t.Setenv("CHROME_PATH", "")
	name := "brave-browser"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	_, firstPath := fakeExecutable(t, name, "")
	t.Setenv("NYALA_BROWSER", filepath.Join(t.TempDir(), "missing-browser"))
	e := New("")
	if err := e.SetProfileDir(""); err != nil {
		t.Fatal(err)
	}
	initialProfile := e.profileDir
	if got, err := e.resolveBrowser(""); err == nil || got != "" {
		t.Fatalf("missing browser resolution = (%q, %v), want failure", got, err)
	}
	if e.profileDir != initialProfile {
		t.Fatalf("failed resolution changed profile from %q to %q", initialProfile, e.profileDir)
	}
	t.Setenv("NYALA_BROWSER", firstPath)
	first, err := e.resolveBrowser("")
	if err != nil {
		t.Fatalf("initial browser resolution: %v", err)
	}
	profile := e.profileDir
	if !strings.HasSuffix(profile, "profile-brave") {
		t.Fatalf("initial profile = %q, want the Brave default", profile)
	}

	// Recovery must not switch browser families when the environment changes.
	t.Setenv("NYALA_BROWSER", filepath.Join(t.TempDir(), "missing-chrome"))
	t.Setenv("NYALA_PROFILE", filepath.Join(t.TempDir(), "different-profile"))
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	if err := os.Remove(firstPath); err != nil {
		t.Fatalf("remove selected launcher: %v", err)
	}
	again, err := e.resolveBrowser("")
	if err != nil {
		t.Fatalf("cached browser resolution: %v", err)
	}
	if first != firstPath || again != firstPath {
		t.Errorf("selected browsers = (%q, %q), want %q twice", first, again, firstPath)
	}
	if e.profileDir != profile {
		t.Errorf("profile after recovery resolution = %q, want %q", e.profileDir, profile)
	}
}
