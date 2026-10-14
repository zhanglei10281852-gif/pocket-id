package cmds

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pocket-id/pocket-id/backend/internal/common"
	"github.com/pocket-id/pocket-id/backend/internal/instanceid"
	jwkutils "github.com/pocket-id/pocket-id/backend/internal/utils/jwk"
	testingutils "github.com/pocket-id/pocket-id/backend/internal/utils/testing"
)

func TestKeyRotate(t *testing.T) {
	tests := []struct {
		name    string
		flags   keyRotateFlags
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid RS256",
			flags: keyRotateFlags{
				Alg: "RS256",
				Yes: true,
			},
			wantErr: false,
		},
		{
			name: "valid EdDSA with Ed25519",
			flags: keyRotateFlags{
				Alg: "EdDSA",
				Crv: "Ed25519",
				Yes: true,
			},
			wantErr: false,
		},
		{
			name: "invalid algorithm",
			flags: keyRotateFlags{
				Alg: "INVALID",
				Yes: true,
			},
			wantErr: true,
			errMsg:  "unsupported key algorithm",
		},
		{
			name: "EdDSA without curve",
			flags: keyRotateFlags{
				Alg: "EdDSA",
				Yes: true,
			},
			wantErr: true,
			errMsg:  "a curve name is required when algorithm is EdDSA",
		},
		{
			name: "empty algorithm",
			flags: keyRotateFlags{
				Alg: "",
				Yes: true,
			},
			wantErr: true,
			errMsg:  "key algorithm is required",
		},
		{
			name: "negative retention",
			flags: keyRotateFlags{
				Alg:           "RS256",
				RetentionDays: -1,
				Yes:           true,
			},
			wantErr: true,
			errMsg:  "retention days must be a positive number",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testKeyRotateWithDatabaseStorage(t, tt.flags, tt.wantErr, tt.errMsg)
		})
	}
}

func testKeyRotateWithDatabaseStorage(t *testing.T, flags keyRotateFlags, wantErr bool, errMsg string) {
	envConfig := &common.EnvConfigSchema{
		EncryptionKey: []byte("test-encryption-key-characters-long"),
	}

	db := testingutils.NewDatabaseForTest(t)
	instanceID, err := instanceid.Load(t.Context(), db)
	require.NoError(t, err)

	// Run the key rotation
	err = keyRotate(t.Context(), flags, db, instanceID, envConfig)

	if wantErr {
		require.Error(t, err)
		if errMsg != "" {
			require.ErrorContains(t, err, errMsg)
		}
		return
	}
	require.NoError(t, err)

	// The new key must be the active generation of the persistent set
	loaded, err := jwkutils.LoadSigningKeySet(t.Context(), db, envConfig, instanceID)
	require.NoError(t, err)
	key, gen := loaded.ActiveKey()
	require.NotNil(t, key)
	require.NotNil(t, gen)
	assert.Nil(t, gen.RetiredAt, "Rotated-to generation must be active")

	alg, _ := key.Algorithm()
	assert.NotEmpty(t, alg)
	if flags.Alg != "" {
		expectedAlg := flags.Alg
		if expectedAlg == "EdDSA" {
			assert.Equal(t, "EdDSA", alg.String())
		} else {
			assert.Equal(t, expectedAlg, alg.String())
		}
	}
}

func TestKeyRotateSessionKey(t *testing.T) {
	envConfig := &common.EnvConfigSchema{
		EncryptionKey: []byte("test-encryption-key-characters-long"),
	}

	db := testingutils.NewDatabaseForTest(t)
	instanceID, err := instanceid.Load(t.Context(), db)
	require.NoError(t, err)

	// Seed both keys so we can check that only the session key is replaced
	signingKey, err := jwkutils.GenerateKey("ES256", "")
	require.NoError(t, err)
	signingKeyProvider, err := jwkutils.GetKeyProvider(db, envConfig, instanceID)
	require.NoError(t, err)
	err = signingKeyProvider.SaveKey(t.Context(), signingKey)
	require.NoError(t, err)
	signingKeyID, _ := signingKey.KeyID()

	sessionKeyProvider, err := jwkutils.GetSessionKeyProvider(db, envConfig, instanceID)
	require.NoError(t, err)
	originalSessionKey, err := jwkutils.GenerateSessionKey()
	require.NoError(t, err)
	err = sessionKeyProvider.SaveKey(t.Context(), originalSessionKey)
	require.NoError(t, err)

	// Rotate the session key
	err = keyRotate(t.Context(), keyRotateFlags{SessionKey: true, Yes: true}, db, instanceID, envConfig)
	require.NoError(t, err)

	// The session key must have been replaced with a new HS256 key
	rotatedSessionKey, err := sessionKeyProvider.LoadKey(t.Context())
	require.NoError(t, err)
	require.NotNil(t, rotatedSessionKey)

	alg, ok := rotatedSessionKey.Algorithm()
	_ = assert.True(t, ok) &&
		assert.Equal(t, "HS256", alg.String())

	originalKeyID, _ := originalSessionKey.KeyID()
	rotatedKeyID, _ := rotatedSessionKey.KeyID()
	assert.NotEqual(t, originalKeyID, rotatedKeyID, "Session key should have been replaced")

	// The seeded signing key row must be left completely untouched, since only the session key was rotated
	unchangedSigningKey, err := signingKeyProvider.LoadKey(t.Context())
	require.NoError(t, err)
	require.NotNil(t, unchangedSigningKey)
	unchangedSigningKeyID, _ := unchangedSigningKey.KeyID()
	assert.Equal(t, signingKeyID, unchangedSigningKeyID, "Token signing key should not have been rotated")
}

func TestKeyRotateDoesNotChangeSessionKey(t *testing.T) {
	envConfig := &common.EnvConfigSchema{
		EncryptionKey: []byte("test-encryption-key-characters-long"),
	}

	db := testingutils.NewDatabaseForTest(t)
	instanceID, err := instanceid.Load(t.Context(), db)
	require.NoError(t, err)

	sessionKeyProvider, err := jwkutils.GetSessionKeyProvider(db, envConfig, instanceID)
	require.NoError(t, err)
	originalSessionKey, err := jwkutils.GenerateSessionKey()
	require.NoError(t, err)
	err = sessionKeyProvider.SaveKey(t.Context(), originalSessionKey)
	require.NoError(t, err)

	// Rotating the token signing key must leave existing sessions valid
	err = keyRotate(t.Context(), keyRotateFlags{Alg: "ES256", Yes: true}, db, instanceID, envConfig)
	require.NoError(t, err)

	sessionKey, err := sessionKeyProvider.LoadKey(t.Context())
	require.NoError(t, err)
	require.NotNil(t, sessionKey)

	originalKeyID, _ := originalSessionKey.KeyID()
	sessionKeyID, _ := sessionKey.KeyID()
	assert.Equal(t, originalKeyID, sessionKeyID, "Session key should not have been rotated")
}

func TestKeyRotateMultipleAlgorithms(t *testing.T) {
	algorithms := []struct {
		alg string
		crv string
	}{
		{"RS256", ""},
		{"RS384", ""},
		// Skip RSA-4096 key generation test as it can take a long time
		// {"RS512", ""},
		{"ES256", ""},
		{"ES384", ""},
		{"ES512", ""},
		{"EdDSA", "Ed25519"},
	}

	for _, algo := range algorithms {
		t.Run(algo.alg, func(t *testing.T) {
			testKeyRotateWithDatabaseStorage(t, keyRotateFlags{
				Alg: algo.alg,
				Crv: algo.crv,
				Yes: true,
			}, false, "")
		})
	}
}

func TestKeyRotateKeepsPreviousGenerationRetired(t *testing.T) {
	envConfig := &common.EnvConfigSchema{
		EncryptionKey: []byte("test-encryption-key-characters-long"),
	}

	db := testingutils.NewDatabaseForTest(t)
	instanceID, err := instanceid.Load(t.Context(), db)
	require.NoError(t, err)

	// First rotation bootstraps the set with RS256
	err = keyRotate(t.Context(), keyRotateFlags{Alg: "RS256", RetentionDays: 30, Yes: true}, db, instanceID, envConfig)
	require.NoError(t, err)
	first, err := jwkutils.LoadSigningKeySet(t.Context(), db, envConfig, instanceID)
	require.NoError(t, err)
	_, firstGen := first.ActiveKey()
	require.NotNil(t, firstGen)
	firstKid := firstGen.Kid

	// Second rotation switches to ES256 and must retire the first generation atomically
	err = keyRotate(t.Context(), keyRotateFlags{Alg: "ES256", RetentionDays: 30, Yes: true}, db, instanceID, envConfig)
	require.NoError(t, err)
	second, err := jwkutils.LoadSigningKeySet(t.Context(), db, envConfig, instanceID)
	require.NoError(t, err)
	require.Len(t, second.Set.Generations, 2)

	_, activeGen := second.ActiveKey()
	require.NotNil(t, activeGen)
	assert.NotEqual(t, firstKid, activeGen.Kid)
	assert.Equal(t, "ES256", activeGen.Alg)

	var retired *jwkutils.SigningKeyGeneration
	for i := range second.Set.Generations {
		if second.Set.Generations[i].Kid == firstKid {
			retired = &second.Set.Generations[i]
			break
		}
	}
	require.NotNil(t, retired, "First generation must be retained")
	require.NotNil(t, retired.RetiredAt, "First generation must be marked retired")
	require.NotNil(t, retired.RetainUntil, "First generation must have a retain-until deadline")
	assert.WithinDuration(t, retired.RetiredAt.Add(30*24*time.Hour), *retired.RetainUntil, time.Minute, "Retention window must match the requested 30 days")

	// Exactly one active generation must ever exist
	activeCount := 0
	for _, gen := range second.Set.Generations {
		if gen.Active() {
			activeCount++
		}
	}
	assert.Equal(t, 1, activeCount, "There must be exactly one active generation")
}
