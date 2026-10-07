package handler

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestWorkSourceCommandSourceDeletion(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	for _, state := range []string{"pending", "claimed", "succeeded", "failed"} {
		t.Run(state, func(t *testing.T) {
			ws, user, runtime := seedWorkSourceFixture(t, "command-delete-"+state)
			source := createSourceViaAPI(t, workSourceHandler(), ws, user, runtime, "delete-command-source")
			sourceID := source["id"].(string)
			h := workSourceCommandHandler()
			command := createSourceCommandViaAPI(t, h, http.StatusCreated, ws, user, sourceID,
				map[string]any{"request_id": uuid.NewString(), "command": "list"})
			commandID := command["id"].(string)
			if state != "pending" {
				claimCommandViaAPI(t, h, http.StatusOK, runtime, commandID)
			}
			if state == "succeeded" {
				reportCommandViaAPI(t, h, http.StatusOK, runtime, commandID,
					map[string]any{"status": state, "result": "[]"})
			}
			if state == "failed" {
				reportCommandViaAPI(t, h, http.StatusOK, runtime, commandID,
					map[string]any{"status": state, "error": "source unavailable"})
			}
			request := workSourceRequest(t, http.MethodDelete, "/api/work-sources/"+sourceID, ws, user, nil)
			testutil.Call(t, workSourceHandler().DeleteWorkSource,
				testutil.WithURLParams(request, "sourceID", sourceID)).Want(http.StatusNoContent)
			fixtures := testutil.New(testPool, ws, user)
			if count := fixtures.Count(t, `SELECT count(*) FROM work_source_command WHERE source_id = $1`, sourceID); count != 0 {
				t.Fatalf("source deletion retained %s command: count=%d", state, count)
			}
		})
	}
}

func TestWorkSourceCommandWorkspaceDeletion(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	h := workSourceCommandHandler()
	ws, user, runtime := seedWorkSourceFixture(t, "command-workspace-delete")
	source := createSourceViaAPI(t, workSourceHandler(), ws, user, runtime, "workspace-command-source")
	createSourceCommandViaAPI(t, h, http.StatusCreated, ws, user, source["id"].(string),
		map[string]any{"request_id": uuid.NewString(), "command": "list"})
	otherWS, otherUser, otherRuntime := seedWorkSourceFixture(t, "command-workspace-survivor")
	otherSource := createSourceViaAPI(t, workSourceHandler(), otherWS, otherUser, otherRuntime, "workspace-command-source")
	otherCommand := createSourceCommandViaAPI(t, h, http.StatusCreated, otherWS, otherUser, otherSource["id"].(string),
		map[string]any{"request_id": uuid.NewString(), "command": "list"})

	request := workSourceRequest(t, http.MethodDelete, "/api/workspaces/"+ws, ws, user, nil)
	testutil.Call(t, testHandler.DeleteWorkspace,
		testutil.WithURLParams(request, "id", ws)).Want(http.StatusNoContent)
	fixtures := testutil.New(testPool, ws, user)
	if count := fixtures.Count(t, `SELECT count(*) FROM work_source_command WHERE workspace_id = $1`, ws); count != 0 {
		t.Fatalf("workspace deletion retained commands: count=%d", count)
	}
	if count := fixtures.Count(t, `SELECT count(*) FROM work_source_command WHERE workspace_id = $1 AND id = $2`, otherWS, otherCommand["id"]); count != 1 {
		t.Fatalf("workspace deletion removed another workspace's command: count=%d", count)
	}
}
