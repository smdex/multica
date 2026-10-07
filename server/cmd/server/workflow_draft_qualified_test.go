package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/beads"
)

// Explicitly gated actual source reads, then production receipt and draft APIs.
// No source writes, installed agents or account-backed execution occur.
func TestWorkflowDraftQualifiedReadsThroughRouter(t *testing.T) {
	if os.Getenv("MULTICA_RUN_BEADS_QUALIFICATION") != "1" {
		t.Skip("actual Beads qualification is opt-in")
	}
	if testPool == nil {
		t.Fatal("explicit qualification requires managed PostgreSQL")
	}
	executable, directory := os.Getenv("MULTICA_BEADS_EXECUTABLE"), os.Getenv("MULTICA_BEADS_DIRECTORY")
	if !filepath.IsAbs(executable) || !filepath.IsAbs(directory) {
		t.Fatal("explicit absolute executable and disposable source directory required")
	}
	ids := []string{os.Getenv("MULTICA_BEADS_A"), os.Getenv("MULTICA_BEADS_B"), os.Getenv("MULTICA_BEADS_ROOT")}
	for _, id := range ids {
		if id == "" {
			t.Fatal("qualification fixture IDs required")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	runtimeID, _ := sourceReadFixture(t, fx, testUserID, "Qualified draft", "qualified-draft-"+uuid.NewString())
	var sourceID string
	fx.QueryRow(t, `SELECT id FROM work_source WHERE runtime_id=$1`, runtimeID).Scan(&sourceID)
	fx.Cleanup(t, `DELETE FROM workflow_run WHERE source_id=$1 AND workspace_id=$2`, sourceID, testWorkspaceID)
	msr := sourceReadExchange(t, ctx, runtimeID, testToken)
	client := beads.Client{Executable: executable, BeadsDir: directory}
	receipts := make([]string, 0, 3)
	revisions := make(map[string]string, 3)
	for _, id := range ids {
		item, err := client.ReadTask(ctx, id)
		if err != nil {
			t.Fatalf("qualified detail read failed: %v", err)
		}
		revisions[id] = item.Revision
		create, _ := json.Marshal(map[string]any{"request_id": uuid.NewString(), "command": "read", "native_id": id})
		_, data := mustSourceReadCall(t, ctx, http.MethodPost, "/api/work-sources/"+sourceID+"/commands", testToken, testWorkspaceID, string(create), http.StatusCreated)
		var command struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(data, &command); err != nil || command.ID == "" {
			t.Fatal("missing command identity")
		}
		base := "/api/daemon/runtimes/" + runtimeID + "/work-source-commands/" + command.ID
		mustSourceReadCall(t, ctx, http.MethodPost, base+"/claim", msr, testWorkspaceID, "", http.StatusOK)
		result, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		report, _ := json.Marshal(map[string]string{"status": "succeeded", "result": string(result)})
		mustSourceReadCall(t, ctx, http.MethodPost, base+"/result", msr, testWorkspaceID, string(report), http.StatusOK)
		receipts = append(receipts, command.ID)
	}
	request := func(root string, receiptIDs []string) string {
		body, _ := json.Marshal(map[string]any{"request_id": uuid.NewString(), "source_id": sourceID, "root_native_id": root, "expected_root_revision": revisions[root], "expected_config_revision": 1, "capacity": 2, "receipt_ids": receiptIDs})
		return string(body)
	}
	_, raw := mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID, request(ids[0], receipts[:1]), http.StatusCreated)
	var draft struct {
		ID     string             `json:"id"`
		Status string             `json:"status"`
		Graph  service.DraftGraph `json:"graph"`
	}
	if err := json.Unmarshal(raw, &draft); err != nil || draft.ID == "" || draft.Status != "draft" || len(draft.Graph.Nodes) != 1 || len(draft.Graph.Edges) != 0 || draft.Graph.Nodes[0].Revision != revisions[ids[0]] {
		t.Fatal("qualified empty predecessor observation did not become a nonexecuting draft")
	}
	mustSourceReadCall(t, ctx, http.MethodGet, "/api/workflow-runs/"+draft.ID, testToken, testWorkspaceID, "", http.StatusOK)
	// All actual native predecessor receipts are present. The qualified root
	// also references an external endpoint, which must remain unsupported.
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/workflow-runs", testToken, testWorkspaceID, request(ids[2], receipts), http.StatusUnprocessableEntity)
}
