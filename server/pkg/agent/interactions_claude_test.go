package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBuildClaudeArgsChatUsesPermissionPromptTransport(t *testing.T) {
	t.Parallel()
	args := buildClaudeArgs(ExecOptions{InteractionMode: "chat"}, slog.Default())
	if !slices.Contains(args, "default") || slices.Contains(args, "bypassPermissions") {
		t.Fatalf("chat permission mode args = %v", args)
	}
	if slices.Contains(args, "AskUserQuestion") {
		t.Fatalf("chat args disabled AskUserQuestion: %v", args)
	}
	if index := slices.Index(args, "--permission-prompt-tool"); index < 0 || index+1 == len(args) || args[index+1] != "stdio" {
		t.Fatalf("chat args missing stdio permission prompt transport: %v", args)
	}
}

func TestBuildClaudeArgsChatFiltersUnsafePermissionOverrides(t *testing.T) {
	t.Parallel()
	args := buildClaudeArgs(ExecOptions{
		InteractionMode: "chat",
		ExtraArgs: []string{
			"--dangerously-skip-permissions",
			`--allow-dangerously-skip-permissions="true"`,
			"--permission-prompt-tool=socket",
		},
		CustomArgs: []string{
			"'--dangerously-skip-permissions'",
			"--allow-dangerously-skip-permissions='true'",
			`--permission-prompt-tool="file"`,
		},
	}, slog.Default())
	for _, unsafe := range []string{
		"--dangerously-skip-permissions",
		"--allow-dangerously-skip-permissions",
		"--permission-prompt-tool=socket",
		"--permission-prompt-tool=file",
	} {
		if slices.Contains(args, unsafe) {
			t.Fatalf("chat args retained unsafe permission override %q: %v", unsafe, args)
		}
	}
	if index := slices.Index(args, "--permission-prompt-tool"); index < 0 || index+1 == len(args) || args[index+1] != "stdio" {
		t.Fatalf("managed prompt transport was replaced: %v", args)
	}
}

func TestBuildClaudeArgsAutonomousRetainsPermissionOverrideBehavior(t *testing.T) {
	t.Parallel()
	args := buildClaudeArgs(ExecOptions{
		ExtraArgs:  []string{"--dangerously-skip-permissions"},
		CustomArgs: []string{"--allow-dangerously-skip-permissions=true"},
	}, slog.Default())
	if !slices.Contains(args, "--dangerously-skip-permissions") || !slices.Contains(args, "--allow-dangerously-skip-permissions=true") {
		t.Fatalf("autonomous args unexpectedly changed: %v", args)
	}
	if slices.Contains(args, "--permission-prompt-tool") {
		t.Fatalf("autonomous args unexpectedly enabled chat prompt transport: %v", args)
	}
}

func TestClaudeChatQuestionProtocolUsesValidatedCallback(t *testing.T) {
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	backend, err := New("claude", Config{
		ExecutablePath: self,
		LaunchPrefix: []string{
			"--dangerously-skip-permissions",
			`--allow-dangerously-skip-permissions="true"`,
			"--permission-prompt-tool=socket",
		},
		Env:    map[string]string{"CLAUDE_FAKE_MODE": "chat_question", "IS_SANDBOX": "1"},
		Logger: slog.Default(),
	})
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "ask", ExecOptions{InteractionMode: "chat", Timeout: 8 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var interaction *InteractionRequest
	for message := range session.Messages {
		if message.Interaction != nil {
			interaction = message.Interaction
			break
		}
	}
	if interaction == nil || interaction.Kind != "question" || len(interaction.Questions) != 1 {
		t.Fatalf("interaction = %+v", interaction)
	}
	delivery, err := session.RespondToInteraction(ctx, InteractionResponse{
		ID: "answer-1", InteractionID: interaction.ID, ExpectedTurnID: interaction.TurnID,
		Answers: []InteractionAnswer{{QuestionID: "provider", OptionIDs: []string{"0"}}},
	})
	if err != nil {
		t.Fatalf("respond: %v", err)
	}
	if delivery.State != "unknown" {
		t.Fatalf("delivery = %+v, want unknown for unacknowledged Claude control response", delivery)
	}
	for range session.Messages {
	}
	select {
	case result, ok := <-session.Result:
		if !ok {
			t.Fatal("result channel closed without a result")
		}
		if result.Status != "completed" || result.Output != "answered" {
			t.Fatalf("result = %+v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Claude fake protocol did not complete after the UI response")
	}
}

func TestClaudeChatApprovalProtocolUsesValidatedCallback(t *testing.T) {
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	backend, err := New("claude", Config{
		ExecutablePath: self,
		Env:            map[string]string{"CLAUDE_FAKE_MODE": "chat_control_request", "IS_SANDBOX": "1"},
		Logger:         slog.Default(),
	})
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "run", ExecOptions{InteractionMode: "chat", Timeout: 8 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var interaction *InteractionRequest
	for message := range session.Messages {
		if message.Interaction != nil {
			interaction = message.Interaction
			break
		}
	}
	if interaction == nil || interaction.Kind != "approval" || interaction.Tool != "Bash" {
		t.Fatalf("interaction = %+v", interaction)
	}
	delivery, err := session.RespondToInteraction(ctx, InteractionResponse{
		ID: "approval-1", InteractionID: interaction.ID, ExpectedTurnID: interaction.TurnID, ChoiceID: "allow_once",
	})
	if err != nil || delivery.State != "unknown" {
		t.Fatalf("approval delivery = %+v, %v", delivery, err)
	}
	for range session.Messages {
	}
	result, ok := <-session.Result
	if !ok || result.Status != "completed" || result.Output != "done after control" {
		t.Fatalf("result = %+v, open=%v", result, ok)
	}
}

func TestClaudeChatApprovalPreservesExecutionInputBeyondDisplayLimit(t *testing.T) {
	t.Parallel()
	command := strings.Repeat("x", maxInteractionTextBytes+1)
	input, err := json.Marshal(map[string]any{"command": command})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	payload, err := json.Marshal(claudeControlRequestPayload{
		Subtype: "can_use_tool", ToolName: "Bash", Input: input,
	})
	if err != nil {
		t.Fatalf("marshal control request: %v", err)
	}

	var provider bytes.Buffer
	writer := &claudeControlWriter{writer: &provider}
	var interaction *InteractionRequest
	controller := newInteractionController(func(message Message) bool {
		if message.Interaction != nil {
			interaction = message.Interaction
		}
		return true
	}, nil)
	controller.setState(ControlState{TurnID: "turn-1", Active: true})
	(&claudeBackend{}).handleInteractiveControlRequest(claudeSDKMessage{
		RequestID: "request-1", Request: payload,
	}, writer, controller, "turn-1")

	if interaction == nil || interaction.Kind != "approval" {
		t.Fatalf("interaction = %+v", interaction)
	}
	displayCommand, ok := interaction.Input["command"].(string)
	if !ok || len(displayCommand) != maxInteractionTextBytes {
		t.Fatalf("display command length = %d, want %d", len(displayCommand), maxInteractionTextBytes)
	}
	if displayCommand == command {
		t.Fatal("display command was not compacted")
	}

	delivery, err := controller.respondToInteraction(context.Background(), InteractionResponse{
		ID: "approval-1", InteractionID: interaction.ID, ExpectedTurnID: "turn-1", ChoiceID: "allow_once",
	})
	if err != nil || delivery.State != "unknown" {
		t.Fatalf("approval delivery = %+v, %v", delivery, err)
	}
	var response struct {
		Response struct {
			RequestID string `json:"request_id"`
			Response  struct {
				Behavior     string         `json:"behavior"`
				UpdatedInput map[string]any `json:"updatedInput"`
			} `json:"response"`
		} `json:"response"`
	}
	if err := json.Unmarshal(provider.Bytes(), &response); err != nil {
		t.Fatalf("decode provider response: %v", err)
	}
	updatedCommand, _ := response.Response.Response.UpdatedInput["command"].(string)
	if response.Response.RequestID != "request-1" || response.Response.Response.Behavior != "allow" || updatedCommand != command {
		t.Fatalf("provider response = %+v", response)
	}
}
