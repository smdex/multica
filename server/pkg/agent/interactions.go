package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxInteractionTextBytes = 32 * 1024

// InteractionCapabilities describes chat controls that an adapter implements
// without starting an agent process. Daemons use this optional interface only
// to make a chat launchable; a live Session.ControlState remains the authority
// for an individual foreground turn.
type InteractionCapabilities struct {
	Steer     bool
	Approvals bool
	Questions bool
}

// InteractionCapabilityProvider is intentionally provider-neutral. Callers
// must type-assert it rather than inferring controls from a runtime name.
type InteractionCapabilityProvider interface {
	InteractionCapabilities() InteractionCapabilities
}

// ControlState describes the provider's current foreground turn and the
// interactive operations that are safe for it.
type ControlState struct {
	TurnID     string
	Active     bool
	CanSteer   bool
	CanApprove bool
	CanAnswer  bool
}

// SteerRequest is a deduplicated request to add text to the active turn.
type SteerRequest struct {
	ID             string
	ExpectedTurnID string
	Content        string
}

// InputDelivery distinguishes provider acknowledgement from an uncertain
// write. Unknown is terminal for a command ID: retrying it could duplicate
// input at the provider.
type InputDelivery struct {
	State string
	Code  string
}

// InteractionRequest is a normalized provider-native approval or question.
type InteractionRequest struct {
	ID          string
	TurnID      string
	Kind        string
	Title       string
	Description string
	Tool        string
	Input       map[string]any
	Choices     []InteractionChoice
	Questions   []InteractionQuestion
	ExpiresAt   time.Time
}

type InteractionChoice struct {
	ID    string
	Label string
}

type InteractionQuestion struct {
	ID        string
	Prompt    string
	Options   []InteractionOption
	Multiple  bool
	AllowText bool
	Secret    bool
}

type InteractionOption struct{ ID, Label, Description string }

type InteractionAnswer struct {
	QuestionID string
	OptionIDs  []string
	Text       string
}

// InteractionResponse resolves one provider interaction. ChoiceID is used by
// approvals; Answers is used by questions.
type InteractionResponse struct {
	ID             string
	InteractionID  string
	ExpectedTurnID string
	ChoiceID       string
	Answers        []InteractionAnswer
	Cancelled      bool
}

// Copies in pending, resolving, and cancellation snapshots share one native
// reply reservation. Only definitive non-delivery can reopen it.
type interactionPending struct {
	replyClaimed *atomic.Bool
	request      InteractionRequest
	respond      func(context.Context, InteractionResponse) (InputDelivery, error)
	cancel       func(context.Context) error
}

// sessionControlRelay keeps the public callbacks bound to the active provider
// attempt. Codex can retry its startup transport before a foreground turn
// exists; callers keep one Session while the relay moves to that retry.
type sessionControlRelay struct {
	mu      sync.RWMutex
	session *Session
}

func newSessionControlRelay(session *Session) *sessionControlRelay {
	return &sessionControlRelay{session: session}
}

func (r *sessionControlRelay) set(session *Session) {
	r.mu.Lock()
	r.session = session
	r.mu.Unlock()
}

func (r *sessionControlRelay) current() *Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.session
}

func (r *sessionControlRelay) supported() bool {
	session := r.current()
	return session != nil && session.ControlState != nil
}

func (r *sessionControlRelay) controlState() ControlState {
	if session := r.current(); session != nil && session.ControlState != nil {
		return session.ControlState()
	}
	return ControlState{}
}

func (r *sessionControlRelay) steer(ctx context.Context, request SteerRequest) (InputDelivery, error) {
	if session := r.current(); session != nil && session.Steer != nil {
		return session.Steer(ctx, request)
	}
	return InputDelivery{State: "rejected", Code: "unsupported"}, nil
}

func (r *sessionControlRelay) respond(ctx context.Context, response InteractionResponse) (InputDelivery, error) {
	if session := r.current(); session != nil && session.RespondToInteraction != nil {
		return session.RespondToInteraction(ctx, response)
	}
	return InputDelivery{State: "rejected", Code: "unsupported"}, nil
}

func (r *sessionControlRelay) cancel(ctx context.Context) error {
	if session := r.current(); session != nil && session.CancelPendingInputs != nil {
		return session.CancelPendingInputs(ctx)
	}
	return nil
}

type inputDeliveryRecord struct {
	delivery InputDelivery
	done     chan struct{}
}

// interactionController serializes admission without holding its mutex across
// provider I/O. A command reserves its ID as unknown while locked, performs the
// write/ack wait unlocked, and then settles that same record. Cancellation can
// close admission while an acknowledgement is in flight; it never allows that
// command ID to be written a second time.
type interactionController struct {
	mu             sync.Mutex
	state          ControlState
	closed         bool
	pending        map[string]interactionPending
	resolving      map[string]interactionPending
	deliveries     map[string]*inputDeliveryRecord
	emit           func(Message) bool
	steer          func(context.Context, SteerRequest) (InputDelivery, error)
	cancelSteering func(context.Context) error
}

func newInteractionController(emit func(Message) bool, steer func(context.Context, SteerRequest) (InputDelivery, error)) *interactionController {
	return &interactionController{
		pending:    make(map[string]interactionPending),
		resolving:  make(map[string]interactionPending),
		deliveries: make(map[string]*inputDeliveryRecord),
		emit:       emit,
		steer:      steer,
	}
}

func (c *interactionController) controlState() ControlState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *interactionController) setState(state ControlState) {
	c.mu.Lock()
	if c.closed {
		state = ControlState{}
	}
	changed := c.state != state
	c.state = state
	emit := c.emit
	c.mu.Unlock()
	if changed && emit != nil {
		stateCopy := state
		if !emit(Message{Type: MessageControlState, ControlState: &stateCopy}) {
			c.failClosedForControlOverflow()
		}
	}
}

func (c *interactionController) present(request InteractionRequest, respond func(context.Context, InteractionResponse) (InputDelivery, error), cancel func(context.Context) error) {
	if request.ID == "" || request.TurnID == "" || (request.Kind != "approval" && request.Kind != "question") {
		cancelInteractionRequest(cancel)
		return
	}
	c.mu.Lock()
	if c.closed || !c.state.Active || c.state.TurnID != request.TurnID {
		c.mu.Unlock()
		cancelInteractionRequest(cancel)
		return
	}
	if _, exists := c.pending[request.ID]; exists {
		c.mu.Unlock()
		cancelInteractionRequest(cancel)
		return
	}
	if _, exists := c.resolving[request.ID]; exists {
		c.mu.Unlock()
		cancelInteractionRequest(cancel)
		return
	}
	c.pending[request.ID] = interactionPending{request: request, respond: respond, cancel: cancel, replyClaimed: &atomic.Bool{}}
	c.state.CanAnswer = true
	c.state.CanApprove = request.Kind == "approval" || c.hasPendingApprovalLocked()
	state := c.state
	emit := c.emit
	c.mu.Unlock()
	if emit != nil {
		requestCopy := request
		published := emit(Message{Type: MessageInteraction, Interaction: &requestCopy})
		stateCopy := state
		published = emit(Message{Type: MessageControlState, ControlState: &stateCopy}) && published
		if !published {
			c.failClosedForControlOverflow()
		}
	}
}

func (c *interactionController) hasPendingApprovalLocked() bool {
	for _, pending := range c.pending {
		if pending.request.Kind == "approval" {
			return true
		}
	}
	return false
}

func (c *interactionController) steerTurn(ctx context.Context, request SteerRequest) (InputDelivery, error) {
	c.mu.Lock()
	if record, exists := c.deliveries[request.ID]; exists {
		c.mu.Unlock()
		return waitForInputDelivery(ctx, record)
	}
	if err := validateSteerRequest(c.state, c.closed, len(c.pending)+len(c.resolving) != 0, request); err != nil {
		delivery := rejectedInputDelivery(err)
		c.rememberDeliveryLocked(request.ID, delivery)
		c.mu.Unlock()
		return delivery, nil
	}
	if c.steer == nil {
		delivery := InputDelivery{State: "rejected", Code: "unsupported"}
		c.rememberDeliveryLocked(request.ID, delivery)
		c.mu.Unlock()
		return delivery, nil
	}
	record := c.reserveDeliveryLocked(request.ID)
	steer := c.steer
	c.mu.Unlock()

	delivery, err := steer(ctx, request)
	if err != nil {
		return c.settleDelivery(record, InputDelivery{State: "unknown", Code: "delivery_unknown"}), err
	}
	return c.settleDelivery(record, delivery), nil
}

func (c *interactionController) respondToInteraction(ctx context.Context, response InteractionResponse) (InputDelivery, error) {
	c.mu.Lock()
	if record, exists := c.deliveries[response.ID]; exists {
		c.mu.Unlock()
		return waitForInputDelivery(ctx, record)
	}
	pending, exists := c.pending[response.InteractionID]
	if err := validateInteractionResponse(c.state, c.closed, exists, pending.request, response); err != nil {
		delivery := rejectedInputDelivery(err)
		c.rememberDeliveryLocked(response.ID, delivery)
		c.mu.Unlock()
		return delivery, nil
	}
	delete(c.pending, response.InteractionID)
	c.resolving[response.InteractionID] = pending
	c.state.CanAnswer = len(c.pending) > 0
	c.state.CanApprove = c.hasPendingApprovalLocked()
	state := c.state
	emit := c.emit
	record := c.reserveDeliveryLocked(response.ID)
	c.mu.Unlock()
	if emit != nil {
		stateCopy := state
		if !emit(Message{Type: MessageControlState, ControlState: &stateCopy}) {
			c.failClosedForControlOverflow()
		}
	}
	c.mu.Lock()
	admitted := !c.closed && pending.replyClaimed.CompareAndSwap(false, true)
	c.mu.Unlock()
	if !admitted {
		return c.settleDelivery(record, InputDelivery{State: "rejected", Code: "cancelled"}), nil
	}
	delivery, err := pending.respond(ctx, response)
	if err != nil {
		return c.finishInteractionDelivery(record, pending, response.InteractionID, InputDelivery{State: "unknown", Code: "delivery_unknown"}), err
	}
	return c.finishInteractionDelivery(record, pending, response.InteractionID, delivery), nil
}

// finishInteractionDelivery removes a settled reservation. A provider that
// definitively rejected the write has not received user input, so restore the
// exact pending request while its foreground turn is still live. Unknown and
// accepted writes are terminal: replaying either could duplicate a response.
func (c *interactionController) finishInteractionDelivery(record *inputDeliveryRecord, pending interactionPending, interactionID string, delivery InputDelivery) InputDelivery {
	if delivery.State == "" {
		delivery = InputDelivery{State: "unknown", Code: "delivery_unknown"}
	}
	c.mu.Lock()
	current, exists := c.resolving[interactionID]
	if exists {
		delete(c.resolving, interactionID)
	}
	restored := false
	cancelRejected := delivery.State == "rejected" && c.closed
	if cancelRejected {
		pending.replyClaimed.Store(false)
	}
	if exists && delivery.State == "rejected" && !c.closed && c.state.Active && c.state.TurnID == pending.request.TurnID && current.request.ID == pending.request.ID {
		pending.replyClaimed.Store(false)
		c.pending[interactionID] = pending
		c.state.CanAnswer = true
		c.state.CanApprove = pending.request.Kind == "approval" || c.hasPendingApprovalLocked()
		restored = true
	}
	state := c.state
	emit := c.emit
	c.mu.Unlock()
	if cancelRejected {
		_ = cancelInteractionSnapshot(context.Background(), []interactionPending{pending}, nil)
	}
	if restored && emit != nil {
		stateCopy := state
		if !emit(Message{Type: MessageControlState, ControlState: &stateCopy}) {
			c.failClosedForControlOverflow()
		}
	}
	return c.settleDelivery(record, delivery)
}

func (c *interactionController) cancelPendingInputs(ctx context.Context) error {
	pending, cancelSteering, state, emit, open := c.closeAdmission()
	if !open {
		return nil
	}
	firstErr := cancelInteractionSnapshot(ctx, pending, cancelSteering)
	if emit != nil {
		stateCopy := state
		_ = emit(Message{Type: MessageControlState, ControlState: &stateCopy})
	}
	return firstErr
}

// closeProviderTurn is used when the provider has already emitted its terminal
// event. There is no live turn to interrupt, so only outstanding interaction
// requests are rejected; sending clear_queue/abort here would wait for an RPC
// acknowledgement after the provider has stopped reading.
func (c *interactionController) closeProviderTurn(ctx context.Context) error {
	pending, _, state, emit, open := c.closeAdmission()
	if !open {
		return nil
	}
	firstErr := cancelInteractionSnapshot(ctx, pending, nil)
	if emit != nil {
		stateCopy := state
		_ = emit(Message{Type: MessageControlState, ControlState: &stateCopy})
	}
	return firstErr
}

func (c *interactionController) closeAdmission() ([]interactionPending, func(context.Context) error, ControlState, func(Message) bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, nil, ControlState{}, nil, false
	}
	c.closed = true
	pending := make([]interactionPending, 0, len(c.pending)+len(c.resolving))
	for _, item := range c.pending {
		pending = append(pending, item)
	}
	for _, item := range c.resolving {
		pending = append(pending, item)
	}
	c.pending = make(map[string]interactionPending)
	c.resolving = make(map[string]interactionPending)
	c.state.Active = false
	c.state.CanSteer = false
	c.state.CanApprove = false
	c.state.CanAnswer = false
	return pending, c.cancelSteering, c.state, c.emit, true
}

func (c *interactionController) failClosedForControlOverflow() {
	pending, cancelSteering, _, _, open := c.closeAdmission()
	if !open {
		return
	}
	go func() {
		_ = cancelInteractionSnapshot(context.Background(), pending, cancelSteering)
	}()
}

func cancelInteractionSnapshot(ctx context.Context, pending []interactionPending, cancelSteering func(context.Context) error) error {
	var firstErr error
	if cancelSteering != nil {
		if err := cancelSteering(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, item := range pending {
		if item.cancel != nil && item.replyClaimed.CompareAndSwap(false, true) {
			if err := item.cancel(ctx); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// Provider readers call present from their scanner goroutine. Rejecting an
// invalid/stale request must still reply fail-closed, but that reply can block
// on provider I/O and therefore cannot hold up the scanner.
func cancelInteractionRequest(cancel func(context.Context) error) {
	if cancel == nil {
		return
	}
	go func() { _ = cancel(context.Background()) }()
}

func (c *interactionController) reserveDeliveryLocked(id string) *inputDeliveryRecord {
	record := &inputDeliveryRecord{
		delivery: InputDelivery{State: "unknown", Code: "delivery_unknown"},
		done:     make(chan struct{}),
	}
	c.deliveries[id] = record
	return record
}

func (c *interactionController) rememberDeliveryLocked(id string, delivery InputDelivery) {
	record := &inputDeliveryRecord{delivery: delivery, done: make(chan struct{})}
	close(record.done)
	c.deliveries[id] = record
}

func (c *interactionController) settleDelivery(record *inputDeliveryRecord, delivery InputDelivery) InputDelivery {
	if delivery.State == "" {
		delivery = InputDelivery{State: "unknown", Code: "delivery_unknown"}
	}
	c.mu.Lock()
	select {
	case <-record.done:
		settled := record.delivery
		c.mu.Unlock()
		return settled
	default:
		record.delivery = delivery
		close(record.done)
		c.mu.Unlock()
		return delivery
	}
}

func waitForInputDelivery(ctx context.Context, record *inputDeliveryRecord) (InputDelivery, error) {
	select {
	case <-record.done:
		return record.delivery, nil
	case <-ctx.Done():
		return InputDelivery{State: "unknown", Code: "delivery_unknown"}, ctx.Err()
	}
}

func validateSteerRequest(state ControlState, closed, pending bool, request SteerRequest) error {
	switch {
	case strings.TrimSpace(request.ID) == "":
		return fmt.Errorf("invalid_command")
	case strings.TrimSpace(request.Content) == "" || len(request.Content) > maxInteractionTextBytes:
		return fmt.Errorf("invalid_input")
	case closed || !state.Active || request.ExpectedTurnID == "" || request.ExpectedTurnID != state.TurnID:
		return fmt.Errorf("stale_turn")
	case !state.CanSteer:
		return fmt.Errorf("unsupported")
	case pending:
		return fmt.Errorf("interaction_pending")
	default:
		return nil
	}
}

func validateInteractionResponse(state ControlState, closed, exists bool, request InteractionRequest, response InteractionResponse) error {
	switch {
	case strings.TrimSpace(response.ID) == "" || strings.TrimSpace(response.InteractionID) == "":
		return fmt.Errorf("invalid_command")
	case closed || !state.Active || response.ExpectedTurnID == "" || response.ExpectedTurnID != state.TurnID:
		return fmt.Errorf("stale_turn")
	case !exists || request.TurnID != response.ExpectedTurnID:
		return fmt.Errorf("not_found")
	}
	if response.Cancelled {
		if response.ChoiceID != "" || len(response.Answers) != 0 {
			return fmt.Errorf("invalid_input")
		}
		return nil
	}
	switch request.Kind {
	case "approval":
		if len(response.Answers) != 0 || (response.ChoiceID != "allow_once" && response.ChoiceID != "deny") {
			return fmt.Errorf("invalid_input")
		}
		return nil
	case "question":
		if response.ChoiceID != "" {
			return fmt.Errorf("invalid_input")
		}
		return validateQuestionAnswers(request.Questions, response.Answers)
	default:
		return fmt.Errorf("invalid_input")
	}
}

func validateQuestionAnswers(questions []InteractionQuestion, answers []InteractionAnswer) error {
	if len(questions) == 0 || len(answers) != len(questions) {
		return fmt.Errorf("invalid_input")
	}
	questionByID := make(map[string]InteractionQuestion, len(questions))
	for _, question := range questions {
		if question.ID == "" {
			return fmt.Errorf("invalid_input")
		}
		if _, duplicate := questionByID[question.ID]; duplicate {
			return fmt.Errorf("invalid_input")
		}
		questionByID[question.ID] = question
	}
	seenQuestions := make(map[string]struct{}, len(answers))
	for _, answer := range answers {
		question, ok := questionByID[answer.QuestionID]
		if !ok || answer.QuestionID == "" {
			return fmt.Errorf("invalid_input")
		}
		if _, duplicate := seenQuestions[answer.QuestionID]; duplicate {
			return fmt.Errorf("invalid_input")
		}
		seenQuestions[answer.QuestionID] = struct{}{}
		if len(answer.Text) > maxInteractionTextBytes || (!question.AllowText && answer.Text != "") {
			return fmt.Errorf("invalid_input")
		}
		if question.Secret && len(answer.OptionIDs) != 0 {
			return fmt.Errorf("invalid_input")
		}
		if !question.Multiple && len(answer.OptionIDs) > 1 {
			return fmt.Errorf("invalid_input")
		}
		options := make(map[string]struct{}, len(question.Options))
		for _, option := range question.Options {
			if option.ID == "" {
				return fmt.Errorf("invalid_input")
			}
			if _, duplicate := options[option.ID]; duplicate {
				return fmt.Errorf("invalid_input")
			}
			options[option.ID] = struct{}{}
		}
		seenOptions := make(map[string]struct{}, len(answer.OptionIDs))
		for _, optionID := range answer.OptionIDs {
			if _, ok := options[optionID]; !ok {
				return fmt.Errorf("invalid_input")
			}
			if _, duplicate := seenOptions[optionID]; duplicate {
				return fmt.Errorf("invalid_input")
			}
			seenOptions[optionID] = struct{}{}
		}
		if len(answer.OptionIDs) == 0 && strings.TrimSpace(answer.Text) == "" {
			return fmt.Errorf("invalid_input")
		}
	}
	return nil
}

func rejectedInputDelivery(err error) InputDelivery {
	return InputDelivery{State: "rejected", Code: err.Error()}
}

func validateInteractionOptions(opts ExecOptions) error {
	if opts.InteractionMode != "" && opts.InteractionMode != "autonomous" && opts.InteractionMode != "chat" {
		return fmt.Errorf("invalid interaction mode %q", opts.InteractionMode)
	}
	if opts.ResumePolicy != "" && opts.ResumePolicy != "allow_fresh" && opts.ResumePolicy != "require_native" {
		return fmt.Errorf("invalid resume policy %q", opts.ResumePolicy)
	}
	if opts.ResumePolicy == "require_native" && strings.TrimSpace(opts.ResumeSessionID) == "" {
		return fmt.Errorf("resume_unavailable: require_native needs a resume session")
	}
	return nil
}

func isChatInteraction(opts ExecOptions) bool {
	return opts.InteractionMode == "chat"
}

func requiresNativeResume(opts ExecOptions) bool {
	return opts.ResumePolicy == "require_native"
}
