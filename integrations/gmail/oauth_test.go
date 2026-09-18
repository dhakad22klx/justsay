package gmail

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestAuthorizeUsesSendScopePKCEAndLoopbackCallback(t *testing.T) {
	var authQuery url.Values
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse token form: %v", err)
		}
		if got := r.Form.Get("client_secret"); got != "test-client-secret" {
			t.Errorf("client_secret = %q", got)
		}
		if got := r.Form.Get("code"); got != "authorization-code" {
			t.Errorf("code = %q", got)
		}
		if got := r.Form.Get("redirect_uri"); got != authQuery.Get("redirect_uri") {
			t.Errorf("redirect_uri = %q, want %q", got, authQuery.Get("redirect_uri"))
		}
		verifier := r.Form.Get("code_verifier")
		hash := sha256.Sum256([]byte(verifier))
		if got, want := base64.RawURLEncoding.EncodeToString(hash[:]), authQuery.Get("code_challenge"); got != want {
			t.Errorf("PKCE challenge = %q, want %q", got, want)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-token",
			"refresh_token": "refresh-token",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"scope":         SendScope,
		})
	}))
	defer tokenServer.Close()

	cfg := OAuthConfig{
		ClientID:     "test-client-id",
		ClientSecret: "test-client-secret",
		AuthURL:      tokenServer.URL + "/authorize",
		TokenURL:     tokenServer.URL + "/token",
		HTTPClient:   tokenServer.Client(),
	}

	record, err := Authorize(context.Background(), cfg, func(rawURL string) {
		authURL, parseErr := url.Parse(rawURL)
		if parseErr != nil {
			t.Fatalf("parse authorization URL: %v", parseErr)
		}
		authQuery = authURL.Query()
		if got := authQuery.Get("scope"); got != SendScope {
			t.Errorf("scope = %q, want %q", got, SendScope)
		}
		if got := authQuery.Get("access_type"); got != "offline" {
			t.Errorf("access_type = %q", got)
		}
		if got := authQuery.Get("code_challenge_method"); got != "S256" {
			t.Errorf("code_challenge_method = %q", got)
		}

		callback, parseErr := url.Parse(authQuery.Get("redirect_uri"))
		if parseErr != nil {
			t.Fatalf("parse callback URL: %v", parseErr)
		}
		q := callback.Query()
		q.Set("state", authQuery.Get("state"))
		q.Set("code", "authorization-code")
		callback.RawQuery = q.Encode()
		res, requestErr := http.Get(callback.String())
		if requestErr != nil {
			t.Fatalf("call OAuth callback: %v", requestErr)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("callback status = %d", res.StatusCode)
		}
	})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if record.AccessToken != "access-token" || record.RefreshToken != "refresh-token" {
		t.Errorf("record tokens were not populated")
	}
	if record.Scope != SendScope {
		t.Errorf("record scope = %q, want %q", record.Scope, SendScope)
	}
	if record.Expiry.Before(time.Now().Add(50 * time.Minute)) {
		t.Errorf("record expiry = %v, want about one hour", record.Expiry)
	}
}
