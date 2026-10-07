package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/daemonws"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// These requests use the production router and actual auth middleware, not
// test-injected daemon context. Runtime providers are inert fixture names.
func registrationRequest(ctx context.Context, token string, payload any) (int, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, testServer.URL+"/api/daemon/register", bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := testServer.Client().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

func registrationPAT(t *testing.T, fx *testutil.Fixture, userID string) string {
	t.Helper()
	token, err := auth.GeneratePATToken()
	if err != nil {
		t.Fatal(err)
	}
	fx.Insert(t, "personal_access_token", testutil.Cols{
		"user_id": userID, "name": "Registration fixture", "token_hash": auth.HashToken(token),
		"token_prefix": token[:12], "expires_at": time.Now().Add(time.Hour),
	})
	return token
}

func TestDaemonRegistrationOwnershipThroughRouter(t *testing.T) {
	for _, role := range []string{"member", "admin"} {
		for _, kind := range []string{"builtin", "profile", "failed-profile", "ownerless", "ownerless-profile"} {
			t.Run(role+"/"+kind, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				fx := testutil.New(testPool, testWorkspaceID, testUserID)
				actor := fx.User(t, "Registration actor", "registration-"+uuid.NewString()+"@example.test")
				fx.Member(t, testWorkspaceID, actor, role)
				token, err := generateTestJWT(actor, "", "Registration actor")
				if err != nil {
					t.Fatal(err)
				}
				ownerToken := testToken
				// Cover both real middleware credential paths without duplicating
				// the ownership matrix: member uses PAT, admin uses JWT.
				if role == "member" {
					token = registrationPAT(t, fx, actor)
					ownerToken = registrationPAT(t, fx, testUserID)
				}
				daemon := "registration-" + uuid.NewString()
				cols := testutil.Cols{"daemon_id": daemon, "provider": "registration-fixture", "name": "Original name"}
				entry := map[string]any{"type": "registration-fixture", "name": "Stolen name"}
				if kind == "profile" || kind == "failed-profile" || kind == "ownerless-profile" {
					profile := fx.Insert(t, "runtime_profile", testutil.Cols{
						"workspace_id": testWorkspaceID, "display_name": "Registration fixture", "protocol_family": "codex",
						"command_name": "test-created-missing-runtime", "created_by": testUserID,
					})
					cols["profile_id"] = profile
					entry["profile_id"] = profile
				}
				if kind == "ownerless" || kind == "ownerless-profile" {
					cols["owner_id"] = nil
				}
				victim := fx.Runtime(t, "Original name", cols)
				payload := map[string]any{
					"workspace_id": testWorkspaceID, "daemon_id": daemon,
					// The first row would be newly inserted before the denied row.
					"runtimes": []map[string]any{{"type": "earlier-fixture", "name": "Must roll back"}, entry},
				}
				if kind == "failed-profile" {
					payload["runtimes"] = []map[string]any{{"type": "earlier-fixture"}}
					payload["failed_profiles"] = []map[string]any{entry}
				}
				fx.Cleanup(t, `DELETE FROM agent_runtime WHERE workspace_id=$1 AND daemon_id=$2`, testWorkspaceID, daemon)
				status, body, err := registrationRequest(ctx, token, payload)
				if err != nil || status != http.StatusForbidden {
					t.Fatalf("foreign registration: status=%d err=%v body=%s", status, err, body)
				}
				var unchanged bool
				fx.QueryRow(t, `SELECT name='Original name' AND metadata='{}'::jsonb AND owner_id IS NOT DISTINCT FROM $2::uuid FROM agent_runtime WHERE id=$1`, victim, colsOwner(kind, testUserID)).Scan(&unchanged)
				if !unchanged {
					t.Fatal("denied registration changed runtime ownership or metadata")
				}
				if n := fx.Count(t, `SELECT count(*) FROM agent_runtime WHERE workspace_id=$1 AND daemon_id=$2 AND provider='earlier-fixture'`, testWorkspaceID, daemon); n != 0 {
					t.Fatalf("denied registration committed %d earlier rows", n)
				}
				if kind != "ownerless" && kind != "ownerless-profile" {
					status, body, err = registrationRequest(ctx, ownerToken, payload)
					if err != nil || status != http.StatusOK {
						t.Fatalf("same-owner reconnect: status=%d err=%v body=%s", status, err, body)
					}
					fx.QueryRow(t, `SELECT owner_id=$2::uuid FROM agent_runtime WHERE id=$1`, victim, testUserID).Scan(&unchanged)
					if !unchanged {
						t.Fatal("same-owner reconnect changed owner")
					}
				}
			})
		}
	}
}

func colsOwner(kind, owner string) any {
	if kind == "ownerless" || kind == "ownerless-profile" {
		return nil
	}
	return owner
}

func TestDaemonRegistrationSkippedFailedProfilesThroughRouter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	profile := fx.Insert(t, "runtime_profile", testutil.Cols{
		"workspace_id": testWorkspaceID, "display_name": "Disabled registration fixture", "protocol_family": "codex",
		"command_name": "test-created-missing-runtime", "created_by": testUserID, "enabled": false,
	})
	daemon := "skipped-profiles-" + uuid.NewString()
	fx.Cleanup(t, `DELETE FROM agent_runtime WHERE workspace_id=$1 AND daemon_id=$2`, testWorkspaceID, daemon)
	status, body, err := registrationRequest(ctx, testToken, map[string]any{
		"workspace_id": testWorkspaceID, "daemon_id": daemon,
		"runtimes":        []map[string]any{{"type": "registration-fixture"}},
		"failed_profiles": []map[string]any{{"profile_id": profile}, {"profile_id": uuid.NewString()}, {"profile_id": ""}},
	})
	if err != nil || status != http.StatusOK {
		t.Fatalf("skipped failed profiles: status=%d err=%v body=%s", status, err, body)
	}
	if n := fx.Count(t, `SELECT count(*) FROM agent_runtime WHERE workspace_id=$1 AND daemon_id=$2 AND profile_id IS NULL`, testWorkspaceID, daemon); n != 1 {
		t.Fatalf("ordinary runtime was not registered: count=%d", n)
	}
	if n := fx.Count(t, `SELECT count(*) FROM agent_runtime WHERE workspace_id=$1 AND daemon_id=$2 AND profile_id IS NOT NULL`, testWorkspaceID, daemon); n != 0 {
		t.Fatalf("disabled/unknown failed profiles registered %d runtimes", n)
	}
}

func TestDaemonRegistrationDaemonTokenIdentityThroughRouter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	daemon := "mdt-register-" + uuid.NewString()
	token, err := auth.GenerateDaemonToken()
	if err != nil {
		t.Fatal(err)
	}
	fx.Insert(t, "daemon_token", testutil.Cols{
		"workspace_id": testWorkspaceID, "daemon_id": daemon, "token_hash": auth.HashToken(token), "expires_at": time.Now().Add(time.Minute),
	})
	payload := map[string]any{"workspace_id": testWorkspaceID, "daemon_id": daemon + "-foreign", "runtimes": []map[string]any{{"type": "registration-fixture"}}}
	fx.Cleanup(t, `DELETE FROM agent_runtime WHERE workspace_id=$1 AND daemon_id IN ($2,$3)`, testWorkspaceID, daemon, daemon+"-foreign")
	status, body, err := registrationRequest(ctx, token, payload)
	if err != nil || status != http.StatusForbidden {
		t.Fatalf("MDT foreign daemon: status=%d err=%v body=%s", status, err, body)
	}
	if n := fx.Count(t, `SELECT count(*) FROM agent_runtime WHERE workspace_id=$1 AND daemon_id=$2`, testWorkspaceID, daemon+"-foreign"); n != 0 {
		t.Fatalf("MDT impersonation inserted %d runtimes", n)
	}
	payload["daemon_id"] = daemon
	payload["legacy_daemon_ids"] = []string{"untrusted-old-daemon"}
	status, body, err = registrationRequest(ctx, token, payload)
	if err != nil || status != http.StatusForbidden {
		t.Fatalf("MDT legacy merge: status=%d err=%v body=%s", status, err, body)
	}
	delete(payload, "legacy_daemon_ids")
	status, body, err = registrationRequest(ctx, token, payload)
	if err != nil || status != http.StatusOK {
		t.Fatalf("MDT same daemon: status=%d err=%v body=%s", status, err, body)
	}
	if n := fx.Count(t, `SELECT count(*) FROM agent_runtime WHERE workspace_id=$1 AND daemon_id=$2 AND owner_id IS NULL`, testWorkspaceID, daemon); n != 1 {
		t.Fatalf("legacy MDT must not invent an owner: count=%d", n)
	}
}

func TestDaemonRegistrationLegacyOwnershipThroughRouter(t *testing.T) {
	for _, kind := range []string{"foreign-member", "foreign-admin", "ownerless", "same-owner"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			fx := testutil.New(testPool, testWorkspaceID, testUserID)
			token := testToken
			if kind == "foreign-member" || kind == "foreign-admin" {
				actor := fx.User(t, "Legacy actor", "legacy-"+uuid.NewString()+"@example.test")
				role := "member"
				if kind == "foreign-admin" {
					role = "admin"
				}
				fx.Member(t, testWorkspaceID, actor, role)
				var err error
				token, err = generateTestJWT(actor, "", "Legacy actor")
				if err != nil {
					t.Fatal(err)
				}
			}
			oldDaemon, newDaemon := "legacy-"+uuid.NewString(), "new-"+uuid.NewString()
			cols := testutil.Cols{"daemon_id": oldDaemon, "provider": "registration-fixture"}
			if kind == "ownerless" || kind == "ownerless-profile" {
				cols["owner_id"] = nil
			}
			old := fx.Runtime(t, "Legacy victim", cols)
			agent := fx.Agent(t, "Legacy agent", old)
			payload := map[string]any{
				"workspace_id": testWorkspaceID, "daemon_id": newDaemon, "legacy_daemon_ids": []string{oldDaemon},
				"runtimes": []map[string]any{{"type": "registration-fixture"}},
			}
			fx.Cleanup(t, `DELETE FROM agent_runtime WHERE workspace_id=$1 AND daemon_id=$2`, testWorkspaceID, newDaemon)
			status, body, err := registrationRequest(ctx, token, payload)
			want := http.StatusForbidden
			if kind == "same-owner" {
				want = http.StatusOK
			}
			if err != nil || status != want {
				t.Fatalf("legacy registration: status=%d want=%d err=%v body=%s", status, want, err, body)
			}
			if kind != "same-owner" {
				if n := fx.Count(t, `SELECT count(*) FROM agent_runtime WHERE id=$1`, old); n != 1 {
					t.Fatal("foreign/ownerless legacy runtime was deleted")
				}
				if n := fx.Count(t, `SELECT count(*) FROM agent WHERE id=$1 AND runtime_id=$2`, agent, old); n != 1 {
					t.Fatal("foreign/ownerless legacy agent was moved")
				}
				if n := fx.Count(t, `SELECT count(*) FROM agent_runtime WHERE workspace_id=$1 AND daemon_id=$2`, testWorkspaceID, newDaemon); n != 0 {
					t.Fatal("rejected legacy registration committed the new runtime")
				}
			} else if n := fx.Count(t, `SELECT count(*) FROM agent a JOIN agent_runtime r ON r.id=a.runtime_id WHERE a.id=$1 AND r.daemon_id=$2 AND r.owner_id=$3`, agent, newDaemon, testUserID); n != 1 {
				t.Fatal("same-owner legacy merge did not preserve the agent")
			}
		})
	}
}

func TestDaemonRegistrationSourceBoundRollbackThroughRouter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	oldDaemon, newDaemon := "source-old-"+uuid.NewString(), "source-new-"+uuid.NewString()
	old := fx.Runtime(t, "Source-bound legacy", testutil.Cols{"daemon_id": oldDaemon, "provider": "registration-fixture"})
	agent := fx.Agent(t, "Source-bound agent", old)
	source := fx.Insert(t, "work_source", testutil.Cols{
		"id": testutil.Raw("gen_random_uuid()"), "workspace_id": testWorkspaceID,
		"runtime_id": old, "daemon_id": oldDaemon, "name": "Registration rollback source",
		"source_handle": "rollback-approved-handle", "mode": "observe",
	})
	fx.Cleanup(t, `DELETE FROM agent_runtime WHERE workspace_id=$1 AND daemon_id=$2`, testWorkspaceID, newDaemon)
	hits := daemonws.M.RuntimeGoneDeliveredHit.Load()
	misses := daemonws.M.RuntimeGoneDeliveredMiss.Load()
	status, body, err := registrationRequest(ctx, testToken, map[string]any{
		"workspace_id": testWorkspaceID, "daemon_id": newDaemon, "legacy_daemon_ids": []string{oldDaemon},
		"runtimes": []map[string]any{{"type": "registration-fixture"}},
	})
	if err != nil || status != http.StatusConflict {
		t.Fatalf("source-bound merge: status=%d want=409 err=%v body=%s", status, err, body)
	}
	if n := fx.Count(t, `SELECT count(*) FROM agent_runtime WHERE workspace_id=$1 AND daemon_id=$2`, testWorkspaceID, newDaemon); n != 0 {
		t.Fatal("late source fence committed the earlier new-runtime upsert")
	}
	if n := fx.Count(t, `SELECT count(*) FROM work_source s JOIN agent_runtime r ON r.id=s.runtime_id WHERE s.id=$1 AND r.id=$2 AND r.owner_id=$3 AND s.daemon_id=$4`, source, old, testUserID, oldDaemon); n != 1 {
		t.Fatal("late source fence changed source/runtime identity")
	}
	if n := fx.Count(t, `SELECT count(*) FROM agent WHERE id=$1 AND runtime_id=$2`, agent, old); n != 1 {
		t.Fatal("late source fence moved the legacy agent")
	}
	if daemonws.M.RuntimeGoneDeliveredHit.Load() != hits || daemonws.M.RuntimeGoneDeliveredMiss.Load() != misses {
		t.Fatal("rolled-back registration emitted runtime-gone notification")
	}
}

func TestDaemonRegistrationMembershipRevokeThroughRouter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	owner := fx.User(t, "Revoked runtime owner", "revoke-register-"+uuid.NewString()+"@example.test")
	member := fx.Member(t, testWorkspaceID, owner, "member")
	token, err := generateTestJWT(owner, "", "Revoked owner")
	if err != nil {
		t.Fatal(err)
	}
	daemon := "revoke-register-" + uuid.NewString()
	runtime := fx.Runtime(t, "Revoked runtime", testutil.Cols{"owner_id": owner, "daemon_id": daemon})
	gate, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	var gatePID int
	if err := gate.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&gatePID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.New(gate).LockAgentRuntime(ctx, pgtype.UUID{Bytes: uuid.MustParse(runtime), Valid: true}); err != nil {
		t.Fatal(err)
	}
	type response struct {
		status int
		body   []byte
		err    error
	}
	revoked := make(chan response, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, testServer.URL+"/api/workspaces/"+testWorkspaceID+"/members/"+member, nil)
		if err != nil {
			revoked <- response{err: err}
			return
		}
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := testServer.Client().Do(req)
		if err != nil {
			revoked <- response{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		revoked <- response{resp.StatusCode, body, err}
	}()
	// Revoke owns the membership advisory lock before blocking on our runtime.
	var revokePID int
	for revokePID == 0 {
		if err := testPool.QueryRow(ctx, `SELECT COALESCE((SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND $1::int=ANY(pg_blocking_pids(pid)) AND wait_event_type='Lock' LIMIT 1),0)`, gatePID).Scan(&revokePID); err != nil {
			t.Fatal(err)
		}
		select {
		case r := <-revoked:
			t.Fatalf("revoke did not wait: %+v", r)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
		}
		if revokePID == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	registered := make(chan response, 1)
	payload := map[string]any{"workspace_id": testWorkspaceID, "daemon_id": daemon, "runtimes": []map[string]any{{"type": "new-after-revoke-fixture"}}}
	fx.Cleanup(t, `DELETE FROM agent_runtime WHERE workspace_id=$1 AND daemon_id=$2`, testWorkspaceID, daemon)
	go func() {
		status, body, err := registrationRequest(ctx, token, payload)
		registered <- response{status, body, err}
	}()
	for {
		var waiting bool
		if err := testPool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1::int=ANY(pg_blocking_pids(pid)) AND wait_event='advisory')`, revokePID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case r := <-registered:
			t.Fatalf("registration escaped revoke serialization: status=%d err=%v body=%s", r.status, r.err, r.body)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for label, ch := range map[string]<-chan response{"revoke": revoked, "registration": registered} {
		select {
		case r := <-ch:
			want := http.StatusNoContent
			if label == "registration" {
				want = http.StatusNotFound
			}
			if r.err != nil || r.status != want {
				t.Fatalf("%s status=%d want=%d err=%v body=%s", label, r.status, want, r.err, r.body)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if n := fx.Count(t, `SELECT count(*) FROM agent_runtime WHERE workspace_id=$1 AND daemon_id=$2 AND provider='new-after-revoke-fixture'`, testWorkspaceID, daemon); n != 0 {
		t.Fatalf("post-revoke registration committed %d runtime rows", n)
	}
}
