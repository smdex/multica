package handler

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// TestWorkSourceParentDeletion follows the production project, issue and
// workspace handlers. These operations change bindings, never source-owned tasks.
func TestWorkSourceParentDeletion(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	h := workSourceHandler()
	ws, user, runtime := seedWorkSourceFixture(t, "parent-delete")
	fixtures := testutil.New(testPool, ws, user)
	project := fixtures.Insert(t, "project", testutil.Cols{"workspace_id": ws, "title": "Source project"})
	issue := seedWorkSourceIssue(t, ws, user, "Linked source issue")
	var source map[string]any
	req := workSourceRequest(t, http.MethodPost, "/api/work-sources", ws, user, map[string]any{
		"runtime_id": runtime, "project_id": project, "name": "Project source", "source_handle": "approved-project-source",
	})
	testutil.Call(t, h.CreateWorkSource, req).Want(http.StatusCreated).JSON(&source)
	sourceID := source["id"].(string)
	// Track API-created no-FK rows even if an assertion fails before teardown.
	t.Cleanup(func() {
		if _, err := testPool.Exec(context.Background(), `DELETE FROM issue_work_link WHERE source_id = $1`, sourceID); err != nil {
			t.Errorf("cleanup links: %v", err)
		}
		if _, err := testPool.Exec(context.Background(), `DELETE FROM work_source WHERE id = $1`, sourceID); err != nil {
			t.Errorf("cleanup source: %v", err)
		}
	})
	link := func() {
		t.Helper()
		req := workSourceRequest(t, http.MethodPost, "/api/issue-work-links", ws, user,
			map[string]any{"issue_id": issue, "source_id": sourceID, "native_id": "source-native-42"})
		testutil.Call(t, h.CreateIssueWorkLink, req).Want(http.StatusCreated)
	}
	link()

	otherWS, otherUser, otherRuntime := seedWorkSourceFixture(t, "parent-survivor")
	otherSource := createSourceViaAPI(t, h, otherWS, otherUser, otherRuntime, "approved-project-source")

	deleteProject := workSourceRequest(t, http.MethodDelete, "/api/projects/"+project, ws, user, nil)
	testutil.Call(t, testHandler.DeleteProject, testutil.WithURLParams(deleteProject, "id", project)).Want(http.StatusNoContent)
	var detached bool
	var revision int
	if err := testPool.QueryRow(ctx, `SELECT project_id IS NULL, config_revision FROM work_source WHERE id = $1`, sourceID).Scan(&detached, &revision); err != nil || !detached || revision != 2 {
		t.Fatalf("project detach: detached=%t revision=%d err=%v, want true/2", detached, revision, err)
	}
	if count := fixtures.Count(t, `SELECT count(*) FROM issue_work_link WHERE source_id = $1 AND native_id = 'source-native-42'`, sourceID); count != 1 {
		t.Fatalf("project deletion lost native link: count=%d", count)
	}

	deleteIssue := workSourceRequest(t, http.MethodDelete, "/api/issues/"+issue, ws, user, nil)
	testutil.Call(t, testHandler.DeleteIssue, testutil.WithURLParams(deleteIssue, "id", issue)).Want(http.StatusNoContent)
	if count := fixtures.Count(t, `SELECT count(*) FROM issue_work_link WHERE source_id = $1`, sourceID); count != 0 {
		t.Fatalf("issue deletion retained links: count=%d", count)
	}
	if count := fixtures.Count(t, `SELECT count(*) FROM work_source WHERE id = $1`, sourceID); count != 1 {
		t.Fatalf("issue deletion removed source binding: count=%d", count)
	}

	issue = seedWorkSourceIssue(t, ws, user, "Workspace delete linked issue")
	link()
	deleteWorkspace := workSourceRequest(t, http.MethodDelete, "/api/workspaces/"+ws, ws, user, nil)
	testutil.Call(t, testHandler.DeleteWorkspace, testutil.WithURLParams(deleteWorkspace, "id", ws)).Want(http.StatusNoContent)
	if count := fixtures.Count(t, `SELECT count(*) FROM work_source WHERE workspace_id = $1`, ws); count != 0 {
		t.Fatalf("workspace deletion retained sources: count=%d", count)
	}
	if count := fixtures.Count(t, `SELECT count(*) FROM issue_work_link WHERE workspace_id = $1`, ws); count != 0 {
		t.Fatalf("workspace deletion retained links: count=%d", count)
	}
	if count := fixtures.Count(t, `SELECT count(*) FROM work_source WHERE id = $1`, otherSource["id"]); count != 1 {
		t.Fatalf("another workspace source removed: count=%d", count)
	}
}

func TestWorkSourceRuntimeMergeRefusal(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	h := workSourceHandler()
	ws, user, runtime := seedWorkSourceFixture(t, "merge-source")
	source := createSourceViaAPI(t, h, ws, user, runtime, "merge-owned-source")
	fixtures := testutil.New(testPool, ws, user)
	target := fixtures.Runtime(t, "Merge target")
	err := testHandler.mergeLegacyRuntime(context.Background(), parseUUID(target), parseUUID(runtime), "merge-source-daemon", "handler_test_runtime")
	if !errors.Is(err, errRuntimeMergeFenced) {
		t.Fatalf("source owner merge err=%v, want fenced", err)
	}
	if count := fixtures.Count(t, `SELECT count(*) FROM work_source source JOIN agent_runtime runtime ON runtime.id = source.runtime_id WHERE source.id = $1 AND runtime.id = $2`, source["id"], runtime); count != 1 {
		t.Fatalf("merge changed source owner: count=%d", count)
	}
}
