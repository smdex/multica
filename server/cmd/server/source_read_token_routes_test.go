package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// Production-router regressions for the source-read-token exchange
// (POST /api/daemon/runtimes/{runtimeId}/source-read-token) and the msr_
// bearer credential it mints. All daemon activity uses inert fixtures:
// observe-mode work sources, no agent execution, no fake daemons.
// Credentials are never logged; failures print status and response body
// only.
//
// Error policy under test:
//   - exchange: anonymous 401; nonowner/admin/NULL-owner 403; membership
//     incarnation mismatch 403; workspace override 404; malformed runtime
//     UUID 400; offline runtime 409; msr_ as exchange credential 403
//     (valid token, wrong scope surface); missing member row 404.
//   - delivery routes: JWT/PAT 403 (not daemon credentials); msr_ on any
//     other daemon route 403 (scope fenced); msr_ on user routes 401;
//     cookie-only 401; runtime-sibling 403; msr_ with no member row 404.

// sourceReadFixture stages an owned online runtime with an observe work
// source. providerSuffix keeps sibling runtimes on the same daemon_id
// distinct (builtin uniqueness is (workspace, daemon, provider)).
func sourceReadFixture(t *testing.T, fx *testutil.Fixture, ownerID, name, provider string) (runtimeID, daemonID string) {
	t.Helper()
	daemonID = "msr-" + uuid.NewString()
	cols := testutil.Cols{
		"workspace_id": testWorkspaceID,
		"name":         name, "daemon_id": daemonID, "provider": provider,
		"runtime_mode": "local", "status": "online", "visibility": "private",
		"device_info": "", "metadata": testutil.Raw("'{}'::jsonb"),
	}
	if ownerID != "" {
		cols["owner_id"] = ownerID
	} // else: ownerless runtime, insert NULL rather than an invalid UUID.
	runtimeID = fx.Insert(t, "agent_runtime", cols)
	fx.Insert(t, "work_source", testutil.Cols{
		"id": testutil.Raw("gen_random_uuid()"), "workspace_id": testWorkspaceID,
		"runtime_id": runtimeID, "daemon_id": daemonID, "name": name,
		"source_handle": "msr-approved-" + uuid.NewString(), "mode": "observe",
	})
	return runtimeID, daemonID
}

// sourceReadPAT creates a real PAT row for userID.
func sourceReadPAT(t *testing.T, fx *testutil.Fixture, userID string) string {
	t.Helper()
	token, err := auth.GeneratePATToken()
	if err != nil {
		t.Fatal(err)
	}
	fx.Insert(t, "personal_access_token", testutil.Cols{
		"user_id": userID, "name": "msr routes fixture", "token_hash": auth.HashToken(token),
		"token_prefix": token[:12], "expires_at": time.Now().Add(time.Hour),
	})
	return token
}

func sourceReadCall(ctx context.Context, method, path, token, workspace, body string) (*http.Response, []byte, error) {
	var reader io.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, testServer.URL+path, reader)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if workspace != "" {
		req.Header.Set("X-Workspace-ID", workspace)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := testServer.Client().Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp, data, err
}

func mustSourceReadCall(t *testing.T, ctx context.Context, method, path, token, workspace, body string, status int) (*http.Response, []byte) {
	t.Helper()
	resp, data, err := sourceReadCall(ctx, method, path, token, workspace, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	if resp.StatusCode != status {
		// Credential exchange bodies are omitted: an exchange response may
		// carry a minted token that must never reach test logs.
		if strings.Contains(path, "/source-read-token") {
			t.Fatalf("%s %s: status=%d want=%d (exchange body omitted)", method, path, resp.StatusCode, status)
		}
		t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, resp.StatusCode, status, data)
	}
	return resp, data
}

// sourceReadExchange performs the exchange and returns the minted msr_ token.
func sourceReadExchange(t *testing.T, ctx context.Context, runtimeID, token string) string {
	t.Helper()
	_, data := mustSourceReadCall(t, ctx, http.MethodPost,
		"/api/daemon/runtimes/"+runtimeID+"/source-read-token", token, testWorkspaceID,
		`{"scope":"source:read"}`, http.StatusOK)
	var ex struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &ex); err != nil || len(ex.Token) == 0 {
		// Exchange bodies are omitted: they may carry a minted credential.
		t.Fatalf("invalid exchange response: err=%v token_present=%t", err, len(ex.Token) > 0)
	}
	return ex.Token
}

func TestSourceReadTokenExchangeOwnership(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)

	outsider := fx.User(t, "msr outsider", "msr-"+uuid.NewString()+"@example.test")
	fx.Member(t, testWorkspaceID, outsider, "member")
	outsiderJWT, err := generateTestJWT(outsider, "", "msr outsider")
	if err != nil {
		t.Fatal(err)
	}
	admin := fx.User(t, "msr admin", "msr-admin-"+uuid.NewString()+"@example.test")
	fx.Member(t, testWorkspaceID, admin, "admin")
	adminJWT, err := generateTestJWT(admin, "", "msr admin")
	if err != nil {
		t.Fatal(err)
	}
	adminPAT := sourceReadPAT(t, fx, admin)
	ownerPAT := sourceReadPAT(t, fx, testUserID)

	ownedRuntime, ownedDaemon := sourceReadFixture(t, fx, testUserID, "msr owner runtime", "msr-routes-owner")
	nullOwner, _ := sourceReadFixture(t, fx, "", "msr null owner runtime", "msr-routes-null")

	exchangeBody := `{"scope":"source:read"}`
	exchangePath := "/api/daemon/runtimes/" + ownedRuntime + "/source-read-token"
	nullPath := "/api/daemon/runtimes/" + nullOwner + "/source-read-token"

	mustSourceReadCall(t, ctx, http.MethodPost, exchangePath, "", testWorkspaceID, exchangeBody, http.StatusUnauthorized)
	mustSourceReadCall(t, ctx, http.MethodPost, exchangePath, outsiderJWT, testWorkspaceID, exchangeBody, http.StatusForbidden)
	mustSourceReadCall(t, ctx, http.MethodPost, exchangePath, sourceReadPAT(t, fx, outsider), testWorkspaceID, exchangeBody, http.StatusForbidden)
	mustSourceReadCall(t, ctx, http.MethodPost, exchangePath, adminJWT, testWorkspaceID, exchangeBody, http.StatusForbidden)
	mustSourceReadCall(t, ctx, http.MethodPost, exchangePath, adminPAT, testWorkspaceID, exchangeBody, http.StatusForbidden)
	mustSourceReadCall(t, ctx, http.MethodPost, nullPath, testToken, testWorkspaceID, exchangeBody, http.StatusForbidden)
	// Membership denial conceals the workspace: override is 404, not 403.
	mustSourceReadCall(t, ctx, http.MethodPost, exchangePath, testToken, uuid.NewString(), exchangeBody, http.StatusNotFound)
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/daemon/runtimes/not-a-uuid/source-read-token", testToken, testWorkspaceID, exchangeBody, http.StatusBadRequest)
	// Offline runtime refuses to mint.
	fx.Exec(t, `UPDATE agent_runtime SET status='offline' WHERE id=$1`, ownedRuntime)
	mustSourceReadCall(t, ctx, http.MethodPost, exchangePath, testToken, testWorkspaceID, exchangeBody, http.StatusConflict)
	fx.Exec(t, `UPDATE agent_runtime SET status='online' WHERE id=$1`, ownedRuntime)

	// Owner success over both real credential kinds.
	resp, data := mustSourceReadCall(t, ctx, http.MethodPost, exchangePath, testToken, testWorkspaceID, exchangeBody, http.StatusOK)
	var exchange struct {
		Token       string `json:"token"`
		TokenType   string `json:"token_type"`
		Scope       string `json:"scope"`
		RuntimeID   string `json:"runtime_id"`
		WorkspaceID string `json:"workspace_id"`
		DaemonID    string `json:"daemon_id"`
		ExpiresAt   string `json:"expires_at"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &exchange); err != nil {
		// Exchange bodies are omitted: they may carry a minted credential.
		t.Fatalf("decode exchange response: %v", err)
	}
	if len(exchange.Token) < len(auth.SourceReadTokenPrefix)+20 || exchange.Token[:4] != auth.SourceReadTokenPrefix {
		t.Fatalf("token must be %s-prefixed", auth.SourceReadTokenPrefix)
	}
	if exchange.TokenType != "Bearer" || exchange.Scope != "source:read" {
		// Safe selectors only; never the whole struct, which includes the token.
		t.Fatalf("token_type/scope wrong: token_type=%q scope=%q", exchange.TokenType, exchange.Scope)
	}
	if exchange.RuntimeID != ownedRuntime || exchange.WorkspaceID != testWorkspaceID || exchange.DaemonID != ownedDaemon {
		// Safe selectors only; never the whole struct, which includes the token.
		t.Fatalf("exchange lost runtime/workspace/daemon identity: runtime=%t workspace=%t daemon=%t",
			exchange.RuntimeID == ownedRuntime, exchange.WorkspaceID == testWorkspaceID, exchange.DaemonID == ownedDaemon)
	}
	if exchange.ExpiresIn <= 0 || exchange.ExpiresIn > 120 {
		t.Fatalf("expires_in must be in (0,120]: %d", exchange.ExpiresIn)
	}
	if expiresAt, err := time.Parse(time.RFC3339, exchange.ExpiresAt); err != nil || time.Until(expiresAt) <= 0 || time.Until(expiresAt) > 121*time.Second {
		t.Fatalf("expires_at must be within 120s: %q err=%v", exchange.ExpiresAt, err)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control must be no-store, got %q", cc)
	}
	msr := exchange.Token

	respPAT, dataPAT := mustSourceReadCall(t, ctx, http.MethodPost, exchangePath, ownerPAT, testWorkspaceID, exchangeBody, http.StatusOK)
	if respPAT.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("PAT exchange must also be no-store")
	}
	var patExchange struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(dataPAT, &patExchange); err != nil || len(patExchange.Token) == 0 {
		// Exchange bodies are omitted: they may carry a minted credential.
		t.Fatalf("PAT exchange failed: err=%v token_present=%t", err, len(patExchange.Token) > 0)
	}

	// Exchange body strictness.
	mustSourceReadCall(t, ctx, http.MethodPost, exchangePath, testToken, testWorkspaceID, `{"scope":"source:write"}`, http.StatusBadRequest)
	mustSourceReadCall(t, ctx, http.MethodPost, exchangePath, testToken, testWorkspaceID, `{"scope":"source:read","extra":1}`, http.StatusBadRequest)
	mustSourceReadCall(t, ctx, http.MethodPost, exchangePath, testToken, testWorkspaceID, `{"scope":"source:read"} trailing`, http.StatusBadRequest)
	mustSourceReadCall(t, ctx, http.MethodPost, exchangePath, testToken, testWorkspaceID, `[{"scope":"source:read"}]`, http.StatusBadRequest)
	mustSourceReadCall(t, ctx, http.MethodPost, exchangePath, testToken, testWorkspaceID, ``, http.StatusBadRequest)

	// A valid msr_ is a scope error on the exchange surface, not a missing
	// credential.
	mustSourceReadCall(t, ctx, http.MethodPost, exchangePath, msr, testWorkspaceID, exchangeBody, http.StatusForbidden)
}

// TestSourceReadTokenCommandFlow walks the real owner flow end to end:
// create a work-source command as a workspace user, exchange for an msr_
// token as the owner, then deliver/claim/result/replay through it, and let
// the requesting user observe the terminal receipt.
func TestSourceReadTokenCommandFlow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	runtimeID, _ := sourceReadFixture(t, fx, testUserID, "msr flow runtime", "msr-routes-flow")

	var sourceID string
	fx.QueryRow(t, `SELECT id FROM work_source WHERE runtime_id=$1`, runtimeID).Scan(&sourceID)
	commandPath := "/api/work-sources/" + sourceID + "/commands"
	requestID := uuid.NewString()
	var receipt struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	_, created := mustSourceReadCall(t, ctx, http.MethodPost, commandPath, testToken, testWorkspaceID,
		`{"request_id":"`+requestID+`","command":"list","limit":2}`, http.StatusCreated)
	if err := json.Unmarshal(created, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.ID == "" || receipt.Status != "pending" {
		t.Fatalf("invalid pending receipt: %+v", receipt)
	}
	commandID := receipt.ID

	msr := sourceReadExchange(t, ctx, runtimeID, testToken)

	pendingPath := "/api/daemon/runtimes/" + runtimeID + "/work-source-commands"
	claimPath := pendingPath + "/" + commandID + "/claim"
	resultPath := pendingPath + "/" + commandID + "/result"

	// JWT and PAT are not valid daemon work-source credentials.
	mustSourceReadCall(t, ctx, http.MethodGet, pendingPath, testToken, testWorkspaceID, "", http.StatusForbidden)
	pat := sourceReadPAT(t, fx, testUserID)
	mustSourceReadCall(t, ctx, http.MethodGet, pendingPath, pat, testWorkspaceID, "", http.StatusForbidden)
	mustSourceReadCall(t, ctx, http.MethodPost, claimPath, pat, testWorkspaceID, "", http.StatusForbidden)

	var rows []map[string]any
	_, delivered := mustSourceReadCall(t, ctx, http.MethodGet, pendingPath, msr, testWorkspaceID, "", http.StatusOK)
	if err := json.Unmarshal(delivered, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["id"] != commandID {
		t.Fatalf("msr pending delivery lost identity: %v", rows)
	}
	mustSourceReadCall(t, ctx, http.MethodPost, claimPath, msr, testWorkspaceID, "", http.StatusOK)
	mustSourceReadCall(t, ctx, http.MethodPost, claimPath, msr, testWorkspaceID, "", http.StatusConflict)
	report := `{"status":"succeeded","result":"[]"}`
	mustSourceReadCall(t, ctx, http.MethodPost, resultPath, msr, testWorkspaceID, report, http.StatusOK)
	mustSourceReadCall(t, ctx, http.MethodPost, resultPath, msr, testWorkspaceID, report, http.StatusOK)
	mustSourceReadCall(t, ctx, http.MethodPost, resultPath, msr, testWorkspaceID, `{"status":"failed","error":"opposite replay"}`, http.StatusConflict)

	var terminal struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Result string `json:"result"`
	}
	_, finalReceipt := mustSourceReadCall(t, ctx, http.MethodGet, "/api/work-source-commands/"+commandID, testToken, testWorkspaceID, "", http.StatusOK)
	if err := json.Unmarshal(finalReceipt, &terminal); err != nil {
		t.Fatal(err)
	}
	if terminal.ID != commandID || terminal.Status != "succeeded" || terminal.Result != "[]" {
		t.Fatalf("terminal receipt wrong: %+v", terminal)
	}
}

// TestSourceReadTokenScopedToFencedRoutes proves the msr_ selector is denied
// on every other credential surface: task claims, heartbeat, register,
// source CRUD, and history — plus cookie-only requests.
func TestSourceReadTokenScopedToFencedRoutes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	runtimeID, daemonID := sourceReadFixture(t, fx, testUserID, "msr fence runtime", "msr-routes-fence")
	msr := sourceReadExchange(t, ctx, runtimeID, testToken)

	// Sibling runtime on the same daemon_id (distinct provider for the
	// builtin uniqueness on (workspace, daemon, provider)): still denied.
	siblingRuntime, _ := sourceReadFixture(t, fx, testUserID, "msr fence sibling", "msr-routes-fence-sibling")
	fx.Exec(t, `UPDATE agent_runtime SET daemon_id=$2 WHERE id=$1`, siblingRuntime, daemonID)
	siblingPending := "/api/daemon/runtimes/" + siblingRuntime + "/work-source-commands"
	mustSourceReadCall(t, ctx, http.MethodGet, siblingPending, msr, testWorkspaceID, "", http.StatusForbidden)
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/daemon/runtimes/"+siblingRuntime+"/work-source-commands/"+uuid.NewString()+"/claim", msr, testWorkspaceID, "", http.StatusForbidden)

	// Valid msr_, wrong surface: scope-denied on other daemon routes.
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/daemon/register", msr, testWorkspaceID, `{"runtimes":[]}`, http.StatusForbidden)
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/daemon/heartbeat", msr, testWorkspaceID, `{}`, http.StatusForbidden)
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/daemon/tasks/claim", msr, testWorkspaceID, `{}`, http.StatusForbidden)
	mustSourceReadCall(t, ctx, http.MethodPost, "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", msr, testWorkspaceID, `{}`, http.StatusForbidden)

	// msr_ is not a user credential at all: user routes stay 401.
	var sourceID string
	fx.QueryRow(t, `SELECT id FROM work_source WHERE runtime_id=$1`, runtimeID).Scan(&sourceID)
	commandPath := "/api/work-sources/" + sourceID + "/commands"
	mustSourceReadCall(t, ctx, http.MethodPost, commandPath, msr, testWorkspaceID, `{"request_id":"`+uuid.NewString()+`","command":"list"}`, http.StatusUnauthorized)
	mustSourceReadCall(t, ctx, http.MethodGet, commandPath, msr, testWorkspaceID, "", http.StatusUnauthorized)
	mustSourceReadCall(t, ctx, http.MethodDelete, "/api/work-sources/"+sourceID, msr, testWorkspaceID, "", http.StatusUnauthorized)
	mustSourceReadCall(t, ctx, http.MethodGet, "/api/work-source-commands/"+uuid.NewString(), msr, testWorkspaceID, "", http.StatusUnauthorized)

	// Cookie-only (no Authorization header) is never sufficient.
	cookieReq, err := http.NewRequestWithContext(ctx, http.MethodGet, testServer.URL+"/api/daemon/runtimes/"+runtimeID+"/work-source-commands", nil)
	if err != nil {
		t.Fatal(err)
	}
	cookieReq.Header.Set("Cookie", "multica_session="+msr)
	cookieReq.Header.Set("X-Workspace-ID", testWorkspaceID)
	cookieResp, err := testServer.Client().Do(cookieReq)
	if err != nil {
		t.Fatal(err)
	}
	cookieResp.Body.Close()
	if cookieResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cookie-only msr must be unauthorized, got %d", cookieResp.StatusCode)
	}
}

// TestSourceReadTokenInvalidationOnPATRevocation proves a warm-cache msr_
// dies when its parent PAT is revoked. Revocation is never undone: each
// later subtest scenario uses fresh fixtures.
func TestSourceReadTokenInvalidationOnPATRevocation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)

	member := fx.User(t, "msr revocable owner", "msr-revoke-"+uuid.NewString()+"@example.test")
	fx.Member(t, testWorkspaceID, member, "member")
	pat := sourceReadPAT(t, fx, member)
	runtimeID, _ := sourceReadFixture(t, fx, member, "msr revocation runtime", "msr-routes-revoke")
	pendingPath := "/api/daemon/runtimes/" + runtimeID + "/work-source-commands"
	exchangePath := "/api/daemon/runtimes/" + runtimeID + "/source-read-token"

	msr := sourceReadExchange(t, ctx, runtimeID, pat)
	mustSourceReadCall(t, ctx, http.MethodGet, pendingPath, msr, testWorkspaceID, "", http.StatusOK)

	fx.Exec(t, `UPDATE personal_access_token SET revoked=TRUE WHERE token_hash=$1`, auth.HashToken(pat))
	mustSourceReadCall(t, ctx, http.MethodPost, exchangePath, pat, testWorkspaceID, `{"scope":"source:read"}`, http.StatusUnauthorized)
	mustSourceReadCall(t, ctx, http.MethodGet, pendingPath, msr, testWorkspaceID, "", http.StatusUnauthorized)
}

// TestSourceReadTokenInvalidationOnMembershipIncarnation proves the ABA case:
// after the owner's member row is deleted through the real HTTP API and a
// NEW member row (different UUID) is created by re-adding, the msr_ minted
// against the old incarnation stays denied while a fresh exchange plus a
// fresh msr_ succeeds — so denial is incarnation mismatch, not a lingering
// PAT revocation or missing membership.
func TestSourceReadTokenInvalidationOnMembershipIncarnation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)

	member := fx.User(t, "msr incarnation owner", "msr-aba-"+uuid.NewString()+"@example.test")
	oldMemberRow := fx.Member(t, testWorkspaceID, member, "member")
	pat := sourceReadPAT(t, fx, member)
	runtimeID, _ := sourceReadFixture(t, fx, member, "msr incarnation runtime", "msr-routes-aba")
	pendingPath := "/api/daemon/runtimes/" + runtimeID + "/work-source-commands"
	exchangePath := "/api/daemon/runtimes/" + runtimeID + "/source-read-token"

	oldMSR := sourceReadExchange(t, ctx, runtimeID, pat)
	mustSourceReadCall(t, ctx, http.MethodGet, pendingPath, oldMSR, testWorkspaceID, "", http.StatusOK)

	// Remove the member through the production API as the workspace owner.
	mustSourceReadCall(t, ctx, http.MethodDelete, "/api/workspaces/"+testWorkspaceID+"/members/"+oldMemberRow, testToken, testWorkspaceID, "", http.StatusNoContent)
	// Member removal marks the runtime offline; restore it online first so
	// the checks below isolate membership status, not runtime status.
	fx.Exec(t, `UPDATE agent_runtime SET status='online' WHERE id=$1`, runtimeID)
	// Still-valid msr_ but no member row: concealed as missing (404).
	mustSourceReadCall(t, ctx, http.MethodGet, pendingPath, oldMSR, testWorkspaceID, "", http.StatusNotFound)
	mustSourceReadCall(t, ctx, http.MethodPost, exchangePath, pat, testWorkspaceID, `{"scope":"source:read"}`, http.StatusNotFound)

	// Re-add creates a NEW member row UUID (incarnation changes).
	newMemberRow := fx.Member(t, testWorkspaceID, member, "member")
	if newMemberRow == oldMemberRow {
		t.Fatal("re-add must create a new member row UUID")
	}

	// The OLD incarnation's msr_ is scope-denied on the new incarnation...
	mustSourceReadCall(t, ctx, http.MethodGet, pendingPath, oldMSR, testWorkspaceID, "", http.StatusForbidden)
	// ...while a fresh exchange and fresh msr_ on the new incarnation work.
	freshMSR := sourceReadExchange(t, ctx, runtimeID, pat)
	mustSourceReadCall(t, ctx, http.MethodGet, pendingPath, freshMSR, testWorkspaceID, "", http.StatusOK)
}
