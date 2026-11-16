package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const profileBrowserName = ".nyala-browser"

func browserIdentity(path string) string {
	if family := browserFamily(path); family != "" {
		return family
	}
	if canonical, err := filepath.EvalSymlinks(path); err == nil {
		return canonical
	}
	return path
}

func checkProfileBrowser(guard *profileGuard, cfg allocatorConfig, exclusive bool) error {
	if exclusive {
		if err := clearStaleProfilePID(guard); err != nil {
			return err
		}
	}
	identity := browserIdentity(cfg.execPath)
	marker := filepath.Join(cfg.profileDir, profileBrowserName)
	stored, err := os.ReadFile(marker)
	if err == nil {
		if string(stored) != identity {
			return fmt.Errorf("profile %q belongs to browser %q, not %q; use a separate --profile", cfg.profileDir, stored, identity)
		}
		if pid, recorded := guard.readPID(); recorded && processAlive(pid) {
			return checkProfileBrowserProcess(pid, cfg, browserFamily(cfg.execPath))
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read profile browser identity: %w", err)
	}
	if err := checkLegacyProfileBrowser(guard, cfg, identity, exclusive); err != nil {
		return err
	}
	if !exclusive {
		return nil
	}
	return writeProfileBrowser(marker, identity)
}

func clearStaleProfilePID(guard *profileGuard) error {
	pid, recorded := guard.readPID()
	if !recorded || !processAlive(pid) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), adoptConnectTimeout)
	defer cancel()
	args, ok := processCommandLine(ctx, pid)
	if ok && args != "" && !strings.Contains(args, "--user-data-dir") {
		// A different path spelling can name the same profile. Only clear a PID
		// with no profile argument, so spawnShared cannot adopt its stale port.
		return guard.writePID(0)
	}
	return nil
}

func checkLegacyProfileBrowser(guard *profileGuard, cfg allocatorConfig, identity string, exclusive bool) error {
	pid, recorded := guard.readPID()
	if !exclusive || recorded && processAlive(pid) {
		if identity != "chromium" {
			return fmt.Errorf("profile %q has a running browser without a matching identity; close its existing Nyala clients or use a separate --profile", cfg.profileDir)
		}
		return checkProfileBrowserProcess(pid, cfg, "chromium")
	}
	if identity != "chromium" {
		for _, name := range []string{"Default", "Local State"} {
			if _, err := os.Stat(filepath.Join(cfg.profileDir, name)); !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("profile %q has browser data without an identity; use a new --profile for %q", cfg.profileDir, identity)
			}
		}
	}
	return nil
}

func checkProfileBrowserProcess(pid int, cfg allocatorConfig, family string) error {
	ctx, cancel := context.WithTimeout(context.Background(), adoptConnectTimeout)
	defer cancel()
	args, ok := processCommandLine(ctx, pid)
	ownerArgs := args
	command, rest, _ := strings.Cut(ownerArgs, "\x00")
	switch filepath.Base(command) {
	case "bash", "sh":
		// Non-exec shell wrappers retain the script as the next argument.
		// Do not search shell options or command payloads for browser names.
		ownerArgs = rest
		command, _, _ = strings.Cut(ownerArgs, "\x00")
		if strings.HasPrefix(command, "-") {
			command = ""
		}
	}
	command, _, _ = strings.Cut(command, " --")
	command = strings.Trim(strings.TrimSpace(command), `"`)
	// Nix wrappers exec a hidden binary with the original browser name.
	name := filepath.Base(command)
	if strings.HasPrefix(name, ".") && strings.HasSuffix(name, "-wrapped") {
		command = strings.TrimSuffix(strings.TrimPrefix(name, "."), "-wrapped")
	}
	ownerFamily := browserFamily(command)
	if filepath.Base(command) == "flatpak" {
		ownerFamily = flatpakBrowserFamily(ownerArgs)
	}
	// Custom launchers can exec a differently named browser, so their marker
	// identifies the launcher and only profile ownership is checked here.
	if !ok || family != "" && ownerFamily != family || !commandLineHasArg(args, "--user-data-dir="+cfg.profileDir) {
		return fmt.Errorf("profile %q has a running browser without a matching identity; close its existing Nyala clients or use a separate --profile", cfg.profileDir)
	}
	return nil
}

func flatpakBrowserFamily(commandLine string) string {
	args := strings.Split(commandLine, "\x00")
	if len(args) < 3 || args[1] != "run" {
		return ""
	}
	for _, arg := range args[2:] {
		// Export wrappers put these options before the application ID.
		if strings.HasPrefix(arg, "--branch=") || strings.HasPrefix(arg, "--arch=") {
			continue
		}
		switch arg {
		case "org.chromium.Chromium", "com.google.Chrome", "com.brave.Browser", "com.microsoft.Edge":
			return browserFamily(arg)
		default:
			return ""
		}
	}
	return ""
}

func writeProfileBrowser(marker, identity string) error {
	file, err := os.CreateTemp(filepath.Dir(marker), ".nyala-browser-*")
	if err != nil {
		return fmt.Errorf("create profile browser identity: %w", err)
	}
	defer os.Remove(file.Name())
	_, writeErr := file.WriteString(identity)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return fmt.Errorf("write profile browser identity: %w", err)
	}
	if err := os.Rename(file.Name(), marker); err != nil {
		return fmt.Errorf("record profile browser identity: %w", err)
	}
	return nil
}
