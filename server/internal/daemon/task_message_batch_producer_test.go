package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	"github.com/multica-ai/multica/server/pkg/agent"
)

// stubBatchRetrySleep removes real backoff waits for bounded-retry tests. It
// mutates the package-global retrySleep, so callers MUST NOT use t.Parallel().
func stubBatchRetrySleep(t *testing.T) {
	t.Helper()
	prev := retrySleep
	retrySleep = func(ctx context.Context, _ time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return nil
	}
	t.Cleanup(func() { retrySleep = prev })
}

// TestClientTaskMessageBatch_ExactReplayReceipt pins the wire contract at the
// client boundary: a transient 503 is resent with an identical body, and only
// the exact {status:ok,batch_id:<echo>} receipt acks. A 200 receipt that does
// not echo the batch_id is a permanent errInvalidResponseBody, not transient,
// so it is never resent within one call.
func TestClientTaskMessageBatch_ExactReplayReceipt(t *testing.T) {
	stubBatchRetrySleep(t)
	var sends atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			BatchID string `json:"batch_id"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		if sends.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable) // ambiguous commit
			return
		}
		// Malformed receipt: not a commit proof, not transient.
		_, _ = io.WriteString(w, `{"status":"ok","batch_id":"other-uuid"}`)
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	batchID := "11111111-1111-1111-1111-111111111111"
	err := c.ReportTaskMessageBatch(context.Background(), "task", batchID,
		[]TaskMessageData{{Seq: 1, Type: "text", Content: "exact replay"}})
	if !errors.Is(err, errInvalidResponseBody) {
		t.Fatalf("malformed receipt must surface errInvalidResponseBody, got %v", err)
	}
	if got := sends.Load(); got != 2 {
		t.Fatalf("sent %d times, want 2 (503 resend, then permanent malformed receipt)", got)
	}
}

// TestClientTaskMessageBatch_NoRetryOnConflict: a 409 (different payload under
// the same batch_id) is permanent and is never resent.
func TestClientTaskMessageBatch_NoRetryOnConflict(t *testing.T) {
	stubBatchRetrySleep(t)
	var sends atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sends.Add(1)
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()
	err := NewClient(srv.URL).ReportTaskMessageBatch(context.Background(), "task",
		"22222222-2222-2222-2222-222222222222",
		[]TaskMessageData{{Seq: 1, Type: "text", Content: "changed"}})
	if !isTaskMessageBatchConflict(err) {
		t.Fatalf("want conflict error, got %v", err)
	}
	if got := sends.Load(); got != 1 {
		t.Fatalf("conflict resent %d times, want 1", got)
	}
}

// TestClientTaskMessageBatch_TransientRetryThenExactEcho: resend after 503
// succeeds when the retry gets the exact receipt.
func TestClientTaskMessageBatch_TransientRetryThenExactEcho(t *testing.T) {
	stubBatchRetrySleep(t)
	var sends atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			BatchID string `json:"batch_id"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		if sends.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprintf(w, `{"status":"ok","batch_id":%q}`, body.BatchID)
	}))
	defer srv.Close()
	batchID := "33333333-3333-3333-3333-333333333333"
	if err := NewClient(srv.URL).ReportTaskMessageBatch(context.Background(), "task", batchID,
		[]TaskMessageData{{Seq: 1, Type: "text", Content: "x"}}); err != nil {
		t.Fatal(err)
	}
	if got := sends.Load(); got != 2 {
		t.Fatalf("sent %d times, want 2", got)
	}
}

// TestClientTaskMessageCapabilities parses the server capability contract and
// treats a missing endpoint (old server) as unsupported.
func TestClientTaskMessageCapabilities(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body string
		code int
		want bool
	}{
		{"supported", `{"batch_receipts":true}`, 200, true},
		{"unsupported", `{"batch_receipts":false}`, 200, false},
		{"missing endpoint (old server)", `{"error":"not found"}`, 404, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			got, err := NewClient(srv.URL).TaskMessageCapabilities(context.Background(), "task")
			if err != nil && tc.code == 200 {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("capabilities = %v, want %v (err=%v)", got, tc.want, err)
			}
		})
	}
}

// batchProducerHarness drives executeAndDrain against a fake server that
// answers the capability GET and forwards identified POSTs to respondBatch
// with the parsed batch_id, raw body, and 1-based attempt number.
func batchProducerHarness(t *testing.T, respondBatch func(w http.ResponseWriter, batchID string, raw []byte, attempt int)) (*Daemon, string) {
	t.Helper()
	root := t.TempDir()
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/capabilities") {
			_, _ = io.WriteString(w, `{"batch_receipts":true}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		attempt := len(bodies)
		mu.Unlock()
		var parsed struct {
			BatchID string `json:"batch_id"`
		}
		_ = json.Unmarshal(raw, &parsed)
		respondBatch(w, parsed.BatchID, raw, attempt)
	}))
	t.Cleanup(srv.Close)
	return &Daemon{cfg: Config{WorkspacesRoot: root}, client: NewClient(srv.URL)}, root
}

func drainOneMessage(d *Daemon, taskID, content string) error {
	messages := make(chan agent.Message, 1)
	messages <- agent.Message{Type: agent.MessageText, Content: content}
	close(messages)
	results := make(chan agent.Result, 1)
	results <- agent.Result{Status: "completed"}
	close(results)
	backend := sessionBackend{session: &agent.Session{Messages: messages, Result: results}}
	_, _, err := d.executeAndDrain(context.Background(), backend, "p", agent.ExecOptions{},
		slog.Default(), taskID, "", new(atomic.Int32))
	return err
}

// TestExecuteAndDrain_IdentifiedBatchRetriesExactPayloadAndAcks: with the
// capability present, a 503 is resent byte-identically and the capture file is
// removed only after the exact receipt.
func TestExecuteAndDrain_IdentifiedBatchRetriesExactPayloadAndAcks(t *testing.T) {
	stubBatchRetrySleep(t)
	var bodies []string
	var mu sync.Mutex
	d, root := batchProducerHarness(t, func(w http.ResponseWriter, batchID string, raw []byte, attempt int) {
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		if attempt == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprintf(w, `{"status":"ok","batch_id":%q}`, batchID)
	})
	if err := drainOneMessage(d, "task-identified", "retry me exactly"); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("identified batch sent %d times, want 2 (503 then exact replay)", len(bodies))
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("resend was not byte-identical: %q vs %q", bodies[0], bodies[1])
	}
	if !strings.Contains(bodies[0], `"batch_id":"`) {
		t.Fatalf("identified batch missing batch_id: %s", bodies[0])
	}
	files, _ := filepath.Glob(filepath.Join(root, ".task-message-spool", "v1", "*", "*.json"))
	if len(files) != 0 {
		t.Fatalf("acknowledged batch capture retained: %d files", len(files))
	}
}

// TestExecuteAndDrain_IdentifiedBatchConflictRetainsCapture: a 409 means the
// server holds a different payload under this batch_id. One attempt, capture
// retained for diagnosis.
func TestExecuteAndDrain_IdentifiedBatchConflictRetainsCapture(t *testing.T) {
	stubBatchRetrySleep(t)
	var sends atomic.Int32
	d, root := batchProducerHarness(t, func(w http.ResponseWriter, _ string, _ []byte, _ int) {
		sends.Add(1)
		w.WriteHeader(http.StatusConflict)
	})
	if err := drainOneMessage(d, "task-conflict", "conflicting"); err != nil {
		t.Fatal(err)
	}
	if got := sends.Load(); got != 1 {
		t.Fatalf("conflict resent %d times, want 1", got)
	}
	files, _ := filepath.Glob(filepath.Join(root, ".task-message-spool", "v1", "*", "*.json"))
	if len(files) != 1 {
		t.Fatalf("conflicted capture not retained: %d files", len(files))
	}
}

// TestExecuteAndDrain_IdentifiedBatchMalformedReceiptRetainsCapture: a 200
// receipt that does not echo the batch_id is not a commit proof; the capture
// must survive.
func TestExecuteAndDrain_IdentifiedBatchMalformedReceiptRetainsCapture(t *testing.T) {
	stubBatchRetrySleep(t)
	d, root := batchProducerHarness(t, func(w http.ResponseWriter, _ string, _ []byte, _ int) {
		_, _ = io.WriteString(w, `{"status":"ok","batch_id":"00000000-0000-0000-0000-000000000000"}`)
	})
	if err := drainOneMessage(d, "task-malformed", "malformed receipt"); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(root, ".task-message-spool", "v1", "*", "*.json"))
	if len(files) != 1 {
		t.Fatalf("unproven capture not retained: %d files", len(files))
	}
}

// TestExecuteAndDrain_CapabilityUnavailableKeepsLegacyOneShot: old server (no
// capabilities endpoint) → legacy POST without batch_id, single attempt even
// on failure, capture retained and marked legacy_one_shot.
func TestExecuteAndDrain_CapabilityUnavailableKeepsLegacyOneShot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	var posts, capCalls atomic.Int32
	var sawBatchID atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/capabilities") {
			capCalls.Add(1)
			http.NotFound(w, r)
			return
		}
		posts.Add(1)
		body, _ := io.ReadAll(r.Body)
		sawBatchID.Store(strings.Contains(string(body), `"batch_id"`))
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	d := &Daemon{cfg: Config{WorkspacesRoot: root}, client: NewClient(srv.URL)}
	if err := drainOneMessage(d, "task-legacy", "legacy path"); err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 1 || capCalls.Load() == 0 || sawBatchID.Load() {
		t.Fatalf("legacy fallback broken: posts=%d capCalls=%d sawBatchID=%v", posts.Load(), capCalls.Load(), sawBatchID.Load())
	}
	files, _ := filepath.Glob(filepath.Join(root, ".task-message-spool", "v1", "*", "*.json"))
	if len(files) != 1 {
		t.Fatalf("failed legacy capture not retained: %d files", len(files))
	}
	body, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"legacy_one_shot":true`) {
		t.Fatalf("legacy capture not marked LegacyOneShot: %s", body)
	}
}
