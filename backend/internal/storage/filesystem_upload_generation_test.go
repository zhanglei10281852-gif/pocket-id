package storage

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func newFsGenerationFixture(t *testing.T) (*filesystemStorage, *filesystemUploadGeneration, string) {
	t.Helper()

	root := filepath.Join(t.TempDir(), "uploads")
	fs, err := NewFilesystemStorage(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = fs.Close() })

	const token = "tok123"
	gen, err := NewUploadGeneration(fs, token)
	require.NoError(t, err)

	return fs.(*filesystemStorage), gen.(*filesystemUploadGeneration), token
}

func writeLive(t *testing.T, fs FileStorage, path, content string) {
	t.Helper()
	require.NoError(t, fs.Save(context.Background(), path, bytes.NewReader([]byte(content))))
}

func readLive(t *testing.T, fs FileStorage, path string) string {
	t.Helper()
	rc, _, err := fs.Open(context.Background(), path)
	require.NoError(t, err)
	defer rc.Close()
	b, err := io.ReadAll(rc)
	require.NoError(t, err)
	return string(b)
}

func stage(t *testing.T, gen UploadGeneration, path, content string) {
	t.Helper()
	require.NoError(t, gen.StageFile(context.Background(), path, bytes.NewReader([]byte(content))))
}

func TestFilesystemGenerationCommitSwitchesFilesAndBacksUpOld(t *testing.T) {
	fs, gen, _ := newFsGenerationFixture(t)
	writeLive(t, fs, "logo.txt", "old-logo")
	writeLive(t, fs, "untouched.txt", "stay")

	// One file overwritten, one brand-new file
	stage(t, gen, "logo.txt", "new-logo")
	stage(t, gen, "dir/new.txt", "new-file")

	require.NoError(t, gen.Commit(context.Background()))
	require.Equal(t, "new-logo", readLive(t, fs, "logo.txt"))
	require.Equal(t, "new-file", readLive(t, fs, "dir/new.txt"))
	require.Equal(t, "stay", readLive(t, fs, "untouched.txt"))

	// Commit is idempotent
	require.NoError(t, gen.Commit(context.Background()))
	require.Equal(t, "new-logo", readLive(t, fs, "logo.txt"))

	require.NoError(t, gen.Cleanup(context.Background()))

	// Live files remain after cleanup, and the work directory is gone
	require.Equal(t, "new-logo", readLive(t, fs, "logo.txt"))
	_, err := os.Stat(gen.workDir)
	require.True(t, os.IsNotExist(err))
}

func TestFilesystemGenerationRollbackAfterCommitRestoresOld(t *testing.T) {
	fs, gen, _ := newFsGenerationFixture(t)
	writeLive(t, fs, "logo.txt", "old-logo")
	writeLive(t, fs, "keep.txt", "old-keep")

	stage(t, gen, "logo.txt", "new-logo")
	stage(t, gen, "brand-new.txt", "totally-new")
	require.NoError(t, gen.Commit(context.Background()))

	// Rollback after the switch restores old contents and removes files that did not exist before
	require.NoError(t, gen.Rollback(context.Background()))
	require.Equal(t, "old-logo", readLive(t, fs, "logo.txt"))
	require.Equal(t, "old-keep", readLive(t, fs, "keep.txt"))
	_, _, err := fs.Open(context.Background(), "brand-new.txt")
	require.Error(t, err)
	require.True(t, IsNotExist(err))
}

func TestFilesystemGenerationRollbackBeforeCommitLeavesLiveUntouched(t *testing.T) {
	fs, gen, _ := newFsGenerationFixture(t)
	writeLive(t, fs, "logo.txt", "old-logo")

	stage(t, gen, "logo.txt", "new-logo")
	stage(t, gen, "new.txt", "brand-new")

	// Nothing switched yet: rolling back must leave the live generation exactly as it was
	require.NoError(t, gen.Rollback(context.Background()))
	require.Equal(t, "old-logo", readLive(t, fs, "logo.txt"))
	_, _, err := fs.Open(context.Background(), "new.txt")
	require.True(t, IsNotExist(err))
}

func TestFilesystemGenerationStageIsInvisibleToStorageWalk(t *testing.T) {
	fs, gen, _ := newFsGenerationFixture(t)
	writeLive(t, fs, "logo.txt", "old-logo")
	stage(t, gen, "staged.txt", "x")

	var seen []string
	err := fs.Walk(context.Background(), "/", func(info ObjectInfo) error {
		seen = append(seen, info.Path)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, []string{"logo.txt"}, seen, "staged files live outside the storage root and must never be walked as live uploads")
}

func TestIsReservedImportPath(t *testing.T) {
	require.True(t, IsReservedImportPath(".import-staging/tok/x"))
	require.True(t, IsReservedImportPath(".import-backup/tok/x"))
	require.True(t, IsReservedImportPath("/.import-staging/x"))
	require.False(t, IsReservedImportPath("logo.txt"))
	require.False(t, IsReservedImportPath(".import-stagingish/x"))
}
