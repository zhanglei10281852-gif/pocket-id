package cmds

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"uuid"

	"github.com/pocket-id/pocket-id/backend/internal/bootstrap"
	"github.com/pocket-id/pocket-id/backend/internal/common"
	"github.com/pocket-id/pocket-id/backend/internal/service"
	"github.com/pocket-id/pocket-id/backend/internal/storage"
	"github.com/pocket-id/pocket-id/backend/internal/utils"
)

type importFlags struct {
	Path                  string
	PathSet               bool
	Yes                   bool
	ForcefullyAcquireLock bool
	Resume                bool
}

func init() {
	var flags importFlags

	importCmd := &cobra.Command{
		Use:   "import",
		Short: "Imports all data of Pocket ID from a ZIP file",
		RunE: func(cmd *cobra.Command, args []string) error {
			flags.PathSet = cmd.Flags().Changed("path")
			return runImport(cmd.Context(), flags)
		},
	}

	importCmd.Flags().StringVarP(&flags.Path, "path", "p", "pocket-id-export.zip", "Path to the ZIP file to import the data from, or '-' to read from stdin")
	importCmd.Flags().BoolVarP(&flags.Yes, "yes", "y", false, "Skip confirmation prompts")
	importCmd.Flags().BoolVarP(&flags.ForcefullyAcquireLock, "forcefully-acquire-lock", "", false, "Forcefully acquire exclusive access by terminating any running Pocket ID instance")
	importCmd.Flags().BoolVar(&flags.Resume, "resume", false, "Continue an interrupted import instead of starting a new one; reuses the staged archive unless --path is given")

	rootCmd.AddCommand(importCmd)
}

// runImport handles the high-level orchestration of the import process
func runImport(ctx context.Context, flags importFlags) error {
	// A standalone Francis runtime owns the actor data, so this import only covers what lives in Pocket ID's own database
	// Nothing here can limit the replicas either, since they are hosts of the runtime's cluster rather than of a cluster in this database
	embeddedRuntime := common.EnvConfig.HasEmbeddedFrancisRuntime()
	if !embeddedRuntime {
		printRemoteActorDataNotice(
			"The actor data will NOT be restored, and Pocket ID replicas will NOT be stopped for you",
			"stop every replica first, then restore the runtime with: francis runtime restore -f actors.bin",
		)
	}

	// Connect to the database without running migrations: the import re-creates the Pocket ID schema itself
	db, pg, err := bootstrap.ConnectDatabase(ctx)
	if err != nil {
		return err
	}

	if err := service.EnsureImportJournalTable(ctx, db); err != nil {
		return err
	}

	// Determine which generation this execution belongs to before opening the archive
	// A resume reuses the pending generation; a new import gets fresh identity here so the archive can be staged under its work directory
	var (
		generationID, token string
		resumeJournal       *service.ImportJournal
	)
	if flags.Resume {
		resumeJournal, err = service.GetLatestPendingImportJournal(ctx, db)
		if err != nil {
			return err
		}
		if resumeJournal == nil {
			return service.ErrNoImportToResume
		}
		generationID = resumeJournal.GenerationID
		token = resumeJournal.Token
		slog.Warn("Resuming interrupted import", slog.String("generation", generationID), slog.String("phase", resumeJournal.Phase))
	} else {
		generationID = uuid.NewV4().String()
		token = strings.ReplaceAll(uuid.NewV4().String(), "-", "")[:12]
	}

	// Resolve the work directory from configuration so the archive can be located before storage is initialized
	workDir := storage.StagingWorkDirPath(common.EnvConfig.FileBackend, common.EnvConfig.UploadPath, token)

	// Resolve where the archive is read from, defaulting to the staged archive copy when resuming
	archivePath := flags.Path
	if flags.Resume && !flags.PathSet {
		archivePath = filepath.Join(workDir, "archive.zip")
		if _, err := os.Stat(archivePath); err != nil {
			return fmt.Errorf("the staged archive of the interrupted import is gone (%s); rerun with --path pointing at the original ZIP: %w", archivePath, err)
		}
	}

	if !flags.Yes && !flags.Resume {
		if err := askForImportConfirmation(); err != nil {
			return err
		}
	}

	var (
		zipReader *zip.ReadCloser
		cleanup   func()
	)
	if archivePath == "-" {
		zipReader, cleanup, err = readZipFromStdin()
		defer cleanup()
	} else {
		zipReader, err = zip.OpenReader(archivePath)
	}
	if err != nil {
		return fmt.Errorf("failed to open zip: %w", err)
	}
	defer zipReader.Close()

	// An archive carrying the actor data was taken from a deployment with an embedded runtime, and there is nowhere to put that data here
	// Restoring only the Pocket ID half of it would leave the runtime holding actor state from a different deployment, so refuse rather than half-restore, before storage or the lease are touched
	if !embeddedRuntime {
		if err := ensureNoActorsBackup(&zipReader.Reader); err != nil {
			return err
		}
	}

	// Init the storage provider only once the archive is known to fit this topology
	fileStorage, err := bootstrap.InitStorage(ctx, db)
	if err != nil {
		return fmt.Errorf("failed to initialize storage: %w", err)
	}
	defer func() {
		_ = fileStorage.Close()
	}()

	importCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Take exclusive access to the cluster so no Pocket ID replica is running while we overwrite the database
	// The lease lives in the actor host's own tables, so it only exists when the runtime is embedded: with a standalone runtime the operator was told to stop the replicas instead
	var leaseLost <-chan struct{}
	if embeddedRuntime {
		release, lost, err := bootstrap.AcquireExclusiveAccess(ctx, db, pg, flags.ForcefullyAcquireLock)
		if err != nil {
			return err
		}
		defer release()
		leaseLost = lost

		// Abort the import if exclusive access is lost partway through (for example if the lease can no longer be renewed)
		go func() {
			select {
			case <-lost:
				cancel()
			case <-importCtx.Done():
			}
		}()
	}

	// The actor data lives outside of the Pocket ID schema, so it is restored through Francis
	// Restoring requires exclusive access to the cluster, which was acquired above, and the import service skips the actor data entirely when no provider is passed
	var actorsProvider service.ActorsBackupProvider
	if embeddedRuntime {
		providerOpts, err := bootstrap.ActorsProviderOptions(db, pg)
		if err != nil {
			return err
		}
		provider, err := bootstrap.NewActorsBackupProvider(importCtx, providerOpts)
		if err != nil {
			return fmt.Errorf("failed to initialize the actor host's data provider: %w", err)
		}
		defer func() {
			_ = provider.Close()
		}()

		actorsProvider = provider
	}

	// Stage a copy of the archive so a crashed import can be continued with --resume
	// Rollback does not depend on this copy, so a staging failure only disables resume and does not abort the import
	stagedPath := filepath.Join(workDir, "archive.zip")
	if !flags.Resume {
		if err := stageArchiveCopy(&zipReader.Reader, archivePath, stagedPath); err != nil {
			slog.Warn("Failed to stage a copy of the archive; --resume will not be available after a crash, but rollback still will", slog.Any("error", err))
		}
	}

	importService := service.NewImportService(db, fileStorage, actorsProvider)

	err = importService.Import(importCtx, &zipReader.Reader, service.ImportOptions{
		Resume:       flags.Resume,
		GenerationID: generationID,
		Token:        token,
		WorkDir:      workDir,
		LeaseLost:    leaseLost,
	})
	if err != nil {
		return fmt.Errorf("failed to import data from zip: %w", err)
	}

	fmt.Println("Import completed successfully.")
	return nil
}

// stageArchiveCopy places a private copy of the archive in the generation work directory
// Stdin archives were already copied to a temporary file by the ZIP reader, so both source kinds are handled as files
func stageArchiveCopy(zr *zip.Reader, sourcePath, stagedPath string) error {
	if sourcePath == "-" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(stagedPath), 0700); err != nil {
		return fmt.Errorf("failed to create work directory: %w", err)
	}

	src, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("failed to open archive for staging: %w", err)
	}
	defer src.Close()

	tmp := stagedPath + ".tmp"
	dst, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("failed to create staged archive: %w", err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("failed to copy archive into the work directory: %w", err)
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, stagedPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func askForImportConfirmation() error {
	fmt.Println("WARNING: This feature is experimental and may not work correctly. Please create a backup before proceeding and report any issues you encounter.")
	fmt.Println()
	fmt.Println("WARNING: Import will erase all existing data at the following locations:")
	fmt.Printf("Database:      %s\n", absolutePathOrOriginal(common.EnvConfig.DbConnectionString))
	fmt.Printf("Uploads Path:  %s\n", absolutePathOrOriginal(common.EnvConfig.UploadPath))

	ok, err := utils.PromptForConfirmation("Do you want to continue?")
	if err != nil {
		return fmt.Errorf("failed to get confirmation: %w", err)
	}
	if !ok {
		fmt.Println("Aborted")
		os.Exit(1)
	}
	return nil
}

// absolutePathOrOriginal returns the absolute path of the given path, or the original if it fails
func absolutePathOrOriginal(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}

// readZipFromStdin copies stdin to a temporary file and opens it as a ZIP reader
func readZipFromStdin() (*zip.ReadCloser, func(), error) {
	tmpFile, err := os.CreateTemp("", "pocket-id-import-*.zip")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create temporary file: %w", err)
	}

	cleanup := func() {
		_ = os.Remove(tmpFile.Name())
	}

	_, err = io.Copy(tmpFile, os.Stdin)
	if err != nil {
		tmpFile.Close()
		cleanup()
		return nil, nil, fmt.Errorf("failed to read data from stdin: %w", err)
	}

	err = tmpFile.Close()
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("failed to close temporary file: %w", err)
	}

	r, err := zip.OpenReader(tmpFile.Name())
	if err != nil {
		cleanup()
		return nil, nil, err
	}

	return r, cleanup, nil
}
