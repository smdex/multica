package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// TestWorkflowDraftFrontendContract is the cross-language acceptance bridge
// for workflow drafts: the Go production router (testServer) plus PostgreSQL
// prepare an A/B->C closure's terminal read receipts through real
// authenticated HTTP, then the exact checked-out core vitest integration test
// consumes the same server through the real ApiClient
// createWorkflowDraft/getWorkflowDraft. Gated behind
// MULTICA_RUN_DRAFT_CLIENT_CONTRACT=1 and MULTICA_SOURCE_CLIENT_PNPM so
// ordinary `go test` never needs node. The private JWT and the source-read
// capability reach the child via environment only and are never printed.
func TestWorkflowDraftFrontendContract(t *testing.T) {
	if os.Getenv("MULTICA_RUN_DRAFT_CLIENT_CONTRACT") != "1" {
		t.Skip("set MULTICA_RUN_DRAFT_CLIENT_CONTRACT=1 (and MULTICA_SOURCE_CLIENT_PNPM) to run the draft-client contract bridge")
	}
	if testPool == nil {
		t.Fatal("explicit draft-client contract requires the managed database fixture")
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
	vitestFile := "api/workflow-draft-contract.integration.test.ts"
	if _, err := os.Stat(filepath.Join(coreDir, "vitest.config.ts")); err != nil {
		t.Fatalf("core workspace missing: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	runtimeID, sourceID, configRevision := draftSourceFixture(t, fx, testWorkspaceID, testUserID, "draft contract runtime", "wfd-contract", "")
	msr := sourceReadExchange(t, ctx, runtimeID, testToken)

	// A/B->C closure: two leaves plus a root that blocks both, prepared
	// through the production router with the scoped source-read credential.
	receiptA := draftReceipt(t, ctx, sourceID, runtimeID, msr, testToken, testWorkspaceID, "wfd-contract-a", draftCompleteItem("wfd-contract-a", "rev-wfd-contract-a"))
	receiptB := draftReceipt(t, ctx, sourceID, runtimeID, msr, testToken, testWorkspaceID, "wfd-contract-b", draftCompleteItem("wfd-contract-b", "rev-wfd-contract-b"))
	receiptRoot := draftReceipt(t, ctx, sourceID, runtimeID, msr, testToken, testWorkspaceID, "wfd-contract-root",
		draftCompleteItem("wfd-contract-root", "rev-wfd-contract-root", [2]string{"wfd-contract-a", "blocks"}, [2]string{"wfd-contract-b", "blocks"}))

	child := exec.CommandContext(ctx, pnpm, "exec", "vitest", "run", vitestFile, "--reporter=json")
	child.Dir = coreDir
	child.Env = append(os.Environ(),
		"MULTICA_DRAFT_CONTRACT_RUN=1",
		"MULTICA_DRAFT_CONTRACT_BASE_URL="+testServer.URL,
		"MULTICA_DRAFT_CONTRACT_TOKEN="+testToken,
		"MULTICA_DRAFT_CONTRACT_WORKSPACE="+testWorkspaceID,
		"MULTICA_DRAFT_CONTRACT_SOURCE_ID="+sourceID,
		"MULTICA_DRAFT_CONTRACT_REQUEST_ID="+uuid.NewString(),
		"MULTICA_DRAFT_CONTRACT_CONFIG_REVISION="+strconv.Itoa(int(configRevision)),
		"MULTICA_DRAFT_CONTRACT_CAPACITY=2",
		"MULTICA_DRAFT_CONTRACT_RECEIPTS="+strings.Join([]string{receiptRoot, receiptA, receiptB}, ","),
	)
	out, err := child.CombinedOutput()
	// Vitest output can embed response bodies from failed expectations; the
	// auth JWT and the source-read capability token must never reach the log.
	safe := sanitizeSourceContractOutput(string(out), testToken, msr)
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
	if json.Unmarshal(out, &summary) != nil || !summary.Success || summary.NumTotalTests != 3 || summary.NumPassedTests != 3 || summary.NumPendingTests != 0 || summary.NumFailedTests != 0 {
		t.Fatalf("core contract must report exactly three passed tests, no skips or failures\n%s", safe)
	}
}
