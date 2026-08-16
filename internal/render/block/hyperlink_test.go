package block

import (
	"strings"
	"testing"
)

func TestSanitiseHyperlink(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		id   string
		uri  string
		ok   bool
	}{
		{name: "clean", id: "a1", uri: "https://example.com/", ok: true},
		{name: "empty uri", id: "a1", uri: "", ok: false},
		{name: "hostile escape injection", id: "a1", uri: "https://x/\x1b]8;;evil\x07", ok: false},
		{name: "c0 in id", id: "a\x01b", uri: "https://example.com/", ok: false},
		{name: "del in uri", id: "a1", uri: "https://x/\x7f", ok: false},
		{name: "c1 control in uri", id: "a1", uri: "https://x/\u0085", ok: false},
		{name: "c1 control in id", id: "a\u009cb", uri: "https://example.com/", ok: false},
		{name: "uri at 2083 bytes", id: "a1", uri: "h" + strings.Repeat("x", 2082), ok: true},
		{name: "uri at 2084 bytes", id: "a1", uri: "h" + strings.Repeat("x", 2083), ok: false},
		{name: "id at 250 bytes", id: strings.Repeat("i", 250), uri: "https://example.com/", ok: true},
		{name: "id at 251 bytes", id: strings.Repeat("i", 251), uri: "https://example.com/", ok: false},
		{name: "empty id allowed", id: "", uri: "https://example.com/", ok: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := SanitiseHyperlink(tc.id, tc.uri)
			if tc.ok && got == nil {
				t.Fatalf("SanitiseHyperlink(%q, %q) = nil, want link", tc.id, tc.uri)
			}
			if !tc.ok && got != nil {
				t.Fatalf("SanitiseHyperlink(%q, %q) = %+v, want nil", tc.id, tc.uri, got)
			}
			if tc.ok && (got.ID != tc.id || got.URI != tc.uri) {
				t.Fatalf("SanitiseHyperlink altered values: got %+v", got)
			}
		})
	}
}

func TestHyperlinkAppendRoundTrip(t *testing.T) {
	t.Parallel()
	l := SanitiseHyperlink("n7", "https://example.com/page")
	if l == nil {
		t.Fatal("SanitiseHyperlink returned nil for a clean link")
	}
	buf := appendLinkOpen(nil, l)
	buf = append(buf, "text"...)
	buf = appendLinkClose(buf)
	want := "\x1b]8;id=n7;https://example.com/page\x1b\\text\x1b]8;;\x1b\\"
	if string(buf) != want {
		t.Fatalf("round trip = %q, want %q", buf, want)
	}
}

func TestSameHyperlink(t *testing.T) {
	t.Parallel()
	a := &Hyperlink{ID: "1", URI: "https://a/"}
	cases := []struct {
		name string
		a, b *Hyperlink
		want bool
	}{
		{name: "both nil", a: nil, b: nil, want: true},
		{name: "nil value", a: nil, b: a, want: false},
		{name: "value nil", a: a, b: nil, want: false},
		{name: "equal values distinct pointers", a: a, b: &Hyperlink{ID: "1", URI: "https://a/"}, want: true},
		{name: "differing uri", a: a, b: &Hyperlink{ID: "1", URI: "https://b/"}, want: false},
		{name: "differing id", a: a, b: &Hyperlink{ID: "2", URI: "https://a/"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sameHyperlink(tc.a, tc.b); got != tc.want {
				t.Fatalf("sameHyperlink = %v, want %v", got, tc.want)
			}
		})
	}
}
