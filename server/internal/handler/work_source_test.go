package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// seedWorkSourceFixture builds an isolated workspace + owner + runtime through
// the canonical dbfx builders. Every row self-cleans by id; rows with no-FK
// dependents (work_source, issue_work_link) are swept by their own
// cascade cleanups, which LIFO-run before the workspace/user deletes.
func seedWorkSourceFixture(t *testing.T, label string) (workspaceID, userID, runtimeID string) {
	t.Helper()
	suffix := time.Now().UnixNano()

	userID = dbfx.Insert(t, "user", testutil.Cols{
		"name":  "WS Source User " + label,
		"email": fmt.Sprintf("wsrc-%s-%d@multica.test", label, suffix),
	})
	workspaceID = dbfx.Insert(t, "workspace", testutil.Cols{
		"name":         "WS Source WS " + label,
		"slug":         fmt.Sprintf("wsrc-%s-%d", label, suffix),
		"description":  "",
		"issue_prefix": "",
	})
	dbfx.InsertNoID(t, "member", testutil.Cols{
		"workspace_id": workspaceID,
		"user_id":      userID,
		"role":         "owner",
	}, "workspace_id = $1 AND user_id = $2", workspaceID, userID)

	runtimeID = dbfx.Insert(t, "agent_runtime", testutil.Cols{
		"workspace_id": workspaceID,
		"daemon_id":    fmt.Sprintf("daemon-%s-%d", label, suffix),
		"name":         "wsrc-runtime " + label,
		"runtime_mode": "cloud",
		"provider":     "handler_test_runtime",
		"status":       "online",
		"device_info":  "",
		"metadata":     testutil.Raw("'{}'::jsonb"),
		"visibility":   "private",
		"owner_id":     userID,
	})
	return workspaceID, userID, runtimeID
}

// seedWorkSourceMemberViewer adds a non-admin member to the fixture workspace.
func seedWorkSourceMemberViewer(t *testing.T, workspaceID, label string) (userID string) {
	t.Helper()
	suffix := time.Now().UnixNano()
	userID = dbfx.Insert(t, "user", testutil.Cols{
		"name":  "WS Viewer " + label,
		"email": fmt.Sprintf("wsrcv-%s-%d@multica.test", label, suffix),
	})
	dbfx.InsertNoID(t, "member", testutil.Cols{
		"workspace_id": workspaceID,
		"user_id":      userID,
		"role":         "member",
	}, "workspace_id = $1 AND user_id = $2", workspaceID, userID)
	return userID
}

func workSourceHandler() *WorkSourceHandler {
	return &WorkSourceHandler{
		Handler:     testHandler,
		WorkSources: service.NewWorkSourceService(testHandler.Queries, testPool),
	}
}

func workSourceRequest(t *testing.T, method, path, workspaceID, userID string, body any) *http.Request {
	t.Helper()
	req := testutil.JSONRequest(method, path, body)
	// requestUserID reads exactly this header; the auth middleware would
	// set it after session auth, tests inject it directly.
	req.Header.Set("X-Workspace-ID", workspaceID)
	req.Header.Set("X-User-ID", userID)
	return req
}

// createSourceViaAPI registers a source through the public handler API and
// schedules its cascade cleanup (links first, source second) with
// t.Cleanup - which runs LIFO, before the fixture's workspace delete.
func createSourceViaAPI(t *testing.T, h *WorkSourceHandler, workspaceID, userID, runtimeID, handle string) map[string]any {
	t.Helper()
	req := workSourceRequest(t, http.MethodPost, "/api/work-sources", workspaceID, userID, map[string]any{
		"runtime_id":    runtimeID,
		"name":          "Source " + handle,
		"source_handle": handle,
	})
	var resp map[string]any
	testutil.Call(t, h.CreateWorkSource, req).Want(http.StatusCreated).JSON(&resp)
	sourceID := resp["id"].(string)
	t.Cleanup(func() {
		// Idempotent: already-deleted sources surface ErrWorkSourceNotFound.
		if err := h.WorkSources.DeleteWorkSourceCascade(context.Background(),
			util.MustParseUUID(workspaceID), util.MustParseUUID(sourceID)); err != nil &&
			!errors.Is(err, service.ErrWorkSourceNotFound) {
			t.Errorf("cleanup source %s: %v", sourceID, err)
		}
	})
	return resp
}

func seedWorkSourceIssue(t *testing.T, workspaceID, userID, title string) string {
	t.Helper()
	return dbfx.Insert(t, "issue", testutil.Cols{
		"workspace_id": workspaceID,
		"title":        title,
		"status":       "todo",
		"priority":     "none",
		"creator_type": "member",
		"creator_id":   userID,
		"assignee_type": "member",
		"assignee_id":   userID,
		"number":        testutil.Raw(fmt.Sprintf(
			"(SELECT COALESCE(MAX(number), 0) + 1 FROM issue WHERE workspace_id = '%s')", workspaceID)),
		"position": 0,
	})
}

// TestWorkSourceLifecycle exercises the public handler APIs end to end:
// create, conflict, update (rename keeps disabled), link + native item
// persistence, link list, unlink, source delete sweeping links.
func TestWorkSourceLifecycle(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	h := workSourceHandler()
	workspaceID, userID, runtimeID := seedWorkSourceFixture(t, "lifecycle")
	ctx := context.Background()

	source := createSourceViaAPI(t, h, workspaceID, userID, runtimeID, "beads-main")
	sourceID := source["id"].(string)
	if source["mode"] != "observe" || source["enabled"] != true {
		t.Fatalf("created source mode/enabled = %v/%v, want observe/true", source["mode"], source["enabled"])
	}
	if source["source_handle"] != "beads-main" {
		t.Fatalf("source_handle = %#v, want the approved handle unchanged", source["source_handle"])
	}

	// Same daemon+handle is a 409 owner conflict.
	req := workSourceRequest(t, http.MethodPost, "/api/work-sources", workspaceID, userID, map[string]any{
		"runtime_id":    runtimeID,
		"name":          "Duplicate",
		"source_handle": "beads-main",
	})
	testutil.Call(t, h.CreateWorkSource, req).Want(http.StatusConflict)

	// Disable, then rename without enabled: the flag must stay false.
	disable := workSourceRequest(t, http.MethodPatch, "/api/work-sources/"+sourceID, workspaceID, userID,
		map[string]any{"name": "Renamed once", "enabled": false})
	testutil.Call(t, h.UpdateWorkSource, testutil.WithURLParams(disable, "sourceID", sourceID)).Want(http.StatusOK)
	rename := workSourceRequest(t, http.MethodPatch, "/api/work-sources/"+sourceID, workspaceID, userID,
		map[string]any{"name": "Renamed twice"})
	var renamed map[string]any
	testutil.Call(t, h.UpdateWorkSource, testutil.WithURLParams(rename, "sourceID", sourceID)).Want(http.StatusOK).JSON(&renamed)
	if renamed["enabled"] != false {
		t.Fatalf("rename without enabled re-enabled the source: %#v", renamed)
	}

	// Link an issue to a native item via the public API.
	issueID := seedWorkSourceIssue(t, workspaceID, userID, "wsrc lifecycle issue")
	nativeItem := "beads-42"
	linkReq := func() *http.Request {
		return workSourceRequest(t, http.MethodPost, "/api/issue-work-links", workspaceID, userID, map[string]any{
			"issue_id":  issueID,
			"source_id": sourceID,
			"native_id": nativeItem,
		})
	}
	var link map[string]any
	testutil.Call(t, h.CreateIssueWorkLink, linkReq()).Want(http.StatusCreated).JSON(&link)
	if link["native_id"] != nativeItem {
		t.Fatalf("link native_id = %#v, want persisted %q", link["native_id"], nativeItem)
	}

	// Opaque identity keeps its original bytes: leading/trailing whitespace
	// survives instead of being silently trimmed into a different value.
	spaced := " spaced-native "
	spacedReq := workSourceRequest(t, http.MethodPost, "/api/issue-work-links", workspaceID, userID, map[string]any{
		"issue_id":  issueID,
		"source_id": sourceID,
		"native_id": spaced,
	})
	var spacedLink map[string]any
	testutil.Call(t, h.CreateIssueWorkLink, spacedReq).Want(http.StatusCreated).JSON(&spacedLink)
	if spacedLink["native_id"] != spaced {
		t.Fatalf("native_id = %#v, want whitespace preserved exactly", spacedLink["native_id"])
	}
	spacedHandle := createSourceViaAPI(t, h, workspaceID, userID, runtimeID, " spaced-handle ")
	if spacedHandle["source_handle"] != " spaced-handle " {
		t.Fatalf("source_handle = %#v, want whitespace preserved exactly", spacedHandle["source_handle"])
	}
	// Whitespace-only identity is rejected, never silently emptied.
	blankLink := workSourceRequest(t, http.MethodPost, "/api/issue-work-links", workspaceID, userID, map[string]any{
		"issue_id":  issueID,
		"source_id": sourceID,
		"native_id": "  ",
	})
	testutil.Call(t, h.CreateIssueWorkLink, blankLink).Want(http.StatusBadRequest)
	blankSource := workSourceRequest(t, http.MethodPost, "/api/work-sources", workspaceID, userID, map[string]any{
		"runtime_id":    runtimeID,
		"name":          "Blank handle",
		"source_handle": "  ",
	})
	testutil.Call(t, h.CreateWorkSource, blankSource).Want(http.StatusBadRequest)

	// The link lists by issue and by source, still workspace-scoped.
	listByIssue := workSourceRequest(t, http.MethodGet, "/api/issue-work-links?issue_id="+issueID, workspaceID, userID, nil)
	var links []map[string]any
	testutil.Call(t, h.ListIssueWorkLinks, listByIssue).Want(http.StatusOK).JSON(&links)
	hasLink := func(id any) bool {
		for _, l := range links {
			if l["id"] == id {
				return true
			}
		}
		return false
	}
	if !hasLink(link["id"]) || !hasLink(spacedLink["id"]) {
		t.Fatalf("list by issue = %#v, want both created links", links)
	}
	listBySource := workSourceRequest(t, http.MethodGet, "/api/issue-work-links?source_id="+sourceID, workspaceID, userID, nil)
	testutil.Call(t, h.ListIssueWorkLinks, listBySource).Want(http.StatusOK).JSON(&links)
	if len(links) != 2 {
		t.Fatalf("list by source = %d links, want 2", len(links))
	}

	// Duplicate native item on the same source conflicts.
	testutil.Call(t, h.CreateIssueWorkLink, linkReq()).Want(http.StatusConflict)

	// Unlink, then delete the source: the sweep leaves no link rows.
	delLink := workSourceRequest(t, http.MethodDelete, "/api/issue-work-links/"+link["id"].(string), workspaceID, userID, nil)
	testutil.Call(t, h.DeleteIssueWorkLink, testutil.WithURLParams(delLink, "linkID", link["id"].(string))).Want(http.StatusNoContent)

	// Re-link so the delete sweep has something to sweep.
	testutil.Call(t, h.CreateIssueWorkLink, linkReq()).Want(http.StatusCreated)
	delSource := workSourceRequest(t, http.MethodDelete, "/api/work-sources/"+sourceID, workspaceID, userID, nil)
	testutil.Call(t, h.DeleteWorkSource, testutil.WithURLParams(delSource, "sourceID", sourceID)).Want(http.StatusNoContent)

	var linkCount int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM issue_work_link WHERE workspace_id = $1`, workspaceID).Scan(&linkCount); err != nil || linkCount != 0 {
		t.Fatalf("links after source delete = %d (err %v), want 0", linkCount, err)
	}
	testutil.Call(t, h.DeleteWorkSource, testutil.WithURLParams(delSource, "sourceID", sourceID)).Want(http.StatusNotFound)
}

// TestWorkSourceHandlerGuards covers malformed workspace header, foreign
// workspace, and the admin role gate on the public APIs.
func TestWorkSourceHandlerGuards(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	h := workSourceHandler()
	workspaceID, userID, runtimeID := seedWorkSourceFixture(t, "guards")
	viewerID := seedWorkSourceMemberViewer(t, workspaceID, "guards")

	// Malformed workspace UUID is a 400 before any DB access.
	badWS := workSourceRequest(t, http.MethodPost, "/api/work-sources", "not-a-uuid", userID, map[string]any{
		"runtime_id": runtimeID, "name": "x", "source_handle": "h",
	})
	testutil.Call(t, h.CreateWorkSource, badWS).Want(http.StatusBadRequest)

	// A well-formed foreign UUID is not this user's workspace: 404.
	foreignWS := workSourceRequest(t, http.MethodPost, "/api/work-sources", uuid.NewString(), userID, map[string]any{
		"runtime_id": runtimeID, "name": "x", "source_handle": "h",
	})
	testutil.Call(t, h.CreateWorkSource, foreignWS).Want(http.StatusNotFound)

	// Member (non-admin) cannot create sources but can list.
	viewerReq := workSourceRequest(t, http.MethodPost, "/api/work-sources", workspaceID, viewerID, map[string]any{
		"runtime_id": runtimeID, "name": "x", "source_handle": "h",
	})
	testutil.Call(t, h.CreateWorkSource, viewerReq).Want(http.StatusForbidden)
	viewerList := workSourceRequest(t, http.MethodGet, "/api/work-sources", workspaceID, viewerID, nil)
	testutil.Call(t, h.ListWorkSources, viewerList).Want(http.StatusOK)

	// Cross-workspace: a source created in ws A is invisible to ws B via
	// list, update, delete, and link paths (all workspace-scoped).
	otherWS, otherUser, _ := seedWorkSourceFixture(t, "guards-other")
	source := createSourceViaAPI(t, h, workspaceID, userID, runtimeID, "scoped-handle")
	sourceID := source["id"].(string)

	otherList := workSourceRequest(t, http.MethodGet, "/api/work-sources", otherWS, otherUser, nil)
	var otherSources []map[string]any
	testutil.Call(t, h.ListWorkSources, otherList).Want(http.StatusOK).JSON(&otherSources)
	for _, s := range otherSources {
		if s["id"] == sourceID {
			t.Fatal("source from another workspace leaked into list")
		}
	}
	otherPatch := workSourceRequest(t, http.MethodPatch, "/api/work-sources/"+sourceID, otherWS, otherUser, map[string]any{"name": "hijack"})
	testutil.Call(t, h.UpdateWorkSource, testutil.WithURLParams(otherPatch, "sourceID", sourceID)).Want(http.StatusNotFound)
	otherDelete := workSourceRequest(t, http.MethodDelete, "/api/work-sources/"+sourceID, otherWS, otherUser, nil)
	testutil.Call(t, h.DeleteWorkSource, testutil.WithURLParams(otherDelete, "sourceID", sourceID)).Want(http.StatusNotFound)

	// Linking with the source id from another workspace fails closed.
	otherIssue := seedWorkSourceIssue(t, otherWS, otherUser, "other ws issue")
	crossLink := workSourceRequest(t, http.MethodPost, "/api/issue-work-links", otherWS, otherUser, map[string]any{
		"issue_id": otherIssue, "source_id": sourceID, "native_id": "n1",
	})
	testutil.Call(t, h.CreateIssueWorkLink, crossLink).Want(http.StatusNotFound)
}

// TestWorkSourceRuntimeTeardownRefusal pins the ownership fence: a runtime
// with a registered work source must never be torn down (TeardownRuntime
// refuses; interactive delete/merge endpoints map it to 409 upstream). No
// source metadata is deleted implicitly.
func TestWorkSourceRuntimeTeardownRefusal(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	h := workSourceHandler()
	workspaceID, userID, runtimeID := seedWorkSourceFixture(t, "teardown")
	ctx := context.Background()

	createSourceViaAPI(t, h, workspaceID, userID, runtimeID, "teardown-handle")

	q := testHandler.Queries
	rt := util.MustParseUUID(runtimeID)
	ws := util.MustParseUUID(workspaceID)
	hasSources, err := q.RuntimeHasWorkSources(ctx, db.RuntimeHasWorkSourcesParams{RuntimeID: rt, WorkspaceID: ws})
	if err != nil {
		t.Fatalf("RuntimeHasWorkSources: %v", err)
	}
	if !hasSources {
		t.Fatal("bound runtime reported no work sources")
	}

	// TeardownRuntime must refuse before mutating anything. The tx is
	// rolled back explicitly before any follow-through so the runtime row
	// lock is released on this connection, not held by a defer.
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	_, terr := service.TeardownRuntime(ctx, q.WithTx(tx), rt, service.RuntimeTeardownOptions{})
	if !errors.Is(terr, service.ErrRuntimeHasWorkSources) {
		tx.Rollback(ctx)
		t.Fatalf("TeardownRuntime err = %v, want ErrRuntimeHasWorkSources", terr)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback refusal tx: %v", err)
	}

	// After the source binding is removed, the fence flips.
	var sourceID string
	if err := testPool.QueryRow(ctx, `SELECT id FROM work_source WHERE runtime_id = $1 AND workspace_id = $2`, rt, ws).Scan(&sourceID); err != nil {
		t.Fatalf("load source id: %v", err)
	}
	if err := h.WorkSources.DeleteWorkSourceCascade(ctx, ws, util.MustParseUUID(sourceID)); err != nil {
		t.Fatalf("delete source: %v", err)
	}
	hasSources, err = q.RuntimeHasWorkSources(ctx, db.RuntimeHasWorkSourcesParams{RuntimeID: rt, WorkspaceID: ws})
	if err != nil {
		t.Fatalf("RuntimeHasWorkSources after delete: %v", err)
	}
	if hasSources {
		t.Fatal("runtime still fenced after its last source binding was deleted")
	}
}

// TestWorkSourceRuntimeDeleteEndpoint409 exercises the public runtime-delete
// API, not just the service: a source-owning runtime is refused with 409 and
// the binding survives untouched.
func TestWorkSourceRuntimeDeleteEndpoint409(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	h := workSourceHandler()
	workspaceID, userID, runtimeID := seedWorkSourceFixture(t, "ep409")

	createSourceViaAPI(t, h, workspaceID, userID, runtimeID, "ep409-handle")

	req := workSourceRequest(t, http.MethodDelete, "/api/runtimes/"+runtimeID, workspaceID, userID, nil)
	req = testutil.WithURLParams(req, "runtimeId", runtimeID)
	rec := testutil.Call(t, testHandler.DeleteAgentRuntime, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("DeleteAgentRuntime on source-owning runtime = %d (%s), want 409", rec.Code, rec.Body.String())
	}
	var bound int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM work_source WHERE runtime_id = $1`, util.MustParseUUID(runtimeID)).Scan(&bound); err != nil || bound != 1 {
		t.Fatalf("binding after refused delete = %d (err %v), want 1 untouched", bound, err)
	}
}

// TestWorkSourceDeleteLinkRace pins the no-FK teardown protocol: a link
// insert and a source cascade delete racing through the same source row must
// serialize on LockWorkSourceForWrite - either the link commits and is swept,
// or the delete commits first and the link insert fails. A committed link
// surviving its source's completed delete is the failure this test forbids.
func TestWorkSourceDeleteLinkRace(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	h := workSourceHandler()
	workspaceID, userID, runtimeID := seedWorkSourceFixture(t, "race")
	ctx := context.Background()

	issueID := seedWorkSourceIssue(t, workspaceID, userID, "wsrc race issue")

	for i := range 5 {
		source := createSourceViaAPI(t, h, workspaceID, userID, runtimeID, fmt.Sprintf("race-handle-%d", i))
		sourceID := source["id"].(string)
		sourceUUID := util.MustParseUUID(sourceID)

		// Buffered so goroutines never block on send; no t.Fatal inside them.
		linkErrCh := make(chan error, 1)
		deleteErrCh := make(chan error, 1)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := h.WorkSources.CreateIssueWorkLink(ctx, service.CreateIssueWorkLinkParams{
				WorkspaceID: util.MustParseUUID(workspaceID),
				IssueID:     util.MustParseUUID(issueID),
				SourceID:    sourceUUID,
				NativeID:    fmt.Sprintf("native-%d", i),
				CreatedBy:   util.MustParseUUID(userID),
			})
			linkErrCh <- err
		}()
		go func() {
			defer wg.Done()
			deleteErrCh <- h.WorkSources.DeleteWorkSourceCascade(ctx, util.MustParseUUID(workspaceID), sourceUUID)
		}()
		wg.Wait()

		// The delete is the teardown under test: it must succeed.
		if err := <-deleteErrCh; err != nil {
			t.Fatalf("round %d: DeleteWorkSourceCascade: %v", i, err)
		}
		// The link either committed (and was swept) or lost the race and saw
		// the source vanish; anything else is a defect.
		if err := <-linkErrCh; err != nil && !errors.Is(err, service.ErrWorkSourceNotFound) {
			t.Fatalf("round %d: CreateIssueWorkLink: %v", i, err)
		}

		// Whatever interleaving happened, the invariant is absolute: once
		// the delete returned, neither the source nor any link to it may exist.
		var orphaned int
		if err := testPool.QueryRow(ctx, `SELECT count(*) FROM issue_work_link WHERE source_id = $1`, sourceID).Scan(&orphaned); err != nil {
			t.Fatalf("round %d: count orphaned links: %v", i, err)
		}
		if orphaned != 0 {
			t.Fatalf("round %d: %d links survived their source's delete", i, orphaned)
		}
		var remaining int
		if err := testPool.QueryRow(ctx, `SELECT count(*) FROM work_source WHERE id = $1`, sourceID).Scan(&remaining); err != nil {
			t.Fatalf("round %d: count remaining source: %v", i, err)
		}
		if remaining != 0 {
			t.Fatalf("round %d: source survived its own cascade delete", i)
		}
	}
}
