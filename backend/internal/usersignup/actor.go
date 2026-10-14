package usersignup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/italypaleale/francis/actor"

	"github.com/pocket-id/pocket-id/backend/internal/common"
)

// Signup tokens are stored entirely in the actor state store.
// Each token is its own actor, whose actor ID is the token value itself.
// The state is persisted with a TTL equal to the token's lifetime, so it's purged automatically when the token expires (there's no separate cleanup job and no expiration alarm).
// Listing tokens uses ListStates, which only returns states that haven't expired yet.

// SignupTokenActorType is the actor type for the signup token actor
const SignupTokenActorType = "SignupToken"

const (
	// attemptReconcileDelay is how long a consumed attempt may stay pending before automatic reconciliation assumes the request is gone.
	// It must be much longer than a signup request can take, so a still-running request is never reconciled out from under itself.
	attemptReconcileDelay = 2 * time.Minute
	// attemptReconcileMaxAttempts caps the retries of one reconciliation job, matching the delivery job pattern
	attemptReconcileMaxAttempts = 5
)

// Methods exposed by the signup token actor
// Because we cannot invoke an actor while a DB transaction is open (that would deadlock on SQLite), consuming a token is done by invoking the actor first (which atomically validates it and reserves one quota slot for a stable attempt), and only afterwards performing the remaining work.
// Every accepted consume carries a stable attempt ID: failures release only that attempt's slot, and reconciliation after a crash ties the attempt to the committed user.
const (
	// SignupTokenMethodCreate stores a new signup token, replacing any existing state
	SignupTokenMethodCreate = "create"
	// SignupTokenMethodDelete removes a signup token
	SignupTokenMethodDelete = "delete"

	signupTokenMethodMigrate   = "migrate"
	signupTokenMethodConsume   = "consume"
	signupTokenMethodComplete  = "complete"
	signupTokenMethodRelease   = "release"
	signupTokenMethodReconcile = "reconcile"
	// signupTokenJobReconcile is the durable job method that reconciles one pending attempt after the grace delay
	signupTokenJobReconcile = "reconcile-attempt"
)

// signupTokenConsumeStatus is the outcome of a "consume" invocation.
type signupTokenConsumeStatus string

const (
	// signupTokenConsumeOK indicates the token was valid and one use has been consumed
	signupTokenConsumeOK signupTokenConsumeStatus = "ok"
	// signupTokenConsumeNotFound indicates the token doesn't exist (or has expired)
	signupTokenConsumeNotFound signupTokenConsumeStatus = "not_found"
	// signupTokenConsumeLimitReached indicates the token has no uses left
	signupTokenConsumeLimitReached signupTokenConsumeStatus = "limit_reached"
)

// signupTokenReconcileStatus is the outcome of a "reconcile" invocation.
type signupTokenReconcileStatus string

const (
	// signupTokenReconcileCompleted means the pending attempt was confirmed against an existing user and marked completed
	signupTokenReconcileCompleted signupTokenReconcileStatus = "completed"
	// signupTokenReconcileReleased means the pending attempt was abandoned without a user and its slot was released
	signupTokenReconcileReleased signupTokenReconcileStatus = "released"
	// signupTokenReconcileDeferred means the attempt is still within the grace window, so no decision was made
	signupTokenReconcileDeferred signupTokenReconcileStatus = "deferred"
	// signupTokenReconcileNoop means there was nothing to reconcile (unknown or already completed attempt, deleted token)
	signupTokenReconcileNoop signupTokenReconcileStatus = "noop"
)

// SignupTokenState is the persisted state of a signup token actor.
// The token value itself is the actor's ID, so it isn't repeated here.
// UsageCount stays the authoritative occupied-slot counter, so states written by older versions (which have no attempts) keep their accounting.
// Attempts is the per-use ledger that makes releasing and crash recovery precise.
type SignupTokenState struct {
	ID           string
	UsageLimit   int
	UsageCount   int
	UserGroupIDs []string
	CreatedAt    time.Time
	ExpiresAt    time.Time
	Attempts     []signupTokenAttempt
}

// signupTokenConsumeRequest is the payload of the "consume" method
type signupTokenConsumeRequest struct {
	// AttemptID is the stable identifier of this registration attempt, and doubles as the ID of the user that will be created
	AttemptID string
}

// signupTokenAttemptRequest references one attempt by its ID
type signupTokenAttemptRequest struct {
	AttemptID string
}

// signupTokenReconcileRequest carries the database lookup result for a pending attempt
type signupTokenReconcileRequest struct {
	AttemptID  string
	UserExists bool
}

// signupTokenConsumeResponse is the response of the "consume" method
type signupTokenConsumeResponse struct {
	Status signupTokenConsumeStatus
	// UserGroupIDs is set only when Status is "ok", and contains the groups the new user should join
	UserGroupIDs []string
	// Replayed is true when an existing attempt with the same ID was returned instead of occupying a new slot
	Replayed bool
}

// signupTokenReconcileResponse is the response of the "reconcile" method
type signupTokenReconcileResponse struct {
	Status signupTokenReconcileStatus
}

// AttemptReconciler lets the actor inspect the database while reconciling an attempt.
// It's implemented by the usersignup Service, which owns the database handle.
type AttemptReconciler interface {
	// UserExistsForAttempt reports whether the user tied to the attempt ID has been committed
	UserExistsForAttempt(ctx context.Context, attemptID string) (bool, error)
	// AttemptConfirmed is called after a pending attempt was confirmed against an existing user, so post-commit side effects a crashed request may have missed can be restored
	AttemptConfirmed(ctx context.Context, attemptID string)
}

// signupTokenActor is the actor that manages a single signup token
type signupTokenActor struct {
	actorID    string
	log        *slog.Logger
	client     actor.Client[SignupTokenState]
	reconciler AttemptReconciler
}

// NewSignupTokenActor allocates a new signup token actor without database recovery support.
// It satisfies actor.Factory and is kept for tests; production wires the reconciler through NewSignupTokenActorWithReconciler.
func NewSignupTokenActor(actorID string, service *actor.Service) actor.Actor {
	return NewSignupTokenActorWithReconciler(actorID, service, nil)
}

// NewSignupTokenActorWithReconciler allocates a new signup token actor with the database accessor used by recovery jobs
func NewSignupTokenActorWithReconciler(actorID string, service *actor.Service, reconciler AttemptReconciler) actor.Actor {
	return &signupTokenActor{
		actorID: actorID,
		log: slog.With(
			slog.String("scope", "actor"),
			slog.String("actorType", SignupTokenActorType),
			slog.String("token", actorID),
		),
		client:     actor.NewActorClient[SignupTokenState](SignupTokenActorType, actorID, service),
		reconciler: reconciler,
	}
}

// Invoke implements actor.ActorInvoke
func (a *signupTokenActor) Invoke(parentCtx context.Context, method string, data actor.Envelope) (any, error) {
	switch method {
	case SignupTokenMethodCreate:
		return nil, a.create(parentCtx, data, false)
	case signupTokenMethodMigrate:
		return nil, a.create(parentCtx, data, true)
	case signupTokenMethodConsume:
		return a.consume(parentCtx, data)
	case signupTokenMethodComplete:
		return nil, a.complete(parentCtx, data)
	case signupTokenMethodRelease:
		return nil, a.release(parentCtx, data)
	case signupTokenMethodReconcile:
		return a.reconcile(parentCtx, data)
	case SignupTokenMethodDelete:
		return nil, a.delete(parentCtx)
	default:
		return nil, common.ErrUnsupportedActorMethod{Method: method}
	}
}

// create stores the token's state.
// When onlyIfMissing is true the write is skipped if the actor already has state: this is used by the one-time migration of the pre-actor tokens, so a token that has already been migrated is never reset.
func (a *signupTokenActor) create(parentCtx context.Context, data actor.Envelope, onlyIfMissing bool) error {
	if data == nil {
		return fmt.Errorf("request body is empty for method '%s'", SignupTokenMethodCreate)
	}

	var state SignupTokenState
	err := data.Decode(&state)
	if err != nil {
		return fmt.Errorf("request body is not valid for method '%s': %w", SignupTokenMethodCreate, err)
	}

	if onlyIfMissing {
		ctx, cancel := context.WithTimeout(parentCtx, 10*time.Second)
		defer cancel()
		current, err := a.client.GetState(ctx)
		if err != nil {
			return fmt.Errorf("error retrieving actor state: %w", err)
		}

		// An empty ID means there's no state yet
		if current.ID != "" {
			return nil
		}
	}

	return a.setState(parentCtx, state)
}

// consume atomically validates the token and reserves one quota slot bound to the request's attempt ID.
// A repeated consume with the same attempt ID returns the existing attempt without occupying another slot.
func (a *signupTokenActor) consume(parentCtx context.Context, data actor.Envelope) (signupTokenConsumeResponse, error) {
	var req signupTokenConsumeRequest
	if data == nil {
		return signupTokenConsumeResponse{}, fmt.Errorf("request body is empty for method '%s'", signupTokenMethodConsume)
	}
	err := data.Decode(&req)
	if err != nil {
		return signupTokenConsumeResponse{}, fmt.Errorf("request body is not valid for method '%s': %w", signupTokenMethodConsume, err)
	}
	if req.AttemptID == "" {
		return signupTokenConsumeResponse{}, fmt.Errorf("request for method '%s' is missing the attempt ID", signupTokenMethodConsume)
	}

	ctx, cancel := context.WithTimeout(parentCtx, 10*time.Second)
	defer cancel()
	state, err := a.client.GetState(ctx)
	if err != nil {
		return signupTokenConsumeResponse{}, fmt.Errorf("error retrieving actor state: %w", err)
	}

	// An empty ID means there's no state: the token doesn't exist (or its state already expired and was purged)
	if state.ID == "" || state.ExpiresAt.Before(time.Now()) {
		return signupTokenConsumeResponse{
			Status: signupTokenConsumeNotFound,
		}, nil
	}

	// A replay of an accepted attempt is idempotent, whether the attempt is still pending or already completed
	if attempt := findSignupAttempt(state.Attempts, req.AttemptID); attempt != nil {
		if attempt.Status == signupAttemptPending {
			// Re-arm reconciliation in case the previous scheduling was lost with the process
			err = a.scheduleReconciliation(parentCtx, req.AttemptID)
			if err != nil {
				return signupTokenConsumeResponse{}, err
			}
		}

		return signupTokenConsumeResponse{
			Status:       signupTokenConsumeOK,
			UserGroupIDs: state.UserGroupIDs,
			Replayed:     true,
		}, nil
	}

	if state.UsageCount >= state.UsageLimit {
		return signupTokenConsumeResponse{
			Status: signupTokenConsumeLimitReached,
		}, nil
	}

	// Reserve one slot for this attempt
	state.Attempts = append(state.Attempts, signupTokenAttempt{
		ID:        req.AttemptID,
		Status:    signupAttemptPending,
		CreatedAt: time.Now(),
	})
	state.UsageCount++
	err = a.setState(parentCtx, state)
	if err != nil {
		return signupTokenConsumeResponse{}, err
	}

	// Schedule the durable reconciliation that resolves the attempt if the request dies before reporting the outcome
	err = a.scheduleReconciliation(parentCtx, req.AttemptID)
	if err != nil {
		return signupTokenConsumeResponse{}, err
	}

	return signupTokenConsumeResponse{
		Status:       signupTokenConsumeOK,
		UserGroupIDs: state.UserGroupIDs,
	}, nil
}

// complete marks an attempt completed once its user has been confirmed in the database.
// It never frees a quota slot: a completed attempt stands for a committed user.
func (a *signupTokenActor) complete(parentCtx context.Context, data actor.Envelope) error {
	req, err := decodeAttemptRequest(data, signupTokenMethodComplete)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(parentCtx, 10*time.Second)
	defer cancel()
	state, err := a.client.GetState(ctx)
	if err != nil {
		return fmt.Errorf("error retrieving actor state: %w", err)
	}

	// The token is gone (revoked or expired): there's no accounting to update
	if state.ID == "" {
		return nil
	}

	idx := indexSignupAttempt(state.Attempts, req.AttemptID)
	if idx >= 0 {
		if state.Attempts[idx].Status == signupAttemptCompleted {
			return nil
		}

		state.Attempts[idx].Status = signupAttemptCompleted
		return a.setState(parentCtx, state)
	}

	// The attempt is missing: reconciliation may have released it just before a transaction that actually committed
	// The committed user must keep occupying a slot, so re-add a completed attempt and restore the counter
	state.UsageCount++
	state.Attempts = append(state.Attempts, signupTokenAttempt{
		ID:        req.AttemptID,
		Status:    signupAttemptCompleted,
		CreatedAt: time.Now(),
	})
	a.log.WarnContext(parentCtx, "Completed a signup attempt that was no longer pending; restored its quota slot",
		slog.String("attemptId", req.AttemptID))
	return a.setState(parentCtx, state)
}

// release frees the slot of one specific pending attempt.
// It never decrements the counter for another attempt, and never touches a completed attempt (its user is committed).
func (a *signupTokenActor) release(parentCtx context.Context, data actor.Envelope) error {
	req, err := decodeAttemptRequest(data, signupTokenMethodRelease)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(parentCtx, 10*time.Second)
	defer cancel()
	state, err := a.client.GetState(ctx)
	if err != nil {
		return fmt.Errorf("error retrieving actor state: %w", err)
	}

	// The token is gone or the attempt was never recorded/already resolved: there's nothing of ours to compensate
	if state.ID == "" {
		return nil
	}

	idx := indexSignupAttempt(state.Attempts, req.AttemptID)
	if idx < 0 || state.Attempts[idx].Status != signupAttemptPending {
		return nil
	}

	state.Attempts = append(state.Attempts[:idx], state.Attempts[idx+1:]...)
	state.UsageCount--
	if state.UsageCount < 0 {
		state.UsageCount = 0
	}
	return a.setState(parentCtx, state)
}

// reconcile applies a database lookup result to a pending attempt.
func (a *signupTokenActor) reconcile(parentCtx context.Context, data actor.Envelope) (signupTokenReconcileResponse, error) {
	var req signupTokenReconcileRequest
	if data == nil {
		return signupTokenReconcileResponse{}, fmt.Errorf("request body is empty for method '%s'", signupTokenMethodReconcile)
	}
	err := data.Decode(&req)
	if err != nil {
		return signupTokenReconcileResponse{}, fmt.Errorf("request body is not valid for method '%s': %w", signupTokenMethodReconcile, err)
	}

	status, err := a.applyReconciliation(parentCtx, req.AttemptID, req.UserExists)
	if err != nil {
		return signupTokenReconcileResponse{}, err
	}

	return signupTokenReconcileResponse{Status: status}, nil
}

// applyReconciliation contains the state transition shared by the "reconcile" method and the durable reconciliation job.
func (a *signupTokenActor) applyReconciliation(parentCtx context.Context, attemptID string, userExists bool) (signupTokenReconcileStatus, error) {
	ctx, cancel := context.WithTimeout(parentCtx, 10*time.Second)
	defer cancel()
	state, err := a.client.GetState(ctx)
	if err != nil {
		return "", fmt.Errorf("error retrieving actor state: %w", err)
	}

	// Token revoked/expired, attempt already resolved, or completed out of band: nothing to decide
	if state.ID == "" {
		return signupTokenReconcileNoop, nil
	}
	idx := indexSignupAttempt(state.Attempts, attemptID)
	if idx < 0 || state.Attempts[idx].Status != signupAttemptPending {
		return signupTokenReconcileNoop, nil
	}

	// A committed user wins over every other outcome: confirm the attempt and keep the slot
	if userExists {
		state.Attempts[idx].Status = signupAttemptCompleted
		err = a.setState(parentCtx, state)
		if err != nil {
			return "", err
		}
		return signupTokenReconcileCompleted, nil
	}

	// Without a user, only release once the request that owns the attempt can plausibly be gone
	if time.Since(state.Attempts[idx].CreatedAt) < attemptReconcileDelay {
		return signupTokenReconcileDeferred, nil
	}

	state.Attempts = append(state.Attempts[:idx], state.Attempts[idx+1:]...)
	state.UsageCount--
	if state.UsageCount < 0 {
		state.UsageCount = 0
	}
	err = a.setState(parentCtx, state)
	if err != nil {
		return "", err
	}
	return signupTokenReconcileReleased, nil
}

// Job implements actor.ActorJob and runs the durable reconciliation scheduled when an attempt is accepted
func (a *signupTokenActor) Job(parentCtx context.Context, method string, data actor.Envelope) error {
	if method != signupTokenJobReconcile {
		return fmt.Errorf("%w: unsupported job method '%s'", actor.ErrJobPermanentFailure, method)
	}

	var req signupTokenAttemptRequest
	if data == nil {
		return fmt.Errorf("%w: job input is empty", actor.ErrJobPermanentFailure)
	}
	err := data.Decode(&req)
	if err != nil {
		return fmt.Errorf("%w: job input is not valid: %w", actor.ErrJobPermanentFailure, err)
	}
	if req.AttemptID == "" {
		return fmt.Errorf("%w: job input is missing the attempt ID", actor.ErrJobPermanentFailure)
	}
	if a.reconciler == nil {
		return fmt.Errorf("%w: no attempt reconciler is registered for the signup token actor", actor.ErrJobPermanentFailure)
	}

	ctx, cancel := context.WithTimeout(parentCtx, 10*time.Second)
	defer cancel()
	userExists, err := a.reconciler.UserExistsForAttempt(ctx, req.AttemptID)
	if err != nil {
		return fmt.Errorf("error looking up the user for signup attempt '%s': %w", req.AttemptID, err)
	}

	status, err := a.applyReconciliation(parentCtx, req.AttemptID, userExists)
	if err != nil {
		return err
	}

	switch status {
	case signupTokenReconcileDeferred:
		// The attempt is still within its grace window: fail so the framework retries the job later
		return fmt.Errorf("signup attempt '%s' is still within its grace window", req.AttemptID)
	case signupTokenReconcileCompleted:
		// Restore post-commit side effects the request that committed the user may have died before running
		a.reconciler.AttemptConfirmed(ctx, req.AttemptID)
	case signupTokenReconcileReleased, signupTokenReconcileNoop:
		// The slot was released or there was nothing to do: no further action
	}

	return nil
}

// JobFailed implements actor.ActorJobFailed and records the abandoned reconciliation for diagnosis
func (a *signupTokenActor) JobFailed(ctx context.Context, _ string, _ string, data actor.Envelope, jobErr error) error {
	var req signupTokenAttemptRequest
	if data != nil {
		_ = data.Decode(&req)
	}

	a.log.ErrorContext(ctx, "Giving up on reconciling a signup token attempt; its quota slot remains occupied",
		slog.String("attemptId", req.AttemptID),
		slog.Any("error", jobErr))
	return nil
}

// delete removes the token.
func (a *signupTokenActor) delete(parentCtx context.Context) error {
	ctx, cancel := context.WithTimeout(parentCtx, 10*time.Second)
	defer cancel()
	err := a.client.DeleteState(ctx)
	if err != nil && !errors.Is(err, actor.ErrStateNotFound) {
		// Deleting a token that doesn't exist (for example, one that expired in the meanwhile) already reaches the desired end state
		return fmt.Errorf("error deleting actor state: %w", err)
	}

	return nil
}

// scheduleReconciliation schedules the durable job that reconciles an attempt if its request never reports back.
// The attempt ID is the job's idempotency key, so re-scheduling for the same attempt never creates duplicate jobs.
func (a *signupTokenActor) scheduleReconciliation(parentCtx context.Context, attemptID string) error {
	ctx, cancel := context.WithTimeout(parentCtx, 10*time.Second)
	defer cancel()
	_, _, err := a.client.Dispatch(ctx, signupTokenJobReconcile, signupTokenAttemptRequest{
		AttemptID: attemptID,
	},
		actor.WithJobDelay(attemptReconcileDelay),
		actor.WithIdempotencyKey(attemptID),
	)
	if err != nil {
		return fmt.Errorf("error scheduling the reconciliation for signup attempt '%s': %w", attemptID, err)
	}

	return nil
}

// setState saves the state with a TTL matching the token's remaining lifetime, so it's purged automatically once the token expires.
// Saving is skipped if the token has already expired, since there would be nothing left to store.
func (a *signupTokenActor) setState(parentCtx context.Context, state SignupTokenState) error {
	ttl := time.Until(state.ExpiresAt)
	if ttl <= 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(parentCtx, 10*time.Second)
	defer cancel()
	err := a.client.SetState(ctx, state, &actor.SetStateOpts{
		TTL: ttl,
	})
	if err != nil {
		return fmt.Errorf("error saving actor state: %w", err)
	}

	return nil
}

// findSignupAttempt returns the attempt with the given ID, or nil when it isn't in the ledger
func findSignupAttempt(attempts []signupTokenAttempt, attemptID string) *signupTokenAttempt {
	idx := indexSignupAttempt(attempts, attemptID)
	if idx < 0 {
		return nil
	}
	return &attempts[idx]
}

// indexSignupAttempt returns the index of the attempt with the given ID, or -1 when it isn't in the ledger
func indexSignupAttempt(attempts []signupTokenAttempt, attemptID string) int {
	for i := range attempts {
		if attempts[i].ID == attemptID {
			return i
		}
	}
	return -1
}

// decodeAttemptRequest decodes a request payload that only references an attempt
func decodeAttemptRequest(data actor.Envelope, method string) (signupTokenAttemptRequest, error) {
	var req signupTokenAttemptRequest
	if data == nil {
		return req, fmt.Errorf("request body is empty for method '%s'", method)
	}
	err := data.Decode(&req)
	if err != nil {
		return req, fmt.Errorf("request body is not valid for method '%s': %w", method, err)
	}
	if req.AttemptID == "" {
		return req, fmt.Errorf("request for method '%s' is missing the attempt ID", method)
	}
	return req, nil
}
