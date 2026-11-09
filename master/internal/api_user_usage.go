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
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/devicev1"
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

	pools, err := a.resourcePoolKinds()
	if err != nil {
		return nil, err
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
		Pools: currentResources(summaries, owned, pools),
		AsOf:  timestamppb.New(asOf),
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

	pools, err := a.resourcePoolKinds()
	if err != nil {
		return nil, err
	}
	coverageStart, err := a.m.userUsageCoverage.get(ctx)
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

	readCtx, cancel := context.WithTimeout(ctx, userUsageReadTimeout)
	defer cancel()
	rows, err := readUserAllocations(readCtx, db.Bun(), target.ID, rangeStart, minTime(rangeEnd, asOf))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reading allocations: %s", err)
	}
	resp.Entries = userUsage(rows, from, to, asOf, pools, req.Period)
	return resp, nil
}

// poolKind is what metering needs to know about a resource pool. The zero value, used for a pool
// that is not reported (removed, or never seen), is an agentrm pool whose slots count as GPUs.
type poolKind struct {
	// k8s is set for a Kubernetes pool, which has no Aux containers.
	k8s bool
	// cpu is set when the pool's slot type is CPU; any other slot type counts as GPUs.
	cpu bool
}

// resourcePoolKinds reads the resource pools from the resource manager.
func (a *apiServer) resourcePoolKinds() (map[string]poolKind, error) {
	resp, err := a.m.rm.GetResourcePools()
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "reading resource pools: %s", err)
	}
	return poolKinds(resp), nil
}

// poolKinds maps each reported pool's name to its kind.
func poolKinds(resp *apiv1.GetResourcePoolsResponse) map[string]poolKind {
	kinds := map[string]poolKind{}
	for _, p := range resp.GetResourcePools() {
		kinds[p.Name] = poolKind{
			k8s: p.Type == resourcepoolv1.ResourcePoolType_RESOURCE_POOL_TYPE_K8S,
			cpu: p.SlotType == devicev1.Type_TYPE_CPU,
		}
	}
	return kinds
}

// auxAvailable reports whether Aux containers are metered: unless every reported pool is a
// Kubernetes pool. Unreported pools count as agentrm pools, so an empty list meters Aux.
func auxAvailable(pools map[string]poolKind) bool {
	if len(pools) == 0 {
		return true
	}
	for _, p := range pools {
		if !p.k8s {
			return true
		}
	}
	return false
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

// currentResources counts the resources the owned tasks hold, by pool. On agentrm each device
// counts by its own type and a container without devices is one Aux container. Kubernetes
// devices carry no type, so they count by the pool's slot type, and Kubernetes has no Aux.
// Pools without any count are left out; the result is sorted by pool name.
func currentResources(
	summaries map[model.AllocationID]sproto.AllocationSummary,
	owned map[model.TaskID]bool,
	pools map[string]poolKind,
) []*apiv1.UserPoolResources {
	counts := map[string]*poolCounts{}
	for _, s := range summaries {
		if !owned[s.TaskID] || len(s.Resources) == 0 {
			continue
		}
		pool := pools[s.ResourcePool]
		c := counts[s.ResourcePool]
		if c == nil {
			c = &poolCounts{k8s: pool.k8s}
			counts[s.ResourcePool] = c
		}
		for _, r := range s.Resources {
			n := 0
			for _, devices := range r.AgentDevices {
				n += len(devices)
			}
			switch {
			case r.ResourcesType == sproto.ResourcesTypeK8sJob || pool.k8s:
				c.k8s = true
				if pool.cpu {
					c.cpuSlots += n
				} else {
					c.gpus += n
				}
			case n == 0:
				c.aux++
			default:
				for _, devices := range r.AgentDevices {
					for _, d := range devices {
						switch {
						case d.Type == device.CPU:
							c.cpuSlots++
						case d.Type == device.CUDA || d.Type == device.ROCM:
							c.gpus++
						case pool.cpu:
							c.cpuSlots++
						default:
							c.gpus++
						}
					}
				}
			}
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

// userAllocationRow is one allocation of a user's tasks, with the end of its first QUEUED
// task_stats row, when its resources were assigned.
type userAllocationRow struct {
	AllocationID string                 `bun:"allocation_id"`
	ResourcePool string                 `bun:"resource_pool"`
	Slots        int                    `bun:"slots"`
	StartTime    *time.Time             `bun:"start_time"`
	EndTime      *time.Time             `bun:"end_time"`
	State        *model.AllocationState `bun:"state"`
	QueuedEnd    *time.Time             `bun:"queued_end"`
}

// readUserAllocations reads the allocations of the user's tasks that may overlap
// [rangeStart, rangeEnd). Checkpoint GC tasks are not counted.
func readUserAllocations(
	ctx context.Context, idb bun.IDB, userID model.UserID, rangeStart, rangeEnd time.Time,
) ([]userAllocationRow, error) {
	rows := []userAllocationRow{}
	// The times are timestamp without time zone, written in UTC; bun sends the bounds as UTC.
	err := idb.NewRaw(`
SELECT a.allocation_id, a.resource_pool, a.slots, a.start_time, a.end_time, a.state,
       q.queued_end
FROM jobs j
JOIN tasks t ON t.job_id = j.job_id
JOIN allocations a ON a.task_id = t.task_id
-- The first QUEUED row ends when the allocation got its resources (a restored allocation
-- records another one at each master start).
LEFT JOIN LATERAL (
    SELECT min(ts.end_time) AS queued_end
    FROM task_stats ts
    WHERE ts.allocation_id = a.allocation_id
      AND ts.event_type = 'QUEUED' AND ts.end_time IS NOT NULL
) q ON true
WHERE j.owner_id = ?
  AND t.task_type <> 'CHECKPOINT_GC'
  AND coalesce(q.queued_end, a.start_time) < ?
  AND (a.end_time IS NULL OR a.end_time > ?)`,
		userID, rangeEnd.UTC(), rangeStart.UTC()).Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// userUsageCoverage caches the first day with allocation records. It only changes from none to a
// day, so it is read again only while there is none.
type userUsageCoverage struct {
	mu    sync.Mutex
	start *time.Time
}

// get returns the first day with allocation records, or nil when there are none.
func (c *userUsageCoverage) get(ctx context.Context) (*time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.start != nil {
		return c.start, nil
	}
	var first sql.NullTime
	if err := db.Bun().NewRaw(
		`SELECT date_trunc('day', min(start_time)) FROM allocations`,
	).Scan(ctx, &first); err != nil {
		return nil, err
	}
	if !first.Valid {
		return nil, nil
	}
	start := truncateDay(first.Time)
	c.start = &start
	return c.start, nil
}

// usageKind is what an interval holds.
type usageKind int

const (
	usageGPU usageKind = iota
	usageCPUSlot
	usageAux
)

// usageInterval is the time an allocation held quantity units of one kind.
type usageInterval struct {
	start, end time.Time
	kind       usageKind
	quantity   float64
}

// usageIntervals builds one interval per allocation, from when it got its resources (the end of
// the first QUEUED row, else start_time) to its end, or asOf while it runs. It skips allocations
// that never held resources: start_time equal to end_time (closed at a master start), and
// start_time and end_time both unset with state TERMINATED (killed before pulling). Slots count
// as CPU slots in a CPU pool and as GPUs otherwise; an allocation without slots is one Aux
// container, except on Kubernetes.
func usageIntervals(
	rows []userAllocationRow, asOf time.Time, pools map[string]poolKind,
) []usageInterval {
	intervals := make([]usageInterval, 0, len(rows))
	for _, r := range rows {
		var start time.Time
		switch {
		case r.QueuedEnd != nil:
			start = r.QueuedEnd.UTC()
		case r.StartTime != nil:
			start = r.StartTime.UTC()
		default:
			continue
		}
		if r.StartTime != nil && r.EndTime != nil && r.StartTime.Equal(*r.EndTime) {
			continue
		}
		if r.StartTime == nil && r.EndTime == nil &&
			r.State != nil && *r.State == model.AllocationStateTerminated {
			continue
		}
		end := asOf.UTC()
		if r.EndTime != nil && r.EndTime.Before(end) {
			end = r.EndTime.UTC()
		}
		if !end.After(start) {
			continue
		}

		pool := pools[r.ResourcePool]
		iv := usageInterval{start: start, end: end, kind: usageGPU, quantity: float64(r.Slots)}
		switch {
		case r.Slots == 0 && pool.k8s:
			continue
		case r.Slots == 0:
			iv.kind, iv.quantity = usageAux, 1
		case pool.cpu:
			iv.kind = usageCPUSlot
		}
		intervals = append(intervals, iv)
	}
	return intervals
}

// dayUsage is the usage in one UTC day, in seconds.
type dayUsage struct {
	gpu, cpuSlot, aux, active float64
}

// timeSpan is a half-open time range.
type timeSpan struct {
	start, end time.Time
}

// usageByDay buckets the intervals into the UTC days [from, to), both UTC midnights. Active
// seconds are the length of the union of the intervals in the day.
func usageByDay(intervals []usageInterval, from, to time.Time) []dayUsage {
	n := int(to.Sub(from) / usageDay)
	if n <= 0 {
		return nil
	}
	days := make([]dayUsage, n)
	spans := make([][]timeSpan, n)
	for _, iv := range intervals {
		start, end := maxTime(iv.start, from), minTime(iv.end, to)
		for i := int(start.Sub(from) / usageDay); i < n; i++ {
			dayStart := from.Add(time.Duration(i) * usageDay)
			s, e := maxTime(start, dayStart), minTime(end, dayStart.Add(usageDay))
			if !e.After(s) {
				break
			}
			seconds := e.Sub(s).Seconds()
			switch iv.kind {
			case usageGPU:
				days[i].gpu += iv.quantity * seconds
			case usageCPUSlot:
				days[i].cpuSlot += iv.quantity * seconds
			case usageAux:
				days[i].aux += iv.quantity * seconds
			}
			spans[i] = append(spans[i], timeSpan{start: s, end: e})
		}
	}
	for i := range days {
		days[i].active = unionSeconds(spans[i])
	}
	return days
}

// unionSeconds returns the length of the union of the spans.
func unionSeconds(spans []timeSpan) float64 {
	if len(spans) == 0 {
		return 0
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start.Before(spans[j].start) })
	var total time.Duration
	cur := spans[0]
	for _, s := range spans[1:] {
		if s.start.After(cur.end) {
			total += cur.end.Sub(cur.start)
			cur = s
		} else if s.end.After(cur.end) {
			cur.end = s.end
		}
	}
	total += cur.end.Sub(cur.start)
	return total.Seconds()
}

// userUsage returns the entries for the periods whose starts run from the first, from, to the
// last, to. A month is the sum of its days, and its active days are the days with any usage.
// Aux seconds are unset when Aux is not metered.
func userUsage(
	rows []userAllocationRow, from, to, asOf time.Time, pools map[string]poolKind,
	period masterv1.ResourceAllocationAggregationPeriod,
) []*apiv1.UserUsageEntry {
	monthly := period ==
		masterv1.ResourceAllocationAggregationPeriod_RESOURCE_ALLOCATION_AGGREGATION_PERIOD_MONTHLY
	daysEnd := to.AddDate(0, 0, 1)
	if monthly {
		daysEnd = to.AddDate(0, 1, 0)
	}
	days := usageByDay(usageIntervals(rows, asOf, pools), from, daysEnd)
	withAux := auxAvailable(pools)

	entries := []*apiv1.UserUsageEntry{}
	var cur *apiv1.UserUsageEntry
	var curAux float64
	flush := func() {
		if cur == nil {
			return
		}
		if withAux {
			aux := curAux
			cur.AuxContainerSeconds = &aux
		}
		entries = append(entries, cur)
	}
	for i, d := range days {
		day := from.AddDate(0, 0, i)
		label := day.Format(usageDayLayout)
		if monthly {
			label = day.Format(usageMonthLayout)
		}
		if cur == nil || cur.PeriodStart != label {
			flush()
			cur, curAux = &apiv1.UserUsageEntry{PeriodStart: label}, 0
		}
		cur.GpuSeconds += d.gpu
		cur.CpuSlotSeconds += d.cpuSlot
		curAux += d.aux
		cur.ActiveSeconds += d.active
		if d.active > 0 {
			cur.ActiveDays++
		}
	}
	flush()
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
