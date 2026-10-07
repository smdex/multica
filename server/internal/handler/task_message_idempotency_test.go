package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestTaskMessageBatchIdentityUsesDecodedFields(t *testing.T) {
	batchID := uuid.NewString()
	payloads := []string{
		`{"batch_id":"` + batchID + `","messages":[{"seq":1,"type":"text","content":"original","input":{"a":1,"b":2}}]}`,
		`{ "messages": [ { "input": { "b": 2, "a": 1 }, "content": "original", "type": "text", "seq": 1 } ], "batch_id": "` + batchID + `" }`,
	}
	var expected string
	for _, payload := range payloads {
		var req TaskMessageBatchRequest
		if err := json.Unmarshal([]byte(payload), &req); err != nil {
			t.Fatal(err)
		}
		_, hash, err := taskMessageBatchIdentity(req)
		if err != nil {
			t.Fatal(err)
		}
		if expected == "" {
			expected = hash.String
		} else if hash.String != expected {
			t.Fatal("equivalent decoded payloads produced different receipt hashes")
		}
		req.Messages[0].Content = "changed"
		_, changed, err := taskMessageBatchIdentity(req)
		if err != nil || changed.String == expected {
			t.Fatalf("changed logical content must change receipt hash: %v", err)
		}
	}
}

func identifiedBatchRequest(t *testing.T, taskID, batchID string, messages []any) *http.Request {
	t.Helper()
	req := testutil.JSONRequest(http.MethodPost, "/api/daemon/tasks/"+taskID+"/messages", map[string]any{
		"batch_id": batchID,
		"messages": messages,
	})
	req = testutil.WithURLParams(req, "taskId", taskID)
	return req.WithContext(middleware.WithDaemonContext(req.Context(), testWorkspaceID, "batch-messages-daemon"))
}

func TestReportTaskMessagesIdentifiedBatchReplay(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	taskID := seedBatchTask(t, "identified-batch")
	batchID := uuid.NewString()
	messages := []any{map[string]any{"seq": 1, "type": "text", "content": "retained output"}}
	sharedBus := testHandler.Bus
	testHandler.Bus = events.New()
	t.Cleanup(func() { testHandler.Bus = sharedBus })
	published := 0
	testHandler.Bus.Subscribe(protocol.EventTaskMessage, func(e events.Event) {
		if e.TaskID == taskID {
			published++
		}
	})
	for range 2 {
		var response map[string]any
		testutil.Call(t, testHandler.ReportTaskMessages, identifiedBatchRequest(t, taskID, batchID, messages)).Want(http.StatusOK).JSON(&response)
		if response["batch_id"] != batchID {
			t.Fatalf("batch receipt = %#v, want batch_id %q", response, batchID)
		}
	}
	stored, err := testHandler.Queries.ListTaskMessages(context.Background(), util.MustParseUUID(taskID))
	if err != nil || len(stored) != 1 {
		t.Fatalf("after replay: rows=%d err=%v, want one persisted record", len(stored), err)
	}
	if published != 1 {
		t.Fatalf("replay published %d events, want exactly one", published)
	}
	changed := []any{map[string]any{"seq": 1, "type": "text", "content": "different output"}}
	testutil.Call(t, testHandler.ReportTaskMessages, identifiedBatchRequest(t, taskID, batchID, changed)).Want(http.StatusConflict)
}

func TestReportTaskMessagesIdentifiedBatchConcurrentReplay(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	taskID := seedBatchTask(t, "concurrent-batch")
	batchID := uuid.NewString()
	messages := []any{map[string]any{"seq": 1, "type": "text", "content": "once"}}
	var wg sync.WaitGroup
	statuses := make(chan int, 4)
	for range 4 {
		req := identifiedBatchRequest(t, taskID, batchID, messages)
		wg.Add(1)
		go func() {
			defer wg.Done()
			recorder := httptest.NewRecorder()
			testHandler.ReportTaskMessages(recorder, req)
			statuses <- recorder.Code
		}()
	}
	wg.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusOK {
			t.Errorf("concurrent replay status = %d, want 200", status)
		}
	}
	stored, err := testHandler.Queries.ListTaskMessages(context.Background(), util.MustParseUUID(taskID))
	if err != nil || len(stored) != 1 {
		t.Fatalf("concurrent replay: rows=%d err=%v, want one persisted record", len(stored), err)
	}
}

func TestReportTaskMessagesIdentifiedBatchTaskScope(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	batchID := uuid.NewString()
	for _, label := range []string{"batch-scope-a", "batch-scope-b"} {
		taskID := seedBatchTask(t, label)
		testutil.Call(t, testHandler.ReportTaskMessages, identifiedBatchRequest(t, taskID, batchID,
			[]any{map[string]any{"seq": 1, "type": "text", "content": label}})).Want(http.StatusOK)
	}
}

// A transport fault hides the first committed receipt. Both attempts use the
// actual daemon client, daemon-token middleware, handler and database query.
// This is transport integration coverage, not complete browser/swarm E2E.
func TestReportTaskMessagesLostReceiptThroughRealClient(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	taskID := seedBatchTask(t, "lost-receipt-http")
	token, err := auth.GenerateDaemonToken()
	if err != nil {
		t.Fatal(err)
	}
	dbfx.Insert(t, "daemon_token", testutil.Cols{
		"token_hash": auth.HashToken(token), "workspace_id": testWorkspaceID,
		"daemon_id": "batch-messages-daemon", "expires_at": testutil.Raw("now() + interval '1 hour'"),
	})
	var posts atomic.Int32
	var committedBeforeRetry atomic.Bool
	router := chi.NewRouter()
	router.Use(middleware.DaemonAuth(testHandler.Queries, nil, nil, nil))
	router.Get("/api/daemon/tasks/{taskId}/messages/capabilities", testHandler.TaskMessageCapabilities)
	router.Post("/api/daemon/tasks/{taskId}/messages", func(w http.ResponseWriter, r *http.Request) {
		if posts.Add(1) == 1 {
			recorder := httptest.NewRecorder()
			testHandler.ReportTaskMessages(recorder, r)
			rows, err := testHandler.Queries.ListTaskMessages(r.Context(), util.MustParseUUID(taskID))
			committedBeforeRetry.Store(recorder.Code == http.StatusOK && err == nil && len(rows) == 1)
			// Deliberately hide the successful receipt after its database commit.
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		testHandler.ReportTaskMessages(w, r)
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	client := daemon.NewClient(server.URL)
	client.SetToken(token)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	capable, err := client.TaskMessageCapabilities(ctx, taskID)
	if err != nil || !capable {
		t.Fatalf("receipt capability = %t, err=%v", capable, err)
	}
	batchID := uuid.NewString()
	if err := client.ReportTaskMessageBatch(ctx, taskID, batchID, []daemon.TaskMessageData{
		{Seq: 1, Type: "text", Content: "persisted before receipt loss", CreatedAt: time.Now().UTC()},
	}); err != nil {
		t.Fatalf("retry after hidden committed receipt: %v", err)
	}
	if !committedBeforeRetry.Load() || posts.Load() != 2 {
		t.Fatalf("commit before retry=%t, HTTP posts=%d, want committed first attempt and exactly two posts",
			committedBeforeRetry.Load(), posts.Load())
	}
	rows, err := testHandler.Queries.ListTaskMessages(ctx, util.MustParseUUID(taskID))
	if err != nil || len(rows) != 1 {
		t.Fatalf("real HTTP replay: rows=%d err=%v, want exactly one", len(rows), err)
	}
	if uuidToString(rows[0].BatchID) != batchID {
		t.Fatal("persisted message lost its receipt identity")
	}
}

func TestTaskMessageCapabilitiesAuthorization(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	taskID := seedBatchTask(t, "batch-capability")
	req := testutil.WithURLParams(httptest.NewRequest(http.MethodGet, "/messages/capabilities", nil), "taskId", taskID)
	var response map[string]bool
	testutil.Call(t, testHandler.TaskMessageCapabilities, req.WithContext(
		middleware.WithDaemonContext(req.Context(), testWorkspaceID, "batch-messages-daemon"))).Want(http.StatusOK).JSON(&response)
	if !response["batch_receipts"] {
		t.Fatal("authorized capability did not advertise receipts")
	}
	testutil.Call(t, testHandler.TaskMessageCapabilities, req.WithContext(
		middleware.WithDaemonContext(req.Context(), uuid.NewString(), "other-daemon"))).Want(http.StatusNotFound)
	testutil.Call(t, testHandler.TaskMessageCapabilities, req).Want(http.StatusUnauthorized)
}

func TestReportTaskMessagesIdentifiedBatchValidation(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	taskID := seedBatchTask(t, "invalid-batch")
	for _, tc := range []struct {
		name     string
		id       string
		messages []any
	}{
		{"invalid UUID", "bad-id", []any{map[string]any{"seq": 1, "type": "text"}}},
		{"empty batch", uuid.NewString(), nil},
		{"duplicate sequence", uuid.NewString(), []any{map[string]any{"seq": 1, "type": "text"}, map[string]any{"seq": 1, "type": "text"}}},
		{"negative sequence", uuid.NewString(), []any{map[string]any{"seq": -1, "type": "text"}}},
		{"sequence overflow", uuid.NewString(), []any{map[string]any{"seq": int64(1) << 32, "type": "text"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testutil.Call(t, testHandler.ReportTaskMessages, identifiedBatchRequest(t, taskID, tc.id, tc.messages)).Want(http.StatusBadRequest)
		})
	}
}
