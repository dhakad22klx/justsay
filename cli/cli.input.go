package cli

import (
	"io"
	"os"

	tui "justsay-harness/cli/tui"

	"github.com/chzyer/readline"
)

// lineReader owns stdin for the lifetime of the CLI. Keeping one reader is
// important for redirected input, where a buffered reader may already hold the
// answers to follow-up prompts.
type lineReader interface {
	read(prompt string, secret bool) (string, error)
}

// terminalInput gives terminal input to readline, which switches a TTY into
// raw mode while a line is being edited and interprets cursor-key escape
// sequences. The same implementation also handles redirected stdin without
// terminal control sequences.
type terminalInput struct {
	line *readline.Instance
}

func newTerminalInput() (*terminalInput, error) {
	line, err := readline.NewEx(&readline.Config{
		DisableAutoSaveHistory: true,
		HistoryLimit:           100,
		Stdin:                  os.Stdin,
		Stdout:                 os.Stdout,
		Stderr:                 os.Stderr,
		// An empty rendered marker keeps EOF and interrupts from being
		// mistaken for text at the prompt. readline still returns distinct
		// errors for both conditions.
		InterruptPrompt: "\n",
		EOFPrompt:       "\n",
	})
	if err != nil {
		return nil, err
	}

	return &terminalInput{line: line}, nil
}

func (in *terminalInput) read(prompt string, secret bool) (string, error) {
	if !secret {
		in.line.SetPrompt(tui.Magenta(prompt))
		return in.line.Readline()
	}

	config := in.line.GenPasswordConfig()
	config.Prompt = tui.Magenta(prompt)
	config.MaskRune = '*'

	value, err := in.line.ReadPasswordWithConfig(config)
	return string(value), err
}

// Output streams go through readline so messages arriving while the user is
// editing a line can be printed without permanently losing the input buffer.
func (in *terminalInput) stdout() io.Writer { return in.line.Stdout() }
func (in *terminalInput) stderr() io.Writer { return in.line.Stderr() }
func (in *terminalInput) close() error      { return in.line.Close() }
