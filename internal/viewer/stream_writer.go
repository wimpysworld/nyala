package viewer

import (
	"bytes"
	"io"
	"time"
)

func msSince(start time.Time) float64 {
	d := time.Since(start)
	if d < 0 {
		return 0
	}
	return float64(d) / float64(time.Millisecond)
}

// writeItem is one unit the writer goroutine emits to the terminal in order. A
// frame item carries a pooled buffer (buf) the producer assembled plus its encode
// time; the writer returns the buffer on free after the write. A control item
// carries one-off bytes (ctrl) for a resize clear, a kitty reset, or an fps badge;
// it owns no pooled buffer, so the writer emits it and returns nothing. timed
// gates the write-phase clock read to the armed fps meter. tmuxSixel marks a
// tmux Sixel frame, whose write is timed unconditionally because the adaptive
// pacing gate feeds on the duration even with the fps meter off.
type writeItem struct {
	buf       *bytes.Buffer
	ctrl      []byte
	encodeMS  float64
	timed     bool
	tmuxSixel bool
}

// freeItem returns a drained frame buffer to the producer with the frame's encode
// and write times. The producer reuses the buffer and records the paint split on
// its own goroutine, because the fps meter is single-goroutine. A tmuxSixel item
// carries the write duration for the adaptive pacing gate, zero on a failed write.
type freeItem struct {
	buf       *bytes.Buffer
	encodeMS  float64
	writeMS   float64
	writeDur  time.Duration
	timed     bool
	tmuxSixel bool
}

// streamWriter owns every terminal write on one goroutine so the producer's next
// frame encode overlaps the current frame's write. The producer assembles a frame
// into a pooled buffer and hands it over ready; the writer emits it with one
// io.Write, then returns the buffer over free. A single writer draining a cap-1
// ready channel keeps strict frame order (N before N+1). Two pooled buffers
// ping-pong, so encode N+1 runs while write N drains. Control writes ride the same
// ready channel, so a resize clear or a kitty reset stays ordered against the
// frames and never interleaves with an in-flight frame's bytes.
type streamWriter struct {
	tty   io.Writer
	ready chan writeItem
	free  chan freeItem
	errc  chan error
	done  chan struct{}
}

// newStreamWriter starts the writer goroutine. ready is cap 1 for strict order;
// free is cap 2 so the writer never blocks returning either pooled buffer; errc is
// cap 1 for the first write error. The two pooled buffers live on the producer
// side (it holds them to encode into). The caller stops it with stop before its
// own teardown escapes run.
func newStreamWriter(tty io.Writer) *streamWriter {
	w := &streamWriter{
		tty:   tty,
		ready: make(chan writeItem, 1),
		free:  make(chan freeItem, 2),
		errc:  make(chan error, 1),
		done:  make(chan struct{}),
	}
	go w.run()
	return w
}

// run emits each queued item to the terminal in order until ready closes. It times
// only the io.Write (the terminal flush) when the item is timed or a tmux Sixel
// frame, so the fps meter attributes write time apart from the producer's encode
// and the adaptive tmux pacing gate reads the write duration even with the meter
// off. On the first write error, or a short write (n < len with a nil error, which
// tears the frame tail), it records the error once on errc, then discards later
// writes while still returning pooled buffers on free, so a blocked producer
// handoff never deadlocks.
func (w *streamWriter) run() {
	defer close(w.done)
	failed := false
	for item := range w.ready {
		data := item.ctrl
		if item.buf != nil {
			data = item.buf.Bytes()
		}
		var writeMS float64
		var writeDur time.Duration
		if !failed {
			clocked := item.timed || item.tmuxSixel
			var start time.Time
			if clocked {
				start = time.Now()
			}
			n, err := w.tty.Write(data)
			if err == nil && n < len(data) {
				// A short write with a nil error drops the frame tail silently. Treat it
				// as a failure so the io.Writer seam cannot tear frames unnoticed.
				err = io.ErrShortWrite
			}
			if err != nil {
				failed = true
				select {
				case w.errc <- err:
				default:
				}
			} else if clocked {
				writeDur = max(time.Since(start), 0)
				if item.timed {
					writeMS = float64(writeDur) / float64(time.Millisecond)
				}
			}
		}
		if item.buf != nil {
			w.free <- freeItem{buf: item.buf, encodeMS: item.encodeMS, writeMS: writeMS, writeDur: writeDur, timed: item.timed, tmuxSixel: item.tmuxSixel}
		}
	}
}

// send hands one item to the writer with a blocking send. It waits only for the
// writer to take the previous item off the cap-1 ready channel, not for the full
// terminal write, so the producer stays close to real time. The producer is the
// sole sender, so the writer sees items in exactly the producer's call order.
func (w *streamWriter) send(item writeItem) {
	w.ready <- item
}

// stop closes ready so the writer drains any queued item and exits, then waits for
// the goroutine to finish. After it returns no goroutine writes the terminal, so
// the caller's teardown escapes are safe. The producer must not send after stop.
func (w *streamWriter) stop() {
	close(w.ready)
	<-w.done
}
