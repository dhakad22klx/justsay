package telegram

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

const (
	// CodeLifetime is how long a verification code is worth sending. Short
	// enough that a code left on screen stops meaning anything, long enough to
	// find the chat and type it.
	CodeLifetime = 5 * time.Minute

	// codeCeiling makes a six-digit code: enough that guessing inside the
	// lifetime is hopeless once attempts are capped, short enough to retype
	// from another device without a mistake.
	codeCeiling = 1_000_000

	// MaxAttempts is how many wrong codes one chat may send before it is
	// ignored for the rest of the pairing. Without it a script could walk the
	// whole six-digit space; with it, a chat gets five tries in five minutes.
	MaxAttempts = 5

	// pairPollWait is shorter than a listening poll, so the deadline on an
	// expired code is noticed while the user is still watching the terminal.
	pairPollWait = 20 * time.Second
)

// ErrCodeExpired is the pairing running out of time. It is its own error
// because it is the one failure that is not a problem: the user walked away, and
// the answer is to run the command again.
var ErrCodeExpired = errors.New("the verification code expired before it was used")

// Identity is who a completed pairing bound the agent to.
type Identity struct {
	ChatID   int64
	Username string
}

// Pair generates a one-time code and waits for it to come back from Telegram,
// returning the chat that sent it.
//
// This is the whole of the trust decision, and it rests on one thing: the code
// is known only to whoever can see the terminal the agent is running in. A
// stranger who finds the bot can send it messages but cannot know the code, so
// the chat that produces it is the chat sitting at this machine.
//
// announce is how the code reaches that person. It is a callback rather than a
// writer because a verifier in this package must not hold the terminal — the
// CLI owns every byte the user sees, and a callback keeps it that way while
// still letting the code be shown before the wait begins, which is the one
// thing a return value could not do.
func Pair(ctx context.Context, client *Client, announce func(code string, lifetime time.Duration)) (Identity, error) {
	code, err := NewCode()
	if err != nil {
		return Identity{}, err
	}

	// Anything Telegram was already holding is discarded first. A code sent
	// before the code existed cannot be an answer to it, and clearing the
	// backlog keeps an old conversation from being replayed into this one.
	offset, err := client.SkipBacklog(ctx)
	if err != nil {
		return Identity{}, err
	}

	deadline := time.Now().Add(CodeLifetime)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	announce(code, CodeLifetime)

	// Per-chat counters, so one chat guessing wildly cannot lock out the person
	// who is about to type the right code, and so the hint is offered once
	// rather than to every message.
	attempts := map[int64]int{}
	hinted := map[int64]bool{}

	// Hints are best effort: a failed reply to an unpaired sender must not
	// abort pairing for the intended user.

	for {
		updates, err := client.GetUpdates(ctx, offset, pairPollWait)
		if err != nil {
			// A cancelled or expired context surfaces here as a failed request,
			// since the poll was the thing waiting. Which of the two it was is
			// the difference between "you stopped" and "it timed out".
			if ctx.Err() != nil {
				if !time.Now().Before(deadline) {
					return Identity{}, ErrCodeExpired
				}

				return Identity{}, ctx.Err()
			}

			return Identity{}, err
		}

		for _, update := range updates {
			offset = update.ID + 1

			message := update.Message
			if message == nil || strings.TrimSpace(message.Text) == "" {
				continue
			}

			given, ok := ParseCode(message.Text)
			if !ok {
				// Before pairing, /verify is the only thing the bot answers.
				// Anything else gets one nudge and is then ignored, because the
				// sender is not yet anybody and a bot that keeps replying is a
				// bot that can be made to send mail for free.
				if !hinted[message.Chat.ID] {
					hinted[message.Chat.ID] = true
					_ = client.SendMessage(ctx, message.Chat.ID, "Send /verify followed by the code shown in the agent's terminal.")
				}

				continue
			}

			if attempts[message.Chat.ID] >= MaxAttempts {
				continue
			}

			// Compared in constant time. The margin this buys over a network is
			// slim, but the cost of doing it is a function call.
			if subtle.ConstantTimeCompare([]byte(given), []byte(code)) == 1 {
				return Identity{ChatID: message.Chat.ID, Username: senderName(message)}, nil
			}

			attempts[message.Chat.ID]++
			left := MaxAttempts - attempts[message.Chat.ID]
			if left > 0 {
				_ = client.SendMessage(ctx, message.Chat.ID, fmt.Sprintf("That code is not right. %d attempt(s) left.", left))
				continue
			}

			_ = client.SendMessage(ctx, message.Chat.ID, "That code is not right. No attempts left — start again from the agent's terminal.")
		}
	}
}

// ParseCode pulls the code out of a message.
//
// Both spellings are accepted: "/verify 123456", which is what the bot is told
// to ask for, and a bare "123456", which is what someone typing on a phone
// actually sends. A group chat rewrites the command as /verify@thebot, so the
// suffix is allowed too.
func ParseCode(text string) (string, bool) {
	fields := strings.Fields(strings.TrimSpace(text))

	switch len(fields) {
	case 1:
		if IsCode(fields[0]) {
			return fields[0], true
		}

		return "", false
	case 2:
		command := strings.ToLower(fields[0])
		if name, _, found := strings.Cut(command, "@"); found {
			command = name
		}

		if command == "/verify" && IsCode(fields[1]) {
			return fields[1], true
		}

		return "", false
	}

	return "", false
}

// IsCode reports whether word has the shape of a code, so a message that merely
// mentions a number is not counted as a failed attempt.
func IsCode(word string) bool {
	if len(word) != codeDigits() {
		return false
	}

	for _, digit := range word {
		if digit < '0' || digit > '9' {
			return false
		}
	}

	return true
}

// codeDigits is how wide a code is printed, derived from the ceiling so the two
// cannot drift apart.
func codeDigits() int { return len(fmt.Sprint(codeCeiling)) - 1 }

// NewCode mints a code from the system's cryptographic source. A predictable
// code would let someone who knows when the agent started guess their way in,
// which is the one thing this whole flow exists to prevent.
func NewCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(codeCeiling))
	if err != nil {
		return "", fmt.Errorf("cannot generate a verification code: %w", err)
	}

	return fmt.Sprintf("%0*d", codeDigits(), n.Int64()), nil
}

// senderName is the account that sent the message, for the record written to
// disk. It is a label for a person reading the file later, never something the
// agent decides on.
func senderName(message *Message) string {
	if message.From != nil && message.From.Username != "" {
		return message.From.Username
	}
	if message.Chat.Username != "" {
		return message.Chat.Username
	}
	if message.From != nil {
		return message.From.FirstName
	}

	return ""
}
