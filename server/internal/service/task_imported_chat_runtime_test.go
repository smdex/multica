package service

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestImportedChatRuntimePinRejectsReboundAgentBeforeTaskCreation(t *testing.T) {
	ctx := context.Background()
	pool := newResolveOriginatorPool(t)
	q := db.New(pool)
	workspaceID, userID, agentID, _ := seedAttributionFixture(t, pool)
	session := seedImportedChatRuntimeSession(t, ctx, pool, workspaceID, agentID, userID)
	reboundRuntimeID := rebindChatAgentRuntime(t, ctx, pool, workspaceID, userID, agentID)

	agent, err := q.GetAgent(ctx, util.MustParseUUID(agentID))
	if err != nil {
		t.Fatalf("load rebound agent: %v", err)
	}
	if agent.RuntimeID != util.MustParseUUID(reboundRuntimeID) {
		t.Fatalf("agent runtime = %s, want rebound runtime %s", util.UUIDToString(agent.RuntimeID), reboundRuntimeID)
	}

	svc := &TaskService{Queries: q, TxStarter: pool, Bus: events.New()}
	for _, tc := range []struct {
		name    string
		enqueue func() error
	}{
		{
			name: "enqueue chat task",
			enqueue: func() error {
				_, err := svc.EnqueueChatTask(ctx, session, util.MustParseUUID(userID), false)
				return err
			},
		},
		{
			name: "send direct chat message",
			enqueue: func() error {
				_, err := svc.SendDirectChatMessage(ctx, session, agent, util.MustParseUUID(userID), "must not persist", nil, "member", util.MustParseUUID(userID))
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.enqueue(); !errors.Is(err, ErrChatTaskRuntimeMismatch) {
				t.Fatalf("enqueue error = %v, want ErrChatTaskRuntimeMismatch", err)
			}

			var taskCount int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, session.ID).Scan(&taskCount); err != nil {
				t.Fatalf("count chat tasks: %v", err)
			}
			if taskCount != 0 {
				t.Fatalf("tasks created for runtime-mismatched import = %d, want 0", taskCount)
			}
		})
	}
}

func TestNonImportedChatFollowsReboundAgentRuntime(t *testing.T) {
	ctx := context.Background()
	pool := newResolveOriginatorPool(t)
	q := db.New(pool)
	workspaceID, userID, agentID, _ := seedAttributionFixture(t, pool)

	var originalRuntimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&originalRuntimeID); err != nil {
		t.Fatalf("load original agent runtime: %v", err)
	}
	reboundRuntimeID := rebindChatAgentRuntime(t, ctx, pool, workspaceID, userID, agentID)
	var sessionID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO chat_session (workspace_id, agent_id, creator_id, runtime_id)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`, workspaceID, agentID, userID, originalRuntimeID).Scan(&sessionID); err != nil {
		t.Fatalf("seed ordinary chat session: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, sessionID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM chat_session WHERE id = $1`, sessionID)
	})
	task, err := (&TaskService{Queries: q, TxStarter: pool, Bus: events.New()}).EnqueueChatTask(ctx, db.ChatSession{
		ID:      util.MustParseUUID(sessionID),
		AgentID: util.MustParseUUID(agentID),
	}, util.MustParseUUID(userID), false)
	if err != nil {
		t.Fatalf("enqueue ordinary chat after rebind: %v", err)
	}
	if got := util.UUIDToString(task.RuntimeID); got != reboundRuntimeID {
		t.Fatalf("ordinary task runtime = %s, want rebound runtime %s", got, reboundRuntimeID)
	}
}

func seedImportedChatRuntimeSession(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID, agentID, userID string) db.ChatSession {
	t.Helper()

	var runtimeID, sessionID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID); err != nil {
		t.Fatalf("load imported chat runtime: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO chat_session (
			workspace_id, agent_id, creator_id, runtime_id,
			native_import_provider, native_import_id
		) VALUES ($1, $2, $3, $4, 'codex', $5)
		RETURNING id
	`, workspaceID, agentID, userID, runtimeID, "source-"+agentID).Scan(&sessionID); err != nil {
		t.Fatalf("seed imported chat session: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, sessionID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM chat_session WHERE id = $1`, sessionID)
	})

	return db.ChatSession{
		ID:             util.MustParseUUID(sessionID),
		AgentID:        util.MustParseUUID(agentID),
		RuntimeID:      util.MustParseUUID(runtimeID),
		NativeImportID: pgtype.Text{String: "source-" + agentID, Valid: true},
	}
}

func rebindChatAgentRuntime(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID, userID, agentID string) string {
	t.Helper()

	var originalRuntimeID, reboundRuntimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&originalRuntimeID); err != nil {
		t.Fatalf("load original runtime: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_runtime (workspace_id, name, runtime_mode, provider, status, device_info, metadata, owner_id)
		VALUES ($1, 'rebound imported-chat runtime', 'cloud', 'codex', 'online', '', '{}'::jsonb, $2)
		RETURNING id
	`, workspaceID, userID).Scan(&reboundRuntimeID); err != nil {
		t.Fatalf("seed rebound runtime: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent SET runtime_id = $1 WHERE id = $2`, reboundRuntimeID, agentID); err != nil {
		t.Fatalf("rebind agent runtime: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = pool.Exec(cleanupCtx, `UPDATE agent SET runtime_id = $1 WHERE id = $2`, originalRuntimeID, agentID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM agent_runtime WHERE id = $1`, reboundRuntimeID)
	})
	return reboundRuntimeID
}
