package migration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	migrationsfs "uvplatform.com/uvp-gb28181/resource/database/gb28181"
)

func TestDeviceIntentSettledMigration(t *testing.T) {
	ups := []string{
		"migrations/2026-11-26-device-intent-settled.sql",
		"migrations/2026-11-26-device-intent-settled-postgresql.sql",
		"migrations/2026-11-26-device-intent-settled-sqlserver.sql",
		"migrations/2026-11-26-device-intent-settled-dm.sql",
		"migrations/2026-11-26-device-intent-settled-sqlite.sql",
	}
	for _, name := range ups {
		t.Run(name, func(t *testing.T) {
			body, err := migrationsfs.FS.ReadFile(name)
			require.NoError(t, err)
			text := strings.ToLower(string(body))
			require.Contains(t, text, "settled")
			require.NotContains(t, text, "drop table")
			require.NotContains(t, text, "delete ")
			if !strings.Contains(name, "sqlite") {
				require.Contains(t, text, "ck_device_intent_phase")
			}
			downName := strings.TrimSuffix(name, ".sql") + "-down.sql"
			down, err := migrationsfs.FS.ReadFile(downName)
			require.NoError(t, err)
			lowerDown := strings.ToLower(string(down))
			require.Contains(t, lowerDown, "select 1")
			for _, forbidden := range []string{"drop ", "delete ", "truncate ", "update ", "alter "} {
				require.NotContains(t, lowerDown, forbidden)
			}
		})
	}
	for _, baseline := range []string{"uvp-gb28181.sql", "postgresql_converted.sql", "sqlserver_converted.sql", "dameng_converted.sql"} {
		body, err := os.ReadFile(filepath.Join("..", "..", "..", "resource", "database", baseline))
		require.NoError(t, err)
		text := strings.ToLower(string(body))
		require.Contains(t, text, "ck_device_intent_phase")
		require.Contains(t, text, "'settled'")
	}
}
