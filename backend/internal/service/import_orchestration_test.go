package service

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"uuid"

	"github.com/pocket-id/pocket-id/backend/internal/storage"
	"github.com/pocket-id/pocket-id/backend/internal/utils"
)

// importTestEnv is a fully migrated instance with database rows, uploaded files, and actor state standing in for the previous generation
type importTestEnv struct {
	t         *testing.T
	dbPath    string
	uploadDir string
	token     string
	workDir   string

	db        *gorm.DB
	fileStore storage.FileStorage
	actors    *stubActorsBackupProvider
}

type uploadFile struct {
	path    string
	content []byte
}

// newImportTestEnv boots the "old instance": migrated database, one kv row, one uploaded file, and current actor state
func newImportTestEnv(t *testing.T) *importTestEnv {
	t.Helper()

	uploadDir := filepath.Join(t.TempDir(), "uploads")
	dbPath := filepath.Join(t.TempDir(), "pocket-id.db")
	token := "abcdef012345"

	db := openImportTestDB(t, dbPath, nil)
	require.NoError(t, utils.MigrateDatabase(t.Context(), mustSQLDB(t, db)))
	require.NoError(t, EnsureImportJournalTable(t.Context(), db))
	require.NoError(t, db.Exec(`INSERT INTO kv ("key", "value") VALUES ('old-key', 'old-value')`).Error)

	fs, err := storage.NewFilesystemStorage(uploadDir)
	require.NoError(t, err)
	require.NoError(t, fs.Save(t.Context(), "logo.txt", bytes.NewReader([]byte("old-logo"))))

	actors := &stubActorsBackupProvider{backupData: []byte("old-actors")}

	return &importTestEnv{
		t:         t,
		dbPath:    dbPath,
		uploadDir: uploadDir,
		token:     token,
		workDir:   storage.StagingWorkDir(fs, token),
		db:        db,
		fileStore: fs,
		actors:    actors,
	}
}

func (e *importTestEnv) close() {
	closeImportTestDB(e.db)
	_ = e.fileStore.Close()
}

// reopen simulates a new process opening the same database and storage
func (e *importTestEnv) reopen() *importTestEnv {
	e.close()

	db := openImportTestDB(e.t, e.dbPath, nil)
	fs, err := storage.NewFilesystemStorage(e.uploadDir)
	require.NoError(e.t, err)

	return &importTestEnv{
		t:         e.t,
		dbPath:    e.dbPath,
		uploadDir: e.uploadDir,
		token:     e.token,
		workDir:   e.workDir,
		db:        db,
		fileStore: fs,
		actors:    e.actors,
	}
}

// newImportDatabaseExport is the database payload of test archives
func newImportDatabaseExport() DatabaseExport {
	return DatabaseExport{
		Provider: "sqlite",
		Version:  importResetTargetVersion,
		Tables: map[string][]map[string]any{
			"kv": {
				{"key": "new-key", "value": "new-value"},
			},
		},
	}
}

// buildImportZip assembles an archive from the database payload, optional actor data, and ordered upload files
func buildImportZip(t *testing.T, dbData DatabaseExport, actorsData []byte, uploads []uploadFile) *zip.Reader {
	t.Helper()

	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)

	writeEntry := func(name string, data []byte) {
		w, err := zw.Create(name)
		require.NoError(t, err)
		_, err = w.Write(data)
		require.NoError(t, err)
	}

	dbJSON, err := json.Marshal(dbData)
	require.NoError(t, err)
	writeEntry("database.json", dbJSON)
	if actorsData != nil {
		writeEntry(ActorsBackupFileName, actorsData)
	}
	for _, f := range uploads {
		writeEntry("uploads/"+f.path, f.content)
	}
	require.NoError(t, zw.Close())

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	require.NoError(t, err)
	return zr
}

func (e *importTestEnv) kvValue(key string) (string, bool) {
	var value string
	result := e.db.Raw(`SELECT value FROM kv WHERE key = ?`, key).Scan(&value)
	if result.Error != nil || result.RowsAffected == 0 {
		return "", false
	}
	return value, true
}

func (e *importTestEnv) fileContent(path string) ([]byte, bool) {
	rc, _, err := e.fileStore.Open(e.t.Context(), path)
	if err != nil {
		return nil, false
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	require.NoError(e.t, err)
	return b, true
}

func (e *importTestEnv) pendingJournals() int64 {
	var n int64
	require.NoError(e.t, e.db.Raw(`SELECT count(*) FROM `+ImportJournalTableName).Scan(&n).Error)
	return n
}

func (e *importTestEnv) shadowsExist() bool {
	has, err := SchemaHasShadows(e.t.Context(), e.db, e.token)
	require.NoError(e.t, err)
	return has
}

func (e *importTestEnv) runImport(zr *zip.Reader, actors ActorsBackupProvider, ownerID string, faults *importFaults) error {
	return NewImportServiceWithFaults(e.db, e.fileStore, actors, faults).Import(e.t.Context(), zr, ImportOptions{
		GenerationID: uuid.NewV4().String(),
		Token:        e.token,
		OwnerID:      ownerID,
		WorkDir:      e.workDir,
	})
}

func NewImportServiceWithFaults(db *gorm.DB, fs storage.FileStorage, actors ActorsBackupProvider, faults *importFaults) *ImportService {
	s := NewImportService(db, fs, actors)
	s.faults = faults
	return s
}

func (e *importTestEnv) latestGeneration(t *testing.T) string {
	t.Helper()
	j, err := GetLatestPendingImportJournal(t.Context(), e.db)
	require.NoError(t, err)
	require.NotNil(t, j)
	return j.GenerationID
}

// TestImportHappyPath verifies a clean import commits all three components to the same generation and leaves no journal or shadow behind
func TestImportHappyPath(t *testing.T) {
	env := newImportTestEnv(t)
	defer env.close()

	zr := buildImportZip(t, newImportDatabaseExport(), []byte("new-actors"), []uploadFile{
		{path: "logo.txt", content: []byte("new-logo")},
		{path: "new.txt", content: []byte("brand-new")},
	})

	require.NoError(t, env.runImport(zr, env.actors, "owner-A", nil))

	// New database generation is live
	value, ok := env.kvValue("new-key")
	require.True(t, ok)
	require.Equal(t, "new-value", value)
	_, oldStillThere := env.kvValue("old-key")
	require.False(t, oldStillThere)

	// New uploads are live
	logo, ok := env.fileContent("logo.txt")
	require.True(t, ok)
	require.Equal(t, "new-logo", string(logo))
	newFile, ok := env.fileContent("new.txt")
	require.True(t, ok)
	require.Equal(t, "brand-new", string(newFile))

	// Actor state was backed up first and replaced once
	require.Equal(t, 1, env.actors.backupCalls)
	require.Equal(t, [][]byte{[]byte("new-actors")}, env.actors.restoredPayload)

	// Nothing pending, no shadows, and the old generation can be migrated like a normal restart
	require.Zero(t, env.pendingJournals())
	require.False(t, env.shadowsExist())
	require.NoError(t, utils.MigrateDatabase(t.Context(), mustSQLDB(t, env.db)))
}

// TestImportOldArchiveWithoutActorsKeepsExistingActors keeps the compatibility semantics of pre-actor archives
func TestImportOldArchiveWithoutActorsKeepsExistingActors(t *testing.T) {
	env := newImportTestEnv(t)
	defer env.close()

	// No francis.bin in the archive even though an embedded provider exists
	zr := buildImportZip(t, newImportDatabaseExport(), nil, nil)

	require.NoError(t, env.runImport(zr, env.actors, "owner-A", nil))

	require.Zero(t, env.actors.backupCalls, "existing actor state must not be touched")
	require.Zero(t, env.actors.restoreCalls)
	require.Zero(t, env.pendingJournals())

	// Database still imported normally
	_, ok := env.kvValue("new-key")
	require.True(t, ok)
}

// TestImportActorArchiveWithoutRuntimeIsRefused enforces that a standalone Francis deployment never silently half-restores an archive carrying actor data
func TestImportActorArchiveWithoutRuntimeIsRefused(t *testing.T) {
	env := newImportTestEnv(t)
	defer env.close()

	zr := buildImportZip(t, newImportDatabaseExport(), []byte("new-actors"), nil)

	err := env.runImport(zr, nil, "owner-A", nil)
	require.ErrorIs(t, err, ErrActorsDataWithoutRuntime)

	// Nothing changed
	value, ok := env.kvValue("old-key")
	require.True(t, ok)
	require.Equal(t, "old-value", value)
	require.Zero(t, env.pendingJournals())
}

// TestImportFailureAfterActorsRollsEverythingBack covers the "actor state replaced, then the database migration fails" case
func TestImportFailureAfterActorsRollsEverythingBack(t *testing.T) {
	env := newImportTestEnv(t)
	defer env.close()

	zr := buildImportZip(t, newImportDatabaseExport(), []byte("new-actors"), nil)

	err := env.runImport(zr, env.actors, "owner-A", &importFaults{
		failAtPhase: ImportPhaseSchemaShadowed,
		failErr:     errors.New("simulated migration failure"),
	})
	require.ErrorContains(t, err, "simulated migration failure")

	// Database and uploads are the old generation, and the actor state was restored from the journal backup
	value, ok := env.kvValue("old-key")
	require.True(t, ok)
	require.Equal(t, "old-value", value)
	logo, ok := env.fileContent("logo.txt")
	require.True(t, ok)
	require.Equal(t, "old-logo", string(logo))
	require.Equal(t, [][]byte{[]byte("new-actors"), []byte("old-actors")}, env.actors.restoredPayload)
	require.Zero(t, env.pendingJournals())
	require.False(t, env.shadowsExist())
}

// TestImportFailureAfterDatabaseRestoresUploadsAndActors covers the "database committed, file storage then fails" case
func TestImportFailureAfterDatabaseRestoresUploadsAndActors(t *testing.T) {
	env := newImportTestEnv(t)
	defer env.close()

	zr := buildImportZip(t, newImportDatabaseExport(), []byte("new-actors"), []uploadFile{
		{path: "a.txt", content: []byte("a-new")},
		{path: "blocked.txt", content: []byte("blocked")},
	})

	// Make staging blocked.txt fail after a.txt was already staged
	blocked := filepath.Join(env.workDir, "new", "blocked.txt")
	require.NoError(t, os.MkdirAll(blocked, 0700))

	err := env.runImport(zr, env.actors, "owner-A", nil)
	require.Error(t, err)

	// All three components rolled back even though some uploads were already staged
	value, ok := env.kvValue("old-key")
	require.True(t, ok)
	require.Equal(t, "old-value", value)
	logo, ok := env.fileContent("logo.txt")
	require.True(t, ok)
	require.Equal(t, "old-logo", string(logo))
	_, exists := env.fileContent("a.txt")
	require.False(t, exists)
	require.Equal(t, [][]byte{[]byte("new-actors"), []byte("old-actors")}, env.actors.restoredPayload)
	require.Zero(t, env.pendingJournals())
}

// TestImportCrashIsConvergedByNextProcess simulates the process dying after the uploads were staged, then a fresh process recovering before serving
func TestImportCrashIsConvergedByNextProcess(t *testing.T) {
	env := newImportTestEnv(t)

	zr := buildImportZip(t, newImportDatabaseExport(), []byte("new-actors"), []uploadFile{
		{path: "logo.txt", content: []byte("new-logo")},
	})

	err := env.runImport(zr, env.actors, "owner-A", &importFaults{crashAtPhase: ImportPhaseUploadsPrepared})
	require.ErrorIs(t, err, errSimulatedCrash)

	// The mixed generation is left pending, with the new schema live, staged uploads not switched, and new actor state live
	require.Equal(t, int64(1), env.pendingJournals())
	_, newThere := env.kvValue("new-key")
	require.True(t, newThere)
	logo, ok := env.fileContent("logo.txt")
	require.True(t, ok)
	require.Equal(t, "old-logo", string(logo), "staged uploads must not be live before commit")

	generation := env.latestGeneration(t)

	// A stale execution of the old owner must not be able to keep committing after another process takes over
	require.NoError(t, StealImportOwnership(t.Context(), env.db, generation, "recovery-owner", time.Now().Add(ownerLeaseTTL)))
	require.ErrorIs(t, AdvanceImportPhase(t.Context(), env.db, generation, "owner-A", ImportPhaseCommitting), ErrImportOwnershipLost)

	// Simulate the server restarting: a new process converges the pending generation before serving
	env2 := env.reopen()
	defer env2.close()
	recovered, err := NewImportService(env2.db, env2.fileStore, env2.actors).RecoverPending(t.Context(), env2.actors)
	require.NoError(t, err)
	require.Equal(t, 1, recovered)

	// The old complete generation is back across all three components
	value, ok := env2.kvValue("old-key")
	require.True(t, ok)
	require.Equal(t, "old-value", value)
	logo, ok = env2.fileContent("logo.txt")
	require.True(t, ok)
	require.Equal(t, "old-logo", string(logo))
	require.Equal(t, [][]byte{[]byte("new-actors"), []byte("old-actors")}, env2.actors.restoredPayload)
	require.Zero(t, env2.pendingJournals())
	require.False(t, env2.shadowsExist())
	require.NoError(t, foreignKeyCheck(t, env2.db))
	require.NoError(t, utils.MigrateDatabase(t.Context(), mustSQLDB(t, env2.db)))
}

// TestImportCrashAfterShadowIsConverged covers a crash between the shadow rename and the database rebuild
func TestImportCrashAfterShadowIsConverged(t *testing.T) {
	env := newImportTestEnv(t)

	zr := buildImportZip(t, newImportDatabaseExport(), []byte("new-actors"), nil)

	err := env.runImport(zr, env.actors, "owner-A", &importFaults{crashAtPhase: ImportPhaseSchemaShadowed})
	require.ErrorIs(t, err, errSimulatedCrash)
	require.Equal(t, int64(1), env.pendingJournals())

	env2 := env.reopen()
	defer env2.close()
	recovered, err := NewImportService(env2.db, env2.fileStore, env2.actors).RecoverPending(t.Context(), env2.actors)
	require.NoError(t, err)
	require.Equal(t, 1, recovered)

	value, ok := env2.kvValue("old-key")
	require.True(t, ok)
	require.Equal(t, "old-value", value)
	require.Equal(t, [][]byte{[]byte("new-actors"), []byte("old-actors")}, env2.actors.restoredPayload)
	require.False(t, env2.shadowsExist())
}

// TestImportResumeCompletesInterruptedGeneration verifies a second execution can continue a crashed generation from its staged phase
func TestImportResumeCompletesInterruptedGeneration(t *testing.T) {
	env := newImportTestEnv(t)

	upload := []uploadFile{{path: "logo.txt", content: []byte("new-logo")}}
	zr1 := buildImportZip(t, newImportDatabaseExport(), []byte("new-actors"), upload)
	err := env.runImport(zr1, env.actors, "owner-A", &importFaults{crashAtPhase: ImportPhaseSchemaShadowed})
	require.ErrorIs(t, err, errSimulatedCrash)
	require.Equal(t, int64(1), env.pendingJournals())

	// A second process resumes with the same archive and completes the new generation
	env2 := env.reopen()
	defer env2.close()
	zr2 := buildImportZip(t, newImportDatabaseExport(), []byte("new-actors"), upload)
	svc := NewImportService(env2.db, env2.fileStore, env2.actors)
	require.NoError(t, svc.Import(t.Context(), zr2, ImportOptions{
		Resume:  true,
		Token:   env2.token,
		WorkDir: env2.workDir,
	}))

	value, ok := env2.kvValue("new-key")
	require.True(t, ok)
	require.Equal(t, "new-value", value)
	logo, ok := env2.fileContent("logo.txt")
	require.True(t, ok)
	require.Equal(t, "new-logo", string(logo))
	// The resumed actor generation was not restored a second time, and the old one was never put back
	require.Equal(t, [][]byte{[]byte("new-actors")}, env2.actors.restoredPayload)
	require.Zero(t, env2.pendingJournals())
	require.False(t, env2.shadowsExist())
}

// TestImportCrashDuringCommittingConvergesForward verifies a crash in the final phase is completed forward rather than rolled back
func TestImportCrashDuringCommittingConvergesForward(t *testing.T) {
	env := newImportTestEnv(t)

	zr := buildImportZip(t, newImportDatabaseExport(), []byte("new-actors"), []uploadFile{
		{path: "logo.txt", content: []byte("new-logo")},
	})

	err := env.runImport(zr, env.actors, "owner-A", &importFaults{crashAtPhase: ImportPhaseUploadsCommitted})
	require.ErrorIs(t, err, errSimulatedCrash)

	// Simulate the process dying in the committing phase: all components are the new generation and only cleanup remains
	generation := env.latestGeneration(t)
	require.NoError(t, env.db.Exec(`UPDATE `+ImportJournalTableName+` SET phase = ? WHERE generation_id = ?`, ImportPhaseCommitting, generation).Error)

	env2 := env.reopen()
	defer env2.close()
	n, err := NewImportService(env2.db, env2.fileStore, env2.actors).RecoverPending(t.Context(), env2.actors)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	// The new generation stays live and the old one is gone
	value, ok := env2.kvValue("new-key")
	require.True(t, ok)
	require.Equal(t, "new-value", value)
	_, oldThere := env2.kvValue("old-key")
	require.False(t, oldThere)
	logo, ok := env2.fileContent("logo.txt")
	require.True(t, ok)
	require.Equal(t, "new-logo", string(logo))
	require.Equal(t, [][]byte{[]byte("new-actors")}, env2.actors.restoredPayload, "actors must not be rolled back during a forward convergence")
	require.Zero(t, env2.pendingJournals())
	require.False(t, env2.shadowsExist())
}

// TestDatabaseStorageCarryOver verifies files absent from the archive survive when uploads live in the database storage
func TestDatabaseStorageCarryOver(t *testing.T) {
	db := newExportTestDB(t)

	store, err := storage.NewDatabaseStorage(db)
	require.NoError(t, err)
	require.NoError(t, store.Save(t.Context(), "keep.png", bytes.NewReader([]byte("kept"))))

	const token = "111122223333"
	zr := buildImportZip(t, newImportDatabaseExport(), nil, []uploadFile{
		{path: "new.png", content: []byte("new")},
	})
	svc := NewImportService(db, store, nil)
	require.NoError(t, svc.Import(t.Context(), zr, ImportOptions{
		GenerationID: uuid.NewV4().String(),
		Token:        token,
		WorkDir:      t.TempDir(),
	}))

	keep, _, err := store.Open(t.Context(), "keep.png")
	require.NoError(t, err)
	b, _ := io.ReadAll(keep)
	keep.Close()
	require.Equal(t, "kept", string(b))

	newFile, _, err := store.Open(t.Context(), "new.png")
	require.NoError(t, err)
	b, _ = io.ReadAll(newFile)
	newFile.Close()
	require.Equal(t, "new", string(b))
}

// TestImportJournalExcludedFromExport verifies the journal never leaks into an export
func TestImportJournalExcludedFromExport(t *testing.T) {
	db := newExportTestDB(t)
	require.NoError(t, CreateImportJournal(t.Context(), db, sampleJournal("owner-A")))

	export, err := NewExportService(db, nil, nil).extractDatabase(t.Context())
	require.NoError(t, err)
	require.NotContains(t, export.Tables, ImportJournalTableName)
}
