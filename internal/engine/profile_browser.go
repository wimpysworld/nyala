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
	identity := browserIdentity(cfg.execPath)
	marker := filepath.Join(cfg.profileDir, profileBrowserName)
	stored, err := os.ReadFile(marker)
	if err == nil {
		if string(stored) != identity {
			return fmt.Errorf("profile %q belongs to browser %q, not %q; use a separate --profile", cfg.profileDir, stored, identity)
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

func checkLegacyProfileBrowser(guard *profileGuard, cfg allocatorConfig, identity string, exclusive bool) error {
	pid, recorded := guard.readPID()
	if !exclusive || recorded && processAlive(pid) {
		ctx, cancel := context.WithTimeout(context.Background(), adoptConnectTimeout)
		defer cancel()
		args, ok := processCommandLine(ctx, pid)
		command, _, _ := strings.Cut(args, "\x00")
		if command == args {
			command, _, _ = strings.Cut(args, " --")
		}
		command = strings.Trim(strings.TrimSpace(command), `"`)
		if identity != "chromium" || !ok || browserFamily(command) != "chromium" || !commandLineHasArg(args, "--user-data-dir="+cfg.profileDir) {
			return fmt.Errorf("profile %q has a running browser without a matching identity; close its existing Nyala clients or use a separate --profile", cfg.profileDir)
		}
		return nil
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
