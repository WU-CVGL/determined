package db

import (
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

// GetOrCreateClusterID queries the master uuid in the database, adding one if it doesn't exist.
// If a nonempty telemetryID is provided, it will be the one added, otherwise a uuid is generated.
func (db *PgDB) GetOrCreateClusterID(telemetryID string) (string, error) {
	var newUUID string
	if telemetryID != "" {
		newUUID = telemetryID
	} else {
		newUUID = uuid.New().String()
	}
	if _, err := db.sql.Exec(`
INSERT INTO cluster_id (cluster_id) SELECT ($1)
WHERE NOT EXISTS ( SELECT * FROM cluster_id );
`, newUUID); err != nil {
		return "", errors.Wrapf(err, "error initializing cluster_id in cluster_id table")
	}

	var uuidVal []string

	if err := db.sql.Select(&uuidVal, `SELECT cluster_id FROM cluster_id`); err != nil {
		return "", errors.Wrapf(err, "error reading cluster_id from cluster_id table")
	}
	if len(uuidVal) != 1 {
		return "", errors.Errorf(
			"expecting exactly one cluster_id from cluster_id table, %d values found", len(uuidVal),
		)
	}

	clusterID := uuidVal[0]
	if telemetryID != "" && telemetryID != clusterID {
		log.Warnf(
			"a telemetry cluster id: %s was provided, but cluster is already initialized with id: %s. ignoring.",
			telemetryID, clusterID)
	}

	return clusterID, nil
}

// UpdateClusterHeartBeat updates the clusterheartbeat column in the cluster_id table.
func (db *PgDB) UpdateClusterHeartBeat(currentClusterHeartbeat time.Time) error {
	_, err := db.sql.Exec(`UPDATE cluster_id SET cluster_heartbeat = $1`, currentClusterHeartbeat)
	return errors.Wrap(err, "updating cluster heartbeat")
}

// PeriodicTelemetryInfo returns anonymous information about the usage of the current
// Determined cluster.
func (db *PgDB) PeriodicTelemetryInfo() ([]byte, error) {
	return db.rawQuery(`
SELECT jsonb_build_object(
    'num_users', (SELECT count(*) FROM users),
    'num_experiments', (SELECT count(*) FROM experiments),
    'num_trials', (SELECT count(*) FROM trials),
	'num_jobs', (SELECT count(*) FROM jobs),
	'num_tasks', (SELECT count(*) FROM tasks),
	'num_allocations', (SELECT count(*) FROM allocations),
    'num_allocations_today', (
		SELECT count(*) FROM allocations WHERE DATE(start_time) >= CURRENT_DATE
	),
	'num_workspaces', (SELECT count(*) FROM workspaces),
	'num_projects', (SELECT count(*) FROM projects),
	'avg_projects_per_workspace', (
		(SELECT count(*) FROM projects WHERE id > 1)::float
		/ (SELECT greatest(count(*), 1) FROM workspaces WHERE id > 1)
	),
	'avg_experiments_per_project', (
		(SELECT count(*) FROM experiments WHERE project_id > 1)::float
		/ (SELECT greatest(count(*), 1) FROM projects WHERE id > 1)
	),
	'notes_gt_zero', (
		(SELECT count(*) FROM projects WHERE id > 1 AND jsonb_array_length(notes) > 0)::float
		/ (SELECT greatest(count(*), 1) FROM projects WHERE id > 1)
	),
	'notes_gt_one', (
		(SELECT count(*) FROM projects WHERE id > 1 AND jsonb_array_length(notes) > 1)::float
		/ (SELECT greatest(count(*), 1) FROM projects WHERE id > 1)
	)
);
`)
}

// UpdateResourceAllocationAggregation updates the aggregated resource allocation table.
func (db *PgDB) UpdateResourceAllocationAggregation() error {
	return db.updateResourceAllocationAggregation(time.Now())
}

// updateResourceAllocationAggregation aggregates each full UTC day that has ended before now.
//
// A day is aggregated for the first time once it has ended: the days after the last aggregated
// day are always computed. An allocation that is still running counts until the end of each day it
// overlaps, so the rows of a day it overlaps are recomputed on every run until it has ended: the
// computation also starts at the day of the earliest start among the allocations that are still
// running or that ended on or after the last aggregated day. The last aggregated day is included
// because an allocation counted there as running can get an end time inside that day afterwards:
// a restart closes the allocations of a failed master at its last heartbeat. A recomputed day
// replaces all its rows in one transaction, so readers see either the previous or the new rows of
// a day.
func (db *PgDB) updateResourceAllocationAggregation(now time.Time) error {
	var lastDatePtr *time.Time
	err := db.sql.QueryRow(
		`SELECT date_trunc('day', max(date)::timestamp) FROM resource_aggregates`,
	).Scan(&lastDatePtr)
	if err != nil {
		return errors.Wrap(err, "failed to find last aggregate")
	}

	// The earliest day with an allocation that has to be (re)aggregated: all allocations when
	// nothing was aggregated yet, otherwise those still running or that ended on or after the
	// start of the last aggregated day.
	var firstDatePtr *time.Time
	if lastDatePtr == nil {
		err = db.sql.QueryRow(
			`SELECT date_trunc('day', min(start_time)) FROM allocations`,
		).Scan(&firstDatePtr)
	} else {
		err = db.sql.QueryRow(`
SELECT date_trunc('day', min(start_time)) FROM allocations
WHERE start_time IS NOT NULL AND (end_time IS NULL OR end_time >= $1)`,
			lastDatePtr.UTC(),
		).Scan(&firstDatePtr)
	}
	if err != nil {
		return errors.Wrap(err, "failed to find first allocation to aggregate")
	}

	// The values periodStart takes on are all midnight UTC (because of date_trunc) for each day that
	// is to be aggregated.
	periodStart, ok := aggregationStart(lastDatePtr, firstDatePtr)
	if !ok {
		// No allocations found; nothing to do.
		return nil
	}

	// targetDate is some time during the day before today, which is the last full day that has ended
	// and can therefore be aggregated; the Before check means that the last value of periodStart is
	// midnight at the beginning of that day.
	targetDate := now.UTC().AddDate(0, 0, -1)
	for ; periodStart.Before(targetDate); periodStart = periodStart.AddDate(0, 0, 1) {
		t0 := time.Now()

		if err := db.withTransaction("aggregate resource allocation", func(tx *sqlx.Tx) error {
			// The day's rows are replaced: keys that no longer have usage on that day go away.
			if _, err := tx.Exec(
				`DELETE FROM resource_aggregates WHERE date = $1::date`, periodStart,
			); err != nil {
				return errors.Wrap(err, "failed to delete aggregates")
			}
			if _, err := tx.Exec(
				db.queries.GetOrLoad("update_aggregated_allocation"), periodStart,
			); err != nil {
				return errors.Wrap(err, "failed to add aggregate allocation")
			}
			if _, err := tx.Exec(
				db.queries.GetOrLoad("update_aggregated_queued_time"), periodStart,
			); err != nil {
				return errors.Wrap(err, "failed to add aggregate queued time")
			}
			return nil
		}); err != nil {
			return err
		}

		log.Infof(
			"aggregated resource allocation statistics for %v in %s",
			periodStart, time.Since(t0),
		)
	}
	return nil
}

// aggregationStart returns the first day to aggregate, given the last aggregated day (nil when
// there is none) and the day of the earliest allocation that has to be aggregated (nil when there
// is none). It returns false when there is nothing to aggregate.
func aggregationStart(lastDate, firstDate *time.Time) (time.Time, bool) {
	switch {
	case lastDate == nil && firstDate == nil:
		return time.Time{}, false
	case lastDate == nil:
		return firstDate.UTC(), true
	}
	next := lastDate.UTC().AddDate(0, 0, 1)
	if firstDate != nil && firstDate.Before(next) {
		return firstDate.UTC(), true
	}
	return next, true
}
