package handler

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	nativeSessionReferenceTTL = 15 * time.Minute
	maxWorkflowResultBytes    = 24 << 20

	workflowKindNativeList   = "native_session_list"
	workflowKindNativeImport = "native_session_import"
	workflowKindSteer        = "steer"
	workflowKindInteraction  = "interaction_response"
)

// Workflow requests are durable server-to-daemon commands. Their command body
// is deliberately private: only a redacted result projection reaches browsers.
type workflowRequestResponse struct {
	ID        string          `json:"id"`
	RuntimeID string          `json:"runtime_id"`
	Kind      string          `json:"kind"`
	Status    string          `json:"status"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     json.RawMessage `json:"error,omitempty"`
	CreatedAt string          `json:"created_at"`
	UpdatedAt string          `json:"updated_at"`
}

type workflowError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type nativeListRequest struct {
	RequestID string `json:"request_id"`
	Cursor    string `json:"cursor,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

type nativeImportRequest struct {
	RequestID  string `json:"request_id"`
	SessionRef string `json:"session_ref"`
	AgentID    string `json:"agent_id"`
	Revision   string `json:"revision"`
}

type nativeSessionListCommand struct {
	Cursor *string `json:"cursor"`
	Limit  int     `json:"limit"`
}

// nativeListResult is stored only in the short-lived request ledger. The
// session_ref is opaque; native_id and resume information are never returned.
type nativeListResult struct {
	Provider   string                `json:"provider"`
	Sessions   []nativeListedSession `json:"sessions"`
	NextCursor *string               `json:"next_cursor"`
	Truncated  bool                  `json:"truncated"`
}

type nativeListedSession struct {
	SessionRef string  `json:"session_ref"`
	NativeID   string  `json:"native_id"`
	Revision   string  `json:"revision"`
	Handle     string  `json:"handle"`
	Title      string  `json:"title,omitempty"`
	Cwd        string  `json:"cwd,omitempty"`
	Preview    string  `json:"preview,omitempty"`
	UpdatedAt  string  `json:"updated_at,omitempty"`
	Model      *string `json:"model"`
}

type nativeListResultResponse struct {
	Sessions   []nativeListedSessionResponse `json:"sessions"`
	NextCursor *string                       `json:"next_cursor"`
	Truncated  bool                          `json:"truncated"`
}

type nativeListedSessionResponse struct {
	SessionRef            string  `json:"session_ref"`
	Revision              string  `json:"revision"`
	Provider              string  `json:"provider"`
	Title                 string  `json:"title"`
	Cwd                   string  `json:"cwd"`
	Preview               string  `json:"preview"`
	UpdatedAt             string  `json:"updated_at"`
	Model                 *string `json:"model"`
	ImportedChatSessionID *string `json:"imported_chat_session_id"`
}

type nativeImportCommand struct {
	ImportID      string `json:"import_id"`
	ChatSessionID string `json:"chat_session_id"`
	SessionRef    string `json:"session_ref"`
	NativeID      string `json:"native_id"`
	Revision      string `json:"revision"`
	Handle        string `json:"handle"`
	AgentID       string `json:"agent_id"`
	Title         string `json:"title,omitempty"`
}

type daemonWorkflowResult struct {
	Status string          `json:"status"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *workflowError  `json:"error,omitempty"`
}

type daemonNativeListResult struct {
	Sessions []struct {
		NativeID  string  `json:"native_id"`
		Revision  string  `json:"revision"`
		Handle    string  `json:"handle"`
		Title     string  `json:"title,omitempty"`
		Cwd       string  `json:"cwd,omitempty"`
		Preview   string  `json:"preview,omitempty"`
		UpdatedAt string  `json:"updated_at,omitempty"`
		Model     *string `json:"model"`
	} `json:"sessions"`
	NextCursor *string `json:"next_cursor"`
	Truncated  bool    `json:"truncated"`
}

type daemonNativeImportResult struct {
	// NativeID is the original selected source identity. OwnedNativeID is the
	// daemon-created clone identity and must never be used for server dedupe.
	NativeID      string                  `json:"native_id"`
	OwnedNativeID string                  `json:"owned_native_id"`
	Provider      string                  `json:"provider"`
	SessionID     string                  `json:"resume_session_id"`
	WorkDir       string                  `json:"work_dir"`
	Messages      []daemonImportedMessage `json:"messages"`
	Warnings      []string                `json:"warnings"`
}

type daemonImportedMessage struct {
	NativeID  string          `json:"native_id"`
	Role      string          `json:"role"`
	Content   string          `json:"content"`
	CreatedAt string          `json:"created_at"`
	Events    json.RawMessage `json:"events,omitempty"`
}

type chatControlsResponse struct {
	ChatSessionID string `json:"chat_session_id"`
	Active        bool   `json:"active"`
	RuntimeID     string `json:"runtime_id,omitempty"`
	TaskID        string `json:"task_id,omitempty"`
	RunID         string `json:"run_id,omitempty"`
	TurnID        string `json:"turn_id,omitempty"`
	CanSteer      bool   `json:"can_steer"`
	CanApprove    bool   `json:"can_approve"`
	CanAnswer     bool   `json:"can_answer"`
}

type steerRequest struct {
	RequestID string `json:"request_id"`
	TaskID    string `json:"task_id"`
	RunID     string `json:"run_id"`
	TurnID    string `json:"turn_id"`
	Content   string `json:"content"`
}

type steerCommand struct {
	ChatSessionID string `json:"chat_session_id"`
	TaskID        string `json:"task_id"`
	RunID         string `json:"run_id"`
	TurnID        string `json:"turn_id"`
	Content       string `json:"content"`
}

type interactionResponseRequest struct {
	RequestID string                                    `json:"request_id"`
	TaskID    string                                    `json:"task_id"`
	RunID     string                                    `json:"run_id"`
	TurnID    string                                    `json:"turn_id"`
	Response  protocol.AgentWorkflowInteractionResponse `json:"response"`
}

type interactionResponseCommand struct {
	ChatSessionID string                                    `json:"chat_session_id"`
	InteractionID string                                    `json:"interaction_id"`
	TaskID        string                                    `json:"task_id"`
	RunID         string                                    `json:"run_id"`
	TurnID        string                                    `json:"turn_id"`
	Response      protocol.AgentWorkflowInteractionResponse `json:"response"`
}

type daemonTaskInteractionReport struct {
	RunID       string                                   `json:"run_id"`
	Interaction protocol.AgentWorkflowInteractionRequest `json:"interaction"`
}

func (h *Handler) ownedWorkflowRuntime(w http.ResponseWriter, r *http.Request, runtimeID string) (db.AgentRuntime, string, string, bool) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return db.AgentRuntime{}, "", "", false
	}
	workspaceID := ctxWorkspaceID(r.Context())
	if _, ok := h.workspaceMember(w, r, workspaceID); !ok {
		return db.AgentRuntime{}, "", "", false
	}
	runtimeUUID, ok := parseUUIDOrBadRequest(w, runtimeID, "runtime_id")
	if !ok {
		return db.AgentRuntime{}, "", "", false
	}
	runtime, err := h.Queries.GetAgentRuntime(r.Context(), runtimeUUID)
	if err != nil || uuidToString(runtime.WorkspaceID) != workspaceID {
		writeErrorCode(w, http.StatusNotFound, "not_found", "runtime not found")
		return db.AgentRuntime{}, "", "", false
	}
	if !runtime.OwnerID.Valid || uuidToString(runtime.OwnerID) != userID {
		writeErrorCode(w, http.StatusForbidden, "forbidden", "runtime owner access is required")
		return db.AgentRuntime{}, "", "", false
	}
	return runtime, userID, workspaceID, true
}

func workflowRuntimeCapabilities(runtime db.AgentRuntime) protocol.AgentWorkflowCapabilities {
	var capabilities protocol.AgentWorkflowCapabilities
	if len(runtime.AgentWorkflowCapabilities) == 0 {
		return capabilities
	}
	_ = json.Unmarshal(runtime.AgentWorkflowCapabilities, &capabilities)
	return capabilities
}

func (h *Handler) GetAgentWorkflowCapabilities(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := ctxWorkspaceID(r.Context())
	if _, ok := h.workspaceMember(w, r, workspaceID); !ok {
		return
	}
	runtimeID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "runtimeId"), "runtime_id")
	if !ok {
		return
	}
	runtime, err := h.Queries.GetAgentRuntime(r.Context(), runtimeID)
	if err != nil || runtime.WorkspaceID != parseUUID(workspaceID) {
		writeErrorCode(w, http.StatusNotFound, "not_found", "runtime not found")
		return
	}
	capabilities := workflowRuntimeCapabilities(runtime)
	if runtime.OwnerID != parseUUID(userID) {
		capabilities.NativeSessions = protocol.AgentWorkflowNativeSessionCapabilities{}
		agents, err := h.Queries.ListWorkflowAgentsForRuntime(r.Context(), db.ListWorkflowAgentsForRuntimeParams{RuntimeID: runtime.ID, WorkspaceID: runtime.WorkspaceID})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to load runtime agents")
			return
		}
		allowed := false
		for _, a := range agents {
			if h.canInvokeAgent(r.Context(), a, "member", userID, userID, workspaceID) {
				allowed = true
				break
			}
		}
		if !allowed {
			writeErrorCode(w, http.StatusForbidden, "forbidden", "agent invocation access is required")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"runtime_id": uuidToString(runtime.ID), "online": runtime.Status == "online",
		"native_sessions": capabilities.NativeSessions, "provider": runtime.Provider,
		"controls": capabilities.Controls, "reason": nil,
	})
}

func (h *Handler) InitiateNativeSessionList(w http.ResponseWriter, r *http.Request) {
	runtimeID := chi.URLParam(r, "runtimeId")
	runtime, userID, workspaceID, ok := h.ownedWorkflowRuntime(w, r, runtimeID)
	if !ok {
		return
	}
	capabilities := workflowRuntimeCapabilities(runtime)
	if !capabilities.NativeSessions.List {
		writeErrorCode(w, http.StatusUnprocessableEntity, "unsupported", "native session listing is unsupported")
		return
	}
	if runtime.Status != "online" {
		writeErrorCode(w, http.StatusServiceUnavailable, "runtime_offline", "runtime is offline")
		return
	}
	if err := h.Queries.PurgeExpiredNativeSessionListResults(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to purge expired native session results")
		return
	}
	var req nativeListRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if _, ok := parseUUIDOrBadRequest(w, req.RequestID, "request_id"); !ok {
		return
	}
	if len(req.Cursor) > 4096 || req.Limit < 0 || req.Limit > 50 {
		writeErrorCode(w, http.StatusBadRequest, "invalid_request", "invalid native session list request")
		return
	}
	if req.Limit == 0 {
		req.Limit = 20
	}
	var cursor *string
	if req.Cursor != "" {
		cursor = &req.Cursor
	}
	command := nativeSessionListCommand{Cursor: cursor, Limit: req.Limit}
	record, replayed, err := h.createWorkflowRequest(r, req.RequestID, workspaceID, userID, runtime, workflowKindNativeList, db.CreateAgentWorkflowRequestParams{Request: mustJSON(command)}, req)
	if err != nil {
		h.writeWorkflowCreateError(w, err)
		return
	}
	if !replayed {
		h.requestDaemonPendingWork(runtimeID, protocol.PendingWorkKindAgentWorkflow)
	}
	status := http.StatusAccepted
	if record.Status == "completed" || record.Status == "failed" || record.Status == "unknown" {
		status = http.StatusOK
	}
	writeJSON(w, status, workflowRecordResponse(record))
}

func (h *Handler) InitiateNativeSessionImport(w http.ResponseWriter, r *http.Request) {
	runtimeID := chi.URLParam(r, "runtimeId")
	runtime, userID, workspaceID, ok := h.ownedWorkflowRuntime(w, r, runtimeID)
	if !ok {
		return
	}
	capabilities := workflowRuntimeCapabilities(runtime)
	if !capabilities.NativeSessions.Import {
		writeErrorCode(w, http.StatusUnprocessableEntity, "unsupported", "native session import is unsupported")
		return
	}
	if runtime.Status != "online" {
		writeErrorCode(w, http.StatusServiceUnavailable, "runtime_offline", "runtime is offline")
		return
	}
	var req nativeImportRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if _, ok := parseUUIDOrBadRequest(w, req.RequestID, "request_id"); !ok {
		return
	}
	if _, ok := parseUUIDOrBadRequest(w, req.AgentID, "agent_id"); !ok {
		return
	}
	if req.SessionRef == "" || req.Revision == "" {
		writeErrorCode(w, http.StatusBadRequest, "invalid_request", "session_ref and revision are required")
		return
	}
	source, err := h.findNativeSessionReference(r.Context(), parseUUID(workspaceID), parseUUID(userID), runtime.ID, req.SessionRef, req.Revision)
	if err != nil {
		writeErrorCode(w, http.StatusGone, "session_ref_expired", "native session reference expired")
		return
	}
	agent, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{ID: parseUUID(req.AgentID), WorkspaceID: parseUUID(workspaceID)})
	if err != nil || agent.ArchivedAt.Valid || !agent.RuntimeID.Valid || agent.RuntimeID != runtime.ID {
		writeErrorCode(w, http.StatusNotFound, "not_found", "agent not found")
		return
	}
	actorType, actorID := h.resolveActor(r, userID, workspaceID)
	if !h.canInvokeAgent(r.Context(), agent, actorType, actorID, h.invokeOriginatorFromRequest(r, actorType, actorID), workspaceID) {
		writeErrorCode(w, http.StatusForbidden, "forbidden", "you do not have access to this agent")
		return
	}
	if existing, err := h.Queries.GetImportedChatSession(r.Context(), db.GetImportedChatSessionParams{WorkspaceID: parseUUID(workspaceID), CreatorID: parseUUID(userID), RuntimeID: runtime.ID, NativeImportProvider: pgtype.Text{String: runtime.Provider, Valid: true}, NativeImportID: pgtype.Text{String: source.NativeID, Valid: true}}); err == nil {
		if existing.AgentID != parseUUID(req.AgentID) {
			writeErrorCode(w, http.StatusConflict, "already_imported", "native session is already imported for another agent")
			return
		}
		record, err := h.recordExistingNativeImport(r, req, runtime, workspaceID, userID, source, existing.ID)
		if err != nil {
			h.writeWorkflowCreateError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, workflowRecordResponse(record))
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "failed to check existing native import")
		return
	}
	if active, err := h.Queries.GetActiveNativeImportWorkflowRequest(r.Context(), db.GetActiveNativeImportWorkflowRequestParams{WorkspaceID: parseUUID(workspaceID), RequesterID: parseUUID(userID), RuntimeID: runtime.ID, NativeSourceID: pgtype.Text{String: source.NativeID, Valid: true}}); err == nil {
		var prior nativeImportCommand
		if json.Unmarshal(active.Request, &prior) != nil || prior.AgentID != req.AgentID {
			writeErrorCode(w, http.StatusConflict, "already_imported", "native session import is already assigned to another agent")
			return
		}
		writeJSON(w, http.StatusAccepted, workflowRecordResponse(active))
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "failed to check native import reservation")
		return
	}
	plannedChatSessionID := dbid.NewV7()
	command := nativeImportCommand{ImportID: req.RequestID, ChatSessionID: uuidToString(plannedChatSessionID), SessionRef: req.SessionRef, NativeID: source.NativeID, Revision: source.Revision, Handle: source.Handle, AgentID: req.AgentID, Title: source.Title}
	record, replayed, err := h.createWorkflowRequest(r, req.RequestID, workspaceID, userID, runtime, workflowKindNativeImport, db.CreateAgentWorkflowRequestParams{ChatSessionID: plannedChatSessionID, NativeSourceID: pgtype.Text{String: source.NativeID, Valid: true}, Request: mustJSON(command)}, req)
	if isUniqueViolation(err) || errors.Is(err, pgx.ErrNoRows) {
		active, lookupErr := h.Queries.GetActiveNativeImportWorkflowRequest(r.Context(), db.GetActiveNativeImportWorkflowRequestParams{WorkspaceID: parseUUID(workspaceID), RequesterID: parseUUID(userID), RuntimeID: runtime.ID, NativeSourceID: pgtype.Text{String: source.NativeID, Valid: true}})
		var prior nativeImportCommand
		if lookupErr == nil && json.Unmarshal(active.Request, &prior) == nil && prior.AgentID == req.AgentID {
			record, replayed, err = active, true, nil
		}
	}
	if err != nil {
		h.writeWorkflowCreateError(w, err)
		return
	}
	if !replayed {
		h.requestDaemonPendingWork(runtimeID, protocol.PendingWorkKindAgentWorkflow)
	}
	status := http.StatusAccepted
	if record.Status == "completed" || record.Status == "failed" || record.Status == "unknown" {
		status = http.StatusOK
	}
	writeJSON(w, status, workflowRecordResponse(record))
}

// recordExistingNativeImport gives a re-import request a durable terminal
// operation without handing a duplicate source to a daemon. It runs insert and
// completion in one transaction, so a heartbeat cannot observe it as pending.
func (h *Handler) recordExistingNativeImport(r *http.Request, req nativeImportRequest, runtime db.AgentRuntime, workspaceID, userID string, source nativeListedSession, chatSessionID pgtype.UUID) (db.AgentWorkflowRequest, error) {
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		return db.AgentWorkflowRequest{}, err
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	if _, err := qtx.LockWorkspaceForChatSessionCreate(r.Context(), parseUUID(workspaceID)); err != nil {
		return db.AgentWorkflowRequest{}, err
	}
	if _, err := qtx.LockChatSessionForDelete(r.Context(), chatSessionID); err != nil {
		return db.AgentWorkflowRequest{}, errWorkflowNotFound
	}
	agent, err := qtx.GetAgentForClaimUpdate(r.Context(), parseUUID(req.AgentID))
	if err != nil || agent.ArchivedAt.Valid || agent.RuntimeID != runtime.ID || !h.canInvokeAgentWithQueries(r.Context(), qtx, agent, "member", userID, userID, workspaceID) {
		return db.AgentWorkflowRequest{}, errWorkflowNotFound
	}
	command := nativeImportCommand{ImportID: req.RequestID, ChatSessionID: uuidToString(chatSessionID), SessionRef: req.SessionRef, NativeID: source.NativeID, Revision: source.Revision, Handle: source.Handle, AgentID: req.AgentID, Title: source.Title}
	record, replayed, err := h.createWorkflowRequestWithQueries(r.Context(), qtx, req.RequestID, workspaceID, userID, runtime, workflowKindNativeImport, db.CreateAgentWorkflowRequestParams{ChatSessionID: chatSessionID, NativeSourceID: pgtype.Text{String: source.NativeID, Valid: true}, Request: mustJSON(command)}, req)
	if err != nil || replayed {
		return record, err
	}
	record, err = qtx.CompleteAgentWorkflowRequest(r.Context(), db.CompleteAgentWorkflowRequestParams{ID: record.ID, RuntimeID: runtime.ID, Status: "completed", Result: mustJSON(map[string]any{"chat_session_id": uuidToString(chatSessionID), "already_imported": true, "warnings": []string{}})})
	if err != nil {
		return db.AgentWorkflowRequest{}, err
	}
	if err := tx.Commit(r.Context()); err != nil {
		return db.AgentWorkflowRequest{}, err
	}
	return record, nil
}

func (h *Handler) GetAgentWorkflowRequest(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := ctxWorkspaceID(r.Context())
	requestID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "requestId"), "request_id")
	if !ok {
		return
	}
	runtimeID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "runtimeId"), "runtime_id")
	if !ok {
		return
	}
	record, err := h.Queries.GetAgentWorkflowRequestForRequester(r.Context(), db.GetAgentWorkflowRequestForRequesterParams{ID: requestID, WorkspaceID: parseUUID(workspaceID), RequesterID: parseUUID(userID)})
	if err != nil || record.RuntimeID != runtimeID {
		writeErrorCode(w, http.StatusNotFound, "not_found", "workflow request not found")
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to begin workflow read")
		return
	}
	defer tx.Rollback(r.Context())
	q := h.Queries.WithTx(tx)
	a, session, _, err := lockWorkflowParents(r.Context(), q, record)
	if err != nil {
		writeErrorCode(w, http.StatusNotFound, "not_found", "workflow request not found")
		return
	}
	if _, err := q.LockWorkflowMember(r.Context(), db.LockWorkflowMemberParams{WorkspaceID: record.WorkspaceID, UserID: record.RequesterID}); err != nil {
		writeErrorCode(w, http.StatusNotFound, "not_found", "workflow request not found")
		return
	}
	runtime, err := q.GetAgentRuntime(r.Context(), record.RuntimeID)
	allowed := err == nil && runtime.WorkspaceID == record.WorkspaceID
	switch record.Kind {
	case workflowKindNativeList, workflowKindNativeImport:
		allowed = allowed && runtime.OwnerID == record.RequesterID
	case workflowKindSteer, workflowKindInteraction:
		allowed = allowed && session.CreatorID == record.RequesterID && session.WorkspaceID == record.WorkspaceID
	default:
		allowed = false
	}
	if record.Kind != workflowKindNativeList {
		allowed = allowed && !a.ArchivedAt.Valid && a.RuntimeID == runtime.ID && h.canInvokeAgentWithQueries(r.Context(), q, a, "member", userID, userID, workspaceID)
	}
	if !allowed {
		writeErrorCode(w, http.StatusForbidden, "forbidden", "workflow access is no longer allowed")
		return
	}
	if record.TaskID.Valid {
		if err := service.ReconcileTaskWorkflow(r.Context(), q, record.TaskID); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to reconcile task inputs")
			return
		}
	}
	if record.ExpiresAt.Valid && record.ExpiresAt.Time.Before(time.Now()) {
		if _, err := q.ExpireAgentWorkflowRequest(r.Context(), record.ID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusInternalServerError, "failed to expire workflow")
			return
		}
	}
	record, err = q.GetAgentWorkflowRequestForRequester(r.Context(), db.GetAgentWorkflowRequestForRequesterParams{ID: requestID, WorkspaceID: parseUUID(workspaceID), RequesterID: parseUUID(userID)})
	if err != nil {
		writeErrorCode(w, http.StatusNotFound, "not_found", "workflow request not found")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to commit workflow read")
		return
	}
	writeJSON(w, http.StatusOK, workflowRecordResponse(record))
}

func (h *Handler) GetChatControls(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	session, ok := h.gatePublicChatSessionForUser(w, r, userID, ctxWorkspaceID(r.Context()), chi.URLParam(r, "sessionId"))
	if !ok {
		return
	}
	task, err := h.Queries.GetActiveChatControlTask(r.Context(), session.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, chatControlsResponse{ChatSessionID: uuidToString(session.ID)})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load chat controls")
		return
	}
	state, ok := taskControlState(task)
	if !ok || !state.Active {
		writeJSON(w, http.StatusOK, chatControlsResponse{ChatSessionID: uuidToString(session.ID)})
		return
	}
	response := chatControlsResponse{ChatSessionID: uuidToString(session.ID), Active: true, RuntimeID: uuidToString(task.RuntimeID), TaskID: uuidToString(task.ID), RunID: uuidToString(task.ActiveRunID), CanSteer: state.CanSteer, CanApprove: state.CanApprove, CanAnswer: state.CanAnswer}
	if state.TurnID != nil {
		response.TurnID = *state.TurnID
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *Handler) InitiateChatSteer(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := ctxWorkspaceID(r.Context())
	session, ok := h.gatePublicChatSessionForUser(w, r, userID, workspaceID, chi.URLParam(r, "sessionId"))
	if !ok {
		return
	}
	var req steerRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if _, ok := parseUUIDOrBadRequest(w, req.RequestID, "request_id"); !ok {
		return
	}
	if _, ok := parseUUIDOrBadRequest(w, req.TaskID, "task_id"); !ok {
		return
	}
	if _, ok := parseUUIDOrBadRequest(w, req.RunID, "run_id"); !ok {
		return
	}
	if strings.TrimSpace(req.TurnID) == "" || strings.TrimSpace(req.Content) == "" || len(req.Content) > 16<<10 {
		writeErrorCode(w, http.StatusBadRequest, "invalid_request", "turn_id and content are required")
		return
	}
	task, err := h.Queries.GetActiveChatControlTask(r.Context(), session.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErrorCode(w, http.StatusConflict, "run_not_active", "no active chat run accepts steering")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load active chat run")
		return
	}
	state, valid := taskControlState(task)
	if !valid || !state.Active || !state.CanSteer || uuidToString(task.ID) != req.TaskID || uuidToString(task.ActiveRunID) != req.RunID || state.TurnID == nil || *state.TurnID != req.TurnID {
		writeErrorCode(w, http.StatusConflict, "stale_turn", "chat run no longer accepts steering")
		return
	}
	if err := h.reconcileWorkflowTask(r.Context(), task.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reconcile chat inputs")
		return
	}
	pending, err := h.Queries.GetPendingTaskInteractionForRun(r.Context(), db.GetPendingTaskInteractionForRunParams{TaskID: task.ID, RunID: task.ActiveRunID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load chat interactions")
		return
	}
	if pending {
		writeErrorCode(w, http.StatusConflict, "interaction_pending", "resolve the pending interaction first")
		return
	}
	runtime, err := h.Queries.GetAgentRuntime(r.Context(), task.RuntimeID)
	if err != nil || runtime.Status != "online" {
		writeErrorCode(w, http.StatusServiceUnavailable, "runtime_offline", "runtime is offline")
		return
	}
	capabilities := workflowRuntimeCapabilities(runtime)
	if !capabilities.Controls.Steer {
		writeErrorCode(w, http.StatusUnprocessableEntity, "unsupported", "chat steering is unsupported")
		return
	}
	command := steerCommand{ChatSessionID: uuidToString(session.ID), TaskID: uuidToString(task.ID), RunID: req.RunID, TurnID: req.TurnID, Content: req.Content}
	record, replayed, err := h.createWorkflowRequest(r, req.RequestID, workspaceID, userID, runtime, workflowKindSteer, db.CreateAgentWorkflowRequestParams{ChatSessionID: session.ID, TaskID: task.ID, RunID: task.ActiveRunID, TurnID: pgtype.Text{String: req.TurnID, Valid: true}, Request: mustJSON(command)}, req)
	if err != nil {
		h.writeWorkflowCreateError(w, err)
		return
	}
	if !replayed {
		h.requestDaemonPendingWork(uuidToString(runtime.ID), protocol.PendingWorkKindAgentWorkflow)
	}
	status := http.StatusAccepted
	if terminalWorkflowStatus(record.Status) {
		status = http.StatusOK
	}
	writeJSON(w, status, workflowRecordResponse(record))
}

func (h *Handler) ListChatInteractions(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := ctxWorkspaceID(r.Context())
	session, ok := h.gatePublicChatSessionForUser(w, r, userID, workspaceID, chi.URLParam(r, "sessionId"))
	if !ok {
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to begin interaction read")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	if _, err := qtx.LockChatSessionForDelete(r.Context(), session.ID); err != nil {
		writeError(w, http.StatusNotFound, "chat session not found")
		return
	}
	if err := reconcileChatWorkflow(r.Context(), qtx, session.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reconcile interactions")
		return
	}
	interactions, err := qtx.ListUnsettledTaskInteractions(r.Context(), db.ListUnsettledTaskInteractionsParams{WorkspaceID: parseUUID(workspaceID), ChatSessionID: session.ID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load interactions")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to commit interaction read")
		return
	}
	type response struct {
		ID          string                                      `json:"id"`
		RuntimeID   string                                      `json:"runtime_id"`
		TaskID      string                                      `json:"task_id"`
		RunID       string                                      `json:"run_id"`
		TurnID      string                                      `json:"turn_id"`
		Kind        string                                      `json:"kind"`
		Title       string                                      `json:"title"`
		Description string                                      `json:"description"`
		Tool        string                                      `json:"tool"`
		Input       map[string]any                              `json:"input"`
		Choices     []protocol.AgentWorkflowInteractionChoice   `json:"choices"`
		Questions   []protocol.AgentWorkflowInteractionQuestion `json:"questions"`
		Status      string                                      `json:"status"`
		Version     int64                                       `json:"version"`
		ExpiresAt   string                                      `json:"expires_at"`
	}
	result := make([]response, 0, len(interactions))
	for _, interaction := range interactions {
		var request protocol.AgentWorkflowInteractionRequest
		if json.Unmarshal(interaction.Request, &request) != nil {
			writeError(w, http.StatusInternalServerError, "stored interaction is invalid")
			return
		}
		result = append(result, response{ID: uuidToString(interaction.ID), RuntimeID: uuidToString(interaction.RuntimeID), TaskID: uuidToString(interaction.TaskID), RunID: uuidToString(interaction.RunID), TurnID: interaction.TurnID, Kind: request.Kind, Title: request.Title, Description: request.Description, Tool: request.Tool, Input: request.Input, Choices: request.Choices, Questions: request.Questions, Status: interaction.Status, Version: interaction.Version, ExpiresAt: timestampToString(interaction.ExpiresAt)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": result})
}

func (h *Handler) RespondChatInteraction(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := ctxWorkspaceID(r.Context())
	session, ok := h.gatePublicChatSessionForUser(w, r, userID, workspaceID, chi.URLParam(r, "sessionId"))
	if !ok {
		return
	}
	interactionID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "interactionId"), "interaction_id")
	if !ok {
		return
	}
	var req interactionResponseRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if _, ok := parseUUIDOrBadRequest(w, req.RequestID, "request_id"); !ok {
		return
	}
	if _, ok := parseUUIDOrBadRequest(w, req.TaskID, "task_id"); !ok {
		return
	}
	if _, ok := parseUUIDOrBadRequest(w, req.RunID, "run_id"); !ok {
		return
	}
	if strings.TrimSpace(req.TurnID) == "" {
		writeErrorCode(w, http.StatusBadRequest, "invalid_request", "turn_id is required")
		return
	}
	if replay, handled, err := h.replayWorkflowForChat(r.Context(), req.RequestID, workspaceID, userID, session.ID, workflowKindInteraction, req); handled {
		if err != nil {
			h.writeWorkflowCreateError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, workflowRecordResponse(replay))
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to begin interaction response")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	a, currentSession, task, err := lockWorkflowParents(r.Context(), qtx, db.AgentWorkflowRequest{Kind: workflowKindInteraction, WorkspaceID: session.WorkspaceID, ChatSessionID: session.ID, TaskID: parseUUID(req.TaskID)})
	if err != nil || currentSession.CreatorID != parseUUID(userID) || a.ArchivedAt.Valid || a.RuntimeID != task.RuntimeID || !h.canInvokeAgentWithQueries(r.Context(), qtx, a, "member", userID, userID, workspaceID) {
		writeErrorCode(w, http.StatusConflict, "stale_turn", "chat run is no longer available")
		return
	}

	interaction, err := qtx.LockTaskInteractionInChatSession(r.Context(), db.LockTaskInteractionInChatSessionParams{ID: interactionID, WorkspaceID: parseUUID(workspaceID), ChatSessionID: session.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		writeErrorCode(w, http.StatusNotFound, "not_found", "interaction not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to lock interaction")
		return
	}
	if uuidToString(interaction.TaskID) != req.TaskID || uuidToString(interaction.RunID) != req.RunID || interaction.TurnID != req.TurnID || !validInteractionResponse(interaction.Request, req.Response) {
		writeErrorCode(w, http.StatusBadRequest, "invalid_response", "interaction response does not match request")
		return
	}
	runtime, err := qtx.GetAgentRuntime(r.Context(), interaction.RuntimeID)
	if err != nil || runtime.Status != "online" {
		writeErrorCode(w, http.StatusServiceUnavailable, "runtime_offline", "runtime is offline")
		return
	}
	capabilities := workflowRuntimeCapabilities(runtime)
	if (interaction.Kind == "approval" && !capabilities.Controls.Approvals) || (interaction.Kind == "question" && !capabilities.Controls.Questions) {
		writeErrorCode(w, http.StatusUnprocessableEntity, "unsupported", "chat interaction response is unsupported")
		return
	}
	state, stateOK := taskControlState(task)
	canRespond := (interaction.Kind == "approval" && state.CanApprove) || (interaction.Kind == "question" && state.CanAnswer)
	if err != nil || task.Status != "running" || task.RuntimeID != interaction.RuntimeID || !task.ChatSessionID.Valid || task.ChatSessionID != session.ID || !task.ActiveRunID.Valid || task.ActiveRunID != interaction.RunID || !stateOK || !state.Active || state.TurnID == nil || *state.TurnID != interaction.TurnID || !canRespond {
		writeErrorCode(w, http.StatusConflict, "stale_turn", "chat run no longer accepts this interaction response")
		return
	}
	if _, err := qtx.StartTaskInteractionResolution(r.Context(), db.StartTaskInteractionResolutionParams{ID: interaction.ID, ResponseRequestID: parseUUID(req.RequestID), Response: mustJSON(req.Response)}); errors.Is(err, pgx.ErrNoRows) {
		writeErrorCode(w, http.StatusConflict, "already_resolved", "interaction is no longer pending")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to resolve interaction")
		return
	}
	command := interactionResponseCommand{ChatSessionID: uuidToString(session.ID), InteractionID: uuidToString(interaction.ID), TaskID: uuidToString(interaction.TaskID), RunID: uuidToString(interaction.RunID), TurnID: interaction.TurnID, Response: req.Response}
	record, _, err := h.createWorkflowRequestWithQueries(r.Context(), qtx, req.RequestID, workspaceID, userID, runtime, workflowKindInteraction, db.CreateAgentWorkflowRequestParams{ChatSessionID: session.ID, TaskID: interaction.TaskID, RunID: interaction.RunID, TurnID: pgtype.Text{String: interaction.TurnID, Valid: true}, Request: mustJSON(command)}, req)
	if err != nil {
		h.writeWorkflowCreateError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to commit interaction response")
		return
	}
	h.requestDaemonPendingWork(uuidToString(runtime.ID), protocol.PendingWorkKindAgentWorkflow)
	writeJSON(w, http.StatusAccepted, workflowRecordResponse(record))
}

func (h *Handler) replayWorkflowForChat(ctx context.Context, requestID, workspaceID, userID string, chatSessionID pgtype.UUID, kind string, canonical any) (db.AgentWorkflowRequest, bool, error) {
	record, err := h.Queries.GetAgentWorkflowRequest(ctx, parseUUID(requestID))
	if errors.Is(err, pgx.ErrNoRows) {
		return db.AgentWorkflowRequest{}, false, nil
	}
	if err != nil {
		return db.AgentWorkflowRequest{}, true, err
	}
	if record.WorkspaceID != parseUUID(workspaceID) || record.RequesterID != parseUUID(userID) || record.ChatSessionID != chatSessionID {
		return db.AgentWorkflowRequest{}, true, errWorkflowNotFound
	}
	if record.Kind != kind || record.RequestHash != workflowRequestHash(canonical) {
		return db.AgentWorkflowRequest{}, true, errWorkflowConflict
	}
	return record, true, nil
}

func taskControlState(task db.AgentTaskQueue) (protocol.TaskControlState, bool) {
	if len(task.ControlState) == 0 {
		return protocol.TaskControlState{}, false
	}
	var state protocol.TaskControlState
	if json.Unmarshal(task.ControlState, &state) != nil || state.RunID != uuidToString(task.ActiveRunID) {
		return protocol.TaskControlState{}, false
	}
	return state, true
}

func terminalWorkflowStatus(status string) bool {
	return status == "completed" || status == "failed" || status == "unknown"
}

func workflowRequestTTLForKind(kind string) time.Duration {
	switch kind {
	case workflowKindNativeList:
		return 30 * time.Second
	case workflowKindNativeImport:
		return 120 * time.Second
	case workflowKindSteer, workflowKindInteraction:
		return 15 * time.Second
	default:
		return 15 * time.Second
	}
}

func validInteractionResponse(raw []byte, response protocol.AgentWorkflowInteractionResponse) bool {
	var request protocol.AgentWorkflowInteractionRequest
	if json.Unmarshal(raw, &request) != nil {
		return false
	}
	branches := 0
	if response.Cancelled {
		branches++
	}
	if response.ChoiceID != "" {
		branches++
	}
	if len(response.Answers) > 0 {
		branches++
	}
	if branches != 1 {
		return false
	}
	if response.Cancelled {
		return true
	}
	if response.ChoiceID != "" {
		if request.Kind != "approval" || len(request.Questions) != 0 {
			return false
		}
		for _, choice := range request.Choices {
			if response.ChoiceID == choice.ID {
				return true
			}
		}
		return false
	}
	if request.Kind != "question" || len(response.Answers) != len(request.Questions) {
		return false
	}
	seenQuestions := make(map[string]struct{}, len(response.Answers))
	for _, answer := range response.Answers {
		if _, duplicate := seenQuestions[answer.QuestionID]; duplicate {
			return false
		}
		seenQuestions[answer.QuestionID] = struct{}{}
		var question *protocol.AgentWorkflowInteractionQuestion
		for i := range request.Questions {
			if request.Questions[i].ID == answer.QuestionID {
				question = &request.Questions[i]
				break
			}
		}
		if question == nil || question.Secret || (!question.AllowText && answer.Text != "") || (!question.Multiple && len(answer.OptionIDs) > 1) || (answer.Text != "" && len(answer.OptionIDs) != 0) || (answer.Text == "" && len(answer.OptionIDs) == 0) {
			return false
		}
		seenOptions := make(map[string]struct{}, len(answer.OptionIDs))
		for _, id := range answer.OptionIDs {
			if _, duplicate := seenOptions[id]; duplicate {
				return false
			}
			seenOptions[id] = struct{}{}
			found := false
			for _, option := range question.Options {
				if option.ID == id {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
	}
	return true
}

func (h *Handler) createWorkflowRequest(r *http.Request, requestID, workspaceID, userID string, runtime db.AgentRuntime, kind string, params db.CreateAgentWorkflowRequestParams, canonical any) (db.AgentWorkflowRequest, bool, error) {
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		return db.AgentWorkflowRequest{}, false, err
	}
	defer tx.Rollback(r.Context())
	q := h.Queries.WithTx(tx)
	record := db.AgentWorkflowRequest{RequesterID: parseUUID(userID), RuntimeID: runtime.ID, WorkspaceID: parseUUID(workspaceID), ChatSessionID: params.ChatSessionID, TaskID: params.TaskID, Kind: kind, Request: params.Request}
	a, session, task, err := lockWorkflowParents(r.Context(), q, record)
	if err != nil {
		return db.AgentWorkflowRequest{}, false, errWorkflowNotFound
	}
	if kind != workflowKindNativeList && (a.ArchivedAt.Valid || a.WorkspaceID != record.WorkspaceID || a.RuntimeID != runtime.ID || !h.canInvokeAgentWithQueries(r.Context(), q, a, "member", userID, userID, workspaceID)) {
		return db.AgentWorkflowRequest{}, false, errWorkflowNotFound
	}
	if params.TaskID.Valid && (session.CreatorID != parseUUID(userID) || task.Status != "running" || task.ActiveRunID != params.RunID || task.RuntimeID != runtime.ID) {
		return db.AgentWorkflowRequest{}, false, errWorkflowNotFound
	}
	if kind == workflowKindSteer {
		state, valid := taskControlState(task)
		if !valid || !state.Active || !state.CanSteer || state.TurnID == nil || *state.TurnID != params.TurnID.String {
			return db.AgentWorkflowRequest{}, false, errWorkflowConflict
		}
		if err := service.ReconcileTaskWorkflow(r.Context(), q, task.ID); err != nil {
			return db.AgentWorkflowRequest{}, false, err
		}
		pending, err := q.GetPendingTaskInteractionForRun(r.Context(), db.GetPendingTaskInteractionForRunParams{TaskID: task.ID, RunID: task.ActiveRunID})
		if err != nil {
			return db.AgentWorkflowRequest{}, false, err
		}
		if pending {
			return db.AgentWorkflowRequest{}, false, errWorkflowConflict
		}
	}
	record, replayed, err := h.createWorkflowRequestWithQueries(r.Context(), q, requestID, workspaceID, userID, runtime, kind, params, canonical)
	if err != nil {
		return record, false, err
	}
	if err := tx.Commit(r.Context()); err != nil {
		return record, false, err
	}
	return record, replayed, nil
}

func (h *Handler) createWorkflowRequestWithQueries(ctx context.Context, queries *db.Queries, requestID, workspaceID, userID string, runtime db.AgentRuntime, kind string, params db.CreateAgentWorkflowRequestParams, canonical any) (db.AgentWorkflowRequest, bool, error) {
	id := parseUUID(requestID)
	hash := workflowRequestHash(canonical)
	if existing, err := queries.GetAgentWorkflowRequest(ctx, id); err == nil {
		if existing.WorkspaceID != parseUUID(workspaceID) || existing.RequesterID != parseUUID(userID) || existing.RuntimeID != runtime.ID {
			return db.AgentWorkflowRequest{}, false, errWorkflowNotFound
		}
		if existing.RequestHash != hash || existing.Kind != kind {
			return db.AgentWorkflowRequest{}, false, errWorkflowConflict
		}
		return existing, true, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return db.AgentWorkflowRequest{}, false, err
	}
	params.ID = id
	params.WorkspaceID = parseUUID(workspaceID)
	params.RequesterID = parseUUID(userID)
	params.RuntimeID = runtime.ID
	params.Kind = kind
	params.RequestHash = hash
	params.ExpiresAt = pgtype.Timestamptz{Time: time.Now().Add(workflowRequestTTLForKind(kind)), Valid: true}
	record, err := queries.CreateAgentWorkflowRequest(ctx, params)
	if err == nil {
		return record, false, nil
	}
	// The global unique index fences request identity. On a concurrent retry, re-read and compare
	// the full requester/workspace/runtime scope before acknowledging it.
	existing, readErr := queries.GetAgentWorkflowRequest(ctx, id)
	if readErr != nil {
		return db.AgentWorkflowRequest{}, false, err
	}
	if existing.WorkspaceID != parseUUID(workspaceID) || existing.RequesterID != parseUUID(userID) || existing.RuntimeID != runtime.ID {
		return db.AgentWorkflowRequest{}, false, errWorkflowNotFound
	}
	if existing.RequestHash != hash || existing.Kind != kind {
		return db.AgentWorkflowRequest{}, false, errWorkflowConflict
	}
	return existing, true, nil
}

var (
	errWorkflowConflict = errors.New("workflow request id reused with a different body")
	errWorkflowNotFound = errors.New("workflow request not found")
)

func (h *Handler) writeWorkflowCreateError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errWorkflowConflict):
		writeErrorCode(w, http.StatusConflict, "idempotency_conflict", err.Error())
	case errors.Is(err, errWorkflowNotFound):
		writeErrorCode(w, http.StatusNotFound, "not_found", "workflow request not found")
	default:
		writeError(w, http.StatusInternalServerError, "failed to create workflow request")
	}
}

func workflowRequestHash(canonical any) string {
	sum := sha256.Sum256(mustJSON(canonical))
	return hex.EncodeToString(sum[:])
}

func mustJSON(v any) []byte { raw, _ := json.Marshal(v); return raw }

func workflowRecordResponse(record db.AgentWorkflowRequest) workflowRequestResponse {
	response := workflowRequestResponse{ID: uuidToString(record.ID), RuntimeID: uuidToString(record.RuntimeID), Kind: record.Kind, Status: record.Status, CreatedAt: timestampToString(record.CreatedAt), UpdatedAt: timestampToString(record.UpdatedAt)}
	if len(record.Result) > 0 {
		response.Result = publicWorkflowResult(record.Kind, record.Result)
	}
	if len(record.Error) > 0 {
		response.Error = append(json.RawMessage(nil), record.Error...)
	}
	return response
}

func workflowCommandFromRecord(record db.AgentWorkflowRequest) protocol.AgentWorkflowCommand {
	body := append(json.RawMessage(nil), record.Request...)
	if record.Kind == workflowKindNativeImport {
		// The ledger retains only server-validation metadata alongside the
		// canonical command. The daemon receives the public wire body exactly.
		var stored nativeImportCommand
		if json.Unmarshal(record.Request, &stored) == nil {
			body = mustJSON(struct {
				ImportID      string `json:"import_id"`
				ChatSessionID string `json:"chat_session_id"`
				AgentID       string `json:"agent_id"`
				Handle        string `json:"handle"`
				Revision      string `json:"revision"`
				NativeID      string `json:"native_id"`
			}{ImportID: stored.ImportID, ChatSessionID: stored.ChatSessionID, AgentID: stored.AgentID, Handle: stored.Handle, Revision: stored.Revision, NativeID: stored.NativeID})
		}
	}
	return protocol.AgentWorkflowCommand{
		ID:          uuidToString(record.ID),
		Kind:        record.Kind,
		RuntimeID:   uuidToString(record.RuntimeID),
		WorkspaceID: uuidToString(record.WorkspaceID),
		RequesterID: uuidToString(record.RequesterID),
		ExpiresAt:   record.ExpiresAt.Time,
		Body:        body,
	}
}

func publicWorkflowResult(kind string, result []byte) json.RawMessage {
	if kind != workflowKindNativeList {
		return append(json.RawMessage(nil), result...)
	}
	var stored nativeListResult
	if err := json.Unmarshal(result, &stored); err != nil {
		return nil
	}
	public := nativeListResultResponse{NextCursor: stored.NextCursor, Truncated: stored.Truncated, Sessions: make([]nativeListedSessionResponse, 0, len(stored.Sessions))}
	for _, session := range stored.Sessions {
		public.Sessions = append(public.Sessions, nativeListedSessionResponse{SessionRef: session.SessionRef, Revision: session.Revision, Provider: stored.Provider, Title: session.Title, Cwd: session.Cwd, Preview: session.Preview, UpdatedAt: session.UpdatedAt, Model: session.Model})
	}
	return mustJSON(public)
}

func (h *Handler) findNativeSessionReference(ctx context.Context, workspaceID, userID, runtimeID pgtype.UUID, ref, revision string) (nativeListedSession, error) {
	records, err := h.Queries.ListRecentNativeSessionListRequests(ctx, db.ListRecentNativeSessionListRequestsParams{WorkspaceID: workspaceID, RequesterID: userID, RuntimeID: runtimeID, MaxRequests: 20})
	if err != nil {
		return nativeListedSession{}, err
	}
	for _, record := range records {
		var result nativeListResult
		if json.Unmarshal(record.Result, &result) != nil {
			continue
		}
		for _, session := range result.Sessions {
			if session.SessionRef == ref && session.Revision == revision {
				return session, nil
			}
		}
	}
	return nativeListedSession{}, pgx.ErrNoRows
}

func opaqueSessionRef() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func (h *Handler) daemonWorkflowRuntime(w http.ResponseWriter, r *http.Request, runtimeID string) (db.AgentRuntime, bool) {
	if middleware.DaemonAuthPathFromContext(r.Context()) != middleware.DaemonAuthPathDaemonToken {
		writeErrorCode(w, http.StatusForbidden, "forbidden", "daemon token is required")
		return db.AgentRuntime{}, false
	}
	runtime, ok := h.requireDaemonRuntimeAccess(w, r, runtimeID)
	if !ok {
		return db.AgentRuntime{}, false
	}
	daemonID := middleware.DaemonIDFromContext(r.Context())
	if daemonID == "" || !runtime.DaemonID.Valid || runtime.DaemonID.String != daemonID {
		writeErrorCode(w, http.StatusNotFound, "not_found", "runtime not found")
		return db.AgentRuntime{}, false
	}
	return runtime, true
}

func (h *Handler) daemonWorkflowTask(w http.ResponseWriter, r *http.Request, taskID string) (db.AgentTaskQueue, db.AgentRuntime, bool) {
	task, ok := h.requireDaemonTaskAccess(w, r, taskID)
	if !ok {
		return db.AgentTaskQueue{}, db.AgentRuntime{}, false
	}
	if middleware.DaemonAuthPathFromContext(r.Context()) != middleware.DaemonAuthPathDaemonToken {
		writeErrorCode(w, http.StatusForbidden, "forbidden", "daemon token is required")
		return db.AgentTaskQueue{}, db.AgentRuntime{}, false
	}
	runtime, err := h.Queries.GetAgentRuntime(r.Context(), task.RuntimeID)
	if err != nil || !runtime.DaemonID.Valid || runtime.DaemonID.String != middleware.DaemonIDFromContext(r.Context()) {
		writeErrorCode(w, http.StatusNotFound, "not_found", "task not found")
		return db.AgentTaskQueue{}, db.AgentRuntime{}, false
	}
	return task, runtime, true
}

func (h *Handler) ReportTaskControls(w http.ResponseWriter, r *http.Request) {
	task, runtime, ok := h.daemonWorkflowTask(w, r, chi.URLParam(r, "taskId"))
	if !ok {
		return
	}
	var state protocol.TaskControlState
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&state); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if _, ok := parseUUIDOrBadRequest(w, state.RunID, "run_id"); !ok {
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to begin controls report")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	if _, err := qtx.LockChatSessionForTask(r.Context(), task.ID); err != nil {
		writeErrorCode(w, http.StatusConflict, "stale_turn", "chat session is unavailable")
		return
	}
	task, err = qtx.LockAgentTaskForControls(r.Context(), task.ID)
	if err != nil {
		writeErrorCode(w, http.StatusConflict, "stale_turn", "task is unavailable")
		return
	}
	if !task.ActiveRunID.Valid || uuidToString(task.ActiveRunID) != state.RunID || task.RuntimeID != runtime.ID || task.Status != "running" {
		writeErrorCode(w, http.StatusConflict, "stale_turn", "task run is not active")
		return
	}
	if state.Active && (state.TurnID == nil || strings.TrimSpace(*state.TurnID) == "") {
		writeErrorCode(w, http.StatusBadRequest, "invalid_request", "active controls require turn_id")
		return
	}
	stateJSON := mustJSON(state)
	if _, err := qtx.UpdateAgentTaskControlState(r.Context(), db.UpdateAgentTaskControlStateParams{ID: task.ID, RuntimeID: runtime.ID, ActiveRunID: task.ActiveRunID, ControlState: stateJSON}); err != nil {
		writeErrorCode(w, http.StatusConflict, "stale_turn", "task run is not active")
		return
	}
	if err := service.ReconcileTaskWorkflow(r.Context(), qtx, task.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to retire task inputs")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to commit controls report")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) ReportTaskInteraction(w http.ResponseWriter, r *http.Request) {
	task, runtime, ok := h.daemonWorkflowTask(w, r, chi.URLParam(r, "taskId"))
	if !ok {
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to begin interaction report")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	if _, err := qtx.LockChatSessionForTask(r.Context(), task.ID); err != nil {
		writeErrorCode(w, http.StatusConflict, "stale_turn", "chat session is unavailable")
		return
	}
	task, err = qtx.LockAgentTaskForControls(r.Context(), task.ID)
	if err != nil || task.RuntimeID != runtime.ID {
		writeErrorCode(w, http.StatusConflict, "stale_turn", "task is unavailable")
		return
	}
	if !task.ChatSessionID.Valid || !task.ActiveRunID.Valid || task.Status != "running" {
		writeErrorCode(w, http.StatusConflict, "stale_turn", "task has no active chat run")
		return
	}
	session, err := qtx.GetChatSession(r.Context(), task.ChatSessionID)
	if err != nil {
		writeErrorCode(w, http.StatusNotFound, "not_found", "chat session not found")
		return
	}
	var report daemonTaskInteractionReport
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&report); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	interactionID, valid := parseUUIDOrBadRequest(w, report.Interaction.ID, "interaction.id")
	if !valid {
		return
	}
	if _, valid := parseUUIDOrBadRequest(w, report.RunID, "run_id"); !valid {
		return
	}
	state, stateOK := taskControlState(task)
	if !stateOK || !state.Active || report.RunID != uuidToString(task.ActiveRunID) || report.Interaction.TurnID == "" || state.TurnID == nil || report.Interaction.TurnID != *state.TurnID {
		writeErrorCode(w, http.StatusConflict, "stale_turn", "task run no longer accepts interactions")
		return
	}
	if (report.Interaction.Kind != "approval" && report.Interaction.Kind != "question") || report.Interaction.ExpiresAt.Before(time.Now()) || interactionHasSecretQuestion(report.Interaction) {
		writeErrorCode(w, http.StatusBadRequest, "invalid_request", "invalid interaction request")
		return
	}
	request := mustJSON(report.Interaction)
	interaction, err := qtx.CreateTaskInteraction(r.Context(), db.CreateTaskInteractionParams{ID: interactionID, WorkspaceID: session.WorkspaceID, RuntimeID: runtime.ID, ChatSessionID: task.ChatSessionID, TaskID: task.ID, RunID: task.ActiveRunID, TurnID: report.Interaction.TurnID, Kind: report.Interaction.Kind, Request: request, ExpiresAt: pgtype.Timestamptz{Time: report.Interaction.ExpiresAt, Valid: true}})
	if errors.Is(err, pgx.ErrNoRows) {
		existing, lookupErr := qtx.GetTaskInteractionInChatSession(r.Context(), db.GetTaskInteractionInChatSessionParams{ID: interactionID, WorkspaceID: session.WorkspaceID, ChatSessionID: task.ChatSessionID})
		if lookupErr != nil || existing.RuntimeID != runtime.ID || existing.TaskID != task.ID || existing.RunID != task.ActiveRunID || existing.TurnID != report.Interaction.TurnID || existing.Kind != report.Interaction.Kind || !workflowJSONEqual(existing.Request, request) {
			writeErrorCode(w, http.StatusConflict, "idempotency_conflict", "interaction id is already in use")
			return
		}
		interaction = existing
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create interaction")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to commit interaction report")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": uuidToString(interaction.ID), "status": interaction.Status})
}

func interactionHasSecretQuestion(request protocol.AgentWorkflowInteractionRequest) bool {
	for _, question := range request.Questions {
		if question.Secret {
			return true
		}
	}
	return false
}
func workflowJSONEqual(left, right []byte) bool {
	var a, b any
	if json.Unmarshal(left, &a) != nil || json.Unmarshal(right, &b) != nil {
		return false
	}
	normalizedLeft, _ := json.Marshal(a)
	normalizedRight, _ := json.Marshal(b)
	return bytes.Equal(normalizedLeft, normalizedRight)
}

type daemonControlDeliveryResult struct {
	Delivery string `json:"delivery"`
	Code     string `json:"code"`
}

// completeControlWorkflow applies the server-owned terminal effect in the
// same transaction as the durable request. The daemon only acknowledges its
// provider write; it never gets to insert transcript rows or settle an
// interaction by itself.
func (h *Handler) completeControlWorkflow(r *http.Request, record db.AgentWorkflowRequest, runtime db.AgentRuntime, report daemonWorkflowResult) (db.AgentWorkflowRequest, *db.ChatMessage, error) {
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		return db.AgentWorkflowRequest{}, nil, err
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	_, session, task, err := lockWorkflowParents(r.Context(), qtx, record)
	if err != nil {
		return db.AgentWorkflowRequest{}, nil, errWorkflowNotFound
	}
	locked, err := qtx.LockAgentWorkflowRequestForRuntime(r.Context(), db.LockAgentWorkflowRequestForRuntimeParams{ID: record.ID, RuntimeID: runtime.ID})
	if err != nil {
		return db.AgentWorkflowRequest{}, nil, err
	}
	if locked.Status == "completed" || locked.Status == "failed" {
		return locked, nil, nil
	}
	// This is outcome reconciliation, not fresh admission. A dispatched command
	// may finish after its run, policy, or binding changed. Never execute it again.
	if !locked.DispatchedAt.Valid || !locked.TaskID.Valid || !locked.RunID.Valid || !locked.ChatSessionID.Valid || task.ID != locked.TaskID || task.ChatSessionID != locked.ChatSessionID || session.WorkspaceID != locked.WorkspaceID || session.CreatorID != locked.RequesterID {
		return db.AgentWorkflowRequest{}, nil, errWorkflowNotFound
	}
	if err := service.ReconcileTaskWorkflow(r.Context(), qtx, task.ID); err != nil {
		return db.AgentWorkflowRequest{}, nil, err
	}

	var message *db.ChatMessage
	var messageID *string
	if report.Status == "completed" {
		var delivery daemonControlDeliveryResult
		if json.Unmarshal(report.Result, &delivery) != nil || delivery.Delivery != "accepted" {
			return db.AgentWorkflowRequest{}, nil, errors.New("invalid control result")
		}
		switch locked.Kind {
		case workflowKindSteer:
			var command steerCommand
			if json.Unmarshal(locked.Request, &command) != nil || command.ChatSessionID != uuidToString(locked.ChatSessionID) || command.TaskID != uuidToString(locked.TaskID) || command.RunID != uuidToString(locked.RunID) || !locked.TurnID.Valid || command.TurnID != locked.TurnID.String || strings.TrimSpace(command.Content) == "" {
				return db.AgentWorkflowRequest{}, nil, errWorkflowNotFound
			}
			created, createErr := qtx.CreateSteeringChatMessage(r.Context(), db.CreateSteeringChatMessageParams{ID: dbid.NewV7(), ChatSessionID: locked.ChatSessionID, Content: command.Content, InputRequestID: locked.ID})
			if errors.Is(createErr, pgx.ErrNoRows) {
				existing, getErr := qtx.GetChatMessageByInputRequestID(r.Context(), locked.ID)
				if getErr != nil || existing.ChatSessionID != locked.ChatSessionID || existing.Role != "user" || existing.Content != command.Content {
					return db.AgentWorkflowRequest{}, nil, errWorkflowConflict
				}
				created = existing
			} else if createErr != nil {
				return db.AgentWorkflowRequest{}, nil, createErr
			} else {
				message = &created
			}
			id := uuidToString(created.ID)
			messageID = &id
		case workflowKindInteraction:
			var command interactionResponseCommand
			if json.Unmarshal(locked.Request, &command) != nil || command.ChatSessionID != uuidToString(locked.ChatSessionID) || command.TaskID != uuidToString(locked.TaskID) || command.RunID != uuidToString(locked.RunID) || !locked.TurnID.Valid || command.TurnID != locked.TurnID.String {
				return db.AgentWorkflowRequest{}, nil, errWorkflowNotFound
			}
			interactionID, parseErr := util.ParseUUID(command.InteractionID)
			if parseErr != nil {
				return db.AgentWorkflowRequest{}, nil, errWorkflowNotFound
			}
			if _, resolveErr := qtx.ResolveTaskInteraction(r.Context(), db.ResolveTaskInteractionParams{ID: interactionID, ResponseRequestID: locked.ID}); resolveErr != nil {
				existing, getErr := qtx.GetTaskInteractionInChatSession(r.Context(), db.GetTaskInteractionInChatSessionParams{ID: interactionID, WorkspaceID: locked.WorkspaceID, ChatSessionID: locked.ChatSessionID})
				if getErr != nil || existing.Status != "resolved" || !existing.ResponseRequestID.Valid || existing.ResponseRequestID != locked.ID {
					return db.AgentWorkflowRequest{}, nil, resolveErr
				}
			}
		}
	} else if locked.Kind == workflowKindInteraction {
		var command interactionResponseCommand
		if json.Unmarshal(locked.Request, &command) != nil {
			return db.AgentWorkflowRequest{}, nil, errWorkflowNotFound
		}
		interactionID, parseErr := util.ParseUUID(command.InteractionID)
		if parseErr != nil {
			return db.AgentWorkflowRequest{}, nil, errWorkflowNotFound
		}
		if report.Status == "unknown" {
			if _, markErr := qtx.MarkTaskInteractionUnknown(r.Context(), db.MarkTaskInteractionUnknownParams{ID: interactionID, ResponseRequestID: locked.ID}); markErr != nil && !errors.Is(markErr, pgx.ErrNoRows) {
				return db.AgentWorkflowRequest{}, nil, markErr
			}
		} else {
			state, valid := taskControlState(task)
			if task.Status == "running" && task.ActiveRunID == locked.RunID && valid && state.Active && state.TurnID != nil && *state.TurnID == locked.TurnID.String {
				if _, err := qtx.RestoreTaskInteractionPending(r.Context(), db.RestoreTaskInteractionPendingParams{ID: interactionID, ResponseRequestID: locked.ID, RunID: locked.RunID}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return db.AgentWorkflowRequest{}, nil, err
				}
			}
		}
	}

	var errorJSON []byte
	if report.Error != nil {
		errorJSON = mustJSON(report.Error)
	}
	delivery := "rejected"
	if report.Status == "completed" {
		delivery = "accepted"
	} else if report.Status == "unknown" {
		delivery = "unknown"
	}
	publicResult := mustJSON(map[string]any{"delivery": delivery, "message_id": messageID})
	completed, err := qtx.CompleteAgentWorkflowRequest(r.Context(), db.CompleteAgentWorkflowRequestParams{ID: locked.ID, RuntimeID: runtime.ID, Status: report.Status, Result: publicResult, Error: errorJSON})
	if err != nil {
		return db.AgentWorkflowRequest{}, nil, err
	}
	if err := tx.Commit(r.Context()); err != nil {
		return db.AgentWorkflowRequest{}, nil, err
	}
	return completed, message, nil
}

func (h *Handler) ReportAgentWorkflowResult(w http.ResponseWriter, r *http.Request) {
	runtimeID := chi.URLParam(r, "runtimeId")
	runtime, ok := h.daemonWorkflowRuntime(w, r, runtimeID)
	if !ok {
		return
	}
	requestID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "requestId"), "request_id")
	if !ok {
		return
	}
	record, err := h.Queries.GetAgentWorkflowRequestForRuntime(r.Context(), db.GetAgentWorkflowRequestForRuntimeParams{ID: requestID, RuntimeID: runtime.ID})
	if err != nil {
		writeErrorCode(w, http.StatusNotFound, "not_found", "workflow request not found")
		return
	}
	var report daemonWorkflowResult
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxWorkflowResultBytes)).Decode(&report); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if report.Status != "completed" && report.Status != "failed" && report.Status != "unknown" {
		writeErrorCode(w, http.StatusBadRequest, "invalid_request", "invalid workflow result status")
		return
	}
	if len(report.Result) > maxWorkflowResultBytes {
		writeErrorCode(w, http.StatusRequestEntityTooLarge, "result_too_large", "workflow result is too large")
		return
	}
	if record.Status == "completed" || record.Status == "failed" {
		writeJSON(w, http.StatusOK, workflowRecordResponse(record))
		return
	}
	if !record.DispatchedAt.Valid {
		writeErrorCode(w, http.StatusConflict, "not_dispatched", "workflow request was not dispatched")
		return
	}
	var acceptedSteer *db.ChatMessage
	if record.Kind == workflowKindSteer || record.Kind == workflowKindInteraction {
		record, acceptedSteer, err = h.completeControlWorkflow(r, record, runtime, report)
	} else if report.Status == "completed" && record.Kind == workflowKindNativeList {
		result, err := h.storeNativeListResult(report.Result, runtime.Provider)
		if err != nil {
			writeErrorCode(w, http.StatusBadRequest, "invalid_result", "invalid native session list result")
			return
		}
		record, err = h.Queries.CompleteAgentWorkflowRequest(r.Context(), db.CompleteAgentWorkflowRequestParams{ID: record.ID, RuntimeID: runtime.ID, Status: "completed", Result: mustJSON(result)})
	} else if report.Status == "completed" && record.Kind == workflowKindNativeImport {
		record, err = h.completeNativeImport(r, record, runtime, report.Result)
	} else {
		var errorJSON []byte
		if report.Error != nil {
			errorJSON = mustJSON(report.Error)
		}
		record, err = h.Queries.CompleteAgentWorkflowRequest(r.Context(), db.CompleteAgentWorkflowRequestParams{ID: record.ID, RuntimeID: runtime.ID, Status: report.Status, Result: report.Result, Error: errorJSON})
	}
	if err != nil {
		if errors.Is(err, errWorkflowNotFound) {
			writeErrorCode(w, http.StatusNotFound, "not_found", "workflow request not found")
		} else {
			writeError(w, http.StatusConflict, "workflow request is no longer pending")
		}
		return
	}
	if acceptedSteer != nil {
		h.publishChat(protocol.EventChatMessage, uuidToString(record.WorkspaceID), "member", uuidToString(record.RequesterID), uuidToString(acceptedSteer.ChatSessionID), protocol.ChatMessagePayload{ChatSessionID: uuidToString(acceptedSteer.ChatSessionID), MessageID: uuidToString(acceptedSteer.ID), Role: acceptedSteer.Role, Content: acceptedSteer.Content, CreatedAt: timestampToString(acceptedSteer.CreatedAt)})
	}
	writeJSON(w, http.StatusOK, workflowRecordResponse(record))
}

func (h *Handler) storeNativeListResult(raw []byte, provider string) (nativeListResult, error) {
	var report daemonNativeListResult
	if err := json.Unmarshal(raw, &report); err != nil {
		return nativeListResult{}, err
	}
	if len(report.Sessions) > 50 {
		return nativeListResult{}, errors.New("too many sessions")
	}
	result := nativeListResult{Provider: provider, NextCursor: report.NextCursor, Truncated: report.Truncated, Sessions: make([]nativeListedSession, 0, len(report.Sessions))}
	for _, session := range report.Sessions {
		if session.NativeID == "" || session.Revision == "" || session.Handle == "" {
			return nativeListResult{}, errors.New("invalid session")
		}
		ref, err := opaqueSessionRef()
		if err != nil {
			return nativeListResult{}, err
		}
		result.Sessions = append(result.Sessions, nativeListedSession{SessionRef: ref, NativeID: session.NativeID, Revision: session.Revision, Handle: session.Handle, Title: strings.TrimSpace(session.Title), Cwd: session.Cwd, Preview: session.Preview, UpdatedAt: session.UpdatedAt, Model: session.Model})
	}
	return result, nil
}

func (h *Handler) completeNativeImport(r *http.Request, record db.AgentWorkflowRequest, runtime db.AgentRuntime, raw json.RawMessage) (db.AgentWorkflowRequest, error) {
	var command nativeImportCommand
	if err := json.Unmarshal(record.Request, &command); err != nil {
		return db.AgentWorkflowRequest{}, err
	}
	var report daemonNativeImportResult
	if err := json.Unmarshal(raw, &report); err != nil {
		return db.AgentWorkflowRequest{}, err
	}
	if !record.ChatSessionID.Valid || command.ImportID != uuidToString(record.ID) || command.ChatSessionID != uuidToString(record.ChatSessionID) || report.NativeID != command.NativeID || report.OwnedNativeID == "" || report.Provider != runtime.Provider || report.SessionID == "" || report.WorkDir == "" || len(report.Messages) > 10000 || len(report.Warnings) > 100 {
		return db.AgentWorkflowRequest{}, errors.New("invalid import result")
	}
	seen := make(map[string]struct{}, len(report.Messages))
	var latestCreatedAt time.Time
	totalEvents := 0
	for _, message := range report.Messages {
		count := 0
		if len(message.Events) > 0 {
			var valid bool
			count, valid = importedEventCount(message.Events)
			if !valid {
				return db.AgentWorkflowRequest{}, errors.New("invalid imported events")
			}
		}
		if message.NativeID == "" || (message.Role != "user" && message.Role != "assistant") || (strings.TrimSpace(message.Content) == "" && !(message.Role == "assistant" && count > 0)) {
			return db.AgentWorkflowRequest{}, errors.New("invalid imported message")
		}
		if _, exists := seen[message.NativeID]; exists {
			return db.AgentWorkflowRequest{}, errors.New("duplicate imported message")
		}
		seen[message.NativeID] = struct{}{}
		createdAt, err := time.Parse(time.RFC3339Nano, message.CreatedAt)
		if err != nil || (!latestCreatedAt.IsZero() && createdAt.Before(latestCreatedAt)) {
			return db.AgentWorkflowRequest{}, errors.New("invalid imported timestamp")
		}
		latestCreatedAt = createdAt
		if message.Role != "assistant" && count > 0 {
			return db.AgentWorkflowRequest{}, errors.New("user imported events")
		}
		totalEvents += count
		if totalEvents > 50000 {
			return db.AgentWorkflowRequest{}, errors.New("too many imported events")
		}
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		return db.AgentWorkflowRequest{}, err
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	agent, _, _, err := lockWorkflowParents(r.Context(), qtx, record)
	if err != nil {
		return db.AgentWorkflowRequest{}, errWorkflowNotFound
	}
	locked, err := qtx.LockAgentWorkflowRequestForRuntime(r.Context(), db.LockAgentWorkflowRequestForRuntimeParams{ID: record.ID, RuntimeID: runtime.ID})
	if err != nil {
		return db.AgentWorkflowRequest{}, err
	}
	if locked.Status != "running" && locked.Status != "pending" && locked.Status != "unknown" {
		return locked, nil
	}
	if !locked.DispatchedAt.Valid {
		return db.AgentWorkflowRequest{}, errWorkflowNotFound
	}
	runtime, err = qtx.GetAgentRuntime(r.Context(), record.RuntimeID)
	if err != nil || !runtime.OwnerID.Valid || runtime.OwnerID != locked.RequesterID {
		return db.AgentWorkflowRequest{}, errWorkflowNotFound
	}
	if _, err := qtx.LockWorkflowMember(r.Context(), db.LockWorkflowMemberParams{WorkspaceID: locked.WorkspaceID, UserID: locked.RequesterID}); err != nil {
		return db.AgentWorkflowRequest{}, errWorkflowNotFound
	}
	if err != nil || agent.ArchivedAt.Valid || !agent.RuntimeID.Valid || agent.RuntimeID != runtime.ID || !h.canInvokeAgentWithQueries(r.Context(), qtx, agent, "member", uuidToString(locked.RequesterID), uuidToString(locked.RequesterID), uuidToString(locked.WorkspaceID)) {
		return db.AgentWorkflowRequest{}, errWorkflowNotFound
	}
	// Check for a concurrent successful import after acquiring the agent lock.
	// Existing chats need no mutation; their deleter takes this same agent lock
	// before pruning the request family. Never acquire a session after its agent.
	session, err := qtx.GetImportedChatSession(r.Context(), db.GetImportedChatSessionParams{WorkspaceID: locked.WorkspaceID, CreatorID: locked.RequesterID, RuntimeID: runtime.ID, NativeImportProvider: pgtype.Text{String: runtime.Provider, Valid: true}, NativeImportID: pgtype.Text{String: command.NativeID, Valid: true}})
	alreadyImported := err == nil
	if alreadyImported {
		if session.AgentID != agent.ID {
			return db.AgentWorkflowRequest{}, errWorkflowConflict
		}
	} else if errors.Is(err, pgx.ErrNoRows) {
		session, err = qtx.CreateImportedChatSession(r.Context(), db.CreateImportedChatSessionParams{ID: locked.ChatSessionID, WorkspaceID: locked.WorkspaceID, AgentID: parseUUID(command.AgentID), CreatorID: locked.RequesterID, Title: command.Title, RuntimeID: runtime.ID, SessionID: pgtype.Text{String: report.SessionID, Valid: true}, WorkDir: pgtype.Text{String: report.WorkDir, Valid: true}, NativeImportProvider: pgtype.Text{String: runtime.Provider, Valid: true}, NativeImportID: pgtype.Text{String: command.NativeID, Valid: true}, NativeImportRevision: pgtype.Text{String: command.Revision, Valid: true}})
		if err != nil {
			return db.AgentWorkflowRequest{}, err
		}
	} else {
		return db.AgentWorkflowRequest{}, err
	}
	if !alreadyImported {
		for _, message := range report.Messages {
			createdAt, _ := time.Parse(time.RFC3339Nano, message.CreatedAt)
			if _, err := qtx.CreateImportedChatMessage(r.Context(), db.CreateImportedChatMessageParams{ID: dbid.NewV7(), ChatSessionID: session.ID, Role: message.Role, Content: message.Content, CreatedAt: pgtype.Timestamptz{Time: createdAt, Valid: true}, ImportedEvents: message.Events, NativeMessageID: pgtype.Text{String: message.NativeID, Valid: true}}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return db.AgentWorkflowRequest{}, err
			}
		}
	}
	if !alreadyImported && !latestCreatedAt.IsZero() {
		if err := qtx.SetImportedChatSessionLastReadAt(r.Context(), db.SetImportedChatSessionLastReadAtParams{ID: session.ID, LastReadAt: pgtype.Timestamptz{Time: latestCreatedAt, Valid: true}}); err != nil {
			return db.AgentWorkflowRequest{}, err
		}
	}
	if len(report.Warnings) > 0 {
		for _, warning := range report.Warnings {
			if len(strings.TrimSpace(warning)) == 0 || len(warning) > 1024 {
				return db.AgentWorkflowRequest{}, errors.New("invalid import warning")
			}
		}
	}
	if report.Warnings == nil {
		report.Warnings = []string{}
	}
	result := map[string]any{"chat_session_id": uuidToString(session.ID), "already_imported": alreadyImported, "warnings": report.Warnings}
	completed, err := qtx.CompleteNativeImportWorkflowRequest(r.Context(), db.CompleteNativeImportWorkflowRequestParams{ID: locked.ID, RuntimeID: runtime.ID, ChatSessionID: session.ID, Result: mustJSON(result)})
	if err != nil {
		return db.AgentWorkflowRequest{}, err
	}
	if err := tx.Commit(r.Context()); err != nil {
		return db.AgentWorkflowRequest{}, err
	}
	return completed, nil
}

func importedEventCount(raw json.RawMessage) (int, bool) {
	var events []struct {
		Seq  int    `json:"seq"`
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &events) != nil || len(events) > 50000 {
		return 0, false
	}
	lastSeq := 0
	for _, event := range events {
		if event.Seq <= lastSeq {
			return 0, false
		}
		lastSeq = event.Seq
		switch event.Type {
		case "text", "thinking", "tool_use", "tool_result", "error":
		default:
			return 0, false
		}
	}
	return len(events), true
}
