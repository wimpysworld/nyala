package viewer

import (
	"io"
	"math"
	"os"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	xwindows "github.com/charmbracelet/x/windows"
	"golang.org/x/sys/windows"
)

// terminalKeyInput waits for console records or a stop event. ReadConsole
// skips non-key records and can block even after the console signals input.
// ReadConsoleInput consumes those records without starting a pending read.
// The console already encodes VT input as characters in key records.
type terminalKeyInput struct {
	handles  [2]windows.Handle
	deadline time.Time
	records  [128]xwindows.InputRecord
	pending  []xwindows.InputRecord
	high     rune
	encoded  [2 * utf8.UTFMax]byte
	size     int
	offset   int
	repeats  int
}

func newTerminalKeyInput(tty *os.File) (*terminalKeyInput, error) {
	wake, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return nil, err
	}
	return &terminalKeyInput{handles: [2]windows.Handle{wake, windows.Handle(tty.Fd())}}, nil
}

func (r *terminalKeyInput) setDeadline(deadline time.Time) {
	r.deadline = deadline
}

func (r *terminalKeyInput) interrupt() { _ = windows.SetEvent(r.handles[0]) }
func (r *terminalKeyInput) close()     { _ = windows.CloseHandle(r.handles[0]) }

func (r *terminalKeyInput) Read(p []byte) (int, error) {
	for len(p) > 0 {
		if n := r.copyKeys(p); n != 0 {
			return n, nil
		}
		if err := r.waitForRecords(); err != nil {
			return 0, err
		}
	}
	return 0, nil
}

func (r *terminalKeyInput) waitForRecords() error {
	wait := uint32(windows.INFINITE)
	if !r.deadline.IsZero() {
		ms := (time.Until(r.deadline) + time.Millisecond - 1).Milliseconds()
		if ms <= 0 {
			return os.ErrDeadlineExceeded
		}
		wait = uint32(min(ms, math.MaxUint32-1))
	}
	event, err := windows.WaitForMultipleObjects(r.handles[:], false, wait)
	if err != nil {
		return err
	}
	switch event {
	case uint32(windows.WAIT_TIMEOUT):
		return os.ErrDeadlineExceeded
	case windows.WAIT_OBJECT_0 + 1:
		var n uint32
		if err := xwindows.ReadConsoleInput(r.handles[1], &r.records[0], uint32(len(r.records)), &n); err != nil {
			return err
		}
		r.pending = r.records[:n]
		return nil
	default:
		return io.EOF
	}
}

func (r *terminalKeyInput) copyKeys(p []byte) int {
	n := 0
	for n < len(p) {
		if r.repeats == 0 {
			if len(r.pending) == 0 {
				break
			}
			record := r.pending[0]
			r.pending = r.pending[1:]
			r.encodeKey(record)
			continue
		}
		copied := copy(p[n:], r.encoded[r.offset:r.size])
		n += copied
		r.offset += copied
		if r.offset == r.size {
			r.offset = 0
			r.repeats--
		}
	}
	return n
}

func (r *terminalKeyInput) encodeKey(record xwindows.InputRecord) {
	if record.EventType != xwindows.KEY_EVENT {
		return
	}
	key := record.KeyEvent()
	// Ignore key releases apart from Alt+numpad input and Unicode surrogates,
	// which can arrive on release. Synthetic VT records have no scan code.
	if !key.KeyDown && key.VirtualKeyCode != xwindows.VK_MENU && !utf16.IsSurrogate(key.Char) {
		return
	}
	if key.Char == 0 && (!key.KeyDown || key.VirtualScanCode != 0 || key.VirtualKeyCode != 0) {
		return
	}
	if key.Char >= 0xd800 && key.Char <= 0xdbff && r.high == 0 {
		r.high = key.Char
		return
	}
	encoded := r.encoded[:0]
	if r.high != 0 {
		decoded := utf16.DecodeRune(r.high, key.Char)
		r.high = 0
		if decoded != utf8.RuneError {
			key.Char = decoded
		} else {
			encoded = utf8.AppendRune(encoded, utf8.RuneError)
			if key.Char >= 0xd800 && key.Char <= 0xdbff {
				r.high = key.Char
				r.size = len(encoded)
				r.repeats = 1
				return
			}
		}
	}
	r.size = len(utf8.AppendRune(encoded, key.Char))
	r.repeats = max(1, int(key.RepeatCount))
}
