package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestInteractionControllerReservesCommandAcrossControlAndStop(t *testing.T) {
	t.Parallel()
	var writes atomic.Int32
	entered := make(chan struct{})
	releaseAck := make(chan struct{})
	controller := newInteractionController(func(Message) bool { return true }, nil)
	controller.setState(ControlState{TurnID: "turn-1", Active: true, CanSteer: true})
	controller.steer = func(context.Context, SteerRequest) (InputDelivery, error) {
		writes.Add(1)
		// Model a scanner notification arriving before the provider ack. This
		// must not wait on the command's reservation or on a later stop.
		controller.setState(ControlState{TurnID: "turn-1", Active: true, CanSteer: true})
		close(entered)
		<-releaseAck
		return InputDelivery{State: "accepted"}, nil
	}

	request := SteerRequest{ID: "command-1", ExpectedTurnID: "turn-1", Content: "continue"}
	firstResult := make(chan InputDelivery, 1)
	go func() {
		delivery, err := controller.steerTurn(context.Background(), request)
		if err != nil {
			t.Errorf("first steering: %v", err)
			return
		}
		firstResult <- delivery
	}()
	<-entered

	stopDone := make(chan struct{})
	go func() {
		if err := controller.cancelPendingInputs(context.Background()); err != nil {
			t.Errorf("cancel pending inputs: %v", err)
		}
		close(stopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("stop blocked behind provider acknowledgement")
	}

	duplicateResult := make(chan InputDelivery, 1)
	go func() {
		delivery, err := controller.steerTurn(context.Background(), request)
		if err != nil {
			t.Errorf("duplicate steering: %v", err)
			return
		}
		duplicateResult <- delivery
	}()
	close(releaseAck)
	for _, result := range []<-chan InputDelivery{firstResult, duplicateResult} {
		select {
		case delivery := <-result:
			if delivery.State != "accepted" {
				t.Fatalf("delivery = %+v, want accepted", delivery)
			}
		case <-time.After(time.Second):
			t.Fatal("reserved command did not settle")
		}
	}
	if got := writes.Load(); got != 1 {
		t.Fatalf("provider writes = %d, want exactly one", got)
	}
}

func TestInteractionControllerFailsClosedWhenControlQueueOverflows(t *testing.T) {
	t.Parallel()
	var available atomic.Bool
	available.Store(true)
	cancelled := make(chan struct{}, 1)
	controller := newInteractionController(func(Message) bool { return available.Load() }, nil)
	controller.setState(ControlState{TurnID: "turn-1", Active: true})
	available.Store(false)
	controller.present(InteractionRequest{ID: "ask-1", TurnID: "turn-1", Kind: "approval"},
		func(context.Context, InteractionResponse) (InputDelivery, error) { return InputDelivery{}, nil },
		func(context.Context) error { cancelled <- struct{}{}; return nil },
	)
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("queue overflow did not fail closed and cancel the provider request")
	}
	if state := controller.controlState(); state.Active || state.CanApprove || state.CanAnswer || state.CanSteer {
		t.Fatalf("control state after overflow = %+v, want closed", state)
	}
}

func TestInteractionControllerRestoresOnlyDefinitivelyRejectedResponse(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	controller := newInteractionController(func(Message) bool { return true }, nil)
	controller.setState(ControlState{TurnID: "turn-1", Active: true})
	request := InteractionRequest{ID: "approval-1", TurnID: "turn-1", Kind: "approval"}
	controller.present(request, func(context.Context, InteractionResponse) (InputDelivery, error) {
		if attempts.Add(1) == 1 {
			return InputDelivery{State: "rejected", Code: "provider_rejected"}, nil
		}
		return InputDelivery{State: "accepted"}, nil
	}, nil)
	first, err := controller.respondToInteraction(context.Background(), InteractionResponse{ID: "answer-1", InteractionID: request.ID, ExpectedTurnID: "turn-1", ChoiceID: "allow_once"})
	if err != nil || first.State != "rejected" {
		t.Fatalf("first response = %+v, %v; want definitive rejection", first, err)
	}
	if state := controller.controlState(); !state.CanApprove || !state.CanAnswer {
		t.Fatalf("rejected response did not restore pending interaction: %+v", state)
	}
	second, err := controller.respondToInteraction(context.Background(), InteractionResponse{ID: "answer-2", InteractionID: request.ID, ExpectedTurnID: "turn-1", ChoiceID: "allow_once"})
	if err != nil || second.State != "accepted" {
		t.Fatalf("retried response = %+v, %v; want accepted", second, err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("provider response attempts = %d, want 2", got)
	}
}

func TestInteractionControllerDoesNotRestoreUnknownResponse(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	controller := newInteractionController(func(Message) bool { return true }, nil)
	controller.setState(ControlState{TurnID: "turn-1", Active: true})
	request := InteractionRequest{ID: "approval-1", TurnID: "turn-1", Kind: "approval"}
	controller.present(request, func(context.Context, InteractionResponse) (InputDelivery, error) {
		attempts.Add(1)
		return InputDelivery{State: "unknown", Code: "delivery_unknown"}, nil
	}, nil)
	first, err := controller.respondToInteraction(context.Background(), InteractionResponse{ID: "answer-1", InteractionID: request.ID, ExpectedTurnID: "turn-1", ChoiceID: "allow_once"})
	if err != nil || first.State != "unknown" {
		t.Fatalf("first response = %+v, %v; want unknown", first, err)
	}
	second, err := controller.respondToInteraction(context.Background(), InteractionResponse{ID: "answer-2", InteractionID: request.ID, ExpectedTurnID: "turn-1", ChoiceID: "allow_once"})
	if err != nil || second.State != "rejected" || second.Code != "not_found" {
		t.Fatalf("second response = %+v, %v; want terminal not_found", second, err)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("provider response attempts = %d, want no retry after unknown delivery", got)
	}
}

func TestInteractionControllerCancelsStaleProviderRequest(t *testing.T) {
	t.Parallel()
	cancelled := make(chan struct{}, 1)
	controller := newInteractionController(func(Message) bool { return true }, nil)
	controller.setState(ControlState{TurnID: "turn-live", Active: true})
	controller.present(InteractionRequest{ID: "stale", TurnID: "turn-old", Kind: "approval"}, nil, func(context.Context) error {
		cancelled <- struct{}{}
		return nil
	})
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("stale provider request was not denied")
	}
}

func TestControlMessagePublisherRejectsSaturatedQueue(t *testing.T) {
	t.Parallel()
	publisher := &controlMessagePublisher{queue: make(chan Message, 1)}
	if !publisher.publish(Message{Type: MessageControlState}) {
		t.Fatal("first control message was rejected")
	}
	if publisher.publish(Message{Type: MessageInteraction}) {
		t.Fatal("saturated control queue accepted a message that would be silently lost")
	}
}

func TestValidateInteractionResponseRejectsMalformedStructuredAnswers(t *testing.T) {
	t.Parallel()
	state := ControlState{TurnID: "turn-1", Active: true}
	request := InteractionRequest{
		ID: "interaction-1", TurnID: "turn-1", Kind: "question",
		Questions: []InteractionQuestion{
			{ID: "pick", Prompt: "Pick", Options: []InteractionOption{{ID: "one", Label: "One"}, {ID: "two", Label: "Two"}}},
			{ID: "secret", Prompt: "Secret", AllowText: true, Secret: true},
		},
	}
	base := InteractionResponse{ID: "command-1", InteractionID: request.ID, ExpectedTurnID: "turn-1"}
	cases := []struct {
		name     string
		response InteractionResponse
	}{
		{"missing answer", base},
		{"unknown question", withAnswers(base, []InteractionAnswer{{QuestionID: "other", OptionIDs: []string{"one"}}, {QuestionID: "secret", Text: "s"}})},
		{"duplicate question", withAnswers(base, []InteractionAnswer{{QuestionID: "pick", OptionIDs: []string{"one"}}, {QuestionID: "pick", OptionIDs: []string{"two"}}})},
		{"unknown option", withAnswers(base, []InteractionAnswer{{QuestionID: "pick", OptionIDs: []string{"other"}}, {QuestionID: "secret", Text: "s"}})},
		{"multiple where single", withAnswers(base, []InteractionAnswer{{QuestionID: "pick", OptionIDs: []string{"one", "two"}}, {QuestionID: "secret", Text: "s"}})},
		{"free text forbidden", withAnswers(base, []InteractionAnswer{{QuestionID: "pick", Text: "other"}, {QuestionID: "secret", Text: "s"}})},
		{"secret option", withAnswers(base, []InteractionAnswer{{QuestionID: "pick", OptionIDs: []string{"one"}}, {QuestionID: "secret", OptionIDs: []string{"one"}}})},
		{"cancelled mixed answers", func() InteractionResponse {
			response := base
			response.Cancelled = true
			response.Answers = []InteractionAnswer{{QuestionID: "pick", OptionIDs: []string{"one"}}}
			return response
		}()},
		{"cancelled mixed choice", func() InteractionResponse {
			response := base
			response.Cancelled = true
			response.ChoiceID = "allow_once"
			return response
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateInteractionResponse(state, false, true, request, tc.response); err == nil {
				t.Fatal("malformed structured response was accepted")
			}
		})
	}
	valid := withAnswers(base, []InteractionAnswer{{QuestionID: "pick", OptionIDs: []string{"one"}}, {QuestionID: "secret", Text: "value"}})
	if err := validateInteractionResponse(state, false, true, request, valid); err != nil {
		t.Fatalf("valid structured response rejected: %v", err)
	}
}

func withAnswers(response InteractionResponse, answers []InteractionAnswer) InteractionResponse {
	response.Answers = answers
	return response
}

func TestInteractionCapabilitiesAreStaticAndProviderSpecific(t *testing.T) {
	t.Parallel()
	cases := []struct {
		backend any
		want    InteractionCapabilities
	}{
		{&codexBackend{}, InteractionCapabilities{Steer: true, Approvals: true, Questions: true}},
		{&piBackend{}, InteractionCapabilities{Steer: true, Approvals: true, Questions: true}},
		{&claudeBackend{}, InteractionCapabilities{Approvals: true, Questions: true}},
	}
	for _, tc := range cases {
		provider, ok := tc.backend.(InteractionCapabilityProvider)
		if !ok {
			t.Fatalf("backend %T does not expose the non-launching capability interface", tc.backend)
		}
		if got := provider.InteractionCapabilities(); got != tc.want {
			t.Fatalf("capabilities for %T = %+v, want %+v", tc.backend, got, tc.want)
		}
	}
	if got := (&piBackend{providerLabel: "omp"}).InteractionCapabilities(); got != (InteractionCapabilities{}) {
		t.Fatalf("OMP-labelled Pi executor capabilities = %+v, want none", got)
	}
}

func TestResolveBackendAdvertisesIdleInteractiveCapabilities(t *testing.T) {
	t.Parallel()
	cases := []struct {
		provider string
		want     InteractionCapabilities
	}{
		{"codex", InteractionCapabilities{Steer: true, Approvals: true, Questions: true}},
		{"pi", InteractionCapabilities{Steer: true, Approvals: true, Questions: true}},
		{"claude", InteractionCapabilities{Approvals: true, Questions: true}},
	}
	for _, tc := range cases {
		backend, err := ResolveBackend(tc.provider, Config{})
		if err != nil {
			t.Fatalf("ResolveBackend(%q): %v", tc.provider, err)
		}
		provider, ok := backend.(InteractionCapabilityProvider)
		if !ok {
			t.Fatalf("ResolveBackend(%q) returned %T without idle interaction capabilities", tc.provider, backend)
		}
		if got := provider.InteractionCapabilities(); got != tc.want {
			t.Fatalf("ResolveBackend(%q) capabilities = %+v, want %+v", tc.provider, got, tc.want)
		}
	}
}
