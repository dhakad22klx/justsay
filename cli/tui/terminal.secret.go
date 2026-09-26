package tui

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
)

// Keys the reader has to recognise itself: a terminal handed over character by
// character stops interpreting them on its own.
const (
	keyBackspace = 0x08
	keyDelete    = 0x7f
	keyEndOfText = 0x04 // Ctrl-D
	keyEscape    = 0x1b
	keyFirstText = 0x20 // everything below this is a control character
)

// MaskedLine reads one line from the terminal, printing an asterisk for each
// character typed instead of the character itself. Backspace rubs one out
// again, and Ctrl-D on an empty line gives up the way it does at the prompt.
//
// The third result is false when this terminal cannot be driven a character at
// a time — input arriving from a pipe, or a machine without stty. Nothing has
// been read in that case, and the caller should read the line however it
// normally would: a visible token beats a check that refuses to run. What keeps
// a secret out of the saved transcript is the caller, not this.
func MaskedLine(out *Output) (line string, read bool, masked bool) {
	restore, ok := takeTerminal()
	if !ok {
		return "", false, false
	}

	// The terminal has to come back even if the user interrupts mid-token: a
	// shell left with no echo and no line editing is worse than a lost prompt.
	release := onInterrupt(restore)
	defer func() {
		release()
		restore()
	}()

	typed, read := readMasked(out)
	if read {
		// Enter was consumed rather than echoed, so the line ends here.
		out.Break()
	}

	return typed, read, true
}

// readMasked collects bytes until the line ends, keeping the mask in step with
// what has been typed. false means the input ended before a line did.
func readMasked(out *Output) (string, bool) {
	var typed []byte
	buf := make([]byte, 1)

	for {
		count, err := os.Stdin.Read(buf)
		if err != nil || count == 0 {
			return "", false
		}

		switch character := buf[0]; {
		case character == '\r' || character == '\n':
			return string(typed), true

		case character == keyBackspace || character == keyDelete:
			if len(typed) > 0 {
				typed = typed[:len(typed)-1]
				// Step back over the asterisk, blank it, step back again.
				out.Mask("\b \b")
			}

		case character == keyEndOfText:
			// Ctrl-D means give up, but only when there is nothing to submit.
			if len(typed) == 0 {
				return "", false
			}

		case character == keyEscape:
			drainEscape()

		case character < keyFirstText:
			// Tab, Ctrl-anything: not text, so neither the token nor the mask
			// should grow.

		default:
			typed = append(typed, character)
			out.Mask("*")
		}
	}
}

// drainEscape swallows the rest of an escape sequence. Arrow keys and Home
// arrive as one, and every byte after the first is printable, so they have to
// be taken together or they would land in the token as characters nobody typed.
//
// A bare Escape has nothing following it, so this waits for the next key; that
// is the price of not mistaking an arrow key for input.
func drainEscape() {
	buf := make([]byte, 1)

	count, err := os.Stdin.Read(buf)
	if err != nil || count == 0 {
		return
	}

	// Anything else was a two-byte sequence, already finished.
	if buf[0] != '[' && buf[0] != 'O' {
		return
	}

	// A control sequence runs until a byte in the 0x40..0x7e range ends it.
	for {
		count, err := os.Stdin.Read(buf)
		if err != nil || count == 0 {
			return
		}
		if buf[0] >= 0x40 && buf[0] <= 0x7e {
			return
		}
	}
}

// takeTerminal switches the terminal to character-at-a-time mode and returns
// the call that puts it back. The old settings are saved verbatim rather than
// assumed, so restoring cannot clobber something the user's shell had set.
func takeTerminal() (restore func(), ok bool) {
	saved, err := stty("-g")
	if err != nil {
		return nil, false
	}

	// -echo stops the terminal showing what is typed, which is what lets a mask
	// stand in for it; -icanon delivers each character as it arrives instead of
	// a line at a time, which is what makes a mask possible at all.
	if _, err := stty("-echo", "-icanon", "min", "1", "time", "0"); err != nil {
		return nil, false
	}

	return func() {
		if _, err := stty(strings.TrimSpace(saved)); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "cannot restore terminal settings: %v\n", err)
		}
	}, true
}

// onInterrupt restores the terminal if the process is signalled while a secret
// is being typed. Exiting here costs the transcript only its closing line,
// since every entry before this was already on disk, and it beats handing back
// a shell the user has to reset.
func onInterrupt(restore func()) (release func()) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	done := make(chan struct{})

	go func() {
		select {
		case <-signals:
			restore()
			// 130 is the conventional exit for a program stopped by Ctrl-C.
			os.Exit(130)
		case <-done:
		}
	}()

	return func() {
		signal.Stop(signals)
		close(done)
	}
}

// stty drives the terminal on stdin, because stty acts on the terminal it is
// given rather than on the one its parent happens to have.
func stty(args ...string) (string, error) {
	command := exec.Command("stty", args...)
	command.Stdin = os.Stdin

	saved, err := command.Output()

	return string(saved), err
}
