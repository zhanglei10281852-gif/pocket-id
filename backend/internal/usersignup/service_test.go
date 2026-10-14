package usersignup

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/italypaleale/francis/actor"
	"github.com/italypaleale/francis/host/local"
	"github.com/italypaleale/francis/protocol"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pocket-id/pocket-id/backend/internal/appconfig"
	"github.com/pocket-id/pocket-id/backend/internal/apperror"
	"github.com/pocket-id/pocket-id/backend/internal/auditlogs"
	"github.com/pocket-id/pocket-id/backend/internal/dto"
	"github.com/pocket-id/pocket-id/backend/internal/model"
	"github.com/pocket-id/pocket-id/backend/internal/utils"
	testutils "github.com/pocket-id/pocket-id/backend/internal/utils/testing"
)

type fakeUserCreator struct {
	err  error
	user model.User
}

func (f fakeUserCreator) CreateUserInternal(_ context.Context, _ *appconfig.AppConfigModel, _ dto.UserCreateDto, _ bool, _ *gorm.DB) (model.User, error) {
	if f.err != nil {
		return model.User{}, f.err
	}
	return f.user, nil
}

// dbUserCreator persists the user (with the attempt ID as primary key) inside the transaction, so tests exercise the real uniqueness constraints
type dbUserCreator struct{}

func (dbUserCreator) CreateUserInternal(ctx context.Context, _ *appconfig.AppConfigModel, input dto.UserCreateDto, _ bool, tx *gorm.DB) (model.User, error) {
	user := model.User{
		Base: model.Base{
			ID: input.ID,
		},
		Username:    input.Username,
		Email:       input.Email,
		FirstName:   input.FirstName,
		LastName:    input.LastName,
		DisplayName: input.DisplayName,
	}
	if err := tx.WithContext(ctx).Create(&user).Error; err != nil {
		return model.User{}, err
	}
	return user, nil
}

type fakeSigner struct{}

func (fakeSigner) GenerateAccessToken(_ model.User, _ string, _ time.Duration) (string, error) {
	return "access-token", nil
}

type fakeAuditLogger struct{}

func (fakeAuditLogger) Create(_ context.Context, _ auditlogs.Event, _, _, _ string, _ auditlogs.Data, _ *gorm.DB) (auditlogs.AuditLog, bool) {
	return auditlogs.AuditLog{}, true
}

// lazyAttemptReconciler resolves the service only once an actor activation happens, breaking the factory/service construction cycle
type lazyAttemptReconciler struct {
	svc **Service
}

func (l lazyAttemptReconciler) UserExistsForAttempt(ctx context.Context, attemptID string) (bool, error) {
	s := *l.svc
	if s == nil {
		return false, errors.New("signup service not initialized yet")
	}
	return s.UserExistsForAttempt(ctx, attemptID)
}

func (l lazyAttemptReconciler) AttemptConfirmed(ctx context.Context, attemptID string) {
	if s := *l.svc; s != nil {
		s.AttemptConfirmed(ctx, attemptID)
	}
}

func newSignupServiceForTest(t *testing.T, db *gorm.DB, userCreator UserCreator) *Service {
	t.Helper()
	var (
		svc      *Service
		actorSvc *actor.Service
	)
	testutils.NewActorHostForTest(t, func(t *testing.T, h *local.Host) {
		// Register the actor with the reconciler so recovery jobs behave like in production
		err := h.RegisterActor(SignupTokenActorType,
			func(actorID string, actorService *actor.Service) actor.Actor {
				return NewSignupTokenActorWithReconciler(actorID, actorService, lazyAttemptReconciler{svc: &svc})
			},
			local.WithMaxAttempts(attemptReconcileMaxAttempts),
		)
		require.NoError(t, err)
		actorSvc = h.Service()
	})

	svc = newService(Dependencies{
		DB:          db,
		UserCreator: userCreator,
		Signer:      fakeSigner{},
		AuditLog:    fakeAuditLogger{},
	}, actorSvc)
	return svc
}

func signupTokenUsageCount(t *testing.T, svc *Service, tokenID string) int {
	t.Helper()
	tokens, _, err := svc.ListSignupTokens(t.Context(), listAllOptions())
	require.NoError(t, err)
	for _, tok := range tokens {
		if tok.ID == tokenID {
			return tok.UsageCount
		}
	}
	t.Fatalf("signup token %q not found", tokenID)
	return 0
}

// signupTokenStateForTest reads the raw actor state of a token by its value
func signupTokenStateForTest(t *testing.T, svc *Service, token string) SignupTokenState {
	t.Helper()
	var state SignupTokenState
	require.NoError(t, svc.actorService.GetState(t.Context(), SignupTokenActorType, token, &state))
	return state
}

// countUsersForTest counts the users currently stored in the database
func countUsersForTest(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var count int64
	require.NoError(t, db.WithContext(t.Context()).Model(&model.User{}).Count(&count).Error)
	return count
}

func TestSignUpConsumesTokenOnSuccess(t *testing.T) {
	db := testutils.NewDatabaseForTest(t)
	svc := newSignupServiceForTest(t, db, fakeUserCreator{user: model.User{Base: model.Base{ID: "new-user"}}})

	token, err := svc.CreateSignupToken(t.Context(), time.Hour, 2, nil)
	require.NoError(t, err)

	config := appconfig.NewTestConfig(nil)
	user, accessToken, err := svc.SignUp(t.Context(), config, signUpDto{
		Username:  "newuser",
		Token:     token.Token,
		AttemptID: uuid.NewV4().String(),
	}, "1.2.3.4", "test-agent")
	require.NoError(t, err)
	require.Equal(t, "new-user", user.ID)
	require.Equal(t, "access-token", accessToken)

	// The token's usage count must have been incremented and not rolled back
	require.Equal(t, 1, signupTokenUsageCount(t, svc, token.ID))
}

func TestSignUpCompensatesTokenOnFailure(t *testing.T) {
	db := testutils.NewDatabaseForTest(t)
	boom := errors.New("could not create user")
	svc := newSignupServiceForTest(t, db, fakeUserCreator{err: boom})

	token, err := svc.CreateSignupToken(t.Context(), time.Hour, 2, nil)
	require.NoError(t, err)

	config := appconfig.NewTestConfig(nil)
	_, _, err = svc.SignUp(t.Context(), config, signUpDto{
		Username:  "newuser",
		Token:     token.Token,
		AttemptID: uuid.NewV4().String(),
	}, "1.2.3.4", "test-agent")
	require.ErrorIs(t, err, boom)

	// The usage count increment must have been compensated (reverted back to 0) by releasing this attempt
	require.Equal(t, 0, signupTokenUsageCount(t, svc, token.ID))
}

// TestSignUpReplayReturnsExistingUser verifies a retried request with the same attempt ID never creates a second user
func TestSignUpReplayReturnsExistingUser(t *testing.T) {
	db := testutils.NewDatabaseForTest(t)
	svc := newSignupServiceForTest(t, db, dbUserCreator{})

	token, err := svc.CreateSignupToken(t.Context(), time.Hour, 1, nil)
	require.NoError(t, err)

	attemptID := uuid.NewV4().String()
	input := signUpDto{
		Username:  "replayuser",
		Token:     token.Token,
		AttemptID: attemptID,
	}
	config := appconfig.NewTestConfig(nil)

	user1, accessToken1, err := svc.SignUp(t.Context(), config, input, "1.2.3.4", "test-agent")
	require.NoError(t, err)
	require.Equal(t, attemptID, user1.ID)
	require.Equal(t, "access-token", accessToken1)

	// Replaying the exact same request (for example after a lost response) returns the existing user with a fresh session
	user2, accessToken2, err := svc.SignUp(t.Context(), config, input, "1.2.3.4", "test-agent")
	require.NoError(t, err)
	require.Equal(t, user1.ID, user2.ID)
	require.Equal(t, "access-token", accessToken2)

	// Exactly one user exists and only one quota slot was ever consumed
	require.Equal(t, int64(1), countUsersForTest(t, db))
	require.Equal(t, 1, signupTokenUsageCount(t, svc, token.ID))
}

// TestSignUpReplayWithMismatchedProfileRejected verifies a replay that knows an attempt ID but not its profile cannot claim the account
func TestSignUpReplayWithMismatchedProfileRejected(t *testing.T) {
	db := testutils.NewDatabaseForTest(t)
	svc := newSignupServiceForTest(t, db, dbUserCreator{})

	token, err := svc.CreateSignupToken(t.Context(), time.Hour, 1, nil)
	require.NoError(t, err)

	attemptID := uuid.NewV4().String()
	config := appconfig.NewTestConfig(nil)

	_, _, err = svc.SignUp(t.Context(), config, signUpDto{
		Username:  "real-owner",
		Token:     token.Token,
		AttemptID: attemptID,
	}, "1.2.3.4", "test-agent")
	require.NoError(t, err)

	// Reusing the attempt ID with a different profile is a conflict, not a session for the existing user
	_, _, err = svc.SignUp(t.Context(), config, signUpDto{
		Username:  "intruder",
		Token:     token.Token,
		AttemptID: attemptID,
	}, "1.2.3.4", "test-agent")
	require.True(t, apperror.IsCode(err, apperror.CodeAlreadyInUse))
	require.Equal(t, int64(1), countUsersForTest(t, db))
	require.Equal(t, 1, signupTokenUsageCount(t, svc, token.ID))
}

// TestSignUpFailureReleasesOnlyOwnAttempt verifies compensating one failed attempt never frees a concurrent attempt's slot
func TestSignUpFailureReleasesOnlyOwnAttempt(t *testing.T) {
	db := testutils.NewDatabaseForTest(t)
	boom := errors.New("could not create user")
	svc := newSignupServiceForTest(t, db, fakeUserCreator{err: boom})

	token, err := svc.CreateSignupToken(t.Context(), time.Hour, 3, nil)
	require.NoError(t, err)

	// Two signups are in flight concurrently
	_, err = svc.actorService.Invoke(t.Context(), SignupTokenActorType, token.Token, signupTokenMethodConsume, signupTokenConsumeRequest{
		AttemptID: "attempt-a",
	})
	require.NoError(t, err)
	_, err = svc.actorService.Invoke(t.Context(), SignupTokenActorType, token.Token, signupTokenMethodConsume, signupTokenConsumeRequest{
		AttemptID: "attempt-b",
	})
	require.NoError(t, err)
	require.Equal(t, 2, signupTokenStateForTest(t, svc, token.Token).UsageCount)

	// Signup A fails: only its slot is returned, B keeps occupying one
	_, _, err = svc.SignUp(t.Context(), appconfig.NewTestConfig(nil), signUpDto{
		Username:  "newuser-a",
		Token:     token.Token,
		AttemptID: "attempt-a",
	}, "1.2.3.4", "test-agent")
	require.ErrorIs(t, err, boom)

	state := signupTokenStateForTest(t, svc, token.Token)
	require.Equal(t, 1, state.UsageCount)
	require.Len(t, state.Attempts, 1)
	require.Equal(t, "attempt-b", state.Attempts[0].ID)
	require.Equal(t, signupAttemptPending, state.Attempts[0].Status)
}

// TestSignUpUnknownCommitOutcomeConfirmsUser verifies an error with a committed user completes the attempt instead of creating a duplicate
func TestSignUpUnknownCommitOutcomeConfirmsUser(t *testing.T) {
	db := testutils.NewDatabaseForTest(t)
	svc := newSignupServiceForTest(t, db, fakeUserCreator{err: errors.New("commit result unknown")})

	token, err := svc.CreateSignupToken(t.Context(), time.Hour, 1, nil)
	require.NoError(t, err)

	attemptID := uuid.NewV4().String()

	// The actor accepted the attempt
	res, err := svc.actorService.Invoke(t.Context(), SignupTokenActorType, token.Token, signupTokenMethodConsume, signupTokenConsumeRequest{
		AttemptID: attemptID,
	})
	require.NoError(t, err)
	var consumeRes signupTokenConsumeResponse
	require.NoError(t, res.Decode(&consumeRes))
	require.Equal(t, signupTokenConsumeOK, consumeRes.Status)

	// The user actually committed before the failure (uncertain commit result)
	require.NoError(t, db.Create(&model.User{
		Base:     model.Base{ID: attemptID},
		Username: "committed-user",
	}).Error)

	cause := errors.New("database connection dropped during commit")
	user, accessToken, err := svc.resolveFailedSignup(t.Context(), appconfig.NewTestConfig(nil), signUpDto{
		Username:  "committed-user",
		Token:     token.Token,
		AttemptID: attemptID,
	}, attemptID, cause)
	require.NoError(t, err)
	require.Equal(t, attemptID, user.ID)
	require.Equal(t, "access-token", accessToken)

	// The attempt is completed, the slot kept, and no duplicate user exists
	state := signupTokenStateForTest(t, svc, token.Token)
	require.Equal(t, 1, state.UsageCount)
	require.Len(t, state.Attempts, 1)
	require.Equal(t, signupAttemptCompleted, state.Attempts[0].Status)
	require.Equal(t, int64(1), countUsersForTest(t, db))
}

// TestSignUpDuplicateProfileReleasesAttempt verifies a profile conflict frees the failed attempt but keeps a concurrent successful one
func TestSignUpDuplicateProfileReleasesAttempt(t *testing.T) {
	db := testutils.NewDatabaseForTest(t)
	svc := newSignupServiceForTest(t, db, dbUserCreator{})

	token, err := svc.CreateSignupToken(t.Context(), time.Hour, 2, nil)
	require.NoError(t, err)
	config := appconfig.NewTestConfig(nil)

	_, _, err = svc.SignUp(t.Context(), config, signUpDto{
		Username:  "duplicateuser",
		Token:     token.Token,
		AttemptID: uuid.NewV4().String(),
	}, "1.2.3.4", "test-agent")
	require.NoError(t, err)

	// A second signup with another attempt ID but the same username conflicts and fails
	_, _, err = svc.SignUp(t.Context(), config, signUpDto{
		Username:  "duplicateuser",
		Token:     token.Token,
		AttemptID: uuid.NewV4().String(),
	}, "1.2.3.4", "test-agent")
	require.Error(t, err)

	// The failed attempt's slot was released, leaving only the committed user's
	require.Equal(t, 1, signupTokenUsageCount(t, svc, token.ID))
	require.Equal(t, int64(1), countUsersForTest(t, db))
}

// TestSignUpConcurrentSameAttemptCreatesOneUser verifies request replay and multi-goroutine concurrency create at most one user
func TestSignUpConcurrentSameAttemptCreatesOneUser(t *testing.T) {
	db := testutils.NewDatabaseForTest(t)
	svc := newSignupServiceForTest(t, db, dbUserCreator{})

	token, err := svc.CreateSignupToken(t.Context(), time.Hour, 1, nil)
	require.NoError(t, err)

	attemptID := uuid.NewV4().String()
	input := signUpDto{
		Username:  "raceuser",
		Token:     token.Token,
		AttemptID: attemptID,
	}
	config := appconfig.NewTestConfig(nil)

	const requestCount = 10
	var wg sync.WaitGroup
	errs := make(chan error, requestCount)
	for range requestCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := svc.SignUp(t.Context(), config, input, "1.2.3.4", "test-agent")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	require.Equal(t, int64(1), countUsersForTest(t, db))
	require.Equal(t, 1, signupTokenUsageCount(t, svc, token.ID))
	state := signupTokenStateForTest(t, svc, token.Token)
	require.Len(t, state.Attempts, 1)
	require.Equal(t, signupAttemptCompleted, state.Attempts[0].Status)
}

// TestRecoverPendingAttempts verifies the startup sweep confirms committed users and releases abandoned slots without touching young attempts
func TestRecoverPendingAttempts(t *testing.T) {
	db := testutils.NewDatabaseForTest(t)
	svc := newSignupServiceForTest(t, db, dbUserCreator{})

	now := time.Now()
	abandonedID := uuid.NewV4().String()
	committedPendingID := uuid.NewV4().String()
	youngPendingID := uuid.NewV4().String()
	youngOnlyID := uuid.NewV4().String()

	state := SignupTokenState{
		ID:         "token-recovery-id",
		ExpiresAt:  now.Add(time.Hour),
		UsageLimit: 3,
		UsageCount: 3,
		CreatedAt:  now,
		Attempts: []signupTokenAttempt{
			{ID: committedPendingID, Status: signupAttemptPending, CreatedAt: now.Add(-10 * time.Minute)},
			{ID: youngPendingID, Status: signupAttemptPending, CreatedAt: now},
			{ID: abandonedID, Status: signupAttemptPending, CreatedAt: now.Add(-10 * time.Minute)},
		},
	}
	_, err := svc.actorService.Invoke(t.Context(), SignupTokenActorType, "token-recovery", SignupTokenMethodCreate, state)
	require.NoError(t, err)

	// Only the old attempt that actually committed a user has a row in the database
	require.NoError(t, db.Create(&model.User{Base: model.Base{ID: committedPendingID}, Username: "committed-pending"}).Error)

	// A second token with a single young, userless attempt must be left untouched for its delayed job
	youngOnlyState := SignupTokenState{
		ID:         "token-young-id",
		ExpiresAt:  now.Add(time.Hour),
		UsageLimit: 1,
		UsageCount: 1,
		CreatedAt:  now,
		Attempts:   []signupTokenAttempt{{ID: youngOnlyID, Status: signupAttemptPending, CreatedAt: now}},
	}
	_, err = svc.actorService.Invoke(t.Context(), SignupTokenActorType, "token-young", SignupTokenMethodCreate, youngOnlyState)
	require.NoError(t, err)

	require.NoError(t, svc.recoverPendingAttempts(t.Context()))

	recovered := signupTokenStateForTest(t, svc, "token-recovery")
	require.Equal(t, 2, recovered.UsageCount)
	require.Equal(t, signupAttemptCompleted, findSignupAttempt(recovered.Attempts, committedPendingID).Status)
	require.NotNil(t, findSignupAttempt(recovered.Attempts, youngPendingID))
	require.Nil(t, findSignupAttempt(recovered.Attempts, abandonedID))

	youngOnly := signupTokenStateForTest(t, svc, "token-young")
	require.Equal(t, 1, youngOnly.UsageCount)
	require.Len(t, youngOnly.Attempts, 1)
	require.Equal(t, signupAttemptPending, youngOnly.Attempts[0].Status)
}

func TestMapSignupActorUnavailable(t *testing.T) {
	require.True(t, apperror.IsCode(mapSignupActorUnavailable(actor.ErrNoHost), apperror.CodeSignupTemporarilyUnavailable))
	require.True(t, apperror.IsCode(mapSignupActorUnavailable(actor.ErrActorHalted), apperror.CodeSignupTemporarilyUnavailable))
	require.True(t, apperror.IsCode(mapSignupActorUnavailable(context.DeadlineExceeded), apperror.CodeSignupTemporarilyUnavailable))
	// A lost response after the actor may have executed is retryable with the same attempt ID
	require.True(t, apperror.IsCode(
		mapSignupActorUnavailable(fmt.Errorf("invoke failed: %w", protocol.NewError(protocol.ErrCodeTransportFailure, "response lost"))),
		apperror.CodeSignupTemporarilyUnavailable,
	))
	require.True(t, apperror.IsCode(mapSignupActorUnavailable(errors.New("boom")), apperror.CodeInternal))
}

func TestSignUpRejectsInvalidToken(t *testing.T) {
	db := testutils.NewDatabaseForTest(t)
	svc := newSignupServiceForTest(t, db, fakeUserCreator{user: model.User{Base: model.Base{ID: "new-user"}}})

	config := appconfig.NewTestConfig(nil)
	_, _, err := svc.SignUp(t.Context(), config, signUpDto{
		Username:  "newuser",
		Token:     "not-a-real-token",
		AttemptID: uuid.NewV4().String(),
	}, "1.2.3.4", "test-agent")

	require.True(t, apperror.IsCode(err, apperror.CodeTokenInvalidOrExpired))
}

func TestSignUpInitialAdminCreatesAdmin(t *testing.T) {
	db := testutils.NewDatabaseForTest(t)
	svc := newSignupServiceForTest(t, db, fakeUserCreator{user: model.User{Base: model.Base{ID: "new-admin"}}})
	config := appconfig.NewTestConfig(nil)

	// Complete setup and return the generated administrator session
	user, accessToken, err := svc.SignUpInitialAdmin(t.Context(), config, signUpDto{Username: "new-admin"})
	require.NoError(t, err)
	require.Equal(t, "new-admin", user.ID)
	require.Equal(t, "access-token", accessToken)
}

func TestSignUpInitialAdminAllowsRetryAfterFailure(t *testing.T) {
	db := testutils.NewDatabaseForTest(t)
	boom := errors.New("could not create initial admin")
	svc := newSignupServiceForTest(t, db, fakeUserCreator{err: boom})
	config := appconfig.NewTestConfig(nil)

	// Fail the first setup transaction before it can commit
	_, _, err := svc.SignUpInitialAdmin(t.Context(), config, signUpDto{Username: "failed-admin"})
	require.ErrorIs(t, err, boom)

	// Confirm a later setup can complete after the failed transaction rolls back
	svc.userCreator = fakeUserCreator{user: model.User{Base: model.Base{ID: "new-admin"}}}
	user, _, err := svc.SignUpInitialAdmin(t.Context(), config, signUpDto{Username: "new-admin"})
	require.NoError(t, err)
	require.Equal(t, "new-admin", user.ID)
}

func TestSignUpInitialAdminRejectsExistingInstallation(t *testing.T) {
	db := testutils.NewDatabaseForTest(t)
	require.NoError(t, db.Create(&model.User{Username: "existing-admin", IsAdmin: true}).Error)
	svc := newSignupServiceForTest(t, db, fakeUserCreator{user: model.User{Base: model.Base{ID: "new-admin"}}})

	// Reject setup when the installation already contains a user
	_, _, err := svc.SignUpInitialAdmin(t.Context(), appconfig.NewTestConfig(nil), signUpDto{Username: "new-admin"})
	require.True(t, apperror.IsCode(err, apperror.CodeSetupAlreadyCompleted))
}

// listAllOptions returns list options that return every token on a single page.
func listAllOptions() utils.ListRequestOptions {
	var opts utils.ListRequestOptions
	opts.Pagination.Page = 1
	opts.Pagination.Limit = 100
	return opts
}
