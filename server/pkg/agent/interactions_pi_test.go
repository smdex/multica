//go:build unix

package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPiChatRPCProtocolKeepsControlsThroughAgentEndAndSettlesWithOpenPipes(t *testing.T) {
	t.Parallel()
	fakePath := filepath.Join(t.TempDir(), "pi")
	script := `#!/bin/sh
case "$*" in
  *"--mode rpc"*) ;;
  *) echo "missing rpc mode" >&2; exit 61 ;;
esac
IFS= read -r clear
clear_id=$(printf '%s\n' "$clear" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
printf '{"type":"response","id":"%s","command":"clear_queue","success":true}\n' "$clear_id"
IFS= read -r prompt
prompt_id=$(printf '%s\n' "$prompt" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
printf '%s\n' '{"type":"agent_start"}'
printf '%s\n' '{"type":"turn_start"}'
printf '{"type":"response","id":"%s","command":"prompt","success":true}\n' "$prompt_id"
printf '%s\n' '{"type":"extension_ui_request","id":"ui-1","method":"select","title":"Pick one","options":["One","Two"]}'
IFS= read -r response
case "$response" in *'"type":"extension_ui_response"'*) ;; *) echo "unexpected UI response: $response" >&2; exit 62 ;; esac
case "$response" in *'"id":"ui-1"'*) ;; *) echo "unexpected UI response: $response" >&2; exit 62 ;; esac
case "$response" in *'"value":"One"'*) ;; *) echo "unexpected UI response: $response" >&2; exit 62 ;; esac
printf '%s\n' '{"type":"agent_end"}'
printf '%s\n' '{"type":"agent_start"}'
printf '%s\n' '{"type":"turn_start"}'
printf '%s\n' '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"continuing"}}'
IFS= read -r steer
case "$steer" in *'"type":"steer"'*) ;; *) echo "unexpected steer: $steer" >&2; exit 63 ;; esac
steer_id=$(printf '%s\n' "$steer" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
printf '{"type":"response","id":"%s","command":"steer","success":true}\n' "$steer_id"
printf '%s\n' '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"done"}}'
printf '%s\n' '{"type":"agent_end"}'
printf '%s\n' '{"type":"agent_settled"}'
# RPC normally remains ready for another prompt after settlement. Keep both
# transport pipes open so the adapter must perform bounded teardown itself.
while :; do sleep 1; done
`
	writeTestExecutable(t, fakePath, []byte(script))
	sessionPath := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(sessionPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write session: %v", err)
	}
	backend, err := New("pi", Config{ExecutablePath: fakePath, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new pi backend: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "hello", ExecOptions{InteractionMode: "chat", ResumeSessionID: sessionPath, Timeout: 8 * time.Second})
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
	state := session.ControlState()
	if !state.Active || !state.CanSteer || state.TurnID != interaction.TurnID {
		t.Fatalf("control state = %+v, want live Pi RPC turn", state)
	}
	delivery, err := session.RespondToInteraction(ctx, InteractionResponse{
		ID: "answer-1", InteractionID: interaction.ID, ExpectedTurnID: interaction.TurnID,
		Answers: []InteractionAnswer{{QuestionID: "answer", OptionIDs: []string{"0"}}},
	})
	if err != nil || delivery.State != "unknown" {
		t.Fatalf("response delivery = %+v, %v; want unknown after unacknowledged extension reply", delivery, err)
	}
	continuationControls := false
	for message := range session.Messages {
		if message.Type != MessageText || message.Content != "continuing" {
			continue
		}
		state := session.ControlState()
		if !state.Active || !state.CanSteer || state.TurnID != interaction.TurnID {
			t.Fatalf("control state after agent_end = %+v, want continuing Pi RPC turn", state)
		}
		delivery, err := session.Steer(ctx, SteerRequest{ID: "steer-1", ExpectedTurnID: interaction.TurnID, Content: "continue"})
		if err != nil || delivery.State != "accepted" {
			t.Fatalf("steer delivery after agent_end = %+v, %v; want accepted", delivery, err)
		}
		continuationControls = true
	}
	if !continuationControls {
		t.Fatal("did not observe the continuation after agent_end")
	}
	select {
	case result, ok := <-session.Result:
		if !ok {
			t.Fatal("result channel closed without a result")
		}
		if result.Status != "completed" || result.Output != "continuingdone" || result.SessionID != sessionPath {
			t.Fatalf("result = %+v", result)
		}
		if !session.TerminalObserved() {
			t.Fatal("settled Pi RPC execution did not expose its terminal boundary")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Pi RPC fake did not complete after agent_settled with open pipes")
	}
}

func TestPiChatRPCProviderFailureSurvivesSettlement(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		failureEvent string
		wantError    string
	}{
		"failed_turn": {
			failureEvent: `{"type":"turn_end","message":{"role":"assistant","stopReason":"error","errorMessage":"turn provider failure"}}`,
			wantError:    "turn provider failure",
		},
		"error_event": {
			failureEvent: `{"type":"error","message":"provider error event"}`,
			wantError:    "provider error event",
		},
		"exhausted_auto_retry": {
			failureEvent: `{"type":"auto_retry_end","success":false,"finalError":"automatic retries exhausted"}`,
			wantError:    "automatic retries exhausted",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			fakePath := filepath.Join(t.TempDir(), "pi")
			script := `#!/bin/sh
IFS= read -r clear
clear_id=$(printf '%s\n' "$clear" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
printf '{"type":"response","id":"%s","command":"clear_queue","success":true}\n' "$clear_id"
IFS= read -r prompt
prompt_id=$(printf '%s\n' "$prompt" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
printf '{"type":"response","id":"%s","command":"prompt","success":true}\n' "$prompt_id"
printf '%s\n' '{"type":"agent_start"}'
printf '%s\n' '{"type":"turn_start"}'
printf '%s\n' '__FAILURE_EVENT__'
printf '%s\n' '{"type":"agent_end"}'
printf '%s\n' '{"type":"agent_settled"}'
while :; do sleep 1; done
`
			script = strings.ReplaceAll(script, "__FAILURE_EVENT__", test.failureEvent)
			writeTestExecutable(t, fakePath, []byte(script))
			sessionPath := filepath.Join(t.TempDir(), "session.jsonl")
			if err := os.WriteFile(sessionPath, []byte("{}\n"), 0o600); err != nil {
				t.Fatalf("write session: %v", err)
			}
			backend, err := New("pi", Config{ExecutablePath: fakePath, Logger: slog.Default()})
			if err != nil {
				t.Fatalf("new pi backend: %v", err)
			}
			session, err := backend.Execute(t.Context(), "hello", ExecOptions{InteractionMode: "chat", ResumeSessionID: sessionPath, Timeout: 5 * time.Second})
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			for range session.Messages {
			}
			result, ok := <-session.Result
			if !ok {
				t.Fatal("result channel closed without a result")
			}
			if result.Status != "failed" || !strings.Contains(result.Error, test.wantError) {
				t.Fatalf("result = %+v, want failed provider outcome %q", result, test.wantError)
			}
			if !session.TerminalObserved() {
				t.Fatal("agent_settled did not expose the terminal transport boundary")
			}
		})
	}
}

func TestPiChatRPCSuccessfulRecoveryClearsPriorFailureAtSettlement(t *testing.T) {
	t.Parallel()
	fakePath := filepath.Join(t.TempDir(), "pi")
	script := `#!/bin/sh
IFS= read -r clear
clear_id=$(printf '%s\n' "$clear" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
printf '{"type":"response","id":"%s","command":"clear_queue","success":true}\n' "$clear_id"
IFS= read -r prompt
prompt_id=$(printf '%s\n' "$prompt" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
printf '{"type":"response","id":"%s","command":"prompt","success":true}\n' "$prompt_id"
printf '%s\n' '{"type":"agent_start"}'
printf '%s\n' '{"type":"turn_start"}'
printf '%s\n' '{"type":"turn_end","message":{"role":"assistant","stopReason":"error","errorMessage":"retryable provider failure"}}'
printf '%s\n' '{"type":"agent_end"}'
printf '%s\n' '{"type":"auto_retry_start"}'
printf '%s\n' '{"type":"agent_start"}'
printf '%s\n' '{"type":"turn_start"}'
printf '%s\n' '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"recovered"}}'
printf '%s\n' '{"type":"turn_end","message":{"role":"assistant"}}'
printf '%s\n' '{"type":"auto_retry_end","success":true}'
printf '%s\n' '{"type":"agent_end"}'
printf '%s\n' '{"type":"agent_settled"}'
while :; do sleep 1; done
`
	writeTestExecutable(t, fakePath, []byte(script))
	sessionPath := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(sessionPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write session: %v", err)
	}
	backend, err := New("pi", Config{ExecutablePath: fakePath, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new pi backend: %v", err)
	}
	session, err := backend.Execute(t.Context(), "hello", ExecOptions{InteractionMode: "chat", ResumeSessionID: sessionPath, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	for range session.Messages {
	}
	result, ok := <-session.Result
	if !ok {
		t.Fatal("result channel closed without a result")
	}
	if result.Status != "completed" || result.Error != "" || result.Output != "recovered" {
		t.Fatalf("result = %+v, want recovered completed outcome", result)
	}
	if !session.TerminalObserved() {
		t.Fatal("agent_settled did not expose the terminal transport boundary")
	}
}

func TestPiChatRPCAgentEndDoesNotCompleteAndTimeoutRemainsTimeout(t *testing.T) {
	t.Parallel()
	fakePath := filepath.Join(t.TempDir(), "pi")
	script := `#!/bin/sh
IFS= read -r clear
clear_id=$(printf '%s\n' "$clear" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
printf '{"type":"response","id":"%s","command":"clear_queue","success":true}\n' "$clear_id"
IFS= read -r prompt
prompt_id=$(printf '%s\n' "$prompt" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
printf '%s\n' '{"type":"agent_start"}'
printf '%s\n' '{"type":"turn_start"}'
printf '{"type":"response","id":"%s","command":"prompt","success":true}\n' "$prompt_id"
printf '%s\n' '{"type":"agent_end"}'
printf '%s\n' '{"type":"agent_start"}'
printf '%s\n' '{"type":"turn_start"}'
printf '%s\n' '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"still-running"}}'
while :; do sleep 1; done
`
	writeTestExecutable(t, fakePath, []byte(script))
	sessionPath := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(sessionPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write session: %v", err)
	}
	backend, err := New("pi", Config{ExecutablePath: fakePath, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new pi backend: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "hello", ExecOptions{InteractionMode: "chat", ResumeSessionID: sessionPath, Timeout: 800 * time.Millisecond})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	seenContinuation := false
	for message := range session.Messages {
		if message.Type == MessageText && message.Content == "still-running" {
			seenContinuation = true
			state := session.ControlState()
			if !state.Active || !state.CanSteer {
				t.Fatalf("control state after agent_end = %+v, want live continuation", state)
			}
		}
	}
	if !seenContinuation {
		t.Fatal("did not observe continuation after agent_end")
	}
	result, ok := <-session.Result
	if !ok {
		t.Fatal("result channel closed without a result")
	}
	if result.Status != "timeout" || !strings.Contains(result.Error, "timed out") {
		t.Fatalf("result = %+v, want timeout after missing agent_settled", result)
	}
	if session.TerminalObserved() {
		t.Fatal("agent_end without agent_settled exposed a terminal boundary")
	}
}

func TestPiChatRPCEOFBeforeAgentSettledFails(t *testing.T) {
	t.Parallel()
	fakePath := filepath.Join(t.TempDir(), "pi")
	script := `#!/bin/sh
IFS= read -r clear
clear_id=$(printf '%s\n' "$clear" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
printf '{"type":"response","id":"%s","command":"clear_queue","success":true}\n' "$clear_id"
IFS= read -r prompt
prompt_id=$(printf '%s\n' "$prompt" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
printf '%s\n' '{"type":"agent_start"}'
printf '%s\n' '{"type":"turn_start"}'
printf '{"type":"response","id":"%s","command":"prompt","success":true}\n' "$prompt_id"
printf '%s\n' '{"type":"agent_end"}'
`
	writeTestExecutable(t, fakePath, []byte(script))
	sessionPath := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(sessionPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write session: %v", err)
	}
	backend, err := New("pi", Config{ExecutablePath: fakePath, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new pi backend: %v", err)
	}
	session, err := backend.Execute(t.Context(), "hello", ExecOptions{InteractionMode: "chat", ResumeSessionID: sessionPath, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	for range session.Messages {
	}
	result, ok := <-session.Result
	if !ok {
		t.Fatal("result channel closed without a result")
	}
	if result.Status != "failed" || !strings.Contains(result.Error, "ended before agent_settled") {
		t.Fatalf("result = %+v, want failed settlement boundary", result)
	}
	if session.TerminalObserved() {
		t.Fatal("EOF before agent_settled exposed a terminal boundary")
	}
}

func TestPiRequireNativeResumeRejectsMissingSourceBeforeLaunch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	launched := filepath.Join(dir, "launched")
	fakePath := filepath.Join(dir, "pi")
	writeTestExecutable(t, fakePath, []byte("#!/bin/sh\nprintf launched > "+launched+"\n"))
	backend, err := New("pi", Config{ExecutablePath: fakePath, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new pi backend: %v", err)
	}
	_, err = backend.Execute(t.Context(), "hello", ExecOptions{
		InteractionMode: "chat", ResumePolicy: "require_native", ResumeSessionID: filepath.Join(dir, "missing.jsonl"),
	})
	if err == nil || !strings.Contains(err.Error(), "resume_unavailable") {
		t.Fatalf("execute error = %v, want strict resume_unavailable", err)
	}
	if _, err := os.Stat(launched); !os.IsNotExist(err) {
		t.Fatalf("Pi was launched for a missing strict resume source: %v", err)
	}
}

func TestPiChatRPCConcurrentCancellationDuringTeardown(t *testing.T) {
	for _, settled := range []bool{false, true} {
		name := "context_cancelled"
		if settled {
			name = "settled_before_abort_ack"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			fakePath := filepath.Join(dir, "pi")
			commandsPath := filepath.Join(dir, "commands")
			script := `#!/bin/sh
IFS= read -r clear
clear_id=$(printf '%s\n' "$clear" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
printf '{"type":"response","id":"%s","command":"clear_queue","success":true}\n' "$clear_id"
IFS= read -r prompt
prompt_id=$(printf '%s\n' "$prompt" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
printf '{"type":"response","id":"%s","command":"prompt","success":true}\n' "$prompt_id"
printf '%s\n' '{"type":"turn_start"}'
printf '%s\n' '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"ready"}}'
while IFS= read -r command; do
  printf '%s\n' "$command" >> '__COMMANDS__'
  id=$(printf '%s\n' "$command" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  case "$command" in
    *'"type":"clear_queue"'*)
      printf '{"type":"response","id":"%s","command":"clear_queue","success":true}\n' "$id" ;;
    *'"type":"abort"'*)
      __ABORT_RESPONSE__
      # Keep the transport open until the adapter tears down the process tree.
      while :; do sleep 1; done ;;
    *) exit 65 ;;
  esac
done
`
			abortResponse := `printf '{"type":"response","id":"%s","command":"abort","success":true}\n' "$id"`
			if settled {
				abortResponse = `printf '%s\n' '{"type":"agent_settled"}'`
			}
			script = strings.ReplaceAll(script, "__COMMANDS__", commandsPath)
			script = strings.ReplaceAll(script, "__ABORT_RESPONSE__", abortResponse)
			writeTestExecutable(t, fakePath, []byte(script))
			sessionPath := filepath.Join(dir, "session.jsonl")
			if err := os.WriteFile(sessionPath, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			backend, err := New("pi", Config{ExecutablePath: fakePath, Logger: slog.Default()})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			session, err := backend.Execute(ctx, "hello", ExecOptions{InteractionMode: "chat", ResumeSessionID: sessionPath, Timeout: 5 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			ready := false
			for message := range session.Messages {
				if message.Type == MessageText && message.Content == "ready" {
					ready = true
					break
				}
			}
			if !ready {
				t.Fatal("fake Pi did not become ready for cancellation")
			}
			callCtx, stopCalls := context.WithTimeout(t.Context(), 2*time.Second)
			defer stopCalls()
			start := make(chan struct{})
			done := make(chan error, 2)
			for range 2 {
				go func() {
					<-start
					done <- session.CancelPendingInputs(callCtx)
				}()
			}
			close(start)
			errors := 0
			for range 2 {
				select {
				case err := <-done:
					if err != nil {
						errors++
					}
				case <-callCtx.Done():
					t.Fatal("concurrent cancellation did not finish")
				}
			}
			wantErrors, wantStatus := 0, "aborted"
			if settled {
				wantErrors, wantStatus = 1, "completed"
			}
			if errors != wantErrors {
				t.Fatalf("cancellation errors = %d, want %d", errors, wantErrors)
			}
			cancel()
			select {
			case result := <-session.Result:
				if result.Status != wantStatus || (settled && result.Error != "") {
					t.Fatalf("result = %+v, want %s", result, wantStatus)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Pi cancellation teardown did not finish")
			}
			if session.TerminalObserved() != settled || session.ControlState().Active {
				t.Fatal("incorrect terminal observation or admission remained active")
			}
			commands, err := os.ReadFile(commandsPath)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(commands)), "\n")
			if len(lines) != 2 || !strings.Contains(lines[0], `"type":"clear_queue"`) || !strings.Contains(lines[1], `"type":"abort"`) {
				t.Fatalf("want exactly clear_queue then abort, got %s", commands)
			}
		})
	}
}
