package telegram

import (
	"context"
	"errors"
	"strconv"

	integrations "justsay-harness/integrations"
)

// This file is what "is this token any good" means for Telegram, kept away from
// the CLI so it can be exercised without a terminal. The command that uses it
// does not stop here — pairing carries on from an accepted token — but where the
// checking ends and the pairing begins is exactly this boundary.

// TokenField is the one credential the Bot API authenticates with.
//
// It is declared here rather than at the prompt so the wording is the
// integration's own, and so the check and the pairing cannot drift into asking
// for the same thing two different ways.
func TokenField() integrations.Field {
	return integrations.Field{
		Key:    "token",
		Label:  "Telegram bot token",
		Help:   "the token BotFather gave you, shaped like 123456789:AA...; it is masked as you type and never written to the transcript",
		Secret: true,
	}
}

// Check looks up the bot a token controls and turns the answer into a verdict.
//
// getMe is the request the Bot API provides for exactly this question: it
// succeeds only for a token Telegram accepts, and answers with the bot that
// token controls.
//
// The bot itself comes back alongside the verdict because what follows a passed
// check needs it — the pairing has to say which bot to send the code to — and
// asking Telegram the same question twice would waste a round trip on a fact it
// has already given.
func Check(ctx context.Context, client *Client) (User, integrations.Result, error) {
	me, err := client.GetMe(ctx)
	result, err := Verdict(me, err)

	return me, result, err
}

// Verdict turns a getMe answer into the three outcomes the CLI tells apart.
//
// A token Telegram refuses is a verdict, not a failure: ok:false comes back as
// Result{OK: false} carrying Telegram's own description, since it explains the
// problem better than a paraphrase would. Anything that stopped the check from
// being made at all stays an error.
//
// It is separate from Check so the mapping can be exercised without a network:
// which of the three a given answer becomes is the part worth being sure of.
func Verdict(me User, err error) (integrations.Result, error) {
	var refusal *Refusal
	if errors.As(err, &refusal) {
		return integrations.Result{OK: false, Summary: refusal.Description}, nil
	}
	if err != nil {
		return integrations.Result{}, err
	}

	return integrations.Result{
		OK:      true,
		Summary: "the token controls @" + me.Username,
		Facts:   Facts(me),
	}, nil
}

// Facts is what a getMe reply is worth showing next to the verdict.
func Facts(me User) []integrations.Fact {
	return []integrations.Fact{
		{Label: "username", Value: "@" + me.Username},
		{Label: "id", Value: strconv.FormatInt(me.ID, 10)},
		{Label: "name", Value: me.FirstName},
		{Label: "can join groups", Value: strconv.FormatBool(me.CanJoinGroups)},
		{Label: "supports inline queries", Value: strconv.FormatBool(me.SupportsInlineQueries)},
	}
}
