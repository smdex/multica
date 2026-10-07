package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// claudeControlWriter serializes the initial stream-json user frame with later
// permission responses. The scanner never waits for this lock or for UI input.
type claudeControlWriter struct {
	mu       sync.Mutex
	writer   io.Writer
	denials  chan func() error
	done     chan struct{}
	drained  chan struct{}
	failOnce sync.Once
	fail     func()
}

func (w *claudeControlWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(data)
}

// One bounded worker handles reader-originated denials. Queue admission never
// waits for stdin; overflow or a write error terminates the control transport.
func (w *claudeControlWriter) startDenials(fail func()) {
	w.denials = make(chan func() error, 64)
	w.done = make(chan struct{})
	w.drained = make(chan struct{})
	w.fail = fail
	go func() {
		defer close(w.drained)
		for {
			select {
			case <-w.done:
				return
			case write := <-w.denials:
				if err := write(); err != nil {
					w.failOnce.Do(w.fail)
					return
				}
			}
		}
	}()
}

func (w *claudeControlWriter) deny(requestID string, input map[string]any) {
	select {
	case <-w.done:
		return
	default:
	}
	select {
	case w.denials <- func() error { return writeClaudeInteractionResponse(w, requestID, input, true) }:
	default:
		w.failOnce.Do(w.fail)
	}
}

func (w *claudeControlWriter) stopDenials() {
	close(w.done)
	<-w.drained
}

func newInteractionTurnID() string { return uuid.NewString() }

func (b *claudeBackend) handleInteractiveControlRequest(msg claudeSDKMessage, stdin *claudeControlWriter, controller *interactionController, turnID string) {
	if msg.RequestID == "" {
		return
	}
	var payload claudeControlRequestPayload
	if err := json.Unmarshal(msg.Request, &payload); err != nil {
		stdin.deny(msg.RequestID, nil)
		return
	}
	input := map[string]any{}
	if len(payload.Input) > 0 {
		if err := json.Unmarshal(payload.Input, &input); err != nil || input == nil {
			stdin.deny(msg.RequestID, nil)
			return
		}
	}
	request, err := claudeInteractionRequest(payload, input, turnID)
	if err != nil {
		// A request we cannot normalize must be denied instead of leaving the
		// provider reader blocked behind an unanswerable UI record.
		stdin.deny(msg.RequestID, input)
		return
	}
	controller.present(request,
		func(_ context.Context, response InteractionResponse) (InputDelivery, error) {
			updatedInput, denied := claudeInteractionResponseInput(request, input, response)
			if err := writeClaudeInteractionResponse(stdin, msg.RequestID, updatedInput, denied); err != nil {
				return InputDelivery{State: "unknown", Code: "delivery_unknown"}, err
			}
			// Claude control_response has no acknowledgement. Preserve unknown so
			// an ambiguous write cannot be sent a second time.
			return InputDelivery{State: "unknown", Code: "delivery_unknown"}, nil
		},
		func(_ context.Context) error {
			stdin.deny(msg.RequestID, input)
			return nil
		},
	)
}

func claudeInteractionRequest(payload claudeControlRequestPayload, input map[string]any, turnID string) (InteractionRequest, error) {
	if turnID == "" {
		return InteractionRequest{}, fmt.Errorf("missing foreground turn")
	}
	if payload.ToolName == "AskUserQuestion" {
		questions, err := claudeAskUserQuestions(input["questions"])
		if err != nil {
			return InteractionRequest{}, err
		}
		return InteractionRequest{ID: uuid.NewString(), TurnID: turnID, Kind: "question", Title: "Question", Tool: payload.ToolName, Input: compactInteractionInput(input), Questions: questions}, nil
	}
	if payload.Subtype != "can_use_tool" && payload.Subtype != "tool_use" && payload.Subtype != "permission" {
		return InteractionRequest{}, fmt.Errorf("unsupported control request")
	}
	if strings.TrimSpace(payload.ToolName) == "" {
		return InteractionRequest{}, fmt.Errorf("missing tool name")
	}
	return InteractionRequest{
		ID: uuid.NewString(), TurnID: turnID, Kind: "approval", Title: "Allow " + payload.ToolName,
		Tool: payload.ToolName, Input: compactInteractionInput(input),
		Choices: []InteractionChoice{{ID: "allow_once", Label: "Allow once"}, {ID: "deny", Label: "Deny"}},
	}, nil
}

func claudeAskUserQuestions(raw any) ([]InteractionQuestion, error) {
	values, ok := raw.([]any)
	if !ok || len(values) == 0 {
		return nil, fmt.Errorf("missing questions")
	}
	questions := make([]InteractionQuestion, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for index, rawQuestion := range values {
		question, ok := rawQuestion.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid question")
		}
		prompt, _ := question["question"].(string)
		if strings.TrimSpace(prompt) == "" {
			return nil, fmt.Errorf("missing question prompt")
		}
		id, _ := question["header"].(string)
		if id == "" {
			id = fmt.Sprintf("%d", index)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("duplicate question")
		}
		seen[id] = struct{}{}
		options, err := codexInteractionOptions(question["options"])
		if err != nil {
			return nil, err
		}
		multiple, _ := question["multiSelect"].(bool)
		// Claude provides "Other" itself, so free text is valid in addition to
		// the offered options and is mapped back to its full question text.
		questions = append(questions, InteractionQuestion{ID: id, Prompt: prompt, Options: options, Multiple: multiple, AllowText: true})
	}
	return questions, nil
}

func claudeInteractionResponseInput(request InteractionRequest, fallback map[string]any, response InteractionResponse) (map[string]any, bool) {
	if response.Cancelled || (request.Kind == "approval" && response.ChoiceID != "allow_once") {
		return fallback, true
	}
	if request.Kind == "approval" {
		updated := make(map[string]any, len(fallback))
		for key, value := range fallback {
			updated[key] = value
		}
		forceClaudeToolInputForeground(updated)
		return updated, false
	}
	updated := make(map[string]any, len(fallback)+1)
	for key, value := range fallback {
		updated[key] = value
	}
	answers := make(map[string]string, len(response.Answers))
	byID := make(map[string]InteractionQuestion, len(request.Questions))
	for _, question := range request.Questions {
		byID[question.ID] = question
	}
	for _, answer := range response.Answers {
		question := byID[answer.QuestionID]
		value := answer.Text
		if value == "" {
			labels := make([]string, 0, len(answer.OptionIDs))
			for _, optionID := range answer.OptionIDs {
				for _, option := range question.Options {
					if option.ID == optionID {
						labels = append(labels, option.Label)
					}
				}
			}
			value = strings.Join(labels, ", ")
		}
		answers[question.Prompt] = value
	}
	updated["answers"] = answers
	return updated, false
}

func writeClaudeInteractionResponse(writer io.Writer, requestID string, updatedInput map[string]any, denied bool) error {
	behavior := "allow"
	if denied {
		behavior = "deny"
	}
	frame := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype": "success", "request_id": requestID,
			"response": map[string]any{"behavior": behavior, "updatedInput": updatedInput},
		},
	}
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	_, err = writer.Write(append(data, '\n'))
	return err
}
