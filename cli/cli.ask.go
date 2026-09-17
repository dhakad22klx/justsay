package cli

import (
	"bufio"
	tui "justsay-harness/cli/tui"
	integrations "justsay-harness/integrations"
	session "justsay-harness/session"
	"strings"
)

// asker reads answers from the same stdin the main loop is already reading, so
// a verification happens inside the prompt instead of handing the terminal to
// something else. It owns the scanner rather than opening its own, because two
// readers on one stdin lose input to each other's buffers.
type asker struct {
	in     *bufio.Scanner
	out    *tui.Output
	record *session.Session
}

// field asks for one answer and keeps asking while a required one is left
// blank. The false result means stdin ended: the user gave up, or the input was
// never a terminal, and either way the caller should stop rather than loop.
func (a *asker) field(field integrations.Field) (string, bool) {
	if field.Help != "" {
		a.out.Notice("  " + field.Help)
	}

	label := "  " + field.Label
	if field.Default != "" {
		label += " [" + field.Default + "]"
	}
	label += ": "

	for {
		a.out.Prompt(label)

		typed, ok := a.read(field.Secret)
		if !ok {
			return "", false
		}

		switch value := strings.TrimSpace(typed); {
		case value != "":
			a.remember(field, value)
			return value, true
		case field.Default != "":
			return field.Default, true
		case field.Optional:
			return "", true
		}

		a.out.Warn("  " + field.Label + " is required — Ctrl-D skips this integration")
	}
}

// read takes one line, masking it while it is typed when it is a secret.
func (a *asker) read(secret bool) (string, bool) {
	if !secret {
		return a.line()
	}

	// Masking needs the terminal a character at a time, which only a real
	// terminal will give. Anything else — a pipe, a machine without stty — is
	// read as an ordinary visible line, because refusing the check would be the
	// worse failure.
	if typed, read, masked := tui.MaskedLine(a.out); masked {
		return typed, read
	}

	return a.line()
}

// line reads one whole line from the loop's own scanner. The scanner is shared
// with the main prompt on purpose: a second reader on the same stdin would
// strand input in the other one's buffer.
func (a *asker) line() (string, bool) {
	if !a.in.Scan() {
		return "", false
	}

	return a.in.Text(), true
}

// remember puts the answer in the transcript, since the main loop only records
// what was typed at its own prompt. A secret is noted but not written: the
// point of a saved transcript is what happened, not the token it happened with.
func (a *asker) remember(field integrations.Field, value string) {
	if a.record == nil {
		return
	}

	if field.Secret {
		a.record.Append("user", "<"+field.Key+" withheld>")
		return
	}

	a.record.Append("user", value)
}
