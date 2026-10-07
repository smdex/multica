package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// WorkSourceCommandHandler serves the read-only source command and receipt
// endpoints. It embeds *Handler for the shared auth/workspace and daemon
// runtime helpers; routes are wired by the root coordinator.
type WorkSourceCommandHandler struct {
	*Handler
	Commands *service.WorkSourceCommandService
}

// maxWorkSourceCommandResultBytes bounds the daemon's result payload so a
// pathological source cannot blow up the receipt row (and every reader).
const maxWorkSourceCommandResultBytes = 2 << 20

// maxWorkSourceCommandErrorBytes bounds the failure diagnostic.
const maxWorkSourceCommandErrorBytes = 8 << 10

type workSourceCommandResponse struct {
	RequestID        string `json:"request_id"`
	ConfigRevision   int32  `json:"config_revision"`
	ExpiresAt        string `json:"expires_at"`
	ID               string `json:"id"`
	WorkspaceID      string `json:"workspace_id"`
	SourceID         string `json:"source_id"`
	Command          string `json:"command"`
	NativeID         string `json:"native_id,omitempty"`
	LimitCount       int32  `json:"limit_count,omitempty"`
	Status           string `json:"status"`
	ClaimedRuntimeID string `json:"claimed_runtime_id,omitempty"`
	ClaimedAt        string `json:"claimed_at,omitempty"`
	Result           string `json:"result,omitempty"`
	Error            string `json:"error,omitempty"`
	CreatedBy        string `json:"created_by,omitempty"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
}

type createWorkSourceCommandRequest struct {
	RequestID string `json:"request_id"`
	Command   string `json:"command"`
	NativeID  string `json:"native_id"`
	Limit     int32  `json:"limit"`
}

type reportWorkSourceCommandRequest struct {
	Status string `json:"status"`
	Result string `json:"result"`
	Error  string `json:"error"`
}

// handleWorkSourceCommandError maps service errors to status codes without
// leaking driver diagnostics.
func handleWorkSourceCommandError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrWorkSourceCommandNotFound),
		errors.Is(err, service.ErrWorkSourceNotFound):
		writeError(w, http.StatusNotFound, "source command not found")
	case errors.Is(err, service.ErrWorkSourceCommandConflict):
		writeError(w, http.StatusConflict, "another source command is in flight")
	case errors.Is(err, service.ErrWorkSourceCommandDisabled):
		writeError(w, http.StatusConflict, "work source is disabled")
	case errors.Is(err, service.ErrWorkSourceCommandNotClaimable):
		writeError(w, http.StatusConflict, "source command is not pending")
	case errors.Is(err, service.ErrWorkSourceCommandNotClaimer):
		writeError(w, http.StatusForbidden, "runtime does not own this source command")
	case errors.Is(err, service.ErrWorkSourceCommandInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid source command request")
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// CreateWorkSourceCommand accepts one allowlisted read command for a bound
// source. The command name is matched against the server-side allowlist;
// there is no command string, executable, or path in the request. 201 for
// a new receipt, 200 for an idempotent retry of the same in-flight
// request.
func (h *WorkSourceCommandHandler) CreateWorkSourceCommand(w http.ResponseWriter, r *http.Request) {
	wsUUID, member, ok := h.requireWorkSourceAdminFromBase(w, r)
	if !ok {
		return
	}
	sourceUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "sourceID"), "source id")
	if !ok {
		return
	}
	var req createWorkSourceCommandRequest
	if !decodeWorkSourceCommandRequest(w, r, &req) {
		return
	}
	requestID, ok := parseUUIDOrBadRequest(w, req.RequestID, "request id")
	if !ok {
		return
	}
	var nativeID string
	var limit int32
	switch service.WorkSourceCommandName(req.Command) {
	case service.WorkSourceCommandRead:
		if req.Limit != 0 {
			writeError(w, http.StatusBadRequest, "limit is only valid for list")
			return
		}
		if strings.TrimSpace(req.NativeID) == "" {
			writeError(w, http.StatusBadRequest, "native_id is required for read")
			return
		}
		// native_id is opaque source identity: keep the original bytes.
		nativeID = req.NativeID
	case service.WorkSourceCommandList:
		if req.NativeID != "" {
			writeError(w, http.StatusBadRequest, "native_id is only valid for read")
			return
		}
		limit = req.Limit
	default:
		writeError(w, http.StatusBadRequest, "unsupported command")
		return
	}
	cmd, created, err := h.Commands.CreateWorkSourceCommand(r.Context(), service.CreateWorkSourceCommandParams{
		WorkspaceID: wsUUID,
		SourceID:    sourceUUID,
		Command:     service.WorkSourceCommandName(req.Command),
		NativeID:    nativeID,
		Limit:       limit,
		CreatedBy:   member.UserID,
		RequestID:   requestID,
	})
	if err != nil {
		handleWorkSourceCommandError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, workSourceCommandToResponse(cmd))
}

// GetWorkSourceCommand returns one receipt to a workspace member.
func (h *WorkSourceCommandHandler) GetWorkSourceCommand(w http.ResponseWriter, r *http.Request) {
	wsUUID, ok := h.workspaceMemberFromBase(w, r)
	if !ok {
		return
	}
	commandUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "commandID"), "command id")
	if !ok {
		return
	}
	cmd, err := h.Commands.GetWorkSourceCommand(r.Context(), wsUUID, commandUUID)
	if err != nil {
		handleWorkSourceCommandError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workSourceCommandToResponse(cmd))
}

// ListWorkSourceCommands returns a source's receipts, newest first.
func (h *WorkSourceCommandHandler) ListWorkSourceCommands(w http.ResponseWriter, r *http.Request) {
	wsUUID, ok := h.workspaceMemberFromBase(w, r)
	if !ok {
		return
	}
	sourceUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "sourceID"), "source id")
	if !ok {
		return
	}
	cmds, err := h.Commands.ListWorkSourceCommands(r.Context(), wsUUID, sourceUUID)
	if err != nil {
		handleWorkSourceCommandError(w, err)
		return
	}
	resp := make([]workSourceCommandResponse, len(cmds))
	for i, c := range cmds {
		resp[i] = workSourceCommandToResponse(c)
		resp[i].Result = ""
	}
	writeJSON(w, http.StatusOK, resp)
}

// ClaimWorkSourceCommand is the daemon-owner claim endpoint: exactly one
// runtime of the source's owning daemon transitions pending -> claimed.
func (h *WorkSourceCommandHandler) ClaimWorkSourceCommand(w http.ResponseWriter, r *http.Request) {
	runtime, ok := h.requireWorkSourceCommandRuntime(w, r)
	if !ok {
		return
	}
	commandUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "commandId"), "command id")
	if !ok {
		return
	}
	cmd, err := h.Commands.ClaimWorkSourceCommand(r.Context(), runtime.WorkspaceID, commandUUID, runtime)
	if err != nil {
		handleWorkSourceCommandError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workSourceCommandToResponse(cmd))
}

// ReportWorkSourceCommand is the daemon-owner terminal report: succeeded
// with a bounded result or failed with a bounded diagnostic. A replay
// against an already-terminal receipt returns the stored receipt.
func (h *WorkSourceCommandHandler) ReportWorkSourceCommand(w http.ResponseWriter, r *http.Request) {
	runtime, ok := h.requireWorkSourceCommandRuntime(w, r)
	if !ok {
		return
	}
	commandUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "commandId"), "command id")
	if !ok {
		return
	}
	var req reportWorkSourceCommandRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 6*maxWorkSourceCommandResultBytes+(16<<10)))
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if decoder.Decode(new(any)) != io.EOF || len(req.Result) > maxWorkSourceCommandResultBytes || len(req.Error) > maxWorkSourceCommandErrorBytes {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if (req.Status == "succeeded" && req.Error != "") || (req.Status == "failed" && req.Result != "") {
		writeError(w, http.StatusBadRequest, "invalid report payload")
		return
	}
	switch req.Status {
	case "succeeded":
		var result pgtype.Text
		if strings.TrimSpace(req.Result) != "" {
			result = pgtype.Text{String: req.Result, Valid: true}
		} else {
			writeError(w, http.StatusBadRequest, "result is required for succeeded")
			return
		}
		cmd, err := h.Commands.ReportWorkSourceCommand(r.Context(), service.ReportWorkSourceCommandParams{
			WorkspaceID: runtime.WorkspaceID, CommandID: commandUUID, Runtime: runtime,
			Status: "succeeded", Result: result,
		})
		if err != nil {
			handleWorkSourceCommandError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, workSourceCommandToResponse(cmd))
	case "failed":
		var errText pgtype.Text
		if strings.TrimSpace(req.Error) != "" {
			errText = pgtype.Text{String: req.Error, Valid: true}
		}
		cmd, err := h.Commands.ReportWorkSourceCommand(r.Context(), service.ReportWorkSourceCommandParams{
			WorkspaceID: runtime.WorkspaceID, CommandID: commandUUID, Runtime: runtime,
			Status: "failed", Error: errText,
		})
		if err != nil {
			handleWorkSourceCommandError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, workSourceCommandToResponse(cmd))
	default:
		writeError(w, http.StatusBadRequest, "status must be succeeded or failed")
	}
}

// requireWorkSourceAdminFromBase and workspaceMemberFromBase mirror
// WorkSourceHandler's guards on this handler's embedded *Handler.
func (h *WorkSourceCommandHandler) requireWorkSourceAdminFromBase(w http.ResponseWriter, r *http.Request) (pgtype.UUID, db.Member, bool) {
	wsUUID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace id")
	if !ok {
		return pgtype.UUID{}, db.Member{}, false
	}
	member, ok := h.workspaceMember(w, r, wsUUID.String())
	if !ok {
		return pgtype.UUID{}, db.Member{}, false
	}
	if !roleAllowed(member.Role, "owner", "admin") {
		writeError(w, http.StatusForbidden, "admin role required")
		return pgtype.UUID{}, db.Member{}, false
	}
	return wsUUID, member, true
}

func (h *WorkSourceCommandHandler) workspaceMemberFromBase(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	wsUUID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace id")
	if !ok {
		return pgtype.UUID{}, false
	}
	if _, ok := h.workspaceMember(w, r, wsUUID.String()); !ok {
		return pgtype.UUID{}, false
	}
	return wsUUID, true
}

// decodeWorkSourceCommandRequest reads one bounded JSON body (16 KiB).
func decodeWorkSourceCommandRequest(w http.ResponseWriter, r *http.Request, dst any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	if err := decoder.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	if decoder.Decode(new(any)) != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

// These endpoints require machine identity, not the legacy workspace-user
// fallback, which cannot prove possession of the addressed daemon identity.
func (h *WorkSourceCommandHandler) requireWorkSourceCommandRuntime(w http.ResponseWriter, r *http.Request) (db.AgentRuntime, bool) {
	runtime, ok := h.requireDaemonRuntimeAccess(w, r, chi.URLParam(r, "runtimeId"))
	if !ok {
		return runtime, false
	}
	daemonID := middleware.DaemonIDFromContext(r.Context())
	if daemonID == "" || !runtime.DaemonID.Valid || daemonID != runtime.DaemonID.String {
		writeError(w, http.StatusForbidden, "daemon identity does not match runtime")
		return runtime, false
	}
	return runtime, true
}

func workSourceCommandToResponse(c db.WorkSourceCommand) workSourceCommandResponse {
	resp := workSourceCommandResponse{
		RequestID:      uuidToString(c.RequestID),
		ConfigRevision: c.ConfigRevision,
		ExpiresAt:      c.ExpiresAt.Time.Format(time.RFC3339),
		ID:             uuidToString(c.ID),
		WorkspaceID:    uuidToString(c.WorkspaceID),
		SourceID:       uuidToString(c.SourceID),
		Command:        c.Command,
		Status:         c.Status,
		CreatedAt:      c.CreatedAt.Time.Format(time.RFC3339),
		UpdatedAt:      c.UpdatedAt.Time.Format(time.RFC3339),
	}
	if c.NativeID.Valid {
		resp.NativeID = c.NativeID.String
	}
	if c.LimitCount.Valid {
		resp.LimitCount = c.LimitCount.Int32
	}
	if c.ClaimedRuntimeID.Valid {
		resp.ClaimedRuntimeID = uuidToString(c.ClaimedRuntimeID)
	}
	if c.ClaimedAt.Valid {
		resp.ClaimedAt = c.ClaimedAt.Time.Format(time.RFC3339)
	}
	if c.Result.Valid {
		resp.Result = c.Result.String
	}
	if c.Error.Valid {
		resp.Error = c.Error.String
	}
	if c.CreatedBy.Valid {
		resp.CreatedBy = uuidToString(c.CreatedBy)
	}
	return resp
}
