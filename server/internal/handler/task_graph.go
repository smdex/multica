package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TaskGraphHandler exposes nonexecuting frozen observation drafts only.
type TaskGraphHandler struct {
	*Handler
	Graphs *service.TaskGraphService
}

type createWorkflowDraftRequest struct {
	RequestID              string   `json:"request_id"`
	SourceID               string   `json:"source_id"`
	RootNativeID           string   `json:"root_native_id"`
	ExpectedRootRevision   string   `json:"expected_root_revision"`
	ExpectedConfigRevision int32    `json:"expected_config_revision"`
	Capacity               int32    `json:"capacity"`
	ReceiptIDs             []string `json:"receipt_ids"`
}

type workflowDraftResponse struct {
	ID             string          `json:"id"`
	WorkspaceID    string          `json:"workspace_id"`
	ProjectID      string          `json:"project_id,omitempty"`
	SourceID       string          `json:"source_id"`
	RequestID      string          `json:"request_id"`
	RootNativeID   string          `json:"root_native_id"`
	ConfigRevision int32           `json:"config_revision"`
	Capacity       int32           `json:"capacity"`
	Status         string          `json:"status"`
	Graph          json.RawMessage `json:"graph"`
	NodeState      json.RawMessage `json:"node_state"`
	CreatedBy      string          `json:"created_by"`
	CreatedAt      string          `json:"created_at"`
}

func workflowDraftToResponse(run db.WorkflowRun) workflowDraftResponse {
	return workflowDraftResponse{ID: uuidToString(run.ID), WorkspaceID: uuidToString(run.WorkspaceID), ProjectID: uuidToString(run.ProjectID), SourceID: uuidToString(run.SourceID), RequestID: uuidToString(run.RequestID), RootNativeID: run.RootNativeID, ConfigRevision: run.ConfigRevision, Capacity: run.Capacity, Status: run.Status, Graph: json.RawMessage(run.Graph), NodeState: json.RawMessage(run.NodeState), CreatedBy: uuidToString(run.CreatedBy), CreatedAt: run.CreatedAt.Time.Format(time.RFC3339Nano)}
}

func handleWorkflowDraftError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrDraftInvalid):
		writeError(w, http.StatusBadRequest, "invalid workflow draft request")
	case errors.Is(err, service.ErrDraftNotFound), errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "workflow draft or source not found")
	case errors.Is(err, service.ErrDraftForbidden):
		writeError(w, http.StatusForbidden, "admin role required")
	case errors.Is(err, service.ErrDraftConflict):
		writeError(w, http.StatusConflict, "workflow draft precondition or request identity changed")
	case errors.Is(err, service.ErrDraftIneligible):
		writeError(w, http.StatusUnprocessableEntity, "receipts do not establish a supported complete root closure")
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func (h *TaskGraphHandler) CreateWorkflowDraft(w http.ResponseWriter, r *http.Request) {
	ws, member, ok := (&WorkSourceHandler{Handler: h.Handler}).requireWorkSourceAdmin(w, r)
	if !ok {
		return
	}
	var req createWorkflowDraftRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil || decoder.Decode(new(any)) != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid workflow draft request")
		return
	}
	requestID, ok := parseUUIDOrBadRequest(w, req.RequestID, "request id")
	if !ok {
		return
	}
	sourceID, ok := parseUUIDOrBadRequest(w, req.SourceID, "source id")
	if !ok {
		return
	}
	if len(req.ReceiptIDs) < 1 || len(req.ReceiptIDs) > 128 {
		writeError(w, http.StatusBadRequest, "receipt_ids must contain 1..128 UUIDs")
		return
	}
	ids := make([]pgtype.UUID, len(req.ReceiptIDs))
	for i, id := range req.ReceiptIDs {
		ids[i], ok = parseUUIDOrBadRequest(w, id, "receipt id")
		if !ok {
			return
		}
	}
	run, created, err := h.Graphs.CreateWorkflowDraft(r.Context(), service.CreateWorkflowDraftParams{WorkspaceID: ws, SourceID: sourceID, RequestID: requestID, CreatedBy: member.UserID, RootNativeID: req.RootNativeID, ExpectedRootRevision: req.ExpectedRootRevision, ExpectedConfigRevision: req.ExpectedConfigRevision, Capacity: req.Capacity, ReceiptIDs: ids})
	if err != nil {
		handleWorkflowDraftError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, workflowDraftToResponse(run))
}

func (h *TaskGraphHandler) GetWorkflowDraft(w http.ResponseWriter, r *http.Request) {
	ws, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace id")
	if !ok {
		return
	}
	if _, ok = h.workspaceMember(w, r, ws.String()); !ok {
		return
	}
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "runID"), "workflow run id")
	if !ok {
		return
	}
	run, err := h.Queries.GetWorkflowRunInWorkspace(r.Context(), db.GetWorkflowRunInWorkspaceParams{ID: id, WorkspaceID: ws})
	if err != nil {
		handleWorkflowDraftError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workflowDraftToResponse(run))
}
