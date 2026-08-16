package engine

import (
	"testing"

	"github.com/chromedp/cdproto/target"
)

// TestCurrentTitleRecordAndRead verifies the record and read pair, including
// that an empty title is recorded as empty rather than kept stale.
func TestCurrentTitleRecordAndRead(t *testing.T) {
	e := &ChromiumEngine{}
	if got := e.CurrentTitle(); got != "" {
		t.Fatalf("initial title = %q, want empty", got)
	}
	e.recordCurrentTitle("Example Domain")
	if got := e.CurrentTitle(); got != "Example Domain" {
		t.Fatalf("title = %q, want %q", got, "Example Domain")
	}
	e.recordCurrentTitle("")
	if got := e.CurrentTitle(); got != "" {
		t.Fatalf("title after empty record = %q, want empty", got)
	}
}

// TestTitleFromTargetInfo verifies the pure decision the target-info listener
// applies: record for the engine's own target when the title differs, and skip
// a nil info, another target, or an unchanged title.
func TestTitleFromTargetInfo(t *testing.T) {
	own := target.ID("OWN")
	cases := []struct {
		name    string
		info    *target.Info
		current string
		want    string
		ok      bool
	}{
		{name: "nil info", info: nil, ok: false},
		{name: "other target", info: &target.Info{TargetID: "OTHER", Title: "New"}, ok: false},
		{name: "changed title", info: &target.Info{TargetID: own, Title: "New"}, current: "Old", want: "New", ok: true},
		{name: "unchanged title", info: &target.Info{TargetID: own, Title: "Same"}, current: "Same", ok: false},
		{name: "cleared title", info: &target.Info{TargetID: own, Title: ""}, current: "Old", want: "", ok: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := titleFromTargetInfo(tc.info, own, tc.current)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("titleFromTargetInfo = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestTitleFromTargetInfoRecordFlow verifies that applying the decision to the
// engine records a changed title and does not re-record an unchanged one.
func TestTitleFromTargetInfoRecordFlow(t *testing.T) {
	own := target.ID("OWN")
	e := &ChromiumEngine{}
	info := &target.Info{TargetID: own, Title: "First"}
	if title, ok := titleFromTargetInfo(info, own, e.CurrentTitle()); ok {
		e.recordCurrentTitle(title)
	}
	if got := e.CurrentTitle(); got != "First" {
		t.Fatalf("title = %q, want %q", got, "First")
	}
	if _, ok := titleFromTargetInfo(info, own, e.CurrentTitle()); ok {
		t.Fatal("unchanged title reported ok, want skip")
	}
}
