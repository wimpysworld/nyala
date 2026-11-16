package engine

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("NYALA_TEST_PROFILE_OWNER") == "1" {
		os.Args = []string{os.Args[0], "--test.run=^TestProfileBrowserOwnerProcess$"}
	}
	os.Exit(m.Run())
}

func TestProfileBrowserOwnerProcess(t *testing.T) {
	if os.Getenv("NYALA_TEST_PROFILE_OWNER") != "1" {
		return
	}
	if _, err := os.Stdout.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal(err)
	}
}

func profileBrowserOwner(t *testing.T, command, profileDir string, args ...string) int {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	args = append(args, "--test.run=^TestProfileBrowserOwnerProcess$", "--")
	if profileDir != "" {
		args = append(args, "--user-data-dir="+profileDir)
	}
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Args[0] = command
	cmd.Env = append(os.Environ(), "NYALA_TEST_PROFILE_OWNER=1")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = input.Close()
		if err := cmd.Wait(); err != nil {
			t.Errorf("profile owner did not exit cleanly: %v", err)
		}
	})
	if _, err := io.ReadFull(output, make([]byte, 1)); err != nil {
		t.Fatalf("wait for profile owner: %v", err)
	}
	return cmd.Process.Pid
}

func TestProfileBrowserMatchingMarkerChecksLiveOwner(t *testing.T) {
	for _, tc := range []struct {
		name, selected, owner              string
		wrongProfile, reject, connectFails bool
		args                               []string
	}{
		{"different family", "chromium", "brave", false, true, false, nil},
		{"different family recovery", "chromium", "brave", false, true, true, nil},
		{"unrecognised owner", "chromium", "unrelated", false, true, false, nil},
		{"wrong profile", "chromium", "chrome", true, true, false, nil},
		{"same family alias", "chromium", "google-chrome", false, false, false, nil},
		{"Brave alias", "brave-browser", "brave", false, false, false, nil},
		{"Edge alias", "microsoft-edge", "msedge", false, false, false, nil},
		{"Nix Chromium wrapper", "chromium", ".chromium-wrapped", false, false, false, nil},
		{"rewritten command line", "chromium", ".chromium-wrapped --headless", false, false, false, nil},
		{"Nix Brave wrapper", "brave-browser", ".brave-wrapped", false, false, false, nil},
		{"Bash Brave wrapper", "brave-browser", "bash", false, false, false, []string{"/opt/brave.com/brave/brave-browser"}},
		{"Nix Bash Brave wrapper", "brave-browser", "bash", false, false, false, []string{"/nix/store/brave/bin/.brave-wrapped"}},
		{"shell Brave wrapper", "brave-browser", "sh", false, false, false, []string{"/opt/brave.com/brave/brave-browser"}},
		{"Bash wrapper wrong family", "chromium", "bash", false, true, false, []string{"/nix/store/brave/bin/.brave-wrapped"}},
		{"Bash wrapper wrong profile", "brave-browser", "bash", true, true, false, []string{"/nix/store/brave/bin/.brave-wrapped"}},
		{"Bash unrelated script", "brave-browser", "bash", false, true, false, []string{"/opt/unrelated", "/opt/brave"}},
		{"Bash command payload", "brave-browser", "bash", false, true, false, []string{"-c", "/opt/brave"}},
		{"Bash stdin payload", "brave-browser", "bash", false, true, false, []string{"-s", "/opt/brave"}},
		{"Bash option value", "brave-browser", "bash", false, true, false, []string{"--rcfile", "/opt/brave"}},
		{"Bash option with browser basename", "brave-browser", "bash", false, true, false, []string{"--rcfile=/opt/brave"}},
		{"unrelated interpreter", "brave-browser", "unrelated", false, true, false, []string{"/opt/brave"}},
		{"Nix Edge wrapper", "microsoft-edge", ".msedge-wrapped", false, false, false, nil},
		{"custom wrapper", "custom-launcher", "chrome", false, false, false, nil},
		{"custom wrong profile", "custom-launcher", "chrome", true, true, false, nil},
		{"Flatpak Chromium", "org.chromium.Chromium", "flatpak", false, false, false, []string{"run", "--branch=stable", "--arch=x86_64", "org.chromium.Chromium"}},
		{"Flatpak Chrome", "com.google.Chrome", "flatpak", false, false, false, []string{"run", "--branch=stable", "--arch=aarch64", "com.google.Chrome"}},
		{"Flatpak Brave", "com.brave.Browser", "flatpak", false, false, false, []string{"run", "--branch=stable", "--arch=x86_64", "com.brave.Browser"}},
		{"Flatpak Edge", "com.microsoft.Edge", "flatpak", false, false, false, []string{"run", "--branch=stable", "--arch=x86_64", "com.microsoft.Edge"}},
		{"Flatpak without options", "chromium", "flatpak", false, false, false, []string{"run", "org.chromium.Chromium"}},
		{"Nix Flatpak wrapper", "chromium", ".flatpak-wrapped", false, false, false, []string{"run", "--branch=stable", "--arch=x86_64", "org.chromium.Chromium"}},
		{"Bash Flatpak wrapper", "chromium", "bash", false, false, false, []string{"/nix/store/flatpak/bin/.flatpak-wrapped", "run", "--branch=stable", "--arch=x86_64", "org.chromium.Chromium"}},
		{"Bash Flatpak later known ID", "chromium", "bash", false, true, false, []string{"/usr/bin/flatpak", "run", "org.example.Unknown", "org.chromium.Chromium"}},
		{"Flatpak wrong family", "chromium", "flatpak", false, true, false, []string{"run", "com.brave.Browser"}},
		{"Flatpak wrong profile", "chromium", "flatpak", true, true, false, []string{"run", "org.chromium.Chromium"}},
		{"Flatpak unknown app", "chromium", "flatpak", false, true, false, []string{"run", "org.example.Unknown"}},
		{"Flatpak later known ID", "chromium", "flatpak", false, true, false, []string{"run", "org.example.Unknown", "org.chromium.Chromium"}},
		{"Flatpak ID in option value", "chromium", "flatpak", false, true, false, []string{"run", "--branch=org.chromium.Chromium", "org.example.Unknown"}},
		{"Flatpak separate option value", "chromium", "flatpak", false, true, false, []string{"run", "--command", "org.chromium.Chromium", "org.example.Unknown"}},
		{"Flatpak ID substring", "chromium", "flatpak", false, true, false, []string{"run", "org.chromium.Chromium.Unknown"}},
		{"Flatpak wrong subcommand", "chromium", "flatpak", false, true, false, []string{"info", "org.chromium.Chromium"}},
		{"Flatpak missing ID", "chromium", "flatpak", false, true, false, []string{"run", "--branch=stable"}},
	} {
		if len(tc.args) > 0 && runtime.GOOS != "linux" {
			continue
		}
		for _, exclusive := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/shared", true: "/exclusive"}[exclusive], func(t *testing.T) {
				dir := t.TempDir()
				guard := &profileGuard{file: openLockFD(t, dir)}
				if won, err := guard.tryExclusive(); err != nil || !won {
					t.Fatalf("exclusive lock = %v, %v", won, err)
				}
				cfg := allocatorConfig{execPath: filepath.Join(t.TempDir(), tc.selected), profileDir: dir}
				if err := checkProfileBrowser(guard, cfg, true); err != nil {
					t.Fatal(err)
				}
				ownerProfile := dir
				if tc.wrongProfile {
					ownerProfile += "-other"
				}
				pid := profileBrowserOwner(t, filepath.Join(t.TempDir(), tc.owner), ownerProfile, tc.args...)
				if err := guard.writePID(pid); err != nil {
					t.Fatal(err)
				}
				keep := filepath.Join(dir, "Default", "Sessions", "keep")
				if err := os.MkdirAll(filepath.Dir(keep), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(keep, []byte("unchanged"), 0o600); err != nil {
					t.Fatal(err)
				}
				port := []byte("9333\n/devtools/browser/live\n")
				if err := os.WriteFile(devToolsActivePortPath(dir), port, 0o600); err != nil {
					t.Fatal(err)
				}
				connected := false
				connect := func(context.Context, string, time.Duration) (context.Context, context.CancelFunc, error) {
					connected = true
					if tc.connectFails {
						return nil, nil, errors.New("browser does not answer")
					}
					return context.Background(), func() {}, nil
				}
				var err error
				if exclusive {
					_, _, err = spawnShared(guard, cfg, connect)
				} else {
					if err := guard.downgrade(); err != nil {
						t.Fatal(err)
					}
					client := &profileGuard{file: openLockFD(t, dir)}
					_, _, err = attachShared(client, cfg, connect)
				}
				if (err != nil) != tc.reject || connected == tc.reject {
					t.Fatalf("owner check: error = %v, connected = %v, want rejection = %v", err, connected, tc.reject)
				}
				if content, err := os.ReadFile(keep); err != nil || string(content) != "unchanged" {
					t.Fatalf("session changed: %q, %v", content, err)
				}
				if content, err := os.ReadFile(devToolsActivePortPath(dir)); err != nil || string(content) != string(port) {
					t.Fatalf("port file changed: %q, %v", content, err)
				}
				if got, recorded := guard.readPID(); !recorded || got != pid {
					t.Fatalf("live PID changed: %d, %v, want %d", got, recorded, pid)
				}
			})
		}
	}
}

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

func TestProfileBrowserPathAliasDoesNotPurge(t *testing.T) {
	for _, marked := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "marked"}[marked], func(t *testing.T) {
			dir := t.TempDir()
			guard := &profileGuard{file: openLockFD(t, dir)}
			if won, err := guard.tryExclusive(); err != nil || !won {
				t.Fatalf("exclusive lock = %v, %v", won, err)
			}
			cfg := allocatorConfig{execPath: filepath.Join(t.TempDir(), "chromium"), profileDir: dir}
			if marked {
				if err := checkProfileBrowser(guard, cfg, true); err != nil {
					t.Fatal(err)
				}
			}
			alias := dir + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.Base(dir)
			pid := profileBrowserOwner(t, cfg.execPath, alias)
			if err := guard.writePID(pid); err != nil {
				t.Fatal(err)
			}
			keep := filepath.Join(dir, "Default", "Sessions", "keep")
			if err := os.MkdirAll(filepath.Dir(keep), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(keep, []byte("unchanged"), 0o600); err != nil {
				t.Fatal(err)
			}
			connect := func(context.Context, string, time.Duration) (context.Context, context.CancelFunc, error) {
				t.Fatal("connected to an unverified profile owner")
				return nil, nil, nil
			}
			if _, _, err := spawnShared(guard, cfg, connect); err == nil || !strings.Contains(err.Error(), "matching identity") {
				t.Errorf("profile alias rejection = %v", err)
			}
			if content, err := os.ReadFile(keep); err != nil || string(content) != "unchanged" {
				t.Errorf("live session changed: %q, %v", content, err)
			}
			if got, recorded := guard.readPID(); !recorded || got != pid {
				t.Errorf("live PID changed: %d, %v, want %d", got, recorded, pid)
			}
		})
	}
}

func TestProfileBrowserReusedPIDLaunchesFresh(t *testing.T) {
	for _, marked := range []bool{false, true} {
		for _, exclusive := range []bool{false, true} {
			name := map[bool]string{false: "legacy", true: "marked"}[marked]
			name += map[bool]string{false: "/shared", true: "/exclusive"}[exclusive]
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				guard := &profileGuard{file: openLockFD(t, dir)}
				if won, err := guard.tryExclusive(); err != nil || !won {
					t.Fatalf("exclusive lock = %v, %v", won, err)
				}
				browserPath, readArgv := argvRecorder(t)
				cfg := allocatorConfig{execPath: browserPath, profileDir: dir, scale: 1}
				if marked {
					if err := checkProfileBrowser(guard, cfg, true); err != nil {
						t.Fatal(err)
					}
				}
				pid := profileBrowserOwner(t, "unrelated", "")
				if err := guard.writePID(pid); err != nil {
					t.Fatal(err)
				}
				port := []byte("9333\n/devtools/browser/stale\n")
				if err := os.WriteFile(devToolsActivePortPath(dir), port, 0o600); err != nil {
					t.Fatal(err)
				}
				connect := func(context.Context, string, time.Duration) (context.Context, context.CancelFunc, error) {
					t.Fatal("connected through a stale PID and port file")
					return nil, nil, nil
				}
				if exclusive {
					_, _, err := spawnShared(guard, cfg, connect)
					if err == nil || !strings.Contains(err.Error(), "browser exited") {
						t.Fatalf("fresh recorder launch error = %v", err)
					}
					if !hasArg(readArgv(), "--user-data-dir="+dir) {
						t.Fatal("fresh browser did not receive the profile")
					}
					if got, recorded := guard.readPID(); !recorded || got == pid {
						t.Fatalf("fresh PID = %d, %v, stale PID = %d", got, recorded, pid)
					}
				} else {
					if err := guard.downgrade(); err != nil {
						t.Fatal(err)
					}
					client := &profileGuard{file: openLockFD(t, dir)}
					if _, _, err := attachShared(client, cfg, connect); err == nil {
						t.Fatal("shared client accepted an unrelated PID")
					}
					if got, recorded := guard.readPID(); !recorded || got != pid {
						t.Fatalf("shared client changed PID: %d, %v", got, recorded)
					}
					if got, err := os.ReadFile(devToolsActivePortPath(dir)); err != nil || string(got) != string(port) {
						t.Fatalf("shared client changed port: %q, %v", got, err)
					}
				}
				if !processAlive(pid) {
					t.Fatal("unrelated process was stopped")
				}
			})
		}
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
			if err := guard.writePID(os.Getpid()); err != nil {
				t.Fatal(err)
			}
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
	pid := profileBrowserOwner(t, filepath.Join(t.TempDir(), "unrelated"), dir)
	if err := guard.writePID(pid); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"chromium", "brave", "msedge"} {
		cfg := allocatorConfig{execPath: filepath.Join(t.TempDir(), name), profileDir: dir}
		if err := checkProfileBrowser(guard, cfg, true); err == nil {
			t.Fatalf("unidentified live owner accepted for %s", name)
		}
	}
}
