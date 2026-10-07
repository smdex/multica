package main

// Production-router lock-wait regressions for the source-read-token flow.
// These complement source_read_token_routes_test.go: instead of invalid
// credentials, they prove that a token VALID at ingress can expire while its
// request is blocked on a real PostgreSQL row lock, and that a parent PAT
// revocation that commits while a claim waits on the PAT FOR SHARE lock wins
// over the already-parsed credential.
//
// All blocking uses real row locks on the production tables acquired through
// dedicated pool connections (BEGIN ... hold ... ROLLBACK/COMMIT), observed
// via pg_blocking_pids. No auth context injection, no mocks, no copied
// service code. Only statuses are asserted; credentials and token response
// bodies are never printed.
//
// Lock order under test (must match the service): workspace KEY SHARE ->
// runtime FOR SHARE -> member FOR SHARE -> PAT FOR SHARE -> source FOR
// UPDATE -> command FOR UPDATE.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// msrLockJWT signs a real owner JWT that dies after ttl, using the same
// issuer material as generateTestJWT. Exchange caps the minted msr_ at this
// expiry, producing a genuinely short-lived credential.
func msrLockJWT(t *testing.T, userID string, ttl time.Duration) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":  userID,
		"name": "msr lock owner",
		"exp":  time.Now().Add(ttl).Unix(),
		"iat":  time.Now().Unix(),
	})
	signed, err := token.SignedString(auth.JWTSecret())
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

// waitForBlockedBy polls until some backend is blocked by blockerPID on a
// lock this test planted, proving the in-flight request reached and waits at
// the intended lock point rather than failing early.
func waitForBlockedBy(ctx context.Context, pool *pgxpool.Pool, blockerPID int) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var blocked int
		err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity
			 WHERE cardinality(pg_blocking_pids(pid)) > 0
			   AND $1 = ANY(pg_blocking_pids(pid))`, blockerPID).Scan(&blocked)
		if err == nil && blocked > 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
	return false
}

// asyncSourceReadCall runs sourceReadCall in a goroutine so the request can
// be observed while it is blocked server-side. Only the status is reported.
func asyncSourceReadCall(ctx context.Context, method, path, token, workspace, body string) <-chan int {
	out := make(chan int, 1)
	go func() {
		defer close(out)
		resp, _, err := sourceReadCall(ctx, method, path, token, workspace, body)
		if err != nil {
			out <- 0
			return
		}
		out <- resp.StatusCode
	}()
	return out
}

func receiptStatus(t *testing.T, fx *testutil.Fixture, commandID string) string {
	t.Helper()
	var status string
	fx.QueryRow(t, `SELECT status FROM work_source_command WHERE id=$1`, commandID).Scan(&status)
	return status
}

func createPendingCommand(t *testing.T, ctx context.Context, fx *testutil.Fixture, runtimeID string) string {
	t.Helper()
	var sourceID string
	fx.QueryRow(t, `SELECT id FROM work_source WHERE runtime_id=$1`, runtimeID).Scan(&sourceID)
	var receipt struct {
		ID string `json:"id"`
	}
	_, created := mustSourceReadCall(t, ctx, http.MethodPost,
		"/api/work-sources/"+sourceID+"/commands", testToken, testWorkspaceID,
		`{"request_id":"`+uuid.NewString()+`","command":"list","limit":2}`, http.StatusCreated)
	if err := json.Unmarshal(created, &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt.ID
}

// TestSourceReadTokenExpiresWhileBlockedOnSourceLock: an msr_ valid at
// ingress must be re-validated after waiting on the source FOR UPDATE lock.
// (1) a claim blocked behind a held source UPDATE until the token expires
//     returns 401 and leaves the receipt pending;
// (2) a terminal-report EXACT replay blocked the same way returns 401, not
//     the idempotent 200 replay it would earn with a live token.
func TestSourceReadTokenExpiresWhileBlockedOnSourceLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	runtimeID, _ := sourceReadFixture(t, fx, testUserID, "msr lock expiry runtime", "msr-routes-lock-expiry")

	// --- Scenario 1: claim expires while blocked on the source UPDATE. ---
	jwtShort := msrLockJWT(t, testUserID, 5*time.Second)
	msrA := sourceReadExchange(t, ctx, runtimeID, jwtShort)
	claimsA, err := auth.ParseSourceReadToken(msrA, time.Now())
	if err != nil || claimsA.ExpiresAt == nil {
		t.Fatalf("parse minted msr_: %v", err)
	}
	expiresA := claimsA.ExpiresAt.Time
	cmdA := createPendingCommand(t, ctx, fx, runtimeID)

	blocker, err := testPool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()
	if _, err := blocker.Exec(ctx, `BEGIN`); err != nil {
		t.Fatal(err)
	}
	defer blocker.Exec(context.Background(), `ROLLBACK`)
	var blockerPID int
	if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	// Hold the same FOR UPDATE the service takes before the command lock.
	if _, err := blocker.Exec(ctx,
		`SELECT 1 FROM work_source WHERE runtime_id=$1 AND workspace_id=$2 FOR UPDATE`,
		runtimeID, testWorkspaceID); err != nil {
		t.Fatal(err)
	}

	claimDone := asyncSourceReadCall(ctx, http.MethodPost,
		"/api/daemon/runtimes/"+runtimeID+"/work-source-commands/"+cmdA+"/claim",
		msrA, testWorkspaceID, "")
	if !waitForBlockedBy(ctx, testPool, blockerPID) {
		t.Fatal("claim never blocked behind the held source lock")
	}
	// Sleep past the verified token expiry, then release: the post-lock time
	// recheck must reject the credential that was valid at ingress.
	if remain := time.Until(expiresA.Add(100 * time.Millisecond)); remain > 0 {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(remain):
		}
	}
	if _, err := blocker.Exec(ctx, `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	if status := <-claimDone; status != http.StatusUnauthorized {
		t.Fatalf("expired-while-blocked claim: status=%d want=401", status)
	}
	if got := receiptStatus(t, fx, cmdA); got != "pending" {
		t.Fatalf("receipt must stay pending after expired claim, got %q", got)
	}

	// --- Scenario 2: terminal-report exact replay expires after the wait. ---
	// cmdA is still pending and the source's one-in-flight guard blocks a
	// second create, so reach a terminal state on cmdA itself with a normal
	// full-lifetime exchange, then replay it behind the lock with a short one.
	pendingPath := "/api/daemon/runtimes/" + runtimeID + "/work-source-commands"
	msrLong := sourceReadExchange(t, ctx, runtimeID, testToken)
	mustSourceReadCall(t, ctx, http.MethodPost, pendingPath+"/"+cmdA+"/claim", msrLong, testWorkspaceID, "", http.StatusOK)
	report := `{"status":"succeeded","result":"[]"}`
	mustSourceReadCall(t, ctx, http.MethodPost, pendingPath+"/"+cmdA+"/result", msrLong, testWorkspaceID, report, http.StatusOK)

	jwtShort2 := msrLockJWT(t, testUserID, 5*time.Second)
	msrB := sourceReadExchange(t, ctx, runtimeID, jwtShort2)
	claimsB, err := auth.ParseSourceReadToken(msrB, time.Now())
	if err != nil || claimsB.ExpiresAt == nil {
		t.Fatalf("parse minted msr_: %v", err)
	}
	expiresB := claimsB.ExpiresAt.Time

	if _, err := blocker.Exec(ctx, `BEGIN`); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(ctx,
		`SELECT 1 FROM work_source WHERE runtime_id=$1 AND workspace_id=$2 FOR UPDATE`,
		runtimeID, testWorkspaceID); err != nil {
		t.Fatal(err)
	}
	replayDone := asyncSourceReadCall(ctx, http.MethodPost, pendingPath+"/"+cmdA+"/result",
		msrB, testWorkspaceID, report)
	if !waitForBlockedBy(ctx, testPool, blockerPID) {
		t.Fatal("terminal replay never blocked behind the held source lock")
	}
	if remain := time.Until(expiresB.Add(100 * time.Millisecond)); remain > 0 {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(remain):
		}
	}
	if _, err := blocker.Exec(ctx, `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	// With a live token this exact replay is idempotent 200; expired after
	// the wait it must be 401.
	if status := <-replayDone; status != http.StatusUnauthorized {
		t.Fatalf("expired terminal replay: status=%d want=401 (not 200)", status)
	}
	if got := receiptStatus(t, fx, cmdA); got != "succeeded" {
		t.Fatalf("terminal receipt must stay succeeded, got %q", got)
	}
}

// TestSourceReadTokenPATRevocationWinsLockBeforeClaim: a parent PAT
// revocation committed while the claim waits on the PAT FOR SHARE lock beats
// the credential parsed at ingress. The claim returns 401 and the receipt
// stays pending.
func TestSourceReadTokenPATRevocationWinsLockBeforeClaim(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)

	member := fx.User(t, "msr revoke-race owner", "msr-revoke-race-"+time.Now().Format("150405.000000000")+`@example.test`)
	fx.Member(t, testWorkspaceID, member, "member")
	pat := sourceReadPAT(t, fx, member)
	runtimeID, _ := sourceReadFixture(t, fx, member, "msr revoke-race runtime", "msr-routes-revoke-race")
	pendingPath := "/api/daemon/runtimes/" + runtimeID + "/work-source-commands"

	msr := sourceReadExchange(t, ctx, runtimeID, pat)
	commandID := createPendingCommand(t, ctx, fx, runtimeID)

	blocker, err := testPool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()
	if _, err := blocker.Exec(ctx, `BEGIN`); err != nil {
		t.Fatal(err)
	}
	defer blocker.Exec(context.Background(), `ROLLBACK`)
	var blockerPID int
	if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	// Uncommitted revocation of the parent PAT row: the claim's PAT FOR
	// SHARE read must queue behind this UPDATE, and once committed the
	// revoked=FALSE predicate fails.
	if _, err := blocker.Exec(ctx,
		`UPDATE personal_access_token SET revoked=TRUE WHERE token_hash=$1`,
		auth.HashToken(pat)); err != nil {
		t.Fatal(err)
	}

	claimDone := asyncSourceReadCall(ctx, http.MethodPost,
		pendingPath+"/"+commandID+"/claim", msr, testWorkspaceID, "")
	if !waitForBlockedBy(ctx, testPool, blockerPID) {
		t.Fatal("claim never blocked behind the uncommitted PAT revocation")
	}
	if _, err := blocker.Exec(ctx, `COMMIT`); err != nil {
		t.Fatal(err)
	}
	if status := <-claimDone; status != http.StatusUnauthorized {
		t.Fatalf("claim after committed revocation: status=%d want=401", status)
	}
	if got := receiptStatus(t, fx, commandID); got != "pending" {
		t.Fatalf("receipt must stay pending, got %q", got)
	}
}
