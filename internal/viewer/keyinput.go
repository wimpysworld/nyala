package viewer

import "os"

// readTerminalKeys returns a stop function that waits for the decoder to exit.
// Stop the decoder before restoring the terminal so it cannot read shell input.
func readTerminalKeys(tty *os.File) (<-chan keyEvent, func(), error) {
	input, err := newTerminalKeyInput(tty)
	if err != nil {
		return nil, nil, err
	}
	done := make(chan struct{})
	keys, stopped := readKeys(newKeyDecoder(input, input.setDeadline), done)
	stop := func() {
		close(done)
		input.interrupt()
		<-stopped
		input.close()
	}
	return keys, stop, nil
}
