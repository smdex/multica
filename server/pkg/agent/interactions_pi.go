package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// piRPCClient owns the only writer for Pi's long-lived RPC stdin. Its request
// table is deliberately separate from interactionController: a cancelled UI
// callback can race an RPC response, but must never cause the same command to
// be written twice.
type piRPCClient struct {
	mu      sync.Mutex
	writer  io.Writer
	pending map[string]chan piInteractiveRPCResponse
	closed  bool
}

type piInteractiveRPCResponse struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Command string          `json:"command"`
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   string          `json:"error"`
}

func newPiRPCClient(writer io.Writer) *piRPCClient {
	return &piRPCClient{writer: writer, pending: make(map[string]chan piInteractiveRPCResponse)}
}

func (c *piRPCClient) request(ctx context.Context, frame map[string]any) (piInteractiveRPCResponse, error) {
	requestID := uuid.NewString()
	return c.requestWithID(ctx, requestID, frame)
}

func (c *piRPCClient) requestWithID(ctx context.Context, requestID string, frame map[string]any) (piInteractiveRPCResponse, error) {
	responseCh := make(chan piInteractiveRPCResponse, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return piInteractiveRPCResponse{}, io.ErrClosedPipe
	}
	frame["id"] = requestID
	c.pending[requestID] = responseCh
	err := c.writeLocked(frame)
	if err != nil {
		delete(c.pending, requestID)
	}
	c.mu.Unlock()
	if err != nil {
		return piInteractiveRPCResponse{}, err
	}
	select {
	case response := <-responseCh:
		if !response.Success {
			if response.Error == "" {
				response.Error = "Pi RPC command was rejected"
			}
			return response, fmt.Errorf("%s", response.Error)
		}
		return response, nil
	case <-ctx.Done():
		// Keep the provider delivery unknown. The frame may have been accepted
		// just before this caller's deadline, so it is never safe to replay it.
		c.mu.Lock()
		delete(c.pending, requestID)
		c.mu.Unlock()
		return piInteractiveRPCResponse{}, ctx.Err()
	}
}

func (c *piRPCClient) notify(frame map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return io.ErrClosedPipe
	}
	return c.writeLocked(frame)
}

func (c *piRPCClient) writeLocked(frame map[string]any) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = c.writer.Write(data)
	return err
}

func (c *piRPCClient) resolve(response piInteractiveRPCResponse) bool {
	c.mu.Lock()
	responseCh, ok := c.pending[response.ID]
	if ok {
		delete(c.pending, response.ID)
	}
	c.mu.Unlock()
	if ok {
		responseCh <- response
	}
	return ok
}

func (c *piRPCClient) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	pending := c.pending
	c.pending = make(map[string]chan piInteractiveRPCResponse)
	c.mu.Unlock()
	for _, responseCh := range pending {
		close(responseCh)
	}
}

// executeInteractive keeps Pi's RPC stdin open for controls. Autonomous Pi
// remains the print/JSON execution in pi.go, including its EOF-on-prompt
// contract.
func (b *piBackend) executeInteractive(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	label := b.providerLabel
	if label == "" {
		label = "pi"
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("%s prompt must not be empty", label)
	}
	execName := b.cfg.ExecutablePath
	if execName == "" {
		execName = b.defaultExecutable
	}
	if execName == "" {
		execName = "pi"
	}
	lookedUp, err := exec.LookPath(execName)
	if err != nil {
		return nil, fmt.Errorf("%s executable not found at %q: %w", label, execName, err)
	}

	sessionPath := opts.ResumeSessionID
	if requiresNativeResume(opts) {
		if _, err := os.Stat(sessionPath); err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("resume_unavailable: %s session file does not exist", label)
			}
			return nil, fmt.Errorf("resume_unavailable: inspect %s session file: %w", label, err)
		}
	}
	if sessionPath == "" {
		if sessionPath, err = newPiSessionPath(); err != nil {
			return nil, fmt.Errorf("%s session path: %w", label, err)
		}
	}
	if err := ensurePiSessionFile(sessionPath); err != nil {
		return nil, fmt.Errorf("%s session file: %w", label, err)
	}
	lock, locked, err := tryLockPiSessionFile(sessionPath)
	if err != nil {
		return nil, fmt.Errorf("%s session lock: %w", label, err)
	}
	if !locked {
		if opts.ResumeSessionID != "" {
			return piSessionBusyResult(label, sessionPath), nil
		}
		return nil, fmt.Errorf("%s session file %q is already in use", label, sessionPath)
	}

	runCtx, cancel := runContext(ctx, opts.Timeout)
	processCtx, cancelProcess := context.WithCancel(runCtx)
	args := buildPiInteractiveArgs(sessionPath, opts, b.cfg.Logger)
	cmd, _, _ := b.cfg.commandAt(execName).execVia(processCtx, choosePiInvocation, lookedUp, args, b.cfg.Logger)
	hideAgentWindow(cmd)
	b.cfg.logAgentCommand(cmd, newAgentCommandLogArgs(args))
	// RPC transports normally outlive one prompt. A verified settlement closes
	// this execution's transport, so bound Wait in case a descendant still owns
	// a copied descriptor after its process group has been stopped.
	cmd.WaitDelay = 10 * time.Second
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}
	cmd.Env = buildEnv(b.cfg.Env)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		releasePiSessionFileLock(lock)
		cancelProcess()
		cancel()
		return nil, fmt.Errorf("%s stdout pipe: %w", label, err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = stdout.Close()
		releasePiSessionFileLock(lock)
		cancelProcess()
		cancel()
		return nil, fmt.Errorf("%s stdin pipe: %w", label, err)
	}
	cmd.Stderr = newLogWriter(b.cfg.Logger, "["+label+":stderr] ")
	if err := startOwnedProcessTree(cmd, b.cfg.Logger); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		releasePiSessionFileLock(lock)
		cancelProcess()
		cancel()
		return nil, fmt.Errorf("start %s: %w", label, err)
	}

	msgCh := make(chan Message, 256)
	resCh := make(chan Result, 1)
	controlsOut := newControlMessagePublisher(msgCh)
	rpc := newPiRPCClient(stdin)
	controller := newInteractionController(controlsOut.publish, nil)
	turnID := uuid.NewString()
	var output strings.Builder
	var outputMu sync.Mutex
	var terminalMu sync.Mutex
	terminal := false
	var terminalObserved atomic.Bool
	setTerminal := func() bool {
		terminalMu.Lock()
		defer terminalMu.Unlock()
		if terminal {
			return false
		}
		terminal = true
		terminalObserved.Store(true)
		return true
	}

	controller.steer = func(callCtx context.Context, request SteerRequest) (InputDelivery, error) {
		if _, err := rpc.request(callCtx, map[string]any{"type": "steer", "message": request.Content}); err != nil {
			if callCtx.Err() != nil || err == io.ErrClosedPipe {
				return InputDelivery{State: "unknown", Code: "delivery_unknown"}, err
			}
			return InputDelivery{State: "rejected", Code: "provider_rejected"}, nil
		}
		return InputDelivery{State: "accepted"}, nil
	}
	controller.cancelSteering = func(callCtx context.Context) error {
		// Clear queue before abort. This is the same compatibility handshake
		// used by Paseo; a refusal does not retry or otherwise replay input.
		if _, err := rpc.request(callCtx, map[string]any{"type": "clear_queue"}); err != nil {
			return err
		}
		_, err := rpc.request(callCtx, map[string]any{"type": "abort"})
		return err
	}

	go func() {
		defer cancel()
		defer cancelProcess()
		defer releasePiSessionFileLock(lock)
		defer close(resCh)
		defer close(msgCh)
		defer controlsOut.close()
		defer rpc.close()
		defer stdin.Close()
		defer releaseProcessGroup(cmd)

		start := time.Now()
		status, finalErr := "failed", ""
		effectiveProviderStatus, effectiveProviderError := "completed", ""
		recordProviderFailure := func(errText, fallback string) {
			effectiveProviderStatus = "failed"
			effectiveProviderError = strings.TrimSpace(errText)
			if effectiveProviderError == "" {
				effectiveProviderError = fallback
			}
		}
		clearProviderFailure := func() {
			effectiveProviderStatus, effectiveProviderError = "completed", ""
		}
		steerEnabled := false
		steerReady := make(chan bool, 1)
		promptDone := make(chan error, 1)
		go func() {
			// Start the handshake after the stdout scanner is live below. Waiting
			// for clear_queue before draining stdout deadlocks any Pi that writes
			// its response eagerly into a full pipe.
			_, clearErr := rpc.request(runCtx, map[string]any{"type": "clear_queue"})
			steerReady <- clearErr == nil
			_, err := rpc.requestWithID(runCtx, turnID, map[string]any{"type": "prompt", "message": prompt})
			promptDone <- err
		}()

		scanner := newAgentStreamScanner(stdout)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var event piInteractiveEvent
			if json.Unmarshal([]byte(line), &event) != nil {
				continue
			}
			if event.Type == "response" {
				rpc.resolve(piInteractiveRPCResponse{ID: event.ID, Type: event.Type, Command: event.Command, Success: event.Success, Data: event.Data, Error: event.Error})
				continue
			}
			select {
			case steerEnabled = <-steerReady:
			default:
			}
			switch event.Type {
			case "agent_start":
				trySend(msgCh, Message{Type: MessageStatus, Status: "running", SessionID: sessionPath})
			case "turn_start":
				controller.setState(ControlState{TurnID: turnID, Active: true, CanSteer: steerEnabled})
			case "turn_end":
				message := decodePiMessage(event.Message)
				if message == nil {
					continue
				}
				if message.StopReason == "error" {
					recordProviderFailure(message.ErrorMessage, label+" ended the turn with an error")
				} else {
					// A successful turn is recovery evidence for a prior retryable
					// failure, even if Pi omits auto_retry_end.
					clearProviderFailure()
				}
			case "message_update":
				if event.AssistantMessageEvent != nil && event.AssistantMessageEvent.Type == "text_delta" && event.AssistantMessageEvent.Delta != "" {
					text := stripPiToolCallMarkup(event.AssistantMessageEvent.Delta)
					if text != "" {
						outputMu.Lock()
						output.WriteString(text)
						outputMu.Unlock()
						trySend(msgCh, Message{Type: MessageText, Content: text})
					}
				}
			case "tool_execution_start":
				var input map[string]any
				_ = json.Unmarshal(event.Args, &input)
				trySend(msgCh, Message{Type: MessageToolUse, Tool: event.ToolName, CallID: event.ToolCallID, Input: input})
			case "tool_execution_end":
				trySend(msgCh, Message{Type: MessageToolResult, CallID: event.ToolCallID, Output: decodePiResult(event.Result)})
			case "error":
				errText := decodePiString(event.Message)
				trySend(msgCh, Message{Type: MessageError, Content: errText})
				recordProviderFailure(errText, label+" reported an error")
			case "auto_retry_end":
				if event.Success {
					clearProviderFailure()
				} else {
					recordProviderFailure(event.FinalError, label+" exhausted automatic retries")
				}
			case "extension_ui_request":
				if request, respond, cancelRequest, ok := piExtensionInteraction(event, turnID, rpc); ok {
					controller.present(request, respond, cancelRequest)
				}
			case "agent_settled":
				if setTerminal() {
					// agent_end is only one low-level run. Pi can automatically
					// retry, compact, or continue after it, so keep the foreground
					// controller live until the session-level settled boundary.
					//
					// RPC itself is long-lived. Once Pi has authoritatively settled
					// this execution, stop its owned transport instead of waiting for
					// stdin/stdout EOF. Closing the RPC first releases in-flight
					// command waiters without replaying any input.
					rpc.close()
					_ = controller.closeProviderTurn(context.Background())
					controller.setState(ControlState{})
					_ = stdin.Close()
					cancelProcess()
					closePiReadPipe(stdout)
				}
			}
		}
		scannerErr := scanner.Err()
		rpc.close()
		promptErr := <-promptDone
		if terminal {
			// Settlement is the provider's authoritative transport boundary. Its
			// outcome remains the last effective provider result: Pi may settle
			// after a failed turn or exhausted retry, while a later successful
			// recovery clears that failure before settlement.
			status, finalErr = effectiveProviderStatus, effectiveProviderError
		} else {
			if scannerErr != nil && runCtx.Err() == nil {
				finalErr = fmt.Sprintf("%s RPC stream failed: %v", label, scannerErr)
			}
			if promptErr != nil && finalErr == "" && runCtx.Err() == nil {
				finalErr = fmt.Sprintf("%s prompt RPC failed: %v", label, promptErr)
			}
			if runCtx.Err() == context.DeadlineExceeded {
				status, finalErr = "timeout", fmt.Sprintf("%s timed out after %s", label, opts.Timeout)
			} else if runCtx.Err() != nil && finalErr == "" {
				status, finalErr = "aborted", "execution cancelled"
			} else if finalErr == "" {
				finalErr = fmt.Sprintf("%s RPC stream ended before agent_settled", label)
			}
		}
		_ = stdin.Close()
		waitErr := cmd.Wait()
		if waitErr != nil && !terminal && finalErr == "" {
			status, finalErr = "failed", fmt.Sprintf("%s exited with error: %v", label, waitErr)
		}
		outputMu.Lock()
		resultOutput := output.String()
		outputMu.Unlock()
		resCh <- Result{Status: status, Output: resultOutput, Error: finalErr, DurationMs: time.Since(start).Milliseconds(), SessionID: sessionPath}
	}()

	return &Session{
		Messages:             msgCh,
		Result:               resCh,
		ControlState:         controller.controlState,
		Steer:                controller.steerTurn,
		RespondToInteraction: controller.respondToInteraction,
		CancelPendingInputs:  controller.cancelPendingInputs,
		TerminalObserved:     terminalObserved.Load,
	}, nil
}

type piInteractiveEvent struct {
	Type                  string                   `json:"type"`
	ID                    string                   `json:"id,omitempty"`
	Command               string                   `json:"command,omitempty"`
	Success               bool                     `json:"success,omitempty"`
	Data                  json.RawMessage          `json:"data,omitempty"`
	Error                 string                   `json:"error,omitempty"`
	AssistantMessageEvent *piAssistantMessageEvent `json:"assistantMessageEvent,omitempty"`
	ToolCallID            string                   `json:"toolCallId,omitempty"`
	ToolName              string                   `json:"toolName,omitempty"`
	Args                  json.RawMessage          `json:"args,omitempty"`
	Result                json.RawMessage          `json:"result,omitempty"`
	Method                string                   `json:"method,omitempty"`
	Title                 string                   `json:"title,omitempty"`
	Message               json.RawMessage          `json:"message,omitempty"`
	FinalError            string                   `json:"finalError,omitempty"`
	Options               []string                 `json:"options,omitempty"`
	Placeholder           string                   `json:"placeholder,omitempty"`
}

func piExtensionInteraction(event piInteractiveEvent, turnID string, rpc *piRPCClient) (InteractionRequest, func(context.Context, InteractionResponse) (InputDelivery, error), func(context.Context) error, bool) {
	if event.ID == "" {
		return InteractionRequest{}, nil, nil, false
	}
	title := strings.TrimSpace(event.Title)
	if title == "" {
		title = strings.TrimSpace(decodePiString(event.Message))
	}
	if title == "" {
		title = "Pi request"
	}
	request := InteractionRequest{ID: uuid.NewString(), TurnID: turnID, Title: title, Tool: "extension_ui:" + event.Method, Input: map[string]any{"method": event.Method}}
	switch event.Method {
	case "confirm":
		request.Kind = "approval"
		request.Choices = []InteractionChoice{{ID: "allow_once", Label: "Yes"}, {ID: "deny", Label: "No"}}
	case "select":
		request.Kind = "question"
		options := make([]InteractionOption, 0, len(event.Options))
		for index, label := range event.Options {
			if strings.TrimSpace(label) == "" {
				return InteractionRequest{}, nil, nil, false
			}
			options = append(options, InteractionOption{ID: fmt.Sprintf("%d", index), Label: label})
		}
		request.Questions = []InteractionQuestion{{ID: "answer", Prompt: title, Options: options}}
	case "input", "editor":
		request.Kind = "question"
		request.Questions = []InteractionQuestion{{ID: "answer", Prompt: title, AllowText: true, Secret: event.Method == "input" && strings.Contains(strings.ToLower(event.Placeholder), "secret")}}
	default:
		return InteractionRequest{}, nil, nil, false
	}
	respond := func(_ context.Context, answer InteractionResponse) (InputDelivery, error) {
		frame := map[string]any{"type": "extension_ui_response", "id": event.ID}
		if answer.Cancelled {
			frame["cancelled"] = true
		} else if event.Method == "confirm" {
			frame["confirmed"] = answer.ChoiceID == "allow_once"
		} else {
			value, ok := piInteractionAnswer(request.Questions[0], answer.Answers[0])
			if !ok {
				return InputDelivery{State: "rejected", Code: "invalid_input"}, nil
			}
			frame["value"] = value
		}
		if err := rpc.notify(frame); err != nil {
			return InputDelivery{State: "unknown", Code: "delivery_unknown"}, err
		}
		// Pi's extension UI reply has no response frame. Successful local write
		// remains unknown so a lost acknowledgement can never be replayed.
		return InputDelivery{State: "unknown", Code: "delivery_unknown"}, nil
	}
	cancelRequest := func(context.Context) error {
		return rpc.notify(map[string]any{"type": "extension_ui_response", "id": event.ID, "cancelled": true})
	}
	return request, respond, cancelRequest, true
}

func piInteractionAnswer(question InteractionQuestion, answer InteractionAnswer) (string, bool) {
	if answer.Text != "" {
		return answer.Text, true
	}
	if len(answer.OptionIDs) != 1 {
		return "", false
	}
	for _, option := range question.Options {
		if option.ID == answer.OptionIDs[0] {
			return option.Label, true
		}
	}
	return "", false
}

func buildPiInteractiveArgs(sessionPath string, opts ExecOptions, logger *slog.Logger) []string {
	args := []string{"--mode", "rpc", "--session", sessionPath}
	if model := strings.TrimSpace(opts.Model); model != "" {
		args = append(args, "--model", model)
	}
	if opts.ThinkingLevel != "" {
		args = append(args, "--thinking", opts.ThinkingLevel)
	}
	return append(args, filterPiCustomArgs(opts.CustomArgs, logger)...)
}
