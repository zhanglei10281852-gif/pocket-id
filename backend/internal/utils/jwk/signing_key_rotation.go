package jwk

// High-level operations on the persistent OIDC signing key set: first-run creation, legacy adoption, atomic rotation, pruning and KEK re-encryption

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	jwxjwk "github.com/lestrrat-go/jwx/v4/jwk"
	"gorm.io/gorm"

	"github.com/pocket-id/pocket-id/backend/internal/common"
)

// LoadedSigningKeySet is a signing key set together with the decrypted private key of every generation
type LoadedSigningKeySet struct {
	Set  *SigningKeySet
	Keys map[string]jwxjwk.Key
}

// ActiveKey returns the decrypted private key of the active generation
func (l *LoadedSigningKeySet) ActiveKey() (jwxjwk.Key, *SigningKeyGeneration) {
	gen, _ := l.Set.Active()
	if gen == nil {
		return nil, nil
	}
	return l.Keys[gen.Kid], gen
}

// EnsureSigningKeySet loads the persistent set, creating it on first startup
// It adopts the pre-generations single key row when present, otherwise generates the default RS256 key
// A lost initialization race returns ErrRetryKeyStorage, exactly like the previous single-key flow
func EnsureSigningKeySet(ctx context.Context, db *gorm.DB, envConfig *common.EnvConfigSchema, instanceID string, now time.Time) (*LoadedSigningKeySet, error) {
	store, err := GetSigningKeySetStore(db, envConfig, instanceID)
	if err != nil {
		return nil, err
	}

	// Fast path: the set already exists, so just load and decrypt every generation
	loaded, err := loadSetWithKeys(ctx, store)
	if err == nil {
		return loaded, nil
	}
	if !errors.Is(err, ErrSigningKeySetNotFound) {
		return nil, err
	}

	// First run with the new schema: adopt the legacy single key so existing tokens keep their kid
	legacyKey, err := store.LoadLegacyKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load legacy signing key: %w", err)
	}
	if legacyKey != nil {
		set, err := newSetFromKey(legacyKey, store, now)
		if err != nil {
			return nil, err
		}
		inserted, err := store.CreateSetAndDeleteLegacy(ctx, set)
		if err != nil {
			return nil, fmt.Errorf("failed to adopt legacy signing key: %w", err)
		}
		if !inserted {
			// Another instance adopted or created the set first; reload its version
			return nil, ErrRetryKeyStorage
		}
		return loadedSetFromSet(set, store)
	}

	// Brand-new deployment: generate the default RS256 key
	key, err := GenerateKey("RS256", "")
	if err != nil {
		return nil, fmt.Errorf("failed to generate new signing key: %w", err)
	}
	set, err := newSetFromKey(key, store, now)
	if err != nil {
		return nil, err
	}
	inserted, err := store.CreateSet(ctx, set)
	if err != nil {
		return nil, fmt.Errorf("failed to store new signing key set: %w", err)
	}
	if !inserted {
		// Another instance generated the first key concurrently; reload the winning one
		return nil, ErrRetryKeyStorage
	}
	return loadedSetFromSet(set, store)
}

// RotationResult reports what a rotation did
type RotationResult struct {
	// NewActiveKid is the kid of the generation that now signs new tokens
	NewActiveKid string
	// RetiredKid is the kid that was retired by the rotation, or empty when a fresh set was bootstrapped
	RetiredKid string
}

// RotateSigningKeys atomically makes a freshly generated key the active generation
// The previous active generation becomes retired and stays verifiable until the retention window elapses
// A concurrent change to the set makes the rotation fail with ErrSigningKeySetConflict instead of risking two active generations
func RotateSigningKeys(ctx context.Context, db *gorm.DB, envConfig *common.EnvConfigSchema, instanceID, alg, crv string, retention time.Duration, now time.Time) (RotationResult, error) {
	if retention <= 0 {
		retention = DefaultRetiredSigningKeyRetention()
	}

	// Generate the new key before touching the database so generation failures leave no trace
	newKey, err := GenerateKey(alg, crv)
	if err != nil {
		return RotationResult{}, fmt.Errorf("failed to generate new signing key: %w", err)
	}

	store, err := GetSigningKeySetStore(db, envConfig, instanceID)
	if err != nil {
		return RotationResult{}, err
	}

	var result RotationResult
	for attempt := 0; ; attempt++ {
		err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			txStore := store.WithDB(tx)
			set, rawValue, err := txStore.LoadSet(ctx)
			if errors.Is(err, ErrSigningKeySetNotFound) {
				// Rotation on a never-initialized database bootstraps the set with the new key as its first active generation
				result, err = bootstrapSetDuringRotation(ctx, txStore, newKey, now)
				return err
			}
			if err != nil {
				return err
			}

			active, _ := set.Active()
			if active == nil {
				return ErrSigningKeySetInvalid
			}

			// Drop generations whose compatibility window has already elapsed; this only removes keys no valid token can reference
			kept := make([]SigningKeyGeneration, 0, len(set.Generations))
			for _, gen := range set.Generations {
				if gen.Expired(now) {
					continue
				}
				kept = append(kept, gen)
			}
			set.Generations = kept

			// Locate the active generation again, since pruning may have shifted its index
			active, activeIdx := set.Active()
			if active == nil {
				return ErrSigningKeySetInvalid
			}

			// Retire the current active generation with the requested compatibility window
			retiredAt := now.UTC()
			retainUntil := now.Add(retention).UTC()
			set.Generations[activeIdx].RetiredAt = &retiredAt
			set.Generations[activeIdx].RetainUntil = &retainUntil

			// Add the new generation as the single active one
			generation, err := txStore.EncryptKey(newKey)
			if err != nil {
				return err
			}
			kid, _ := newKey.KeyID()
			set.Generations = append(set.Generations, SigningKeyGeneration{
				Kid:          kid,
				Alg:          alg,
				EncryptedJWK: generation,
				CreatedAt:    now.UTC(),
			})
			set.ActiveKid = kid
			set.Version++

			swapped, err := txStore.CompareAndSwapSet(ctx, rawValue, set)
			if err != nil {
				return err
			}
			if !swapped {
				// Refuse to overwrite a concurrent rotation; the operator can rerun the command
				return ErrSigningKeySetConflict
			}

			result = RotationResult{NewActiveKid: kid, RetiredKid: active.Kid}
			return nil
		})
		if err == nil {
			return result, nil
		}
		if errors.Is(err, ErrSigningKeySetConflict) {
			return RotationResult{}, err
		}
		// Transient SQLite write locks can occur when two rotation commands start at the exact same time
		if attempt < 4 && isDatabaseLockedError(err) {
			if err := sleepWithContext(ctx, 100*time.Millisecond); err != nil {
				return RotationResult{}, err
			}
			continue
		}
		return RotationResult{}, fmt.Errorf("failed to rotate signing keys: %w", err)
	}
}

// bootstrapSetDuringRotation creates the first set when no key exists yet
// A legacy single key is adopted as the (immediately retired) first generation, so tokens it signed stay verifiable
func bootstrapSetDuringRotation(ctx context.Context, store *SigningKeySetStore, newKey jwxjwk.Key, now time.Time) (RotationResult, error) {
	legacyKey, err := store.LoadLegacyKey(ctx)
	if err != nil {
		return RotationResult{}, fmt.Errorf("failed to load legacy signing key: %w", err)
	}

	var generations []SigningKeyGeneration
	result := RotationResult{}
	if legacyKey != nil {
		legacyGen, err := store.EncryptKey(legacyKey)
		if err != nil {
			return RotationResult{}, err
		}
		legacyKid, _ := legacyKey.KeyID()
		legacyAlg, _ := legacyKey.Algorithm()
		retiredAt := now.UTC()
		generations = append(generations, SigningKeyGeneration{
			Kid:          legacyKid,
			Alg:          legacyAlg.String(),
			EncryptedJWK: legacyGen,
			CreatedAt:    now.UTC(),
			RetiredAt:    &retiredAt,
			RetainUntil:  &retiredAt,
		})
		result.RetiredKid = legacyKid
	}

	newEncrypted, err := store.EncryptKey(newKey)
	if err != nil {
		return RotationResult{}, err
	}
	newKid, _ := newKey.KeyID()
	newAlg, _ := newKey.Algorithm()
	generations = append(generations, SigningKeyGeneration{
		Kid:          newKid,
		Alg:          newAlg.String(),
		EncryptedJWK: newEncrypted,
		CreatedAt:    now.UTC(),
	})
	set := &SigningKeySet{
		Version:     signingKeySetVersion,
		ActiveKid:   newKid,
		Generations: generations,
	}

	inserted, err := store.CreateSetAndDeleteLegacy(ctx, set)
	if err != nil {
		return RotationResult{}, err
	}
	if !inserted {
		return RotationResult{}, ErrSigningKeySetConflict
	}
	result.NewActiveKid = newKid
	return result, nil
}

// PruneRetiredSigningKeys deletes retired generations once their retain-until deadline has passed
// The active generation is never removed, and a failed update leaves the set untouched so a later run can retry
func PruneRetiredSigningKeys(ctx context.Context, db *gorm.DB, envConfig *common.EnvConfigSchema, instanceID string, now time.Time) (removedKids []string, err error) {
	store, err := GetSigningKeySetStore(db, envConfig, instanceID)
	if err != nil {
		return nil, err
	}

	for attempt := 0; ; attempt++ {
		removedKids = nil
		err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			txStore := store.WithDB(tx)
			set, rawValue, err := txStore.LoadSet(ctx)
			if errors.Is(err, ErrSigningKeySetNotFound) {
				// Nothing to prune before the set exists
				return nil
			}
			if err != nil {
				return err
			}

			kept := make([]SigningKeyGeneration, 0, len(set.Generations))
			for _, gen := range set.Generations {
				if gen.Expired(now) {
					removedKids = append(removedKids, gen.Kid)
					continue
				}
				kept = append(kept, gen)
			}
			if len(removedKids) == 0 {
				return nil
			}
			set.Generations = kept
			set.Version++

			swapped, err := txStore.CompareAndSwapSet(ctx, rawValue, set)
			if err != nil {
				return err
			}
			if !swapped {
				// A rotation or another pruner changed the set; retry from the fresh row
				return ErrSigningKeySetConflict
			}
			return nil
		})
		if err == nil {
			return removedKids, nil
		}
		if errors.Is(err, ErrSigningKeySetNotFound) {
			return nil, nil
		}
		if errors.Is(err, ErrSigningKeySetConflict) {
			// A conflict here means the read set was stale; reloading and retrying cannot remove a wrong generation
			if attempt < 4 {
				if err := sleepWithContext(ctx, 100*time.Millisecond); err != nil {
					return nil, err
				}
				continue
			}
			return nil, err
		}
		if attempt < 4 && isDatabaseLockedError(err) {
			if err := sleepWithContext(ctx, 100*time.Millisecond); err != nil {
				return nil, err
			}
			continue
		}
		return nil, fmt.Errorf("failed to prune retired signing keys: %w", err)
	}
}

// ReEncryptSigningKeySet re-wraps every generation's private JWK with a new KEK, preserving active generation and deadlines
// A missing set is not an error, since the encryption-key-rotate command also runs on deployments still using the legacy row
func ReEncryptSigningKeySet(ctx context.Context, db *gorm.DB, oldKek, newKek []byte) error {
	if len(oldKek) == 0 || len(newKek) == 0 {
		return errors.New("both the old and new encryption keys are required to re-encrypt the signing key set")
	}

	oldStore := &SigningKeySetStore{db: db, kek: oldKek}
	set, rawValue, err := oldStore.LoadSet(ctx)
	if errors.Is(err, ErrSigningKeySetNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to load signing key set with old encryption key: %w", err)
	}

	newStore := &SigningKeySetStore{db: db, kek: newKek}
	for i := range set.Generations {
		key, err := oldStore.DecryptKey(set.Generations[i])
		if err != nil {
			return fmt.Errorf("failed to decrypt generation %q: %w", set.Generations[i].Kid, err)
		}
		encrypted, err := newStore.EncryptKey(key)
		if err != nil {
			return fmt.Errorf("failed to re-encrypt generation %q: %w", set.Generations[i].Kid, err)
		}
		set.Generations[i].EncryptedJWK = encrypted
	}
	set.Version++

	swapped, err := newStore.CompareAndSwapSet(ctx, rawValue, set)
	if err != nil {
		return err
	}
	if !swapped {
		return errors.New("the signing key set changed while re-encrypting it, please retry the encryption-key-rotate command")
	}
	return nil
}

// LoadSigningKeySet reads the set and decrypts every generation, used by running instances that refresh their signer
func LoadSigningKeySet(ctx context.Context, db *gorm.DB, envConfig *common.EnvConfigSchema, instanceID string) (*LoadedSigningKeySet, error) {
	store, err := GetSigningKeySetStore(db, envConfig, instanceID)
	if err != nil {
		return nil, err
	}
	return loadSetWithKeys(ctx, store)
}

// newSetFromKey builds the first document around an existing or freshly generated private key
func newSetFromKey(key jwxjwk.Key, store *SigningKeySetStore, now time.Time) (*SigningKeySet, error) {
	encrypted, err := store.EncryptKey(key)
	if err != nil {
		return nil, err
	}
	kid, _ := key.KeyID()
	alg, _ := key.Algorithm()
	return &SigningKeySet{
		Version: signingKeySetVersion,
		ActiveKid: kid,
		Generations: []SigningKeyGeneration{{
			Kid:          kid,
			Alg:          alg.String(),
			EncryptedJWK: encrypted,
			CreatedAt:    now.UTC(),
		}},
	}, nil
}

func loadSetWithKeys(ctx context.Context, store *SigningKeySetStore) (*LoadedSigningKeySet, error) {
	set, _, err := store.LoadSet(ctx)
	if err != nil {
		return nil, err
	}
	return loadedSetFromSet(set, store)
}

func loadedSetFromSet(set *SigningKeySet, store *SigningKeySetStore) (*LoadedSigningKeySet, error) {
	keys := make(map[string]jwxjwk.Key, len(set.Generations))
	for _, gen := range set.Generations {
		key, err := store.DecryptKey(gen)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt signing key generation %q: %w", gen.Kid, err)
		}
		keys[gen.Kid] = key
	}
	if _, ok := keys[set.ActiveKid]; !ok {
		return nil, fmt.Errorf("%w: private key for active kid %q is missing", ErrSigningKeySetInvalid, set.ActiveKid)
	}
	return &LoadedSigningKeySet{Set: set, Keys: keys}, nil
}

// isDatabaseLockedError recognizes SQLite's busy/locked errors, which are safe to retry
func isDatabaseLockedError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") || strings.Contains(message, "database table is locked") || strings.Contains(message, "database is busy")
}

// sleepWithContext waits for the delay unless the context is canceled
func sleepWithContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
