package usersignup

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/italypaleale/francis/actor"
	"github.com/italypaleale/francis/host/local"
	"github.com/stretchr/testify/require"

	testutils "github.com/pocket-id/pocket-id/backend/internal/utils/testing"
)

// newSignupTokenActorService starts a test actor host with the signup token actor registered and returns its service
func newSignupTokenActorService(t *testing.T) *actor.Service {
	t.Helper()

	var svc *actor.Service
	testutils.NewActorHostForTest(t, func(t *testing.T, h *local.Host) {
		err := h.RegisterActor(SignupTokenActorType, NewSignupTokenActor)
		require.NoError(t, err)
		svc = h.Service()
	})
	require.NotNil(t, svc)

	return svc
}

// newSignupTokenActorServiceWithReconciler starts a test host whose signup token actors use the given reconciler
func newSignupTokenActorServiceWithReconciler(t *testing.T, reconciler AttemptReconciler) *actor.Service {
	t.Helper()

	var svc *actor.Service
	testutils.NewActorHostForTest(t, func(t *testing.T, h *local.Host) {
		err := h.RegisterActor(SignupTokenActorType,
			func(actorID string, actorService *actor.Service) actor.Actor {
				return NewSignupTokenActorWithReconciler(actorID, actorService, reconciler)
			},
			local.WithMaxAttempts(attemptReconcileMaxAttempts),
		)
		require.NoError(t, err)
		svc = h.Service()
	})
	require.NotNil(t, svc)

	return svc
}

func createSignupTokenForTest(t *testing.T, svc *actor.Service, token string, state SignupTokenState) {
	t.Helper()
	_, err := svc.Invoke(t.Context(), SignupTokenActorType, token, SignupTokenMethodCreate, state)
	require.NoError(t, err)
}

// consumeSignupTokenForTest consumes one use of the token for the given attempt and returns the actor's response
func consumeSignupTokenForTest(t *testing.T, svc *actor.Service, token, attemptID string) signupTokenConsumeResponse {
	t.Helper()
	res, err := svc.Invoke(t.Context(), SignupTokenActorType, token, signupTokenMethodConsume, signupTokenConsumeRequest{
		AttemptID: attemptID,
	})
	require.NoError(t, err)

	var out signupTokenConsumeResponse
	err = res.Decode(&out)
	require.NoError(t, err)

	return out
}

// invokeAttemptMethodForTest invokes one of the attempt-scoped methods with the given payload
func invokeAttemptMethodForTest(t *testing.T, svc *actor.Service, token, method string, payload any) any {
	t.Helper()
	res, err := svc.Invoke(t.Context(), SignupTokenActorType, token, method, payload)
	require.NoError(t, err)
	if res == nil {
		return nil
	}

	switch method {
	case signupTokenMethodReconcile:
		var out signupTokenReconcileResponse
		require.NoError(t, res.Decode(&out))
		return out
	default:
		return nil
	}
}

// getSignupTokenStateForTest reads the raw stored state of a signup token
func getSignupTokenStateForTest(t *testing.T, svc *actor.Service, token string) SignupTokenState {
	t.Helper()
	var state SignupTokenState
	require.NoError(t, svc.GetState(t.Context(), SignupTokenActorType, token, &state))
	return state
}

// listSignupTokenIDsForTest returns the actor IDs (that is, the token values) of every stored signup token
func listSignupTokenIDsForTest(t *testing.T, svc *actor.Service) []string {
	t.Helper()
	res, err := svc.ListStates(t.Context(), SignupTokenActorType, nil)
	require.NoError(t, err)

	ids := make([]string, len(res.States))
	for i, st := range res.States {
		ids[i] = st.ActorID
	}

	return ids
}

// jsonEnvelope is a minimal actor.Envelope used to call the Job handler directly in tests
type jsonEnvelope struct {
	payload any
}

func (e jsonEnvelope) Decode(into any) error {
	b, err := json.Marshal(e.payload)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}

// fakeAttemptReconciler is a scripted AttemptReconciler for actor tests
type fakeAttemptReconciler struct {
	userExists bool
	err        error
	confirmed  []string
}

func (f *fakeAttemptReconciler) UserExistsForAttempt(_ context.Context, _ string) (bool, error) {
	return f.userExists, f.err
}

func (f *fakeAttemptReconciler) AttemptConfirmed(_ context.Context, attemptID string) {
	f.confirmed = append(f.confirmed, attemptID)
}

func TestSignupTokenActorConsume(t *testing.T) {
	svc := newSignupTokenActorService(t)

	createSignupTokenForTest(t, svc, "token-1", SignupTokenState{
		ID:           "id-1",
		ExpiresAt:    time.Now().Add(time.Hour),
		UsageLimit:   1,
		UserGroupIDs: []string{"group-a", "group-b"},
		CreatedAt:    time.Now(),
	})

	// First consume succeeds and returns the token's user groups
	res := consumeSignupTokenForTest(t, svc, "token-1", "attempt-1")
	require.Equal(t, signupTokenConsumeOK, res.Status)
	require.False(t, res.Replayed)
	require.Equal(t, []string{"group-a", "group-b"}, res.UserGroupIDs)

	// The attempt occupies exactly one slot and is recorded as pending
	state := getSignupTokenStateForTest(t, svc, "token-1")
	require.Equal(t, 1, state.UsageCount)
	require.Len(t, state.Attempts, 1)
	require.Equal(t, "attempt-1", state.Attempts[0].ID)
	require.Equal(t, signupAttemptPending, state.Attempts[0].Status)

	// A different attempt fails: the usage limit (1) has been reached
	res = consumeSignupTokenForTest(t, svc, "token-1", "attempt-2")
	require.Equal(t, signupTokenConsumeLimitReached, res.Status)
}

func TestSignupTokenActorConsumeReplayIsIdempotent(t *testing.T) {
	svc := newSignupTokenActorService(t)

	createSignupTokenForTest(t, svc, "token-replay", SignupTokenState{
		ID:         "id-replay",
		ExpiresAt:  time.Now().Add(time.Hour),
		UsageLimit: 1,
		CreatedAt:  time.Now(),
	})

	// The original accept reserves the slot
	first := consumeSignupTokenForTest(t, svc, "token-replay", "attempt-replay")
	require.Equal(t, signupTokenConsumeOK, first.Status)
	require.False(t, first.Replayed)

	// Replaying the same attempt returns it without occupying another slot
	replay := consumeSignupTokenForTest(t, svc, "token-replay", "attempt-replay")
	require.Equal(t, signupTokenConsumeOK, replay.Status)
	require.True(t, replay.Replayed)

	state := getSignupTokenStateForTest(t, svc, "token-replay")
	require.Equal(t, 1, state.UsageCount)
	require.Len(t, state.Attempts, 1)

	// The replay must not have scheduled a second reconciliation job
	jobs, err := svc.ListJobs(t.Context(), SignupTokenActorType, "token-replay")
	require.NoError(t, err)
	require.Len(t, jobs, 1)
}

func TestSignupTokenActorConsumeReplayAfterCompletion(t *testing.T) {
	svc := newSignupTokenActorService(t)

	createSignupTokenForTest(t, svc, "token-done", SignupTokenState{
		ID:         "id-done",
		ExpiresAt:  time.Now().Add(time.Hour),
		UsageLimit: 2,
		CreatedAt:  time.Now(),
	})

	require.Equal(t, signupTokenConsumeOK, consumeSignupTokenForTest(t, svc, "token-done", "attempt-done").Status)
	invokeAttemptMethodForTest(t, svc, "token-done", signupTokenMethodComplete, signupTokenAttemptRequest{
		AttemptID: "attempt-done",
	})

	// A replay of an already completed attempt succeeds without re-occupying a slot
	replay := consumeSignupTokenForTest(t, svc, "token-done", "attempt-done")
	require.Equal(t, signupTokenConsumeOK, replay.Status)
	require.True(t, replay.Replayed)

	state := getSignupTokenStateForTest(t, svc, "token-done")
	require.Equal(t, 1, state.UsageCount)
	require.Len(t, state.Attempts, 1)
	require.Equal(t, signupAttemptCompleted, state.Attempts[0].Status)

	// A fresh attempt can still use the remaining slot
	require.Equal(t, signupTokenConsumeOK, consumeSignupTokenForTest(t, svc, "token-done", "attempt-new").Status)
}

func TestSignupTokenActorConsumeNotFound(t *testing.T) {
	svc := newSignupTokenActorService(t)

	res := consumeSignupTokenForTest(t, svc, "does-not-exist", "attempt-x")
	require.Equal(t, signupTokenConsumeNotFound, res.Status)
}

// TestSignupTokenActorCreateExpired verifies that a token that has already expired is never stored, since its state TTL would be in the past
func TestSignupTokenActorCreateExpired(t *testing.T) {
	svc := newSignupTokenActorService(t)

	createSignupTokenForTest(t, svc, "token-expired", SignupTokenState{
		ID:         "id-expired",
		ExpiresAt:  time.Now().Add(-time.Minute),
		UsageLimit: 1,
		CreatedAt:  time.Now().Add(-time.Hour),
	})

	require.Empty(t, listSignupTokenIDsForTest(t, svc))
	require.Equal(t, signupTokenConsumeNotFound, consumeSignupTokenForTest(t, svc, "token-expired", "attempt-x").Status)
}

func TestSignupTokenActorConsumeRejectsEmptyAttemptID(t *testing.T) {
	svc := newSignupTokenActorService(t)

	createSignupTokenForTest(t, svc, "token-empty", SignupTokenState{
		ID:         "id-empty",
		ExpiresAt:  time.Now().Add(time.Hour),
		UsageLimit: 1,
		CreatedAt:  time.Now(),
	})

	_, err := svc.Invoke(t.Context(), SignupTokenActorType, "token-empty", signupTokenMethodConsume, signupTokenConsumeRequest{})
	require.Error(t, err)
}

// TestSignupTokenActorReleaseIsScopedToAttempt verifies releasing one attempt never frees another concurrent attempt's slot
func TestSignupTokenActorReleaseIsScopedToAttempt(t *testing.T) {
	svc := newSignupTokenActorService(t)

	createSignupTokenForTest(t, svc, "token-parallel", SignupTokenState{
		ID:         "id-parallel",
		ExpiresAt:  time.Now().Add(time.Hour),
		UsageLimit: 3,
		CreatedAt:  time.Now(),
	})

	require.Equal(t, signupTokenConsumeOK, consumeSignupTokenForTest(t, svc, "token-parallel", "attempt-a").Status)
	require.Equal(t, signupTokenConsumeOK, consumeSignupTokenForTest(t, svc, "token-parallel", "attempt-b").Status)
	require.Equal(t, 2, getSignupTokenStateForTest(t, svc, "token-parallel").UsageCount)

	// Failing signup A releases only A's slot
	invokeAttemptMethodForTest(t, svc, "token-parallel", signupTokenMethodRelease, signupTokenAttemptRequest{
		AttemptID: "attempt-a",
	})

	state := getSignupTokenStateForTest(t, svc, "token-parallel")
	require.Equal(t, 1, state.UsageCount)
	require.Len(t, state.Attempts, 1)
	require.Equal(t, "attempt-b", state.Attempts[0].ID)
	require.Equal(t, signupAttemptPending, state.Attempts[0].Status)

	// Releasing A again or releasing an unknown attempt is a no-op, B's slot stays occupied
	invokeAttemptMethodForTest(t, svc, "token-parallel", signupTokenMethodRelease, signupTokenAttemptRequest{
		AttemptID: "attempt-a",
	})
	invokeAttemptMethodForTest(t, svc, "token-parallel", signupTokenMethodRelease, signupTokenAttemptRequest{
		AttemptID: "attempt-unknown",
	})
	require.Equal(t, 1, getSignupTokenStateForTest(t, svc, "token-parallel").UsageCount)

	// One slot is available again, and using it doesn't disturb B
	require.Equal(t, signupTokenConsumeOK, consumeSignupTokenForTest(t, svc, "token-parallel", "attempt-c").Status)
	state = getSignupTokenStateForTest(t, svc, "token-parallel")
	require.Equal(t, 2, state.UsageCount)
	require.ElementsMatch(t, []string{"attempt-b", "attempt-c"}, []string{
		state.Attempts[0].ID, state.Attempts[1].ID,
	})
}

// TestSignupTokenActorReleaseKeepsCompletedAttempt verifies a committed user's slot can never be released
func TestSignupTokenActorReleaseKeepsCompletedAttempt(t *testing.T) {
	svc := newSignupTokenActorService(t)

	createSignupTokenForTest(t, svc, "token-keep", SignupTokenState{
		ID:         "id-keep",
		ExpiresAt:  time.Now().Add(time.Hour),
		UsageLimit: 1,
		CreatedAt:  time.Now(),
	})

	require.Equal(t, signupTokenConsumeOK, consumeSignupTokenForTest(t, svc, "token-keep", "attempt-keep").Status)
	invokeAttemptMethodForTest(t, svc, "token-keep", signupTokenMethodComplete, signupTokenAttemptRequest{
		AttemptID: "attempt-keep",
	})

	invokeAttemptMethodForTest(t, svc, "token-keep", signupTokenMethodRelease, signupTokenAttemptRequest{
		AttemptID: "attempt-keep",
	})

	state := getSignupTokenStateForTest(t, svc, "token-keep")
	require.Equal(t, 1, state.UsageCount)
	require.Len(t, state.Attempts, 1)
	require.Equal(t, signupAttemptCompleted, state.Attempts[0].Status)
}

// TestSignupTokenActorCompleteRestoresMissingAttempt verifies a late commit after reconciliation restores the slot
func TestSignupTokenActorCompleteRestoresMissingAttempt(t *testing.T) {
	svc := newSignupTokenActorService(t)

	createSignupTokenForTest(t, svc, "token-late", SignupTokenState{
		ID:         "id-late",
		ExpiresAt:  time.Now().Add(time.Hour),
		UsageLimit: 1,
		CreatedAt:  time.Now(),
	})

	require.Equal(t, signupTokenConsumeOK, consumeSignupTokenForTest(t, svc, "token-late", "attempt-late").Status)
	invokeAttemptMethodForTest(t, svc, "token-late", signupTokenMethodRelease, signupTokenAttemptRequest{
		AttemptID: "attempt-late",
	})
	require.Zero(t, getSignupTokenStateForTest(t, svc, "token-late").UsageCount)

	// The transaction actually committed: completing the missing attempt must restore its slot
	invokeAttemptMethodForTest(t, svc, "token-late", signupTokenMethodComplete, signupTokenAttemptRequest{
		AttemptID: "attempt-late",
	})

	state := getSignupTokenStateForTest(t, svc, "token-late")
	require.Equal(t, 1, state.UsageCount)
	require.Len(t, state.Attempts, 1)
	require.Equal(t, signupAttemptCompleted, state.Attempts[0].Status)
}

// TestSignupTokenActorReconcile covers the recovery decisions for pending attempts
func TestSignupTokenActorReconcile(t *testing.T) {
	svc := newSignupTokenActorService(t)
	now := time.Now()
	createSignupTokenForTest(t, svc, "token-reconcile", SignupTokenState{
		ID:         "id-reconcile",
		ExpiresAt:  now.Add(time.Hour),
		UsageLimit: 3,
		UsageCount: 3,
		CreatedAt:  now,
		Attempts: []signupTokenAttempt{
			// A young attempt whose request may still be running
			{ID: "attempt-young", Status: signupAttemptPending, CreatedAt: now},
			// An old attempt whose process is certainly gone
			{ID: "attempt-old-gone", Status: signupAttemptPending, CreatedAt: now.Add(-10 * time.Minute)},
			// An old attempt whose user committed before the crash
			{ID: "attempt-old-done", Status: signupAttemptPending, CreatedAt: now.Add(-10 * time.Minute)},
		},
	})

	reconcile := func(attemptID string, userExists bool) signupTokenReconcileStatus {
		out := invokeAttemptMethodForTest(t, svc, "token-reconcile", signupTokenMethodReconcile, signupTokenReconcileRequest{
			AttemptID:  attemptID,
			UserExists: userExists,
		}).(signupTokenReconcileResponse)
		return out.Status
	}

	// Young + no user: wait
	require.Equal(t, signupTokenReconcileDeferred, reconcile("attempt-young", false))
	require.Equal(t, 3, getSignupTokenStateForTest(t, svc, "token-reconcile").UsageCount)

	// Old + committed user: confirm and keep the slot
	require.Equal(t, signupTokenReconcileCompleted, reconcile("attempt-old-done", true))
	state := getSignupTokenStateForTest(t, svc, "token-reconcile")
	require.Equal(t, 3, state.UsageCount)
	require.Equal(t, signupAttemptCompleted, findSignupAttempt(state.Attempts, "attempt-old-done").Status)

	// Old + no user: release only that slot
	require.Equal(t, signupTokenReconcileReleased, reconcile("attempt-old-gone", false))
	state = getSignupTokenStateForTest(t, svc, "token-reconcile")
	require.Equal(t, 2, state.UsageCount)
	require.Nil(t, findSignupAttempt(state.Attempts, "attempt-old-gone"))
	require.NotNil(t, findSignupAttempt(state.Attempts, "attempt-young"))

	// Reconciling again, a completed attempt, or an unknown attempt is a no-op
	require.Equal(t, signupTokenReconcileNoop, reconcile("attempt-old-gone", false))
	require.Equal(t, signupTokenReconcileNoop, reconcile("attempt-old-done", true))
	require.Equal(t, signupTokenReconcileNoop, reconcile("attempt-unknown", false))
	require.Equal(t, 2, getSignupTokenStateForTest(t, svc, "token-reconcile").UsageCount)
}

// TestSignupTokenActorLegacyStateKeepsAccounting verifies states written without an attempt ledger keep their usage count
func TestSignupTokenActorLegacyStateKeepsAccounting(t *testing.T) {
	svc := newSignupTokenActorService(t)

	createSignupTokenForTest(t, svc, "token-legacy", SignupTokenState{
		ID:         "id-legacy",
		ExpiresAt:  time.Now().Add(time.Hour),
		UsageLimit: 3,
		UsageCount: 2,
		CreatedAt:  time.Now(),
	})

	// One legacy-style use remains and is correctly enforced
	res := consumeSignupTokenForTest(t, svc, "token-legacy", "attempt-new")
	require.Equal(t, signupTokenConsumeOK, res.Status)
	require.Equal(t, signupTokenConsumeLimitReached, consumeSignupTokenForTest(t, svc, "token-legacy", "attempt-newer").Status)

	state := getSignupTokenStateForTest(t, svc, "token-legacy")
	require.Equal(t, 3, state.UsageCount)
	require.Len(t, state.Attempts, 1)
}

func TestSignupTokenActorDelete(t *testing.T) {
	svc := newSignupTokenActorService(t)

	createSignupTokenForTest(t, svc, "token-3", SignupTokenState{
		ID:         "id-3",
		ExpiresAt:  time.Now().Add(time.Hour),
		UsageLimit: 1,
		CreatedAt:  time.Now(),
	})
	require.Equal(t, []string{"token-3"}, listSignupTokenIDsForTest(t, svc))

	_, err := svc.Invoke(t.Context(), SignupTokenActorType, "token-3", SignupTokenMethodDelete, nil)
	require.NoError(t, err)

	require.Empty(t, listSignupTokenIDsForTest(t, svc))

	// The token can no longer be consumed
	require.Equal(t, signupTokenConsumeNotFound, consumeSignupTokenForTest(t, svc, "token-3", "attempt-x").Status)

	// Deleting a token that no longer exists is a no-op
	_, err = svc.Invoke(t.Context(), SignupTokenActorType, "token-3", SignupTokenMethodDelete, nil)
	require.NoError(t, err)
}

// TestSignupTokenActorMigrateDoesNotOverwrite verifies that the one-time migration never resets a token that was already migrated and used since
func TestSignupTokenActorMigrateDoesNotOverwrite(t *testing.T) {
	svc := newSignupTokenActorService(t)

	state := SignupTokenState{
		ID:         "id-4",
		ExpiresAt:  time.Now().Add(time.Hour),
		UsageLimit: 2,
		CreatedAt:  time.Now(),
	}
	createSignupTokenForTest(t, svc, "token-4", state)

	// Use the token once
	require.Equal(t, signupTokenConsumeOK, consumeSignupTokenForTest(t, svc, "token-4", "attempt-1").Status)

	// Re-running the migration must not reset the usage count
	_, err := svc.Invoke(t.Context(), SignupTokenActorType, "token-4", signupTokenMethodMigrate, state)
	require.NoError(t, err)

	// Only one use is left, so a single consume succeeds and the next one doesn't
	require.Equal(t, signupTokenConsumeOK, consumeSignupTokenForTest(t, svc, "token-4", "attempt-2").Status)
	require.Equal(t, signupTokenConsumeLimitReached, consumeSignupTokenForTest(t, svc, "token-4", "attempt-3").Status)
}

// TestSignupTokenActorJob verifies the durable reconciliation job drives the same transitions and signals deferral with a retryable error
func TestSignupTokenActorJob(t *testing.T) {
	reconciler := &fakeAttemptReconciler{userExists: true}
	svc := newSignupTokenActorServiceWithReconciler(t, reconciler)

	now := time.Now()
	createSignupTokenForTest(t, svc, "token-job", SignupTokenState{
		ID:         "id-job",
		ExpiresAt:  now.Add(time.Hour),
		UsageLimit: 2,
		UsageCount: 2,
		CreatedAt:  now,
		Attempts: []signupTokenAttempt{
			{ID: "attempt-done", Status: signupAttemptPending, CreatedAt: now.Add(-10 * time.Minute)},
			{ID: "attempt-young", Status: signupAttemptPending, CreatedAt: now},
		},
	})

	a := &signupTokenActor{
		actorID:    "token-job",
		log:        slog.Default(),
		client:     actor.NewActorClient[SignupTokenState](SignupTokenActorType, "token-job", svc),
		reconciler: reconciler,
	}

	// User exists: the job completes the attempt and restores the post-commit callback
	require.NoError(t, a.Job(t.Context(), signupTokenJobReconcile, jsonEnvelope{payload: signupTokenAttemptRequest{AttemptID: "attempt-done"}}))
	require.Equal(t, []string{"attempt-done"}, reconciler.confirmed)
	require.Equal(t, signupAttemptCompleted, findSignupAttempt(getSignupTokenStateForTest(t, svc, "token-job").Attempts, "attempt-done").Status)

	// Young attempt without a user: the job asks for a retry
	reconciler.userExists = false
	err := a.Job(t.Context(), signupTokenJobReconcile, jsonEnvelope{payload: signupTokenAttemptRequest{AttemptID: "attempt-young"}})
	require.Error(t, err)
	require.NotErrorIs(t, err, actor.ErrJobPermanentFailure)

	// A database lookup error is a regular, retryable job failure
	reconciler.err = errors.New("database unavailable")
	err = a.Job(t.Context(), signupTokenJobReconcile, jsonEnvelope{payload: signupTokenAttemptRequest{AttemptID: "attempt-young"}})
	require.Error(t, err)
	require.NotErrorIs(t, err, actor.ErrJobPermanentFailure)

	// Unknown methods and bad payloads fail permanently
	err = a.Job(t.Context(), "nope", jsonEnvelope{payload: signupTokenAttemptRequest{}})
	require.ErrorIs(t, err, actor.ErrJobPermanentFailure)
	err = a.Job(t.Context(), signupTokenJobReconcile, jsonEnvelope{payload: signupTokenAttemptRequest{}})
	require.ErrorIs(t, err, actor.ErrJobPermanentFailure)
}
