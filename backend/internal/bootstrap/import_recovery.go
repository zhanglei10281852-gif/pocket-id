package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/italypaleale/francis/components"
	"github.com/jackc/pgx/v5/pgxpool"
	"gorm.io/gorm"

	"github.com/pocket-id/pocket-id/backend/internal/common"
	"github.com/pocket-id/pocket-id/backend/internal/service"
)

// exclusiveAcquirePollInterval is how long startup waits between attempts to take the exclusive lease from a possibly dead import
const exclusiveAcquirePollInterval = 2 * time.Second

// pendingOwnerWaitPollInterval is how long startup waits for an expired owner deadline when a standalone Francis runtime provides no lease
const pendingOwnerWaitPollInterval = 2 * time.Second

// RecoverInterruptedImports converges any import generation left pending by a canceled, failed, or killed process before the instance serves traffic
//
// A generation in its final phase is completed forward; every other phase is rolled back to the previous complete generation, so the database, uploads, and the embedded actor state never serve as a mixed generation.
func RecoverInterruptedImports(ctx context.Context, db *gorm.DB, pg *pgxpool.Pool) error {
	if err := service.EnsureImportJournalTable(ctx, db); err != nil {
		return err
	}

	pending, err := service.ListPendingImportJournals(ctx, db)
	if err != nil {
		return fmt.Errorf("failed to inspect the import journal: %w", err)
	}
	if len(pending) == 0 {
		return nil
	}

	slog.Warn("Found an interrupted import; converging it before starting the instance",
		slog.String("generation", pending[0].GenerationID), slog.String("phase", pending[0].Phase))

	// File storage is needed to converge the uploads switch; it is closed before normal startup builds its own long-lived handle
	fileStorage, err := InitStorage(ctx, db)
	if err != nil {
		return fmt.Errorf("failed to initialize file storage for import recovery: %w", err)
	}
	defer func() {
		_ = fileStorage.Close()
	}()

	var actorsProvider service.ActorsBackupProvider
	if common.EnvConfig.HasEmbeddedFrancisRuntime() {
		release, err := acquireRecoveryLease(ctx, db, pg)
		if err != nil {
			return err
		}
		defer release()

		providerOpts, err := ActorsProviderOptions(db, pg)
		if err != nil {
			return err
		}
		provider, err := NewActorsBackupProvider(ctx, providerOpts)
		if err != nil {
			return fmt.Errorf("failed to initialize the actor host's data provider for import recovery: %w", err)
		}
		defer func() {
			_ = provider.Close()
		}()
		actorsProvider = provider
	} else {
		// With a standalone runtime there is no lease in this database, so convergence waits until the previous owner's deadline has demonstrably expired
		if err := waitForPendingOwnerExpiry(ctx, db); err != nil {
			return err
		}
	}

	importService := service.NewImportService(db, fileStorage, actorsProvider)
	recovered, err := importService.RecoverPending(ctx, actorsProvider)
	if err != nil {
		return fmt.Errorf("failed to recover from the interrupted import: %w", err)
	}
	slog.Info("Interrupted import converged", slog.Int("generations", recovered))
	return nil
}

// acquireRecoveryLease takes the exclusive lease, waiting for another live holder to release or expire
func acquireRecoveryLease(ctx context.Context, db *gorm.DB, pg *pgxpool.Pool) (func(), error) {
	for {
		// The lost channel is unused here: recovery is a short, bounded sequence before any host starts, and a lost lease simply leaves the journal row pending for the next attempt
		release, _, err := AcquireExclusiveAccess(ctx, db, pg, true)
		if err == nil {
			return release, nil
		}
		if !errors.Is(err, components.ErrExclusiveHeld) {
			return nil, err
		}

		slog.Warn("Waiting for the in-progress exclusive operation to release or expire")
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(exclusiveAcquirePollInterval):
		}
	}
}

// waitForPendingOwnerExpiry blocks until every pending generation's owner deadline is in the past
func waitForPendingOwnerExpiry(ctx context.Context, db *gorm.DB) error {
	for {
		pending, err := service.ListPendingImportJournals(ctx, db)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			return nil
		}

		wait := time.Until(pending[0].OwnerDeadline)
		if wait <= 0 {
			return nil
		}
		slog.Warn("Waiting for the interrupted import's execution ownership to expire before recovering",
			slog.Duration("wait", wait))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(minDuration(wait+paddedOwnerWait, pendingOwnerWaitPollInterval)):
		}
	}
}

const paddedOwnerWait = time.Second

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
