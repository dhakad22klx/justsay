package cli

import (
	"io"
	"strings"

	tui "justsay-harness/cli/tui"
	integrations "justsay-harness/integrations"
	session "justsay-harness/session"

	"github.com/chzyer/readline"
)

// asker reads answers from the same input owner as the main loop, so a
// verification happens inside the prompt without introducing a competing
// buffered reader on stdin.
type asker struct {
	in     lineReader
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
		typed, ok := a.read(label, field.Secret)
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
func (a *asker) read(prompt string, secret bool) (string, bool) {
	typed, err := a.in.read(prompt, secret)
	if err == io.EOF || err == readline.ErrInterrupt {
		return "", false
	}
	if err != nil {
		a.out.Errorf("error reading input: %v", err)
		return "", false
	}

	return typed, true
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
