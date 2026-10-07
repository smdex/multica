package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

var ErrNativeEnrollmentConflict = errors.New("native source enrollment has changed")

// NativeEnrollmentProof binds approval to a fresh domain's immutable marker.
// It conveys no filesystem path, provider readiness, or execution authority.
type NativeEnrollmentProof struct {
	EnrollmentID   pgtype.UUID
	ConfigRevision int32
	ManifestHash   string
}

func (p NativeEnrollmentProof) valid() bool {
	hash, err := hex.DecodeString(p.ManifestHash)
	return p.EnrollmentID.Valid && p.EnrollmentID.Bytes != [16]byte{} && p.ConfigRevision > 0 && err == nil && len(hash) == sha256.Size && hex.EncodeToString(hash) == p.ManifestHash
}

// Native enrollment uses the existing owner lock order, with subscriber
// serialization first so member removal cannot race a new ownership grant.
func lockNativeEnrollmentOwner(ctx context.Context, q *db.Queries, workspaceID, runtimeID, ownerID pgtype.UUID, patHash string) (sourceReadOwner, error) {
	if auth.IsTemporarilyDisabledUserID(ownerID.String()) {
		return sourceReadOwner{}, ErrSourceReadUnauthorized
	}
	if err := q.LockSubscriberWrites(ctx, db.LockSubscriberWritesParams{WorkspaceID: workspaceID, UserID: ownerID}); err != nil {
		return sourceReadOwner{}, err
	}
	owner, err := lockSourceReadOwner(ctx, q, workspaceID, runtimeID, ownerID, patHash)
	if err != nil {
		return owner, err
	}
	if owner.member.Role != "owner" && owner.member.Role != "admin" {
		return owner, ErrSourceReadForbidden
	}
	return owner, nil
}

func checkNativeEnrollmentTime(expiresAt, parentExpiresAt time.Time) error {
	now := time.Now()
	if (!expiresAt.IsZero() && !expiresAt.After(now)) || (!parentExpiresAt.IsZero() && !parentExpiresAt.After(now)) {
		return ErrSourceReadUnauthorized
	}
	return nil
}

func nativeEnrollmentMatchesOwner(source db.WorkSource, owner sourceReadOwner) bool {
	return source.Mode == "native" && source.NativeEnrollmentID.Valid && source.CreatedBy == owner.runtime.OwnerID &&
		source.RuntimeID == owner.runtime.ID && source.WorkspaceID == owner.runtime.WorkspaceID && source.DaemonID == owner.runtime.DaemonID.String &&
		source.NativeOwnerMemberID == owner.member.ID && source.NativeRuntimeCreatedAt.Valid && owner.runtime.CreatedAt.Valid &&
		source.NativeRuntimeCreatedAt.Time.Equal(owner.runtime.CreatedAt.Time)
}

func (s *WorkSourceCommandService) CreateNativeSourceIntent(ctx context.Context, workspaceID, runtimeID, ownerID, requestID pgtype.UUID, name, patHash string, parentExpiry time.Time) (db.WorkSource, bool, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 200 || !requestID.Valid || requestID.Bytes == [16]byte{} {
		return db.WorkSource{}, false, ErrWorkSourceInvalidInput
	}
	requestBytes, err := json.Marshal(struct{ Owner, Runtime, Name string }{ownerID.String(), runtimeID.String(), name})
	if err != nil {
		return db.WorkSource{}, false, err
	}
	digest := sha256.Sum256(requestBytes)
	hash := hex.EncodeToString(digest[:])
	var source db.WorkSource
	created := false
	err = s.runInTx(ctx, func(q *db.Queries) error {
		owner, err := lockNativeEnrollmentOwner(ctx, q, workspaceID, runtimeID, ownerID, patHash)
		if err != nil {
			return err
		}
		if patHash != "" {
			parentExpiry = owner.parentExpiresAt
		}
		if err := q.LockNativeSourceRequest(ctx, workspaceID.String()+":"+requestID.String()); err != nil {
			return err
		}
		prior, err := q.GetNativeSourceByRequest(ctx, db.GetNativeSourceByRequestParams{WorkspaceID: workspaceID, NativeRequestID: requestID})
		if err == nil {
			if !prior.NativeRequestHash.Valid || prior.NativeRequestHash.String != hash || !nativeEnrollmentMatchesOwner(prior, owner) {
				return ErrNativeEnrollmentConflict
			}
			if err := checkNativeEnrollmentTime(time.Time{}, parentExpiry); err != nil {
				return err
			}
			source = prior
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err := checkNativeEnrollmentTime(time.Time{}, parentExpiry); err != nil {
			return err
		}
		id := dbid.NewV7()
		source, err = q.CreateNativeSourceIntent(ctx, db.CreateNativeSourceIntentParams{
			ID: id, WorkspaceID: workspaceID, RuntimeID: runtimeID, DaemonID: owner.runtime.DaemonID.String,
			Name: name, SourceHandle: "managed:" + id.String(), CreatedBy: ownerID,
			NativeRequestID: requestID, NativeRequestHash: pgtype.Text{String: hash, Valid: true},
			NativeEnrollmentID: dbid.NewV7(), NativeOwnerMemberID: owner.member.ID, NativeRuntimeCreatedAt: owner.runtime.CreatedAt,
		})
		created = err == nil
		return err
	})
	return source, created, err
}

func lockNativeEnrollmentSource(ctx context.Context, q *db.Queries, owner sourceReadOwner, sourceID pgtype.UUID, proof NativeEnrollmentProof) (db.WorkSource, error) {
	if !proof.valid() {
		return db.WorkSource{}, ErrWorkSourceInvalidInput
	}
	if _, err := q.LockWorkSourceForWrite(ctx, db.LockWorkSourceForWriteParams{ID: sourceID, WorkspaceID: owner.runtime.WorkspaceID}); errors.Is(err, pgx.ErrNoRows) {
		return db.WorkSource{}, ErrWorkSourceCommandNotFound
	} else if err != nil {
		return db.WorkSource{}, err
	}
	source, err := q.GetWorkSourceInWorkspace(ctx, db.GetWorkSourceInWorkspaceParams{ID: sourceID, WorkspaceID: owner.runtime.WorkspaceID})
	if err != nil {
		return source, err
	}
	if !nativeEnrollmentMatchesOwner(source, owner) {
		return source, ErrSourceReadForbidden
	}
	if source.NativeEnrollmentID != proof.EnrollmentID || source.ConfigRevision != proof.ConfigRevision ||
		(source.NativeManifestHash.Valid && source.NativeManifestHash.String != proof.ManifestHash) {
		return source, ErrNativeEnrollmentConflict
	}
	return source, nil
}

func (s *WorkSourceCommandService) MintSourceEnrollmentToken(ctx context.Context, workspaceID, runtimeID, ownerID, sourceID pgtype.UUID, proof NativeEnrollmentProof, patHash string, parentExpiry time.Time) (string, time.Time, error) {
	var token string
	var expiresAt time.Time
	err := s.runInTx(ctx, func(q *db.Queries) error {
		owner, err := lockNativeEnrollmentOwner(ctx, q, workspaceID, runtimeID, ownerID, patHash)
		if err != nil {
			return err
		}
		source, err := lockNativeEnrollmentSource(ctx, q, owner, sourceID, proof)
		if err != nil {
			return err
		}
		if patHash != "" {
			parentExpiry = owner.parentExpiresAt
		}
		if err := checkNativeEnrollmentTime(time.Time{}, parentExpiry); err != nil {
			return err
		}
		claims := auth.SourceEnrollmentClaims{
			WorkspaceID: workspaceID.String(), RuntimeID: runtimeID.String(), DaemonID: owner.runtime.DaemonID.String,
			MemberID: owner.member.ID.String(), ParentKind: "jwt", SourceID: source.ID.String(),
			EnrollmentID: source.NativeEnrollmentID.String(), ConfigRevision: int64(source.ConfigRevision), ManifestHash: proof.ManifestHash,
		}
		claims.Subject = ownerID.String()
		if patHash != "" {
			claims.ParentKind, claims.ParentPATID, claims.ParentPATHash = "pat", owner.pat.ID.String(), patHash
		}
		token, expiresAt, err = auth.SignSourceEnrollmentToken(claims, parentExpiry, time.Now())
		if err != nil {
			return ErrSourceReadUnauthorized
		}
		_, err = q.ApproveNativeSourceEnrollment(ctx, db.ApproveNativeSourceEnrollmentParams{
			ID: sourceID, WorkspaceID: workspaceID, NativeEnrollmentID: proof.EnrollmentID,
			ConfigRevision: proof.ConfigRevision, NativeManifestHash: pgtype.Text{String: proof.ManifestHash, Valid: true},
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNativeEnrollmentConflict
		}
		return err
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return token, expiresAt, nil
}

func (s *WorkSourceCommandService) FinalizeNativeSourceEnrollment(ctx context.Context, claims auth.SourceEnrollmentClaims, proof NativeEnrollmentProof) (db.WorkSource, error) {
	var source db.WorkSource
	if !proof.valid() {
		return source, ErrWorkSourceInvalidInput
	}
	if claims.ExpiresAt == nil {
		return source, ErrSourceReadUnauthorized
	}
	if proof.EnrollmentID.String() != claims.EnrollmentID || int64(proof.ConfigRevision) != claims.ConfigRevision || proof.ManifestHash != claims.ManifestHash {
		return source, ErrNativeEnrollmentConflict
	}
	workspaceID, err := util.ParseUUID(claims.WorkspaceID)
	if err != nil {
		return source, ErrSourceReadUnauthorized
	}
	runtimeID, err := util.ParseUUID(claims.RuntimeID)
	if err != nil {
		return source, ErrSourceReadUnauthorized
	}
	ownerID, err := util.ParseUUID(claims.Subject)
	if err != nil {
		return source, ErrSourceReadUnauthorized
	}
	sourceID, err := util.ParseUUID(claims.SourceID)
	if err != nil {
		return source, ErrSourceReadUnauthorized
	}
	err = s.runInTx(ctx, func(q *db.Queries) error {
		owner, err := lockNativeEnrollmentOwner(ctx, q, workspaceID, runtimeID, ownerID, claims.ParentPATHash)
		if err != nil {
			return err
		}
		if owner.member.ID.String() != claims.MemberID || owner.runtime.DaemonID.String != claims.DaemonID {
			return ErrSourceReadForbidden
		}
		if claims.ParentKind == "pat" && owner.pat.ID.String() != claims.ParentPATID {
			return ErrSourceReadUnauthorized
		}
		source, err = lockNativeEnrollmentSource(ctx, q, owner, sourceID, proof)
		if err != nil {
			return err
		}
		if err := checkNativeEnrollmentTime(claims.ExpiresAt.Time, owner.parentExpiresAt); err != nil {
			return err
		}
		if !source.NativeApprovedAt.Valid || !source.NativeManifestHash.Valid {
			return ErrNativeEnrollmentConflict
		}
		// A terminal replay still passes every current authority and time fence.
		if source.NativeEnrolledAt.Valid {
			return nil
		}
		source, err = q.FinalizeNativeSourceEnrollment(ctx, db.FinalizeNativeSourceEnrollmentParams{
			ID: sourceID, WorkspaceID: workspaceID, NativeEnrollmentID: proof.EnrollmentID,
			ConfigRevision: proof.ConfigRevision, NativeManifestHash: pgtype.Text{String: proof.ManifestHash, Valid: true},
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNativeEnrollmentConflict
		}
		return err
	})
	return source, err
}
