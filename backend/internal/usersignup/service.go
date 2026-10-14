package usersignup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"uuid"

	"github.com/italypaleale/francis/actor"
	"github.com/italypaleale/francis/protocol"
	"gorm.io/gorm"

	"github.com/pocket-id/pocket-id/backend/internal/appconfig"
	"github.com/pocket-id/pocket-id/backend/internal/apperror"
	"github.com/pocket-id/pocket-id/backend/internal/auditlogs"
	"github.com/pocket-id/pocket-id/backend/internal/common"
	"github.com/pocket-id/pocket-id/backend/internal/dto"
	"github.com/pocket-id/pocket-id/backend/internal/model"
	datatype "github.com/pocket-id/pocket-id/backend/internal/model/types"
	"github.com/pocket-id/pocket-id/backend/internal/utils"
)

// authenticationMethodOneTimePassword identifies one-time-password authentication, used for the initial admin setup token
// It must match the value emitted by the JWT service in the access token's "amr" claim
const authenticationMethodOneTimePassword = "otp"

type Service struct {
	db           *gorm.DB
	actorService *actor.Service
	userCreator  UserCreator
	signer       TokenService
	auditLog     AuditLogger
	scimSync     ScimSyncScheduler
}

func newService(deps Dependencies, actorService *actor.Service) *Service {
	return &Service{
		db:           deps.DB,
		actorService: actorService,
		userCreator:  deps.UserCreator,
		signer:       deps.Signer,
		auditLog:     deps.AuditLog,
		scimSync:     deps.ScimSync,
	}
}

func (s *Service) SignUp(ctx context.Context, config *appconfig.AppConfigModel, signupData signUpDto, ipAddress, userAgent string) (model.User, string, error) {
	tokenProvided := signupData.Token != ""

	if config.AllowUserSignups.String() != "open" && !tokenProvided {
		return model.User{}, "", apperror.OpenSignupDisabled()
	}

	// The attempt ID identifies this registration across retries and doubles as the new user's ID, so every replay lands on the same users row
	attemptID := signupData.AttemptID

	var userGroupIDs []string
	if tokenProvided {
		if attemptID == "" {
			attemptID = uuid.NewV4().String()
		}

		// Consume the signup token by invoking its actor: this atomically validates it and reserves one quota slot for this exact attempt
		// Note: must invoke outside of a DB transaction, since invoking an actor while a transaction is open would deadlock on SQLite
		res, err := s.actorService.Invoke(ctx, SignupTokenActorType, signupData.Token, signupTokenMethodConsume, signupTokenConsumeRequest{
			AttemptID: attemptID,
		})
		if err != nil {
			return model.User{}, "", mapSignupActorUnavailable(fmt.Errorf("error invoking signup token actor: %w", err))
		}

		var consumeRes signupTokenConsumeResponse
		err = res.Decode(&consumeRes)
		if err != nil {
			return model.User{}, "", fmt.Errorf("error decoding signup token actor response: %w", err)
		}

		// not_found (expired or deleted token) and limit_reached share the public response but stay distinct statuses internally
		if consumeRes.Status != signupTokenConsumeOK {
			return model.User{}, "", apperror.TokenInvalidOrExpired()
		}
		userGroupIDs = consumeRes.UserGroupIDs
	}

	userToCreate := dto.UserCreateDto{
		Username:      signupData.Username,
		Email:         signupData.Email,
		FirstName:     signupData.FirstName,
		LastName:      signupData.LastName,
		DisplayName:   strings.TrimSpace(signupData.FirstName + " " + signupData.LastName),
		UserGroupIds:  userGroupIDs,
		EmailVerified: config.EmailsVerified.IsTrue(),
	}
	// The attempt ID only ties token signups to their quota slot; open signups keep server-generated user IDs
	if tokenProvided {
		userToCreate.ID = attemptID
	}

	// A replay whose user was already committed must not create another account: confirm the attempt and return the existing user
	if tokenProvided {
		existing, found, err := s.findCommittedUser(ctx, attemptID)
		if err != nil {
			// With the database unreadable we cannot tell whether a user exists, so leave the attempt pending for durable reconciliation
			return model.User{}, "", err
		}
		if found {
			// Only the request with the attempt's original profile may claim its session, so a guessed attempt ID can't mint a token for someone else
			if conflictField, matches := replayProfileMatch(signupData, existing); !matches {
				s.markAttemptCompleted(ctx, signupData.Token, attemptID)
				return model.User{}, "", apperror.AlreadyInUse(conflictField)
			}
			return s.finishReplayedSignup(ctx, config, signupData.Token, existing)
		}
	}

	// From here on the attempt is pending: create the user, issue the token, and write the audit log in one transaction
	user, accessToken, err := s.createSignedUpUser(ctx, config, userToCreate, signupData.Token, tokenProvided, ipAddress, userAgent)
	if err == nil {
		if tokenProvided {
			s.markAttemptCompleted(ctx, signupData.Token, attemptID)
		}
		return user, accessToken, nil
	}

	if !tokenProvided {
		return model.User{}, "", err
	}

	// The request failed: decide the attempt's fate from what actually landed in the database instead of blindly freeing a slot
	return s.resolveFailedSignup(ctx, config, signupData, attemptID, err)
}

// replayProfileMatch checks whether a replay request carries the same unique profile the attempt created.
// It returns the conflicting field name when the request must not be treated as that attempt's replay.
func replayProfileMatch(signupData signUpDto, user model.User) (string, bool) {
	if !strings.EqualFold(user.Username, signupData.Username) {
		return "username", false
	}

	email := ""
	if signupData.Email != nil {
		email = *signupData.Email
	}
	userEmail := ""
	if user.Email != nil {
		userEmail = *user.Email
	}
	if !strings.EqualFold(userEmail, email) {
		return "email", false
	}

	return "", true
}

// finishReplayedSignup completes an attempt whose user already exists and returns a fresh session for that user.
// The audit log was written by the original committing request, so it isn't written again; the SCIM schedule is idempotent.
func (s *Service) finishReplayedSignup(ctx context.Context, config *appconfig.AppConfigModel, token string, user model.User) (model.User, string, error) {
	s.markAttemptCompleted(ctx, token, user.ID)

	accessToken, err := s.signer.GenerateAccessToken(user, "", config.SessionDuration.AsDurationMinutes())
	if err != nil {
		return model.User{}, "", err
	}

	s.scheduleScimSync(ctx)
	return user, accessToken, nil
}

// resolveFailedSignup reconciles an attempt after its request returned an error.
// A committed user wins: the attempt is completed and the signup is recovered as success.
// Without a user the transaction rolled back, so only this attempt's slot is released.
func (s *Service) resolveFailedSignup(ctx context.Context, config *appconfig.AppConfigModel, signupData signUpDto, attemptID string, cause error) (model.User, string, error) {
	user, found, lookupErr := s.findCommittedUser(ctx, attemptID)
	if lookupErr != nil {
		// Whether the transaction committed is now undecidable: leave the attempt pending and let durable reconciliation resolve it after re-checking the database
		slog.ErrorContext(ctx, "Unable to determine whether the signup transaction committed; leaving the attempt pending for reconciliation",
			slog.String("attemptId", attemptID),
			slog.Any("signupError", cause),
			slog.Any("lookupError", lookupErr))
		return model.User{}, "", cause
	}

	if !found {
		s.releaseSignupAttempt(ctx, signupData.Token, attemptID)
		return model.User{}, "", cause
	}

	// A committed user with a different profile isn't this request's account: keep the attempt completed but don't hand over a session
	if conflictField, matches := replayProfileMatch(signupData, user); !matches {
		s.markAttemptCompleted(ctx, signupData.Token, attemptID)
		return model.User{}, "", apperror.AlreadyInUse(conflictField)
	}

	// The user was committed despite the error (an uncertain commit result, or a lost race with a concurrent replay): confirm rather than duplicate
	s.markAttemptCompleted(ctx, signupData.Token, attemptID)

	accessToken, err := s.signer.GenerateAccessToken(user, "", config.SessionDuration.AsDurationMinutes())
	if err != nil {
		return model.User{}, "", err
	}

	s.scheduleScimSync(ctx)
	return user, accessToken, nil
}

// findCommittedUser looks up a user by the attempt ID outside any signup transaction.
func (s *Service) findCommittedUser(ctx context.Context, attemptID string) (model.User, bool, error) {
	var user model.User
	err := s.db.WithContext(ctx).Where("id = ?", attemptID).First(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return model.User{}, false, nil
	}
	if err != nil {
		return model.User{}, false, fmt.Errorf("error looking up the user of signup attempt '%s': %w", attemptID, err)
	}

	return user, true, nil
}

// createSignedUpUser creates the user and issues an access token within a single transaction.
// It performs no actor calls, so it's safe to keep the transaction open for its whole duration.
func (s *Service) createSignedUpUser(ctx context.Context, config *appconfig.AppConfigModel, userToCreate dto.UserCreateDto, token string, tokenProvided bool, ipAddress, userAgent string) (model.User, string, error) {
	tx := s.db.Begin()
	defer func() {
		tx.Rollback()
	}()

	user, err := s.userCreator.CreateUserInternal(ctx, config, userToCreate, false, tx)
	if err != nil {
		return model.User{}, "", err
	}

	accessToken, err := s.signer.GenerateAccessToken(user, "", config.SessionDuration.AsDurationMinutes())
	if err != nil {
		return model.User{}, "", err
	}

	if tokenProvided {
		s.auditLog.Create(ctx, auditlogs.EventAccountCreated, ipAddress, userAgent, user.ID, auditlogs.Data{
			"signupToken": token,
		}, tx)
	} else {
		s.auditLog.Create(ctx, auditlogs.EventAccountCreated, ipAddress, userAgent, user.ID, auditlogs.Data{
			"method": "open_signup",
		}, tx)
	}

	err = tx.Commit().Error
	if err != nil {
		// A commit error is ambiguous (the transaction may have landed): the caller verifies the outcome against the database
		return model.User{}, "", fmt.Errorf("error committing the new user transaction: %w", err)
	}

	s.scheduleScimSync(ctx)
	return user, accessToken, nil
}

// markAttemptCompleted tells the actor that the attempt's user is committed, so its slot stays occupied.
// It's best-effort: if the actor is unreachable, the durable reconciliation job confirms the attempt after seeing the user.
func (s *Service) markAttemptCompleted(parentCtx context.Context, token, attemptID string) {
	err := s.invokeSignupTokenActor(parentCtx, token, signupTokenMethodComplete, signupTokenAttemptRequest{
		AttemptID: attemptID,
	})
	if err != nil {
		slog.ErrorContext(parentCtx, "Failed to mark a signup attempt completed; reconciliation will confirm it",
			slog.String("attemptId", attemptID),
			slog.Any("error", err))
	}
}

// releaseSignupAttempt frees the slot belonging to one specific attempt, used to compensate when the signup could not commit a user.
// It's best-effort: if the actor is unreachable, the durable reconciliation job releases the attempt once the database shows no user for it.
func (s *Service) releaseSignupAttempt(parentCtx context.Context, token, attemptID string) {
	err := s.invokeSignupTokenActor(parentCtx, token, signupTokenMethodRelease, signupTokenAttemptRequest{
		AttemptID: attemptID,
	})
	if err != nil {
		slog.ErrorContext(parentCtx, "Failed to release a signup attempt after a failed signup; reconciliation will release it",
			slog.String("attemptId", attemptID),
			slog.Any("error", err))
	}
}

// invokeSignupTokenActor invokes one method of a signup token actor with a context that survives the request's cancellation.
func (s *Service) invokeSignupTokenActor(parentCtx context.Context, token, method string, payload any) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parentCtx), 10*time.Second)
	defer cancel()

	_, err := s.actorService.Invoke(ctx, SignupTokenActorType, token, method, payload)
	if err != nil {
		return fmt.Errorf("error invoking signup token actor method '%s': %w", method, err)
	}

	return nil
}

// scheduleScimSync requests a SCIM synchronization after a user change has committed.
// Scheduling replaces a single debounced alarm, so replaying a signup never duplicates the side effect.
func (s *Service) scheduleScimSync(ctx context.Context) {
	if s.scimSync != nil {
		s.scimSync.ScheduleSync(ctx)
	}
}

// UserExistsForAttempt implements AttemptReconciler for recovery jobs
func (s *Service) UserExistsForAttempt(ctx context.Context, attemptID string) (bool, error) {
	_, found, err := s.findCommittedUser(ctx, attemptID)
	return found, err
}

// AttemptConfirmed implements AttemptReconciler and restores the post-commit side effect a crashed request may have missed
func (s *Service) AttemptConfirmed(ctx context.Context, attemptID string) {
	slog.InfoContext(ctx, "Recovered a signup attempt whose user was already committed",
		slog.String("attemptId", attemptID))
	s.scheduleScimSync(ctx)
}

// mapSignupActorUnavailable turns transient actor-routing failures into a distinct, retryable client error while keeping other failures as internal errors.
// Retrying with the same attempt ID is always safe: the actor either never accepted the use or returns the existing attempt.
func mapSignupActorUnavailable(err error) error {
	retryableProtocolCodes := []protocol.ErrorCode{
		protocol.ErrCodeTransportFailure,
		protocol.ErrCodeRetryLater,
		protocol.ErrCodeOverloaded,
		protocol.ErrCodeHostDraining,
		protocol.ErrCodeDeadlineExceeded,
		protocol.ErrCodeNoHost,
		protocol.ErrCodeSessionSuperseded,
	}
	for _, code := range retryableProtocolCodes {
		if errors.Is(err, protocol.NewError(code, "")) {
			return apperror.SignupTemporarilyUnavailable()
		}
	}

	switch {
	case errors.Is(err, actor.ErrNoHost),
		errors.Is(err, actor.ErrActorHalted),
		errors.Is(err, actor.ErrActorNotHosted),
		errors.Is(err, actor.ErrActorTypeUnsupported),
		errors.Is(err, context.DeadlineExceeded):
		return apperror.SignupTemporarilyUnavailable()
	default:
		return apperror.Internal(err)
	}
}

// recoverPendingAttempts reconciles every pending attempt after a restart, covering attempts whose durable job was lost with the process.
// Young attempts are skipped: another replica may still be serving their request, and the delayed job handles them.
func (s *Service) recoverPendingAttempts(ctx context.Context) error {
	entries, err := s.listSignupTokenStates(ctx)
	if err != nil {
		return err
	}

	// Collect every pending attempt so user lookups can run in one batch
	pendingByToken := make(map[string][]signupTokenAttempt)
	pendingIDs := make([]string, 0)
	for _, e := range entries {
		for _, a := range e.State.Attempts {
			if a.Status != signupAttemptPending {
				continue
			}
			pendingByToken[e.Token] = append(pendingByToken[e.Token], a)
			pendingIDs = append(pendingIDs, a.ID)
		}
	}
	if len(pendingIDs) == 0 {
		return nil
	}

	existingUsers, err := s.lookupExistingAttemptUsers(ctx, pendingIDs)
	if err != nil {
		return err
	}

	var recovered, released int
	for token, attempts := range pendingByToken {
		for _, a := range attempts {
			res, err := s.actorService.Invoke(ctx, SignupTokenActorType, token, signupTokenMethodReconcile, signupTokenReconcileRequest{
				AttemptID:  a.ID,
				UserExists: existingUsers[a.ID],
			})
			if err != nil {
				return fmt.Errorf("error reconciling signup attempt '%s' on token '%s': %w", a.ID, token, err)
			}

			var reconcileRes signupTokenReconcileResponse
			err = res.Decode(&reconcileRes)
			if err != nil {
				return fmt.Errorf("error decoding reconciliation response for attempt '%s': %w", a.ID, err)
			}

			switch reconcileRes.Status {
			case signupTokenReconcileCompleted:
				recovered++
				s.AttemptConfirmed(ctx, a.ID)
			case signupTokenReconcileReleased:
				released++
			case signupTokenReconcileDeferred, signupTokenReconcileNoop:
				// Young attempts wait for their delayed job, and resolved attempts need no action
			}
		}
	}

	if recovered > 0 || released > 0 {
		slog.InfoContext(ctx, "Recovered pending signup attempts after startup",
			slog.Int("confirmed", recovered),
			slog.Int("released", released))
	}

	return nil
}

// lookupExistingAttemptUsers returns which of the given attempt IDs already have a committed user.
func (s *Service) lookupExistingAttemptUsers(ctx context.Context, attemptIDs []string) (map[string]bool, error) {
	var foundIDs []string
	err := s.db.WithContext(ctx).
		Model(&model.User{}).
		Where("id IN ?", attemptIDs).
		Pluck("id", &foundIDs).
		Error
	if err != nil {
		return nil, fmt.Errorf("error looking up users for pending signup attempts: %w", err)
	}

	existing := make(map[string]bool, len(foundIDs))
	for _, id := range foundIDs {
		existing[id] = true
	}
	return existing, nil
}

func (s *Service) SignUpInitialAdmin(ctx context.Context, config *appconfig.AppConfigModel, signUpData signUpDto) (model.User, string, error) {
	tx := s.db.Begin()
	defer func() {
		tx.Rollback()
	}()

	// We lock the users table to prevent concurrent initial admin setups from racing to create the first user
	// This is only necessary for Postgres, since SQLite serializes all writes anyway
	if err := lockInitialAdminSetup(ctx, tx); err != nil {
		return model.User{}, "", err
	}

	// Reject setup when a committed user already exists
	setupCompleted, err := s.isInitialAdminSetupCompleted(ctx, tx)
	if err != nil {
		return model.User{}, "", err
	}
	if setupCompleted {
		return model.User{}, "", apperror.SetupAlreadyCompleted()
	}

	// Build the first user with administrator privileges
	userToCreate := dto.UserCreateDto{
		FirstName:   signUpData.FirstName,
		LastName:    signUpData.LastName,
		DisplayName: strings.TrimSpace(signUpData.FirstName + " " + signUpData.LastName),
		Username:    signUpData.Username,
		Email:       signUpData.Email,
		IsAdmin:     true,
	}

	user, err := s.userCreator.CreateUserInternal(ctx, config, userToCreate, false, tx)
	if err != nil {
		return model.User{}, "", err
	}

	// Issue the setup session before committing so failures roll back the transaction
	token, err := s.signer.GenerateAccessToken(user, authenticationMethodOneTimePassword, config.SessionDuration.AsDurationMinutes())
	if err != nil {
		return model.User{}, "", err
	}

	err = tx.Commit().Error
	if err != nil {
		return model.User{}, "", err
	}
	s.scheduleScimSync(ctx)

	return user, token, nil
}

func lockInitialAdminSetup(ctx context.Context, tx *gorm.DB) error {
	if tx.Name() != "postgres" {
		return nil
	}

	if err := tx.WithContext(ctx).Exec("LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE").Error; err != nil {
		return fmt.Errorf("failed to lock users table for initial admin setup: %w", err)
	}

	return nil
}

func (s *Service) IsInitialAdminSetupCompleted(ctx context.Context) (bool, error) {
	return s.isInitialAdminSetupCompleted(ctx, s.db)
}

func (s *Service) isInitialAdminSetupCompleted(ctx context.Context, db *gorm.DB) (bool, error) {
	var userCount int64
	if err := db.WithContext(ctx).Model(&model.User{}).
		Where("id != ?", common.StaticApiKeyUserID).
		Count(&userCount).Error; err != nil {
		return false, err
	}

	return userCount != 0, nil
}

func (s *Service) ListSignupTokens(ctx context.Context, listRequestOptions utils.ListRequestOptions) ([]SignupToken, utils.PaginationResponse, error) {
	// Each signup token is its own actor, so we enumerate the stored states (expired ones are filtered out by the state store), then sort and paginate in memory
	entries, err := s.listSignupTokenStates(ctx)
	if err != nil {
		return nil, utils.PaginationResponse{}, err
	}

	// Resolve the referenced user groups so they can be included in the response
	groupsByID, err := s.loadUserGroupsByID(ctx, entries)
	if err != nil {
		return nil, utils.PaginationResponse{}, err
	}

	tokens := make([]SignupToken, len(entries))
	for i, e := range entries {
		tokens[i] = signupTokenModelFromState(e.Token, e.State, resolveUserGroups(e.State.UserGroupIDs, groupsByID))
	}

	return paginateSignupTokens(tokens, listRequestOptions)
}

func (s *Service) DeleteSignupToken(ctx context.Context, tokenID string) error {
	// Tokens are addressed by their value (the actor ID), while the API deletes them by ID, so we look up the matching token first
	entries, err := s.listSignupTokenStates(ctx)
	if err != nil {
		return err
	}

	for _, e := range entries {
		if e.State.ID != tokenID {
			continue
		}

		_, err = s.actorService.Invoke(ctx, SignupTokenActorType, e.Token, SignupTokenMethodDelete, nil)
		if err != nil {
			return fmt.Errorf("error deleting signup token via actor: %w", err)
		}
		return nil
	}

	// The token doesn't exist (or has expired): deleting it already reaches the desired end state
	return nil
}

// signupTokenEntry pairs a signup token's value (which is its actor ID) with its stored state
type signupTokenEntry struct {
	Token string
	State SignupTokenState
}

// listSignupTokenStates returns every signup token currently stored in the actor state store.
// Expired tokens are not returned, since the state store filters out states whose TTL has passed.
func (s *Service) listSignupTokenStates(ctx context.Context) ([]signupTokenEntry, error) {
	var (
		entries []signupTokenEntry
		after   string
	)
	for {
		res, err := s.actorService.ListStates(ctx, SignupTokenActorType, &actor.ListStatesOpts{
			IncludeData: true,
			After:       after,
		})
		if err != nil {
			return nil, fmt.Errorf("error listing signup token states: %w", err)
		}

		for _, st := range res.States {
			if st.Data == nil {
				continue
			}

			var state SignupTokenState
			err = st.Data.Decode(&state)
			if err != nil {
				return nil, fmt.Errorf("error decoding state of signup token actor '%s': %w", st.ActorID, err)
			}

			entries = append(entries, signupTokenEntry{
				Token: st.ActorID,
				State: state,
			})
		}

		// An empty cursor means we've just read the last page
		after = res.AfterID()
		if after == "" {
			break
		}
	}

	return entries, nil
}

func (s *Service) CreateSignupToken(ctx context.Context, ttl time.Duration, usageLimit int, userGroupIDs []string) (SignupToken, error) {
	// Load the referenced user groups to validate them and to include them in the response
	var userGroups []model.UserGroup
	if len(userGroupIDs) > 0 {
		err := s.db.WithContext(ctx).
			Where("id IN ?", userGroupIDs).
			Find(&userGroups).
			Error
		if err != nil {
			return SignupToken{}, err
		}
	}

	validGroupIDs := make([]string, len(userGroups))
	for i, g := range userGroups {
		validGroupIDs[i] = g.ID
	}

	// Generate a random token
	randomString, err := utils.GenerateRandomAlphanumericString(16)
	if err != nil {
		return SignupToken{}, err
	}

	now := time.Now().Round(time.Second)
	state := SignupTokenState{
		ID:           uuid.NewV4().String(),
		ExpiresAt:    now.Add(ttl),
		UsageLimit:   usageLimit,
		UsageCount:   0,
		UserGroupIDs: validGroupIDs,
		CreatedAt:    now,
	}

	// The token's value is the actor's ID
	_, err = s.actorService.Invoke(ctx, SignupTokenActorType, randomString, SignupTokenMethodCreate, state)
	if err != nil {
		return SignupToken{}, fmt.Errorf("error creating signup token via actor: %w", err)
	}

	return signupTokenModelFromState(randomString, state, userGroups), nil
}

// loadUserGroupsByID loads every user group referenced by the given tokens, keyed by ID.
func (s *Service) loadUserGroupsByID(ctx context.Context, entries []signupTokenEntry) (map[string]model.UserGroup, error) {
	idSet := make(map[string]struct{})
	for _, e := range entries {
		for _, id := range e.State.UserGroupIDs {
			idSet[id] = struct{}{}
		}
	}
	if len(idSet) == 0 {
		return map[string]model.UserGroup{}, nil
	}

	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}

	var groups []model.UserGroup
	err := s.db.WithContext(ctx).
		Where("id IN ?", ids).
		Find(&groups).
		Error
	if err != nil {
		return nil, err
	}

	byID := make(map[string]model.UserGroup, len(groups))
	for _, g := range groups {
		byID[g.ID] = g
	}

	return byID, nil
}

// resolveUserGroups maps the given group IDs to the corresponding UserGroup objects, preserving order and skipping any that no longer exist.
func resolveUserGroups(ids []string, byID map[string]model.UserGroup) []model.UserGroup {
	if len(ids) == 0 {
		return nil
	}

	groups := make([]model.UserGroup, 0, len(ids))
	for _, id := range ids {
		g, ok := byID[id]
		if ok {
			groups = append(groups, g)
		}
	}

	return groups
}

// signupTokenModelFromState builds the API/model representation of a signup token from its actor ID (the token's value) and stored state.
func signupTokenModelFromState(token string, state SignupTokenState, groups []model.UserGroup) SignupToken {
	return SignupToken{
		Base: model.Base{
			ID:        state.ID,
			CreatedAt: datatype.DateTime(state.CreatedAt),
		},
		Token:      token,
		ExpiresAt:  datatype.DateTime(state.ExpiresAt),
		UsageLimit: state.UsageLimit,
		UsageCount: state.UsageCount,
		UserGroups: groups,
	}
}

// paginateSignupTokens sorts and paginates the in-memory list of signup tokens, mirroring the behavior of the DB-backed pagination utility.
func paginateSignupTokens(tokens []SignupToken, params utils.ListRequestOptions) ([]SignupToken, utils.PaginationResponse, error) {
	sortSignupTokens(tokens, params.Sort.Column, params.Sort.Direction)

	page := max(params.Pagination.Page, 1)
	pageSize := params.Pagination.Limit
	switch {
	case pageSize < 1:
		pageSize = 20
	case pageSize > 100:
		pageSize = 100
	}

	totalItems := int64(len(tokens))
	totalPages := (totalItems + int64(pageSize) - 1) / int64(pageSize)
	if totalItems == 0 {
		totalPages = 1
	}
	if int64(page) > totalPages {
		page = int(totalPages)
	}

	start := min((page-1)*pageSize, len(tokens))
	end := min(start+pageSize, len(tokens))

	return tokens[start:end], utils.PaginationResponse{
		TotalPages:   totalPages,
		TotalItems:   totalItems,
		CurrentPage:  page,
		ItemsPerPage: pageSize,
	}, nil
}

// sortSignupTokens sorts the tokens by the given column and direction.
// It defaults to sorting by creation date ascending, matching the DB-backed listing.
func sortSignupTokens(tokens []SignupToken, column, direction string) {
	desc := utils.NormalizeSortDirection(direction) == "desc"

	less := func(i, j int) bool {
		caI := time.Time(tokens[i].CreatedAt)
		caJ := time.Time(tokens[j].CreatedAt)
		return caI.Before(caJ)
	}
	switch column {
	case "expiresAt":
		less = func(i, j int) bool {
			eaI := time.Time(tokens[i].ExpiresAt)
			eaJ := time.Time(tokens[j].ExpiresAt)
			return eaI.Before(eaJ)
		}
	case "usageLimit":
		less = func(i, j int) bool { return tokens[i].UsageLimit < tokens[j].UsageLimit }
	case "usageCount":
		less = func(i, j int) bool {
			return tokens[i].UsageCount < tokens[j].UsageCount
		}
	case "createdAt", "":
		// Use the default comparator (creation date)
	default:
		// Unknown or non-sortable column: keep the default (creation date)
	}

	sort.SliceStable(tokens, func(i, j int) bool {
		if desc {
			return less(j, i)
		}
		return less(i, j)
	})
}
