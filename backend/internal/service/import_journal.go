package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// ImportJournalTableName is the name of the table that records in-progress imports
// It is excluded from exports and preserved while an import rebuilds every other Pocket ID table
const ImportJournalTableName = "import_journal"

// Import phases recorded in the journal
//
// The phases describe which destructive steps have already committed, so any process can tell from the row alone which old-generation components still exist and how to converge the import.
const (
	// ImportPhasePrepared means the journal exists, but no component has been replaced yet
	ImportPhasePrepared = "prepared"
	// ImportPhaseActorsRestored means the embedded actor host's state is already the new generation
	ImportPhaseActorsRestored = "actors_restored"
	// ImportPhaseSchemaShadowed means the old database tables are renamed aside and no longer live
	ImportPhaseSchemaShadowed = "schema_shadowed"
	// ImportPhaseDatabaseRestored means the new schema is migrated and populated
	ImportPhaseDatabaseRestored = "database_restored"
	// ImportPhaseUploadsPrepared means every file from the archive is staged outside live storage
	ImportPhaseUploadsPrepared = "uploads_prepared"
	// ImportPhaseUploadsCommitted means the staged files have been switched into live storage
	ImportPhaseUploadsCommitted = "uploads_committed"
	// ImportPhaseCommitting means all components are the new generation and only old-generation cleanup remains
	// Recovery from this phase converges forward: rollback is no longer possible
	ImportPhaseCommitting = "committing"
)

// ErrImportOwnershipLost is returned when an import operation can no longer be fenced by its owner, for example because another process acquired the generation after the lease was lost
var ErrImportOwnershipLost = errors.New("this import process no longer owns the import generation and must stop without committing")

// ErrImportAlreadyInProgress is returned when a new import is started while another generation is still pending
var ErrImportAlreadyInProgress = errors.New("another import is already in progress")

// ownerLeaseTTL is how long an owner keeps a generation without renewing it
// It is short on purpose: a restarted process only has to wait this long before it can safely steal a generation whose owner died
const ownerLeaseTTL = 45 * time.Second

// ImportJournal is one persistently identifiable import generation and the ownership of its execution
type ImportJournal struct {
	GenerationID  string
	Token         string
	OwnerID       string
	OwnerDeadline time.Time
	Phase         string

	// TargetVersion is the database migration version the archive carries
	TargetVersion uint

	// ActorsIncluded records whether the archive carries the actor host's data, which decides whether the actor component participates in the import
	ActorsIncluded bool

	// OldActors is the backup stream of the previous actor generation, retained so a rollback can restore it after the new one was loaded
	OldActors []byte

	// WorkDir is the absolute, process-local staging directory holding the staged archive, when one could be created
	WorkDir string

	// UploadsState holds backend-specific state needed to converge the uploads switch, for example S3 manifests
	UploadsState []byte

	// LastError records the error that interrupted the import, for diagnostics
	LastError string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// EnsureImportJournalTable creates the journal table if it does not exist yet
// The import command runs without applying migrations, and a brand-new database may have no tables at all, so the table cannot be assumed to exist
func EnsureImportJournalTable(ctx context.Context, db *gorm.DB) error {
	var ddl string
	switch db.Name() {
	case "sqlite":
		ddl = sqliteImportJournalDDL
	case "postgres":
		ddl = postgresImportJournalDDL
	default:
		return fmt.Errorf("unsupported database dialect: %s", db.Name())
	}

	if err := db.WithContext(ctx).Exec(ddl).Error; err != nil {
		return fmt.Errorf("failed to create the %s table: %w", ImportJournalTableName, err)
	}
	return nil
}

const sqliteImportJournalDDL = `
CREATE TABLE IF NOT EXISTS import_journal
(
    generation_id   TEXT NOT NULL PRIMARY KEY,
    token           TEXT NOT NULL,
    owner_id        TEXT NOT NULL,
    owner_deadline  DATETIME NOT NULL,
    phase           TEXT NOT NULL,
    target_version  INTEGER NOT NULL,
    actors_included BOOLEAN NOT NULL,
    old_actors      BLOB,
    work_dir        TEXT,
    uploads_state   TEXT,
    last_error      TEXT,
    created_at      DATETIME NOT NULL,
    updated_at      DATETIME NOT NULL
)`

const postgresImportJournalDDL = `
CREATE TABLE IF NOT EXISTS import_journal
(
    generation_id   TEXT NOT NULL PRIMARY KEY,
    token           TEXT NOT NULL,
    owner_id        TEXT NOT NULL,
    owner_deadline  TIMESTAMPTZ NOT NULL,
    phase           TEXT NOT NULL,
    target_version  BIGINT NOT NULL,
    actors_included BOOLEAN NOT NULL,
    old_actors      BYTEA,
    work_dir        TEXT,
    uploads_state   TEXT,
    last_error      TEXT,
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL
)`

// CreateImportJournal inserts a new generation row
func CreateImportJournal(ctx context.Context, db *gorm.DB, j *ImportJournal) error {
	now := time.Now()
	if j.CreatedAt.IsZero() {
		j.CreatedAt = now
	}
	j.UpdatedAt = now

	err := db.WithContext(ctx).Exec(
		`INSERT INTO import_journal
			(generation_id, token, owner_id, owner_deadline, phase, target_version, actors_included, old_actors, work_dir, uploads_state, last_error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.GenerationID, j.Token, j.OwnerID, j.OwnerDeadline, j.Phase, j.TargetVersion, j.ActorsIncluded, j.OldActors, j.WorkDir, j.UploadsState, j.LastError, j.CreatedAt, j.UpdatedAt,
	).Error
	if err != nil {
		return fmt.Errorf("failed to create import journal entry: %w", err)
	}
	return nil
}

// GetImportJournal loads the journal row for the given generation
func GetImportJournal(ctx context.Context, db *gorm.DB, generationID string) (*ImportJournal, error) {
	return queryImportJournal(ctx, db, `generation_id = ?`, generationID)
}

// GetLatestPendingImportJournal loads the most recent journal row that has not reached a terminal state
// Only one pending generation may exist at a time
func GetLatestPendingImportJournal(ctx context.Context, db *gorm.DB) (*ImportJournal, error) {
	return queryImportJournal(ctx, db, `phase <> '' ORDER BY created_at DESC LIMIT 1`)
}

// ListPendingImportJournals loads every non-terminal journal row, newest first
func ListPendingImportJournals(ctx context.Context, db *gorm.DB) ([]*ImportJournal, error) {
	rows, err := db.WithContext(ctx).Raw(
		`SELECT generation_id, token, owner_id, owner_deadline, phase, target_version, actors_included, old_actors, work_dir, uploads_state, last_error, created_at, updated_at
		FROM import_journal
		ORDER BY created_at DESC`,
	).Rows()
	if err != nil {
		return nil, fmt.Errorf("failed to list import journal entries: %w", err)
	}
	defer rows.Close()

	out := make([]*ImportJournal, 0, 1)
	for rows.Next() {
		j := &ImportJournal{}
		if err := scanImportJournal(rows, j); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func queryImportJournal(ctx context.Context, db *gorm.DB, where string, args ...any) (*ImportJournal, error) {
	j := &ImportJournal{}
	row := db.WithContext(ctx).Raw(
		`SELECT generation_id, token, owner_id, owner_deadline, phase, target_version, actors_included, old_actors, work_dir, uploads_state, last_error, created_at, updated_at
		FROM import_journal WHERE `+where,
		args...,
	).Row()
	if err := scanImportJournal(row, j); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return j, nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows
type rowScanner interface {
	Scan(dest ...any) error
}

func scanImportJournal(s rowScanner, j *ImportJournal) error {
	err := s.Scan(
		&j.GenerationID, &j.Token, &j.OwnerID, &j.OwnerDeadline, &j.Phase, &j.TargetVersion, &j.ActorsIncluded, &j.OldActors, &j.WorkDir, &j.UploadsState, &j.LastError, &j.CreatedAt, &j.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to scan import journal entry: %w", err)
	}
	return nil
}

// AdvanceImportPhase moves a generation to the next phase, fenced by the owner
// It returns ErrImportOwnershipLost if the owner no longer holds the generation, which tells the calling process to abort without committing anything
func AdvanceImportPhase(ctx context.Context, db *gorm.DB, generationID, ownerID, phase string) error {
	res := db.WithContext(ctx).Exec(
		`UPDATE import_journal SET phase = ?, updated_at = ? WHERE generation_id = ? AND owner_id = ?`,
		phase, time.Now(), generationID, ownerID,
	)
	if res.Error != nil {
		return fmt.Errorf("failed to update import phase: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrImportOwnershipLost
	}
	return nil
}

// UpdateImportJournal persists mutable fields of the generation, fenced by the owner
// Only the provided fields are written
func UpdateImportJournal(ctx context.Context, db *gorm.DB, generationID, ownerID string, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}

	cols := make([]string, 0, len(fields)+1)
	args := make([]any, 0, len(fields)*2+2)
	for col, val := range fields {
		cols = append(cols, col+" = ?")
		args = append(args, val)
	}
	cols = append(cols, "updated_at = ?")
	args = append(args, time.Now())
	args = append(args, generationID, ownerID)

	query := `UPDATE import_journal SET ` + strings.Join(cols, ", ") + ` WHERE generation_id = ? AND owner_id = ?`
	res := db.WithContext(ctx).Exec(query, args...)
	if res.Error != nil {
		return fmt.Errorf("failed to update import journal entry: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrImportOwnershipLost
	}
	return nil
}

// RecordImportError attaches a diagnostic error to the generation
// It is intentionally NOT fenced: a process that lost ownership must still be able to record why it aborted, while convergence decisions stay owned by the current owner
func RecordImportError(ctx context.Context, db *gorm.DB, generationID string, importErr error) error {
	message := ""
	if importErr != nil {
		message = importErr.Error()
	}
	res := db.WithContext(ctx).Exec(
		`UPDATE import_journal SET last_error = ?, updated_at = ? WHERE generation_id = ?`,
		message, time.Now(), generationID,
	)
	if res.Error != nil {
		return fmt.Errorf("failed to record import error: %w", res.Error)
	}
	return nil
}

// RenewImportOwnership extends the owner deadline
// It returns ErrImportOwnershipLost if the row was stolen, so the caller aborts
func RenewImportOwnership(ctx context.Context, db *gorm.DB, generationID, ownerID string, deadline time.Time) error {
	res := db.WithContext(ctx).Exec(
		`UPDATE import_journal SET owner_deadline = ?, updated_at = ? WHERE generation_id = ? AND owner_id = ?`,
		deadline, time.Now(), generationID, ownerID,
	)
	if res.Error != nil {
		return fmt.Errorf("failed to renew import ownership: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrImportOwnershipLost
	}
	return nil
}

// StealImportOwnership assigns a new owner to a generation
// The caller must already hold the exclusive cluster lease (embedded runtime) or have waited for the previous owner deadline to expire
func StealImportOwnership(ctx context.Context, db *gorm.DB, generationID, newOwnerID string, deadline time.Time) error {
	res := db.WithContext(ctx).Exec(
		`UPDATE import_journal SET owner_id = ?, owner_deadline = ?, updated_at = ? WHERE generation_id = ?`,
		newOwnerID, deadline, time.Now(), generationID,
	)
	if res.Error != nil {
		return fmt.Errorf("failed to acquire import ownership: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("import generation %q not found", generationID)
	}
	return nil
}

// DeleteImportJournal removes the generation row once it reached a terminal state
func DeleteImportJournal(ctx context.Context, db *gorm.DB, generationID string) error {
	if err := db.WithContext(ctx).Exec(`DELETE FROM import_journal WHERE generation_id = ?`, generationID).Error; err != nil {
		return fmt.Errorf("failed to delete import journal entry: %w", err)
	}
	return nil
}
