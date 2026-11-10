//go:build integration
// +build integration

package olddata

import (
	"testing"

	"github.com/determined-ai/determined/master/internal/db"
)

// migrationBeforeAggregatesRebuild is the last migration before the one that keeps the resource
// aggregates in resource_aggregates_legacy and empties resource_aggregates.
const migrationBeforeAggregatesRebuild = 20261108010744

// ResourceAggregatesRow is a row of resource_aggregates.
type ResourceAggregatesRow struct {
	Date            string
	AggregationType string
	AggregationKey  string
	Seconds         float64
}

// OldResourceAggregates are the rows that MigrateWithResourceAggregates adds before the rebuild,
// including usage of a user and a pool that no allocation record has.
var OldResourceAggregates = []ResourceAggregatesRow{
	{"2026-01-01", "total", "total", 7200},
	{"2026-01-01", "username", "olddata-deleted-user", 7200},
	{"2026-01-01", "resource_pool", "olddata-pool", 7200},
	{"2026-01-01", "queued", "total", 30},
	{"2026-01-02", "total", "total", 0.5},
}

const resourceAggregatesSQL = `
INSERT INTO resource_aggregates (date, aggregation_type, aggregation_key, seconds) VALUES
    ('2026-01-01', 'total', 'total', 7200),
    ('2026-01-01', 'username', 'olddata-deleted-user', 7200),
    ('2026-01-01', 'resource_pool', 'olddata-pool', 7200),
    ('2026-01-01', 'queued', 'total', 30),
    ('2026-01-02', 'total', 'total', 0.5);
`

// MigrateWithResourceAggregates migrates an empty database fully, with the rows of
// OldResourceAggregates added to resource_aggregates before the rebuild.
func MigrateWithResourceAggregates(t *testing.T, pgdb *db.PgDB, migrationsPath string) {
	mustMigrateWithExtras(t, pgdb, migrationsPath, migrationExtra{
		When: migrationBeforeAggregatesRebuild,
		SQL:  resourceAggregatesSQL,
	})
}
