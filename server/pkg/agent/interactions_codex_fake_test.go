//go:build unix

package agent

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

func TestCodexChatFakeAppServerApprovalLifecycle(t *testing.T) {
	t.Parallel()
	fakePath := writeFakeCodexAppServer(t, ""+
		`read initialize`+"\n"+
		`echo '{"jsonrpc":"2.0","id":1,"result":{}}'`+"\n"+
		`read initialized`+"\n"+
		`read thread_start`+"\n"+
		`case "$thread_start" in *'"approvalPolicy":"on-request"'*) ;; *) echo "chat approval policy missing" >&2; exit 71 ;; esac`+"\n"+
		`echo '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-chat"}}}'`+"\n"+
		`read turn_start`+"\n"+
		`echo '{"jsonrpc":"2.0","id":3,"result":{}}'`+"\n"+
		`echo '{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"thread-chat","turn":{"id":"turn-chat"}}}'`+"\n"+
		`read supplement`+"\n"+
		`case "$supplement" in *'"expectedTurnId":"turn-chat"'*'"text":"additional task context"'*) ;; *) echo "unexpected supplement: $supplement" >&2; exit 73 ;; esac`+"\n"+
		`echo '{"jsonrpc":"2.0","id":4,"result":{"turnId":"turn-chat"}}'`+"\n"+
		`read steer`+"\n"+
		`case "$steer" in *'"clientUserMessageId":"steer-1"'*'"expectedTurnId":"turn-chat"'*'"text":"native chat steering"'*) ;; *) echo "unexpected steering: $steer" >&2; exit 74 ;; esac`+"\n"+
		`echo '{"jsonrpc":"2.0","id":5,"result":{"turnId":"turn-chat"}}'`+"\n"+
		`echo '{"jsonrpc":"2.0","id":90,"method":"item/commandExecution/requestApproval","params":{"threadId":"thread-chat","turnId":"turn-chat","itemId":"cmd-1","command":"pwd"}}'`+"\n"+
		`read approval`+"\n"+
		`case "$approval" in *'"id":90'*'"decision":"accept"'*) ;; *) echo "unexpected approval: $approval" >&2; exit 72 ;; esac`+"\n"+
		`echo '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thread-chat","turn":{"id":"turn-chat","status":"completed"}}}'`+"\n")
	backend, err := New("codex", Config{ExecutablePath: fakePath, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new codex backend: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "run", ExecOptions{InteractionMode: "chat", Timeout: 8 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if session.Supplement == nil || session.SupplementReady == nil || session.ControlState == nil || session.Steer == nil || session.RespondToInteraction == nil || session.CancelPendingInputs == nil {
		t.Fatal("Codex supplement and native chat interfaces must coexist")
	}
	for message := range session.Messages {
		if message.ControlState != nil && message.ControlState.Active {
			break
		}
	}
	if !session.SupplementReady() {
		t.Fatal("foreground turn did not enable supplements")
	}
	if err := session.Supplement(ctx, "additional task context"); err != nil {
		t.Fatalf("supplement: %v", err)
	}
	steerDelivery, err := session.Steer(ctx, SteerRequest{ID: "steer-1", ExpectedTurnID: "turn-chat", Content: "native chat steering"})
	if err != nil || steerDelivery.State != "accepted" {
		t.Fatalf("steer delivery = %+v, %v", steerDelivery, err)
	}
	var interaction *InteractionRequest
	for message := range session.Messages {
		if message.Interaction != nil {
			interaction = message.Interaction
			break
		}
	}
	if interaction == nil || interaction.Kind != "approval" || interaction.TurnID != "turn-chat" {
		t.Fatalf("interaction = %+v", interaction)
	}
	delivery, err := session.RespondToInteraction(ctx, InteractionResponse{
		ID: "approval-1", InteractionID: interaction.ID, ExpectedTurnID: "turn-chat", ChoiceID: "allow_once",
	})
	if err != nil || delivery.State != "unknown" {
		t.Fatalf("approval delivery = %+v, %v", delivery, err)
	}
	for range session.Messages {
	}
	select {
	case result, ok := <-session.Result:
		if !ok || result.Status != "completed" || result.SessionID != "thread-chat" {
			t.Fatalf("result = %+v, open=%v", result, ok)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Codex fake app-server did not complete after approval")
	}
	if session.SupplementReady() || session.ControlState().Active {
		t.Fatal("completed Codex turn still accepts live input")
	}
}
