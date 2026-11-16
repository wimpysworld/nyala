package engine

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

var chromiumNames = []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "chrome"}

type browserCandidate struct {
	path   string
	family string
}

type browserDiscovery struct {
	goos, home, username string
	getenv               func(string) string
	lookPath             func(string) (string, error)
}

func resolveExecutable(flag string) (string, error) {
	home, _ := os.UserHomeDir()
	d := browserDiscovery{goos: runtime.GOOS, home: home, getenv: os.Getenv, lookPath: exec.LookPath}
	if account, err := user.Current(); err == nil {
		d.username = account.Username
	}
	return d.resolve(flag)
}

func (d browserDiscovery) resolve(flag string) (string, error) {
	for _, override := range []string{flag, d.getenv("NYALA_BROWSER"), d.getenv("CHROME_PATH")} {
		if override != "" {
			path, err := d.lookPath(override)
			if err != nil {
				return "", fmt.Errorf("selected browser %q: %w", override, err)
			}
			return filepath.Abs(path)
		}
	}
	for _, candidate := range d.candidates() {
		if path, err := d.lookPath(candidate.path); err == nil {
			return filepath.Abs(path)
		}
	}
	return "", fmt.Errorf("no Chromium found: set --browser, NYALA_BROWSER or CHROME_PATH; searched Chrome/Chromium on PATH, native installations and active Nix profiles, then Brave and Edge")
}

func (d browserDiscovery) candidates() []browserCandidate {
	var candidates []browserCandidate
	seen := make(map[string]bool)
	for _, family := range []string{"chromium", "brave", "edge"} {
		paths := append([]string{}, d.names(family)...)
		paths = append(paths, d.nativePaths(family)...)
		if d.goos == "linux" || d.goos == "darwin" {
			for _, dir := range d.nixDirs() {
				for _, name := range d.names(family) {
					paths = append(paths, d.join(dir, name))
				}
			}
		}
		paths = append(paths, d.packagedPaths(family)...)
		for _, path := range paths {
			key := path
			if d.goos == "windows" {
				key = strings.ToLower(key)
			}
			if !seen[key] {
				seen[key] = true
				candidates = append(candidates, browserCandidate{path: path, family: family})
			}
		}
	}
	return candidates
}

func (d browserDiscovery) names(family string) []string {
	switch family {
	case "brave":
		if d.goos == "windows" {
			return []string{"brave"}
		}
		return []string{"brave-browser", "brave-browser-stable", "brave"}
	case "edge":
		if d.goos == "windows" {
			return []string{"msedge"}
		}
		return []string{"microsoft-edge", "microsoft-edge-stable"}
	default:
		return chromiumNames
	}
}

func (d browserDiscovery) join(parts ...string) string {
	if d.goos == "windows" {
		return strings.ReplaceAll(strings.Join(parts, "/"), "/", `\`)
	}
	return filepath.Join(parts...)
}

func (d browserDiscovery) absolute(path string) bool {
	if d.goos == "windows" {
		return len(path) >= 3 && path[1] == ':' && (path[2] == '\\' || path[2] == '/') || strings.HasPrefix(path, `\\`)
	}
	return filepath.IsAbs(path)
}

func (d browserDiscovery) nativePaths(family string) []string {
	var paths []string
	switch d.goos {
	case "linux":
		for _, name := range d.names(family) {
			paths = append(paths, d.join("/usr/bin", name))
		}
		wrappers := map[string]string{"chromium": "/opt/google/chrome/google-chrome", "brave": "/opt/brave.com/brave/brave-browser", "edge": "/opt/microsoft/msedge/microsoft-edge"}
		paths = append(paths, wrappers[family])
	case "darwin":
		bundles := map[string][]string{"chromium": {"Google Chrome", "Chromium"}, "brave": {"Brave Browser"}, "edge": {"Microsoft Edge"}}
		for _, name := range bundles[family] {
			paths = append(paths, d.join("/Applications", name+".app", "Contents", "MacOS", name))
			if d.absolute(d.home) {
				paths = append(paths, d.join(d.home, "Applications", name+".app", "Contents", "MacOS", name))
			}
		}
	case "windows":
		suffixes := map[string][]string{"chromium": {"Google/Chrome/Application/chrome.exe", "Chromium/Application/chrome.exe"}, "brave": {"BraveSoftware/Brave-Browser/Application/brave.exe"}, "edge": {"Microsoft/Edge/Application/msedge.exe"}}
		roots := []string{"ProgramFiles", "ProgramFiles(x86)", "LOCALAPPDATA", "ProgramW6432"}
		if family == "edge" {
			roots[0], roots[1] = roots[1], roots[0]
		}
		for _, suffix := range suffixes[family] {
			for _, env := range roots {
				if root := d.getenv(env); d.absolute(root) {
					paths = append(paths, d.join(strings.TrimRight(root, `/\`), suffix))
				}
			}
		}
	}
	return paths
}

func (d browserDiscovery) nixDirs() []string {
	var dirs []string
	if root := d.getenv("NIX_STATE_HOME"); d.absolute(root) {
		dirs = append(dirs, d.join(root, "profile/bin"))
	}
	state := d.getenv("XDG_STATE_HOME")
	if !d.absolute(state) && d.absolute(d.home) {
		state = d.join(d.home, ".local/state")
	}
	if d.absolute(state) {
		dirs = append(dirs, d.join(state, "nix/profile/bin"))
	}
	if d.absolute(d.home) {
		dirs = append(dirs, d.join(d.home, ".nix-profile/bin"))
	}
	if d.username != "" && d.username != "." && d.username != ".." && !strings.ContainsAny(d.username, `/\`) {
		dirs = append(dirs, d.join("/etc/profiles/per-user", d.username, "bin"))
	}
	return append(dirs, "/run/current-system/sw/bin", "/nix/var/nix/profiles/default/bin")
}

func (d browserDiscovery) packagedPaths(family string) []string {
	if d.goos != "linux" {
		return nil
	}
	var paths []string
	snap := map[string]string{"chromium": "chromium", "brave": "brave"}
	if name := snap[family]; name != "" {
		paths = append(paths, "/snap/bin/"+name, "/var/lib/snapd/snap/bin/"+name)
	}
	ids := map[string][]string{"chromium": {"org.chromium.Chromium", "com.google.Chrome"}, "brave": {"com.brave.Browser"}, "edge": {"com.microsoft.Edge"}}
	for _, id := range ids[family] {
		paths = append(paths, "/var/lib/flatpak/exports/bin/"+id)
		if d.absolute(d.home) {
			paths = append(paths, d.join(d.home, ".local/share/flatpak/exports/bin", id))
		}
	}
	return paths
}

func browserFamily(path string) string {
	name := strings.ToLower(filepath.Base(strings.ReplaceAll(path, `\`, "/")))
	name = strings.TrimSuffix(name, ".exe")
	switch name {
	case "chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "chrome", "google chrome", "org.chromium.chromium", "com.google.chrome":
		return "chromium"
	case "brave", "brave-browser", "brave-browser-stable", "brave browser", "com.brave.browser":
		return "brave"
	case "msedge", "microsoft-edge", "microsoft-edge-stable", "microsoft edge", "com.microsoft.edge":
		return "edge"
	default:
		return ""
	}
}

func (e *ChromiumEngine) resolveBrowser(flag string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.execPath != "" {
		return e.execPath, nil
	}
	path, err := resolveExecutable(flag)
	if err != nil {
		return "", err
	}
	e.execPath = path
	if family := browserFamily(path); e.defaultProfile && (family == "brave" || family == "edge") {
		e.profileDir += "-" + family
	}
	return path, nil
}

func managedBrowserError(path string, err error) error {
	hint := ""
	if strings.Contains(path, "/snap/bin/") || strings.Contains(path, "/flatpak/exports/bin/") {
		hint = "; this confined launcher must permit the selected profile and CDP files (private /tmp can differ); use a native browser or a wrapper with the required permissions"
	}
	return fmt.Errorf("browser %q%s: %w", path, hint, err)
}
