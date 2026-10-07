package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

// nseLocalEnv isolates HOME with no MULTICA_TASK_CONFIG_ROOT so ProfileDir("")
// resolves under a private tree.
func nseLocalEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(cli.TaskConfigRootEnv, "")
	local, err := nseHomeNativeSources()
	if err != nil {
		t.Fatalf("resolve native-sources root: %v", err)
	}
	return local
}

func nseHomeNativeSources() (string, error) {
	base, err := cli.ProfileDir("")
	if err != nil {
		return "", err
	}
	return filepath.Join(base, nativeSourcesDir), nil
}

// nseLocalSource builds a valid pending enrollment record.
func nseLocalSource() NativeSourceEnrollment {
	return NativeSourceEnrollment{
		ID: nseSourceID, WorkspaceID: nseWorkspace, RuntimeID: nseRuntimeID,
		DaemonID: nseDaemonID, Name: "docs", Mode: "native", Enabled: false,
		SourceHandle: "managed:" + nseSourceID, ConfigRevision: 3,
		NativeEnrollmentID: nseEnrollID, NativeEnrollmentStatus: "pending",
		NativeOwnerMemberID: nseMemberID, NativeRuntimeCreatedAt: "2026-10-07T10:00:00Z",
	}
}

// nseProvision provisions the canonical source and returns its real manifest
// hash plus the physical native-sources root.
func nseProvision(t *testing.T, profile string, backend string) (hash string, rootDir string) {
	t.Helper()
	if backend == "" {
		backend = "http://127.0.0.1:1"
	}
	rootDir = nseLocalEnv(t)
	if err := ProvisionNativeSourceLocal(context.Background(), profile, backend, nseLocalSource(), ""); err != nil {
		t.Fatalf("provision: %v", err)
	}
	local, err := LoadNativeSourceEnrollmentLocal(profile, backend, nseWorkspace, nseSourceID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return local.ManifestHash, rootDir
}

func TestNativeSourceEnrollmentLocalProvisionFreshCreate(t *testing.T) {
	hash, rootDir := nseProvision(t, "", "")
	// Exactly one leaf, named by SourceID, 0700, with a strict marker.
	leaves, err := os.ReadDir(rootDir)
	if err != nil || len(leaves) != 1 || leaves[0].Name() != nseSourceID {
		t.Fatalf("expected exactly one %s leaf: %v %v", nseSourceID, leaves, err)
	}
	leaf := filepath.Join(rootDir, nseSourceID)
	info, err := os.Stat(leaf)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("leaf perms: %v %v", info, err)
	}
	marker, err := os.ReadFile(filepath.Join(leaf, ".native_owner"))
	if err != nil {
		t.Fatalf("marker: %v", err)
	}
	if !strings.Contains(string(marker), nseEnrollID) {
		t.Fatalf("marker missing enrollment id: %s", marker)
	}
	// Index exists with 0600 and carries the same identity.
	home, _ := cli.ProfileDir("")
	idx := filepath.Join(home, nativeSourceIndexDir, nseSourceID+".json")
	idxInfo, err := os.Stat(idx)
	if err != nil || idxInfo.Mode().Perm() != 0o600 {
		t.Fatalf("index perms: %v %v", idxInfo, err)
	}
	var entry nativeSourceIndexEntry
	if err := json.Unmarshal([]byte(mustRead(t, idx)), &entry); err != nil {
		t.Fatalf("index decode: %v", err)
	}
	if entry.Identity.SourceID != nseSourceID || entry.Identity.EnrollmentID != nseEnrollID {
		t.Fatalf("index identity: %+v", entry)
	}
	if hash != nseManifest {
		// The hash is derived from the marker, independent of the fixture hash.
		if len(hash) != 64 {
			t.Fatalf("hash shape: %q", hash)
		}
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestNativeSourceEnrollmentLocalProvisionReplayWhileHeldIsNoop(t *testing.T) {
	hash, rootDir := nseProvision(t, "", "")
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	id := execenv.NativeSourceIdentity{
		BackendURL: "http://127.0.0.1:1", WorkspaceID: nseWorkspace, SourceID: nseSourceID,
		RuntimeID: nseRuntimeID, DaemonID: nseDaemonID, EnrollmentID: nseEnrollID,
	}
	held, err := execenv.OpenNativeSource(root, id)
	if err != nil {
		t.Fatalf("open held: %v", err)
	}
	defer held.Close()
	// Replay with the exact same identity is a no-op while the lock is held.
	if err := ProvisionNativeSourceLocal(context.Background(), "", "http://127.0.0.1:1", nseLocalSource(), ""); err != nil {
		t.Fatalf("replay while held must be a no-op: %v", err)
	}
	got, err := held.ManifestHash()
	if err != nil || got != hash {
		t.Fatalf("domain changed during replay: %v", err)
	}
}

func TestNativeSourceEnrollmentLocalProvisionMismatchFails(t *testing.T) {
	_, _ = nseProvision(t, "", "")
	conflicting := nseLocalSource()
	conflicting.DaemonID = "other-daemon"
	conflicting.SourceHandle = "managed:" + nseSourceID
	if err := ProvisionNativeSourceLocal(context.Background(), "", "http://127.0.0.1:1", conflicting, ""); err == nil {
		t.Fatal("mismatched identity accepted on replay")
	}
}

func TestNativeSourceEnrollmentLocalUnmarkedDomainFailsClosed(t *testing.T) {
	rootDir := nseLocalEnv(t)
	// A pre-existing empty, unmarked leaf must never be adopted or reset.
	if err := os.MkdirAll(filepath.Join(rootDir, nseSourceID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ProvisionNativeSourceLocal(context.Background(), "", "http://127.0.0.1:1", nseLocalSource(), ""); err == nil {
		t.Fatal("empty unmarked domain adopted")
	}
	if _, err := os.Stat(filepath.Join(rootDir, nseSourceID, ".native_owner")); !os.IsNotExist(err) {
		t.Fatal("marker was written into an existing unmarked domain")
	}
}

func TestNativeSourceEnrollmentLocalMissingLockFailsClosed(t *testing.T) {
	_, rootDir := nseProvision(t, "", "")
	if err := os.Remove(filepath.Join(rootDir, nseSourceID, ".native_lock")); err != nil {
		t.Fatal(err)
	}
	// Direct domain access (the daemon loop's path) refuses a missing lock.
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	id := execenv.NativeSourceIdentity{
		BackendURL: "http://127.0.0.1:1", WorkspaceID: nseWorkspace, SourceID: nseSourceID,
		RuntimeID: nseRuntimeID, DaemonID: nseDaemonID, EnrollmentID: nseEnrollID,
	}
	if _, err := execenv.OpenNativeSource(root, id); err == nil {
		t.Fatal("missing lock accepted on domain open")
	}
	if err := ProvisionNativeSourceLocal(context.Background(), "", "http://127.0.0.1:1", nseLocalSource(), ""); err == nil {
		t.Fatal("missing lock recreated by provision")
	}
}

func TestNativeSourceEnrollmentLocalTaskLocalReject(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(cli.TaskConfigRootEnv, t.TempDir())
	if err := ProvisionNativeSourceLocal(context.Background(), "", "http://127.0.0.1:1", nseLocalSource(), ""); err == nil {
		t.Fatal("task-local provision accepted")
	}
	if _, err := LoadNativeSourceEnrollmentLocal("", "http://127.0.0.1:1", nseWorkspace, nseSourceID); err == nil {
		t.Fatal("task-local load accepted")
	}
	cred := nseLocalCredential(t, strings.Repeat("a", 64))
	if err := StageNativeSourceEnrollmentApproval("", "http://127.0.0.1:1", nseLocalSource(), cred); err == nil {
		t.Fatal("task-local stage accepted")
	}
}

func nseLocalCredential(t *testing.T, manifestHash string) SourceEnrollmentCredential {
	t.Helper()
	return SourceEnrollmentCredential{
		Token: nseMSEToken, ExpiresAt: time.Now().Add(90 * time.Second), ExpiresIn: 90,
		RuntimeID: nseRuntimeID, WorkspaceID: nseWorkspace, DaemonID: nseDaemonID,
		SourceID: nseSourceID, EnrollmentID: nseEnrollID,
		ConfigRevision: 3, ManifestHash: manifestHash,
	}
}

func TestNativeSourceEnrollmentLocalGlobalRootAcrossProfiles(t *testing.T) {
	_, rootDir := nseProvision(t, "alpha", "")
	// Profile beta has no index: load fails even though the domain exists.
	if _, err := LoadNativeSourceEnrollmentLocal("beta", "http://127.0.0.1:1", nseWorkspace, nseSourceID); err == nil {
		t.Fatal("load without index accepted")
	}
	// Provisioning through beta reuses the same physical domain: still one leaf.
	if err := ProvisionNativeSourceLocal(context.Background(), "beta", "http://127.0.0.1:1", nseLocalSource(), ""); err != nil {
		t.Fatalf("provision via beta: %v", err)
	}
	leaves, err := os.ReadDir(rootDir)
	if err != nil || len(leaves) != 1 {
		t.Fatalf("second physical domain created: %v", leaves)
	}
	if _, err := LoadNativeSourceEnrollmentLocal("beta", "http://127.0.0.1:1", nseWorkspace, nseSourceID); err != nil {
		t.Fatalf("load via beta: %v", err)
	}
}

func TestNativeSourceEnrollmentLocalStageInbox(t *testing.T) {
	hash, _ := nseProvision(t, "", "")
	cred := nseLocalCredential(t, hash)
	if err := StageNativeSourceEnrollmentApproval("", "http://127.0.0.1:1", nseLocalSource(), cred); err != nil {
		t.Fatalf("stage: %v", err)
	}
	home, _ := cli.ProfileDir("")
	inbox := filepath.Join(home, nativeSourceIndexDir, nativeSourceInboxDir)
	names, err := os.ReadDir(inbox)
	if err != nil || len(names) != 1 {
		t.Fatalf("inbox: %v %v", names, err)
	}
	name := names[0].Name()
	if !strings.HasPrefix(name, nseSourceID+".") || !strings.HasSuffix(name, ".json") {
		t.Fatalf("inbox filename: %s", name)
	}
	info, err := os.Stat(filepath.Join(inbox, name))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("inbox perms: %v %v", info, err)
	}
	// The token is staged in the inbox only, never in the immutable marker.
	if strings.Contains(mustRead(t, filepath.Join(home, nativeSourcesDir, nseSourceID, ".native_owner")), nseMSEToken) {
		t.Fatal("token leaked into the domain marker")
	}
	// Expired credential is rejected outright.
	expired := nseLocalCredential(t, hash)
	expired.ExpiresAt = time.Now().Add(-time.Second)
	if err := StageNativeSourceEnrollmentApproval("", "http://127.0.0.1:1", nseLocalSource(), expired); err == nil {
		t.Fatal("expired credential staged")
	}
}

func TestNativeSourceEnrollmentLocalProvisionIdleIdenticalReplayNoop(t *testing.T) {
	_, rootDir := nseProvision(t, "", "")
	before, err := os.ReadDir(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := cli.ProfileDir("")
	idxPath := filepath.Join(home, nativeSourceIndexDir, nseSourceID+".json")
	idxBefore := mustRead(t, idxPath)
	// Second identical provision with no daemon holding the lock: no-op, no
	// "already exists" error, no index overwrite.
	if err := ProvisionNativeSourceLocal(context.Background(), "", "http://127.0.0.1:1", nseLocalSource(), ""); err != nil {
		t.Fatalf("idle identical replay must be a no-op: %v", err)
	}
	after, err := os.ReadDir(rootDir)
	if err != nil || len(after) != len(before) {
		t.Fatalf("domain tree changed on replay: %v %v", before, after)
	}
	if got := mustRead(t, idxPath); got != idxBefore {
		t.Fatal("index rewritten on identical replay")
	}
}

func TestNativeSourceEnrollmentLocalProvisionEnabledEnrolledExistingReplay(t *testing.T) {
	hash, _ := nseProvision(t, "", "")
	replay := nseLocalSource()
	replay.NativeEnrollmentStatus = "enrolled"
	replay.NativeManifestHash = hash
	replay.NativeEnrolledAt = "2026-10-07T11:00:00Z"
	replay.Enabled = true // terminal HTTP 200 replay may already be enabled
	if err := ProvisionNativeSourceLocal(context.Background(), "", "http://127.0.0.1:1", replay, ""); err != nil {
		t.Fatalf("enabled enrolled replay: %v", err)
	}
}

func TestNativeSourceEnrollmentLocalProvisionApprovedMissingDomainRejectsNoCreation(t *testing.T) {
	nseLocalEnv(t) // clean global root, no domain, no index
	approved := nseLocalSource()
	approved.NativeEnrollmentStatus = "enrolled"
	approved.NativeManifestHash = strings.Repeat("a", 64)
	approved.NativeEnrolledAt = "2026-10-07T11:00:00Z"
	if err := ProvisionNativeSourceLocal(context.Background(), "", "http://127.0.0.1:1", approved, ""); err == nil {
		t.Fatal("approved source with missing domain provisioned")
	}
	rootDir, _ := nseHomeNativeSources()
	leaves, err := os.ReadDir(rootDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(leaves) != 0 {
		t.Fatalf("domain created for approved missing source: %v", leaves)
	}
}

func TestNativeSourceEnrollmentLocalRootSymlinkAndPublicPermsRejected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(cli.TaskConfigRootEnv, "")
	base := filepath.Join(home, ".multica")
	// Global native-sources root as a symlink: rejected, never followed.
	if err := os.MkdirAll(filepath.Join(home, "elsewhere"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "elsewhere"), filepath.Join(base, nativeSourcesDir)); err != nil {
		t.Fatal(err)
	}
	if err := ProvisionNativeSourceLocal(context.Background(), "", "http://127.0.0.1:1", nseLocalSource(), ""); err == nil {
		t.Fatal("symlinked global root accepted")
	}
	// World-readable pre-existing inbox: rejected without chmod adoption and
	// no HTTP is made (stage fails before any transport).
	inbox := filepath.Join(base, nativeSourceIndexDir, nativeSourceInboxDir)
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	cred := nseLocalCredential(t, strings.Repeat("a", 64))
	err := StageNativeSourceEnrollmentApproval("", "http://127.0.0.1:1", nseLocalSource(), cred)
	if err == nil {
		t.Fatal("public inbox permissions accepted")
	}
	info, statErr := os.Stat(inbox)
	if statErr != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("public inbox was chmod'd/adopted: %v %v", info, statErr)
	}
	entries, _ := os.ReadDir(inbox)
	if len(entries) != 0 {
		t.Fatalf("packet staged into public inbox: %v", entries)
	}
}

func TestNativeSourceEnrollmentLocalLoadAndStageWhileOwnerHeld(t *testing.T) {
	hash, rootDir := nseProvision(t, "", "")
	// Hold the domain exactly as the daemon loop does.
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	id := execenv.NativeSourceIdentity{
		BackendURL: "http://127.0.0.1:1", WorkspaceID: nseWorkspace, SourceID: nseSourceID,
		RuntimeID: nseRuntimeID, DaemonID: nseDaemonID, EnrollmentID: nseEnrollID,
	}
	held, err := execenv.OpenNativeSource(root, id)
	if err != nil {
		t.Fatalf("hold domain: %v", err)
	}
	defer held.Close()
	// Load must NOT take the source owner lock: the CLI approve path calls it
	// while the daemon holds the domain.
	local, err := LoadNativeSourceEnrollmentLocal("", "http://127.0.0.1:1", nseWorkspace, nseSourceID)
	if err != nil {
		t.Fatalf("load while owner held: %v", err)
	}
	if local.ManifestHash != hash {
		t.Fatalf("recorded hash %q != provisioned hash %q", local.ManifestHash, hash)
	}
	// Stage likewise succeeds without the owner lock, and the recorded hash
	// flows into the packet the daemon loop will validate against its own held
	// domain.ManifestHash.
	if err := StageNativeSourceEnrollmentApproval("", "http://127.0.0.1:1", nseLocalSource(), nseLocalCredential(t, local.ManifestHash)); err != nil {
		t.Fatalf("stage while owner held: %v", err)
	}
}

func TestNativeSourceEnrollmentLocalReadRejectsPublicFilePerms(t *testing.T) {
	_, _ = nseProvision(t, "", "")
	home, _ := cli.ProfileDir("")
	idx := filepath.Join(home, nativeSourceIndexDir, nseSourceID+".json")
	if err := os.Chmod(idx, 0o644); err != nil {
		t.Fatal(err)
	}
	// Group/world-readable index file is rejected at the strict read boundary;
	// nothing is chmod'd back or repaired.
	if _, err := LoadNativeSourceEnrollmentLocal("", "http://127.0.0.1:1", nseWorkspace, nseSourceID); err == nil {
		t.Fatal("world-readable index accepted")
	}
	if info, err := os.Stat(idx); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("index perms repaired: %v %v", info, err)
	}
}

func TestNativeSourceEnrollmentLocalApprovedPendingHashMustAgree(t *testing.T) {
	hash, _ := nseProvision(t, "", "")
	// Approved-but-still-pending replay carrying a DIFFERENT pinned server
	// hash: rejected against both index and held domain.
	approvedPending := nseLocalSource()
	approvedPending.NativeManifestHash = strings.Repeat("b", 64) // != local hash
	if err := ProvisionNativeSourceLocal(context.Background(), "", "http://127.0.0.1:1", approvedPending, ""); err == nil {
		t.Fatal("approved pending with mismatched server hash accepted")
	}
	// Same replay with the agreeing hash is a no-op.
	agreeing := nseLocalSource()
	agreeing.NativeManifestHash = hash
	if err := ProvisionNativeSourceLocal(context.Background(), "", "http://127.0.0.1:1", agreeing, ""); err != nil {
		t.Fatalf("approved pending with agreeing hash: %v", err)
	}
}

func TestNativeSourceEnrollmentLocalAliasedIndexParentRejected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(cli.TaskConfigRootEnv, "")
	base := filepath.Join(home, ".multica")
	if err := os.MkdirAll(filepath.Join(home, "aliased"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	// native-source-enrollments as a symlink: the inbox must not be created
	// through it, and staging must fail before any HTTP.
	if err := os.Symlink(filepath.Join(home, "aliased"), filepath.Join(base, nativeSourceIndexDir)); err != nil {
		t.Fatal(err)
	}
	cred := nseLocalCredential(t, strings.Repeat("a", 64))
	if err := StageNativeSourceEnrollmentApproval("", "http://127.0.0.1:1", nseLocalSource(), cred); err == nil {
		t.Fatal("inbox staged through symlinked index parent")
	}
	if entries, _ := os.ReadDir(filepath.Join(home, "aliased")); len(entries) != 0 {
		t.Fatalf("inbox created behind alias: %v", entries)
	}
}

func TestNativeSourceEnrollmentLoopUnpairedRuntimeNoAcquire(t *testing.T) {
	client, mux := newNSETestClient(t)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})
	hash, _ := nseProvision(t, "", client.baseURL)
	// Runtime present in the global index but NOT in the workspace pairing.
	d := nseTestDaemon(t, client.baseURL)
	d.mu.Lock()
	d.workspaces[nseWorkspace] = newWorkspaceState(nseWorkspace, nil, "", nil, nil)
	d.runtimeIndex[nseRuntimeID] = Runtime{ID: nseRuntimeID}
	d.mu.Unlock()
	if err := StageNativeSourceEnrollmentApproval("", client.baseURL, nseLocalSource(), nseLocalCredential(t, hash)); err != nil {
		t.Fatal(err)
	}
	held := map[string]*heldNativeSource{}
	d.nativeSourceEnrollmentReconcile(context.Background(), held)
	if len(held) != 0 {
		t.Fatalf("unpaired runtime adopted: %+v", held)
	}
	// The daemon never acquired the lock: an external open must succeed.
	rootDir, _ := nseHomeNativeSources()
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	id := execenv.NativeSourceIdentity{
		BackendURL: client.baseURL, WorkspaceID: nseWorkspace, SourceID: nseSourceID,
		RuntimeID: nseRuntimeID, DaemonID: nseDaemonID, EnrollmentID: nseEnrollID,
	}
	ext, err := execenv.OpenNativeSource(root, id)
	if err != nil {
		t.Fatalf("daemon acquired the domain lock without pairing: %v", err)
	}
	ext.Close()
	// Inbox packet untouched: no finalize attempt was made.
	if entries, _ := os.ReadDir(nseInboxDir(t)); len(entries) != 1 {
		t.Fatalf("inbox packet consumed without pairing: %v", entries)
	}
}

func TestNativeSourceEnrollmentLoopMarkerMismatchRetainsLock(t *testing.T) {
	client, mux := newNSETestClient(t)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})
	_, rootDir := nseProvision(t, "", client.baseURL)
	d := nseTestDaemon(t, client.baseURL)
	d.nseTrackRuntime()
	held := map[string]*heldNativeSource{}
	defer func() {
		for _, h := range held {
			h.domain.Close()
		}
	}()
	d.nativeSourceEnrollmentReconcile(context.Background(), held)
	if len(held) != 1 {
		t.Fatal("reconcile did not acquire the domain")
	}
	id := execenv.NativeSourceIdentity{
		BackendURL: client.baseURL, WorkspaceID: nseWorkspace, SourceID: nseSourceID,
		RuntimeID: nseRuntimeID, DaemonID: nseDaemonID, EnrollmentID: nseEnrollID,
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.WriteFile(filepath.Join(rootDir, nseSourceID, ".native_owner"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Explicit passes reach the marker-failure branch. A short sleep before
	// the next polling tick would not prove that branch retains the lock.
	for i := 0; i < 3; i++ {
		d.nativeSourceEnrollmentReconcile(context.Background(), held)
		if _, err := execenv.OpenNativeSource(root, id); !errors.Is(err, execenv.ErrNativeSourceBusy) {
			t.Fatalf("marker failure released the owner lock: %v", err)
		}
	}
	// The loop's cancellation defer uses this same Close operation; actual
	// cancellation is covered by HoldsLockAndReleasesOnCancel.
	held[nseSourceID].domain.Close()
	if _, err := execenv.OpenNativeSource(root, id); err == nil || errors.Is(err, execenv.ErrNativeSourceBusy) {
		t.Fatalf("after release, corrupt marker must fail closed without a busy lock: %v", err)
	}
}

func nseTestDaemon(t *testing.T, backend string) *Daemon {
	t.Helper()
	d := &Daemon{
		cfg:          Config{ServerBaseURL: backend, DaemonID: nseDaemonID},
		client:       NewClient(backend),
		logger:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
		workspaces:   map[string]*workspaceState{},
		runtimeIndex: map[string]Runtime{},
	}
	return d
}

func (d *Daemon) nseTrackRuntime() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.workspaces[nseWorkspace] = newWorkspaceState(nseWorkspace, []string{nseRuntimeID}, "", nil, nil)
	d.runtimeIndex[nseRuntimeID] = Runtime{ID: nseRuntimeID}
}

// nseWaitFor polls cond until true or the deadline passes.
func nseWaitFor(t *testing.T, deadline time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not reached in time")
}

func nseInboxDir(t *testing.T) string {
	t.Helper()
	home, _ := cli.ProfileDir("")
	return filepath.Join(home, nativeSourceIndexDir, nativeSourceInboxDir)
}

func TestNativeSourceEnrollmentLoopFinalizesWithActualClient(t *testing.T) {
	client, mux := newNSETestClient(t)
	hash, _ := nseProvision(t, "", client.baseURL)
	d := nseTestDaemon(t, client.baseURL)
	d.nseTrackRuntime()

	var mu sync.Mutex
	calls := 0
	mux.HandleFunc("/api/daemon/runtimes/"+nseRuntimeID+"/source-enrollments/"+nseSourceID+"/finalize",
		func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != "Bearer "+nseMSEToken {
				t.Errorf("finalize auth: %q", got)
			}
			if got := r.Header.Get("X-Workspace-ID"); got != nseWorkspace {
				t.Errorf("workspace header: %q", got)
			}
			body := decodeRequest(r)
			if body["enrollment_id"] != nseEnrollID || body["manifest_hash"] != hash {
				t.Errorf("proof body: %v", body)
			}
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n == 1 {
				// Response loss: a transient 500 must be retried with the same
				// payload while the credential is unexpired.
				writeJSONBody(t, w, http.StatusInternalServerError, map[string]string{})
				return
			}
			writeJSONBody(t, w, http.StatusOK, nseEnrolledBodyWithHash(hash))
		})

	if err := StageNativeSourceEnrollmentApproval("", client.baseURL, nseLocalSource(), nseLocalCredential(t, hash)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.nativeSourceEnrollmentLoop(ctx); close(done) }()
	nseWaitFor(t, 6*time.Second, func() bool {
		entries, _ := os.ReadDir(nseInboxDir(t))
		entries2, _ := os.ReadDir(nseInboxDir(t))
		return len(entries) == 0 && len(entries2) == 0
	})
	mu.Lock()
	if calls < 2 {
		t.Fatalf("expected retry after response loss, calls=%d", calls)
	}
	mu.Unlock()
	cancel()
	<-done
	// Marker and index survive the ack; only the ephemeral packet was removed.
	home, _ := cli.ProfileDir("")
	if _, err := os.Stat(filepath.Join(home, nativeSourcesDir, nseSourceID, ".native_owner")); err != nil {
		t.Fatalf("marker removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, nativeSourceIndexDir, nseSourceID+".json")); err != nil {
		t.Fatalf("index removed: %v", err)
	}
}

func nseEnrolledBodyWithHash(hash string) map[string]any {
	s := nseEnrolledSource(false)
	s["native_manifest_hash"] = hash
	return s
}

func TestNativeSourceEnrollmentLoopUntrackedRuntimeMakesNoRequests(t *testing.T) {
	client, mux := newNSETestClient(t)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})
	// No tracked workspace/runtime pairing.
	hash, _ := nseProvision(t, "", client.baseURL)
	d := nseTestDaemon(t, client.baseURL)
	if err := StageNativeSourceEnrollmentApproval("", client.baseURL, nseLocalSource(), nseLocalCredential(t, hash)); err != nil {
		t.Fatal(err)
	}
	held := map[string]*heldNativeSource{}
	d.nativeSourceEnrollmentReconcile(context.Background(), held)
	if len(held) != 0 {
		t.Fatalf("untracked runtime adopted: %+v", held)
	}
	// Foreign daemon id is likewise ignored.
	d2 := nseTestDaemon(t, client.baseURL)
	d2.cfg.DaemonID = "another-daemon"
	d2.nseTrackRuntime()
	held2 := map[string]*heldNativeSource{}
	d2.nativeSourceEnrollmentReconcile(context.Background(), held2)
	if len(held2) != 0 {
		t.Fatalf("foreign daemon adopted: %+v", held2)
	}
}

func TestNativeSourceEnrollmentLoopHoldsLockAndReleasesOnCancel(t *testing.T) {
	client, _ := newNSETestClient(t)
	_, rootDir := nseProvision(t, "", client.baseURL)
	d := nseTestDaemon(t, client.baseURL)
	d.nseTrackRuntime()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.nativeSourceEnrollmentLoop(ctx); close(done) }()

	id := execenv.NativeSourceIdentity{
		BackendURL: client.baseURL, WorkspaceID: nseWorkspace, SourceID: nseSourceID,
		RuntimeID: nseRuntimeID, DaemonID: nseDaemonID, EnrollmentID: nseEnrollID,
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	// While the loop holds the domain, a second open reports busy - exactly
	// ErrNativeSourceBusy, not any other error.
	busy := false
	nseWaitFor(t, 6*time.Second, func() bool {
		held, err := execenv.OpenNativeSource(root, id)
		if err == nil {
			held.Close()
			return false
		}
		if !errors.Is(err, execenv.ErrNativeSourceBusy) {
			t.Fatalf("unexpected open error while held: %v", err)
		}
		busy = true
		return true
	})
	if !busy {
		t.Fatal("second holder acquired the same domain lock")
	}
	cancel()
	<-done
	// Cancellation releases the lock.
	nseWaitFor(t, 2*time.Second, func() bool {
		held, err := execenv.OpenNativeSource(root, id)
		if err == nil {
			held.Close()
			return true
		}
		return false
	})
}

func TestNativeSourceEnrollmentLoopDefiniteRejectionRemovesOnlyPacket(t *testing.T) {
	client, mux := newNSETestClient(t)
	hash, _ := nseProvision(t, "", client.baseURL)
	mux.HandleFunc("/api/daemon/runtimes/"+nseRuntimeID+"/source-enrollments/"+nseSourceID+"/finalize",
		func(w http.ResponseWriter, r *http.Request) {
			writeJSONBody(t, w, http.StatusForbidden, map[string]string{})
		})
	d := nseTestDaemon(t, client.baseURL)
	d.nseTrackRuntime()
	if err := StageNativeSourceEnrollmentApproval("", client.baseURL, nseLocalSource(), nseLocalCredential(t, hash)); err != nil {
		t.Fatal(err)
	}
	held := map[string]*heldNativeSource{}
	// Two passes: adopt, then drain.
	d.nativeSourceEnrollmentReconcile(context.Background(), held)
	d.nativeSourceEnrollmentReconcile(context.Background(), held)
	nseWaitFor(t, 2*time.Second, func() bool {
		entries, _ := os.ReadDir(nseInboxDir(t))
		return len(entries) == 0
	})
	home, _ := cli.ProfileDir("")
	if _, err := os.Stat(filepath.Join(home, nativeSourcesDir, nseSourceID, ".native_owner")); err != nil {
		t.Fatalf("marker deleted on rejection: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, nativeSourceIndexDir, nseSourceID+".json")); err != nil {
		t.Fatalf("index deleted on rejection: %v", err)
	}
	for _, h := range held {
		h.domain.Close()
	}
}
