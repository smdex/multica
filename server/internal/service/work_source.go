package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// WorkSource errors surfaced to handlers as 400/404/409. Only these typed
// errors describe client-visible outcomes; raw driver errors stay 500.
var (
	ErrWorkSourceInvalidInput  = errors.New("invalid work source request")
	ErrWorkSourceNotFound      = errors.New("work source not found")
	ErrWorkSourceOwnerConflict = errors.New("physical source already registered")
)

// WorkSourceService owns work_source and issue_work_link rows. TxStarter is
// required for destructive calls: delete-with-dependent-sweep must be atomic
// (no FKs by project rule).
type WorkSourceService struct {
	Queries   *db.Queries
	TxStarter TxStarter
}

// NewWorkSourceService wires queries plus the tx starter used by cascades.
// A nil TxStarter is rejected: non-atomic dependent cleanup would orphan
// links on partial failure.
func NewWorkSourceService(q *db.Queries, tx TxStarter) *WorkSourceService {
	return &WorkSourceService{Queries: q, TxStarter: tx}
}

func (s *WorkSourceService) runInTx(ctx context.Context, fn func(*db.Queries) error) error {
	if s.TxStarter == nil {
		return errors.New("work source service requires a transaction starter")
	}
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := fn(s.Queries.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CreateWorkSourceParams is the validated create input. ProjectID is a
// pointer so nil means an unassigned workspace source.
type CreateWorkSourceParams struct {
	WorkspaceID  pgtype.UUID
	ProjectID    *pgtype.UUID
	RuntimeID    pgtype.UUID
	Name         string
	SourceHandle string
	CreatedBy    pgtype.UUID
}

// CreateIssueWorkLinkParams is the validated link create input.
type CreateIssueWorkLinkParams struct {
	WorkspaceID pgtype.UUID
	IssueID     pgtype.UUID
	SourceID    pgtype.UUID
	NativeID    string
	CreatedBy   pgtype.UUID
}

// WorkSourceConfig is the mutable payload for updating a work source.
// Identity - workspace, project scope, runtime/daemon owner - and
// source_handle are immutable; updates change only name and optionally
// enabled. A nil Enabled keeps the stored flag so a rename can never
// silently re-enable a disabled source.
type WorkSourceConfig struct {
	Name    string
	Enabled *bool
}

// CreateWorkSource validates the owning runtime and optional project inside
// one transaction that holds the create-side locks (workspace and project
// FOR KEY SHARE), so the insert cannot commit after a concurrent workspace
// or project delete has swept sources. daemonID is derived server-side from
// the runtime row, never taken from the request. mode is pinned to
// 'observe' (read-only) per the qualified bd 1.3.1 source contract until a
// conditional-write path exists.
func (s *WorkSourceService) CreateWorkSource(ctx context.Context, p CreateWorkSourceParams) (db.WorkSource, error) {
	if p.Name == "" || p.SourceHandle == "" {
		return db.WorkSource{}, ErrWorkSourceInvalidInput
	}
	var created db.WorkSource
	err := s.runInTx(ctx, func(q *db.Queries) error {
		// Create-side lock: conflicts with workspace teardown's FOR UPDATE.
		if _, err := q.LockWorkspaceForChatSessionCreate(ctx, p.WorkspaceID); err != nil {
			return fmt.Errorf("lock workspace: %w", err)
		}
		// Runtime lock before validation: a plain read would let a concurrent
		// runtime delete commit between validation and INSERT, leaving a
		// dangling runtime_id (no FK). Lock order: workspace -> runtime ->
		// project. LockAgentRuntime is not workspace-scoped, so workspace
		// membership is verified by the read below inside the same tx.
		if _, err := q.LockAgentRuntime(ctx, p.RuntimeID); errors.Is(err, pgx.ErrNoRows) {
			return ErrWorkSourceInvalidInput
		} else if err != nil {
			return fmt.Errorf("lock runtime: %w", err)
		}
		runtime, err := q.GetAgentRuntimeForWorkspace(ctx, db.GetAgentRuntimeForWorkspaceParams{
			ID: p.RuntimeID, WorkspaceID: p.WorkspaceID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrWorkSourceInvalidInput
		}
		if err != nil {
			return fmt.Errorf("load runtime: %w", err)
		}
		// daemon IDs are opaque strings; reject missing values only.
		if !runtime.DaemonID.Valid || runtime.DaemonID.String == "" {
			return ErrWorkSourceInvalidInput
		}
		var projectID pgtype.UUID
		if p.ProjectID != nil {
			// Create-side lock: conflicts with project deletion's sweep.
			if _, err := q.LockProjectForChatSessionCreate(ctx, db.LockProjectForChatSessionCreateParams{
				ID: *p.ProjectID, WorkspaceID: p.WorkspaceID,
			}); errors.Is(err, pgx.ErrNoRows) {
				return ErrWorkSourceInvalidInput
			} else if err != nil {
				return fmt.Errorf("lock project: %w", err)
			}
			projectID = *p.ProjectID
		}
		if _, err := q.GetWorkSourceOwnerConflict(ctx, db.GetWorkSourceOwnerConflictParams{
			WorkspaceID: p.WorkspaceID, DaemonID: runtime.DaemonID.String, SourceHandle: p.SourceHandle,
		}); err == nil {
			return ErrWorkSourceOwnerConflict
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		row, err := q.CreateWorkSource(ctx, db.CreateWorkSourceParams{
			ID:           dbid.NewV7(),
			WorkspaceID:  p.WorkspaceID,
			ProjectID:    projectID,
			RuntimeID:    p.RuntimeID,
			DaemonID:     runtime.DaemonID.String,
			Name:         p.Name,
			Mode:         "observe",
			Enabled:      true,
			SourceHandle: p.SourceHandle,
			CreatedBy:    p.CreatedBy,
		})
		if err != nil {
			return err
		}
		created = row
		return nil
	})
	if err != nil {
		return db.WorkSource{}, err
	}
	return created, nil
}

// UpdateWorkSource renames or toggles the binding. The source UUID, scope,
// owner, and source_handle stay immutable; config_revision increments on
// every change. A nil cfg.Enabled leaves the stored flag untouched.
func (s *WorkSourceService) UpdateWorkSource(ctx context.Context, workspaceID pgtype.UUID, sourceID pgtype.UUID, cfg WorkSourceConfig) (db.WorkSource, error) {
	if cfg.Name == "" {
		return db.WorkSource{}, ErrWorkSourceInvalidInput
	}
	var enabled pgtype.Bool
	if cfg.Enabled != nil {
		enabled = pgtype.Bool{Bool: *cfg.Enabled, Valid: true}
	}
	row, err := s.Queries.UpdateWorkSourceConfig(ctx, db.UpdateWorkSourceConfigParams{
		ID: sourceID, WorkspaceID: workspaceID,
		Name: cfg.Name, Enabled: enabled,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// The SQL predicate atomically refuses enabling pending native intent.
		if current, lookupErr := s.Queries.GetWorkSourceInWorkspace(ctx, db.GetWorkSourceInWorkspaceParams{ID: sourceID, WorkspaceID: workspaceID}); lookupErr == nil && current.Mode == "native" && !current.NativeEnrolledAt.Valid && cfg.Enabled != nil && *cfg.Enabled {
			return db.WorkSource{}, ErrNativeEnrollmentConflict
		}
		return db.WorkSource{}, ErrWorkSourceNotFound
	}
	if err != nil {
		return db.WorkSource{}, err
	}
	return row, nil
}

// CreateIssueWorkLink links a resolved workspace Issue to a source-owned
// native work item inside one transaction holding the issue FOR KEY SHARE
// and the source FOR UPDATE, so the link cannot commit after a concurrent
// issue delete or source cascade delete. It never creates or deletes
// either object.
func (s *WorkSourceService) CreateIssueWorkLink(ctx context.Context, p CreateIssueWorkLinkParams) (db.IssueWorkLink, error) {
	if p.NativeID == "" {
		return db.IssueWorkLink{}, ErrWorkSourceInvalidInput
	}
	var created db.IssueWorkLink
	err := s.runInTx(ctx, func(q *db.Queries) error {
		if _, err := q.LockWorkspaceForChatSessionCreate(ctx, p.WorkspaceID); err != nil {
			return fmt.Errorf("lock workspace: %w", err)
		}
		// Create-side issue lock: conflicts with issue deletion's FOR UPDATE.
		if _, err := q.LockIssueForChannelMediaBind(ctx, db.LockIssueForChannelMediaBindParams{
			ID: p.IssueID, WorkspaceID: p.WorkspaceID,
		}); errors.Is(err, pgx.ErrNoRows) {
			return ErrWorkSourceInvalidInput
		} else if err != nil {
			return fmt.Errorf("lock issue: %w", err)
		}
		// Serializes against DeleteWorkSourceCascade, which takes the same
		// FOR UPDATE before sweeping this source's links.
		if _, err := q.LockWorkSourceForWrite(ctx, db.LockWorkSourceForWriteParams{
			ID: p.SourceID, WorkspaceID: p.WorkspaceID,
		}); errors.Is(err, pgx.ErrNoRows) {
			return ErrWorkSourceNotFound
		} else if err != nil {
			return fmt.Errorf("lock source: %w", err)
		}
		row, err := q.CreateIssueWorkLink(ctx, db.CreateIssueWorkLinkParams{
			ID: dbid.NewV7(), WorkspaceID: p.WorkspaceID, IssueID: p.IssueID,
			SourceID: p.SourceID, NativeID: p.NativeID, CreatedBy: p.CreatedBy,
		})
		if err != nil {
			return err
		}
		created = row
		return nil
	})
	if err != nil {
		return db.IssueWorkLink{}, err
	}
	return created, nil
}

// ListWorkSources lists the workspace's source bindings.
func (s *WorkSourceService) ListWorkSources(ctx context.Context, workspaceID pgtype.UUID) ([]db.WorkSource, error) {
	return s.Queries.ListWorkSourcesByWorkspace(ctx, workspaceID)
}

// DeleteWorkSourceCascade removes the binding and its links in one
// transaction, taking the source FOR UPDATE before the sweep so a
// concurrent link insert either commits first (and is swept) or waits and
// then fails the source lock; dependent cleanup is application code per the
// no-FK rule.
func (s *WorkSourceService) DeleteWorkSourceCascade(ctx context.Context, workspaceID, sourceID pgtype.UUID) error {
	return s.runInTx(ctx, func(q *db.Queries) error {
		if _, err := q.LockWorkspaceForChatSessionCreate(ctx, workspaceID); errors.Is(err, pgx.ErrNoRows) {
			return ErrWorkSourceNotFound
		} else if err != nil {
			return fmt.Errorf("lock workspace: %w", err)
		}
		if _, err := q.LockWorkSourceForWrite(ctx, db.LockWorkSourceForWriteParams{
			ID: sourceID, WorkspaceID: workspaceID,
		}); errors.Is(err, pgx.ErrNoRows) {
			return ErrWorkSourceNotFound
		} else if err != nil {
			return fmt.Errorf("lock source: %w", err)
		}
		if err := q.DeleteWorkflowRunsForSource(ctx, db.DeleteWorkflowRunsForSourceParams{SourceID: sourceID, WorkspaceID: workspaceID}); err != nil {
			return err
		}
		if _, err := q.DeleteWorkSourceCommandsForWorkSource(ctx, db.DeleteWorkSourceCommandsForWorkSourceParams{
			SourceID: sourceID, WorkspaceID: workspaceID,
		}); err != nil {
			return err
		}
		if _, err := q.DeleteIssueWorkLinksForWorkSource(ctx, db.DeleteIssueWorkLinksForWorkSourceParams{
			SourceID: sourceID, WorkspaceID: workspaceID,
		}); err != nil {
			return err
		}
		n, err := q.DeleteWorkSource(ctx, db.DeleteWorkSourceParams{ID: sourceID, WorkspaceID: workspaceID})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrWorkSourceNotFound
		}
		return nil
	})
}

// ListIssueWorkLinksByIssue lists links on one issue.
func (s *WorkSourceService) ListIssueWorkLinksByIssue(ctx context.Context, workspaceID, issueID pgtype.UUID) ([]db.IssueWorkLink, error) {
	return s.Queries.ListIssueWorkLinksByIssue(ctx, db.ListIssueWorkLinksByIssueParams{
		IssueID: issueID, WorkspaceID: workspaceID,
	})
}

// ListIssueWorkLinksBySource lists links on one source binding.
func (s *WorkSourceService) ListIssueWorkLinksBySource(ctx context.Context, workspaceID, sourceID pgtype.UUID) ([]db.IssueWorkLink, error) {
	return s.Queries.ListIssueWorkLinksBySource(ctx, db.ListIssueWorkLinksBySourceParams{
		SourceID: sourceID, WorkspaceID: workspaceID,
	})
}

// DeleteIssueWorkLink unlinks without any issue or source side effect.
func (s *WorkSourceService) DeleteIssueWorkLink(ctx context.Context, workspaceID, linkID pgtype.UUID) error {
	n, err := s.Queries.DeleteIssueWorkLink(ctx, db.DeleteIssueWorkLinkParams{
		ID: linkID, WorkspaceID: workspaceID,
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrWorkSourceNotFound
	}
	return nil
}
