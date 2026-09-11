package tests

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	agent "agent-harness/agent"
	humanintheloop "agent-harness/agent/human-in-the-loop"
	state "agent-harness/agent/state"
	providers "agent-harness/providers"
	tools "agent-harness/tools"

	"github.com/alicebob/miniredis/v2"
)

// gatedTool stands in for a tool the policy may hold: it does nothing except
// record that it ran, which is the whole question a HITL test asks.
type gatedTool struct {
	name string
	ran  int
}

func (t *gatedTool) Schema() tools.Schema {
	return tools.Schema{Name: t.name, Description: "records that it ran"}
}

func (t *gatedTool) Call(context.Context, map[string]any) (string, error) {
	t.ran++
	return "done", nil
}

// hitlWorkspace puts a .env and a hitl_config.yml in a fresh directory, runs
// the test there, and loads the policy from them. The reload matters both ways:
// the policy is held across calls, so without it a test would read whatever the
// previous one left behind.
func hitlWorkspace(t *testing.T, env string, config string) {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o600); err != nil {
		t.Fatalf("cannot write .env: %v", err)
	}
	if config != "" {
		if err := os.WriteFile(filepath.Join(dir, humanintheloop.ConfigFile), []byte(config), 0o600); err != nil {
			t.Fatalf("cannot write %s: %v", humanintheloop.ConfigFile, err)
		}
	}

	t.Chdir(dir)

	if err := humanintheloop.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	t.Cleanup(func() { humanintheloop.Reload() })
}

// callThenAnswer scripts a model that asks for one tool and then answers.
func callThenAnswer(tool string) *scriptedProvider {
	return &scriptedProvider{replies: []providers.Message{
		{
			Role:      providers.RoleModel,
			ToolCalls: []providers.ToolCall{{ID: "call-1", Name: tool, Args: map[string]any{}}},
		},
		{Role: providers.RoleModel, Text: "Sent."},
	}}
}

const gatedConfig = "tools:\n  require_approval:\n    - send_updates_to_manager\n"

// With HITL_ENABLED off the config is beside the point: a tool listed there
// still runs, and the turn finishes in one go.
func TestHitlDisabledRunsTheGatedTool(t *testing.T) {
	hitlWorkspace(t, "MOCK_AGENT_CALL=\"false\"\nHITL_ENABLED=\"false\"\n", gatedConfig)

	if humanintheloop.RequiresApproval("send_updates_to_manager") {
		t.Fatal("a gated tool needs approval with HITL_ENABLED off")
	}

	tool := &gatedTool{name: "send_updates_to_manager"}
	a := agent.New(callThenAnswer(tool.name), tools.NewRegistry(tool))

	answer, err := a.Run(context.Background(), "tell my manager", "hitl-off")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if tool.ran != 1 {
		t.Errorf("tool ran %d times, want 1", tool.ran)
	}
	if want := "Sent."; answer != want {
		t.Errorf("answer = %q, want %q", answer, want)
	}
}

// HITL on and the tool listed: the call is held, so the turn ends without
// running it and without asking the model again.
func TestHitlEnabledPausesTheGatedTool(t *testing.T) {
	redis := miniredis.RunT(t)

	env := "MOCK_AGENT_CALL=\"false\"\nHITL_ENABLED=\"true\"\nREDIS_ADDR=\"" + redis.Addr() + "\"\n"
	hitlWorkspace(t, env, gatedConfig)

	if !humanintheloop.RequiresApproval("send_updates_to_manager") {
		t.Fatal("a listed tool does not need approval with HITL_ENABLED on")
	}

	tool := &gatedTool{name: "send_updates_to_manager"}
	provider := callThenAnswer(tool.name)
	a := agent.New(provider, tools.NewRegistry(tool))

	var announced []state.PendingApproval
	a.SetApprovalNotifier(func(_ context.Context, _ string, pending state.PendingApproval) {
		announced = append(announced, pending)
	})

	answer, err := a.Run(context.Background(), "tell my manager", "hitl-pause")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if tool.ran != 0 {
		t.Errorf("a held tool ran %d times, want 0", tool.ran)
	}
	if answer == "Sent." {
		t.Fatalf("the run finished instead of pausing: %q", answer)
	}
	if provider.calls != 1 {
		t.Errorf("provider was asked %d times, want 1: a paused run does not take another step", provider.calls)
	}

	// Whoever can approve has to be told, and told about the call that was held.
	if len(announced) != 1 {
		t.Fatalf("the pause was announced %d times, want 1", len(announced))
	}
	if announced[0].ToolCall.Name != tool.name {
		t.Errorf("announced %q, want %q", announced[0].ToolCall.Name, tool.name)
	}

	// The pause is only useful if the run was written down for the approval to
	// come back to.
	if len(redis.Keys()) == 0 {
		t.Error("nothing was saved to the state store, so the run cannot be resumed")
	}
}

// HITL on but the tool is not listed: it runs like any other call.
func TestHitlEnabledRunsAnUngatedTool(t *testing.T) {
	hitlWorkspace(t, "MOCK_AGENT_CALL=\"false\"\nHITL_ENABLED=\"true\"\n", gatedConfig)

	if humanintheloop.RequiresApproval("run_bash") {
		t.Fatal("a tool the config does not list needs approval")
	}

	tool := &gatedTool{name: "run_bash"}
	a := agent.New(callThenAnswer(tool.name), tools.NewRegistry(tool))

	answer, err := a.Run(context.Background(), "run something", "hitl-ungated")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if tool.ran != 1 {
		t.Errorf("tool ran %d times, want 1", tool.ran)
	}
	if want := "Sent."; answer != want {
		t.Errorf("answer = %q, want %q", answer, want)
	}
}

// No config file means nothing was declared, so nothing is gated even with
// HITL on.
func TestHitlWithoutAConfigGatesNothing(t *testing.T) {
	hitlWorkspace(t, "HITL_ENABLED=\"true\"\n", "")

	if humanintheloop.RequiresApproval("send_updates_to_manager") {
		t.Error("a tool needs approval with no config file to say so")
	}
}

// A config that cannot be parsed fails closed: every call waits for a person,
// and Reload says why, rather than a typo quietly opening the gate.
func TestHitlWithABrokenConfigGatesEverything(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("HITL_ENABLED=\"true\"\n"), 0o600); err != nil {
		t.Fatalf("cannot write .env: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, humanintheloop.ConfigFile), []byte("tools: [oops\n"), 0o600); err != nil {
		t.Fatalf("cannot write %s: %v", humanintheloop.ConfigFile, err)
	}

	t.Chdir(dir)
	t.Cleanup(func() { humanintheloop.Reload() })

	if err := humanintheloop.Reload(); err == nil {
		t.Error("a config that does not parse reloaded without an error")
	}
	if !humanintheloop.RequiresApproval("run_bash") {
		t.Error("a broken config left a tool ungated")
	}
}
