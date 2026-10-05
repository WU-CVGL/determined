//go:build integration

package db

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// withTestDynamicPoolSpec gives a record the spec columns that a create through the resource
// manager writes. Only spec rows can be replayed.
func withTestDynamicPoolSpec(record DynamicResourcePool, specHash string) DynamicResourcePool {
	spec := json.RawMessage(`{"pool_name":"` + record.PoolName + `"}`)
	specVersion := 1
	record.Spec = &spec
	record.SpecVersion = &specVersion
	record.SpecHash = &specHash
	return record
}

func TestDynamicResourcePoolCommittedInsertReadFailure(t *testing.T) {
	database, cleanup := MustResolveNewPostgresDatabase(t)
	defer cleanup()
	MustMigrateTestPostgres(t, database, "file://../../static/migrations", "up")

	originalRead := readCreatedDynamicResourcePool
	t.Cleanup(func() { readCreatedDynamicResourcePool = originalRead })
	readFailure := errors.New("injected post-insert read failure")
	readCreatedDynamicResourcePool = func(
		_ *PgDB, _ context.Context, _ string,
	) (DynamicResourcePool, error) {
		return DynamicResourcePool{}, readFailure
	}
	desired := withTestDynamicPoolSpec(DynamicResourcePool{
		ClusterName: "agents-a", PoolName: "post-insert-failure",
		ConfigVersion: 1, IdempotencyKey: "post-insert-failure",
		Config:     json.RawMessage(`{"pool_name":"post-insert-failure"}`),
		ConfigHash: "hash-a",
	}, "spec-hash-a")
	_, created, err := database.CreateDynamicResourcePool(context.Background(), desired)
	require.True(t, created)
	require.ErrorIs(t, err, readFailure)

	stored, err := database.DynamicResourcePoolByName(context.Background(), desired.PoolName)
	require.NoError(t, err)
	require.Equal(t, DynamicResourcePoolPending, stored.State)
	readCreatedDynamicResourcePool = originalRead
	replayed, created, err := database.CreateDynamicResourcePool(context.Background(), desired)
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, stored.CreatedAt, replayed.CreatedAt)
}

func TestDynamicResourcePoolCommittedInsertRequestCanceled(t *testing.T) {
	database, cleanup := MustResolveNewPostgresDatabase(t)
	defer cleanup()
	MustMigrateTestPostgres(t, database, "file://../../static/migrations", "up")

	originalRead := readCreatedDynamicResourcePool
	t.Cleanup(func() { readCreatedDynamicResourcePool = originalRead })
	ctx, cancel := context.WithCancel(context.Background())
	readCreatedDynamicResourcePool = func(
		database *PgDB, ctx context.Context, name string,
	) (DynamicResourcePool, error) {
		cancel()
		return database.DynamicResourcePoolByName(ctx, name)
	}
	desired := DynamicResourcePool{
		ClusterName: "agents-a", PoolName: "post-insert-canceled",
		ConfigVersion: 1, IdempotencyKey: "post-insert-canceled",
		Config:     json.RawMessage(`{"pool_name":"post-insert-canceled"}`),
		ConfigHash: "hash-a",
	}
	_, created, err := database.CreateDynamicResourcePool(ctx, desired)
	require.True(t, created)
	require.ErrorIs(t, err, context.Canceled)
	stored, err := database.DynamicResourcePoolByName(context.Background(), desired.PoolName)
	require.NoError(t, err)
	require.Equal(t, DynamicResourcePoolPending, stored.State)
}

func TestDynamicResourcePoolPersistenceAndIdempotency(t *testing.T) {
	database, cleanup := MustResolveNewPostgresDatabase(t)
	defer cleanup()
	MustMigrateTestPostgres(t, database, "file://../../static/migrations", "up")

	ctx := context.Background()
	desired := withTestDynamicPoolSpec(DynamicResourcePool{
		ClusterName:    "agents-a",
		PoolName:       "online-a",
		ConfigVersion:  1,
		IdempotencyKey: "operation-a",
		Config:         json.RawMessage(`{"pool_name":"online-a"}`),
		ConfigHash:     "hash-a",
	}, "spec-hash-a")
	createdRecord, created, err := database.CreateDynamicResourcePool(ctx, desired)
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, DynamicResourcePoolPending, createdRecord.State)

	replayed, created, err := database.CreateDynamicResourcePool(ctx, desired)
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, createdRecord.PoolName, replayed.PoolName)
	require.Equal(t, createdRecord.CreatedAt, replayed.CreatedAt)

	conflictingKey := desired
	conflictingKey.PoolName = "online-b"
	conflictingKey.ConfigHash = "hash-b"
	_, _, err = database.CreateDynamicResourcePool(ctx, conflictingKey)
	require.ErrorIs(t, err, ErrDynamicResourcePoolConflict)

	conflictingName := desired
	conflictingName.IdempotencyKey = "operation-b"
	_, _, err = database.CreateDynamicResourcePool(ctx, conflictingName)
	require.ErrorIs(t, err, ErrDynamicResourcePoolConflict)

	message := "initialization failed"
	failed, err := database.SetDynamicResourcePoolState(
		ctx, desired.PoolName, DynamicResourcePoolFailed, &message,
	)
	require.NoError(t, err)
	require.Equal(t, DynamicResourcePoolFailed, failed.State)
	require.Equal(t, message, *failed.Error)
	pending, err := database.BeginDynamicResourcePoolRetry(
		ctx, desired.ClusterName, desired.PoolName,
	)
	require.NoError(t, err)
	require.Equal(t, DynamicResourcePoolPending, pending.State)
	_, err = database.BeginDynamicResourcePoolRetry(ctx, desired.ClusterName, desired.PoolName)
	require.ErrorIs(t, err, ErrDynamicResourcePoolNotFailed)

	// Reconnect to the same database to exercise the restart read path rather than an in-memory
	// object retained by the writer.
	restarted, err := ConnectPostgres(database.URL)
	require.NoError(t, err)
	defer func() { require.NoError(t, restarted.Close()) }()
	restored, err := restarted.ListDynamicResourcePools(ctx, desired.ClusterName)
	require.NoError(t, err)
	require.Len(t, restored, 1)
	require.Equal(t, desired.ConfigHash, restored[0].ConfigHash)
	require.JSONEq(t, string(*desired.Spec), string(*restored[0].Spec))
	require.Equal(t, 1, *restored[0].SpecVersion)
	require.Equal(t, *desired.SpecHash, *restored[0].SpecHash)
	require.EqualValues(t, 1, restored[0].Revision)
	require.Equal(t, DynamicResourcePoolPending, restored[0].State)
}

func TestDynamicResourcePoolConcurrentCreate(t *testing.T) {
	database, cleanup := MustResolveNewPostgresDatabase(t)
	defer cleanup()
	MustMigrateTestPostgres(t, database, "file://../../static/migrations", "up")

	desired := withTestDynamicPoolSpec(DynamicResourcePool{
		ClusterName:    "agents-a",
		PoolName:       "concurrent-online",
		ConfigVersion:  1,
		IdempotencyKey: "concurrent-operation",
		Config:         json.RawMessage(`{"pool_name":"concurrent-online"}`),
		ConfigHash:     "concurrent-hash",
	}, "concurrent-spec-hash")
	const creators = 8
	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(creators)
	created := make(chan bool, creators)
	errs := make(chan error, creators)
	for i := 0; i < creators; i++ {
		go func() {
			defer wait.Done()
			<-start
			_, wasCreated, err := database.CreateDynamicResourcePool(context.Background(), desired)
			created <- wasCreated
			errs <- err
		}()
	}
	close(start)
	wait.Wait()
	close(created)
	close(errs)
	createdCount := 0
	for wasCreated := range created {
		if wasCreated {
			createdCount++
		}
	}
	for err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, 1, createdCount)
	records, err := database.ListDynamicResourcePools(context.Background(), desired.ClusterName)
	require.NoError(t, err)
	require.Len(t, records, 1)
}

func TestDynamicResourcePoolLegacyInsertDefaults(t *testing.T) {
	database, cleanup := MustResolveNewPostgresDatabase(t)
	defer cleanup()
	MustMigrateTestPostgres(t, database, "file://../../static/migrations", "up")
	ctx := context.Background()

	// This is the column list that masters without spec columns insert.
	_, err := database.sql.ExecContext(ctx, `
INSERT INTO dynamic_resource_pools
    (cluster_name, pool_name, config_version, idempotency_key, config, config_hash, state)
VALUES ('agents-a', 'legacy', 1, 'legacy-operation', '{"pool_name":"legacy"}', 'hash', 'Pending')`)
	require.NoError(t, err)
	legacy, err := database.DynamicResourcePoolByName(ctx, "legacy")
	require.NoError(t, err)
	require.EqualValues(t, 1, legacy.Revision)
	require.Nil(t, legacy.Spec)
	require.Nil(t, legacy.SpecVersion)
	require.Nil(t, legacy.SpecHash)

	_, err = database.sql.ExecContext(ctx, `
INSERT INTO dynamic_resource_pools
    (cluster_name, pool_name, config_version, idempotency_key, config, config_hash,
     spec, spec_version, state)
VALUES ('agents-a', 'partial', 1, 'partial-operation', '{"pool_name":"partial"}', 'hash',
        '{"pool_name":"partial"}', 1, 'Pending')`)
	require.ErrorContains(t, err, "dynamic_resource_pools_spec_complete")
}

func TestCreateReplayComparesSpecHash(t *testing.T) {
	database, cleanup := MustResolveNewPostgresDatabase(t)
	defer cleanup()
	MustMigrateTestPostgres(t, database, "file://../../static/migrations", "up")
	ctx := context.Background()

	desired := withTestDynamicPoolSpec(DynamicResourcePool{
		ClusterName: "agents-a", PoolName: "replayed", ConfigVersion: 1,
		IdempotencyKey: "replayed-operation",
		Config:         json.RawMessage(`{"pool_name":"replayed","description":"before"}`),
		ConfigHash:     "snapshot-a",
	}, "spec-a")
	original, created, err := database.CreateDynamicResourcePool(ctx, desired)
	require.NoError(t, err)
	require.True(t, created)

	// A startup refresh rewrites the snapshot but leaves the spec and its hash alone.
	ready, err := database.MarkDynamicResourcePoolReady(ctx, desired.PoolName,
		&DynamicResourcePoolSnapshot{
			Config:     json.RawMessage(`{"pool_name":"replayed","description":"after"}`),
			ConfigHash: "snapshot-b",
		})
	require.NoError(t, err)
	require.Equal(t, DynamicResourcePoolReady, ready.State)
	require.Equal(t, "snapshot-b", ready.ConfigHash)
	require.JSONEq(t, `{"pool_name":"replayed","description":"after"}`, string(ready.Config))
	require.Equal(t, *original.SpecHash, *ready.SpecHash)
	require.JSONEq(t, string(*original.Spec), string(*ready.Spec))
	require.Equal(t, original.Revision, ready.Revision)

	replayed, created, err := database.CreateDynamicResourcePool(ctx, desired)
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, original.CreatedAt, replayed.CreatedAt)
	require.Equal(t, "snapshot-b", replayed.ConfigHash)

	differentSpec := desired
	differentHash := "spec-b"
	differentSpec.SpecHash = &differentHash
	_, _, err = database.CreateDynamicResourcePool(ctx, differentSpec)
	require.ErrorIs(t, err, ErrDynamicResourcePoolConflict)

	// A row written without a spec is never a replay, even for an identical request.
	legacy := DynamicResourcePool{
		ClusterName: "agents-a", PoolName: "legacy", ConfigVersion: 1,
		IdempotencyKey: "legacy-operation",
		Config:         json.RawMessage(`{"pool_name":"legacy"}`),
		ConfigHash:     "legacy-snapshot",
	}
	_, created, err = database.CreateDynamicResourcePool(ctx, legacy)
	require.NoError(t, err)
	require.True(t, created)
	_, _, err = database.CreateDynamicResourcePool(ctx, legacy)
	require.ErrorIs(t, err, ErrDynamicResourcePoolConflict)
	_, _, err = database.CreateDynamicResourcePool(ctx, withTestDynamicPoolSpec(legacy, "spec-c"))
	require.ErrorIs(t, err, ErrDynamicResourcePoolConflict)

	// Without a snapshot, marking Ready only changes the state and clears the error.
	failure := "initialization failed"
	_, err = database.SetDynamicResourcePoolState(
		ctx, legacy.PoolName, DynamicResourcePoolFailed, &failure,
	)
	require.NoError(t, err)
	marked, err := database.MarkDynamicResourcePoolReady(ctx, legacy.PoolName, nil)
	require.NoError(t, err)
	require.Equal(t, DynamicResourcePoolReady, marked.State)
	require.Nil(t, marked.Error)
	require.Equal(t, legacy.ConfigHash, marked.ConfigHash)
	require.JSONEq(t, string(legacy.Config), string(marked.Config))
	require.Nil(t, marked.Spec)
	_, err = database.MarkDynamicResourcePoolReady(ctx, "missing", nil)
	require.ErrorIs(t, err, ErrDynamicResourcePoolNotFound)
}
