package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var errTaskMessageBatchConflict = errors.New("task message batch conflict")

func taskMessageBatchIdentity(req TaskMessageBatchRequest) (pgtype.UUID, pgtype.Text, error) {
	if req.BatchID == "" {
		return pgtype.UUID{}, pgtype.Text{}, nil
	}
	id, err := uuid.Parse(req.BatchID)
	if err != nil || id == uuid.Nil {
		return pgtype.UUID{}, pgtype.Text{}, fmt.Errorf("batch_id must be a nonzero UUID")
	}
	if len(req.Messages) == 0 {
		return pgtype.UUID{}, pgtype.Text{}, fmt.Errorf("identified batch must contain messages")
	}
	seen := make(map[int]struct{}, len(req.Messages))
	for _, msg := range req.Messages {
		if msg.Seq < 1 || msg.Seq > math.MaxInt32 {
			return pgtype.UUID{}, pgtype.Text{}, fmt.Errorf("message seq must be between 1 and %d", math.MaxInt32)
		}
		if _, exists := seen[msg.Seq]; exists {
			return pgtype.UUID{}, pgtype.Text{}, fmt.Errorf("message seq must be unique within a batch")
		}
		seen[msg.Seq] = struct{}{}
	}
	// Hash before redaction and server-time fallback so identical retries remain
	// identifiable even if those derived values change. Raw data is not stored here.
	encoded, err := json.Marshal(req.Messages)
	if err != nil {
		return pgtype.UUID{}, pgtype.Text{}, fmt.Errorf("invalid batch messages")
	}
	digest := sha256.Sum256(encoded)
	return pgtype.UUID{Bytes: id, Valid: true}, pgtype.Text{String: hex.EncodeToString(digest[:]), Valid: true}, nil
}

func (h *Handler) persistTaskMessageBatch(ctx context.Context, params db.CreateTaskMessagesParams) ([]db.CreateTaskMessagesRow, error) {
	if !params.BatchID.Valid {
		return h.Queries.CreateTaskMessages(ctx, params)
	}
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	q := h.Queries.WithTx(tx)
	// Serialize identified reports for this task through the existing task row.
	// The insert and receipt commit together, including concurrent HTTP retries.
	if _, err := q.LockTaskForMessageBatch(ctx, params.TaskID); err != nil {
		return nil, err
	}
	stored, err := q.GetTaskMessageBatchHash(ctx, db.GetTaskMessageBatchHashParams{TaskID: params.TaskID, BatchID: params.BatchID})
	if err == nil {
		if !stored.Valid || stored.String != params.BatchHash.String {
			return nil, errTaskMessageBatchConflict
		}
		return nil, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	created, err := q.CreateTaskMessages(ctx, params)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return created, nil
}
