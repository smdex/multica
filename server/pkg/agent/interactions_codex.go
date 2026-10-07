package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// handleInteractiveServerRequest leaves the app-server reader free after it
// receives a human request. The provider RPC request ID remains private here;
// the control stream gets an unrelated UUID and only a later callback writes
// the matching JSON-RPC response.
func (c *codexClient) handleInteractiveServerRequest(id int, method string, raw json.RawMessage) {
	if c.interactions == nil {
		c.respondError(id, -32601, "interactive controls are unavailable")
		return
	}
	params, err := codexInteractionParams(raw)
	if err != nil {
		c.respondError(id, -32602, "unsupported codex interaction request")
		return
	}
	turnID, _ := params["turnId"].(string)
	if turnID == "" {
		turnID = c.activeTurnID()
	}
	state := c.interactions.controlState()
	if turnID == "" || !state.Active || state.TurnID != turnID || codexInteractionOtherThread(c, params) {
		c.respondError(id, -32001, "stale or nonforeground codex interaction")
		return
	}

	switch method {
	case "item/commandExecution/requestApproval", "execCommandApproval":
		request, err := codexCommandInteraction(params, turnID)
		if err != nil {
			c.respondError(id, -32602, "unsupported codex command approval")
			return
		}
		c.presentCodexInteraction(id, request, func(response InteractionResponse) any {
			return map[string]any{"decision": codexApprovalDecision(response)}
		})
	case "item/fileChange/requestApproval", "applyPatchApproval":
		request, err := codexFileInteraction(params, turnID)
		if err != nil {
			c.respondError(id, -32602, "unsupported codex file approval")
			return
		}
		c.presentCodexInteraction(id, request, func(response InteractionResponse) any {
			return map[string]any{"decision": codexApprovalDecision(response)}
		})
	case "item/permissions/requestApproval":
		request, err := codexPermissionsInteraction(params, turnID)
		if err != nil {
			c.respondError(id, -32602, "unsupported codex permission approval")
			return
		}
		c.presentCodexInteraction(id, request, func(response InteractionResponse) any {
			if response.Cancelled || response.ChoiceID != "allow_once" {
				return map[string]any{"permissions": map[string]any{}, "scope": "turn"}
			}
			return codexPermissionsApprovalResponse(raw, c.cfg.Logger)
		})
	case "item/tool/requestUserInput", "tool/requestUserInput":
		request, err := codexQuestionInteraction(params, turnID)
		if err != nil {
			c.respondError(id, -32602, "unsupported codex user-input request")
			return
		}
		c.presentCodexInteraction(id, request, func(response InteractionResponse) any {
			return codexQuestionResponse(request, response)
		})
	case "mcpServer/elicitation/request":
		request, err := codexMcpElicitationInteraction(params, turnID)
		if err != nil {
			c.respondError(id, -32602, "unsupported codex elicitation request")
			return
		}
		c.presentCodexInteraction(id, request, func(response InteractionResponse) any {
			if response.Cancelled || response.ChoiceID != "allow_once" {
				return map[string]any{"action": "decline", "content": nil, "_meta": nil}
			}
			return map[string]any{"action": "accept", "content": map[string]any{}, "_meta": nil}
		})
	default:
		c.respondError(id, -32601, "unsupported codex app-server request: "+method)
	}
}

func (c *codexClient) presentCodexInteraction(serverRequestID int, request InteractionRequest, response func(InteractionResponse) any) {
	c.interactions.present(request,
		func(_ context.Context, interactionResponse InteractionResponse) (InputDelivery, error) {
			if err := c.respondWithError(serverRequestID, response(interactionResponse)); err != nil {
				return InputDelivery{State: "unknown", Code: "delivery_unknown"}, err
			}
			// JSON-RPC server requests have no response acknowledgement. A successful
			// pipe write cannot prove Codex processed it, so retain unknown instead
			// of retrying a potentially delivered human answer.
			return InputDelivery{State: "unknown", Code: "delivery_unknown"}, nil
		},
		func(_ context.Context) error {
			return c.respondWithError(serverRequestID, response(InteractionResponse{Cancelled: true}))
		},
	)
}

func codexInteractionParams(raw json.RawMessage) (map[string]any, error) {
	params := map[string]any{}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, fmt.Errorf("invalid params")
	}
	return params, nil
}

func codexInteractionOtherThread(c *codexClient, params map[string]any) bool {
	threadID, _ := params["threadId"].(string)
	return threadID != "" && threadID != c.getThreadID()
}

func codexApprovalInteraction(id, turnID, title, description, tool string, input map[string]any) InteractionRequest {
	return InteractionRequest{
		ID:          uuid.NewString(),
		TurnID:      turnID,
		Kind:        "approval",
		Title:       title,
		Description: description,
		Tool:        tool,
		Input:       input,
		Choices: []InteractionChoice{
			{ID: "allow_once", Label: "Allow once"},
			{ID: "deny", Label: "Deny"},
		},
	}
}

func codexCommandInteraction(params map[string]any, turnID string) (InteractionRequest, error) {
	itemID, _ := params["itemId"].(string)
	if itemID == "" {
		return InteractionRequest{}, fmt.Errorf("missing item id")
	}
	command, _ := params["command"].(string)
	cwd, _ := params["cwd"].(string)
	reason, _ := params["reason"].(string)
	title := "Run command"
	if command != "" {
		title += ": " + command
	}
	return codexApprovalInteraction(itemID, turnID, title, reason, "command_execution", compactInteractionInput(map[string]any{"command": command, "cwd": cwd})), nil
}

func codexFileInteraction(params map[string]any, turnID string) (InteractionRequest, error) {
	if itemID, _ := params["itemId"].(string); itemID == "" {
		return InteractionRequest{}, fmt.Errorf("missing item id")
	}
	reason, _ := params["reason"].(string)
	return codexApprovalInteraction("file", turnID, "Apply file changes", reason, "file_change", compactInteractionInput(map[string]any{"reason": reason})), nil
}

func codexPermissionsInteraction(params map[string]any, turnID string) (InteractionRequest, error) {
	permissions, ok := params["permissions"].(map[string]any)
	if !ok || len(permissions) == 0 {
		return InteractionRequest{}, fmt.Errorf("missing permissions")
	}
	return codexApprovalInteraction("permissions", turnID, "Approve permission profile", "Codex requested a turn-scoped permission profile.", "permissions", compactInteractionInput(map[string]any{"permissions": permissions})), nil
}

func codexMcpElicitationInteraction(params map[string]any, turnID string) (InteractionRequest, error) {
	mode, _ := params["mode"].(string)
	serverName, _ := params["serverName"].(string)
	message, _ := params["message"].(string)
	if (mode != "form" && mode != "openai/form") || serverName == "" || message == "" || codexMcpHasRequiredFields(params["requestedSchema"]) {
		return InteractionRequest{}, fmt.Errorf("unsupported elicitation")
	}
	return codexApprovalInteraction("mcp", turnID, "MCP approval: "+serverName, message, "mcp_elicitation", compactInteractionInput(map[string]any{"mode": mode})), nil
}

func codexMcpHasRequiredFields(value any) bool {
	schema, ok := value.(map[string]any)
	if !ok {
		return false
	}
	required, _ := schema["required"].([]any)
	return len(required) > 0
}

func codexQuestionInteraction(params map[string]any, turnID string) (InteractionRequest, error) {
	rawQuestions, ok := params["questions"].([]any)
	if !ok || len(rawQuestions) == 0 {
		return InteractionRequest{}, fmt.Errorf("missing questions")
	}
	questions := make([]InteractionQuestion, 0, len(rawQuestions))
	seenQuestionIDs := make(map[string]struct{}, len(rawQuestions))
	for index, rawQuestion := range rawQuestions {
		question, ok := rawQuestion.(map[string]any)
		if !ok {
			return InteractionRequest{}, fmt.Errorf("invalid question")
		}
		prompt, _ := question["question"].(string)
		if prompt == "" {
			prompt, _ = question["title"].(string)
		}
		if strings.TrimSpace(prompt) == "" {
			return InteractionRequest{}, fmt.Errorf("missing question prompt")
		}
		questionID, _ := question["id"].(string)
		if questionID == "" {
			questionID = strconv.Itoa(index)
		}
		if _, duplicate := seenQuestionIDs[questionID]; duplicate {
			return InteractionRequest{}, fmt.Errorf("duplicate question id")
		}
		seenQuestionIDs[questionID] = struct{}{}
		if secret, _ := question["isSecret"].(bool); secret {
			// The generic UI can carry secret values but cannot prove Codex's
			// request/response secrecy semantics. Do not render it at all.
			return InteractionRequest{}, fmt.Errorf("secret question is unsupported")
		}
		allowText, _ := question["isOther"].(bool)
		options, err := codexInteractionOptions(question["options"])
		if err != nil {
			return InteractionRequest{}, err
		}
		questions = append(questions, InteractionQuestion{ID: questionID, Prompt: prompt, Options: options, AllowText: allowText})
	}
	return InteractionRequest{ID: uuid.NewString(), TurnID: turnID, Kind: "question", Title: "Question", Tool: "request_user_input", Input: map[string]any{}, Questions: questions}, nil
}

func codexInteractionOptions(raw any) ([]InteractionOption, error) {
	if raw == nil {
		return nil, nil
	}
	values, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("invalid options")
	}
	options := make([]InteractionOption, 0, len(values))
	seenIDs := make(map[string]struct{}, len(values))
	for index, rawValue := range values {
		switch value := rawValue.(type) {
		case string:
			if strings.TrimSpace(value) == "" {
				return nil, fmt.Errorf("missing option label")
			}
			optionID := strconv.Itoa(index)
			if _, duplicate := seenIDs[optionID]; duplicate {
				return nil, fmt.Errorf("duplicate option id")
			}
			seenIDs[optionID] = struct{}{}
			options = append(options, InteractionOption{ID: optionID, Label: value})
		case map[string]any:
			label, _ := value["label"].(string)
			if label == "" {
				return nil, fmt.Errorf("missing option label")
			}
			optionID, _ := value["id"].(string)
			if optionID == "" {
				optionID = strconv.Itoa(index)
			}
			if _, duplicate := seenIDs[optionID]; duplicate {
				return nil, fmt.Errorf("duplicate option id")
			}
			seenIDs[optionID] = struct{}{}
			description, _ := value["description"].(string)
			options = append(options, InteractionOption{ID: optionID, Label: label, Description: description})
		default:
			return nil, fmt.Errorf("invalid option")
		}
	}
	return options, nil
}

func codexApprovalDecision(response InteractionResponse) string {
	if response.Cancelled {
		return "cancel"
	}
	if response.ChoiceID == "allow_once" {
		return "accept"
	}
	return "decline"
}

func codexQuestionResponse(request InteractionRequest, response InteractionResponse) map[string]any {
	answers := make(map[string]any)
	if response.Cancelled {
		return map[string]any{"answers": answers}
	}
	questions := make(map[string]InteractionQuestion, len(request.Questions))
	for _, question := range request.Questions {
		questions[question.ID] = question
	}
	for _, answer := range response.Answers {
		question, ok := questions[answer.QuestionID]
		if !ok {
			continue
		}
		labels := make([]string, 0, len(answer.OptionIDs)+1)
		for _, optionID := range answer.OptionIDs {
			for _, option := range question.Options {
				if option.ID == optionID {
					labels = append(labels, option.Label)
					break
				}
			}
		}
		if text := strings.TrimSpace(answer.Text); text != "" {
			labels = append(labels, text)
		}
		if len(labels) > 0 {
			answers[answer.QuestionID] = map[string]any{"answers": labels}
		}
	}
	return map[string]any{"answers": answers}
}

func compactInteractionInput(input map[string]any) map[string]any {
	compact := make(map[string]any, len(input))
	for key, value := range input {
		compact[key] = value
		text, ok := value.(string)
		if !ok || len(text) <= maxInteractionTextBytes {
			continue
		}
		compact[key] = truncateUTF8(text, maxInteractionTextBytes)
	}
	return compact
}
