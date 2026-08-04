package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestChromiumRecipesUseDisposableProfiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the recipes and fake tools use POSIX shell")
	}
	justPath, err := exec.LookPath("just")
	if err != nil {
		t.Fatalf("just is required to test repository recipes: %v", err)
	}
	justfile, err := filepath.Abs(filepath.Join("..", "..", "justfile"))
	if err != nil {
		t.Fatalf("resolve justfile: %v", err)
	}
	toolDir := t.TempDir()
	fakeGo := `#!/bin/sh
case " $* " in
  *" -tags cdpinterop "*|*" -tags live "*) ;;
  *) exit 0 ;;
esac
if [ -z "${NYALA_PROFILE:-}" ] || [ ! -d "$NYALA_PROFILE" ]; then
  exit 20
fi
if [ "$NYALA_PROFILE" = "$NYALA_WATCHED_PROFILE" ]; then
  exit 21
fi
printf '%s\n' "$NYALA_PROFILE" > "$NYALA_RECIPE_RECORD"
printf '%s\n' '    --- PASS: TestRecipeProfile (0.00s)'
`
	for name, body := range map[string]string{
		"go":        fakeGo,
		"chromium":  "#!/bin/sh\nexit 0\n",
		"sixel2png": "#!/bin/sh\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(toolDir, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}

	for _, recipe := range []string{"diagnostics", "cdp-interop", "live", "internal-render-metrics"} {
		t.Run(recipe, func(t *testing.T) {
			recordPath := filepath.Join(t.TempDir(), "profile-path")
			watchedProfile := filepath.Join(t.TempDir(), "watched-profile")
			profileRoot := t.TempDir()
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, justPath, "--justfile", justfile, recipe)
			cmd.Env = append(os.Environ(),
				"PATH="+toolDir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"TMPDIR="+profileRoot,
				"NYALA_PROFILE="+watchedProfile,
				"NYALA_WATCHED_PROFILE="+watchedProfile,
				"NYALA_RECIPE_RECORD="+recordPath,
				"NYALA_BROWSER=",
				"CHROME_PATH=",
			)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("just %s: %v\n%s", recipe, err, output)
			}
			raw, err := os.ReadFile(recordPath)
			if err != nil {
				t.Fatalf("read recorded profile path: %v", err)
			}
			profileDir := strings.TrimSpace(string(raw))
			if profileDir == "" || profileDir == watchedProfile {
				t.Fatalf("recipe profile = %q, want a non-ambient disposable path", profileDir)
			}
			profileName := filepath.Base(profileDir)
			if profileDir != filepath.Join(profileRoot, profileName) {
				t.Fatalf("recipe profile = %q, want a direct child of %q", profileDir, profileRoot)
			}
			entries, err := os.ReadDir(profileRoot)
			if err != nil {
				t.Fatalf("read disposable profile root: %v", err)
			}
			for _, entry := range entries {
				if entry.Name() == profileName {
					t.Fatalf("recipe profile %q still exists after cleanup", profileDir)
				}
			}
			if _, err := os.Stat(watchedProfile); !os.IsNotExist(err) {
				t.Fatalf("ambient profile Stat error = %v, want path absent", err)
			}
		})
	}
}
