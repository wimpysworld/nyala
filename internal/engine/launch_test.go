package engine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/chromedp"
)

// fakeExecutable writes an executable file named name into a fresh temp dir and
// returns the dir and the full path. body is the shell body appended after the
// shebang: empty for a stub that only has to exist for a PATH lookup, or a
// recorder for a test that drives a real launch. The file is mode 0o755 so
// exec.LookPath accepts it on the test PATH, keeping the lookup hermetic with no
// real browser.
func fakeExecutable(t *testing.T, name, body string) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write fake executable: %v", err)
	}
	return dir, path
}

func TestResolveExecutablePrecedence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH lookup semantics differ on windows")
	}
	// A fake chromium on a temp dir prepended to PATH is the lowest-precedence
	// source. Each higher source must beat it.
	pathDir, pathExec := fakeExecutable(t, "chromium", "")

	cases := []struct {
		name         string
		flag         string
		nyalaBrowser string
		chromePath   string
		want         string
	}{
		{"flag wins over all", "/from/flag", "/from/nyala", "/from/chrome", "/from/flag"},
		{"nyala wins over chrome and path", "", "/from/nyala", "/from/chrome", "/from/nyala"},
		{"chrome wins over path", "", "", "/from/chrome", "/from/chrome"},
		{"path lookup is the fallback", "", "", "", pathExec},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("PATH", pathDir)
			t.Setenv("NYALA_BROWSER", c.nyalaBrowser)
			t.Setenv("CHROME_PATH", c.chromePath)

			got, err := resolveExecutable(c.flag)
			if err != nil {
				t.Fatalf("resolveExecutable(%q) error: %v", c.flag, err)
			}
			if got != c.want {
				t.Errorf("resolveExecutable(%q) = %q, want %q", c.flag, got, c.want)
			}
		})
	}
}

func TestResolveExecutableNoneResolves(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH lookup semantics differ on windows")
	}
	// Empty flag, unset envs, and an empty PATH leave nothing to find, so the
	// call must report a clear error rather than return a stray path.
	t.Setenv("PATH", t.TempDir())
	t.Setenv("NYALA_BROWSER", "")
	t.Setenv("CHROME_PATH", "")

	got, err := resolveExecutable("")
	if err == nil {
		t.Fatalf("resolveExecutable(\"\") = %q, want error", got)
	}
}

func TestResolveProfileDirPrecedence(t *testing.T) {
	root := t.TempDir()
	flagPath := filepath.Join(root, "flag-profile")
	nyalaPath := filepath.Join(root, "nyala-profile")
	dataHome := filepath.Join(root, "xdg-data")
	home := filepath.Join(root, "home")

	cases := []struct {
		name         string
		flag         string
		nyalaProfile string
		dataHome     string
		want         string
	}{
		{"flag wins over all", flagPath, nyalaPath, dataHome, flagPath},
		{"NYALA_PROFILE wins over XDG and home", "", nyalaPath, dataHome, nyalaPath},
		{"XDG_DATA_HOME wins over home", "", "", dataHome, filepath.Join(dataHome, "nyala", "profile")},
		{"home is the fallback", "", "", "", filepath.Join(home, ".local", "share", "nyala", "profile")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("NYALA_PROFILE", c.nyalaProfile)
			t.Setenv("XDG_DATA_HOME", c.dataHome)
			setHome(t, home)

			got, err := resolveProfileDir(c.flag)
			if err != nil {
				t.Fatalf("resolveProfileDir(%q) error: %v", c.flag, err)
			}
			if got != c.want {
				t.Errorf("resolveProfileDir(%q) = %q, want %q", c.flag, got, c.want)
			}
		})
	}
}

func TestResolveProfileDirMissingHome(t *testing.T) {
	t.Setenv("NYALA_PROFILE", "")
	t.Setenv("XDG_DATA_HOME", "")
	setHome(t, "")

	got, err := resolveProfileDir("")
	if err == nil {
		t.Fatalf("resolveProfileDir(\"\") = %q, want error", got)
	}
	if !strings.Contains(err.Error(), "profile directory") {
		t.Errorf("resolveProfileDir(\"\") error = %q, want profile directory context", err)
	}
}

func TestNewDoesNotTouchProfile(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	profileDir := filepath.Join(home, ".local", "share", "nyala", "profile")
	t.Setenv("NYALA_PROFILE", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", home)

	e := New("")
	if e.profileDir != "" {
		t.Errorf("New(\"\") profileDir = %q, want empty", e.profileDir)
	}
	if _, err := os.Stat(profileDir); !os.IsNotExist(err) {
		t.Fatalf("profile path after New(\"\") Stat error = %v, want not exist", err)
	}
}

func TestSetProfileDirResolvesWithoutTouchingPath(t *testing.T) {
	profileDir := filepath.Join(t.TempDir(), "profile")
	e := New("")

	if err := e.SetProfileDir(profileDir); err != nil {
		t.Fatalf("SetProfileDir(%q): %v", profileDir, err)
	}
	if e.profileDir != profileDir {
		t.Errorf("profileDir = %q, want %q", e.profileDir, profileDir)
	}
	if _, err := os.Stat(profileDir); !os.IsNotExist(err) {
		t.Fatalf("profile path after SetProfileDir Stat error = %v, want not exist", err)
	}
}

func TestLaunchResolvesExecutableBeforeTouchingProfile(t *testing.T) {
	// launch resolves the executable first and opens the profile guard only on
	// the shared path, so a missing browser leaves the profile untouched and no
	// lock file behind for a later run to misread.
	profileDir := filepath.Join(t.TempDir(), "profile")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("NYALA_BROWSER", "")
	t.Setenv("CHROME_PATH", "")
	hideInstalledBrowsers(t)
	clearProxyEnv(t, nil)

	e := New("")
	t.Cleanup(func() {
		if err := e.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	if err := e.SetProfileDir(profileDir); err != nil {
		t.Fatalf("SetProfileDir(%q): %v", profileDir, err)
	}
	if _, err := os.Stat(profileDir); !os.IsNotExist(err) {
		t.Fatalf("profile path before launch Stat error = %v, want not exist", err)
	}

	err := e.launch("")
	if err == nil || !strings.Contains(err.Error(), "no Chromium found") {
		t.Fatalf("launch error = %v, want executable resolution error", err)
	}
	if _, err := os.Stat(profileDir); !os.IsNotExist(err) {
		t.Fatalf("profile path after failed resolution Stat error = %v, want not exist", err)
	}
	if e.profileGuard != nil {
		t.Fatal("launch opened the profile guard before executable resolution")
	}
}

// openLockFD opens a second, independent descriptor on the profile lock file.
// flock locks belong to the open file description, so a lock on this fd
// conflicts with the engine's own fd inside one test process, the same way a
// second nyala process would.
func openLockFD(t *testing.T, profileDir string) *os.File {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(profileDir, profileLockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open independent lock fd: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// setHome points os.UserHomeDir at home, which reads HOME on Unix and
// USERPROFILE on Windows.
func setHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

// hideInstalledBrowsers empties the directories chromiumInstallPaths reads, so
// a browser installed on the host cannot satisfy a test that expects none.
func hideInstalledBrowsers(t *testing.T) {
	t.Helper()
	for _, name := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
		t.Setenv(name, "")
	}
}

// flockNB tries a non-blocking flock in mode on f and reports whether it won.
func flockNB(t *testing.T, f *os.File, mode int) bool {
	t.Helper()
	err := tryLockFile(f, mode)
	if isLockBusy(err) {
		return false
	}
	if err != nil {
		t.Fatalf("flock mode %d: %v", mode, err)
	}
	return true
}

func TestProfileGuardContentionAndRecovery(t *testing.T) {
	// Two engines on one profile hold the guard at the same time: the launcher
	// wins LOCK_EX and downgrades, the client takes LOCK_SH. A third exclusive
	// attempt fails while either shared holder lives and succeeds once both
	// close, on the same lock file throughout.
	profileDir := filepath.Join(t.TempDir(), "profile")
	first := New("")
	second := New("")
	for _, e := range []*ChromiumEngine{first, second} {
		if err := e.SetProfileDir(profileDir); err != nil {
			t.Fatalf("SetProfileDir(%q): %v", profileDir, err)
		}
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Errorf("second Close: %v", err)
		}
		if err := first.Close(); err != nil {
			t.Errorf("first Close: %v", err)
		}
	})
	if err := first.openProfileGuard(); err != nil {
		t.Fatalf("first openProfileGuard: %v", err)
	}
	won, err := first.profileGuard.tryExclusive()
	if err != nil || !won {
		t.Fatalf("first tryExclusive = (%v, %v), want (true, nil)", won, err)
	}
	lockPath := filepath.Join(profileDir, profileLockName)
	before, err := os.Stat(lockPath)
	if err != nil {
		t.Fatalf("Stat profile guard: %v", err)
	}

	if err := second.openProfileGuard(); err != nil {
		t.Fatalf("second openProfileGuard: %v", err)
	}
	// While the launcher holds LOCK_EX the client cannot take LOCK_SH.
	blockedCtx, cancelBlocked := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancelBlocked()
	if err := second.profileGuard.acquireShared(blockedCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second acquireShared while LOCK_EX held = %v, want deadline exceeded", err)
	}

	if err := first.profileGuard.downgrade(); err != nil {
		t.Fatalf("first downgrade: %v", err)
	}
	sharedCtx, cancelShared := context.WithTimeout(context.Background(), time.Second)
	defer cancelShared()
	if err := second.profileGuard.acquireShared(sharedCtx); err != nil {
		t.Fatalf("second acquireShared after downgrade: %v", err)
	}
	if first.profileGuard.mode != lockShared || second.profileGuard.mode != lockShared {
		t.Fatalf("modes = (%d, %d), want both LOCK_SH", first.profileGuard.mode, second.profileGuard.mode)
	}

	third := openLockFD(t, profileDir)
	if flockNB(t, third, lockExclusive) {
		t.Fatal("third LOCK_EX succeeded while two shared holders exist")
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if flockNB(t, third, lockExclusive) {
		t.Fatal("third LOCK_EX succeeded while the second shared holder exists")
	}
	if err := second.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if !flockNB(t, third, lockExclusive) {
		t.Fatal("third LOCK_EX failed after both holders closed")
	}
	after, err := os.Stat(lockPath)
	if err != nil {
		t.Fatalf("Stat profile guard after recovery: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("recovery replaced the existing profile guard file")
	}
}

func TestProfileGuardExclusiveThenSharedDowngrade(t *testing.T) {
	// downgrade must convert LOCK_EX to LOCK_SH on the same fd: an independent
	// fd cannot take LOCK_SH before the downgrade and can after it, and LOCK_EX
	// stays refused until unlock.
	profileDir := t.TempDir()
	e := New("")
	if err := e.SetProfileDir(profileDir); err != nil {
		t.Fatalf("SetProfileDir(%q): %v", profileDir, err)
	}
	t.Cleanup(func() {
		if err := e.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	if err := e.openProfileGuard(); err != nil {
		t.Fatalf("openProfileGuard: %v", err)
	}
	g := e.profileGuard
	if g.mode != 0 {
		t.Fatalf("mode after open = %d, want 0", g.mode)
	}
	won, err := g.tryExclusive()
	if err != nil || !won {
		t.Fatalf("tryExclusive = (%v, %v), want (true, nil)", won, err)
	}
	if g.mode != lockExclusive {
		t.Fatalf("mode after tryExclusive = %d, want LOCK_EX", g.mode)
	}
	other := openLockFD(t, profileDir)
	if flockNB(t, other, lockShared) {
		t.Fatal("independent LOCK_SH succeeded while LOCK_EX held")
	}
	if err := g.downgrade(); err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	if g.mode != lockShared {
		t.Fatalf("mode after downgrade = %d, want LOCK_SH", g.mode)
	}
	if !flockNB(t, other, lockShared) {
		t.Fatal("independent LOCK_SH failed after downgrade")
	}
	if err := unlockFile(other); err != nil {
		t.Fatalf("unlock independent fd: %v", err)
	}
	if flockNB(t, other, lockExclusive) {
		t.Fatal("independent LOCK_EX succeeded while the guard holds LOCK_SH")
	}
	if err := g.unlock(); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if g.mode != 0 {
		t.Fatalf("mode after unlock = %d, want 0", g.mode)
	}
	if !flockNB(t, other, lockExclusive) {
		t.Fatal("independent LOCK_EX failed after the guard unlocked")
	}
}

func TestProfileGuardPIDRoundTrip(t *testing.T) {
	// The lock file carries the launcher's Chromium pid for the last client
	// out. readPID must refuse an empty file and return exactly what writePID
	// stored, including after a shorter pid overwrites a longer one.
	e := New("")
	if err := e.SetProfileDir(t.TempDir()); err != nil {
		t.Fatalf("SetProfileDir: %v", err)
	}
	t.Cleanup(func() {
		if err := e.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	if err := e.openProfileGuard(); err != nil {
		t.Fatalf("openProfileGuard: %v", err)
	}
	g := e.profileGuard
	if pid, ok := g.readPID(); ok {
		t.Fatalf("readPID on empty file = (%d, true), want ok false", pid)
	}
	for _, want := range []int{123456, 42} {
		if err := g.writePID(want); err != nil {
			t.Fatalf("writePID(%d): %v", want, err)
		}
		got, ok := g.readPID()
		if !ok || got != want {
			t.Fatalf("readPID after writePID(%d) = (%d, %v), want (%d, true)", want, got, ok, want)
		}
	}
	data, err := os.ReadFile(filepath.Join(e.profileDir, profileLockName))
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	if string(data) != "42" {
		t.Errorf("lock file content = %q, want %q", data, "42")
	}
}

func TestProfileGuardReusesStaleFile(t *testing.T) {
	profileDir := t.TempDir()
	lockPath := filepath.Join(profileDir, profileLockName)
	const staleContents = "stale guard file\n"
	if err := os.WriteFile(lockPath, []byte(staleContents), 0o600); err != nil {
		t.Fatalf("write stale profile guard: %v", err)
	}
	e := New("")
	t.Cleanup(func() {
		if err := e.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	if err := e.SetProfileDir(profileDir); err != nil {
		t.Fatalf("SetProfileDir(%q): %v", profileDir, err)
	}
	if err := e.openProfileGuard(); err != nil {
		t.Fatalf("openProfileGuard with stale file: %v", err)
	}
	if won, err := e.profileGuard.tryExclusive(); err != nil || !won {
		t.Fatalf("tryExclusive with stale file = (%v, %v), want (true, nil)", won, err)
	}
	contents, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read reused profile guard: %v", err)
	}
	if string(contents) != staleContents {
		t.Errorf("reused profile guard contents = %q, want %q", contents, staleContents)
	}
}

func TestFrameRateLimitFlags(t *testing.T) {
	if got := frameRateLimitFlags(false); got != nil {
		t.Errorf("frameRateLimitFlags(false) = %v, want nil", got)
	}
	want := []string{"disable-frame-rate-limit"}
	if got := frameRateLimitFlags(true); !reflect.DeepEqual(got, want) {
		t.Errorf("frameRateLimitFlags(true) = %v, want %v", got, want)
	}
}

func TestSetDisableFrameRateLimit(t *testing.T) {
	e := New("")
	if e.disableFrameRateLimit {
		t.Fatal("new engine has disableFrameRateLimit = true, want false")
	}
	e.SetDisableFrameRateLimit(true)
	if !e.disableFrameRateLimit {
		t.Fatal("SetDisableFrameRateLimit(true) left disableFrameRateLimit false")
	}
}

func TestGPUFlags(t *testing.T) {
	if got := gpuFlags(false); got != nil {
		t.Errorf("gpuFlags(false) = %v, want nil", got)
	}
	// This pins the pure builder's result only. It cannot see whether newAllocator
	// still appends that result to the launch options, so
	// TestLaunchArgvCarriesGPUFlags is the test that guards the flags actually
	// reaching Chromium.
	want := []string{"enable-gpu", "enable-unsafe-swiftshader"}
	if got := gpuFlags(true); !reflect.DeepEqual(got, want) {
		t.Errorf("gpuFlags(true) = %v, want %v", got, want)
	}
}

// shortDevToolsWait shrinks the DevToolsActivePort wait for the test and
// restores it afterwards. A fake browser never writes the file, so without this
// each shared launch attempt would wait the production 30 s.
func shortDevToolsWait(t *testing.T) {
	t.Helper()
	saved := devToolsWaitTimeout
	devToolsWaitTimeout = 200 * time.Millisecond
	t.Cleanup(func() { devToolsWaitTimeout = saved })
}

// argvRecorder installs a fake browser that records its argv into a file and
// exits at once. It returns the browser path and a reader for the recorded
// argv. The recorder reads the file path from the NYALA_TEST_ARGV_FILE
// environment variable, which both the exec allocator and spawnDetached
// inherit. spawnDetached points stdio at /dev/null, so the recorder writes to
// the file rather than to stdout.
func argvRecorder(t *testing.T) (browserPath string, readArgv func() []string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the recorder is a POSIX shell script")
	}
	argvFile := filepath.Join(t.TempDir(), "argv")
	t.Setenv("NYALA_TEST_ARGV_FILE", argvFile)
	_, browserPath = fakeExecutable(t, "chromium",
		"printf '%s\\n' \"$@\" > \"$NYALA_TEST_ARGV_FILE\"\nexit 1\n")
	readArgv = func() []string {
		t.Helper()
		raw, err := os.ReadFile(argvFile)
		if err != nil {
			t.Fatalf("read recorded argv: %v", err)
		}
		return strings.Split(strings.TrimSpace(string(raw)), "\n")
	}
	return browserPath, readArgv
}

// recordLaunchArgv drives a real launch against the argv recorder and returns
// the argv the browser received. Both the private and the shared path fail
// because the recorder exits without serving CDP, and it returns the error so
// a caller can assert on the shared retry cap.
func recordLaunchArgv(t *testing.T, configure func(*ChromiumEngine)) ([]string, error) {
	t.Helper()
	browserPath, readArgv := argvRecorder(t)
	shortDevToolsWait(t)

	// NYALA_BROWSER beats the empty PATH, so the recorder is the resolved browser.
	t.Setenv("PATH", t.TempDir())
	t.Setenv("NYALA_BROWSER", browserPath)
	t.Setenv("CHROME_PATH", "")
	// A stray proxy variable in the ambient environment would fail launch at the
	// proxy guard, before any exec.
	clearProxyEnv(t, nil)

	e := New("")
	if configure != nil {
		configure(e)
	}
	t.Cleanup(func() {
		if err := e.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	err := e.launch("")
	if err == nil {
		t.Fatal("launch() = nil, want the start failure from the recording browser")
	}
	return readArgv(), err
}

// hasArg reports whether argv contains arg exactly or as a --arg=value prefix.
func hasArg(argv []string, arg string) bool {
	for _, a := range argv {
		if a == arg || strings.HasPrefix(a, arg+"=") {
			return true
		}
	}
	return false
}

func TestLaunchArgvCarriesGPUFlags(t *testing.T) {
	// gpuFlags is pure, so a test on it alone stays green if newAllocator stops
	// appending the result. The fake browser records the real launch argv and exits
	// before a browser starts.
	argv, _ := recordLaunchArgv(t, nil)
	// Assert presence, not position: chromedp holds flags in a map, so argv order
	// is not stable between runs.
	for _, want := range []string{"--enable-gpu", "--enable-unsafe-swiftshader"} {
		if !slices.Contains(argv, want) {
			t.Errorf("launch argv %v is missing %s", argv, want)
		}
	}
}

func TestLaunchArgvHidesAutomationSignals(t *testing.T) {
	argv, _ := recordLaunchArgv(t, nil)
	for _, want := range []string{
		"--disable-blink-features=AutomationControlled",
		"--remote-debugging-port=0",
	} {
		if !slices.Contains(argv, want) {
			t.Errorf("launch argv %v is missing %s", argv, want)
		}
	}
	for _, arg := range argv {
		if arg == "--enable-automation" || strings.HasPrefix(arg, "--enable-automation=") {
			t.Errorf("launch argv contains automation flag %q", arg)
		}
	}
}

func TestSpawnSharedFailsWhenBrowserExitsBeforePortFile(t *testing.T) {
	// A browser that dies before it publishes DevToolsActivePort must fail the
	// spawn at once. The recorder exits at once and the production 30 s port
	// wait stays in force, so a prompt return proves the exit ended the wait.
	profileDir := filepath.Join(t.TempDir(), "profile")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatalf("create profile dir: %v", err)
	}
	browserPath, _ := argvRecorder(t)
	e := New("")
	if err := e.SetProfileDir(profileDir); err != nil {
		t.Fatalf("SetProfileDir: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err := e.openProfileGuard(); err != nil {
		t.Fatalf("openProfileGuard: %v", err)
	}
	if won, err := e.profileGuard.tryExclusive(); err != nil || !won {
		t.Fatalf("tryExclusive = (%v, %v), want (true, nil)", won, err)
	}
	connect := func(context.Context, string, time.Duration) (context.Context, context.CancelFunc, error) {
		t.Fatal("connect ran without a port file")
		return nil, nil, nil
	}
	start := time.Now()
	_, _, err := spawnShared(e.profileGuard, allocatorConfig{execPath: browserPath, profileDir: profileDir, scale: 1}, connect)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("spawnShared took %v after the browser exited, want well under 3 s", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "browser exited") {
		t.Errorf("spawnShared error = %v, want one that reports the browser exit", err)
	}
}

func TestLaunchArgvCarriesPersistentProfile(t *testing.T) {
	// A persistent profile takes the shared path: the recorder is spawned
	// detached with the profile, the remote debugging port, and the keep-alive
	// switch, and without the automation banner. It exits without writing
	// DevToolsActivePort, so launch must give up after maxLaunchAttempts.
	profileDir := filepath.Join(t.TempDir(), "profile")
	start := time.Now()
	argv, err := recordLaunchArgv(t, func(e *ChromiumEngine) {
		if err := e.SetProfileDir(profileDir); err != nil {
			t.Fatalf("SetProfileDir(%q): %v", profileDir, err)
		}
	})
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("shared launch took %v with a 200 ms port wait, want well under 3 s", elapsed)
	}
	want := "after " + strconv.Itoa(maxLaunchAttempts) + " attempts"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("launch error %q does not report %q", err, want)
	}
	if !strings.Contains(err.Error(), devToolsActivePortFile) {
		t.Errorf("launch error %q does not name %s", err, devToolsActivePortFile)
	}
	for _, arg := range []string{
		"--user-data-dir=" + profileDir,
		"--keep-alive-for-test",
		"--remote-debugging-port=0",
	} {
		if !slices.Contains(argv, arg) {
			t.Errorf("shared launch argv %v is missing %q", argv, arg)
		}
	}
	if hasArg(argv, "--enable-automation") {
		t.Errorf("shared launch argv %v carries --enable-automation", argv)
	}
	// The shared path creates the profile and the lock file with private modes.
	info, err := os.Stat(profileDir)
	if err != nil {
		t.Fatalf("Stat created profile directory: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("profile directory mode = %04o, want 0700", got)
	}
	lockInfo, err := os.Stat(filepath.Join(profileDir, profileLockName))
	if err != nil {
		t.Fatalf("Stat profile guard: %v", err)
	}
	if got := lockInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("profile guard mode = %04o, want 0600", got)
	}
}

func TestLaunchWithoutConfiguredProfileUsesDisposableUserDataDir(t *testing.T) {
	watchedProfile := filepath.Join(t.TempDir(), "watched-profile")
	t.Setenv("NYALA_PROFILE", watchedProfile)
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	t.Setenv("HOME", filepath.Join(t.TempDir(), "home"))

	argv, _ := recordLaunchArgv(t, nil)
	// The private path keeps chromedp's exec allocator, which owns the browser
	// lifetime, so the shared keep-alive switch must not appear.
	if slices.Contains(argv, "--keep-alive-for-test") {
		t.Errorf("private launch argv %v carries --keep-alive-for-test", argv)
	}
	var userDataDir string
	for _, arg := range argv {
		if strings.HasPrefix(arg, "--user-data-dir=") {
			userDataDir = arg
		}
		if strings.Contains(arg, watchedProfile) {
			t.Errorf("launch argv contains configured-profile-derived argument %q", arg)
		}
	}
	if userDataDir == "" {
		t.Fatalf("launch argv %v has no chromedp disposable --user-data-dir", argv)
	}
	if _, err := os.Stat(watchedProfile); !os.IsNotExist(err) {
		t.Fatalf("unconfigured launch profile Stat error = %v, want path absent", err)
	}
}

// stripLaunchPathArgs removes the switches each launch path sets itself, so
// the remaining argv is the flag set both paths must share.
func stripLaunchPathArgs(argv []string) []string {
	out := make([]string, 0, len(argv))
	for _, arg := range argv {
		if hasArg([]string{arg}, "--user-data-dir") ||
			arg == "--remote-debugging-port=0" ||
			arg == "--keep-alive-for-test" {
			continue
		}
		out = append(out, arg)
	}
	return out
}

func TestSharedArgvMirrorsExecAllocatorOrder(t *testing.T) {
	// sharedArgv and newAllocator must build from one flag source, so a switch
	// added to one path cannot go missing from the other. chromedp emits its
	// flags from a map in random order, so the comparison sorts both sides.
	browserPath, readArgv := argvRecorder(t)
	profileDir := filepath.Join(t.TempDir(), "profile")
	cfg := allocatorConfig{
		execPath:              browserPath,
		profileDir:            profileDir,
		surfaceWidth:          1280,
		surfaceHeight:         720,
		scale:                 1.5,
		proxy:                 ProxyConfig{Server: "http://host:3128", Bypass: "localhost"},
		enableGPU:             true,
		enableAudio:           true,
		disableFrameRateLimit: true,
	}

	allocCtx, allocCancel := newAllocator(context.Background(), cfg)
	defer allocCancel()
	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()
	if err := chromedp.Run(ctx); err == nil {
		t.Fatal("chromedp.Run against the recorder = nil, want a start failure")
	}
	execArgv := stripLaunchPathArgs(readArgv())
	shared := stripLaunchPathArgs(sharedArgv(launchFlags(cfg), profileDir))
	slices.Sort(execArgv)
	slices.Sort(shared)
	if !slices.Equal(execArgv, shared) {
		t.Errorf("sharedArgv (sorted)\n%v\ndiffers from the exec allocator argv (sorted)\n%v", shared, execArgv)
	}
	full := sharedArgv(launchFlags(cfg), profileDir)
	if n := len(full); n < 3 || full[n-1] != "about:blank" || full[n-2] != "--keep-alive-for-test" || full[n-3] != "--remote-debugging-port=0" {
		t.Errorf("sharedArgv tail = %v, want --remote-debugging-port=0, --keep-alive-for-test, about:blank", full)
	}
}

func TestCommandLineHasArgBounds(t *testing.T) {
	// Chromium rewrites its /proc cmdline into one space-joined string, so a
	// NUL-only split never matched a real browser and the last client left it
	// running. Both NUL and space must bound an argument.
	want := "--user-data-dir=/p/profile"
	cases := []struct {
		name string
		args string
		ok   bool
	}{
		{"nul separated", "chromium\x00--headless\x00--user-data-dir=/p/profile\x00about:blank\x00", true},
		{"space joined by chromium", "chromium --headless --user-data-dir=/p/profile --remote-debugging-port=0 about:blank", true},
		{"at end", "chromium --user-data-dir=/p/profile", true},
		{"prefix of longer path, nul", "chromium\x00--user-data-dir=/p/profile-2\x00", false},
		{"prefix of longer path, space", "chromium --user-data-dir=/p/profile-2 about:blank", false},
		{"embedded in another flag", "chromium --x--user-data-dir=/p/profile about:blank", false},
		{"absent", "chromium --headless", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := commandLineHasArg(tc.args, want); got != tc.ok {
				t.Errorf("commandLineHasArg = %v, want %v", got, tc.ok)
			}
		})
	}
}

func TestBrowserOwnsProfileGatesOnCommandLine(t *testing.T) {
	// A pid from the lock file may belong to a stranger after recycling. Only a
	// process whose command line names the profile may receive a signal.
	if runtime.GOOS == "windows" {
		t.Skip("the fake browser is a POSIX shell process")
	}
	profileDir := filepath.Join(t.TempDir(), "profile")
	ctx := context.Background()
	if browserOwnsProfile(ctx, os.Getpid(), profileDir) {
		t.Error("the test binary was reported as a browser on the profile")
	}
	// sh keeps the positional argument in its argv and ignores it. The trailing
	// true stops sh from replacing itself with sleep, which would drop the argv.
	//nolint:gosec // The profile path is a test temp dir.
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30; true", "sh", "--user-data-dir="+profileDir+"-child")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake browser: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	// Start returns at the exec point of no return, a moment before the kernel
	// publishes the new argv, so the command line can read empty at once.
	time.Sleep(50 * time.Millisecond)
	if !browserOwnsProfile(ctx, cmd.Process.Pid, profileDir+"-child") {
		t.Error("a process whose argv names the profile was not recognised")
	}
	if browserOwnsProfile(ctx, cmd.Process.Pid, profileDir+"-other") {
		t.Error("a process on another profile was recognised")
	}
	// The profile is a prefix of the process's profile path, so a substring
	// match would claim it.
	if browserOwnsProfile(ctx, cmd.Process.Pid, profileDir) {
		t.Error("a process on a profile that extends this path was recognised")
	}
}

func TestParseDevToolsActivePort(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{"two lines", "9222\n/devtools/browser/abc", "ws://127.0.0.1:9222/devtools/browser/abc", false},
		{"trailing newline", "9222\n/devtools/browser/abc\n", "ws://127.0.0.1:9222/devtools/browser/abc", false},
		{"missing second line", "9222\n", "", true},
		{"non-numeric port", "port\n/devtools/browser/abc", "", true},
		{"empty", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseDevToolsActivePort([]byte(c.input))
			if (err != nil) != c.wantErr {
				t.Fatalf("parseDevToolsActivePort(%q) error = %v, wantErr %v", c.input, err, c.wantErr)
			}
			if got != c.want {
				t.Errorf("parseDevToolsActivePort(%q) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}

// sharedTestEngine returns an engine on profileDir together with a launch
// config that points at the argv recorder. Close runs at cleanup.
func sharedTestEngine(t *testing.T, profileDir string) (*ChromiumEngine, allocatorConfig, func() []string) {
	t.Helper()
	browserPath, readArgv := argvRecorder(t)
	shortDevToolsWait(t)
	e := New("")
	if err := e.SetProfileDir(profileDir); err != nil {
		t.Fatalf("SetProfileDir(%q): %v", profileDir, err)
	}
	t.Cleanup(func() {
		if err := e.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return e, allocatorConfig{execPath: browserPath, profileDir: profileDir, scale: 1}, readArgv
}

func TestSharedClientRetriesWhenLauncherDies(t *testing.T) {
	// Another process holds LOCK_EX and dies without publishing a port file.
	// The kernel drops its lock, the client's LOCK_SH poll succeeds, the port
	// read fails, and the client must retry as the launcher itself rather than
	// wait for a launcher that no longer exists.
	profileDir := filepath.Join(t.TempDir(), "profile")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatalf("create profile: %v", err)
	}
	launcher := openLockFD(t, profileDir)
	if !flockNB(t, launcher, lockExclusive) {
		t.Fatal("independent LOCK_EX failed on a fresh lock file")
	}
	released := time.AfterFunc(100*time.Millisecond, func() { _ = launcher.Close() })
	defer released.Stop()

	e, cfg, readArgv := sharedTestEngine(t, profileDir)
	start := time.Now()
	_, _, _, err := e.launchShared(cfg)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("launchShared = nil, want failure after the retry cap")
	}
	if elapsed > 3*time.Second {
		t.Errorf("launchShared took %v, want under 3 s", elapsed)
	}
	if elapsed < 100*time.Millisecond {
		t.Errorf("launchShared returned in %v, before the dead launcher released LOCK_EX", elapsed)
	}
	msg := err.Error()
	if !strings.Contains(msg, "attempts") || !strings.Contains(msg, devToolsActivePortFile) {
		t.Errorf("launchShared error %q does not report the attempt cap and the port file", msg)
	}
	// The client became the launcher: the recorder was spawned with the profile.
	if !slices.Contains(readArgv(), "--user-data-dir="+profileDir) {
		t.Errorf("client did not spawn the browser after the launcher died")
	}
	if e.profileGuard.mode != 0 {
		t.Errorf("guard mode after failed launch = %d, want 0 (unlocked)", e.profileGuard.mode)
	}
}

func TestLaunchAdoptsLiveBrowser(t *testing.T) {
	// The lock file names a live pid and DevToolsActivePort parses, so the
	// LOCK_EX winner adopts the browser: it connects to the published URL,
	// spawns nothing, and keeps Default/Sessions. spawnShared is the seam,
	// because launchShared goes on to newInstanceTarget, which needs a real
	// Browser. The current process stands in for the live browser pid.
	profileDir := filepath.Join(t.TempDir(), "profile")
	sessionFile := filepath.Join(profileDir, "Default", "Sessions", "x")
	if err := os.MkdirAll(filepath.Dir(sessionFile), 0o700); err != nil {
		t.Fatalf("create sessions dir: %v", err)
	}
	if err := os.WriteFile(sessionFile, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write session file: %v", err)
	}
	const wantURL = "ws://127.0.0.1:9333/devtools/browser/live"
	if err := os.WriteFile(devToolsActivePortPath(profileDir), []byte("9333\n/devtools/browser/live\n"), 0o600); err != nil {
		t.Fatalf("write port file: %v", err)
	}
	e, cfg, _ := sharedTestEngine(t, profileDir)
	if err := e.openProfileGuard(); err != nil {
		t.Fatalf("openProfileGuard: %v", err)
	}
	guard := e.profileGuard
	if won, err := guard.tryExclusive(); err != nil || !won {
		t.Fatalf("tryExclusive = (%v, %v), want (true, nil)", won, err)
	}
	if err := guard.writePID(os.Getpid()); err != nil {
		t.Fatalf("writePID: %v", err)
	}

	type key struct{}
	var gotURLs []string
	var gotTimeouts []time.Duration
	connect := func(parent context.Context, wsURL string, timeout time.Duration) (context.Context, context.CancelFunc, error) {
		gotURLs = append(gotURLs, wsURL)
		gotTimeouts = append(gotTimeouts, timeout)
		ctx, cancel := context.WithCancel(context.WithValue(parent, key{}, "fake"))
		return ctx, cancel, nil
	}
	holder, closeSocket, err := spawnShared(guard, cfg, connect)
	if err != nil {
		t.Fatalf("spawnShared: %v", err)
	}
	defer closeSocket()
	if holder.Value(key{}) != "fake" {
		t.Error("spawnShared did not return the connect fake's holder context")
	}
	if !slices.Equal(gotURLs, []string{wantURL}) {
		t.Errorf("connect URLs = %v, want exactly [%s]", gotURLs, wantURL)
	}
	if !slices.Equal(gotTimeouts, []time.Duration{adoptConnectTimeout}) {
		t.Errorf("connect timeouts = %v, want [%v]", gotTimeouts, adoptConnectTimeout)
	}
	if _, err := os.Stat(sessionFile); err != nil {
		t.Errorf("adopt purged Default/Sessions: %v", err)
	}
	if pid, ok := guard.readPID(); !ok || pid != os.Getpid() {
		t.Errorf("adopt rewrote the lock file pid to (%d, %v), want own pid kept", pid, ok)
	}
}

func TestProxyFlagsCredentialFreeServer(t *testing.T) {
	// A credentialed URL must yield a proxy-server flag whose value is the
	// credential-free Server, with no user:pass@ in any flag value. This is the
	// process-arg guard: credentials must never reach a Chromium command-line flag.
	clearProxyEnv(t, map[string]string{"HTTPS_PROXY": "http://user:pass@host:3128"})

	cfg, err := parseProxyEnv()
	if err != nil {
		t.Fatalf("parseProxyEnv() error: %v", err)
	}
	pairs := proxyFlags(cfg)
	if len(pairs) != 1 {
		t.Fatalf("proxyFlags = %v, want one proxy-server pair", pairs)
	}
	if pairs[0][0] != "proxy-server" {
		t.Errorf("flag name = %q, want %q", pairs[0][0], "proxy-server")
	}
	if pairs[0][1] != "http://host:3128" {
		t.Errorf("proxy-server value = %q, want %q", pairs[0][1], "http://host:3128")
	}
	// No flag value may carry credentials, in any form.
	for _, pair := range pairs {
		v := pair[1]
		if strings.Contains(v, "user") || strings.Contains(v, "pass") || strings.Contains(v, "@") {
			t.Errorf("flag %q value %q leaks credentials", pair[0], v)
		}
	}
}

func TestProxyFlagsServerOnly(t *testing.T) {
	// HTTPS_PROXY alone, no NO_PROXY, must yield a single proxy-server flag with
	// the verbatim server and no bypass flag.
	clearProxyEnv(t, map[string]string{"HTTPS_PROXY": "http://host:3128"})

	cfg, err := parseProxyEnv()
	if err != nil {
		t.Fatalf("parseProxyEnv() error: %v", err)
	}
	pairs := proxyFlags(cfg)
	want := [][2]string{{"proxy-server", "http://host:3128"}}
	if !reflect.DeepEqual(pairs, want) {
		t.Errorf("proxyFlags = %v, want %v", pairs, want)
	}
}

func TestProxyFlagsWithBypass(t *testing.T) {
	// A bypass value with a proxy set must yield both flags, the bypass verbatim.
	const bypass = ".internal,10.0.0.0/8,localhost"
	clearProxyEnv(t, map[string]string{"HTTPS_PROXY": "http://host:3128", "NO_PROXY": bypass})

	cfg, err := parseProxyEnv()
	if err != nil {
		t.Fatalf("parseProxyEnv() error: %v", err)
	}
	pairs := proxyFlags(cfg)
	want := [][2]string{
		{"proxy-server", "http://host:3128"},
		{"proxy-bypass-list", bypass},
	}
	if !reflect.DeepEqual(pairs, want) {
		t.Errorf("proxyFlags = %v, want %v", pairs, want)
	}
}

func TestProxyFlagsStrayBypassNoProxy(t *testing.T) {
	// A NO_PROXY with no proxy var set must yield neither flag: the bypass is
	// gated on a Server, so a stray NO_PROXY alone adds nothing.
	clearProxyEnv(t, map[string]string{"NO_PROXY": "localhost,127.0.0.1"})

	cfg, err := parseProxyEnv()
	if err != nil {
		t.Fatalf("parseProxyEnv() error: %v", err)
	}
	if pairs := proxyFlags(cfg); len(pairs) != 0 {
		t.Errorf("proxyFlags = %v, want no flags for a stray NO_PROXY", pairs)
	}
}

func TestProxyFlagsNoProxy(t *testing.T) {
	// Nothing set must yield no proxy flags at all.
	clearProxyEnv(t, nil)

	cfg, err := parseProxyEnv()
	if err != nil {
		t.Fatalf("parseProxyEnv() error: %v", err)
	}
	if pairs := proxyFlags(cfg); pairs != nil {
		t.Errorf("proxyFlags = %v, want nil for no proxy", pairs)
	}
}

func TestLaunchReturnsProxyErrorBeforeResolve(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH lookup semantics differ on windows")
	}
	// launch returns the stored proxy parse error before resolveExecutable, so a
	// malformed proxy fails loudly and never connects direct. The seam: build a
	// real engine via New with a malformed proxy env, then drive launch with no
	// resolvable browser. The error must name the source variable, not the
	// resolveExecutable "no Chromium found" message, proving the guard fires first.
	// A control byte in the host forces a real url.Parse failure; the embedded
	// token stands in for a leaked credential.
	const secret = "leaktoken"
	clearProxyEnv(t, map[string]string{"HTTPS_PROXY": "http://user:" + secret + "@host\x7f:3128"})
	// No browser is resolvable: empty flag, unset browser envs, empty PATH. The
	// guard must still surface the proxy error before any executable lookup.
	t.Setenv("PATH", t.TempDir())
	t.Setenv("NYALA_BROWSER", "")
	t.Setenv("CHROME_PATH", "")

	e := New("")
	err := e.launch("")
	if err == nil {
		t.Fatal("launch() = nil, want the stored proxy parse error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "HTTPS_PROXY") {
		t.Errorf("launch error %q does not name the source variable HTTPS_PROXY", msg)
	}
	if strings.Contains(msg, "no Chromium found") {
		t.Errorf("launch error %q is the resolveExecutable error; the proxy guard must fire first", msg)
	}
	if strings.Contains(msg, secret) {
		t.Errorf("launch error %q leaks the credential value", msg)
	}
}

func TestProxyAuthDecisionFirstProxyChallenge(t *testing.T) {
	// A Proxy-source EventAuthRequired, first seen for a request id, must answer
	// continueWithAuth with ProvideCredentials and the parsed user and password.
	cfg := ProxyConfig{User: "user", Password: "pass", HasCredentials: true}
	answered := make(map[fetch.RequestID]bool)
	ev := &fetch.EventAuthRequired{
		RequestID:     fetch.RequestID("req-1"),
		AuthChallenge: &fetch.AuthChallenge{Source: fetch.AuthChallengeSourceProxy},
	}

	action := proxyAuthDecision(ev, answered, cfg)
	if action.Kind != proxyAuthContinueWithAuth {
		t.Fatalf("Kind = %d, want proxyAuthContinueWithAuth", action.Kind)
	}
	if action.RequestID != ev.RequestID {
		t.Errorf("RequestID = %q, want %q", action.RequestID, ev.RequestID)
	}
	if action.Resp == nil {
		t.Fatal("Resp = nil, want an AuthChallengeResponse")
	}
	if action.Resp.Response != fetch.AuthChallengeResponseResponseProvideCredentials {
		t.Errorf("Response = %q, want ProvideCredentials", action.Resp.Response)
	}
	if action.Resp.Username != "user" || action.Resp.Password != "pass" {
		t.Errorf("credentials = %q/%q, want user/pass", action.Resp.Username, action.Resp.Password)
	}
}

func TestProxyAuthDecisionRepeatAnswersDefault(t *testing.T) {
	// A repeat challenge for the same request id must answer Default, the loop
	// guard: a second ProvideCredentials with wrong credentials would loop. The
	// transition depends on the answered map, which the first call populates.
	cfg := ProxyConfig{User: "user", Password: "pass", HasCredentials: true}
	answered := make(map[fetch.RequestID]bool)
	ev := &fetch.EventAuthRequired{
		RequestID:     fetch.RequestID("req-1"),
		AuthChallenge: &fetch.AuthChallenge{Source: fetch.AuthChallengeSourceProxy},
	}

	first := proxyAuthDecision(ev, answered, cfg)
	if first.Resp.Response != fetch.AuthChallengeResponseResponseProvideCredentials {
		t.Fatalf("first Response = %q, want ProvideCredentials", first.Resp.Response)
	}
	if !answered[ev.RequestID] {
		t.Fatal("answered map not updated after the first challenge")
	}

	repeat := proxyAuthDecision(ev, answered, cfg)
	if repeat.Kind != proxyAuthContinueWithAuth {
		t.Errorf("repeat Kind = %d, want proxyAuthContinueWithAuth", repeat.Kind)
	}
	if repeat.Resp == nil || repeat.Resp.Response != fetch.AuthChallengeResponseResponseDefault {
		t.Errorf("repeat Response = %v, want Default", repeat.Resp)
	}
	// Default must carry no credentials, so a wrong-credential loop cannot leak.
	if repeat.Resp.Username != "" || repeat.Resp.Password != "" {
		t.Errorf("repeat carries credentials %q/%q, want empty", repeat.Resp.Username, repeat.Resp.Password)
	}
}

func TestProxyAuthDecisionRequestPaused(t *testing.T) {
	// EventRequestPaused must answer continueRequest, the drain arm that releases
	// the pause enabling Fetch with handleAuthRequests creates.
	cfg := ProxyConfig{User: "user", Password: "pass", HasCredentials: true}
	answered := make(map[fetch.RequestID]bool)
	ev := &fetch.EventRequestPaused{RequestID: fetch.RequestID("req-2")}

	action := proxyAuthDecision(ev, answered, cfg)
	if action.Kind != proxyAuthContinueRequest {
		t.Errorf("Kind = %d, want proxyAuthContinueRequest", action.Kind)
	}
	if action.RequestID != ev.RequestID {
		t.Errorf("RequestID = %q, want %q", action.RequestID, ev.RequestID)
	}
}

func TestProxyAuthDecisionNonProxySource(t *testing.T) {
	// A non-Proxy auth source (server auth) is out of scope, so the policy answers
	// Default and lets Chromium's net stack handle it. No credentials are sent.
	cfg := ProxyConfig{User: "user", Password: "pass", HasCredentials: true}
	answered := make(map[fetch.RequestID]bool)
	ev := &fetch.EventAuthRequired{
		RequestID:     fetch.RequestID("req-3"),
		AuthChallenge: &fetch.AuthChallenge{Source: fetch.AuthChallengeSourceServer},
	}

	action := proxyAuthDecision(ev, answered, cfg)
	if action.Kind != proxyAuthContinueWithAuth {
		t.Fatalf("Kind = %d, want proxyAuthContinueWithAuth", action.Kind)
	}
	if action.Resp == nil || action.Resp.Response != fetch.AuthChallengeResponseResponseDefault {
		t.Errorf("Response = %v, want Default for a server-source challenge", action.Resp)
	}
	if action.Resp.Username != "" || action.Resp.Password != "" {
		t.Errorf("server challenge carries credentials %q/%q, want empty", action.Resp.Username, action.Resp.Password)
	}
}

func TestProxyAuthDecisionTrackerFreshPerSession(t *testing.T) {
	// A fresh answered map is empty per session, so the same request id seen in a
	// new map gets ProvideCredentials again. This is what gives each rebuilt
	// session its own first-vs-repeat state, with no leak across sessions.
	cfg := ProxyConfig{User: "user", Password: "pass", HasCredentials: true}
	ev := &fetch.EventAuthRequired{
		RequestID:     fetch.RequestID("req-1"),
		AuthChallenge: &fetch.AuthChallenge{Source: fetch.AuthChallengeSourceProxy},
	}

	first := make(map[fetch.RequestID]bool)
	if proxyAuthDecision(ev, first, cfg).Resp.Response != fetch.AuthChallengeResponseResponseProvideCredentials {
		t.Fatal("first session: want ProvideCredentials on first challenge")
	}
	if proxyAuthDecision(ev, first, cfg).Resp.Response != fetch.AuthChallengeResponseResponseDefault {
		t.Fatal("first session: want Default on repeat in the same map")
	}

	// A new session starts with a fresh, empty map: the same id is first again.
	second := make(map[fetch.RequestID]bool)
	if len(second) != 0 {
		t.Fatalf("new session map = %v, want empty", second)
	}
	if proxyAuthDecision(ev, second, cfg).Resp.Response != fetch.AuthChallengeResponseResponseProvideCredentials {
		t.Error("second session: want ProvideCredentials again with a fresh map")
	}
}

func TestSurfaceSizeNonZeroFloor(t *testing.T) {
	// A 0x0 grid (the agent shell reports no pixel size) must resolve to the
	// non-zero floor, never a blank 0x0 surface Chromium cannot lay out.
	e := &ChromiumEngine{backend: backendBlock, cellW: 9, cellH: 18}

	surfW, surfH := e.surfaceSize(0, 0)
	if surfW < surfaceFloorPx || surfH < surfaceFloorPx {
		t.Fatalf("surfaceSize(0,0) = (%d, %d), want each >= floor %d", surfW, surfH, surfaceFloorPx)
	}
	if surfW == 0 || surfH == 0 {
		t.Fatalf("surfaceSize(0,0) = (%d, %d), want a non-zero surface", surfW, surfH)
	}
}

func TestPaintedSurfaceHeightFloorNeverExceedsCap(t *testing.T) {
	// The historical fault geometry: a 2528x2838 surface at the fractional
	// Z = 1.975 the removed fixed-width formula produced there. A rounded CSS
	// height (round(2838/1.975) = 1437) paints 1437*1.975 = 2839 device rows, one
	// over the 2838 cap, so Chromium downscales every frame. The floor (1436)
	// paints 2836 <= 2838, so no downscale. Fractional Z still occurs under the
	// density formula (Ghostty's cell 26 gives 3.25), so the floor stays
	// load-bearing.
	const surfW, surfH = 2528, 2838
	const z = 1.975
	cssW, cssH, paintW, paintH := paintedSurface(surfW, surfH, z)
	if paintW > surfW || paintH > surfH {
		t.Fatalf("paintedSurface(%d,%d,%g) painted (%d,%d) exceeds the cap (%d,%d)", surfW, surfH, z, paintW, paintH, surfW, surfH)
	}
	// The width divides exactly (Z is surface over cssW by construction), so it
	// rounds back to the layout width the scale was derived from.
	if cssW != 1280 {
		t.Errorf("cssW = %d, want 1280 (round of 2528/1.975)", cssW)
	}
	// The height floors rather than rounds, so cssH is 1436 not 1437.
	if cssH != 1436 {
		t.Errorf("cssH = %d, want 1436 (floor of 2838/1.975)", cssH)
	}
	if paintH != 2836 {
		t.Errorf("paintH = %d, want 2836 (round(1436*1.975), one row-band under the cap)", paintH)
	}
}

func TestPaintedSurfaceUnityScaleIdentity(t *testing.T) {
	// At Z == 1 (a graphics backend, or a block grid at or under the target width)
	// the painted surface must equal the input surface exactly, so the cap and the
	// compose model are unchanged from the pre-fix behaviour.
	const surfW, surfH = 1440, 720
	cssW, cssH, paintW, paintH := paintedSurface(surfW, surfH, 1)
	if cssW != surfW || cssH != surfH || paintW != surfW || paintH != surfH {
		t.Fatalf("paintedSurface(%d,%d,1) = css(%d,%d) paint(%d,%d), want all equal to the input surface", surfW, surfH, cssW, cssH, paintW, paintH)
	}
}

func TestScreencastCapNeverExceedsSurface(t *testing.T) {
	// screencastCap on the block backend must return a painted surface at or under
	// the real cell-grid surface, so the screencast cap never forces a downscale.
	const cols, rows = 158, 86
	const cellW, cellH = 16, 33
	e := &ChromiumEngine{backend: backendBlock, cellW: cellW, cellH: cellH, cols: cols, rows: rows}

	surfW, surfH := e.surfaceSize(cols, rows)
	capW, capH := e.screencastCap(cols, rows)
	if capW > surfW || capH > surfH {
		t.Fatalf("screencastCap = (%d,%d) exceeds surfaceSize (%d,%d); Chromium would downscale", capW, capH, surfW, surfH)
	}
	// A graphics backend on a logical-point cell runs a density Z of 1, so its
	// cap equals the surface exactly, byte-identical to the pre-density path.
	g := &ChromiumEngine{backend: backendSixel, cellW: cellW, cellH: cellH, cols: cols, rows: rows}
	gSurfW, gSurfH := g.surfaceSize(cols, rows)
	gCapW, gCapH := g.screencastCap(cols, rows)
	if gCapW != gSurfW || gCapH != gSurfH {
		t.Errorf("graphics screencastCap = (%d,%d), want the surface (%d,%d) at Z 1", gCapW, gCapH, gSurfW, gSurfH)
	}
	// A device-pixel cell (Ghostty, WezTerm) runs a fractional density Z, so the
	// painted cap stays at the surface width and within one rounded row of the
	// surface height: capture resolution is set by the terminal grid, never shrunk.
	d := &ChromiumEngine{backend: backendKitty, cellW: 26, cellH: 53, cols: 156, rows: 43}
	dSurfW, dSurfH := d.surfaceSize(156, 43)
	dCapW, dCapH := d.screencastCap(156, 43)
	if dCapW != dSurfW || dCapH > dSurfH || dCapH < dSurfH-1 {
		t.Errorf("density screencastCap = (%d,%d), want width %d and height within one row of %d", dCapW, dCapH, dSurfW, dSurfH)
	}
}
