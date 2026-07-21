package viewer

import (
	"bytes"
	"math"
	"os"
	"time"

	"github.com/charmbracelet/x/ansi"
	emmb64 "github.com/emmansun/base64"
	"golang.org/x/sys/unix"

	"github.com/wimpysworld/nyala/internal/cellraster"
)

// probeResult is the outcome of the startup terminal handshake: which graphics
// backends the terminal supports and the real terminal cell pixel size.
type probeResult struct {
	Sixel    bool   // the terminal answered DA1 with the sixel attribute (element "4")
	Kitty    bool   // the terminal answered the kitty direct graphics query with OK
	KittySHM bool   // the terminal answered the kitty shm transport query with OK for its image id
	TermID   string // XTVERSION reply text, e.g. "kitty 0.40.1"; empty when the terminal ignores the query
	cellPx          // resolved cell pixel size
}

// kittyDirectID and kittySHMID are the image ids of the two kitty graphics
// probes. Distinct ids let the per-id reply parser attribute each ";OK" to the
// transport that earned it.
const (
	kittyDirectID = "31"
	kittySHMID    = "32"
)

// da1Prefix introduces a DA1 reply; da1Terminator ends it. The handshake reads
// until this terminator arrives, because DA1 is the fence: a terminal answers it
// after any kitty or cell-size replies, so its arrival means the buffer is whole.
const (
	da1Prefix     = "\x1b[?"
	da1Terminator = byte('c')
)

// probeTimeout caps the whole handshake. A bare TTY never answers DA1, so the
// read stops here with no graphics support detected.
const probeTimeout = 200 * time.Millisecond

// probePollTimeout bounds one poll wait inside the handshake loop, so the loop
// rechecks the cumulative deadline often.
const probePollTimeout = 120 * time.Millisecond

// queryBytes builds the single handshake write: the kitty direct graphics
// query, the kitty shm transport query when shmName names a prepared POSIX
// shared-memory object holding a 1x1 f=24 pixel, the three cell-size queries,
// the XTVERSION identity query, then the DA1 fence. The kitty queries store
// nothing (a=q); DA1 is last so its reply marks the end of the batch. Terminals
// that ignore a query stay silent, and the DA1 fence still bounds the read.
func queryBytes(shmName string) []byte {
	var b bytes.Buffer
	b.WriteString(ansi.KittyGraphics([]byte("AAAA"), "i="+kittyDirectID, "s=1", "v=1", "a=q", "t=d", "f=24"))
	if shmName != "" {
		payload := emmb64.StdEncoding.EncodeToString([]byte(shmName))
		b.WriteString(ansi.KittyGraphics([]byte(payload), "i="+kittySHMID, "s=1", "v=1", "a=q", "t=s", "f=24"))
	}
	b.WriteString("\x1b[16t")
	b.WriteString("\x1b[14t")
	b.WriteString("\x1b[18t")
	b.WriteString("\x1b[>0q")
	b.WriteString(ansi.RequestPrimaryDeviceAttributes)
	return b.Bytes()
}

// probeTerminal runs one handshake on the raw tty and returns the detected
// support and cell pixel size. cols and rows are the terminal size in cells from
// GetSize, used to derive a cell pixel size from the text-area reply when needed.
// It writes the query, polls for replies until the DA1 terminator arrives or the
// deadline passes, then parses the buffer with the pure functions below. On any
// I/O error it returns a zero result, so detection fails closed to block only.
func probeTerminal(tty *os.File, cols, rows int) probeResult {
	res := probeResult{cellPx: cellPx{W: cellraster.CellPxW, H: cellraster.CellPxH}}

	shmName, shmErr := createProbeSHM()
	if shmErr == nil {
		// A kitty-like terminal unlinks the object itself after reading it;
		// this deferred unlink covers every other terminal and the timeout and
		// error paths, so the probe never leaks an shm object.
		defer unlinkProbeSHM(shmName)
	} else {
		shmName = ""
	}

	if _, err := tty.Write(queryBytes(shmName)); err != nil {
		return res
	}

	fd := int(tty.Fd())
	buf := readUntilDA1(fd)

	res.Kitty = parseKittySupport(buf, kittyDirectID)
	res.KittySHM = shmName != "" && parseKittySupport(buf, kittySHMID)
	res.Sixel = parseDA1Sixel(buf)
	res.TermID = parseXTVersion(buf)
	if px, ok := parseCellPixels(buf, cols, rows); ok {
		res.cellPx = px
	}
	return res
}

// readUntilDA1 reads bytes from the tty fd with unix.Poll until the buffer holds
// a DA1 terminator or the cumulative deadline passes, then returns the buffer. It
// uses Poll rather than SetReadDeadline because SetReadDeadline returns
// ErrNoDeadline on stdin.
func readUntilDA1(fd int) []byte {
	if fd < 0 || fd > math.MaxInt32 {
		return nil
	}
	pollFD := int32(fd)
	deadline := time.Now().Add(probeTimeout)
	var buf []byte
	chunk := make([]byte, 256)
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		wait := probePollTimeout
		if remaining < wait {
			wait = remaining
		}
		fds := []unix.PollFd{{Fd: pollFD, Events: unix.POLLIN}}
		n, err := unix.Poll(fds, int(wait.Milliseconds()))
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return buf
		}
		if n == 0 {
			continue
		}
		rn, rerr := unix.Read(fd, chunk)
		if rn > 0 {
			buf = append(buf, chunk[:rn]...)
			if hasDA1Terminator(buf) {
				return buf
			}
		}
		if rerr != nil {
			return buf
		}
	}
	return buf
}

// hasDA1Terminator reports whether buf holds a complete DA1 reply: a CSI that
// starts with "\x1b[?" and ends with 'c' after that prefix.
func hasDA1Terminator(buf []byte) bool {
	i := bytes.Index(buf, []byte(da1Prefix))
	if i < 0 {
		return false
	}
	return bytes.IndexByte(buf[i+len(da1Prefix):], da1Terminator) >= 0
}

// parseKittySupport reports whether the buffer holds a kitty graphics reply
// carrying ";OK" for the probe with the given image id. The terminal answers
// each a=q query with an APC introduced by "\x1b_G" whose control data echoes
// the probe's i= key. It matches the id as a whole comma-separated element, so
// id "32" never matches "320". An error reply or no reply for the id yields
// false.
func parseKittySupport(buf []byte, id string) bool {
	search := buf
	for {
		i := bytes.Index(search, []byte("\x1b_G"))
		if i < 0 {
			return false
		}
		rest := search[i+3:]
		body := rest
		next := len(rest)
		if end := bytes.Index(rest, []byte("\x1b\\")); end >= 0 {
			body = rest[:end]
			next = end + 2
		}
		control := body
		if sep := bytes.IndexByte(body, ';'); sep >= 0 {
			control = body[:sep]
		}
		if kittyControlHasID(control, id) && bytes.Contains(body, []byte(";OK")) {
			return true
		}
		search = rest[next:]
	}
}

// kittyControlHasID reports whether the comma-separated APC control data holds
// an element exactly equal to "i=<id>".
func kittyControlHasID(control []byte, id string) bool {
	want := []byte("i=" + id)
	for _, p := range bytes.Split(control, []byte(",")) {
		if bytes.Equal(p, want) {
			return true
		}
	}
	return false
}

// parseXTVersion extracts the terminal identity from an XTVERSION reply, a DCS
// introduced by "\x1bP>|" and ended by the ST "\x1b\\". It returns the text
// between them, e.g. "kitty 0.40.1", or an empty string when no reply is
// present. When the ST is missing it cuts at the next escape byte, so a
// following DA1 reply never bleeds into the identity.
func parseXTVersion(buf []byte) string {
	i := bytes.Index(buf, []byte("\x1bP>|"))
	if i < 0 {
		return ""
	}
	rest := buf[i+4:]
	if end := bytes.IndexByte(rest, '\x1b'); end >= 0 {
		rest = rest[:end]
	}
	return string(rest)
}

// parseDA1Sixel reports whether the DA1 reply lists the sixel attribute. It
// extracts the parameter list between "\x1b[?" and the trailing 'c', splits on
// ';', and tests for an element exactly equal to "4". It matches the element, not
// a substring, so "64" and "14" do not false-match.
func parseDA1Sixel(buf []byte) bool {
	i := bytes.Index(buf, []byte(da1Prefix))
	if i < 0 {
		return false
	}
	rest := buf[i+len(da1Prefix):]
	end := bytes.IndexByte(rest, da1Terminator)
	if end < 0 {
		return false
	}
	for _, p := range bytes.Split(rest[:end], []byte(";")) {
		if bytes.Equal(p, []byte("4")) {
			return true
		}
	}
	return false
}

// parseCellPixels resolves the cell pixel size from the handshake replies. It
// tries the 16t reply first (cell size direct), then the 14t reply (text-area
// pixels) divided by the cell counts from the 18t reply, the winsize, or the
// passed-in cols and rows. It reports ok=false when no reply yields a positive
// pair. The CSI t replies order parameters height ; width, the opposite of
// GetSize.
func parseCellPixels(buf []byte, cols, rows int) (cellPx, bool) {
	if h, w, found := parseCSIt(buf, 6); found && w > 0 && h > 0 {
		return cellPx{W: w, H: h}, true
	}
	if h, w, found := parseCSIt(buf, 4); found && w > 0 && h > 0 {
		c, r := cols, rows
		if rr, cc, ok18 := parseCSIt(buf, 8); ok18 && cc > 0 && rr > 0 {
			c, r = cc, rr
		}
		if c > 0 && r > 0 {
			return cellPx{W: w / c, H: h / r}, true
		}
	}
	return cellPx{}, false
}

// parseCSIt parses a CSI t report "\x1b[<id>;A;Bt" and returns A and B. It scans
// every CSI t report in the buffer and returns the first whose leading id field
// equals id. found is false when no matching report exists.
func parseCSIt(buf []byte, id int) (a, b int, found bool) {
	search := buf
	for {
		i := bytes.Index(search, []byte("\x1b["))
		if i < 0 {
			return 0, 0, false
		}
		rest := search[i+2:]
		end := bytes.IndexByte(rest, 't')
		if end < 0 {
			return 0, 0, false
		}
		fields := bytes.Split(rest[:end], []byte(";"))
		if len(fields) == 3 {
			if gotID, ok := atoiBytes(fields[0]); ok && gotID == id {
				av, aok := atoiBytes(fields[1])
				bv, bok := atoiBytes(fields[2])
				if aok && bok {
					return av, bv, true
				}
			}
		}
		search = rest[end+1:]
	}
}

// atoiBytes parses a non-negative decimal integer from b. ok is false on an empty
// or non-digit field, so a malformed report does not yield a bogus zero.
func atoiBytes(b []byte) (n int, ok bool) {
	if len(b) == 0 {
		return 0, false
	}
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}
