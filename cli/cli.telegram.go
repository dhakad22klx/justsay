package cli

import (
	"context"
	"errors"
	"fmt"
	agent "justsay-harness/agent"
	state "justsay-harness/agent/state"
	credentials "justsay-harness/credentials"
	telegram "justsay-harness/integrations/telegram"
	providers "justsay-harness/providers"
	"strings"
	"time"
)

// notifyTimeout bounds the approval message. It is sent while the agent holds
// its turn, so a slow Telegram must not hold the prompt with it.
const notifyTimeout = 15 * time.Second

// telegramLink is dispatched as a handler and bracketed as a resumable. The
// second is asserted at run time by design — most handlers are not resumable —
// so it is pinned here: without this, renaming resume or stop would leave the
// saved pairing silently never coming back up.
var (
	_ handler   = (*telegramLink)(nil)
	_ resumable = (*telegramLink)(nil)
)

// telegramLink is the agent's side of a Telegram conversation: the saved
// pairing, and the background poll that acts on it.
//
// Only one poll may run at a time — Telegram refuses a second getUpdates on the
// same bot — so this owns the cancel that stops it and waits for it to be gone
// before starting another.
type telegramLink struct {
	// The shared opening of every /verify: ask for the credential, check it,
	// report the verdict. Telegram's command is that, and then a pairing.
	credential

	store    *credentials.Store
	provider providers.IProvider

	// session names the run in the state store. It is the prompt's own id: one
	// agent answers both, so both sides of the conversation pause under one key.
	session string

	// unavailable is the reason there is no link to speak of, kept rather than
	// raised at startup: an unreadable credentials file is worth reporting when
	// the user asks for Telegram, not worth refusing to start the agent over.
	unavailable error

	cancel context.CancelFunc
	done   chan struct{}
}

// newTelegramLink opens the credentials file and reports whether a pairing is
// already on disk.
func newTelegramLink(shared credential, provider providers.IProvider, sessionID string) *telegramLink {
	link := &telegramLink{credential: shared, provider: provider, session: sessionID}

	store, err := credentials.Open(credentials.DefaultPath)
	if err != nil {
		link.unavailable = err
		return link
	}

	link.store = store

	return link
}

// name and summary are how this appears to the command dispatcher, which knows
// nothing else about it.
func (t *telegramLink) name() string { return "telegram" }

func (t *telegramLink) summary() string {
	return "pair a chat, so requests can arrive from your phone"
}

// resume brings the link back up from a pairing made in an earlier run, so the
// agent is reachable from Telegram as soon as it starts rather than only after
// someone pairs it again.
func (t *telegramLink) resume(ctx context.Context) {
	if t.unavailable != nil {
		t.out.Warn(fmt.Sprintf("telegram unavailable: %v", t.unavailable))
		return
	}

	record, found := t.record()
	if !found || !record.Paired() {
		return
	}

	t.listen(ctx, record)
}

// verify is what `/verify telegram` runs, and the whole of it: ask for the
// token, check it, show a one-time code, wait for that code to come back from
// Telegram, remember the chat that sent it, and leave the link listening.
//
// It does more than the other integrations' checks because it is a different
// question. Theirs ask whether a credential works and stop; this one hands the
// agent to a chat, and what it writes to disk outlives the run.
func (t *telegramLink) verify(ctx context.Context) {
	if t.unavailable != nil {
		t.out.Errorf("telegram: %v", t.unavailable)
		return
	}

	// A pairing and a listening link would be two polls on one bot, which
	// Telegram refuses. The link comes back at the end, pointed at whichever
	// chat this pairing settles on.
	t.stop()

	t.heading(t.name(), t.summary())

	saved, _ := t.record()

	token, ok := t.token(saved)
	if !ok {
		t.skipped(t.name())
		return
	}

	client := telegram.NewClient(token)

	// The token is checked before a code is minted. Waiting five minutes to
	// discover it was mistyped is a bad way to learn it. This is the same check
	// and the same reporting GitHub gets; everything below is what Telegram
	// alone does with a token that passed.
	me, result, err := telegram.Check(ctx, client)
	if !t.report(t.name(), result, err) {
		return
	}

	who, err := telegram.Pair(ctx, client, t.announceCode(me))
	if errors.Is(err, telegram.ErrCodeExpired) {
		t.out.Warn("  telegram: " + err.Error() + " — run `/verify telegram` to start over")
		return
	}
	if err != nil {
		if ctx.Err() != nil {
			return
		}

		t.out.Errorf("  telegram: pairing failed — %v", err)
		return
	}

	record := telegram.NewRecord(token, who, time.Now())

	// The pairing is reported before the save is judged: it did happen, and
	// saying otherwise because a file could not be written would be wrong. A
	// failed save is a link that works until the agent is restarted, which is
	// worth its own line.
	t.out.Answer("  telegram: paired with " + describe(who))

	if err := t.save(record); err != nil {
		t.out.Errorf("  telegram: the pairing could not be saved, so it will be lost on exit — %v", err)
	} else {
		t.out.Notice("  saved to " + t.store.Path() + ", readable only by you")
	}

	t.listen(ctx, record)
}

// announceCode shows the code and says what to do with it. The pairing package
// hands this out rather than printing, so the CLI stays the only thing writing
// to the terminal.
func (t *telegramLink) announceCode(me telegram.User) func(string, time.Duration) {
	return func(code string, lifetime time.Duration) {
		t.out.Plain("  send this to @" + me.Username + " from the Telegram account you want to pair:")
		t.out.Answer("      /verify " + code)
		t.out.Notice("  good for " + lifetime.String() + ", once; waiting for it to come back…")
	}
}

// token asks for the bot token, or keeps the one already saved.
//
// A saved token is described rather than offered as a Field default, because a
// default is printed at the prompt and this one must not be. A blank answer
// keeps it; anything typed replaces it, which is how a rotated token gets in.
func (t *telegramLink) token(saved telegram.Record) (string, bool) {
	field := telegram.TokenField()

	if saved.BotToken != "" {
		field.Help = "press enter to keep the token already saved, or paste a new one to replace it"
		field.Optional = true
	}

	value, ok := t.ask.field(field)
	if !ok {
		return "", false
	}

	if strings.TrimSpace(value) == "" {
		return saved.BotToken, true
	}

	return value, true
}

// listen starts the background poll for a paired chat, replacing whatever was
// running before it.
func (t *telegramLink) listen(ctx context.Context, record telegram.Record) {
	t.stop()

	client := telegram.NewClient(record.BotToken)
	assistant := agent.GetAgent(t.provider)

	// The same agent as the prompt, not one of its own: a request from a phone
	// lands in the conversation the user has been having. The agent serializes
	// turns, so this goroutine cannot arrive in the middle of a local one.
	listener := &telegram.Listener{
		Client:  client,
		ChatID:  record.AuthorizedChatID,
		Handle:  remoteHandler(assistant, t.session),
		Approve: approvalHandler(assistant),
		Trace:   t.out.Trace,
	}

	// A pause has somewhere to announce to only while this link is up.
	if assistant != nil {
		assistant.SetApprovalNotifier(t.notifier(client, record.AuthorizedChatID))
	}

	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	t.cancel, t.done = cancel, done

	go func() {
		defer close(done)

		if err := listener.Run(ctx); err != nil {
			t.out.Errorf("telegram: the link stopped — %v", err)
		}
	}()
}

// stop ends the background poll and waits for it to finish.
//
// The wait is the point. Cancelling only asks the poll to stop; starting another
// before the old one has let go would leave two polls on one bot for a moment,
// and Telegram answers the second with a refusal.
func (t *telegramLink) stop() {
	if t.cancel == nil {
		return
	}

	t.cancel()
	<-t.done
	t.cancel, t.done = nil, nil

	// Nothing is polling now, so a pause must not block sending buttons to a
	// chat no one is watching.
	if assistant := agent.GetAgent(t.provider); assistant != nil {
		assistant.SetApprovalNotifier(nil)
	}
}

// record reads the saved pairing. A file that exists but holds an entry this
// cannot read is reported rather than treated as "never paired", since the two
// call for different fixes.
func (t *telegramLink) record() (telegram.Record, bool) {
	var record telegram.Record

	found, err := t.store.Get(telegram.CredentialsKey, &record)
	if err != nil {
		t.out.Errorf("telegram: %v", err)
		return telegram.Record{}, false
	}

	return record, found
}

// save writes the pairing to the credentials file.
func (t *telegramLink) save(record telegram.Record) error {
	if err := t.store.Set(telegram.CredentialsKey, record); err != nil {
		return err
	}

	return t.store.Save()
}

// remoteHandler turns a request from Telegram into one agent turn.
//
// A tool that fails, a model that gives up, or no model at all comes back as an
// error and the listener reports it in the chat: the person who asked is not at
// this terminal, so silence would leave them waiting on nothing.
func remoteHandler(assistant *agent.Agent, sessionID string) telegram.Handler {
	return func(ctx context.Context, text string) (string, error) {
		if assistant == nil {
			return "", errors.New("this agent has no model provider configured, so it cannot answer")
		}

		return assistant.Run(ctx, text, sessionID)
	}
}

// notifier asks the paired chat to decide on a held call, with a button for
// each answer.
//
// The send is bounded because it runs while the agent holds its turn: the
// client's own timeout is sized for a held-open poll, and inheriting it would
// freeze the local prompt for a minute on a Telegram hiccup.
func (t *telegramLink) notifier(client *telegram.Client, chatID int64) agent.ApprovalNotifier {
	return func(ctx context.Context, sessionID string, pending state.PendingApproval) {
		ctx, cancel := context.WithTimeout(ctx, notifyTimeout)
		defer cancel()

		text := fmt.Sprintf(
			"Approval needed\n\nsession: %s\n\nThe agent wants to run:\n%s(%s)\n\nApprove to run it, Decline to skip it.",
			sessionID, pending.ToolCall.Name, formatArgs(pending.ToolCall.Args),
		)

		err := client.SendButtons(ctx, chatID, text,
			telegram.Button{Text: "Approve", Data: telegram.ApprovalData(telegram.DecisionApprove, sessionID, pending.ID)},
			telegram.Button{Text: "Decline", Data: telegram.ApprovalData(telegram.DecisionDecline, sessionID, pending.ID)},
		)
		if err != nil {
			t.out.Errorf("telegram: could not ask for approval — %v", err)
		}
	}
}

// approvalHandler turns a tap into the run resuming, or into the held call
// being dropped.
//
// The session id comes from the button rather than from this link: that one
// names the run the buttons were sent for, and it outlives this process.
func approvalHandler(assistant *agent.Agent) telegram.Approvals {
	return func(ctx context.Context, decision, sessionID, approvalID string) (string, error) {
		if assistant == nil {
			return "", errors.New("this agent has no model provider configured, so it cannot answer")
		}

		switch decision {
		case telegram.DecisionApprove:
			return assistant.ResumeApproval(ctx, sessionID, approvalID)
		case telegram.DecisionDecline:
			return assistant.Decline(ctx, sessionID, approvalID)
		}

		return "", fmt.Errorf("unknown decision %q", decision)
	}
}

// describe names a paired identity for the terminal. The chat id is always
// shown, since it is the thing authorisation actually rests on.
func describe(who telegram.Identity) string {
	if who.Username == "" {
		return fmt.Sprintf("chat %d", who.ChatID)
	}

	return fmt.Sprintf("@%s (chat %d)", who.Username, who.ChatID)
}
