package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	srcRuntimeID  = "11111111-1111-4111-8111-111111111111"
	srcWorkspace  = "22222222-2222-4222-8222-222222222222"
	srcDaemonID   = "daemon-opaque-7f3k2!!zz"
	srcCommandID  = "44444444-4444-4444-8444-444444444444"
	srcRequestID  = "55555555-5555-4555-8555-555555555555"
	srcSourceID   = "66666666-6666-4666-8666-666666666666"
	srcHumanToken = "mul_human_test_token"
	srcMSRToken   = "msr_test_capability_token"
)

// newSourceReadTestClient spins up a test-created server capturing the mint
// request and answering a valid capability.
func newSourceReadTestClient(t *testing.T) (*Client, *http.ServeMux) {
	t.Helper()
	mux := http.NewServeMux()
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return NewClient(ts.URL), mux
}

func validMintBody(now time.Time) map[string]any {
	expiresAt := now.Add(90 * time.Second)
	return map[string]any{
		"token":        srcMSRToken,
		"token_type":   "Bearer",
		"scope":        "source:read",
		"runtime_id":   srcRuntimeID,
		"workspace_id": srcWorkspace,
		"daemon_id":    srcDaemonID,
		"expires_at":   expiresAt.UTC().Format(time.RFC3339),
		"expires_in":   90,
	}
}

func TestSourceReadClientTokenResponseLatency(t *testing.T) {
	issued := time.Date(2026, time.October, 7, 12, 0, 0, 0, time.UTC)
	resp := sourceReadTokenResponse{
		Token: srcMSRToken, TokenType: "Bearer", Scope: sourceReadScope,
		RuntimeID: srcRuntimeID, WorkspaceID: srcWorkspace, DaemonID: srcDaemonID,
		ExpiresAt: issued.Add(90 * time.Second).Format(time.RFC3339), ExpiresIn: 90,
	}
	if err := validateSourceReadToken(&resp, srcRuntimeID, srcWorkspace, srcDaemonID, issued, issued.Add(10*time.Second)); err != nil {
		t.Fatalf("a still-valid delayed capability response was rejected: %v", err)
	}
	for _, expiresIn := range []int64{1, 120, 121} {
		resp.ExpiresIn = expiresIn
		if err := validateSourceReadToken(&resp, srcRuntimeID, srcWorkspace, srcDaemonID, issued, issued.Add(10*time.Second)); err == nil {
			t.Fatalf("inconsistent mint lifetime %d was accepted", expiresIn)
		}
	}
}

func pendingCommandJSON() map[string]any {
	return map[string]any{
		"id": srcCommandID, "request_id": srcRequestID, "workspace_id": srcWorkspace,
		"source_id": srcSourceID, "source_handle": "beads-main", "command": "list",
		"native_id": "", "limit_count": 50, "config_revision": 3,
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"status":     "pending", "created_at": time.Now().UTC().Format(time.RFC3339),
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	}
}

func receiptJSON(status string) map[string]any {
	m := pendingCommandJSON()
	delete(m, "source_handle")
	m["status"] = status
	if status == "claimed" || status == "succeeded" || status == "failed" {
		m["claimed_runtime_id"] = srcRuntimeID
	}
	return m
}

func TestExchangeSourceReadTokenContract(t *testing.T) {
	client, mux := newSourceReadTestClient(t)
	var gotAuth, gotBody, gotMethod, gotPath string
	mux.HandleFunc("/api/daemon/runtimes/"+srcRuntimeID+"/source-read-token", func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		raw, _ := json.Marshal(body)
		gotBody = string(raw)
		json.NewEncoder(w).Encode(validMintBody(time.Now()))
	})
	client.SetToken(srcHumanToken)

	cred, err := client.ExchangeSourceReadToken(context.Background(), srcRuntimeID, srcWorkspace, srcDaemonID)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/api/daemon/runtimes/"+srcRuntimeID+"/source-read-token" {
		t.Errorf("path = %s", gotPath)
	}
	if gotAuth != "Bearer "+srcHumanToken {
		t.Errorf("parent auth = %q, want human token", gotAuth)
	}
	if gotBody != `{"scope":"source:read"}` {
		t.Errorf("body = %s", gotBody)
	}
	if cred.Token != srcMSRToken || !cred.ExpiresAt.After(time.Now()) || cred.ExpiresIn <= 0 {
		t.Errorf("credential = %+v", cred)
	}
	if client.Token() != srcHumanToken {
		t.Fatalf("client token was mutated to %q", client.Token())
	}
}

func TestExchangeSourceReadTokenAcceptsOpaqueDaemonID(t *testing.T) {
	client, mux := newSourceReadTestClient(t)
	mux.HandleFunc("/api/daemon/runtimes/"+srcRuntimeID+"/source-read-token", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(validMintBody(time.Now()))
	})
	// Daemon IDs are opaque server-issued strings (JWT `sub`, PAT labels, ...)
	// and must never be UUID-validated at this boundary.
	if _, err := client.ExchangeSourceReadToken(context.Background(), srcRuntimeID, srcWorkspace, srcDaemonID); err != nil {
		t.Fatalf("opaque daemon id rejected: %v", err)
	}
	if _, err := client.ExchangeSourceReadToken(context.Background(), srcRuntimeID, srcWorkspace, "   "); err == nil {
		t.Fatal("blank daemon id accepted")
	}
}

func TestExchangeSourceReadTokenMalformedMatrix(t *testing.T) {
	cases := map[string]func(m map[string]any){
		"empty suffix":            func(m map[string]any) { m["token"] = "msr_" },
		"overflow expires_in":     func(m map[string]any) { m["expires_in"] = int64(9223372036854775807) },
		"missing msr prefix":      func(m map[string]any) { m["token"] = "not_msr" },
		"wrong token type":        func(m map[string]any) { m["token_type"] = "Basic" },
		"wrong scope":             func(m map[string]any) { m["scope"] = "source:write" },
		"runtime echo mismatch":   func(m map[string]any) { m["runtime_id"] = srcWorkspace },
		"workspace echo mismatch": func(m map[string]any) { m["workspace_id"] = srcRuntimeID },
		"daemon echo mismatch":    func(m map[string]any) { m["daemon_id"] = srcRuntimeID },
		"expired": func(m map[string]any) {
			m["expires_at"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
			m["expires_in"] = -60
		},
		"ttl over 120s": func(m map[string]any) {
			m["expires_at"] = time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)
			m["expires_in"] = 600
		},
		"zero expires_in":      func(m map[string]any) { m["expires_in"] = 0 },
		"malformed expires_at": func(m map[string]any) { m["expires_at"] = "soon" },
		"expires disagreement": func(m map[string]any) {
			m["expires_in"] = 90
			m["expires_at"] = time.Now().Add(60 * time.Second).UTC().Format(time.RFC3339)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			client, mux := newSourceReadTestClient(t)
			mux.HandleFunc("/api/daemon/runtimes/"+srcRuntimeID+"/source-read-token", func(w http.ResponseWriter, r *http.Request) {
				m := validMintBody(time.Now())
				mutate(m)
				json.NewEncoder(w).Encode(m)
			})
			if _, err := client.ExchangeSourceReadToken(context.Background(), srcRuntimeID, srcWorkspace, srcDaemonID); err == nil {
				t.Fatalf("expected rejection for %s", name)
			}
		})
	}
}

func TestExchangeSourceReadTokenRejectsBadIDsBeforeHTTP(t *testing.T) {
	client := NewClient("http://127.0.0.1:0") // unreachable: no request may be made
	bad := []string{"", "   ", "not-a-uuid", srcRuntimeID[:35], "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA", "00000000-0000-0000-0000-000000000000"}
	for _, id := range bad {
		if _, err := client.ExchangeSourceReadToken(context.Background(), id, srcWorkspace, srcDaemonID); err == nil {
			t.Errorf("runtime id %q accepted", id)
		}
		if _, err := client.ExchangeSourceReadToken(context.Background(), srcRuntimeID, id, srcDaemonID); err == nil {
			t.Errorf("workspace id %q accepted", id)
		}
	}
	if _, err := client.ExchangeSourceReadToken(context.Background(), srcRuntimeID, srcWorkspace, ""); err == nil {
		t.Error("blank daemon id accepted")
	}
}

func TestListSourceReadCommandsContract(t *testing.T) {
	client, mux := newSourceReadTestClient(t)
	var gotAuth, gotMethod, gotPath string
	mux.HandleFunc("/api/daemon/runtimes/"+srcRuntimeID+"/work-source-commands", func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode([]map[string]any{pendingCommandJSON()})
	})
	cmds, err := client.ListSourceReadCommands(context.Background(), srcRuntimeID, srcWorkspace, srcMSRToken)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %s, want GET", gotMethod)
	}
	if gotPath != "/api/daemon/runtimes/"+srcRuntimeID+"/work-source-commands" {
		t.Errorf("path = %s", gotPath)
	}
	if gotAuth != "Bearer "+srcMSRToken {
		t.Errorf("auth = %q, want scoped msr token", gotAuth)
	}
	if len(cmds) != 1 {
		t.Fatalf("cmds = %d", len(cmds))
	}
	c := cmds[0]
	if c.ID != srcCommandID || c.WorkspaceID != srcWorkspace || c.SourceHandle != "beads-main" ||
		c.Command != "list" || c.LimitCount != 50 || c.ConfigRevision != 3 || c.Status != "pending" {
		t.Errorf("command = %+v", c)
	}
}

func TestListSourceReadCommandsMalformedMatrix(t *testing.T) {
	cases := map[string]func(m map[string]any){
		"zero UUID":              func(m map[string]any) { m["id"] = "00000000-0000-0000-0000-000000000000" },
		"uppercase UUID":         func(m map[string]any) { m["id"] = "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA" },
		"malformed expiry":       func(m map[string]any) { m["expires_at"] = "soon" },
		"expired command":        func(m map[string]any) { m["expires_at"] = time.Now().Add(-time.Hour).Format(time.RFC3339) },
		"duplicate id":           func(m map[string]any) {},
		"negative limit":         func(m map[string]any) { m["limit_count"] = -1 },
		"null":                   func(m map[string]any) { m["__null__"] = true },
		"malformed id":           func(m map[string]any) { m["id"] = "nope" },
		"blank request id":       func(m map[string]any) { m["request_id"] = "" },
		"malformed workspace":    func(m map[string]any) { m["workspace_id"] = "abc" },
		"malformed source id":    func(m map[string]any) { m["source_id"] = "42" },
		"blank source handle":    func(m map[string]any) { m["source_handle"] = "  " },
		"unknown command":        func(m map[string]any) { m["command"] = "delete" },
		"unknown status":         func(m map[string]any) { m["status"] = "weird" },
		"zero revision":          func(m map[string]any) { m["config_revision"] = 0 },
		"negative revision":      func(m map[string]any) { m["config_revision"] = -2 },
		"list with native id":    func(m map[string]any) { m["native_id"] = "abc" },
		"list limit over 200":    func(m map[string]any) { m["limit_count"] = 201 },
		"read missing native id": func(m map[string]any) { m["command"] = "read" },
		"read with limit":        func(m map[string]any) { m["command"] = "read"; m["native_id"] = "x" },
		"not pending":            func(m map[string]any) { m["status"] = "claimed" },
		"already claimed":        func(m map[string]any) { m["claimed_runtime_id"] = srcRuntimeID },
		"carries result":         func(m map[string]any) { m["result"] = "{}" },
		"carries error":          func(m map[string]any) { m["error"] = "boom" },
		"wrong workspace":        func(m map[string]any) { m["workspace_id"] = srcSourceID },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			client, mux := newSourceReadTestClient(t)
			mux.HandleFunc("/api/daemon/runtimes/"+srcRuntimeID+"/work-source-commands", func(w http.ResponseWriter, r *http.Request) {
				if name == "null" {
					w.Write([]byte("null"))
					return
				}
				m := pendingCommandJSON()
				mutate(m)
				items := []map[string]any{m}
				if name == "duplicate id" {
					items = append(items, m)
				}
				json.NewEncoder(w).Encode(items)
			})
			if _, err := client.ListSourceReadCommands(context.Background(), srcRuntimeID, srcWorkspace, srcMSRToken); err == nil {
				t.Fatalf("expected rejection for %s", name)
			}
		})
	}
}

func TestListSourceReadCommandsRejectsNonScopedToken(t *testing.T) {
	client := NewClient("http://127.0.0.1:0")
	for _, tok := range []string{"", "msr_", "msr_  ", "mul_pat", srcHumanToken} {
		if _, err := client.ListSourceReadCommands(context.Background(), srcRuntimeID, srcWorkspace, tok); err == nil {
			t.Errorf("token %q accepted", tok)
		}
	}
}

func TestClaimSourceReadCommandContract(t *testing.T) {
	client, mux := newSourceReadTestClient(t)
	var gotAuth, gotMethod, gotPath, gotBody string
	mux.HandleFunc("/api/daemon/runtimes/"+srcRuntimeID+"/work-source-commands/"+srcCommandID+"/claim", func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		raw, _ := json.Marshal(body)
		gotBody = string(raw)
		json.NewEncoder(w).Encode(receiptJSON("claimed"))
	})
	cmd, err := client.ClaimSourceReadCommand(context.Background(), srcRuntimeID, srcWorkspace, srcCommandID, srcMSRToken)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/daemon/runtimes/"+srcRuntimeID+"/work-source-commands/"+srcCommandID+"/claim" {
		t.Errorf("request = %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer "+srcMSRToken {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotBody != "{}" {
		t.Errorf("body = %s", gotBody)
	}
	if cmd.ID != srcCommandID || cmd.WorkspaceID != srcWorkspace || cmd.Status != "claimed" {
		t.Errorf("claimed command = %+v", cmd)
	}
	if cmd.SourceHandle != "" {
		t.Errorf("claim receipt must not carry source handle, got %q", cmd.SourceHandle)
	}
}

func TestClaimSourceReadCommandReceiptMismatches(t *testing.T) {
	cases := map[string]func(m map[string]any){
		"write command":     func(m map[string]any) { m["command"] = "delete" },
		"bad list params":   func(m map[string]any) { m["native_id"] = "x" },
		"limit over 200":    func(m map[string]any) { m["limit_count"] = 201 },
		"read no native id": func(m map[string]any) { m["command"] = "read"; m["limit_count"] = 0 },
		"read with limit":   func(m map[string]any) { m["command"] = "read"; m["native_id"] = "x" },
		"zero revision":     func(m map[string]any) { m["config_revision"] = 0 },
		"expired claim":     func(m map[string]any) { m["expires_at"] = time.Now().Add(-time.Hour).Format(time.RFC3339) },
		"malformed expiry":  func(m map[string]any) { m["expires_at"] = "soon" },
		"claim with result": func(m map[string]any) { m["result"] = "{}" },
		"claim with error":  func(m map[string]any) { m["error"] = "boom" },
		"wrong id":          func(m map[string]any) { m["id"] = srcRequestID },
		"wrong workspace":   func(m map[string]any) { m["workspace_id"] = srcRuntimeID },
		"blank status":      func(m map[string]any) { m["status"] = "" },
		"wrong status":      func(m map[string]any) { m["status"] = "pending" },
		"wrong claimed run": func(m map[string]any) { m["claimed_runtime_id"] = srcSourceID },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			client, mux := newSourceReadTestClient(t)
			mux.HandleFunc("/api/daemon/runtimes/"+srcRuntimeID+"/work-source-commands/"+srcCommandID+"/claim", func(w http.ResponseWriter, r *http.Request) {
				m := receiptJSON("claimed")
				mutate(m)
				json.NewEncoder(w).Encode(m)
			})
			if _, err := client.ClaimSourceReadCommand(context.Background(), srcRuntimeID, srcWorkspace, srcCommandID, srcMSRToken); err == nil {
				t.Fatalf("expected rejection for %s", name)
			}
		})
	}
}

func TestReportSourceReadCommandContract(t *testing.T) {
	resultJSON := `{"items":[],"truncated":false}`
	client, mux := newSourceReadTestClient(t)
	var gotAuth, gotMethod, gotPath string
	var gotBody map[string]any
	mux.HandleFunc("/api/daemon/runtimes/"+srcRuntimeID+"/work-source-commands/"+srcCommandID+"/result", func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&gotBody)
		m := receiptJSON("succeeded")
		m["result"] = resultJSON
		json.NewEncoder(w).Encode(m)
	})
	cmd, err := client.ReportSourceReadCommand(context.Background(), srcRuntimeID, srcWorkspace, srcCommandID, srcMSRToken, "succeeded", resultJSON, "")
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/daemon/runtimes/"+srcRuntimeID+"/work-source-commands/"+srcCommandID+"/result" {
		t.Errorf("request = %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer "+srcMSRToken {
		t.Errorf("auth = %q", gotAuth)
	}
	// result must travel as a JSON-encoded STRING, not a raw object
	if gotBody["result"] != resultJSON {
		t.Errorf("body result = %v (%T), want encoded string", gotBody["result"], gotBody["result"])
	}
	if cmd.ID != srcCommandID || cmd.Status != "succeeded" || cmd.Result != resultJSON {
		t.Errorf("receipt = %+v", cmd)
	}
}

func TestReportSourceReadCommandFailureContract(t *testing.T) {
	client, mux := newSourceReadTestClient(t)
	mux.HandleFunc("/api/daemon/runtimes/"+srcRuntimeID+"/work-source-commands/"+srcCommandID+"/result", func(w http.ResponseWriter, r *http.Request) {
		m := receiptJSON("failed")
		m["error"] = "binding missing"
		json.NewEncoder(w).Encode(m)
	})
	cmd, err := client.ReportSourceReadCommand(context.Background(), srcRuntimeID, srcWorkspace, srcCommandID, srcMSRToken, "failed", "", "binding missing")
	if err != nil {
		t.Fatalf("report failed: %v", err)
	}
	if cmd.Status != "failed" || cmd.Error != "binding missing" {
		t.Errorf("receipt = %+v", cmd)
	}
}

func TestReportSourceReadCommandRejectsInvalidInput(t *testing.T) {
	client := NewClient("http://127.0.0.1:0")
	hugeResult := `"` + strings.Repeat("x", 2*(2<<20)) + `"`
	hugeErr := strings.Repeat("x", 8<<10+1)
	cases := []struct {
		name           string
		status, result string
		diagnostic     string
	}{
		{"unknown status", "cancelled", "{}", ""},
		{"succeeded without result", "succeeded", "", ""},
		{"succeeded with raw non-json result", "succeeded", "{not json", ""},
		{"succeeded with diagnostic", "succeeded", `{}`, "why"},
		{"failed with result", "failed", "{}", "why"},
		{"result over 2MiB", "succeeded", hugeResult, ""},
		{"error over 8KiB", "failed", "", hugeErr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := client.ReportSourceReadCommand(context.Background(), srcRuntimeID, srcWorkspace, srcCommandID, srcMSRToken, tc.status, tc.result, tc.diagnostic); err == nil {
				t.Fatalf("expected rejection for %s", tc.name)
			}
		})
	}
}

func TestReportSourceReadCommandReceiptMismatch(t *testing.T) {
	client, mux := newSourceReadTestClient(t)
	mux.HandleFunc("/api/daemon/runtimes/"+srcRuntimeID+"/work-source-commands/"+srcCommandID+"/result", func(w http.ResponseWriter, r *http.Request) {
		m := receiptJSON("failed") // server says failed, caller said succeeded
		json.NewEncoder(w).Encode(m)
	})
	if _, err := client.ReportSourceReadCommand(context.Background(), srcRuntimeID, srcWorkspace, srcCommandID, srcMSRToken, "succeeded", `{}`, ""); err == nil {
		t.Fatal("expected status mismatch rejection")
	}
}

func TestReportSourceReadCommandReceiptMatrix(t *testing.T) {
	cases := map[string]func(map[string]any){
		"id":               func(m map[string]any) { m["id"] = srcRequestID },
		"workspace":        func(m map[string]any) { m["workspace_id"] = srcRuntimeID },
		"claimer":          func(m map[string]any) { m["claimed_runtime_id"] = srcSourceID },
		"result":           func(m map[string]any) { m["result"] = `{"different":true}` },
		"error":            func(m map[string]any) { m["error"] = "unexpected" },
		"oversized result": func(m map[string]any) { m["result"] = strings.Repeat("x", sourceReadResultMaxBytes+1) },
		"oversized error":  func(m map[string]any) { m["error"] = strings.Repeat("x", sourceReadErrorMaxBytes+1) },
		"terminal replay":  func(m map[string]any) { m["expires_at"] = time.Now().Add(-time.Hour).Format(time.RFC3339) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			client, mux := newSourceReadTestClient(t)
			mux.HandleFunc("/api/daemon/runtimes/"+srcRuntimeID+"/work-source-commands/"+srcCommandID+"/result", func(w http.ResponseWriter, r *http.Request) {
				m := receiptJSON("succeeded")
				m["result"] = "{}"
				mutate(m)
				json.NewEncoder(w).Encode(m)
			})
			_, err := client.ReportSourceReadCommand(context.Background(), srcRuntimeID, srcWorkspace, srcCommandID, srcMSRToken, "succeeded", "{}", "")
			if (err == nil) != (name == "terminal replay") {
				t.Fatalf("receipt validation: %v", err)
			}
		})
	}
}

// TestSourceReadClientConcurrentSetTokenAndRequests covers the defensive public
// Client contract, not PAT renewal, which extends the same token in place.
func TestSourceReadClientConcurrentSetTokenAndRequests(t *testing.T) {
	client, mux := newSourceReadTestClient(t)
	// Exercise a concurrent public setter alongside scoped and parent requests.
	tokens := []string{"mul_first", "mul_second", "mul_third"}
	mux.HandleFunc("/api/daemon/runtimes/"+srcRuntimeID+"/source-read-token", func(w http.ResponseWriter, r *http.Request) {
		// Parent credential must be one of the whole tokens, Bearer-formatted.
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer mul_") {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintf(w, "bad parent auth %q", got)
			return
		}
		json.NewEncoder(w).Encode(validMintBody(time.Now()))
	})
	client.SetToken(tokens[0])
	mux.HandleFunc("/api/daemon/runtimes/"+srcRuntimeID+"/work-source-commands", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{pendingCommandJSON()})
	})

	var wg sync.WaitGroup
	stop := make(chan struct{})
	setterDone := make(chan struct{})
	go func() {
		defer close(setterDone)
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			client.SetToken(tokens[i%len(tokens)])
			_ = client.Token()
			i++
		}
	}()
	errs := make(chan error, 64)
	for i := 0; i < 12; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := client.ExchangeSourceReadToken(context.Background(), srcRuntimeID, srcWorkspace, srcDaemonID)
			errs <- err
		}()
		go func() {
			defer wg.Done()
			_, err := client.ListSourceReadCommands(context.Background(), srcRuntimeID, srcWorkspace, srcMSRToken)
			errs <- err
		}()
	}
	wg.Wait()
	close(stop)
	<-setterDone
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("request during concurrent SetToken failed: %v", err)
		}
	}
}

// TestSourceReadClientConcurrentScopedAndNormalRequests proves scoped msr_
// requests and normal human-token requests can run concurrently without the
// capability leaking into or clobbering the client identity.
func TestSourceReadClientConcurrentScopedAndNormalRequests(t *testing.T) {
	client, mux := newSourceReadTestClient(t)
	mux.HandleFunc("/api/daemon/runtimes/"+srcRuntimeID+"/work-source-commands", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+srcMSRToken {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintf(w, "bad scoped auth %q", got)
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{pendingCommandJSON()})
	})
	mux.HandleFunc("/api/daemon/workspaces", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+srcHumanToken {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintf(w, "bad human auth %q", got)
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{})
	})
	client.SetToken(srcHumanToken)

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := client.ListSourceReadCommands(context.Background(), srcRuntimeID, srcWorkspace, srcMSRToken)
			errs <- err
		}()
		go func() { defer wg.Done(); _, err := client.ListWorkspaces(context.Background()); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent request failed: %v", err)
		}
	}
	if client.Token() != srcHumanToken {
		t.Fatalf("client token mutated: %q", client.Token())
	}
}
