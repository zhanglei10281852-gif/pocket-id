package service

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"testing"
	"time"

	jwxjwa "github.com/lestrrat-go/jwx/v4/jwa"
	jwxjwk "github.com/lestrrat-go/jwx/v4/jwk"
	jwxjwt "github.com/lestrrat-go/jwx/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pocket-id/pocket-id/backend/internal/common"
	"github.com/pocket-id/pocket-id/backend/internal/instanceid"
	jwkutils "github.com/pocket-id/pocket-id/backend/internal/utils/jwk"
	testutils "github.com/pocket-id/pocket-id/backend/internal/utils/testing"
)

// rotateAndReload rotates the signing key like the key-rotate command would, then forces the running service to reload it
func rotateAndReload(t *testing.T, svc *JwtService, alg, crv string, retention time.Duration) jwkutils.RotationResult {
	t.Helper()

	result, err := jwkutils.RotateSigningKeys(t.Context(), svc.db, svc.envConfig, svc.instanceID, alg, crv, retention, time.Now())
	require.NoError(t, err)
	require.NoError(t, svc.ReloadSigningKeys(t.Context()), "The running signer must reload the rotated active generation")
	return result
}

// expireRetiredGeneration rewrites the set so every retired generation's compatibility deadline is in the past
func expireRetiredGeneration(t *testing.T, db *gorm.DB, envConfig *common.EnvConfigSchema, instanceID string) {
	t.Helper()

	store, err := jwkutils.GetSigningKeySetStore(db, envConfig, instanceID)
	require.NoError(t, err)
	set, rawValue, err := store.LoadSet(t.Context())
	require.NoError(t, err)

	past := time.Now().Add(-time.Minute)
	for i := range set.Generations {
		if !set.Generations[i].Active() {
			set.Generations[i].RetainUntil = &past
		}
	}
	set.Version++

	swapped, err := store.CompareAndSwapSet(t.Context(), rawValue, set)
	require.NoError(t, err)
	require.True(t, swapped)
}

func signTokenForTest(t *testing.T, alg jwxjwa.KeyAlgorithm, key jwxjwk.Key, issuer string, expiry time.Time) string {
	t.Helper()

	token, err := jwxjwt.NewBuilder().
		Issuer(issuer).
		Subject("user-1").
		IssuedAt(time.Now()).
		Expiration(expiry).
		Build()
	require.NoError(t, err)
	signed, err := jwxjwt.Sign(token, jwxjwt.WithKey(alg, key))
	require.NoError(t, err)
	return string(signed)
}

// captureJWKSAlg extracts the alg value for a given kid from a raw JWKS document
func captureJWKSAlg(t *testing.T, raw []byte, kid string) string {
	t.Helper()

	set, err := jwxjwk.ParseString(string(raw))
	require.NoError(t, err)
	key, ok := set.LookupKeyID(kid)
	require.True(t, ok, "kid %s must be present in the JWKS", kid)
	alg, ok := key.Algorithm()
	require.True(t, ok)
	return `"` + alg.String() + `"`
}

func TestSigningKeyGenerationsJWKS(t *testing.T) {
	db := testutils.NewDatabaseForTest(t)
	envConfig := newTestEnvConfig()
	instanceID, err := instanceid.Load(t.Context(), db)
	require.NoError(t, err)
	svc := initJwtService(t, db, instanceID, nil, envConfig)

	firstKid, ok := svc.GetKeyID()
	require.True(t, ok)
	require.NotEmpty(t, firstKid)

	// Rotate RS256 -> ES256 while the service keeps running
	rotation := rotateAndReload(t, svc, "ES256", "", 48*time.Hour)
	require.Equal(t, firstKid, rotation.RetiredKid)

	// The JWKS must publish both public keys
	jwksJSON, err := svc.GetPublicJWKSAsJSON()
	require.NoError(t, err)
	jwksString := string(jwksJSON)
	assert.Contains(t, jwksString, firstKid, "JWKS must keep the retired kid within its compatibility window")
	assert.Contains(t, jwksString, rotation.NewActiveKid, "JWKS must publish the new active kid")
	assert.Equal(t, `"RS256"`, captureJWKSAlg(t, jwksJSON, firstKid))
	assert.Equal(t, `"ES256"`, captureJWKSAlg(t, jwksJSON, rotation.NewActiveKid))

	// The JWKS must never leak private key material or the symmetric session key
	assert.NotContains(t, jwksString, `"d"`, "JWKS must not contain RSA private material")
	assert.NotContains(t, jwksString, `"p"`, "JWKS must not contain RSA private primes")
	assert.NotContains(t, jwksString, `"k"`, "JWKS must not contain symmetric private material")
	sessionKid, _ := svc.sessionKey.KeyID()
	assert.NotContains(t, jwksString, sessionKid, "JWKS must never contain the HS256 session key kid")
	assert.NotContains(t, jwksString, jwxjwa.OctetSeq().String(), "JWKS must never contain symmetric keys")

	// Discovery advertises every published algorithm, active first
	algs, err := svc.GetKeyAlgorithms()
	require.NoError(t, err)
	assert.Equal(t, []string{"ES256", "RS256"}, algs)

	// The active kid is the new one
	activeKid, _ := svc.GetKeyID()
	assert.Equal(t, rotation.NewActiveKid, activeKid)
}

func TestSigningKeyGenerationsVerifyOldTokensByKid(t *testing.T) {
	for _, tc := range []struct {
		name      string
		alg       string
		crv       string
		nextAlg   string
		nextCrv   string
		retiredIs func(any) bool
	}{
		{name: "RS384", alg: "RS384", nextAlg: "ES256", retiredIs: func(k any) bool { _, ok := k.(*rsa.PublicKey); return ok }},
		{name: "ES256", alg: "ES256", nextAlg: "RS256", retiredIs: func(k any) bool { _, ok := k.(*ecdsa.PublicKey); return ok }},
		{name: "EdDSA", alg: "EdDSA", crv: "Ed25519", nextAlg: "ES256", retiredIs: func(k any) bool { _, ok := k.(ed25519.PublicKey); return ok }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testutils.NewDatabaseForTest(t)
			envConfig := newTestEnvConfig()
			instanceID, err := instanceid.Load(t.Context(), db)
			require.NoError(t, err)
			svc := initJwtService(t, db, instanceID, nil, envConfig)

			// First rotation creates the generation under test, the second one retires it so it lands in the JWKS window
			retiredRotation := rotateAndReload(t, svc, tc.alg, tc.crv, time.Hour)
			loadedRetired, err := jwkutils.LoadSigningKeySet(t.Context(), db, envConfig, instanceID)
			require.NoError(t, err)
			retiredKey := loadedRetired.Keys[retiredRotation.NewActiveKid]
			require.NotNil(t, retiredKey)
			retiredAlg, _ := retiredKey.Algorithm()
			rotateAndReload(t, svc, tc.nextAlg, tc.nextCrv, time.Hour)

			// Sign a token with the retired key, as an application holding a pre-rotation token would have
			tokenString := signTokenForTest(t, retiredAlg, retiredKey, envConfig.AppURL, time.Now().Add(time.Minute))

			// The retired kid resolves to its raw public key for go-jose/fosite verification
			rawPublic, ok := svc.GetVerificationKey(retiredRotation.NewActiveKid)
			require.True(t, ok, "Retired kid must resolve to a verification key")
			assert.True(t, tc.retiredIs(rawPublic), "Resolved key must be the retired algorithm's raw public key")

			// Verification against the published key set works without picking the active key
			keySet, err := svc.GetVerificationKeySet()
			require.NoError(t, err)
			parsed, err := jwxjwt.ParseString(tokenString,
				jwxjwt.WithValidate(true),
				jwxjwt.WithKeySet(keySet),
				jwxjwt.WithIssuer(envConfig.AppURL),
			)
			require.NoError(t, err, "A token signed by the retired generation must verify through the JWKS key set")
			subject, _ := parsed.Subject()
			assert.Equal(t, "user-1", subject)

			// An unknown kid does not resolve and must not fall back silently
			_, ok = svc.GetVerificationKey("does-not-exist")
			assert.False(t, ok)
		})
	}
}

func TestSigningKeyGenerationsReloadPicksUpRotation(t *testing.T) {
	db := testutils.NewDatabaseForTest(t)
	envConfig := newTestEnvConfig()
	instanceID, err := instanceid.Load(t.Context(), db)
	require.NoError(t, err)
	svc := initJwtService(t, db, instanceID, nil, envConfig)

	originalKid, _ := svc.GetKeyID()

	// Simulate the key-rotate command in a separate process: no service method is involved
	result, err := jwkutils.RotateSigningKeys(t.Context(), db, envConfig, instanceID, "ES256", "", time.Hour, time.Now())
	require.NoError(t, err)

	// Before reloading, the running signer still uses the old key, as the reload interval has not elapsed yet
	kidBeforeReload, _ := svc.GetKeyID()
	assert.Equal(t, originalKid, kidBeforeReload)

	// A reload aligns the signer with the persistent active generation
	require.NoError(t, svc.ReloadSigningKeys(t.Context()))
	newKid, _ := svc.GetKeyID()
	assert.Equal(t, result.NewActiveKid, newKid)
	alg, err := svc.GetKeyAlg()
	require.NoError(t, err)
	assert.Equal(t, "ES256", alg.String())

	// The retired key remains verifiable after reload
	_, ok := svc.GetVerificationKey(originalKid)
	assert.True(t, ok)
}

func TestSigningKeyGenerationsCleanup(t *testing.T) {
	db := testutils.NewDatabaseForTest(t)
	envConfig := newTestEnvConfig()
	instanceID, err := instanceid.Load(t.Context(), db)
	require.NoError(t, err)
	svc := initJwtService(t, db, instanceID, nil, envConfig)

	firstKid, _ := svc.GetKeyID()
	rotateAndReload(t, svc, "ES256", "", time.Hour)

	// Both keys are published before the deadline
	_, ok := svc.GetVerificationKey(firstKid)
	require.True(t, ok)

	// Force the retired generation past its retain-until deadline, then run the cleanup job
	expireRetiredGeneration(t, db, envConfig, instanceID)
	count, err := svc.CleanupExpiredRetiredKeys(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	// The expired retired key is gone, while the active key keeps signing
	_, ok = svc.GetVerificationKey(firstKid)
	assert.False(t, ok, "Keys past their compatibility window must be removed from the JWKS")
	activeKid, _ := svc.GetKeyID()
	assert.NotEqual(t, firstKid, activeKid)

	// Running the cleanup again is a safe, idempotent no-op
	count, err = svc.CleanupExpiredRetiredKeys(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

func TestSigningKeyGenerationsReclaimStopsVerification(t *testing.T) {
	db := testutils.NewDatabaseForTest(t)
	envConfig := newTestEnvConfig()
	instanceID, err := instanceid.Load(t.Context(), db)
	require.NoError(t, err)
	svc := initJwtService(t, db, instanceID, nil, envConfig)

	loaded, err := jwkutils.LoadSigningKeySet(t.Context(), db, envConfig, instanceID)
	require.NoError(t, err)
	oldKey, _ := loaded.ActiveKey()
	oldAlg, _ := oldKey.Algorithm()

	rotateAndReload(t, svc, "ES256", "", time.Hour)

	// While the window is open the old token verifies
	tokenString := signTokenForTest(t, oldAlg, oldKey, envConfig.AppURL, time.Now().Add(time.Minute))
	keySet, err := svc.GetVerificationKeySet()
	require.NoError(t, err)
	_, err = jwxjwt.ParseString(tokenString, jwxjwt.WithValidate(true), jwxjwt.WithKeySet(keySet))
	require.NoError(t, err)

	// After the cleanup removes the generation the same token can no longer verify
	expireRetiredGeneration(t, db, envConfig, instanceID)
	_, err = svc.CleanupExpiredRetiredKeys(t.Context())
	require.NoError(t, err)
	keySet, err = svc.GetVerificationKeySet()
	require.NoError(t, err)
	_, err = jwxjwt.ParseString(tokenString, jwxjwt.WithValidate(true), jwxjwt.WithKeySet(keySet))
	require.Error(t, err, "Verification must fail once the retired key is reclaimed")
}
