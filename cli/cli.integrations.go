package cli

import (
	"strings"

	tui "justsay-harness/cli/tui"
	"justsay-harness/credentials"
	integrations "justsay-harness/integrations"
	github "justsay-harness/integrations/github"
	"justsay-harness/integrations/gmail"
	"justsay-harness/integrations/telegram"
	providers "justsay-harness/providers"
	session "justsay-harness/session"
)

// integrationStatuses describes saved setup without making network requests or
// exposing credentials. A saved grant is not a live service health check.
func integrationStatuses(path string) []tui.IntegrationStatus {
	rows := []tui.IntegrationStatus{
		{Name: "Gmail", Status: "not-configured", Detail: "/verify gmail"},
		{Name: "Telegram", Status: "not-configured", Detail: "/verify telegram"},
		{Name: "GitHub", Status: "not-configured", Detail: "Setup coming soon"},
	}
	store, err := credentials.Open(path)
	if err != nil {
		for i := 0; i < 2; i++ {
			rows[i].Status = "unavailable"
			rows[i].Detail = "Cannot read saved credentials"
		}
		return rows
	}

	var mail gmail.Record
	found, err := store.Get(gmail.CredentialsKey, &mail)
	switch {
	case err != nil:
		rows[0].Status, rows[0].Detail = "needs attention", "Invalid saved authorization"
	case found && strings.TrimSpace(mail.RefreshToken) != "":
		rows[0].Status, rows[0].Detail, rows[0].Ready = "connected", "Send email", true
	case found:
		rows[0].Status = "needs attention"
	}

	var chat telegram.Record
	found, err = store.Get(telegram.CredentialsKey, &chat)
	switch {
	case err != nil:
		rows[1].Status, rows[1].Detail = "needs attention", "Invalid saved pairing"
	case found && chat.Paired() && strings.TrimSpace(chat.BotToken) != "":
		rows[1].Status, rows[1].Detail, rows[1].Ready = "paired successfully", "Chat with your agent", true
	case found:
		rows[1].Status = "needs attention"
	}
	return rows
}

// newCommands assembles the prompt's command set: the handlers that exist, and
// the dispatcher that parses a line and picks one.
//
// This is the only file that names an integration. commands dispatches over the
// list it is given and never learns what is in it, so adding Slack is a package
// under integrations/ and one more entry here.
//
// in is the loop's own line reader, shared rather than reopened — two readers on
// one stdin strand input in each other's buffers. provider is what Telegram
// answers over, and is the one piece the loop also needs, for its own agent.
func newCommands(out *tui.Output, in lineReader, record *session.Session, provider providers.IProvider, sessionID string) *commands {
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
		newGmailLink(out),
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
