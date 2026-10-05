//go:build integration

package agentrm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/rm"
	"github.com/determined-ai/determined/master/pkg/model"
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
}
