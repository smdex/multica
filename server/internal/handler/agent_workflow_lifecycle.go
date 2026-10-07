package handler

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Parent locks precede request/interaction locks: workspace, runtime, session
// (when present), agent, task. This matches chat deletion and task settlement. An
// import reservation has no session yet; the workspace and agent fence its
// publication against deletion. Never hold a request while waiting for a parent.
func lockWorkflowParents(ctx context.Context, q *db.Queries, record db.AgentWorkflowRequest) (db.Agent, db.ChatSession, db.AgentTaskQueue, error) {
	var a db.Agent
	var session db.ChatSession
	var task db.AgentTaskQueue
	if _, err := q.LockWorkspaceForChatSessionCreate(ctx, record.WorkspaceID); err != nil {
		return a, session, task, err
	}
	if record.RuntimeID.Valid {
		if _, err := q.LockWorkflowRuntime(ctx, record.RuntimeID); err != nil {
			return a, session, task, err
		}
	}
	var agentID pgtype.UUID
	switch record.Kind {
	case workflowKindSteer, workflowKindInteraction:
		if _, err := q.LockChatSessionForDelete(ctx, record.ChatSessionID); err != nil {
			return a, session, task, err
		}
		var err error
		session, err = q.GetChatSession(ctx, record.ChatSessionID)
		if err != nil {
			return a, session, task, err
		}
		agentID = session.AgentID
	case workflowKindNativeImport:
		var command nativeImportCommand
		if json.Unmarshal(record.Request, &command) != nil {
			return a, session, task, errWorkflowNotFound
		}
		// A replay/deduplicated import can already name a published session.
		// Take that session before the agent, just like the chat deleter.
		if _, err := q.LockChatSessionForDelete(ctx, record.ChatSessionID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return a, session, task, err
		}
		var err error
		agentID, err = util.ParseUUID(command.AgentID)
		if err != nil {
			return a, session, task, errWorkflowNotFound
		}
	}
	if agentID.Valid {
		var err error
		a, err = q.GetAgentForClaimUpdate(ctx, agentID)
		if err != nil {
			return a, session, task, err
		}
	}
	if record.TaskID.Valid {
		var err error
		task, err = q.LockAgentTaskForControls(ctx, record.TaskID)
		if err != nil {
			return a, session, task, err
		}
	}
	return a, session, task, nil
}

// Dispatch is the authorization boundary. Selection alone cannot authorize a
// native write; acceptance before a policy change carries no dispatch authority.
func (h *Handler) claimAgentWorkflowRequests(ctx context.Context, runtimeID pgtype.UUID, capabilities protocol.AgentWorkflowCapabilities) ([]db.AgentWorkflowRequest, error) {
	if err := h.reconcileRuntimeWorkflow(ctx, runtimeID); err != nil {
		return nil, err
	}
	candidates, err := h.Queries.ListPendingAgentWorkflowRequests(ctx, db.ListPendingAgentWorkflowRequestsParams{RuntimeID: runtimeID, MaxCommands: 10})
	if err != nil {
		return nil, err
	}
	var admitted []db.AgentWorkflowRequest
	for _, candidate := range candidates {
		row, ok, err := h.admitWorkflowRequest(ctx, candidate, capabilities)
		if err != nil {
			return nil, err
		}
		if ok {
			admitted = append(admitted, row)
		}
	}
	return admitted, nil
}

func (h *Handler) admitWorkflowRequest(ctx context.Context, candidate db.AgentWorkflowRequest, capabilities protocol.AgentWorkflowCapabilities) (db.AgentWorkflowRequest, bool, error) {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return db.AgentWorkflowRequest{}, false, err
	}
	defer tx.Rollback(ctx)
	q := h.Queries.WithTx(tx)
	a, session, task, parentErr := lockWorkflowParents(ctx, q, candidate)
	if parentErr != nil && !errors.Is(parentErr, pgx.ErrNoRows) && !errors.Is(parentErr, errWorkflowNotFound) {
		return db.AgentWorkflowRequest{}, false, parentErr
	}
	row, err := q.LockAgentWorkflowRequestForRuntime(ctx, db.LockAgentWorkflowRequestForRuntimeParams{ID: candidate.ID, RuntimeID: candidate.RuntimeID})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.AgentWorkflowRequest{}, false, nil
	}
	if err != nil {
		return db.AgentWorkflowRequest{}, false, err
	}
	if row.Status != "pending" {
		return row, false, nil
	}
	admitted := parentErr == nil && row.ExpiresAt.Time.After(time.Now())
	member, err := q.LockWorkflowMember(ctx, db.LockWorkflowMemberParams{WorkspaceID: row.WorkspaceID, UserID: row.RequesterID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return row, false, err
	}
	admitted = admitted && err == nil && member.UserID == row.RequesterID
	runtime, err := q.GetAgentRuntime(ctx, row.RuntimeID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return row, false, err
	}
	admitted = admitted && err == nil && runtime.WorkspaceID == row.WorkspaceID && runtime.Status == "online"
	if row.Kind != workflowKindNativeList {
		admitted = admitted && a.WorkspaceID == row.WorkspaceID && !a.ArchivedAt.Valid && a.RuntimeID == runtime.ID &&
			h.canInvokeAgentWithQueries(ctx, q, a, "member", uuidToString(row.RequesterID), uuidToString(row.RequesterID), uuidToString(row.WorkspaceID))
	}
	switch row.Kind {
	case workflowKindNativeList:
		admitted = admitted && capabilities.NativeSessions.List && runtime.OwnerID == row.RequesterID
	case workflowKindNativeImport:
		var command nativeImportCommand
		valid := json.Unmarshal(row.Request, &command) == nil && command.ImportID == uuidToString(row.ID) && command.ChatSessionID == uuidToString(row.ChatSessionID) && command.AgentID == uuidToString(a.ID) && row.NativeSourceID.Valid && command.NativeID == row.NativeSourceID.String
		admitted = admitted && valid && capabilities.NativeSessions.Import && runtime.OwnerID == row.RequesterID
	case workflowKindSteer, workflowKindInteraction:
		state, valid := taskControlState(task)
		admitted = admitted && session.WorkspaceID == row.WorkspaceID && session.CreatorID == row.RequesterID && session.Status == "active" &&
			task.AgentID == session.AgentID && task.ChatSessionID == session.ID && task.RuntimeID == row.RuntimeID && task.Status == "running" &&
			task.ActiveRunID == row.RunID && row.RunID.Valid && valid && state.Active && state.TurnID != nil && row.TurnID.Valid && *state.TurnID == row.TurnID.String
		if row.Kind == workflowKindSteer {
			var command steerCommand
			admitted = admitted && json.Unmarshal(row.Request, &command) == nil && command.ChatSessionID == uuidToString(row.ChatSessionID) && command.TaskID == uuidToString(row.TaskID) && command.RunID == uuidToString(row.RunID) && command.TurnID == row.TurnID.String
			pending, err := q.GetPendingTaskInteractionForRun(ctx, db.GetPendingTaskInteractionForRunParams{TaskID: row.TaskID, RunID: row.RunID})
			if err != nil {
				return row, false, err
			}
			admitted = admitted && capabilities.Controls.Steer && state.CanSteer && !pending
		} else {
			var command interactionResponseCommand
			parseErr := json.Unmarshal(row.Request, &command)
			id, idErr := util.ParseUUID(command.InteractionID)
			if parseErr != nil || idErr != nil || command.ChatSessionID != uuidToString(row.ChatSessionID) || command.TaskID != uuidToString(row.TaskID) || command.RunID != uuidToString(row.RunID) || command.TurnID != row.TurnID.String {
				admitted = false
			} else {
				i, err := q.LockTaskInteractionInChatSession(ctx, db.LockTaskInteractionInChatSessionParams{ID: id, WorkspaceID: row.WorkspaceID, ChatSessionID: row.ChatSessionID})
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return row, false, err
				}
				admitted = admitted && err == nil && i.TaskID == task.ID && i.RunID == row.RunID && i.RuntimeID == row.RuntimeID && i.TurnID == row.TurnID.String &&
					i.Status == "resolving" && i.ResponseRequestID == row.ID && i.ExpiresAt.Time.After(time.Now()) &&
					workflowJSONEqual(i.Response, mustJSON(command.Response)) && validInteractionResponse(i.Request, command.Response) &&
					((i.Kind == "approval" && capabilities.Controls.Approvals && state.CanApprove) || (i.Kind == "question" && capabilities.Controls.Questions && state.CanAnswer))
			}
		}
	default:
		admitted = false
	}
	if admitted {
		row, err = q.DispatchAgentWorkflowRequest(ctx, row.ID)
	} else {
		row, err = q.CompleteAgentWorkflowRequest(ctx, db.CompleteAgentWorkflowRequestParams{ID: row.ID, RuntimeID: row.RuntimeID, Status: "failed", Error: mustJSON(protocol.AgentWorkflowError{Code: "admission_revoked", Message: "workflow request is no longer authorized or active"})})
		if err == nil {
			err = q.CancelTaskInteractionResponse(ctx, row.ID)
		}
	}
	if err != nil {
		return row, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return row, false, err
	}
	return row, admitted, nil
}

func reconcileChatWorkflow(ctx context.Context, q *db.Queries, sessionID pgtype.UUID) error {
	ids, err := q.ListChatWorkflowTaskIDs(ctx, sessionID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := q.LockAgentTaskForControls(ctx, id); err != nil {
			return err
		}
		if err := service.ReconcileTaskWorkflow(ctx, q, id); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) reconcileRuntimeWorkflow(ctx context.Context, runtimeID pgtype.UUID) error {
	ids, err := h.Queries.ListRuntimeWorkflowTaskIDs(ctx, runtimeID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := h.reconcileWorkflowTask(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) reconcileWorkflowTask(ctx context.Context, id pgtype.UUID) error {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := h.Queries.WithTx(tx)
	if _, err := q.LockChatSessionForTask(ctx, id); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err := q.LockAgentTaskForControls(ctx, id); errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	if err := service.ReconcileTaskWorkflow(ctx, q, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
