package handler

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Pin the SQL guard itself, independently of registration's request preflight.
// Removing either ON CONFLICT predicate must fail even when a caller bypasses
// the handler's earlier ownership check.
func TestDaemonRegistrationUpsertOwnerGuards(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	foreign := dbfx.User(t, "Foreign registration owner", "upsert-owner-"+uuid.NewString()+"@example.test")
	for _, kind := range []string{"builtin", "profile"} {
		for _, ownerless := range []bool{false, true} {
			name := kind + "/owned"
			if ownerless {
				name = kind + "/ownerless"
			}
			t.Run(name, func(t *testing.T) {
				daemon := "upsert-owner-" + uuid.NewString()
				cols := testutil.Cols{"daemon_id": daemon, "provider": "owner-query-fixture"}
				profile := ""
				if kind == "profile" {
					profile = insertRuntimeProfileFixture(t, ctx, "Owner query fixture", "codex", "test-created-missing-runtime")
					cols["profile_id"] = profile
				}
				if ownerless {
					cols["owner_id"] = nil
				}
				victim := dbfx.Runtime(t, "Original runtime", cols)
				var err error
				if kind == "profile" {
					_, err = testHandler.Queries.UpsertAgentRuntimeWithProfile(ctx, db.UpsertAgentRuntimeWithProfileParams{
						WorkspaceID: parseUUID(testWorkspaceID), DaemonID: strToText(daemon), ProfileID: parseUUID(profile),
						Name: "Stolen runtime", RuntimeMode: "local", Provider: "owner-query-fixture", Status: "online",
						DeviceInfo: "", Metadata: []byte(`{"changed":true}`), OwnerID: parseUUID(foreign),
					})
				} else {
					_, err = testHandler.Queries.UpsertAgentRuntime(ctx, db.UpsertAgentRuntimeParams{
						WorkspaceID: parseUUID(testWorkspaceID), DaemonID: strToText(daemon),
						Name: "Stolen runtime", RuntimeMode: "local", Provider: "owner-query-fixture", Status: "online",
						DeviceInfo: "", Metadata: []byte(`{"changed":true}`), OwnerID: parseUUID(foreign),
					})
				}
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("guarded %s upsert: err=%v, want no rows", name, err)
				}
				var owner any = testUserID
				if ownerless {
					owner = nil
				}
				var unchanged bool
				dbfx.QueryRow(t, `SELECT name='Original runtime' AND metadata='{}'::jsonb AND owner_id IS NOT DISTINCT FROM $2::uuid FROM agent_runtime WHERE id=$1`, victim, owner).Scan(&unchanged)
				if !unchanged {
					t.Fatal("guarded upsert changed the existing runtime")
				}
			})
		}
	}
}
