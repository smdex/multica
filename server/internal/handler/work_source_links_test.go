package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestWorkSourceManyToManyLinks(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	h := workSourceHandler()
	ws, user, runtime := seedWorkSourceFixture(t, "many-links")
	issueA := seedWorkSourceIssue(t, ws, user, "First linked Issue")
	issueB := seedWorkSourceIssue(t, ws, user, "Second linked Issue")
	source := createSourceViaAPI(t, h, ws, user, runtime, "many-links-source")
	sourceID := source["id"].(string)
	createLink := func(issueID, nativeID string, status int) map[string]any {
		t.Helper()
		req := workSourceRequest(t, http.MethodPost, "/api/issue-work-links", ws, user,
			map[string]any{"issue_id": issueID, "source_id": sourceID, "native_id": nativeID})
		var link map[string]any
		testutil.Call(t, h.CreateIssueWorkLink, req).Want(status).JSON(&link)
		return link
	}
	first := createLink(issueA, "shared-bead", http.StatusCreated)
	createLink(issueB, "shared-bead", http.StatusCreated)
	createLink(issueA, "another-bead", http.StatusCreated)
	createLink(issueA, "shared-bead", http.StatusConflict)

	var links []map[string]any
	list := workSourceRequest(t, http.MethodGet, "/api/issue-work-links?source_id="+sourceID, ws, user, nil)
	testutil.Call(t, h.ListIssueWorkLinks, list).Want(http.StatusOK).JSON(&links)
	if len(links) != 3 {
		t.Fatalf("source links=%d, want 3 distinct relationships", len(links))
	}
	list = workSourceRequest(t, http.MethodGet, "/api/issue-work-links?issue_id="+issueA, ws, user, nil)
	testutil.Call(t, h.ListIssueWorkLinks, list).Want(http.StatusOK).JSON(&links)
	if len(links) != 2 {
		t.Fatalf("first Issue links=%d, want 2 native items", len(links))
	}

	linkID := first["id"].(string)
	unlink := workSourceRequest(t, http.MethodDelete, "/api/issue-work-links/"+linkID, ws, user, nil)
	testutil.Call(t, h.DeleteIssueWorkLink, testutil.WithURLParams(unlink, "linkID", linkID)).Want(http.StatusNoContent)
	fixtures := testutil.New(testPool, ws, user)
	if count := fixtures.Count(t, `SELECT count(*) FROM issue_work_link WHERE source_id = $1`, sourceID); count != 2 {
		t.Fatalf("unlink removed other relationships: remaining=%d, want 2", count)
	}
	if count := fixtures.Count(t, `SELECT count(*) FROM issue WHERE id IN ($1, $2)`, issueA, issueB); count != 2 {
		t.Fatalf("unlink changed collaboration Issues: remaining=%d, want 2", count)
	}
	if count := fixtures.Count(t, `SELECT count(*) FROM work_source WHERE id = $1`, sourceID); count != 1 {
		t.Fatalf("unlink removed source binding: count=%d, want 1", count)
	}
}
