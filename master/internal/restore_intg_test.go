//go:build integration
// +build integration

package internal

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/experiment"
	"github.com/determined-ai/determined/master/internal/rm/rmevents"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/model"
)

func countUserSessions(ctx context.Context, t *testing.T, userID model.UserID) int {
	count, err := db.Bun().NewSelect().Table("user_sessions").
		Where("user_id = ?", userID).
		Count(ctx)
	require.NoError(t, err)
	return count
}

// TestRestoreExperimentSession checks that a failed experiment restore deletes the user session it
// created, and that a successful restore keeps it.
func TestRestoreExperimentSession(t *testing.T) {
	api, curUser, ctx := setupAPITest(t, nil)

	t.Run("failed restore", func(t *testing.T) {
		exp := createTestExp(t, api, curUser)
		// A snapshot newer than this master cannot be shimmed, so the restore fails after it
		// created the experiment's session.
		require.NoError(t, api.m.db.SaveSnapshot(exp.ID, experimentSnapshotVersion+1, []byte(`{}`)))

		before := countUserSessions(ctx, t, curUser.ID)
		err := api.m.restoreExperiment(exp)
		require.ErrorContains(t, err, "cannot shim")
		require.Equal(t, before, countUserSessions(ctx, t, curUser.ID))
	})

	t.Run("successful restore", func(t *testing.T) {
		exp := createTestExp(t, api, curUser)

		before := countUserSessions(ctx, t, curUser.ID)
		require.NoError(t, api.m.restoreExperiment(exp))
		t.Cleanup(func() {
			if e, ok := experiment.ExperimentRegistry.Load(exp.ID); ok {
				require.NoError(t, e.KillExperiment())
			}
		})
		require.Equal(t, before+1, countUserSessions(ctx, t, curUser.ID))
	})
}

// Restoring an experiment is a continuation, exempt from the resource pool ACL: it restores and
// allocates its trial even when its pool is restricted for its non-admin owner, and never checks
// access.
func TestRestoreIgnoresPoolAccess(t *testing.T) {
	mockRM := MockRM()
	mockRM.On("SmallerValueIsHigherPriority", mock.Anything).Return(true, nil)
	mockRM.On("Release", mock.Anything).Return()
	var mu sync.Mutex
	var allocated []sproto.AllocateRequest
	for _, call := range mockRM.ExpectedCalls {
		if call.Method == "Allocate" {
			call.ReturnArguments = mock.Arguments{
				func(msg sproto.AllocateRequest) *sproto.ResourcesSubscription {
					mu.Lock()
					defer mu.Unlock()
					allocated = append(allocated, msg)
					return rmevents.Subscribe(msg.AllocationID)
				}, nil,
			}
		}
	}
	api, _, ctx := setupAPITest(t, nil, mockRM)
	owner := db.RequireMockUser(t, api.m.db)
	accessReads := restrictEveryPoolForTest(t)

	exp := createTestExp(t, api, owner)
	_, err := db.Bun().NewUpdate().Table("experiments").Set("state = ?", model.ActiveState).
		Where("id = ?", exp.ID).Exec(ctx)
	require.NoError(t, err)
	exp.State = model.ActiveState

	require.NoError(t, api.m.restoreExperiment(exp))
	t.Cleanup(func() {
		if e, ok := experiment.ExperimentRegistry.Load(exp.ID); ok {
			require.NoError(t, e.KillExperiment())
		}
	})
	_, ok := experiment.ExperimentRegistry.Load(exp.ID)
	require.True(t, ok, "the experiment was not restored")
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, req := range allocated {
			if req.JobID == exp.JobID && req.ResourcePool == "kubernetes" {
				return true
			}
		}
		return false
	}, 10*time.Second, 10*time.Millisecond, "the restored experiment's trial did not allocate")
	require.Zero(t, accessReads())
}
