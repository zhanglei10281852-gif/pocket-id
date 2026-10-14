package cmds

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lestrrat-go/jwx/v4/jwa"
	"github.com/spf13/cobra"
	"gorm.io/gorm"

	"github.com/pocket-id/pocket-id/backend/internal/bootstrap"
	"github.com/pocket-id/pocket-id/backend/internal/common"
	"github.com/pocket-id/pocket-id/backend/internal/instanceid"
	"github.com/pocket-id/pocket-id/backend/internal/model"
	"github.com/pocket-id/pocket-id/backend/internal/utils"
	jwkutils "github.com/pocket-id/pocket-id/backend/internal/utils/jwk"
)

type keyRotateFlags struct {
	Alg           string
	Crv           string
	SessionKey    bool
	RetentionDays int
	Yes           bool
}

func init() {
	var flags keyRotateFlags

	keyRotateCmd := &cobra.Command{
		Use:   "key-rotate",
		Short: "Generates a new signing key and replaces the current one",
		RunE: func(cmd *cobra.Command, args []string) error {
			// The session key is always a symmetric HS256 key, so the algorithm and retention flags don't apply to it
		if flags.SessionKey && (cmd.Flags().Changed("alg") || cmd.Flags().Changed("crv") || cmd.Flags().Changed("retention-days")) {
			return errors.New("the --alg, --crv and --retention-days flags cannot be used together with --session-key")
		}

			db, _, err := bootstrap.NewDatabase(cmd.Context())
			if err != nil {
				return err
			}

			instanceID, err := instanceid.Load(cmd.Context(), db)
			if err != nil {
				return err
			}

			return keyRotate(cmd.Context(), flags, db, instanceID, &common.EnvConfig)
		},
	}

	keyRotateCmd.Flags().StringVarP(&flags.Alg, "alg", "a", "RS256", "Key algorithm. Supported values: RS256, RS384, RS512, ES256, ES384, ES512, EdDSA")
	keyRotateCmd.Flags().StringVarP(&flags.Crv, "crv", "c", "", "Curve name when using EdDSA keys. Supported values: Ed25519")
	keyRotateCmd.Flags().IntVar(&flags.RetentionDays, "retention-days", 0, fmt.Sprintf("Days the previous key stays published for verification after rotation (default: %d, the maximum token lifetime)", model.MaxTokenDurationMinutes/(24*60)))
	keyRotateCmd.Flags().BoolVar(&flags.SessionKey, "session-key", false, "Rotate the key used to sign session tokens instead of the token signing key")
	keyRotateCmd.Flags().BoolVarP(&flags.Yes, "yes", "y", false, "Do not prompt for confirmation")

	rootCmd.AddCommand(keyRotateCmd)
}

func keyRotate(ctx context.Context, flags keyRotateFlags, db *gorm.DB, instanceID string, envConfig *common.EnvConfigSchema) error {
	// The session key is a separate key, generated with a fixed algorithm, so it's rotated on its own
	if flags.SessionKey {
		return sessionKeyRotate(ctx, flags, db, instanceID, envConfig)
	}

	// Validate the flags
	switch strings.ToUpper(flags.Alg) {
	case jwa.RS256().String(), jwa.RS384().String(), jwa.RS512().String(),
		jwa.ES256().String(), jwa.ES384().String(), jwa.ES512().String():
		// All good, but uppercase it for consistency
		flags.Alg = strings.ToUpper(flags.Alg)
	case strings.ToUpper(jwa.EdDSA().String()):
		// Ensure Crv is set and valid
		switch strings.ToUpper(flags.Crv) {
		case strings.ToUpper(jwa.Ed25519().String()):
			// All good, but ensure consistency in casing
			flags.Crv = jwa.Ed25519().String()
		case "":
			return errors.New("a curve name is required when algorithm is EdDSA")
		default:
			return errors.New("unsupported EdDSA curve; supported values: Ed25519")
		}
	case "":
		return errors.New("key algorithm is required")
	default:
		return errors.New("unsupported key algorithm; supported values: RS256, RS384, RS512, ES256, ES384, ES512, EdDSA")
	}

	if flags.RetentionDays < 0 {
		return errors.New("retention days must be a positive number")
	}

	// Zero means "use the default", which covers the longest configurable OIDC token lifetime
	retention := time.Duration(flags.RetentionDays) * 24 * time.Hour
	if flags.RetentionDays > 0 && retention < jwkutils.DefaultRetiredSigningKeyRetention() {
		fmt.Printf("WARNING: a retention of %d day(s) is shorter than the maximum token lifetime of %d day(s); tokens issued with the previous key may fail verification before they expire.\n", flags.RetentionDays, model.MaxTokenDurationMinutes/(24*60))
	}

	if !flags.Yes {
		fmt.Printf("A new signing key becomes active immediately. Tokens issued with the previous key remain valid for %d day(s), and running instances pick up the new key automatically.\n", int(effectiveRetention(flags).Hours()/24))
		ok, err := utils.PromptForConfirmation("Confirm")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("Aborted")
			os.Exit(1)
		}
	}

	// Rotation persists the new generation, retires the previous one and switches the active kid in one atomic operation
	result, err := jwkutils.RotateSigningKeys(ctx, db, envConfig, instanceID, flags.Alg, flags.Crv, retention, time.Now())
	if err != nil {
		if errors.Is(err, jwkutils.ErrSigningKeySetConflict) {
			return errors.New("another key rotation changed the signing keys at the same time; no key was rotated, please retry")
		}
		return fmt.Errorf("failed to rotate signing key: %w", err)
	}

	fmt.Println("Key rotated successfully")
	fmt.Printf("New active key id: %s (%s)\n", result.NewActiveKid, flags.Alg)
	if result.RetiredKid != "" {
		fmt.Printf("Previous key %q remains published for verification for %d day(s)\n", result.RetiredKid, int(effectiveRetention(flags).Hours()/24))
	}
	fmt.Println("Running pocket-id instances load the new key automatically; client applications do not need to restart")

	return nil
}

// effectiveRetention returns the requested compatibility window or the default that covers every configurable token lifetime
func effectiveRetention(flags keyRotateFlags) time.Duration {
	if flags.RetentionDays > 0 {
		return time.Duration(flags.RetentionDays) * 24 * time.Hour
	}
	return jwkutils.DefaultRetiredSigningKeyRetention()
}

func sessionKeyRotate(ctx context.Context, flags keyRotateFlags, db *gorm.DB, instanceID string, envConfig *common.EnvConfigSchema) error {
	if !flags.Yes {
		fmt.Println("WARNING: Rotating the session key will invalidate all existing sessions, and all users will need to sign in again. Tokens issued to client applications are not affected.")
		ok, err := utils.PromptForConfirmation("Confirm")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("Aborted")
			os.Exit(1)
		}
	}

	// Get the key provider for the session key
	keyProvider, err := jwkutils.GetSessionKeyProvider(db, envConfig, instanceID)
	if err != nil {
		return fmt.Errorf("failed to get session key provider: %w", err)
	}

	// Generate a new key
	key, err := jwkutils.GenerateSessionKey()
	if err != nil {
		return fmt.Errorf("failed to generate session key: %w", err)
	}

	// Save the key
	err = keyProvider.ReplaceKey(ctx, key)
	if err != nil {
		return fmt.Errorf("failed to store new session key: %w", err)
	}

	fmt.Println("Session key rotated successfully")
	fmt.Println("Note: if pocket-id is running, you will need to restart it for the new key to be loaded")

	return nil
}
