package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var (
	ErrSourceReadUnauthorized = errors.New("source credential is no longer valid")
	ErrSourceReadForbidden    = errors.New("source credential does not own runtime")
	ErrSourceReadOffline      = errors.New("source runtime is offline")
)

type sourceReadOwner struct {
	runtime         db.AgentRuntime
	member          db.Member
	pat             db.PersonalAccessToken
	parentExpiresAt time.Time
}

// lockSourceReadOwner is shared by exchange and consumption. Workspace, runtime,
// member and parent locks live until the enclosing receipt transaction commits.
func lockSourceReadOwner(ctx context.Context, q *db.Queries, workspaceID, runtimeID, ownerID pgtype.UUID, patHash string) (sourceReadOwner, error) {
	var owner sourceReadOwner
	if _, err := q.LockWorkspaceForChatSessionCreate(ctx, workspaceID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return owner, ErrWorkSourceCommandNotFound
		}
		return owner, err
	}
	runtime, err := q.GetWorkSourceCommandRuntimeForShare(ctx, runtimeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return owner, ErrWorkSourceCommandNotFound
	}
	if err != nil {
		return owner, err
	}
	if runtime.WorkspaceID != workspaceID {
		return owner, ErrWorkSourceCommandNotFound
	}
	if !runtime.OwnerID.Valid || runtime.OwnerID != ownerID || !runtime.DaemonID.Valid || strings.TrimSpace(runtime.DaemonID.String) == "" {
		return owner, ErrSourceReadForbidden
	}
	if runtime.Status != "online" {
		return owner, ErrSourceReadOffline
	}
	member, err := q.LockWorkflowMember(ctx, db.LockWorkflowMemberParams{WorkspaceID: workspaceID, UserID: ownerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return owner, ErrWorkSourceCommandNotFound
	}
	if err != nil {
		return owner, err
	}
	owner.runtime, owner.member = runtime, member
	if patHash != "" {
		pat, err := q.GetPersonalAccessTokenForSourceRead(ctx, patHash)
		if errors.Is(err, pgx.ErrNoRows) {
			return owner, ErrSourceReadUnauthorized
		}
		if err != nil {
			return owner, err
		}
		if pat.UserID != ownerID || pat.TokenHash != patHash || pat.Revoked {
			return owner, ErrSourceReadUnauthorized
		}
		owner.pat = pat
		if pat.ExpiresAt.Valid {
			owner.parentExpiresAt = pat.ExpiresAt.Time
		}
	}
	return owner, nil
}

// MintSourceReadToken receives a freshly verified parent identity, never cached
// middleware headers. Its database locks revalidate PAT and live membership.
func (s *WorkSourceCommandService) MintSourceReadToken(ctx context.Context, runtime db.AgentRuntime, ownerID pgtype.UUID, patHash string, parentExpiresAt time.Time) (string, time.Time, error) {
	var token string
	var expiresAt time.Time
	err := s.runInTx(ctx, func(q *db.Queries) error {
		owner, err := lockSourceReadOwner(ctx, q, runtime.WorkspaceID, runtime.ID, ownerID, patHash)
		if err != nil {
			return err
		}
		claims := auth.SourceReadClaims{
			WorkspaceID: owner.runtime.WorkspaceID.String(), RuntimeID: owner.runtime.ID.String(),
			DaemonID: owner.runtime.DaemonID.String, MemberID: owner.member.ID.String(), ParentKind: "jwt",
		}
		claims.Subject = ownerID.String()
		if patHash != "" {
			claims.ParentKind, claims.ParentPATID, claims.ParentPATHash = "pat", owner.pat.ID.String(), patHash
			parentExpiresAt = owner.parentExpiresAt
		}
		token, expiresAt, err = auth.SignSourceReadToken(claims, parentExpiresAt, time.Now())
		if err != nil {
			return ErrSourceReadUnauthorized
		}
		return nil
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return token, expiresAt, nil
}

// authorizeSourceRead runs inside the actual operation transaction, not a
// middleware transaction. The signed membership UUID prevents remove/re-add ABA.
func authorizeSourceRead(ctx context.Context, q *db.Queries, claims auth.SourceReadClaims) (sourceReadOwner, error) {
	var owner sourceReadOwner
	workspaceID, err := util.ParseUUID(claims.WorkspaceID)
	if err != nil {
		return owner, ErrSourceReadUnauthorized
	}
	runtimeID, err := util.ParseUUID(claims.RuntimeID)
	if err != nil {
		return owner, ErrSourceReadUnauthorized
	}
	ownerID, err := util.ParseUUID(claims.Subject)
	if err != nil || auth.IsTemporarilyDisabledUserID(claims.Subject) {
		return owner, ErrSourceReadUnauthorized
	}
	owner, err = lockSourceReadOwner(ctx, q, workspaceID, runtimeID, ownerID, claims.ParentPATHash)
	if err != nil {
		return owner, err
	}
	if owner.runtime.DaemonID.String != claims.DaemonID || owner.member.ID.String() != claims.MemberID {
		return owner, ErrSourceReadForbidden
	}
	if claims.ParentKind == "pat" && owner.pat.ID.String() != claims.ParentPATID {
		return owner, ErrSourceReadUnauthorized
	}
	if err := checkSourceReadTime(claims, owner.parentExpiresAt); err != nil {
		return owner, err
	}
	return owner, nil
}

// Recheck after source/command locks as well, including before terminal replay.
func checkSourceReadTime(claims auth.SourceReadClaims, parentExpiresAt time.Time) error {
	now := time.Now()
	if claims.ExpiresAt == nil || !claims.ExpiresAt.After(now) || (!parentExpiresAt.IsZero() && !parentExpiresAt.After(now)) {
		return ErrSourceReadUnauthorized
	}
	return nil
}

func (s *WorkSourceCommandService) ListPendingSourceReadCommands(ctx context.Context, claims auth.SourceReadClaims) ([]db.ListPendingWorkSourceCommandsForRuntimeRow, error) {
	var rows []db.ListPendingWorkSourceCommandsForRuntimeRow
	err := s.runInTx(ctx, func(q *db.Queries) error {
		owner, err := authorizeSourceRead(ctx, q, claims)
		if err != nil {
			return err
		}
		rows, err = q.ListPendingWorkSourceCommandsForRuntime(ctx, db.ListPendingWorkSourceCommandsForRuntimeParams{
			RuntimeID: owner.runtime.ID, WorkspaceID: owner.runtime.WorkspaceID, DaemonID: owner.runtime.DaemonID,
		})
		if err != nil {
			return err
		}
		return checkSourceReadTime(claims, owner.parentExpiresAt)
	})
	return rows, err
}
