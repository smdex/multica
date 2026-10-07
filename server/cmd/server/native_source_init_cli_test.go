package main

// ACTUAL-CLI acceptance for `multica daemon source provision
// --beads-executable <ABS>` against the production router + PostgreSQL. The
// only executed Beads is an explicit TEST-CREATED fake; no installed bd.
//
// Covered: success (argv, allowlist env, receipt 0600, .beads, index
// published only after init, pending disabled row, no graph rows, marker
// bytes unchanged across init), exact-Q replay without relaunch, crash
// leaves partial .beads with no receipt/index and same-Q retry refuses the
// non-fresh domain, busy lock (flag errors, default stays idempotent), and
// authority denials before any initializer launch or filesystem work.
// CLI-unit relative/managed-task refusals live in cmd/multica.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func nsiRequireInitCLI(t *testing.T) string {
	t.Helper()
	cliPath := nseRequireCLI(t)
	if runtime.GOOS != "linux" {
		t.Skip("native source initialization is Linux-only")
	}
	return cliPath
}

// nsiWriteFakeBD writes a TEST-CREATED fake Beads executable. POSIX shell
// only; mkdir/ls/cat are baked in as absolute test-resolved utilities
// because PATH is not inherited by the pinned environment. After resolving
// them it pins PATH to dir, once per test. The script records argv, CWD,
// HOME/BEADS_DIR/PATH, the four metrics opts, the marker bytes BEFORE init
// runs, and whether the profile index already exists; crash=true exits 1
// after creating .beads. It never prints secrets: it only appends to its
// own absolute log.
func nsiWriteFakeBD(t *testing.T, dir, name, indexDir string, crash bool) (string, string) {
	t.Helper()
	absUtil := func(u string) string {
		p, err := exec.LookPath(u)
		if err != nil {
			t.Fatal(err)
		}
		if !filepath.IsAbs(p) {
			t.Fatalf("resolved %s must be absolute: %q", u, p)
		}
		return p
	}
	mkdir, ls, cat := absUtil("mkdir"), absUtil("ls"), absUtil("cat")
	t.Setenv("PATH", dir)
	path := filepath.Join(dir, name)
	log := path + ".log"
	lines := []string{
		"#!/bin/sh",
		"# TEST-CREATED fake Beads: never an installed bd",
		`LOG="` + log + `"`,
		`MKDIR="` + mkdir + `"`,
		`LS="` + ls + `"`,
		`CAT="` + cat + `"`,
		`printf 'ARGV:%s\n' "$*" >> "$LOG"`,
		`printf 'CWD:%s\n' "$(pwd)" >> "$LOG"`,
		`printf 'HOME:%s\n' "$HOME" >> "$LOG"`,
		`printf 'BEADS_DIR:%s\n' "${BEADS_DIR:-}" >> "$LOG"`,
		`printf 'PATH:%s\n' "$PATH" >> "$LOG"`,
		`printf 'METRICS:%s %s %s %s\n' "$BD_DISABLE_METRICS" "$BD_DISABLE_EVENT_FLUSH" "$DO_NOT_TRACK" "$DOLT_DISABLE_EVENT_FLUSH" >> "$LOG"`,
		`if [ "$1" = "--version" ]; then printf 'bd version 1.3.1 (dev)\n'; exit 0; fi`,
		`printf 'MARKER:%s\n' "$("$CAT" /proc/self/fd/3/.native_owner)" >> "$LOG"`,
		`if [ -d "` + indexDir + `" ] && "$LS" "` + indexDir + `"/*.json >/dev/null 2>&1; then`,
		`  printf 'IDX_PRESENT\n' >> "$LOG"`,
		`else`,
		`  printf 'IDX_ABSENT\n' >> "$LOG"`,
		`fi`,
		`"$MKDIR" -p "$BEADS_DIR" && : > "$BEADS_DIR/db"`,
	}
	if crash {
		lines = append(lines, `exit 1`)
	}
	lines = append(lines, `exit 0`)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, log
}

func nsiLog(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// nsiLaunches counts launches by kind: version preflight and init.
func nsiLaunches(t *testing.T, log string) (versions, inits int) {
	t.Helper()
	for _, l := range nsiLog(t, log) {
		switch {
		case l == "ARGV:--version":
			versions++
		case strings.HasPrefix(l, "ARGV:--sandbox init"):
			inits++
		}
	}
	return versions, inits
}

func nsiProvision(t *testing.T, ctx context.Context, cliPath, workspaceID, runtimeID, requestID, name, daemonID, executable string) (nseCLISource, string, error) {
	t.Helper()
	args := []string{"--profile", nseProfile, "daemon", "source", "provision",
		"--workspace", workspaceID, "--runtime", runtimeID,
		"--name", name, "--request-id", requestID, "--daemon-id", daemonID}
	if executable != "" {
		args = append(args, "--beads-executable", executable)
	}
	out, errOut, err := nseRunCLI(t, ctx, cliPath, nil, args...)
	nseAssertSanitized(t, out, errOut)
	var source nseCLISource
	if jsonErr := json.Unmarshal([]byte(out), &source); jsonErr != nil && err == nil {
		t.Fatalf("provision stdout is not source JSON: %v", jsonErr)
	}
	return source, strings.TrimSpace(errOut), err
}

// nsiHome pins the fresh HOME/XDG/SHELL environment and CLI credentials.
// PATH is pinned later by nsiWriteFakeBD, after absolute utility resolution.
func nsiHome(t *testing.T) (home, indexDir string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("SHELL", "")
	t.Setenv("MULTICA_TASK_CONFIG_ROOT", "")
	t.Setenv("MULTICA_SERVER_URL", testServer.URL)
	t.Setenv("MULTICA_TOKEN", testToken)
	indexDir = filepath.Join(home, ".multica", "profiles", nseProfile, "native-source-enrollments")
	return home, indexDir
}

// nsiCleanupSource registers the fixture cleanup for a service-created row.
func nsiCleanupSource(t *testing.T, fx *testutil.Fixture, sourceID string) {
	t.Helper()
	fx.Cleanup(t, `DELETE FROM work_source WHERE id=$1`, sourceID)
}

// nsiSourceIDByRequest resolves the actual work_source id for a stable
// request UUID (the CLI prints no source JSON on failure).
func nsiSourceIDByRequest(t *testing.T, fx *testutil.Fixture, requestID string) string {
	t.Helper()
	var sourceID string
	fx.QueryRow(t, `SELECT id FROM work_source WHERE workspace_id=$1 AND native_request_id=$2`,
		testWorkspaceID, requestID).Scan(&sourceID)
	if sourceID == "" {
		t.Fatal("no work_source row for the request UUID")
	}
	return sourceID
}

func TestNativeSourceInitCLIAcceptance(t *testing.T) {
	cliPath := nsiRequireInitCLI(t)
	if testPool == nil {
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

	home, indexDir := nsiHome(t)
	// Hostile inherited variables the allowlist must scrub.
	t.Setenv("BEADS_DOLT_SERVER_URL", "hostile-dolt-server")
	t.Setenv("BEADS_SANDBOX_HOOK", "/hostile/hook")
	t.Setenv("GIT_CONFIG", "/hostile/gitconfig")
	t.Setenv("BEADS_AGENT_CONFIG", "/hostile/agent")

	exe, log := nsiWriteFakeBD(t, t.TempDir(), "bd", indexDir, false)

	daemonID := "nsi-" + uuid.NewString()
	runtimeID := nseDaemonRuntime(t, fx, testUserID, daemonID, "claude")
	requestID := uuid.NewString()

	source, errOut, err := nsiProvision(t, ctx, cliPath, testWorkspaceID, runtimeID, requestID, "native init acceptance", daemonID, exe)
	if err != nil {
		t.Fatalf("provision+init CLI failed: %v stderr=%q", err, errOut)
	}
	if source.Mode != "native" || source.Enabled || source.NativeEnrollmentStat != "pending" || source.NativeEnrollmentID == "" {
		t.Fatalf("initialized source must stay pending disabled native: %+v", source)
	}
	sourceID := source.ID
	nsiCleanupSource(t, fx, sourceID)

	versions, inits := nsiLaunches(t, log)
	if versions != 1 || inits != 1 {
		t.Fatalf("first init must launch version+init exactly once: versions=%d inits=%d", versions, inits)
	}

	// The fake observed the profile index absent while init ran.
	sawAbsent := false
	for _, l := range nsiLog(t, log) {
		if l == "IDX_ABSENT" {
			sawAbsent = true
		}
		if l == "IDX_PRESENT" {
			t.Fatal("profile index must not exist while the initializer runs")
		}
	}
	if !sawAbsent {
		t.Fatal("fake did not record the index-absent observation")
	}

	// Phase-aware allowlist assertions. The version preflight legitimately
	// uses HOME=/proc/self/fd/3 (the held root itself); the init phase uses
	// the operation-owned private home and BEADS_DIR. Each phase's exact
	// environment is asserted, not merely checked for the absence of wrong
	// values.
	type phaseEnv struct {
		home, beadsDir, path, metrics string
	}
	phases := map[string]*phaseEnv{"--version": {}, "--sandbox": {}}
	cur := ""
	markerBeforeInit := ""
	for _, l := range nsiLog(t, log) {
		switch {
		case l == "ARGV:--version":
			cur = "--version"
		case strings.HasPrefix(l, "ARGV:--sandbox"):
			cur = "--sandbox"
		case strings.HasPrefix(l, "HOME:"):
			if p := phases[cur]; p != nil {
				p.home = l
			}
		case strings.HasPrefix(l, "BEADS_DIR:"):
			if p := phases[cur]; p != nil {
				p.beadsDir = l
			}
		case strings.HasPrefix(l, "PATH:"):
			if p := phases[cur]; p != nil {
				p.path = l
			}
		case strings.HasPrefix(l, "METRICS:"):
			if p := phases[cur]; p != nil {
				p.metrics = l
			}
		case strings.HasPrefix(l, "MARKER:"):
			markerBeforeInit = strings.TrimPrefix(l, "MARKER:")
		}
		if strings.Contains(l, "hostile") || strings.Contains(l, testToken) ||
			strings.Contains(l, "MULTICA_TOKEN") || strings.Contains(l, "MULTICA_SERVER_URL") {
			t.Fatalf("initializer environment leaked forbidden material (hostile vars, credentials, or server URL)")
		}
	}
	exeDir := "PATH:" + filepath.Dir(exe)
	for phase, want := range map[string]struct{ home, beadsDir string }{
		"--version": {"HOME:/proc/self/fd/3", "BEADS_DIR:/proc/self/fd/3/.beads"},
		"--sandbox": {"HOME:/proc/self/fd/3/.native_init_home", "BEADS_DIR:/proc/self/fd/3/.beads"},
	} {
		p := phases[phase]
		if p == nil {
			t.Fatalf("fake never ran the %s phase", phase)
		}
		if p.home != want.home {
			t.Fatalf("%s phase HOME wrong: got %q want %q", phase, p.home, want.home)
		}
		if p.beadsDir != want.beadsDir {
			t.Fatalf("%s phase BEADS_DIR wrong: got %q want %q", phase, p.beadsDir, want.beadsDir)
		}
		if p.path != exeDir {
			t.Fatalf("%s phase PATH must be only the executable's directory: got %q", phase, p.path)
		}
		if p.metrics != "METRICS:1 1 1 1" {
			t.Fatalf("%s phase metrics opts must all be disabled: got %q", phase, p.metrics)
		}
	}
	if markerBeforeInit == "" {
		t.Fatal("fake did not record the pre-init marker bytes")
	}

	domainDir := filepath.Join(home, ".multica", "native-sources", sourceID)
	nseFileMode(t, filepath.Join(domainDir, ".native_initialized"), 0o600, "init receipt")
	if info, statErr := os.Stat(filepath.Join(domainDir, ".beads")); statErr != nil || !info.IsDir() {
		t.Fatalf("init must create the .beads directory: %v", statErr)
	}
	// Marker bytes unchanged ACROSS init: compared against the fake's
	// pre-init observation. (Unchanged across replay is checked below.)
	markerNow, err := os.ReadFile(filepath.Join(domainDir, ".native_owner"))
	if err != nil {
		t.Fatalf("owner marker missing: %v", err)
	}
	if string(markerNow) != markerBeforeInit {
		t.Fatal("owner marker bytes must be unchanged across init")
	}
	identity := nseReadIndex(t, sourceID)
	if identity.SourceID != sourceID || identity.RuntimeID != runtimeID || identity.DaemonID != daemonID {
		t.Fatalf("published index identity wrong: %+v", identity)
	}

	var mode string
	var enabled bool
	var status string
	var hash *string
	fx.QueryRow(t, `SELECT mode, enabled,
		CASE WHEN native_enrolled_at IS NULL THEN 'pending' ELSE 'enrolled' END,
		native_manifest_hash FROM work_source WHERE id=$1 AND workspace_id=$2`,
		sourceID, testWorkspaceID).Scan(&mode, &enabled, &status, &hash)
	if mode != "native" || enabled || status != "pending" || hash != nil {
		t.Fatalf("server row after init wrong: mode=%q enabled=%t status=%q hash_present=%t", mode, enabled, status, hash != nil)
	}
	for table, query := range rowQueries {
		if n := fx.Count(t, query, testWorkspaceID); n != baseline[table] {
			t.Fatalf("init changed %s rows: before=%d after=%d", table, baseline[table], n)
		}
	}

	replay, errOut, err := nsiProvision(t, ctx, cliPath, testWorkspaceID, runtimeID, requestID, "native init acceptance", daemonID, exe)
	if err != nil {
		t.Fatalf("replay CLI failed: %v stderr=%q", err, errOut)
	}
	if replay.ID != sourceID || replay.NativeEnrollmentID != source.NativeEnrollmentID {
		t.Fatalf("replay must keep S/E: got %q/%q", replay.ID, replay.NativeEnrollmentID)
	}
	if versions, inits = nsiLaunches(t, log); versions != 1 || inits != 1 {
		t.Fatalf("valid receipt replay must not relaunch: versions=%d inits=%d", versions, inits)
	}
	if marker, err := os.ReadFile(filepath.Join(domainDir, ".native_owner")); err != nil || string(marker) != string(markerNow) {
		t.Fatalf("owner marker bytes must be unchanged across replay: %v", err)
	}
}

func TestNativeSourceInitCLICrashRefusesNonFreshRetry(t *testing.T) {
	cliPath := nsiRequireInitCLI(t)
	if testPool == nil {
		t.Fatal("explicit native enrollment acceptance requires the managed database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)

	home, indexDir := nsiHome(t)
	crashExe, log := nsiWriteFakeBD(t, t.TempDir(), "bd-crash", indexDir, true)

	daemonID := "nsi-crash-" + uuid.NewString()
	runtimeID := nseDaemonRuntime(t, fx, testUserID, daemonID, "claude")
	requestID := uuid.NewString()

	_, errOut, err := nsiProvision(t, ctx, cliPath, testWorkspaceID, runtimeID, requestID, "native init crash", daemonID, crashExe)
	if err == nil {
		t.Fatal("provision must fail when the initializer crashes")
	}
	if !strings.Contains(errOut, "initialization failed") && !strings.Contains(errOut, "init failed") {
		t.Fatalf("failure must name the initializer, stderr=%q", errOut)
	}
	// The CLI prints no source JSON on failure: resolve the real row by the
	// stable request UUID.
	sourceID := nsiSourceIDByRequest(t, fx, requestID)
	nsiCleanupSource(t, fx, sourceID)
	domainDir := filepath.Join(home, ".multica", "native-sources", sourceID)
	if _, statErr := os.Stat(filepath.Join(domainDir, ".native_initialized")); !os.IsNotExist(statErr) {
		t.Fatalf("crash must leave no receipt: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(domainDir, ".beads")); statErr != nil {
		t.Fatalf("crash leaves the partial .beads directory (no reset): %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(indexDir, sourceID+".json")); !os.IsNotExist(statErr) {
		t.Fatalf("failed init must not publish the profile index: %v", statErr)
	}

	if _, errOut, err = nsiProvision(t, ctx, cliPath, testWorkspaceID, runtimeID, requestID, "native init crash", daemonID, crashExe); err == nil {
		t.Fatal("retry must refuse the non-fresh domain")
	}
	nseAssertSanitized(t, "", errOut)
	if _, inits := nsiLaunches(t, log); inits != 1 {
		t.Fatalf("retry must not launch a second init: inits=%d", inits)
	}
}

func TestNativeSourceInitCLIBusyLock(t *testing.T) {
	cliPath := nsiRequireInitCLI(t)
	if testPool == nil {
		t.Fatal("explicit native enrollment acceptance requires the managed database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)

	home, indexDir := nsiHome(t)
	exe, log := nsiWriteFakeBD(t, t.TempDir(), "bd", indexDir, false)

	daemonID := "nsi-busy-" + uuid.NewString()
	runtimeID := nseDaemonRuntime(t, fx, testUserID, daemonID, "claude")
	requestID := uuid.NewString()

	source, errOut, err := nsiProvision(t, ctx, cliPath, testWorkspaceID, runtimeID, requestID, "native init busy", daemonID, "")
	if err != nil {
		t.Fatalf("default provision failed: %v stderr=%q", err, errOut)
	}
	sourceID := source.ID
	nsiCleanupSource(t, fx, sourceID)
	identityBefore := nseReadIndex(t, sourceID)
	domainDir := filepath.Join(home, ".multica", "native-sources", sourceID)
	markerBefore, err := os.ReadFile(filepath.Join(domainDir, ".native_owner"))
	if err != nil {
		t.Fatal(err)
	}

	// Hold the domain lock in-process, as a running daemon would. Release
	// immediately on any path, including failures.
	root, _ := nseOpenGlobalRoot(t)
	defer root.Close()
	domain, err := execenv.OpenNativeSource(root, identityBefore)
	if err != nil {
		t.Fatalf("hold domain lock: %v", err)
	}
	defer domain.Close()

	if _, errOut, err = nsiProvision(t, ctx, cliPath, testWorkspaceID, runtimeID, requestID, "native init busy", daemonID, exe); err == nil {
		t.Fatal("busy replay with the initializer flag must fail")
	}
	nseAssertSanitized(t, "", errOut)
	if nsiLog(t, log) != nil {
		t.Fatal("busy replay must not launch the initializer")
	}

	if _, errOut, err = nsiProvision(t, ctx, cliPath, testWorkspaceID, runtimeID, requestID, "native init busy", daemonID, ""); err != nil {
		t.Fatalf("default busy replay must stay idempotent: %v stderr=%q", err, errOut)
	}

	marker, err := os.ReadFile(filepath.Join(domainDir, ".native_owner"))
	if err != nil || string(marker) != string(markerBefore) {
		t.Fatalf("marker bytes must be preserved: %v", err)
	}
	if after := nseReadIndex(t, sourceID); after != identityBefore {
		t.Fatalf("index identity changed: before=%+v after=%+v", identityBefore, after)
	}
}

// TestNativeSourceInitCLIAuthority pins that the initializer only ever runs
// after the intent endpoint accepted the exact owner+admin+runtime-owner
// authority: denials happen server-side, before any local filesystem work.
func TestNativeSourceInitCLIAuthority(t *testing.T) {
	cliPath := nsiRequireInitCLI(t)
	if testPool == nil {
		t.Fatal("explicit native enrollment acceptance requires the managed database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)

	home, indexDir := nsiHome(t)
	exe, log := nsiWriteFakeBD(t, t.TempDir(), "bd", indexDir, false)

	member := fx.User(t, "NSI Member", "nsi-member-"+uuid.NewString()+"@multica.ai")
	fx.Member(t, testWorkspaceID, member, "member")
	memberJWT, err := generateTestJWT(member, "", "NSI Member")
	if err != nil {
		t.Fatal(err)
	}
	// Member-owned runtime: membership without admin is not enough.
	memberRuntime := nseDaemonRuntime(t, fx, member, "nsi-member-daemon", "claude")
	// Admin-owned-by-someone-else runtime: admin without runtime ownership.
	outsider := fx.User(t, "NSI Outsider", "nsi-outsider-"+uuid.NewString()+"@multica.ai")
	outsiderRuntime := nseDaemonRuntime(t, fx, outsider, "nsi-outsider-daemon", "claude")

	for _, tc := range []struct {
		name      string
		token     string
		runtimeID string
	}{
		{"member who owns the runtime", memberJWT, memberRuntime},
		{"admin who does not own the runtime", testToken, outsiderRuntime},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MULTICA_TOKEN", tc.token)
			out, errOut, err := nseRunCLI(t, ctx, cliPath, nil,
				"--profile", nseProfile, "daemon", "source", "provision",
				"--workspace", testWorkspaceID, "--runtime", tc.runtimeID,
				"--name", "native init authority", "--request-id", uuid.NewString(),
				"--daemon-id", "nsi-authority", "--beads-executable", exe)
			nseAssertSanitized(t, out, errOut)
			if err == nil {
				t.Fatal("unauthorized provision must be denied")
			}
			if nsiLog(t, log) != nil {
				t.Fatal("denial must happen before any initializer launch")
			}
			if _, statErr := os.Stat(filepath.Join(home, ".multica", "native-sources")); !os.IsNotExist(statErr) {
				t.Fatalf("denied provision must not touch native-sources: %v", statErr)
			}
		})
	}
}
