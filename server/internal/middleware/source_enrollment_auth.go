package middleware

import (
	"context"
	"net/http"
	"time"

	"github.com/multica-ai/multica/server/internal/auth"
)

type sourceEnrollmentContextKey struct{}

func SourceEnrollmentClaimsFromContext(ctx context.Context) *auth.SourceEnrollmentClaims {
	claims, _ := ctx.Value(sourceEnrollmentContextKey{}).(*auth.SourceEnrollmentClaims)
	return claims
}

func serveSourceEnrollmentToken(w http.ResponseWriter, r *http.Request, token string, next http.Handler) {
	for _, header := range []string{"X-User-ID", "X-Actor-Source", "X-Agent-ID", "X-Task-ID"} {
		r.Header.Del(header)
	}
	claims, err := auth.ParseSourceEnrollmentToken(token, time.Now())
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid enrollment credential")
		return
	}
	path := "/api/daemon/runtimes/" + claims.RuntimeID + "/source-enrollments/" + claims.SourceID + "/finalize"
	if r.Method != http.MethodPost || r.URL.EscapedPath() != path {
		writeError(w, http.StatusForbidden, "enrollment credential is not permitted on this route")
		return
	}
	if workspace := r.Header.Get("X-Workspace-ID"); workspace != "" && workspace != claims.WorkspaceID {
		writeError(w, http.StatusNotFound, "workspace not found")
		return
	}
	ctx := context.WithValue(r.Context(), sourceEnrollmentContextKey{}, &claims)
	ctx = context.WithValue(ctx, ctxKeyDaemonAuthPath, "source_enrollment")
	next.ServeHTTP(w, r.WithContext(ctx))
}
