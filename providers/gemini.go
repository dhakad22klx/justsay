package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	tools "justsay-harness/tools"

	"github.com/joho/godotenv"
	"google.golang.org/genai"
)

// ProviderGemini names Gemini in a stored history, so a turn it recorded is
// only ever replayed to Gemini.
const ProviderGemini = "gemini"

// A Gemini turn is a *genai.Content, which is the API's own wire type and so
// reads back from the bytes it was stored as, thought signatures included.
func init() {
	registerRawCodec(ProviderGemini, func(encoded json.RawMessage) (any, error) {
		var content genai.Content
		if err := json.Unmarshal(encoded, &content); err != nil {
			return nil, err
		}

		return &content, nil
	})
}

// Gemini talks to the Gemini API through the official genai SDK.
type Gemini struct {
	client *genai.Client
	model  string
}

var _ IProvider = (*Gemini)(nil)

// NewGemini builds a provider from the GEMINI_API_KEY and GEMINI_MODEL
// entries in .env.
func NewGemini(ctx context.Context) (*Gemini, error) {
	env, err := godotenv.Read(".env")
	if err != nil {
		return nil, fmt.Errorf("read .env: %w", err)
	}

	apiKey := env["GEMINI_API_KEY"]
	if apiKey == "" {
		return nil, errors.New("GEMINI_API_KEY is not set in .env")
	}

	model := env["GEMINI_MODEL"]
	if model == "" {
		return nil, errors.New("GEMINI_MODEL is not set in .env")
	}

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("create gemini client: %w", err)
	}

	return &Gemini{client: client, model: model}, nil
}

// Model reports which model the provider is talking to.
func (g *Gemini) Model() string { return g.model }

// Generate sends input to the model and returns the generated text.
func (g *Gemini) Generate(ctx context.Context, input string) (string, error) {
	res, err := g.client.Models.GenerateContent(ctx, g.model, genai.Text(input), nil)
	if err != nil {
		return "", err
	}

	return res.Text(), nil
}

// Chat sends the whole conversation plus the tool catalogue to the model and
// returns its reply, which is either an answer, a batch of tool calls, or both.
func (g *Gemini) Chat(ctx context.Context, system string, history []Message, schemas []tools.Schema) (Message, error) {
	config := &genai.GenerateContentConfig{}
	if system != "" {
		config.SystemInstruction = genai.NewContentFromText(system, genai.RoleUser)
	}
	if decls := declarations(schemas); len(decls) > 0 {
		config.Tools = []*genai.Tool{{FunctionDeclarations: decls}}
	}

	res, err := g.client.Models.GenerateContent(ctx, g.model, contents(history), config)
	if err != nil {
		return Message{}, err
	}

	if len(res.Candidates) == 0 || res.Candidates[0].Content == nil {
		return Message{}, errors.New("model returned no content")
	}

	// Walk the parts by hand rather than using res.Text(): it logs a warning
	// whenever a response mixes text with function calls, which is the normal
	// case here.
	reply := Message{Role: RoleModel}.withRaw(ProviderGemini, res.Candidates[0].Content)
	var text strings.Builder
	for _, part := range res.Candidates[0].Content.Parts {
		switch {
		case part.Thought:
			continue
		case part.FunctionCall != nil:
			reply.ToolCalls = append(reply.ToolCalls, ToolCall{
				ID:   part.FunctionCall.ID,
				Name: part.FunctionCall.Name,
				Args: part.FunctionCall.Args,
			})
		case part.Text != "":
			text.WriteString(part.Text)
		}
	}
	reply.Text = strings.TrimSpace(text.String())

	return reply, nil
}

// declarations renders the tool catalogue in the shape the Gemini API expects.
func declarations(schemas []tools.Schema) []*genai.FunctionDeclaration {
	out := make([]*genai.FunctionDeclaration, 0, len(schemas))
	for _, schema := range schemas {
		out = append(out, &genai.FunctionDeclaration{
			Name:                 schema.Name,
			Description:          schema.Description,
			ParametersJsonSchema: schema.Parameters,
		})
	}

	return out
}

// contents converts the agent's history into Gemini contents. Tool results go
// back as function responses on a user turn, which is what the API expects.
func contents(history []Message) []*genai.Content {
	out := make([]*genai.Content, 0, len(history))

	for _, msg := range history {
		switch msg.Role {
		case RoleModel:
			// Replay the turn Gemini sent, so thought signatures and part order
			// survive the round trip.
			if raw, ok := msg.rawOf(ProviderGemini).(*genai.Content); ok && raw != nil {
				content := *raw
				if content.Role == "" {
					content.Role = string(genai.RoleModel)
				}
				out = append(out, &content)
				continue
			}

			parts := make([]*genai.Part, 0, len(msg.ToolCalls)+1)
			if msg.Text != "" {
				parts = append(parts, genai.NewPartFromText(msg.Text))
			}
			for _, call := range msg.ToolCalls {
				part := genai.NewPartFromFunctionCall(call.Name, call.Args)
				part.FunctionCall.ID = call.ID
				parts = append(parts, part)
			}
			if len(parts) == 0 {
				continue
			}
			out = append(out, genai.NewContentFromParts(parts, genai.RoleModel))

		case RoleTool:
			parts := make([]*genai.Part, 0, len(msg.ToolResults))
			for _, result := range msg.ToolResults {
				key := "output"
				if result.IsError {
					key = "error"
				}
				part := genai.NewPartFromFunctionResponse(result.Name, map[string]any{key: result.Output})
				part.FunctionResponse.ID = result.ID
				parts = append(parts, part)
			}
			if len(parts) == 0 {
				continue
			}
			out = append(out, genai.NewContentFromParts(parts, genai.RoleUser))

		default:
			out = append(out, genai.NewContentFromText(msg.Text, genai.RoleUser))
		}
	}

	return out
}
