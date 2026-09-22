package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	agent "justsay-harness/agent"
	tui "justsay-harness/cli/tui"
	providers "justsay-harness/providers"
	session "justsay-harness/session"

	"github.com/chzyer/readline"
	"github.com/google/uuid"
)

// StartCli runs the read-prompt-answer loop until the user leaves. Once terminal
// setup succeeds, out owns application messages while readline owns prompts and
// the line currently being edited.
func StartCli() {
	ctx := context.Background()
	in, err := newTerminalInput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error preparing terminal input: %v\n", err)
		return
	}
	defer in.close()

	out := tui.NewOutputTo(in.stdout(), in.stderr())

	out.Banner("Welcome to the Just-Say! Your personal AI assistant")

	// Every run gets its own transcript, and the id it was filed under is the
	// last thing the user sees, however they leave.
	session := newSession(out)
	if session != nil {
		out.Record(session)
		defer closeSession(out, session)
	}

	// One id names this run wherever it is answered from, so a turn paused for
	// approval is found under the same key whether it started here or over
	// Telegram. The transcript's id when there is one, since that is what the
	// user is shown on the way out.
	runID := sessionKey(session)

	// The prompt still works without a provider; only answering needs one. The
	// provider is kept rather than passed straight through, because Telegram
	// reaches for the same agent, built on the same provider.
	provider := newProvider(ctx, out)

	// One agent for the whole process; nil when no model is configured, which
	// the paths that use it report for themselves.
	assistant := agent.GetAgent(provider)

	// Attached here and only here. One agent serves both callers, so the trace
	// is the agent's rather than a caller's — a Telegram request is announced by
	// the listener ("telegram ← …") just before, which is what makes the calls
	// after it attributable. Writing the field at startup, before the poll's
	// goroutine exists, is also what keeps the read in that goroutine safe.
	if assistant != nil {
		assistant.OnToolCall = func(call providers.ToolCall, result providers.ToolResult) {
			out.Tracef("Tool Call· %s(%s)", call.Name, formatArgs(call.Args))
			if result.IsError {
				out.Warn("  " + result.Output)
			}
		}
	}

	// Everything reachable by a slash command, assembled once. The loop below
	// only decides what a line is; this decides what to do about it, and owns
	// whatever a command leaves running — a pairing saved by an earlier run
	// starts polling here, and is stopped on the way out.
	cmds := newCommands(out, in, session, provider, runID)
	cmds.resume(ctx)
	defer cmds.stop()

	for {
		typed, err := in.read("justsay>", false)
		if err == readline.ErrInterrupt {
			out.Farewell("Goodbye!")
			return
		}
		if err != nil {
			if err != io.EOF {
				out.Errorf("error reading input: %v", err)
			}
			break
		}

		input := strings.TrimSpace(typed)

		// What the user typed never passes through out, so the transcript has
		// to be told about it here.
		if session != nil && input != "" {
			session.Append("user", input)
		}

		switch {
		case input == "":
			continue
		case input == "exit":
			out.Farewell("Goodbye!")
			return
		case input == "help":
			out.Plain("Available commands: help, reset, exit")
			cmds.usage()
		case input == "reset":
			if assistant != nil {
				assistant.Reset()
			}
			out.Notice("conversation cleared")
		case isCommand(input):
			// A slash line belongs to the CLI. It never reaches the model, so
			// one that is not recognised is refused here rather than answered.
			cmds.run(ctx, input)
		default:
			answer(ctx, out, assistant, input, runID)
			// assistant.Resume(ctx, input) -- to test Resume function by providing session id as input
		}
	}
}

// newSession opens this run's transcript, or nil when it cannot be written: a
// missing log is worth a warning, never a refusal to start.
func newSession(out *tui.Output) *session.Session {
	record, err := session.Start(session.DefaultDir)
	if err != nil {
		out.Warn(fmt.Sprintf("not recording this session: %v", err))
		return nil
	}

	return record
}

// closeSession finishes the transcript and leaves the id behind, so the user
// knows which file this run just became.
func closeSession(out *tui.Output, record *session.Session) {
	out.Notice("session " + record.ID())
	out.Record(nil)

	if err := record.Close(); err != nil {
		out.Warn(fmt.Sprintf("transcript incomplete: %v", err))
	}
}

// sessionKey names this run for the state store. A transcript that failed to
// open leaves the run without an id of its own, and a pause still has to be
// findable, so one is minted instead.
func sessionKey(record *session.Session) string {
	if record == nil {
		return uuid.NewString()
	}

	return record.ID()
}

// newProvider picks the model provider to run with, or nil when none is
// configured. Returning the interface keeps the rest of the CLI vendor-neutral.
func newProvider(ctx context.Context, out *tui.Output) providers.IProvider {
	gemini, err := providers.NewGemini(ctx)
	if err != nil {
		out.Errorf("gemini unavailable: %v", err)
		return nil
	}

	out.Notice("model: " + gemini.Model())

	return gemini
}

// answer runs one prompt through the agent and prints the result.
func answer(ctx context.Context, out *tui.Output, assistant *agent.Agent, input string, sessionID string) {
	if assistant == nil {
		out.Error("cannot answer: the agent is not configured")
		return
	}

	reply, err := assistant.Run(ctx, input, sessionID)
	if err != nil {
		out.Errorf("error: %v", err)
		return
	}

	out.Answer(reply)
}

// formatArgs renders tool arguments compactly for the trace line.
func formatArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}

	encoded, err := json.Marshal(args)
	if err != nil {
		return fmt.Sprint(args)
	}

	return string(encoded)
}
