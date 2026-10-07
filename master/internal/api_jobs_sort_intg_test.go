//go:build integration
// +build integration

package internal

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/guregu/null.v3"

	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/tasks"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/commonv1"
	"github.com/determined-ai/determined/proto/pkg/rbacv1"
)

// jobsSortFixtureExperimentKeys are the experiment sorts of the fixture's keys.
var jobsSortFixtureExperimentKeys = map[string]apiv1.GetExperimentsRequest_SortBy{
	"name":       apiv1.GetExperimentsRequest_SORT_BY_NAME,
	"owner":      apiv1.GetExperimentsRequest_SORT_BY_USER,
	"pool":       apiv1.GetExperimentsRequest_SORT_BY_RESOURCE_POOL,
	"slots":      apiv1.GetExperimentsRequest_SORT_BY_SLOTS,
	"stateGroup": apiv1.GetExperimentsRequest_SORT_BY_STATE_GROUP,
	"start":      apiv1.GetExperimentsRequest_SORT_BY_START_TIME,
	"end":        apiv1.GetExperimentsRequest_SORT_BY_END_TIME,
}

// addJobsSortUser adds a user with this display name ("" for none, nil for NULL) and a username
// that starts with the prefix.
func addJobsSortUser(
	ctx context.Context, t *testing.T, usernamePrefix string, displayName *string,
) model.User {
	t.Helper()
	user := model.User{Username: usernamePrefix + uuid.NewString(), Active: true}
	if displayName != nil {
		user.DisplayName = null.StringFrom(*displayName)
	}
	id, err := db.HackAddUser(ctx, &user)
	require.NoError(t, err)
	user.ID = id
	return user
}

// addJobsSortOwners adds a user for each owner display name of the fixture.
func addJobsSortOwners(
	ctx context.Context, t *testing.T, fixture jobsSortFixture,
) map[string]model.UserID {
	t.Helper()
	owners := map[string]model.UserID{}
	for _, row := range fixture.Rows {
		if row.Owner == nil {
			continue
		}
		if _, ok := owners[*row.Owner]; !ok {
			owners[*row.Owner] = addJobsSortUser(ctx, t, "jobsort-", row.Owner).ID
		}
	}
	return owners
}

// updateJobsSortExperiment sets an experiment's fields that the Jobs page sorts by. A nil pool
// leaves resources.resource_pool out of the config, and nil slots resources.slots_per_trial.
func updateJobsSortExperiment(
	ctx context.Context, t *testing.T, id int, owner model.UserID, name string, pool *string,
	slotsPerTrial *int, state string, start time.Time, end *time.Time,
) {
	t.Helper()
	config := "jsonb_set(config, '{name}', to_jsonb(?::text))"
	args := []any{name}
	if slotsPerTrial == nil {
		config = "(" + config + ") #- '{resources,slots_per_trial}'"
	} else {
		config = "jsonb_set(" + config + ", '{resources,slots_per_trial}', to_jsonb(?::int))"
		args = append(args, *slotsPerTrial)
	}
	if pool == nil {
		config = "(" + config + ") #- '{resources,resource_pool}'"
	} else {
		config = "jsonb_set(" + config + ", '{resources,resource_pool}', to_jsonb(?::text))"
		args = append(args, *pool)
	}
	args = append(args, owner, state, start, end, id)
	_, err := db.Bun().NewRaw("UPDATE experiments SET config = "+config+
		", owner_id = ?, state = ?, start_time = ?, end_time = ? WHERE id = ?", args...).Exec(ctx)
	require.NoError(t, err)
}

// addJobsSortGenericTask adds a generic task with this ID and the fields that the Jobs page sorts
// by. A nil owner or state leaves it out; a nil pool leaves resources.resource_pool out.
func addJobsSortGenericTask(
	ctx context.Context, t *testing.T, taskID model.TaskID, owner *model.UserID,
	workspaceID, projectID int, name string, pool *string, slots int, state *model.TaskState,
	start time.Time, end *time.Time,
) {
	t.Helper()
	jobID := model.NewJobID()
	require.NoError(t, db.AddJob(&model.Job{
		JobID: jobID, JobType: model.JobTypeGeneric, OwnerID: owner,
	}))
	require.NoError(t, db.AddTask(ctx, &model.Task{
		TaskID: taskID, TaskType: model.TaskTypeGeneric, JobID: &jobID, StartTime: start,
		State: state, NoPause: ptrs.Ptr(false),
	}))
	_, err := db.Bun().NewRaw("UPDATE tasks SET end_time = ? WHERE task_id = ?", end, taskID).
		Exec(ctx)
	require.NoError(t, err)
	allocationID := model.AllocationID(taskID.String() + ".0")
	require.NoError(t, db.AddAllocation(ctx, &model.Allocation{
		AllocationID: allocationID, TaskID: taskID, Slots: slots, ResourcePool: "default",
		StartTime: &start, Ports: map[string]int{},
	}))
	spec := tasks.GenericTaskSpec{WorkspaceID: workspaceID, ProjectID: projectID, JobID: jobID}
	spec.GenericTaskConfig.Name = name
	spec.GenericTaskConfig.Resources.RawResourcePool = pool
	spec.GenericTaskConfig.Resources.RawSlots = &slots
	require.NoError(t, persistGenericTaskSpec(ctx, taskID, spec, allocationID))
}

func TestGetExperimentsJobsSortFixture(t *testing.T) {
	api, curUser, ctx := setupAPITest(t, nil)
	_, projectID := createProjectAndWorkspace(ctx, t, api)
	fixture := loadJobsSortFixture(t)
	owners := addJobsSortOwners(ctx, t, fixture)

	// The last row first, so that a row listed earlier has a higher ID and wins a tie.
	labels := map[int32]string{}
	for i := len(fixture.Rows) - 1; i >= 0; i-- {
		row := fixture.Rows[i]
		if row.GenericTaskOnly {
			continue
		}
		slots := ptrs.Ptr(row.Slots)
		if row.SlotsDefault {
			require.Equal(t, 1, row.Slots, row.ID)
			slots = nil
		}
		exp := createTestExpWithProjectID(t, api, curUser, projectID)
		updateJobsSortExperiment(ctx, t, exp.ID, owners[*row.Owner], row.Name, row.Pool, slots,
			*row.State, row.Start, row.End)
		labels[int32(exp.ID)] = row.ID
	}

	list := func(sortBy apiv1.GetExperimentsRequest_SortBy, orderBy apiv1.OrderBy,
		offset, limit int32,
	) ([]string, int32) {
		t.Helper()
		resp, err := api.GetExperiments(ctx, &apiv1.GetExperimentsRequest{
			ProjectId: int32(projectID), SortBy: sortBy, OrderBy: orderBy,
			Offset: offset, Limit: limit,
		})
		require.NoError(t, err)
		out := []string{}
		for _, exp := range resp.Experiments {
			out = append(out, labels[exp.Id])
		}
		return out, resp.Pagination.Total
	}
	for key, sortBy := range jobsSortFixtureExperimentKeys {
		for _, orderBy := range []apiv1.OrderBy{apiv1.OrderBy_ORDER_BY_ASC, apiv1.OrderBy_ORDER_BY_DESC} {
			want := fixture.Orders[key].Ascend
			if orderBy == apiv1.OrderBy_ORDER_BY_DESC {
				want = fixture.Orders[key].Descend
			}
			want = fixture.experimentOrder(want)
			got, _ := list(sortBy, orderBy, 0, -1)
			require.Equal(t, want, got, "%s %s", key, orderBy)

			// Pages put together are the whole list: no run twice, none left out.
			var paged []string
			for offset := int32(0); offset < int32(len(want)); offset += 5 {
				page, total := list(sortBy, orderBy, offset, 5)
				require.Equal(t, int32(len(want)), total)
				paged = append(paged, page...)
			}
			require.Equal(t, want, paged, "%s %s by pages", key, orderBy)
		}
	}
}

func TestGetGenericTasksJobsSortFixture(t *testing.T) {
	api, _, ctx := setupAPITest(t, nil)
	workspaceID, projectID := createProjectAndWorkspace(ctx, t, api)
	fixture := loadJobsSortFixture(t)
	owners := addJobsSortOwners(ctx, t, fixture)

	// Task IDs that sort in the rows' order, so that a row listed earlier wins a tie.
	prefix := "jobsort-" + uuid.NewString()
	labels := map[string]string{}
	for i, row := range fixture.Rows {
		taskID := model.TaskID(fmt.Sprintf("%s-%02d", prefix, i))
		var owner *model.UserID
		if row.Owner != nil {
			owner = ptrs.Ptr(owners[*row.Owner])
		}
		var state *model.TaskState
		if row.State != nil {
			state = ptrs.Ptr(model.TaskState(*row.State))
		}
		addJobsSortGenericTask(ctx, t, taskID, owner, workspaceID, projectID, row.Name, row.Pool,
			row.Slots, state, row.Start, row.End)
		labels[taskID.String()] = row.ID
	}

	list := func(sortBy apiv1.GetGenericTasksRequest_SortBy, orderBy apiv1.OrderBy,
		offset, limit int32,
	) ([]string, int32) {
		t.Helper()
		resp, err := api.GetGenericTasks(ctx, &apiv1.GetGenericTasksRequest{
			ProjectId: int32(projectID), SortBy: sortBy, OrderBy: orderBy,
			Offset: offset, Limit: limit,
		})
		require.NoError(t, err)
		out := []string{}
		for _, task := range resp.Tasks {
			out = append(out, labels[task.TaskId])
		}
		return out, resp.Pagination.Total
	}
	for key, sortBy := range jobsSortFixtureGenericKeys {
		for _, orderBy := range []apiv1.OrderBy{apiv1.OrderBy_ORDER_BY_ASC, apiv1.OrderBy_ORDER_BY_DESC} {
			want := fixture.Orders[key].Ascend
			if orderBy == apiv1.OrderBy_ORDER_BY_DESC {
				want = fixture.Orders[key].Descend
			}
			got, _ := list(sortBy, orderBy, 0, 0)
			require.Equal(t, want, got, "%s %s", key, orderBy)

			var paged []string
			for offset := int32(0); offset < int32(len(want)); offset += 5 {
				page, total := list(sortBy, orderBy, offset, 5)
				require.Equal(t, int32(len(want)), total)
				paged = append(paged, page...)
			}
			require.Equal(t, want, paged, "%s %s by pages", key, orderBy)
		}
	}

	// Without a sort: newest first.
	got, _ := list(apiv1.GetGenericTasksRequest_SORT_BY_UNSPECIFIED,
		apiv1.OrderBy_ORDER_BY_UNSPECIFIED, 0, 0)
	require.Equal(t, fixture.Orders["start"].Descend, got)

	// The owner's display name is listed.
	resp, err := api.GetGenericTasks(ctx, &apiv1.GetGenericTasksRequest{
		TaskIds: []string{prefix + "-00"},
	})
	require.NoError(t, err)
	require.Len(t, resp.Tasks, 1)
	require.Equal(t, *fixture.Rows[0].Owner, resp.Tasks[0].DisplayName)
}

func TestGetExperimentsJobsStateGroupsOwnersAndFilters(t *testing.T) {
	api, curUser, ctx := setupAPITest(t, nil)
	workspaceID, projectID := createProjectAndWorkspace(ctx, t, api)
	otherWorkspaceID, otherProjectID := createProjectAndWorkspace(ctx, t, api)
	thirdWorkspaceID, thirdProjectID := createProjectAndWorkspace(ctx, t, api)

	// The owner is the display name, or the username when the display name is empty or NULL.
	prefix := "jobsort-" + uuid.NewString()[:8] + "-"
	namedB := addJobsSortUser(ctx, t, prefix+"z-", ptrs.Ptr(prefix+"b"))
	emptyA := addJobsSortUser(ctx, t, prefix+"a-", ptrs.Ptr(""))
	nullC := addJobsSortUser(ctx, t, prefix+"c-", nil)

	at := func(sec int) time.Time { return time.Date(2026, 2, 1, 0, 0, sec, 0, time.UTC) }
	add := func(projectID int, owner model.User, slots int, state string, start time.Time) int32 {
		t.Helper()
		exp := createTestExpWithProjectID(t, api, curUser, projectID)
		updateJobsSortExperiment(ctx, t, exp.ID, owner.ID, "exp", ptrs.Ptr("default"), &slots, state,
			start, nil)
		return int32(exp.ID)
	}
	running := add(projectID, namedB, 0, "RUNNING", at(1))
	killing := add(projectID, emptyA, 1, "STOPPING_KILLED", at(2))
	deleteFailed := add(otherProjectID, nullC, 4, "DELETE_FAILED", at(3))
	paused := add(thirdProjectID, namedB, 8, "PAUSED", at(4))
	all := []int32{running, killing, deleteFailed, paused}

	list := func(req *apiv1.GetExperimentsRequest) []int32 {
		t.Helper()
		if req.ExperimentIdFilter == nil {
			req.ExperimentIdFilter = &commonv1.Int32FieldFilter{Incl: all}
		}
		resp, err := api.GetExperiments(ctx, req)
		require.NoError(t, err)
		out := []int32{}
		for _, exp := range resp.Experiments {
			out = append(out, exp.Id)
		}
		return out
	}
	asc, desc := apiv1.OrderBy_ORDER_BY_ASC, apiv1.OrderBy_ORDER_BY_DESC

	// State groups: running and stopping are active, DELETE_FAILED is ended. Ties: newest first.
	stateGroup := apiv1.GetExperimentsRequest_SORT_BY_STATE_GROUP
	require.Equal(t, []int32{killing, running, paused, deleteFailed},
		list(&apiv1.GetExperimentsRequest{SortBy: stateGroup, OrderBy: asc}))
	require.Equal(t, []int32{deleteFailed, paused, killing, running},
		list(&apiv1.GetExperimentsRequest{SortBy: stateGroup, OrderBy: desc}))

	// Owners: b (display name), a- (username for an empty display name), c- (NULL).
	user := apiv1.GetExperimentsRequest_SORT_BY_USER
	require.Equal(t, []int32{killing, paused, running, deleteFailed},
		list(&apiv1.GetExperimentsRequest{SortBy: user, OrderBy: asc}))
	require.Equal(t, []int32{deleteFailed, paused, running, killing},
		list(&apiv1.GetExperimentsRequest{SortBy: user, OrderBy: desc}))

	// Slots per trial, numerically.
	slots := apiv1.GetExperimentsRequest_SORT_BY_SLOTS
	require.Equal(t, []int32{paused, deleteFailed, killing, running},
		list(&apiv1.GetExperimentsRequest{SortBy: slots, OrderBy: desc}))

	// Slot filters: counts, above, either.
	require.ElementsMatch(t, []int32{running, deleteFailed},
		list(&apiv1.GetExperimentsRequest{Slots: []int32{0, 4}}))
	require.ElementsMatch(t, []int32{paused},
		list(&apiv1.GetExperimentsRequest{SlotsAbove: ptrs.Ptr(int32(4))}))
	require.ElementsMatch(t, []int32{running, paused},
		list(&apiv1.GetExperimentsRequest{Slots: []int32{0}, SlotsAbove: ptrs.Ptr(int32(4))}))
	require.ElementsMatch(t, all, list(&apiv1.GetExperimentsRequest{Slots: []int32{}}))

	// Workspaces: any of them; one that is gone is skipped; with none left, nothing.
	gone := int32(1 << 30)
	require.ElementsMatch(t, []int32{running, killing, deleteFailed},
		list(&apiv1.GetExperimentsRequest{
			WorkspaceIds: []int32{int32(workspaceID), int32(otherWorkspaceID)},
		}))
	require.ElementsMatch(t, []int32{paused}, list(&apiv1.GetExperimentsRequest{
		WorkspaceIds: []int32{int32(thirdWorkspaceID), gone},
	}))
	resp, err := api.GetExperiments(ctx, &apiv1.GetExperimentsRequest{
		WorkspaceIds:       []int32{gone},
		ExperimentIdFilter: &commonv1.Int32FieldFilter{Incl: all},
	})
	require.NoError(t, err)
	require.Empty(t, resp.Experiments)
	require.Equal(t, int32(0), resp.Pagination.Total)
	require.Empty(t, list(&apiv1.GetExperimentsRequest{
		WorkspaceId: int32(workspaceID), WorkspaceIds: []int32{int32(otherWorkspaceID)},
	}))
}

func TestAuthZGetExperimentsWorkspaceIDs(t *testing.T) {
	api, authZExp, _, curUser, ctx := setupExpAuthTest(t, nil)
	workspaceID, projectID := createProjectAndWorkspace(ctx, t, api)
	hiddenWorkspaceID, hiddenProjectID := createProjectAndWorkspace(ctx, t, api)
	exp := createTestExpWithProjectID(t, api, curUser, projectID)
	createTestExpWithProjectID(t, api, curUser, hiddenProjectID)

	mockUserArg := mock.MatchedBy(func(u model.User) bool { return u.ID == curUser.ID })
	passThrough := func() {
		resQuery := &bun.SelectQuery{}
		authZExp.On("FilterExperimentsQuery", mock.Anything, mockUserArg, mock.Anything,
			mock.Anything,
			[]rbacv1.PermissionType{rbacv1.PermissionType_PERMISSION_TYPE_VIEW_EXPERIMENT_METADATA}).
			Return(resQuery, nil).Once().Run(func(args mock.Arguments) {
			*resQuery = *args.Get(3).(*bun.SelectQuery)
		})
	}
	requested := []int32{int32(workspaceID), int32(hiddenWorkspaceID)}

	// A workspace the user cannot view is skipped.
	wAuthZ.On("FilterWorkspaceIDs", mock.Anything, mockUserArg, requested).
		Return([]int32{int32(workspaceID)}, nil).Once()
	passThrough()
	resp, err := api.GetExperiments(ctx, &apiv1.GetExperimentsRequest{WorkspaceIds: requested})
	require.NoError(t, err)
	require.Len(t, resp.Experiments, 1)
	require.Equal(t, int32(exp.ID), resp.Experiments[0].Id)

	// With none left, nothing.
	wAuthZ.On("FilterWorkspaceIDs", mock.Anything, mockUserArg, requested).
		Return([]int32{}, nil).Once()
	passThrough()
	resp, err = api.GetExperiments(ctx, &apiv1.GetExperimentsRequest{WorkspaceIds: requested})
	require.NoError(t, err)
	require.Empty(t, resp.Experiments)
	require.Equal(t, int32(0), resp.Pagination.Total)

	// The authorization's error is returned.
	wAuthZ.On("FilterWorkspaceIDs", mock.Anything, mockUserArg, requested).
		Return(nil, fmt.Errorf("lookup failed")).Once()
	_, err = api.GetExperiments(ctx, &apiv1.GetExperimentsRequest{WorkspaceIds: requested})
	require.ErrorContains(t, err, "lookup failed")
}

func TestGetGenericTasksJobsMissingValuesAndFilters(t *testing.T) {
	api, _, ctx := setupAPITest(t, nil)
	workspaceID, projectID := createProjectAndWorkspace(ctx, t, api)
	otherWorkspaceID, otherProjectID := createProjectAndWorkspace(ctx, t, api)
	prefix := "jobsort-" + uuid.NewString()[:8] + "-"
	named := addJobsSortUser(ctx, t, prefix+"z-", ptrs.Ptr(prefix+"b"))
	emptyName := addJobsSortUser(ctx, t, prefix+"a-", ptrs.Ptr(""))

	at := func(sec int) time.Time { return time.Date(2026, 3, 1, 0, 0, sec, 0, time.UTC) }
	add := func(
		suffix string, owner *model.UserID, workspaceID, projectID, slots int,
		state *model.TaskState, start time.Time,
	) string {
		t.Helper()
		taskID := model.TaskID(prefix + suffix)
		addJobsSortGenericTask(ctx, t, taskID, owner, workspaceID, projectID, "task", nil, slots,
			state, start, nil)
		return taskID.String()
	}
	active := ptrs.Ptr(model.TaskStateActive)
	noState := add("no-state", &named.ID, workspaceID, projectID, 0, nil, at(5))
	noOwner := add("no-owner", nil, workspaceID, projectID, 2, active, at(4))
	stopping := add("stopping", &emptyName.ID, workspaceID, projectID, 8,
		ptrs.Ptr(model.TaskStateStoppingPaused), at(3))
	paused := add("paused", &named.ID, workspaceID, projectID, 1,
		ptrs.Ptr(model.TaskStatePaused), at(2))
	elsewhere := add("elsewhere", &named.ID, otherWorkspaceID, otherProjectID, 1, active, at(1))

	list := func(req *apiv1.GetGenericTasksRequest) []string {
		t.Helper()
		if req.WorkspaceIds == nil {
			req.ProjectId = int32(projectID)
		}
		resp, err := api.GetGenericTasks(ctx, req)
		require.NoError(t, err)
		out := []string{}
		for _, task := range resp.Tasks {
			out = append(out, task.TaskId)
		}
		return out
	}
	asc, desc := apiv1.OrderBy_ORDER_BY_ASC, apiv1.OrderBy_ORDER_BY_DESC

	// A task without a state or an owner comes last in either order.
	stateGroup := apiv1.GetGenericTasksRequest_SORT_BY_STATE_GROUP
	require.Equal(t, []string{noOwner, stopping, paused, noState},
		list(&apiv1.GetGenericTasksRequest{SortBy: stateGroup, OrderBy: asc}))
	require.Equal(t, []string{paused, noOwner, stopping, noState},
		list(&apiv1.GetGenericTasksRequest{SortBy: stateGroup, OrderBy: desc}))
	// Owners: a- (username for an empty display name), then b (display name).
	user := apiv1.GetGenericTasksRequest_SORT_BY_USER
	require.Equal(t, []string{stopping, noState, paused, noOwner},
		list(&apiv1.GetGenericTasksRequest{SortBy: user, OrderBy: asc}))
	require.Equal(t, []string{noState, paused, stopping, noOwner},
		list(&apiv1.GetGenericTasksRequest{SortBy: user, OrderBy: desc}))

	// Slot filters: counts, above, either.
	require.ElementsMatch(t, []string{noState, paused},
		list(&apiv1.GetGenericTasksRequest{Slots: []int32{0, 1}}))
	require.ElementsMatch(t, []string{stopping},
		list(&apiv1.GetGenericTasksRequest{SlotsAbove: ptrs.Ptr(int32(2))}))
	require.ElementsMatch(t, []string{noState, stopping}, list(&apiv1.GetGenericTasksRequest{
		Slots: []int32{0}, SlotsAbove: ptrs.Ptr(int32(2)),
	}))

	// Workspaces: any of them; one that is gone is skipped; with none left, nothing.
	gone := int32(1 << 30)
	both := []int32{int32(workspaceID), int32(otherWorkspaceID)}
	require.ElementsMatch(t, []string{noState, noOwner, stopping, paused, elsewhere},
		list(&apiv1.GetGenericTasksRequest{WorkspaceIds: both, Search: prefix}))
	require.ElementsMatch(t, []string{elsewhere}, list(&apiv1.GetGenericTasksRequest{
		WorkspaceIds: []int32{int32(otherWorkspaceID), gone}, Search: prefix,
	}))
	require.Empty(t, list(&apiv1.GetGenericTasksRequest{WorkspaceIds: []int32{gone}}))

	// A workspace the user cannot view is skipped.
	nscAuthZ := &mocks.NSCAuthZ{}
	nscAuthZ.Test(t)
	command.AuthZProvider.RegisterOverride("basic", nscAuthZ)
	t.Cleanup(func() { command.AuthZProvider.RegisterOverride("basic", &command.NSCAuthZBasic{}) })
	nscAuthZ.On("AccessibleScopes", mock.Anything, mock.Anything, model.AccessScopeID(0)).
		Return(model.AccessScopeSet{model.AccessScopeID(otherWorkspaceID): true}, nil).Once()
	require.ElementsMatch(t, []string{elsewhere},
		list(&apiv1.GetGenericTasksRequest{WorkspaceIds: both, Search: prefix}))

	_, err := api.GetGenericTasks(ctx, &apiv1.GetGenericTasksRequest{SortBy: 99})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
	_, err = api.GetGenericTasks(ctx, &apiv1.GetGenericTasksRequest{OrderBy: 99})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
}
