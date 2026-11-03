package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testDBPath(t *testing.T) string {
	t.Helper()
	return t.TempDir() + "/pocket-id.db"
}

func sampleJournal(owner string) *ImportJournal {
	now := time.Now()
	return &ImportJournal{
		GenerationID:   "gen-1",
		Token:          "abc123",
		OwnerID:        owner,
		OwnerDeadline:  now.Add(ownerLeaseTTL),
		Phase:          ImportPhasePrepared,
		TargetVersion:  20260707170000,
		ActorsIncluded: true,
		OldActors:      []byte("old-actor-generation"),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

func TestImportJournalCreateAndLoad(t *testing.T) {
	db := openImportTestDB(t, testDBPath(t), nil)
	t.Cleanup(func() { closeImportTestDB(db) })
	require.NoError(t, EnsureImportJournalTable(t.Context(), db))

	require.NoError(t, CreateImportJournal(t.Context(), db, sampleJournal("owner-A")))

	loaded, err := GetImportJournal(t.Context(), db, "gen-1")
	require.NoError(t, err)
	require.NotNil(t, loaded)
	require.Equal(t, "owner-A", loaded.OwnerID)
	require.Equal(t, ImportPhasePrepared, loaded.Phase)
	require.Equal(t, []byte("old-actor-generation"), loaded.OldActors)
	require.True(t, loaded.ActorsIncluded)

	missing, err := GetImportJournal(t.Context(), db, "nope")
	require.NoError(t, err)
	require.Nil(t, missing)
}

func TestImportJournalEnsureTableIsIdempotent(t *testing.T) {
	db := openImportTestDB(t, testDBPath(t), nil)
	t.Cleanup(func() { closeImportTestDB(db) })

	require.NoError(t, EnsureImportJournalTable(t.Context(), db))
	require.NoError(t, EnsureImportJournalTable(t.Context(), db))
}

func TestImportJournalPhaseAdvanceIsFencedByOwner(t *testing.T) {
	ctx := t.Context()
	db := openImportTestDB(t, testDBPath(t), nil)
	t.Cleanup(func() { closeImportTestDB(db) })
	require.NoError(t, EnsureImportJournalTable(ctx, db))
	require.NoError(t, CreateImportJournal(ctx, db, sampleJournal("owner-A")))

	// The current owner advances normally
	require.NoError(t, AdvanceImportPhase(ctx, db, "gen-1", "owner-A", ImportPhaseActorsRestored))

	// A stale owner that lost the generation cannot advance it anymore
	err := AdvanceImportPhase(ctx, db, "gen-1", "owner-A-stale", ImportPhaseSchemaShadowed)
	require.ErrorIs(t, err, ErrImportOwnershipLost)

	// The failed advance must not have moved the phase
	loaded, err := GetImportJournal(ctx, db, "gen-1")
	require.NoError(t, err)
	require.Equal(t, ImportPhaseActorsRestored, loaded.Phase)

	// Whoever stole the generation can advance it
	require.NoError(t, StealImportOwnership(ctx, db, "gen-1", "owner-B", time.Now().Add(ownerLeaseTTL)))
	require.NoError(t, AdvanceImportPhase(ctx, db, "gen-1", "owner-B", ImportPhaseSchemaShadowed))
}

func TestImportJournalRenewalAndSteal(t *testing.T) {
	ctx := t.Context()
	db := openImportTestDB(t, testDBPath(t), nil)
	t.Cleanup(func() { closeImportTestDB(db) })
	require.NoError(t, EnsureImportJournalTable(ctx, db))
	require.NoError(t, CreateImportJournal(ctx, db, sampleJournal("owner-A")))

	require.NoError(t, RenewImportOwnership(ctx, db, "gen-1", "owner-A", time.Now().Add(time.Minute)))
	require.ErrorIs(t, RenewImportOwnership(ctx, db, "gen-1", "intruder", time.Now().Add(time.Minute)), ErrImportOwnershipLost)

	require.NoError(t, StealImportOwnership(ctx, db, "gen-1", "owner-B", time.Now().Add(ownerLeaseTTL)))
	require.ErrorIs(t, RenewImportOwnership(ctx, db, "gen-1", "owner-A", time.Now().Add(time.Minute)), ErrImportOwnershipLost)
	require.NoError(t, RenewImportOwnership(ctx, db, "gen-1", "owner-B", time.Now().Add(time.Minute)))

	// Recording an error is intentionally possible without ownership, for diagnostics
	require.NoError(t, RecordImportError(ctx, db, "gen-1", context.Canceled))
}

func TestImportJournalPendingAndDelete(t *testing.T) {
	ctx := t.Context()
	db := openImportTestDB(t, testDBPath(t), nil)
	t.Cleanup(func() { closeImportTestDB(db) })
	require.NoError(t, EnsureImportJournalTable(ctx, db))

	pending, err := GetLatestPendingImportJournal(ctx, db)
	require.NoError(t, err)
	require.Nil(t, pending)

	j := sampleJournal("owner-A")
	require.NoError(t, CreateImportJournal(ctx, db, j))
	pending, err = GetLatestPendingImportJournal(ctx, db)
	require.NoError(t, err)
	require.NotNil(t, pending)
	require.Equal(t, "gen-1", pending.GenerationID)

	require.NoError(t, DeleteImportJournal(ctx, db, "gen-1"))
	pending, err = GetLatestPendingImportJournal(ctx, db)
	require.NoError(t, err)
	require.Nil(t, pending)
}

func TestImportJournalUpdateFieldsFenced(t *testing.T) {
	ctx := t.Context()
	db := openImportTestDB(t, testDBPath(t), nil)
	t.Cleanup(func() { closeImportTestDB(db) })
	require.NoError(t, EnsureImportJournalTable(ctx, db))
	require.NoError(t, CreateImportJournal(ctx, db, sampleJournal("owner-A")))

	require.NoError(t, UpdateImportJournal(ctx, db, "gen-1", "owner-A", map[string]any{
		"work_dir": "/tmp/work",
	}))
	require.ErrorIs(t, UpdateImportJournal(ctx, db, "gen-1", "other", map[string]any{
		"work_dir": "/tmp/other",
	}), ErrImportOwnershipLost)

	loaded, err := GetImportJournal(ctx, db, "gen-1")
	require.NoError(t, err)
	require.Equal(t, "/tmp/work", loaded.WorkDir)
}
