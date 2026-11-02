package internal

import (
	"context"
	"fmt"
	"time"

	"github.com/uptrace/bun"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/determined-ai/determined/master/internal/db"
)

// allocationStartTarget is an already-authorized list entry and its optional start timestamp.
type allocationStartTarget struct {
	id   string
	dest **timestamppb.Timestamp
}

// enrichAllocationStartTimes reads the first resource assignment of all entries in one query.
// A QUEUED task_stats row is written only when resources are assigned. Unlike an allocation's
// accounting start_time, it cannot be synthesized for a never-started allocation at master restart.
// For a generic task this includes all its runs, but no descendants; for an experiment all trials.
func enrichAllocationStartTimes(ctx context.Context, targets []allocationStartTarget, experiments bool) error {
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
	query := db.Bun().NewSelect().TableExpr("allocations AS a").
		Join("JOIN task_stats AS ts ON ts.allocation_id = a.allocation_id").
		Where("ts.event_type = 'QUEUED'").
		Where("ts.end_time IS NOT NULL").
		ColumnExpr("min(ts.end_time) AS start")
	if experiments {
		query = query.Join("JOIN run_id_task_id AS rt ON rt.task_id = a.task_id").
			Join("JOIN trials AS t ON t.id = rt.run_id").
			ColumnExpr("t.experiment_id::text AS id").
			Where("t.experiment_id IN (?)", bun.In(ids)).GroupExpr("t.experiment_id")
	} else {
		query = query.ColumnExpr("a.task_id AS id").
			Where("a.task_id IN (?)", bun.In(ids)).GroupExpr("a.task_id")
	}
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
