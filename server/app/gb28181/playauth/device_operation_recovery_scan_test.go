package playauth

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDeviceOperationRecoveryPageEpochsIdentityAndBounds(t *testing.T) {
	f, s := newIntentFixture(t)
	ctx := context.Background()
	for n := 1; n <= 102; n++ {
		id := intentIdentity(n)
		if n%2 == 0 {
			id.Kind = "playback"
		} else {
			id.Kind = "download"
		}
		_, err := s.Reserve(ctx, id)
		require.NoError(t, err)
	}
	var excluded []string
	for index, kind := range []string{"ptz", "live", "talk"} {
		id := intentIdentity(300 + index)
		id.Kind = kind
		_, err := s.Reserve(ctx, id)
		require.NoError(t, err)
		excluded = append(excluded, id.OperationID)
	}
	// Other devices are never included, including when the epoch is equal.
	other := intentIdentity(103)
	other.DevicePK, other.DeviceCode, other.TargetPK = 2, cleanupDeviceB, 12
	_, err := s.Reserve(ctx, other)
	require.NoError(t, err)
	require.NoError(t, f.db.Exec("UPDATE gb_device SET access_epoch = 2, cleanup_completed_epoch = 2 WHERE id = 1").Error)
	second := intentIdentity(104)
	second.DeviceEpoch = 2
	second.Kind = "playback"
	_, err = s.Reserve(ctx, second)
	require.NoError(t, err)
	require.NoError(t, f.db.Exec("UPDATE gb_device SET access_epoch = 3, cleanup_completed_epoch = 1 WHERE id = 1").Error)
	third := intentIdentity(105)
	third.DeviceEpoch = 3
	// A fixture-only current row verifies strict < target filtering.
	rows, err := s.ListUnsettled(ctx, 1, cleanupDeviceA, 3, "", 1)
	require.NoError(t, err)
	current := rows[0]
	current.DeviceOperationIntentIdentity = third
	require.NoError(t, f.db.Create(&current).Error)
	rows, err = s.ListRecoveryPage(ctx, 1, cleanupDeviceA, 3, "", 1000)
	require.NoError(t, err)
	require.Len(t, rows, 100)
	firstPage := append([]DeviceOperationIntent(nil), rows...)
	rows, err = s.ListRecoveryPage(ctx, 1, cleanupDeviceA, 3, rows[99].OperationID, 1000)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	require.Equal(t, int64(2), rows[2].DeviceEpoch)
	seen := map[string]bool{}
	for _, row := range append(firstPage, rows...) {
		require.Contains(t, []string{"playback", "download"}, row.Kind)
		seen[row.OperationID] = true
	}
	for _, id := range excluded {
		require.False(t, seen[id], "playback recovery must not scan %s", id)
	}
	old, err := s.ListRecoveryPage(ctx, 1, cleanupDeviceA, 2, fmt.Sprintf("%032x", 100), 100)
	require.NoError(t, err)
	require.Len(t, old, 2, "old target 2 never scans epoch 2 or 3")
	for _, bad := range []struct {
		pk, epoch int64
		code      string
	}{
		{1, 4, cleanupDeviceA}, {1, 1, cleanupDeviceA}, {2, 3, cleanupDeviceA}, {1, 3, cleanupDeviceB},
	} {
		_, err := s.ListRecoveryPage(ctx, bad.pk, bad.code, bad.epoch, "", 1)
		require.Error(t, err)
	}
	state, err := NewDeviceCleanupStore(f.db).Load(ctx, cleanupDeviceA)
	require.NoError(t, err)
	require.Equal(t, int64(1), state.CleanupCompletedEpoch)
}
