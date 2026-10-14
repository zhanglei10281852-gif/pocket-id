package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
	"uuid"

	"github.com/lestrrat-go/jwx/v4/jwa"
	"github.com/lestrrat-go/jwx/v4/jwk"
	"github.com/lestrrat-go/jwx/v4/jws"
	"github.com/lestrrat-go/jwx/v4/jwt"
	"gorm.io/gorm"

	"github.com/pocket-id/pocket-id/backend/internal/common"
	"github.com/pocket-id/pocket-id/backend/internal/model"
	"github.com/pocket-id/pocket-id/backend/internal/utils"
	jwkutils "github.com/pocket-id/pocket-id/backend/internal/utils/jwk"
)

const (
	// KeyUsageSigning is the usage for the private keys, for the "use" property
	KeyUsageSigning = "sig"

	// IsAdminClaim is a boolean claim used in access tokens for admin users
	// This may be omitted on non-admin tokens
	IsAdminClaim = "isAdmin"

	// TokenTypeClaim is the claim used to identify the type of token
	TokenTypeClaim = "type"

	// AuthenticationMethodPhishingResistant identifies phishing-resistant authentication, such as passkeys
	AuthenticationMethodPhishingResistant = "phr"

	// AuthenticationMethodOneTimePassword identifies one-time password/code authentication
	AuthenticationMethodOneTimePassword = "otp"

	// AccessTokenJWTType identifies a JWT as an access token used by Pocket ID
	AccessTokenJWTType = "access-token"

	// LogoutTokenJWTTyp is the JOSE "typ" header for OIDC back-channel logout tokens
	LogoutTokenJWTTyp = "logout+jwt" //nolint:gosec // this is a JOSE typ header, not a credential

	// BackchannelLogoutEvent is the member of the "events" claim that marks a JWT as a back-channel logout token
	BackchannelLogoutEvent = "http://schemas.openid.net/event/backchannel-logout"

	// logoutTokenDuration is the lifetime of a logout token, which only needs to survive the delivery to the client
	logoutTokenDuration = 2 * time.Minute

	// Acceptable clock skew for verifying tokens
	clockSkew = time.Minute

	// signingKeyReloadInterval is how often a running instance picks up rotations performed by the key-rotate command or another instance
	signingKeyReloadInterval = 30 * time.Second
)

// signingKeyState is an immutable snapshot of everything token signing and verification need
// It is replaced atomically when the persistent signing key set changes, so requests never observe a half-switched key
type signingKeyState struct {
	// activePrivate is the private key of the active generation and signs new tokens
	activePrivate jwk.Key
	// keyID is the kid of the active generation
	keyID string
	// alg is the algorithm of the active generation
	alg jwa.KeyAlgorithm
	// jwksEncoded is the published JWKS: the active public key plus every retired key still within its compatibility window
	jwksEncoded []byte
	// publicKeys is the same set as jwksEncoded in programmatic form, used to verify tokens by kid
	publicKeys jwk.Set
}

type JwtService struct {
	db        *gorm.DB
	envConfig *common.EnvConfigSchema
	// state is the active signing generation plus every still-published retired generation
	state atomic.Pointer[signingKeyState]
	// sessionKey is the symmetric key that signs Pocket ID's own session tokens
	sessionKey jwk.Key
	instanceID string
}

func NewJwtService(ctx context.Context, db *gorm.DB, instanceID string) (*JwtService, error) {
	service := &JwtService{}

	err := service.init(ctx, db, instanceID, &common.EnvConfig)
	if err != nil {
		return nil, err
	}

	return service, nil
}

func (s *JwtService) init(ctx context.Context, db *gorm.DB, instanceID string, envConfig *common.EnvConfigSchema) (err error) {
	s.envConfig = envConfig
	s.db = db
	s.instanceID = instanceID

	// Ensure keys are generated or loaded
	return s.LoadOrGenerateKey(ctx)
}

// LoadOrGenerateKey loads the signing keys from the database, generating and persisting them if they don't exist yet
func (s *JwtService) LoadOrGenerateKey(ctx context.Context) error {
	// Load the persistent set used for tokens that are consumed externally, such as ID tokens and access tokens for apps
	err := retryKeyStorage(ctx, s.loadOrGenerateSigningKeySet)
	if err != nil {
		return fmt.Errorf("error loading signing key: %w", err)
	}

	// Load the key used for Pocket ID's own sessions, which is symmetric and lives in its own kv row
	err = retryKeyStorage(ctx, s.loadOrGenerateSessionKey)
	if err != nil {
		return fmt.Errorf("error loading session key: %w", err)
	}

	return nil
}

func retryKeyStorage(ctx context.Context, loadOrGenerate func(context.Context) error) error {
	for retries := 0; ; retries++ {
		// Run the full load and store sequence so a key written by another replica takes precedence
		err := loadOrGenerate(ctx)
		if !errors.Is(err, jwkutils.ErrRetryKeyStorage) {
			return err
		}

		// Return the last conflict after the configured number of retries has been exhausted
		if retries == 3 {
			return err
		}

		// Wait briefly before reloading so the competing database transaction has time to finish
		slog.WarnContext(ctx, "Failed to store key, retrying", slog.Int("retry", retries+1), slog.Any("error", err))
		err = utils.SleepWithContext(ctx, 200*time.Millisecond)
		if err != nil {
			return err
		}
	}
}

// loadOrGenerateSigningKeySet loads the persistent generation set, adopting the legacy single key or generating the first one
func (s *JwtService) loadOrGenerateSigningKeySet(ctx context.Context) error {
	loaded, err := jwkutils.EnsureSigningKeySet(ctx, s.db, s.envConfig, s.instanceID, time.Now())
	if err != nil {
		return err
	}
	return s.applySigningKeySet(loaded, time.Now())
}

func (s *JwtService) loadOrGenerateSessionKey(ctx context.Context) error {
	// Get the key provider for the session key
	keyProvider, err := jwkutils.GetSessionKeyProvider(s.db, s.envConfig, s.instanceID)
	if err != nil {
		return fmt.Errorf("failed to get session key provider: %w", err)
	}

	// Try loading a key
	key, err := keyProvider.LoadKey(ctx)
	if err != nil {
		return fmt.Errorf("failed to load session key: %w", err)
	}

	// If we have a key, store it in the object and we're done
	if key != nil {
		err = s.SetSessionKey(key)
		if err != nil {
			return fmt.Errorf("failed to set session key: %w", err)
		}
		return nil
	}

	// If we are here, we need to generate a new key
	key, err = jwkutils.GenerateSessionKey()
	if err != nil {
		return fmt.Errorf("failed to generate session key: %w", err)
	}

	// Set the key in the object, which also validates it
	err = s.SetSessionKey(key)
	if err != nil {
		return fmt.Errorf("failed to set session key: %w", err)
	}

	// Save the newly-generated key
	err = keyProvider.SaveKey(ctx, key)
	if err != nil {
		return fmt.Errorf("failed to save session key: %w", err)
	}

	return nil
}

// applySigningKeySet builds an immutable snapshot from a decrypted persistent set and swaps it in atomically
// Retired generations past their retain-until deadline are kept for local verification only after the cleanup job removes them; here they are simply not published
func (s *JwtService) applySigningKeySet(loaded *jwkutils.LoadedSigningKeySet, now time.Time) error {
	activeKey, activeGen := loaded.ActiveKey()
	if activeKey == nil {
		return errors.New("persisted signing key set has no active generation")
	}
	if err := ValidateKey(activeKey); err != nil {
		return fmt.Errorf("active signing key is not valid: %w", err)
	}

	alg, ok := activeKey.Algorithm()
	if !ok || alg == nil {
		return errors.New("active signing key has no algorithm")
	}

	// Public keys are published with the active generation first, then the still-valid retired generations
	publicSet := jwk.NewSet()
	added := make(map[string]struct{}, len(loaded.Set.Generations))

	activePublic, err := publicJWK(activeKey)
	if err != nil {
		return err
	}
	if err := publicSet.AddKey(activePublic); err != nil {
		return fmt.Errorf("failed to add active public key to JWKS: %w", err)
	}
	added[activeGen.Kid] = struct{}{}

	for _, gen := range orderedRetiredGenerations(loaded.Set, now) {
		privateKey, ok := loaded.Keys[gen.Kid]
		if !ok {
			return fmt.Errorf("private key for retired generation %q is not available", gen.Kid)
		}
		publicKey, err := publicJWK(privateKey)
		if err != nil {
			return fmt.Errorf("failed to get public key for retired generation %q: %w", gen.Kid, err)
		}
		if _, exists := added[gen.Kid]; exists {
			continue
		}
		if err := publicSet.AddKey(publicKey); err != nil {
			return fmt.Errorf("failed to add retired public key %q to JWKS: %w", gen.Kid, err)
		}
		added[gen.Kid] = struct{}{}
	}

	jwksEncoded, err := json.Marshal(publicSet)
	if err != nil {
		return fmt.Errorf("failed to encode JWKS to JSON: %w", err)
	}

	newState := &signingKeyState{
		activePrivate: activeKey,
		keyID:         activeGen.Kid,
		alg:           alg,
		jwksEncoded:   jwksEncoded,
		publicKeys:    publicSet,
	}

	previous := s.state.Swap(newState)
	if previous == nil || previous.keyID != newState.keyID {
		slog.Info("Active OIDC signing key", slog.String("kid", newState.keyID), slog.String("alg", alg.String()), slog.Int("published_keys", publicSet.Len()))
	}
	return nil
}

// orderedRetiredGenerations returns generations that are neither active nor expired, most recently retired first
func orderedRetiredGenerations(set *jwkutils.SigningKeySet, now time.Time) []jwkutils.SigningKeyGeneration {
	retired := make([]jwkutils.SigningKeyGeneration, 0, len(set.Generations))
	for _, gen := range set.Generations {
		if gen.Active() || gen.Expired(now) {
			continue
		}
		retired = append(retired, gen)
	}
	// Sort without importing slices to keep the dependency surface small
	for i := 0; i < len(retired); i++ {
		for j := i + 1; j < len(retired); j++ {
			if retired[j].RetiredAt != nil && (retired[i].RetiredAt == nil || retired[j].RetiredAt.After(*retired[i].RetiredAt)) {
				retired[i], retired[j] = retired[j], retired[i]
			}
		}
	}
	return retired
}

// publicJWK derives the asymmetric public key published in the JWKS, carrying the key id and algorithm
func publicJWK(privateKey jwk.Key) (jwk.Key, error) {
	isPrivate, err := jwk.IsPrivateKey(privateKey)
	if err != nil || !isPrivate {
		// Defense in depth: a symmetric or already-public key can never be part of the external JWKS
		return nil, errors.New("signing key is not an asymmetric private key")
	}
	publicKey, err := privateKey.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("failed to get public key: %w", err)
	}
	jwkutils.EnsureAlgInKey(publicKey, "", "")
	return publicKey, nil
}

func ValidateKey(privateKey jwk.Key) error {
	// Validate the loaded key
	err := privateKey.Validate()
	if err != nil {
		return fmt.Errorf("key object is invalid: %w", err)
	}
	keyID, ok := privateKey.KeyID()
	if !ok || keyID == "" {
		return errors.New("key object does not contain a key ID")
	}
	usage, ok := privateKey.KeyUsage()
	if !ok || usage != KeyUsageSigning {
		return errors.New("key object is not valid for signing")
	}
	ok, err = jwk.IsPrivateKey(privateKey)
	if err != nil || !ok {
		return errors.New("key object is not a private key")
	}

	return nil
}

// ValidateSessionKey validates the symmetric key used to sign session tokens
func ValidateSessionKey(sessionKey jwk.Key) error {
	// Validate the loaded key
	err := sessionKey.Validate()
	if err != nil {
		return fmt.Errorf("key object is invalid: %w", err)
	}
	if sessionKey.KeyType() != jwa.OctetSeq() {
		return errors.New("key object is not a symmetric key")
	}
	keyID, ok := sessionKey.KeyID()
	if !ok || keyID == "" {
		return errors.New("key object does not contain a key ID")
	}
	usage, ok := sessionKey.KeyUsage()
	if !ok || usage != KeyUsageSigning {
		return errors.New("key object is not valid for signing")
	}

	// Session tokens are always signed with the same algorithm, so a key for anything else can't be used
	alg, ok := sessionKey.Algorithm()
	if !ok || alg == nil || alg.String() != jwkutils.SessionKeyAlg().String() {
		return fmt.Errorf("key object is not valid for the %s algorithm", jwkutils.SessionKeyAlg().String())
	}

	return nil
}

// SetKey sets a single key as the active signing key
// It is used when no persistent set is involved, for example in minimal controllers and tests
func (s *JwtService) SetKey(privateKey jwk.Key) error {
	// Validate the loaded key
	err := ValidateKey(privateKey)
	if err != nil {
		return fmt.Errorf("private key is not valid: %w", err)
	}

	kid, ok := privateKey.KeyID()
	if !ok || kid == "" {
		return errors.New("key object does not contain a key ID")
	}
	alg, ok := privateKey.Algorithm()
	if !ok || alg == nil {
		return errors.New("key object does not contain an algorithm")
	}

	publicKey, err := publicJWK(privateKey)
	if err != nil {
		return err
	}
	publicSet := jwk.NewSet()
	if err := publicSet.AddKey(publicKey); err != nil {
		return fmt.Errorf("failed to add public key to JWKS: %w", err)
	}
	jwksEncoded, err := json.Marshal(publicSet)
	if err != nil {
		return fmt.Errorf("failed to encode JWKS to JSON: %w", err)
	}

	s.state.Store(&signingKeyState{
		activePrivate: privateKey,
		keyID:         kid,
		alg:           alg,
		jwksEncoded:   jwksEncoded,
		publicKeys:    publicSet,
	})
	return nil
}

// SetSessionKey sets the symmetric key used to sign session tokens
// This key is never published in the JWKS, since it's a shared secret that only Pocket ID needs
func (s *JwtService) SetSessionKey(sessionKey jwk.Key) error {
	err := ValidateSessionKey(sessionKey)
	if err != nil {
		return fmt.Errorf("session key is not valid: %w", err)
	}

	s.sessionKey = sessionKey

	return nil
}

// currentState returns the active signing snapshot or an error when the signer is not initialized
func (s *JwtService) currentState() (*signingKeyState, error) {
	state := s.state.Load()
	if state == nil {
		return nil, errors.New("key is not initialized")
	}
	return state, nil
}

func (s *JwtService) GenerateAccessToken(user model.User, authenticationMethod string, sessionDuration time.Duration) (string, error) {
	if s.sessionKey == nil {
		return "", errors.New("session key is not initialized")
	}

	now := time.Now()
	token, err := jwt.NewBuilder().
		Subject(user.ID).
		Expiration(now.Add(sessionDuration)).
		IssuedAt(now).
		Issuer(s.envConfig.AppURL).
		JwtID(uuid.NewV4().String()).
		Build()
	if err != nil {
		return "", fmt.Errorf("failed to build token: %w", err)
	}

	err = SetAudienceString(token, s.envConfig.AppURL)
	if err != nil {
		return "", fmt.Errorf("failed to set 'aud' claim in token: %w", err)
	}

	err = SetTokenType(token, AccessTokenJWTType)
	if err != nil {
		return "", fmt.Errorf("failed to set 'type' claim in token: %w", err)
	}

	err = SetIsAdmin(token, user.IsAdmin)
	if err != nil {
		return "", fmt.Errorf("failed to set 'isAdmin' claim in token: %w", err)
	}

	err = SetAuthenticationMethods(token, authenticationMethod)
	if err != nil {
		return "", fmt.Errorf("failed to set '%s' claim in token: %w", common.AuthenticationMethodsClaim, err)
	}

	return s.SignSessionToken(token)
}

// GenerateLogoutToken creates a logout token for OIDC Back-Channel Logout 1.0
// The token identifies the user with a "sub" claim only, as Pocket ID does not track per-session "sid" values
func (s *JwtService) GenerateLogoutToken(userID string, clientID string) (string, error) {
	state, err := s.currentState()
	if err != nil {
		return "", err
	}

	now := time.Now()
	token, err := jwt.NewBuilder().
		Subject(userID).
		Audience([]string{clientID}).
		Expiration(now.Add(logoutTokenDuration)).
		IssuedAt(now).
		Issuer(s.envConfig.AppURL).
		JwtID(uuid.NewV4().String()).
		Claim("events", map[string]any{BackchannelLogoutEvent: struct{}{}}).
		Build()
	if err != nil {
		return "", fmt.Errorf("failed to build token: %w", err)
	}

	// The spec requires the "typ" header so clients can tell logout tokens apart from ID tokens
	headers := jws.NewHeaders()
	err = headers.Set(jws.TypeKey, LogoutTokenJWTTyp)
	if err != nil {
		return "", fmt.Errorf("failed to set 'typ' header: %w", err)
	}

	signed, err := jwt.Sign(token, jwt.WithKey(state.alg, state.activePrivate, jws.WithProtectedHeaders(headers)))
	if err != nil {
		return "", fmt.Errorf("failed to sign token: %w", err)
	}

	return string(signed), nil
}

// SignSessionToken signs feature-owned claims with the private symmetric session key
func (s *JwtService) SignSessionToken(token jwt.Token) (string, error) {
	if s.sessionKey == nil {
		return "", errors.New("session key is not initialized")
	}

	signed, err := jwt.Sign(token, jwt.WithKey(jwkutils.SessionKeyAlg(), s.sessionKey))
	if err != nil {
		return "", fmt.Errorf("failed to sign token: %w", err)
	}
	return string(signed), nil
}

// VerifySessionToken pins signature verification while the caller supplies its feature's validation rules
func (s *JwtService) VerifySessionToken(tokenString string, options ...jwt.ValidateOption) (jwt.Token, error) {
	if s.sessionKey == nil {
		return nil, errors.New("session key is not initialized")
	}

	parseOptions := []jwt.ParseOption{
		jwt.WithValidate(true),
		jwt.WithKey(jwkutils.SessionKeyAlg(), s.sessionKey),
	}
	for _, option := range options {
		parseOptions = append(parseOptions, option)
	}
	token, err := jwt.ParseString(tokenString, parseOptions...)
	if err != nil {
		return nil, fmt.Errorf("failed to parse token: %w", err)
	}
	return token, nil
}

func (s *JwtService) VerifyAccessToken(tokenString string) (jwt.Token, error) {
	if s.sessionKey == nil {
		return nil, errors.New("session key is not initialized")
	}
	return s.VerifySessionToken(tokenString,
		jwt.WithAcceptableSkew(clockSkew),
		jwt.WithAudience(s.envConfig.AppURL),
		jwt.WithIssuer(s.envConfig.AppURL),
		jwt.WithValidator(TokenTypeValidator(AccessTokenJWTType)),
	)
}

// GetPublicJWK returns the JSON Web Key (JWK) for the active public key
func (s *JwtService) GetPublicJWK() (jwk.Key, error) {
	state, err := s.currentState()
	if err != nil {
		return nil, err
	}
	return publicJWK(state.activePrivate)
}

// GetPublicJWKSAsJSON returns the JWKS used for token verification, encoded as JSON
// It contains the active public key and every retired public key still within its compatibility window
// The value is cached per snapshot and only rebuilt when the persistent set changes
func (s *JwtService) GetPublicJWKSAsJSON() ([]byte, error) {
	state, err := s.currentState()
	if err != nil {
		return nil, err
	}
	return state.jwksEncoded, nil
}

// GetKeyAlg returns the algorithm of the active signing key
func (s *JwtService) GetKeyAlg() (jwa.KeyAlgorithm, error) {
	state, err := s.currentState()
	if err != nil {
		return nil, err
	}
	return state.alg, nil
}

// GetKeyAlgorithms returns the distinct algorithms of every published key, active first
// Retired keys stay published during their compatibility window, so discovery must advertise their algorithms too
func (s *JwtService) GetKeyAlgorithms() ([]string, error) {
	state, err := s.currentState()
	if err != nil {
		return nil, err
	}

	algs := make([]string, 0, state.publicKeys.Len())
	seen := make(map[string]struct{}, state.publicKeys.Len())
	for i := range state.publicKeys.Len() {
		key, ok := state.publicKeys.Key(i)
		if !ok {
			continue
		}
		alg, ok := key.Algorithm()
		if !ok || alg == nil {
			continue
		}
		name := alg.String()
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		algs = append(algs, name)
	}
	if len(algs) == 0 {
		return nil, errors.New("no signing algorithm is published")
	}
	return algs, nil
}

// GetKeyID returns the key ID (kid) of the active signing key, if one is set
func (s *JwtService) GetKeyID() (string, bool) {
	state := s.state.Load()
	if state == nil {
		return "", false
	}
	return state.keyID, true
}

// GetVerificationKeySet returns the public keys used to verify OIDC tokens by kid
// The set never contains private keys or the symmetric session key
func (s *JwtService) GetVerificationKeySet() (jwk.Set, error) {
	state, err := s.currentState()
	if err != nil {
		return nil, err
	}
	return state.publicKeys, nil
}

// GetVerificationKey returns the raw public key for a kid, used by the internal JWT signer when verifying tokens by their "kid" header
// An unknown kid falls back to the caller, which should reject the token
func (s *JwtService) GetVerificationKey(kid string) (any, bool) {
	state := s.state.Load()
	if state == nil || kid == "" {
		return nil, false
	}
	pubKey, ok := state.publicKeys.LookupKeyID(kid)
	if !ok {
		return nil, false
	}
	rawKey, err := jwk.Export[any](pubKey)
	if err != nil {
		return nil, false
	}
	return rawKey, true
}

// ActivePrivateKey returns the private key of the active generation, mainly for diagnostics and tests
func (s *JwtService) ActivePrivateKey() jwk.Key {
	state := s.state.Load()
	if state == nil {
		return nil
	}
	return state.activePrivate
}

// ReloadSigningKeys refreshes the in-memory snapshot from the persistent set without generating anything
// It is used by running instances to pick up rotations performed by the key-rotate command or another instance
func (s *JwtService) ReloadSigningKeys(ctx context.Context) error {
	loaded, err := jwkutils.LoadSigningKeySet(ctx, s.db, s.envConfig, s.instanceID)
	if err != nil {
		return fmt.Errorf("failed to reload signing key set: %w", err)
	}
	return s.applySigningKeySet(loaded, time.Now())
}

// RunSigningKeyReloader periodically reloads the persistent signing key set until the context is canceled
func (s *JwtService) RunSigningKeyReloader(ctx context.Context) error {
	ticker := time.NewTicker(signingKeyReloadInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.ReloadSigningKeys(ctx); err != nil {
				// Keep serving with the previous snapshot: it still signs new tokens and the next tick retries
				slog.WarnContext(ctx, "Failed to reload OIDC signing key set, keeping current keys", slog.Any("error", err))
			}
		}
	}
}

// CleanupExpiredRetiredKeys removes generations whose compatibility window has elapsed, then refreshes the snapshot
// Removal is atomic and failure leaves everything in place, so the next scheduled run can retry safely
func (s *JwtService) CleanupExpiredRetiredKeys(ctx context.Context) (int, error) {
	removed, err := jwkutils.PruneRetiredSigningKeys(ctx, s.db, s.envConfig, s.instanceID, time.Now())
	if err != nil {
		return 0, err
	}
	if len(removed) == 0 {
		return 0, nil
	}

	if err := s.ReloadSigningKeys(ctx); err != nil {
		// The prune is already committed; a failed refresh only delays dropping the keys from the JWKS
		slog.WarnContext(ctx, "Removed retired signing keys but failed to refresh JWKS snapshot", slog.Any("error", err))
	}
	return len(removed), nil
}

// GetAuthenticationMethod returns the first authentication method in the "amr" claim in the token
func (s *JwtService) GetAuthenticationMethod(token jwt.Token) (string, error) {
	if !token.Has(common.AuthenticationMethodsClaim) {
		return "", nil
	}
	rawAuthenticationMethods, err := jwt.Get[[]any](token, common.AuthenticationMethodsClaim)
	if err != nil {
		return "", fmt.Errorf("failed to get '%s' claim: %w", common.AuthenticationMethodsClaim, err)
	}

	if len(rawAuthenticationMethods) == 0 {
		return "", nil
	}
	authenticationMethod, ok := rawAuthenticationMethods[0].(string)
	if !ok {
		return "", fmt.Errorf("invalid '%s' claim in token: expected array of strings", common.AuthenticationMethodsClaim)
	}
	return authenticationMethod, nil
}

// SetTokenType sets the "type" claim in the token
func SetTokenType(token jwt.Token, tokenType string) error {
	if tokenType == "" {
		return nil
	}
	return token.Set(TokenTypeClaim, tokenType)
}

// SetIsAdmin sets the "isAdmin" claim in the token
func SetIsAdmin(token jwt.Token, isAdmin bool) error {
	// Only set if true
	if !isAdmin {
		return nil
	}
	return token.Set(IsAdminClaim, isAdmin)
}

// SetAuthenticationMethods sets the authentication method references claim in the token
func SetAuthenticationMethods(token jwt.Token, authenticationMethod string) error {
	if authenticationMethod == "" {
		return nil
	}
	return token.Set(common.AuthenticationMethodsClaim, []string{authenticationMethod})
}

// SetAudienceString sets the "aud" claim with a value that is a string, and not an array
// This is permitted by RFC 7519, and it's done here for backwards-compatibility
func SetAudienceString(token jwt.Token, audience string) error {
	return token.Set(jwt.AudienceKey, audience)
}

// TokenTypeValidator is a validator function that checks the "type" claim in the token
func TokenTypeValidator(expectedTokenType string) jwt.ValidatorFunc {
	return func(_ context.Context, t jwt.Token) error {
		tokenType, err := jwt.Get[string](t, TokenTypeClaim)
		if err != nil {
			return fmt.Errorf("failed to get token type claim: %w", err)
		}
		if tokenType != expectedTokenType {
			return fmt.Errorf("invalid token type: expected %s, got %s", expectedTokenType, tokenType)
		}
		return nil
	}
}

// GetPrivateKey returns the raw private key of the active generation for the fosite signer
func (s *JwtService) GetPrivateKey() any {
	state := s.state.Load()
	if state == nil {
		return nil
	}
	privateKey, _ := jwk.Export[any](state.activePrivate)
	return privateKey
}
