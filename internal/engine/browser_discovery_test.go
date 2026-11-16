package engine

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestBrowserDiscoveryOverridePrecedence(t *testing.T) {
	cases := []struct {
		name   string
		flag   string
		nyala  string
		chrome string
		want   string
	}{
		{"flag overrides both environment variables", "flag-wrapper", "nyala-wrapper", "chrome-wrapper", "flag-wrapper"},
		{"Nyala overrides Chrome", "", "nyala-wrapper", "chrome-wrapper", "nyala-wrapper"},
		{"Chrome overrides automatic discovery", "", "", "chrome-wrapper", "chrome-wrapper"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolved := filepath.Join(t.TempDir(), tc.want)
			var attempts []string
			d := browserDiscovery{
				goos: "linux",
				getenv: func(name string) string {
					return map[string]string{"NYALA_BROWSER": tc.nyala, "CHROME_PATH": tc.chrome}[name]
				},
				lookPath: func(name string) (string, error) {
					attempts = append(attempts, name)
					if name == tc.want {
						return resolved, nil
					}
					return "", exec.ErrNotFound
				},
			}
			got, err := d.resolve(tc.flag)
			if err != nil {
				t.Fatalf("resolve(%q): %v", tc.flag, err)
			}
			if got != resolved || !slices.Equal(attempts, []string{tc.want}) {
				t.Errorf("browser = %q, lookups = %v, want %q from only %q", got, attempts, resolved, tc.want)
			}
		})
	}
}

func TestBrowserDiscoveryInvalidOverrideDoesNotFallBack(t *testing.T) {
	for _, source := range []string{"flag", "NYALA_BROWSER", "CHROME_PATH"} {
		t.Run(source, func(t *testing.T) {
			const missing = "chosen-but-missing-browser"
			flag := ""
			if source == "flag" {
				flag = missing
			}
			var attempts []string
			d := browserDiscovery{
				goos: "linux",
				getenv: func(name string) string {
					if name == source {
						return missing
					}
					return ""
				},
				lookPath: func(name string) (string, error) {
					attempts = append(attempts, name)
					if name == missing {
						return "", exec.ErrNotFound
					}
					return filepath.Join(t.TempDir(), "available-chromium"), nil
				},
			}
			got, err := d.resolve(flag)
			if err == nil || got != "" {
				t.Fatalf("resolve(%q) = (%q, %v), want the override error", flag, got, err)
			}
			if !strings.Contains(err.Error(), missing) {
				t.Errorf("error = %q, want the failed override named", err)
			}
			if !slices.Equal(attempts, []string{missing}) {
				t.Errorf("lookups = %v, want no automatic fallback", attempts)
			}
		})
	}
}

func TestBrowserDiscoveryPlatformPriority(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	programFiles := `C:\Program Files`
	programFilesX86 := `C:\Program Files (x86)`
	localAppData := `C:\Users\browser\AppData\Local`
	chromeApp := "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	windowsChrome := programFiles + `\Google\Chrome\Application\chrome.exe`
	windowsUserChrome := localAppData + `\Google\Chrome\Application\chrome.exe`
	windowsBrave := programFiles + `\BraveSoftware\Brave-Browser\Application\brave.exe`
	windowsEdge := programFilesX86 + `\Microsoft\Edge\Application\msedge.exe`
	nixChrome := filepath.Join(home, ".nix-profile", "bin", "chromium")
	userBraveApp := filepath.Join(home, "Applications", "Brave Browser.app", "Contents", "MacOS", "Brave Browser")
	cases := []struct {
		name      string
		goos      string
		available []string
		want      string
	}{
		{"Chromium PATH beats Chrome and fallbacks", "linux", []string{"chromium", "google-chrome", "brave-browser", "microsoft-edge"}, "chromium"},
		{"Chrome PATH beats Brave PATH", "linux", []string{"google-chrome-stable", "brave-browser"}, "google-chrome-stable"},
		{"Brave is used without a primary browser", "linux", []string{"brave-browser", "microsoft-edge"}, "brave-browser"},
		{"Edge is used when no earlier browser exists", "linux", []string{"microsoft-edge"}, "microsoft-edge"},
		{"native Linux Chrome beats Brave PATH", "linux", []string{"/opt/google/chrome/google-chrome", "brave-browser"}, "/opt/google/chrome/google-chrome"},
		{"active Nix Chromium beats Brave PATH", "linux", []string{nixChrome, "brave-browser"}, nixChrome},
		{"packaged Chromium beats Brave PATH", "linux", []string{"/var/lib/flatpak/exports/bin/org.chromium.Chromium", "brave-browser"}, "/var/lib/flatpak/exports/bin/org.chromium.Chromium"},
		{"macOS native Chrome beats Brave PATH", "darwin", []string{chromeApp, "brave-browser"}, chromeApp},
		{"macOS user Brave is a fallback", "darwin", []string{userBraveApp}, userBraveApp},
		{"macOS native Edge is a fallback", "darwin", []string{"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge"}, "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge"},
		{"Windows native Chrome beats Brave PATH", "windows", []string{windowsChrome, "brave"}, windowsChrome},
		{"Windows per-user Chrome beats Edge PATH", "windows", []string{windowsUserChrome, "msedge"}, windowsUserChrome},
		{"Windows native Brave is a fallback", "windows", []string{windowsBrave}, windowsBrave},
		{"Windows x86 Edge is a fallback", "windows", []string{windowsEdge}, windowsEdge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			found := make(map[string]string)
			for i, candidate := range tc.available {
				found[candidate] = filepath.Join(root, tc.goos, string(rune('a'+i)), "launcher")
			}
			d := browserDiscovery{
				goos: tc.goos,
				home: home,
				getenv: func(name string) string {
					return map[string]string{
						"ProgramFiles": programFiles, "ProgramFiles(x86)": programFilesX86,
						"LOCALAPPDATA": localAppData,
					}[name]
				},
				lookPath: func(name string) (string, error) {
					if path, ok := found[name]; ok {
						return path, nil
					}
					return "", exec.ErrNotFound
				},
			}
			got, err := d.resolve("")
			if err != nil {
				t.Fatalf("resolve(): %v", err)
			}
			if got != found[tc.want] {
				t.Errorf("browser = %q, want %q selected through %q", got, found[tc.want], tc.want)
			}
		})
	}
}

func TestBrowserDiscoverySkipsUnsetOrRelativeRoots(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			for _, root := range []string{"", "relative-root"} {
				d := browserDiscovery{goos: goos, home: root, getenv: func(string) string { return root }}
				for _, candidate := range d.candidates() {
					path := candidate.path
					if strings.ContainsAny(path, `/\`) && !filepath.IsAbs(path) {
						t.Errorf("root %q produced relative install path %q", root, path)
					}
					if strings.HasPrefix(path, "/.nix-profile/") || strings.HasPrefix(path, "/.local/") {
						t.Errorf("empty home produced home-relative install path %q", path)
					}
				}
			}
		})
	}
}

func TestBrowserDiscoveryDoesNotSelectPreviewChannels(t *testing.T) {
	preview := map[string]bool{
		"google-chrome-beta": true, "google-chrome-unstable": true,
		"chromium-dev": true, "brave-browser-beta": true, "brave-browser-nightly": true,
		"microsoft-edge-beta": true, "microsoft-edge-dev": true,
		"/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary": true,
	}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			d := browserDiscovery{
				goos:   goos,
				getenv: func(string) string { return "" },
				lookPath: func(name string) (string, error) {
					if preview[name] {
						return filepath.Join(t.TempDir(), "preview-browser"), nil
					}
					return "", exec.ErrNotFound
				},
			}
			got, err := d.resolve("")
			if err == nil || got != "" {
				t.Fatalf("preview-only discovery = (%q, %v), want missing-browser error", got, err)
			}
			for _, hint := range []string{"--browser", "NYALA_BROWSER", "CHROME_PATH"} {
				if !strings.Contains(err.Error(), hint) {
					t.Errorf("missing-browser error = %q, want %s hint", err, hint)
				}
			}
		})
	}
}

func TestBrowserDiscoveryChecksLaunchers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix executable permissions are not available on Windows")
	}
	d := browserDiscovery{goos: runtime.GOOS, getenv: func(string) string { return "" }, lookPath: exec.LookPath}
	_, wrapper := fakeExecutable(t, "custom-wrapper", "exit 0\n")
	notExecutable := filepath.Join(t.TempDir(), "not-executable")
	if err := os.WriteFile(notExecutable, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{notExecutable, t.TempDir()} {
		if got, err := d.resolve(path); err == nil || got != "" {
			t.Errorf("resolve(%q) = (%q, %v), want invalid executable error", path, got, err)
		}
	}
	if got, err := d.resolve(wrapper); err != nil || got != wrapper {
		t.Errorf("resolve(wrapper) = (%q, %v), want executable shell wrapper %q", got, err, wrapper)
	}
	alias := filepath.Join(t.TempDir(), "launcher-alias")
	if err := os.Symlink(wrapper, alias); err != nil {
		t.Fatal(err)
	}
	if got, err := d.resolve(alias); err != nil || got != alias {
		t.Errorf("resolve(alias) = (%q, %v), want selected wrapper symlink %q", got, err, alias)
	}
}

func TestBrowserDiscoveryReturnsAbsoluteLauncher(t *testing.T) {
	const relative = "relative-wrapper"
	d := browserDiscovery{
		goos:   "linux",
		getenv: func(string) string { return "" },
		lookPath: func(name string) (string, error) {
			if name != relative {
				return "", errors.New("unexpected discovery lookup")
			}
			return filepath.Join("wrappers", name), nil
		},
	}
	want, err := filepath.Abs(filepath.Join("wrappers", relative))
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.resolve(relative)
	if err != nil || got != want {
		t.Errorf("resolve(%q) = (%q, %v), want absolute launcher %q", relative, got, err, want)
	}
}
