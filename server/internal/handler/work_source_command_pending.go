package handler

import (
	"net/http"

	"github.com/multica-ai/multica/server/internal/middleware"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ListPendingWorkSourceCommands discovers bounded read receipts for the exact
// authenticated runtime. Paths and executables never come from the server.
// The subsequent claim remains the authoritative locked execution fence.
func (h *WorkSourceCommandHandler) ListPendingWorkSourceCommands(w http.ResponseWriter, r *http.Request) {
	runtime, ok := h.requireWorkSourceCommandRuntime(w, r)
	if !ok {
		return
	}
	var rows []db.ListPendingWorkSourceCommandsForRuntimeRow
	var err error
	if claims := middleware.SourceReadClaimsFromContext(r.Context()); claims != nil {
		rows, err = h.Commands.ListPendingSourceReadCommands(r.Context(), *claims)
	} else {
		rows, err = h.Queries.ListPendingWorkSourceCommandsForRuntime(r.Context(), db.ListPendingWorkSourceCommandsForRuntimeParams{
			RuntimeID: runtime.ID, WorkspaceID: runtime.WorkspaceID, DaemonID: runtime.DaemonID,
		})
	}
	if err != nil {
		handleWorkSourceCommandError(w, err)
		return
	}
	type pendingResponse struct {
		workSourceCommandResponse
		SourceHandle string `json:"source_handle"`
	}
	response := make([]pendingResponse, len(rows))
	for i, row := range rows {
		response[i] = pendingResponse{workSourceCommandToResponse(row.WorkSourceCommand), row.SourceHandle}
	}
	writeJSON(w, http.StatusOK, response)
}
