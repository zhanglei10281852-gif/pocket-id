package service

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"gorm.io/gorm"
)

// shadowPrefix returns the prefix of the shadow tables and indexes of an import generation
// Shadow objects live alongside the live schema so the previous generation survives the schema being rebuilt
// The token keeps multiple generations apart, and the leading "i" plus trailing underscore delimit the original name
func shadowPrefix(token string) string {
	return "i" + token + "_"
}

// shadowName returns the shadow object name for the given generation
func shadowName(token, name string) string {
	return shadowPrefix(token) + name
}

// unshadowName strips the generation prefix from a shadow object name
// It returns the original name and whether the object belongs to the generation
func unshadowName(token, name string) (string, bool) {
	prefix := shadowPrefix(token)
	if !strings.HasPrefix(name, prefix) {
		return "", false
	}
	return strings.TrimPrefix(name, prefix), true
}

// ShadowSchema renames every live Pocket ID table (and its explicitly created indexes) aside into the generation's shadow namespace
// The old generation stays fully intact in the shadows while the new schema is migrated into the live names, and a rollback only has to rename them back
func ShadowSchema(ctx context.Context, db *gorm.DB, token string) error {
	switch db.Name() {
	case "sqlite":
		return shadowSchemaSQLite(ctx, db, token)
	case "postgres":
		return shadowSchemaPostgres(ctx, db, token)
	default:
		return fmt.Errorf("unsupported database dialect: %s", db.Name())
	}
}

// RestoreSchema drops the live schema and renames the shadowed previous generation back, restoring the database component of the old generation
func RestoreSchema(ctx context.Context, db *gorm.DB, token string) error {
	sqlDb, err := db.DB()
	if err != nil {
		return fmt.Errorf("failed to get sql.DB: %w", err)
	}

	switch db.Name() {
	case "sqlite":
		return restoreSchemaSQLite(ctx, sqlDb, token)
	case "postgres":
		return restoreSchemaPostgres(ctx, sqlDb, token)
	default:
		return fmt.Errorf("unsupported database dialect: %s", db.Name())
	}
}

// DropShadows removes the shadow objects of a generation after the new generation has committed
func DropShadows(ctx context.Context, db *gorm.DB, token string) error {
	sqlDb, err := db.DB()
	if err != nil {
		return fmt.Errorf("failed to get sql.DB: %w", err)
	}

	tables, err := listTablesWithPrefix(ctx, sqlDb, db.Name(), shadowPrefix(token))
	if err != nil {
		return err
	}

	switch db.Name() {
	case "sqlite":
		conn, err := sqlDb.Conn(ctx)
		if err != nil {
			return err
		}
		defer conn.Close()

		if _, err = conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
			return fmt.Errorf("failed to disable foreign keys: %w", err)
		}
		for _, t := range tables {
			if _, err = conn.ExecContext(ctx, `DROP TABLE IF EXISTS "`+t+`"`); err != nil {
				return fmt.Errorf("failed to drop shadow table %q: %w", t, err)
			}
		}
		return nil
	case "postgres":
		for _, t := range tables {
			if _, err = sqlDb.ExecContext(ctx, `DROP TABLE IF EXISTS "`+t+`" CASCADE`); err != nil {
				return fmt.Errorf("failed to drop shadow table %q: %w", t, err)
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported database dialect: %s", db.Name())
	}
}

// SchemaHasShadows reports whether the generation's shadow tables are present
func SchemaHasShadows(ctx context.Context, db *gorm.DB, token string) (bool, error) {
	sqlDb, err := db.DB()
	if err != nil {
		return false, fmt.Errorf("failed to get sql.DB: %w", err)
	}
	tables, err := listTablesWithPrefix(ctx, sqlDb, db.Name(), shadowPrefix(token))
	if err != nil {
		return false, err
	}
	return len(tables) > 0, nil
}

// listLivePocketIDTables returns the live Pocket ID tables, excluding SQLite internals, the actor host's tables, the journal table, and the generation's shadows
func listLivePocketIDTables(ctx context.Context, sqlDb *sql.DB, dialect, token string) ([]string, error) {
	var (
		query string
		args  []any
	)
	switch dialect {
	case "sqlite":
		// GLOB treats "_" as a literal, unlike LIKE where it matches any character
		query = `
			SELECT name FROM sqlite_master
			WHERE type = 'table'
				AND name NOT GLOB 'sqlite_*'
				AND name NOT GLOB 'francis_*'
				AND name <> ?
				AND name NOT GLOB ?`
		args = []any{ImportJournalTableName, shadowPrefix(token) + "*"}
	case "postgres":
		query = `
			SELECT tablename FROM pg_tables
			WHERE schemaname = current_schema()
				AND tablename NOT LIKE 'francis\_%' ESCAPE '\'
				AND tablename <> $1
				AND tablename NOT LIKE $2 ESCAPE '\'`
		args = []any{ImportJournalTableName, postgresLiteralLikePrefix(shadowPrefix(token))}
	default:
		return nil, fmt.Errorf("unsupported database dialect: %s", dialect)
	}
	return queryTableNames(ctx, sqlDb, query, args...)
}

func listTablesWithPrefix(ctx context.Context, sqlDb *sql.DB, dialect, prefix string) ([]string, error) {
	var (
		query string
		args  []any
	)
	switch dialect {
	case "sqlite":
		query = `SELECT name FROM sqlite_master WHERE type = 'table' AND name GLOB ? ORDER BY name`
		args = []any{prefix + "*"}
	case "postgres":
		query = `SELECT tablename FROM pg_tables WHERE schemaname = current_schema() AND tablename LIKE $1 ESCAPE '\' ORDER BY tablename`
		args = []any{postgresLiteralLikePrefix(prefix)}
	default:
		return nil, fmt.Errorf("unsupported database dialect: %s", dialect)
	}
	return queryTableNames(ctx, sqlDb, query, args...)
}

// postgresLiteralLikePrefix escapes LIKE wildcards in a prefix that must match literally, and adds the trailing wildcard
func postgresLiteralLikePrefix(prefix string) string {
	return strings.ReplaceAll(prefix, "_", "\\_") + "%"
}

func queryTableNames(ctx context.Context, sqlDb *sql.DB, query string, args ...any) ([]string, error) {
	rows, err := sqlDb.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list tables: %w", err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, name)
	}
	return tables, rows.Err()
}

// listExplicitIndexesSQLite returns the names of indexes created with CREATE INDEX for the given table
// Indexes backing inline PRIMARY KEY/UNIQUE constraints are named sqlite_autoindex_* and follow table renames automatically, so they are excluded
func listExplicitIndexesSQLite(ctx context.Context, conn *sql.Conn, table string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `PRAGMA index_list("`+table+`")`)
	if err != nil {
		return nil, fmt.Errorf("failed to list indexes of %q: %w", table, err)
	}
	defer rows.Close()

	var indexes []string
	for rows.Next() {
		var (
			seq                   int
			name, origin, partial string
			unique                int
		)
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			return nil, err
		}
		if origin == "c" {
			indexes = append(indexes, name)
		}
	}
	return indexes, rows.Err()
}

func shadowSchemaSQLite(ctx context.Context, db *gorm.DB, token string) error {
	sqlDb, err := db.DB()
	if err != nil {
		return fmt.Errorf("failed to get sql.DB: %w", err)
	}

	conn, err := sqlDb.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	tables, err := listLivePocketIDTables(ctx, sqlDb, "sqlite", token)
	if err != nil {
		return err
	}

	// Rename on one pinned connection with foreign keys disabled and inside one transaction
	// foreign_keys is a per-connection pragma, and with legacy_alter_table off (the default) renaming a table rewrites the foreign-key clauses that point at it, which keeps the shadow set self-consistent
	if _, err = conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return fmt.Errorf("failed to disable foreign keys: %w", err)
	}
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
	}()

	// Tables are renamed first: with legacy_alter_table off, SQLite rewrites the definitions of their indexes to reference the shadow table names
	for _, table := range tables {
		if _, err = conn.ExecContext(ctx, `ALTER TABLE "`+table+`" RENAME TO "`+shadowName(token, table)+`"`); err != nil {
			return fmt.Errorf("failed to rename table %q: %w", table, err)
		}
	}

	// SQLite has no ALTER INDEX RENAME, so the named indexes are dropped and recreated under shadow names, reusing the definition the table rename updated
	for _, table := range tables {
		shadowTable := shadowName(token, table)
		indexes, err := listExplicitIndexesSQLite(ctx, conn, shadowTable)
		if err != nil {
			return err
		}
		for _, index := range indexes {
			if err = recreateSQLiteIndex(ctx, conn, index, shadowName(token, index)); err != nil {
				return err
			}
		}
	}

	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("failed to commit schema shadowing: %w", err)
	}
	committed = true
	return nil
}

// recreateSQLiteIndex drops oldName and recreates the index as newName using the stored CREATE statement, with only the index name replaced
func recreateSQLiteIndex(ctx context.Context, conn *sql.Conn, oldName, newName string) error {
	var sqlText string
	err := conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`, oldName).Scan(&sqlText)
	if err != nil {
		return fmt.Errorf("failed to read definition of index %q: %w", oldName, err)
	}

	rewritten, err := rewriteIndexText(sqlText, oldName, newName)
	if err != nil {
		return err
	}

	if _, err = conn.ExecContext(ctx, `DROP INDEX "`+oldName+`"`); err != nil {
		return fmt.Errorf("failed to drop index %q: %w", oldName, err)
	}
	if _, err = conn.ExecContext(ctx, rewritten); err != nil {
		return fmt.Errorf("failed to recreate index %q as %q: %w", oldName, newName, err)
	}
	return nil
}

// rewriteIndexName replaces the index name in a "CREATE [UNIQUE] INDEX name ON ..." statement
func rewriteIndexText(sqlText, oldName, newName string) (string, error) {
	pattern := `(?i)^(\s*CREATE\s+(?:UNIQUE\s+)?INDEX\s+)` + regexp.QuoteMeta(oldName) + `(\s+ON\s+)`
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", err
	}
	if !re.MatchString(sqlText) {
		return "", fmt.Errorf("unexpected index definition for %q: %s", oldName, sqlText)
	}
	return re.ReplaceAllString(sqlText, "${1}"+newName+"${2}"), nil
}

func restoreSchemaSQLite(ctx context.Context, sqlDb *sql.DB, token string) error {
	conn, err := sqlDb.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err = conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return fmt.Errorf("failed to disable foreign keys: %w", err)
	}
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
	}()

	// The partially imported live schema goes away first, which also frees every index name
	liveTables, err := listLivePocketIDTables(ctx, sqlDb, "sqlite", token)
	if err != nil {
		return err
	}
	for _, t := range liveTables {
		if _, err = conn.ExecContext(ctx, `DROP TABLE IF EXISTS "`+t+`"`); err != nil {
			return fmt.Errorf("failed to drop imported table %q: %w", t, err)
		}
	}

	shadows, err := listTablesWithPrefix(ctx, sqlDb, "sqlite", shadowPrefix(token))
	if err != nil {
		return err
	}

	// Rename the shadow tables back first so their index definitions reference the live table names again
	for _, shadowTable := range shadows {
		original, ok := unshadowName(token, shadowTable)
		if !ok {
			continue
		}
		if _, err = conn.ExecContext(ctx, `ALTER TABLE "`+shadowTable+`" RENAME TO "`+original+`"`); err != nil {
			return fmt.Errorf("failed to restore table %q: %w", shadowTable, err)
		}
	}

	// Then drop and recreate the shadowed indexes under their original names
	for _, shadowTable := range shadows {
		original, ok := unshadowName(token, shadowTable)
		if !ok {
			continue
		}
		indexes, err := listExplicitIndexesSQLite(ctx, conn, original)
		if err != nil {
			return err
		}
		for _, index := range indexes {
			originalIndex, ok := unshadowName(token, index)
			if !ok {
				continue
			}
			if err = recreateSQLiteIndex(ctx, conn, index, originalIndex); err != nil {
				return fmt.Errorf("failed to restore index %q: %w", index, err)
			}
		}
	}

	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("failed to commit schema restore: %w", err)
	}
	committed = true
	return nil
}

func shadowSchemaPostgres(ctx context.Context, db *gorm.DB, token string) error {
	sqlDb, err := db.DB()
	if err != nil {
		return fmt.Errorf("failed to get sql.DB: %w", err)
	}

	tables, err := listLivePocketIDTables(ctx, sqlDb, "postgres", token)
	if err != nil {
		return err
	}

	// All DDL runs in one transaction, which Postgres commits atomically
	tx, err := sqlDb.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Rename every index of the tables, including the indexes backing primary-key constraints, since index names are schema-global and would collide with the ones the fresh migration creates
	for _, table := range tables {
		indexes, err := listPostgresTableIndexes(ctx, tx, table)
		if err != nil {
			return err
		}
		for _, index := range indexes {
			if _, err = tx.ExecContext(ctx, `ALTER INDEX "`+index+`" RENAME TO "`+shadowName(token, index)+`"`); err != nil {
				return fmt.Errorf("failed to rename index %q: %w", index, err)
			}
		}

		if _, err = tx.ExecContext(ctx, `ALTER TABLE "`+table+`" RENAME TO "`+shadowName(token, table)+`"`); err != nil {
			return fmt.Errorf("failed to rename table %q: %w", table, err)
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit schema shadowing: %w", err)
	}
	committed = true
	return nil
}

func restoreSchemaPostgres(ctx context.Context, sqlDb *sql.DB, token string) error {
	tx, err := sqlDb.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	liveTables, err := listLivePocketIDTables(ctx, sqlDb, "postgres", token)
	if err != nil {
		return err
	}
	for _, t := range liveTables {
		if _, err = tx.ExecContext(ctx, `DROP TABLE IF EXISTS "`+t+`" CASCADE`); err != nil {
			return fmt.Errorf("failed to drop imported table %q: %w", t, err)
		}
	}

	shadows, err := listTablesWithPrefix(ctx, sqlDb, "postgres", shadowPrefix(token))
	if err != nil {
		return err
	}
	for _, shadowTable := range shadows {
		indexes, err := listPostgresTableIndexes(ctx, tx, shadowTable)
		if err != nil {
			return err
		}
		for _, index := range indexes {
			original, ok := unshadowName(token, index)
			if !ok {
				continue
			}
			if _, err = tx.ExecContext(ctx, `ALTER INDEX "`+index+`" RENAME TO "`+original+`"`); err != nil {
				return fmt.Errorf("failed to restore index %q: %w", index, err)
			}
		}
	}

	for _, shadowTable := range shadows {
		original, ok := unshadowName(token, shadowTable)
		if !ok {
			continue
		}
		if _, err = tx.ExecContext(ctx, `ALTER TABLE "`+shadowTable+`" RENAME TO "`+original+`"`); err != nil {
			return fmt.Errorf("failed to restore table %q: %w", shadowTable, err)
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit schema restore: %w", err)
	}
	committed = true
	return nil
}

// listPostgresTableIndexes returns every index attached to the given table
func listPostgresTableIndexes(ctx context.Context, tx *sql.Tx, table string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT i.relname
		FROM pg_index AS x
		JOIN pg_class AS i ON i.oid = x.indexrelid
		JOIN pg_class AS t ON t.oid = x.indrelid
		JOIN pg_namespace AS n ON n.oid = t.relnamespace
		WHERE n.nspname = current_schema() AND t.relname = $1
		ORDER BY i.relname`, table)
	if err != nil {
		return nil, fmt.Errorf("failed to list indexes of %q: %w", table, err)
	}
	defer rows.Close()

	var indexes []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		indexes = append(indexes, name)
	}
	return indexes, rows.Err()
}

// DropLivePocketIDTables drops the current Pocket ID tables while preserving shadows, the journal, and the actor host's tables
// A resumed import uses it to discard a partially built schema before migrating it again
func DropLivePocketIDTables(ctx context.Context, db *gorm.DB, token string) error {
	sqlDb, err := db.DB()
	if err != nil {
		return fmt.Errorf("failed to get sql.DB: %w", err)
	}

	tables, err := listLivePocketIDTables(ctx, sqlDb, db.Name(), token)
	if err != nil {
		return err
	}

	switch db.Name() {
	case "sqlite":
		conn, err := sqlDb.Conn(ctx)
		if err != nil {
			return err
		}
		defer conn.Close()

		if _, err = conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
			return fmt.Errorf("failed to disable foreign keys: %w", err)
		}
		for _, t := range tables {
			if _, err = conn.ExecContext(ctx, `DROP TABLE IF EXISTS "`+t+`"`); err != nil {
				return fmt.Errorf("failed to drop table %q: %w", t, err)
			}
		}
		return nil
	case "postgres":
		for _, t := range tables {
			if _, err = sqlDb.ExecContext(ctx, `DROP TABLE IF EXISTS "`+t+`" CASCADE`); err != nil {
				return fmt.Errorf("failed to drop table %q: %w", t, err)
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported database dialect: %s", db.Name())
	}
}

// orderedImportTables returns every table in the export in insertion order, without duplicates
// Tables owned by Pocket ID internals or the actor host are never imported from an archive
func orderedImportTables(dbData DatabaseExport) []string {
	isImportable := func(t string) bool {
		return t != "schema_migrations" && t != ImportJournalTableName && !strings.HasPrefix(t, "francis_")
	}

	tables := make([]string, 0, len(dbData.Tables))
	seen := make(map[string]struct{}, len(dbData.Tables))
	for _, t := range dbData.TableOrder {
		if _, ok := dbData.Tables[t]; !ok || !isImportable(t) {
			continue
		}
		tables = append(tables, t)
		seen[t] = struct{}{}
	}
	for t := range dbData.Tables {
		if _, ok := seen[t]; ok || !isImportable(t) {
			continue
		}
		tables = append(tables, t)
	}
	return tables
}
