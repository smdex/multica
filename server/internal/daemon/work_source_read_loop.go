package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
)

const (
	// workSourceReadPollInterval is the cadence between pending-command
	// polls per daemon. Deliberately coarser than task polling: source
	// reads are operator-initiated and short-lived.
	workSourceReadPollInterval = 15 * time.Second
	// workSourceReadTokenMargin forces a re-exchange this long before the
	// capability actually expires, so an in-flight operation never rides a
	// token that dies mid-request.
	workSourceReadTokenMargin = 15 * time.Second
	// workSourceReadHTTPTimeout bounds each single HTTP round trip.
	workSourceReadHTTPTimeout = 30 * time.Second
	// workSourceReadMinBudget is the minimum remaining lifetime a command
	// must have before we are willing to claim it. Claiming with less would
	// risk claiming and then being fenced out at report time.
	workSourceReadMinBudget = 20 * time.Second
	// workSourceReadMaxExec bounds a single boundedBeads subprocess.
	workSourceReadMaxExec = 30 * time.Second
	// workSourceReadMaxPending bounds retained unreported outcomes. Past
	// this the oldest is dropped; the server's terminal expiry is the honest
	// fallback for anything we could not deliver.
	workSourceReadMaxPending = 64
)

// workSourceReadFailedDiagnostic is the ONLY failure diagnostic ever sent to
// the server. Arbitrary executable stderr can embed local paths, secrets, or
// fragments that defeat masking, so no error text from the subprocess is
// forwarded, sanitized or not.
const workSourceReadFailedDiagnostic = "Source read failed; inspect local daemon configuration and source availability."

// ponytail: one serial loop runs at most one source command per target per
// round. Ceiling: each target adds bounded exchange/list/claim/report network
// waits plus up to workSourceReadMaxExec, so round latency scales with target
// count. Upgrade path: a bounded worker pool keyed by runtime if needed.

// workSourceReadTarget is one immutable snapshot of (workspace, runtime)
// taken under d.mu each poll round. Handle resolution against local bindings
// happens per pending command, not here.
type workSourceReadTarget struct {
	workspaceID string
	runtimeID   string
}

// workSourceReadOutcome is a terminal result retained in memory until the
// server definitely receives it or the command's deadline fence discards it.
// Retrying never re-runs the executable; only the exact stored fields are
// re-sent. The workspace is frozen at claim time so later runtime reshuffles
// cannot misroute the retry.
type workSourceReadOutcome struct {
	workspaceID string
	runtimeID   string
	commandID   string
	status      string
	result      string
	// diagnostic is always workSourceReadFailedDiagnostic; kept per-outcome
	// so retries re-send the exact stored receipt.
	diagnostic string
	expiresAt  time.Time
}

// workSourceReadState is the loop-local state, owned by the single loop
// goroutine. No mutex: every reader (poll, claim, report, flush retry) runs
// inline in that goroutine.
type workSourceReadState struct {
	client   *Client
	daemonID string
	logger   *slog.Logger
	now      func() time.Time

	creds   map[string]SourceReadCredential  // runtimeID -> capability
	pending map[string]workSourceReadOutcome // commandID -> outcome
}

// workSourceReadLoop is the production entry point wired from Run. With no
// operator bindings it returns before any HTTP request or process launch, so
// an unconfigured daemon spends nothing on source reads.
func (d *Daemon) workSourceReadLoop(ctx context.Context) {
	if len(d.cfg.WorkSourceReads) == 0 {
		return
	}
	// Fail closed before any HTTP: the whole binding list must satisfy the
	// operator contract (canonical workspace UUIDs, explicit absolute paths,
	// no duplicates) or the loop refuses to run at all.
	if err := cli.ValidateWorkSourceReads(d.cfg.WorkSourceReads); err != nil {
		d.logger.Error("work source read loop disabled by invalid local bindings", "error", err)
		return
	}
	s := &workSourceReadState{
		client:   d.client,
		daemonID: d.cfg.DaemonID,
		logger:   d.logger,
		now:      time.Now,
		creds:    make(map[string]SourceReadCredential),
		pending:  make(map[string]workSourceReadOutcome),
	}
	timer := time.NewTimer(workSourceReadPollInterval)
	defer timer.Stop()
	for {
		// Retries for reports lost to network blips run before new polls so
		// an old outcome never waits behind fresh work.
		s.flushPendingReports(ctx)
		for _, t := range d.workSourceReadTargets() {
			if ctx.Err() != nil {
				return
			}
			s.pollTarget(ctx, t, d.cfg.WorkSourceReads)
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			timer.Reset(workSourceReadPollInterval)
		}
	}
}

// workSourceReadTargets snapshots every (workspace, runtime) pair tracked
// under d.mu whose workspace has at least one approved local binding,
// deduplicated so no runtime is polled twice per round. Only bound
// workspaces are polled: an unbound workspace has no operator-approved
// source, so exchanging capabilities or listing commands for it is pure
// unapproved traffic. All runtimes within an approved workspace are listed —
// the server decides which runtime a source command targets; handle
// resolution happens per pending command against the local matcher.
// Ownership comes from the workspaces map; runtimeIndex only confirms the
// runtime row still exists.
func (d *Daemon) workSourceReadTargets() []workSourceReadTarget {
	d.mu.Lock()
	defer d.mu.Unlock()
	bound := make(map[string]struct{}, len(d.cfg.WorkSourceReads))
	for _, b := range d.cfg.WorkSourceReads {
		bound[b.WorkspaceID] = struct{}{}
	}
	seen := make(map[string]struct{})
	var targets []workSourceReadTarget
	var wsIDs []string
	for wsID := range d.workspaces {
		if _, ok := bound[wsID]; !ok {
			continue
		}
		wsIDs = append(wsIDs, wsID)
	}
	sort.Strings(wsIDs)
	for _, wsID := range wsIDs {
		for _, rtID := range d.workspaces[wsID].runtimeIDs {
			if _, ok := d.runtimeIndex[rtID]; !ok {
				continue
			}
			if _, dup := seen[rtID]; dup {
				continue
			}
			seen[rtID] = struct{}{}
			targets = append(targets, workSourceReadTarget{workspaceID: wsID, runtimeID: rtID})
		}
	}
	return targets
}

// token returns a usable capability for the target's runtime, exchanging a
// fresh one when none is cached or the cached one is near expiry. The client's
// human token is only read by ExchangeSourceReadToken; it is never replaced.
func (s *workSourceReadState) token(ctx context.Context, t workSourceReadTarget, force bool) (string, error) {
	if cred, ok := s.creds[t.runtimeID]; ok && !force && s.now().Add(workSourceReadTokenMargin).Before(cred.ExpiresAt) {
		return cred.Token, nil
	}
	httpCtx, cancel := context.WithTimeout(ctx, workSourceReadHTTPTimeout)
	defer cancel()
	fresh, err := s.client.ExchangeSourceReadToken(httpCtx, t.runtimeID, t.workspaceID, s.daemonID)
	if err != nil {
		return "", fmt.Errorf("source read: exchange capability: %w", err)
	}
	s.creds[t.runtimeID] = fresh
	return fresh.Token, nil
}

// withSourceReadToken runs op with a capability token; a 401 refreshes the
// capability at most once per operation and retries exactly once. There is no
// fallback to the human credential for scoped source-read routes.
func (s *workSourceReadState) withSourceReadToken(ctx context.Context, t workSourceReadTarget, force bool, op func(ctx context.Context, token string) error) error {
	token, err := s.token(ctx, t, force)
	if err != nil {
		return err
	}
	httpCtx, cancel := context.WithTimeout(ctx, workSourceReadHTTPTimeout)
	defer cancel()
	opErr := op(httpCtx, token)
	if opErr == nil || !isUnauthorizedError(opErr) {
		return opErr
	}
	token, err = s.token(ctx, t, true)
	if err != nil {
		return err
	}
	httpCtx2, cancel2 := context.WithTimeout(ctx, workSourceReadHTTPTimeout)
	defer cancel2()
	return op(httpCtx2, token)
}

// pollTarget lists pending commands for one runtime and processes each one
// whose (workspace, source handle) has an exact local binding. Anything else
// is ignored, never claimed.
func (s *workSourceReadState) pollTarget(ctx context.Context, t workSourceReadTarget, bindings []cli.WorkSourceReadBinding) {
	var cmds []SourceReadCommand
	list := func(hctx context.Context, token string) error {
		var err error
		cmds, err = s.client.ListSourceReadCommands(hctx, t.runtimeID, t.workspaceID, token)
		return err
	}
	if err := s.withSourceReadToken(ctx, t, false, list); err != nil {
		// No error field: a requestError embeds the server's response body,
		// which is arbitrary content we do not forward into logs.
		s.logger.Debug("work source read poll failed", "runtime_id", t.runtimeID, "unauthorized", isUnauthorizedError(err))
		return
	}
	for _, cmd := range cmds {
		if ctx.Err() != nil {
			return
		}
		if cmd.Status != "pending" || cmd.WorkspaceID != t.workspaceID {
			continue
		}
		// Exact local binding matcher: the pending handle must match one
		// binding of this exact workspace.
		bound := false
		for i := range bindings {
			if bindings[i].WorkspaceID == cmd.WorkspaceID && bindings[i].SourceHandle == cmd.SourceHandle {
				bound = true
				break
			}
		}
		if !bound {
			continue
		}
		expiresAt, err := time.Parse(time.RFC3339, cmd.ExpiresAt)
		if err != nil {
			s.logger.Warn("work source read command has malformed expiry; skipping", "command_id", cmd.ID)
			continue
		}
		if s.now().Add(workSourceReadMinBudget).After(expiresAt) {
			// Too close to the fence: claiming now risks claiming and being
			// unable to finish before terminal expiry. Leave it pending so
			// server-side expiry is the honest outcome.
			continue
		}
		s.runCommand(ctx, t, bindings, cmd, expiresAt)
		return // Give the next runtime a turn before consuming another command.
	}
}

// sourceReadClaimMatches verifies the claim receipt against the pending item
// field by field. The server is trusted for the transition itself, but the
// receipt must echo our pending expectations (and name this runtime as the
// claimer) before any local executable runs: a divergent receipt means the
// claim and the pending view disagree, and executing would read the wrong
// source or race another claimer.
func sourceReadClaimMatches(claimed SourceReadCommand, pending SourceReadCommand, runtimeID string) error {
	if claimed.ID != pending.ID || claimed.RequestID != pending.RequestID {
		return fmt.Errorf("claim receipt id/request mismatch")
	}
	if claimed.WorkspaceID != pending.WorkspaceID || claimed.SourceID != pending.SourceID {
		return fmt.Errorf("claim receipt workspace/source mismatch")
	}
	if claimed.Command != pending.Command || claimed.NativeID != pending.NativeID {
		return fmt.Errorf("claim receipt command mismatch")
	}
	if claimed.LimitCount != pending.LimitCount || claimed.ConfigRevision != pending.ConfigRevision {
		return fmt.Errorf("claim receipt limit/revision mismatch")
	}
	if claimed.ClaimedRuntimeID != runtimeID {
		return fmt.Errorf("claim receipt names another runtime as claimer")
	}
	return nil
}

// sourceReadClaimExpiry validates the claim receipt's expiry: it must parse,
// match the pending item's instant exactly, and still be in the future. There
// is deliberately no "receipt omitted it" fallback path: a claim receipt
// without a usable, identical, unexpired deadline is a divergent receipt and
// must never launch an executable.
func sourceReadClaimExpiry(claimed SourceReadCommand, pendingExpiry time.Time, now time.Time) (time.Time, error) {
	if claimed.ExpiresAt == "" {
		return time.Time{}, fmt.Errorf("claim receipt expiry missing")
	}
	at, err := time.Parse(time.RFC3339, claimed.ExpiresAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("claim receipt expiry malformed")
	}
	if !at.Equal(pendingExpiry) {
		return time.Time{}, fmt.Errorf("claim receipt expiry does not match pending")
	}
	if !at.After(now) {
		return time.Time{}, fmt.Errorf("claim receipt expiry not in the future")
	}
	return at, nil
}

// runCommand claims, executes, and reports one command. The claim receipt
// omits SourceHandle, so the pending item's fields are the source of truth.
// The claimed receipt's expiry is authoritative once it validates.
func (s *workSourceReadState) runCommand(ctx context.Context, t workSourceReadTarget, bindings []cli.WorkSourceReadBinding, cmd SourceReadCommand, pendingExpiry time.Time) {
	var claimed SourceReadCommand
	claim := func(hctx context.Context, token string) error {
		var err error
		claimed, err = s.client.ClaimSourceReadCommand(hctx, t.runtimeID, t.workspaceID, cmd.ID, token)
		return err
	}
	if err := s.withSourceReadToken(ctx, t, false, claim); err != nil {
		// Conflict-style failures mean another runtime claimed it or it left
		// pending; either way there is nothing to do this round. No error
		// field: a requestError embeds the server's arbitrary response body.
		s.logger.Debug("work source read claim failed", "command_id", cmd.ID, "unauthorized", isUnauthorizedError(err))
		return
	}
	if claimed.Status != "claimed" {
		s.logger.Warn("work source read claim returned unexpected status; leaving to server", "command_id", cmd.ID, "status", claimed.Status)
		return
	}
	if err := sourceReadClaimMatches(claimed, cmd, t.runtimeID); err != nil {
		// Never execute on a divergent receipt; leave the row to the server.
		s.logger.Warn("work source read claim receipt mismatch; not executing", "command_id", cmd.ID, "error", err)
		return
	}
	// The claim receipt's deadline is authoritative only when it parses,
	// equals the pending instant, and is still future; anything else is a
	// divergent receipt and must not execute.
	expiresAt, err := sourceReadClaimExpiry(claimed, pendingExpiry, s.now())
	if err != nil {
		s.logger.Warn("work source read claim receipt expiry invalid; not executing", "command_id", cmd.ID, "error", err)
		return
	}

	// Bound the subprocess by both the loop context and the command deadline,
	// whichever is earlier.
	execDeadline := s.now().Add(workSourceReadMaxExec)
	if expiresAt.Before(execDeadline) {
		execDeadline = expiresAt
	}
	execCtx, cancel := context.WithDeadline(ctx, execDeadline)
	defer cancel()
	out, execErr := executeWorkSourceRead(execCtx, bindings, WorkSourceReadRequest{
		WorkspaceID:  cmd.WorkspaceID,
		SourceHandle: cmd.SourceHandle,
		Command:      cmd.Command,
		NativeID:     cmd.NativeID,
		Limit:        int(cmd.LimitCount),
	})

	// The workspace is frozen into the outcome so report retries never
	// re-derive routing from mutable daemon state.
	outcome := workSourceReadOutcome{
		workspaceID: t.workspaceID,
		runtimeID:   t.runtimeID,
		commandID:   cmd.ID,
		expiresAt:   expiresAt,
	}
	if execErr == nil {
		outcome.status = "succeeded"
		outcome.result = string(out)
	} else {
		outcome.status = "failed"
		// Fixed generic diagnostic: execErr can carry arbitrary subprocess
		// stderr (paths, secrets) and is never forwarded in any form.
		outcome.diagnostic = workSourceReadFailedDiagnostic
		// Local-only log of the same generic message; the raw error stays
		// out of structured fields too, since logs ship with reports.
		s.logger.Warn("work source read execution failed", "command_id", cmd.ID)
	}
	// The outcome is terminal and retained in memory; report retries re-send
	// exactly these fields and never re-run the executable.
	s.retainOutcome(outcome)
	s.deliverOutcome(ctx, t, outcome)
}

// retainOutcome stores an unreported outcome, bounded by dropping the oldest.
func (s *workSourceReadState) retainOutcome(o workSourceReadOutcome) {
	if _, exists := s.pending[o.commandID]; !exists && len(s.pending) >= workSourceReadMaxPending {
		var oldest string
		var oldestAt time.Time
		for id, p := range s.pending {
			if oldest == "" || p.expiresAt.Before(oldestAt) {
				oldest, oldestAt = id, p.expiresAt
			}
		}
		if oldest != "" {
			delete(s.pending, oldest)
		}
	}
	s.pending[o.commandID] = o
}

// deliverOutcome sends one report with the single-refresh 401 policy. The
// whole operation — capability exchange and report — is bounded by the
// receipt's own deadline, so no in-flight report can straggle past ExpiresAt.
// On a definite terminal receipt (or a non-retryable fence) the outcome is
// discarded; anything else stays pending for the next flush.
func (s *workSourceReadState) deliverOutcome(ctx context.Context, t workSourceReadTarget, o workSourceReadOutcome) {
	if !s.now().Before(o.expiresAt) {
		delete(s.pending, o.commandID)
		return
	}
	deadlineCtx, cancel := context.WithDeadline(ctx, o.expiresAt)
	defer cancel()
	report := func(hctx context.Context, token string) error {
		_, err := s.client.ReportSourceReadCommand(hctx, o.runtimeID, o.workspaceID, o.commandID, token, o.status, o.result, o.diagnostic)
		return err
	}
	err := s.withSourceReadToken(deadlineCtx, t, false, report)
	if err == nil {
		delete(s.pending, o.commandID)
		return
	}
	if !sourceReadReportRetryable(err) {
		// A definite non-retryable rejection means the server already resolved
		// the command; re-sending the same receipt cannot change the outcome.
		delete(s.pending, o.commandID)
		return
	}
	if !s.now().Before(o.expiresAt) {
		// Deadline fence at equality: the command is terminally expired.
		delete(s.pending, o.commandID)
	}
	// Lost or transiently-failed reports stay pending; retry happens in
	// flushPendingReports with the exact stored fields.
}

// flushPendingReports retries every retained outcome whose command deadline
// has not fenced it out (now must still be before the receipt's ExpiresAt),
// routed by the workspace frozen into the outcome. A restart loses this
// memory entirely; the server's terminal expiry is then the honest fallback,
// which is why nothing here is persisted.
func (s *workSourceReadState) flushPendingReports(ctx context.Context) {
	outcomes := make([]workSourceReadOutcome, 0, len(s.pending))
	for _, o := range s.pending {
		outcomes = append(outcomes, o)
	}
	for _, o := range outcomes {
		if ctx.Err() != nil {
			return
		}
		if !s.now().Before(o.expiresAt) {
			// Deadline fence at equality: expired outcomes are never re-sent.
			delete(s.pending, o.commandID)
			continue
		}
		s.deliverOutcome(ctx, workSourceReadTarget{workspaceID: o.workspaceID, runtimeID: o.runtimeID}, o)
	}
}

// sourceReadReportRetryable decides whether a failed report deserves another
// attempt: network/context failures, 5xx, and the transient 408/429 responses
// yes; any other definite 4xx means the server already resolved the command,
// so retrying is pointless.
func sourceReadReportRetryable(err error) bool {
	if err == nil {
		return false
	}
	if isUnauthorizedError(err) {
		return true
	}
	var reqErr *requestError
	if errors.As(err, &reqErr) {
		if reqErr.StatusCode >= 500 {
			return true
		}
		// 408 request-timeout and 429 rate-limit are transient server-side
		// conditions, not a verdict on the command itself.
		return reqErr.StatusCode == http.StatusRequestTimeout || reqErr.StatusCode == http.StatusTooManyRequests
	}
	// No typed status (transport failure, cancellation): retry.
	return true
}
