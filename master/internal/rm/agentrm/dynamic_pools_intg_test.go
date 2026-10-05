//go:build integration

package agentrm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/rm"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/task/taskmodel"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
)

func testDynamicPoolRMConfig(defaultPriority int) *config.ResourceManagerWithPoolsConfig {
	scheduler := config.DefaultSchedulerConfig()
	*scheduler.Priority.DefaultPriority = defaultPriority
	return &config.ResourceManagerWithPoolsConfig{
		ResourceManager: &config.ResourceManagerConfig{AgentRM: &config.AgentResourceManagerConfig{
			ClusterName:                "agent-cluster",
			DefaultComputeResourcePool: "default",
			DefaultAuxResourcePool:     "default",
			Scheduler:                  scheduler,
		}},
		ResourcePools: []config.ResourcePoolConfig{{
			PoolName:                 "default",
			MaxAuxContainersPerAgent: 100,
		}},
	}
}

// insertLegacyDynamicPool writes a record the way masters without spec support create one: the
// effective config resolved against masterDefaults and no spec.
func insertLegacyDynamicPool(
	t *testing.T,
	database *db.PgDB,
	poolName string,
	masterDefaults model.TaskContainerDefaultsConfig,
) db.DynamicResourcePool {
	normalized, err := testDynamicPoolRM().NormalizeDynamicResourcePoolConfig(
		config.ResourcePoolConfig{PoolName: poolName, MaxAuxContainersPerAgent: 100},
		masterDefaults,
	)
	require.NoError(t, err)
	raw, hash, err := marshalDynamicResourcePoolConfig(normalized)
	require.NoError(t, err)
	record, created, err := database.CreateDynamicResourcePool(
		context.Background(), db.DynamicResourcePool{
			ClusterName: "agent-cluster", PoolName: poolName,
			ConfigVersion: dynamicResourcePoolConfigVersion, IdempotencyKey: poolName + "-operation",
			Config: raw, ConfigHash: hash,
		},
	)
	require.NoError(t, err)
	require.True(t, created)
	return record
}

// insertSpecDynamicPool writes the record that a create through the resource manager writes,
// without waking its worker.
func insertSpecDynamicPool(
	t *testing.T,
	manager *ResourceManager,
	idempotencyKey string,
	spec string,
	masterDefaults model.TaskContainerDefaultsConfig,
) db.DynamicResourcePool {
	prepared, err := manager.prepareDynamicPoolSpec(json.RawMessage(spec), masterDefaults)
	require.NoError(t, err)
	record, created, err := manager.db.CreateDynamicResourcePool(
		context.Background(), prepared.record(manager.config.ClusterName, idempotencyKey),
	)
	require.NoError(t, err)
	require.True(t, created)
	return record
}

func waitForDynamicPoolReady(
	t *testing.T, database *db.PgDB, manager *ResourceManager, poolName string,
) {
	require.Eventually(t, func() bool {
		stored, readErr := database.DynamicResourcePoolByName(context.Background(), poolName)
		return readErr == nil && stored.State == db.DynamicResourcePoolReady &&
			manager.IsDynamicResourcePoolReady(poolName)
	}, 10*time.Second, 20*time.Millisecond)
}

func TestDynamicPoolPersistenceRestart(t *testing.T) {
	database, cleanup := db.MustResolveNewPostgresDatabase(t)
	defer cleanup()
	db.MustMigrateTestPostgres(t, database, "file://../../../static/migrations", "up")

	masterDefaults := *model.DefaultTaskContainerDefaults()
	masterDefaults.ForcePullImage = true
	insertLegacyDynamicPool(t, database, "legacy-frozen", masterDefaults)

	first, err := New(
		context.Background(), database, echo.New(), testDynamicPoolRMConfig(42), nil, nil,
		&masterDefaults,
	)
	require.NoError(t, err)
	defer first.stop()
	staticBefore, ok := first.registry.readyPool("default")
	require.True(t, ok)

	record, created, err := first.CreateDynamicResourcePool(
		context.Background(),
		"restart-operation",
		json.RawMessage(`{"pool_name":"online-restart","max_aux_containers_per_agent":100}`),
		masterDefaults,
	)
	require.NoError(t, err)
	require.True(t, created)
	require.Contains(t, []db.DynamicResourcePoolState{
		db.DynamicResourcePoolPending, db.DynamicResourcePoolReady,
	}, record.State)
	require.JSONEq(t, `{"max_aux_containers_per_agent":100,"pool_name":"online-restart"}`,
		string(*record.Spec))
	waitForDynamicPoolReady(t, database, first, record.PoolName)
	staticAfter, ok := first.registry.readyPool("default")
	require.True(t, ok)
	require.Same(t, staticBefore, staticAfter, "online create must preserve existing runtime pools")
	activeRevision, active := first.ActiveDynamicResourcePoolRevision(record.PoolName)
	require.True(t, active)
	require.EqualValues(t, 1, activeRevision)

	// The master is restarted with different task container defaults and RM scheduler.
	first.stop()
	changedMasterDefaults := *model.DefaultTaskContainerDefaults()
	changedMasterDefaults.ShmSizeBytes = 16 << 30
	restarted, err := New(
		context.Background(), database, echo.New(), testDynamicPoolRMConfig(7), nil, nil,
		&changedMasterDefaults,
	)
	require.NoError(t, err)
	defer restarted.stop()
	require.True(t, restarted.IsDynamicResourcePoolReady(record.PoolName))
	restartedDefaults, err := restarted.TaskContainerDefaults(
		rm.ResourcePoolName(record.PoolName), changedMasterDefaults,
	)
	require.NoError(t, err)
	require.Equal(t, changedMasterDefaults, restartedDefaults,
		"a created pool must inherit master defaults like a master.yaml pool")
	scheduler, ok := restarted.ResourcePoolSchedulerConfig(record.PoolName)
	require.True(t, ok)
	require.Equal(t, 7, *scheduler.Priority.DefaultPriority)
	restored, err := database.DynamicResourcePoolByName(context.Background(), record.PoolName)
	require.NoError(t, err)
	require.Equal(t, db.DynamicResourcePoolReady, restored.State)

	// A record written without a spec keeps running its frozen effective config.
	require.True(t, restarted.IsDynamicResourcePoolReady("legacy-frozen"))
	legacyDefaults, err := restarted.TaskContainerDefaults(
		rm.ResourcePoolName("legacy-frozen"), changedMasterDefaults,
	)
	require.NoError(t, err)
	require.True(t, legacyDefaults.ForcePullImage, "legacy dynamic defaults must remain frozen")
	require.EqualValues(t, 4<<30, legacyDefaults.ShmSizeBytes)
	legacyScheduler, ok := restarted.ResourcePoolSchedulerConfig("legacy-frozen")
	require.True(t, ok)
	require.Equal(t, 42, *legacyScheduler.Priority.DefaultPriority)
	legacy, err := database.DynamicResourcePoolByName(context.Background(), "legacy-frozen")
	require.NoError(t, err)
	require.Nil(t, legacy.Spec)
}

func TestDynamicPoolStartupRefreshesSnapshot(t *testing.T) {
	database, cleanup := db.MustResolveNewPostgresDatabase(t)
	defer cleanup()
	db.MustMigrateTestPostgres(t, database, "file://../../../static/migrations", "up")
	ctx := context.Background()

	masterDefaults := *model.DefaultTaskContainerDefaults()
	masterDefaults.ForcePullImage = true
	legacyBefore := insertLegacyDynamicPool(t, database, "legacy", masterDefaults)
	first, err := New(
		ctx, database, echo.New(), testDynamicPoolRMConfig(42), nil, nil, &masterDefaults,
	)
	require.NoError(t, err)
	defer first.stop()
	first.StopDynamicPoolWorker()
	insertSpecDynamicPool(t, first, "refresh-operation", `{"pool_name":"refreshed"}`, masterDefaults)
	first.stop()
	specBefore, err := database.DynamicResourcePoolByName(ctx, "refreshed")
	require.NoError(t, err)

	startWith := func(masterDefaults *model.TaskContainerDefaultsConfig) {
		manager, err := New(
			ctx, database, echo.New(), testDynamicPoolRMConfig(42), nil, nil, masterDefaults,
		)
		require.NoError(t, err)
		manager.stop()
	}

	// Without master defaults nothing is resolved, so no snapshot is written.
	startWith(nil)
	unchanged, err := database.DynamicResourcePoolByName(ctx, "refreshed")
	require.NoError(t, err)
	require.Equal(t, db.DynamicResourcePoolReady, unchanged.State)
	require.Equal(t, specBefore.ConfigHash, unchanged.ConfigHash)

	changedMasterDefaults := *model.DefaultTaskContainerDefaults()
	changedMasterDefaults.ShmSizeBytes = 16 << 30
	startWith(&changedMasterDefaults)
	refreshed, err := database.DynamicResourcePoolByName(ctx, "refreshed")
	require.NoError(t, err)
	require.NotEqual(t, specBefore.ConfigHash, refreshed.ConfigHash)
	require.JSONEq(t, string(*specBefore.Spec), string(*refreshed.Spec))
	require.Equal(t, *specBefore.SpecHash, *refreshed.SpecHash)
	require.Equal(t, specBefore.Revision, refreshed.Revision)
	snapshotRecord := refreshed
	snapshotRecord.Spec = nil
	snapshot, inherit, err := decodeStoredDynamicResourcePool(snapshotRecord)
	require.NoError(t, err)
	require.False(t, inherit)
	require.False(t, snapshot.TaskContainerDefaults.ForcePullImage)
	require.EqualValues(t, 16<<30, snapshot.TaskContainerDefaults.ShmSizeBytes)
	legacyAfter, err := database.DynamicResourcePoolByName(ctx, "legacy")
	require.NoError(t, err)
	require.Equal(t, legacyBefore.ConfigHash, legacyAfter.ConfigHash)
	require.JSONEq(t, string(legacyBefore.Config), string(legacyAfter.Config))

	// Defaults that no longer resolve keep the stored snapshot; the pool still starts.
	logs := logrustest.NewGlobal()
	t.Cleanup(logs.Reset)
	invalidMasterDefaults := changedMasterDefaults
	invalidMasterDefaults.ShmSizeBytes = -1
	startWith(&invalidMasterDefaults)
	kept, err := database.DynamicResourcePoolByName(ctx, "refreshed")
	require.NoError(t, err)
	require.Equal(t, refreshed.ConfigHash, kept.ConfigHash)
	warned := false
	for _, entry := range logs.AllEntries() {
		if entry.Level == logrus.WarnLevel && strings.Contains(entry.Message, `"refreshed"`) {
			warned = true
		}
	}
	require.True(t, warned, "an unresolvable snapshot must be reported")
}

func TestCreatedSnapshotReadableByLegacyMaster(t *testing.T) {
	database, cleanup := db.MustResolveNewPostgresDatabase(t)
	defer cleanup()
	db.MustMigrateTestPostgres(t, database, "file://../../../static/migrations", "up")
	ctx := context.Background()

	masterDefaults := *model.DefaultTaskContainerDefaults()
	masterDefaults.ShmSizeBytes = 16 << 30
	manager, err := New(
		ctx, database, echo.New(), testDynamicPoolRMConfig(42), nil, nil, &masterDefaults,
	)
	require.NoError(t, err)
	defer manager.stop()
	spec := json.RawMessage(`{"pool_name":"rollback","description":"d",
		"agent_reconnect_wait":"10m","task_container_defaults":{"add_capabilities":["CAP_X"]}}`)
	created, _, err := manager.CreateDynamicResourcePool(ctx, "rollback-operation", spec, masterDefaults)
	require.NoError(t, err)

	// Read the row with the column list of a master without spec support.
	legacy := db.DynamicResourcePool{}
	err = db.Bun().QueryRowContext(ctx, `
SELECT cluster_name, pool_name, config_version, idempotency_key, config, config_hash, state
FROM dynamic_resource_pools
WHERE pool_name = ?`, created.PoolName).Scan(
		&legacy.ClusterName, &legacy.PoolName, &legacy.ConfigVersion, &legacy.IdempotencyKey,
		&legacy.Config, &legacy.ConfigHash, &legacy.State,
	)
	require.NoError(t, err)
	require.Equal(t, 1, legacy.ConfigVersion)
	decoded, inherit, err := decodeStoredDynamicResourcePool(legacy)
	require.NoError(t, err)
	require.False(t, inherit)

	specConfig, err := decodeDynamicPoolSpec(*created.Spec)
	require.NoError(t, err)
	normalized, err := manager.NormalizeDynamicResourcePoolConfig(specConfig, masterDefaults)
	require.NoError(t, err)
	require.Equal(t, normalized, decoded)
	require.Equal(t, model.Duration(10*time.Minute), decoded.AgentReconnectWait)
}

func TestDynamicPoolPendingWorkerRecoversReplayWithoutRestart(t *testing.T) {
	database, cleanup := db.MustResolveNewPostgresDatabase(t)
	defer cleanup()
	db.MustMigrateTestPostgres(t, database, "file://../../../static/migrations", "up")
	rmConfig := &config.ResourceManagerWithPoolsConfig{
		ResourceManager: &config.ResourceManagerConfig{AgentRM: &config.AgentResourceManagerConfig{
			ClusterName: "agent-cluster", DefaultComputeResourcePool: "default",
			DefaultAuxResourcePool: "default", Scheduler: config.DefaultSchedulerConfig(),
		}},
		ResourcePools: []config.ResourcePoolConfig{{
			PoolName: "default", MaxAuxContainersPerAgent: 100,
		}},
	}
	manager, err := New(context.Background(), database, echo.New(), rmConfig, nil, nil, nil)
	require.NoError(t, err)
	defer manager.stop()
	staticPool, ok := manager.registry.readyPool("default")
	require.True(t, ok)
	originalCreate := createDynamicPoolRuntime
	t.Cleanup(func() { createDynamicPoolRuntime = originalCreate })
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var runtimeCreates atomic.Int32
	createDynamicPoolRuntime = func(
		manager *ResourceManager, cfg config.ResourcePoolConfig,
	) (*resourcePool, error) {
		if cfg.PoolName == "recover-pending" {
			runtimeCreates.Add(1)
			started <- struct{}{}
			<-release
		}
		return originalCreate(manager, cfg)
	}

	cfg := config.ResourcePoolConfig{PoolName: "recover-pending", MaxAuxContainersPerAgent: 100}
	spec := json.RawMessage(`{"pool_name":"recover-pending","max_aux_containers_per_agent":100}`)
	// This is the durable state left by an insert whose following read failed or was canceled.
	insertSpecDynamicPool(t, manager, "recover-key", string(spec), *model.DefaultTaskContainerDefaults())
	require.Eventually(t, func() bool {
		select {
		case <-started:
			return true
		default:
			return false
		}
	}, 10*time.Second, 20*time.Millisecond)

	const replays = 16
	var group sync.WaitGroup
	for i := 0; i < replays; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			replayed, wasCreated, replayErr := manager.CreateDynamicResourcePool(
				context.Background(), "recover-key", spec, *model.DefaultTaskContainerDefaults(),
			)
			if replayErr != nil || wasCreated || replayed.PoolName != cfg.PoolName {
				t.Errorf("replay: record=%+v created=%v err=%v", replayed, wasCreated, replayErr)
			}
		}()
	}
	group.Wait()
	releaseOnce.Do(func() { close(release) })
	require.Eventually(t, func() bool {
		stored, readErr := database.DynamicResourcePoolByName(context.Background(), cfg.PoolName)
		return readErr == nil && stored.State == db.DynamicResourcePoolReady &&
			manager.IsDynamicResourcePoolReady(cfg.PoolName)
	}, 10*time.Second, 20*time.Millisecond)
	runtime, ok := manager.registry.readyPool(cfg.PoolName)
	require.True(t, ok)
	_, _, err = manager.CreateDynamicResourcePool(
		context.Background(), "recover-key", spec, *model.DefaultTaskContainerDefaults(),
	)
	require.NoError(t, err)
	afterReplay, ok := manager.registry.readyPool(cfg.PoolName)
	require.True(t, ok)
	require.Same(t, runtime, afterReplay)
	require.EqualValues(t, 1, runtimeCreates.Load())
	staticAfter, ok := manager.registry.readyPool("default")
	require.True(t, ok)
	require.Same(t, staticPool, staticAfter)
}

func TestDynamicPoolReadyWorkerRecoversCommittedWriteError(t *testing.T) {
	database, cleanup := db.MustResolveNewPostgresDatabase(t)
	defer cleanup()
	db.MustMigrateTestPostgres(t, database, "file://../../../static/migrations", "up")
	rmConfig := &config.ResourceManagerWithPoolsConfig{
		ResourceManager: &config.ResourceManagerConfig{AgentRM: &config.AgentResourceManagerConfig{
			ClusterName: "agent-cluster", DefaultComputeResourcePool: "default",
			DefaultAuxResourcePool: "default", Scheduler: config.DefaultSchedulerConfig(),
		}},
		ResourcePools: []config.ResourcePoolConfig{{
			PoolName: "default", MaxAuxContainersPerAgent: 100,
		}},
	}
	manager, err := New(context.Background(), database, echo.New(), rmConfig, nil, nil, nil)
	require.NoError(t, err)
	defer manager.stop()
	manager.StopDynamicPoolWorker()
	staticPool, ok := manager.registry.readyPool("default")
	require.True(t, ok)

	originalSetState := setDynamicResourcePoolState
	originalStop := stopPreparedDynamicResourcePool
	originalCreate := createDynamicPoolRuntime
	t.Cleanup(func() {
		setDynamicResourcePoolState = originalSetState
		stopPreparedDynamicResourcePool = originalStop
		createDynamicPoolRuntime = originalCreate
	})
	type createdRuntime struct {
		pool *resourcePool
		cfg  config.ResourcePoolConfig
	}
	created := make(chan createdRuntime, 3)
	var stopped atomic.Int32
	var failRecovery atomic.Bool
	createDynamicPoolRuntime = func(
		manager *ResourceManager, cfg config.ResourcePoolConfig,
	) (*resourcePool, error) {
		if cfg.PoolName == "recover-ready" && failRecovery.CompareAndSwap(true, false) {
			return nil, errors.New("temporary runtime initialization failure")
		}
		pool, createErr := originalCreate(manager, cfg)
		if createErr == nil && cfg.PoolName == "recover-ready" {
			created <- createdRuntime{pool: pool, cfg: cfg}
		}
		return pool, createErr
	}
	stopPreparedDynamicResourcePool = func(pool *resourcePool) {
		stopped.Add(1)
		originalStop(pool)
	}
	setDynamicResourcePoolState = func(
		database *db.PgDB, ctx context.Context, poolName string,
		state db.DynamicResourcePoolState, errText *string,
	) (db.DynamicResourcePool, error) {
		if poolName == "recover-ready" {
			if state == db.DynamicResourcePoolReady {
				_, setErr := originalSetState(database, ctx, poolName, state, errText)
				if setErr != nil {
					return db.DynamicResourcePool{}, setErr
				}
				return db.DynamicResourcePool{}, errors.New("response lost after Ready commit")
			}
			if state == db.DynamicResourcePoolFailed {
				return db.DynamicResourcePool{}, errors.New("database unavailable for Failed write")
			}
		}
		return originalSetState(database, ctx, poolName, state, errText)
	}

	cfg := config.ResourcePoolConfig{PoolName: "recover-ready", MaxAuxContainersPerAgent: 100}
	defaults := *model.DefaultTaskContainerDefaults()
	defaults.ForcePullImage = true
	normalized, err := manager.NormalizeDynamicResourcePoolConfig(cfg, defaults)
	require.NoError(t, err)
	raw, hash, err := marshalDynamicResourcePoolConfig(normalized)
	require.NoError(t, err)
	record, wasCreated, err := database.CreateDynamicResourcePool(
		context.Background(), db.DynamicResourcePool{
			ClusterName: "agent-cluster", PoolName: cfg.PoolName,
			ConfigVersion: dynamicResourcePoolConfigVersion, IdempotencyKey: "recover-ready-key",
			Config: raw, ConfigHash: hash,
		},
	)
	require.NoError(t, err)
	require.True(t, wasCreated)
	_, err = manager.initializeDynamicResourcePool(context.Background(), record, normalized, false)
	require.ErrorIs(t, err, ErrDynamicResourcePoolPersistence)
	first := <-created
	require.EqualValues(t, 1, stopped.Load(), "the unpublishable prepared runtime must stop")
	require.False(t, manager.IsDynamicResourcePoolReady(cfg.PoolName))
	stored, err := database.DynamicResourcePoolByName(context.Background(), cfg.PoolName)
	require.NoError(t, err)
	require.Equal(t, db.DynamicResourcePoolReady, stored.State)

	// Restore database writes and change the master scheduler. The worker must use the saved config.
	setDynamicResourcePoolState = originalSetState
	failedCfg := normalized
	failedCfg.PoolName = "remain-failed"
	failedRaw, failedHash, err := marshalDynamicResourcePoolConfig(failedCfg)
	require.NoError(t, err)
	_, wasCreated, err = database.CreateDynamicResourcePool(
		context.Background(), db.DynamicResourcePool{
			ClusterName: "agent-cluster", PoolName: failedCfg.PoolName,
			ConfigVersion: dynamicResourcePoolConfigVersion, IdempotencyKey: "remain-failed-key",
			Config: failedRaw, ConfigHash: failedHash,
		},
	)
	require.NoError(t, err)
	require.True(t, wasCreated)
	failure := "requires an explicit retry"
	_, err = database.SetDynamicResourcePoolState(
		context.Background(), failedCfg.PoolName, db.DynamicResourcePoolFailed, &failure,
	)
	require.NoError(t, err)
	*manager.config.Scheduler.Priority.DefaultPriority = 99
	failRecovery.Store(true)
	manager.startDynamicPoolWorker(context.Background())
	require.Eventually(t, func() bool {
		return manager.IsDynamicResourcePoolReady(cfg.PoolName)
	}, 10*time.Second, 20*time.Millisecond)
	second := <-created
	require.NotSame(t, first.pool, second.pool)
	require.Equal(t, normalized, first.cfg)
	require.Equal(t, normalized, second.cfg)
	require.EqualValues(t, 1, stopped.Load())
	runtime, ok := manager.registry.readyPool(cfg.PoolName)
	require.True(t, ok)
	require.Same(t, second.pool, runtime)
	staticAfter, ok := manager.registry.readyPool("default")
	require.True(t, ok)
	require.Same(t, staticPool, staticAfter)
	stored, err = database.DynamicResourcePoolByName(context.Background(), cfg.PoolName)
	require.NoError(t, err)
	require.Equal(t, db.DynamicResourcePoolReady, stored.State)
	manager.StopDynamicPoolWorker()
	manager.advancePendingDynamicPools(context.Background())
	require.Len(t, created, 0, "reconciliation must not create another runtime")
	failed, err := database.DynamicResourcePoolByName(context.Background(), failedCfg.PoolName)
	require.NoError(t, err)
	require.Equal(t, db.DynamicResourcePoolFailed, failed.State)
	require.False(t, manager.IsDynamicResourcePoolReady(failedCfg.PoolName))
}

func TestDynamicPoolRetryWorkerStopsWithMasterContext(t *testing.T) {
	database, cleanup := db.MustResolveNewPostgresDatabase(t)
	defer cleanup()
	db.MustMigrateTestPostgres(t, database, "file://../../../static/migrations", "up")
	masterCtx, cancelMaster := context.WithCancel(context.Background())
	defer cancelMaster()
	rmConfig := &config.ResourceManagerWithPoolsConfig{
		ResourceManager: &config.ResourceManagerConfig{AgentRM: &config.AgentResourceManagerConfig{
			ClusterName: "agent-cluster", DefaultComputeResourcePool: "default",
			DefaultAuxResourcePool: "default", Scheduler: config.DefaultSchedulerConfig(),
		}},
		ResourcePools: []config.ResourcePoolConfig{{
			PoolName: "default", MaxAuxContainersPerAgent: 100,
		}},
	}
	manager, err := New(masterCtx, database, echo.New(), rmConfig, nil, nil, nil)
	require.NoError(t, err)
	defer manager.stop()
	// Keep the initial scanner out of the setup window while arranging a durable failure.
	manager.StopDynamicPoolWorker()
	cfg := config.ResourcePoolConfig{PoolName: "retry-failed", MaxAuxContainersPerAgent: 100}
	normalized, err := manager.NormalizeDynamicResourcePoolConfig(
		cfg, *model.DefaultTaskContainerDefaults(),
	)
	require.NoError(t, err)
	raw, hash, err := marshalDynamicResourcePoolConfig(normalized)
	require.NoError(t, err)
	_, created, err := database.CreateDynamicResourcePool(context.Background(), db.DynamicResourcePool{
		ClusterName: "agent-cluster", PoolName: cfg.PoolName,
		ConfigVersion: dynamicResourcePoolConfigVersion, IdempotencyKey: "retry-key",
		Config: raw, ConfigHash: hash,
	})
	require.NoError(t, err)
	require.True(t, created)
	failure := "previous runtime initialization failed"
	_, err = database.SetDynamicResourcePoolState(
		context.Background(), cfg.PoolName, db.DynamicResourcePoolFailed, &failure,
	)
	require.NoError(t, err)

	manager.startDynamicPoolWorker(masterCtx)
	retried, err := manager.RetryDynamicResourcePool(context.Background(), cfg.PoolName)
	require.NoError(t, err)
	require.Equal(t, db.DynamicResourcePoolPending, retried.State)
	require.Eventually(t, func() bool {
		stored, readErr := database.DynamicResourcePoolByName(context.Background(), cfg.PoolName)
		return readErr == nil && stored.State == db.DynamicResourcePoolReady &&
			manager.IsDynamicResourcePoolReady(cfg.PoolName)
	}, 10*time.Second, 20*time.Millisecond)

	cancelMaster()
	select {
	case <-manager.dynamicPoolDone:
	case <-time.After(2 * time.Second):
		t.Fatal("dynamic pool worker did not exit after master context cancellation")
	}
}

func TestDynamicPoolStartupRejectsUnsupportedVersion(t *testing.T) {
	database, cleanup := db.MustResolveNewPostgresDatabase(t)
	defer cleanup()
	db.MustMigrateTestPostgres(t, database, "file://../../../static/migrations", "up")

	_, created, err := database.CreateDynamicResourcePool(context.Background(), db.DynamicResourcePool{
		ClusterName:    "agent-cluster",
		PoolName:       "future-version",
		ConfigVersion:  999,
		IdempotencyKey: "future-operation",
		Config:         []byte(`{"pool_name":"future-version"}`),
		ConfigHash:     "future-hash",
	})
	require.NoError(t, err)
	require.True(t, created)
	err = ValidatePersistedDynamicPoolConfigs(
		context.Background(), database, []*config.ResourceManagerWithPoolsConfig{{
			ResourceManager: &config.ResourceManagerConfig{AgentRM: &config.AgentResourceManagerConfig{
				ClusterName: "agent-cluster",
			}},
			ResourcePools: []config.ResourcePoolConfig{{PoolName: "default"}},
		}},
	)
	require.ErrorContains(t, err, "unsupported config version 999")

	_, err = database.MarkDynamicResourcePoolReady(context.Background(), "future-version", nil)
	require.NoError(t, err)
	_, err = db.Bun().NewRaw(`
UPDATE dynamic_resource_pools
SET config_version = 1, spec = '{"pool_name":"future-version"}', spec_version = 2,
    spec_hash = 'future-spec-hash'
WHERE pool_name = 'future-version'`).Exec(context.Background())
	require.NoError(t, err)
	err = ValidatePersistedDynamicPoolConfigs(
		context.Background(), database, []*config.ResourceManagerWithPoolsConfig{{
			ResourceManager: &config.ResourceManagerConfig{AgentRM: &config.AgentResourceManagerConfig{
				ClusterName: "agent-cluster",
			}},
			ResourcePools: []config.ResourcePoolConfig{{PoolName: "default"}},
		}},
	)
	require.ErrorContains(t, err, "unsupported spec version 2")
}

func TestDynamicPoolStartupRejectsStaticCollision(t *testing.T) {
	database, cleanup := db.MustResolveNewPostgresDatabase(t)
	defer cleanup()
	db.MustMigrateTestPostgres(t, database, "file://../../../static/migrations", "up")

	rmForNormalization := testDynamicPoolRM()
	cfg, err := rmForNormalization.NormalizeDynamicResourcePoolConfig(
		config.ResourcePoolConfig{PoolName: "collision", MaxAuxContainersPerAgent: 100},
		*model.DefaultTaskContainerDefaults(),
	)
	require.NoError(t, err)
	raw, hash, err := marshalDynamicResourcePoolConfig(cfg)
	require.NoError(t, err)
	_, created, err := database.CreateDynamicResourcePool(context.Background(), db.DynamicResourcePool{
		ClusterName:    "agent-cluster",
		PoolName:       cfg.PoolName,
		ConfigVersion:  dynamicResourcePoolConfigVersion,
		IdempotencyKey: "collision-operation",
		Config:         raw,
		ConfigHash:     hash,
	})
	require.NoError(t, err)
	require.True(t, created)
	err = ValidatePersistedDynamicPoolConfigs(
		context.Background(), database, []*config.ResourceManagerWithPoolsConfig{{
			ResourceManager: &config.ResourceManagerConfig{AgentRM: &config.AgentResourceManagerConfig{
				ClusterName: "agent-cluster",
			}},
			ResourcePools: []config.ResourcePoolConfig{{PoolName: cfg.PoolName}},
		}},
	)
	require.ErrorContains(t, err, "conflicts with static pool")
	require.ErrorContains(t, err, "it was saved without a spec; if the pool was adopted, its "+
		"master.yaml entry must equal the saved spec or be removed")
}

func poolSummaryDescription(t *testing.T, manager *ResourceManager, poolName string) string {
	pools, err := manager.GetResourcePools()
	require.NoError(t, err)
	for _, pool := range pools.ResourcePools {
		if pool.Name == poolName {
			return pool.Description
		}
	}
	require.Failf(t, "resource pool is not listed", "%q", poolName)
	return ""
}

func TestDynamicPoolUpdateAppliesAtRestart(t *testing.T) {
	database, cleanup := db.MustResolveNewPostgresDatabase(t)
	defer cleanup()
	db.MustMigrateTestPostgres(t, database, "file://../../../static/migrations", "up")
	ctx := context.Background()

	masterDefaults := *model.DefaultTaskContainerDefaults()
	masterDefaults.ForcePullImage = true
	insertLegacyDynamicPool(t, database, "legacy", masterDefaults)
	first, err := New(
		ctx, database, echo.New(), testDynamicPoolRMConfig(42), nil, nil, &masterDefaults,
	)
	require.NoError(t, err)
	defer first.stop()
	created, _, err := first.CreateDynamicResourcePool(ctx, "updated-operation",
		json.RawMessage(`{"pool_name":"updated","description":"before"}`), masterDefaults)
	require.NoError(t, err)
	waitForDynamicPoolReady(t, database, first, "updated")
	runtimeBefore, ok := first.registry.readyPool("updated")
	require.True(t, ok)

	updated, err := first.UpdateDynamicResourcePool(ctx, "updated", &created.Revision,
		json.RawMessage(`{"pool_name":"updated","description":"after","agent_reconnect_wait":"10m"}`),
		masterDefaults)
	require.NoError(t, err)
	require.EqualValues(t, 2, updated.Revision)
	require.Equal(t, db.DynamicResourcePoolReady, updated.State)
	require.JSONEq(t, `{"agent_reconnect_wait":"10m","description":"after","pool_name":"updated"}`,
		string(*updated.Spec))
	require.Equal(t, created.IdempotencyKey, updated.IdempotencyKey)
	// The returned config is the new effective snapshot, in the shape that a master without spec
	// support runs.
	snapshotRecord := updated
	snapshotRecord.Spec = nil
	snapshot, _, err := decodeStoredDynamicResourcePool(snapshotRecord)
	require.NoError(t, err)
	require.Equal(t, "after", snapshot.Description)
	require.Equal(t, model.Duration(10*time.Minute), snapshot.AgentReconnectWait)
	require.True(t, snapshot.TaskContainerDefaults.ForcePullImage)
	require.Equal(t, 42, *snapshot.Scheduler.Priority.DefaultPriority)

	// The running master keeps the runtime pool it has until it restarts.
	runtimeAfter, ok := first.registry.readyPool("updated")
	require.True(t, ok)
	require.Same(t, runtimeBefore, runtimeAfter)
	require.Equal(t, "before", poolSummaryDescription(t, first, "updated"))
	activeRevision, active := first.ActiveDynamicResourcePoolRevision("updated")
	require.True(t, active)
	require.EqualValues(t, 1, activeRevision)

	// A record saved without a spec gains one and inherits master defaults from the next restart.
	converted, err := first.UpdateDynamicResourcePool(ctx, "legacy", nil,
		json.RawMessage(`{"pool_name":"legacy","max_aux_containers_per_agent":100}`), masterDefaults)
	require.NoError(t, err)
	require.EqualValues(t, 2, converted.Revision)
	require.Equal(t, db.DynamicResourcePoolReady, converted.State)
	require.NotNil(t, converted.Spec)
	require.Equal(t, dynamicResourcePoolSpecVersion, *converted.SpecVersion)
	changedMasterDefaults := *model.DefaultTaskContainerDefaults()
	changedMasterDefaults.ShmSizeBytes = 16 << 30
	frozenDefaults, err := first.TaskContainerDefaults(
		rm.ResourcePoolName("legacy"), changedMasterDefaults,
	)
	require.NoError(t, err)
	require.True(t, frozenDefaults.ForcePullImage)
	legacyRevision, active := first.ActiveDynamicResourcePoolRevision("legacy")
	require.True(t, active)
	require.EqualValues(t, 1, legacyRevision)

	first.stop()
	restarted, err := New(
		ctx, database, echo.New(), testDynamicPoolRMConfig(7), nil, nil, &changedMasterDefaults,
	)
	require.NoError(t, err)
	defer restarted.stop()
	require.Equal(t, "after", poolSummaryDescription(t, restarted, "updated"))
	running, ok := restarted.registry.readyConfig("updated")
	require.True(t, ok)
	require.Equal(t, model.Duration(10*time.Minute), running.AgentReconnectWait)
	activeRevision, active = restarted.ActiveDynamicResourcePoolRevision("updated")
	require.True(t, active)
	require.EqualValues(t, 2, activeRevision)

	inherited, err := restarted.TaskContainerDefaults(
		rm.ResourcePoolName("legacy"), changedMasterDefaults,
	)
	require.NoError(t, err)
	require.Equal(t, changedMasterDefaults, inherited)
	scheduler, ok := restarted.ResourcePoolSchedulerConfig("legacy")
	require.True(t, ok)
	require.Equal(t, 7, *scheduler.Priority.DefaultPriority)
	legacyRevision, active = restarted.ActiveDynamicResourcePoolRevision("legacy")
	require.True(t, active)
	require.EqualValues(t, 2, legacyRevision)
	// Startup rewrote the snapshot of the converted pool for the changed master defaults.
	refreshed, err := database.DynamicResourcePoolByName(ctx, "legacy")
	require.NoError(t, err)
	require.NotEqual(t, converted.ConfigHash, refreshed.ConfigHash)
	require.EqualValues(t, 2, refreshed.Revision)
}

func TestDynamicPoolUpdateStateRules(t *testing.T) {
	database, cleanup := db.MustResolveNewPostgresDatabase(t)
	defer cleanup()
	db.MustMigrateTestPostgres(t, database, "file://../../../static/migrations", "up")
	ctx := context.Background()

	// Hooks are installed before the worker starts so that it reads them after they are written.
	originalCreate := createDynamicPoolRuntime
	t.Cleanup(func() { createDynamicPoolRuntime = originalCreate })
	var failNextRuntime atomic.Bool
	createDynamicPoolRuntime = func(
		manager *ResourceManager, cfg config.ResourcePoolConfig,
	) (*resourcePool, error) {
		if cfg.PoolName == "failed" && failNextRuntime.CompareAndSwap(true, false) {
			return nil, errors.New("injected runtime initialization failure")
		}
		return originalCreate(manager, cfg)
	}
	masterDefaults := *model.DefaultTaskContainerDefaults()
	manager, err := New(
		ctx, database, echo.New(), testDynamicPoolRMConfig(42), nil, nil, &masterDefaults,
	)
	require.NoError(t, err)
	defer manager.stop()
	manager.StopDynamicPoolWorker()
	update := func(
		poolName string, spec string, expectedRevision *int64,
	) (db.DynamicResourcePool, error) {
		return manager.UpdateDynamicResourcePool(
			ctx, poolName, expectedRevision, json.RawMessage(spec), masterDefaults,
		)
	}
	changed := `{"pool_name":"busy","description":"changed"}`

	// A Pending pool is still being initialized.
	pending := insertSpecDynamicPool(t, manager, "busy-operation", `{"pool_name":"busy"}`,
		masterDefaults)
	_, err = update("busy", changed, nil)
	require.ErrorIs(t, err, ErrDynamicResourcePoolInitializing)
	// The saved spec is answered without a write in any state, even for a stale revision.
	staleRevision := pending.Revision + 1
	same, err := update("busy", `{ "description": null, "pool_name": "busy" }`, &staleRevision)
	require.NoError(t, err)
	require.Equal(t, pending, same)
	stored, err := database.DynamicResourcePoolByName(ctx, "busy")
	require.NoError(t, err)
	require.Equal(t, pending, stored)

	// A durable Ready written before its runtime is published is still initializing.
	_, err = database.MarkDynamicResourcePoolReady(ctx, "busy", nil)
	require.NoError(t, err)
	_, err = update("busy", changed, nil)
	require.ErrorIs(t, err, ErrDynamicResourcePoolInitializing)

	// A missing pool and a pool of another resource manager are not found.
	_, err = update("missing", `{"pool_name":"missing"}`, nil)
	require.ErrorIs(t, err, db.ErrDynamicResourcePoolNotFound)
	other := insertSpecDynamicPool(t, manager, "other-operation", `{"pool_name":"other"}`,
		masterDefaults)
	_, err = db.Bun().NewRaw(`UPDATE dynamic_resource_pools SET cluster_name = 'other-cluster'
WHERE pool_name = ?`, other.PoolName).Exec(ctx)
	require.NoError(t, err)
	_, err = update("other", `{"pool_name":"other","description":"changed"}`, nil)
	require.ErrorIs(t, err, db.ErrDynamicResourcePoolNotFound)
	_, err = db.Bun().NewRaw(`DELETE FROM dynamic_resource_pools WHERE pool_name = ?`,
		other.PoolName).Exec(ctx)
	require.NoError(t, err)

	// Once the worker publishes the pool, an update must name the current revision if it names one.
	manager.startDynamicPoolWorker(ctx)
	waitForDynamicPoolReady(t, database, manager, "busy")
	ready, err := database.DynamicResourcePoolByName(ctx, "busy")
	require.NoError(t, err)
	_, err = update("busy", changed, &staleRevision)
	require.ErrorIs(t, err, db.ErrDynamicResourcePoolChanged)
	require.ErrorContains(t, err, "current revision is 1")
	same, err = update("busy", `{"pool_name":"busy"}`, &staleRevision)
	require.NoError(t, err)
	require.Equal(t, ready, same)
	updated, err := update("busy", changed, &ready.Revision)
	require.NoError(t, err)
	require.EqualValues(t, 2, updated.Revision)
	require.Equal(t, db.DynamicResourcePoolReady, updated.State)
	require.True(t, updated.UpdatedAt.After(ready.UpdatedAt))

	// Of two updates based on the same revision, exactly one is saved.
	var group sync.WaitGroup
	start := make(chan struct{})
	results := make([]error, 2)
	for i := range results {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, results[i] = update(
				"busy", fmt.Sprintf(`{"pool_name":"busy","description":"writer %d"}`, i),
				&updated.Revision,
			)
		}()
	}
	close(start)
	group.Wait()
	saved := 0
	for _, result := range results {
		if result == nil {
			saved++
		} else {
			require.ErrorIs(t, result, db.ErrDynamicResourcePoolChanged)
		}
	}
	require.Equal(t, 1, saved)
	stored, err = database.DynamicResourcePoolByName(ctx, "busy")
	require.NoError(t, err)
	require.EqualValues(t, 3, stored.Revision)

	// An update makes a Failed pool Pending, and the worker initializes the new spec in place of
	// the registry entry that failed.
	failNextRuntime.Store(true)
	insertSpecDynamicPool(t, manager, "failed-operation", `{"pool_name":"failed"}`, masterDefaults)
	manager.wakeDynamicPoolWorker()
	var failed db.DynamicResourcePool
	require.Eventually(t, func() bool {
		failed, err = database.DynamicResourcePoolByName(ctx, "failed")
		return err == nil && failed.State == db.DynamicResourcePoolFailed
	}, 10*time.Second, 20*time.Millisecond)
	_, desired := manager.registry.desiredConfig("failed")
	require.True(t, desired)
	recovered, err := update("failed", `{"pool_name":"failed","description":"fixed"}`,
		&failed.Revision)
	require.NoError(t, err)
	require.Equal(t, db.DynamicResourcePoolPending, recovered.State)
	require.Nil(t, recovered.Error)
	require.EqualValues(t, 2, recovered.Revision)
	waitForDynamicPoolReady(t, database, manager, "failed")
	require.Equal(t, "fixed", poolSummaryDescription(t, manager, "failed"))
	activeRevision, active := manager.ActiveDynamicResourcePoolRevision("failed")
	require.True(t, active)
	require.EqualValues(t, 2, activeRevision)

	// A retry and an update of the same Failed pool race; exactly one of them succeeds.
	manager.StopDynamicPoolWorker()
	insertSpecDynamicPool(t, manager, "raced-operation", `{"pool_name":"raced"}`, masterDefaults)
	failure := "previous runtime initialization failed"
	_, err = database.SetDynamicResourcePoolState(
		ctx, "raced", db.DynamicResourcePoolFailed, &failure,
	)
	require.NoError(t, err)
	start = make(chan struct{})
	group.Add(2)
	go func() {
		defer group.Done()
		<-start
		_, results[0] = manager.RetryDynamicResourcePool(ctx, "raced")
	}()
	go func() {
		defer group.Done()
		<-start
		_, results[1] = update("raced", `{"pool_name":"raced","description":"raced"}`, nil)
	}()
	close(start)
	group.Wait()
	saved = 0
	for _, result := range results {
		if result == nil {
			saved++
		}
	}
	require.Equal(t, 1, saved, "retry: %v, update: %v", results[0], results[1])
	raced, err := database.DynamicResourcePoolByName(ctx, "raced")
	require.NoError(t, err)
	require.Equal(t, db.DynamicResourcePoolPending, raced.State)
}

// adoptedPoolEntry is a master.yaml pool entry; adopting it saves the same entry as the spec.
const adoptedPoolEntry = `{"pool_name":"adopted","description":"gpu","agent_reconnect_wait":"10m"}`

// testAdoptRMConfig returns an RM configuration whose master.yaml pools are default and the given
// entries.
func testAdoptRMConfig(t *testing.T, entries ...string) *config.ResourceManagerWithPoolsConfig {
	rmConfig := testDynamicPoolRMConfig(42)
	for _, entry := range entries {
		rmConfig.ResourcePools = append(rmConfig.ResourcePools, staticPoolConfig(t, entry))
	}
	return rmConfig
}

func TestAdoptStaticPool(t *testing.T) {
	database, cleanup := db.MustResolveNewPostgresDatabase(t)
	defer cleanup()
	db.MustMigrateTestPostgres(t, database, "file://../../../static/migrations", "up")
	ctx := context.Background()

	originalCreate := createDynamicPoolRuntime
	t.Cleanup(func() { createDynamicPoolRuntime = originalCreate })
	var initialized []string
	createDynamicPoolRuntime = func(
		manager *ResourceManager, cfg config.ResourcePoolConfig,
	) (*resourcePool, error) {
		initialized = append(initialized, cfg.PoolName)
		return originalCreate(manager, cfg)
	}
	masterDefaults := *model.DefaultTaskContainerDefaults()
	rmConfig := testAdoptRMConfig(t, adoptedPoolEntry)
	manager, err := New(ctx, database, echo.New(), rmConfig, nil, nil, &masterDefaults)
	require.NoError(t, err)
	defer manager.stop()
	// The worker is driven by hand below so that every scan is complete when it is checked.
	manager.StopDynamicPoolWorker()
	static := rmConfig.ResourcePools[1]
	runtimeBefore, ok := manager.registry.readyPool("adopted")
	require.True(t, ok)

	adopted, created, err := manager.AdoptStaticResourcePool(
		ctx, static, json.RawMessage(adoptedPoolEntry), masterDefaults,
	)
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, db.DynamicResourcePoolReady, adopted.State)
	require.Equal(t, "adopt:adopted", adopted.IdempotencyKey)
	require.Equal(t, "agent-cluster", adopted.ClusterName)
	require.EqualValues(t, 1, adopted.Revision)
	require.JSONEq(t, adoptedPoolEntry, string(*adopted.Spec))
	// Its snapshot is readable by a master without spec support.
	snapshotRecord := adopted
	snapshotRecord.Spec = nil
	snapshot, _, err := decodeStoredDynamicResourcePool(snapshotRecord)
	require.NoError(t, err)
	require.Equal(t, model.Duration(10*time.Minute), snapshot.AgentReconnectWait)

	// The running master keeps serving the pool from master.yaml, and the worker leaves it alone.
	manager.advancePendingDynamicPools(ctx)
	require.Empty(t, initialized)
	runtimeAfter, ok := manager.registry.readyPool("adopted")
	require.True(t, ok)
	require.Same(t, runtimeBefore, runtimeAfter)
	revision, published, _ := manager.registry.activeRevision("adopted")
	require.True(t, published)
	require.Zero(t, revision)
	_, active := manager.ActiveDynamicResourcePoolRevision("adopted")
	require.False(t, active)
	stored, err := database.DynamicResourcePoolByName(ctx, "adopted")
	require.NoError(t, err)
	require.Equal(t, adopted, stored)

	// A replay of the same spec returns the saved record.
	replayed, created, err := manager.AdoptStaticResourcePool(ctx, static, json.RawMessage(
		`{ "agent_reconnect_wait": "10m", "pool_name": "adopted", "description": "gpu" }`,
	), masterDefaults)
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, adopted, replayed)
	// A different spec that decodes to the same entry is not a replay.
	_, _, err = manager.AdoptStaticResourcePool(ctx, static, json.RawMessage(
		`{"pool_name":"adopted","description":"gpu","agent_reconnect_wait":"600s"}`,
	), masterDefaults)
	require.ErrorIs(t, err, db.ErrDynamicResourcePoolConflict)

	// A spec that differs from the running master.yaml entry is rejected.
	_, _, err = manager.AdoptStaticResourcePool(ctx, static, json.RawMessage(
		`{"pool_name":"adopted","description":"gpu"}`,
	), masterDefaults)
	require.ErrorIs(t, err, ErrInvalidDynamicResourcePool)
	require.ErrorContains(t, err, "including agent_reconnect_wait")

	// Creates cannot take an adopt key.
	_, _, err = manager.CreateDynamicResourcePool(
		ctx, "adopt:created", json.RawMessage(`{"pool_name":"created"}`), masterDefaults,
	)
	require.ErrorIs(t, err, ErrInvalidDynamicResourcePool)
	_, err = database.DynamicResourcePoolByName(ctx, "created")
	require.ErrorIs(t, err, db.ErrDynamicResourcePoolNotFound)
	stored, err = database.DynamicResourcePoolByName(ctx, "adopted")
	require.NoError(t, err)
	require.Equal(t, adopted, stored)
}

func TestStartupAdoptedRowWithMatchingYAMLEntry(t *testing.T) {
	database, cleanup := db.MustResolveNewPostgresDatabase(t)
	defer cleanup()
	db.MustMigrateTestPostgres(t, database, "file://../../../static/migrations", "up")
	ctx := context.Background()

	masterDefaults := *model.DefaultTaskContainerDefaults()
	rmConfig := testAdoptRMConfig(t, adoptedPoolEntry)
	first, err := New(ctx, database, echo.New(), rmConfig, nil, nil, &masterDefaults)
	require.NoError(t, err)
	defer first.stop()
	adopted, _, err := first.AdoptStaticResourcePool(
		ctx, rmConfig.ResourcePools[1], json.RawMessage(adoptedPoolEntry), masterDefaults,
	)
	require.NoError(t, err)
	first.stop()

	// While master.yaml holds the saved spec, master.yaml serves the pool and a warning asks for
	// the entry to be removed.
	logs := logrustest.NewGlobal()
	t.Cleanup(logs.Reset)
	require.NoError(t, ValidatePersistedDynamicPoolConfigs(
		ctx, database, []*config.ResourceManagerWithPoolsConfig{rmConfig},
	))
	warned := false
	for _, entry := range logs.AllEntries() {
		if entry.Level == logrus.WarnLevel && entry.Message == `resource pool "adopted" is saved `+
			`as a dynamic pool and still defined in master.yaml; remove it from master.yaml` {
			warned = true
		}
	}
	require.True(t, warned, "a pool that master.yaml still defines must be reported")

	// The startup refresh would rewrite the snapshot of a registered spec row for these defaults.
	changedMasterDefaults := masterDefaults
	changedMasterDefaults.ShmSizeBytes = 16 << 30
	restarted, err := New(ctx, database, echo.New(), rmConfig, nil, nil, &changedMasterDefaults)
	require.NoError(t, err)
	defer restarted.stop()
	revision, published, _ := restarted.registry.activeRevision("adopted")
	require.True(t, published)
	require.Zero(t, revision, "the pool must be served from master.yaml")
	stored, err := database.DynamicResourcePoolByName(ctx, "adopted")
	require.NoError(t, err)
	require.Equal(t, adopted, stored, "the saved record must not be refreshed or marked")
	restarted.stop()

	// An edited master.yaml entry no longer equals the saved spec, so startup fails closed.
	edited := testAdoptRMConfig(t,
		`{"pool_name":"adopted","description":"edited","agent_reconnect_wait":"10m"}`)
	err = ValidatePersistedDynamicPoolConfigs(
		ctx, database, []*config.ResourceManagerWithPoolsConfig{edited},
	)
	require.ErrorContains(t, err, `dynamic resource pool "adopted" conflicts with static pool`)
	require.ErrorContains(t, err, "its saved spec differs in description; if the pool was "+
		"adopted, its master.yaml entry must equal the saved spec or be removed")
	_, err = New(ctx, database, echo.New(), edited, nil, nil, &masterDefaults)
	require.ErrorContains(t, err, "must equal the saved spec or be removed")
}

func TestStartupRestoresAgentStateForAdoptedPoolRemovedFromYAML(t *testing.T) {
	database, cleanup := db.MustResolveNewPostgresDatabase(t)
	defer cleanup()
	db.MustMigrateTestPostgres(t, database, "file://../../../static/migrations", "up")
	ctx := context.Background()

	masterDefaults := *model.DefaultTaskContainerDefaults()
	const removedPoolEntry = `{"pool_name":"removed","agent_reconnect_wait":"10m"}`
	rmConfig := testAdoptRMConfig(t, adoptedPoolEntry, removedPoolEntry)
	first, err := New(ctx, database, echo.New(), rmConfig, nil, nil, &masterDefaults)
	require.NoError(t, err)
	defer first.stop()
	_, _, err = first.AdoptStaticResourcePool(
		ctx, rmConfig.ResourcePools[1], json.RawMessage(adoptedPoolEntry), masterDefaults,
	)
	require.NoError(t, err)
	first.stop()

	// An agent of each pool was connected when the master stopped. The agent of the adopted pool
	// runs a container of an allocation in that pool.
	taskID := model.TaskID(uuid.NewString())
	require.NoError(t, db.AddTask(ctx, &model.Task{
		TaskID: taskID, TaskType: model.TaskTypeCommand, StartTime: time.Now(),
		LogVersion: model.CurrentTaskLogVersion,
	}))
	allocationID := model.AllocationID(uuid.NewString())
	require.NoError(t, db.AddAllocation(ctx, &model.Allocation{
		AllocationID: allocationID, TaskID: taskID, Slots: 1, ResourcePool: "adopted",
		StartTime: ptrs.Ptr(time.Now()), State: ptrs.Ptr(model.AllocationStateRunning),
	}))
	containerID := cproto.ID(uuid.NewString())
	gpu := device.Device{ID: 0, Brand: "nvidia", UUID: uuid.NewString(), Type: device.CUDA}
	_, err = db.Bun().NewInsert().Model(&taskmodel.ResourcesWithState{
		ResourceID: sproto.ResourcesID(containerID), AllocationID: allocationID,
	}).Exec(ctx)
	require.NoError(t, err)
	_, err = db.Bun().NewInsert().Model(&containerSnapshot{
		ResourceID: sproto.ResourcesID(containerID), AgentID: "adopted-agent", ID: containerID,
		State: cproto.Running, Devices: []device.Device{gpu},
	}).Exec(ctx)
	require.NoError(t, err)
	for _, snapshot := range []agentSnapshot{
		{
			AgentID: "adopted-agent", UUID: uuid.NewString(), ResourcePoolName: "adopted",
			UserEnabled: true, Containers: []cproto.ID{containerID},
			Slots: []slotData{{Device: gpu, UserEnabled: true, ContainerID: &containerID}},
		},
		{
			AgentID: "removed-agent", UUID: uuid.NewString(), ResourcePoolName: "removed",
			UserEnabled: true,
			Slots: []slotData{{Device: device.Device{
				ID: 0, Brand: "nvidia", UUID: uuid.NewString(), Type: device.CUDA,
			}, UserEnabled: true}},
		},
	} {
		_, err = db.Bun().NewInsert().Model(&snapshot).Exec(ctx)
		require.NoError(t, err)
	}

	// Both entries are removed from master.yaml. Only the adopted pool is saved.
	restarted, err := New(
		ctx, database, echo.New(), testDynamicPoolRMConfig(42), nil, nil, &masterDefaults,
	)
	require.NoError(t, err)
	defer restarted.stop()
	revision, active := restarted.ActiveDynamicResourcePoolRevision("adopted")
	require.True(t, active)
	require.EqualValues(t, 1, revision)

	agents := restarted.agentService.list("adopted")
	require.Contains(t, agents, aproto.ID("adopted-agent"))
	restored := agents["adopted-agent"]
	require.Contains(t, restored.containerState, containerID)
	require.Equal(t, &containerID, restored.slotStates[gpu.ID].containerID)
	exists, err := db.Bun().NewSelect().Model((*agentSnapshot)(nil)).
		Where("agent_id = ?", "adopted-agent").Exists(ctx)
	require.NoError(t, err)
	require.True(t, exists, "the adopted pool's agent state must be kept")
	exists, err = db.Bun().NewSelect().Model((*containerSnapshot)(nil)).
		Where("container_id = ?", containerID).Exists(ctx)
	require.NoError(t, err)
	require.True(t, exists, "the adopted pool's container state must be kept")

	// The agent of the pool that was removed without being adopted is dropped with its state.
	_, ok := restarted.agentService.get("removed-agent")
	require.False(t, ok)
	exists, err = db.Bun().NewSelect().Model((*agentSnapshot)(nil)).
		Where("agent_id = ?", "removed-agent").Exists(ctx)
	require.NoError(t, err)
	require.False(t, exists)
}
