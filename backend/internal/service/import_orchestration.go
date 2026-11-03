package service

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"gorm.io/gorm"
	"uuid"

	"github.com/pocket-id/pocket-id/backend/internal/storage"
)

// ErrActorsDataWithoutRuntime is returned when an archive carries the actor host's data but no embedded Francis runtime is available to restore it into
// Restoring only the database half of such an archive would leave the deployment mixing two actor generations, so the import refuses instead of silently half-restoring.
var ErrActorsDataWithoutRuntime = errors.New("the archive contains the actor host's data but no embedded Francis runtime is configured to restore it into; restore the runtime with 'francis runtime restore' or use an archive without actor data")

// ErrNoImportToResume is returned when Import is asked to resume but no generation is pending
var ErrNoImportToResume = errors.New("there is no interrupted import to resume")

// ImportOptions controls one import execution
type ImportOptions struct {
	// Resume continues the pending generation instead of creating a new one
	Resume bool

	// GenerationID, Token, and OwnerID identify and fence this specific execution
	// They are generated when empty
	GenerationID string
	Token        string
	OwnerID      string

	// WorkDir is the process-local directory holding the staged archive copy
	// It is recorded in the journal and removed once the generation terminates
	WorkDir string

	// LeaseLost, when non-nil, is closed if the exclusive cluster lease is lost while the import runs
	LeaseLost <-chan struct{}
}

// ownerRenewInterval is how often an execution extends its ownership deadline
const ownerRenewInterval = 10 * time.Second

// rollbackTimeout bounds how long a convergence may run after the originating request was canceled
const rollbackTimeout = 3 * time.Minute

// importPhaseRank orders the phases so resumptions know which steps already committed
var importPhaseRank = map[string]int{
	ImportPhasePrepared:         0,
	ImportPhaseActorsRestored:   1,
	ImportPhaseSchemaShadowed:   2,
	ImportPhaseDatabaseRestored: 3,
	ImportPhaseUploadsPrepared:  4,
	ImportPhaseUploadsCommitted: 5,
	ImportPhaseCommitting:       6,
}

// Import runs the full import from the given ZIP reader as one fenced generation
//
// The database, uploads, and embedded actor state only become a successful instance once all three committed to the same generation; any failure rolls the generation back or leaves a diagnosable pending phase that a later process converges.
func (s *ImportService) Import(ctx context.Context, zr *zip.Reader, opts ImportOptions) (err error) {
	dbData, err := processZipDatabaseJson(zr.File)
	if err != nil {
		return err
	}

	actorEntry := zipFileNamed(zr.File, ActorsBackupFileName)
	if actorEntry != nil && s.actors == nil {
		return ErrActorsDataWithoutRuntime
	}

	if err := EnsureImportJournalTable(ctx, s.db); err != nil {
		return err
	}

	var j *ImportJournal
	if opts.Resume {
		j, err = GetLatestPendingImportJournal(ctx, s.db)
		if err != nil {
			return err
		}
		if j == nil {
			return ErrNoImportToResume
		}
		// Resuming takes over the generation: the caller already holds the exclusive lease, so stealing the owner fences any execution of the previous process
		ownerID := opts.OwnerID
		if ownerID == "" {
			ownerID = uuid.NewV4().String()
		}
		if err := StealImportOwnership(ctx, s.db, j.GenerationID, ownerID, time.Now().Add(ownerLeaseTTL)); err != nil {
			return err
		}
		j.OwnerID = ownerID
	} else {
		pending, err := GetLatestPendingImportJournal(ctx, s.db)
		if err != nil {
			return err
		}
		if pending != nil {
			return fmt.Errorf("%w: generation %s is stuck in phase %q (start the server to roll it back, or rerun with --resume to continue it)", ErrImportAlreadyInProgress, pending.GenerationID, pending.Phase)
		}

		j = s.newJournal(opts, dbData.Version, actorEntry != nil)
		if err := CreateImportJournal(ctx, s.db, j); err != nil {
			return err
		}
	}

	sw, err := storage.NewUploadGeneration(s.storage, j.Token)
	if err != nil {
		return err
	}

	// The guard both renews ownership and cancels the import context when ownership or the exclusive lease is lost
	var leaseLost atomic.Bool
	importCtx, stopGuard := s.runOwnershipGuard(ctx, j, opts.LeaseLost, func() {
		leaseLost.Store(true)
	})
	defer stopGuard()

	finished := false
	var crashed atomic.Bool
	defer func() {
		if finished {
			return
		}
		if crashed.Load() {
			// Simulates a killed process: nothing is rolled back and the journal stays pending for the next process to converge
			return
		}
		if leaseLost.Load() {
			// A process that lost ownership must not touch the generation anymore; the new owner or the next startup converges it
			_ = RecordImportError(context.WithoutCancel(importCtx), s.db, j.GenerationID, err)
			slog.Error("Interrupted import abandoned because ownership or the exclusive lease was lost",
				slog.String("generation", j.GenerationID), slog.Any("error", err))
			return
		}

		// Every other failure triggers a rollback to the previous complete generation
		rbErr := s.rollbackGeneration(context.WithoutCancel(importCtx), j, s.actors, sw, err)
		if rbErr != nil {
			slog.Error("Import failed and the automatic rollback did not finish; the next startup converges it before serving",
				slog.String("generation", j.GenerationID), slog.Any("import_error", err), slog.Any("rollback_error", rbErr))
			err = errors.Join(err, fmt.Errorf("automatic rollback incomplete: %w", rbErr))
		}
	}()

	// advance records the phase, then runs any test fault injection
	// The in-memory phase is set before the journal update so a failure between a committed side effect and its journaling still rolls back every component that may have changed
	advance := func(phase string) error {
		j.Phase = phase
		if err := AdvanceImportPhase(importCtx, s.db, j.GenerationID, j.OwnerID, phase); err != nil {
			return err
		}
		if s.faults != nil {
			if s.faults.crashAtPhase == phase {
				crashed.Store(true)
				return errSimulatedCrash
			}
			if s.faults.failAtPhase == phase {
				return s.faults.failErr
			}
		}
		return nil
	}

	rank := importPhaseRank[j.Phase]
	if rank >= importPhaseRank[ImportPhaseCommitting] {
		if err = s.finalizeCommit(importCtx, j, sw); err != nil {
			return err
		}
		finished = true
		return nil
	}

	// Restore the actor component first, like the previous flow: it validates that no host is connected and is atomic on its own
	if actorEntry != nil && rank < importPhaseRank[ImportPhaseActorsRestored] {
		if err = s.backupAndRestoreActors(importCtx, j, actorEntry); err != nil {
			return err
		}
		if err = advance(ImportPhaseActorsRestored); err != nil {
			return err
		}
	}
	rank = importPhaseRank[j.Phase]

	// Move the old database generation aside, then build the new one into the live schema names
	if rank < importPhaseRank[ImportPhaseDatabaseRestored] {
		hasShadows, err := SchemaHasShadows(importCtx, s.db, j.Token)
		if err != nil {
			return err
		}
		if !hasShadows {
			if err = ShadowSchema(importCtx, s.db, j.Token); err != nil {
				return err
			}
		} else if rank == importPhaseRank[ImportPhaseSchemaShadowed] {
			// A resumed import may have left a partially migrated live schema, so drop it and migrate again while the shadows stay untouched
			if err = DropLivePocketIDTables(importCtx, s.db, j.Token); err != nil {
				return err
			}
		}
		if err = advance(ImportPhaseSchemaShadowed); err != nil {
			return err
		}

		if err = migrateToVersion(importCtx, s.db, dbData.Version); err != nil {
			return err
		}
		if err = s.insertData(dbData); err != nil {
			return err
		}

		if err = carryOverDatabaseStorage(importCtx, s.db, j, sw); err != nil {
			return err
		}

		if err = advance(ImportPhaseDatabaseRestored); err != nil {
			return err
		}
	}

	// Stage and switch uploads
	if importPhaseRank[j.Phase] < importPhaseRank[ImportPhaseUploadsCommitted] {
		entries, err := listUploadEntries(zr.File)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err = stageUploadEntry(importCtx, sw, entry); err != nil {
				return err
			}
		}
		if err = advance(ImportPhaseUploadsPrepared); err != nil {
			return err
		}

		if err = sw.Commit(importCtx); err != nil {
			return err
		}
		if err = advance(ImportPhaseUploadsCommitted); err != nil {
			return err
		}
	}

	// Final commit: from here recovery only converges forward, since the old-generation shadows and backups are about to be deleted
	if err = advance(ImportPhaseCommitting); err != nil {
		return err
	}
	if err = s.finalizeCommit(importCtx, j, sw); err != nil {
		return err
	}

	finished = true
	slog.Info("Import completed successfully", slog.String("generation", j.GenerationID))
	return nil
}

// errSimulatedCrash is returned by the test fault hooks when they simulate the process dying mid-import
var errSimulatedCrash = errors.New("simulated process crash during import")

// newJournal builds a generation row with fresh identity and ownership
func (s *ImportService) newJournal(opts ImportOptions, targetVersion uint, actorsIncluded bool) *ImportJournal {
	generationID := opts.GenerationID
	if generationID == "" {
		generationID = uuid.NewV4().String()
	}
	token := opts.Token
	if token == "" {
		token = strings.ReplaceAll(uuid.NewV4().String(), "-", "")[:12]
	}
	ownerID := opts.OwnerID
	if ownerID == "" {
		ownerID = uuid.NewV4().String()
	}

	return &ImportJournal{
		GenerationID:   generationID,
		Token:          token,
		OwnerID:        ownerID,
		OwnerDeadline:  time.Now().Add(ownerLeaseTTL),
		Phase:          ImportPhasePrepared,
		TargetVersion:  targetVersion,
		ActorsIncluded: actorsIncluded,
		WorkDir:        opts.WorkDir,
	}
}

// runOwnershipGuard renews the owner deadline and cancels the returned context when ownership is lost or the lease goes away
// onLost is invoked exactly when the execution must consider itself fenced, so the caller skips any rollback.
func (s *ImportService) runOwnershipGuard(parent context.Context, j *ImportJournal, leaseLost <-chan struct{}, onLost func()) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	stop := make(chan struct{})

	go func() {
		ticker := time.NewTicker(ownerRenewInterval)
		defer ticker.Stop()
		lost := false
		markLost := func() {
			if !lost {
				lost = true
				onLost()
			}
			cancel()
		}
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-leaseLost:
				slog.Warn("Exclusive lease lost during import", slog.String("generation", j.GenerationID))
				markLost()
				return
			case <-ticker.C:
				err := RenewImportOwnership(ctx, s.db, j.GenerationID, j.OwnerID, time.Now().Add(ownerLeaseTTL))
				if err != nil {
					slog.Warn("Import ownership lost", slog.String("generation", j.GenerationID), slog.Any("error", err))
					markLost()
					return
				}
			}
		}
	}()

	return ctx, func() {
		close(stop)
		cancel()
	}
}

// backupAndRestoreActors streams the current actor generation into the journal for rollback, then loads the archive's generation
func (s *ImportService) backupAndRestoreActors(ctx context.Context, j *ImportJournal, entry *zip.File) error {
	var old bytes.Buffer
	if err := s.actors.Backup(ctx, &old); err != nil {
		return fmt.Errorf("failed to back up the current actor host's data: %w", err)
	}
	if err := UpdateImportJournal(ctx, s.db, j.GenerationID, j.OwnerID, map[string]any{
		"old_actors": old.Bytes(),
	}); err != nil {
		return err
	}
	j.OldActors = old.Bytes()

	if err := restoreActorsEntry(ctx, s.actors, entry); err != nil {
		return err
	}
	return nil
}

// restoreActorsEntry loads the actor host's data of one archive entry through the provider
func restoreActorsEntry(ctx context.Context, actors ActorsBackupProvider, entry *zip.File) error {
	rc, err := entry.Open()
	if err != nil {
		return fmt.Errorf("failed to open %s: %w", ActorsBackupFileName, err)
	}
	defer rc.Close()

	if err := actors.Restore(ctx, rc); err != nil {
		return fmt.Errorf("failed to restore the actor host's data: %w", err)
	}
	return nil
}

// storageShadowCarrier is implemented by the database-backed upload generation, whose previous file rows are parked in a shadow table
type storageShadowCarrier interface {
	CarryOverFromShadow(ctx context.Context, shadowTable string) error
}

// carryOverDatabaseStorage preserves files that exist in the old generation but are absent from the archive when uploads live in the database
func carryOverDatabaseStorage(ctx context.Context, db *gorm.DB, j *ImportJournal, sw storage.UploadGeneration) error {
	carrier, ok := sw.(storageShadowCarrier)
	if !ok {
		return nil
	}

	sqlDb, err := db.DB()
	if err != nil {
		return fmt.Errorf("failed to get sql.DB: %w", err)
	}
	tables, err := listTablesWithPrefix(ctx, sqlDb, db.Name(), shadowPrefix(j.Token))
	if err != nil {
		return err
	}
	shadowStorage := shadowName(j.Token, "storage")
	for _, t := range tables {
		if t == shadowStorage {
			return carrier.CarryOverFromShadow(ctx, shadowStorage)
		}
	}
	return nil
}

// finalizeCommit finishes a generation by removing every old-generation artifact, converging forward
func (s *ImportService) finalizeCommit(ctx context.Context, j *ImportJournal, sw storage.UploadGeneration) error {
	// Switching uploads is idempotent, so a restart in the committing phase converges it again first
	if err := sw.Commit(ctx); err != nil {
		return fmt.Errorf("failed to finish the uploads switch: %w", err)
	}
	if err := sw.Cleanup(ctx); err != nil {
		return fmt.Errorf("failed to clean up staged uploads: %w", err)
	}
	if err := DropShadows(ctx, s.db, j.Token); err != nil {
		return err
	}
	if err := cleanupWorkDir(j.WorkDir); err != nil {
		slog.Warn("Failed to remove the import work directory", slog.String("dir", j.WorkDir), slog.Any("error", err))
	}
	return DeleteImportJournal(ctx, s.db, j.GenerationID)
}

// rollbackGeneration restores the previous complete generation and removes the pending row
// It returns an error if any component could not be rolled back, in which case the row stays pending for another attempt
func (s *ImportService) rollbackGeneration(ctx context.Context, j *ImportJournal, actors ActorsBackupProvider, sw storage.UploadGeneration, cause error) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, rollbackTimeout)
	defer cancel()
	ctx = timeoutCtx

	if err := RecordImportError(ctx, s.db, j.GenerationID, cause); err != nil {
		slog.Warn("Failed to record the import error in the journal", slog.Any("error", err))
	}

	var rollbackErrs []error
	rank := importPhaseRank[j.Phase]

	if rank >= importPhaseRank[ImportPhaseUploadsPrepared] {
		if err := sw.Rollback(ctx); err != nil {
			rollbackErrs = append(rollbackErrs, fmt.Errorf("uploads rollback failed: %w", err))
		}
	}

	if rank >= importPhaseRank[ImportPhaseSchemaShadowed] {
		if err := RestoreSchema(ctx, s.db, j.Token); err != nil {
			rollbackErrs = append(rollbackErrs, fmt.Errorf("database rollback failed: %w", err))
		}
	}

	if rank >= importPhaseRank[ImportPhaseActorsRestored] && j.ActorsIncluded {
		if actors == nil {
			rollbackErrs = append(rollbackErrs, errors.New("actor rollback is required but no embedded Francis runtime provider is available; refusing to leave mixed actor state"))
		} else if len(j.OldActors) == 0 {
			rollbackErrs = append(rollbackErrs, errors.New("actor rollback is required but the previous actor generation was not recorded in the journal"))
		} else if err := actors.Restore(ctx, bytes.NewReader(j.OldActors)); err != nil {
			rollbackErrs = append(rollbackErrs, fmt.Errorf("actor rollback failed: %w", err))
		}
	}

	if len(rollbackErrs) > 0 {
		return errors.Join(rollbackErrs...)
	}

	if err := sw.Cleanup(ctx); err != nil {
		slog.Warn("Failed to clean up staged uploads after rollback", slog.Any("error", err))
	}
	if err := cleanupWorkDir(j.WorkDir); err != nil {
		slog.Warn("Failed to remove the import work directory", slog.String("dir", j.WorkDir), slog.Any("error", err))
	}
	return DeleteImportJournal(ctx, s.db, j.GenerationID)
}

// RecoverPending converges every pending import generation before the instance is allowed to serve
//
// A generation already in its committing phase converges forward (commit is the only remaining action); every other phase rolls back to the previous complete generation.
func (s *ImportService) RecoverPending(ctx context.Context, actors ActorsBackupProvider) (int, error) {
	if err := EnsureImportJournalTable(ctx, s.db); err != nil {
		return 0, err
	}

	pending, err := ListPendingImportJournals(ctx, s.db)
	if err != nil {
		return 0, err
	}
	if len(pending) == 0 {
		return 0, nil
	}

	ownerID := uuid.NewV4().String()
	recovered := 0
	for _, j := range pending {
		// Recovery is serialized by the exclusive cluster lease acquired by the caller, so taking over the owner is safe and fences a process that might still be alive
		if err := StealImportOwnership(ctx, s.db, j.GenerationID, ownerID, time.Now().Add(ownerLeaseTTL)); err != nil {
			return recovered, err
		}
		j.OwnerID = ownerID

		sw, err := storage.NewUploadGeneration(s.storage, j.Token)
		if err != nil {
			return recovered, fmt.Errorf("failed to prepare recovery for generation %s: %w", j.GenerationID, err)
		}

		if importPhaseRank[j.Phase] >= importPhaseRank[ImportPhaseCommitting] {
			err = s.finalizeCommit(ctx, j, sw)
		} else {
			err = s.rollbackGeneration(ctx, j, actors, sw, fmt.Errorf("recovering interrupted import from phase %q", j.Phase))
		}
		if err != nil {
			return recovered, fmt.Errorf("failed to converge import generation %s (phase %q): %w", j.GenerationID, j.Phase, err)
		}
		slog.Info("Converged interrupted import generation", slog.String("generation", j.GenerationID), slog.String("phase", j.Phase))
		recovered++
	}
	return recovered, nil
}

func cleanupWorkDir(workDir string) error {
	if workDir == "" {
		return nil
	}
	if err := os.RemoveAll(workDir); err != nil {
		return fmt.Errorf("failed to remove import work directory %q: %w", workDir, err)
	}
	return nil
}

func zipFileNamed(files []*zip.File, name string) *zip.File {
	for _, f := range files {
		if f.Name == name {
			return f
		}
	}
	return nil
}

const (
	uploadsPrefix     = "uploads/"
	maxUploadFileSize = 50 << 20 // 50 MiB
)

// listUploadEntries returns the files carried under uploads/ in archive order
func listUploadEntries(files []*zip.File) ([]*zip.File, error) {
	entries := make([]*zip.File, 0, len(files))
	for _, f := range files {
		if !strings.HasPrefix(f.Name, uploadsPrefix) {
			continue
		}
		if strings.HasSuffix(f.Name, "/") {
			continue
		}
		if f.UncompressedSize64 > maxUploadFileSize {
			return nil, fmt.Errorf("file %s too large (%d bytes)", f.Name, f.UncompressedSize64)
		}
		targetPath := strings.TrimPrefix(f.Name, uploadsPrefix)
		if targetPath == "" {
			continue
		}
		entries = append(entries, f)
	}
	return entries, nil
}

// stageUploadEntry streams one archive file into the new generation without touching live storage
func stageUploadEntry(ctx context.Context, sw storage.UploadGeneration, f *zip.File) error {
	targetPath := strings.TrimPrefix(f.Name, uploadsPrefix)

	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("failed to open %s: %w", f.Name, err)
	}
	defer rc.Close()

	if err := sw.StageFile(ctx, targetPath, rc); err != nil {
		return fmt.Errorf("failed to stage file %s: %w", targetPath, err)
	}
	return nil
}
