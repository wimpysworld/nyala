package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// decodeFindScriptConfig extracts the JSON configuration buildFindScript
// substituted into the page script and decodes it, so a test asserts on the
// parameters the page really received rather than on a substring match.
func decodeFindScriptConfig(t *testing.T, script string) findConfig {
	t.Helper()
	at := strings.Index(findSource, findConfigPlaceholder)
	if at < 0 {
		t.Fatal("the find script carries no configuration placeholder")
	}
	prefix, suffix := findSource[:at], findSource[at+len(findConfigPlaceholder):]
	if !strings.HasPrefix(script, prefix) || !strings.HasSuffix(script, suffix) {
		t.Fatal("the built script does not wrap its configuration in the page script")
	}
	var cfg findConfig
	raw := script[len(prefix) : len(script)-len(suffix)]
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("decode the substituted configuration %q: %v", raw, err)
	}
	return cfg
}

// findEngine returns an engine on backend b at an 8x16 cell over an 80x24 grid,
// the state the search configuration is derived from, with no browser attached.
func findEngine(b backend) *ChromiumEngine {
	e := New("")
	e.SetCellPx(8, 16)
	e.SetBackend(b)
	e.Resize(80, 24)
	return e
}

// TestBuildFindScriptSubstitutesConfig asserts one query dispatch carries its
// operation, query, match cap, highlight registrations, and colours into the
// page, and that the placeholder is gone so the page never sees it.
func TestBuildFindScriptSubstitutesConfig(t *testing.T) {
	e := findEngine(backendBlock)
	cfg, run := e.findConfigLocked(findOpQuery, "needle", 0)
	if !run {
		t.Fatal("a query dispatched nothing")
	}
	script := buildFindScript(cfg)
	if strings.Contains(script, findConfigPlaceholder) {
		t.Fatal("the built script still carries the configuration placeholder")
	}

	got := decodeFindScriptConfig(t, script)
	if got.Op != findOpQuery || got.Query != "needle" || got.Step != 0 {
		t.Fatalf("script configuration = %+v, want a query for needle with no step", got)
	}
	if got.Max != findMaxMatches {
		t.Fatalf("match cap = %d, want %d", got.Max, findMaxMatches)
	}
	if got.AllName != findAllName || got.CurrentName != findCurrentName || got.StyleID != findStyleID {
		t.Fatalf("highlight identity = %+v, want the engine's registrations", got)
	}
	if got.AllColour != "rgb(255,214,0)" || got.CurrentColour != "rgb(255,122,0)" {
		t.Fatalf("highlight colours = (%q, %q), want the match and current colours",
			got.AllColour, got.CurrentColour)
	}
	if got.StateProperty != findStateProperty {
		t.Fatalf("page state property = %q, want %q", got.StateProperty, findStateProperty)
	}
}

// TestBuildFindScriptEscapesQuery asserts a query carrying quotes, a backslash,
// and a newline reaches the page intact through the JSON configuration, so a
// typed search string can never break out of the script it travels in.
func TestBuildFindScriptEscapesQuery(t *testing.T) {
	const hostile = "a'b\"c\\d\ne</script>"
	e := findEngine(backendBlock)
	cfg, _ := e.findConfigLocked(findOpQuery, hostile, 0)

	got := decodeFindScriptConfig(t, buildFindScript(cfg))
	if got.Query != hostile {
		t.Fatalf("script query = %q, want %q", got.Query, hostile)
	}
}

// TestFindConfigPerBackend asserts the two backend-dependent parameters: the
// highlight paints no glyph ink and a revealing scroll snaps to the cell pitch
// where the backend takes the crisp text overlay, and neither applies where the
// page's own ink is the only text layer.
func TestFindConfigPerBackend(t *testing.T) {
	for _, c := range []struct {
		name  string
		b     backend
		ink   string
		snaps bool
	}{
		{"block", backendBlock, "transparent", true},
		{"kitty", backendKitty, "black", false},
		{"sixel", backendSixel, "black", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := findEngine(c.b)
			cfg, _ := e.findConfigLocked(findOpQuery, "needle", 0)
			if cfg.Ink != c.ink {
				t.Fatalf("highlight ink = %q, want %q", cfg.Ink, c.ink)
			}
			if snaps := cfg.CellHeight > 0; snaps != c.snaps {
				t.Fatalf("cell snap height = %g, want snapping = %v", cfg.CellHeight, c.snaps)
			}
		})
	}
}

// TestFindStepDirection asserts next and previous carry the step direction the
// page walks its match list by, and that both reuse the held query rather than
// re-querying.
func TestFindStepDirection(t *testing.T) {
	e := findEngine(backendBlock)
	e.find = FindState{Query: "needle", Total: 4, Current: 2}

	for _, c := range []struct {
		name string
		step int
	}{{"next", 1}, {"previous", -1}} {
		t.Run(c.name, func(t *testing.T) {
			cfg, run := e.findConfigLocked(findOpStep, "", c.step)
			if !run {
				t.Fatal("a step with an active search dispatched nothing")
			}
			if cfg.Op != findOpStep || cfg.Step != c.step || cfg.Query != "needle" {
				t.Fatalf("step configuration = %+v, want a %s step of the held query", cfg, c.name)
			}
		})
	}
}

// TestFindStepWithoutQueryDispatchesNothing asserts a step with no active search
// is dropped before it reaches the page, so a stray next or previous cannot
// invent a search or clear the state.
func TestFindStepWithoutQueryDispatchesNothing(t *testing.T) {
	e := findEngine(backendBlock)

	if _, run := e.findConfigLocked(findOpStep, "", 1); run {
		t.Fatal("a step with no active search dispatched")
	}
	if e.find != (FindState{}) {
		t.Fatalf("find state = %+v after a dropped step, want no search", e.find)
	}
}

// TestFindEmptyQueryClears asserts an empty query is a clear, not a search for
// nothing: the state resets and the page is told to drop its highlights.
func TestFindEmptyQueryClears(t *testing.T) {
	e := findEngine(backendBlock)
	e.find = FindState{Query: "needle", Total: 4, Current: 3}

	cfg, run := e.findConfigLocked(findOpQuery, "", 0)
	if !run {
		t.Fatal("an empty query dispatched nothing, so the page keeps its highlights")
	}
	if cfg.Op != findOpClear {
		t.Fatalf("empty-query operation = %q, want a clear", cfg.Op)
	}
	if e.find.Query != "" || e.find.Total != 0 || e.find.Current != 0 {
		t.Fatalf("find state = %+v after an empty query, want no search", e.find)
	}
	if !e.find.Pending {
		t.Fatal("the clear left no dispatch pending, so a repaint cannot tell it is in flight")
	}
}

// TestApplyFindResultRecordsCounts asserts a settled dispatch publishes the
// counts the viewer renders and adopts the page's scroll offset as the engine's
// held offset, so a revealing scroll leaves the wheel transport aligned.
func TestApplyFindResultRecordsCounts(t *testing.T) {
	e := findEngine(backendBlock)
	e.find = FindState{Query: "needle", Pending: true}

	e.applyFindResult(findResult{Total: 4, Current: 2, ScrollX: 8, ScrollY: 863.6}, nil)

	want := FindState{Query: "needle", Total: 4, Current: 2}
	if e.find != want {
		t.Fatalf("find state = %+v, want %+v", e.find, want)
	}
	if e.scrollX != 8 || e.scrollY != 864 || e.scrollYTarget != 864 {
		t.Fatalf("held scroll offset = (%d, %d, target %d), want the page offset (8, 864, 864)",
			e.scrollX, e.scrollY, e.scrollYTarget)
	}
}

// TestApplyFindResultKeepsCountsOnFailure asserts a dropped dispatch marks the
// state failed and keeps the last known counts, because the previous search's
// highlights are still painted on the page, and leaves the held scroll offset
// alone, because the page never reported a new one.
func TestApplyFindResultKeepsCountsOnFailure(t *testing.T) {
	e := findEngine(backendBlock)
	e.find = FindState{Query: "needle", Total: 4, Current: 2, Pending: true}
	e.scrollY, e.scrollYTarget = 320, 320

	e.applyFindResult(findResult{}, errNoSession)

	want := FindState{Query: "needle", Total: 4, Current: 2, Failed: true}
	if e.find != want {
		t.Fatalf("find state = %+v, want %+v", e.find, want)
	}
	if e.scrollY != 320 || e.scrollYTarget != 320 {
		t.Fatalf("held scroll offset = (%d, target %d), want it untouched", e.scrollY, e.scrollYTarget)
	}
}

// TestFindWithoutSessionSettlesFailed asserts a search with no live session
// resolves rather than leaving a repaint staring at a pending search forever:
// the query is recorded, the dispatch is marked failed, and nothing panics.
func TestFindWithoutSessionSettlesFailed(t *testing.T) {
	e := findEngine(backendBlock)

	e.Find("needle")

	want := FindState{Query: "needle", Failed: true}
	if got := e.FindState(); got != want {
		t.Fatalf("find state = %+v, want %+v", got, want)
	}
}

// TestFindOnStalledBrowserSettlesFailed asserts the stalled-browser path: a
// dispatch that cannot reach the page settles the state as failed, counts the
// input failure the debug path reads, and keeps the counts the last search
// published. A plain context stands in for a browser that never answers.
func TestFindOnStalledBrowserSettlesFailed(t *testing.T) {
	e := findEngine(backendBlock)
	e.mu.Lock()
	e.ctx = context.Background()
	e.find = FindState{Query: "needle", Total: 4, Current: 2}
	e.mu.Unlock()

	e.FindNext()

	want := FindState{Query: "needle", Total: 4, Current: 2, Failed: true}
	if got := e.FindState(); got != want {
		t.Fatalf("find state = %+v, want %+v", got, want)
	}
	e.mu.Lock()
	failures := e.inputFailures
	e.mu.Unlock()
	if failures != 1 {
		t.Fatalf("inputFailures = %d, want 1 after a dropped search dispatch", failures)
	}
}

// TestResetFindDropsStateOnCommit asserts a main-frame commit drops the search
// state, so a navigated page never shows a count for highlights it does not
// carry.
func TestResetFindDropsStateOnCommit(t *testing.T) {
	e := findEngine(backendBlock)
	e.find = FindState{Query: "needle", Total: 4, Current: 2}

	e.resetFind()

	if got := e.FindState(); got != (FindState{}) {
		t.Fatalf("find state = %+v after a commit, want no search", got)
	}
}
