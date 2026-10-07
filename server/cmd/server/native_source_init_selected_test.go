package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// Explicit operator qualification only. Default tests never discover or run an
// installed Beads executable. The CLI creates a fresh disposable native source
// through the production router before invoking the selected initializer.
func TestNativeSourceInitCLISelectedBeads(t *testing.T) {
	executable := os.Getenv("MULTICA_NATIVE_INIT_TEST_BEADS")
	if executable == "" {
		t.Skip("explicit selected Beads qualification is not enabled")
	}
	if !filepath.IsAbs(executable) || filepath.Clean(executable) != executable {
		t.Fatal("MULTICA_NATIVE_INIT_TEST_BEADS must be an absolute clean executable path")
	}
	if os.Getenv(nseCLIEnv) == "" {
		t.Fatal("explicit selected Beads qualification requires the built CLI gate")
	}
	cliPath := nsiRequireInitCLI(t)
	if testPool == nil {
		t.Fatal("explicit selected Beads qualification requires the managed database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	home, _ := nsiHome(t)
	t.Setenv("PATH", filepath.Dir(executable))
	daemonID := "nsi-selected-" + uuid.NewString()
	runtimeID := nseDaemonRuntime(t, fx, testUserID, daemonID, "claude")
	requestID := uuid.NewString()
	source, _, err := nsiProvision(t, ctx, cliPath, testWorkspaceID, runtimeID, requestID, "selected Beads init qualification", daemonID, executable)
	// Register cleanup even when initialization fails after the durable intent.
	sourceID := nsiSourceIDByRequest(t, fx, requestID)
	nsiCleanupSource(t, fx, sourceID)
	if err != nil {
		t.Fatalf("selected Beads provision failed: %v", err)
	}
	if source.ID != sourceID || source.Enabled || source.NativeEnrollmentStat != "pending" {
		t.Fatal("fresh initialization must preserve the pending disabled source identity")
	}
	domainDir := filepath.Join(home, ".multica", "native-sources", sourceID)
	nseFileMode(t, filepath.Join(domainDir, ".native_initialized"), 0o600, "selected init receipt")
	if info, err := os.Lstat(filepath.Join(domainDir, ".beads")); err != nil || !info.IsDir() {
		t.Fatalf("selected initializer did not create a real Beads directory: %v", err)
	}
	markerBefore, err := os.ReadFile(filepath.Join(domainDir, ".native_owner"))
	if err != nil {
		t.Fatal(err)
	}
	receiptBefore, err := os.ReadFile(filepath.Join(domainDir, ".native_initialized"))
	if err != nil {
		t.Fatal(err)
	}
	indexBefore := nseReadIndex(t, sourceID)
	replay, _, err := nsiProvision(t, ctx, cliPath, testWorkspaceID, runtimeID, requestID, "selected Beads init qualification", daemonID, executable)
	if err != nil || replay.ID != sourceID || replay.NativeEnrollmentID != source.NativeEnrollmentID {
		t.Fatalf("selected initializer replay changed the durable intent or failed: %v", err)
	}
	markerAfter, err := os.ReadFile(filepath.Join(domainDir, ".native_owner"))
	if err != nil || string(markerBefore) != string(markerAfter) {
		t.Fatal("selected initializer replay changed the owner marker")
	}
	receiptAfter, err := os.ReadFile(filepath.Join(domainDir, ".native_initialized"))
	if err != nil || string(receiptBefore) != string(receiptAfter) {
		t.Fatal("selected initializer replay changed the successful init receipt")
	}
	if nseReadIndex(t, sourceID) != indexBefore {
		t.Fatal("selected initializer replay changed the profile index")
	}
	if !strings.Contains(string(receiptAfter), `"version":"1.3.1"`) {
		t.Fatal("selected initializer receipt does not identify the qualified version")
	}
}
