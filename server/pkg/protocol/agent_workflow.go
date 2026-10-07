package protocol

import (
	"encoding/json"
	"time"
)

// AgentWorkflowCapabilities is reported by a live runtime. Every false or
// omitted field is unsupported; provider family is not a capability signal.
type AgentWorkflowCapabilities struct {
	NativeSessions AgentWorkflowNativeSessionCapabilities `json:"native_sessions"`
	Controls       AgentWorkflowControlCapabilities       `json:"controls"`
}

type AgentWorkflowNativeSessionCapabilities struct {
	List   bool `json:"list"`
	Import bool `json:"import"`
}

type AgentWorkflowControlCapabilities struct {
	Steer     bool `json:"steer"`
	Approvals bool `json:"approvals"`
	Questions bool `json:"questions"`
}

// AgentWorkflowCommand is a durable server-to-daemon operation. Body is a
// kind-specific object; the daemon decodes it only after validating the kind.
type AgentWorkflowCommand struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	RuntimeID   string          `json:"runtime_id"`
	WorkspaceID string          `json:"workspace_id"`
	RequesterID string          `json:"requester_id"`
	ExpiresAt   time.Time       `json:"expires_at"`
	Body        json.RawMessage `json:"body"`
}

// AgentWorkflowError is the terminal, typed error returned with a workflow
// result. It intentionally never exposes provider protocol payloads.
type AgentWorkflowError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// AgentWorkflowCommandResult is the daemon's idempotent report for a claimed
// workflow command. Result is kind-specific and may be null on failure.
type AgentWorkflowCommandResult struct {
	Status string              `json:"status"`
	Result json.RawMessage     `json:"result"`
	Error  *AgentWorkflowError `json:"error"`
}

// TaskControlState records one daemon-registered foreground execution. The
// server fences every steering and interaction response with task_id, run_id,
// and turn_id before a command can be dispatched.
type TaskControlState struct {
	RunID      string  `json:"run_id"`
	TurnID     *string `json:"turn_id"`
	Active     bool    `json:"active"`
	CanSteer   bool    `json:"can_steer"`
	CanApprove bool    `json:"can_approve"`
	CanAnswer  bool    `json:"can_answer"`
}

// AgentWorkflowInteractionRequest is the normalized, browser-safe provider
// request. Provider RPC closure IDs remain in the daemon process and are not
// represented here.
type AgentWorkflowInteractionRequest struct {
	ID          string                             `json:"id"`
	TurnID      string                             `json:"turn_id"`
	Kind        string                             `json:"kind"`
	Title       string                             `json:"title"`
	Description string                             `json:"description"`
	Tool        string                             `json:"tool"`
	Input       map[string]any                     `json:"input"`
	Choices     []AgentWorkflowInteractionChoice   `json:"choices"`
	Questions   []AgentWorkflowInteractionQuestion `json:"questions"`
	ExpiresAt   time.Time                          `json:"expires_at"`
}

type AgentWorkflowInteractionChoice struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type AgentWorkflowInteractionQuestion struct {
	ID        string                           `json:"id"`
	Prompt    string                           `json:"prompt"`
	Options   []AgentWorkflowInteractionOption `json:"options"`
	Multiple  bool                             `json:"multiple"`
	AllowText bool                             `json:"allow_text"`
	Secret    bool                             `json:"secret"`
}

type AgentWorkflowInteractionOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// AgentWorkflowInteractionResponse is exactly one of approval choice,
// question answers, or cancelled. The handler validates the selected branch
// against the stored request before dispatch.
type AgentWorkflowInteractionResponse struct {
	ChoiceID  string                           `json:"choice_id,omitempty"`
	Answers   []AgentWorkflowInteractionAnswer `json:"answers,omitempty"`
	Cancelled bool                             `json:"cancelled,omitempty"`
}

type AgentWorkflowInteractionAnswer struct {
	QuestionID string   `json:"question_id"`
	OptionIDs  []string `json:"option_ids"`
	Text       string   `json:"text"`
}
