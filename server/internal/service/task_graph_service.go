package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/pkg/beads"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

var (
	ErrDraftInvalid    = errors.New("invalid workflow draft request")
	ErrDraftNotFound   = errors.New("workflow draft or source not found")
	ErrDraftForbidden  = errors.New("workflow draft requires admin membership")
	ErrDraftConflict   = errors.New("workflow draft precondition or request identity changed")
	ErrDraftIneligible = errors.New("receipts do not establish a supported complete root closure")
)

// TaskGraphService saves observations only. It never creates tasks or grants Start.
type TaskGraphService struct {
	Queries   *db.Queries
	TxStarter TxStarter
}

type CreateWorkflowDraftParams struct {
	WorkspaceID            pgtype.UUID
	SourceID               pgtype.UUID
	RequestID              pgtype.UUID
	CreatedBy              pgtype.UUID
	RootNativeID           string
	ExpectedRootRevision   string
	ExpectedConfigRevision int32
	Capacity               int32
	ReceiptIDs             []pgtype.UUID
}

func workflowDraftRequestHash(p CreateWorkflowDraftParams) string {
	ids := make([]string, len(p.ReceiptIDs))
	for i, id := range p.ReceiptIDs {
		ids[i] = id.String()
	}
	sort.Strings(ids)
	payload, _ := json.Marshal(struct {
		SourceID               string
		RootNativeID           string
		ExpectedRootRevision   string
		ExpectedConfigRevision int32
		Capacity               int32
		ReceiptIDs             []string
	}{p.SourceID.String(), p.RootNativeID, p.ExpectedRootRevision, p.ExpectedConfigRevision, p.Capacity, ids})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// CreateWorkflowDraft uses membership -> workspace -> request -> project -> source
// locks. Parent deletion either sweeps a committed draft or prevents its insert.
func (s *TaskGraphService) CreateWorkflowDraft(ctx context.Context, p CreateWorkflowDraftParams) (run db.WorkflowRun, created bool, err error) {
	for _, id := range []pgtype.UUID{p.WorkspaceID, p.SourceID, p.RequestID, p.CreatedBy} {
		if !id.Valid || id.Bytes == [16]byte{} {
			return run, false, ErrDraftInvalid
		}
	}
	if strings.TrimSpace(p.RootNativeID) == "" || strings.TrimSpace(p.ExpectedRootRevision) == "" || p.ExpectedConfigRevision < 1 || p.Capacity < 1 || p.Capacity > 2 || len(p.ReceiptIDs) < 1 || len(p.ReceiptIDs) > 128 {
		return run, false, ErrDraftInvalid
	}
	seen := make(map[pgtype.UUID]bool, len(p.ReceiptIDs))
	for _, id := range p.ReceiptIDs {
		if !id.Valid || id.Bytes == [16]byte{} || seen[id] {
			return run, false, ErrDraftInvalid
		}
		seen[id] = true
	}
	if s.TxStarter == nil {
		return run, false, errors.New("workflow draft requires a transaction starter")
	}
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return run, false, err
	}
	defer tx.Rollback(ctx)
	q := s.Queries.WithTx(tx)
	if err = q.LockSubscriberWrites(ctx, db.LockSubscriberWritesParams{WorkspaceID: p.WorkspaceID, UserID: p.CreatedBy}); err != nil {
		return run, false, err
	}
	if _, err = q.LockWorkspaceForChatSessionCreate(ctx, p.WorkspaceID); errors.Is(err, pgx.ErrNoRows) {
		return run, false, ErrDraftNotFound
	} else if err != nil {
		return run, false, err
	}
	if _, err = q.LockActiveMember(ctx, db.LockActiveMemberParams{UserID: p.CreatedBy, WorkspaceID: p.WorkspaceID}); errors.Is(err, pgx.ErrNoRows) {
		return run, false, ErrDraftNotFound
	} else if err != nil {
		return run, false, err
	}
	member, err := q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: p.CreatedBy, WorkspaceID: p.WorkspaceID})
	if err != nil {
		return run, false, err
	}
	if member.Role != "owner" && member.Role != "admin" {
		return run, false, ErrDraftForbidden
	}
	if err = q.LockDraftWorkflowRequest(ctx, db.LockDraftWorkflowRequestParams{WorkspaceID: p.WorkspaceID, RequestID: p.RequestID}); err != nil {
		return run, false, err
	}
	hash := workflowDraftRequestHash(p)
	prior, priorErr := q.GetWorkflowRunByRequest(ctx, db.GetWorkflowRunByRequestParams{WorkspaceID: p.WorkspaceID, RequestID: p.RequestID})
	if priorErr == nil {
		if prior.RequestHash != hash {
			return run, false, ErrDraftConflict
		}
		return prior, false, tx.Commit(ctx)
	}
	if !errors.Is(priorErr, pgx.ErrNoRows) {
		return run, false, priorErr
	}
	// Project identity cannot change except by project deletion, which clears it.
	source, err := q.GetWorkSourceInWorkspace(ctx, db.GetWorkSourceInWorkspaceParams{ID: p.SourceID, WorkspaceID: p.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return run, false, ErrDraftNotFound
	} else if err != nil {
		return run, false, err
	}
	if source.ProjectID.Valid {
		if _, err = q.LockProjectForChatSessionCreate(ctx, db.LockProjectForChatSessionCreateParams{ID: source.ProjectID, WorkspaceID: p.WorkspaceID}); errors.Is(err, pgx.ErrNoRows) {
			return run, false, ErrDraftConflict
		} else if err != nil {
			return run, false, err
		}
	}
	if _, err = q.LockWorkSourceForWrite(ctx, db.LockWorkSourceForWriteParams{ID: p.SourceID, WorkspaceID: p.WorkspaceID}); errors.Is(err, pgx.ErrNoRows) {
		return run, false, ErrDraftNotFound
	} else if err != nil {
		return run, false, err
	}
	source, err = q.GetWorkSourceInWorkspace(ctx, db.GetWorkSourceInWorkspaceParams{ID: p.SourceID, WorkspaceID: p.WorkspaceID})
	if err != nil {
		return run, false, err
	}
	if !source.Enabled || source.ConfigRevision != p.ExpectedConfigRevision {
		return run, false, ErrDraftConflict
	}
	observations := make([]GraphObservation, 0, len(p.ReceiptIDs))
	bytes := 0
	for _, id := range p.ReceiptIDs {
		c, e := q.GetWorkSourceCommandInWorkspace(ctx, db.GetWorkSourceCommandInWorkspaceParams{ID: id, WorkspaceID: p.WorkspaceID})
		if errors.Is(e, pgx.ErrNoRows) {
			return run, false, ErrDraftNotFound
		} else if e != nil {
			return run, false, e
		}
		if c.SourceID != p.SourceID || c.ConfigRevision != source.ConfigRevision || c.Command != "read" || c.Status != "succeeded" || !c.Result.Valid || !c.NativeID.Valid {
			return run, false, ErrDraftIneligible
		}
		bytes += len(c.Result.String)
		if bytes > 2<<20 {
			return run, false, ErrDraftIneligible
		}
		var item beads.Issue
		if e = json.Unmarshal([]byte(c.Result.String), &item); e != nil || item.ID != c.NativeID.String {
			return run, false, ErrDraftIneligible
		}
		observations = append(observations, GraphObservation{ReceiptID: id.String(), ObservedAt: c.UpdatedAt.Time, Item: item})
	}
	graph, e := BuildDraftGraph(p.WorkspaceID.String(), p.SourceID.String(), p.RootNativeID, p.ExpectedRootRevision, source.ConfigRevision, observations)
	if e != nil {
		return run, false, fmt.Errorf("%w: %v", ErrDraftIneligible, e)
	}
	graphJSON, e := json.Marshal(graph)
	if e != nil {
		return run, false, e
	}
	state := make(map[string]struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}, len(graph.Nodes))
	for _, node := range graph.Nodes {
		state[node.NativeID] = struct {
			Status string `json:"status"`
			Reason string `json:"reason"`
		}{"blocked", "draft"}
	}
	stateJSON, e := json.Marshal(state)
	if e != nil {
		return run, false, e
	}
	run, err = q.CreateDraftWorkflowRun(ctx, db.CreateDraftWorkflowRunParams{ID: dbid.NewV7(), WorkspaceID: p.WorkspaceID, ProjectID: source.ProjectID, SourceID: p.SourceID, RequestID: p.RequestID, RequestHash: hash, RootNativeID: p.RootNativeID, ConfigRevision: source.ConfigRevision, Capacity: p.Capacity, Graph: graphJSON, NodeState: stateJSON, CreatedBy: p.CreatedBy})
	if err != nil {
		return run, false, err
	}
	return run, true, tx.Commit(ctx)
}
