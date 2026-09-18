package cli

import (
	"context"
	"fmt"

	tui "justsay-harness/cli/tui"
	credentials "justsay-harness/credentials"
	gmail "justsay-harness/integrations/gmail"
)

// gmailLink owns the browser-based OAuth flow. It is a handler of its own
// because an OAuth redirect cannot be represented by the verifier interface's
// sequence of terminal fields.
type gmailLink struct {
	out             *tui.Output
	credentialsPath string
}

func newGmailLink(out *tui.Output) *gmailLink {
	return &gmailLink{out: out, credentialsPath: credentials.DefaultPath}
}

func (g *gmailLink) name() string { return "gmail" }

func (g *gmailLink) summary() string {
	return "authorize Gmail so the agent can send email"
}

func (g *gmailLink) verify(ctx context.Context) {
	g.out.Notice(g.name() + ": " + g.summary())

	cfg, err := gmail.ConfigFromEnv(envPath)
	if err != nil {
		g.out.Errorf("  gmail: cannot start authorization — %v", err)
		return
	}

	g.out.Notice("  A browser must complete Google authorization. The callback is accepted only on this machine.")
	record, err := gmail.Authorize(ctx, cfg, func(authURL string) {
		g.out.Plain("  Open this URL in your browser:")
		g.out.Private("  " + authURL)
	})
	if err != nil {
		g.out.Errorf("  gmail: authorization failed — %v", err)
		return
	}

	store, err := credentials.Open(g.credentialsPath)
	if err != nil {
		g.out.Errorf("  gmail: authorization succeeded but credentials could not be opened — %v", err)
		return
	}
	if err := store.Set(gmail.CredentialsKey, record); err != nil {
		g.out.Errorf("  gmail: authorization succeeded but credentials could not be saved — %v", err)
		return
	}
	if err := store.Save(); err != nil {
		g.out.Errorf("  gmail: authorization succeeded but credentials could not be saved — %v", err)
		return
	}

	g.out.Answer(fmt.Sprintf("  gmail: connected — tokens saved securely in %s", store.Path()))
}
