package gmail

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"justsay-harness/credentials"
)

// ErrReconnect means the saved grant can no longer authenticate Gmail calls.
var ErrReconnect = errors.New("saved Gmail authorization is unavailable or no longer valid; reconnect Gmail with /verify gmail")

// Locks are shared by credential path across Tool instances in this process.
// The lock covers loading, refreshing, and persisting a grant. Gmail requests
// run outside it, so independent sends can proceed concurrently.
var tokenLocks = struct {
	sync.Mutex
	byPath map[string]*tokenLock
}{byPath: make(map[string]*tokenLock)}

type tokenLock struct {
	busy chan struct{}
	refs int
}

func lockTokens(ctx context.Context, path string) (func(), error) {
	tokenLocks.Lock()
	lock := tokenLocks.byPath[path]
	if lock == nil {
		lock = &tokenLock{busy: make(chan struct{}, 1)}
		tokenLocks.byPath[path] = lock
	}
	lock.refs++
	tokenLocks.Unlock()
	releaseRef := func() {
		tokenLocks.Lock()
		defer tokenLocks.Unlock()
		lock.refs--
		if lock.refs == 0 {
			delete(tokenLocks.byPath, path)
		}
	}
	select {
	case lock.busy <- struct{}{}:
		return func() { <-lock.busy; releaseRef() }, nil
	case <-ctx.Done():
		releaseRef()
		return nil, ctx.Err()
	}
}

func (t *Tool) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// rejectedToken is empty for a preflight check, or the token rejected by a
// 401. Re-read under the shared lock so a caller can reuse another call's
// refresh, including rotation of the refresh token.
func (t *Tool) accessToken(ctx context.Context, cfg OAuthConfig, rejectedToken string) (Record, error) {
	path := t.CredentialsPath
	if path == "" {
		path = credentials.DefaultPath
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return Record{}, errors.New("cannot resolve Gmail credentials path")
	}
	unlock, err := lockTokens(ctx, path)
	if err != nil {
		return Record{}, err
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}

	store, err := credentials.Open(path)
	if err != nil {
		return Record{}, errors.New("cannot open saved Gmail credentials")
	}
	var record Record
	found, err := store.Get(CredentialsKey, &record)
	if err != nil {
		// JSON decoding errors can quote credential values (for example expiry).
		return Record{}, errors.New("cannot read saved Gmail credentials; reconnect Gmail with /verify gmail")
	}
	if !found || strings.TrimSpace(record.RefreshToken) == "" {
		return Record{}, ErrReconnect
	}

	valid := strings.TrimSpace(record.AccessToken) != "" && record.Expiry.After(t.now().Add(time.Minute))
	if valid && (rejectedToken == "" || rejectedToken != record.AccessToken) {
		return record, nil
	}

	form := url.Values{
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
		"refresh_token": {record.RefreshToken},
		"grant_type":    {"refresh_token"},
	}
	// Measure expiry from request start, conservatively accounting for latency.
	issuedAt := t.now()
	token, err := requestToken(ctx, cfg, form)
	if err != nil {
		return Record{}, fmt.Errorf("refresh Google access token: %w", err)
	}
	if token.ExpiresIn <= 0 {
		return Record{}, errors.New("refresh response from Google has no valid access token lifetime")
	}
	refreshed := recordFromToken(token, issuedAt)
	if strings.TrimSpace(refreshed.RefreshToken) == "" {
		refreshed.RefreshToken = record.RefreshToken
	}
	if token.Scope == "" {
		refreshed.Scope = record.Scope
	}
	if token.TokenType == "" {
		refreshed.TokenType = record.TokenType
	}

	// Reopen after the network request to retain other integrations' updates
	// that may have been saved while the refresh was in flight.
	store, err = credentials.Open(path)
	if err != nil {
		return Record{}, errors.New("cannot open credentials to save refreshed Gmail authorization")
	}
	if err := store.Set(CredentialsKey, refreshed); err != nil {
		return Record{}, errors.New("cannot encode refreshed Gmail authorization")
	}
	if err := store.Save(); err != nil {
		return Record{}, errors.New("cannot save refreshed Gmail authorization")
	}
	return refreshed, nil
}

func redactSecrets(value string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	return value
}

// Transport errors may contain URLs or arbitrary response/request details.
// Only context errors are safe to retain for callers that check cancellation.
func safeTransportError(ctx context.Context, message string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", message, err)
	}
	return errors.New(message)
}
