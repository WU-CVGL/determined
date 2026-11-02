package internal

import (
	"context"
	"fmt"
	"time"

	"github.com/uptrace/bun"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/pkg/model"
)

// QUEUED ends at confirmed device assignment only for the Agent RM. In particular the
// Kubernetes RM emits ResourcesAllocated before a Pod is scheduled. Until backend-specific
// assignment events are available, do not infer starts in non-Agent or mixed clusters.
func agentOnlyAllocationStarts(conf config.ResourceConfig) bool {
	for _, manager := range conf.ResourceManagers() {
		if manager == nil || manager.ResourceManager == nil || manager.ResourceManager.AgentRM == nil ||
			manager.ResourceManager.KubernetesRM != nil || manager.ResourceManager.DispatcherRM != nil ||
			manager.ResourceManager.PbsRM != nil {
			return false
		}
	}
	return true
}

func (a *apiServer) enrichAllocationStartTimes(
	ctx context.Context, targets []allocationStartTarget, scope allocationStartScope,
) error {
	if !agentOnlyAllocationStarts(a.m.config.ResourceConfig) {
		for _, target := range targets {
			*target.dest = nil
		}
		return nil
	}
	return enrichAllocationStartTimes(ctx, targets, scope)
}

// allocationStartTarget is an already-authorized list entry and its optional start timestamp.
type allocationStartTarget struct {
	id   string
	dest **timestamppb.Timestamp
}

type allocationStartScope int

const (
	allocationStartTask allocationStartScope = iota
	allocationStartExperiment
	allocationStartJob
)

// enrichAllocationStartTimes reads the first resource assignment of all entries in one query.
// In an Agent-only cluster, a QUEUED task_stats row ends when resources are assigned. Unlike an
// allocation's accounting start_time, it cannot be synthesized at restart for a never-started run.
// For a generic task this includes all its runs, but no descendants; for an experiment all trials.
func enrichAllocationStartTimes(ctx context.Context, targets []allocationStartTarget, scope allocationStartScope) error {
	if len(targets) == 0 {
		return nil
	}
	ids := make([]string, 0, len(targets))
	for _, target := range targets {
		ids = append(ids, target.id)
	}
	var rows []struct {
		ID    string    `bun:"id"`
		Start time.Time `bun:"start"`
	}
	query := allocationStartTimesQuery(ids, scope)
	if err := query.Scan(ctx, &rows); err != nil {
		return fmt.Errorf("reading allocation start times: %w", err)
	}
	starts := make(map[string]*timestamppb.Timestamp, len(rows))
	for _, row := range rows {
		starts[row.ID] = timestamppb.New(row.Start)
	}
	for _, target := range targets {
		*target.dest = starts[target.id]
	}
	return nil
}

func completedQueuedAllocationStatsQuery() *bun.SelectQuery {
	return db.Bun().NewSelect().TableExpr("allocations AS a").
		Join("JOIN task_stats AS ts ON ts.allocation_id = a.allocation_id").
		Where("ts.event_type = 'QUEUED'").
		Where("ts.end_time IS NOT NULL").
		ColumnExpr("min(ts.end_time) AS start")
}

// experimentAllocationStartsQuery is shared by page enrichment and Started sorting. It returns
// a raw timestamp; callers choose the experiment scope and convert only the output to protobuf.
func experimentAllocationStartsQuery() *bun.SelectQuery {
	return completedQueuedAllocationStatsQuery().
		Join("JOIN run_id_task_id AS rt ON rt.task_id = a.task_id").
		Join("JOIN trials AS t ON t.id = rt.run_id")
}

func allocationStartTimesQuery(ids []string, scope allocationStartScope) *bun.SelectQuery {
	query := completedQueuedAllocationStatsQuery()
	switch scope {
	case allocationStartExperiment:
		query = experimentAllocationStartsQuery().
			ColumnExpr("t.experiment_id::text AS id").
			Where("t.experiment_id IN (?)", bun.In(ids)).GroupExpr("t.experiment_id")
	case allocationStartJob:
		query = query.Join("JOIN tasks AS t ON t.task_id = a.task_id").
			ColumnExpr("t.job_id AS id").
			Where("t.job_id IN (?)", bun.In(ids)).
			// GC persists its experiment's job ID, even though the scheduler gives GC its own
			// auxiliary job. It is not evidence that the experiment's training has started.
			Where("t.task_type != ?", model.TaskTypeCheckpointGC).GroupExpr("t.job_id")
	default:
		query = query.ColumnExpr("a.task_id AS id").
			Where("a.task_id IN (?)", bun.In(ids)).GroupExpr("a.task_id")
	}
	return query
}
