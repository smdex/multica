package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// Production-router regressions for the non-executing workflow draft
// endpoints (POST /api/workflow-runs, GET /api/workflow-runs/{runID}).
// All receipts are created, claimed, and reported through the actual
// production router using scoped source-read credentials; no fake daemons,
// no synthetic tables, no direct handler calls. Credentials are never
// logged.
//
// Contract under test:
//   - create: 201 new draft, 200 identical retry (same request UUID, any
//     receipt order), 409 changed payload / stale config / disabled source
//     (except identical existing), 422 incomplete or unsupported closure,
//     404 missing scoped receipt/source/workspace, 400 malformed body,
//     capacity, or duplicate receipt UUIDs, 403 member create.
//   - read: any workspace member may GET; 404 after the source or workspace
//     is swept; project deletion detaches project_id and leaves the frozen
//     graph unchanged.
//   - the flow must not queue agent tasks or write synthetic issues, and no
//     workflow-run start route exists.

// draftSourceFixture stages an owned online runtime and observe work source
// in workspace ws. Returns runtime id, source id, and the source's live
// config revision. project may be empty.
func draftSourceFixture(t *testing.T, fx *testutil.Fixture, ws, owner, name, provider, project string) (runtimeID, sourceID string, configRevision int32) {
	t.Helper()
	daemonID := "wfd-" + uuid.NewString()
	runtimeID = fx.Insert(t, "agent_runtime", testutil.Cols{
		"workspace_id": ws,
		"owner_id":     owner,
		"name":         name, "daemon_id": daemonID, "provider": provider,
		"runtime_mode": "local", "status": "online", "visibility": "private",
		"device_info": "", "metadata": testutil.Raw("'{}'::jsonb"),
	})
	cols := testutil.Cols{
		"id": testutil.Raw("gen_random_uuid()"), "workspace_id": ws,
		"runtime_id": runtimeID, "daemon_id": daemonID, "name": name,
		"source_handle": "wfd-" + uuid.NewString(), "mode": "observe",
	}
	if project != "" {
		cols["project_id"] = project
	}
	sourceID = fx.Insert(t, "work_source", cols)
	// workflow_run has no FKs: sweep drafts created against exactly this
	// test-created source so a failing assertion cannot leave orphans behind
	// the fixture cleanup. Never workspace-wide: the DB is shared.
	fx.Cleanup(t, `DELETE FROM workflow_run WHERE workspace_id=$1 AND source_id=$2`, ws, sourceID)
	fx.QueryRow(t, `SELECT config_revision FROM work_source WHERE id=$1`, sourceID).Scan(&configRevision)
	return runtimeID, sourceID, configRevision
}

// draftCompleteItem is a complete read observation: explicit dependency
// list, exact dependency_count, blocks edges only.
func draftCompleteItem(id, revision string, deps ...[2]string) string {
	edges := make([]string, 0, len(deps))
	for _, dep := range deps {
		edges = append(edges, fmt.Sprintf(`{"id":%q,"dependency_type":%q}`, dep[0], dep[1]))
	}
	return fmt.Sprintf(`{"id":%q,"revision":%q,"title":"title-%s","status":"todo","dependency_count":%d,"dependencies_complete":true,"dependencies":[%s]}`,
		id, revision, id, len(deps), strings.Join(edges, ","))
}

// draftLegacyItem is a legacy observation with no dependency evidence.
func draftLegacyItem(id, revision string) string {
	return fmt.Sprintf(`{"id":%q,"revision":%q,"title":"title-%s","status":"todo"}`, id, revision, id)
}

// draftReceipt drives one read command through the production router:
// created by userToken, claimed and reported by the scoped msr credential.
func draftReceipt(t *testing.T, ctx context.Context, sourceID, runtimeID, msr, userToken, ws, nativeID, itemJSON string) string {
	t.Helper()
	create := `{"request_id":"` + uuid.NewString() + `","command":"read","native_id":` + fmt.Sprintf("%q", nativeID) + `}`
	_, raw := mustSourceReadCall(t, ctx, http.MethodPost, "/api/work-sources/"+sourceID+"/commands", userToken, ws, create, http.StatusCreated)
	var receipt struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &receipt); err != nil || receipt.ID == "" {
		t.Fatalf("missing read receipt identity: err=%v id_present=%t", err, receipt.ID != "")
	}
	base := "/api/daemon/runtimes/" + runtimeID + "/work-source-commands/" + receipt.ID
	mustSourceReadCall(t, ctx, http.MethodPost, base+"/claim", msr, ws, "", http.StatusOK)
	report, err := json.Marshal(map[string]string{"status": "succeeded", "result": itemJSON})
	if err != nil {
		t.Fatal(err)
	}
	mustSourceReadCall(t, ctx, http.MethodPost, base+"/result", msr, ws, string(report), http.StatusOK)
	return receipt.ID
}

func draftBody(requestID, sourceID, rootNativeID, rootRevision string, configRevision, capacity int, receiptIDs []string) string {
	body, err := json.Marshal(map[string]any{
		"request_id": requestID, "source_id": sourceID,
		"root_native_id": rootNativeID, "expected_root_revision": rootRevision,
		"expected_config_revision": configRevision, "capacity": capacity,
		"receipt_ids": receiptIDs,
	})
	if err != nil {
		panic(err)
	}
	return string(body)
}

type draftGraphJSON struct {
	WorkspaceID    string `json:"workspace_id"`
	SourceID       string `json:"source_id"`
	RootNativeID   string `json:"root_native_id"`
	ConfigRevision int32  `json:"config_revision"`
	Nodes          []struct {
		NativeID   string `json:"native_id"`
		Revision   string `json:"revision"`
		Title      string `json:"title"`
		Status     string `json:"status"`
		ReceiptID  string `json:"receipt_id"`
		ObservedAt string `json:"observed_at"`
	} `json:"nodes"`
	Edges []struct {
		PredecessorNativeID string `json:"predecessor_native_id"`
		ConsumerNativeID    string `json:"consumer_native_id"`
		DependencyType      string `json:"dependency_type"`
	} `json:"edges"`
	Digest string `json:"digest"`
}

type workflowDraftRun struct {
	ID             string          `json:"id"`
	WorkspaceID    string          `json:"workspace_id"`
	ProjectID      string          `json:"project_id"`
	SourceID       string          `json:"source_id"`
	RequestID      string          `json:"request_id"`
	RootNativeID   string          `json:"root_native_id"`
	ConfigRevision int32           `json:"config_revision"`
	Capacity       int32           `json:"capacity"`
	Status         string          `json:"status"`
	Graph          json.RawMessage `json:"graph"`
	NodeState      json.RawMessage `json:"node_state"`
	CreatedBy      string          `json:"created_by"`
	CreatedAt      string          `json:"created_at"`
}

func draftGet(t *testing.T, ctx context.Context, token, ws, runID string, status int) workflowDraftRun {
	t.Helper()
	_, raw := mustSourceReadCall(t, ctx, http.MethodGet, "/api/workflow-runs/"+runID, token, ws, "", status)
	var run workflowDraftRun
	if status < 300 {
		if err := json.Unmarshal(raw, &run); err != nil {
			t.Fatalf("decode workflow draft: %v", err)
		}
	}
	return run
}

// TestWorkflowDraftHappyPathThroughRouter walks the full owner flow: read
// receipts for a two-predecessor closure created, claimed, and reported
// through the production router with a scoped source-read credential, then
// a draft created from them, read back, replayed identically, and refused
// on changed payload, disabled source, and stale config without
// queueing a task or writing a synthetic issue.
func TestWorkflowDraftHappyPathThroughRouter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	runtimeID, sourceID, configRevision := draftSourceFixture(t, fx, testWorkspaceID, testUserID, "draft happy runtime", "wfd-happy", "")
	msr := sourceReadExchange(t, ctx, runtimeID, testToken)

	issuesBefore := fx.Count(t, `SELECT count(*) FROM issue WHERE workspace_id=$1`, testWorkspaceID)
	tasksBefore := fx.Count(t, `SELECT count(*) FROM agent_task_queue q JOIN agent a ON a.id=q.agent_id WHERE a.workspace_id=$1`, testWorkspaceID)

	receiptA := draftReceipt(t, ctx, sourceID, runtimeID, msr, testToken, testWorkspaceID, "wfd-a", draftCompleteItem("wfd-a", "rev-a"))
	receiptB := draftReceipt(t, ctx, sourceID, runtimeID, msr, testToken, testWorkspaceID, "wfd-b", draftCompleteItem("wfd-b", "rev-b"))
	receiptRoot := draftReceipt(t, ctx, sourceID, runtimeID, msr, testToken, testWorkspaceID, "wfd-root", draftCompleteItem("wfd-root", "rev-root", [2]string{"wfd-a", "blocks"}, [2]string{"wfd-b", "blocks"}))

	requestID := uuid.NewString()
	body := draftBody(requestID, sourceID, "wfd-root", "rev-root", int(configRevision), 2, []string{receiptRoot, receiptA, receiptB})
	_, raw := mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID, body, http.StatusCreated)
	var run workflowDraftRun
	if err := json.Unmarshal(raw, &run); err != nil {
		t.Fatal(err)
	}
	if run.ID == "" || run.RequestID != requestID || run.SourceID != sourceID || run.WorkspaceID != testWorkspaceID {
		t.Fatalf("draft lost scope identity: %+v", run)
	}
	if run.RootNativeID != "wfd-root" || run.ConfigRevision != configRevision || run.Capacity != 2 || run.Status != "draft" || run.CreatedBy != testUserID || run.CreatedAt == "" {
		t.Fatalf("draft fields wrong: %+v", run)
	}
	var graph draftGraphJSON
	if err := json.Unmarshal(run.Graph, &graph); err != nil {
		t.Fatal(err)
	}
	if graph.WorkspaceID != testWorkspaceID || graph.SourceID != sourceID || graph.RootNativeID != "wfd-root" || graph.ConfigRevision != configRevision {
		t.Fatalf("graph scope wrong: %+v", graph)
	}
	wantNodes := []struct{ id, rev, receipt string }{{"wfd-a", "rev-a", receiptA}, {"wfd-b", "rev-b", receiptB}, {"wfd-root", "rev-root", receiptRoot}}
	if len(graph.Nodes) != len(wantNodes) {
		t.Fatalf("graph nodes: %d want %d", len(graph.Nodes), len(wantNodes))
	}
	for i, want := range wantNodes {
		node := graph.Nodes[i]
		if node.NativeID != want.id || node.Revision != want.rev || node.ReceiptID != want.receipt {
			t.Fatalf("node %d wrong: %+v want %v", i, node, want)
		}
		if node.Title != "title-"+want.id || node.Status != "todo" || node.ObservedAt == "" {
			t.Fatalf("node %d observation fields wrong: %+v", i, node)
		}
	}
	wantEdges := [][2]string{{"wfd-a", "wfd-root"}, {"wfd-b", "wfd-root"}}
	if len(graph.Edges) != len(wantEdges) {
		t.Fatalf("graph edges: %d want %d", len(graph.Edges), len(wantEdges))
	}
	for i, want := range wantEdges {
		edge := graph.Edges[i]
		if edge.PredecessorNativeID != want[0] || edge.ConsumerNativeID != want[1] || edge.DependencyType != "blocks" {
			t.Fatalf("edge %d wrong: %+v want %v", i, edge, want)
		}
	}
	if len(graph.Digest) != 64 || strings.Trim(graph.Digest, "0123456789abcdef") != "" {
		t.Fatalf("digest must be 64 hex chars: %q", graph.Digest)
	}
	var nodeState map[string]struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(run.NodeState, &nodeState); err != nil {
		t.Fatal(err)
	}
	if len(nodeState) != 3 {
		t.Fatalf("node_state entries: %d want 3", len(nodeState))
	}
	for _, id := range []string{"wfd-a", "wfd-b", "wfd-root"} {
		state, ok := nodeState[id]
		if !ok || state.Status != "blocked" || state.Reason != "draft" {
			t.Fatalf("node_state[%s] wrong: %+v present=%t", id, state, ok)
		}
	}

	// Member read of the frozen draft.
	member := fx.User(t, "draft reader", "wfd-reader-"+uuid.NewString()+"@example.test")
	fx.Member(t, testWorkspaceID, member, "member")
	memberJWT, err := generateTestJWT(member, "", "draft reader")
	if err != nil {
		t.Fatal(err)
	}
	fetched := draftGet(t, ctx, memberJWT, testWorkspaceID, run.ID, http.StatusOK)
	if string(fetched.Graph) != string(run.Graph) || string(fetched.NodeState) != string(run.NodeState) {
		t.Fatal("GET changed frozen graph or node state bytes")
	}

	// Identical retry with reordered receipts: 200, same run.
	retry := draftBody(requestID, sourceID, "wfd-root", "rev-root", int(configRevision), 2, []string{receiptA, receiptB, receiptRoot})
	_, rawRetry := mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID, retry, http.StatusOK)
	var replay workflowDraftRun
	if err := json.Unmarshal(rawRetry, &replay); err != nil || replay.ID != run.ID {
		t.Fatalf("identical retry must return same run: err=%v same=%t", err, replay.ID == run.ID)
	}

	// Same request UUID, changed capacity: 409, original unchanged.
	changed := draftBody(requestID, sourceID, "wfd-root", "rev-root", int(configRevision), 1, []string{receiptRoot, receiptA, receiptB})
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID, changed, http.StatusConflict)
	draftGet(t, ctx, testToken, testWorkspaceID, run.ID, http.StatusOK)

	// Disabled source refuses new drafts but keeps serving identical ones.
	fx.Exec(t, `UPDATE work_source SET enabled=false WHERE id=$1`, sourceID)
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID,
		draftBody(uuid.NewString(), sourceID, "wfd-root", "rev-root", int(configRevision), 2, []string{receiptRoot, receiptA, receiptB}), http.StatusConflict)
	_, rawDisabled := mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID, retry, http.StatusOK)
	var disabledReplay workflowDraftRun
	if err := json.Unmarshal(rawDisabled, &disabledReplay); err != nil || disabledReplay.ID != run.ID {
		t.Fatalf("identical retry on disabled source must return same run: err=%v same=%t", err, disabledReplay.ID == run.ID)
	}
	fx.Exec(t, `UPDATE work_source SET enabled=true WHERE id=$1`, sourceID)

	// Stale expected config revision: 409.
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID,
		draftBody(uuid.NewString(), sourceID, "wfd-root", "rev-root", int(configRevision)+1, 2, []string{receiptRoot, receiptA, receiptB}), http.StatusConflict)

	// No start surface exists for drafts.
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs/"+run.ID+"/start", testToken, testWorkspaceID, "{}", http.StatusNotFound)

	// The whole flow never queues a task or writes a synthetic issue.
	if got := fx.Count(t, `SELECT count(*) FROM issue WHERE workspace_id=$1`, testWorkspaceID); got != issuesBefore {
		t.Fatalf("draft flow wrote issues: before=%d after=%d", issuesBefore, got)
	}
	if got := fx.Count(t, `SELECT count(*) FROM agent_task_queue q JOIN agent a ON a.id=q.agent_id WHERE a.workspace_id=$1`, testWorkspaceID); got != tasksBefore {
		t.Fatalf("draft flow queued agent tasks: before=%d after=%d", tasksBefore, got)
	}
}

// TestWorkflowDraftClosureEligibility proves every unsupported closure is a
// 422 without side effects: missing dependency receipt, non-blocks edge
// kind, dependency endpoint without a receipt, legacy incomplete
// observation, cycle, stale root revision, and a receipt from a different
// source.
func TestWorkflowDraftClosureEligibility(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	runtimeID, sourceID, configRevision := draftSourceFixture(t, fx, testWorkspaceID, testUserID, "draft eligibility runtime", "wfd-eligible", "")
	msr := sourceReadExchange(t, ctx, runtimeID, testToken)
	otherRuntimeID, otherSourceID, _ := draftSourceFixture(t, fx, testWorkspaceID, testUserID, "draft other runtime", "wfd-other", "")

	create := func(root, revision string, receiptIDs []string) {
		t.Helper()
		body := draftBody(uuid.NewString(), sourceID, root, revision, int(configRevision), 1, receiptIDs)
		mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID, body, http.StatusUnprocessableEntity)
	}

	t.Run("missing dependency receipt", func(t *testing.T) {
		a := draftReceipt(t, ctx, sourceID, runtimeID, msr, testToken, testWorkspaceID, "el1-a", draftCompleteItem("el1-a", "el1-a-r"))
		root := draftReceipt(t, ctx, sourceID, runtimeID, msr, testToken, testWorkspaceID, "el1-root", draftCompleteItem("el1-root", "el1-root-r", [2]string{"el1-a", "blocks"}, [2]string{"el1-b", "blocks"}))
		create("el1-root", "el1-root-r", []string{root, a})
	})
	t.Run("unknown dependency kind", func(t *testing.T) {
		a := draftReceipt(t, ctx, sourceID, runtimeID, msr, testToken, testWorkspaceID, "el2-a", draftCompleteItem("el2-a", "el2-a-r"))
		root := draftReceipt(t, ctx, sourceID, runtimeID, msr, testToken, testWorkspaceID, "el2-root", draftCompleteItem("el2-root", "el2-root-r", [2]string{"el2-a", "relates_to"}))
		create("el2-root", "el2-root-r", []string{root, a})
	})
	t.Run("legacy incomplete observation", func(t *testing.T) {
		root := draftReceipt(t, ctx, sourceID, runtimeID, msr, testToken, testWorkspaceID, "el4-root", draftLegacyItem("el4-root", "el4-root-r"))
		create("el4-root", "el4-root-r", []string{root})
	})
	t.Run("dependency cycle", func(t *testing.T) {
		a := draftReceipt(t, ctx, sourceID, runtimeID, msr, testToken, testWorkspaceID, "el5-a", draftCompleteItem("el5-a", "el5-a-r", [2]string{"el5-root", "blocks"}))
		root := draftReceipt(t, ctx, sourceID, runtimeID, msr, testToken, testWorkspaceID, "el5-root", draftCompleteItem("el5-root", "el5-root-r", [2]string{"el5-a", "blocks"}))
		create("el5-root", "el5-root-r", []string{root, a})
	})
	t.Run("stale root revision", func(t *testing.T) {
		root := draftReceipt(t, ctx, sourceID, runtimeID, msr, testToken, testWorkspaceID, "el6-root", draftCompleteItem("el6-root", "el6-root-r"))
		create("el6-root", "stale-revision", []string{root})
	})
	t.Run("receipt from another source", func(t *testing.T) {
		root := draftReceipt(t, ctx, sourceID, runtimeID, msr, testToken, testWorkspaceID, "el7-root", draftCompleteItem("el7-root", "el7-root-r"))
		// msr is fenced to its runtime: the foreign source's receipt needs a
		// token exchanged for that runtime, not a reuse of the first.
		otherMSR := sourceReadExchange(t, ctx, otherRuntimeID, testToken)
		foreign := draftReceipt(t, ctx, otherSourceID, otherRuntimeID, otherMSR, testToken, testWorkspaceID, "el7-foreign", draftCompleteItem("el7-foreign", "el7-f-r"))
		create("el7-root", "el7-root-r", []string{root, foreign})
	})

	if got := fx.Count(t, `SELECT count(*) FROM workflow_run WHERE workspace_id=$1`, testWorkspaceID); got != 0 {
		t.Fatalf("ineligible requests must not persist drafts: count=%d", got)
	}
}

// TestWorkflowDraftAuthorizationAndValidation covers the credential, role,
// workspace-scope, and body validation surface of both routes.
func TestWorkflowDraftAuthorizationAndValidation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	runtimeID, sourceID, configRevision := draftSourceFixture(t, fx, testWorkspaceID, testUserID, "draft authz runtime", "wfd-authz", "")
	msr := sourceReadExchange(t, ctx, runtimeID, testToken)

	// One real draft for the member-read allowance.
	receipt := draftReceipt(t, ctx, sourceID, runtimeID, msr, testToken, testWorkspaceID, "authz-root", draftCompleteItem("authz-root", "authz-r"))
	_, raw := mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID,
		draftBody(uuid.NewString(), sourceID, "authz-root", "authz-r", int(configRevision), 1, []string{receipt}), http.StatusCreated)
	var run workflowDraftRun
	if err := json.Unmarshal(raw, &run); err != nil {
		t.Fatal(err)
	}

	member := fx.User(t, "draft member", "wfd-member-"+uuid.NewString()+"@example.test")
	fx.Member(t, testWorkspaceID, member, "member")
	memberJWT, err := generateTestJWT(member, "", "draft member")
	if err != nil {
		t.Fatal(err)
	}

	dummyReceipts := []string{uuid.NewString()}
	valid := draftBody(uuid.NewString(), sourceID, "authz-root", "authz-r", int(configRevision), 1, dummyReceipts)

	// Credentials and roles.
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", "", testWorkspaceID, valid, http.StatusUnauthorized)
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", msr, testWorkspaceID, valid, http.StatusUnauthorized)
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", memberJWT, testWorkspaceID, valid, http.StatusForbidden)
	// Member read is allowed.
	draftGet(t, ctx, memberJWT, testWorkspaceID, run.ID, http.StatusOK)

	// Scope concealment: unknown workspace header, unknown source, and a
	// source that lives in another workspace are all 404.
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, uuid.NewString(), valid, http.StatusNotFound)
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID,
		draftBody(uuid.NewString(), uuid.NewString(), "authz-root", "authz-r", int(configRevision), 1, dummyReceipts), http.StatusNotFound)
	foreignWS := fx.Workspace(t, "Draft foreign workspace", "wfd-foreign-"+uuid.NewString())
	_, foreignSource, _ := draftSourceFixture(t, fx, foreignWS, testUserID, "draft foreign runtime", "wfd-foreign", "")
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID,
		draftBody(uuid.NewString(), foreignSource, "authz-root", "authz-r", int(configRevision), 1, dummyReceipts), http.StatusNotFound)

	// Malformed identifiers and bounds.
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID,
		draftBody("not-a-uuid", sourceID, "authz-root", "authz-r", int(configRevision), 1, dummyReceipts), http.StatusBadRequest)
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID,
		draftBody(uuid.NewString(), "not-a-uuid", "authz-root", "authz-r", int(configRevision), 1, dummyReceipts), http.StatusBadRequest)
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID,
		draftBody(uuid.NewString(), sourceID, "authz-root", "authz-r", int(configRevision), 1, []string{"not-a-uuid"}), http.StatusBadRequest)
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID,
		draftBody(uuid.NewString(), sourceID, "authz-root", "authz-r", int(configRevision), 1, nil), http.StatusBadRequest)
	tooMany := make([]string, 129)
	for i := range tooMany {
		tooMany[i] = uuid.NewString()
	}
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID,
		draftBody(uuid.NewString(), sourceID, "authz-root", "authz-r", int(configRevision), 1, tooMany), http.StatusBadRequest)
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID,
		draftBody(uuid.NewString(), sourceID, "authz-root", "authz-r", int(configRevision), 1, []string{receipt, receipt}), http.StatusBadRequest)
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID,
		draftBody(uuid.NewString(), sourceID, "authz-root", "authz-r", int(configRevision), 0, dummyReceipts), http.StatusBadRequest)
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID,
		draftBody(uuid.NewString(), sourceID, "authz-root", "authz-r", int(configRevision), 3, dummyReceipts), http.StatusBadRequest)

	// Body strictness: unknown field, trailing data, array, null, empty.
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID, strings.TrimSuffix(valid, "}")+`,"extra":1}`, http.StatusBadRequest)
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID, valid+" {}", http.StatusBadRequest)
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID, "[]", http.StatusBadRequest)
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID, "null", http.StatusBadRequest)
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID, "", http.StatusBadRequest)

	// GET validation.
	mustSourceReadCall(t, ctx, http.MethodGet, "/api/workflow-runs/not-a-uuid", testToken, testWorkspaceID, "", http.StatusBadRequest)
	mustSourceReadCall(t, ctx, http.MethodGet, "/api/workflow-runs/"+uuid.NewString(), testToken, testWorkspaceID, "", http.StatusNotFound)
	mustSourceReadCall(t, ctx, http.MethodGet, "/api/workflow-runs/"+run.ID, testToken, uuid.NewString(), "", http.StatusNotFound)
}

// TestWorkflowDraftConcurrentIdenticalCreate proves the request guard under
// bounded concurrency: two identical POSTs with the same request UUID race
// through the production router and resolve to exactly one created draft
// (201 + 200, same run id, one row), and a later POST with the same request
// UUID but a different source is a 409 from the request-identity check
// before any project/source precondition is consulted.
func TestWorkflowDraftConcurrentIdenticalCreate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	runtimeID, sourceID, configRevision := draftSourceFixture(t, fx, testWorkspaceID, testUserID, "draft concurrent runtime", "wfd-concurrent", "")
	_, otherSourceID, _ := draftSourceFixture(t, fx, testWorkspaceID, testUserID, "draft concurrent other", "wfd-concurrent-other", "")
	msr := sourceReadExchange(t, ctx, runtimeID, testToken)
	receipt := draftReceipt(t, ctx, sourceID, runtimeID, msr, testToken, testWorkspaceID, "conc-root", draftCompleteItem("conc-root", "conc-r"))

	requestID := uuid.NewString()
	body := draftBody(requestID, sourceID, "conc-root", "conc-r", int(configRevision), 1, []string{receipt})

	// Bounded: exactly two racing callers, both the owner. mustSourceReadCall
	// would t.Fatalf from a non-test goroutine, so collect raw outcomes and
	// assert in the main goroutine.
	type outcome struct {
		status int
		run    workflowDraftRun
		err    error
	}
	done := make(chan outcome, 2)
	for i := 0; i < 2; i++ {
		go func() {
			resp, data, err := sourceReadCall(ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID, body)
			if err != nil {
				done <- outcome{err: err}
				return
			}
			resp.Body.Close()
			var run workflowDraftRun
			if resp.StatusCode < 300 {
				if err := json.Unmarshal(data, &run); err != nil {
					done <- outcome{err: err}
					return
				}
			}
			done <- outcome{status: resp.StatusCode, run: run}
		}()
	}
	var first, second outcome
	for i := 0; i < 2; i++ {
		out := <-done
		if out.err != nil {
			t.Fatalf("concurrent create: %v", out.err)
		}
		if out.status == http.StatusCreated {
			first = out
		} else {
			second = out
		}
	}
	if first.status != http.StatusCreated || second.status != http.StatusOK {
		t.Fatalf("statuses must be 201 and 200: created=%d other=%d", first.status, second.status)
	}
	if first.run.ID == "" || first.run.ID != second.run.ID {
		t.Fatalf("both callers must observe the same run: %q vs %q", first.run.ID, second.run.ID)
	}
	if got := fx.Count(t, `SELECT count(*) FROM workflow_run WHERE workspace_id=$1 AND request_id::text=$2`, testWorkspaceID, requestID); got != 1 {
		t.Fatalf("exactly one row must persist: count=%d", got)
	}

	// Same workspace and request UUID, different source: the request guard
	// rejects the changed identity before project/source preconditions.
	changedSource := draftBody(requestID, otherSourceID, "conc-root", "conc-r", int(configRevision), 1, []string{receipt})
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID, changedSource, http.StatusConflict)
	if got := fx.Count(t, `SELECT count(*) FROM workflow_run WHERE workspace_id=$1 AND request_id::text=$2`, testWorkspaceID, requestID); got != 1 {
		t.Fatalf("changed-source conflict must not add rows: count=%d", got)
	}
}

// TestWorkflowDraftParentLifecycle proves source deletion sweeps its drafts
// (GET turns 404), project deletion detaches project_id while the frozen
// graph stays byte-identical, and workspace deletion sweeps drafts in a
// disposable workspace fixture.
func TestWorkflowDraftParentLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)

	// Source deletion sweeps the draft.
	runtimeID, sourceID, configRevision := draftSourceFixture(t, fx, testWorkspaceID, testUserID, "draft sweep runtime", "wfd-sweep", "")
	msr := sourceReadExchange(t, ctx, runtimeID, testToken)
	receipt := draftReceipt(t, ctx, sourceID, runtimeID, msr, testToken, testWorkspaceID, "sweep-root", draftCompleteItem("sweep-root", "sweep-r"))
	_, raw := mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID,
		draftBody(uuid.NewString(), sourceID, "sweep-root", "sweep-r", int(configRevision), 1, []string{receipt}), http.StatusCreated)
	var swept workflowDraftRun
	if err := json.Unmarshal(raw, &swept); err != nil {
		t.Fatal(err)
	}
	mustSourceReadCall(t, ctx, http.MethodDelete, "/api/work-sources/"+sourceID, testToken, testWorkspaceID, "", http.StatusNoContent)
	draftGet(t, ctx, testToken, testWorkspaceID, swept.ID, http.StatusNotFound)

	// Project deletion detaches the draft without touching its graph.
	project := fx.Project(t, "Draft project")
	pruntimeID, psourceID, pconfigRevision := draftSourceFixture(t, fx, testWorkspaceID, testUserID, "draft project runtime", "wfd-project", project)
	pmsr := sourceReadExchange(t, ctx, pruntimeID, testToken)
	preceipt := draftReceipt(t, ctx, psourceID, pruntimeID, pmsr, testToken, testWorkspaceID, "proj-root", draftCompleteItem("proj-root", "proj-r"))
	_, praw := mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID,
		draftBody(uuid.NewString(), psourceID, "proj-root", "proj-r", int(pconfigRevision), 1, []string{preceipt}), http.StatusCreated)
	var attached workflowDraftRun
	if err := json.Unmarshal(praw, &attached); err != nil {
		t.Fatal(err)
	}
	if attached.ProjectID != project {
		t.Fatalf("project draft must carry project_id: got %q want %q", attached.ProjectID, project)
	}
	mustSourceReadCall(t, ctx, http.MethodDelete, "/api/projects/"+project, testToken, testWorkspaceID, "", http.StatusNoContent)
	detached := draftGet(t, ctx, testToken, testWorkspaceID, attached.ID, http.StatusOK)
	if detached.ProjectID != "" {
		t.Fatalf("project deletion must detach project_id: got %q", detached.ProjectID)
	}
	if string(detached.Graph) != string(attached.Graph) || string(detached.NodeState) != string(attached.NodeState) {
		t.Fatal("project deletion changed the frozen graph")
	}

	// Workspace deletion sweeps drafts, exercised on a disposable workspace.
	ws := fx.Workspace(t, "Draft disposable workspace", "wfd-ws-"+uuid.NewString())
	owner := fx.User(t, "draft ws owner", "wfd-owner-"+uuid.NewString()+"@example.test")
	fx.Member(t, ws, owner, "owner")
	ownerJWT, err := generateTestJWT(owner, "", "draft ws owner")
	if err != nil {
		t.Fatal(err)
	}
	wruntimeID, wsourceID, wconfigRevision := draftSourceFixture(t, fx, ws, owner, "draft ws runtime", "wfd-ws", "")
	_, exRaw := mustSourceReadCall(t, ctx, http.MethodPost, "/api/daemon/runtimes/"+wruntimeID+"/source-read-token", ownerJWT, ws, `{"scope":"source:read"}`, http.StatusOK)
	var exchange struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(exRaw, &exchange); err != nil || exchange.Token == "" {
		t.Fatalf("invalid exchange response: err=%v token_present=%t", err, exchange.Token != "")
	}
	wmsr := exchange.Token
	wreceipt := draftReceipt(t, ctx, wsourceID, wruntimeID, wmsr, ownerJWT, ws, "ws-root", draftCompleteItem("ws-root", "ws-r"))
	_, wraw := mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", ownerJWT, ws,
		draftBody(uuid.NewString(), wsourceID, "ws-root", "ws-r", int(wconfigRevision), 1, []string{wreceipt}), http.StatusCreated)
	var wsRun workflowDraftRun
	if err := json.Unmarshal(wraw, &wsRun); err != nil {
		t.Fatal(err)
	}
	mustSourceReadCall(t, ctx, http.MethodDelete, "/api/workspaces/"+ws, ownerJWT, ws, "", http.StatusNoContent)
	mustSourceReadCall(t, ctx, http.MethodGet, "/api/workflow-runs/"+wsRun.ID, ownerJWT, ws, "", http.StatusNotFound)
	if got := fx.Count(t, `SELECT count(*) FROM workflow_run WHERE workspace_id=$1`, ws); got != 0 {
		t.Fatalf("workspace deletion retained drafts: count=%d", got)
	}
}
