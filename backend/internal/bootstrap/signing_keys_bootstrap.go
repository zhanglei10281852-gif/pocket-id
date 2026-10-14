package bootstrap

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/italypaleale/francis/builtin/cronjob"
	francishost "github.com/italypaleale/francis/host"

	"github.com/pocket-id/pocket-id/backend/internal/service"
)

const (
	retiredSigningKeysCleanupInterval = 24 * time.Hour
	retiredSigningKeysCleanupJitter   = 5 * time.Minute
)

// registerRetiredSigningKeysCleanup registers the daily actor that removes signing generations after their compatibility window
func registerRetiredSigningKeysCleanup(actors francishost.Host, jwtService *service.JwtService) error {
	job := func(ctx context.Context) error {
		count, err := jwtService.CleanupExpiredRetiredKeys(ctx)
		if err != nil {
			return fmt.Errorf("failed to prune retired OIDC signing keys: %w", err)
		}
		if count > 0 {
			slog.InfoContext(ctx, "Removed retired OIDC signing keys past their compatibility window", slog.Int("count", count))
		}
		return nil
	}

	cronActor, err := cronjob.New(
		"PruneRetiredOIDCSigningKeys",
		cronjob.WithJob(job),
		cronjob.WithInterval(retiredSigningKeysCleanupInterval),
		cronjob.WithJitter(retiredSigningKeysCleanupJitter),
		// Also run at startup so generations that expired while Pocket ID was stopped are removed immediately
		cronjob.WithImmediate(),
		cronjob.WithLogger(slog.Default()),
	)
	if err != nil {
		return fmt.Errorf("error creating PruneRetiredOIDCSigningKeys cron job: %w", err)
	}

	if err := actors.RegisterBuiltInActor(cronActor); err != nil {
		return fmt.Errorf("error registering PruneRetiredOIDCSigningKeys cron actor: %w", err)
	}
	return nil
}
