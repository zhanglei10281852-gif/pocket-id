package jwk

import (
	"testing"
	"time"

	jwxjwk "github.com/lestrrat-go/jwx/v4/jwk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pocket-id/pocket-id/backend/internal/common"
	"github.com/pocket-id/pocket-id/backend/internal/instanceid"
	"github.com/pocket-id/pocket-id/backend/internal/model"
	testutils "github.com/pocket-id/pocket-id/backend/internal/utils/testing"
)

func newSetTestEnv(t *testing.T) (*gorm.DB, *common.EnvConfigSchema, string) {
	t.Helper()

	envConfig := &common.EnvConfigSchema{EncryptionKey: []byte("test-encryption-key-characters-long")}
	db := testutils.NewDatabaseForTest(t)
	instanceID, err := instanceid.Load(t.Context(), db)
	require.NoError(t, err)
	return db, envConfig, instanceID
}

func TestEnsureSigningKeySetGeneratesDefaultKey(t *testing.T) {
	db, envConfig, instanceID := newSetTestEnv(t)

	loaded, err := EnsureSigningKeySet(t.Context(), db, envConfig, instanceID, time.Now())
	require.NoError(t, err)
	require.Len(t, loaded.Set.Generations, 1)

	key, gen := loaded.ActiveKey()
	require.NotNil(t, key)
	require.NotNil(t, gen)
	assert.True(t, gen.Active(), "The first generation must be active")
	assert.Nil(t, gen.RetainUntil)

	alg, _ := key.Algorithm()
	assert.Equal(t, "RS256", alg.String(), "The default key must be RS256")

	// A second startup must load the same key instead of generating another
	again, err := EnsureSigningKeySet(t.Context(), db, envConfig, instanceID, time.Now())
	require.NoError(t, err)
	againKey, _ := again.ActiveKey()
	firstKid, _ := key.KeyID()
	againKid, _ := againKey.KeyID()
	assert.Equal(t, firstKid, againKid)
}

func TestEnsureSigningKeySetAdoptsLegacyKey(t *testing.T) {
	db, envConfig, instanceID := newSetTestEnv(t)

	// Simulate a pre-upgrade deployment with only the legacy single-key row
	legacyKey, err := GenerateKey("ES256", "")
	require.NoError(t, err)
	legacyProvider, err := GetKeyProvider(db, envConfig, instanceID)
	require.NoError(t, err)
	require.NoError(t, legacyProvider.SaveKey(t.Context(), legacyKey))
	legacyKid, _ := legacyKey.KeyID()

	loaded, err := EnsureSigningKeySet(t.Context(), db, envConfig, instanceID, time.Now())
	require.NoError(t, err)
	key, gen := loaded.ActiveKey()
	require.NotNil(t, key)
	require.NotNil(t, gen)
	assert.Equal(t, legacyKid, gen.Kid, "The legacy key must be adopted as the active generation with the same kid")

	// Adoption removes the legacy row in the same transaction
	stillThere, err := legacyProvider.LoadKey(t.Context())
	require.NoError(t, err)
	assert.Nil(t, stillThere, "The legacy single-key row must be removed after adoption")

	// Restarting keeps the adopted generation
	again, err := EnsureSigningKeySet(t.Context(), db, envConfig, instanceID, time.Now())
	require.NoError(t, err)
	againKey, _ := again.ActiveKey()
	againKid, _ := againKey.KeyID()
	assert.Equal(t, legacyKid, againKid)
}

func TestEnsureSigningKeySetAnotherWritersSetWins(t *testing.T) {
	db, envConfig, instanceID := newSetTestEnv(t)

	// Another replica initializes first and owns the set row
	first, err := EnsureSigningKeySet(t.Context(), db, envConfig, instanceID, time.Now())
	require.NoError(t, err)
	firstGen, _ := first.Set.Active()

	// A late writer attempting a first-run insert must be told to reload instead of overwriting
	store, err := GetSigningKeySetStore(db, envConfig, instanceID)
	require.NoError(t, err)
	competingKey, err := GenerateKey("RS256", "")
	require.NoError(t, err)
	competingSet, err := newSetFromKey(competingKey, store, time.Now())
	require.NoError(t, err)
	inserted, err := store.CreateSet(t.Context(), competingSet)
	require.NoError(t, err)
	assert.False(t, inserted, "Inserting a second initial set must not overwrite the existing one")

	loaded, err := LoadSigningKeySet(t.Context(), db, envConfig, instanceID)
	require.NoError(t, err)
	_, activeGen := loaded.ActiveKey()
	assert.Equal(t, firstGen.Kid, activeGen.Kid, "The winning replica's key must stay active")
}

func TestRotateSigningKeysSwitchesActiveGenerationAtomically(t *testing.T) {
	db, envConfig, instanceID := newSetTestEnv(t)

	_, err := EnsureSigningKeySet(t.Context(), db, envConfig, instanceID, time.Now())
	require.NoError(t, err)
	before, err := LoadSigningKeySet(t.Context(), db, envConfig, instanceID)
	require.NoError(t, err)
	_, oldGen := before.ActiveKey()

	const retention = 24 * time.Hour
	now := time.Now()
	result, err := RotateSigningKeys(t.Context(), db, envConfig, instanceID, "ES256", "", retention, now)
	require.NoError(t, err)
	assert.NotEmpty(t, result.NewActiveKid)
	assert.Equal(t, oldGen.Kid, result.RetiredKid)
	assert.NotEqual(t, result.NewActiveKid, result.RetiredKid)

	after, err := LoadSigningKeySet(t.Context(), db, envConfig, instanceID)
	require.NoError(t, err)
	require.Len(t, after.Set.Generations, 2)

	_, activeGen := after.ActiveKey()
	assert.Equal(t, result.NewActiveKid, activeGen.Kid)
	assert.Equal(t, "ES256", activeGen.Alg)
	assert.True(t, activeGen.Active())

	var retired *SigningKeyGeneration
	for i := range after.Set.Generations {
		if after.Set.Generations[i].Kid == oldGen.Kid {
			retired = &after.Set.Generations[i]
		}
	}
	require.NotNil(t, retired)
	require.NotNil(t, retired.RetiredAt)
	require.NotNil(t, retired.RetainUntil)
	assert.WithinDuration(t, now.Add(retention), *retired.RetainUntil, time.Second)

	// The old private key must still decrypt, since tokens signed with it are still valid
	oldKey := after.Keys[oldGen.Kid]
	require.NotNil(t, oldKey)

	// Exactly one active generation is an invariant of the persisted document
	activeCount := 0
	for _, gen := range after.Set.Generations {
		if gen.Active() {
			activeCount++
		}
	}
	assert.Equal(t, 1, activeCount)
}

func TestRotateSigningKeysBootstrapsWithoutKey(t *testing.T) {
	db, envConfig, instanceID := newSetTestEnv(t)

	// Rotation on a pristine deployment creates the requested key as the first active generation
	result, err := RotateSigningKeys(t.Context(), db, envConfig, instanceID, "ES384", "", 0, time.Now())
	require.NoError(t, err)
	assert.NotEmpty(t, result.NewActiveKid)
	assert.Empty(t, result.RetiredKid, "There is no previous generation to retire")

	loaded, err := LoadSigningKeySet(t.Context(), db, envConfig, instanceID)
	require.NoError(t, err)
	require.Len(t, loaded.Set.Generations, 1)
	_, gen := loaded.ActiveKey()
	assert.Equal(t, "ES384", gen.Alg)
	assert.True(t, gen.Active())
}

func TestRotateSigningKeysAdoptsAndRetiresLegacyKey(t *testing.T) {
	db, envConfig, instanceID := newSetTestEnv(t)

	legacyKey, err := GenerateKey("RS256", "")
	require.NoError(t, err)
	legacyProvider, err := GetKeyProvider(db, envConfig, instanceID)
	require.NoError(t, err)
	require.NoError(t, legacyProvider.SaveKey(t.Context(), legacyKey))
	legacyKid, _ := legacyKey.KeyID()

	result, err := RotateSigningKeys(t.Context(), db, envConfig, instanceID, "ES256", "", time.Hour, time.Now())
	require.NoError(t, err)
	assert.Equal(t, legacyKid, result.RetiredKid)
	assert.NotEqual(t, legacyKid, result.NewActiveKid)

	loaded, err := LoadSigningKeySet(t.Context(), db, envConfig, instanceID)
	require.NoError(t, err)
	require.Len(t, loaded.Set.Generations, 2, "The legacy key must be kept as a retired generation")
	require.NotNil(t, loaded.Keys[legacyKid], "The retired legacy key must still be decryptable")
}

func TestCompareAndSwapRejectsStaleReads(t *testing.T) {
	db, envConfig, instanceID := newSetTestEnv(t)

	store, err := GetSigningKeySetStore(db, envConfig, instanceID)
	require.NoError(t, err)

	first, err := EnsureSigningKeySet(t.Context(), db, envConfig, instanceID, time.Now())
	require.NoError(t, err)
	_, staleRaw, err := store.LoadSet(t.Context())
	require.NoError(t, err)

	// A committed rotation changes the row, so a write based on the previous value must fail
	_, err = RotateSigningKeys(t.Context(), db, envConfig, instanceID, "ES256", "", time.Hour, time.Now())
	require.NoError(t, err)

	// Attempt to commit the stale document back: nothing must be overwritten
	stale := first.Set
	swapped, err := store.CompareAndSwapSet(t.Context(), staleRaw, stale)
	require.NoError(t, err)
	assert.False(t, swapped, "A stale compare-and-swap must not replace the current set")

	current, err := LoadSigningKeySet(t.Context(), db, envConfig, instanceID)
	require.NoError(t, err)
	_, currentActive := current.ActiveKey()
	assert.Equal(t, "ES256", currentActive.Alg, "The committed rotation must survive the stale write")
}

func TestRotateSigningKeysDropsAlreadyExpiredGenerations(t *testing.T) {
	db, envConfig, instanceID := newSetTestEnv(t)
	store, err := GetSigningKeySetStore(db, envConfig, instanceID)
	require.NoError(t, err)

	// Seed three generations: one active, one still in its compatibility window, one expired
	seeded := buildSeedSet(t, store, []seedGenerationSpec{
		{alg: "RS256", retired: true, expired: true},
		{alg: "RS384", retired: true, expired: false},
		{alg: "ES256", active: true},
	})
	inserted, err := store.CreateSet(t.Context(), seeded)
	require.NoError(t, err)
	require.True(t, inserted)

	// Rotation must remove the expired generation but keep the one still within the window
	result, err := RotateSigningKeys(t.Context(), db, envConfig, instanceID, "EdDSA", "Ed25519", time.Hour, time.Now())
	require.NoError(t, err)

	loaded, err := LoadSigningKeySet(t.Context(), db, envConfig, instanceID)
	require.NoError(t, err)
	assert.Len(t, loaded.Set.Generations, 3, "Expired generation removed; active and unexpired retained; new generation appended")
	assert.Equal(t, result.NewActiveKid, loaded.Set.ActiveKid)
	for _, gen := range loaded.Set.Generations {
		assert.NotEqual(t, "RS256", gen.Alg, "The expired RS256 generation must have been pruned")
	}
}

func TestPruneRetiredSigningKeys(t *testing.T) {
	db, envConfig, instanceID := newSetTestEnv(t)
	store, err := GetSigningKeySetStore(db, envConfig, instanceID)
	require.NoError(t, err)

	seeded := buildSeedSet(t, store, []seedGenerationSpec{
		{kid: "expired", alg: "RS256", retired: true, expired: true},
		{kid: "valid", alg: "RS384", retired: true, expired: false},
		{kid: "active", alg: "ES256", active: true},
	})
	inserted, err := store.CreateSet(t.Context(), seeded)
	require.NoError(t, err)
	require.True(t, inserted)

	now := time.Now()
	removed, err := PruneRetiredSigningKeys(t.Context(), db, envConfig, instanceID, now)
	require.NoError(t, err)
	assert.Equal(t, []string{"expired"}, removed, "Only the generation past its retain-until deadline is removed")

	loaded, err := LoadSigningKeySet(t.Context(), db, envConfig, instanceID)
	require.NoError(t, err)
	assert.Len(t, loaded.Set.Generations, 2)
	_, activeGen := loaded.ActiveKey()
	assert.Equal(t, "active", activeGen.Kid, "The active generation can never be pruned")

	var kids []string
	for _, gen := range loaded.Set.Generations {
		kids = append(kids, gen.Kid)
	}
	assert.Contains(t, kids, "valid")
	assert.NotContains(t, kids, "expired")

	// A second run after the deadline is a safe no-op, so cleanup restarts and failures can retry
	removed, err = PruneRetiredSigningKeys(t.Context(), db, envConfig, instanceID, now)
	require.NoError(t, err)
	assert.Empty(t, removed)
}

func TestPruneRetiredSigningKeysWithoutSet(t *testing.T) {
	db, envConfig, instanceID := newSetTestEnv(t)

	// Pruning before the first startup must be a no-op rather than an error
	removed, err := PruneRetiredSigningKeys(t.Context(), db, envConfig, instanceID, time.Now())
	require.NoError(t, err)
	assert.Empty(t, removed)
}

func TestReEncryptSigningKeySet(t *testing.T) {
	db, envConfig, instanceID := newSetTestEnv(t)

	_, err := EnsureSigningKeySet(t.Context(), db, envConfig, instanceID, time.Now())
	require.NoError(t, err)
	_, err = RotateSigningKeys(t.Context(), db, envConfig, instanceID, "ES256", "", 2*time.Hour, time.Now())
	require.NoError(t, err)

	oldKek, err := LoadKeyEncryptionKey(envConfig, instanceID)
	require.NoError(t, err)
	newMaster := []byte("different-encryption-key-32-chars")
	newKek, err := LoadKeyEncryptionKey(&common.EnvConfigSchema{EncryptionKey: newMaster}, instanceID)
	require.NoError(t, err)

	require.NoError(t, ReEncryptSigningKeySet(t.Context(), db, oldKek, newKek))

	// Every generation is readable with the new KEK, and active kid plus deadlines survive re-wrapping
	loaded, err := LoadSigningKeySet(t.Context(), db, &common.EnvConfigSchema{EncryptionKey: newMaster}, instanceID)
	require.NoError(t, err)
	assert.Len(t, loaded.Set.Generations, 2)
	_, gen := loaded.ActiveKey()
	assert.Equal(t, "ES256", gen.Alg)

	// The old KEK must no longer be able to decrypt the generations
	_, err = LoadSigningKeySet(t.Context(), db, envConfig, instanceID)
	require.Error(t, err, "Ciphertext re-wrapped with the new KEK must fail to decrypt with the old KEK")
}

func TestSigningKeySetValidate(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Hour)

	tests := []struct {
		name string
		set  *SigningKeySet
	}{
		{name: "zero version", set: &SigningKeySet{Version: 0, ActiveKid: "a", Generations: []SigningKeyGeneration{{Kid: "a", EncryptedJWK: "x"}}}},
		{name: "empty active kid", set: &SigningKeySet{Version: 1, ActiveKid: "", Generations: []SigningKeyGeneration{{Kid: "a", EncryptedJWK: "x"}}}},
		{name: "no active generation", set: &SigningKeySet{Version: 1, ActiveKid: "a", Generations: []SigningKeyGeneration{{Kid: "a", EncryptedJWK: "x", RetiredAt: &past}}}},
		{name: "two active generations", set: &SigningKeySet{Version: 1, ActiveKid: "a", Generations: []SigningKeyGeneration{{Kid: "a", EncryptedJWK: "x"}, {Kid: "b", EncryptedJWK: "y"}}}},
		{name: "active kid unknown", set: &SigningKeySet{Version: 1, ActiveKid: "c", Generations: []SigningKeyGeneration{{Kid: "a", EncryptedJWK: "x"}}}},
		{name: "duplicate kids", set: &SigningKeySet{Version: 1, ActiveKid: "a", Generations: []SigningKeyGeneration{{Kid: "a", EncryptedJWK: "x"}, {Kid: "a", EncryptedJWK: "y", RetiredAt: &past}}}},
		{name: "missing key material", set: &SigningKeySet{Version: 1, ActiveKid: "a", Generations: []SigningKeyGeneration{{Kid: "a"}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.ErrorIs(t, tt.set.Validate(), ErrSigningKeySetInvalid)
		})
	}

	// The valid shape passes
	valid := &SigningKeySet{
		Version:   1,
		ActiveKid: "a",
		Generations: []SigningKeyGeneration{
			{Kid: "a", EncryptedJWK: "x"},
			{Kid: "b", EncryptedJWK: "y", RetiredAt: &past, RetainUntil: &now},
		},
	}
	require.NoError(t, valid.Validate())

	// Default retention must cover the maximum OIDC token lifetime
	assert.Equal(t, time.Duration(model.MaxTokenDurationMinutes)*time.Minute, DefaultRetiredSigningKeyRetention())
}

type seedGenerationSpec struct {
	kid     string
	alg     string
	active  bool
	retired bool
	expired bool
}

// buildSeedSet constructs a valid persistent set directly, so tests can place generations at arbitrary deadlines
func buildSeedSet(t *testing.T, store *SigningKeySetStore, specs []seedGenerationSpec) *SigningKeySet {
	t.Helper()

	now := time.Now()
	set := &SigningKeySet{Version: signingKeySetVersion}
	for _, spec := range specs {
		key, err := GenerateKey(spec.alg, crvForAlg(spec.alg))
		require.NoError(t, err)
		if spec.kid != "" {
			require.NoError(t, key.Set(jwxjwk.KeyIDKey, spec.kid))
		}
		encrypted, err := store.EncryptKey(key)
		require.NoError(t, err)
		kid, _ := key.KeyID()

		gen := SigningKeyGeneration{
			Kid:          kid,
			Alg:          spec.alg,
			EncryptedJWK: encrypted,
			CreatedAt:    now.Add(-2 * time.Hour),
		}
		if spec.retired || spec.expired {
			retiredAt := now.Add(-time.Hour)
			gen.RetiredAt = &retiredAt
			if spec.expired {
				retainUntil := now.Add(-time.Minute)
				gen.RetainUntil = &retainUntil
			} else {
				retainUntil := now.Add(time.Hour)
				gen.RetainUntil = &retainUntil
			}
		}
		if spec.active {
			set.ActiveKid = kid
		}
		set.Generations = append(set.Generations, gen)
	}
	require.NotEmpty(t, set.ActiveKid, "Seed set must designate an active generation")
	return set
}

func crvForAlg(alg string) string {
	if alg == "EdDSA" {
		return "Ed25519"
	}
	return ""
}
