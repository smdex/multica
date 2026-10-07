package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestDeleteWorkspace_RemovesChatAndWorkflowRecords(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()
	workspaceID := dbfx.Insert(t, "workspace", testutil.Cols{
		"name":        "Workflow deletion test",
		"slug":        "workflow-delete-" + uuid.NewString(),
		"description": "",
	})
	fixture := testutil.New(testPool, workspaceID, testUserID)
	projectID := fixture.Insert(t, "project", testutil.Cols{
		"workspace_id": workspaceID,
		"title":        "Working copy deletion project",
	})
	workingCopyID := fixture.Insert(t, "project_working_copy", testutil.Cols{
		"workspace_id":            workspaceID,
		"project_id":              projectID,
		"daemon_id":               "workflow-delete-daemon",
		"name":                    "Working copy to delete",
		"kind":                    "directory",
		"ownership":               "external",
		"provisioning_request_id": uuid.NewString(),
	})
	otherWorkspaceID := dbfx.Workspace(t, "Working copy survivor", "workflow-survivor-"+uuid.NewString())
	otherFixture := testutil.New(testPool, otherWorkspaceID, testUserID)
	otherProjectID := otherFixture.Insert(t, "project", testutil.Cols{
		"workspace_id": otherWorkspaceID,
		"title":        "Working copy survivor project",
	})
	survivingWorkingCopyID := otherFixture.Insert(t, "project_working_copy", testutil.Cols{
		"workspace_id":            otherWorkspaceID,
		"project_id":              otherProjectID,
		"daemon_id":               "workflow-delete-daemon",
		"name":                    "Working copy to preserve",
		"kind":                    "directory",
		"ownership":               "external",
		"provisioning_request_id": uuid.NewString(),
	})
	runtimeID := fixture.Runtime(t, "Workflow deletion runtime")
	agentID := fixture.Agent(t, "Workflow deletion agent", runtimeID)
	sessionID := fixture.ChatSession(t, agentID)
	messageID := fixture.Insert(t, "chat_message", testutil.Cols{
		"chat_session_id": sessionID,
		"role":            "user",
		"content":         "workflow deletion regression",
	})
	workflowID := fixture.Insert(t, "agent_workflow_request", testutil.Cols{
		"id":              uuid.NewString(),
		"workspace_id":    workspaceID,
		"requester_id":    testUserID,
		"runtime_id":      runtimeID,
		"chat_session_id": sessionID,
		"kind":            "steer",
		"status":          "pending",
		"request_hash":    "workflow-delete-hash",
		"request":         testutil.Raw("'{}'::jsonb"),
		"expires_at":      testutil.Raw("now() + interval '1 hour'"),
	})
	interactionID := fixture.Insert(t, "task_interaction", testutil.Cols{
		"id":              uuid.NewString(),
		"workspace_id":    workspaceID,
		"runtime_id":      runtimeID,
		"chat_session_id": sessionID,
		"task_id":         uuid.NewString(),
		"run_id":          uuid.NewString(),
		"turn_id":         "turn-delete-regression",
		"kind":            "question",
		"status":          "pending",
		"request":         testutil.Raw("'{}'::jsonb"),
		"expires_at":      testutil.Raw("now() + interval '1 hour'"),
	})
	dbfx.Exec(t, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`, workspaceID, testUserID)

	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM task_interaction WHERE id = $1`, interactionID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM agent_workflow_request WHERE id = $1`, workflowID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM chat_message WHERE id = $1`, messageID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM project_working_copy WHERE id = $1`, workingCopyID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, workspaceID)
	})

	req := newRequest("DELETE", "/api/workspaces/"+workspaceID, nil)
	req = withURLParam(req, "id", workspaceID)
	testutil.Call(t, testHandler.DeleteWorkspace, req).Want(http.StatusNoContent)

	checks := map[string]struct {
		query string
		id    string
	}{
		"chat_message":           {query: `SELECT count(*) FROM chat_message WHERE id = $1`, id: messageID},
		"agent_workflow_request": {query: `SELECT count(*) FROM agent_workflow_request WHERE id = $1`, id: workflowID},
		"task_interaction":       {query: `SELECT count(*) FROM task_interaction WHERE id = $1`, id: interactionID},
		"project_working_copy":   {query: `SELECT count(*) FROM project_working_copy WHERE id = $1`, id: workingCopyID},
	}
	for table, check := range checks {
		var count int
		if err := testPool.QueryRow(ctx, check.query, check.id).Scan(&count); err != nil {
			t.Fatalf("count %s rows: %v", table, err)
		}
		if count != 0 {
			t.Errorf("%s rows survived workspace delete: %d", table, count)
		}
	}
	if count := otherFixture.Count(t, `SELECT count(*) FROM project_working_copy WHERE id = $1`, survivingWorkingCopyID); count != 1 {
		t.Errorf("another workspace's working copy was removed: got %d rows, want 1", count)
	}
}
