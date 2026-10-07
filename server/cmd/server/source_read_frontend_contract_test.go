package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/beads"
)

// TestSourceReadFrontendContract is the cross-language acceptance bridge: the
// Go production router (testServer) plus PostgreSQL prepare terminal
// work-source read receipts through real authenticated HTTP, then the exact
// checked-out core vitest integration test consumes the same server through
// the real ApiClient and parseWorkSourceReadResult. Gated behind
// MULTICA_RUN_SOURCE_CLIENT_CONTRACT=1 and MULTICA_SOURCE_CLIENT_PNPM so
// ordinary `go test` never needs node. The private JWT reaches the child via
// environment only and is never printed.
func TestSourceReadFrontendContract(t *testing.T) {
	if os.Getenv("MULTICA_RUN_SOURCE_CLIENT_CONTRACT") != "1" {
		t.Skip("set MULTICA_RUN_SOURCE_CLIENT_CONTRACT=1 (and MULTICA_SOURCE_CLIENT_PNPM) to run the source-client contract bridge")
	}
	if testPool == nil {
		t.Fatal("explicit source-client contract requires the managed database fixture")
	}
	pnpm := os.Getenv("MULTICA_SOURCE_CLIENT_PNPM")
	if pnpm == "" {
		t.Fatal("MULTICA_SOURCE_CLIENT_PNPM must be an absolute pnpm path")
	}
	if !filepath.IsAbs(pnpm) {
		t.Fatalf("MULTICA_SOURCE_CLIENT_PNPM must be absolute: %q", pnpm)
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source directory")
	}
	coreDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "packages", "core")
	vitestFile := "work-sources/read-contract.integration.test.ts"
	if _, err := os.Stat(filepath.Join(coreDir, "vitest.config.ts")); err != nil {
		t.Fatalf("core workspace missing: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	runtimeID, _ := sourceReadFixture(t, fx, testUserID, "source contract runtime", "source-contract-"+uuid.NewString())
	var sourceID string
	fx.QueryRow(t, `SELECT id FROM work_source WHERE runtime_id=$1`, runtimeID).Scan(&sourceID)

	commandPath := "/api/work-sources/" + sourceID + "/commands"
	msr := sourceReadExchange(t, ctx, runtimeID, testToken)
	pendingPath := "/api/daemon/runtimes/" + runtimeID + "/work-source-commands"

	// prepareTerminal drives one command from create to a succeeded receipt
	// carrying exactly the canonical result payload the server must store.
	prepareTerminal := func(body, result string) (commandID, requestID string) {
		t.Helper()
		requestID = uuid.NewString()
		created := strings.Replace(body, `"REQUEST"`, `"`+requestID+`"`, 1)
		_, data := mustSourceReadCall(t, ctx, http.MethodPost, commandPath, testToken, testWorkspaceID, created, http.StatusCreated)
		var receipt struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(data, &receipt); err != nil {
			t.Fatal(err)
		}
		commandID = receipt.ID
		claimPath := pendingPath + "/" + commandID + "/claim"
		resultPath := pendingPath + "/" + commandID + "/result"
		mustSourceReadCall(t, ctx, http.MethodPost, claimPath, msr, testWorkspaceID, "", http.StatusOK)
		report := map[string]any{"status": "succeeded", "result": result}
		reportBody, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		mustSourceReadCall(t, ctx, http.MethodPost, resultPath, msr, testWorkspaceID, string(reportBody), http.StatusOK)
		return commandID, requestID
	}

	listResult, err := json.Marshal([]beads.IssueSummary{
		{ID: "bd-list-1", Title: "Contract list one", Status: "queued-weird", Priority: 1, IssueType: "task", DependencyCount: 0, DependentCount: 2},
		{ID: "bd-list-2", Title: "Contract list two", Description: strPtr("second"), Status: "open", Priority: 0, IssueType: "bug", DependencyCount: 3, DependentCount: 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	listCommandID, listRequestID := prepareTerminal(`{"request_id":"REQUEST","command":"list","limit":2}`, string(listResult))

	// Revision is opaque and whitespace-significant; the trailing spaces must
	// survive the server round trip into the TS parser untouched.
	detailResult, err := json.Marshal(beads.Issue{
		IssueSummary: beads.IssueSummary{ID: "bd-detail-1", Title: "Contract detail", Status: "in-progress", Priority: 2, IssueType: "story", DependencyCount: 1, DependentCount: 0},
		Revision:     "rev opaque  trailing  ",
	})
	if err != nil {
		t.Fatal(err)
	}
	detailCommandID, _ := prepareTerminal(`{"request_id":"REQUEST","command":"read","native_id":"bd-detail-1"}`, string(detailResult))

	// The TS leg owns the identical-retry check against the already-terminal
	// list receipt, so no outstanding read command is ever left behind.
	defaultListResult, err := json.Marshal([]beads.IssueSummary{
		{ID: "bd-default-1", Title: "Default list one", Status: "backlog-weird", Priority: 0, IssueType: "issue", DependencyCount: 0, DependentCount: 1},
		{ID: "bd-default-2", Title: "Default list two", Description: strPtr("default second"), Status: "triaged-weird", Priority: 1, IssueType: "task", DependencyCount: 2, DependentCount: 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The create body omits `limit`: the reproduced default-list bug is a
	// receipt whose wire limit_count is absent, not defaulted server-side.
	defaultListCommandID, defaultListRequestID := prepareTerminal(`{"request_id":"REQUEST","command":"list"}`, string(defaultListResult))

	// Native enrollment leg: prepare one pending and one enrolled source
	// through the real enrollment routes (intent/token/finalize) using the
	// fixture-owned runtime and the human owner credential. The manifest
	// hashes are test-owned opaque values; physical filesystem proof is a
	// client concern and deliberately not asserted here.
	pendingRuntime := nativeEnrollFixture(t, fx, testUserID, "mse contract pending runtime", "mse-contract-pending")
	pendingSourceID, _ := nativeEnrollIntent(t, ctx, pendingRuntime, testToken, uuid.NewString(), "native contract pending")

	enrolledRuntime := nativeEnrollFixture(t, fx, testUserID, "mse contract enrolled runtime", "mse-contract-enrolled")
	enrolledSourceID, enrolledEnrollmentID := nativeEnrollIntent(t, ctx, enrolledRuntime, testToken, uuid.NewString(), "native contract enrolled")
	enrolledHash := strings.Repeat("f6", 32)
	mse := nativeEnrollToken(t, ctx, enrolledRuntime, enrolledSourceID, testToken, enrolledEnrollmentID, 1, enrolledHash)
	mustSourceReadCall(t, ctx, http.MethodPost, nativeEnrollFinalizePath(enrolledRuntime, enrolledSourceID), mse, testWorkspaceID,
		nativeEnrollProofBody(enrolledEnrollmentID, 1, enrolledHash), http.StatusOK)

	child := exec.CommandContext(ctx, pnpm, "exec", "vitest", "run", vitestFile, "--reporter=json")
	child.Dir = coreDir
	child.Env = append(os.Environ(),
		"MULTICA_SOURCE_READ_CONTRACT_RUN=1",
		"MULTICA_SOURCE_READ_CONTRACT_BASE_URL="+testServer.URL,
		"MULTICA_SOURCE_READ_CONTRACT_TOKEN="+testToken,
		"MULTICA_SOURCE_READ_CONTRACT_WORKSPACE="+testWorkspaceID,
		"MULTICA_SOURCE_READ_CONTRACT_SOURCE_ID="+sourceID,
		"MULTICA_SOURCE_READ_CONTRACT_LIST_COMMAND="+listCommandID,
		"MULTICA_SOURCE_READ_CONTRACT_LIST_REQUEST="+listRequestID,
		"MULTICA_SOURCE_READ_CONTRACT_READ_COMMAND="+detailCommandID,
		"MULTICA_SOURCE_READ_CONTRACT_DEFAULT_LIST_COMMAND="+defaultListCommandID,
		"MULTICA_SOURCE_READ_CONTRACT_DEFAULT_LIST_REQUEST="+defaultListRequestID,
		"MULTICA_SOURCE_READ_CONTRACT_PENDING_SOURCE_ID="+pendingSourceID,
		"MULTICA_SOURCE_READ_CONTRACT_ENROLLED_SOURCE_ID="+enrolledSourceID,
		"MULTICA_SOURCE_READ_CONTRACT_ENROLLED_HASH="+enrolledHash,
	)
	out, err := child.CombinedOutput()
	// Vitest output can embed response bodies from failed expectations; the
	// auth JWT and the source-read capability token must never reach the log.
	safe := sanitizeSourceContractOutput(string(out), testToken, msr, mse)
	if err != nil {
		t.Fatalf("core vitest contract run failed: %v\n%s", err, safe)
	}
	var summary struct {
		Success         bool `json:"success"`
		NumTotalTests   int  `json:"numTotalTests"`
		NumPassedTests  int  `json:"numPassedTests"`
		NumPendingTests int  `json:"numPendingTests"`
		NumFailedTests  int  `json:"numFailedTests"`
	}
	if json.Unmarshal(out, &summary) != nil || !summary.Success || summary.NumTotalTests != 7 || summary.NumPassedTests != 7 || summary.NumPendingTests != 0 || summary.NumFailedTests != 0 {
		t.Fatalf("core contract must report exactly seven passed tests, no skips or failures\n%s", safe)
	}
}

// sanitizeSourceContractOutput redacts the literal session credentials plus
// source-read and source-enrollment capabilities from bounded child output
// before any failure message.
func sanitizeSourceContractOutput(out, jwt string, capabilities ...string) string {
	redact := func(s, secret string) string {
		if secret == "" {
			return s
		}
		return strings.ReplaceAll(s, secret, "[REDACTED]")
	}
	out = redact(out, jwt)
	for _, capability := range capabilities {
		out = redact(out, capability)
	}
	if len(out) > 8192 {
		out = out[:8192] + "\n[output truncated]"
	}
	return out
}
