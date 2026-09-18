package gmail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"sync"
	"time"

	credentials "justsay-harness/credentials"
	tools "justsay-harness/tools"
)

const defaultAPIBaseURL = "https://gmail.googleapis.com/gmail/v1"

// Tool sends mail through the authenticated user's Gmail account.
type Tool struct {
	CredentialsPath string
	OAuth           OAuthConfig
	APIBaseURL      string
	HTTPClient      *http.Client
	Now             func() time.Time

	mu sync.Mutex
}

var _ tools.Tool = (*Tool)(nil)

// NewTool builds a Gmail tool backed by the shared credentials file. OAuth
// application settings are loaded lazily from .env, so constructing the agent
// does not require Gmail to have been configured.
func NewTool(credentialsPath string) *Tool {
	return &Tool{CredentialsPath: credentialsPath}
}

// Schema exposes message fields only. OAuth credentials and tokens are never
// accepted from the model.
func (t *Tool) Schema() tools.Schema {
	return tools.Schema{
		Name: "gmail_send",
		Description: "Send an email from the user's authenticated Gmail account. Use this when the user asks to send or email a message. " +
			"Do not claim it was sent until this tool succeeds. Gmail must first be connected with /verify gmail.",
		Parameters: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"to": map[string]any{
					"type":        "string",
					"description": "One or more recipient email addresses, separated by commas.",
				},
				"subject": map[string]any{
					"type":        "string",
					"description": "The email subject.",
				},
				"body": map[string]any{
					"type":        "string",
					"description": "The plain-text email body.",
				},
				"cc": map[string]any{
					"type":        "string",
					"description": "Optional CC email addresses, separated by commas.",
				},
				"bcc": map[string]any{
					"type":        "string",
					"description": "Optional BCC email addresses, separated by commas.",
				},
			},
			"required": []string{"to", "subject", "body"},
		},
	}
}

// Call validates the model-provided message, obtains a current access token,
// and sends it. A 401 is refreshed and retried once.
func (t *Tool) Call(ctx context.Context, args map[string]any) (string, error) {
	message, err := messageFromArgs(args)
	if err != nil {
		return "", err
	}
	raw, err := buildMessage(message)
	if err != nil {
		return "", err
	}

	cfg, err := t.oauthConfig()
	if err != nil {
		return "", fmt.Errorf("gmail authentication is not configured: %w", err)
	}
	record, err := t.accessToken(ctx, cfg, false)
	if err != nil {
		return "", fmt.Errorf("gmail authentication failed: %w", err)
	}

	output, unauthorized, err := t.send(ctx, record.AccessToken, raw)
	if unauthorized {
		record, refreshErr := t.accessToken(ctx, cfg, true)
		if refreshErr != nil {
			return "", fmt.Errorf("gmail authentication failed after access was rejected: %w", refreshErr)
		}
		output, _, err = t.send(ctx, record.AccessToken, raw)
	}
	if err != nil {
		return "", err
	}

	return output, nil
}

type messageArgs struct {
	to      string
	subject string
	body    string
	cc      string
	bcc     string
}

func messageFromArgs(args map[string]any) (messageArgs, error) {
	read := func(name string, required bool) (string, error) {
		value, ok := args[name]
		if !ok {
			if required {
				return "", fmt.Errorf("argument %q is required", name)
			}
			return "", nil
		}
		text, ok := value.(string)
		if !ok {
			return "", fmt.Errorf("argument %q must be a string", name)
		}
		if required && name != "body" && strings.TrimSpace(text) == "" {
			return "", fmt.Errorf("argument %q is required", name)
		}
		return text, nil
	}

	var msg messageArgs
	var err error
	if msg.to, err = read("to", true); err != nil {
		return msg, err
	}
	if msg.subject, err = read("subject", true); err != nil {
		return msg, err
	}
	if msg.body, err = read("body", true); err != nil {
		return msg, err
	}
	if msg.cc, err = read("cc", false); err != nil {
		return msg, err
	}
	if msg.bcc, err = read("bcc", false); err != nil {
		return msg, err
	}
	if strings.ContainsAny(msg.subject, "\r\n") {
		return msg, errors.New(`argument "subject" must not contain line breaks`)
	}
	return msg, nil
}

func buildMessage(msg messageArgs) ([]byte, error) {
	to, err := normalizeAddresses("to", msg.to, true)
	if err != nil {
		return nil, err
	}
	cc, err := normalizeAddresses("cc", msg.cc, false)
	if err != nil {
		return nil, err
	}
	bcc, err := normalizeAddresses("bcc", msg.bcc, false)
	if err != nil {
		return nil, err
	}

	var out bytes.Buffer
	fmt.Fprintf(&out, "To: %s\r\n", to)
	if cc != "" {
		fmt.Fprintf(&out, "Cc: %s\r\n", cc)
	}
	if bcc != "" {
		fmt.Fprintf(&out, "Bcc: %s\r\n", bcc)
	}
	fmt.Fprintf(&out, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", msg.subject))
	out.WriteString("MIME-Version: 1.0\r\n")
	out.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	out.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")

	encodedBody := quotedprintable.NewWriter(&out)
	body := strings.ReplaceAll(strings.ReplaceAll(msg.body, "\r\n", "\n"), "\r", "\n")
	body = strings.ReplaceAll(body, "\n", "\r\n")
	if _, err := encodedBody.Write([]byte(body)); err != nil {
		return nil, errors.New("encode email body")
	}
	if err := encodedBody.Close(); err != nil {
		return nil, errors.New("finish email body")
	}

	return out.Bytes(), nil
}

func normalizeAddresses(field, value string, required bool) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		if required {
			return "", fmt.Errorf("argument %q is required", field)
		}
		return "", nil
	}
	addresses, err := mail.ParseAddressList(value)
	if err != nil || len(addresses) == 0 {
		return "", fmt.Errorf("argument %q contains an invalid email address", field)
	}
	formatted := make([]string, 0, len(addresses))
	for _, address := range addresses {
		if address.Name == "" {
			formatted = append(formatted, address.Address)
			continue
		}
		formatted = append(formatted, address.String())
	}
	return strings.Join(formatted, ", "), nil
}

func (t *Tool) oauthConfig() (OAuthConfig, error) {
	if strings.TrimSpace(t.OAuth.ClientID) != "" || strings.TrimSpace(t.OAuth.ClientSecret) != "" {
		cfg := t.OAuth.withDefaults()
		if cfg.ClientID == "" || cfg.ClientSecret == "" {
			return OAuthConfig{}, errors.New("Google OAuth client ID and client secret are required")
		}
		return cfg, nil
	}
	return ConfigFromEnv(".env")
}

func (t *Tool) accessToken(ctx context.Context, cfg OAuthConfig, forceRefresh bool) (Record, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	store, err := credentials.Open(t.CredentialsPath)
	if err != nil {
		return Record{}, err
	}
	var record Record
	found, err := store.Get(CredentialsKey, &record)
	if err != nil {
		return Record{}, err
	}
	if !found || strings.TrimSpace(record.RefreshToken) == "" {
		return Record{}, errors.New("Gmail is not connected; run /verify gmail")
	}

	now := time.Now()
	if t.Now != nil {
		now = t.Now()
	}
	valid := strings.TrimSpace(record.AccessToken) != "" && (record.Expiry.IsZero() || record.Expiry.After(now.Add(time.Minute)))
	if valid && !forceRefresh {
		return record, nil
	}

	form := url.Values{
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
		"refresh_token": {record.RefreshToken},
		"grant_type":    {"refresh_token"},
	}
	token, err := requestToken(ctx, cfg, form)
	if err != nil {
		return Record{}, fmt.Errorf("refresh Google access token: %w", err)
	}
	refreshed := recordFromToken(token, now)
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = record.RefreshToken
	}
	if err := store.Set(CredentialsKey, refreshed); err != nil {
		return Record{}, err
	}
	if err := store.Save(); err != nil {
		return Record{}, err
	}

	return refreshed, nil
}

func (t *Tool) send(ctx context.Context, accessToken string, raw []byte) (string, bool, error) {
	payload, err := json.Marshal(map[string]string{
		"raw": base64.RawURLEncoding.EncodeToString(raw),
	})
	if err != nil {
		return "", false, errors.New("encode Gmail request")
	}

	base := strings.TrimRight(t.APIBaseURL, "/")
	if base == "" {
		base = defaultAPIBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/users/me/messages/send", bytes.NewReader(payload))
	if err != nil {
		return "", false, errors.New("create Gmail request")
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	client := t.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("Gmail API is unavailable: %w", err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, 64*1024))
	if err != nil {
		return "", false, errors.New("read Gmail API response")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", res.StatusCode == http.StatusUnauthorized, gmailAPIError(res.StatusCode, body, accessToken)
	}

	var sent struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		return "", false, errors.New("Gmail API returned an unreadable success response")
	}
	if sent.ID == "" {
		return "email sent through Gmail", false, nil
	}
	return fmt.Sprintf("email sent through Gmail (message ID %s)", sent.ID), false, nil
}

func gmailAPIError(status int, body []byte, accessToken string) error {
	var response struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &response)
	detail := safeOAuthText(response.Error.Message)
	if detail == "" {
		detail = safeOAuthText(response.Error.Status)
	}
	if detail == "" {
		detail = http.StatusText(status)
	}
	if accessToken != "" {
		detail = strings.ReplaceAll(detail, accessToken, "[redacted]")
	}
	return fmt.Errorf("Gmail API rejected the message (HTTP %d): %s", status, detail)
}
