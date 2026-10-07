package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// WorkSourceHandler serves work_source and issue_work_link endpoints.
// It embeds *Handler for the shared auth/workspace helpers; routes are
// wired by the root coordinator within the authenticated group.
type WorkSourceHandler struct {
	*Handler
	WorkSources *service.WorkSourceService
}

type workSourceResponse struct {
	ID                     string `json:"id"`
	WorkspaceID            string `json:"workspace_id"`
	ProjectID              string `json:"project_id,omitempty"`
	RuntimeID              string `json:"runtime_id"`
	DaemonID               string `json:"daemon_id"`
	Name                   string `json:"name"`
	Mode                   string `json:"mode"`
	Enabled                bool   `json:"enabled"`
	SourceHandle           string `json:"source_handle"`
	ConfigRevision         int32  `json:"config_revision"`
	LastHealth             string `json:"last_health,omitempty"`
	LastError              string `json:"last_error,omitempty"`
	CreatedBy              string `json:"created_by,omitempty"`
	CreatedAt              string `json:"created_at"`
	UpdatedAt              string `json:"updated_at"`
	NativeEnrollmentID     string `json:"native_enrollment_id,omitempty"`
	NativeEnrollmentStatus string `json:"native_enrollment_status,omitempty"`
	NativeManifestHash     string `json:"native_manifest_hash,omitempty"`
	NativeOwnerMemberID    string `json:"native_owner_member_id,omitempty"`
	NativeRuntimeCreatedAt string `json:"native_runtime_created_at,omitempty"`
	NativeEnrolledAt       string `json:"native_enrolled_at,omitempty"`
}

type createWorkSourceRequest struct {
	ProjectID    string `json:"project_id"`
	RuntimeID    string `json:"runtime_id"`
	Name         string `json:"name"`
	SourceHandle string `json:"source_handle"`
}

type updateWorkSourceRequest struct {
	Name    string `json:"name"`
	Enabled *bool  `json:"enabled"`
}

type issueWorkLinkResponse struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	IssueID     string `json:"issue_id"`
	SourceID    string `json:"source_id"`
	NativeID    string `json:"native_id"`
	CreatedBy   string `json:"created_by,omitempty"`
	CreatedAt   string `json:"created_at"`
}

type createIssueWorkLinkRequest struct {
	IssueID  string `json:"issue_id"`
	SourceID string `json:"source_id"`
	NativeID string `json:"native_id"`
}

// decodeWorkSourceRequest reads one bounded JSON body, answering 400 on
// malformed input.
func decodeWorkSourceRequest(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

// requireWorkSourceAdmin validates the header workspace as a UUID and gates
// the request on an owner/admin membership.
func (h *WorkSourceHandler) requireWorkSourceAdmin(w http.ResponseWriter, r *http.Request) (pgtype.UUID, db.Member, bool) {
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

// handleWorkSourceError maps service errors to status codes without leaking
// driver diagnostics; anything unrecognized is a 500.
func handleWorkSourceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrWorkSourceNotFound):
		writeError(w, http.StatusNotFound, "work source not found")
	case errors.Is(err, service.ErrWorkSourceOwnerConflict):
		writeError(w, http.StatusConflict, "physical source already registered")
	case errors.Is(err, service.ErrWorkSourceInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid work source request")
	case errors.Is(err, service.ErrNativeEnrollmentConflict):
		writeError(w, http.StatusConflict, "native source awaits enrollment approval")
	case isUniqueViolation(err):
		writeError(w, http.StatusConflict, "physical source already registered")
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// CreateWorkSource registers a workspace/project source binding.
// mode is pinned server-side to 'observe' (read-only) per the qualified
// source contract; the request carries no executable/path, only the opaque
// approved source handle plus the owning runtime.
func (h *WorkSourceHandler) CreateWorkSource(w http.ResponseWriter, r *http.Request) {
	wsUUID, member, ok := h.requireWorkSourceAdmin(w, r)
	if !ok {
		return
	}
	var req createWorkSourceRequest
	if !decodeWorkSourceRequest(w, r, &req) {
		return
	}
	runtimeUUID, ok := parseUUIDOrBadRequest(w, req.RuntimeID, "runtime id")
	if !ok {
		return
	}
	var projectUUID *pgtype.UUID
	if req.ProjectID != "" {
		p, ok := parseUUIDOrBadRequest(w, req.ProjectID, "project id")
		if !ok {
			return
		}
		projectUUID = &p
	}
	// source_handle is opaque identity: keep the original bytes, reject only
	// empty/whitespace-only values (name may trim).
	if req.SourceHandle == "" || strings.TrimSpace(req.SourceHandle) == "" {
		writeError(w, http.StatusBadRequest, "source_handle is required")
		return
	}
	source, err := h.WorkSources.CreateWorkSource(r.Context(), service.CreateWorkSourceParams{
		WorkspaceID:  wsUUID,
		ProjectID:    projectUUID,
		RuntimeID:    runtimeUUID,
		Name:         strings.TrimSpace(req.Name),
		SourceHandle: req.SourceHandle,
		CreatedBy:    member.UserID,
	})
	if err != nil {
		handleWorkSourceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, workSourceToResponse(source))
}

// UpdateWorkSource renames or toggles the binding. Identity - scope and
// owner - and source_handle are immutable so the source UUID stays a stable
// pointer to one physical source across name/config changes.
func (h *WorkSourceHandler) UpdateWorkSource(w http.ResponseWriter, r *http.Request) {
	wsUUID, _, ok := h.requireWorkSourceAdmin(w, r)
	if !ok {
		return
	}
	sourceUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "sourceID"), "source id")
	if !ok {
		return
	}
	var req updateWorkSourceRequest
	if !decodeWorkSourceRequest(w, r, &req) {
		return
	}
	source, err := h.WorkSources.UpdateWorkSource(r.Context(), wsUUID, sourceUUID,
		service.WorkSourceConfig{Name: strings.TrimSpace(req.Name), Enabled: req.Enabled})
	if err != nil {
		handleWorkSourceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workSourceToResponse(source))
}

// DeleteWorkSource removes the binding. Links are swept in the same
// transaction so no orphan issue_work_link rows survive (no FKs by project
// rule).
func (h *WorkSourceHandler) DeleteWorkSource(w http.ResponseWriter, r *http.Request) {
	wsUUID, _, ok := h.requireWorkSourceAdmin(w, r)
	if !ok {
		return
	}
	sourceUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "sourceID"), "source id")
	if !ok {
		return
	}
	if err := h.WorkSources.DeleteWorkSourceCascade(r.Context(), wsUUID, sourceUUID); err != nil {
		handleWorkSourceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListWorkSources lists the workspace's source bindings.
func (h *WorkSourceHandler) ListWorkSources(w http.ResponseWriter, r *http.Request) {
	wsUUID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace id")
	if !ok {
		return
	}
	if _, ok := h.workspaceMember(w, r, wsUUID.String()); !ok {
		return
	}
	sources, err := h.WorkSources.ListWorkSources(r.Context(), wsUUID)
	if err != nil {
		handleWorkSourceError(w, err)
		return
	}
	resp := make([]workSourceResponse, len(sources))
	for i, s := range sources {
		resp[i] = workSourceToResponse(s)
	}
	writeJSON(w, http.StatusOK, resp)
}

// CreateIssueWorkLink links an existing Issue to a source-owned native work
// item. It never creates or deletes either object.
func (h *WorkSourceHandler) CreateIssueWorkLink(w http.ResponseWriter, r *http.Request) {
	wsUUID, member, ok := h.requireWorkSourceAdmin(w, r)
	if !ok {
		return
	}
	var req createIssueWorkLinkRequest
	if !decodeWorkSourceRequest(w, r, &req) {
		return
	}
	issueUUID, ok := parseUUIDOrBadRequest(w, req.IssueID, "issue id")
	if !ok {
		return
	}
	sourceUUID, ok := parseUUIDOrBadRequest(w, req.SourceID, "source id")
	if !ok {
		return
	}
	// native_id is opaque identity: keep the original bytes, reject only
	// empty/whitespace-only values.
	if req.NativeID == "" || strings.TrimSpace(req.NativeID) == "" {
		writeError(w, http.StatusBadRequest, "native_id is required")
		return
	}
	link, err := h.WorkSources.CreateIssueWorkLink(r.Context(), service.CreateIssueWorkLinkParams{
		WorkspaceID: wsUUID,
		IssueID:     issueUUID,
		SourceID:    sourceUUID,
		NativeID:    req.NativeID,
		CreatedBy:   member.UserID,
	})
	if err != nil {
		handleWorkSourceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, linkToResponse(link))
}

// DeleteIssueWorkLink unlinks; no issue or source side effect.
func (h *WorkSourceHandler) DeleteIssueWorkLink(w http.ResponseWriter, r *http.Request) {
	wsUUID, _, ok := h.requireWorkSourceAdmin(w, r)
	if !ok {
		return
	}
	linkUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "linkID"), "link id")
	if !ok {
		return
	}
	if err := h.WorkSources.DeleteIssueWorkLink(r.Context(), wsUUID, linkUUID); err != nil {
		handleWorkSourceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListIssueWorkLinks lists links for one issue (issue_id query param) or
// one source (source_id query param).
func (h *WorkSourceHandler) ListIssueWorkLinks(w http.ResponseWriter, r *http.Request) {
	wsUUID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace id")
	if !ok {
		return
	}
	if _, ok := h.workspaceMember(w, r, wsUUID.String()); !ok {
		return
	}
	if issueParam := r.URL.Query().Get("issue_id"); issueParam != "" {
		issueUUID, ok := parseUUIDOrBadRequest(w, issueParam, "issue id")
		if !ok {
			return
		}
		links, err := h.WorkSources.ListIssueWorkLinksByIssue(r.Context(), wsUUID, issueUUID)
		if err != nil {
			handleWorkSourceError(w, err)
			return
		}
		h.writeLinks(w, links)
		return
	}
	if sourceParam := r.URL.Query().Get("source_id"); sourceParam != "" {
		sourceUUID, ok := parseUUIDOrBadRequest(w, sourceParam, "source id")
		if !ok {
			return
		}
		links, err := h.WorkSources.ListIssueWorkLinksBySource(r.Context(), wsUUID, sourceUUID)
		if err != nil {
			handleWorkSourceError(w, err)
			return
		}
		h.writeLinks(w, links)
		return
	}
	writeError(w, http.StatusBadRequest, "issue_id or source_id is required")
}

func workSourceToResponse(s db.WorkSource) workSourceResponse {
	resp := workSourceResponse{
		ID:             uuidToString(s.ID),
		WorkspaceID:    uuidToString(s.WorkspaceID),
		RuntimeID:      uuidToString(s.RuntimeID),
		DaemonID:       s.DaemonID,
		Name:           s.Name,
		Mode:           s.Mode,
		Enabled:        s.Enabled,
		SourceHandle:   s.SourceHandle,
		ConfigRevision: s.ConfigRevision,
		CreatedAt:      s.CreatedAt.Time.Format(time.RFC3339),
		UpdatedAt:      s.UpdatedAt.Time.Format(time.RFC3339),
	}
	if s.ProjectID.Valid {
		resp.ProjectID = uuidToString(s.ProjectID)
	}
	if s.LastHealth.Valid {
		resp.LastHealth = s.LastHealth.String
	}
	if s.LastError.Valid {
		resp.LastError = s.LastError.String
	}
	if s.CreatedBy.Valid {
		resp.CreatedBy = uuidToString(s.CreatedBy)
	}
	if s.NativeEnrollmentID.Valid {
		resp.NativeEnrollmentID = s.NativeEnrollmentID.String()
		resp.NativeEnrollmentStatus = "pending"
		resp.NativeOwnerMemberID = s.NativeOwnerMemberID.String()
		resp.NativeRuntimeCreatedAt = s.NativeRuntimeCreatedAt.Time.UTC().Format(time.RFC3339Nano)
		if s.NativeManifestHash.Valid {
			resp.NativeManifestHash = s.NativeManifestHash.String
		}
		if s.NativeEnrolledAt.Valid {
			resp.NativeEnrollmentStatus = "enrolled"
			resp.NativeEnrolledAt = s.NativeEnrolledAt.Time.UTC().Format(time.RFC3339Nano)
		}
	}
	return resp
}

func linkToResponse(l db.IssueWorkLink) issueWorkLinkResponse {
	resp := issueWorkLinkResponse{
		ID:          uuidToString(l.ID),
		WorkspaceID: uuidToString(l.WorkspaceID),
		IssueID:     uuidToString(l.IssueID),
		SourceID:    uuidToString(l.SourceID),
		NativeID:    l.NativeID,
		CreatedAt:   l.CreatedAt.Time.Format(time.RFC3339),
	}
	if l.CreatedBy.Valid {
		resp.CreatedBy = uuidToString(l.CreatedBy)
	}
	return resp
}

func (h *WorkSourceHandler) writeLinks(w http.ResponseWriter, links []db.IssueWorkLink) {
	resp := make([]issueWorkLinkResponse, len(links))
	for i, l := range links {
		resp[i] = linkToResponse(l)
	}
	writeJSON(w, http.StatusOK, resp)
}
