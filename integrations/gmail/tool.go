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
	"strings"
	"time"

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
	record, err := t.accessToken(ctx, cfg, "")
	if err != nil {
		return "", fmt.Errorf("gmail authentication failed: %w", err)
	}

	secrets := []string{record.AccessToken, record.RefreshToken, cfg.ClientSecret}
	output, unauthorized, err := t.send(ctx, record.AccessToken, raw, secrets...)
	if unauthorized {
		record, refreshErr := t.accessToken(ctx, cfg, record.AccessToken)
		if refreshErr != nil {
			return "", fmt.Errorf("gmail authentication failed after access was rejected: %w", refreshErr)
		}
		secrets = append(secrets, record.AccessToken, record.RefreshToken)
		output, unauthorized, err = t.send(ctx, record.AccessToken, raw, secrets...)
		if unauthorized {
			return "", ErrReconnect
		}
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
			return OAuthConfig{}, errors.New("missing Google OAuth client ID or client secret")
		}
		return cfg, nil
	}
	return ConfigFromEnv(".env")
}

func (t *Tool) send(ctx context.Context, accessToken string, raw []byte, secrets ...string) (string, bool, error) {
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
		return "", false, safeTransportError(ctx, "cannot reach Gmail API")
	}
	// Response reads report their own errors; closing the body is cleanup.
	defer func() { _ = res.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(res.Body, 64*1024))
	if err != nil {
		return "", false, errors.New("read Gmail API response")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", res.StatusCode == http.StatusUnauthorized, gmailAPIError(res.StatusCode, body, secrets...)
	}

	var sent struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		return "", false, errors.New("unreadable success response from Gmail API")
	}
	if sent.ID == "" {
		return "email sent through Gmail", false, nil
	}
	return fmt.Sprintf("email sent through Gmail (message ID %s)", safeOAuthText(redactSecrets(sent.ID, secrets...))), false, nil
}

func gmailAPIError(status int, body []byte, secrets ...string) error {
	var response struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &response)
	detail := safeOAuthText(redactSecrets(response.Error.Message, secrets...))
	if detail == "" {
		detail = safeOAuthText(redactSecrets(response.Error.Status, secrets...))
	}
	if detail == "" {
		detail = http.StatusText(status)
	}
	return fmt.Errorf("message rejected by Gmail API (HTTP %d): %s", status, detail)
}
