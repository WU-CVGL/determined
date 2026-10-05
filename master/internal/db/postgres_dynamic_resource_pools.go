package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// DynamicResourcePoolState is the durable initialization state of a dynamic resource pool.
type DynamicResourcePoolState string

const (
	// DynamicResourcePoolPending means the desired config is durable but runtime initialization has
	// not completed.
	DynamicResourcePoolPending DynamicResourcePoolState = "Pending"
	// DynamicResourcePoolReady means the runtime resource pool is available for admission.
	DynamicResourcePoolReady DynamicResourcePoolState = "Ready"
	// DynamicResourcePoolFailed means runtime initialization failed and requires an explicit retry.
	DynamicResourcePoolFailed DynamicResourcePoolState = "Failed"
)

var (
	// ErrDynamicResourcePoolConflict indicates a pool name or idempotency key conflict.
	ErrDynamicResourcePoolConflict = errors.New("dynamic resource pool conflict")
	// ErrDynamicResourcePoolNotFound indicates that a dynamic resource pool does not exist.
	ErrDynamicResourcePoolNotFound = errors.New("dynamic resource pool not found")
	// ErrDynamicResourcePoolNotFailed indicates that a retry targeted a non-failed operation.
	ErrDynamicResourcePoolNotFailed = errors.New("dynamic resource pool is not failed")
	// ErrDynamicResourcePoolChanged indicates that a conditional write found the record changed
	// since it was read.
	ErrDynamicResourcePoolChanged = errors.New("dynamic resource pool changed")
)

// Kept as a package variable so integration tests can fail the read after a committed insert.
var readCreatedDynamicResourcePool = func(
	database *PgDB, ctx context.Context, poolName string,
) (DynamicResourcePool, error) {
	return database.DynamicResourcePoolByName(ctx, poolName)
}

// DynamicResourcePool is the durable desired configuration and operation status for a dynamic
// resource pool. Config is the normalized, effective ResourcePoolConfig JSON. Spec, when set, is
// the administrator's sparse ResourcePoolConfig and the source of truth; Config is then a snapshot
// of its effective values that earlier masters can still run.
type DynamicResourcePool struct {
	ClusterName    string                   `db:"cluster_name" json:"cluster_name"`
	PoolName       string                   `db:"pool_name" json:"pool_name"`
	ConfigVersion  int                      `db:"config_version" json:"config_version"`
	IdempotencyKey string                   `db:"idempotency_key" json:"-"`
	Config         json.RawMessage          `db:"config" json:"config"`
	ConfigHash     string                   `db:"config_hash" json:"-"`
	Spec           *json.RawMessage         `db:"spec" json:"spec,omitempty"`
	SpecVersion    *int                     `db:"spec_version" json:"spec_version,omitempty"`
	SpecHash       *string                  `db:"spec_hash" json:"-"`
	Revision       int64                    `db:"revision" json:"revision"`
	State          DynamicResourcePoolState `db:"state" json:"state"`
	Error          *string                  `db:"error" json:"error,omitempty"`
	CreatedAt      time.Time                `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time                `db:"updated_at" json:"updated_at"`
}

// DynamicResourcePoolSnapshot is an effective config written alongside a spec.
type DynamicResourcePoolSnapshot struct {
	Config     json.RawMessage
	ConfigHash string
}

const dynamicResourcePoolColumns = `cluster_name, pool_name, config_version, idempotency_key, config,
       config_hash, spec, spec_version, spec_hash, revision, state, error, created_at, updated_at`

// CreateDynamicResourcePool durably inserts a Pending desired config. An exact replay using the
// same resource-manager-scoped idempotency key returns the original operation. Pool names are
// globally unique because regular multi-RM admission routes by pool name alone.
func (db *PgDB) CreateDynamicResourcePool(
	ctx context.Context,
	record DynamicResourcePool,
) (stored DynamicResourcePool, created bool, err error) {
	var spec interface{}
	if record.Spec != nil {
		spec = []byte(*record.Spec)
	}
	result, err := db.sql.ExecContext(ctx, `
INSERT INTO dynamic_resource_pools
    (cluster_name, pool_name, config_version, idempotency_key, config, config_hash,
     spec, spec_version, spec_hash, state)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT DO NOTHING`,
		record.ClusterName,
		record.PoolName,
		record.ConfigVersion,
		record.IdempotencyKey,
		[]byte(record.Config),
		record.ConfigHash,
		spec,
		record.SpecVersion,
		record.SpecHash,
		DynamicResourcePoolPending,
	)
	if err != nil {
		return DynamicResourcePool{}, false, fmt.Errorf("inserting dynamic resource pool: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return DynamicResourcePool{}, false, fmt.Errorf("checking dynamic resource pool insert: %w", err)
	}
	if rows == 1 {
		stored, err = readCreatedDynamicResourcePool(db, ctx, record.PoolName)
		return stored, true, err
	}

	stored, err = db.dynamicResourcePoolByIdempotencyKey(
		ctx, record.ClusterName, record.IdempotencyKey,
	)
	if err == nil {
		// The spec hash is computed only when a spec is written. A row written without a spec, or
		// whose spec was replaced since, is not a replay of this request.
		if stored.PoolName == record.PoolName &&
			stored.ConfigVersion == record.ConfigVersion &&
			stored.SpecHash != nil && record.SpecHash != nil &&
			*stored.SpecHash == *record.SpecHash {
			return stored, false, nil
		}
		return DynamicResourcePool{}, false, fmt.Errorf(
			"%w: idempotency key already names a different desired config",
			ErrDynamicResourcePoolConflict,
		)
	}
	if !errors.Is(err, ErrDynamicResourcePoolNotFound) {
		return DynamicResourcePool{}, false, err
	}

	if _, err = db.DynamicResourcePoolByName(ctx, record.PoolName); err == nil {
		return DynamicResourcePool{}, false, fmt.Errorf(
			"%w: resource pool name %q already exists", ErrDynamicResourcePoolConflict, record.PoolName,
		)
	} else if !errors.Is(err, ErrDynamicResourcePoolNotFound) {
		return DynamicResourcePool{}, false, err
	}
	return DynamicResourcePool{}, false, fmt.Errorf(
		"%w: desired config conflicted with an existing operation", ErrDynamicResourcePoolConflict,
	)
}

// DynamicResourcePoolByName returns a dynamic resource pool by its globally unique name.
func (db *PgDB) DynamicResourcePoolByName(
	ctx context.Context, poolName string,
) (DynamicResourcePool, error) {
	var record DynamicResourcePool
	err := db.sql.GetContext(ctx, &record, `
SELECT `+dynamicResourcePoolColumns+`
FROM dynamic_resource_pools
WHERE pool_name = $1`, poolName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DynamicResourcePool{}, ErrDynamicResourcePoolNotFound
		}
		return DynamicResourcePool{}, fmt.Errorf("reading dynamic resource pool: %w", err)
	}
	return record, nil
}

func (db *PgDB) dynamicResourcePoolByIdempotencyKey(
	ctx context.Context, clusterName, idempotencyKey string,
) (DynamicResourcePool, error) {
	var record DynamicResourcePool
	err := db.sql.GetContext(ctx, &record, `
SELECT `+dynamicResourcePoolColumns+`
FROM dynamic_resource_pools
WHERE cluster_name = $1 AND idempotency_key = $2`, clusterName, idempotencyKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DynamicResourcePool{}, ErrDynamicResourcePoolNotFound
		}
		return DynamicResourcePool{}, fmt.Errorf("reading dynamic resource pool by idempotency key: %w", err)
	}
	return record, nil
}

// ListDynamicResourcePools lists all dynamic resource pools, optionally scoped to a resource
// manager cluster name, in creation order.
func (db *PgDB) ListDynamicResourcePools(
	ctx context.Context, clusterName string,
) ([]DynamicResourcePool, error) {
	records := []DynamicResourcePool{}
	query := `
SELECT ` + dynamicResourcePoolColumns + `
FROM dynamic_resource_pools`
	args := []interface{}{}
	if clusterName != "" {
		query += " WHERE cluster_name = $1"
		args = append(args, clusterName)
	}
	query += " ORDER BY created_at, pool_name"
	if err := db.sql.SelectContext(ctx, &records, query, args...); err != nil {
		return nil, fmt.Errorf("listing dynamic resource pools: %w", err)
	}
	return records, nil
}

// SetDynamicResourcePoolState records an initialization result.
func (db *PgDB) SetDynamicResourcePoolState(
	ctx context.Context,
	poolName string,
	state DynamicResourcePoolState,
	errText *string,
) (DynamicResourcePool, error) {
	var record DynamicResourcePool
	err := db.sql.GetContext(ctx, &record, `
UPDATE dynamic_resource_pools
SET state = $2, error = $3, updated_at = NOW()
WHERE pool_name = $1
RETURNING `+dynamicResourcePoolColumns, poolName, state, errText)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DynamicResourcePool{}, ErrDynamicResourcePoolNotFound
		}
		return DynamicResourcePool{}, fmt.Errorf("updating dynamic resource pool state: %w", err)
	}
	return record, nil
}

// MarkDynamicResourcePoolReady records a successful startup initialization. A non-nil snapshot
// replaces the stored effective config in the same write.
func (db *PgDB) MarkDynamicResourcePoolReady(
	ctx context.Context,
	poolName string,
	snapshot *DynamicResourcePoolSnapshot,
) (DynamicResourcePool, error) {
	var config interface{}
	var configHash *string
	if snapshot != nil {
		config = []byte(snapshot.Config)
		configHash = &snapshot.ConfigHash
	}
	var record DynamicResourcePool
	err := db.sql.GetContext(ctx, &record, `
UPDATE dynamic_resource_pools
SET state = $2, error = NULL,
    config = COALESCE($3::jsonb, config), config_hash = COALESCE($4, config_hash),
    updated_at = NOW()
WHERE pool_name = $1
RETURNING `+dynamicResourcePoolColumns, poolName, DynamicResourcePoolReady, config, configHash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DynamicResourcePool{}, ErrDynamicResourcePoolNotFound
		}
		return DynamicResourcePool{}, fmt.Errorf("marking dynamic resource pool ready: %w", err)
	}
	return record, nil
}

// UpdateDynamicResourcePoolSpec replaces a record's spec and snapshot with those of desired and
// increments its revision, but only while the record keeps the revision and state that read holds.
// A Failed record becomes Pending with its error cleared; any other state is kept.
func (db *PgDB) UpdateDynamicResourcePoolSpec(
	ctx context.Context, read DynamicResourcePool, desired DynamicResourcePool,
) (DynamicResourcePool, error) {
	if desired.Spec == nil || desired.SpecVersion == nil || desired.SpecHash == nil {
		return DynamicResourcePool{}, fmt.Errorf("updating dynamic resource pool: spec is incomplete")
	}
	var record DynamicResourcePool
	err := db.sql.GetContext(ctx, &record, `
UPDATE dynamic_resource_pools
SET spec = $5, spec_version = $6, spec_hash = $7, config_version = $8, config = $9,
    config_hash = $10, revision = revision + 1,
    state = CASE WHEN state = $11 THEN $12 ELSE state END,
    error = CASE WHEN state = $11 THEN NULL ELSE error END,
    updated_at = NOW()
WHERE cluster_name = $1 AND pool_name = $2 AND revision = $3 AND state = $4
RETURNING `+dynamicResourcePoolColumns,
		read.ClusterName, read.PoolName, read.Revision, read.State,
		[]byte(*desired.Spec), *desired.SpecVersion, *desired.SpecHash,
		desired.ConfigVersion, []byte(desired.Config), desired.ConfigHash,
		DynamicResourcePoolFailed, DynamicResourcePoolPending,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return DynamicResourcePool{}, fmt.Errorf(
			"%w concurrently; read it again and retry", ErrDynamicResourcePoolChanged,
		)
	}
	if err != nil {
		return DynamicResourcePool{}, fmt.Errorf("updating dynamic resource pool spec: %w", err)
	}
	return record, nil
}

// BeginDynamicResourcePoolRetry atomically changes a Failed operation back to Pending.
func (db *PgDB) BeginDynamicResourcePoolRetry(
	ctx context.Context, clusterName, poolName string,
) (DynamicResourcePool, error) {
	var record DynamicResourcePool
	err := db.sql.GetContext(ctx, &record, `
UPDATE dynamic_resource_pools
SET state = $3, error = NULL, updated_at = NOW()
WHERE cluster_name = $1 AND pool_name = $2 AND state = $4
RETURNING `+dynamicResourcePoolColumns,
		clusterName, poolName, DynamicResourcePoolPending, DynamicResourcePoolFailed)
	if err == nil {
		return record, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return DynamicResourcePool{}, fmt.Errorf("beginning dynamic resource pool retry: %w", err)
	}
	record, getErr := db.DynamicResourcePoolByName(ctx, poolName)
	if getErr != nil {
		return DynamicResourcePool{}, getErr
	}
	if record.ClusterName != clusterName {
		return DynamicResourcePool{}, ErrDynamicResourcePoolNotFound
	}
	return DynamicResourcePool{}, fmt.Errorf(
		"%w: current state is %s", ErrDynamicResourcePoolNotFailed, record.State,
	)
}
