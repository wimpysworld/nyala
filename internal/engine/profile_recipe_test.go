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
		t.Skipf("just is required to test repository recipes: %v", err)
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
case "$NYALA_RECIPE_RESULT" in
  fail) printf '%s\n' '--- FAIL: TestRecipeProfile (0.00s)'; exit 1 ;;
  skip) printf '%s\n' '--- SKIP: TestRecipeBrowser (0.00s)' ;;
  empty) exit 0 ;;
esac
printf '%s\n' '    --- PASS: TestRecipeProfile (0.00s)'
`
	for name, body := range map[string]string{
		"go":        fakeGo,
		"sixel2png": "#!/bin/sh\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(toolDir, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	for _, name := range []string{"bash", "sh", "env", "mktemp", "rm", "tee", "grep", "sed"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("find recipe tool %s: %v", name, err)
		}
		if err := os.Symlink(path, filepath.Join(toolDir, name)); err != nil {
			t.Fatalf("link recipe tool %s: %v", name, err)
		}
	}

	for _, tc := range []struct {
		recipe  string
		result  string
		wantErr bool
	}{
		{recipe: "diagnostics", result: "pass"},
		{recipe: "cdp-interop", result: "pass"},
		{recipe: "live", result: "pass"},
		{recipe: "internal-render-metrics", result: "pass"},
		{recipe: "diagnostics", result: "fail", wantErr: true},
		{recipe: "live", result: "fail", wantErr: true},
		{recipe: "live", result: "skip", wantErr: true},
		{recipe: "live", result: "empty", wantErr: true},
	} {
		t.Run(tc.recipe+"/"+tc.result, func(t *testing.T) {
			recordPath := filepath.Join(t.TempDir(), "profile-path")
			watchedProfile := filepath.Join(t.TempDir(), "watched-profile")
			profileRoot := t.TempDir()
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, justPath, "--justfile", justfile, tc.recipe)
			cmd.Env = append(os.Environ(),
				"PATH="+toolDir,
				"TMPDIR="+profileRoot,
				"NYALA_PROFILE="+watchedProfile,
				"NYALA_WATCHED_PROFILE="+watchedProfile,
				"NYALA_RECIPE_RECORD="+recordPath,
				"NYALA_RECIPE_RESULT="+tc.result,
				"NYALA_BROWSER=",
				"CHROME_PATH=",
			)
			if output, err := cmd.CombinedOutput(); (err != nil) != tc.wantErr {
				t.Fatalf("just %s: %v, want error: %t\n%s", tc.recipe, err, tc.wantErr, output)
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
