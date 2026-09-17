package providers

import (
	"context"

	tools "justsay-harness/tools"
)

// IProvider is the contract every model provider implements, so the CLI and the
// agent depend on this rather than on one vendor's client.
type IProvider interface {
	// Model reports which model is being talked to.
	Model() string

	// Generate answers a single prompt: no history, no tools.
	Generate(ctx context.Context, input string) (string, error)

	// Chat sends the conversation and the tool catalogue, and returns the reply:
	// text, tool calls, or both.
	Chat(ctx context.Context, system string, history []Message, schemas []tools.Schema) (Message, error)
}

// Roles used in the conversation history.
const (
	RoleUser  = "user"  // what the person typed
	RoleModel = "model" // what the model answered
	RoleTool  = "tool"  // what the tools returned
)

// Message is one entry of history. A model message carries text, tool calls or
// both; a tool message carries one result per call.
type Message struct {
	Role        string
	Text        string
	ToolCalls   []ToolCall
	ToolResults []ToolResult

	// rawTurn is the provider's own copy of a model turn, opaque to everyone
	// else: Gemini wants the thought signature attached to a function call
	// echoed back untouched, Anthropic its thinking blocks, so a replayed turn
	// uses this instead of the fields above. rawProvider says who wrote it, so
	// one provider never replays another's turn. See providers.message.go for
	// how the pair survives being stored.
	rawTurn     any
	rawProvider string
}

// ToolCall is the model asking for one tool to run.
type ToolCall struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

// ToolResult is what running a ToolCall produced.
type ToolResult struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Output  string `json:"output,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
}
