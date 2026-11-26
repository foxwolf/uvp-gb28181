package playauth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	gbmodels "uvplatform.com/uvp-gb28181/app/gb28181/models"
)

func TestSettleDispatchedRefusesAnythingButADispatchedIntent(t *testing.T) {
	f, store := newIntentFixture(t)
	ctx := context.Background()
	id := intentIdentity(11)
	_, err := store.Reserve(ctx, id)
	require.NoError(t, err)
	require.ErrorIs(t, store.SettleDispatched(ctx, id.OperationID, 1), ErrDeviceIntentConflict)
	require.ErrorIs(t, store.SettleDispatched(ctx, "not-an-id", 1), ErrDeviceIntentInvalid)

	dispatched, err := store.Dispatch(ctx, id, 1)
	require.NoError(t, err)
	require.ErrorIs(t, store.SettleDispatched(ctx, id.OperationID, 1), ErrDeviceIntentConflict)
	require.NoError(t, store.SettleDispatched(ctx, id.OperationID, dispatched.RowVersion))
	require.ErrorIs(t, store.SettleDispatched(ctx, id.OperationID, dispatched.RowVersion+1), ErrDeviceIntentConflict)
	require.ErrorIs(t, store.CancelReserved(ctx, id.OperationID, dispatched.RowVersion+1), ErrDeviceIntentConflict)

	var parent DeviceOperationIntent
	require.NoError(t, f.db.First(&parent, "operation_id = ?", id.OperationID).Error)
	require.Equal(t, IntentSettled, parent.State)
	require.Equal(t, dispatched.RowVersion+1, parent.RowVersion)
	require.Nil(t, parent.CancelledAt)
	require.NotNil(t, parent.DispatchStartedAt)
	require.False(t, parent.UpdatedAt.Before(*parent.DispatchStartedAt))
	require.True(t, validIntentRow(parent))

	rows, err := store.ListUnsettled(ctx, id.DevicePK, id.DeviceCode, id.DeviceEpoch+1, "", 10)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestBackfillSettlesFinishedPTZParentsAndLeavesWaitingOnes(t *testing.T) {
	f, store := newIntentFixture(t)
	require.NoError(t, f.db.AutoMigrate(&gbmodels.GbPTZOperation{}, &gbmodels.GbPTZOperationAttempt{}))
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	past := now.Add(-time.Minute)
	future := now.Add(time.Minute)

	type expect struct {
		id     DeviceOperationIntentIdentity
		opID   uint
		status gbmodels.PTZOperationStatus
		settle bool
	}
	var cases []expect
	add := func(n int, responseRequired bool, mutate func(*gbmodels.GbPTZOperation)) expect {
		t.Helper()
		seed := gbmodels.GbPTZOperation{MaxAttempts: 1}
		if responseRequired {
			seed.ResponseRequired = true
			seed.QueueDeadlineAt = &future
		}
		id, op := dispatchSettledPTZFixture(t, store, n, seed)
		require.NoError(t, f.db.First(&op, op.ID).Error)
		mutate(&op)
		require.NoError(t, f.db.Model(&gbmodels.GbPTZOperation{}).Where("id = ?", op.ID).Updates(map[string]any{
			"status": op.Status, "response_required": op.ResponseRequired, "attempt": op.Attempt,
			"completed_at": op.CompletedAt, "sent_at": op.SentAt, "deadline_at": op.DeadlineAt,
			"next_attempt_at": op.NextAttemptAt,
		}).Error)
		item := expect{id: id, opID: op.ID, status: op.Status, settle: true}
		cases = append(cases, item)
		return item
	}

	finished := []gbmodels.PTZOperationStatus{
		gbmodels.PTZOperationRejected, gbmodels.PTZOperationTimeout,
		gbmodels.PTZOperationCancelled, gbmodels.PTZOperationAccepted,
	}
	for index, status := range finished {
		status := status
		add(index+1, true, func(op *gbmodels.GbPTZOperation) {
			op.Status = status
			op.CompletedAt = &past
		})
	}
	add(20, false, func(op *gbmodels.GbPTZOperation) {
		op.Status = gbmodels.PTZOperationSent
		op.ResponseRequired = false
		op.Attempt = 1
		op.SentAt = &past
		op.DeadlineAt = nil
		op.CompletedAt = nil
		op.NextAttemptAt = nil
	})
	waiting := add(21, true, func(op *gbmodels.GbPTZOperation) {
		op.Status = gbmodels.PTZOperationSent
		op.Attempt = 1
		op.SentAt = &past
		op.DeadlineAt = &future
		op.CompletedAt = nil
	})
	cases[len(cases)-1].settle = false
	pastDeadline := add(22, true, func(op *gbmodels.GbPTZOperation) {
		op.Status = gbmodels.PTZOperationSent
		op.Attempt = 1
		op.SentAt = &past
		op.DeadlineAt = &past
		op.CompletedAt = nil
	})
	cases[len(cases)-1].settle = false
	add(23, true, func(op *gbmodels.GbPTZOperation) {
		op.Status = gbmodels.PTZOperationUnknown
		op.CompletedAt = &past
		op.NextAttemptAt = nil
	})
	add(24, true, func(op *gbmodels.GbPTZOperation) {
		op.Status = gbmodels.PTZOperationUnknown
		op.CompletedAt = &past
		op.NextAttemptAt = &future
	})
	cases[len(cases)-1].settle = false
	blocked := add(25, true, func(op *gbmodels.GbPTZOperation) {
		op.Status = gbmodels.PTZOperationRejected
		op.CompletedAt = &past
	})
	cases[len(cases)-1].settle = false
	require.NoError(t, f.db.Create(&gbmodels.GbPTZOperationAttempt{
		OperationID: blocked.opID, AttemptNo: 1, SN: 1,
		Status: gbmodels.PTZOperationAttemptDispatching, StartedAt: past, LeaseUntil: future,
	}).Error)
	add(26, true, func(op *gbmodels.GbPTZOperation) {
		op.Status = gbmodels.PTZOperationQueued
	})
	cases[len(cases)-1].settle = false
	_ = waiting

	require.NoError(t, f.db.Exec("UPDATE gb_device SET access_epoch = 2 WHERE id = 1").Error)
	unsettled, err := store.HasUnsettledBefore(ctx, cleanupDeviceA, 2)
	require.NoError(t, err)
	require.True(t, unsettled)

	examined, err := BackfillSettledPTZIntents(ctx, f.db, now, 100)
	require.NoError(t, err)
	require.NotZero(t, examined)

	for _, item := range cases {
		var parent DeviceOperationIntent
		require.NoError(t, f.db.First(&parent, "operation_id = ?", item.id.OperationID).Error)
		var op gbmodels.GbPTZOperation
		require.NoError(t, f.db.First(&op, item.opID).Error)
		require.Equal(t, item.status, op.Status, "settlement must not rewrite the business row")
		if item.settle {
			require.Equal(t, IntentSettled, parent.State, item.id.OperationID)
			require.Nil(t, parent.CancelledAt)
			require.True(t, validIntentRow(parent))
		} else {
			require.Equal(t, IntentDispatched, parent.State, item.id.OperationID)
		}
	}

	require.NoError(t, f.db.Model(&gbmodels.GbPTZOperationAttempt{}).
		Where("operation_id = ?", blocked.opID).
		Update("status", gbmodels.PTZOperationAttemptSent).Error)
	_, err = BackfillSettledPTZIntents(ctx, f.db, now, 100)
	require.NoError(t, err)
	var parent DeviceOperationIntent
	require.NoError(t, f.db.First(&parent, "operation_id = ?", blocked.id.OperationID).Error)
	require.Equal(t, IntentSettled, parent.State)

	require.NoError(t, f.db.Model(&gbmodels.GbPTZOperation{}).Where("id = ?", pastDeadline.opID).
		Updates(map[string]any{"status": gbmodels.PTZOperationTimeout, "completed_at": now, "next_attempt_at": nil}).Error)
	_, err = BackfillSettledPTZIntents(ctx, f.db, now, 100)
	require.NoError(t, err)
	parent = DeviceOperationIntent{}
	require.NoError(t, f.db.First(&parent, "operation_id = ?", pastDeadline.id.OperationID).Error)
	require.Equal(t, IntentSettled, parent.State)

	unsettled, err = store.HasUnsettledBefore(ctx, cleanupDeviceA, 2)
	require.NoError(t, err)
	require.True(t, unsettled, "a response that is still outstanding keeps the gate closed")
}

func TestSettlePTZParentRollsBackWithTheBusinessWrite(t *testing.T) {
	f, store := newIntentFixture(t)
	require.NoError(t, f.db.AutoMigrate(&gbmodels.GbPTZOperation{}, &gbmodels.GbPTZOperationAttempt{}))
	id, op := dispatchSettledPTZFixture(t, store, 40, gbmodels.GbPTZOperation{MaxAttempts: 1, ResponseRequired: true, QueueDeadlineAt: ptrTime(time.Now().Add(time.Minute))})
	now := time.Now().UTC()
	err := f.db.Transaction(func(tx *gorm.DB) error {
		require.NoError(t, tx.Model(&gbmodels.GbPTZOperation{}).Where("id = ?", op.ID).
			Updates(map[string]any{"status": gbmodels.PTZOperationRejected, "completed_at": now}).Error)
		require.NoError(t, SettlePTZParent(tx, op.ID, now))
		var parent DeviceOperationIntent
		require.NoError(t, tx.First(&parent, "operation_id = ?", id.OperationID).Error)
		require.Equal(t, IntentSettled, parent.State)
		return errors.New("rollback")
	})
	require.Error(t, err)
	var parent DeviceOperationIntent
	require.NoError(t, f.db.First(&parent, "operation_id = ?", id.OperationID).Error)
	require.Equal(t, IntentDispatched, parent.State)
	var stored gbmodels.GbPTZOperation
	require.NoError(t, f.db.First(&stored, op.ID).Error)
	require.Equal(t, gbmodels.PTZOperationQueued, stored.Status)
}

func dispatchSettledPTZFixture(t *testing.T, store *DeviceOperationIntentStore, n int, seed gbmodels.GbPTZOperation) (DeviceOperationIntentIdentity, gbmodels.GbPTZOperation) {
	t.Helper()
	id := intentIdentity(n)
	id.Kind = "ptz"
	seed.OperationID = id.OperationID
	seed.IdempotencyKey = id.OperationID
	seed.DeviceID = uint(id.DevicePK)
	seed.DeviceCode = id.DeviceCode
	seed.ChannelID = uint(id.TargetPK)
	seed.ChannelCode = id.TargetCode
	seed.TargetScope = id.TargetScope
	seed.TargetCode = id.TargetCode
	seed.Status = gbmodels.PTZOperationQueued
	if seed.MaxAttempts < 1 {
		seed.MaxAttempts = 1
	}
	op, err := store.ReservePTZOperation(context.Background(), id, seed)
	require.NoError(t, err)
	_, err = store.Dispatch(context.Background(), id, 1)
	require.NoError(t, err)
	return id, op
}
