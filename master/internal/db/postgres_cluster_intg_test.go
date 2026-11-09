//go:build integration
// +build integration

package db

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"

	"github.com/determined-ai/determined/master/pkg/etc"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
)

func TestClusterAPI(t *testing.T) {
	require.NoError(t, etc.SetRootPath(RootFromDB))

	db, closeDB := MustResolveTestPostgres(t)
	defer closeDB()
	MustMigrateTestPostgres(t, db, MigrationsFromDB)

	_, err := db.GetOrCreateClusterID("")
	require.NoError(t, err, "failed to get or create cluster id")

	_, tIn := CreateMockJobAndTask(t, db)
	tID := tIn.TaskID
	// Add an allocation
	aID := model.AllocationID(string(tID) + "-1")
	aIn := &model.Allocation{
		AllocationID: aID,
		TaskID:       tID,
		Slots:        8,
		ResourcePool: "somethingelse",
		StartTime:    ptrs.Ptr(time.Now().UTC().Truncate(time.Millisecond)),
	}

	err = AddAllocation(context.TODO(), aIn)
	require.NoError(t, err, "failed to add allocation")

	// Add a cluster heartbeat after allocation, so it is as if the master died with it open.
	currentTime := time.Now().UTC().Truncate(time.Millisecond)
	require.NoError(t, db.UpdateClusterHeartBeat(currentTime))

	var clusterHeartbeat time.Time
	err = db.sql.QueryRow("SELECT cluster_heartbeat FROM cluster_id").Scan(&clusterHeartbeat)
	require.NoError(t, err, "error reading cluster_heartbeat from cluster_id table")

	require.Equal(t, currentTime, clusterHeartbeat,
		"Retrieved cluster heartbeat doesn't match the correct time")

	// Don't complete the above allocation and call CloseOpenAllocations
	require.NoError(t, CloseOpenAllocations(context.TODO(), nil))

	// Retrieve the open allocation and check if end time is set to cluster_heartbeat
	aOut, err := AllocationByID(context.TODO(), aIn.AllocationID)
	require.NoError(t, err)
	require.NotNil(t, aOut, "aOut is Nil")
	require.NotNil(t, aOut.EndTime, "aOut.EndTime is Nil")
	require.Equal(t, *aOut.EndTime, clusterHeartbeat,
		"Expected end time of open allocation is = %q but it is = %q instead",
		clusterHeartbeat.String(), aOut.EndTime.String())
}

func CreateMockJobAndTask(t *testing.T, db *PgDB) (*model.Job, *model.Task) {
	// Add a mock user
	user := RequireMockUser(t, db)

	// Add a job
	jID := model.NewJobID()
	jIn := &model.Job{
		JobID:   jID,
		JobType: model.JobTypeExperiment,
		OwnerID: &user.ID,
	}

	err := AddJob(jIn)
	require.NoError(t, err, "failed to add job")

	// Add a task
	tID := model.NewTaskID()
	tIn := &model.Task{
		TaskID:    tID,
		JobID:     &jID,
		TaskType:  model.TaskTypeTrial,
		StartTime: time.Now().UTC().Truncate(time.Millisecond),
	}

	err = AddTask(context.TODO(), tIn)
	require.NoError(t, err, "failed to add task")

	return jIn, tIn
}

type allocAggTest struct {
	name       string
	tzQuery    string
	timeOfDay  string
	numSlots   int
	seconds    int
	queuedTime int
}

func TestUpdateResourceAllocationAggregation(t *testing.T) {
	require.NoError(t, etc.SetRootPath(RootFromDB))

	db, closeDB := MustResolveTestPostgres(t)
	defer closeDB()
	MustMigrateTestPostgres(t, db, MigrationsFromDB)

	ctx := context.Background()
	bunDB := bun.NewDB(db.sql.DB, pgdialect.New())

	today := time.Now()

	tests := []allocAggTest{
		{
			name:       "UTC basic add",
			tzQuery:    `SET TIME ZONE 'Etc/UTC'`,
			timeOfDay:  "04:20:00AM",
			numSlots:   5,
			seconds:    5,
			queuedTime: 2,
		},
		{
			name:       "Positive UTC offset",
			tzQuery:    `SET TIME ZONE 'Europe/Athens'`,
			timeOfDay:  "11:30:00PM",
			numSlots:   5,
			seconds:    10,
			queuedTime: 2,
		},
		{
			name:       "Negative UTC offset",
			tzQuery:    `SET TIME ZONE 'America/Montreal'`,
			timeOfDay:  "01:20:00AM",
			numSlots:   7,
			seconds:    2,
			queuedTime: 2,
		},
	}

	type resourceAggregate struct {
		bun.BaseModel `bun:"table:resource_aggregates"`
		Date          time.Time `bun:"date"`
		Seconds       float64   `bun:"seconds"`
	}

	for ind, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			offset := -1 * (480 - (24 * ind))
			recentDate := today.Add(time.Hour * time.Duration(offset))
			formattedDate, prevSeconds := setupUpdateResourceAllocationAggregation(ctx, t, db,
				bunDB, recentDate, test)
			err := db.UpdateResourceAllocationAggregation()
			require.NoError(t, err)

			ra := resourceAggregate{}
			err = bunDB.NewSelect().
				Model(&ra).
				Where("date = ? AND aggregation_type = ? AND aggregation_key = ?", formattedDate,
					"total", "total").
				Scan(ctx)
			require.NoError(t, err)

			var expectedSeconds interface{} = float64(test.numSlots*test.seconds) + prevSeconds
			var seconds interface{} = ra.Seconds
			var diffTooLarge interface{} = fmt.Sprintf("expected time: %v \nactual time: %v \n",
				expectedSeconds, seconds)
			require.InEpsilon(t, expectedSeconds, seconds, 0.5, diffTooLarge)

			err = bunDB.NewSelect().
				Model(&ra).
				Where("date = ? AND aggregation_type = ? AND aggregation_key = ?", formattedDate,
					"queued", "total").
				Scan(ctx)
			require.NoError(t, err)

			var expectedQueuedTime interface{} = test.queuedTime
			seconds = ra.Seconds
			diffTooLarge = fmt.Sprintf("expected queued time: %v \nactual queued "+
				" time: %v \n",
				expectedQueuedTime, seconds)
			require.InEpsilon(t, expectedQueuedTime, seconds, 0.5, diffTooLarge)
		})
	}
}

func setupUpdateResourceAllocationAggregation(ctx context.Context, t *testing.T, db *PgDB,
	bunDB *bun.DB, recentDate time.Time, test allocAggTest,
) (string, float64) {
	// (Setup) Set the timezone.
	_, err := bunDB.NewRaw(test.tzQuery).Exec(ctx)
	require.NoError(t, err)

	_, tIn := CreateMockJobAndTask(t, db)
	tID := tIn.TaskID

	formattedDate := recentDate.Format(time.DateOnly)
	yearMonthDay := strings.Split(formattedDate, "-")
	startDate := fmt.Sprintf("%s/%s %s %s +00", yearMonthDay[1], yearMonthDay[2], test.timeOfDay,
		yearMonthDay[0])

	// The total aggregated seconds from previously added allocations on the day recentDate.
	var prevSeconds float64
	err = bunDB.NewRaw(`
	WITH d AS (
    SELECT
        tsrange(
            ?::timestamp,
            (?::timestamp + interval '1 day')
        ) AS period
	),
	allocs_in_range AS (
    SELECT
        extract(
            EPOCH
            FROM
            upper(d.period * alloc.range) - lower(d.period * alloc.range)
        ) * alloc.slots::float AS seconds
    FROM
        (
            SELECT
				slots,
                tsrange(start_time, greatest(start_time, end_time)) AS range
            FROM
                allocations
            WHERE
                start_time IS NOT NULL
        ) AS alloc,
        d
    WHERE
        d.period && alloc.range
	)
	SELECT coalesce(sum(allocs_in_range.seconds), 0) FROM allocs_in_range
	`, formattedDate, formattedDate).Scan(ctx, &prevSeconds)
	require.NoError(t, err)

	// (Setup) the allocation's start and end time.
	startTime, err := time.Parse("01/02 03:04:05PM 2006 -07", startDate)
	require.NoError(t, err)
	endTime := startTime.Add(time.Second * time.Duration(test.seconds))

	allocID := uuid.NewString()
	alloc := model.Allocation{
		AllocationID: *model.NewAllocationID(&allocID),
		TaskID:       tID,
		Slots:        test.numSlots,
		ResourcePool: uuid.NewString(),
		StartTime:    &startTime,
		EndTime:      &endTime,
	}

	// The function UpdateResourceAllocationAggregation starts aggregating allocations from the day
	// after the highest date in the resource_aggregates table. Therefore, we need to ensure that
	// the highest date in resource_aggregates is less than the date on which we are adding the
	// allocation in this setup.
	_, err = bunDB.NewDelete().
		Table("resource_aggregates").
		Where("date >= ?", formattedDate).
		Exec(ctx)
	require.NoError(t, err)

	_, err = bunDB.NewInsert().Model(&alloc).Exec(ctx)
	require.NoError(t, err)

	queuedEndTime := startTime.Add(time.Second * time.Duration(test.queuedTime))
	taskStats := model.TaskStats{
		AllocationID: model.AllocationID(allocID),
		EventType:    "QUEUED",
		StartTime:    &startTime,
		EndTime:      &queuedEndTime,
	}
	_, err = bunDB.NewInsert().Model(&taskStats).Exec(ctx)
	require.NoError(t, err)

	return formattedDate, prevSeconds
}

// aggTestDay returns hour h of day d of March 2015 in UTC.
func aggTestDay(d, h int) time.Time {
	return time.Date(2015, 3, d, h, 0, 0, 0, time.UTC)
}

// aggTestRun aggregates as of 00:01 UTC on day d.
func aggTestRun(t *testing.T, db *PgDB, d int) {
	require.NoError(t, db.updateResourceAllocationAggregation(aggTestDay(d, 0).Add(time.Minute)))
}

// addAggTestAllocation adds an allocation of the task; a nil end leaves it open.
func addAggTestAllocation(
	ctx context.Context, t *testing.T, taskID model.TaskID, slots int, pool string,
	start time.Time, end *time.Time,
) model.AllocationID {
	allocID := model.AllocationID(uuid.NewString())
	_, err := Bun().NewInsert().Model(&model.Allocation{
		AllocationID: allocID,
		TaskID:       taskID,
		Slots:        slots,
		ResourcePool: pool,
		StartTime:    &start,
		EndTime:      end,
	}).Exec(ctx)
	require.NoError(t, err)
	return allocID
}

// requireSeconds requires that got equals want up to rounding.
func requireSeconds(t *testing.T, want, got float64, msgAndArgs ...any) {
	require.InDelta(t, want, got, 1e-6, msgAndArgs...)
}

// aggTestRows returns the aggregates of day d of March 2015 keyed by "type/key".
func aggTestRows(ctx context.Context, t *testing.T, d int) map[string]float64 {
	var rows []model.ResourceAggregates
	require.NoError(t, Bun().NewSelect().Model(&rows).
		Where("date = ?", aggTestDay(d, 0).Format(time.DateOnly)).Scan(ctx))
	res := map[string]float64{}
	for _, r := range rows {
		res[r.AggregationType+"/"+r.AggregationKey] = float64(r.Seconds)
	}
	return res
}

// newAggTestDB returns a new migrated database for a test of the aggregation.
func newAggTestDB(t *testing.T) (*PgDB, func()) {
	require.NoError(t, etc.SetRootPath(RootFromDB))
	db, cleanup := MustResolveNewPostgresDatabase(t)
	MustMigrateTestPostgres(t, db, MigrationsFromDB)
	return db, cleanup
}

func TestAggregationOpenAllocation(t *testing.T) {
	db, cleanup := newAggTestDB(t)
	defer cleanup()
	ctx := context.Background()

	user := RequireMockUser(t, db)
	task := RequireMockTask(t, db, &user.ID)
	allocID := addAggTestAllocation(ctx, t, task.TaskID, 1, "pool", aggTestDay(1, 20), nil)
	// An open allocation that starts after the aggregated day adds nothing to that day.
	addAggTestAllocation(ctx, t, task.TaskID, 1, "pool", aggTestDay(2, 6), nil)
	rows := func(seconds float64) map[string]float64 {
		return map[string]float64{
			"total/total":               seconds,
			"username/" + user.Username: seconds,
			"resource_pool/pool":        seconds,
			"queued/total":              0,
		}
	}
	hour := float64(3600)

	// Day 1 is aggregated while the allocation is still running: it counts until the end of day 1.
	aggTestRun(t, db, 2)
	day1 := rows(4 * hour)
	require.Equal(t, day1, aggTestRows(ctx, t, 1))
	require.Empty(t, aggTestRows(ctx, t, 2))

	// The first allocation ends on day 2: day 1 keeps its usage and day 2 gets the rest, plus the
	// second allocation from 06:00.
	end := aggTestDay(2, 4)
	_, err := Bun().NewUpdate().Table("allocations").Set("end_time = ?", end).
		Where("allocation_id = ?", allocID).Exec(ctx)
	require.NoError(t, err)
	aggTestRun(t, db, 3)
	day2 := rows((4 + 18) * hour)
	require.Equal(t, day1, aggTestRows(ctx, t, 1))
	require.Equal(t, day2, aggTestRows(ctx, t, 2))

	// Aggregating again changes nothing.
	aggTestRun(t, db, 3)
	require.Equal(t, day1, aggTestRows(ctx, t, 1))
	require.Equal(t, day2, aggTestRows(ctx, t, 2))
}

func TestAggregationRecomputesDayOfRestartClose(t *testing.T) {
	db, cleanup := newAggTestDB(t)
	defer cleanup()
	ctx := context.Background()

	user := RequireMockUser(t, db)
	task := RequireMockTask(t, db, &user.ID)
	allocID := addAggTestAllocation(ctx, t, task.TaskID, 1, "pool", aggTestDay(1, 20), nil)
	hour := float64(3600)

	aggTestRun(t, db, 2)
	requireSeconds(t, 4*hour, aggTestRows(ctx, t, 1)["total/total"])

	// A restart after the run closes the allocation at the last heartbeat, before the end of
	// day 1: the last aggregated day is recomputed.
	_, err := Bun().NewUpdate().Table("allocations").
		Set("end_time = ?", aggTestDay(1, 23).Add(55*time.Minute)).
		Where("allocation_id = ?", allocID).Exec(ctx)
	require.NoError(t, err)
	aggTestRun(t, db, 2)
	requireSeconds(t, 3*hour+55*60, aggTestRows(ctx, t, 1)["total/total"])
	aggTestRun(t, db, 3)
	requireSeconds(t, 3*hour+55*60, aggTestRows(ctx, t, 1)["total/total"])
	requireSeconds(t, 0, aggTestRows(ctx, t, 2)["total/total"])
}

func TestAggregationRecomputesOpenDays(t *testing.T) {
	db, cleanup := newAggTestDB(t)
	defer cleanup()
	ctx := context.Background()

	user := RequireMockUser(t, db)
	task := RequireMockTask(t, db, &user.ID)
	allocID := addAggTestAllocation(ctx, t, task.TaskID, 2, "a", aggTestDay(1, 12), nil)
	setAlloc := func(column string, value any) {
		_, err := Bun().NewUpdate().Table("allocations").Set("? = ?", bun.Ident(column), value).
			Where("allocation_id = ?", allocID).Exec(ctx)
		require.NoError(t, err)
	}
	hour := float64(3600)

	aggTestRun(t, db, 2)
	requireSeconds(t, 2*12*hour, aggTestRows(ctx, t, 1)["total/total"])

	aggTestRun(t, db, 3)
	requireSeconds(t, 2*12*hour, aggTestRows(ctx, t, 1)["total/total"])
	requireSeconds(t, 2*24*hour, aggTestRows(ctx, t, 2)["total/total"])

	// Every day the open allocation overlaps is recomputed on each run, and a recomputed day
	// replaces its rows: the slots and pool changed here show on days 1 and 2, and pool "a" goes.
	setAlloc("slots", 3)
	setAlloc("resource_pool", "b")
	aggTestRun(t, db, 4)
	for d, want := range map[int]float64{1: 3 * 12 * hour, 2: 3 * 24 * hour, 3: 3 * 24 * hour} {
		rows := aggTestRows(ctx, t, d)
		requireSeconds(t, want, rows["total/total"], "day %d", d)
		requireSeconds(t, want, rows["resource_pool/b"], "day %d", d)
		require.NotContains(t, rows, "resource_pool/a", "day %d", d)
	}

	// Once it has ended and a day after its end is aggregated, its days are not recomputed.
	setAlloc("end_time", aggTestDay(4, 6))
	aggTestRun(t, db, 5)
	requireSeconds(t, 3*6*hour, aggTestRows(ctx, t, 4)["total/total"])
	aggTestRun(t, db, 6)
	setAlloc("slots", 5)
	aggTestRun(t, db, 7)
	for d, want := range map[int]float64{
		1: 3 * 12 * hour, 2: 3 * 24 * hour, 3: 3 * 24 * hour, 4: 3 * 6 * hour, 5: 0, 6: 0,
	} {
		requireSeconds(t, want, aggTestRows(ctx, t, d)["total/total"], "day %d", d)
	}
}

func TestAggregationClosedAllocationNotRecomputed(t *testing.T) {
	db, cleanup := newAggTestDB(t)
	defer cleanup()
	ctx := context.Background()

	user := RequireMockUser(t, db)
	task := RequireMockTask(t, db, &user.ID)
	end := aggTestDay(1, 12)
	allocID := addAggTestAllocation(ctx, t, task.TaskID, 1, "pool", aggTestDay(1, 10), &end)

	aggTestRun(t, db, 2)
	day1 := aggTestRows(ctx, t, 1)
	requireSeconds(t, float64(2*3600), day1["total/total"])

	// Once a day after its end is aggregated, a change to the records of a closed allocation does
	// not show.
	aggTestRun(t, db, 3)
	require.Equal(t, day1, aggTestRows(ctx, t, 1))
	_, err := Bun().NewUpdate().Table("allocations").Set("slots = 4").
		Where("allocation_id = ?", allocID).Exec(ctx)
	require.NoError(t, err)
	aggTestRun(t, db, 4)
	aggTestRun(t, db, 4)
	require.Equal(t, day1, aggTestRows(ctx, t, 1))
	for _, d := range []int{2, 3} {
		requireSeconds(t, float64(0), aggTestRows(ctx, t, d)["total/total"], "day %d", d)
	}
}

func TestAggregationTotalsAgree(t *testing.T) {
	db, cleanup := newAggTestDB(t)
	defer cleanup()
	ctx := context.Background()

	u1, u2 := RequireMockUser(t, db), RequireMockUser(t, db)
	t1, t2 := RequireMockTask(t, db, &u1.ID), RequireMockTask(t, db, &u2.ID)
	end1, end2 := aggTestDay(1, 3), aggTestDay(2, 2)
	addAggTestAllocation(ctx, t, t1.TaskID, 4, "a", aggTestDay(1, 1), &end1)
	addAggTestAllocation(ctx, t, t1.TaskID, 2, "b", aggTestDay(1, 22), &end2)
	addAggTestAllocation(ctx, t, t2.TaskID, 1, "a", aggTestDay(1, 18), nil)

	aggTestRun(t, db, 2)
	aggTestRun(t, db, 3)
	for _, d := range []int{1, 2} {
		rows := aggTestRows(ctx, t, d)
		sums := map[string]float64{}
		for k, v := range rows {
			typ, _, _ := strings.Cut(k, "/")
			sums[typ] += v
		}
		requireSeconds(t, rows["total/total"], sums["username"], "day %d", d)
		requireSeconds(t, rows["total/total"], sums["resource_pool"], "day %d", d)
	}
	hour := float64(3600)
	requireSeconds(t, (4*2+2*2+1*6)*hour, aggTestRows(ctx, t, 1)["total/total"])
	requireSeconds(t, (2*2+1*24)*hour, aggTestRows(ctx, t, 2)["total/total"])
}

func TestAggregationQueuedTimeOpenAllocation(t *testing.T) {
	db, cleanup := newAggTestDB(t)
	defer cleanup()
	ctx := context.Background()

	user := RequireMockUser(t, db)
	task := RequireMockTask(t, db, &user.ID)
	start := aggTestDay(1, 20)
	allocID := addAggTestAllocation(ctx, t, task.TaskID, 1, "pool", start, nil)
	queuedStart := start.Add(-time.Minute)
	_, err := Bun().NewInsert().Model(&model.TaskStats{
		AllocationID: allocID, EventType: "QUEUED", StartTime: &queuedStart, EndTime: &start,
	}).Exec(ctx)
	require.NoError(t, err)

	// An allocation still queued has no end time and counts on the day its queued time ends.
	stillQueued := addAggTestAllocation(ctx, t, task.TaskID, 1, "pool", aggTestDay(1, 21), nil)
	_, err = Bun().NewInsert().Model(&model.TaskStats{
		AllocationID: stillQueued, EventType: "QUEUED", StartTime: &queuedStart,
	}).Exec(ctx)
	require.NoError(t, err)

	aggTestRun(t, db, 2)
	rows := aggTestRows(ctx, t, 1)
	requireSeconds(t, float64(60), rows["queued/total"])
	requireSeconds(t, float64(60), rows["queued/pool"])
}
