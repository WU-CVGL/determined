// Package poolaccess decides who may start new work in a resource pool (Pool ACL v1).
//
// A user may start new work in pool P when any of these holds:
//   - the user passes the admin predicate (cluster CanUpdateMasterConfig; users.admin in basic
//     mode), which is checked first and reads no access table;
//   - P has no restriction record: such a pool is public;
//   - P is restricted and the user has a grant on P.
//
// Everything else is denied: a restricted pool without a grant for the user (a restricted pool
// with no grants at all is for admins only), access tables that cannot be read (Unavailable, never
// treated as "no record"), and an empty pool name (Internal). The check never picks another pool.
//
// Invariant: CanUseResourcePool is called only from the admission check sites (command, shell,
// notebook and TensorBoard launch; generic task create and unpause; experiment create, continue
// and activate; job-queue pool moves) and when a workspace default pool is set. UsablePools is
// called only by the resource pool list (GetResourcePools). Nothing at or below
// task.DefaultService.StartAllocation or rm.Allocate calls either. Continuations (experiment,
// trial, command and generic task restore; trial allocations and restarts; generic task resume
// recovery and retried resume plans) and system tasks (checkpoint GC) are exempt by design.
package poolaccess

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/cluster"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/pkg/model"
)

// ReadRestrictions reads, for the given pools, the ones that are restricted and whether userID has
// a grant on each. It is a variable only so that tests can make the read fail or count it; nothing
// else may replace it.
var ReadRestrictions = restrictionsFor

// CanUseResourcePool returns nil when user may start new work in pool, PermissionDenied when
// not, and Unavailable or Internal when access could not be decided. pool must be the final,
// resolved name.
func CanUseResourcePool(ctx context.Context, user model.User, pool string) error {
	if pool == "" {
		return status.Error(codes.Internal,
			"resource pool access checked before the pool was resolved")
	}
	admin, err := isAdmin(ctx, user)
	if err != nil {
		return err
	}
	if admin {
		return nil
	}
	restricted, err := ReadRestrictions(ctx, user.ID, []string{pool})
	if err != nil {
		return status.Errorf(codes.Unavailable,
			"could not check access to resource pool %q: %s; try again", pool, err)
	}
	if granted, ok := restricted[pool]; !ok || granted {
		return nil
	}
	log.Infof("resource pool access: refused user %q in restricted pool %q", user.Username, pool)
	return status.Errorf(codes.PermissionDenied,
		"user %q may not use resource pool %q: the pool is restricted; choose another pool or ask "+
			"an administrator for access (if resources.resource_pool was not set, %q is the "+
			"default pool for this workspace or the cluster)", user.Username, pool, pool)
}

// UsablePools returns, for each name, whether user may use it (admins: all true, no read).
// One query, no cache; an error is returned, never a partial answer.
func UsablePools(ctx context.Context, user model.User, pools []string) (map[string]bool, error) {
	admin, err := isAdmin(ctx, user)
	if err != nil {
		return nil, err
	}
	usable := make(map[string]bool, len(pools))
	if admin {
		for _, pool := range pools {
			usable[pool] = true
		}
		return usable, nil
	}
	restricted, err := ReadRestrictions(ctx, user.ID, pools)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable,
			"could not check access to resource pools: %s; try again", err)
	}
	for _, pool := range pools {
		granted, ok := restricted[pool]
		usable[pool] = !ok || granted
	}
	return usable, nil
}

// isAdmin is the admin predicate: the permission that manages access to resource pools.
func isAdmin(ctx context.Context, user model.User) (bool, error) {
	permErr, err := cluster.AuthZProvider.Get().CanUpdateMasterConfig(ctx, &user)
	if err != nil {
		return false, status.Errorf(codes.Internal,
			"checking resource pool access for user %q: %s", user.Username, err)
	}
	return permErr == nil, nil
}

// restrictionsFor returns the restricted pools among pools, each mapped to whether userID has a
// grant on it. A pool that is absent from the map is public. A query error is returned as it is,
// never as an empty map; the callers' Unavailable errors already name the operation.
func restrictionsFor(
	ctx context.Context, userID model.UserID, pools []string,
) (map[string]bool, error) {
	restricted := map[string]bool{}
	if len(pools) == 0 {
		return restricted, nil
	}
	var rows []struct {
		PoolName string `bun:"pool_name"`
		Granted  bool   `bun:"granted"`
	}
	if err := db.Bun().NewRaw(`
SELECT r.pool_name, (g.user_id IS NOT NULL) AS granted
FROM resource_pool_restrictions r
LEFT JOIN resource_pool_grants g ON g.pool_name = r.pool_name AND g.user_id = ?
WHERE r.pool_name IN (?)`, userID, bun.In(pools)).Scan(ctx, &rows); err != nil {
		return nil, err
	}
	for _, row := range rows {
		restricted[row.PoolName] = row.Granted
	}
	return restricted, nil
}

// RestrictionRecord is a restricted pool. RestrictedBy and RestrictedByUsername are nil once the
// user who restricted it is deleted.
type RestrictionRecord struct {
	PoolName             string        `bun:"pool_name"`
	RestrictedBy         *model.UserID `bun:"restricted_by"`
	RestrictedByUsername *string       `bun:"restricted_by_username"`
	RestrictedAt         time.Time     `bun:"restricted_at"`
}

// GrantRecord is a user's grant on a pool, with the user's current username, active and admin
// flags.
type GrantRecord struct {
	PoolName  string        `bun:"pool_name"`
	UserID    model.UserID  `bun:"user_id"`
	Username  string        `bun:"username"`
	Active    bool          `bun:"active"`
	Admin     bool          `bun:"admin"`
	GrantedBy *model.UserID `bun:"granted_by"`
	GrantedAt time.Time     `bun:"granted_at"`
}

// Restrict makes pool restricted. changed is false when it already was.
func Restrict(ctx context.Context, pool string, byUserID model.UserID) (changed bool, err error) {
	res, err := db.Bun().NewRaw(`
INSERT INTO resource_pool_restrictions (pool_name, restricted_by) VALUES (?, ?)
ON CONFLICT DO NOTHING`, pool, byUserID).Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("restricting resource pool %q: %w", pool, err)
	}
	return rowsChanged(res)
}

// MakePublic removes pool's restriction and keeps its grants. changed is false when the pool was
// already public.
func MakePublic(ctx context.Context, pool string) (changed bool, err error) {
	res, err := db.Bun().NewRaw(
		`DELETE FROM resource_pool_restrictions WHERE pool_name = ?`, pool).Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("making resource pool %q public: %w", pool, err)
	}
	return rowsChanged(res)
}

// Grant gives users access to pool, whether it is restricted or public. added holds the users
// that did not already have a grant.
func Grant(
	ctx context.Context, pool string, userIDs []model.UserID, byUserID model.UserID,
) (added []model.UserID, err error) {
	if len(userIDs) == 0 {
		return nil, nil
	}
	if err := db.Bun().NewRaw(`
INSERT INTO resource_pool_grants (pool_name, user_id, granted_by)
SELECT DISTINCT ?, u, ?::int FROM unnest(?::int[]) AS u
ON CONFLICT DO NOTHING
RETURNING user_id`, pool, byUserID, pgdialect.Array(intIDs(userIDs))).Scan(ctx, &added); err != nil {
		return nil, fmt.Errorf("granting access to resource pool %q: %w", pool, err)
	}
	return added, nil
}

// Revoke removes users' grants on pool. removed holds the users that had one.
func Revoke(
	ctx context.Context, pool string, userIDs []model.UserID,
) (removed []model.UserID, err error) {
	if len(userIDs) == 0 {
		return nil, nil
	}
	if err := db.Bun().NewRaw(`
DELETE FROM resource_pool_grants WHERE pool_name = ? AND user_id IN (?)
RETURNING user_id`, pool, bun.In(userIDs)).Scan(ctx, &removed); err != nil {
		return nil, fmt.Errorf("revoking access to resource pool %q: %w", pool, err)
	}
	return removed, nil
}

// List returns every restriction and every grant, ordered by pool name and then username.
func List(ctx context.Context) ([]RestrictionRecord, []GrantRecord, error) {
	restrictions := []RestrictionRecord{}
	if err := db.Bun().NewRaw(`
SELECT r.pool_name, r.restricted_by, u.username AS restricted_by_username, r.restricted_at
FROM resource_pool_restrictions r LEFT JOIN users u ON u.id = r.restricted_by
ORDER BY r.pool_name`).Scan(ctx, &restrictions); err != nil {
		return nil, nil, fmt.Errorf("listing resource pool restrictions: %w", err)
	}
	grants := []GrantRecord{}
	if err := db.Bun().NewRaw(`
SELECT g.pool_name, g.user_id, u.username, u.active, u.admin, g.granted_by, g.granted_at
FROM resource_pool_grants g JOIN users u ON u.id = g.user_id
ORDER BY g.pool_name, u.username`).Scan(ctx, &grants); err != nil {
		return nil, nil, fmt.Errorf("listing resource pool grants: %w", err)
	}
	return restrictions, grants, nil
}

func rowsChanged(res sql.Result) (bool, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func intIDs(userIDs []model.UserID) []int {
	ids := make([]int, 0, len(userIDs))
	for _, id := range userIDs {
		ids = append(ids, int(id))
	}
	return ids
}
