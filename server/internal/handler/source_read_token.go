package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
)

// MintSourceReadToken exchanges a fresh human bearer credential for a short,
// runtime-specific read capability. It never accepts cookies or daemon tokens.
func (h *WorkSourceCommandHandler) MintSourceReadToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	runtimeID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "runtimeId"), "runtime id")
	if !ok {
		return
	}
	var req struct {
		Scope string `json:"scope"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&req) != nil || decoder.Decode(new(any)) != io.EOF || req.Scope != auth.SourceReadTokenPurpose {
		writeError(w, http.StatusBadRequest, "scope must be source:read")
		return
	}
	ownerID, patHash, parentExpiry, ok := h.sourceReadParent(w, r)
	if !ok {
		return
	}
	runtime, err := h.Queries.GetAgentRuntime(r.Context(), runtimeID)
	if errors.Is(err, pgx.ErrNoRows) {
		handleWorkSourceCommandError(w, service.ErrWorkSourceCommandNotFound)
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "source authorization unavailable")
		return
	}
	if workspace := r.Header.Get("X-Workspace-ID"); workspace != "" && workspace != runtime.WorkspaceID.String() {
		handleWorkSourceCommandError(w, service.ErrWorkSourceCommandNotFound)
		return
	}
	token, expiresAt, err := h.Commands.MintSourceReadToken(r.Context(), runtime, ownerID, patHash, parentExpiry)
	if err != nil {
		handleWorkSourceCommandError(w, err)
		return
	}
	// Echo the signed selectors and refuse credentials that expired during commit.
	claims, err := auth.ParseSourceReadToken(token, time.Now())
	if err != nil {
		writeError(w, http.StatusUnauthorized, "source credential has expired")
		return
	}
	expiresIn := int64(time.Until(expiresAt).Seconds())
	if expiresIn <= 0 {
		writeError(w, http.StatusUnauthorized, "parent credential has expired")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Token       string `json:"token"`
		TokenType   string `json:"token_type"`
		Scope       string `json:"scope"`
		RuntimeID   string `json:"runtime_id"`
		WorkspaceID string `json:"workspace_id"`
		DaemonID    string `json:"daemon_id"`
		ExpiresAt   string `json:"expires_at"`
		ExpiresIn   int64  `json:"expires_in"`
	}{token, "Bearer", auth.SourceReadTokenPurpose, claims.RuntimeID, claims.WorkspaceID, claims.DaemonID, expiresAt.UTC().Format(time.RFC3339), expiresIn})
}

// Middleware caches are deliberately not authority for this exchange. The
// service subsequently locks and revalidates the current PAT and membership.
func (h *WorkSourceCommandHandler) sourceReadParent(w http.ResponseWriter, r *http.Request) (pgtype.UUID, string, time.Time, bool) {
	raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if raw == "" || raw == r.Header.Get("Authorization") {
		writeError(w, http.StatusUnauthorized, "human bearer credential required")
		return pgtype.UUID{}, "", time.Time{}, false
	}
	if strings.HasPrefix(raw, "mul_") {
		hash := auth.HashToken(raw)
		pat, err := h.Queries.GetPersonalAccessTokenByHash(r.Context(), hash)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusUnauthorized, "invalid parent credential")
			} else {
				writeError(w, http.StatusServiceUnavailable, "source authorization unavailable")
			}
			return pgtype.UUID{}, "", time.Time{}, false
		}
		if pat.Revoked || !pat.UserID.Valid || (pat.ExpiresAt.Valid && !pat.ExpiresAt.Time.After(time.Now())) || auth.IsTemporarilyDisabledUserID(pat.UserID.String()) {
			writeError(w, http.StatusUnauthorized, "invalid parent credential")
			return pgtype.UUID{}, "", time.Time{}, false
		}
		return pat.UserID, hash, time.Time{}, true
	}
	claims, err := auth.ParseSessionToken(raw)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid parent credential")
		return pgtype.UUID{}, "", time.Time{}, false
	}
	subject, _ := claims["sub"].(string)
	email, _ := claims["email"].(string)
	ownerID, err := util.ParseUUID(subject)
	expiry := auth.SessionExpiry(claims)
	if err != nil || !ownerID.Valid || expiry.IsZero() || !expiry.After(time.Now()) || auth.IsTemporarilyDisabledUser(subject, email) {
		writeError(w, http.StatusUnauthorized, "invalid parent credential")
		return pgtype.UUID{}, "", time.Time{}, false
	}
	return ownerID, "", expiry, true
}
