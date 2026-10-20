//go:build integration
// +build integration

package internal

import (
	"context"
	"fmt"
	"testing"

	"github.com/uptrace/bun"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/master/internal/poolaccess"
	"github.com/determined-ai/determined/master/internal/rm"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/set"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/resourcepoolv1"
)

const (
	testPoolName       = "testRP"
	testPool2Name      = "testRP2"
	testWorkspaceName  = "bindings_test_workspace_1"
	testWorkspace2Name = "bindings_test_workspace_2"
)

func setupWorkspaces(ctx context.Context, t *testing.T, api *apiServer) []int32 {
	w1, err := api.PostWorkspace(ctx, &apiv1.PostWorkspaceRequest{Name: testWorkspaceName})
	require.NoError(t, err)
	w2, err := api.PostWorkspace(ctx, &apiv1.PostWorkspaceRequest{Name: testWorkspace2Name})
	require.NoError(t, err)

	return []int32{w1.Workspace.Id, w2.Workspace.Id}
}

func cleanupWorkspaces(ctx context.Context) {
	workspaces := []string{testWorkspaceName, testWorkspace2Name}
	_, err := db.Bun().NewDelete().Table("workspaces").
		Where("name IN (?)", bun.In(workspaces)).
		Exec(ctx)
	if err != nil {
		logrus.Errorf("Error deleting the following workspaces: %s", workspaces)
	}
}

func TestPostBindingFails(t *testing.T) {
	api, _, ctx := setupAPITest(t, nil)
	var mockRM mocks.ResourceManager
	api.m.rm = &mockRM

	defer func() { cleanupWorkspaces(ctx) }()

	_ = setupWorkspaces(ctx, t, api)

	// TODO (eliu): add some tests for workspaceIDs
	// test resource pools on workspaces that do not exist
	mockRM.On("GetDefaultComputeResourcePool", mock.Anything, mock.Anything).
		Return(rm.ResourcePoolName(""), nil).Once()
	mockRM.On("GetDefaultAuxResourcePool", mock.Anything, mock.Anything).
		Return(rm.ResourcePoolName(""), nil).Once()
	_, err := api.BindRPToWorkspace(ctx, &apiv1.BindRPToWorkspaceRequest{
		ResourcePoolName: testPoolName,
		WorkspaceNames:   []string{"nonexistent_workspace"},
	})
	require.ErrorContains(t, err, "the following workspaces do not exist: [nonexistent_workspace]")

	// test resource pool doesn't exist
	mockRM.On("GetDefaultComputeResourcePool", mock.Anything, mock.Anything).
		Return(rm.ResourcePoolName(""), nil).Once()
	mockRM.On("GetDefaultAuxResourcePool", mock.Anything, mock.Anything).
		Return(rm.ResourcePoolName(""), nil).Twice()
	mockRM.On("GetResourcePools", mock.Anything, mock.Anything).
		Return(&apiv1.GetResourcePoolsResponse{
			ResourcePools: []*resourcepoolv1.ResourcePool{},
		}, nil).Once()
	_, err = api.BindRPToWorkspace(ctx, &apiv1.BindRPToWorkspaceRequest{
		ResourcePoolName: testPoolName,
		WorkspaceNames:   []string{testWorkspaceName, testWorkspace2Name},
	})

	require.ErrorContains(t, err, "pool with name testRP doesn't exist")

	// test resource pool is a default resource pool
	mockRM.On("GetDefaultComputeResourcePool", mock.Anything, mock.Anything).
		Return(rm.ResourcePoolName(testPoolName), nil).Twice()

	_, err = api.BindRPToWorkspace(ctx, &apiv1.BindRPToWorkspaceRequest{
		ResourcePoolName: testPoolName,
		WorkspaceNames:   []string{testWorkspaceName, testWorkspace2Name},
	})

	require.ErrorContains(t, err, "default resource pool testRP cannot be bound to any workspace")

	mockRM.On("GetDefaultAuxResourcePool", mock.Anything, mock.Anything).
		Return(rm.ResourcePoolName(testPoolName), nil).Once()

	_, err = api.BindRPToWorkspace(ctx, &apiv1.BindRPToWorkspaceRequest{
		ResourcePoolName: testPoolName,
		WorkspaceNames:   []string{testWorkspaceName, testWorkspace2Name},
	})

	require.ErrorContains(t, err, "default resource pool testRP cannot be bound to any workspace")

	// test no resource pool specified
	mockRM.On("GetDefaultComputeResourcePool", mock.Anything, mock.Anything).
		Return(rm.ResourcePoolName(testPoolName), nil).Once()
	mockRM.On("GetDefaultAuxResourcePool", mock.Anything, mock.Anything).
		Return(rm.ResourcePoolName(testPoolName), nil).Once()
	mockRM.On("GetResourcePools", mock.Anything, mock.Anything).
		Return(&apiv1.GetResourcePoolsResponse{
			ResourcePools: []*resourcepoolv1.ResourcePool{{Name: testPoolName}},
		}, nil).Once()
	_, err = api.BindRPToWorkspace(ctx, &apiv1.BindRPToWorkspaceRequest{
		ResourcePoolName: "",
		WorkspaceNames:   []string{testWorkspaceName, testWorkspace2Name},
	})

	require.ErrorContains(t, err, "doesn't exist")

	require.True(t, mockRM.AssertExpectations(t))
}

func TestPostBindingSucceeds(t *testing.T) {
	api, _, ctx := setupAPITest(t, nil)
	var mockRM mocks.ResourceManager
	api.m.rm = &mockRM

	defer func() { cleanupWorkspaces(ctx) }()

	_ = setupWorkspaces(ctx, t, api)

	// bind first resource pool
	mockRM.On("GetDefaultComputeResourcePool", mock.Anything, mock.Anything).
		Return(rm.ResourcePoolName(""), nil).Twice()
	mockRM.On("GetDefaultAuxResourcePool", mock.Anything, mock.Anything).
		Return(rm.ResourcePoolName(""), nil).Twice()
	mockRM.On("GetResourcePools", mock.Anything, mock.Anything).
		Return(&apiv1.GetResourcePoolsResponse{
			ResourcePools: []*resourcepoolv1.ResourcePool{{Name: testPoolName}},
		}, nil).Twice()

	_, err := api.BindRPToWorkspace(ctx, &apiv1.BindRPToWorkspaceRequest{
		ResourcePoolName: testPoolName,
		WorkspaceNames:   []string{testWorkspaceName},
	})
	require.NoError(t, err)

	// test bind no workspaces specified
	_, err = api.BindRPToWorkspace(ctx, &apiv1.BindRPToWorkspaceRequest{
		ResourcePoolName: testPoolName,
		WorkspaceNames:   []string{},
	})

	require.NoError(t, err)

	require.True(t, mockRM.AssertExpectations(t))
}

func TestListWorkspacesBoundToRPFails(t *testing.T) {
	api, _, ctx := setupAPITest(t, nil)
	var mockRM mocks.ResourceManager
	api.m.rm = &mockRM

	defer func() { cleanupWorkspaces(ctx) }()

	_ = setupWorkspaces(ctx, t, api)

	// bind first workspace
	mockRM.On("GetDefaultComputeResourcePool", mock.Anything, mock.Anything).
		Return(rm.ResourcePoolName(""), nil).Once()
	mockRM.On("GetDefaultAuxResourcePool", mock.Anything, mock.Anything).
		Return(rm.ResourcePoolName(""), nil).Once()
	mockRM.On("GetResourcePools", mock.Anything, mock.Anything).
		Return(&apiv1.GetResourcePoolsResponse{
			ResourcePools: []*resourcepoolv1.ResourcePool{{Name: testPoolName}},
		}, nil).Times(3)

	_, err := api.BindRPToWorkspace(ctx, &apiv1.BindRPToWorkspaceRequest{
		ResourcePoolName: testPoolName,
		WorkspaceNames:   []string{testWorkspaceName},
	})
	require.NoError(t, err)

	// fail listing a non-existent resource pool
	_, err = api.ListWorkspacesBoundToRP(ctx, &apiv1.ListWorkspacesBoundToRPRequest{
		ResourcePoolName: "nonExistentRP",
	})
	require.Error(t, err)

	_, err = api.ListWorkspacesBoundToRP(ctx, &apiv1.ListWorkspacesBoundToRPRequest{})
	require.Error(t, err)

	require.True(t, mockRM.AssertExpectations(t))
}

func TestListWorkspacesBoundToRPSucceeds(t *testing.T) {
	api, _, ctx := setupAPITest(t, nil)
	var mockRM mocks.ResourceManager
	api.m.rm = &mockRM

	defer func() { cleanupWorkspaces(ctx) }()

	workspaceIDs := setupWorkspaces(ctx, t, api)

	// test bind resource pool to workspace
	mockRM.On("GetDefaultComputeResourcePool", mock.Anything, mock.Anything).
		Return(rm.ResourcePoolName(""), nil).Once()
	mockRM.On("GetDefaultAuxResourcePool", mock.Anything, mock.Anything).
		Return(rm.ResourcePoolName(""), nil).Once()
	mockRM.On("GetResourcePools", mock.Anything, mock.Anything).
		Return(&apiv1.GetResourcePoolsResponse{
			ResourcePools: []*resourcepoolv1.ResourcePool{{Name: testPoolName}},
		}, nil).Twice()

	_, err := api.BindRPToWorkspace(ctx, &apiv1.BindRPToWorkspaceRequest{
		ResourcePoolName: testPoolName,
		WorkspaceNames:   []string{testWorkspaceName},
	})
	require.NoError(t, err)

	// test listing the workspace that was just bound
	resp, err := api.ListWorkspacesBoundToRP(ctx, &apiv1.ListWorkspacesBoundToRPRequest{
		ResourcePoolName: testPoolName,
	})
	require.NoError(t, err)
	require.Len(t, resp.WorkspaceIds, 1)
	require.Equal(t, workspaceIDs[0], resp.WorkspaceIds[0])

	// test listing on resource pool that has no bindings
	mockRM.On("GetResourcePools", mock.Anything, mock.Anything).
		Return(&apiv1.GetResourcePoolsResponse{
			ResourcePools: []*resourcepoolv1.ResourcePool{
				{Name: testPoolName}, {Name: testPool2Name},
			},
		}, nil).Once()
	resp, err = api.ListWorkspacesBoundToRP(ctx, &apiv1.ListWorkspacesBoundToRPRequest{
		ResourcePoolName: testPool2Name,
	})
	require.NoError(t, err)
	require.Zero(t, len(resp.WorkspaceIds))

	require.True(t, mockRM.AssertExpectations(t))
}

func TestPatchBindingsSucceeds(t *testing.T) {
	api, _, ctx := setupAPITest(t, nil)
	var mockRM mocks.ResourceManager
	api.m.rm = &mockRM

	defer func() { cleanupWorkspaces(ctx) }()

	workspaceIDs := setupWorkspaces(ctx, t, api)

	// setup first binding
	mockRM.On("GetDefaultComputeResourcePool", mock.Anything, mock.Anything).
		Return(rm.ResourcePoolName(""), nil).Times(4)
	mockRM.On("GetDefaultAuxResourcePool", mock.Anything, mock.Anything).
		Return(rm.ResourcePoolName(""), nil).Times(4)
	mockRM.On("GetResourcePools", mock.Anything, mock.Anything).
		Return(&apiv1.GetResourcePoolsResponse{
			ResourcePools: []*resourcepoolv1.ResourcePool{{Name: testPoolName}},
		}, nil).Times(7)

	_, err := api.BindRPToWorkspace(ctx, &apiv1.BindRPToWorkspaceRequest{
		ResourcePoolName: testPoolName,
		WorkspaceNames:   []string{testWorkspaceName},
	})
	require.NoError(t, err)

	// test patch binding with empty workspaces
	_, err = api.OverwriteRPWorkspaceBindings(ctx, &apiv1.OverwriteRPWorkspaceBindingsRequest{
		ResourcePoolName: testPoolName,
		WorkspaceNames:   []string{},
	})
	require.NoError(t, err)

	resp, err := api.ListWorkspacesBoundToRP(ctx, &apiv1.ListWorkspacesBoundToRPRequest{
		ResourcePoolName: testPoolName,
	})
	require.NoError(t, err)
	require.Zero(t, len(resp.WorkspaceIds))

	// test patch binding with different workspace
	_, err = api.OverwriteRPWorkspaceBindings(ctx, &apiv1.OverwriteRPWorkspaceBindingsRequest{
		ResourcePoolName: testPoolName,
		WorkspaceNames:   []string{testWorkspace2Name},
	})
	require.NoError(t, err)
	resp, err = api.ListWorkspacesBoundToRP(ctx, &apiv1.ListWorkspacesBoundToRPRequest{
		ResourcePoolName: testPoolName,
	})
	require.NoError(t, err)
	require.Len(t, resp.WorkspaceIds, 1)
	require.Equal(t, workspaceIDs[1], resp.WorkspaceIds[0])

	// test patch binding with different workspaceID
	_, err = api.OverwriteRPWorkspaceBindings(ctx, &apiv1.OverwriteRPWorkspaceBindingsRequest{
		ResourcePoolName: testPoolName,
		WorkspaceIds:     []int32{workspaceIDs[0]},
		WorkspaceNames:   []string{testWorkspace2Name},
	})
	require.NoError(t, err)
	resp, err = api.ListWorkspacesBoundToRP(ctx, &apiv1.ListWorkspacesBoundToRPRequest{
		ResourcePoolName: testPoolName,
	})
	require.NoError(t, err)
	require.Len(t, resp.WorkspaceIds, 2)
	expectedIds := set.FromSlice[int32](workspaceIDs)
	for _, id := range resp.WorkspaceIds {
		require.True(t, expectedIds.Contains(id))
	}

	require.True(t, mockRM.AssertExpectations(t))
}

func TestDeleteBindingsSucceeds(t *testing.T) {
	api, _, ctx := setupAPITest(t, nil)
	var mockRM mocks.ResourceManager
	api.m.rm = &mockRM

	defer func() { cleanupWorkspaces(ctx) }()

	workspaceIDs := setupWorkspaces(ctx, t, api)

	// TODO: fix all comments
	// setup first binding
	mockRM.On("GetDefaultComputeResourcePool", mock.Anything, mock.Anything).
		Return(rm.ResourcePoolName(""), nil).Times(1)
	mockRM.On("GetDefaultAuxResourcePool", mock.Anything, mock.Anything).
		Return(rm.ResourcePoolName(""), nil).Times(1)
	mockRM.On("GetResourcePools", mock.Anything, mock.Anything).
		Return(&apiv1.GetResourcePoolsResponse{
			ResourcePools: []*resourcepoolv1.ResourcePool{{Name: testPoolName}},
		}, nil).Times(3)

	_, err := api.BindRPToWorkspace(ctx, &apiv1.BindRPToWorkspaceRequest{
		ResourcePoolName: testPoolName,
		WorkspaceNames:   []string{testWorkspaceName, testWorkspace2Name},
	})
	require.NoError(t, err)

	_, err = api.UnbindRPFromWorkspace(ctx, &apiv1.UnbindRPFromWorkspaceRequest{
		ResourcePoolName: testPoolName,
		WorkspaceIds:     []int32{workspaceIDs[0]},
	})
	require.NoError(t, err)

	listReq := &apiv1.ListWorkspacesBoundToRPRequest{ResourcePoolName: testPoolName}
	resp, err := api.ListWorkspacesBoundToRP(ctx, listReq)
	require.NoError(t, err)
	require.Len(t, resp.WorkspaceIds, 1)
	require.Equal(t, workspaceIDs[1], resp.WorkspaceIds[0])

	_, err = api.UnbindRPFromWorkspace(ctx, &apiv1.UnbindRPFromWorkspaceRequest{
		ResourcePoolName: testPoolName,
		WorkspaceNames:   []string{testWorkspace2Name},
	})
	require.NoError(t, err)

	resp, err = api.ListWorkspacesBoundToRP(ctx, listReq)
	require.NoError(t, err)
	require.Zero(t, len(resp.WorkspaceIds))

	require.True(t, mockRM.AssertExpectations(t))
}

func TestGetResourcePoolsMarksUsable(t *testing.T) {
	var names []string
	mockRM := MockRM()
	// Fresh pools for every request, as the resource managers build them.
	mockRM.On("GetResourcePools").Return(func() *apiv1.GetResourcePoolsResponse {
		resp := &apiv1.GetResourcePoolsResponse{}
		for _, name := range names {
			resp.ResourcePools = append(resp.ResourcePools, &resourcepoolv1.ResourcePool{Name: name})
		}
		return resp
	}, nil)
	api, admin, adminCtx := setupAPITest(t, nil, mockRM)
	u1 := db.RequireMockUser(t, api.m.db)
	u2 := db.RequireMockUser(t, api.m.db)
	public := accessTestPool(t, "a-public", admin, false)
	granted := accessTestPool(t, "b-granted", admin, true, u1)
	adminsOnly := accessTestPool(t, "c-admins-only", admin, true)
	// The resource manager lists the pools out of order; the response is sorted by name.
	names = []string{adminsOnly, public, granted}
	usable := func(resp *apiv1.GetResourcePoolsResponse) map[string]bool {
		out := map[string]bool{}
		for _, pool := range resp.ResourcePools {
			out[pool.Name] = pool.Usable
		}
		return out
	}
	listed := func(resp *apiv1.GetResourcePoolsResponse) []string {
		out := []string{}
		for _, pool := range resp.ResourcePools {
			out = append(out, pool.Name)
		}
		return out
	}
	all := []string{public, granted, adminsOnly}

	u2Ctx := ntscUserCtx(t, u2)
	ctxs := map[string]context.Context{
		"granted user": ntscUserCtx(t, u1), "other user": u2Ctx, "admin": adminCtx,
	}
	wants := map[string]map[string]bool{
		"granted user": {public: true, granted: true, adminsOnly: false},
		"other user":   {public: true, granted: false, adminsOnly: false},
		"admin":        {public: true, granted: true, adminsOnly: true},
	}
	for name, want := range wants {
		// Every pool is listed, restricted or not, with whether the user may use it.
		resp, err := api.GetResourcePools(ctxs[name], &apiv1.GetResourcePoolsRequest{})
		require.NoError(t, err, name)
		require.Equal(t, all, listed(resp), name)
		require.Equal(t, want, usable(resp), name)
		require.Equal(t, int32(len(all)), resp.Pagination.Total, name)

		resp, err = api.GetResourcePools(ctxs[name], &apiv1.GetResourcePoolsRequest{Unbound: true})
		require.NoError(t, err, name)
		require.Subset(t, all, listed(resp), name)
		for pool, u := range usable(resp) {
			require.Equal(t, want[pool], u, name)
		}
	}

	// Pagination counts the pools that are not usable too.
	resp, err := api.GetResourcePools(u2Ctx, &apiv1.GetResourcePoolsRequest{Offset: 1, Limit: 1})
	require.NoError(t, err)
	require.Equal(t, []string{granted}, listed(resp))
	require.False(t, resp.ResourcePools[0].Usable)
	require.Equal(t, int32(3), resp.Pagination.Total)

	// Submitting to a listed pool that the user may not use is still refused.
	require.Equal(t, codes.PermissionDenied,
		status.Code(poolaccess.CanUseResourcePool(u2Ctx, u2, granted)))

	// With hide_unusable_pools, the list leaves out the pools that the user may not use.
	api.m.config.ResourcePoolAccess.HideUnusablePools = true
	t.Cleanup(func() { api.m.config.ResourcePoolAccess.HideUnusablePools = false })
	for name, want := range map[string][]string{
		"granted user": {public, granted},
		"other user":   {public},
		"admin":        all,
	} {
		resp, err := api.GetResourcePools(ctxs[name], &apiv1.GetResourcePoolsRequest{})
		require.NoError(t, err, name)
		require.Equal(t, want, listed(resp), name)
		for _, pool := range resp.ResourcePools {
			require.True(t, pool.Usable, name)
		}
		require.Equal(t, int32(len(want)), resp.Pagination.Total, name)
	}
	resp, err = api.GetResourcePools(u2Ctx, &apiv1.GetResourcePoolsRequest{Offset: 1, Limit: 1})
	require.NoError(t, err)
	require.Empty(t, resp.ResourcePools)
	require.Equal(t, int32(1), resp.Pagination.Total)

	// A failed read fails the request, whether unusable pools are hidden or not: no pool is
	// reported usable.
	poolaccess.SetReaderForTest(t, func(context.Context, model.UserID, []string) (map[string]bool, error) {
		return nil, fmt.Errorf("the database went away")
	})
	for _, hide := range []bool{true, false} {
		api.m.config.ResourcePoolAccess.HideUnusablePools = hide
		_, err = api.GetResourcePools(u2Ctx, &apiv1.GetResourcePoolsRequest{})
		require.Equal(t, codes.Unavailable, status.Code(err), hide)
		resp, err = api.GetResourcePools(adminCtx, &apiv1.GetResourcePoolsRequest{})
		require.NoError(t, err, hide)
		require.Equal(t, all, listed(resp), hide)
		require.Equal(t, wants["admin"], usable(resp), hide)
	}
}
