package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// Exercise additive dependency evidence and legacy terminal replay through the
// production HTTP boundary, including rejection without changing the receipt.
func TestSourceReadDependencyReceiptsThroughRouter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	runtimeID, _ := sourceReadFixture(t, fx, testUserID, "Dependency receipts", "dependency-receipts")
	var sourceID string
	if err := testPool.QueryRow(ctx, `SELECT id FROM work_source WHERE runtime_id=$1`, runtimeID).Scan(&sourceID); err != nil {
		t.Fatal(err)
	}
	token := sourceReadExchange(t, ctx, runtimeID, testToken)
	for _, complete := range []bool{false, true} {
		name := "legacy"
		result := `{"id":"opaque-root","revision":"unchanged-item-revision"}`
		if complete {
			name = "complete"
			result = `{"id":"opaque-root","revision":"unchanged-item-revision","dependency_count":2,"dependencies_complete":true,"dependencies":[{"id":"opaque-a","dependency_type":"blocks"},{"id":"external:fixture:future","dependency_type":"future-kind"}]}`
		}
		t.Run(name, func(t *testing.T) {
			request := `{"request_id":"` + uuid.NewString() + `","command":"read","native_id":"opaque-root"}`
			createPath := "/api/work-sources/" + sourceID + "/commands"
			_, raw := mustSourceReadCall(t, ctx, http.MethodPost, createPath, testToken, testWorkspaceID, request, http.StatusCreated)
			var receipt struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				Result string `json:"result"`
			}
			if err := json.Unmarshal(raw, &receipt); err != nil || receipt.ID == "" {
				t.Fatalf("missing command identity: %v", err)
			}
			base := "/api/daemon/runtimes/" + runtimeID + "/work-source-commands/" + receipt.ID
			mustSourceReadCall(t, ctx, http.MethodPost, base+"/claim", token, testWorkspaceID, "", http.StatusOK)
			report := func(value string, status int) {
				t.Helper()
				body, err := json.Marshal(map[string]string{"status": "succeeded", "result": value})
				if err != nil {
					t.Fatal(err)
				}
				mustSourceReadCall(t, ctx, http.MethodPost, base+"/result", token, testWorkspaceID, string(body), status)
			}
			if complete {
				report(strings.Replace(result, `"dependency_count":2`, `"dependency_count":1`, 1), http.StatusBadRequest)
			}
			report(result, http.StatusOK)
			report(result, http.StatusOK)
			mustSourceReadCall(t, ctx, http.MethodPost, createPath, testToken, testWorkspaceID, request, http.StatusOK)
			_, raw = mustSourceReadCall(t, ctx, http.MethodGet, "/api/work-source-commands/"+receipt.ID, testToken, testWorkspaceID, "", http.StatusOK)
			if err := json.Unmarshal(raw, &receipt); err != nil || receipt.Status != "succeeded" {
				t.Fatalf("terminal receipt lost: %v", err)
			}
			accepted := receipt.Result
			if complete {
				if !strings.Contains(accepted, `"dependencies_complete":true`) || !strings.Contains(accepted, `"dependency_type":"future-kind"`) {
					t.Fatal("complete typed edge evidence lost in stored receipt")
				}
				report(strings.Replace(result, "opaque-a", "opaque-changed", 1), http.StatusConflict)
			} else if strings.Contains(accepted, `"dependencies"`) || strings.Contains(accepted, `"dependencies_complete"`) {
				t.Fatal("legacy canonical receipt gained additive dependency fields")
			}
			_, raw = mustSourceReadCall(t, ctx, http.MethodGet, "/api/work-source-commands/"+receipt.ID, testToken, testWorkspaceID, "", http.StatusOK)
			if err := json.Unmarshal(raw, &receipt); err != nil || receipt.Result != accepted {
				t.Fatalf("replay changed stored canonical bytes: %v", err)
			}
		})
	}
}
