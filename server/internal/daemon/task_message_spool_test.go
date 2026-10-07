package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/agent"
)

// This is the actual producer boundary, not a retrying imitation of the server.
// A 5xx can be an ambiguous commit, so preserving output must not resend it.
func TestExecuteAndDrain_PreservesFailedBatchWithoutResending(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/capabilities") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	root := t.TempDir()
	d := &Daemon{cfg: Config{WorkspacesRoot: root}, client: NewClient(srv.URL)}
	messages := make(chan agent.Message, 1)
	messages <- agent.Message{Type: agent.MessageText, Content: "output before attach"}
	close(messages)
	results := make(chan agent.Result, 1)
	results <- agent.Result{Status: "completed"}
	close(results)
	backend := sessionBackend{session: &agent.Session{Messages: messages, Result: results}}
	if _, _, err := d.executeAndDrain(context.Background(), backend, "p", agent.ExecOptions{},
		slog.Default(), "task-failed-batch", "", new(atomic.Int32)); err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("ambiguous batch was sent %d times, want one", got)
	}
	files, err := filepath.Glob(filepath.Join(root, ".task-message-spool", "v1", "*", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("failed batch was lost: found %d local capture files, want one", len(files))
	}
	body, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var captured struct {
		TaskID   string            `json:"task_id"`
		Messages []TaskMessageData `json:"messages"`
	}
	if err := json.Unmarshal(body, &captured); err != nil {
		t.Fatal(err)
	}
	if captured.TaskID != "task-failed-batch" || len(captured.Messages) != 1 ||
		captured.Messages[0].Seq != 1 || captured.Messages[0].Content != "output before attach" {
		t.Fatalf("capture lost task/sequence/payload: %+v", captured)
	}
}

func TestExecuteAndDrain_CaptureBeforeLegacySend(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"success", "lost_reply"} {
		t.Run(outcome, func(t *testing.T) {
			root := t.TempDir()
			var requests atomic.Int32
			observed := make(chan error, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/capabilities") {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				requests.Add(1)
				files, err := filepath.Glob(filepath.Join(root, ".task-message-spool", "v1", "*", "*.json"))
				if err == nil && len(files) != 1 {
					err = fmt.Errorf("send began before capture: found %d files", len(files))
				}
				if err == nil {
					var record capturedTaskMessageBatch
					record, err = readCapturedTaskMessageBatch(files[0])
					if err == nil && !record.LegacyOneShot {
						err = fmt.Errorf("legacy capture was incorrectly made replayable")
					}
				}
				observed <- err
				if outcome == "lost_reply" {
					// Model a receiver that applied the request but lost its reply.
					// The producer cannot distinguish this from a pre-commit failure.
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						_ = conn.Close()
					}
					return
				}
				_, _ = io.WriteString(w, `{"status":"ok"}`)
			}))
			defer srv.Close()
			d := &Daemon{cfg: Config{WorkspacesRoot: root}, client: NewClient(srv.URL)}
			messages := make(chan agent.Message, 1)
			messages <- agent.Message{Type: agent.MessageText, Content: "captured before HTTP"}
			close(messages)
			results := make(chan agent.Result, 1)
			results <- agent.Result{Status: "completed"}
			backend := sessionBackend{session: &agent.Session{Messages: messages, Result: results}}
			if _, _, err := d.executeAndDrain(t.Context(), backend, "p", agent.ExecOptions{},
				slog.Default(), "task-one-shot", "", new(atomic.Int32)); err != nil {
				t.Fatal(err)
			}
			if err := <-observed; err != nil {
				t.Fatal(err)
			}
			if requests.Load() != 1 {
				t.Fatalf("must never retry legacy request, got %d requests", requests.Load())
			}
			files, _ := filepath.Glob(filepath.Join(root, ".task-message-spool", "v1", "*", "*.json"))
			want := 0
			if outcome == "lost_reply" {
				want = 1
			}
			if len(files) != want {
				t.Fatalf("retained %d captures, want %d for %s", len(files), want, outcome)
			}
		})
	}
}

func TestTaskMessageSpool_PrivateRedactedCaptureSurvivesReopen(t *testing.T) {
	t.Parallel()
	cfg := Config{WorkspacesRoot: t.TempDir(), ServerBaseURL: "https://fixture.invalid", DaemonID: "daemon", Profile: "test"}
	s := newTaskMessageSpool(cfg, "task/../../identity", "known-incarnation", true)
	secret := "ghp_" + strings.Repeat("x", 36)
	input := map[string]any{"changes": []any{map[string]any{"content": secret + "\x00"}}, "large_integer": int64(1<<60) + 3}
	messages := []TaskMessageData{{Seq: 42, Type: "tool_use", Content: secret + "\x00", Output: secret + "\x00",
		Input: input, CreatedAt: time.Now().UTC()}}
	record, err := s.capture(messages)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.dir, record.BatchID+".json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), secret) || strings.Contains(string(body), `\u0000`) {
		t.Fatal("secret or NUL survived local capture")
	}
	if messages[0].Content != secret+"\x00" || input["changes"].([]any)[0].(map[string]any)["content"] != secret+"\x00" {
		t.Fatal("capture mutated producer-owned input")
	}
	if runtime.GOOS != "windows" {
		for _, item := range []struct {
			path string
			mode os.FileMode
		}{{s.dir, 0o700}, {path, 0o600}} {
			info, err := os.Stat(item.path)
			if err != nil || info.Mode().Perm() != item.mode {
				t.Fatalf("private mode for %s: info=%v error=%v", item.path, info, err)
			}
		}
	}
	reopened := newTaskMessageSpool(cfg, s.taskID, "next-incarnation", true)
	if reopened.dir != s.dir || reopened.captureID == s.captureID {
		t.Fatal("reopen lost namespace isolation or reused local execution identity")
	}
	read, err := readCapturedTaskMessageBatch(path)
	if err != nil {
		t.Fatal(err)
	}
	if read.BatchID != record.BatchID || read.Digest != record.Digest || read.CaptureID != s.captureID ||
		read.IncarnationID != "known-incarnation" || read.Messages[0].Seq != 42 || !read.LegacyOneShot {
		t.Fatalf("reopen changed identity/sequence/delivery semantics: %+v", read)
	}
	if err := reopened.acknowledge(record); err == nil {
		t.Fatal("new execution acknowledged old execution's capture")
	}
	wrong := record
	wrong.TaskID = "other-task"
	if err := s.acknowledge(wrong); err == nil {
		t.Fatal("wrong task acknowledged capture")
	}
	wrong = record
	wrong.Messages = append([]TaskMessageData(nil), record.Messages...)
	wrong.Messages[0].Content = "changed payload"
	if err := s.acknowledge(wrong); err == nil {
		t.Fatal("changed payload acknowledged capture")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("invalid acknowledgement deleted retained capture")
	}
	if err := s.acknowledge(record); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("successful local acknowledgement did not remove capture: %v", err)
	}
	otherServer := cfg
	otherServer.ServerBaseURL = "https://another-fixture.invalid"
	if newTaskMessageSpool(otherServer, s.taskID, "", true).dir == s.dir {
		t.Fatal("another server reused capture namespace")
	}
}

func TestTaskMessageSpool_CorruptionIsVisibleAndRetained(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"truncated", "changed", "future_version"} {
		t.Run(kind, func(t *testing.T) {
			s := newTaskMessageSpool(Config{WorkspacesRoot: t.TempDir()}, "task", "", true)
			record, err := s.capture([]TaskMessageData{{Seq: 1, Type: "text", Content: "original"}})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(s.dir, record.BatchID+".json")
			var body []byte
			switch kind {
			case "truncated":
				body = []byte(`{"version":1`)
			case "changed":
				record.Messages[0].Content = "different"
				body, _ = json.Marshal(record)
			case "future_version":
				record.Version++
				body, _ = json.Marshal(record)
			}
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readCapturedTaskMessageBatch(path); err == nil {
				t.Fatalf("%s capture was silently accepted", kind)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("corrupt/unsupported capture was discarded")
			}
		})
	}
}

func TestTaskMessageSpool_QuotaRejectsWithoutEvicting(t *testing.T) {
	t.Parallel()
	s := newTaskMessageSpool(Config{WorkspacesRoot: t.TempDir()}, "task", "", true)
	record, err := s.capture([]TaskMessageData{{Seq: 1, Type: "text", Content: "must retain"}})
	if err != nil {
		t.Fatal(err)
	}
	// A sparse file reaches the real byte quota without a large test payload.
	quotaPath := filepath.Join(s.dir, "interrupted.tmp")
	f, err := os.Create(quotaPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(taskMessageSpoolMaxBytes); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := s.capture([]TaskMessageData{{Seq: 2, Type: "text", Content: "over quota"}}); err == nil {
		t.Fatal("quota-full capture silently accepted")
	}
	if _, err := readCapturedTaskMessageBatch(filepath.Join(s.dir, record.BatchID+".json")); err != nil {
		t.Fatal("quota failure evicted existing evidence")
	}
	if _, err := s.capture([]TaskMessageData{{Seq: 2, Type: "text", Content: strings.Repeat("x", taskMessageSpoolMaxBatchBytes)}}); err == nil {
		t.Fatal("oversized batch silently accepted")
	}
}

func TestTaskMessageSpool_RejectsUnavailableDirectoryAndInvalidSequence(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".task-message-spool"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newTaskMessageSpool(Config{WorkspacesRoot: root}, "task", "", true)
	if _, err := s.capture([]TaskMessageData{{Seq: 1, Type: "text"}}); err == nil {
		t.Fatal("unavailable disk capture silently accepted")
	}
	for _, seqs := range [][]int{{0}, {2, 1}, {1, 1}} {
		messages := make([]TaskMessageData, len(seqs))
		for i, seq := range seqs {
			messages[i] = TaskMessageData{Seq: seq, Type: "text"}
		}
		if _, err := s.capture(messages); err == nil {
			t.Fatalf("invalid producer sequence accepted: %v", seqs)
		}
	}
}

func TestExecuteAndDrain_LaterOutputDoesNotResendFailedBatch(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	reported := make(chan []TaskMessageData, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/capabilities") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			Messages []TaskMessageData `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		reported <- body.Messages
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	cfg := Config{WorkspacesRoot: t.TempDir()}
	d := &Daemon{cfg: cfg, client: NewClient(srv.URL)}
	var seq atomic.Int32
	for _, text := range []string{"failed first", "new output"} {
		messages := make(chan agent.Message, 1)
		messages <- agent.Message{Type: agent.MessageText, Content: text}
		close(messages)
		results := make(chan agent.Result, 1)
		results <- agent.Result{Status: "completed"}
		backend := sessionBackend{session: &agent.Session{Messages: messages, Result: results}}
		if _, _, err := d.executeAndDrain(t.Context(), backend, "p", agent.ExecOptions{},
			slog.Default(), "task", "", &seq); err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 2 {
		t.Fatalf("new output retried legacy batch: %d requests", requests.Load())
	}
	for i := 1; i <= 2; i++ {
		if messages := <-reported; len(messages) != 1 || messages[0].Seq != i {
			t.Fatalf("request %d included wrong/duplicate sequence: %+v", i, messages)
		}
	}
	s := newTaskMessageSpool(cfg, "task", "", true)
	files, _ := filepath.Glob(filepath.Join(s.dir, "*.json"))
	if len(files) != 1 {
		t.Fatalf("later success deleted failed capture: %d files", len(files))
	}
	record, err := readCapturedTaskMessageBatch(files[0])
	if err != nil || record.Messages[0].Seq != 1 {
		t.Fatalf("wrong retained batch: %+v, error=%v", record, err)
	}
}

func TestExecuteAndDrain_CaptureFailureIsExplicitAndKeepsLegacySend(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".task-message-spool"), []byte("blocked disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/capabilities") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		requests.Add(1)
	}))
	defer srv.Close()
	d := &Daemon{cfg: Config{WorkspacesRoot: root}, client: NewClient(srv.URL)}
	var logs bytes.Buffer
	messages := make(chan agent.Message, 1)
	messages <- agent.Message{Type: agent.MessageText, Content: "output"}
	close(messages)
	results := make(chan agent.Result, 1)
	results <- agent.Result{Status: "completed"}
	backend := sessionBackend{session: &agent.Session{Messages: messages, Result: results}}
	if _, _, err := d.executeAndDrain(t.Context(), backend, "p", agent.ExecOptions{},
		slog.New(slog.NewTextHandler(&logs, nil)), "task", "", new(atomic.Int32)); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 || !strings.Contains(logs.String(), "this batch is not durably retained") ||
		!strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("capture failure must be explicit and preserve legacy one-shot send: requests=%d logs=%s", requests.Load(), logs.String())
	}
}

// TestExecuteAndDrain_CaptureFailureLargeBatchDeliveredOnce: when capture
// fails on a large batch, the fallback one-shot must deliver the ORIGINAL
// messages (no truncation/drop) exactly once and complete promptly — the
// transport handler is authoritative for redaction, so the producer must not
// re-run sanitizeTaskMessageBatch (which stalls on oversized output). No
// claim here about server-side redaction of giant payloads; the fake
// transport accepts whatever arrives.
func TestExecuteAndDrain_CaptureFailureLargeBatchDeliveredOnce(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".task-message-spool"), []byte("blocked disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	var mu sync.Mutex
	var received []TaskMessageData
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/capabilities") {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Messages []TaskMessageData `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode large fallback batch: %v", err)
		}
		mu.Lock()
		received = body.Messages
		mu.Unlock()
		requests.Add(1)
		close(done)
	}))
	defer srv.Close()
	d := &Daemon{cfg: Config{WorkspacesRoot: root}, client: NewClient(srv.URL)}
	// Large but under the 8 MiB admission ceiling: capture fails on the blocked
	// spool directory, and the fallback must still deliver it whole. Sized so
	// the -race build's JSON cost stays far inside the 60s bound.
	large := strings.Repeat("x", 1<<20)
	messages := make(chan agent.Message, 1)
	messages <- agent.Message{Type: agent.MessageText, Content: large}
	close(messages)
	results := make(chan agent.Result, 1)
	results <- agent.Result{Status: "completed"}
	close(results)
	backend := sessionBackend{session: &agent.Session{Messages: messages, Result: results}}
	finished := make(chan error, 1)
	go func() {
		_, _, err := d.executeAndDrain(context.Background(), backend, "p", agent.ExecOptions{},
			slog.Default(), "task-large-fallback", "", new(atomic.Int32))
		finished <- err
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("large capture-failure batch not delivered within 60s (fallback stalled)")
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("fallback delivered %d times, want exactly 1 (never-retry)", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 || received[0].Content != large {
		t.Fatalf("original large message truncated/dropped/changed: got %d messages, content len %d want %d",
			len(received), len(received[0].Content), len(large))
	}
}
