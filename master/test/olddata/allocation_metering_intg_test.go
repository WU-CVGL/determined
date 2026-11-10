//go:build integration
// +build integration

package olddata

import (
	"context"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/pkg/etc"
	"github.com/determined-ai/determined/master/pkg/ptrs"
)

// migrationBeforeAllocationMetering is the last migration before the one that adds the metering
// columns to allocations.
const migrationBeforeAllocationMetering = 20261110030311

// The metering migration counts an allocation from before metering as GPUs, one per slot, when it
// has slots and its task container recorded GPUs of type cuda or rocm with at least one UUID. Every
// other one is of an unknown kind.
func TestMigrateClassifiesAllocationsFromAcceleratorRecords(t *testing.T) {
	require.NoError(t, etc.SetRootPath("../../static/srv"))
	pgDB, cleanup := db.MustResolveNewPostgresDatabase(t)
	defer cleanup()
	migrations := "file://../../static/migrations"
	db.MustMigrateTestPostgres(t, pgDB, migrations, "up",
		strconv.Itoa(migrationBeforeAllocationMetering))

	type record struct {
		accelerator string
		uuids       string
	}
	cases := []struct {
		name     string
		slots    int
		records  []record
		kind     string
		quantity *int
	}{
		{
			name: "cuda", slots: 2,
			records: []record{{"cuda", `{GPU-0a6bc1f2-1111-4c4e-9d1a-000000000001,` +
				`GPU-0a6bc1f2-1111-4c4e-9d1a-000000000002}`}},
			kind: "gpu", quantity: ptrs.Ptr(2),
		},
		{
			name: "rocm", slots: 1,
			records: []record{{"rocm", `{GPU-7c1d0e3a-2222-4b5f-8e2b-000000000003}`}},
			kind:    "gpu", quantity: ptrs.Ptr(1),
		},
		{name: "cpu", slots: 1, records: []record{{"cpu", `{}`}}, kind: "unclassified"},
		{name: "no record", slots: 1, kind: "unclassified"},
		{name: "cuda without uuids", slots: 1, records: []record{{"cuda", `{}`}}, kind: "unclassified"},
		{name: "cuda with null uuids", slots: 1, records: []record{{"cuda", ""}}, kind: "unclassified"},
		{name: "no slots, cpu", records: []record{{"cpu", `{}`}}, kind: "unclassified"},
		{name: "no slots, no record", kind: "unclassified"},
		{
			name: "no slots, cuda", records: []record{{"cuda", `{GPU-9e8f7a6b-3333-4a1c-b2d3-000000000004}`}},
			kind: "unclassified",
		},
	}
	for i, c := range cases {
		taskID := "olddata-metering-" + strconv.Itoa(i)
		allocationID := taskID + ".0"
		pgDB.MustExec(t, `INSERT INTO tasks (task_id, task_type, start_time)
			VALUES ($1, 'COMMAND', now())`, taskID)
		pgDB.MustExec(t, `INSERT INTO allocations (task_id, allocation_id, resource_pool, slots)
			VALUES ($1, $2, 'default', $3)`, taskID, allocationID, c.slots)
		for j, r := range c.records {
			var uuids *string
			if r.uuids != "" {
				uuids = &r.uuids
			}
			pgDB.MustExec(t, `INSERT INTO allocation_accelerators
				(container_id, allocation_id, node_name, accelerator_type, accelerator_uuids)
				VALUES ($1, $2, 'agent', $3, $4::text[])`,
				allocationID+"-"+strconv.Itoa(j), allocationID, r.accelerator, uuids)
		}
	}

	db.MustMigrateTestPostgres(t, pgDB, migrations, "up")

	for i, c := range cases {
		var got struct {
			Kind     *string `bun:"metered_kind"`
			Quantity *int    `bun:"metered_quantity"`
			Assigned *string `bun:"assigned_time"`
		}
		require.NoError(t, db.Bun().NewRaw(`SELECT metered_kind, metered_quantity, assigned_time::text
			FROM allocations WHERE allocation_id = ?`, "olddata-metering-"+strconv.Itoa(i)+".0").
			Scan(context.Background(), &got), c.name)
		require.NotNil(t, got.Kind, c.name)
		require.Equal(t, c.kind, *got.Kind, c.name)
		require.Equal(t, c.quantity, got.Quantity, c.name)
		require.Nil(t, got.Assigned, c.name)
	}
}
