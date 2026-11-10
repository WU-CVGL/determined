//go:build integration
// +build integration

package olddata

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/pkg/etc"
)

// The migration keeps the old aggregates in resource_aggregates_legacy and empties
// resource_aggregates for the rebuild.
func TestMigrateKeepsLegacyResourceAggregates(t *testing.T) {
	require.NoError(t, etc.SetRootPath("../../static/srv"))
	pgDB, cleanup := db.MustResolveNewPostgresDatabase(t)
	defer cleanup()
	ctx := context.Background()

	MigrateWithResourceAggregates(t, pgDB, "file://../../static/migrations")

	var n int
	require.NoError(t, db.Bun().NewRaw(`SELECT count(*) FROM resource_aggregates`).Scan(ctx, &n))
	require.Zero(t, n)

	var legacy []ResourceAggregatesRow
	require.NoError(t, db.Bun().NewRaw(`
SELECT date::text AS date, aggregation_type, aggregation_key, seconds
FROM resource_aggregates_legacy`).Scan(ctx, &legacy))
	require.ElementsMatch(t, OldResourceAggregates, legacy)

	// The legacy table keeps the key of resource_aggregates.
	_, err := db.Bun().NewRaw(`
INSERT INTO resource_aggregates_legacy (date, aggregation_type, aggregation_key, seconds)
VALUES ('2026-01-01', 'total', 'total', 1)`).Exec(ctx)
	require.ErrorContains(t, err, "duplicate key")
}
