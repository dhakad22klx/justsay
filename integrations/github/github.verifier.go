// Package github verifies a GitHub token. The check itself is not written yet;
// what is here is the shape it will be written into.
package github

import (
	"context"

	integrations "justsay-harness/integrations"
)

// defaultAPI is the public API. GitHub Enterprise lives somewhere else, which
// is why the host is a field rather than a constant the user cannot reach.
const defaultAPI = "https://api.github.com"

// Verifier will check a personal access token, OAuth token or installation
// token. It holds no state, so one instance serves every check.
type Verifier struct{}

// New builds the verifier.
func New() *Verifier { return &Verifier{} }

// Name is what the user types.
func (v *Verifier) Name() string { return "github" }

// Description is the line shown when listing integrations.
func (v *Verifier) Description() string {
	return "checks a GitHub token by looking up the account it authenticates"
}

// Fields asks for the token, and lets the base URL be overridden for
// Enterprise without making everyone else type it.
func (v *Verifier) Fields() []integrations.Field {
	return []integrations.Field{
		{
			Key:    "token",
			Label:  "GitHub token",
			Help:   "a personal access token, OAuth token or installation token; it is masked as you type and never written to the transcript",
			Secret: true,
		},
		{
			Key:     "api",
			Label:   "API base URL",
			Help:    "press enter unless this is GitHub Enterprise",
			Default: defaultAPI,
		},
	}
}

// Verify will call GET {api}/user with the token as a bearer credential: the
// cheapest request that succeeds only for a credential GitHub accepts. A 200
// carries the account to report, a 401 is a rejected token, and a 403 is a
// token GitHub knows but will not act on — those last two are verdicts to
// return, not errors to raise.
func (v *Verifier) Verify(ctx context.Context, in integrations.Input) (integrations.Result, error) {
	return integrations.Result{}, integrations.ErrNotImplemented
}
