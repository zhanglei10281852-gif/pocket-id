package service

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

// stubActorsBackupProvider stands in for the Francis provider and records what the export and import did with it
type stubActorsBackupProvider struct {
	backupData  []byte
	backupErr   error
	backupCalls int

	restored        []byte
	restoredPayload [][]byte
	restoreCalls    int
	restoreErr      error
}

func (s *stubActorsBackupProvider) Backup(_ context.Context, w io.Writer) error {
	s.backupCalls++
	if s.backupErr != nil {
		return s.backupErr
	}
	_, err := w.Write(s.backupData)
	return err
}

func (s *stubActorsBackupProvider) Restore(_ context.Context, r io.Reader) error {
	s.restoreCalls++
	if s.restoreErr != nil {
		return s.restoreErr
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.restored = data
	s.restoredPayload = append(s.restoredPayload, data)
	return nil
}

func TestExportActorsBackup(t *testing.T) {
	t.Run("adds the actor host's data to the archive", func(t *testing.T) {
		actors := &stubActorsBackupProvider{backupData: []byte("francis-backup-payload")}

		files := writeActorsBackupZip(t, NewExportService(nil, nil, actors))

		require.Equal(t, map[string][]byte{ActorsBackupFileName: actors.backupData}, files)
	})

	t.Run("adds nothing without a provider", func(t *testing.T) {
		files := writeActorsBackupZip(t, NewExportService(nil, nil, nil))

		require.Empty(t, files)
	})

	t.Run("surfaces backup errors", func(t *testing.T) {
		actors := &stubActorsBackupProvider{backupErr: errors.New("provider is unhappy")}

		zw := zip.NewWriter(&bytes.Buffer{})
		err := NewExportService(nil, nil, actors).addActorsBackupToZip(t.Context(), zw)

		require.ErrorContains(t, err, "provider is unhappy")
	})
}

func TestImportActorsBackup(t *testing.T) {
	// Archives created before Pocket ID exported the actor host's data have no francis.bin entry, so the orchestration leaves the existing actor data untouched
	t.Run("archives without actor data are detected", func(t *testing.T) {
		files := readZip(t, buildZip(t, map[string][]byte{"database.json": []byte("{}")}))

		require.Nil(t, zipFileNamed(files, ActorsBackupFileName))
	})

	t.Run("restores the actor host's data from the archive", func(t *testing.T) {
		actors := &stubActorsBackupProvider{}
		files := readZip(t, buildZip(t, map[string][]byte{
			"database.json":      []byte("{}"),
			ActorsBackupFileName: []byte("francis-backup-payload"),
		}))

		entry := zipFileNamed(files, ActorsBackupFileName)
		require.NotNil(t, entry)
		err := restoreActorsEntry(t.Context(), actors, entry)

		require.NoError(t, err)
		require.Equal(t, 1, actors.restoreCalls)
		require.Equal(t, []byte("francis-backup-payload"), actors.restored)
	})

	t.Run("surfaces restore errors", func(t *testing.T) {
		actors := &stubActorsBackupProvider{restoreErr: errors.New("a host is still connected")}
		files := readZip(t, buildZip(t, map[string][]byte{ActorsBackupFileName: []byte("francis-backup-payload")}))

		err := restoreActorsEntry(t.Context(), actors, zipFileNamed(files, ActorsBackupFileName))

		require.ErrorContains(t, err, "a host is still connected")
	})
}

// writeActorsBackupZip runs the export's actor-host backup step into an archive and returns its contents
func writeActorsBackupZip(t *testing.T, s *ExportService) map[string][]byte {
	t.Helper()

	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)
	require.NoError(t, s.addActorsBackupToZip(t.Context(), zw))
	require.NoError(t, zw.Close())

	res := make(map[string][]byte)
	for _, f := range readZip(t, buf.Bytes()) {
		rc, err := f.Open()
		require.NoError(t, err)
		data, err := io.ReadAll(rc)
		rc.Close()
		require.NoError(t, err)
		res[f.Name] = data
	}
	return res
}

func buildZip(t *testing.T, files map[string][]byte) []byte {
	t.Helper()

	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)
	for name, data := range files {
		w, err := zw.Create(name)
		require.NoError(t, err)
		_, err = w.Write(data)
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())

	return buf.Bytes()
}

func readZip(t *testing.T, data []byte) []*zip.File {
	t.Helper()

	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)

	return zr.File
}
