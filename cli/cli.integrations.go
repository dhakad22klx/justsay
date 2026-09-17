package cli

import (
	"bufio"
	tui "justsay-harness/cli/tui"
	integrations "justsay-harness/integrations"
	github "justsay-harness/integrations/github"
	providers "justsay-harness/providers"
	session "justsay-harness/session"
)

// newCommands assembles the prompt's command set: the handlers that exist, and
// the dispatcher that parses a line and picks one.
//
// This is the only file that names an integration. commands dispatches over the
// list it is given and never learns what is in it, so adding Slack is a package
// under integrations/ and one more entry here.
//
// in is the loop's own scanner, shared rather than reopened — two readers on one
// stdin strand input in each other's buffers. provider is what Telegram answers
// over, and is the one piece the loop also needs, for its own agent.
func newCommands(out *tui.Output, in *bufio.Scanner, record *session.Session, provider providers.IProvider, sessionID string) *commands {
	// The opening every handler shares. It holds the loop's own stdin and this
	// run's transcript, so a handler can ask for a token without handing the
	// terminal to anything else.
	shared := credential{out: out, ask: &asker{in: in, out: out, record: record}}

	handlers := []handler{
		// Telegram brings its own handler. A credential check is where its
		// command starts, not where it ends: it goes on to ask a chat to
		// answer, write what it learns, and leave the agent listening — none of
		// which fits behind IVerifier.
		newTelegramLink(shared, provider, sessionID),
	}

	// Everything else is a credential check and needs no code of its own; the
	// verifier is enough to build a handler from.
	for _, verifier := range newVerifiers().All() {
		handlers = append(handlers, &verifierHandler{credential: shared, verifier: verifier})
	}

	return &commands{out: out, handlers: handlers}
}

// newVerifiers lists the integrations that are answered by a plain credential
// check. Telegram is absent on purpose: it is reached through its own handler
// above, so an entry here would be one nothing could ever dispatch to.
func newVerifiers() *integrations.Registry {
	return integrations.NewRegistry(
		github.New(),
	)
}
