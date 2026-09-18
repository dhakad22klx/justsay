# Integrations

```
/verify <name> → handler.verify(ctx)
   │
   ├─ github    verifierHandler.verify  = heading → collect → Verify → report ∎
   │
   ├─ telegram  telegramLink.verify     = heading → collect → Check  → report
   │                                      → Pair → save → listen
   │
   └─ gmail     gmailLink.verify        = OAuth URL → loopback callback
                                          → token exchange → save ∎
```

`/verify github`, `/verify telegram`, and `/verify gmail` set a service up without leaving the
prompt. A line opening with `/` is the CLI's to answer and never reaches the
model, so an unsupported one is refused out loud rather than handed to something
that will try to make sense of it.

GitHub's check is still a stub, returning `ErrNotImplemented` — reported as
"nothing was checked", never as a verdict.

## Shape

- `integrations.IVerifier` — what a check-only integration implements: name,
  description, the `Field`s it needs, and `Verify`.
- `integrations.Registry` — name to verifier, so the dispatcher holds no list of
  services.
- `cli/cli.command.go` — parses a slash line and hands it to a handler, or
  refuses it. Names no integration and imports none.
- `cli/cli.verify.go` — `credential`, the opening every command shares, and
  `verifierHandler`, the one adapter that serves every check-only integration.
- `cli/cli.ask.go` — asks for each field on the loop's own stdin.
- `cli/cli.integrations.go` — builds the handler list; the only file naming an
  integration.

The seam between parsing and doing is `cli.handler`: a name, a summary, and
`verify`. The dispatcher matches the name, calls `verify`, and learns nothing
else, so adding a command touches no parsing code.

The typed-credential handlers embed `credential` and so open identically — ask, check, report.
`report` answers whether the credential was accepted, and that is where the two
part: GitHub is finished, Telegram carries on into pairing. An integration only
writes a handler of its own when a check is not the whole story; such a handler
may also implement `cli.resumable`, which brackets the life of the prompt so
background work is restarted at startup and stopped on the way out.

A verifier never reads stdin and never prints. It is handed a map of answers and
returns a verdict, which makes it testable and keeps the terminal under the
CLI's control. It also matters mechanically: two readers on one stdin lose input
to each other's buffers.

## The outcomes

Kept apart because they call for different reactions:

- `Result{OK: true}` — the service confirmed the credential.
- `Result{OK: false}` — the check ran and the credential was refused; `Summary`
  says why.
- a non-nil `error` — the check could not be made at all: unreachable host,
  unreadable reply, an endpoint that is not the API it claims to be.
- `ErrNotImplemented` — the check does not exist yet, so a stub can never read
  as a judgement on a token.

## Secrets

A `Field` marked `Secret` shows an asterisk per character and is never written
to the transcript; `<key withheld>` is recorded instead.

Masking needs the terminal a character at a time, which
`cli/tui/terminal.secret.go` takes with `stty`. When it cannot — a pipe, no
`stty` — the line is read visibly rather than the check refusing to run. The
masking is a courtesy; the transcript rule always holds.

## Filling in a stub

Replace the `Verify` body. Each package's doc comment on `Verify` records the
request to make and how to read the answer; the service's `.md` has the links.

There is no shared HTTP helper. Telegram brought its own client because the Bot
API puts the token in the URL path and every error must be scrubbed before it
can be shown — a constraint GitHub does not have. When the GitHub check is
written and the two sit side by side, that is the moment to see what is actually
common.

## Credentials that outlive the run

A check only reads a credential. Anything that must be trusted again next time
writes one, and `credentials/` is where it goes: one JSON file, an entry per
integration, created readable by its owner alone and listed in `.gitignore`.

The store does not know what an entry holds — an integration hands over a value
to save and a value to decode into — so the shape of a credential stays in the
package that understands it. Unrecognised entries survive a save untouched, so
one integration writing cannot drop another's.

## Adding an integration

1. New package under `integrations/`, one type implementing `IVerifier`.
2. Add it to the list in `cli/cli.integrations.go`.

Nothing else changes. Parsing, prompting, secret handling and reporting are all
written against the interface.

Gmail is intentionally a dedicated handler rather than an `IVerifier`: it uses
an OAuth authorization-code flow with PKCE and a temporary loopback callback,
not terminal-entered credentials. Its `gmail_send` tool loads and refreshes the
saved grant internally. The model sees only message fields and never receives
OAuth application credentials or user tokens.
