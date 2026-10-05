//go:build integration
// +build integration

package internal

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/master/internal/poolaccess"
	"github.com/determined-ai/determined/master/internal/rm"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

// accessTestPool returns a pool name of this test only, restricted by admin when restricted is
// set, with a grant for each grantee. Its access records are removed when the test ends.
func accessTestPool(
	t *testing.T, prefix string, admin model.User, restricted bool, grantees ...model.User,
) string {
	t.Helper()
	ctx := context.Background()
	pool := prefix + "-" + uuid.NewString()
	t.Cleanup(func() {
		_, err := db.Bun().NewRaw(
			`DELETE FROM resource_pool_restrictions WHERE pool_name = ?`, pool).Exec(ctx)
		require.NoError(t, err)
		_, err = db.Bun().NewRaw(
			`DELETE FROM resource_pool_grants WHERE pool_name = ?`, pool).Exec(ctx)
		require.NoError(t, err)
	})
	if restricted {
		_, err := poolaccess.Restrict(ctx, pool, admin.ID)
		require.NoError(t, err)
	}
	var ids []model.UserID
	for _, u := range grantees {
		ids = append(ids, u.ID)
	}
	_, err := poolaccess.Grant(ctx, pool, ids, admin.ID)
	require.NoError(t, err)
	return pool
}

// mockRMWithResolver returns MockRM with ResolveResourcePool answered by resolve.
func mockRMWithResolver(
	resolve func(name rm.ResourcePoolName, workspaceID, slots int) (rm.ResourcePoolName, error),
) *mocks.ResourceManager {
	mockRM := MockRM()
	for _, call := range mockRM.ExpectedCalls {
		if call.Method == "ResolveResourcePool" {
			call.ReturnArguments = mock.Arguments{resolve}
		}
	}
	mockRM.On("SmallerValueIsHigherPriority", mock.Anything).Return(true, nil)
	return mockRM
}

// resolveOmittedPoolTo resolves an omitted pool to aux for zero slots and to compute otherwise,
// as workspace or cluster defaults do, and any other name to itself.
func resolveOmittedPoolTo(aux, compute *string) func(
	rm.ResourcePoolName, int, int,
) (rm.ResourcePoolName, error) {
	return func(name rm.ResourcePoolName, _, slots int) (rm.ResourcePoolName, error) {
		switch {
		case name != "":
			return name, nil
		case slots == 0:
			return rm.ResourcePoolName(*aux), nil
		default:
			return rm.ResourcePoolName(*compute), nil
		}
	}
}

// restrictEveryPoolForTest makes every access check until the test ends see every pool as
// restricted with no grants, and returns the number of access reads made since. Continuations and
// system tasks never check access, so they must leave the count at zero.
func restrictEveryPoolForTest(t *testing.T) func() int {
	var mu sync.Mutex
	reads := 0
	readRestrictions := poolaccess.ReadRestrictions
	poolaccess.ReadRestrictions = func(
		_ context.Context, _ model.UserID, pools []string,
	) (map[string]bool, error) {
		mu.Lock()
		defer mu.Unlock()
		reads++
		restricted := map[string]bool{}
		for _, pool := range pools {
			restricted[pool] = false
		}
		return restricted, nil
	}
	t.Cleanup(func() { poolaccess.ReadRestrictions = readRestrictions })
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return reads
	}
}

// requirePoolDenied checks that err is the refusal of u in pool.
func requirePoolDenied(t *testing.T, err error, u model.User, pool string) {
	t.Helper()
	require.Equal(t, codes.PermissionDenied, status.Code(err), "%v", err)
	require.Contains(t, status.Convert(err).Message(),
		fmt.Sprintf("user %q may not use resource pool %q: the pool is restricted", u.Username, pool))
}

func ntscPoolConfig(t *testing.T, pool string) *structpb.Struct {
	config, err := structpb.NewStruct(map[string]any{
		"resources": map[string]any{"resource_pool": pool},
	})
	require.NoError(t, err)
	return config
}

func TestLaunchNTSCChecksResolvedPool(t *testing.T) {
	var wsaux, wscompute string
	mockRM := mockRMWithResolver(resolveOmittedPoolTo(&wsaux, &wscompute))
	api, admin, adminCtx := setupAPITest(t, nil, mockRM)
	cs, err := command.NewService(api.m.db, api.m.rm)
	require.NoError(t, err)
	command.SetDefaultService(cs)

	granted := db.RequireMockUser(t, api.m.db)
	other := db.RequireMockUser(t, api.m.db)
	wsaux = accessTestPool(t, "wsaux", admin, true, granted)
	wscompute = accessTestPool(t, "wscompute", admin, true, granted)
	open := accessTestPool(t, "open", admin, false)
	otherCtx := ntscUserCtx(t, other)
	sessions := countUserSessions(adminCtx, t, other.ID)

	// The pool that an omitted resources.resource_pool resolves to is checked.
	_, err = api.LaunchCommand(otherCtx, &apiv1.LaunchCommandRequest{})
	requirePoolDenied(t, err, other, wscompute)
	_, err = api.LaunchTensorboard(otherCtx, &apiv1.LaunchTensorboardRequest{ExperimentIds: []int32{1}})
	requirePoolDenied(t, err, other, wsaux)
	// So is an explicit pool, and a notebook preview reports the refusal.
	_, err = api.LaunchShell(otherCtx, &apiv1.LaunchShellRequest{Config: ntscPoolConfig(t, wscompute)})
	requirePoolDenied(t, err, other, wscompute)
	_, err = api.LaunchNotebook(otherCtx, &apiv1.LaunchNotebookRequest{
		Config: ntscPoolConfig(t, wsaux), Preview: true,
	})
	requirePoolDenied(t, err, other, wsaux)
	_, err = api.LaunchNotebook(otherCtx, &apiv1.LaunchNotebookRequest{Preview: true})
	requirePoolDenied(t, err, other, wscompute)
	require.Equal(t, sessions, countUserSessions(adminCtx, t, other.ID),
		"a refused launch must not start a task session")

	resp, err := api.LaunchCommand(otherCtx, &apiv1.LaunchCommandRequest{Config: ntscPoolConfig(t, open)})
	require.NoError(t, err)
	require.Equal(t, open, resp.Config.AsMap()["resources"].(map[string]any)["resource_pool"])

	for name, ctx := range map[string]context.Context{
		"granted": ntscUserCtx(t, granted), "admin": adminCtx,
	} {
		resp, err := api.LaunchCommand(ctx, &apiv1.LaunchCommandRequest{})
		require.NoError(t, err, name)
		require.Equal(t, wscompute, resp.Config.AsMap()["resources"].(map[string]any)["resource_pool"])
		_, err = api.LaunchNotebook(ctx, &apiv1.LaunchNotebookRequest{
			Config: ntscPoolConfig(t, wsaux), Preview: true,
		})
		require.NoError(t, err, name)
	}
}
