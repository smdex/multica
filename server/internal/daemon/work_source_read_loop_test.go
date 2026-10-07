package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
)

const (
	wsrDaemonID   = "11111111-1111-1111-1111-111111111111"
	wsrRuntimeID  = "22222222-2222-2222-2222-222222222222"
	wsrRuntimeID2 = "66666666-6666-6666-6666-666666666666"
	wsrCommandID  = "33333333-3333-3333-3333-333333333333"
	wsrRequestID  = "44444444-4444-4444-4444-444444444444"
	wsrSourceID   = "55555555-5555-5555-5555-555555555555"
)

// wsrServer is a minimal in-process source-read endpoint backed by httptest,
// with one route set per tracked runtime.
type wsrServer struct {
	t         *testing.T
	srv       *httptest.Server
	mu        sync.Mutex
	exchanges atomic.Int64
	humanAuth atomic.Int64 // exchanges attempted without the daemon PAT

	// Scriptable behaviors.
	listBody   map[string]func() []any // runtimeID -> pending list
	claimMut   func(dto map[string]any)
	reportFunc func(attempt int) int // attempt counts prior failures
	reportSeen atomic.Int64
	// cmdExpiry is the single expiry instant shared by the pending item and
	// the claim receipt, so receipt-vs-pending equality is deterministic.
	cmdExpiry  string
	lastReport struct {
		Status, Result, Error string
		Token                 string
	}
}

func newWsrServer(t *testing.T) *wsrServer {
	t.Helper()
	w := &wsrServer{t: t, listBody: map[string]func() []any{}, cmdExpiry: time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)}
	mux := http.NewServeMux()
	register := func(runtimeID string) {
		mux.HandleFunc("POST /api/daemon/runtimes/"+runtimeID+"/source-read-token", func(rw http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer pat_daemon" {
				w.humanAuth.Add(1)
				http.Error(rw, "exchange must use the daemon PAT", http.StatusUnauthorized)
				return
			}
			w.exchanges.Add(1)
			expires := time.Now().Add(90 * time.Second).UTC().Format(time.RFC3339)
			fmt.Fprintf(rw, `{"token":"msr_%s_%d","token_type":"Bearer","scope":"source:read","runtime_id":%q,"workspace_id":%q,"daemon_id":%q,"expires_at":%q,"expires_in":90}`,
				runtimeID, w.exchanges.Load(), runtimeID, boundWS, wsrDaemonID, expires)
		})
		mux.HandleFunc("GET /api/daemon/runtimes/"+runtimeID+"/work-source-commands", func(rw http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer msr_") {
				http.Error(rw, "scoped token required", http.StatusUnauthorized)
				return
			}
			w.mu.Lock()
			fn := w.listBody[runtimeID]
			w.mu.Unlock()
			body := []any{}
			if fn != nil {
				body = fn()
			}
			json.NewEncoder(rw).Encode(body)
		})
		mux.HandleFunc("POST /api/daemon/runtimes/"+runtimeID+"/work-source-commands/"+wsrCommandID+"/claim", func(rw http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer msr_") {
				http.Error(rw, "scoped token required", http.StatusUnauthorized)
				return
			}
			w.mu.Lock()
			dto := map[string]any{
				"id": wsrCommandID, "request_id": wsrRequestID, "workspace_id": boundWS,
				"source_id": wsrSourceID, "command": "list", "native_id": "",
				"limit_count": 5, "config_revision": 1,
				"expires_at":         w.cmdExpiry,
				"status":             "claimed",
				"claimed_runtime_id": runtimeID,
			}
			if w.claimMut != nil {
				w.claimMut(dto)
			}
			w.mu.Unlock()
			json.NewEncoder(rw).Encode(dto)
		})
		mux.HandleFunc("POST /api/daemon/runtimes/"+runtimeID+"/work-source-commands/"+wsrCommandID+"/result", func(rw http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer msr_") {
				http.Error(rw, "scoped token required", http.StatusUnauthorized)
				return
			}
			var body struct {
				Status, Result, Error string
			}
			json.NewDecoder(r.Body).Decode(&body)
			attempt := w.reportSeen.Add(1)
			w.mu.Lock()
			fn := w.reportFunc
			w.mu.Unlock()
			if fn != nil {
				if status := fn(int(attempt)); status != http.StatusOK {
					http.Error(rw, "retry", status)
					return
				}
			}
			w.mu.Lock()
			w.lastReport.Status, w.lastReport.Result, w.lastReport.Error, w.lastReport.Token = body.Status, body.Result, body.Error, r.Header.Get("Authorization")
			w.mu.Unlock()
			fmt.Fprintf(rw, `{"id":%q,"request_id":%q,"workspace_id":%q,"source_id":%q,"command":"list","config_revision":1,"expires_at":%q,"status":%q,"claimed_runtime_id":%q,"result":%q,"error":%q}`,
				wsrCommandID, wsrRequestID, boundWS, wsrSourceID, w.cmdExpiry, body.Status, runtimeID, body.Result, body.Error)
		})
	}
	register(wsrRuntimeID)
	register(wsrRuntimeID2)
	w.srv = httptest.NewServer(mux)
	t.Cleanup(w.srv.Close)
	return w
}

func (w *wsrServer) client() *Client {
	c := NewClient(w.srv.URL)
	c.SetToken("pat_daemon")
	return c
}

func (w *wsrServer) setPending(runtimeID, handle string, expiresIn time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cmdExpiry = time.Now().Add(expiresIn).UTC().Format(time.RFC3339)
	expiry := w.cmdExpiry
	w.listBody[runtimeID] = func() []any {
		return []any{map[string]any{
			"id": wsrCommandID, "request_id": wsrRequestID, "workspace_id": boundWS,
			"source_id": wsrSourceID, "source_handle": handle, "command": "list",
			"limit_count": 5, "config_revision": 1,
			"expires_at": expiry,
			"status":     "pending",
		}}
	}
}

func newWsrDaemon(t *testing.T, w *wsrServer, bindings []cli.WorkSourceReadBinding, runtimeIDs ...string) *Daemon {
	t.Helper()
	if len(runtimeIDs) == 0 {
		runtimeIDs = []string{wsrRuntimeID}
	}
	d := &Daemon{
		cfg:    Config{DaemonID: wsrDaemonID, WorkSourceReads: bindings},
		client: w.client(),
		logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	idx := map[string]Runtime{}
	for _, id := range runtimeIDs {
		idx[id] = Runtime{ID: id, Provider: "test"}
	}
	d.mu.Lock()
	d.workspaces = map[string]*workspaceState{boundWS: {workspaceID: boundWS, runtimeIDs: runtimeIDs}}
	d.runtimeIndex = idx
	d.mu.Unlock()
	return d
}

func newWsrState(d *Daemon) *workSourceReadState {
	return &workSourceReadState{
		client: d.client, daemonID: d.cfg.DaemonID, logger: d.logger,
		now: time.Now, creds: map[string]SourceReadCredential{}, pending: map[string]workSourceReadOutcome{},
	}
}

// oneLaunchBD is a fake bd that records each launch in "runs".
func oneLaunchBD(t *testing.T, script string) cli.WorkSourceReadBinding {
	t.Helper()
	exe, dir := newFakeBD(t, script)
	return binding(exe, dir)
}

func launchCount(t *testing.T, b cli.WorkSourceReadBinding) int {
	t.Helper()
	out, err := os.ReadFile(filepath.Join(filepath.Dir(b.Executable), "runs"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(strings.TrimSpace(string(out)), "\n") + 1
}

func wsrPoll(t *testing.T, s *workSourceReadState, b cli.WorkSourceReadBinding, runtimeID string) {
	t.Helper()
	s.pollTarget(context.Background(), workSourceReadTarget{workspaceID: b.WorkspaceID, runtimeID: runtimeID}, []cli.WorkSourceReadBinding{b})
}

func TestWorkSourceReadLoopNoBindingsNoRequests(t *testing.T) {
	w := newWsrServer(t)
	w.srv.Config.Handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		t.Errorf("no HTTP expected with zero bindings, got %s %s", r.Method, r.URL.Path)
	})
	d := newWsrDaemon(t, w, nil)
	done := make(chan struct{})
	go func() { d.workSourceReadLoop(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not return immediately with no bindings")
	}
}

func TestWorkSourceReadLoopInvalidBindingsFailClosed(t *testing.T) {
	w := newWsrServer(t)
	w.srv.Config.Handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		t.Errorf("no HTTP expected with invalid bindings, got %s %s", r.Method, r.URL.Path)
	})
	bad := binding("/bin/bd", "rel/.beads") // relative beads_dir: invalid
	d := newWsrDaemon(t, w, []cli.WorkSourceReadBinding{bad})
	done := make(chan struct{})
	go func() { d.workSourceReadLoop(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not fail closed on invalid bindings")
	}
}

// Regression: every tracked runtime of a bound workspace is a poll target,
// not just the first sorted one. A source command routed by the server to the
// second runtime must still be discovered and executed.
func TestWorkSourceReadLoopSecondRuntimeDiscovered(t *testing.T) {
	w := newWsrServer(t)
	// The pending command arrives only at the second runtime.
	w.setPending(wsrRuntimeID2, "primary", 5*time.Minute)
	b := oneLaunchBD(t, `
printf 'launch\n' >> "$FAKE_BD_DIR/runs"
printf '[]'
`)
	d := newWsrDaemon(t, w, []cli.WorkSourceReadBinding{b}, wsrRuntimeID, wsrRuntimeID2)

	targets := d.workSourceReadTargets()
	if len(targets) != 2 {
		t.Fatalf("both runtimes must be poll targets, got %d: %+v", len(targets), targets)
	}
	s := newWsrState(d)
	for _, tg := range targets {
		s.pollTarget(context.Background(), tg, d.cfg.WorkSourceReads)
	}
	if launchCount(t, b) != 1 {
		t.Fatalf("command on second runtime must execute exactly once, launches=%d", launchCount(t, b))
	}
	if w.reportSeen.Load() != 1 {
		t.Fatalf("expected one report, got %d", w.reportSeen.Load())
	}
}

// workSourceReadTargets must not duplicate a runtime shared across workspaces.
func TestWorkSourceReadTargetsDeduplicates(t *testing.T) {
	w := newWsrServer(t)
	d := newWsrDaemon(t, w, nil, wsrRuntimeID, wsrRuntimeID2)
	d.mu.Lock()
	other := "99999999-9999-9999-9999-999999999999"
	d.workspaces[other] = &workspaceState{workspaceID: other, runtimeIDs: []string{wsrRuntimeID2}}
	d.mu.Unlock()
	b := binding("/bin/bd", "/beads")
	b.WorkspaceID = boundWS // only boundWS is approved
	d.cfg.WorkSourceReads = []cli.WorkSourceReadBinding{b}
	targets := d.workSourceReadTargets()
	for _, tg := range targets {
		if tg.workspaceID != boundWS {
			t.Fatalf("unbound workspace leaked into targets: %+v", tg)
		}
	}
	if len(targets) != 2 {
		t.Fatalf("both runtimes of the bound workspace must be targets, got %d: %+v", len(targets), targets)
	}
}

// Regression: workspaces without an approved local binding are never polled —
// no capability exchange, no list request, nothing.
func TestWorkSourceReadLoopUnboundWorkspaceZeroHTTP(t *testing.T) {
	w := newWsrServer(t)
	unbound := "99999999-9999-9999-9999-999999999999"
	unboundRuntime := "88888888-8888-8888-8888-888888888888"
	// Any request whose path names the unbound runtime is a violation;
	// bound-workspace traffic is fine.
	unboundRuntimePath := "/" + unboundRuntime + "/"
	w.srv.Config.Handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, unboundRuntimePath) {
			t.Errorf("unbound runtime must never be polled, got %s %s", r.Method, r.URL.Path)
		}
		http.NotFound(rw, r)
	})
	b := oneLaunchBD(t, `printf 'launch\n' >> "$FAKE_BD_DIR/runs"; printf '[]'`)
	d := newWsrDaemon(t, w, []cli.WorkSourceReadBinding{b}, wsrRuntimeID, wsrRuntimeID2)
	d.mu.Lock()
	d.workspaces[unbound] = &workspaceState{workspaceID: unbound, runtimeIDs: []string{unboundRuntime}}
	d.runtimeIndex[unboundRuntime] = Runtime{ID: unboundRuntime, Provider: "test"}
	d.mu.Unlock()

	targets := d.workSourceReadTargets()
	for _, tg := range targets {
		if tg.workspaceID == unbound {
			t.Fatalf("unbound workspace must not be a poll target: %+v", tg)
		}
	}
	// Exercising every target may serve the two bound-workspace runtimes; the
	// unbound runtime must never appear in any request path.
	for _, tg := range targets {
		if tg.runtimeID == unboundRuntime {
			t.Fatalf("unbound runtime must never be polled: %+v", tg)
		}
	}
	s := newWsrState(d)
	for _, tg := range targets {
		s.pollTarget(context.Background(), tg, d.cfg.WorkSourceReads)
	}
	if launchCount(t, b) != 0 {
		t.Fatal("nothing should have executed")
	}
}

func TestWorkSourceReadLoopExactBindingSuccess(t *testing.T) {
	w := newWsrServer(t)
	w.setPending(wsrRuntimeID, "primary", 5*time.Minute)
	b := oneLaunchBD(t, `
printf 'launch\n' >> "$FAKE_BD_DIR/runs"
printf '[]'
`)
	d := newWsrDaemon(t, w, []cli.WorkSourceReadBinding{b})
	s := newWsrState(d)
	wsrPoll(t, s, b, wsrRuntimeID)

	if got := w.reportSeen.Load(); got != 1 {
		t.Fatalf("expected exactly one report, got %d", got)
	}
	if launchCount(t, b) != 1 {
		t.Fatalf("expected exactly one subprocess launch, got %d", launchCount(t, b))
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.lastReport.Status != "succeeded" {
		t.Fatalf("expected succeeded, got %s (%s)", w.lastReport.Status, w.lastReport.Error)
	}
	if !strings.HasPrefix(w.lastReport.Token, "Bearer msr_") {
		t.Fatalf("report must use scoped credential, got %q", w.lastReport.Token)
	}
	if len(s.pending) != 0 {
		t.Fatalf("outcome must be discarded after receipt, pending=%v", s.pending)
	}
}

func TestWorkSourceReadLoopForeignHandleNotClaimed(t *testing.T) {
	w := newWsrServer(t)
	w.setPending(wsrRuntimeID, "other-handle", 5*time.Minute)
	b := oneLaunchBD(t, `printf 'launch\n' >> "$FAKE_BD_DIR/runs"; printf '[]'`)
	d := newWsrDaemon(t, w, []cli.WorkSourceReadBinding{b})
	s := newWsrState(d)
	wsrPoll(t, s, b, wsrRuntimeID)

	if launchCount(t, b) != 0 {
		t.Fatal("foreign source handle must not execute anything")
	}
	if w.reportSeen.Load() != 0 {
		t.Fatal("foreign handle must not be reported")
	}
}

func TestWorkSourceReadLoopNearExpiryNotClaimed(t *testing.T) {
	w := newWsrServer(t)
	w.setPending(wsrRuntimeID, "primary", 5*time.Second) // below MinBudget
	b := oneLaunchBD(t, `printf 'launch\n' >> "$FAKE_BD_DIR/runs"; printf '[]'`)
	d := newWsrDaemon(t, w, []cli.WorkSourceReadBinding{b})
	s := newWsrState(d)
	wsrPoll(t, s, b, wsrRuntimeID)

	if launchCount(t, b) != 0 {
		t.Fatal("command near expiry must not be claimed or executed")
	}
}

// Mismatched claim receipts must never launch the executable.
func TestWorkSourceReadLoopMismatchedClaimNoLaunch(t *testing.T) {
	cases := map[string]func(map[string]any){
		"other claimer":    func(dto map[string]any) { dto["claimed_runtime_id"] = wsrRuntimeID2 },
		"other request":    func(dto map[string]any) { dto["request_id"] = "77777777-7777-7777-7777-777777777777" },
		"other source":     func(dto map[string]any) { dto["source_id"] = "88888888-8888-8888-8888-888888888888" },
		"other native":     func(dto map[string]any) { dto["native_id"] = "bd-9" },
		"other revision":   func(dto map[string]any) { dto["config_revision"] = 7 },
		"other limit":      func(dto map[string]any) { dto["limit_count"] = 99 },
		"missing expiry":   func(dto map[string]any) { dto["expires_at"] = "" },
		"malformed expiry": func(dto map[string]any) { dto["expires_at"] = "not-a-timestamp" },
		"extended expiry": func(dto map[string]any) {
			dto["expires_at"] = time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)
		},
		"expiry in the past": func(dto map[string]any) { dto["expires_at"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339) },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWsrServer(t)
			w.setPending(wsrRuntimeID, "primary", 5*time.Minute)
			w.mu.Lock()
			w.claimMut = mut
			w.mu.Unlock()
			b := oneLaunchBD(t, `printf 'launch\n' >> "$FAKE_BD_DIR/runs"; printf '[]'`)
			d := newWsrDaemon(t, w, []cli.WorkSourceReadBinding{b})
			s := newWsrState(d)
			wsrPoll(t, s, b, wsrRuntimeID)

			if launchCount(t, b) != 0 {
				t.Fatal("mismatched claim receipt must not execute anything")
			}
			if w.reportSeen.Load() != 0 {
				t.Fatal("mismatched claim must not be reported")
			}
		})
	}
}

func TestWorkSourceReadLoopReportLossRetryWithoutReexecute(t *testing.T) {
	w := newWsrServer(t)
	w.setPending(wsrRuntimeID, "primary", 5*time.Minute)
	w.mu.Lock()
	w.reportFunc = func(attempt int) int {
		if attempt == 1 {
			return http.StatusInternalServerError // first report lost
		}
		return http.StatusOK
	}
	w.mu.Unlock()
	b := oneLaunchBD(t, `
printf 'launch\n' >> "$FAKE_BD_DIR/runs"
printf '[{"id":"bd-1","title":"A","status":"open","priority":2,"issue_type":"task","created_at":"t","updated_at":"t","dependency_count":0,"dependent_count":0,"comment_count":0}]'
`)
	d := newWsrDaemon(t, w, []cli.WorkSourceReadBinding{b})
	s := newWsrState(d)
	wsrPoll(t, s, b, wsrRuntimeID)

	if w.reportSeen.Load() != 1 {
		t.Fatalf("expected one report attempt after poll, got %d", w.reportSeen.Load())
	}
	if len(s.pending) != 1 {
		t.Fatal("lost report outcome must be retained")
	}
	// Server recovers: flush retries the exact stored outcome.
	s.flushPendingReports(context.Background())
	if got := w.reportSeen.Load(); got != 2 {
		t.Fatalf("expected exactly one retry, got %d attempts", got)
	}
	if launchCount(t, b) != 1 {
		t.Fatalf("retry must not re-run the executable, launches=%d", launchCount(t, b))
	}
	if len(s.pending) != 0 {
		t.Fatal("outcome must be discarded after receipt")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.lastReport.Status != "succeeded" || w.lastReport.Result == "" {
		t.Fatalf("retry must re-send exact stored fields, got %+v", w.lastReport)
	}
}

func TestWorkSourceReadLoopUnauthorizedRefreshesOncePerOperation(t *testing.T) {
	w := newWsrServer(t)
	w.setPending(wsrRuntimeID, "primary", 5*time.Minute)
	b := oneLaunchBD(t, `printf 'launch\n' >> "$FAKE_BD_DIR/runs"; printf '[]'`)
	d := newWsrDaemon(t, w, []cli.WorkSourceReadBinding{b})
	s := newWsrState(d)
	wsrPoll(t, s, b, wsrRuntimeID)

	if w.reportSeen.Load() != 1 {
		t.Fatalf("expected the claim+execute+report flow to complete, reports=%d", w.reportSeen.Load())
	}
	if d.client.Token() != "pat_daemon" {
		t.Fatal("loop must never replace the client's human token")
	}
	if w.humanAuth.Load() != 0 {
		t.Fatal("exchange must always use the daemon PAT")
	}
}

func TestWorkSourceReadLoopCancellationStopsWaits(t *testing.T) {
	w := newWsrServer(t)
	w.setPending(wsrRuntimeID, "primary", 5*time.Minute)
	// A bd that blocks until its parent dies: the loop context must cancel
	// the subprocess instead of waiting out MaxExec.
	b := oneLaunchBD(t, `
printf 'launch\n' >> "$FAKE_BD_DIR/runs"
sleep 60
`)
	d := newWsrDaemon(t, w, []cli.WorkSourceReadBinding{b})
	s := newWsrState(d)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	s.pollTarget(ctx, workSourceReadTarget{workspaceID: b.WorkspaceID, runtimeID: wsrRuntimeID}, []cli.WorkSourceReadBinding{b})
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("cancellation must stop the subprocess promptly, took %s", elapsed)
	}
	cancel()
}

// The failure diagnostic is the fixed generic sentence: no subprocess stderr,
// path, or secret may reach the server in any form.
func TestWorkSourceReadLoopFixedGenericDiagnostic(t *testing.T) {
	w := newWsrServer(t)
	w.setPending(wsrRuntimeID, "primary", 5*time.Minute)
	// A bd that fails, printing a distinctive fake secret and real local
	// paths on stderr.
	b := oneLaunchBD(t, `
printf 'launch\n' >> "$FAKE_BD_DIR/runs"
echo "SECRET-TOKEN-9f2b1c at $FAKE_BD_DIR/src/.beads/archive" >&2
exit 7
`)
	d := newWsrDaemon(t, w, []cli.WorkSourceReadBinding{b})
	s := newWsrState(d)
	wsrPoll(t, s, b, wsrRuntimeID)

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.lastReport.Status != "failed" {
		t.Fatalf("expected failed report, got %s", w.lastReport.Status)
	}
	diag := w.lastReport.Error
	if diag != workSourceReadFailedDiagnostic {
		t.Fatalf("diagnostic must be the fixed generic sentence, got %q", diag)
	}
	if strings.Contains(diag, "SECRET-TOKEN-9f2b1c") || strings.Contains(diag, b.Executable) ||
		strings.Contains(diag, b.BeadsDir) || strings.Contains(diag, ".beads") || strings.Contains(diag, "/") {
		t.Fatalf("diagnostic leaks subprocess content: %q", diag)
	}
}

// A pending outcome inside the last 10 seconds before its deadline must
// still be retried (there is no early-discard grace): the fence is exactly
// ExpiresAt.
func TestWorkSourceReadLoopRetryWithinLastTenSeconds(t *testing.T) {
	w := newWsrServer(t)
	w.mu.Lock()
	w.reportFunc = func(attempt int) int {
		if attempt == 1 {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	}
	w.mu.Unlock()
	b := oneLaunchBD(t, `printf '[]'`)
	d := newWsrDaemon(t, w, []cli.WorkSourceReadBinding{b})
	s := newWsrState(d)
	s.retainOutcome(workSourceReadOutcome{
		workspaceID: b.WorkspaceID, runtimeID: wsrRuntimeID, commandID: wsrCommandID,
		status: "succeeded", result: "[]",
		expiresAt: time.Now().Add(5 * time.Second), // within the old 10s grace window
	})
	// First flush: the 503 fails, outcome retained (not grace-discarded).
	s.flushPendingReports(context.Background())
	if w.reportSeen.Load() != 1 {
		t.Fatalf("first attempt expected, attempts=%d", w.reportSeen.Load())
	}
	if len(s.pending) != 1 {
		t.Fatal("outcome within last 10s must not be discarded before ExpiresAt")
	}
	// Next round: retry is acknowledged.
	s.flushPendingReports(context.Background())
	if w.reportSeen.Load() != 2 {
		t.Fatalf("outcome within last 10s must be retried then acknowledged, attempts=%d", w.reportSeen.Load())
	}
	if len(s.pending) != 0 {
		t.Fatal("outcome must be discarded after receipt")
	}
}

func TestWorkSourceReadLoopExpiryFenceDiscardsPending(t *testing.T) {
	w := newWsrServer(t)
	w.mu.Lock()
	w.reportFunc = func(int) int { return http.StatusBadGateway } // always lost
	w.mu.Unlock()
	b := oneLaunchBD(t, `printf '[]'`)
	d := newWsrDaemon(t, w, []cli.WorkSourceReadBinding{b})
	s := newWsrState(d)
	// An outcome already past its deadline: the fence must drop it.
	s.retainOutcome(workSourceReadOutcome{
		workspaceID: b.WorkspaceID, runtimeID: wsrRuntimeID, commandID: wsrCommandID,
		status: "succeeded", result: "[]",
		expiresAt: time.Now().Add(-time.Second),
	})
	s.flushPendingReports(context.Background())
	if len(s.pending) != 0 {
		t.Fatal("expired outcome must be discarded by the deadline fence")
	}
	if w.reportSeen.Load() != 0 {
		t.Fatal("expired outcome must not be re-sent")
	}
}

// At exactly the deadline the fence discards without any HTTP: equality is
// expiry, not "strictly after".
func TestWorkSourceReadLoopExpiryEqualityNoHTTP(t *testing.T) {
	w := newWsrServer(t)
	w.srv.Config.Handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		t.Errorf("no HTTP expected at expiry equality, got %s %s", r.Method, r.URL.Path)
	})
	b := oneLaunchBD(t, `printf '[]'`)
	d := newWsrDaemon(t, w, []cli.WorkSourceReadBinding{b})
	s := newWsrState(d)
	now := time.Now().Add(time.Minute)
	s.now = func() time.Time { return now }
	outcome := workSourceReadOutcome{
		workspaceID: b.WorkspaceID, runtimeID: wsrRuntimeID, commandID: wsrCommandID,
		status: "succeeded", result: "[]",
		expiresAt: now,
	}
	s.retainOutcome(outcome)
	s.flushPendingReports(context.Background())
	s.retainOutcome(outcome)
	s.deliverOutcome(context.Background(), workSourceReadTarget{workspaceID: b.WorkspaceID, runtimeID: wsrRuntimeID}, outcome)
	if len(s.pending) != 0 {
		t.Fatal("outcome at expiry equality must be discarded")
	}
	if w.reportSeen.Load() != 0 {
		t.Fatal("no report may be attempted at expiry equality")
	}
}

func TestWorkSourceReadLoopOneCommandPerTargetRound(t *testing.T) {
	w := newWsrServer(t)
	w.setPending(wsrRuntimeID, "primary", 5*time.Minute)
	w.mu.Lock()
	first := w.listBody[wsrRuntimeID]
	w.listBody[wsrRuntimeID] = func() []any {
		rows := first()
		second := first()[0].(map[string]any)
		second["id"] = wsrRequestID
		return append(rows, second)
	}
	w.mu.Unlock()
	var secondClaims atomic.Int32
	w.srv.Config.Handler.(*http.ServeMux).HandleFunc("POST /api/daemon/runtimes/"+wsrRuntimeID+"/work-source-commands/"+wsrRequestID+"/claim", func(rw http.ResponseWriter, r *http.Request) {
		secondClaims.Add(1)
		http.Error(rw, "not this round", http.StatusConflict)
	})
	b := oneLaunchBD(t, `printf '[]'`)
	d := newWsrDaemon(t, w, []cli.WorkSourceReadBinding{b})
	wsrPoll(t, newWsrState(d), b, wsrRuntimeID)
	if w.reportSeen.Load() != 1 || secondClaims.Load() != 0 {
		t.Fatalf("one target consumed more than one command in a round: reports=%d second_claims=%d", w.reportSeen.Load(), secondClaims.Load())
	}
}

// 429 is a transient server condition, not a verdict: the outcome stays
// pending and is retried.
func TestWorkSourceReadLoopRateLimitRetainedAndRetried(t *testing.T) {
	w := newWsrServer(t)
	w.mu.Lock()
	w.reportFunc = func(attempt int) int {
		if attempt == 1 {
			return http.StatusTooManyRequests
		}
		return http.StatusOK
	}
	w.mu.Unlock()
	b := oneLaunchBD(t, `printf '[]'`)
	d := newWsrDaemon(t, w, []cli.WorkSourceReadBinding{b})
	s := newWsrState(d)
	s.retainOutcome(workSourceReadOutcome{
		workspaceID: b.WorkspaceID, runtimeID: wsrRuntimeID, commandID: wsrCommandID,
		status: "succeeded", result: "[]",
		expiresAt: time.Now().Add(time.Minute),
	})
	// First flush: the 429 is transient, outcome retained.
	s.flushPendingReports(context.Background())
	if w.reportSeen.Load() != 1 {
		t.Fatalf("first attempt expected, attempts=%d", w.reportSeen.Load())
	}
	if len(s.pending) != 1 {
		t.Fatal("429 must retain the outcome for retry")
	}
	// Next round: retry is acknowledged.
	s.flushPendingReports(context.Background())
	if w.reportSeen.Load() != 2 {
		t.Fatalf("429 must be retried, attempts=%d", w.reportSeen.Load())
	}
	if len(s.pending) != 0 {
		t.Fatal("outcome must be discarded after acknowledged retry")
	}
}

// Report retries route by the workspace frozen into the outcome, so a runtime
// that later disappears from the index cannot strand or misroute the retry.
func TestWorkSourceReadLoopRetryUsesFrozenWorkspace(t *testing.T) {
	w := newWsrServer(t)
	w.setPending(wsrRuntimeID, "primary", 5*time.Minute)
	w.mu.Lock()
	w.reportFunc = func(attempt int) int {
		if attempt == 1 {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	}
	w.mu.Unlock()
	b := oneLaunchBD(t, `printf '[]'`)
	d := newWsrDaemon(t, w, []cli.WorkSourceReadBinding{b})
	s := newWsrState(d)
	wsrPoll(t, s, b, wsrRuntimeID)

	// Simulate the runtime vanishing from tracking between poll and retry.
	d.mu.Lock()
	delete(d.runtimeIndex, wsrRuntimeID)
	d.workspaces[boundWS].runtimeIDs = nil
	d.mu.Unlock()

	s.flushPendingReports(context.Background())
	if w.reportSeen.Load() != 2 {
		t.Fatalf("retry must still reach the server via frozen routing, attempts=%d", w.reportSeen.Load())
	}
	if len(s.pending) != 0 {
		t.Fatal("outcome must be discarded after receipt")
	}
}
