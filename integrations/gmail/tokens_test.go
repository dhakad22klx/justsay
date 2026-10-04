package gmail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"justsay-harness/credentials"
)

var lifecycleNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func saveGrant(t *testing.T, path string, record Record) {
	t.Helper()
	store, err := credentials.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(CredentialsKey, record); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
}

func loadGrant(t *testing.T, path string) Record {
	t.Helper()
	store, err := credentials.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var record Record
	if found, err := store.Get(CredentialsKey, &record); err != nil || !found {
		t.Fatalf("load grant: found=%v err=%v", found, err)
	}
	return record
}

func lifecycleTool(path string, server *httptest.Server) *Tool {
	return &Tool{
		CredentialsPath: path,
		OAuth: OAuthConfig{
			ClientID: "client-id", ClientSecret: "client-secret",
			TokenURL: server.URL + "/token", HTTPClient: server.Client(),
		},
		APIBaseURL: server.URL, HTTPClient: server.Client(),
		Now: func() time.Time { return lifecycleNow },
	}
}

func sendArgs() map[string]any {
	return map[string]any{"to": "recipient@example.com", "subject": "Hello", "body": "Body"}
}

func TestToolTokenValidity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		expiry  time.Time
		access  string
		refresh bool
	}{
		{"valid", lifecycleNow.Add(time.Hour), "original-access", false},
		{"expired", lifecycleNow.Add(-time.Minute), "original-access", true},
		{"near expiry", lifecycleNow.Add(30 * time.Second), "original-access", true},
		{"expiry margin boundary", lifecycleNow.Add(time.Minute), "original-access", true},
		{"unknown expiry", time.Time{}, "original-access", true},
		{"missing access token", lifecycleNow.Add(time.Hour), "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credentials.json")
			saveGrant(t, path, Record{AccessToken: tc.access, RefreshToken: "original-refresh", Expiry: tc.expiry, Scope: SendScope, TokenType: "Bearer"})
			var refreshes, sends atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					refreshes.Add(1)
					if err := r.ParseForm(); err != nil {
						t.Error(err)
						return
					}
					if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "original-refresh" || r.Form.Get("client_id") != "client-id" {
						t.Error("incorrect refresh form")
					}
					_, _ = io.WriteString(w, `{"access_token":"updated-access","expires_in":3600}`)
					return
				}
				sends.Add(1)
				want := "Bearer original-access"
				if tc.refresh {
					want = "Bearer updated-access"
				}
				if r.Header.Get("Authorization") != want {
					t.Error("send used incorrect access token")
				}
				_, _ = io.WriteString(w, `{"id":"sent"}`)
			}))
			defer server.Close()
			output, err := lifecycleTool(path, server).Call(t.Context(), sendArgs())
			if err != nil || !strings.Contains(output, "sent") {
				t.Fatalf("send failed: %v", err)
			}
			wantRefreshes := int32(0)
			if tc.refresh {
				wantRefreshes = 1
			}
			if refreshes.Load() != wantRefreshes || sends.Load() != 1 {
				t.Fatalf("refreshes=%d sends=%d", refreshes.Load(), sends.Load())
			}
			saved := loadGrant(t, path)
			if tc.refresh && (saved.AccessToken != "updated-access" || !saved.Expiry.Equal(lifecycleNow.Add(time.Hour))) {
				t.Error("refreshed access token and expiry were not persisted")
			}
			if saved.RefreshToken != "original-refresh" || saved.Scope != SendScope || saved.TokenType != "Bearer" {
				t.Error("refresh lost existing grant metadata")
			}
		})
	}
}

func TestToolRefreshTokenRotation(t *testing.T) {
	for _, rotate := range []bool{false, true} {
		t.Run(fmt.Sprintf("rotate=%v", rotate), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credentials.json")
			saveGrant(t, path, Record{AccessToken: "old-access", RefreshToken: "old-refresh", Expiry: lifecycleNow.Add(-time.Hour)})
			wantRefresh := "old-refresh"
			if rotate {
				wantRefresh = "rotated-refresh"
			}
			var refreshes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/token" {
					_, _ = io.WriteString(w, `{"id":"sent"}`)
					return
				}
				n := refreshes.Add(1)
				if err := r.ParseForm(); err != nil {
					t.Error(err)
					return
				}
				expected := "old-refresh"
				if n > 1 {
					expected = wantRefresh
				}
				if r.Form.Get("refresh_token") != expected {
					t.Error("refresh did not use the latest persisted refresh token")
				}
				response := map[string]any{"access_token": "updated-access", "expires_in": 3600}
				if rotate && n == 1 {
					response["refresh_token"] = wantRefresh
				}
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()
			tool := lifecycleTool(path, server)
			for n := 0; n < 2; n++ {
				if _, err := tool.Call(t.Context(), sendArgs()); err != nil {
					t.Fatal(err)
				}
				if loadGrant(t, path).RefreshToken != wantRefresh {
					t.Error("refresh token was not persisted correctly")
				}
				tool.Now = func() time.Time { return lifecycleNow.Add(2 * time.Hour) }
			}
			if refreshes.Load() != 2 {
				t.Errorf("refreshes=%d, want 2", refreshes.Load())
			}
		})
	}
}

func TestToolConcurrentRefreshes(t *testing.T) {
	for _, unauthorized := range []bool{false, true} {
		t.Run(fmt.Sprintf("unauthorized=%v", unauthorized), func(t *testing.T) {
			const callers = 12
			path := filepath.Join(t.TempDir(), "credentials.json")
			expiry := lifecycleNow.Add(-time.Minute)
			if unauthorized {
				expiry = lifecycleNow.Add(time.Hour)
			}
			saveGrant(t, path, Record{AccessToken: "old-access", RefreshToken: "old-refresh", Expiry: expiry})
			var refreshes, staleSends, sends atomic.Int32
			allStaleSends := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					if refreshes.Add(1) != 1 {
						t.Error("concurrent calls triggered redundant refreshes")
					}
					_, _ = io.WriteString(w, `{"access_token":"updated-access","refresh_token":"rotated-refresh","expires_in":3600}`)
					return
				}
				if r.Header.Get("Authorization") == "Bearer old-access" {
					if !unauthorized {
						t.Error("expired token reached Gmail")
					}
					if staleSends.Add(1) == callers {
						close(allStaleSends)
					}
					select {
					case <-allStaleSends:
					case <-r.Context().Done():
						return
					}
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = io.WriteString(w, `{"error":{"message":"Expired credentials"}}`)
					return
				}
				if r.Header.Get("Authorization") != "Bearer updated-access" {
					t.Error("unexpected access token")
				}
				sends.Add(1)
				_, _ = io.WriteString(w, `{"id":"sent"}`)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			start := make(chan struct{})
			var wg sync.WaitGroup
			shared := lifecycleTool(path, server)
			for i := 0; i < callers; i++ {
				tool := shared
				if i%2 == 0 {
					tool = lifecycleTool(path, server)
					// Relative and absolute spellings must share the same lock.
					cwd, err := os.Getwd()
					if err != nil {
						t.Fatal(err)
					}
					tool.CredentialsPath, err = filepath.Rel(cwd, path)
					if err != nil {
						t.Fatal(err)
					}
				}
				wg.Go(func() {
					<-start
					if _, err := tool.Call(ctx, sendArgs()); err != nil {
						t.Errorf("concurrent send failed: %v", err)
					}
				})
			}
			close(start)
			wg.Wait()
			if refreshes.Load() != 1 || sends.Load() != callers {
				t.Errorf("refreshes=%d successful sends=%d", refreshes.Load(), sends.Load())
			}
			if unauthorized && staleSends.Load() != callers {
				t.Errorf("stale sends=%d, want %d", staleSends.Load(), callers)
			}
			if loadGrant(t, path).RefreshToken != "rotated-refresh" {
				t.Error("concurrent calls lost refresh token rotation")
			}
		})
	}
}

func TestToolRequiresReconnect(t *testing.T) {
	for _, mode := range []string{"missing grant", "missing refresh token", "revoked", "retry unauthorized"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credentials.json")
			record := Record{AccessToken: "old-access", RefreshToken: "old-refresh", Expiry: lifecycleNow.Add(-time.Hour)}
			if mode == "missing refresh token" {
				record.RefreshToken = ""
			}
			if mode == "retry unauthorized" {
				record.Expiry = lifecycleNow.Add(time.Hour)
			}
			if mode != "missing grant" {
				saveGrant(t, path, record)
			}
			var refreshes, sends atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					refreshes.Add(1)
					if mode == "revoked" {
						w.WriteHeader(http.StatusBadRequest)
						_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"Revoked old-refresh"}`)
						return
					}
					_, _ = io.WriteString(w, `{"access_token":"updated-access","expires_in":3600}`)
					return
				}
				sends.Add(1)
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":{"message":"Invalid credentials"}}`)
			}))
			defer server.Close()
			output, err := lifecycleTool(path, server).Call(t.Context(), sendArgs())
			if output != "" || !errors.Is(err, ErrReconnect) || !strings.Contains(err.Error(), "/verify gmail") {
				t.Fatalf("expected reconnect error, got %v", err)
			}
			wantRefreshes, wantSends := int32(0), int32(0)
			if mode == "revoked" || mode == "retry unauthorized" {
				wantRefreshes = 1
			}
			if mode == "retry unauthorized" {
				wantSends = 2
			}
			if refreshes.Load() != wantRefreshes || sends.Load() != wantSends {
				t.Errorf("refreshes=%d sends=%d", refreshes.Load(), sends.Load())
			}
			if mode == "revoked" && loadGrant(t, path) != record {
				t.Error("failed refresh changed stored credentials")
			}
		})
	}
}

func TestTokenLockWaitHonorsCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	unlock, err := lockTokens(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := (&Tool{CredentialsPath: path}).accessToken(ctx, OAuthConfig{}, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func TestToolRefreshFailuresPreserveGrant(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"temporary failure", http.StatusServiceUnavailable, `{"error":"temporarily_unavailable","error_description":"old-refresh"}`},
		{"invalid client", http.StatusUnauthorized, `{"error":"invalid_client","error_description":"client-secret"}`},
		{"malformed response", http.StatusOK, `old-access old-refresh`},
		{"missing access token", http.StatusOK, `{"expires_in":3600}`},
		{"missing expiry", http.StatusOK, `{"access_token":"updated-access"}`},
		{"invalid expiry", http.StatusOK, `{"access_token":"updated-access","expires_in":-1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credentials.json")
			record := Record{AccessToken: "old-access", RefreshToken: "old-refresh", Expiry: lifecycleNow.Add(-time.Hour)}
			saveGrant(t, path, record)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/token" {
					t.Error("Gmail was called after failed refresh")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			output, err := lifecycleTool(path, server).Call(t.Context(), sendArgs())
			if output != "" || err == nil || errors.Is(err, ErrReconnect) {
				t.Fatalf("expected refresh failure, got %v", err)
			}
			if loadGrant(t, path) != record {
				t.Error("failed refresh changed the saved grant")
			}
			for _, secret := range []string{"old-access", "old-refresh", "client-secret", "updated-access"} {
				if strings.Contains(err.Error(), secret) {
					t.Error("refresh error exposed a secret")
				}
			}
		})
	}
}

func TestToolSavesRefreshBeforeSendingAndRetainsOtherIntegrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	saveGrant(t, path, Record{AccessToken: "old-access", RefreshToken: "old-refresh", Expiry: lifecycleNow.Add(-time.Hour)})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			store, err := credentials.Open(path)
			if err != nil {
				t.Error(err)
				return
			}
			if err := store.Set("other-integration", map[string]string{"account": "saved-during-refresh"}); err != nil {
				t.Error(err)
				return
			}
			if err := store.Save(); err != nil {
				t.Error(err)
				return
			}
			_, _ = io.WriteString(w, `{"access_token":"updated-access","refresh_token":"rotated-refresh","expires_in":3600}`)
			return
		}
		saved := loadGrant(t, path)
		if saved.AccessToken != "updated-access" || saved.RefreshToken != "rotated-refresh" || !saved.Expiry.Equal(lifecycleNow.Add(time.Hour)) {
			t.Error("Gmail was called before refreshed credentials were persisted")
		}
		_, _ = io.WriteString(w, `{"id":"sent"}`)
	}))
	defer server.Close()
	if _, err := lifecycleTool(path, server).Call(t.Context(), sendArgs()); err != nil {
		t.Fatal(err)
	}
	store, err := credentials.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var other map[string]string
	if found, err := store.Get("other-integration", &other); err != nil || !found || other["account"] != "saved-during-refresh" {
		t.Error("refresh overwrote an unrelated integration's credentials")
	}
}

func TestToolDoesNotSendWhenRefreshCannotBeSaved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	saveGrant(t, path, Record{AccessToken: "old-access", RefreshToken: "old-refresh", Expiry: lifecycleNow.Add(-time.Hour)})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			t.Error("Gmail was called before refresh could be saved")
		}
		// Replace the file with a directory to cause a deterministic storage
		// failure even when tests run as a privileged user.
		if err := os.Remove(path); err != nil {
			t.Error(err)
			return
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Error(err)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"updated-access","refresh_token":"rotated-refresh","expires_in":3600}`)
	}))
	defer server.Close()
	output, err := lifecycleTool(path, server).Call(t.Context(), sendArgs())
	if output != "" || err == nil || !strings.Contains(err.Error(), "save refreshed Gmail authorization") {
		t.Fatalf("expected persistence failure, got %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestToolNeverExposesTokensInResultsOrErrors(t *testing.T) {
	access := "secret-access-" + strings.Repeat("X", 50)
	refresh := "short7"
	for _, mode := range []string{"token error", "unknown OAuth error", "Gmail error", "Gmail success", "token transport", "Gmail transport", "malformed credentials"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credentials.json")
			expiry := lifecycleNow.Add(time.Hour)
			if strings.HasPrefix(mode, "token") || mode == "unknown OAuth error" {
				expiry = lifecycleNow.Add(-time.Hour)
			}
			saveGrant(t, path, Record{AccessToken: access, RefreshToken: refresh, Expiry: expiry})
			if mode == "malformed credentials" {
				raw, err := json.Marshal(map[string]any{CredentialsKey: map[string]string{"access_token": access, "refresh_token": refresh, "expiry": access}})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(mode, "transport") {
					return nil, fmt.Errorf("transport leaked %s %s client-secret", access, refresh)
				}
				status := http.StatusOK
				var body any
				switch mode {
				case "token error", "unknown OAuth error":
					status = http.StatusBadRequest
					code := "invalid_client"
					if mode == "unknown OAuth error" {
						code = access + refresh
					}
					body = map[string]string{"error": code, "error_description": access + refresh + "response-access", "access_token": "response-access"}
				case "Gmail error":
					status = http.StatusForbidden
					// Put a token across the text truncation boundary to ensure
					// redaction occurs before truncation, including short secrets.
					body = map[string]any{"error": map[string]string{"message": strings.Repeat(".", 480) + access + refresh}}
				case "Gmail success":
					body = map[string]string{"id": access + refresh}
				default:
					t.Error("unexpected request")
				}
				encoded, err := json.Marshal(body)
				if err != nil {
					return nil, err
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(encoded)))}, nil
			})}
			tool := &Tool{
				CredentialsPath: path,
				OAuth:           OAuthConfig{ClientID: "client-id", ClientSecret: "client-secret", HTTPClient: client, TokenURL: "https://example.test/token"},
				HTTPClient:      client, APIBaseURL: "https://example.test/gmail",
				Now: func() time.Time { return lifecycleNow },
			}
			output, err := tool.Call(t.Context(), sendArgs())
			if mode == "Gmail success" && err != nil || mode != "Gmail success" && err == nil {
				t.Errorf("unexpected outcome: %v", err)
			}
			text := output
			if err != nil {
				text += err.Error()
			}
			for _, secret := range []string{access, "secret-access-", refresh, "client-secret", "response-access"} {
				if strings.Contains(text, secret) {
					t.Error("tool result exposed a secret")
				}
			}
		})
	}
}
