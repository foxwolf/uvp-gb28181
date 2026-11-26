package playauth

import "context"

// ListRecoveryPage adds a fresh device identity/epoch check to the read-only
// inventory API. An invented future target must not cancel current work.
// This grants neither dispatch permission nor coverage/completion evidence.
//
// Only playback and download intents belong here. PTZ, live and talk rows are
// not playback-dialog cleanup; feeding them to recoverPlaybackIntent reports
// a false playback_recovery_incomplete.
func (s *DeviceOperationIntentStore) ListRecoveryPage(ctx context.Context, pk int64, code string, beforeEpoch int64, afterID string, limit int) ([]DeviceOperationIntent, error) {
	if !s.available(ctx) {
		return nil, ErrDeviceIntentUnavailable
	}
	if pk <= 0 || !validGBID(code) || beforeEpoch <= 0 {
		return nil, ErrDeviceIntentInvalid
	}
	rows, err := queryDeviceCleanupRows(s.db, ctx, code, false)
	if err != nil || len(rows) != 1 || rows[0].ID != pk {
		return nil, ErrDeviceIntentUnavailable
	}
	state, err := validateDeviceCleanupRow(rows[0])
	if err != nil || beforeEpoch > state.AccessEpoch || beforeEpoch <= state.CleanupCompletedEpoch {
		return nil, ErrDeviceIntentRevoked
	}
	return s.listIntentPage(ctx, pk, code, beforeEpoch, afterID, limit, []string{"playback", "download"})
}
