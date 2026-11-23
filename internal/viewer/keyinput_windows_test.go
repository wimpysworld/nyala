package viewer

import (
	"encoding/binary"
	"testing"

	xwindows "github.com/charmbracelet/x/windows"
)

func TestConsoleKeyRecords(t *testing.T) {
	const hi, lo = 0xd83e, 0xdda1 // U+1F9A1 in UTF-16
	r := &terminalKeyInput{
		pending: []xwindows.InputRecord{
			{EventType: xwindows.WINDOW_BUFFER_SIZE_EVENT},
			testConsoleKey(true, xwindows.VK_SHIFT, 0, 1),
			testConsoleKey(false, 0, 'q', 1),
			testConsoleKey(true, 0, keyEsc, 1),
			testConsoleKey(true, 0, '[', 1),
			testConsoleKey(true, 0, 'A', 1),
			testConsoleKey(true, 0, 'é', 3),
			testConsoleKey(false, 0, hi, 1),
		},
	}
	var got []byte
	var buf [3]byte
	read := func() {
		for {
			n := r.copyKeys(buf[:])
			if n == 0 {
				return
			}
			got = append(got, buf[:n]...)
		}
	}
	read()
	r.pending = []xwindows.InputRecord{
		testConsoleKey(false, 0, lo, 1),
		testConsoleKey(false, xwindows.VK_MENU, '£', 1),
	}
	read()
	if string(got) != "\x1b[Aééé🦡£" {
		t.Fatalf("console bytes = %q, want VT bytes, repeats and Unicode", got)
	}
}

func testConsoleKey(down bool, vk, char, repeats uint16) xwindows.InputRecord {
	record := xwindows.InputRecord{EventType: xwindows.KEY_EVENT}
	if down {
		record.Event[0] = 1
	}
	binary.LittleEndian.PutUint16(record.Event[4:6], repeats)
	binary.LittleEndian.PutUint16(record.Event[6:8], vk)
	binary.LittleEndian.PutUint16(record.Event[10:12], char)
	return record
}
