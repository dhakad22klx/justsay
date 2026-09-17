package state

import (
	"encoding/json"
	"fmt"

	providers "justsay-harness/providers"
)

// RunStatus is where a run stands. A string, so a key read straight out of
// Redis says what it means.
type RunStatus string

const (
	StatusRunning         RunStatus = "running"          // the loop holds the run
	StatusWaitingApproval RunStatus = "waiting_approval" // parked on a human decision
	StatusCompleted       RunStatus = "completed"        // finished with an answer
	StatusFailed          RunStatus = "failed"           // stopped on an error
)

// AgentState is everything a paused run needs to be resumed by another
// process. It is stored as JSON, so the tags are the wire format.
type AgentState struct {
	SessionID string `json:"session_id"`

	// History round-trips whole: providers.Message stores the provider's own
	// copy of a model turn alongside the shared fields, so a resumed run
	// replays what the model actually sent - thought signatures included.
	History []providers.Message `json:"history"`

	// PendingApproval is set only while Status is StatusWaitingApproval.
	PendingApproval *PendingApproval `json:"pending_approval,omitempty"`

	Status RunStatus `json:"status"`

	// Step counts turns taken, so a resumed run does not restart its budget.
	Step int `json:"step"`
}

// Waiting reports whether the run is parked on a human decision.
func (s *AgentState) Waiting() bool {
	return s.Status == StatusWaitingApproval && s.PendingApproval != nil
}

// PendingApproval is the tool call the model asked for, held whole so that
// whatever runs later is what was actually requested.
type PendingApproval struct {
	// ID matches a decision to the request it answers, so a stale approval is
	// not applied to whatever is pending now.
	ID             string             `json:"id"`
	ToolCall       providers.ToolCall `json:"tool_call"`
	ApprovalStatus string             `json:"approval_status"`
}

// ApprovalDecision is the answer a human gave: approved alone is a plain yes,
// approved with arguments is "yes, but like this", not approved is a no.
type ApprovalDecision struct {
	Approved bool `json:"approved"`

	// ModifiedArgs stays raw because it arrives as JSON and the tool, not this
	// package, knows its shape.
	ModifiedArgs json.RawMessage `json:"modified_args,omitempty"`
}

// Modified reports whether the decision carries replacement arguments.
func (d ApprovalDecision) Modified() bool {
	return len(d.ModifiedArgs) > 0 && string(d.ModifiedArgs) != "null"
}

// Args is what the tool should run with. A replacement is used whole rather
// than merged, so an argument someone deleted stays deleted.
func (d ApprovalDecision) Args(call providers.ToolCall) (map[string]any, error) {
	if !d.Modified() {
		return call.Args, nil
	}

	var args map[string]any
	if err := json.Unmarshal(d.ModifiedArgs, &args); err != nil {
		return nil, fmt.Errorf("cannot decode the edited arguments for %s: %w", call.Name, err)
	}

	return args, nil
}
