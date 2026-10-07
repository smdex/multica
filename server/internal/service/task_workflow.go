package service

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ReconcileTaskWorkflow runs with the task row locked, in the same transaction
// as its lifecycle transition. Interaction insertion and dispatch take that
// lock too. Retired responses retain their identity for late acknowledgements.
func ReconcileTaskWorkflow(ctx context.Context, q *db.Queries, taskID pgtype.UUID) error {
	if err := q.RetireStaleTaskInteractions(ctx, taskID); err != nil {
		return err
	}
	if err := q.RetireStaleTaskWorkflowRequests(ctx, taskID); err != nil {
		return err
	}
	return q.RetireUndeliverableTaskInteractionResponses(ctx, taskID)
}

// RetireTaskWorkflow is part of terminal task settlement, including bulk
// cancellation, runtime loss and restart recovery. No provider input is sent.
func RetireTaskWorkflow(ctx context.Context, q *db.Queries, tasks ...db.AgentTaskQueue) error {
	for _, task := range tasks {
		if task.ActiveRunID.Valid {
			if _, err := q.CancelTaskInteractionsForRun(ctx, db.CancelTaskInteractionsForRunParams{TaskID: task.ID, RunID: task.ActiveRunID}); err != nil {
				return err
			}
		}
		if err := ReconcileTaskWorkflow(ctx, q, task.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *TaskService) StartTaskWithRun(ctx context.Context, taskID, runID pgtype.UUID, supplementSupport ...bool) (db.AgentTaskQueue, error) {
	var task db.AgentTaskQueue
	err := s.runInTx(ctx, func(q *db.Queries) error {
		if err := lockChatSessionForTaskWrite(ctx, q, taskID); err != nil {
			return err
		}
		var err error
		task, err = q.StartAgentTaskWithSupplement(ctx, db.StartAgentTaskWithSupplementParams{
			TaskID:               taskID,
			ActiveRunID:          runID,
			EnableTaskSupplement: len(supplementSupport) > 0 && supplementSupport[0],
		})
		if err != nil {
			return err
		}
		return ReconcileTaskWorkflow(ctx, q, taskID)
	})
	if err == nil {
		s.taskStarted(ctx, task)
	}
	return task, err
}
