// Package gmail provides Google OAuth authentication and the Gmail send tool.
package gmail

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

const (
	// CredentialsKey is this integration's entry in credentials.json.
	CredentialsKey = "gmail"

	// SendScope is the narrowest Gmail OAuth scope that permits sending mail.
	SendScope = "https://www.googleapis.com/auth/gmail.send"

	defaultAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	defaultTokenURL = "https://oauth2.googleapis.com/token"
	callbackPath    = "/oauth2/callback"
)

// OAuthConfig contains the application credential and OAuth endpoints. The
// client secret stays here, outside Record, so it is never copied into agent
// history or tool arguments.
type OAuthConfig struct {
	ClientID     string
	ClientSecret string
	AuthURL      string
	TokenURL     string
	HTTPClient   *http.Client
}

// Record is the user grant saved in the owner-only credential store.
type Record struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type,omitempty"`
	Expiry       time.Time `json:"expiry"`
	Scope        string    `json:"scope"`
}

// ConfigFromEnv reads the OAuth application credential from path. These
// values identify the application; the user's tokens are never put in .env.
func ConfigFromEnv(path string) (OAuthConfig, error) {
	env, err := godotenv.Read(path)
	if err != nil {
		return OAuthConfig{}, fmt.Errorf("read %s: %w", path, err)
	}

	cfg := OAuthConfig{
		ClientID:     strings.TrimSpace(env["GOOGLE_OAUTH_CLIENT_ID"]),
		ClientSecret: strings.TrimSpace(env["GOOGLE_OAUTH_CLIENT_SECRET"]),
	}
	if cfg.ClientID == "" {
		return OAuthConfig{}, errors.New("GOOGLE_OAUTH_CLIENT_ID is not set in .env")
	}
	if cfg.ClientSecret == "" {
		return OAuthConfig{}, errors.New("GOOGLE_OAUTH_CLIENT_SECRET is not set in .env")
	}

	return cfg.withDefaults(), nil
}

// Authorize performs an OAuth authorization-code flow with PKCE. A temporary
// loopback listener receives Google's redirect; showURL is responsible only
// for showing the authorization URL to the user.
func Authorize(ctx context.Context, cfg OAuthConfig, showURL func(string)) (Record, error) {
	cfg = cfg.withDefaults()
	if strings.TrimSpace(cfg.ClientID) == "" || strings.TrimSpace(cfg.ClientSecret) == "" {
		return Record{}, errors.New("Google OAuth client ID and client secret are required")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Record{}, fmt.Errorf("start OAuth callback listener: %w", err)
	}

	state, err := randomURLToken(32)
	if err != nil {
		listener.Close()
		return Record{}, fmt.Errorf("create OAuth state: %w", err)
	}
	verifier, err := randomURLToken(48)
	if err != nil {
		listener.Close()
		return Record{}, fmt.Errorf("create PKCE verifier: %w", err)
	}
	challengeBytes := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeBytes[:])
	redirectURL := "http://" + listener.Addr().String() + callbackPath

	type callbackResult struct {
		code string
		err  error
	}
	result := make(chan callbackResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("state") != state {
			http.Error(w, "Invalid OAuth state. Return to the terminal and try again.", http.StatusForbidden)
			return
		}

		var got callbackResult
		switch {
		case query.Get("error") != "":
			got.err = fmt.Errorf("Google authorization was not granted: %s", safeOAuthText(query.Get("error")))
		case strings.TrimSpace(query.Get("code")) == "":
			got.err = errors.New("Google authorization returned no code")
		default:
			got.code = query.Get("code")
		}

		select {
		case result <- got:
		default:
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if got.err != nil {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintln(w, "Gmail authorization failed. You may close this window.")
			return
		}
		fmt.Fprintln(w, "Gmail authorization complete. You may close this window.")
	})

	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = server.Serve(listener)
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		<-serveDone
	}()

	authURL, err := authorizationURL(cfg, redirectURL, state, challenge)
	if err != nil {
		return Record{}, err
	}
	if showURL != nil {
		showURL(authURL)
	}

	var callback callbackResult
	select {
	case callback = <-result:
	case <-ctx.Done():
		return Record{}, fmt.Errorf("wait for Google authorization: %w", ctx.Err())
	}
	if callback.err != nil {
		return Record{}, callback.err
	}

	record, err := exchangeCode(ctx, cfg, redirectURL, callback.code, verifier)
	if err != nil {
		return Record{}, err
	}
	if record.RefreshToken == "" {
		return Record{}, errors.New("Google returned no refresh token; revoke the app grant and authorize Gmail again")
	}

	return record, nil
}

func (c OAuthConfig) withDefaults() OAuthConfig {
	if c.AuthURL == "" {
		c.AuthURL = defaultAuthURL
	}
	if c.TokenURL == "" {
		c.TokenURL = defaultTokenURL
	}
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	return c
}

func authorizationURL(cfg OAuthConfig, redirectURL, state, challenge string) (string, error) {
	u, err := url.Parse(cfg.AuthURL)
	if err != nil {
		return "", fmt.Errorf("parse Google authorization URL: %w", err)
	}
	q := u.Query()
	q.Set("client_id", cfg.ClientID)
	q.Set("redirect_uri", redirectURL)
	q.Set("response_type", "code")
	q.Set("scope", SendScope)
	q.Set("access_type", "offline")
	q.Set("prompt", "consent")
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
}

func exchangeCode(ctx context.Context, cfg OAuthConfig, redirectURL, code, verifier string) (Record, error) {
	form := url.Values{
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
		"code":          {code},
		"code_verifier": {verifier},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {redirectURL},
	}
	token, err := requestToken(ctx, cfg, form)
	if err != nil {
		return Record{}, fmt.Errorf("exchange Google authorization code: %w", err)
	}
	return recordFromToken(token, time.Now()), nil
}

func requestToken(ctx context.Context, cfg OAuthConfig, form url.Values) (tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, errors.New("create token request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	res, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("Google token endpoint is unavailable: %w", err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, 64*1024))
	if err != nil {
		return tokenResponse{}, errors.New("read Google token response")
	}
	var token tokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return tokenResponse{}, fmt.Errorf("Google token endpoint returned HTTP %d with an unreadable response", res.StatusCode)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 || token.Error != "" {
		detail := safeOAuthText(token.Error)
		if description := safeOAuthText(token.Description); description != "" {
			detail += ": " + description
		}
		for _, values := range form {
			for _, secret := range values {
				if len(secret) >= 8 {
					detail = strings.ReplaceAll(detail, secret, "[redacted]")
				}
			}
		}
		if detail == "" {
			detail = http.StatusText(res.StatusCode)
		}
		return tokenResponse{}, fmt.Errorf("Google token endpoint rejected the request (HTTP %d): %s", res.StatusCode, detail)
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return tokenResponse{}, errors.New("Google token endpoint returned no access token")
	}

	return token, nil
}

func recordFromToken(token tokenResponse, now time.Time) Record {
	expiry := time.Time{}
	if token.ExpiresIn > 0 {
		expiry = now.Add(time.Duration(token.ExpiresIn) * time.Second)
	}
	scope := token.Scope
	if scope == "" {
		scope = SendScope
	}
	return Record{
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		TokenType:    token.TokenType,
		Expiry:       expiry,
		Scope:        scope,
	}
}

func randomURLToken(bytes int) (string, error) {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// safeOAuthText removes control characters before a provider error is shown.
// It deliberately never includes request data, where codes and secrets live.
func safeOAuthText(value string) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
	if len(value) > 500 {
		value = value[:500]
	}
	return strings.TrimSpace(value)
}
