package playauth

import (
	"context"
	"errors"
	"math"
	"time"

	"gorm.io/gorm"
	gbmodels "uvplatform.com/uvp-gb28181/app/gb28181/models"
)

// ptzOperationReadyToSettle reports whether the business row will not send
// again. It does not rewrite the business status: a one-way success stays
// sent, because that command was delivered and has no application reply.
func ptzOperationReadyToSettle(op gbmodels.GbPTZOperation) bool {
	switch op.Status {
	case gbmodels.PTZOperationRejected, gbmodels.PTZOperationTimeout, gbmodels.PTZOperationCancelled, gbmodels.PTZOperationAccepted:
		return op.CompletedAt != nil
	case gbmodels.PTZOperationUnknown:
		// A transport-unknown row can still be retried while the next attempt
		// is scheduled. Claim needs the parent to stay dispatched until then.
		return op.CompletedAt != nil && op.NextAttemptAt == nil
	case gbmodels.PTZOperationSent:
		// Response-required commands stay sent until expireApplication moves
		// them to timeout. A null deadline on that path is not proof the wait
		// ended, so it is left dispatched. One-way success clears the deadline
		// and does not complete the business row.
		if op.ResponseRequired || op.DeadlineAt != nil || op.NextAttemptAt != nil {
			return false
		}
		return op.Attempt > 0 || op.SentAt != nil
	default:
		return false
	}
}

func settleTimestamp(parent DeviceOperationIntent, now time.Time) time.Time {
	at := now.UTC()
	if parent.DispatchStartedAt != nil && at.Before(parent.DispatchStartedAt.UTC()) {
		at = parent.DispatchStartedAt.UTC()
	}
	if at.Before(parent.UpdatedAt.UTC()) {
		at = parent.UpdatedAt.UTC()
	}
	if at.Before(parent.CreatedAt.UTC()) {
		at = parent.CreatedAt.UTC()
	}
	return at
}

func inOpenTransaction(db *gorm.DB) bool {
	if db == nil || db.Statement == nil || db.Statement.ConnPool == nil {
		return false
	}
	pool := db.Statement.ConnPool
	if prepared, ok := pool.(*gorm.PreparedStmtTX); ok && prepared != nil && prepared.Tx != nil {
		pool = prepared.Tx
	}
	_, ok := pool.(gorm.TxCommitter)
	return ok
}

// SettlePTZParent CAS-settles the dispatched PTZ parent of one business row.
// It joins the caller's transaction when one is open. A missing parent, a
// parent that is not a valid dispatched PTZ intent, a live dispatching
// attempt, or a business row that is still waiting returns nil: those rows
// keep blocking the cleanup watermark. Already settled is success.
func SettlePTZParent(db *gorm.DB, operationPK uint, now time.Time) error {
	if db == nil || operationPK == 0 || now.IsZero() {
		return nil
	}
	if inOpenTransaction(db) {
		return settlePTZParentTx(db, operationPK, now)
	}
	return db.Transaction(func(tx *gorm.DB) error {
		return settlePTZParentTx(tx, operationPK, now)
	})
}

func settlePTZParentTx(tx *gorm.DB, operationPK uint, now time.Time) error {
	var op gbmodels.GbPTZOperation
	if err := tx.First(&op, operationPK).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	if op.DeviceIntentID == nil || !validIntentID(*op.DeviceIntentID) || !ptzOperationReadyToSettle(op) {
		return nil
	}
	var live int64
	if err := tx.Model(&gbmodels.GbPTZOperationAttempt{}).
		Where("operation_id = ? AND status = ?", op.ID, gbmodels.PTZOperationAttemptDispatching).
		Count(&live).Error; err != nil {
		return err
	}
	if live != 0 {
		return nil
	}
	var parents []DeviceOperationIntent
	if err := tx.Where("operation_id = ?", *op.DeviceIntentID).Limit(2).Find(&parents).Error; err != nil {
		return err
	}
	if len(parents) != 1 || parents[0].Kind != "ptz" || parents[0].State != IntentDispatched || !validIntentRow(parents[0]) || parents[0].RowVersion == math.MaxInt64 {
		return nil
	}
	return settleDispatchedCAS(tx, parents[0].OperationID, parents[0].RowVersion, settleTimestamp(parents[0], now), true)
}

func settleDispatchedCAS(tx *gorm.DB, operationID string, version int64, at time.Time, alreadySettledOK bool) error {
	result := tx.Model(&DeviceOperationIntent{}).
		Where("operation_id = ? AND state = ? AND row_version = ? AND contract_version = 1 AND cancelled_at IS NULL AND dispatch_started_at IS NOT NULL",
			operationID, IntentDispatched, version).
		Updates(map[string]any{"state": IntentSettled, "row_version": version + 1, "updated_at": at})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 1 {
		return nil
	}
	if !alreadySettledOK {
		return ErrDeviceIntentConflict
	}
	var rows []DeviceOperationIntent
	if err := tx.Where("operation_id = ?", operationID).Limit(2).Find(&rows).Error; err != nil {
		return err
	}
	if len(rows) == 1 && rows[0].State == IntentSettled {
		return nil
	}
	return ErrDeviceIntentConflict
}

// BackfillSettledPTZIntents examines up to limit dispatched PTZ parents whose
// business rows are already finished, and CAS-settles each one. The count is
// the number of candidates examined, so a short page means the sweep is caught
// up. In-flight rows are not candidates and cannot starve the finished ones.
func BackfillSettledPTZIntents(ctx context.Context, db *gorm.DB, now time.Time, limit int) (int, error) {
	if db == nil || limit <= 0 || now.IsZero() {
		return 0, nil
	}
	terminal := []gbmodels.PTZOperationStatus{
		gbmodels.PTZOperationRejected,
		gbmodels.PTZOperationTimeout,
		gbmodels.PTZOperationCancelled,
		gbmodels.PTZOperationAccepted,
		gbmodels.PTZOperationUnknown,
	}
	var ids []uint
	err := db.WithContext(ctx).Table("gb_ptz_operation AS operation").
		Select("operation.id").
		Joins("JOIN gb_device_operation_intent AS intent ON intent.operation_id = operation.device_intent_id").
		Where("intent.state = ? AND intent.kind = ? AND intent.contract_version = 1 AND intent.cancelled_at IS NULL AND intent.dispatch_started_at IS NOT NULL", IntentDispatched, "ptz").
		Where(`(operation.status IN ? AND operation.completed_at IS NOT NULL AND (operation.status <> ? OR operation.next_attempt_at IS NULL))
			OR (operation.status = ? AND operation.response_required = ? AND operation.deadline_at IS NULL AND operation.next_attempt_at IS NULL AND (operation.attempt > 0 OR operation.sent_at IS NOT NULL))`,
			terminal, gbmodels.PTZOperationUnknown, gbmodels.PTZOperationSent, false).
		Where(`NOT EXISTS (SELECT 1 FROM gb_ptz_operation_attempt AS attempt WHERE attempt.operation_id = operation.id AND attempt.status = ?)`, gbmodels.PTZOperationAttemptDispatching).
		Order("operation.id").
		Limit(limit).
		Pluck("operation.id", &ids).Error
	if err != nil {
		return 0, err
	}
	var firstErr error
	for _, id := range ids {
		err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			return settlePTZParentTx(tx, id, now)
		})
		if err != nil && !errors.Is(err, ErrDeviceIntentConflict) && firstErr == nil {
			firstErr = err
		}
	}
	return len(ids), firstErr
}
