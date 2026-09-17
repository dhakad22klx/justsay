// Package tests holds black-box tests: they import justsay the way the CLI
// does, so they exercise the exported surface only.
package tests

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agent "justsay-harness/agent"
	providers "justsay-harness/providers"
	tools "justsay-harness/tools"
)

// scriptedProvider stands in for a model: it hands back replies that were
// written in advance, and keeps the history it was given so a test can check
// what the agent sent.
type scriptedProvider struct {
	replies []providers.Message

	calls     int
	histories [][]providers.Message
}

func (p *scriptedProvider) Model() string { return "scripted" }

func (p *scriptedProvider) Generate(context.Context, string) (string, error) {
	return "", nil
}

func (p *scriptedProvider) Chat(_ context.Context, _ string, history []providers.Message, _ []tools.Schema) (providers.Message, error) {
	// The agent keeps appending to one slice, so copy it: without this every
	// recorded history would be the same backing array read at the end.
	snapshot := make([]providers.Message, len(history))
	copy(snapshot, history)
	p.histories = append(p.histories, snapshot)

	reply := p.replies[p.calls]
	p.calls++

	return reply, nil
}

// writeEnv puts a .env in a fresh directory and makes the test run there, since
// Run reads .env from the working directory.
func writeEnv(t *testing.T, contents string) {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(contents), 0o600); err != nil {
		t.Fatalf("cannot write .env: %v", err)
	}

	t.Chdir(dir)
}

// The use case: the model asks for a tool, the agent runs it, hands the output
// back, and answers from what the second reply says.
func TestRunCallsToolThenAnswers(t *testing.T) {
	writeEnv(t, "MOCK_AGENT_CALL=\"false\"\n")

	provider := &scriptedProvider{replies: []providers.Message{
		{
			Role: providers.RoleModel,
			ToolCalls: []providers.ToolCall{{
				ID:   "call-1",
				Name: "run_bash",
				Args: map[string]any{"command": "echo hello-from-bash"},
			}},
		},
		{Role: providers.RoleModel, Text: "The command printed hello-from-bash."},
	}}

	a := agent.New(provider, tools.NewRegistry(tools.NewRunBash()))

	var seen []providers.ToolResult
	a.OnToolCall = func(_ providers.ToolCall, result providers.ToolResult) {
		seen = append(seen, result)
	}

	answer, err := a.Run(context.Background(), "run echo for me", "test-session")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if want := "The command printed hello-from-bash."; answer != want {
		t.Errorf("answer = %q, want %q", answer, want)
	}

	if provider.calls != 2 {
		t.Errorf("provider was asked %d times, want 2 (once for the tool call, once for the answer)", provider.calls)
	}

	// The callback is how the UI learns what ran, so it must fire once per call
	// with the tool's real output.
	if len(seen) != 1 {
		t.Fatalf("OnToolCall fired %d times, want 1", len(seen))
	}
	if seen[0].IsError {
		t.Errorf("tool reported an error: %s", seen[0].Output)
	}
	if !strings.Contains(seen[0].Output, "hello-from-bash") {
		t.Errorf("tool output = %q, want it to contain hello-from-bash", seen[0].Output)
	}

	// The second request is the one that matters: without the result in it the
	// model would be answering blind.
	second := provider.histories[1]
	last := second[len(second)-1]
	if last.Role != providers.RoleTool {
		t.Fatalf("last message of the second request is a %q, want %q", last.Role, providers.RoleTool)
	}
	if len(last.ToolResults) != 1 || last.ToolResults[0].ID != "call-1" {
		t.Fatalf("second request carried %+v, want one result for call-1", last.ToolResults)
	}
	if !strings.Contains(last.ToolResults[0].Output, "hello-from-bash") {
		t.Errorf("result sent back = %q, want it to contain hello-from-bash", last.ToolResults[0].Output)
	}
}

// A tool the model names but the registry does not have comes back as tool
// output, not as a Go error: the model gets to read it and try something else.
func TestRunReportsUnknownToolToTheModel(t *testing.T) {
	writeEnv(t, "MOCK_AGENT_CALL=\"false\"\n")

	provider := &scriptedProvider{replies: []providers.Message{
		{
			Role: providers.RoleModel,
			ToolCalls: []providers.ToolCall{{
				ID:   "call-1",
				Name: "read_file",
				Args: map[string]any{"path": "/etc/hostname"},
			}},
		},
		{Role: providers.RoleModel, Text: "I do not have that tool."},
	}}

	a := agent.New(provider, tools.NewRegistry(tools.NewRunBash()))

	answer, err := a.Run(context.Background(), "read a file", "test-session")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if want := "I do not have that tool."; answer != want {
		t.Errorf("answer = %q, want %q", answer, want)
	}

	second := provider.histories[1]
	last := second[len(second)-1]
	if len(last.ToolResults) != 1 {
		t.Fatalf("second request carried %d results, want 1", len(last.ToolResults))
	}
	if !last.ToolResults[0].IsError {
		t.Errorf("unknown tool was not reported as an error: %+v", last.ToolResults[0])
	}
	if !strings.Contains(last.ToolResults[0].Output, "read_file") {
		t.Errorf("result = %q, want it to name the missing tool", last.ToolResults[0].Output)
	}
}

// With MOCK_AGENT_CALL on, Run answers by itself and never spends a request.
func TestRunSkipsTheProviderWhenMocked(t *testing.T) {
	writeEnv(t, "MOCK_AGENT_CALL=\"true\"\n")

	provider := &scriptedProvider{}
	a := agent.New(provider, tools.NewRegistry(tools.NewRunBash()))

	answer, err := a.Run(context.Background(), "anything", "test-session")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if provider.calls != 0 {
		t.Errorf("provider was asked %d times, want 0", provider.calls)
	}
	if !strings.Contains(answer, "mocked") {
		t.Errorf("answer = %q, want it to say the call was mocked", answer)
	}
}
