package main

// Production-router lock-wait and incarnation-fence regressions for the
// native source enrollment flow (companion to
// native_source_enrollment_routes_test.go and the pg_blocking_pids pattern
// from source_read_token_lock_test.go):
//
//  1. An mse_ valid at ingress must be re-validated after waiting on the real
//     work_source FOR UPDATE lock: expiry committed while blocked yields 401,
//     a pending enrollment stays pending, a terminal source stays enrolled.
//     Token expiry is derived from the ACTUAL parsed capability
//     (auth.ParseSourceEnrollmentToken), never from a copied helper constant.
//  2. Membership incarnation fence: removing the owner member kills the
//     capability (404) and re-adding the user under a new member UUID cannot
//     rehabilitate the grant (403 finalize, 403 re-issuance). Runtime
//     force-offline during member delete is reversed by fixture SQL so the
//     assertion isolates membership itself.
//  3. Authority pins after issuance: role demotion 403, runtime created_at
//     change 403 (incarnation pin), config revision change via the real PATCH
//     route 409s the old capability, and a fresh approval at the new revision
//     finalizes 200 with the same enrollment/hash.
//
// All blocking uses real row locks observed via pg_blocking_pids. Only
// statuses and pinned selectors are asserted; credential exchange bodies are
// never printed (mustNativeEnrollCall sanitization). No task/issue/agent
// creation happens anywhere; a baseline query asserts that.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// mseExpiry derives the actual expiry of a minted capability from its parsed
// claims, so lock-wait timing never assumes a helper's expiration constant.
func mseExpiry(t *testing.T, token string) time.Time {
	t.Helper()
	claims, err := auth.ParseSourceEnrollmentToken(token, time.Now())
	if err != nil || claims.ExpiresAt == nil {
		t.Fatalf("parse minted mse_: err=%v expires_present=%t", err, claims.ExpiresAt != nil)
	}
	return claims.ExpiresAt.Time
}

// mseSourceState reads the enrollment columns straight from the row.
func mseSourceState(t *testing.T, fx *testutil.Fixture, sourceID string) (status string, enrolled bool) {
	t.Helper()
	var enrolledAt *time.Time
	fx.QueryRow(t,
		`SELECT CASE WHEN native_enrolled_at IS NULL THEN 'pending' ELSE 'enrolled' END, native_enrolled_at FROM work_source WHERE id=$1`, sourceID).
		Scan(&status, &enrolledAt)
	return status, enrolledAt != nil
}

// mseNoSpawnBaseline snapshots the tables no enrollment route may touch.
func mseNoSpawnBaseline(t *testing.T, fx *testutil.Fixture) (agents, issues, tasks int) {
	t.Helper()
	fx.QueryRow(t, `SELECT count(*) FROM agent`).Scan(&agents)
	fx.QueryRow(t, `SELECT count(*) FROM issue`).Scan(&issues)
	fx.QueryRow(t, `SELECT count(*) FROM agent_task_queue`).Scan(&tasks)
	return
}

func mseAssertNoSpawn(t *testing.T, fx *testutil.Fixture, agents, issues, tasks int) {
	t.Helper()
	a, i, q := mseNoSpawnBaseline(t, fx)
	if a != agents || i != issues || q != tasks {
		t.Fatalf("enrollment routes must not create tasks/issues/agents: agents %d->%d issues %d->%d tasks %d->%d",
			agents, a, issues, i, tasks, q)
	}
}

// mseBlocker acquires a dedicated pool connection, begins a transaction, and
// holds the work_source FOR UPDATE the finalize path takes before its
// post-lock time recheck. Release with the returned rollback func.
func mseBlocker(t *testing.T, ctx context.Context, sourceID string) (blockerPID int, release func()) {
	t.Helper()
	conn, err := testPool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `BEGIN`); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SELECT 1 FROM work_source WHERE id=$1 FOR UPDATE`, sourceID); err != nil {
		t.Fatal(err)
	}
	return blockerPID, func() {
		conn.Exec(context.Background(), `ROLLBACK`)
		conn.Release()
	}
}

// waitPast sleeps until just after the verified expiry, honoring ctx.
func waitPast(ctx context.Context, expiresAt time.Time) {
	if remain := time.Until(expiresAt.Add(100 * time.Millisecond)); remain > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(remain):
		}
	}
}

// TestNativeSourceEnrollmentExpiryAfterSourceLockWait: an mse_ minted
// short-lived via a clipped parent JWT, valid at ingress, must be re-validated
// after waiting on the work_source FOR UPDATE lock.
func TestNativeSourceEnrollmentExpiryAfterSourceLockWait(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	agents, issues, tasks := mseNoSpawnBaseline(t, fx)

	runtimeID := nativeEnrollFixture(t, fx, testUserID, "mse lock expiry runtime", "mse-lock-expiry")
	sourceID, enrollmentID := nativeEnrollIntent(t, ctx, runtimeID, testToken, uuid.NewString(), "native lock expiry")
	finalizePath := nativeEnrollFinalizePath(runtimeID, sourceID)
	revision := int32(1)
	hash := strings.Repeat("f6", 32)

	// --- Short cap expires while blocked: 401, stays pending. ---
	jwtShort := msrLockJWT(t, testUserID, 5*time.Second)
	mseShort := nativeEnrollToken(t, ctx, runtimeID, sourceID, jwtShort, enrollmentID, revision, hash)
	expiresShort := mseExpiry(t, mseShort)

	blockerPID, release := mseBlocker(t, ctx, sourceID)
	done := asyncSourceReadCall(ctx, http.MethodPost, finalizePath, mseShort, testWorkspaceID,
		nativeEnrollProofBody(enrollmentID, revision, hash))
	if !waitForBlockedBy(ctx, testPool, blockerPID) {
		release()
		t.Fatal("finalize never blocked behind the held source lock")
	}
	waitPast(ctx, expiresShort)
	release()
	if status := <-done; status != http.StatusUnauthorized {
		t.Fatalf("expired-while-blocked finalize: status=%d want=401", status)
	}
	if status, enrolled := mseSourceState(t, fx, sourceID); status != "pending" || enrolled {
		t.Fatalf("enrollment must stay pending after expired finalize, got status=%q enrolled=%t", status, enrolled)
	}

	// --- Long cap finalizes 200 once unblocked. ---
	mseLong := nativeEnrollToken(t, ctx, runtimeID, sourceID, testToken, enrollmentID, revision, hash)
	mustNativeEnrollCall(t, ctx, http.MethodPost, finalizePath, mseLong, testWorkspaceID,
		nativeEnrollProofBody(enrollmentID, revision, hash), http.StatusOK)
	if status, enrolled := mseSourceState(t, fx, sourceID); status != "enrolled" || !enrolled {
		t.Fatalf("long-cap finalize must enroll, got status=%q enrolled=%t", status, enrolled)
	}

	// --- Terminal replay with a fresh short cap behind the lock: 401, stays
	// enrolled. Same enrollment/source throughout: no second intent. ---
	jwtShort2 := msrLockJWT(t, testUserID, 5*time.Second)
	mseShort2 := nativeEnrollToken(t, ctx, runtimeID, sourceID, jwtShort2, enrollmentID, revision, hash)
	expiresShort2 := mseExpiry(t, mseShort2)

	blockerPID2, release2 := mseBlocker(t, ctx, sourceID)
	done2 := asyncSourceReadCall(ctx, http.MethodPost, finalizePath, mseShort2, testWorkspaceID,
		nativeEnrollProofBody(enrollmentID, revision, hash))
	if !waitForBlockedBy(ctx, testPool, blockerPID2) {
		release2()
		t.Fatal("terminal replay never blocked behind the held source lock")
	}
	waitPast(ctx, expiresShort2)
	release2()
	if status := <-done2; status != http.StatusUnauthorized {
		t.Fatalf("expired terminal replay: status=%d want=401 (not the idempotent 200)", status)
	}
	if status, enrolled := mseSourceState(t, fx, sourceID); status != "enrolled" || !enrolled {
		t.Fatalf("terminal source must stay enrolled, got status=%q enrolled=%t", status, enrolled)
	}
	mseAssertNoSpawn(t, fx, agents, issues, tasks)
}

// TestNativeSourceEnrollmentMemberIncarnationFence: the source pins the member
// row identity, so remove/re-add cannot rehabilitate a grant. Member delete
// force-offlines owned runtimes; fixture SQL restores online so the final
// assertions isolate membership itself.
func TestNativeSourceEnrollmentMemberIncarnationFence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	agents, issues, tasks := mseNoSpawnBaseline(t, fx)

	member := fx.User(t, "mse incarnation owner", "mse-incarn-"+uuid.NewString()+"@example.test")
	memberRow := fx.Member(t, testWorkspaceID, member, "admin")
	memberToken := msrLockJWT(t, member, time.Hour)
	runtimeID := nativeEnrollFixture(t, fx, member, "mse incarnation runtime", "mse-incarnation")
	sourceID, enrollmentID := nativeEnrollIntent(t, ctx, runtimeID, memberToken, uuid.NewString(), "native incarnation")
	revision := int32(1)
	hash := strings.Repeat("b7", 32)
	proof := nativeEnrollProofBody(enrollmentID, revision, hash)
	finalizePath := nativeEnrollFinalizePath(runtimeID, sourceID)
	tokenPath := "/api/daemon/runtimes/" + runtimeID + "/source-enrollments/" + sourceID + "/token"

	// Grant approved but not yet finalized: pending.
	mse := nativeEnrollToken(t, ctx, runtimeID, sourceID, memberToken, enrollmentID, revision, hash)

	// Owned admin removal by the workspace owner.
	mustSourceReadCall(t, ctx, http.MethodDelete,
		"/api/workspaces/"+testWorkspaceID+"/members/"+memberRow, testToken, testWorkspaceID, "", http.StatusNoContent)

	// Isolate membership: reverse the force-offline side effect.
	fx.Exec(t, `UPDATE agent_runtime SET status='online' WHERE id=$1`, runtimeID)

	// Membership row gone: 404, never a commit.
	mustNativeEnrollCall(t, ctx, http.MethodPost, finalizePath, mse, testWorkspaceID, proof, http.StatusNotFound)
	if status, enrolled := mseSourceState(t, fx, sourceID); status != "pending" || enrolled {
		t.Fatalf("must stay pending after incarnation break, got status=%q enrolled=%t", status, enrolled)
	}

	// Re-add the same user as admin: a DIFFERENT member UUID. The capability
	// pins the original member identity, so finalize is 403 and re-issuance on
	// the same terminal-source grant is 403: no un-revoke.
	newMemberRow := fx.Member(t, testWorkspaceID, member, "admin")
	if newMemberRow == memberRow {
		t.Fatal("re-added member must have a different incarnation UUID")
	}
	mustNativeEnrollCall(t, ctx, http.MethodPost, finalizePath, mse, testWorkspaceID, proof, http.StatusForbidden)
	mustNativeEnrollCall(t, ctx, http.MethodPost, tokenPath, memberToken, testWorkspaceID, proof, http.StatusForbidden)
	if status, enrolled := mseSourceState(t, fx, sourceID); status != "pending" || enrolled {
		t.Fatalf("must stay pending after re-add, got status=%q enrolled=%t", status, enrolled)
	}
	mseAssertNoSpawn(t, fx, agents, issues, tasks)
}

// TestNativeSourceEnrollmentAuthorityPinsAfterIssuance: after issuance the
// capability authority is re-derived live: role demotion 403, runtime
// incarnation (created_at) change 403, config revision change 409s the old
// capability, and a fresh approval at the new revision finalizes 200.
func TestNativeSourceEnrollmentAuthorityPinsAfterIssuance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	agents, issues, tasks := mseNoSpawnBaseline(t, fx)

	revision := int32(1)
	hash := strings.Repeat("9c", 32)

	// --- Demotion: fresh fixture, member -> 403. ---
	{
		member := fx.User(t, "mse demoted owner", "mse-demote-"+uuid.NewString()+"@example.test")
		fx.Member(t, testWorkspaceID, member, "admin")
		memberToken := msrLockJWT(t, member, time.Hour)
		runtimeID := nativeEnrollFixture(t, fx, member, "mse demotion runtime", "mse-demotion")
		sourceID, enrollmentID := nativeEnrollIntent(t, ctx, runtimeID, memberToken, uuid.NewString(), "native demotion")
		mse := nativeEnrollToken(t, ctx, runtimeID, sourceID, memberToken, enrollmentID, revision, hash)
		fx.Exec(t, `UPDATE member SET role='member' WHERE workspace_id=$1 AND user_id=$2`, testWorkspaceID, member)
		mustNativeEnrollCall(t, ctx, http.MethodPost, nativeEnrollFinalizePath(runtimeID, sourceID), mse,
			testWorkspaceID, nativeEnrollProofBody(enrollmentID, revision, hash), http.StatusForbidden)
		if status, enrolled := mseSourceState(t, fx, sourceID); status != "pending" || enrolled {
			t.Fatalf("demotion must leave pending, got status=%q enrolled=%t", status, enrolled)
		}
	}

	// --- Runtime incarnation change: created_at moved -> 403. ---
	{
		runtimeID := nativeEnrollFixture(t, fx, testUserID, "mse incarnation-bump runtime", "mse-created-bump")
		sourceID, enrollmentID := nativeEnrollIntent(t, ctx, runtimeID, testToken, uuid.NewString(), "native incarnation bump")
		mse := nativeEnrollToken(t, ctx, runtimeID, sourceID, testToken, enrollmentID, revision, hash)
		fx.Exec(t, `UPDATE agent_runtime SET created_at = created_at + interval '1 second' WHERE id=$1`, runtimeID)
		mustNativeEnrollCall(t, ctx, http.MethodPost, nativeEnrollFinalizePath(runtimeID, sourceID), mse,
			testWorkspaceID, nativeEnrollProofBody(enrollmentID, revision, hash), http.StatusForbidden)
		if status, enrolled := mseSourceState(t, fx, sourceID); status != "pending" || enrolled {
			t.Fatalf("incarnation change must leave pending, got status=%q enrolled=%t", status, enrolled)
		}
	}

	// --- Config revision via the real PATCH route: old cap 409, fresh
	// approval at the new revision finalizes 200. ---
	{
		runtimeID := nativeEnrollFixture(t, fx, testUserID, "mse revision runtime", "mse-revision")
		sourceID, enrollmentID := nativeEnrollIntent(t, ctx, runtimeID, testToken, uuid.NewString(), "native revision")
		mse := nativeEnrollToken(t, ctx, runtimeID, sourceID, testToken, enrollmentID, revision, hash)
		// Name-only PATCH bumps config_revision without touching mode.
		mustSourceReadCall(t, ctx, http.MethodPatch, "/api/work-sources/"+sourceID, testToken, testWorkspaceID,
			`{"name":"native revision renamed"}`, http.StatusOK)
		var current int
		fx.QueryRow(t, `SELECT config_revision FROM work_source WHERE id=$1`, sourceID).Scan(&current)
		if current != 2 {
			t.Fatalf("PATCH must bump config_revision to 2, got %d", current)
		}
		// The capability signed revision 1: proof matches the CAPABILITY, so
		// the old-revision proof passes the ingress binding but conflicts with
		// the row: 409, not a commit.
		mustNativeEnrollCall(t, ctx, http.MethodPost, nativeEnrollFinalizePath(runtimeID, sourceID), mse,
			testWorkspaceID, nativeEnrollProofBody(enrollmentID, revision, hash), http.StatusConflict)
		if status, enrolled := mseSourceState(t, fx, sourceID); status != "pending" || enrolled {
			t.Fatalf("stale revision must leave pending, got status=%q enrolled=%t", status, enrolled)
		}
		// Fresh approval: same enrollment, same pinned hash, NEW revision.
		newRevision := int32(current)
		mseNew := nativeEnrollToken(t, ctx, runtimeID, sourceID, testToken, enrollmentID, newRevision, hash)
		mustNativeEnrollCall(t, ctx, http.MethodPost, nativeEnrollFinalizePath(runtimeID, sourceID), mseNew,
			testWorkspaceID, nativeEnrollProofBody(enrollmentID, newRevision, hash), http.StatusOK)
		if status, enrolled := mseSourceState(t, fx, sourceID); status != "enrolled" || !enrolled {
			t.Fatalf("fresh approval must enroll, got status=%q enrolled=%t", status, enrolled)
		}
	}
	mseAssertNoSpawn(t, fx, agents, issues, tasks)
}
