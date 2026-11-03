package service

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pocket-id/pocket-id/backend/internal/utils"
)

// TestSchemaShadowAndRestore verifies the old database generation survives a shadow/restore cycle with its data, foreign keys, indexes, and schema version intact
func TestSchemaShadowAndRestore(t *testing.T) {
	const token = "0123456789ab"
	dbPath := testDBPath(t)

	// Boot a fully migrated instance with data, the journal table, and the actor host schema
	boot := openImportTestDB(t, dbPath, nil)
	sqlDB, err := boot.DB()
	require.NoError(t, err)
	require.NoError(t, utils.MigrateDatabase(t.Context(), sqlDB))
	require.NoError(t, EnsureImportJournalTable(t.Context(), boot))
	require.NoError(t, boot.Exec(`INSERT INTO kv ("key", "value") VALUES ('old-key', 'old-value')`).Error)
	seedActorHostSchema(t, boot)
	closeImportTestDB(boot)

	// A new process shadows the schema, builds an empty new generation, then restores the old one like recovery would
	imp := openImportTestDB(t, dbPath, nil)
	t.Cleanup(func() { closeImportTestDB(imp) })
	require.NoError(t, EnsureImportJournalTable(t.Context(), imp))

	hasShadows, err := SchemaHasShadows(t.Context(), imp, token)
	require.NoError(t, err)
	require.False(t, hasShadows)

	require.NoError(t, ShadowSchema(t.Context(), imp, token))
	hasShadows, err = SchemaHasShadows(t.Context(), imp, token)
	require.NoError(t, err)
	require.True(t, hasShadows)

	// The live schema is gone while the old one sits in shadow, but the journal and actor host tables survive
	var liveTables int64
	require.NoError(t, imp.Raw(
		`SELECT count(*) FROM sqlite_master
		WHERE type='table'
			AND name NOT GLOB 'sqlite_*'
			AND name NOT GLOB 'francis_*'
			AND name NOT GLOB ?
			AND name <> ?`,
		"i"+token+"_*", ImportJournalTableName).Scan(&liveTables).Error)
	require.Zero(t, liveTables, "every Pocket ID table must be shadowed away")

	var journalRows int64
	require.NoError(t, imp.Raw(`SELECT count(*) FROM `+ImportJournalTableName).Scan(&journalRows).Error)
	requireActorHostSchemaPreserved(t, imp)

	// The old generation is renamed back and must be fully functional again
	require.NoError(t, RestoreSchema(t.Context(), imp, token))
	require.NoError(t, utils.MigrateDatabase(t.Context(), mustSQLDB(t, imp)))
	require.NoError(t, foreignKeyCheck(t, imp))
	requireActorHostSchemaPreserved(t, imp)

	var value string
	require.NoError(t, imp.Raw(`SELECT value FROM kv WHERE key = 'old-key'`).Scan(&value).Error)
	require.Equal(t, "old-value", value)
}

// TestSchemaShadowDropOldGeneration verifies committing drops the shadowed previous generation
func TestSchemaShadowDropOldGeneration(t *testing.T) {
	const token = "0123456789ab"
	dbPath := testDBPath(t)

	boot := openImportTestDB(t, dbPath, nil)
	require.NoError(t, utils.MigrateDatabase(t.Context(), mustSQLDB(t, boot)))
	closeImportTestDB(boot)

	imp := openImportTestDB(t, dbPath, nil)
	t.Cleanup(func() { closeImportTestDB(imp) })

	require.NoError(t, ShadowSchema(t.Context(), imp, token))
	hasShadows, err := SchemaHasShadows(t.Context(), imp, token)
	require.NoError(t, err)
	require.True(t, hasShadows)

	require.NoError(t, DropShadows(t.Context(), imp, token))
	hasShadows, err = SchemaHasShadows(t.Context(), imp, token)
	require.NoError(t, err)
	require.False(t, hasShadows)
}

// TestSchemaShadowKeepsNamedIndexesWorking proves the migration after a shadow can recreate the same named indexes, which requires renaming the old indexes out of the way
func TestSchemaShadowKeepsNamedIndexesWorking(t *testing.T) {
	const token = "fedcba987654"
	dbPath := testDBPath(t)

	boot := openImportTestDB(t, dbPath, nil)
	require.NoError(t, utils.MigrateDatabase(t.Context(), mustSQLDB(t, boot)))
	closeImportTestDB(boot)

	imp := openImportTestDB(t, dbPath, nil)
	t.Cleanup(func() { closeImportTestDB(imp) })

	require.NoError(t, ShadowSchema(t.Context(), imp, token))

	// Migrating to the target version recreates every named index, which collides unless shadowing renamed the old ones
	require.NoError(t, migrateToVersion(t.Context(), imp, importResetTargetVersion))
	require.NoError(t, RestoreSchema(t.Context(), imp, token))
}

func mustSQLDB(t *testing.T, db *gorm.DB) *sql.DB {
	t.Helper()
	d, err := db.DB()
	require.NoError(t, err)
	return d
}

// foreignKeyCheck asserts SQLite finds no foreign-key violations, which would indicate the shadow renames desynchronized references
func foreignKeyCheck(t *testing.T, db *gorm.DB) error {
	t.Helper()
	sqlDB := mustSQLDB(t, db)
	rows, err := sqlDB.QueryContext(t.Context(), `PRAGMA foreign_key_check`)
	require.NoError(t, err)
	defer rows.Close()
	var violations []string
	for rows.Next() {
		var table, target string
		var rowid, fkid any
		require.NoError(t, rows.Scan(&table, &rowid, &target, &fkid))
		violations = append(violations, table+"->"+target)
	}
	require.NoError(t, rows.Err())
	require.Empty(t, violations, "foreign-key violations after schema restore")
	return nil
}
