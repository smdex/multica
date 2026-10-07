package main

// Production-router + PostgreSQL acceptance for native source enrollment
// driven by ACTUAL CLI subprocesses and an ACTUAL daemon.Daemon.Run.
//
// Contract under test (CLI surface, server routes, and daemon loop are all
// production code; no test handlers, no mock server):
//
//   - `multica daemon source provision` (real binary at
//     $MULTICA_NATIVE_ENROLLMENT_TEST_CLI, run as a subprocess) creates a
//     pending DISABLED native source through the production router and the
//     machine-global cli.ProfileDir("")/native-sources/<S> marker plus lock
//     and the per-profile index. Exact request-id replay keeps S/E.
//   - A real daemon.New(Config, logger).Run adopts the indexed domain, holds
//     its OS lock (proven in-process: execenv.OpenNativeSource fails with
//     ErrNativeSourceBusy), and reconciles the CLI-staged approval inbox into
//     a finalized enrollment through the production finalize route.
//   - `multica daemon source approve` stages the ephemeral packet while the
//     daemon holds the lock; `multica daemon source status` observes only
//     public responses.
//   - Credentials ride the environment, never argv, and never appear in
//     CLI stdout/stderr, the marker, or the profile index. The approval
//     packet deliberately carries the short-lived credential, but only
//     inside the private (0600) ephemeral inbox, which the daemon consumes.
//   - No Beads launch, no Issue/agent/agent_task_queue rows appear.
//   - Cancel returns Run within a bound; the OS lock is released and the
//     marker/index survive shutdown.
//
// The multica binary is built by the operator (go build ./cmd/multica) and
// pointed at by MULTICA_NATIVE_ENROLLMENT_TEST_CLI; without it the tests
// skip. The managed database is owned by the root environment; when the CLI
// gate is set but the database is missing, the acceptance FAILS rather than
// skips. The managed-task refusal test needs only the CLI binary: no HTTP
// and no filesystem access may occur.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// nseCLIEnv names the absolute path of the operator-built multica binary.
const nseCLIEnv = "MULTICA_NATIVE_ENROLLMENT_TEST_CLI"

// nseProfile is the named CLI/daemon profile both the CLI subprocesses and
// the in-process daemon resolve, so the staged inbox is the daemon's own.
const nseProfile = "nse-acceptance"

func nseRequireCLI(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake agent executables are POSIX shell scripts")
	}
	path := os.Getenv(nseCLIEnv)
	if path == "" {
		t.Skipf("%s is not set: build the CLI (go build ./cmd/multica) and set it to the absolute binary path to run this acceptance", nseCLIEnv)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("%s must be an absolute path, got %q", nseCLIEnv, path)
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		t.Fatalf("%s must point at an executable file: %v", nseCLIEnv, err)
	}
	return path
}

// nseRunCLI runs the real binary with extraEnv applied over the test process
// environment. Credentials are only ever passed through the environment.
// Failure output prints stderr only; stdout may embed public source JSON and
// is never echoed raw.
func nseRunCLI(t *testing.T, ctx context.Context, cliPath string, extraEnv map[string]string, args ...string) (string, string, error) {
	t.Helper()
	cmd := exec.CommandContext(ctx, cliPath, args...)
	env := os.Environ()
	for k, v := range extraEnv {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// nseAssertSanitized fails when any credential material appears in CLI
// output. mse_ capabilities and the human token must never be printed.
func nseAssertSanitized(t *testing.T, out, errOut string) {
	t.Helper()
	for _, s := range []string{out, errOut} {
		if strings.Contains(s, testToken) {
			t.Fatal("CLI output leaked the human token")
		}
		if strings.Contains(s, "mse_") {
			t.Fatal("CLI output leaked an enrollment capability")
		}
	}
}

// nseCLISource decodes only the public fields of the CLI source JSON.
type nseCLISource struct {
	ID                   string `json:"id"`
	RuntimeID            string `json:"runtime_id"`
	WorkspaceID          string `json:"workspace_id"`
	DaemonID             string `json:"daemon_id"`
	Mode                 string `json:"mode"`
	Enabled              bool   `json:"enabled"`
	SourceHandle         string `json:"source_handle"`
	NativeEnrollmentID   string `json:"native_enrollment_id"`
	NativeEnrollmentStat string `json:"native_enrollment_status"`
	NativeManifestHash   string `json:"native_manifest_hash"`
}

// nseProvision runs the provision CLI and returns the decoded public source.
func nseProvision(t *testing.T, ctx context.Context, cliPath, workspaceID, runtimeID, requestID, name, daemonID string) nseCLISource {
	t.Helper()
	out, errOut, err := nseRunCLI(t, ctx, cliPath, nil,
		"--profile", nseProfile, "daemon", "source", "provision",
		"--workspace", workspaceID, "--runtime", runtimeID,
		"--name", name, "--request-id", requestID, "--daemon-id", daemonID)
	nseAssertSanitized(t, out, errOut)
	if err != nil {
		t.Fatalf("provision CLI failed: %v stderr=%q", err, strings.TrimSpace(errOut))
	}
	var source nseCLISource
	if err := json.Unmarshal([]byte(out), &source); err != nil {
		t.Fatalf("provision CLI stdout is not source JSON: %v", err)
	}
	return source
}

// nseDaemonRuntime stages an owned online local runtime under daemonID with
// the exact provider backing the fake agent executable. No work source is
// inserted: the provision CLI creates it.
func nseDaemonRuntime(t *testing.T, fx *testutil.Fixture, ownerID, daemonID, provider string) string {
	t.Helper()
	return fx.Insert(t, "agent_runtime", testutil.Cols{
		"workspace_id": testWorkspaceID,
		"name":         "nse acceptance runtime " + provider, "daemon_id": daemonID, "provider": provider,
		"runtime_mode": "local", "status": "online", "visibility": "private",
		"device_info": "", "metadata": testutil.Raw("'{}'::jsonb"), "owner_id": ownerID,
	})
}

// nseOpenGlobalRoot pins the machine-global native-sources root the same way
// production does: cli.ProfileDir("") is the global profile root regardless
// of the selected profile.
func nseOpenGlobalRoot(t *testing.T) (*os.Root, string) {
	t.Helper()
	base, err := cli.ProfileDir("")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(filepath.Join(base, "native-sources"))
	if err != nil {
		t.Fatal(err)
	}
	return root, filepath.Join(base, "native-sources")
}

// nseReadIndex decodes the per-profile index entry for sourceID.
func nseReadIndex(t *testing.T, sourceID string) execenv.NativeSourceIdentity {
	t.Helper()
	base, err := cli.ProfileDir(nseProfile)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(base, "native-source-enrollments", sourceID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var entry struct {
		SourceID string                       `json:"source_id"`
		Identity execenv.NativeSourceIdentity `json:"identity"`
	}
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatal(err)
	}
	if entry.SourceID != sourceID {
		t.Fatalf("index names source %q, want %q", entry.SourceID, sourceID)
	}
	return entry.Identity
}

// nseWaitFor polls pred until it returns true or the deadline passes.
func nseWaitFor(t *testing.T, what string, deadline time.Duration, pred func() bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for {
		if pred() {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// nseFileMode asserts an exact permission mask.
func nseFileMode(t *testing.T, path string, want os.FileMode, what string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Mode().Perm() != want {
		t.Fatalf("%s must be %v, got %v", what, want, info.Mode().Perm())
	}
}

func TestNativeSourceEnrollmentDaemonRunAcceptance(t *testing.T) {
	cliPath := nseRequireCLI(t)
	if testPool == nil {
		// Explicit opt-in with no managed database is a broken run, not a
		// silent skip: the acceptance cannot pass without the real router/PG.
		t.Fatal("explicit native enrollment acceptance requires the managed database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	rowQueries := map[string]string{
		"issue":            `SELECT count(*) FROM issue WHERE workspace_id=$1`,
		"agent":            `SELECT count(*) FROM agent WHERE workspace_id=$1`,
		"agent_task_queue": `SELECT count(*) FROM agent_task_queue atq JOIN agent a ON atq.agent_id=a.id WHERE a.workspace_id=$1`,
	}
	baseline := make(map[string]int)
	for table, query := range rowQueries {
		baseline[table] = fx.Count(t, query, testWorkspaceID)
	}

	// --- Isolated environment shared by the daemon (in-process) and every
	// CLI subprocess: fresh HOME, XDG dirs, empty SHELL, PATH reduced to the
	// fixture directory so no installed agent CLI is discoverable.
	home := t.TempDir()
	fixtureDir := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("SHELL", "")
	t.Setenv("PATH", fixtureDir)
	t.Setenv("MULTICA_TASK_CONFIG_ROOT", "")

	// --- Test-created fake agent: answers --version with a supported
	// version, refuses all real work, so the daemon can register its runtime
	// but can never launch agent work or Beads.
	fakeClaude := filepath.Join(fixtureDir, "claude")
	if err := os.WriteFile(fakeClaude, []byte(`#!/bin/sh
case "$1" in
--version) echo "2.3.0 (Claude Code)"; exit 0 ;;
*) echo "fake claude refuses real work" >&2; exit 64 ;;
esac
`), 0o755); err != nil {
		t.Fatal(err)
	}

	// --- Credentials for the CLI subprocesses: human token and server URL
	// through the environment only, never argv, never printed.
	t.Setenv("MULTICA_SERVER_URL", testServer.URL)
	t.Setenv("MULTICA_TOKEN", testToken)
	if err := cli.SaveCLIConfigForProfile(cli.CLIConfig{Token: testToken, ServerURL: testServer.URL}, nseProfile); err != nil {
		t.Fatal(err)
	}

	// --- Runtime fixture: claude/local runtime owned by testUserID under the
	// daemon identity the real daemon will register with. The provider must
	// match the Agents map key below so production registration adopts this
	// exact owned runtime row (unique index is workspace+daemon+provider).
	daemonID := "nse-" + uuid.NewString()
	runtimeID := nseDaemonRuntime(t, fx, testUserID, daemonID, "claude")

	// --- Phase 1: provision through the real CLI. First call creates the
	// pending disabled native source plus the machine-global marker/lock and
	// the per-profile index entry.
	requestID := uuid.NewString()
	source := nseProvision(t, ctx, cliPath, testWorkspaceID, runtimeID, requestID, "native acceptance source", daemonID)
	if source.Mode != "native" || source.Enabled || source.NativeEnrollmentStat != "pending" || source.NativeEnrollmentID == "" {
		t.Fatalf("provision must create a pending disabled native source: mode=%q enabled=%t status=%q enrollment_present=%t",
			source.Mode, source.Enabled, source.NativeEnrollmentStat, source.NativeEnrollmentID != "")
	}
	if source.RuntimeID != runtimeID || source.WorkspaceID != testWorkspaceID || source.DaemonID != daemonID || source.SourceHandle == "" {
		t.Fatal("provision lost runtime/workspace/daemon identity or handle")
	}
	sourceID, enrollmentID := source.ID, source.NativeEnrollmentID

	// Server row agrees: pending, disabled, native, no hash yet. The status
	// is derived from native_enrolled_at, matching the shipped queries.
	var mode string
	var enabled bool
	var status string
	var hash *string
	fx.QueryRow(t, `SELECT mode, enabled,
		CASE WHEN native_enrolled_at IS NULL THEN 'pending' ELSE 'enrolled' END,
		native_manifest_hash FROM work_source WHERE id=$1 AND workspace_id=$2`,
		sourceID, testWorkspaceID).Scan(&mode, &enabled, &status, &hash)
	if mode != "native" || enabled || status != "pending" || hash != nil {
		t.Fatalf("server row after provision wrong: mode=%q enabled=%t status=%q hash_present=%t", mode, enabled, status, hash != nil)
	}

	// Machine-global marker exists, is private, and carries no secrets.
	markerPath := filepath.Join(home, ".multica", "native-sources", sourceID, ".native_owner")
	markerData, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("global marker missing: %v", err)
	}
	if strings.Contains(string(markerData), testToken) || strings.Contains(string(markerData), "mse_") {
		t.Fatal("global marker leaked credential material")
	}
	nseFileMode(t, markerPath, 0o600, "global marker")
	nseFileMode(t, filepath.Dir(markerPath), 0o700, "source domain directory")
	nseFileMode(t, filepath.Dir(filepath.Dir(markerPath)), 0o700, "native-sources root")

	// Per-profile index: private, exact identity.
	profileBase := filepath.Join(home, ".multica", "profiles", nseProfile)
	indexPath := filepath.Join(profileBase, "native-source-enrollments", sourceID+".json")
	nseFileMode(t, indexPath, 0o600, "profile index entry")
	nseFileMode(t, filepath.Dir(indexPath), 0o700, "profile index directory")
	identity := nseReadIndex(t, sourceID)
	if identity.BackendURL != testServer.URL || identity.WorkspaceID != testWorkspaceID ||
		identity.RuntimeID != runtimeID || identity.DaemonID != daemonID ||
		identity.SourceID != sourceID || identity.EnrollmentID != enrollmentID {
		t.Fatalf("index identity wrong: %+v", identity)
	}

	// --- Phase 2a: deterministic packet-privacy proof on a second source
	// BEFORE any daemon runs, so nothing can race the observation. S2 shares
	// the runtime/daemon identity (different request-id/name, so no 409) and
	// its approval is staged with no lock held; the packet is observed 0600
	// in the private inbox at rest. The later daemon adopts S2 too and either
	// finalizes or expires its packet through the production routes.
	source2 := nseProvision(t, ctx, cliPath, testWorkspaceID, runtimeID, uuid.NewString(), "native acceptance source 2", daemonID)
	if source2.ID == sourceID {
		t.Fatal("second provision must create a distinct source")
	}
	out2, errOut2, err := nseRunCLI(t, ctx, cliPath, nil,
		"--profile", nseProfile, "daemon", "source", "approve",
		"--workspace", testWorkspaceID, "--source", source2.ID, "--daemon-id", daemonID)
	nseAssertSanitized(t, out2, errOut2)
	if err != nil {
		t.Fatalf("approve CLI (pre-daemon) failed: %v stderr=%q", err, strings.TrimSpace(errOut2))
	}
	inboxDir := filepath.Join(profileBase, "native-source-enrollments", "inbox")
	nseFileMode(t, inboxDir, 0o700, "approval inbox directory")
	nseWaitFor(t, "staged approval packet", 10*time.Second, func() bool {
		entries, err := os.ReadDir(inboxDir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), source2.ID+".") || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("approval packet must be 0600, got %v", info.Mode().Perm())
			}
			return true
		}
		return false
	})

	// --- Phase 2b: exact request-id replay keeps S/E (idempotent retry).
	replay := nseProvision(t, ctx, cliPath, testWorkspaceID, runtimeID, requestID, "native acceptance source", daemonID)
	if replay.ID != sourceID || replay.NativeEnrollmentID != enrollmentID {
		t.Fatalf("exact replay must keep S/E: got S_ok=%t E_ok=%t", replay.ID == sourceID, replay.NativeEnrollmentID == enrollmentID)
	}

	// --- Phase 3: real daemon Run. HealthPort 0, no GC/auto-update/reload,
	// fast polls; the enrollment loop polls its own profile's index.
	cfg := daemon.Config{
		ServerBaseURL:       testServer.URL,
		Profile:             nseProfile,
		DaemonID:            daemonID,
		DeviceName:          "nse-acceptance-test",
		RuntimeName:         "NSE acceptance runtime",
		CLIVersion:          "9.9.9-test",
		Agents:              map[string]daemon.AgentEntry{"claude": {Path: fakeClaude, Command: fakeClaude}},
		WorkspacesRoot:      t.TempDir(),
		HealthPort:          0,
		MaxConcurrentTasks:  1,
		GCEnabled:           false,
		AutoUpdateEnabled:   false,
		AutoReloadEnabled:   false,
		PollInterval:        time.Second,
		WSClaimPollInterval: time.Minute,
		HeartbeatInterval:   time.Second,
		AgentTimeout:        30 * time.Second,
		AgentIdleWatchdog:   time.Minute,
		AgentToolWatchdog:   time.Minute,
	}
	runCtx, runCancel := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := daemon.New(cfg, logger)
	go func() { runDone <- d.Run(runCtx); close(runDone) }()
	t.Cleanup(func() {
		runCancel()
		select {
		case <-runDone:
		case <-time.After(30 * time.Second):
			t.Error("daemon did not stop during fixture cleanup")
		}
	})

	// The daemon must adopt the domain: the OS lock is held by the running
	// daemon, so an in-process open fails busy.
	nseWaitFor(t, "daemon to hold the source domain lock", 30*time.Second, func() bool {
		root, _ := nseOpenGlobalRoot(t)
		defer root.Close()
		domain, err := execenv.OpenNativeSource(root, identity)
		if err == nil {
			domain.Close()
			return false
		}
		return errors.Is(err, execenv.ErrNativeSourceBusy)
	})

	// --- Phase 4: approve through the real CLI while the lock is held. The
	// staged packet is ephemeral (short-lived mse_ credential in the private
	// inbox) and is never printed.
	out, errOut, err := nseRunCLI(t, ctx, cliPath, nil,
		"--profile", nseProfile, "daemon", "source", "approve",
		"--workspace", testWorkspaceID, "--source", sourceID, "--daemon-id", daemonID)
	nseAssertSanitized(t, out, errOut)
	if err != nil {
		t.Fatalf("approve CLI failed: %v stderr=%q", err, strings.TrimSpace(errOut))
	}
	if !strings.Contains(out, "Approval staged") || strings.Contains(out, "mse_") {
		t.Fatal("approve output must be the public staging notice only")
	}
	// The staged packet legitimately carries the short-lived credential by
	// design; it must simply live in the private ephemeral inbox and be
	// consumed. Credentials never appear in marker, index, or output.
	// (Packet-at-rest 0600 is proven deterministically in Phase 2a.)
	packetCount := func() int {
		entries, err := os.ReadDir(inboxDir)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".json") {
				n++
			}
		}
		return n
	}

	// --- Phase 5: the daemon finalizes the staged approval through the
	// production finalize route. Poll the real server row to enrolled.
	var dbHash string
	nseWaitFor(t, "daemon to finalize enrollment", 45*time.Second, func() bool {
		var s string
		var h *string
		fx.QueryRow(t, `SELECT CASE WHEN native_enrolled_at IS NULL THEN 'pending' ELSE 'enrolled' END,
		native_manifest_hash FROM work_source WHERE id=$1 AND workspace_id=$2`,
			sourceID, testWorkspaceID).Scan(&s, &h)
		if s == "enrolled" {
			if h == nil {
				t.Fatal("enrolled row must pin the manifest hash")
			}
			dbHash = *h
			return true
		}
		return false
	})
	if len(dbHash) != 64 {
		t.Fatalf("enrolled manifest hash must be sha256 hex, got %d bytes", len(dbHash))
	}
	// The inbox is fully drained after the acknowledged finalize.
	nseWaitFor(t, "approval inbox to drain", 15*time.Second, func() bool { return packetCount() == 0 })

	// --- Phase 6: status CLI observes only public fields.
	out, errOut, err = nseRunCLI(t, ctx, cliPath, nil,
		"--profile", nseProfile, "daemon", "source", "status",
		"--workspace", testWorkspaceID, "--source", sourceID, "--daemon-id", daemonID)
	nseAssertSanitized(t, out, errOut)
	if err != nil {
		t.Fatalf("status CLI failed: %v stderr=%q", err, strings.TrimSpace(errOut))
	}
	var observed nseCLISource
	if err := json.Unmarshal([]byte(out), &observed); err != nil {
		t.Fatalf("status CLI stdout is not source JSON: %v", err)
	}
	if observed.ID != sourceID || observed.NativeEnrollmentStat != "enrolled" || observed.NativeManifestHash != dbHash {
		t.Fatalf("status receipt wrong: id_ok=%t status=%q hash_ok=%t",
			observed.ID == sourceID, observed.NativeEnrollmentStat, observed.NativeManifestHash == dbHash)
	}

	// --- Phase 7: exact completed-Q replay while the daemon still holds the
	// lock: same S/E, still enrolled, no local churn.
	completed := nseProvision(t, ctx, cliPath, testWorkspaceID, runtimeID, requestID, "native acceptance source", daemonID)
	if completed.ID != sourceID || completed.NativeEnrollmentID != enrollmentID || completed.NativeEnrollmentStat != "enrolled" {
		t.Fatalf("completed replay must keep S/E enrolled: S_ok=%t E_ok=%t status=%q",
			completed.ID == sourceID, completed.NativeEnrollmentID == enrollmentID, completed.NativeEnrollmentStat)
	}

	// --- No side effects anywhere: enrollment never launches Beads, creates
	// issues, agents, or queued tasks.
	for table, query := range rowQueries {
		if n := fx.Count(t, query, testWorkspaceID); n != baseline[table] {
			t.Fatalf("enrollment changed %s rows: before=%d after=%d", table, baseline[table], n)
		}
	}

	// --- Phase 8: cancel Run with a bounded join, then prove the OS lock is
	// released while marker and index survive.
	runCancel()
	select {
	case <-runDone:
	case <-time.After(30 * time.Second):
		t.Fatal("daemon Run did not return after cancel")
	}
	root, _ := nseOpenGlobalRoot(t)
	domain, err := execenv.OpenNativeSource(root, identity)
	if err != nil {
		t.Fatalf("lock must be released after Run returns: %v", err)
	}
	local, err := domain.ManifestHash()
	domain.Close()
	root.Close()
	if err != nil {
		t.Fatalf("marker must survive shutdown: %v", err)
	}
	if local != dbHash {
		t.Fatal("local manifest hash must equal the enrolled server hash")
	}
	// The exported loader agrees, and the marker/index are still present.
	loaded, err := daemon.LoadNativeSourceEnrollmentLocal(nseProfile, testServer.URL, testWorkspaceID, sourceID)
	if err != nil {
		t.Fatalf("LoadNativeSourceEnrollmentLocal after shutdown: %v", err)
	}
	if loaded.ManifestHash != dbHash || loaded.Identity.EnrollmentID != enrollmentID {
		t.Fatal("loaded local enrollment disagrees with the enrolled server state")
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("marker preserved: %v", err)
	}
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatalf("index preserved: %v", err)
	}
}

// TestNativeSourceEnrollmentCLIRefusesManagedTask pins the CLI guard: with a
// daemon-managed task config root active, provision refuses before any HTTP
// request or filesystem access. The server URL points at a closed port, so
// any attempted HTTP would surface as a connection failure, not the refusal.
func TestNativeSourceEnrollmentCLIRefusesManagedTask(t *testing.T) {
	cliPath := nseRequireCLI(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	home := t.TempDir()
	taskRoot := t.TempDir()
	env := map[string]string{
		"HOME":                     home,
		"MULTICA_TASK_CONFIG_ROOT": taskRoot,
		"MULTICA_SERVER_URL":       "http://127.0.0.1:1", // closed port: no real server
		"MULTICA_TOKEN":            "task-context-token-must-not-matter",
		"MULTICA_DAEMON_ID":        "nse-refusal",
	}
	args := []string{"daemon", "source", "provision",
		"--workspace", uuid.NewString(), "--runtime", uuid.NewString(),
		"--name", "refusal", "--request-id", uuid.NewString(), "--daemon-id", "nse-refusal"}
	out, errOut, err := nseRunCLI(t, ctx, cliPath, env, args...)
	nseAssertSanitized(t, out, errOut)
	if err == nil {
		t.Fatal("provision must refuse inside a daemon-managed task")
	}
	msg := out + errOut
	if !strings.Contains(msg, "daemon-managed task") && !strings.Contains(msg, "MULTICA_TASK_CONFIG_ROOT") {
		t.Fatalf("refusal must name the managed-task guard, stderr=%q", strings.TrimSpace(errOut))
	}
	if strings.Contains(msg, "connection refused") || strings.Contains(msg, "dial tcp") {
		t.Fatal("refusal must happen before any HTTP attempt")
	}
	// The global CLI root must not be created by the managed-task refusal.
	if _, err := os.Stat(filepath.Join(home, ".multica")); !os.IsNotExist(err) {
		t.Fatalf("managed-task refusal touched the global CLI root: %v", err)
	}
	// No filesystem provisioning occurred under either root.
	for _, base := range []string{home, taskRoot} {
		if _, err := os.Stat(filepath.Join(base, "native-sources")); !os.IsNotExist(err) {
			if err != nil {
				t.Fatalf("stat %s: %v", base, err)
			}
			t.Fatalf("managed-task provision touched %s/native-sources", base)
		}
		if _, err := os.Stat(filepath.Join(base, ".multica", "profiles", "native-source-enrollments")); !os.IsNotExist(err) {
			if err != nil {
				t.Fatalf("stat profile dir under %s: %v", base, err)
			}
			t.Fatalf("managed-task provision wrote a profile index under %s", base)
		}
	}
}
