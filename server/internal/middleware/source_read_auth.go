package middleware

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/auth"
)

type sourceReadContextKey struct{}

// SourceReadClaimsFromContext is separate from human and general daemon identity.
// Only the exact read delivery routes receive this signed capability.
func SourceReadClaimsFromContext(ctx context.Context) *auth.SourceReadClaims {
	claims, _ := ctx.Value(sourceReadContextKey{}).(*auth.SourceReadClaims)
	return claims
}

func serveSourceReadToken(w http.ResponseWriter, r *http.Request, token string, next http.Handler) {
	r.Header.Del("X-User-ID")
	r.Header.Del("X-Actor-Source")
	r.Header.Del("X-Agent-ID")
	r.Header.Del("X-Task-ID")
	claims, err := auth.ParseSourceReadToken(token, time.Now())
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid source credential")
		return
	}
	if !sourceReadRouteMatches(r, claims.RuntimeID) {
		writeError(w, http.StatusForbidden, "source credential is not permitted on this route")
		return
	}
	if workspace := r.Header.Get("X-Workspace-ID"); workspace != "" && workspace != claims.WorkspaceID {
		writeError(w, http.StatusNotFound, "workspace not found")
		return
	}
	ctx := context.WithValue(r.Context(), sourceReadContextKey{}, &claims)
	ctx = context.WithValue(ctx, ctxKeyDaemonAuthPath, "source_read")
	next.ServeHTTP(w, r.WithContext(ctx))
}

func sourceReadRouteMatches(r *http.Request, runtimeID string) bool {
	// EscapedPath keeps encoded separators/selectors distinguishable. No
	// decoding, traversal cleanup or prefix-only grant broadens this scope.
	path := r.URL.EscapedPath()
	pending := "/api/daemon/runtimes/" + runtimeID + "/work-source-commands"
	if r.Method == http.MethodGet {
		return path == pending
	}
	if r.Method != http.MethodPost || !strings.HasPrefix(path, pending+"/") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(path, pending+"/"), "/")
	if len(parts) != 2 || (parts[1] != "claim" && parts[1] != "result") {
		return false
	}
	id, err := uuid.Parse(parts[0])
	return err == nil && id != uuid.Nil && id.String() == parts[0]
}
