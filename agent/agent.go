package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	humanintheloop "justsay-harness/agent/human-in-the-loop"
	state "justsay-harness/agent/state"
	credentials "justsay-harness/credentials"
	gmail "justsay-harness/integrations/gmail"
	providers "justsay-harness/providers"
	tools "justsay-harness/tools"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
)

// systemPrompt frames the job for the model. It says nothing about individual
// tools: the catalogue goes with every request and each tool's schema describes
// itself, so a second copy here would only be the one that goes stale. What
// stays is what no single schema can say - how to treat what tools report, and
// when not to call one at all.
const systemPrompt = `You are justsay, an assistant running in the user's terminal.

Answer from what your tools actually report, never from what you assume. Each
tool describes when it applies and what it cannot do; read those descriptions
and pick by what the question needs.

Gather with one tool, then write in your own words. Output another tool handed
you is material for your answer, not the answer itself, and not something to
paste into a tool that sends text somewhere.

A call may be held for the user to approve before it runs. If a result says the
call was rejected or changed, that is the user's decision: say so plainly and do
not reach for another tool to get the same thing done.

Most questions need no tool at all. Explaining, summarising, or answering from
what a tool already told you is your own work, so do not call a tool to confirm
something you have been told, and stop calling tools once you can answer.`

// defaultMaxSteps bounds a single Run so a confused model cannot call tools
// forever.
const defaultMaxSteps = 10

// pausedForApproval is what Run answers once a call has been handed to a human.
const pausedForApproval = "Processed for Human approval"

// Where a held call stands. Both spellings live here so the one written at the
// pause and the one read at the resume cannot drift apart.
const (
	approvalPending  = "PENDING"
	approvalApproved = "APPROVED"
	approvalRejected = "REJECTED"
)

// ApprovalNotifier announces a call waiting on a human. A func rather than an
// import, so this package stays free of whatever does the asking.
//
// It must announce and return: calling back into the Agent, or waiting for the
// decision it is announcing, deadlocks against the turn lock held over it.
type ApprovalNotifier func(ctx context.Context, sessionID string, pending state.PendingApproval)

// Agent runs the ask-model / run-tools / ask-again loop and keeps the
// conversation history across turns.
// One agent serves the whole process (see GetAgent), and the Telegram poll calls
// Run from its own goroutine. mu guards the history for a whole turn, so a
// request from a phone waits for the local one instead of interleaving with it.
type Agent struct {
	provider providers.IProvider
	tools    *tools.Registry
	maxSteps int

	mu      sync.Mutex
	history []providers.Message

	// OnToolCall, when set, runs after each tool call so the UI can show what
	// the agent is doing.
	OnToolCall func(call providers.ToolCall, result providers.ToolResult)

	// onApprovalNeeded, when set, tells whoever can approve that a call is
	// held. Unexported and guarded by mu because, unlike OnToolCall, it is
	// re-pointed while the poll goroutine may be reading it.
	onApprovalNeeded ApprovalNotifier
}

// SetApprovalNotifier points the pause at whoever should be told, or nil when
// nothing is listening.
func (a *Agent) SetApprovalNotifier(notify ApprovalNotifier) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.onApprovalNeeded = notify
}

// New builds an agent over a provider and the tools it is allowed to use.
func New(provider providers.IProvider, registry *tools.Registry) *Agent {
	return &Agent{
		provider: provider,
		tools:    registry,
		maxSteps: defaultMaxSteps,
	}
}

// The one agent this process runs. Two things reach for it — the prompt and the
// Telegram link — and one instance means one conversation, wherever the request
// came from. sync.Once because only the building has to happen once; every later
// call reads a pointer the Once already published safely.
var (
	agentInstance *Agent
	agentOnce     sync.Once
)

// GetAgent returns that agent, building it on the first call.
//
// The first caller's provider is the one it keeps; a later call cannot replace
// it. Tools are chosen here rather than by the caller, so adding one is a change
// to this function alone.
//
// A nil provider means no model is configured, and the answer is a nil agent
// rather than one that would panic when asked. The Once is left unspent, so a
// later call with a working provider still builds; callers read nil as
// "unavailable".
func GetAgent(provider providers.IProvider) *Agent {
	if provider == nil {
		return nil
	}

	agentOnce.Do(func() {
		agentInstance = New(provider, tools.NewRegistry(
			tools.NewRunBash(),
			tools.NewSendUpdatesToManager(),
			gmail.NewTool(credentials.DefaultPath),
		))
	})

	return agentInstance
}

// Run answers one user prompt, running as many tool rounds as the model asks
// for along the way.
func (a *Agent) Run(ctx context.Context, prompt string, sessionID string) (string, error) {
	env, err := godotenv.Read(".env")
	if err != nil {
		return "", fmt.Errorf("read .env: %w", err)
	}
	mockAgentCall := env["MOCK_AGENT_CALL"]
	if mockAgentCall == "true" {
		return "Agent call is mocked, to turn it on type `/on` and to turn it back off type `/off`.", nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	a.history = append(a.history, providers.Message{Role: providers.RoleUser, Text: prompt})

	for step := 0; step < a.maxSteps; step++ {
		reply, err := a.provider.Chat(ctx, systemPrompt, a.history, a.tools.Schemas())
		if err != nil {
			return "", err
		}

		// Everything the model says stays in the history, so the next request
		// carries the full trail of calls and results.
		a.history = append(a.history, reply)

		// No tool calls means the model is done thinking: this is the answer.
		if len(reply.ToolCalls) == 0 {
			return reply.Text, nil
		}

		results := make([]providers.ToolResult, 0, len(reply.ToolCalls))
		for _, call := range reply.ToolCalls {
			// The first call a human has to sign off on ends the turn. What ran
			// before it is kept, so an approval arriving later resumes instead
			// of running those tools a second time.
			if needHumanApproval(call.Name) {
				if err := a.pause(ctx, sessionID, call, results, step); err != nil {
					return "", err
				}

				return pausedForApproval, nil
			}

			result := a.runTool(ctx, call)
			if a.OnToolCall != nil {
				a.OnToolCall(call, result)
			}
			results = append(results, result)
		}

		// Hand every result back and let the model take the next step.
		a.history = append(a.history, providers.Message{Role: providers.RoleTool, ToolResults: results})
	}

	return "", fmt.Errorf("stopped after %d tool rounds without a final answer", a.maxSteps)
}

// Resume picks a paused run back up: it reads the state saved under sessionID,
// runs the call the human approved, and carries on from the step the pause
// stopped at.
//
// The conversation comes from the store rather than from this process, so a run
// paused elsewhere resumes here with the history it actually had.
func (a *Agent) Resume(ctx context.Context, sessionID string) (string, error) {
	return a.ResumeApproval(ctx, sessionID, "")
}

// ResumeApproval is Resume for a decision that names the pause it answers. An
// empty approvalID skips that check.
func (a *Agent) ResumeApproval(ctx context.Context, sessionID string, approvalID string) (string, error) {
	store, err := state.OpenRedis(ctx)
	if err != nil {
		return "", err
	}
	// Redis operations are acknowledged before pool cleanup. A close failure
	// must not turn a completed operation into a retryable failure.
	defer func() { _ = store.Close() }()

	saved, err := held(ctx, store, sessionID, approvalID)
	if err != nil {
		return "", err
	}

	// The decision is recorded before anything acts on it, so a resume that
	// fails halfway does not leave the call looking like it is still waiting.
	saved.PendingApproval.ApprovalStatus = approvalApproved
	saved.Status = state.StatusRunning
	if err := store.Put(ctx, sessionID, saved); err != nil {
		return "", err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	a.history = saved.History

	// The approved call runs without another policy check: it is the one the
	// human just decided on, so gating it again would pause on it forever.
	approved := saved.PendingApproval.ToolCall
	result := a.runTool(ctx, approved)
	if a.OnToolCall != nil {
		a.OnToolCall(approved, result)
	}
	a.history = append(a.history, providers.Message{Role: providers.RoleTool, ToolResults: []providers.ToolResult{result}})

	// From here it is the same cycle Run walks, continuing the step budget the
	// paused run had already spent.
	for step := saved.Step + 1; step < a.maxSteps; step++ {
		reply, err := a.provider.Chat(ctx, systemPrompt, a.history, a.tools.Schemas())
		if err != nil {
			return "", err
		}

		a.history = append(a.history, reply)

		if len(reply.ToolCalls) == 0 {
			// The run is over, so the paused copy of it is not worth keeping.
			// if err := store.Delete(ctx, sessionID); err != nil {
			// 	return "", err
			// }

			return reply.Text, nil
		}

		results := make([]providers.ToolResult, 0, len(reply.ToolCalls))
		for _, call := range reply.ToolCalls {
			if needHumanApproval(call.Name) {
				if err := a.pause(ctx, sessionID, call, results, step); err != nil {
					return "", err
				}

				return pausedForApproval, nil
			}

			result := a.runTool(ctx, call)
			if a.OnToolCall != nil {
				a.OnToolCall(call, result)
			}
			results = append(results, result)
		}

		a.history = append(a.history, providers.Message{Role: providers.RoleTool, ToolResults: results})
	}

	return "", fmt.Errorf("stopped after %d tool rounds without a final answer", a.maxSteps)
}

func needHumanApproval(toolName string) bool {
	return humanintheloop.RequiresApproval(toolName)
}

// Decline drops a held call: the decision is recorded and the run stops there.
// Nothing runs and the history is untouched.
func (a *Agent) Decline(ctx context.Context, sessionID string, approvalID string) (string, error) {
	store, err := state.OpenRedis(ctx)
	if err != nil {
		return "", err
	}
	// Redis operations are acknowledged before pool cleanup. A close failure
	// must not turn a completed operation into a retryable failure.
	defer func() { _ = store.Close() }()

	saved, err := held(ctx, store, sessionID, approvalID)
	if err != nil {
		return "", err
	}

	call := saved.PendingApproval.ToolCall
	saved.PendingApproval.ApprovalStatus = approvalRejected

	// Off StatusWaitingApproval is what makes the run un-resumable. Nothing
	// failed, so completed rather than failed.
	saved.Status = state.StatusCompleted
	if err := store.Put(ctx, sessionID, saved); err != nil {
		return "", err
	}

	return fmt.Sprintf("Declined. %s was not run.", call.Name), nil
}

// held loads a run that is genuinely waiting on the decision being made.
//
// The three checks are one race each: a run that has moved on, a second tap on
// a call already decided, and a button from an earlier pause of the same
// session. Sound within one process, where the poll dispatches taps one at a
// time; two processes on one Redis could both pass, since this is not atomic.
func held(ctx context.Context, store state.Store, sessionID string, approvalID string) (state.AgentState, error) {
	var saved state.AgentState

	found, err := store.Get(ctx, sessionID, &saved)
	if err != nil {
		return saved, err
	}
	if !found {
		return saved, fmt.Errorf("no paused run saved for session %s", sessionID)
	}
	if !saved.Waiting() {
		return saved, fmt.Errorf("session %s is %s, with nothing waiting on approval", sessionID, saved.Status)
	}
	if saved.PendingApproval.ApprovalStatus != approvalPending {
		return saved, fmt.Errorf("that call was already %s", strings.ToLower(saved.PendingApproval.ApprovalStatus))
	}
	if approvalID != "" && saved.PendingApproval.ID != approvalID {
		return saved, errors.New("that request is no longer the one waiting")
	}

	return saved, nil
}

// pause saves the run to the state store and leaves it there. The results
// already gathered go into the history first, so the state holds everything up
// to the call that is waiting.
func (a *Agent) pause(ctx context.Context, sessionID string, call providers.ToolCall, done []providers.ToolResult, step int) error {
	if len(done) > 0 {
		a.history = append(a.history, providers.Message{Role: providers.RoleTool, ToolResults: done})
	}

	store, err := state.OpenRedis(ctx)
	if err != nil {
		return err
	}
	// Redis operations are acknowledged before pool cleanup. A close failure
	// must not turn a completed operation into a retryable failure.
	defer func() { _ = store.Close() }()

	pending := state.PendingApproval{
		ID:             uuid.NewString(),
		ToolCall:       call,
		ApprovalStatus: approvalPending,
	}

	if err := store.Put(ctx, sessionID, state.AgentState{
		SessionID:       sessionID,
		History:         a.history,
		PendingApproval: &pending,
		Status:          state.StatusWaitingApproval,
		Step:            step,
	}); err != nil {
		return err
	}

	// Announced only once the state is saved: the decision may come back from
	// another process, and it has to find the run already there.
	if a.onApprovalNeeded != nil {
		a.onApprovalNeeded(ctx, sessionID, pending)
	}

	return nil
}

// Reset forgets the conversation so far. One agent answers everywhere, so
// `reset` at the prompt also drops what was said over Telegram.
func (a *Agent) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.history = nil
}

// runTool executes one call. Failures come back as tool output rather than as a
// Go error, so the model can read what went wrong and try something else.
func (a *Agent) runTool(ctx context.Context, call providers.ToolCall) providers.ToolResult {
	result := providers.ToolResult{ID: call.ID, Name: call.Name}

	tool, ok := a.tools.Get(call.Name)
	if !ok {
		result.Output = fmt.Sprintf("unknown tool %q", call.Name)
		result.IsError = true
		return result
	}

	output, err := tool.Call(ctx, call.Args)
	if err != nil {
		result.Output = err.Error()
		result.IsError = true
		return result
	}

	result.Output = output

	return result
}
