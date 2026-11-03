package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/italypaleale/francis/clusteradmin"
	"github.com/italypaleale/francis/components"
	"github.com/jackc/pgx/v5/pgxpool"
	"gorm.io/gorm"

	"github.com/pocket-id/pocket-id/backend/internal/common"
)

// AcquireExclusiveAccess takes an exclusive-access lease on the embedded Francis cluster so an import can safely overwrite the database
//
// It returns a release function that must be called once the operation is done, and a channel that is closed if the lease is lost while it is held.
func AcquireExclusiveAccess(ctx context.Context, db *gorm.DB, pg *pgxpool.Pool, force bool) (release func(), lost <-chan struct{}, err error) {
	// New initializes the provider, applying the actor host's schema migrations, so this also works against a brand-new (empty) database
	providerOpts, err := ActorsProviderOptions(db, pg)
	if err != nil {
		return nil, nil, err
	}

	admin, err := clusteradmin.New(ctx, providerOpts, clusteradmin.Options{
		// Match the actor host so the admin waits the right amount of time for hosts to drain
		HostHealthCheckDeadline: ActorsHostHealthCheckDeadline(common.EnvConfig.HAEnabled),
		Logger:                  slog.Default(),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create cluster admin: %w", err)
	}

	lost, err = admin.AcquireExclusive(ctx, clusteradmin.AcquireOptions{Force: force})
	if err != nil {
		_ = admin.Close()
		switch {
		case errors.Is(err, components.ErrHostsConnected):
			//nolint:staticcheck
			return nil, nil, errors.New("Pocket ID must be stopped before importing data - please stop the running instance or run with --forcefully-acquire-lock to terminate the other instance")
		case errors.Is(err, components.ErrExclusiveHeld):
			return nil, nil, fmt.Errorf("another exclusive operation, such as another import, is already in progress; please wait for it to complete and try again: %w", components.ErrExclusiveHeld)
		default:
			return nil, nil, fmt.Errorf("failed to acquire exclusive access: %w", err)
		}
	}

	release = func() {
		// The import preserves the actor host's "francis_" tables, including the lease row, so the lease must be released explicitly
		// Detach from ctx so the release still runs even if the import was canceled
		releaseCtx, cancelRelease := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancelRelease()
		rErr := admin.ReleaseExclusive(releaseCtx)
		if rErr != nil {
			slog.WarnContext(ctx, "Failed to release exclusive access", slog.Any("error", rErr))
		}
		_ = admin.Close()
	}
	return release, lost, nil
}
