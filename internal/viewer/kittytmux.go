package viewer

import (
	"crypto/rand"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/wimpysworld/nyala/internal/render/kitty"
)

const kittyPlaceholder = '\U0010EEEE'

const (
	tmuxKittyActive = iota + 1
	tmuxKittyCleared
)

// tmuxKittyPlacement owns the two pane image IDs and the reused cell buffer.
// Only the stream producer paints or resets the grid. The signal watcher can
// read the immutable deletes and the atomic paint state during teardown.
type tmuxKittyPlacement struct {
	ids     [2]int
	deletes string
	phase   atomic.Uint32
	cells   []byte
}

func newTmuxKittyPlacement() *tmuxKittyPlacement {
	first := randomKittyColourID(0)
	second := randomKittyColourID(first)
	return &tmuxKittyPlacement{
		ids:     [2]int{first, second},
		deletes: tmuxDeleteKittyImage(first) + tmuxDeleteKittyImage(second),
	}
}

func randomKittyColourID(exclude int) int {
	for {
		var data [3]byte
		_, _ = rand.Read(data[:]) // crypto/rand.Read always fills data or terminates the process.
		id := int(data[0])<<16 | int(data[1])<<8 | int(data[2])
		// IDs above 255 stay out of the range used by indexed colour clients.
		if id > 255 && id != exclude {
			return id
		}
	}
}

func tmuxDeleteKittyImage(id int) string {
	return kitty.WrapTmuxAPC(kitty.DeleteImage(id))
}

func tmuxKittyGridFits(cols, rows int) bool {
	limit := len(kittyPlaceholderDiacritics)
	return cols > 0 && cols <= limit && rows > 0 && rows <= limit
}

func (s *State) encodeTmuxKittyPNG(data string, w io.Writer) error {
	cols, rows := s.Cols, engineRows(s.Rows)
	if cols <= 0 || rows <= 0 {
		return nil
	}
	if !tmuxKittyGridFits(cols, rows) {
		limit := len(kittyPlaceholderDiacritics)
		return fmt.Errorf("tmux Kitty page grid %dx%d exceeds %dx%d: reduce the pane size or use --backend block", cols, rows, limit, limit)
	}
	if s.kittyTmuxPaint == nil {
		s.kittyTmuxPaint = newTmuxKittyPlacement()
	}
	p := s.kittyTmuxPaint
	next := p.ids[0]
	if s.kittyID == next {
		next = p.ids[1]
	}
	if err := s.kittyEnc.EncodeTmuxPNGID(data, next, cols, rows, w); err != nil {
		return err
	}
	p.phase.Store(tmuxKittyActive)
	p.cells = appendTmuxKittyCells(p.cells[:0], next, cols, rows)
	if _, err := w.Write(p.cells); err != nil {
		return err
	}
	if s.kittyID != 0 {
		if _, err := io.WriteString(w, tmuxDeleteKittyImage(s.kittyID)); err != nil {
			return err
		}
	}
	s.kittyID = next
	return nil
}

func appendTmuxKittyCells(dst []byte, id, cols, rows int) []byte {
	// tmux needs delayed wrap to attach combining marks to the last column.
	// Each row starts with CUP, not a line feed. The final CUP also cancels a
	// pending wrap before restoring the session's no-wrap mode.
	dst = append(dst, "\x1b[?7h\x1b[38;2;"...)
	dst = strconv.AppendInt(dst, int64(id>>16&255), 10)
	dst = append(dst, ';')
	dst = strconv.AppendInt(dst, int64(id>>8&255), 10)
	dst = append(dst, ';')
	dst = strconv.AppendInt(dst, int64(id&255), 10)
	dst = append(dst, 'm')
	for y, rowMark := range kittyPlaceholderDiacritics[:rows] {
		dst = append(dst, "\x1b["...)
		dst = strconv.AppendInt(dst, int64(y+1), 10)
		dst = append(dst, ";1H"...)
		for _, colMark := range kittyPlaceholderDiacritics[:cols] {
			dst = utf8.AppendRune(dst, kittyPlaceholder)
			dst = utf8.AppendRune(dst, rowMark)
			dst = utf8.AppendRune(dst, colMark)
		}
	}
	return append(dst, cursorHome+"\x1b[?7l"+sgrReset...)
}

func (s *State) resetTmuxKitty(w io.Writer) error {
	p := s.kittyTmuxPaint
	if p == nil || p.phase.Load() != tmuxKittyActive {
		return nil
	}
	if _, err := io.WriteString(w, p.deletes); err != nil {
		return err
	}
	p.phase.Store(tmuxKittyCleared)
	s.kittyID = 0
	return nil
}

func (p *tmuxKittyPlacement) exitSequence() string {
	if p == nil {
		return ""
	}
	switch p.phase.Load() {
	case tmuxKittyActive:
		// The image delete does not erase placeholder text from the tmux pane.
		return p.deletes + sgrReset + clearScreen
	case tmuxKittyCleared:
		// A fatal signal can interrupt a queued reset before the delete writes.
		return p.deletes
	default:
		return ""
	}
}

// tmuxKittyTTY keeps scoped deletion last, including when a fatal signal
// arrives while the stream writer has another frame queued.
type tmuxKittyTTY struct {
	mu     sync.Mutex
	tty    io.Writer
	closed bool
}

// Write sends terminal data unless teardown already ran.
func (t *tmuxKittyTTY) Write(data []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return 0, io.ErrClosedPipe
	}
	return t.tty.Write(data)
}

func (t *tmuxKittyTTY) finish(sequence string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	_, _ = io.WriteString(t.tty, sequence)
}
