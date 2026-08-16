package block

import "unicode/utf8"

// Hyperlink is an OSC 8 hyperlink carried as a cell attribute. Every cell of
// one run shares the same *Hyperlink pointer, and a nil pointer means the cell
// carries no link.
type Hyperlink struct {
	// ID is the OSC 8 id parameter that groups the cells of one link.
	ID string
	// URI is the link target.
	URI string
}

// Maximum sizes accepted by SanitiseHyperlink. These match the VTE and iTerm2
// limits, the strictest of the mainstream OSC 8 terminals.
const (
	maxHyperlinkURIBytes = 2083
	maxHyperlinkIDBytes  = 250
)

// SanitiseHyperlink validates an id and URI and returns a Hyperlink, or nil
// when the pair is unsafe or oversized. It rejects any byte below 0x20, the
// DEL byte 0x7F, and any C1 control (U+0080 to U+009F) in either field,
// because such bytes can terminate or forge the OSC sequence. It rejects an
// empty URI, a URI over 2083 bytes, and an id over 250 bytes. It never
// truncates: a shorter URI is a different, wrong target, so an oversized link
// is dropped whole.
func SanitiseHyperlink(id, uri string) *Hyperlink {
	if uri == "" || len(uri) > maxHyperlinkURIBytes || len(id) > maxHyperlinkIDBytes {
		return nil
	}
	if containsControl(id) || containsControl(uri) {
		return nil
	}
	return &Hyperlink{ID: id, URI: uri}
}

// containsControl reports whether s contains a C0 control, DEL, or a C1
// control (U+0080 to U+009F).
func containsControl(s string) bool {
	for i := 0; i < len(s); {
		b := s[i]
		if b < 0x20 || b == 0x7F {
			return true
		}
		if b < utf8.RuneSelf {
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r >= 0x80 && r <= 0x9F {
			return true
		}
		i += size
	}
	return false
}

// appendLinkOpen appends the OSC 8 open sequence for l, always with the id=
// parameter and an ST terminator, never BEL.
func appendLinkOpen(dst []byte, l *Hyperlink) []byte {
	dst = append(dst, "\x1b]8;id="...)
	dst = append(dst, l.ID...)
	dst = append(dst, ';')
	dst = append(dst, l.URI...)
	return append(dst, "\x1b\\"...)
}

// appendLinkClose appends the OSC 8 close sequence with an ST terminator.
func appendLinkClose(dst []byte) []byte {
	return append(dst, "\x1b]8;;\x1b\\"...)
}

// sameHyperlink reports whether a and b are the same link. It is nil-fast and
// then compares values, because the compose path rebuilds Hyperlink pointers
// each frame, so pointer identity would defeat the cell diff.
func sameHyperlink(a, b *Hyperlink) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.ID == b.ID && a.URI == b.URI
}
