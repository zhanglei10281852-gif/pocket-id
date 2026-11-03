package storage

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
)

// ImportStagingSegment is the first path segment under which a new uploads generation is staged
const ImportStagingSegment = ".import-staging"

// ImportBackupSegment is the first path segment under which overwritten live files are backed up during an import
const ImportBackupSegment = ".import-backup"

// IsReservedImportPath reports whether the path belongs to import staging or backup artifacts rather than live uploads
// Storage listings and walks hide these paths so exports, cleanup jobs, and application code never see an in-progress import
func IsReservedImportPath(relPath string) bool {
	for len(relPath) > 0 && relPath[0] == '/' {
		relPath = relPath[1:]
	}
	if relPath == ImportStagingSegment || relPath == ImportBackupSegment {
		return true
	}
	if len(relPath) > len(ImportStagingSegment)+1 && relPath[:len(ImportStagingSegment)+1] == ImportStagingSegment+"/" {
		return true
	}
	if len(relPath) > len(ImportBackupSegment)+1 && relPath[:len(ImportBackupSegment)+1] == ImportBackupSegment+"/" {
		return true
	}
	return false
}

// UploadGeneration stages the files of one import generation without touching live storage, then switches live storage to them all at once or rolls the switch back
//
// Every method is safe to call again after a process restart: both Commit and Rollback converge the backend toward their target state instead of assuming the previous run finished.
type UploadGeneration interface {
	// StageFile stores one file from the archive under relPath in the new generation, leaving live storage untouched
	StageFile(ctx context.Context, relPath string, data io.Reader) error

	// Commit makes every staged file live
	// Files that already exist live are moved to the backup namespace before being overwritten, so the switch can be rolled back
	Commit(ctx context.Context) error

	// Rollback restores backed-up files over their live replacements and deletes files that did not exist before the import
	Rollback(ctx context.Context) error

	// Cleanup removes every staging and backup artifact
	Cleanup(ctx context.Context) error
}

// NewUploadGeneration creates the upload switch that matches the configured storage backend
func NewUploadGeneration(fileStorage FileStorage, token string) (UploadGeneration, error) {
	switch s := fileStorage.(type) {
	case *filesystemStorage:
		return newFilesystemUploadGeneration(s, token)
	case *s3Storage:
		return newS3UploadGeneration(s, token), nil
	case *databaseStorage:
		return newDatabaseUploadGeneration(s, token), nil
	default:
		return nil, fmt.Errorf("storage backend %T does not support import generations", fileStorage)
	}
}

// defaultImportWorkDir is the process-local directory that holds import artifacts when the storage backend has no co-located staging directory of its own
const defaultImportWorkDir = "data" + string(filepath.Separator) + "import-work"

// StagingWorkDir returns the absolute directory an import should use for process-local artifacts such as the staged archive copy
//
// Filesystem storage must stage uploads next to the upload root because the file switch is a sequence of atomic renames that require the same filesystem; the other backends use a conventional data directory.
func StagingWorkDir(fileStorage FileStorage, token string) string {
	if rootPather, ok := fileStorage.(interface{ RootPath() string }); ok {
		return filesystemStagingWorkDir(rootPather.RootPath(), token)
	}
	return filepath.Join(defaultImportWorkDir, token)
}

// StagingWorkDirPath resolves the same work directory from the backend configuration without constructing a storage client
func StagingWorkDirPath(fileBackend, uploadPath, token string) string {
	if fileBackend == TypeFileSystem {
		abs, err := filepath.Abs(uploadPath)
		if err != nil {
			abs = uploadPath
		}
		return filesystemStagingWorkDir(abs, token)
	}
	return filepath.Join(defaultImportWorkDir, token)
}

func filesystemStagingWorkDir(absRoot, token string) string {
	return filepath.Join(filepath.Dir(absRoot), ".pocket-id-import-"+token)
}

// databaseUploadGeneration writes the new generation straight into the new storage table created by the schema swap
// The previous generation lives in that table's shadow until the import commits, so Commit and Rollback need no per-file work
type databaseUploadGeneration struct {
	storage *databaseStorage
	token   string
}

func newDatabaseUploadGeneration(s *databaseStorage, token string) *databaseUploadGeneration {
	return &databaseUploadGeneration{storage: s, token: token}
}

func (g *databaseUploadGeneration) StageFile(ctx context.Context, relPath string, data io.Reader) error {
	return g.storage.Save(ctx, relPath, data)
}

func (g *databaseUploadGeneration) Commit(_ context.Context) error {
	return nil
}

func (g *databaseUploadGeneration) Rollback(_ context.Context) error {
	return nil
}

func (g *databaseUploadGeneration) Cleanup(_ context.Context) error {
	return nil
}

// CarryOverFromShadow copies rows of files the archive does not contain from the previous generation's shadow table into live storage
// Imports merge with existing files rather than replacing the whole bucket, so a file that exists in the old generation but not in the archive must survive
func (g *databaseUploadGeneration) CarryOverFromShadow(ctx context.Context, shadowTable string) error {
	err := g.storage.db.WithContext(ctx).Exec(
		`INSERT INTO storage (path, data, size, mod_time, created_at)
		SELECT old.path, old.data, old.size, old.mod_time, old.created_at
		FROM "`+shadowTable+`" AS old
		WHERE NOT EXISTS (
			SELECT 1 FROM storage AS new WHERE new.path = old.path
		)`,
	).Error
	if err != nil {
		return fmt.Errorf("failed to carry over files absent from the archive: %w", err)
	}
	return nil
}
