package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A real pipe holds a human response inside Write while stop races it. Count
// decoded native frames so the test detects double replies even without -race.
func TestInteractionStopDoesNotWriteSecondNativeReply(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	controller := newInteractionController(func(Message) bool { return true }, nil)
	controller.setState(ControlState{TurnID: "turn", Active: true})
	entered := make(chan struct{})
	controller.present(InteractionRequest{ID: "interaction", TurnID: "turn", Kind: "approval"},
		func(context.Context, InteractionResponse) (InputDelivery, error) {
			close(entered)
			err := writeClaudeInteractionResponse(writer, "native-id", nil, false)
			return InputDelivery{State: "unknown"}, err
		}, func(context.Context) error {
			return writeClaudeInteractionResponse(writer, "native-id", nil, true)
		})
	responded := make(chan struct{})
	go func() {
		defer close(responded)
		_, _ = controller.respondToInteraction(context.Background(), InteractionResponse{ID: "answer", InteractionID: "interaction", ExpectedTurnID: "turn", ChoiceID: "allow_once"})
	}()
	<-entered
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = controller.cancelPendingInputs(context.Background())
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("stop waited behind response write")
	}
	var writes atomic.Int32
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			var frame struct {
				Response struct {
					RequestID string `json:"request_id"`
				} `json:"response"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil || frame.Response.RequestID != "native-id" {
				t.Errorf("invalid native frame: %s", scanner.Text())
			}
			writes.Add(1)
		}
	}()
	select {
	case <-responded:
	case <-time.After(time.Second):
		t.Fatal("response did not finish")
	}
	writer.Close()
	<-drained
	if got := writes.Load(); got != 1 {
		t.Fatalf("native reply count = %d, want 1", got)
	}
}

func TestClaudeMalformedDenialQueueFailsClosedWithoutBlockingReader(t *testing.T) {
	reader, pipe := io.Pipe()
	defer reader.Close()
	defer pipe.Close()
	controller := newInteractionController(func(Message) bool { return true }, nil)
	controller.setState(ControlState{TurnID: "turn", Active: true})
	writer := &claudeControlWriter{writer: pipe}
	failed := make(chan struct{})
	writer.startDenials(func() {
		controller.failClosedForControlOverflow()
		pipe.Close()
		close(failed)
	})
	defer writer.stopDenials()
	completed := make(chan struct{})
	go func() {
		defer close(completed)
		for i := 0; i < 1000; i++ {
			(&claudeBackend{}).handleInteractiveControlRequest(claudeSDKMessage{RequestID: "malformed", Request: json.RawMessage(`{"input":`)}, writer, controller, "turn")
		}
	}()
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("malformed frame blocked reader")
	}
	select {
	case <-failed:
	case <-time.After(time.Second):
		t.Fatal("overflow did not terminate transport")
	}
	if controller.controlState().Active {
		t.Fatal("overflow left input admission open")
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = controller.cancelPendingInputs(context.Background())
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("stop blocked")
	}
}

func TestClaudeMalformedDenialWriteErrorClosesAdmission(t *testing.T) {
	reader, pipe := io.Pipe()
	reader.CloseWithError(io.ErrClosedPipe)
	defer pipe.Close()
	controller := newInteractionController(func(Message) bool { return true }, nil)
	controller.setState(ControlState{TurnID: "turn", Active: true})
	writer := &claudeControlWriter{writer: pipe}
	failed := make(chan struct{})
	writer.startDenials(func() {
		controller.failClosedForControlOverflow()
		pipe.Close()
		close(failed)
	})
	defer writer.stopDenials()
	(&claudeBackend{}).handleInteractiveControlRequest(claudeSDKMessage{RequestID: "bad", Request: json.RawMessage(strings.Repeat("{", 2))}, writer, controller, "turn")
	select {
	case <-failed:
	case <-time.After(time.Second):
		t.Fatal("write error was ignored")
	}
	if controller.controlState().Active {
		t.Fatal("write error left admission open")
	}
}

func TestInteractionStopWinsBeforeHumanReplyAdmission(t *testing.T) {
	var native bytes.Buffer
	controller := newInteractionController(func(Message) bool { return true }, nil)
	controller.setState(ControlState{TurnID: "turn", Active: true})
	controller.present(InteractionRequest{ID: "interaction", TurnID: "turn", Kind: "approval"},
		func(context.Context, InteractionResponse) (InputDelivery, error) {
			return InputDelivery{State: "unknown"}, writeClaudeInteractionResponse(&native, "native-id", nil, false)
		}, func(context.Context) error {
			return writeClaudeInteractionResponse(&native, "native-id", nil, true)
		})
	reserved := make(chan struct{})
	release := make(chan struct{})
	var emissions atomic.Int32
	controller.emit = func(Message) bool {
		if emissions.Add(1) == 1 {
			close(reserved)
			<-release
		}
		return true
	}
	completed := make(chan InputDelivery, 1)
	go func() {
		delivery, _ := controller.respondToInteraction(context.Background(), InteractionResponse{ID: "answer", InteractionID: "interaction", ExpectedTurnID: "turn", ChoiceID: "allow_once"})
		completed <- delivery
	}()
	<-reserved
	if err := controller.cancelPendingInputs(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case delivery := <-completed:
		if delivery.State != "rejected" {
			t.Fatalf("delivery = %+v", delivery)
		}
	case <-time.After(time.Second):
		t.Fatal("response admission blocked")
	}
	if strings.Count(native.String(), "\n") != 1 || !strings.Contains(native.String(), `"behavior":"deny"`) {
		t.Fatalf("want exactly one native denial, got %s", native.String())
	}
}
