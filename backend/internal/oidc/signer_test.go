package oidc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"strconv"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/require"
)

// newTestECKey generates an ECDSA P-256 key wrapped as a JOSE JSON Web Key with the given kid
func newTestECKey(t *testing.T, kid string) *ecdsa.PrivateKey {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return key
}

func signCompact(t *testing.T, key *ecdsa.PrivateKey, kid string) string {
	t.Helper()

	jwk := &jose.JSONWebKey{Key: key, KeyID: kid, Algorithm: string(jose.ES256)}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: jwk}, nil)
	require.NoError(t, err)

	claims := `{"sub":"user-1","exp":` + currentUnixPlusOneHour(t) + `}`
	obj, err := signer.Sign([]byte(claims))
	require.NoError(t, err)
	serialized, err := obj.CompactSerialize()
	require.NoError(t, err)
	return serialized
}

func currentUnixPlusOneHour(t *testing.T) string {
	t.Helper()
	return strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
}

func TestJWTSignerDecodesByKid(t *testing.T) {
	active := newTestECKey(t, "active")
	retired := newTestECKey(t, "retired")
	unknown := newTestECKey(t, "unknown")

	keyGetter := func(context.Context) (any, error) {
		return &jose.JSONWebKey{Key: active, KeyID: "active", Algorithm: string(jose.ES256)}, nil
	}
	resolve := func(kid string) (any, bool) {
		switch kid {
		case "active":
			return &active.PublicKey, true
		case "retired":
			return &retired.PublicKey, true
		default:
			return nil, false
		}
	}
	signer := newJWTSigner(keyGetter, resolve)

	// Tokens from the active generation verify
	activeToken := signCompact(t, active, "active")
	_, err := signer.Decode(t.Context(), activeToken)
	require.NoError(t, err, "The active generation must verify")

	// Tokens signed before a rotation by a retired-but-published generation verify through the kid resolver
	retiredToken := signCompact(t, retired, "retired")
	decoded, err := signer.Decode(t.Context(), retiredToken)
	require.NoError(t, err, "A token with a retired kid still in the JWKS window must verify")
	require.Equal(t, "retired", decoded.Header["kid"])

	// A kid that is neither active nor published must not fall back to the active key
	unknownToken := signCompact(t, unknown, "unknown")
	_, err = signer.Decode(t.Context(), unknownToken)
	require.Error(t, err, "An unknown kid must fail verification")

	// Validate returns the signature when verification succeeds
	signature, err := signer.Validate(t.Context(), retiredToken)
	require.NoError(t, err)
	require.NotEmpty(t, signature)
}

func TestJWTSignerWithoutResolverFallsBackToActiveKey(t *testing.T) {
	active := newTestECKey(t, "active")
	retired := newTestECKey(t, "retired")

	keyGetter := func(context.Context) (any, error) {
		return &jose.JSONWebKey{Key: active, KeyID: "active", Algorithm: string(jose.ES256)}, nil
	}
	// Signers without generations (test doubles) keep the legacy behavior: verify with the active key only
	signer := newJWTSigner(keyGetter, nil)

	activeToken := signCompact(t, active, "active")
	_, err := signer.Decode(t.Context(), activeToken)
	require.NoError(t, err)

	retiredToken := signCompact(t, retired, "retired")
	_, err = signer.Decode(t.Context(), retiredToken)
	require.Error(t, err, "Without a resolver only the active key can verify")
}
