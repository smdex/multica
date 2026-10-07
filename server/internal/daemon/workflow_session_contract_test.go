//go:build unix

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/agent"
)

// TestWorkflowRunSteersTheBoundSessionWithReplaySafety exercises production
// Session callbacks against one test-created provider process, with both a
// definite acknowledgement and an acknowledgement lost after input delivery.
func TestWorkflowRunSteersTheBoundSessionWithReplaySafety(t *testing.T) {
	t.Parallel()
	for _, acknowledged := range []bool{true, false} {
		t.Run(fmt.Sprintf("acknowledged=%t", acknowledged), func(t *testing.T) {
			t.Parallel()
			testWorkflowBoundSession(t, acknowledged)
		})
	}
}

func testWorkflowBoundSession(t *testing.T, acknowledged bool) {
	t.Helper()
	root := t.TempDir()
	requestLog := filepath.Join(root, "steer-requests.jsonl")
	startLog := filepath.Join(root, "process-starts.txt")
	nonce := fmt.Sprintf("start-%d", time.Now().UnixNano())
	fakePath := filepath.Join(root, "codex")
	script := "#!/bin/sh\n" +
		`if [ "$1" = "--version" ]; then echo "codex-cli 0.0.0-test"; exit 0; fi` + "\n" +
		`printf '%s %s\n' "$$" "$MULTICA_TEST_START_NONCE" >> "$MULTICA_TEST_START_LOG"` + "\n" +
		`IFS= read -r initialize || exit 10` + "\n" +
		`echo '{"jsonrpc":"2.0","id":1,"result":{}}'` + "\n" +
		`IFS= read -r initialized || exit 11` + "\n" +
		`IFS= read -r thread_start || exit 12` + "\n" +
		`echo '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-contract"}}}'` + "\n" +
		`IFS= read -r turn_start || exit 13` + "\n" +
		`echo '{"jsonrpc":"2.0","id":3,"result":{}}'` + "\n" +
		`echo '{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"thread-contract","turn":{"id":"turn-contract"}}}'` + "\n" +
		`IFS= read -r steer || exit 14` + "\n" +
		`printf '%s %s %s\n' "$$" "$MULTICA_TEST_START_NONCE" "$steer" >> "$MULTICA_TEST_REQUEST_LOG"` + "\n" +
		`echo '{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"thread-contract","turnId":"turn-contract","itemId":"output-contract","delta":"same-worker-output"}}'` + "\n"
	if acknowledged {
		script += `echo '{"jsonrpc":"2.0","id":4,"result":{"turnId":"turn-contract"}}'` + "\n"
	}
	script += `while IFS= read -r _; do :; done`
	if err := os.WriteFile(fakePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	backend, err := agent.New("codex", agent.Config{
		ExecutablePath: fakePath,
		Logger:         slog.Default(),
		Env: map[string]string{
			"MULTICA_TEST_START_NONCE": nonce,
			"MULTICA_TEST_START_LOG":   startLog,
			"MULTICA_TEST_REQUEST_LOG": requestLog,
		},
	})
	if err != nil {
		t.Fatalf("create fake Codex backend: %v", err)
	}
	sessionCtx, cancelSession := context.WithCancel(context.Background())
	defer cancelSession()
	session, err := backend.Execute(sessionCtx, "controlled no-repository worker", agent.ExecOptions{
		InteractionMode: "chat",
		Cwd:             root,
		Timeout:         time.Minute,
	})
	if err != nil {
		t.Fatalf("start test-created provider process: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for {
		state := session.ControlState()
		if state.Active && state.TurnID == "turn-contract" && state.CanSteer {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("provider turn did not become steerable: %+v", state)
		case <-time.After(10 * time.Millisecond):
		}
	}

	run := &workflowRun{taskID: "task-contract", runtimeID: "runtime-contract", runID: "incarnation-contract"}
	run.bind(session)
	commandID := "input-contract-1"
	timeout := 100 * time.Millisecond
	wantState := "unknown"
	if acknowledged {
		timeout = 5 * time.Second
		wantState = "accepted"
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	first, firstErr := run.steer(ctx, commandID, "turn-contract", "continue this same worker")
	cancel()
	if first.State != wantState || (firstErr == nil) != acknowledged {
		t.Fatalf("first steer delivery = %+v, err=%v, want %s (acknowledged=%t)", first, firstErr, wantState, acknowledged)
	}

	replayCtx, cancelReplay := context.WithTimeout(context.Background(), time.Second)
	second, secondErr := run.steer(replayCtx, commandID, "turn-contract", "continue this same worker")
	cancelReplay()
	if secondErr != nil || second.State != wantState {
		t.Fatalf("replayed steer delivery = %+v, err=%v, want memoized %s", second, secondErr, wantState)
	}
	outputDeadline := time.After(5 * time.Second)
readOutput:
	for {
		select {
		case message, open := <-session.Messages:
			if !open {
				t.Fatal("session ended before same-worker output")
			}
			if message.Content == "same-worker-output" {
				break readOutput
			}
		case <-outputDeadline:
			t.Fatal("existing session did not capture output after input delivery")
		}
	}

	starts, err := os.ReadFile(startLog)
	if err != nil {
		t.Fatalf("read worker start evidence: %v", err)
	}
	startLines := strings.Fields(string(starts))
	if len(startLines) != 2 || startLines[1] != nonce || startLines[0] == "" {
		t.Fatalf("process starts = %q, want exactly one PID and nonce %q", starts, nonce)
	}
	requests, err := os.ReadFile(requestLog)
	if err != nil {
		t.Fatalf("read worker input evidence: %v", err)
	}
	requestLines := strings.Split(strings.TrimSpace(string(requests)), "\n")
	if len(requestLines) != 1 {
		t.Fatalf("provider input writes = %q, want exactly one write", requests)
	}
	requestEvidence := strings.SplitN(requestLines[0], " ", 3)
	if len(requestEvidence) != 3 || requestEvidence[0] != startLines[0] || requestEvidence[1] != nonce || !strings.Contains(requestEvidence[2], `"clientUserMessageId":"`+commandID+`"`) || !strings.Contains(requestEvidence[2], `"text":"continue this same worker"`) {
		t.Fatalf("provider input writes = %q, want matching command/text from PID %s nonce %s", requests, startLines[0], nonce)
	}

	cancelSession()
	select {
	case <-session.Result:
	case <-time.After(5 * time.Second):
		t.Fatal("test-created provider process did not stop after session cancellation")
	}
}
