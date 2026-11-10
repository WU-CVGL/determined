package internal

import (
	"context"
	"database/sql"
	"sort"
	"sync"
	"time"

	"github.com/uptrace/bun"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/grpcutil"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/userusage"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/masterv1"
	"github.com/determined-ai/determined/proto/pkg/resourcepoolv1"
)

const (
	usageDayLayout   = "2006-01-02"
	usageMonthLayout = "2006-01"
	usageDay         = 24 * time.Hour
	// userUsageMaxDays is the longest DAILY range: 13 whole months back from any day of the
	// month, and a 53-week grid.
	userUsageMaxDays = 400
	// userUsageMaxMonths is the longest MONTHLY range.
	userUsageMaxMonths = 36
	// userUsageReadTimeout bounds the history read.
	userUsageReadTimeout = 10 * time.Second
)

// canViewUserPage checks whether cur may read the resources and usage of target. Every
// authenticated user may read every user's page; a later access rule goes here, and must then
// also cover ResourceAllocationAggregated and ResourceAllocationRaw.
func canViewUserPage(ctx context.Context, cur model.User, target model.User) error {
	return nil
}

// GetUserResources returns the resources a user's tasks hold now.
func (a *apiServer) GetUserResources(
	ctx context.Context, req *apiv1.GetUserResourcesRequest,
) (*apiv1.GetUserResourcesResponse, error) {
	curUser, _, err := grpcutil.GetUser(ctx)
	if err != nil {
		return nil, err
	}
	target, err := getFullModelUser(ctx, model.UserID(req.UserId))
	if err != nil {
		return nil, err
	}
	if err := canViewUserPage(ctx, *curUser, target.ToUser()); err != nil {
		return nil, err
	}
	asOf := time.Now().UTC().Truncate(time.Second)

	// The resource manager's pools are not filtered by the viewer's pool access.
	pools, err := a.m.rm.GetResourcePools()
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "reading resource pools: %s", err)
	}
	k8sPools := map[string]bool{}
	auxMetered := false
	for _, p := range pools.GetResourcePools() {
		k8s := p.Type == resourcepoolv1.ResourcePoolType_RESOURCE_POOL_TYPE_K8S
		k8sPools[p.Name] = k8s
		auxMetered = auxMetered || !k8s
	}
	summaries, err := a.m.rm.GetAllocationSummaries()
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "reading allocation summaries: %s", err)
	}

	seen := map[model.TaskID]bool{}
	var taskIDs []model.TaskID
	for _, s := range summaries {
		if len(s.Resources) > 0 && !seen[s.TaskID] {
			seen[s.TaskID] = true
			taskIDs = append(taskIDs, s.TaskID)
		}
	}
	owned, err := userTaskIDs(ctx, target.ID, taskIDs)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reading task owners: %s", err)
	}

	return &apiv1.GetUserResourcesResponse{
		Pools:      currentResources(summaries, owned, k8sPools),
		AsOf:       timestamppb.New(asOf),
		AuxMetered: auxMetered,
	}, nil
}

// GetUserUsage returns a user's daily or monthly resource usage.
func (a *apiServer) GetUserUsage(
	ctx context.Context, req *apiv1.GetUserUsageRequest,
) (*apiv1.GetUserUsageResponse, error) {
	curUser, _, err := grpcutil.GetUser(ctx)
	if err != nil {
		return nil, err
	}
	target, err := getFullModelUser(ctx, model.UserID(req.UserId))
	if err != nil {
		return nil, err
	}
	if err := canViewUserPage(ctx, *curUser, target.ToUser()); err != nil {
		return nil, err
	}
	rangeStart, rangeEnd, err := parseUsageRange(req.StartDate, req.EndDate, req.Period)
	if err != nil {
		return nil, err
	}
	asOf := time.Now().UTC().Truncate(time.Second)

	readCtx, cancel := context.WithTimeout(ctx, userUsageReadTimeout)
	defer cancel()
	coverageStart, err := a.m.userUsageCoverage.get(readCtx, db.Bun())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reading the first allocation: %s", err)
	}

	resp := &apiv1.GetUserUsageResponse{
		Entries: []*apiv1.UserUsageEntry{},
		AsOf:    timestamppb.New(asOf),
	}
	if coverageStart != nil {
		resp.CoverageStart = coverageStart.Format(usageDayLayout)
	}
	from, to, ok := usageEntryBounds(rangeStart, rangeEnd, coverageStart, asOf, req.Period)
	if !ok {
		return resp, nil
	}

	rows, err := readUserAllocations(readCtx, db.Bun(), target.ID, rangeStart, minTime(rangeEnd, asOf))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reading allocations: %s", err)
	}
	resp.Entries = userUsage(rows, from, to, asOf, req.Period)
	return resp, nil
}

// userTaskIDs returns which of taskIDs belong to jobs the user owns. Checkpoint GC tasks run
// under the experiment's job but are not counted.
func userTaskIDs(
	ctx context.Context, userID model.UserID, taskIDs []model.TaskID,
) (map[model.TaskID]bool, error) {
	owned := map[model.TaskID]bool{}
	if len(taskIDs) == 0 {
		return owned, nil
	}
	var ids []string
	if err := db.Bun().NewRaw(`
SELECT t.task_id
FROM tasks t
JOIN jobs j ON j.job_id = t.job_id
WHERE t.task_id IN (?) AND j.owner_id = ? AND t.task_type <> 'CHECKPOINT_GC'`,
		bun.In(taskIDs), userID).Scan(ctx, &ids); err != nil {
		return nil, err
	}
	for _, id := range ids {
		owned[model.TaskID(id)] = true
	}
	return owned, nil
}

// poolCounts accumulates the resources held in one pool.
type poolCounts struct {
	gpus, cpuSlots, aux int
	// k8s is set when the pool or any of its resources is Kubernetes.
	k8s bool
}

// currentResources counts the resources the owned tasks hold, by pool, as the assignment meters
// them (userusage.Assigned); unclassified resources are not counted. k8sPools marks the
// Kubernetes pools, which have no aux containers. Pools without any count are left out; the
// result is sorted by pool name.
func currentResources(
	summaries map[model.AllocationID]sproto.AllocationSummary,
	owned map[model.TaskID]bool,
	k8sPools map[string]bool,
) []*apiv1.UserPoolResources {
	counts := map[string]*poolCounts{}
	for _, s := range summaries {
		if !owned[s.TaskID] || len(s.Resources) == 0 {
			continue
		}
		c := counts[s.ResourcePool]
		if c == nil {
			c = &poolCounts{k8s: k8sPools[s.ResourcePool]}
			counts[s.ResourcePool] = c
		}
		c.k8s = c.k8s || s.Resources[0].ResourcesType == sproto.ResourcesTypeK8sJob
		kind, n := userusage.Assigned(s.Resources, s.SlotsNeeded)
		switch kind {
		case userusage.KindGPU:
			c.gpus += n
		case userusage.KindCPU:
			c.cpuSlots += n
		case userusage.KindAux:
			c.aux += n
		}
	}

	names := make([]string, 0, len(counts))
	for name, c := range counts {
		if c.gpus > 0 || c.cpuSlots > 0 || c.aux > 0 {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	result := make([]*apiv1.UserPoolResources, 0, len(names))
	for _, name := range names {
		c := counts[name]
		pc := &apiv1.UserResourceCounts{Gpus: int32(c.gpus), CpuSlots: int32(c.cpuSlots)}
		if !c.k8s {
			aux := int32(c.aux)
			pc.AuxContainers = &aux
		}
		result = append(result, &apiv1.UserPoolResources{ResourcePool: name, Counts: pc})
	}
	return result
}

// parseUsageRange parses a GetUserUsage range into the UTC instants [start, end). Dates are
// parsed as in ResourceAllocationAggregated; the last day or month is inclusive.
func parseUsageRange(
	startDate, endDate string, period masterv1.ResourceAllocationAggregationPeriod,
) (start, end time.Time, err error) {
	switch period {
	case masterv1.ResourceAllocationAggregationPeriod_RESOURCE_ALLOCATION_AGGREGATION_PERIOD_DAILY:
		start, err = time.Parse(usageDayLayout, startDate)
		if err != nil {
			return start, end, status.Errorf(codes.InvalidArgument, "invalid start date %s", err)
		}
		last, err := time.Parse(usageDayLayout, endDate)
		if err != nil {
			return start, end, status.Errorf(codes.InvalidArgument, "invalid end date %s", err)
		}
		if start.After(last) {
			return start, end, status.Error(codes.InvalidArgument, "start date cannot be after end date")
		}
		if days := int(last.Sub(start)/usageDay) + 1; days > userUsageMaxDays {
			return start, end, status.Errorf(codes.InvalidArgument,
				"the range spans %d days, more than %d", days, userUsageMaxDays)
		}
		return start, last.AddDate(0, 0, 1), nil

	case masterv1.ResourceAllocationAggregationPeriod_RESOURCE_ALLOCATION_AGGREGATION_PERIOD_MONTHLY:
		start, err = time.Parse(usageMonthLayout, startDate)
		if err != nil {
			return start, end, status.Errorf(codes.InvalidArgument, "invalid start date %s", err)
		}
		last, err := time.Parse(usageMonthLayout, endDate)
		if err != nil {
			return start, end, status.Errorf(codes.InvalidArgument, "invalid end date %s", err)
		}
		if start.After(last) {
			return start, end, status.Error(codes.InvalidArgument, "start date cannot be after end date")
		}
		months := (last.Year()-start.Year())*12 + int(last.Month()-start.Month()) + 1
		if months > userUsageMaxMonths {
			return start, end, status.Errorf(codes.InvalidArgument,
				"the range spans %d months, more than %d", months, userUsageMaxMonths)
		}
		return start, last.AddDate(0, 1, 0), nil

	default:
		return start, end, status.Error(codes.InvalidArgument, "no aggregation period specified")
	}
}

// usageEntryBounds returns the first and last period start of the entries for the range
// [rangeStart, rangeEnd): from the later of the range start and the coverage start to the
// earlier of the range end and the day of asOf. ok is false when there are none.
func usageEntryBounds(
	rangeStart, rangeEnd time.Time, coverageStart *time.Time, asOf time.Time,
	period masterv1.ResourceAllocationAggregationPeriod,
) (from, to time.Time, ok bool) {
	if coverageStart == nil {
		return from, to, false
	}
	from = maxTime(rangeStart, truncateDay(*coverageStart))
	to = minTime(rangeEnd.AddDate(0, 0, -1), truncateDay(asOf))
	if to.Before(from) {
		return from, to, false
	}
	if period == masterv1.ResourceAllocationAggregationPeriod_RESOURCE_ALLOCATION_AGGREGATION_PERIOD_MONTHLY {
		from, to = truncateMonth(from), truncateMonth(to)
	}
	return from, to, true
}

// userAllocationRow is one allocation of a user's tasks as stored.
type userAllocationRow struct {
	AllocationID string                 `bun:"allocation_id"`
	State        *model.AllocationState `bun:"state"`
	MeteredKind  string                 `bun:"metered_kind"`
	Quantity     int                    `bun:"quantity"`
	AssignedTime *time.Time             `bun:"assigned_time"`
	ReleasedTime *time.Time             `bun:"released_time"`
	StartTime    *time.Time             `bun:"start_time"`
	EndTime      *time.Time             `bun:"end_time"`
	QueuedEnd    *time.Time             `bun:"queued_end"`
}

// record returns the row as userusage meters it.
func (r userAllocationRow) record() userusage.Record {
	return userusage.Record{
		Kind:      userusage.Kind(r.MeteredKind),
		Quantity:  r.Quantity,
		Terminal:  r.State != nil && *r.State == model.AllocationStateTerminated,
		Assigned:  r.AssignedTime,
		Released:  r.ReleasedTime,
		QueuedEnd: r.QueuedEnd,
		Start:     r.StartTime,
		End:       r.EndTime,
	}
}

// heldSQL joins, for the allocations a, when their resources were held: held_from and held_until,
// timestamp with time zone. The allocations times are timestamp without time zone in UTC; AT TIME
// ZONE 'UTC' reads them as UTC whatever the session time zone. Allocations from before metering
// have no assignment record: they are held from the end of their first QUEUED task_stats row (a
// restored allocation records another one at each master start), else their start time, to their
// end time. The QUEUED row is looked up for those only.
const heldSQL = `
LEFT JOIN LATERAL (
    SELECT min(ts.end_time) AS queued_end
    FROM task_stats ts
    WHERE a.assigned_time IS NULL
      AND ts.allocation_id = a.allocation_id
      AND ts.event_type = 'QUEUED' AND ts.end_time IS NOT NULL
) q ON true
CROSS JOIN LATERAL (
    SELECT
        CASE WHEN a.assigned_time IS NOT NULL THEN a.assigned_time AT TIME ZONE 'UTC'
             ELSE coalesce(q.queued_end, a.start_time AT TIME ZONE 'UTC') END AS held_from,
        CASE WHEN a.assigned_time IS NOT NULL THEN a.released_time
             ELSE a.end_time END AT TIME ZONE 'UTC' AS held_until
) h`

// meteredSQL holds for the allocations a, joined with heldSQL, that userusage.Intervals meters
// them, apart from the cut at the time of the request: a kind other than none, a quantity, a
// start, and a positive time held, or, without an end, no terminated state. An allocation from
// before metering whose start time equals its end time was closed at a master start without ever
// holding resources.
const meteredSQL = `(
    a.metered_kind IS NOT NULL AND a.metered_kind <> 'none'
    AND coalesce(a.metered_quantity, a.slots) > 0
    AND h.held_from IS NOT NULL
    AND NOT (a.assigned_time IS NULL AND a.start_time IS NOT NULL
             AND a.start_time IS NOT DISTINCT FROM a.end_time)
    AND CASE WHEN h.held_until IS NULL THEN a.state IS DISTINCT FROM 'TERMINATED'
             ELSE h.held_until > h.held_from END
)`

// userAllocationColumns selects a userAllocationRow from the allocations a joined with heldSQL. An
// allocation from before metering whose kind is not known yet counts its slots.
const userAllocationColumns = `a.allocation_id, a.state, a.metered_kind,
       coalesce(a.metered_quantity, a.slots) AS quantity,
       a.assigned_time, a.released_time, a.start_time, a.end_time, q.queued_end`

// readUserAllocations reads the metered allocations of the user's tasks that may overlap
// [rangeStart, rangeEnd). Checkpoint GC tasks are not counted.
func readUserAllocations(
	ctx context.Context, idb bun.IDB, userID model.UserID, rangeStart, rangeEnd time.Time,
) ([]userAllocationRow, error) {
	rows := []userAllocationRow{}
	// bun sends the bounds as UTC literals with the offset +00:00.
	err := idb.NewRaw(`
SELECT `+userAllocationColumns+`
FROM jobs j
JOIN tasks t ON t.job_id = j.job_id
JOIN allocations a ON a.task_id = t.task_id`+heldSQL+`
WHERE j.owner_id = ?
  AND t.task_type <> 'CHECKPOINT_GC'
  AND `+meteredSQL+`
  AND h.held_from < ?
  AND (h.held_until IS NULL OR h.held_until > ?)`,
		userID, rangeEnd.UTC(), rangeStart.UTC()).Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// readUsageCoverage returns the start of the first metered allocation, or nil when there is none.
func readUsageCoverage(ctx context.Context, idb bun.IDB) (*time.Time, error) {
	var first sql.NullTime
	if err := idb.NewRaw(`
SELECT min(h.held_from)
FROM allocations a`+heldSQL+`
WHERE `+meteredSQL).Scan(ctx, &first); err != nil {
		return nil, err
	}
	if !first.Valid {
		return nil, nil
	}
	return &first.Time, nil
}

// userUsageCoverage caches the first day of metered allocations, and reads it again only while
// there is none: no allocation gets an earlier assignment later. The one exception is an allocation
// from before metering without slots, which is metered from its classification on; a day cached
// before the classifier's first step, which runs when the master starts, can be later than it.
type userUsageCoverage struct {
	mu    sync.Mutex
	start *time.Time
}

// get returns the first day of metered allocations, or nil when there are none.
func (c *userUsageCoverage) get(ctx context.Context, idb bun.IDB) (*time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.start != nil {
		return c.start, nil
	}
	first, err := readUsageCoverage(ctx, idb)
	if err != nil || first == nil {
		return nil, err
	}
	start := truncateDay(*first)
	c.start = &start
	return c.start, nil
}

// userUsage returns the entries for the periods whose starts run from the first, from, to the
// last, to.
func userUsage(
	rows []userAllocationRow, from, to, asOf time.Time,
	period masterv1.ResourceAllocationAggregationPeriod,
) []*apiv1.UserUsageEntry {
	records := make([]userusage.Record, 0, len(rows))
	for _, r := range rows {
		records = append(records, r.record())
	}
	intervals := userusage.Intervals(records, asOf)
	var usage []userusage.Entry
	layout := usageDayLayout
	if period == masterv1.ResourceAllocationAggregationPeriod_RESOURCE_ALLOCATION_AGGREGATION_PERIOD_MONTHLY {
		usage, layout = userusage.Monthly(intervals, from, to.AddDate(0, 1, 0)), usageMonthLayout
	} else {
		usage = userusage.Daily(intervals, from, to.AddDate(0, 0, 1))
	}

	entries := make([]*apiv1.UserUsageEntry, 0, len(usage))
	for _, e := range usage {
		entries = append(entries, &apiv1.UserUsageEntry{
			PeriodStart:             e.Start.Format(layout),
			GpuSeconds:              e.GPU,
			CpuSlotSeconds:          e.CPU,
			AuxContainerSeconds:     e.Aux,
			UnclassifiedSlotSeconds: e.Unclassified,
			GpuActiveSeconds:        e.GPUActive,
			AuxActiveSeconds:        e.AuxActive,
			GpuOrAuxActiveSeconds:   e.GPUOrAuxActive,
			GpuActiveDays:           int32(e.GPUDays),
			AuxActiveDays:           int32(e.AuxDays),
			GpuOrAuxActiveDays:      int32(e.GPUOrAuxDays),
		})
	}
	return entries
}

// truncateDay returns the UTC midnight that starts t's UTC day.
func truncateDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// truncateMonth returns the first UTC midnight of t's UTC month.
func truncateMonth(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// minTime returns the earlier of a and b.
func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// maxTime returns the later of a and b.
func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
