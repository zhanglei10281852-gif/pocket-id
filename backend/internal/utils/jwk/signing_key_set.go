package jwk

// The OIDC token signing keys live as a persistent set of "generations" in a single "kv" row
// One generation is active and signs new tokens; retired generations stay verifiable until their retain-until deadline passes
// A single-row document makes switching the active generation an atomic compare-and-swap on both SQLite and Postgres

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	jwxjwk "github.com/lestrrat-go/jwx/v4/jwk"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/pocket-id/pocket-id/backend/internal/common"
	"github.com/pocket-id/pocket-id/backend/internal/model"
	cryptoutils "github.com/pocket-id/pocket-id/backend/internal/utils/crypto"
)

const (
	// SigningKeysDBKey is the kv row containing the persistent set of OIDC token signing keys
	SigningKeysDBKey = "oidc_signing_keys.json"

	// signingKeySetVersion is the schema version of the persisted document
	signingKeySetVersion = 1
)

var (
	// ErrSigningKeySetNotFound signals that the persistent signing key set has not been created yet
	ErrSigningKeySetNotFound = errors.New("signing key set not found")
	// ErrSigningKeySetConflict signals that the signing key set changed between read and write, so another writer won the race
	ErrSigningKeySetConflict = errors.New("the signing key set was modified by another process, please retry")
	// ErrSigningKeySetInvalid signals that the persisted signing key set does not contain exactly one active generation
	ErrSigningKeySetInvalid = errors.New("persisted signing key set is invalid")
)

// DefaultRetiredSigningKeyRetention is how long a retired signing key remains published for verification after rotation
// It covers the longest configurable OIDC token lifetime, so every token issued before rotation stays verifiable until it expires
func DefaultRetiredSigningKeyRetention() time.Duration {
	return time.Duration(model.MaxTokenDurationMinutes) * time.Minute
}

// SigningKeyGeneration is one generation of the OIDC signing key, with its private JWK encrypted at rest
type SigningKeyGeneration struct {
	// Kid identifies the key in JWS headers and the JWKS
	Kid string
	// Alg is the JWA signature algorithm the key was generated for
	Alg string
	// EncryptedJWK is the base64-encoded KEK ciphertext of the private JWK
	EncryptedJWK string
	// CreatedAt is when the generation was created
	CreatedAt time.Time
	// RetiredAt is when the generation stopped being the active one, or nil while it is active
	RetiredAt *time.Time
	// RetainUntil is when a retired generation may stop being published in the JWKS and be deleted
	RetainUntil *time.Time
}

// Active reports whether the generation is the current signing generation
func (g SigningKeyGeneration) Active() bool {
	return g.RetiredAt == nil
}

// Expired reports whether a retired generation has passed its retain-until deadline
func (g SigningKeyGeneration) Expired(now time.Time) bool {
	return !g.Active() && g.RetainUntil != nil && !g.RetainUntil.After(now)
}

// SigningKeySet is the persistent document: one active generation and zero or more retired generations
type SigningKeySet struct {
	Version     int
	ActiveKid   string
	Generations []SigningKeyGeneration
}

// Active returns the active generation and its index
func (s *SigningKeySet) Active() (*SigningKeyGeneration, int) {
	for i := range s.Generations {
		if s.Generations[i].Kid == s.ActiveKid {
			return &s.Generations[i], i
		}
	}
	return nil, -1
}

// Validate enforces the invariant the rest of the code relies on: exactly one active generation, and it must match ActiveKid
func (s *SigningKeySet) Validate() error {
	if s == nil || s.Version == 0 {
		return fmt.Errorf("%w: missing version", ErrSigningKeySetInvalid)
	}
	if s.ActiveKid == "" {
		return fmt.Errorf("%w: no active key id", ErrSigningKeySetInvalid)
	}

	activeCount := 0
	kids := make(map[string]struct{}, len(s.Generations))
	for i := range s.Generations {
		gen := &s.Generations[i]
		if gen.Kid == "" || gen.EncryptedJWK == "" {
			return fmt.Errorf("%w: generation %d is missing key material", ErrSigningKeySetInvalid, i)
		}
		if _, exists := kids[gen.Kid]; exists {
			return fmt.Errorf("%w: duplicate key id %q", ErrSigningKeySetInvalid, gen.Kid)
		}
		kids[gen.Kid] = struct{}{}
		if gen.Active() {
			activeCount++
		}
	}

	if activeCount != 1 {
		return fmt.Errorf("%w: expected exactly one active generation, found %d", ErrSigningKeySetInvalid, activeCount)
	}
	if _, ok := kids[s.ActiveKid]; !ok {
		return fmt.Errorf("%w: active key id %q is not among the stored generations", ErrSigningKeySetInvalid, s.ActiveKid)
	}
	return nil
}

// Persisted DTOs keep the on-disk format explicit and independent of the domain types
type signingKeyGenerationDTO struct {
	Kid          string     `json:"kid"`
	Alg          string     `json:"alg"`
	EncryptedJWK string     `json:"encrypted_jwk"`
	CreatedAt    time.Time  `json:"created_at"`
	RetiredAt    *time.Time `json:"retired_at,omitempty"`
	RetainUntil  *time.Time `json:"retain_until,omitempty"`
}

type signingKeySetDTO struct {
	Version     int                       `json:"version"`
	ActiveKid   string                    `json:"active_kid"`
	Generations []signingKeyGenerationDTO `json:"generations"`
}

func (s *SigningKeySet) toDTO() *signingKeySetDTO {
	dto := &signingKeySetDTO{
		Version:     s.Version,
		ActiveKid:   s.ActiveKid,
		Generations: make([]signingKeyGenerationDTO, len(s.Generations)),
	}
	for i, gen := range s.Generations {
		dto.Generations[i] = signingKeyGenerationDTO(gen)
	}
	return dto
}

func (d *signingKeySetDTO) toSet() *SigningKeySet {
	set := &SigningKeySet{
		Version:     d.Version,
		ActiveKid:   d.ActiveKid,
		Generations: make([]SigningKeyGeneration, len(d.Generations)),
	}
	for i, gen := range d.Generations {
		set.Generations[i] = SigningKeyGeneration(gen)
	}
	return set
}

// SigningKeySetStore reads and writes the persistent signing key set, encrypting every private JWK with the KEK
type SigningKeySetStore struct {
	db  *gorm.DB
	kek []byte
}

// GetSigningKeySetStore returns the store for the persistent OIDC token signing key set
func GetSigningKeySetStore(db *gorm.DB, envConfig *common.EnvConfigSchema, instanceID string) (*SigningKeySetStore, error) {
	kek, err := LoadKeyEncryptionKey(envConfig, instanceID)
	if err != nil {
		return nil, fmt.Errorf("failed to load encryption key: %w", err)
	}
	if len(kek) == 0 {
		return nil, errors.New("an encryption key is required for the signing key set")
	}
	return &SigningKeySetStore{db: db, kek: kek}, nil
}

// WithDB returns a store using the given transaction, keeping the same KEK
func (s *SigningKeySetStore) WithDB(db *gorm.DB) *SigningKeySetStore {
	return &SigningKeySetStore{db: db, kek: s.kek}
}

// LoadSet reads and parses the persistent set, returning the exact raw row value needed for a compare-and-swap
// A missing row returns ErrSigningKeySetNotFound so callers can distinguish "not initialized yet" from database errors
func (s *SigningKeySetStore) LoadSet(ctx context.Context) (set *SigningKeySet, rawValue string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	row := model.KV{Key: SigningKeysDBKey}
	err = s.db.WithContext(ctx).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, "", ErrSigningKeySetNotFound
	} else if err != nil {
		return nil, "", fmt.Errorf("failed to retrieve signing key set from the database: %w", err)
	}
	if row.Value == nil || *row.Value == "" {
		return nil, "", ErrSigningKeySetNotFound
	}

	set, err = decodeSigningKeySet([]byte(*row.Value))
	if err != nil {
		return nil, "", err
	}
	return set, *row.Value, nil
}

// CreateSet inserts the set only if no row exists yet, which serializes first-time initialization across instances
func (s *SigningKeySetStore) CreateSet(ctx context.Context, set *SigningKeySet) (inserted bool, err error) {
	value, err := s.encodeSet(set)
	if err != nil {
		return false, err
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result := s.db.
		WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "key"}},
			DoNothing: true,
		}).
		Create(&model.KV{Key: SigningKeysDBKey, Value: &value})
	if result.Error != nil {
		return false, fmt.Errorf("failed to store signing key set: %w", result.Error)
	}
	return result.RowsAffected == 1, nil
}

// CreateSetAndDeleteLegacy inserts the set and, only for the winning inserter, removes the superseded single-key row
// Both statements run in one transaction so a crash can never leave the adoption half-applied
func (s *SigningKeySetStore) CreateSetAndDeleteLegacy(ctx context.Context, set *SigningKeySet) (inserted bool, err error) {
	value, err := s.encodeSet(set)
	if err != nil {
		return false, err
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.
			Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "key"}},
				DoNothing: true,
			}).
			Create(&model.KV{Key: SigningKeysDBKey, Value: &value})
		if result.Error != nil {
			return fmt.Errorf("failed to store signing key set: %w", result.Error)
		}
		inserted = result.RowsAffected == 1

		// Only the writer that actually created the row may remove the legacy key, so a losing adopter can't delete it
		if inserted {
			if err := tx.Delete(&model.KV{}, "key = ?", PrivateKeyDBKey).Error; err != nil {
				return fmt.Errorf("failed to remove legacy signing key row: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return inserted, nil
}

// CompareAndSwapSet replaces the row value only if it still equals the raw value that was read
// A zero affected count means another process changed the set first
func (s *SigningKeySetStore) CompareAndSwapSet(ctx context.Context, previousRawValue string, set *SigningKeySet) (swapped bool, err error) {
	value, err := s.encodeSet(set)
	if err != nil {
		return false, err
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result := s.db.
		WithContext(ctx).
		Model(&model.KV{}).
		Where("key = ? AND value = ?", SigningKeysDBKey, previousRawValue).
		Update("value", value)
	if result.Error != nil {
		return false, fmt.Errorf("failed to update signing key set: %w", result.Error)
	}
	return result.RowsAffected == 1, nil
}

// LoadLegacyKey loads the single-key row used before signing key generations existed
// It returns nil when the row is absent
func (s *SigningKeySetStore) LoadLegacyKey(ctx context.Context) (jwxjwk.Key, error) {
	provider := &KeyProviderDatabase{db: s.db, kek: s.kek, dbKey: PrivateKeyDBKey}
	return provider.LoadKey(ctx)
}

// EncryptKey stores a private JWK as a base64-encoded KEK ciphertext
func (s *SigningKeySetStore) EncryptKey(key jwxjwk.Key) (string, error) {
	data, err := EncodeJWKBytes(key)
	if err != nil {
		return "", fmt.Errorf("failed to encode key to JSON: %w", err)
	}
	enc, err := cryptoutils.Encrypt(s.kek, data, nil)
	if err != nil {
		return "", fmt.Errorf("failed to encrypt key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(enc), nil
}

// DecryptKey parses a generation's private JWK back into a jwk.Key
func (s *SigningKeySetStore) DecryptKey(generation SigningKeyGeneration) (jwxjwk.Key, error) {
	enc, err := base64.StdEncoding.DecodeString(generation.EncryptedJWK)
	if err != nil {
		return nil, fmt.Errorf("failed to read encrypted key for kid %q: not a valid base64 value: %w", generation.Kid, err)
	}
	data, err := cryptoutils.Decrypt(s.kek, enc, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt key for kid %q: %w", generation.Kid, err)
	}
	key, err := jwxjwk.ParseKey(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse encrypted key for kid %q: %w", generation.Kid, err)
	}
	return key, nil
}

func (s *SigningKeySetStore) encodeSet(set *SigningKeySet) (string, error) {
	if err := set.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(set.toDTO())
	if err != nil {
		return "", fmt.Errorf("failed to encode signing key set: %w", err)
	}
	return string(data), nil
}

func decodeSigningKeySet(data []byte) (*SigningKeySet, error) {
	var dto signingKeySetDTO
	if err := json.Unmarshal(data, &dto); err != nil {
		return nil, fmt.Errorf("failed to parse signing key set: %w", err)
	}
	set := dto.toSet()
	if err := set.Validate(); err != nil {
		return nil, err
	}
	return set, nil
}
