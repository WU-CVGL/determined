//go:build integration
// +build integration

package internal

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"gopkg.in/guregu/null.v3"

	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/user"
	"github.com/determined-ai/determined/master/internal/userusage"
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

// usageAllocation is an allocation row with its QUEUED task_stats rows. An allocation with an
// assignment is metered from assigned to released; one without is from before metering.
type usageAllocation struct {
	pool       string
	slots      int
	kind       userusage.Kind
	assigned   string
	released   string
	queuedEnds []string
	start      string
	end        string
	state      model.AllocationState
}

// addUsageAllocation adds the allocation and its QUEUED rows; times are "2006-01-02 15:04" UTC.
// The quantity is one aux container, the slots otherwise, and none for an unclassified allocation
// from before metering.
func addUsageAllocation(t *testing.T, taskID model.TaskID, n int, a usageAllocation) {
	insertUsageAllocation(t, db.Bun(), taskID, n, a)
}

// insertUsageAllocation adds the allocation and its QUEUED rows as addUsageAllocation does, with
// idb.
func insertUsageAllocation(
	t *testing.T, idb bun.IDB, taskID model.TaskID, n int, a usageAllocation,
) {
	ctx := context.TODO()
	at := func(s string) *time.Time {
		if s == "" {
			return nil
		}
		v, err := time.Parse("2006-01-02 15:04", s)
		require.NoError(t, err)
		return &v
	}
	var kind *string
	var quantity *int
	switch {
	case a.kind == "":
	case a.kind == userusage.KindAux:
		kind, quantity = ptrs.Ptr(string(a.kind)), ptrs.Ptr(1)
	case a.kind == userusage.KindUnclassified && a.assigned == "":
		kind = ptrs.Ptr(string(a.kind))
	default:
		kind, quantity = ptrs.Ptr(string(a.kind)), ptrs.Ptr(a.slots)
	}
	allocationID := model.AllocationID(fmt.Sprintf("%s.%d", taskID, n))
	_, err := idb.NewInsert().Model(&model.Allocation{
		AllocationID:    allocationID,
		TaskID:          taskID,
		Slots:           a.slots,
		ResourcePool:    a.pool,
		StartTime:       at(a.start),
		EndTime:         at(a.end),
		State:           &a.state,
		Ports:           map[string]int{},
		AssignedTime:    at(a.assigned),
		ReleasedTime:    at(a.released),
		MeteredKind:     kind,
		MeteredQuantity: quantity,
	}).Exec(ctx)
	require.NoError(t, err)
	for _, q := range a.queuedEnds {
		end := at(q)
		_, err := idb.NewInsert().Model(&model.TaskStats{
			AllocationID: allocationID,
			EventType:    "QUEUED",
			StartTime:    ptrs.Ptr(end.Add(-time.Hour)),
			EndTime:      end,
		}).Exec(ctx)
		require.NoError(t, err)
	}
}

// userUsageEntry is the part of an entry the tests compare, in hours.
type userUsageEntry struct {
	period                               string
	gpu, cpu, aux, unclassified          float64
	gpuActive, auxActive, gpuOrAuxActive float64
	gpuDays, auxDays, gpuOrAuxDays       int32
}

// usageEntries converts the entries to hours.
func usageEntries(entries []*apiv1.UserUsageEntry) []userUsageEntry {
	result := make([]userUsageEntry, 0, len(entries))
	for _, e := range entries {
		result = append(result, userUsageEntry{
			period: e.PeriodStart,
			gpu:    e.GpuSeconds / 3600, cpu: e.CpuSlotSeconds / 3600, aux: e.AuxContainerSeconds / 3600,
			unclassified: e.UnclassifiedSlotSeconds / 3600,
			gpuActive:    e.GpuActiveSeconds / 3600, auxActive: e.AuxActiveSeconds / 3600,
			gpuOrAuxActive: e.GpuOrAuxActiveSeconds / 3600,
			gpuDays:        e.GpuActiveDays, auxDays: e.AuxActiveDays, gpuOrAuxDays: e.GpuOrAuxActiveDays,
		})
	}
	return result
}

// gpuHours is a day's entry with GPUs only: the GPU-hours and the active hours.
func gpuHours(period string, gpu, active float64) userUsageEntry {
	return userUsageEntry{
		period: period, gpu: gpu, gpuActive: active, gpuOrAuxActive: active, gpuDays: 1, gpuOrAuxDays: 1,
	}
}

// setupUserUsageTest adds two users with jobs, tasks, allocations and QUEUED rows in March 2020,
// and returns the API, a context of a third user who is not an admin, and the two users. The
// resource manager reports nothing: the history does not depend on it.
func setupUserUsageTest(t *testing.T) (*apiServer, context.Context, model.User, model.User) {
	api, _, _ := setupAPITest(t, nil, &mocks.ResourceManager{})
	_, ctx := userUsageLogin(t, api)
	owner := db.RequireMockUser(t, api.m.db)
	other := db.RequireMockUser(t, api.m.db)

	ownerJob := db.RequireMockJob(t, api.m.db, &owner.ID)
	cmd := addUsageTask(t, ownerJob, model.TaskTypeCommand)
	terminated := model.AllocationStateTerminated
	gpu, cpu, aux := userusage.KindGPU, userusage.KindCPU, userusage.KindAux
	for i, a := range []usageAllocation{
		// 0: From before metering, across midnight, restored once: the first QUEUED row counts.
		{
			pool: "gpu", slots: 1, kind: gpu, queuedEnds: []string{"2020-03-01 20:00", "2020-03-02 01:00"},
			start: "2020-03-01 20:05", end: "2020-03-02 04:00", state: terminated,
		},
		// 1: From before metering, killed before pulling.
		{pool: "gpu", slots: 1, kind: gpu, queuedEnds: []string{"2020-03-02 10:00"}, state: terminated},
		// 2: From before metering, closed at a master start without holding resources.
		{
			pool: "gpu", slots: 1, kind: gpu, queuedEnds: []string{"2020-03-02 11:00"},
			start: "2020-03-02 12:00", end: "2020-03-02 12:00", state: terminated,
		},
		// 3: From before metering, an aux container.
		{
			pool: "gpu", kind: aux, queuedEnds: []string{"2020-03-02 06:00"},
			start: "2020-03-02 06:01", end: "2020-03-02 07:00", state: terminated,
		},
		// 4: CPU slots across the US daylight saving time change of 2020-03-08, released at the
		// last heartbeat of a master that stopped.
		{
			pool: "cpu", slots: 2, kind: cpu, assigned: "2020-03-07 22:00", released: "2020-03-09 02:00",
			start: "2020-03-07 22:00", end: "2020-03-09 02:00", state: terminated,
		},
		// 5: From before task_stats: no QUEUED row.
		{pool: "gpu", slots: 1, kind: gpu, start: "2020-03-09 10:00", end: "2020-03-09 11:00", state: terminated},
		// 6: Assigned before the range, released just after its start.
		{
			pool: "gpu", slots: 1, kind: gpu, assigned: "2020-02-29 22:00", released: "2020-03-01 03:00",
			start: "2020-02-29 22:01", end: "2020-03-01 03:00", state: terminated,
		},
		// 7: Assigned before midnight, pulled after it: counts from the assignment.
		{
			pool: "gpu", slots: 1, kind: gpu, assigned: "2020-03-01 23:30", released: "2020-03-02 01:00",
			start: "2020-03-02 00:30", end: "2020-03-02 01:00", state: terminated,
		},
		// 8, 9: Released exactly at midnight, and a minute after it.
		{
			pool: "gpu", slots: 1, kind: gpu, assigned: "2020-03-04 22:00", released: "2020-03-05 00:00",
			start: "2020-03-04 22:00", end: "2020-03-05 00:00", state: terminated,
		},
		{
			pool: "gpu", slots: 1, kind: gpu, assigned: "2020-03-04 23:00", released: "2020-03-05 00:01",
			start: "2020-03-04 23:00", end: "2020-03-05 00:01", state: terminated,
		},
		// 10: Assigned exactly at midnight.
		{
			pool: "gpu", slots: 1, kind: gpu, assigned: "2020-03-06 00:00", released: "2020-03-06 01:00",
			start: "2020-03-06 00:00", end: "2020-03-06 01:00", state: terminated,
		},
		// 11, 12: From before metering, no QUEUED row, a few hours before the end of a day.
		{pool: "gpu", slots: 1, kind: gpu, start: "2020-03-09 20:00", end: "2020-03-09 22:00", state: terminated},
		{pool: "gpu", slots: 1, kind: gpu, start: "2020-03-10 20:00", end: "2020-03-10 23:00", state: terminated},
		// 13: Assigned, then canceled before pulling.
		{
			pool: "gpu", slots: 2, kind: gpu, assigned: "2020-03-10 10:00", released: "2020-03-10 10:30",
			state: terminated,
		},
		// 14: Ended without a release: never counts.
		{pool: "gpu", slots: 1, kind: gpu, assigned: "2020-03-03 10:00", state: terminated},
		// 15, 16: From before metering, of a pool whose slot type was never known: the slots are
		// unclassified, and an allocation without slots counts nothing.
		{
			pool: "gone", slots: 3, kind: userusage.KindUnclassified, queuedEnds: []string{"2020-03-03 08:00"},
			start: "2020-03-03 08:00", end: "2020-03-03 09:00", state: terminated,
		},
		{
			pool: "gone", kind: userusage.KindUnclassified, queuedEnds: []string{"2020-03-03 08:00"},
			start: "2020-03-03 08:00", end: "2020-03-03 09:00", state: terminated,
		},
		// 17: Not metered (Kubernetes without slots).
		{
			pool: "k8s", kind: userusage.KindNone, assigned: "2020-03-03 08:00", released: "2020-03-03 09:00",
			state: terminated,
		},
		// 18: An aux container while a GPU is held, and 19: one alone.
		{
			pool: "gpu", kind: aux, assigned: "2020-03-01 21:00", released: "2020-03-01 22:00",
			start: "2020-03-01 21:00", end: "2020-03-01 22:00", state: terminated,
		},
		{
			pool: "gpu", kind: aux, assigned: "2020-03-03 12:00", released: "2020-03-03 13:00",
			start: "2020-03-03 12:00", end: "2020-03-03 13:00", state: terminated,
		},
	} {
		addUsageAllocation(t, cmd, i, a)
	}
	gc := addUsageTask(t, ownerJob, model.TaskTypeCheckpointGC)
	addUsageAllocation(t, gc, 0, usageAllocation{
		pool: "gpu", kind: aux, assigned: "2020-03-02 13:00", released: "2020-03-02 15:00",
		start: "2020-03-02 13:00", end: "2020-03-02 15:00", state: terminated,
	})

	otherJob := db.RequireMockJob(t, api.m.db, &other.ID)
	otherTask := addUsageTask(t, otherJob, model.TaskTypeCommand)
	addUsageAllocation(t, otherTask, 0, usageAllocation{
		pool: "gpu", slots: 4, kind: gpu, queuedEnds: []string{"2020-03-01 00:00"},
		start: "2020-03-01 00:00", end: "2020-03-01 08:00", state: terminated,
	})
	return api, ctx, owner, other
}

// usageMinutes converts minutes to hours as the entries do, from seconds.
func usageMinutes(n int) float64 {
	return float64(n*60) / 3600
}

// userUsageDailyMarch2020 is the owner's usage from 2020-03-01 to 2020-03-10.
var userUsageDailyMarch2020 = []userUsageEntry{
	{
		period: "2020-03-01", gpu: 4 + 3 + 0.5, aux: 1, gpuActive: 4 + 3, auxActive: 1, gpuOrAuxActive: 4 + 3,
		gpuDays: 1, auxDays: 1, gpuOrAuxDays: 1,
	},
	{
		period: "2020-03-02", gpu: 4 + 1, aux: 1, gpuActive: 4, auxActive: 1, gpuOrAuxActive: 5,
		gpuDays: 1, auxDays: 1, gpuOrAuxDays: 1,
	},
	{
		period: "2020-03-03", aux: 1, unclassified: 3, auxActive: 1, gpuOrAuxActive: 1,
		auxDays: 1, gpuOrAuxDays: 1,
	},
	gpuHours("2020-03-04", 2+1, 2),
	gpuHours("2020-03-05", usageMinutes(1), usageMinutes(1)),
	gpuHours("2020-03-06", 1, 1),
	{period: "2020-03-07", cpu: 2 * 2},
	{period: "2020-03-08", cpu: 2 * 24},
	{
		period: "2020-03-09", gpu: 1 + 2, cpu: 2 * 2, gpuActive: 1 + 2, gpuOrAuxActive: 1 + 2,
		gpuDays: 1, gpuOrAuxDays: 1,
	},
	gpuHours("2020-03-10", 3+2*0.5, 3+0.5),
}

// otherUsageDailyMarch2020 is the other user's usage from 2020-03-01 to 2020-03-10.
var otherUsageDailyMarch2020 = []userUsageEntry{
	gpuHours("2020-03-01", 4*8, 8),
	{period: "2020-03-02"},
	{period: "2020-03-03"},
	{period: "2020-03-04"},
	{period: "2020-03-05"},
	{period: "2020-03-06"},
	{period: "2020-03-07"},
	{period: "2020-03-08"},
	{period: "2020-03-09"},
	{period: "2020-03-10"},
}

// requireUserUsageDaily checks the user's daily usage from 2020-03-01 to 2020-03-10, read as
// one range and one day at a time, so that each day's range cuts through the allocations.
func requireUserUsageDaily(
	ctx context.Context, t *testing.T, api *apiServer, userID model.UserID, want []userUsageEntry,
) {
	read := func(start, end string) []userUsageEntry {
		resp, err := api.GetUserUsage(ctx, &apiv1.GetUserUsageRequest{
			UserId: int32(userID), StartDate: start, EndDate: end, Period: usageDaily,
		})
		require.NoError(t, err)
		require.NotNil(t, resp.AsOf)
		require.NotEmpty(t, resp.CoverageStart)
		require.LessOrEqual(t, resp.CoverageStart, start)
		return usageEntries(resp.Entries)
	}
	require.Equal(t, want, read("2020-03-01", "2020-03-10"))
	for _, e := range want {
		require.Equal(t, []userUsageEntry{e}, read(e.period, e.period), e.period)
	}
}

func TestUserUsage(t *testing.T) {
	api, ctx, owner, other := setupUserUsageTest(t)
	requireUserUsageDaily(ctx, t, api, owner.ID, userUsageDailyMarch2020)
	requireUserUsageDaily(ctx, t, api, other.ID, otherUsageDailyMarch2020)

	// The read keeps the allocations that overlap the range; it leaves out the one killed before
	// pulling, which is not metered, the one released exactly at the range start and the one
	// assigned exactly at its end.
	rows, err := readUserAllocations(context.TODO(), db.Bun(), owner.ID,
		time.Date(2020, 3, 5, 0, 0, 0, 0, time.UTC), time.Date(2020, 3, 6, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	var read []string
	for _, r := range rows {
		read = append(read, r.AllocationID[strings.LastIndex(r.AllocationID, "."):])
	}
	require.Equal(t, []string{".9"}, read)
	require.NotContains(t, read, ".1")
	require.NotContains(t, read, ".8")
	require.NotContains(t, read, ".10")

	resp, err := api.GetUserUsage(ctx, &apiv1.GetUserUsageRequest{
		UserId: int32(owner.ID), StartDate: "2020-03", EndDate: "2020-04", Period: usageMonthly,
	})
	require.NoError(t, err)
	require.Equal(t, []userUsageEntry{
		{
			period: "2020-03", gpu: usageMinutes(23.5*60 + 1), cpu: 2*2 + 2*24 + 2*2, aux: 3, unclassified: 3,
			gpuActive: usageMinutes(20.5*60 + 1), auxActive: 3, gpuOrAuxActive: usageMinutes(22.5*60 + 1),
			gpuDays: 7, auxDays: 3, gpuOrAuxDays: 8,
		},
		{period: "2020-04"},
	}, usageEntries(resp.Entries))

	// The history is keyed by the user's ID.
	_, err = db.Bun().NewUpdate().Table("users").Set("username = ?", uuid.NewString()).
		Where("id = ?", owner.ID).Exec(context.TODO())
	require.NoError(t, err)
	requireUserUsageDaily(ctx, t, api, owner.ID, userUsageDailyMarch2020)

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
	api, ctx, owner, other := setupUserUsageTest(t)

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
			requireUserUsageDaily(ctx, t, api, owner.ID, userUsageDailyMarch2020)
			requireUserUsageDaily(ctx, t, api, other.ID, otherUsageDailyMarch2020)

			var got string
			require.NoError(t, db.Bun().NewRaw("SHOW TIME ZONE").Scan(context.TODO(), &got))
			require.Equal(t, tz, got)
		})
	}
}

// userUsageScratchTx begins a transaction in which the allocations and task_stats tables are empty
// copies in a scratch schema. It is rolled back when the test ends.
func userUsageScratchTx(ctx context.Context, t *testing.T) bun.Tx {
	tx, err := db.Bun().BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tx.Rollback()) })
	schema := "user_usage_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	for _, stmt := range []string{
		"CREATE SCHEMA " + schema,
		"CREATE TABLE " + schema + ".allocations (LIKE public.allocations INCLUDING ALL)",
		"CREATE TABLE " + schema + ".task_stats (LIKE public.task_stats INCLUDING ALL)",
		"SET LOCAL search_path = " + schema + ", public",
	} {
		_, err := tx.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	return tx
}

// The first allocation of an empty database, assigned at 23:50 and pulling at 00:10, counts 10
// minutes on its first day, which is the coverage start from its assignment on. The database is
// emptied in a scratch schema of a transaction that is rolled back.
func TestUserUsageEmptyDatabase(t *testing.T) {
	ctx := context.Background()
	api, _, _ := setupAPITest(t, nil, &mocks.ResourceManager{})
	owner := db.RequireMockUser(t, api.m.db)
	task := addUsageTask(t, db.RequireMockJob(t, api.m.db, &owner.ID), model.TaskTypeCommand)
	tx := userUsageScratchTx(ctx, t)

	coverage := &userUsageCoverage{}
	first, err := coverage.get(ctx, tx)
	require.NoError(t, err)
	require.Nil(t, first, "no allocation")

	assigned := time.Date(2021, 6, 1, 23, 50, 0, 0, time.UTC)
	a := &model.Allocation{
		AllocationID:    model.AllocationID(fmt.Sprintf("%s.0", task)),
		TaskID:          task,
		Slots:           1,
		ResourcePool:    "gpu",
		State:           ptrs.Ptr(model.AllocationStateAssigned),
		Ports:           map[string]int{},
		AssignedTime:    &assigned,
		MeteredKind:     ptrs.Ptr(string(userusage.KindGPU)),
		MeteredQuantity: ptrs.Ptr(1),
	}
	_, err = tx.NewInsert().Model(a).Exec(ctx)
	require.NoError(t, err)

	// Before pulling.
	first, err = coverage.get(ctx, tx)
	require.NoError(t, err)
	require.Equal(t, ptrs.Ptr(time.Date(2021, 6, 1, 0, 0, 0, 0, time.UTC)), first)

	// Pulling at 00:10, released at 01:00.
	_, err = tx.NewUpdate().Model(a).
		Set("start_time = ?, released_time = ?, end_time = ?, state = ?",
			assigned.Add(20*time.Minute), assigned.Add(70*time.Minute), assigned.Add(70*time.Minute),
			model.AllocationStateTerminated).
		WherePK().Exec(ctx)
	require.NoError(t, err)
	for _, c := range []*userUsageCoverage{coverage, {}} {
		got, err := c.get(ctx, tx)
		require.NoError(t, err)
		require.Equal(t, first, got)
	}

	rangeStart, rangeEnd, err := parseUsageRange("2021-05-31", "2021-06-02", usageDaily)
	require.NoError(t, err)
	asOf := time.Now().UTC()
	from, to, ok := usageEntryBounds(rangeStart, rangeEnd, first, asOf, usageDaily)
	require.True(t, ok)
	rows, err := readUserAllocations(ctx, tx, owner.ID, rangeStart, rangeEnd)
	require.NoError(t, err)
	require.Equal(t, []userUsageEntry{
		gpuHours("2021-06-01", usageMinutes(10), usageMinutes(10)),
		gpuHours("2021-06-02", 1, 1),
	}, usageEntries(userUsage(rows, from, to, asOf, usageDaily)))
}

// The coverage starts with the first allocation that is metered. Older allocations that the
// metering skips do not move it: one of a kind that is not metered, one from before metering
// closed at a master start without ever holding resources, ones from before metering that ended
// without an end or with only a QUEUED end, one from before metering without slots whose kind is
// not known, and assigned ones released at their assignment or ended without a release. The read
// skips exactly the allocations that userusage.Intervals gives no interval.
func TestUserUsageCoverageSkipsUnmetered(t *testing.T) {
	ctx := context.Background()
	api, _, _ := setupAPITest(t, nil, &mocks.ResourceManager{})
	owner := db.RequireMockUser(t, api.m.db)
	task := addUsageTask(t, db.RequireMockJob(t, api.m.db, &owner.ID), model.TaskTypeCommand)
	tx := userUsageScratchTx(ctx, t)

	terminated, running := model.AllocationStateTerminated, model.AllocationStateRunning
	gpu := userusage.KindGPU
	skipped := []usageAllocation{
		{
			pool: "k8s", kind: userusage.KindNone, assigned: "2021-01-01 10:00",
			released: "2021-01-01 11:00", start: "2021-01-01 10:00", end: "2021-01-01 11:00", state: terminated,
		},
		{
			pool: "gpu", slots: 1, kind: gpu, queuedEnds: []string{"2021-01-02 10:00"},
			start: "2021-01-02 10:00", end: "2021-01-02 10:00", state: terminated,
		},
		{
			pool: "gpu", slots: 1, kind: gpu, queuedEnds: []string{"2021-01-03 10:00"},
			start: "2021-01-03 10:05", state: terminated,
		},
		{pool: "gpu", slots: 1, kind: gpu, queuedEnds: []string{"2021-01-04 10:00"}, state: terminated},
		{
			pool: "gone", kind: userusage.KindUnclassified, queuedEnds: []string{"2021-01-05 10:00"},
			start: "2021-01-05 10:00", end: "2021-01-05 11:00", state: terminated,
		},
		{
			pool: "gpu", slots: 1, kind: gpu, assigned: "2021-01-06 10:00", released: "2021-01-06 10:00",
			state: terminated,
		},
		{pool: "gpu", slots: 1, kind: gpu, assigned: "2021-01-07 10:00", state: terminated},
	}
	for i, a := range skipped {
		insertUsageAllocation(t, tx, task, i, a)
	}
	first, err := readUsageCoverage(ctx, tx)
	require.NoError(t, err)
	require.Nil(t, first, "only allocations that are not metered")

	metered := []usageAllocation{
		{
			pool: "gpu", slots: 1, kind: gpu, assigned: "2021-06-01 23:50", released: "2021-06-02 01:00",
			start: "2021-06-02 00:10", end: "2021-06-02 01:00", state: terminated,
		},
		{
			pool: "gpu", slots: 2, kind: gpu, queuedEnds: []string{"2021-06-03 10:00"},
			start: "2021-06-03 10:05", end: "2021-06-03 11:00", state: terminated,
		},
		{pool: "cpu", slots: 1, kind: userusage.KindCPU, assigned: "2021-06-04 10:00", state: running},
		{
			pool: "gone", slots: 1, kind: userusage.KindUnclassified, queuedEnds: []string{"2021-06-05 10:00"},
			state: running,
		},
	}
	for i, a := range metered {
		insertUsageAllocation(t, tx, task, len(skipped)+i, a)
	}
	first, err = readUsageCoverage(ctx, tx)
	require.NoError(t, err)
	require.NotNil(t, first)
	require.True(t, time.Date(2021, 6, 1, 23, 50, 0, 0, time.UTC).Equal(*first), "coverage %s", first)
	day, err := (&userUsageCoverage{}).get(ctx, tx)
	require.NoError(t, err)
	require.Equal(t, ptrs.Ptr(time.Date(2021, 6, 1, 0, 0, 0, 0, time.UTC)), day)

	var rows []struct {
		userAllocationRow
		Metered bool `bun:"metered"`
	}
	require.NoError(t, tx.NewRaw(`
SELECT `+userAllocationColumns+`, `+meteredSQL+` AS metered
FROM allocations a`+heldSQL).Scan(ctx, &rows))
	require.Len(t, rows, len(skipped)+len(metered))
	asOf := time.Now().UTC()
	n := 0
	for _, r := range rows {
		intervals := userusage.Intervals([]userusage.Record{r.record()}, asOf)
		require.Equal(t, len(intervals) == 1, r.Metered, r.AllocationID)
		if r.Metered {
			n++
		}
	}
	require.Equal(t, len(metered), n)
}

// After a master restart, before the agents reconnect, finished history keeps its kind: a new
// allocation by its assigned devices, and one from before metering by the kind its pool's slot
// type gave it once, whatever the pool reports later.
func TestUserUsageRestartBeforeAgentsReconnect(t *testing.T) {
	ctx := context.Background()
	api, _, userCtx := setupAPITest(t, nil, &mocks.ResourceManager{})
	owner := db.RequireMockUser(t, api.m.db)
	job := db.RequireMockJob(t, api.m.db, &owner.ID)
	pool := "reconnect-" + uuid.NewString()[:8]
	terminated := model.AllocationStateTerminated
	// CPU slots assigned at 10:00 and released at the stopped master's last heartbeat at 11:00.
	addUsageAllocation(t, addUsageTask(t, job, model.TaskTypeCommand), 0, usageAllocation{
		pool: pool, slots: 2, kind: userusage.KindCPU, assigned: "2020-05-01 10:00",
		released: "2020-05-01 11:00", start: "2020-05-01 10:00", end: "2020-05-01 11:00", state: terminated,
	})
	// From before metering, not classified yet.
	addUsageAllocation(t, addUsageTask(t, job, model.TaskTypeCommand), 0, usageAllocation{
		pool: pool, slots: 2, kind: userusage.KindUnclassified, queuedEnds: []string{"2020-05-02 10:00"},
		start: "2020-05-02 10:00", end: "2020-05-02 11:00", state: terminated,
	})

	read := func() []userUsageEntry {
		resp, err := api.GetUserUsage(userCtx, &apiv1.GetUserUsageRequest{
			UserId: int32(owner.ID), StartDate: "2020-05-01", EndDate: "2020-05-02", Period: usageDaily,
		})
		require.NoError(t, err)
		return usageEntries(resp.Entries)
	}
	cpuDay := userUsageEntry{period: "2020-05-01", cpu: 2}
	require.Equal(t, []userUsageEntry{cpuDay, {period: "2020-05-02", unclassified: 2}}, read())

	var classifierRM mocks.ResourceManager
	report := func(slotType devicev1.Type, agents int32) {
		classifierRM.On("GetResourcePools").Return(&apiv1.GetResourcePoolsResponse{
			ResourcePools: []*resourcepoolv1.ResourcePool{{
				Name: pool, Type: resourcepoolv1.ResourcePoolType_RESOURCE_POOL_TYPE_STATIC,
				SlotType: slotType, NumAgents: agents,
			}},
		}, nil).Once()
	}
	c := &legacyClassifier{rm: &classifierRM, stableTicks: 1, pools: map[string]*legacyPoolObservation{}}
	for _, step := range []struct {
		slotType devicev1.Type
		agents   int32
		want     userUsageEntry
	}{
		{devicev1.Type_TYPE_UNSPECIFIED, 0, userUsageEntry{period: "2020-05-02", unclassified: 2}},
		{devicev1.Type_TYPE_CPU, 1, userUsageEntry{period: "2020-05-02", cpu: 2}},
		{devicev1.Type_TYPE_UNSPECIFIED, 0, userUsageEntry{period: "2020-05-02", cpu: 2}},
		{devicev1.Type_TYPE_CUDA, 2, userUsageEntry{period: "2020-05-02", cpu: 2}},
	} {
		report(step.slotType, step.agents)
		_, err := c.step(ctx)
		require.NoError(t, err)
		require.Equal(t, []userUsageEntry{cpuDay, step.want}, read(), step.slotType)
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
		summary(k8s, "k8s", k8sSummary(device.CUDA, 3), k8sSummary(device.CUDA, 0)),
		summary(queued, "gpu"),
		summary(otherTask, "gpu", containerSummary("a3", devices(device.CUDA, 1)...)),
	} {
		summaries[s.AllocationID] = s
	}
	api.m.rm = userUsageMockRM(summaries)

	resp, err := api.GetUserResources(ctx, &apiv1.GetUserResourcesRequest{UserId: int32(owner.ID)})
	require.NoError(t, err)
	require.NotNil(t, resp.AsOf)
	require.True(t, resp.AuxMetered)
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

	// Aux containers are metered unless every pool is a Kubernetes one.
	for _, c := range []struct {
		pools []*resourcepoolv1.ResourcePool
		want  bool
	}{
		{nil, false},
		{[]*resourcepoolv1.ResourcePool{{Name: "k8s", Type: resourcepoolv1.ResourcePoolType_RESOURCE_POOL_TYPE_K8S}}, false},
		{[]*resourcepoolv1.ResourcePool{
			{Name: "k8s", Type: resourcepoolv1.ResourcePoolType_RESOURCE_POOL_TYPE_K8S},
			{Name: "aws", Type: resourcepoolv1.ResourcePoolType_RESOURCE_POOL_TYPE_AWS},
		}, true},
	} {
		var rm mocks.ResourceManager
		rm.On("GetResourcePools").Return(&apiv1.GetResourcePoolsResponse{ResourcePools: c.pools}, nil)
		rm.On("GetAllocationSummaries").Return(summaries, nil)
		api.m.rm = &rm
		resp, err = api.GetUserResources(ctx, &apiv1.GetUserResourcesRequest{UserId: int32(other.ID)})
		require.NoError(t, err)
		require.Equal(t, c.want, resp.AuxMetered, "%v", c.pools)
	}

	_, err = api.GetUserResources(ctx, &apiv1.GetUserResourcesRequest{UserId: 1 << 30})
	require.Equal(t, codes.NotFound, status.Code(err), err)
	_, err = api.GetUserResources(context.TODO(), &apiv1.GetUserResourcesRequest{UserId: int32(owner.ID)})
	require.Equal(t, codes.Unauthenticated, status.Code(err), err)
}
