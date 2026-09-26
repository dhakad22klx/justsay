package gmail

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/quotedprintable"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	credentials "justsay-harness/credentials"
)

func TestToolRefreshesTokenPersistsItAndSendsMessage(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	credentialPath := filepath.Join(t.TempDir(), "credentials.json")
	store, err := credentials.Open(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(CredentialsKey, Record{
		AccessToken:  "expired-access-token",
		RefreshToken: "durable-refresh-token",
		TokenType:    "Bearer",
		Expiry:       now.Add(-time.Minute),
		Scope:        SendScope,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}

	var refreshes, sends int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			refreshes++
			if err := r.ParseForm(); err != nil {
				t.Fatalf("parse refresh form: %v", err)
			}
			if got := r.Form.Get("refresh_token"); got != "durable-refresh-token" {
				t.Errorf("refresh_token = %q", got)
			}
			if got := r.Form.Get("client_secret"); got != "client-secret" {
				t.Errorf("client_secret = %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			if _, err := io.WriteString(w, `{"access_token":"fresh-access-token","token_type":"Bearer","expires_in":3600}`); err != nil {
				t.Errorf("write refresh response: %v", err)
			}

		case "/gmail/v1/users/me/messages/send":
			sends++
			if got := r.Header.Get("Authorization"); got != "Bearer fresh-access-token" {
				t.Errorf("Authorization = %q", got)
			}
			var payload struct {
				Raw string `json:"raw"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode send request: %v", err)
			}
			raw, err := base64.RawURLEncoding.DecodeString(payload.Raw)
			if err != nil {
				t.Fatalf("decode raw message: %v", err)
			}
			message, err := mail.ReadMessage(strings.NewReader(string(raw)))
			if err != nil {
				t.Fatalf("parse message: %v", err)
			}
			if got := message.Header.Get("To"); got != "john@example.com" {
				t.Errorf("To = %q", got)
			}
			if got := message.Header.Get("Cc"); got != "copy@example.com" {
				t.Errorf("Cc = %q", got)
			}
			if got := message.Header.Get("Bcc"); got != "blind@example.com" {
				t.Errorf("Bcc = %q", got)
			}
			decodedBody, err := io.ReadAll(quotedprintable.NewReader(message.Body))
			if err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if got := string(decodedBody); got != "The meeting is at 3 PM." {
				t.Errorf("body = %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			if _, err := io.WriteString(w, `{"id":"message-123","threadId":"thread-456"}`); err != nil {
				t.Errorf("write send response: %v", err)
			}

		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	tool := &Tool{
		CredentialsPath: credentialPath,
		OAuth: OAuthConfig{
			ClientID:     "client-id",
			ClientSecret: "client-secret",
			TokenURL:     server.URL + "/token",
			HTTPClient:   server.Client(),
		},
		APIBaseURL: server.URL + "/gmail/v1",
		HTTPClient: server.Client(),
		Now:        func() time.Time { return now },
	}

	output, err := tool.Call(t.Context(), map[string]any{
		"to":      "john@example.com",
		"subject": "Meeting Update",
		"body":    "The meeting is at 3 PM.",
		"cc":      "copy@example.com",
		"bcc":     "blind@example.com",
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(output, "message-123") {
		t.Errorf("output = %q", output)
	}
	if refreshes != 1 || sends != 1 {
		t.Errorf("refreshes = %d, sends = %d; want 1 each", refreshes, sends)
	}

	reopened, err := credentials.Open(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	var saved Record
	found, err := reopened.Get(CredentialsKey, &saved)
	if err != nil || !found {
		t.Fatalf("read saved token: found=%v err=%v", found, err)
	}
	if saved.AccessToken != "fresh-access-token" || saved.RefreshToken != "durable-refresh-token" {
		t.Errorf("saved token was not refreshed correctly")
	}
	info, err := os.Stat(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("credential permissions = %o, want 600", got)
	}
}

func TestToolReportsGmailAPIErrorWithoutLeakingToken(t *testing.T) {
	credentialPath := filepath.Join(t.TempDir(), "credentials.json")
	store, _ := credentials.Open(credentialPath)
	_ = store.Set(CredentialsKey, Record{
		AccessToken:  "access-token-that-must-not-leak",
		RefreshToken: "refresh-token-that-must-not-leak",
		Expiry:       time.Now().Add(time.Hour),
		Scope:        SendScope,
	})
	_ = store.Save()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		if _, err := io.WriteString(w, `{"error":{"code":403,"message":"Gmail API has not been used in this project","status":"PERMISSION_DENIED"}}`); err != nil {
			t.Errorf("write API error response: %v", err)
		}
	}))
	defer server.Close()

	tool := &Tool{
		CredentialsPath: credentialPath,
		OAuth:           OAuthConfig{ClientID: "client-id", ClientSecret: "client-secret"},
		APIBaseURL:      server.URL,
		HTTPClient:      server.Client(),
	}
	_, err := tool.Call(t.Context(), map[string]any{
		"to": "john@example.com", "subject": "Hello", "body": "Body",
	})
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "Gmail API has not been used") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), "access-token") || strings.Contains(err.Error(), "refresh-token") {
		t.Fatalf("error leaked a token: %v", err)
	}
}

func TestToolSchemaAndValidationNeverAcceptCredentials(t *testing.T) {
	tool := NewTool("unused")
	encoded, err := json.Marshal(tool.Schema())
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"access_token", "refresh_token", "client_secret"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("schema contains %q", forbidden)
		}
	}
	_, err = tool.Call(t.Context(), map[string]any{
		"to": "not-an-address", "subject": "Hello", "body": "Body",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid email address") {
		t.Errorf("invalid address error = %v", err)
	}
}
