//go:build integration
// +build integration

package poolaccess

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/pkg/etc"
	"github.com/determined-ai/determined/master/pkg/model"
)

func TestMain(m *testing.M) {
	pgDB, _, err := db.ResolveTestPostgres()
	if err != nil {
		log.Panicln(err)
	}
	if err := db.MigrateTestPostgres(pgDB, "file://../../static/migrations", "up"); err != nil {
		log.Panicln(err)
	}
	if err := etc.SetRootPath("../../static/srv"); err != nil {
		log.Panicln(err)
	}
	config.GetMasterConfig().Security.AuthZ = config.AuthZConfig{Type: "basic"}
	os.Exit(m.Run())
}

// testPool returns a pool name of this test only and removes its records when the test ends.
func testPool(t *testing.T) string {
	pool := "acl-test-" + uuid.NewString()
	t.Cleanup(func() {
		ctx := context.Background()
		_, err := db.Bun().NewRaw(
			`DELETE FROM resource_pool_restrictions WHERE pool_name = ?`, pool).Exec(ctx)
		require.NoError(t, err)
		_, err = db.Bun().NewRaw(`DELETE FROM resource_pool_grants WHERE pool_name = ?`, pool).Exec(ctx)
		require.NoError(t, err)
	})
	return pool
}

func TestPoolAccessStore(t *testing.T) {
	ctx := context.Background()
	u1 := db.RequireMockUser(t, db.SingleDB())
	u2 := db.RequireMockUser(t, db.SingleDB())
	admin := db.RequireMockUser(t, db.SingleDB())

	// The empty name is refused by both tables, and a grant needs an existing user.
	_, err := Restrict(ctx, "", admin.ID)
	require.ErrorContains(t, err, "violates check constraint")
	_, err = Grant(ctx, "", []model.UserID{u1.ID}, admin.ID)
	require.ErrorContains(t, err, "violates check constraint")
	_, err = Grant(ctx, testPool(t), []model.UserID{-1}, admin.ID)
	require.ErrorContains(t, err, "violates foreign key constraint")

	pool := testPool(t)
	other := testPool(t)

	// A grant on a public pool is stored and leaves the pool public.
	added, err := Grant(ctx, pool, []model.UserID{u1.ID, u1.ID}, admin.ID)
	require.NoError(t, err)
	require.Equal(t, []model.UserID{u1.ID}, added)
	added, err = Grant(ctx, pool, []model.UserID{u1.ID}, admin.ID)
	require.NoError(t, err)
	require.Empty(t, added)
	restricted, err := restrictionsFor(ctx, u1.ID, []string{pool, other})
	require.NoError(t, err)
	require.Empty(t, restricted)

	changed, err := Restrict(ctx, pool, admin.ID)
	require.NoError(t, err)
	require.True(t, changed)
	changed, err = Restrict(ctx, pool, admin.ID)
	require.NoError(t, err)
	require.False(t, changed)

	// Only restricted pools are returned, granted only for the queried user.
	restricted, err = restrictionsFor(ctx, u1.ID, []string{pool, other})
	require.NoError(t, err)
	require.Equal(t, map[string]bool{pool: true}, restricted)
	restricted, err = restrictionsFor(ctx, u2.ID, []string{pool, other})
	require.NoError(t, err)
	require.Equal(t, map[string]bool{pool: false}, restricted)
	restricted, err = restrictionsFor(ctx, u2.ID, nil)
	require.NoError(t, err)
	require.Empty(t, restricted)

	// Listing every pool includes the pool's records, and listing the pool returns only those.
	restrictions, grants, err := List(ctx, "")
	require.NoError(t, err)
	var found []RestrictionRecord
	for _, r := range restrictions {
		if r.PoolName == pool {
			found = append(found, r)
		}
	}
	var foundGrants []GrantRecord
	for _, g := range grants {
		if g.PoolName == pool {
			foundGrants = append(foundGrants, g)
		}
	}
	restrictions, grants, err = List(ctx, pool)
	require.NoError(t, err)
	require.Equal(t, found, restrictions)
	require.Equal(t, foundGrants, grants)
	require.Len(t, found, 1)
	require.Equal(t, admin.ID, *found[0].RestrictedBy)
	require.Equal(t, admin.Username, *found[0].RestrictedByUsername)
	require.False(t, found[0].RestrictedAt.IsZero())
	require.Len(t, foundGrants, 1)
	require.Equal(t, u1.ID, foundGrants[0].UserID)
	require.Equal(t, u1.Username, foundGrants[0].Username)
	require.True(t, foundGrants[0].Active)
	require.False(t, foundGrants[0].Admin)
	require.Equal(t, admin.ID, *foundGrants[0].GrantedBy)
	restrictions, grants, err = List(ctx, other)
	require.NoError(t, err)
	require.Empty(t, restrictions)
	require.Empty(t, grants)

	// Making the pool public keeps its grants.
	changed, err = MakePublic(ctx, pool)
	require.NoError(t, err)
	require.True(t, changed)
	changed, err = MakePublic(ctx, pool)
	require.NoError(t, err)
	require.False(t, changed)
	restricted, err = restrictionsFor(ctx, u1.ID, []string{pool})
	require.NoError(t, err)
	require.Empty(t, restricted)
	_, err = Restrict(ctx, pool, admin.ID)
	require.NoError(t, err)
	restricted, err = restrictionsFor(ctx, u1.ID, []string{pool})
	require.NoError(t, err)
	require.Equal(t, map[string]bool{pool: true}, restricted)

	// Revoke reports only the grants it removed.
	added, err = Grant(ctx, pool, []model.UserID{u1.ID, u2.ID}, admin.ID)
	require.NoError(t, err)
	require.Equal(t, []model.UserID{u2.ID}, added)
	removed, err := Revoke(ctx, pool, []model.UserID{u1.ID, admin.ID})
	require.NoError(t, err)
	require.Equal(t, []model.UserID{u1.ID}, removed)
	removed, err = Revoke(ctx, pool, []model.UserID{u1.ID})
	require.NoError(t, err)
	require.Empty(t, removed)
	removed, err = Revoke(ctx, pool, nil)
	require.NoError(t, err)
	require.Empty(t, removed)
	restricted, err = restrictionsFor(ctx, u1.ID, []string{pool})
	require.NoError(t, err)
	require.Equal(t, map[string]bool{pool: false}, restricted)
	restricted, err = restrictionsFor(ctx, u2.ID, []string{pool})
	require.NoError(t, err)
	require.Equal(t, map[string]bool{pool: true}, restricted)
}

func TestCanUseResourcePool(t *testing.T) {
	ctx := context.Background()
	u1 := db.RequireMockUser(t, db.SingleDB())
	u2 := db.RequireMockUser(t, db.SingleDB())
	admin := db.RequireMockUser(t, db.SingleDB())
	admin.Admin = true

	public := testPool(t)
	adminsOnly := testPool(t)
	granted := testPool(t)
	dormantGrant := testPool(t)
	madePublic := testPool(t)
	for _, pool := range []string{adminsOnly, granted, madePublic} {
		_, err := Restrict(ctx, pool, admin.ID)
		require.NoError(t, err)
	}
	for _, pool := range []string{granted, dormantGrant} {
		_, err := Grant(ctx, pool, []model.UserID{u1.ID}, admin.ID)
		require.NoError(t, err)
	}
	_, err := MakePublic(ctx, madePublic)
	require.NoError(t, err)

	for _, tc := range []struct {
		name string
		user model.User
		pool string
		code codes.Code
	}{
		{"no record is public", u1, public, codes.OK},
		{"restricted with no grants", u1, adminsOnly, codes.PermissionDenied},
		{"restricted with a grant", u1, granted, codes.OK},
		{"restricted with another user's grant", u2, granted, codes.PermissionDenied},
		{"a grant without a restriction", u1, dormantGrant, codes.OK},
		{"restricted, then made public", u2, madePublic, codes.OK},
		{"an empty pool name", u1, "", codes.Internal},
		{"an empty pool name, admin", admin, "", codes.Internal},
		{"admin on a restricted pool with no grants", admin, adminsOnly, codes.OK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CanUseResourcePool(ctx, tc.user, tc.pool)
			require.Equal(t, tc.code, status.Code(err), "%v", err)
			if tc.code == codes.PermissionDenied {
				require.Equal(t, fmt.Sprintf("user %q may not use resource pool %q: the pool is "+
					"restricted; choose another pool or ask an administrator for access (if "+
					"resources.resource_pool was not set, %q is the default pool for this "+
					"workspace or the cluster)", tc.user.Username, tc.pool, tc.pool),
					status.Convert(err).Message())
			}
		})
	}

	t.Run("a failed read is never public", func(t *testing.T) {
		reads := 0
		ReadRestrictions = func(context.Context, model.UserID, []string) (map[string]bool, error) {
			reads++
			return nil, errors.New("connection refused")
		}
		t.Cleanup(func() { ReadRestrictions = restrictionsFor })

		err := CanUseResourcePool(ctx, u1, public)
		require.Equal(t, codes.Unavailable, status.Code(err))
		require.Equal(t, fmt.Sprintf(
			"could not check access to resource pool %q: connection refused; try again", public),
			status.Convert(err).Message())
		require.Equal(t, 1, reads)

		require.NoError(t, CanUseResourcePool(ctx, admin, public))
		require.NoError(t, CanUseResourcePool(ctx, admin, adminsOnly))
		require.Equal(t, 1, reads, "the admin predicate reads no access table")

		_, err = UsablePools(ctx, u1, []string{public})
		require.Equal(t, codes.Unavailable, status.Code(err))
		usable, err := UsablePools(ctx, admin, []string{public, adminsOnly})
		require.NoError(t, err)
		require.Equal(t, map[string]bool{public: true, adminsOnly: true}, usable)
		require.Equal(t, 2, reads)
	})

	t.Run("a failed query names the pool and the database error", func(t *testing.T) {
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		err := CanUseResourcePool(canceled, u1, public)
		require.Equal(t, codes.Unavailable, status.Code(err))
		require.Equal(t, fmt.Sprintf(
			"could not check access to resource pool %q: %s; try again", public, context.Canceled),
			status.Convert(err).Message())
	})

	t.Run("usable pools", func(t *testing.T) {
		pools := []string{public, adminsOnly, granted, dormantGrant, madePublic}
		usable, err := UsablePools(ctx, u1, pools)
		require.NoError(t, err)
		require.Equal(t, map[string]bool{
			public: true, adminsOnly: false, granted: true, dormantGrant: true, madePublic: true,
		}, usable)
		usable, err = UsablePools(ctx, u2, pools)
		require.NoError(t, err)
		require.Equal(t, map[string]bool{
			public: true, adminsOnly: false, granted: false, dormantGrant: true, madePublic: true,
		}, usable)
		usable, err = UsablePools(ctx, u2, nil)
		require.NoError(t, err)
		require.Empty(t, usable)
	})
}
