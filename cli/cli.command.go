package cli

import (
	"context"
	"fmt"
	tui "justsay-harness/cli/tui"
	"strings"

	"github.com/joho/godotenv"
)

// commandPrefix marks a line as an instruction to the CLI rather than a question
// for the model.
//
// The split is total on purpose: a line that opens with it never reaches the
// agent. A mistyped command is a mistake the CLI should say out loud, not
// something a model should be left to interpret helpfully.
const commandPrefix = "/"

const (
	mockEnvKey = "MOCK_AGENT_CALL"
	envPath    = ".env"
)

// handler is what a matched command is handed to.
//
// A handler owns the prompt for as long as its work takes: asking for whatever
// it needs, doing it, and reporting the outcome itself. Nothing about that
// belongs here, which is the point — this file parses lines and picks a handler,
// and never learns what any of them do.
type handler interface {
	// name is the word that selects this handler, and what /verify is followed
	// by to reach it.
	name() string

	// summary is the one line the usage listing shows.
	summary() string

	// verify runs the handler's setup. It reports its own progress and its own
	// failures; there is nothing to hand back.
	verify(ctx context.Context)
}

// resumable is a handler that leaves work running after it returns — a poll, a
// listener, anything with a goroutine behind it.
//
// It is separate from handler because most handlers finish when they return, and
// should not have to say so with two empty methods.
type resumable interface {
	// resume restarts work left behind by an earlier run of the program.
	resume(ctx context.Context)

	// stop ends that work and waits for it to be gone.
	stop()
}

// commands parses a slash line and delegates it.
//
// It holds a list of handlers and nothing else: which handlers exist is decided
// in cli.integrations.go, and what one does when it runs is its own business. A
// new integration is a new handler in that list and no change here.
type commands struct {
	out      *tui.Output
	handlers []handler
}

// isCommand reports whether a line is addressed to the CLI.
func isCommand(input string) bool {
	return strings.HasPrefix(strings.TrimSpace(input), commandPrefix)
}

// parseCommand splits a slash line into its name and arguments. A bare "/" has
// no name, which run refuses along with every other line it does not know.
func parseCommand(input string) (string, []string) {
	fields := strings.Fields(strings.TrimPrefix(strings.TrimSpace(input), commandPrefix))
	if len(fields) == 0 {
		return "", nil
	}

	return strings.ToLower(fields[0]), fields[1:]
}

// run acts on one slash command.
//
// Every path out of here either delegates the work or prints a refusal. Nothing
// falls through to the agent, which is the point of the prefix.
func (c *commands) run(ctx context.Context, input string) {
	name, args := parseCommand(input)

	switch name {
	case "verify":
		c.verify(ctx, args)
	case "on":
		setMock(c.out, false)
	case "off":
		setMock(c.out, true)
	default:
		c.out.Errorf("unknown command: %s", strings.TrimSpace(input))
		c.usage()
	}
}

// verify hands the prompt to the named handler.
//
// The name is resolved before anything is asked for or written, so an
// unsupported one is refused rather than half-run.
func (c *commands) verify(ctx context.Context, args []string) {
	if len(args) != 1 {
		c.out.Error("/verify takes one integration name")
		c.usage()

		return
	}

	name := strings.ToLower(args[0])

	for _, target := range c.handlers {
		if target.name() == name {
			target.verify(ctx)
			return
		}
	}

	c.out.Errorf("cannot verify %q", name)
	c.usage()
}

// usage lists what the slash namespace holds, built from the handlers rather
// than from a table kept alongside them, so it cannot fall out of step with what
// actually runs. It follows every refusal, since being told a command is wrong
// is only half of what the user needs.
func (c *commands) usage() {
	c.out.Plain("Commands:")
	c.out.Plain("  /on — send prompts to the model")
	c.out.Plain("  /off — request does not reach the model")
	c.out.Plain("  reset — reset the conversation")
	for _, target := range c.handlers {
		c.out.Plain("  /verify " + target.name() + " — " + target.summary())
	}
}

// resume restarts whatever the handlers left running in an earlier run of the
// program. A handler that has nothing to resume says so by not implementing
// resumable.
func (c *commands) resume(ctx context.Context) {
	for _, target := range c.handlers {
		if background, ok := target.(resumable); ok {
			background.resume(ctx)
		}
	}
}

// stop ends that work, in reverse order, and waits for it to be gone. It pairs
// with resume around the whole life of the prompt.
func (c *commands) stop() {
	for i := len(c.handlers) - 1; i >= 0; i-- {
		if background, ok := c.handlers[i].(resumable); ok {
			background.stop()
		}
	}
}

func setMock(out *tui.Output, mocked bool) {
	env, err := godotenv.Read(envPath)
	if err != nil {
		out.Errorf("cannot read %s: %v", envPath, err)
		return
	}

	env[mockEnvKey] = fmt.Sprintf("%t", mocked)

	if err := godotenv.Write(env, envPath); err != nil {
		out.Errorf("cannot write %s: %v", envPath, err)
		return
	}

	if mocked {
		out.Notice("agent off: prompts are answered locally, and nothing is sent to the model")
	} else {
		out.Notice("agent on: prompts go to the model")
	}
}
