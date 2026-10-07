package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/pkg/beads"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// WorkSourceCommand errors surfaced to handlers as 400/404/409. Reads only:
// the T01 qualification (bd 1.3.1) found no revision precondition or
// atomic create attribution, so no write commands exist in the allowlist.
var (
	ErrWorkSourceCommandInvalidInput = errors.New("invalid source command request")
	ErrWorkSourceCommandNotFound     = errors.New("source command not found")
	ErrWorkSourceCommandConflict     = errors.New("another source command is in flight")
	ErrWorkSourceCommandNotClaimable = errors.New("source command is not pending")
	ErrWorkSourceCommandNotClaimer   = errors.New("runtime did not claim this source command")
	ErrWorkSourceCommandDisabled     = errors.New("work source is disabled")
)

// WorkSourceCommandName is the reviewed read-only allowlist. Adding an
// entry requires a design decision, never a string from a request.
type WorkSourceCommandName string

const (
	WorkSourceCommandList WorkSourceCommandName = "list"
	WorkSourceCommandRead WorkSourceCommandName = "read"
)

// maxWorkSourceCommandLimit bounds list size; 0 uses the source default.
const maxWorkSourceCommandLimit = 200

// WorkSourceCommandService owns read-only receipts. All mutations lock the
// source before command rows, sharing the source deletion/configuration fence.
// Claim/report also lock the exact runtime's identity and online status.
type WorkSourceCommandService struct {
	Queries   *db.Queries
	TxStarter TxStarter
}

func NewWorkSourceCommandService(q *db.Queries, tx TxStarter) *WorkSourceCommandService {
	return &WorkSourceCommandService{Queries: q, TxStarter: tx}
}

func (s *WorkSourceCommandService) runInTx(ctx context.Context, fn func(*db.Queries) error) error {
	if s.TxStarter == nil {
		return errors.New("work source command service requires a transaction starter")
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

// CreateWorkSourceCommandParams is the validated user create input.
// NativeID is required for 'read' and forbidden for 'list'; Limit applies to
// 'list' only (0 = default50). RequestID is the stable client-generated UUID.
type CreateWorkSourceCommandParams struct {
	WorkspaceID pgtype.UUID
	SourceID    pgtype.UUID
	Command     WorkSourceCommandName
	NativeID    string
	Limit       int32
	CreatedBy   pgtype.UUID
	RequestID   pgtype.UUID
}

// workSourceCommandRequestHash fingerprints the validated command payload.
// Reusing a client request UUID with a changed payload conflicts even after
// terminal completion; an identical retry returns the original receipt.
func workSourceCommandRequestHash(p CreateWorkSourceCommandParams) string {
	limit := int32(0)
	if p.Command == WorkSourceCommandList {
		if p.Limit > 0 {
			limit = p.Limit
		}
	}
	native := ""
	if p.Command == WorkSourceCommandRead {
		native = p.NativeID
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x1f%s\x1f%d", p.Command, native, limit)))
	return hex.EncodeToString(sum[:])
}

// CreateWorkSourceCommand locks the source, expires stale reads, and checks
// stable request identity before enforcing enabled/owner guards for new work.
// Both native and observe sources support reads, never native Start or writes.
// Created distinguishes a new pending receipt (201) from a durable replay (200).
func (s *WorkSourceCommandService) CreateWorkSourceCommand(ctx context.Context, p CreateWorkSourceCommandParams) (cmd db.WorkSourceCommand, created bool, err error) {
	if !p.RequestID.Valid || p.RequestID.Bytes == [16]byte{} {
		return cmd, false, ErrWorkSourceCommandInvalidInput
	}
	switch p.Command {
	case WorkSourceCommandList:
		if p.NativeID != "" || p.Limit < 0 || p.Limit > maxWorkSourceCommandLimit {
			return db.WorkSourceCommand{}, false, ErrWorkSourceCommandInvalidInput
		}
	case WorkSourceCommandRead:
		if strings.TrimSpace(p.NativeID) == "" || p.Limit != 0 {
			return db.WorkSourceCommand{}, false, ErrWorkSourceCommandInvalidInput
		}
	default:
		return db.WorkSourceCommand{}, false, ErrWorkSourceCommandInvalidInput
	}

	var nativeID pgtype.Text
	if p.Command == WorkSourceCommandRead {
		nativeID = pgtype.Text{String: p.NativeID, Valid: true}
	}
	var limit pgtype.Int4
	if p.Command == WorkSourceCommandList && p.Limit > 0 {
		limit = pgtype.Int4{Int32: p.Limit, Valid: true}
	}
	requestHash := workSourceCommandRequestHash(p)

	err = s.runInTx(ctx, func(q *db.Queries) error {
		// Same FOR UPDATE as the cascade delete: the command cannot commit
		// after DeleteWorkSourceCascade has swept, and the in-flight check
		// below cannot race that sweep. The lock returns only the id; the
		// guards are read right after it under the same lock.
		if _, lockErr := q.LockWorkSourceForWrite(ctx, db.LockWorkSourceForWriteParams{
			ID: p.SourceID, WorkspaceID: p.WorkspaceID,
		}); errors.Is(lockErr, pgx.ErrNoRows) {
			return ErrWorkSourceNotFound
		} else if lockErr != nil {
			return fmt.Errorf("lock source: %w", lockErr)
		}
		source, srcErr := q.GetWorkSourceInWorkspace(ctx, db.GetWorkSourceInWorkspaceParams{
			ID: p.SourceID, WorkspaceID: p.WorkspaceID,
		})
		if errors.Is(srcErr, pgx.ErrNoRows) {
			return ErrWorkSourceNotFound
		}
		if srcErr != nil {
			return fmt.Errorf("load source: %w", srcErr)
		}
		if _, err := q.ExpireWorkSourceCommandsForSource(ctx, db.ExpireWorkSourceCommandsForSourceParams{SourceID: p.SourceID, WorkspaceID: p.WorkspaceID}); err != nil {
			return err
		}
		prior, priorErr := q.GetWorkSourceCommandByRequest(ctx, db.GetWorkSourceCommandByRequestParams{SourceID: p.SourceID, WorkspaceID: p.WorkspaceID, RequestID: p.RequestID})
		if priorErr == nil {
			if prior.RequestHash != requestHash {
				return ErrWorkSourceCommandConflict
			}
			cmd, created = prior, false
			return nil
		}
		if !errors.Is(priorErr, pgx.ErrNoRows) {
			return priorErr
		}
		if !source.Enabled {
			return ErrWorkSourceCommandDisabled
		}
		if source.DaemonID == "" {
			// Owner guard precondition: without a daemon owner there is no
			// runtime that may ever claim this command.
			return ErrWorkSourceCommandInvalidInput
		}
		// In-flight check under the source lock; the partial unique index
		// backstops this against cross-transaction races.
		_, getErr := q.GetWorkSourceCommandInFlightForUpdate(ctx, db.GetWorkSourceCommandInFlightForUpdateParams{
			SourceID: p.SourceID, WorkspaceID: p.WorkspaceID,
		})
		if getErr == nil {
			return ErrWorkSourceCommandConflict
		}
		if !errors.Is(getErr, pgx.ErrNoRows) {
			return getErr
		}
		row, insertErr := q.CreateWorkSourceCommand(ctx, db.CreateWorkSourceCommandParams{
			ID:             dbid.NewV7(),
			WorkspaceID:    p.WorkspaceID,
			SourceID:       p.SourceID,
			Command:        string(p.Command),
			NativeID:       nativeID,
			LimitCount:     limit,
			RequestHash:    requestHash,
			CreatedBy:      p.CreatedBy,
			RequestID:      p.RequestID,
			ConfigRevision: source.ConfigRevision,
		})
		if insertErr != nil {
			return insertErr
		}
		cmd, created = row, true
		return nil
	})
	if err != nil {
		return db.WorkSourceCommand{}, false, err
	}
	return cmd, created, nil
}

// commandForOwnerRuntime locks runtime before source before command, matching
// workspace deletion's runtime-preparation lock order. The initial command
// read only locates the source. Identity/status, configuration, deletion and
// terminal replay remain fenced until this transaction commits.
func (s *WorkSourceCommandService) commandForOwnerRuntime(ctx context.Context, q *db.Queries, workspaceID, commandID pgtype.UUID, runtime db.AgentRuntime) (db.WorkSourceCommand, error) {
	cmd, err := q.GetWorkSourceCommandInWorkspace(ctx, db.GetWorkSourceCommandInWorkspaceParams{
		ID: commandID, WorkspaceID: workspaceID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.WorkSourceCommand{}, ErrWorkSourceCommandNotFound
	}
	if err != nil {
		return db.WorkSourceCommand{}, err
	}
	// Workspace deletion holds runtime FOR UPDATE before touching sources.
	authenticatedDaemon := runtime.DaemonID
	runtime, err = q.GetWorkSourceCommandRuntimeForShare(ctx, runtime.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return cmd, ErrWorkSourceCommandNotClaimer
	}
	if err != nil {
		return cmd, err
	}
	// Source remains before command, matching create, deletion and expiration.
	if _, err := q.LockWorkSourceForWrite(ctx, db.LockWorkSourceForWriteParams{ID: cmd.SourceID, WorkspaceID: workspaceID}); errors.Is(err, pgx.ErrNoRows) {
		return db.WorkSourceCommand{}, ErrWorkSourceCommandNotFound
	} else if err != nil {
		return cmd, err
	}
	cmd, err = q.GetWorkSourceCommandForUpdate(ctx, db.GetWorkSourceCommandForUpdateParams{ID: commandID, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return cmd, ErrWorkSourceCommandNotFound
	}
	if err != nil {
		return cmd, err
	}
	source, err := q.GetWorkSourceInWorkspace(ctx, db.GetWorkSourceInWorkspaceParams{
		ID: cmd.SourceID, WorkspaceID: workspaceID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Swept by a concurrent cascade delete: not claimable/reportable.
		return db.WorkSourceCommand{}, ErrWorkSourceCommandNotFound
	}
	if err != nil {
		return db.WorkSourceCommand{}, err
	}
	if !runtime.DaemonID.Valid || runtime.DaemonID != authenticatedDaemon || source.DaemonID != runtime.DaemonID.String || source.RuntimeID != runtime.ID || runtime.WorkspaceID != workspaceID || runtime.Status != "online" {
		// Owner routing: only the runtime bound to this source's daemon may
		// claim or report. A different daemon under the same workspace is
		// a routing error, not a 404: answer not-claimable.
		return db.WorkSourceCommand{}, ErrWorkSourceCommandNotClaimer
	}
	if !source.Enabled {
		return cmd, ErrWorkSourceCommandDisabled
	}
	if source.ConfigRevision != cmd.ConfigRevision {
		return cmd, ErrWorkSourceCommandConflict
	}
	return cmd, nil
}

// ClaimWorkSourceCommand transitions pending -> claimed for the owner
// runtime. Exactly one concurrent claimer wins; others get
// ErrWorkSourceCommandNotClaimable.
func (s *WorkSourceCommandService) ClaimWorkSourceCommand(ctx context.Context, workspaceID, commandID pgtype.UUID, runtime db.AgentRuntime, capability *auth.SourceReadClaims) (db.WorkSourceCommand, error) {
	var claimed db.WorkSourceCommand
	err := s.runInTx(ctx, func(q *db.Queries) error {
		var parentExpiresAt time.Time
		if capability != nil {
			owner, err := authorizeSourceRead(ctx, q, *capability)
			if err != nil {
				return err
			}
			if owner.runtime.ID != runtime.ID || owner.runtime.WorkspaceID != workspaceID {
				return ErrSourceReadForbidden
			}
			runtime, parentExpiresAt = owner.runtime, owner.parentExpiresAt
		}
		cmd, err := s.commandForOwnerRuntime(ctx, q, workspaceID, commandID, runtime)
		if err != nil {
			return err
		}
		if capability != nil {
			if err := checkSourceReadTime(*capability, parentExpiresAt); err != nil {
				return err
			}
		}
		if cmd.Status != "pending" {
			return ErrWorkSourceCommandNotClaimable
		}
		if !cmd.ExpiresAt.Valid || !cmd.ExpiresAt.Time.After(time.Now()) {
			return ErrWorkSourceCommandNotClaimable
		}
		row, err := q.ClaimWorkSourceCommand(ctx, db.ClaimWorkSourceCommandParams{
			ID: cmd.ID, WorkspaceID: workspaceID, RuntimeID: runtime.ID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrWorkSourceCommandNotClaimable
		}
		if err != nil {
			return err
		}
		claimed = row
		return nil
	})
	if err != nil {
		return db.WorkSourceCommand{}, err
	}
	return claimed, nil
}

// ReportWorkSourceCommand records a bounded, typed result or diagnostic from
// the actual claiming runtime. Locked terminal replay succeeds only when the
// canonical status/result/error matches the committed receipt exactly.
func (s *WorkSourceCommandService) ReportWorkSourceCommand(ctx context.Context, p ReportWorkSourceCommandParams) (db.WorkSourceCommand, error) {
	var reported db.WorkSourceCommand
	err := s.runInTx(ctx, func(q *db.Queries) error {
		var parentExpiresAt time.Time
		if p.Capability != nil {
			owner, err := authorizeSourceRead(ctx, q, *p.Capability)
			if err != nil {
				return err
			}
			if owner.runtime.ID != p.Runtime.ID || owner.runtime.WorkspaceID != p.WorkspaceID {
				return ErrSourceReadForbidden
			}
			p.Runtime, parentExpiresAt = owner.runtime, owner.parentExpiresAt
		}
		cmd, err := s.commandForOwnerRuntime(ctx, q, p.WorkspaceID, p.CommandID, p.Runtime)
		if err != nil {
			return err
		}
		if p.Capability != nil {
			if err := checkSourceReadTime(*p.Capability, parentExpiresAt); err != nil {
				return err
			}
		}
		if !cmd.ClaimedRuntimeID.Valid || cmd.ClaimedRuntimeID != p.Runtime.ID {
			return ErrWorkSourceCommandNotClaimer
		}
		if cmd.Status == "succeeded" || cmd.Status == "failed" {
			if validateWorkSourceCommandReport(cmd, &p) != nil {
				return ErrWorkSourceCommandConflict
			}
			if cmd.Status != p.Status || cmd.Result != p.Result || cmd.Error != p.Error {
				return ErrWorkSourceCommandConflict
			}
			// Idempotent replay of a report the server already committed.
			reported = cmd
			return nil
		}
		if err := validateWorkSourceCommandReport(cmd, &p); err != nil {
			return err
		}
		if !cmd.ExpiresAt.Valid || !cmd.ExpiresAt.Time.After(time.Now()) {
			return ErrWorkSourceCommandNotClaimable
		}
		if cmd.Status != "claimed" || !cmd.ClaimedRuntimeID.Valid || cmd.ClaimedRuntimeID.Bytes != p.Runtime.ID.Bytes {
			return ErrWorkSourceCommandNotClaimer
		}
		switch p.Status {
		case "succeeded":
			row, err := q.CompleteWorkSourceCommand(ctx, db.CompleteWorkSourceCommandParams{
				ID: cmd.ID, WorkspaceID: p.WorkspaceID,
				RuntimeID: p.Runtime.ID, Result: p.Result,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrWorkSourceCommandNotClaimer
			}
			if err != nil {
				return err
			}
			reported = row
		case "failed":
			row, err := q.FailWorkSourceCommand(ctx, db.FailWorkSourceCommandParams{
				ID: cmd.ID, WorkspaceID: p.WorkspaceID,
				RuntimeID: p.Runtime.ID, Error: p.Error,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrWorkSourceCommandNotClaimer
			}
			if err != nil {
				return err
			}
			reported = row
		default:
			return ErrWorkSourceCommandInvalidInput
		}
		return nil
	})
	if err != nil {
		return db.WorkSourceCommand{}, err
	}
	return reported, nil
}

// ReportWorkSourceCommandParams is the validated daemon report input.
// Status must be 'succeeded' or 'failed'; Result/Error are pre-bounded by
// the handler.
type ReportWorkSourceCommandParams struct {
	Capability  *auth.SourceReadClaims
	WorkspaceID pgtype.UUID
	CommandID   pgtype.UUID
	Runtime     db.AgentRuntime
	Status      string
	Result      pgtype.Text
	Error       pgtype.Text
}

// validateWorkSourceCommandReport canonicalizes only the qualified typed payload.
// json.Unmarshal rejects trailing JSON, and explicit shape checks reject null.
func validateWorkSourceCommandReport(cmd db.WorkSourceCommand, p *ReportWorkSourceCommandParams) error {
	if len(p.Result.String) > 2<<20 || len(p.Error.String) > 8<<10 {
		return ErrWorkSourceCommandInvalidInput
	}
	if p.Status == "failed" {
		if p.Result.Valid || strings.TrimSpace(p.Error.String) == "" || !p.Error.Valid {
			return ErrWorkSourceCommandInvalidInput
		}
		return nil
	}
	if p.Status != "succeeded" || !p.Result.Valid || p.Error.Valid {
		return ErrWorkSourceCommandInvalidInput
	}
	raw := bytes.TrimSpace([]byte(p.Result.String))
	var value any
	switch cmd.Command {
	case "list":
		if len(raw) == 0 || raw[0] != '[' {
			return ErrWorkSourceCommandInvalidInput
		}
		var rows []beads.IssueSummary
		if json.Unmarshal(raw, &rows) != nil {
			return ErrWorkSourceCommandInvalidInput
		}
		limit := int32(50)
		if cmd.LimitCount.Valid {
			limit = cmd.LimitCount.Int32
		}
		if len(rows) > int(limit) {
			return ErrWorkSourceCommandInvalidInput
		}
		seen := make(map[string]bool, len(rows))
		for _, row := range rows {
			if strings.TrimSpace(row.ID) == "" || seen[row.ID] {
				return ErrWorkSourceCommandInvalidInput
			}
			seen[row.ID] = true
		}
		value = rows
	case "read":
		if len(raw) == 0 || raw[0] != '{' {
			return ErrWorkSourceCommandInvalidInput
		}
		var issue beads.Issue
		if json.Unmarshal(raw, &issue) != nil || issue.ID != cmd.NativeID.String || strings.TrimSpace(issue.ID) == "" || strings.TrimSpace(issue.Revision) == "" {
			return ErrWorkSourceCommandInvalidInput
		}
		// Completeness describes the qualified raw outgoing edge observation,
		// not an atomic metadata/cross-item snapshot or revision topology CAS.
		// Legacy omitted fields remain omitted for byte-identical terminal replay.
		if issue.DependenciesComplete {
			var evidence struct {
				Count *int `json:"dependency_count"`
			}
			if json.Unmarshal(raw, &evidence) != nil || evidence.Count == nil || issue.Dependencies == nil || len(issue.Dependencies) > beads.MaxDependencies || *evidence.Count != len(issue.Dependencies) {
				return ErrWorkSourceCommandInvalidInput
			}
			seen := make(map[beads.Dependency]bool, len(issue.Dependencies))
			for _, dependency := range issue.Dependencies {
				if strings.TrimSpace(dependency.ID) == "" || strings.TrimSpace(dependency.DependencyType) == "" || seen[dependency] {
					return ErrWorkSourceCommandInvalidInput
				}
				seen[dependency] = true
			}
		}
		value = issue
	default:
		return ErrWorkSourceCommandInvalidInput
	}
	canonical, err := json.Marshal(value)
	if err != nil || len(canonical) > 2<<20 {
		return ErrWorkSourceCommandInvalidInput
	}
	p.Result.String = string(canonical)
	return nil
}

// ExpireWorkSourceCommands is the bounded sweeper entry point. It acquires
// source then command locks, the same order as create/claim/report/deletion.
func (s *WorkSourceCommandService) ExpireWorkSourceCommands(ctx context.Context, limit int32) (int64, error) {
	if limit < 1 || limit > 200 {
		return 0, ErrWorkSourceCommandInvalidInput
	}
	sources, err := s.Queries.ListExpiredWorkSourceCommandSources(ctx, limit)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, source := range sources {
		var expired int64
		err = s.runInTx(ctx, func(q *db.Queries) error {
			if _, err := q.LockWorkSourceForWrite(ctx, db.LockWorkSourceForWriteParams{ID: source.SourceID, WorkspaceID: source.WorkspaceID}); errors.Is(err, pgx.ErrNoRows) {
				return nil
			} else if err != nil {
				return err
			}
			n, err := q.ExpireWorkSourceCommandsForSource(ctx, db.ExpireWorkSourceCommandsForSourceParams{SourceID: source.SourceID, WorkspaceID: source.WorkspaceID})
			if err == nil {
				expired = n
			}
			return err
		})
		if err != nil {
			return total, err
		}
		total += expired
	}
	return total, nil
}

// GetWorkSourceCommand returns one receipt for a workspace member.
func (s *WorkSourceCommandService) GetWorkSourceCommand(ctx context.Context, workspaceID, commandID pgtype.UUID) (db.WorkSourceCommand, error) {
	cmd, err := s.Queries.GetWorkSourceCommandInWorkspace(ctx, db.GetWorkSourceCommandInWorkspaceParams{
		ID: commandID, WorkspaceID: workspaceID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.WorkSourceCommand{}, ErrWorkSourceCommandNotFound
	}
	if err != nil {
		return db.WorkSourceCommand{}, err
	}
	return cmd, nil
}

// ListWorkSourceCommands lists a source's receipts, newest first, capped
// at the same 200-row bound as list commands.
func (s *WorkSourceCommandService) ListWorkSourceCommands(ctx context.Context, workspaceID, sourceID pgtype.UUID) ([]db.WorkSourceCommand, error) {
	limit := int32(maxWorkSourceCommandLimit)
	rows, err := s.Queries.ListWorkSourceCommandsBySource(ctx, db.ListWorkSourceCommandsBySourceParams{
		SourceID: sourceID, WorkspaceID: workspaceID, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	commands := make([]db.WorkSourceCommand, len(rows))
	for i, row := range rows {
		commands[i] = db.WorkSourceCommand(row)
	}
	return commands, nil
}
