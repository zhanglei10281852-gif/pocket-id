package oidc

import (
	"context"
	"errors"

	jose "github.com/go-jose/go-jose/v4"
	fositejwt "github.com/pocket-id/fosite/token/jwt"
)

// SigningKeyFromSigner wraps the raw signing key in a *jose.JSONWebKey that carries the
// key's algorithm.
//
// fosite's DefaultSigner picks the JOSE algorithm from the Go key TYPE alone: every
// *rsa.PrivateKey is signed with RS256 and every *ecdsa.PrivateKey with ES256, while an
// ed25519.PrivateKey is not supported at all.
func SigningKeyFromSigner(signer TokenSigner) (*jose.JSONWebKey, error) {
	rawKey := signer.GetPrivateKey()
	if rawKey == nil {
		return nil, errors.New("signing key is not available")
	}

	alg, err := signer.GetKeyAlg()
	if err != nil {
		return nil, err
	}

	signingKey := &jose.JSONWebKey{
		Key:       rawKey,
		Algorithm: alg.String(),
	}

	if keyID, ok := signer.GetKeyID(); ok {
		signingKey.KeyID = keyID
	}

	return signingKey, nil
}

type jwtSigner struct {
	*fositejwt.DefaultSigner
	// resolveVerificationKey looks up the public key of active and still-compatible retired generations by kid
	// It may be nil for signers that only expose the active key, such as minimal test doubles
	resolveVerificationKey func(kid string) (any, bool)
}

func newJWTSigner(keyGetter fositejwt.GetPrivateKeyFunc, resolveVerificationKey func(kid string) (any, bool)) *jwtSigner {
	return &jwtSigner{
		DefaultSigner:          &fositejwt.DefaultSigner{GetPrivateKey: keyGetter},
		resolveVerificationKey: resolveVerificationKey,
	}
}

func (s *jwtSigner) Validate(ctx context.Context, token string) (string, error) {
	if _, err := s.Decode(ctx, token); err != nil {
		return "", err
	}
	return s.GetSignature(ctx, token)
}

func (s *jwtSigner) Decode(ctx context.Context, token string) (*fositejwt.Token, error) {
	// The active key doubles as the fallback for tokens without a kid and for test signers
	key, err := s.GetPrivateKey(ctx)
	if err != nil {
		return nil, err
	}

	return fositejwt.Parse(token, func(parsed *fositejwt.Token) (any, error) {
		// Pick the public key matching the token's kid so tokens signed by a retired-but-still-valid generation verify
		if s.resolveVerificationKey != nil {
			if kid, _ := parsed.Header["kid"].(string); kid != "" {
				if verificationKey, ok := s.resolveVerificationKey(kid); ok {
					return verificationKey, nil
				}
			}
		}

		verificationKey := key
		if jsonWebKey, ok := key.(*jose.JSONWebKey); ok {
			publicKey := jsonWebKey.Public()
			return &publicKey, nil
		}
		return verificationKey, nil
	})
}
