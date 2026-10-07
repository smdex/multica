package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

func TestCodexInteractiveQuestionDefersResponseUntilValidatedCallback(t *testing.T) {
	t.Parallel()
	client, stdin, _ := newTestCodexClient(t)
	var mu sync.Mutex
	var controls []Message
	controller := newInteractionController(func(message Message) bool {
		mu.Lock()
		controls = append(controls, message)
		mu.Unlock()
		return true
	}, nil)
	client.interactions = controller
	client.setThreadID("thread-1")
	client.setActiveTurnID("turn-1")
	controller.setState(ControlState{TurnID: "turn-1", Active: true, CanSteer: true})

	client.handleLine(`{"jsonrpc":"2.0","id":41,"method":"item/tool/requestUserInput","params":{"threadId":"thread-1","turnId":"turn-1","questions":[{"id":"choice","question":"Choose","options":["One","Two"]}]}}`)
	if lines := stdin.Lines(); len(lines) != 0 {
		t.Fatalf("provider response was written before UI input: %v", lines)
	}
	mu.Lock()
	var interaction *InteractionRequest
	for _, message := range controls {
		if message.Interaction != nil {
			interaction = message.Interaction
		}
	}
	mu.Unlock()
	if interaction == nil {
		t.Fatal("Codex request did not emit an interaction")
	}
	if interaction.TurnID != "turn-1" || interaction.Kind != "question" || len(interaction.Questions) != 1 {
		t.Fatalf("interaction = %+v", interaction)
	}

	delivery, err := controller.respondToInteraction(context.Background(), InteractionResponse{
		ID: "answer-1", InteractionID: interaction.ID, ExpectedTurnID: "turn-1",
		Answers: []InteractionAnswer{{QuestionID: "choice", OptionIDs: []string{"0"}}},
	})
	if err != nil {
		t.Fatalf("respond: %v", err)
	}
	if delivery.State != "unknown" || delivery.Code != "delivery_unknown" {
		t.Fatalf("delivery = %+v, want unknown after non-acknowledged JSON-RPC server response", delivery)
	}
	lines := stdin.Lines()
	if len(lines) != 1 {
		t.Fatalf("writes = %d, want one response", len(lines))
	}
	var response struct {
		ID     int `json:"id"`
		Result struct {
			Answers map[string]any `json:"answers"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &response); err != nil {
		t.Fatalf("decode provider response: %v", err)
	}
	answer, ok := response.Result.Answers["choice"].(map[string]any)
	if response.ID != 41 || !ok {
		t.Fatalf("response = %s", lines[0])
	}
	values, ok := answer["answers"].([]any)
	if !ok || len(values) != 1 || values[0] != "One" {
		t.Fatalf("response = %s", lines[0])
	}
}

func TestCodexQuestionInteractionValidatesProviderQuestionShape(t *testing.T) {
	t.Parallel()
	valid := map[string]any{"questions": []any{map[string]any{
		"id": "q1", "question": "Choose", "isOther": true,
		"options": []any{map[string]any{"id": "one", "label": "One"}},
	}}}
	request, err := codexQuestionInteraction(valid, "turn-1")
	if err != nil {
		t.Fatalf("valid request: %v", err)
	}
	if len(request.Questions) != 1 || !request.Questions[0].AllowText || request.Questions[0].Secret {
		t.Fatalf("normalized question = %+v", request.Questions)
	}
	cases := []struct {
		name   string
		params map[string]any
	}{
		{"secret", map[string]any{"questions": []any{map[string]any{"id": "q", "question": "Secret", "isSecret": true}}}},
		{"duplicate question ids", map[string]any{"questions": []any{map[string]any{"id": "q", "question": "First"}, map[string]any{"id": "q", "question": "Second"}}}},
		{"duplicate option ids", map[string]any{"questions": []any{map[string]any{"id": "q", "question": "Choose", "options": []any{map[string]any{"id": "same", "label": "One"}, map[string]any{"id": "same", "label": "Two"}}}}}},
		{"invalid options", map[string]any{"questions": []any{map[string]any{"id": "q", "question": "Choose", "options": "not an array"}}}},
		{"blank option", map[string]any{"questions": []any{map[string]any{"id": "q", "question": "Choose", "options": []any{" "}}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := codexQuestionInteraction(tc.params, "turn-1"); err == nil {
				t.Fatal("malformed provider question was normalized into a UI record")
			}
		})
	}
}

func TestCodexInteractiveCommandApprovalDefersProviderResponse(t *testing.T) {
	t.Parallel()
	client, stdin, _ := newTestCodexClient(t)
	var interaction *InteractionRequest
	controller := newInteractionController(func(message Message) bool {
		if message.Interaction != nil {
			interaction = message.Interaction
		}
		return true
	}, nil)
	client.interactions = controller
	client.setThreadID("thread-1")
	client.setActiveTurnID("turn-1")
	controller.setState(ControlState{TurnID: "turn-1", Active: true, CanSteer: true})
	client.handleLine(`{"jsonrpc":"2.0","id":42,"method":"item/commandExecution/requestApproval","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"command-1","command":"pwd","cwd":"/work"}}`)
	if interaction == nil || interaction.Kind != "approval" || interaction.Tool != "command_execution" {
		t.Fatalf("interaction = %+v", interaction)
	}
	if lines := stdin.Lines(); len(lines) != 0 {
		t.Fatalf("provider response was written before approval: %v", lines)
	}
	delivery, err := controller.respondToInteraction(context.Background(), InteractionResponse{
		ID: "approval-1", InteractionID: interaction.ID, ExpectedTurnID: "turn-1", ChoiceID: "allow_once",
	})
	if err != nil || delivery.State != "unknown" {
		t.Fatalf("approval delivery = %+v, %v", delivery, err)
	}
	lines := stdin.Lines()
	if len(lines) != 1 || !strings.Contains(lines[0], `"id":42`) || !strings.Contains(lines[0], `"decision":"accept"`) {
		t.Fatalf("command approval response = %v", lines)
	}
}

func TestCodexRequireNativeResumeNeverFallsBackToThreadStart(t *testing.T) {
	t.Parallel()
	client, stdin, _ := newTestCodexClient(t)
	wait := drainRPCScript(t, client, stdin, []rpcResponse{{
		method: "thread/resume", errCode: -32000, errMsg: "thread unavailable",
	}})
	defer wait()
	_, resumed, err := client.startOrResumeThread(context.Background(), ExecOptions{
		ResumeSessionID: "thread-old", ResumePolicy: "require_native", InteractionMode: "chat",
	}, slog.Default())
	if err == nil || resumed {
		t.Fatalf("strict resume = resumed:%v err:%v, want resume failure", resumed, err)
	}
	for _, line := range stdin.Lines() {
		if strings.Contains(line, `"method":"thread/start"`) {
			t.Fatalf("strict resume sent a fresh thread/start after refusal: %s", line)
		}
	}
}
