//go:build integration
// +build integration

package internal

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"gopkg.in/guregu/null.v3"

	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/user"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/devicev1"
	"github.com/determined-ai/determined/proto/pkg/resourcepoolv1"
)

// userUsageMockRM is a resource manager with a CUDA, a CPU and a Kubernetes pool, holding the
// given allocations.
func userUsageMockRM(
	summaries map[model.AllocationID]sproto.AllocationSummary,
) *mocks.ResourceManager {
	var rm mocks.ResourceManager
	rm.On("GetResourcePools").Return(&apiv1.GetResourcePoolsResponse{
		ResourcePools: []*resourcepoolv1.ResourcePool{
			{
				Name: "gpu", Type: resourcepoolv1.ResourcePoolType_RESOURCE_POOL_TYPE_STATIC,
				SlotType: devicev1.Type_TYPE_CUDA,
			},
			{
				Name: "cpu", Type: resourcepoolv1.ResourcePoolType_RESOURCE_POOL_TYPE_STATIC,
				SlotType: devicev1.Type_TYPE_CPU,
			},
			{
				Name: "k8s", Type: resourcepoolv1.ResourcePoolType_RESOURCE_POOL_TYPE_K8S,
				SlotType: devicev1.Type_TYPE_CUDA,
			},
		},
	}, nil)
	rm.On("GetAllocationSummaries").Return(summaries, nil)
	return &rm
}

// userUsageLogin adds a user who is not an admin and returns a context with their session.
func userUsageLogin(t *testing.T, api *apiServer) (model.User, context.Context) {
	u := model.User{
		Username:     uuid.NewString(),
		PasswordHash: null.NewString("", false),
		Active:       true,
	}
	id, err := user.Add(context.TODO(), &u, nil)
	require.NoError(t, err)
	u.ID = id
	resp, err := api.Login(context.TODO(), &apiv1.LoginRequest{Username: u.Username})
	require.NoError(t, err)
	return u, metadata.NewIncomingContext(context.TODO(),
		metadata.Pairs("x-user-token", "Bearer "+resp.Token))
}

// addUsageTask adds a task of a type in the job.
func addUsageTask(t *testing.T, jobID model.JobID, taskType model.TaskType) model.TaskID {
	task := &model.Task{
		TaskID:    model.NewTaskID(),
		JobID:     &jobID,
		TaskType:  taskType,
		StartTime: time.Now().UTC().Truncate(time.Millisecond),
	}
	require.NoError(t, db.AddTask(context.TODO(), task))
	return task.TaskID
}

// usageAllocation is an allocation row with its QUEUED task_stats rows.
type usageAllocation struct {
	pool       string
	slots      int
	queuedEnds []string
	start      string
	end        string
	state      model.AllocationState
}

// addUsageAllocation adds the allocation and its QUEUED rows; times are "2006-01-02 15:04" UTC.
func addUsageAllocation(t *testing.T, taskID model.TaskID, n int, a usageAllocation) {
	ctx := context.TODO()
	at := func(s string) *time.Time {
		if s == "" {
			return nil
		}
		v, err := time.Parse("2006-01-02 15:04", s)
		require.NoError(t, err)
		return &v
	}
	allocationID := model.AllocationID(fmt.Sprintf("%s.%d", taskID, n))
	_, err := db.Bun().NewInsert().Model(&model.Allocation{
		AllocationID: allocationID,
		TaskID:       taskID,
		Slots:        a.slots,
		ResourcePool: a.pool,
		StartTime:    at(a.start),
		EndTime:      at(a.end),
		State:        &a.state,
		Ports:        map[string]int{},
	}).Exec(ctx)
	require.NoError(t, err)
	for _, q := range a.queuedEnds {
		end := at(q)
		require.NoError(t, db.RecordTaskStats(ctx, &model.TaskStats{
			AllocationID: allocationID,
			EventType:    "QUEUED",
			StartTime:    ptrs.Ptr(end.Add(-time.Hour)),
			EndTime:      end,
		}))
	}
}

// userUsageEntry is the part of an entry the tests compare, in hours.
type userUsageEntry struct {
	period        string
	gpu, cpu, aux float64
	active        float64
	activeDays    int32
}

// usageEntries converts the entries to hours.
func usageEntries(entries []*apiv1.UserUsageEntry) []userUsageEntry {
	result := make([]userUsageEntry, 0, len(entries))
	for _, e := range entries {
		aux := -1.0
		if e.AuxContainerSeconds != nil {
			aux = *e.AuxContainerSeconds / 3600
		}
		result = append(result, userUsageEntry{
			period: e.PeriodStart, gpu: e.GpuSeconds / 3600, cpu: e.CpuSlotSeconds / 3600, aux: aux,
			active: e.ActiveSeconds / 3600, activeDays: e.ActiveDays,
		})
	}
	return result
}

// setupUserUsageTest adds two users with jobs, tasks, allocations and QUEUED rows in March 2020,
// and returns the API, a context of a third user who is not an admin, and the two users.
func setupUserUsageTest(t *testing.T) (*apiServer, context.Context, model.User, model.User) {
	api, _, _ := setupAPITest(t, nil, userUsageMockRM(nil))
	_, ctx := userUsageLogin(t, api)
	owner := db.RequireMockUser(t, api.m.db)
	other := db.RequireMockUser(t, api.m.db)

	ownerJob := db.RequireMockJob(t, api.m.db, &owner.ID)
	cmd := addUsageTask(t, ownerJob, model.TaskTypeCommand)
	terminated := model.AllocationStateTerminated
	for i, a := range []usageAllocation{
		// Across midnight, restored once: the first QUEUED row counts.
		{
			pool: "gpu", slots: 1, queuedEnds: []string{"2020-03-01 20:00", "2020-03-02 01:00"},
			start: "2020-03-01 20:05", end: "2020-03-02 04:00", state: terminated,
		},
		// Killed before pulling.
		{pool: "gpu", slots: 1, queuedEnds: []string{"2020-03-02 10:00"}, state: terminated},
		// Closed at a master start without holding resources.
		{
			pool: "gpu", slots: 1, queuedEnds: []string{"2020-03-02 11:00"},
			start: "2020-03-02 12:00", end: "2020-03-02 12:00", state: terminated,
		},
		// An Aux container.
		{
			pool: "gpu", queuedEnds: []string{"2020-03-02 06:00"},
			start: "2020-03-02 06:01", end: "2020-03-02 07:00", state: terminated,
		},
		// Across the US daylight saving time change of 2020-03-08.
		{
			pool: "cpu", slots: 2, queuedEnds: []string{"2020-03-07 22:00"},
			start: "2020-03-07 22:00", end: "2020-03-09 02:00", state: terminated,
		},
		// Before task_stats: no QUEUED row.
		{pool: "gpu", slots: 1, start: "2020-03-09 10:00", end: "2020-03-09 11:00", state: terminated},
	} {
		addUsageAllocation(t, cmd, i, a)
	}
	gc := addUsageTask(t, ownerJob, model.TaskTypeCheckpointGC)
	addUsageAllocation(t, gc, 0, usageAllocation{
		pool: "gpu", queuedEnds: []string{"2020-03-02 13:00"},
		start: "2020-03-02 13:00", end: "2020-03-02 15:00", state: terminated,
	})

	otherJob := db.RequireMockJob(t, api.m.db, &other.ID)
	otherTask := addUsageTask(t, otherJob, model.TaskTypeCommand)
	addUsageAllocation(t, otherTask, 0, usageAllocation{
		pool: "gpu", slots: 4, queuedEnds: []string{"2020-03-01 00:00"},
		start: "2020-03-01 00:00", end: "2020-03-01 08:00", state: terminated,
	})
	return api, ctx, owner, other
}

// userUsageDailyMarch2020 is the owner's usage from 2020-03-01 to 2020-03-10.
var userUsageDailyMarch2020 = []userUsageEntry{
	{"2020-03-01", 4, 0, 0, 4, 1},
	{"2020-03-02", 4, 0, 1, 5, 1},
	{"2020-03-03", 0, 0, 0, 0, 0},
	{"2020-03-04", 0, 0, 0, 0, 0},
	{"2020-03-05", 0, 0, 0, 0, 0},
	{"2020-03-06", 0, 0, 0, 0, 0},
	{"2020-03-07", 0, 2 * 2, 0, 2, 1},
	{"2020-03-08", 0, 2 * 24, 0, 24, 1},
	{"2020-03-09", 1, 2 * 2, 0, 3, 1},
	{"2020-03-10", 0, 0, 0, 0, 0},
}

func TestUserUsage(t *testing.T) {
	api, ctx, owner, other := setupUserUsageTest(t)
	daily := func(userID model.UserID) []userUsageEntry {
		resp, err := api.GetUserUsage(ctx, &apiv1.GetUserUsageRequest{
			UserId: int32(userID), StartDate: "2020-03-01", EndDate: "2020-03-10", Period: usageDaily,
		})
		require.NoError(t, err)
		require.NotNil(t, resp.AsOf)
		require.NotEmpty(t, resp.CoverageStart)
		require.LessOrEqual(t, resp.CoverageStart, "2020-03-01")
		return usageEntries(resp.Entries)
	}

	require.Equal(t, userUsageDailyMarch2020, daily(owner.ID))
	otherDaily := daily(other.ID)
	require.Len(t, otherDaily, 10)
	require.Equal(t, userUsageEntry{"2020-03-01", 4 * 8, 0, 0, 8, 1}, otherDaily[0])
	for _, e := range otherDaily[1:] {
		require.Equal(t, userUsageEntry{period: e.period}, e)
	}

	resp, err := api.GetUserUsage(ctx, &apiv1.GetUserUsageRequest{
		UserId: int32(owner.ID), StartDate: "2020-03", EndDate: "2020-04", Period: usageMonthly,
	})
	require.NoError(t, err)
	require.Equal(t, []userUsageEntry{
		{"2020-03", 4 + 4 + 1, 2*2 + 2*24 + 2*2, 1, 4 + 5 + 2 + 24 + 3, 5},
		{"2020-04", 0, 0, 0, 0, 0},
	}, usageEntries(resp.Entries))

	// The history is keyed by the user's ID.
	_, err = db.Bun().NewUpdate().Table("users").Set("username = ?", uuid.NewString()).
		Where("id = ?", owner.ID).Exec(context.TODO())
	require.NoError(t, err)
	require.Equal(t, userUsageDailyMarch2020, daily(owner.ID))

	_, err = api.GetUserUsage(ctx, &apiv1.GetUserUsageRequest{
		UserId: 1 << 30, StartDate: "2020-03-01", EndDate: "2020-03-10", Period: usageDaily,
	})
	require.Equal(t, codes.NotFound, status.Code(err), err)
	_, err = api.GetUserUsage(ctx, &apiv1.GetUserUsageRequest{
		UserId: int32(owner.ID), StartDate: "2020-03-10", EndDate: "2020-03-01", Period: usageDaily,
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err), err)
	_, err = api.GetUserUsage(context.TODO(), &apiv1.GetUserUsageRequest{
		UserId: int32(owner.ID), StartDate: "2020-03-01", EndDate: "2020-03-10", Period: usageDaily,
	})
	require.Equal(t, codes.Unauthenticated, status.Code(err), err)
}

func TestUserUsageSessionTimeZone(t *testing.T) {
	api, ctx, owner, _ := setupUserUsageTest(t)

	// Pin the pool to one connection so that the session time zone applies to the RPC's reads.
	sqlDB := db.Bun().DB
	maxOpen := sqlDB.Stats().MaxOpenConnections
	sqlDB.SetMaxOpenConns(1)
	defer func() {
		_, err := db.Bun().ExecContext(context.TODO(), "RESET TIME ZONE")
		require.NoError(t, err)
		sqlDB.SetMaxOpenConns(maxOpen)
	}()

	for _, tz := range []string{"Asia/Shanghai", "America/Los_Angeles"} {
		t.Run(tz, func(t *testing.T) {
			_, err := db.Bun().ExecContext(context.TODO(), fmt.Sprintf("SET TIME ZONE '%s'", tz))
			require.NoError(t, err)
			resp, err := api.GetUserUsage(ctx, &apiv1.GetUserUsageRequest{
				UserId: int32(owner.ID), StartDate: "2020-03-01", EndDate: "2020-03-10", Period: usageDaily,
			})
			require.NoError(t, err)
			require.Equal(t, userUsageDailyMarch2020, usageEntries(resp.Entries))

			var got string
			require.NoError(t, db.Bun().NewRaw("SHOW TIME ZONE").Scan(context.TODO(), &got))
			require.Equal(t, tz, got)
		})
	}
}

func TestUserResources(t *testing.T) {
	api, _, _ := setupAPITest(t, nil, userUsageMockRM(nil))
	_, ctx := userUsageLogin(t, api)
	owner := db.RequireMockUser(t, api.m.db)
	other := db.RequireMockUser(t, api.m.db)

	ownerJob := db.RequireMockJob(t, api.m.db, &owner.ID)
	trial := addUsageTask(t, ownerJob, model.TaskTypeTrial)
	gc := addUsageTask(t, ownerJob, model.TaskTypeCheckpointGC)
	cmd := addUsageTask(t, db.RequireMockJob(t, api.m.db, &owner.ID), model.TaskTypeCommand)
	shell := addUsageTask(t, db.RequireMockJob(t, api.m.db, &owner.ID), model.TaskTypeShell)
	k8s := addUsageTask(t, db.RequireMockJob(t, api.m.db, &owner.ID), model.TaskTypeGeneric)
	queued := addUsageTask(t, db.RequireMockJob(t, api.m.db, &owner.ID), model.TaskTypeGeneric)
	otherTask := addUsageTask(t, db.RequireMockJob(t, api.m.db, &other.ID), model.TaskTypeCommand)

	summary := func(taskID model.TaskID, pool string, r ...sproto.ResourcesSummary) sproto.AllocationSummary {
		return sproto.AllocationSummary{
			TaskID: taskID, AllocationID: model.AllocationID(taskID + ".0"), ResourcePool: pool, Resources: r,
		}
	}
	summaries := map[model.AllocationID]sproto.AllocationSummary{}
	for _, s := range []sproto.AllocationSummary{
		summary(trial, "gpu", containerSummary("a1", devices(device.CUDA, 4)...),
			containerSummary("a2", devices(device.CUDA, 4)...)),
		summary(gc, "gpu", containerSummary("a1")),
		summary(cmd, "gpu", containerSummary("a1")),
		summary(shell, "cpu", containerSummary("c1", devices(device.CPU, 2)...)),
		summary(k8s, "k8s", k8sSummary(3), k8sSummary(0)),
		summary(queued, "gpu"),
		summary(otherTask, "gpu", containerSummary("a3", devices(device.CUDA, 1)...)),
	} {
		summaries[s.AllocationID] = s
	}
	api.m.rm = userUsageMockRM(summaries)

	resp, err := api.GetUserResources(ctx, &apiv1.GetUserResourcesRequest{UserId: int32(owner.ID)})
	require.NoError(t, err)
	require.NotNil(t, resp.AsOf)
	require.Equal(t, []*apiv1.UserPoolResources{
		{ResourcePool: "cpu", Counts: &apiv1.UserResourceCounts{CpuSlots: 2, AuxContainers: ptrs.Ptr(int32(0))}},
		{ResourcePool: "gpu", Counts: &apiv1.UserResourceCounts{Gpus: 8, AuxContainers: ptrs.Ptr(int32(1))}},
		{ResourcePool: "k8s", Counts: &apiv1.UserResourceCounts{Gpus: 3}},
	}, resp.Pools)

	resp, err = api.GetUserResources(ctx, &apiv1.GetUserResourcesRequest{UserId: int32(other.ID)})
	require.NoError(t, err)
	require.Equal(t, []*apiv1.UserPoolResources{
		{ResourcePool: "gpu", Counts: &apiv1.UserResourceCounts{Gpus: 1, AuxContainers: ptrs.Ptr(int32(0))}},
	}, resp.Pools)

	_, err = api.GetUserResources(ctx, &apiv1.GetUserResourcesRequest{UserId: 1 << 30})
	require.Equal(t, codes.NotFound, status.Code(err), err)
	_, err = api.GetUserResources(context.TODO(), &apiv1.GetUserResourcesRequest{UserId: int32(owner.ID)})
	require.Equal(t, codes.Unauthenticated, status.Code(err), err)
}
