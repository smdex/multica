package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// TestSourceReadDaemonDispatchThroughProductionRouter drives a real
// daemon.New(Config, logger).Run(ctx) against the production testServer and
// PG fixtures: the daemon registers its one runtime (provider claude backed by
// a test-created fake executable), discovers the pending work-source read
// command through its own polling loop, exchanges an msr_ capability,
// claims, executes the read with a test-created fake bd, and reports the
// result. The requesting user then observes the succeeded receipt over HTTP.
//
// No mock coordinator, no auth injection, no test-exported daemon internals.
// Both executables are test-created files; no installed CLI is probed, since
// PATH is reduced to the fixture directory and HOME/XDG/SHELL are isolated.
// The fake claude answers only `--version` (with a supported version) and
// refuses any other invocation, so the daemon can register but can never run
// real agent work. The fake bd writes a launch record before emitting the
// typed empty list, proving both that exactly one launch happened and that
// the exact workspace/handle-bound BEADS_DIR reached the executable.
func TestSourceReadDaemonDispatchThroughProductionRouter(t *testing.T) {
	for _, tc := range []struct {
		name      string
		command   string
		loseReply bool
	}{
		{"normal", "list", false},
		{"lost-terminal-reply", "list", true},
		{"detail", "read", false},
		{"source-failure", "missing", false},
	} {
		t.Run(tc.name, func(t *testing.T) { exerciseSourceReadDaemon(t, tc.command, tc.loseReply) })
	}
}

func exerciseSourceReadDaemon(t *testing.T, command string, loseReply bool) {
	if runtime.GOOS == "windows" {
		t.Skip("fake executables are POSIX shell scripts")
	}
	if testPool == nil {
		t.Skip("database not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)

	// --- Isolated environment: HOME, XDG dirs, SHELL, PATH. Everything the
	// daemon could probe lives in one fixture directory. SHELL points at an
	// explicitly missing executable whose basename is not a supported login
	// shell, so no login-shell agent resolution can run.
	home := t.TempDir()
	fixtureDir := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("SHELL", filepath.Join(fixtureDir, "no-login-shell"))
	t.Setenv("PATH", fixtureDir) // no installed CLIs discoverable

	// --- Test-created fake claude: answers --version with a supported
	// version, refuses any real work.
	fakeClaude := filepath.Join(fixtureDir, "claude")
	if err := os.WriteFile(fakeClaude, []byte(`#!/bin/sh
case "$1" in
--version) echo "2.3.0 (Claude Code)"; exit 0 ;;
*) echo "fake claude refuses real work" >&2; exit 64 ;;
esac
`), 0o755); err != nil {
		t.Fatal(err)
	}

	// --- Test-created fake bd: writes a launch record (argv + BEADS_DIR),
	// then answers the typed list contract. The log path is shell-quoted so
	// TMPDIR paths with spaces or quotes cannot break the script.
	bdDir := t.TempDir()
	fakeBD := filepath.Join(bdDir, "bd")
	launchLog := filepath.Join(bdDir, "launches")
	if err := os.WriteFile(fakeBD, []byte("#!/bin/sh\n"+
		"LAUNCH_LOG="+shellQuote(launchLog)+"\n"+
		"printf '%s|%s\n' \"$BEADS_DIR\" \"$*\" >> \"$LAUNCH_LOG\"\n"+
		"case \"$1:$2\" in\n"+
		"list:--json) printf '[]' ;;\n"+
		"show:--id=bd-1) printf '[{\"id\":\"bd-1\",\"revision\":\"r1\"}]' ;;\n"+
		"*) printf 'SECRET fixture-path /.beads/archive' >&2; exit 5 ;;\n"+
		"esac\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// --- PG fixtures: owned online runtime + observe work source, with the
	// daemon_id the real daemon will register under, so production
	// registration reuses the exact owned runtime/source rows. Scan fails
	// the test on query error or no row.
	runtimeID, daemonID := sourceReadFixture(t, fx, testUserID, "msr dispatch runtime", "claude")
	var sourceID, sourceHandle string
	fx.QueryRow(t, `SELECT id, source_handle FROM work_source WHERE runtime_id=$1`, runtimeID).Scan(&sourceID, &sourceHandle)
	bindingBeadsDir := filepath.Join(bdDir, "src", ".beads")
	if err := os.MkdirAll(bindingBeadsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// --- Real auth for the daemon: save the test credential under the
	// isolated HOME so Run.resolveAuth finds it through the production
	// CLI-config path.
	if err := cli.SaveCLIConfig(cli.CLIConfig{Token: testToken}); err != nil {
		t.Fatalf("save CLI config: %v", err)
	}

	// --- User creates the read receipt over HTTP before the daemon starts.
	requestID := uuid.NewString()
	commandPath := "/api/work-sources/" + sourceID + "/commands"
	commandBody := `{"request_id":"` + requestID + `","command":"list","limit":2}`
	nativeID := "bd-1"
	if command == "missing" {
		nativeID = "missing"
	}
	if command != "list" {
		commandBody = `{"request_id":"` + requestID + `","command":"read","native_id":"` + nativeID + `"}`
	}
	_, created := mustSourceReadCall(t, ctx, http.MethodPost, commandPath, testToken, testWorkspaceID,
		commandBody, http.StatusCreated)
	var receipt struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(created, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.ID == "" || receipt.Status != "pending" {
		t.Fatalf("invalid pending receipt: %+v", receipt)
	}

	// --- Real daemon config. HealthPort 0 lets listenHealth bind an
	// ephemeral port itself, avoiding a listen/close/rebind TOCTOU.
	serverURL := testServer.URL
	var reportReplies atomic.Int32
	if loseReply {
		target, err := url.Parse(testServer.URL)
		if err != nil {
			t.Fatal(err)
		}
		proxy := httputil.NewSingleHostReverseProxy(target)
		lostReply := errors.New("test dropped committed source report reply")
		proxy.ModifyResponse = func(resp *http.Response) error {
			if strings.HasSuffix(resp.Request.URL.Path, "/result") && reportReplies.Add(1) == 1 {
				// Production committed the receipt. Lose only its reply so
				// the real daemon must retry without rerunning the executable.
				resp.Body.Close()
				return lostReply
			}
			return nil
		}
		proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, lostReply) {
				conn, _, hijackErr := w.(http.Hijacker).Hijack()
				if hijackErr == nil {
					conn.Close()
				}
				return
			}
			http.Error(w, "test proxy transport failed", http.StatusBadGateway)
		}
		gateway := httptest.NewServer(proxy)
		t.Cleanup(gateway.Close)
		serverURL = gateway.URL
	}
	cfg := daemon.Config{
		ServerBaseURL:       serverURL,
		DaemonID:            daemonID,
		DeviceName:          "msr-dispatch-test",
		RuntimeName:         "MSR dispatch runtime",
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
		WorkSourceReads: []cli.WorkSourceReadBinding{{
			WorkspaceID:  testWorkspaceID,
			SourceHandle: sourceHandle,
			BeadsDir:     bindingBeadsDir,
			Executable:   fakeBD,
		}},
	}

	// --- Start the real daemon. The cleanup runs before t.Setenv restores
	// HOME/PATH even on a mid-test Fatal, and waits a bounded time for Run
	// to return. runDone is closed after the error is sent, so the explicit
	// wait below and this cleanup cannot block on each other.
	runCtx, runCancel := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := daemon.New(cfg, logger)
	go func() { runDone <- d.Run(runCtx); close(runDone) }()
	t.Cleanup(func() {
		runCancel()
		select {
		case <-runDone:
		case <-time.After(10 * time.Second):
			t.Error("daemon did not stop during fixture cleanup")
		}
	})

	// --- Observe the user-visible terminal receipt within 30s. A fresh
	// struct each poll: reusing one leaves stale fields from a prior body
	// on a decode hiccup.
	deadline := time.Now().Add(30 * time.Second)
	var terminal struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Result string `json:"result"`
		Error  string `json:"error"`
	}
	for {
		_, body := mustSourceReadCall(t, ctx, http.MethodGet, "/api/work-source-commands/"+receipt.ID, testToken, testWorkspaceID, "", http.StatusOK)
		var poll struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Result string `json:"result"`
			Error  string `json:"error"`
		}
		if err := json.Unmarshal(body, &poll); err != nil {
			t.Fatalf("decode receipt: %v", err)
		}
		terminal = poll
		if terminal.Status != "pending" && terminal.Status != "claimed" && (!loseReply || reportReplies.Load() >= 2) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("command never reached a terminal state: %+v", terminal)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if terminal.ID != receipt.ID {
		t.Fatal("terminal receipt changed identity")
	}
	switch command {
	case "list":
		if terminal.Status != "succeeded" || terminal.Result != "[]" || terminal.Error != "" {
			t.Fatalf("list receipt wrong: %+v", terminal)
		}
	case "read":
		var detail struct{ ID, Revision string }
		if err := json.Unmarshal([]byte(terminal.Result), &detail); err != nil || terminal.Status != "succeeded" || detail.ID != "bd-1" || detail.Revision != "r1" || terminal.Error != "" {
			t.Fatalf("detail receipt wrong: %+v, decode error: %v", terminal, err)
		}
	case "missing":
		if terminal.Status != "failed" || terminal.Result != "" || terminal.Error != "Source read failed; inspect local daemon configuration and source availability." {
			t.Fatalf("source failure did not produce a safe terminal receipt: %+v", terminal)
		}
	}

	// --- Exactly one bd launch, bound to the exact workspace/handle pair.
	launches := readLaunches(t, launchLog)
	if len(launches) != 1 {
		t.Fatalf("want exactly one bd launch, got %d: %q", len(launches), launches)
	}
	wantArgs := "list --json --flat -n 2"
	if command != "list" {
		wantArgs = "show --id=" + nativeID + " --json"
	}
	if launches[0] != bindingBeadsDir+"|"+wantArgs {
		t.Fatalf("launch binding or argv wrong: %q", launches[0])
	}

	// --- Cancel and await a bounded Run return.
	runCancel()
	select {
	case <-runDone:
	case <-time.After(30 * time.Second):
		t.Fatal("daemon Run did not return after cancel")
	}
	// No further launch after shutdown.
	after := readLaunches(t, launchLog)
	if len(after) != 1 {
		t.Fatalf("bd relaunched after cancel: %q", after)
	}
}

// shellQuote single-quotes s for safe interpolation into a POSIX shell
// script ('...' with embedded quotes escaped as '\”').
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func readLaunches(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}
