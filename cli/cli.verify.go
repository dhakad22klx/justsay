package cli

import (
	"context"
	"errors"
	"fmt"
	tui "justsay-harness/cli/tui"
	integrations "justsay-harness/integrations"
)

// credential is the part of /verify that every integration does the same way:
// ask for what the service needs, and report what the service said about it.
//
// Both handlers embed it, so both open identically. Where they differ is what an
// accepted credential is good for — GitHub is finished at that point, Telegram
// is only getting started — and that difference is the only thing either handler
// has to spell out.
type credential struct {
	out *tui.Output
	ask *asker
}

// heading opens a command by naming what is about to happen.
func (c *credential) heading(name, summary string) {
	c.out.Notice(name + ": " + summary)
}

// collect asks for each field in turn, in the order the integration listed them.
//
// The false result is the user giving up — stdin ended, or a required answer was
// abandoned. That is not a failure to report as one, so the caller says "skipped"
// and stops.
func (c *credential) collect(fields []integrations.Field) (integrations.Input, bool) {
	input := integrations.Input{}

	for _, field := range fields {
		value, ok := c.ask.field(field)
		if !ok {
			return nil, false
		}

		input[field.Key] = value
	}

	return input, true
}

// report prints one verdict and answers whether the credential was accepted.
//
// The outcomes stay distinct on purpose: a check that could not run is not the
// same as a credential that was refused, and neither is a check that was never
// written. Only the first return value is a yes.
func (c *credential) report(name string, result integrations.Result, err error) bool {
	if errors.Is(err, integrations.ErrNotImplemented) {
		// A placeholder is not a failure, and saying so plainly keeps the
		// skeleton runnable without ever implying a token was judged.
		c.out.Warn("  " + name + ": nothing was checked — this verifier is still a stub")
		return false
	}
	if err != nil {
		c.out.Errorf("  %s: the check could not be made — %v", name, err)
		return false
	}

	if !result.OK {
		c.out.Warn(fmt.Sprintf("  %s: not verified — %s", name, result.Summary))
		return false
	}

	c.out.Answer(fmt.Sprintf("  %s: verified — %s", name, result.Summary))
	for _, fact := range result.Facts {
		c.out.Plain("    " + fact.Label + ": " + fact.Value)
	}

	return true
}

// skipped says a command was abandoned partway. It is a warning rather than an
// error because walking away from a prompt is a choice, not a fault.
func (c *credential) skipped(name string) {
	c.out.Warn("  skipped " + name)
}

// verifierHandler is the handler for an integration that a credential check
// answers completely.
//
// One adapter serves all of them: such an integration writes an IVerifier and no
// CLI code at all. Telegram is the counter-example, and it is a handler of its
// own for exactly that reason.
type verifierHandler struct {
	credential

	verifier integrations.IVerifier
}

// name and summary come from the verifier, so the usage listing describes the
// integration in its own words rather than in a copy kept next to it.
func (v *verifierHandler) name() string { return v.verifier.Name() }

func (v *verifierHandler) summary() string { return v.verifier.Description() }

// verify is the whole of a check-only integration: ask, check, report, done.
func (v *verifierHandler) verify(ctx context.Context) {
	v.heading(v.name(), v.summary())

	input, ok := v.collect(v.verifier.Fields())
	if !ok {
		v.skipped(v.name())
		return
	}

	result, err := v.verifier.Verify(ctx, input)
	v.report(v.name(), result, err)
}
