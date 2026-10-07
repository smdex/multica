package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type nativeEnrollmentProofRequest struct {
	EnrollmentID   string `json:"enrollment_id"`
	ConfigRevision int32  `json:"config_revision"`
	ManifestHash   string `json:"manifest_hash"`
}

func decodeNativeEnrollmentRequest(w http.ResponseWriter, r *http.Request, dst any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048))
	decoder.DisallowUnknownFields()
	if decoder.Decode(dst) != nil || decoder.Decode(new(any)) != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid enrollment request")
		return false
	}
	return true
}

func nativeEnrollmentProof(w http.ResponseWriter, req nativeEnrollmentProofRequest) (service.NativeEnrollmentProof, bool) {
	id, ok := parseUUIDOrBadRequest(w, req.EnrollmentID, "enrollment id")
	return service.NativeEnrollmentProof{EnrollmentID: id, ConfigRevision: req.ConfigRevision, ManifestHash: req.ManifestHash}, ok
}

func handleNativeEnrollmentError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrNativeEnrollmentConflict):
		writeError(w, http.StatusConflict, "native source enrollment has changed")
	case errors.Is(err, service.ErrWorkSourceInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid enrollment request")
	default:
		handleWorkSourceCommandError(w, err)
	}
}

func (h *WorkSourceCommandHandler) nativeEnrollmentRuntime(w http.ResponseWriter, r *http.Request) (db.AgentRuntime, bool) {
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "runtimeId"), "runtime id")
	if !ok {
		return db.AgentRuntime{}, false
	}
	runtime, err := h.Queries.GetAgentRuntime(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "runtime not found")
		return runtime, false
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "enrollment authorization unavailable")
		return runtime, false
	}
	if r.Header.Get("X-Workspace-ID") != runtime.WorkspaceID.String() {
		writeError(w, http.StatusNotFound, "workspace not found")
		return runtime, false
	}
	return runtime, true
}

func (h *WorkSourceCommandHandler) CreateNativeSourceIntent(w http.ResponseWriter, r *http.Request) {
	ownerID, patHash, parentExpiry, ok := h.sourceReadParent(w, r)
	if !ok {
		return
	}
	runtime, ok := h.nativeEnrollmentRuntime(w, r)
	if !ok {
		return
	}
	var req struct {
		RequestID string `json:"request_id"`
		Name      string `json:"name"`
	}
	if !decodeNativeEnrollmentRequest(w, r, &req) {
		return
	}
	requestID, ok := parseUUIDOrBadRequest(w, req.RequestID, "request id")
	if !ok {
		return
	}
	source, created, err := h.Commands.CreateNativeSourceIntent(r.Context(), runtime.WorkspaceID, runtime.ID, ownerID, requestID, req.Name, patHash, parentExpiry)
	if err != nil {
		handleNativeEnrollmentError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, workSourceToResponse(source))
}

func (h *WorkSourceCommandHandler) MintSourceEnrollmentToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ownerID, patHash, parentExpiry, ok := h.sourceReadParent(w, r)
	if !ok {
		return
	}
	runtime, ok := h.nativeEnrollmentRuntime(w, r)
	if !ok {
		return
	}
	sourceID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "sourceId"), "source id")
	if !ok {
		return
	}
	var req nativeEnrollmentProofRequest
	if !decodeNativeEnrollmentRequest(w, r, &req) {
		return
	}
	proof, ok := nativeEnrollmentProof(w, req)
	if !ok {
		return
	}
	token, expiresAt, err := h.Commands.MintSourceEnrollmentToken(r.Context(), runtime.WorkspaceID, runtime.ID, ownerID, sourceID, proof, patHash, parentExpiry)
	if err != nil {
		handleNativeEnrollmentError(w, err)
		return
	}
	claims, err := auth.ParseSourceEnrollmentToken(token, time.Now())
	if err != nil {
		writeError(w, http.StatusUnauthorized, "enrollment credential expired")
		return
	}
	expiresIn := int64(time.Until(expiresAt).Seconds())
	if expiresIn <= 0 {
		writeError(w, http.StatusUnauthorized, "enrollment credential expired")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Token          string `json:"token"`
		TokenType      string `json:"token_type"`
		Scope          string `json:"scope"`
		WorkspaceID    string `json:"workspace_id"`
		RuntimeID      string `json:"runtime_id"`
		DaemonID       string `json:"daemon_id"`
		SourceID       string `json:"source_id"`
		EnrollmentID   string `json:"enrollment_id"`
		ConfigRevision int64  `json:"config_revision"`
		ManifestHash   string `json:"manifest_hash"`
		ExpiresAt      string `json:"expires_at"`
		ExpiresIn      int64  `json:"expires_in"`
	}{token, "Bearer", auth.SourceEnrollmentTokenPurpose, claims.WorkspaceID, claims.RuntimeID, claims.DaemonID, claims.SourceID, claims.EnrollmentID, claims.ConfigRevision, claims.ManifestHash, expiresAt.UTC().Format(time.RFC3339), expiresIn})
}

func (h *WorkSourceCommandHandler) FinalizeNativeSourceEnrollment(w http.ResponseWriter, r *http.Request) {
	claims := middleware.SourceEnrollmentClaimsFromContext(r.Context())
	if claims == nil || chi.URLParam(r, "runtimeId") != claims.RuntimeID || chi.URLParam(r, "sourceId") != claims.SourceID {
		writeError(w, http.StatusForbidden, "enrollment capability required")
		return
	}
	var req nativeEnrollmentProofRequest
	if !decodeNativeEnrollmentRequest(w, r, &req) {
		return
	}
	proof, ok := nativeEnrollmentProof(w, req)
	if !ok {
		return
	}
	source, err := h.Commands.FinalizeNativeSourceEnrollment(r.Context(), *claims, proof)
	if err != nil {
		handleNativeEnrollmentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workSourceToResponse(source))
}
