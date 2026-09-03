//go:build live

package engine

import (
	"testing"

	"github.com/wimpysworld/nyala/internal/render"
)

// TestLiveWebGLProbeAnswers asserts that the session start probe ran and
// recorded an answer: the tier is no longer unknown once a page exists. It
// asserts nothing about which tier, because headless Chromium on the test
// host can have no WebGL context, and an unavailable answer is still an
// answer. Run it with `just live`.
func TestLiveWebGLProbeAnswers(t *testing.T) {
	e, _ := liveEngineOn(t, liveTestPage)

	if got := e.WebGL(); got.Tier == render.WebGLUnknown {
		t.Fatalf("WebGL().Tier = %v after a session, want a probe answer", got.Tier)
	}
}
