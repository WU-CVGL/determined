package poolaccess

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/pkg/model"
)

// fakeRestrictions is a RestrictionReader over restricted pools and their grants, which counts
// its reads and the pools it was asked for.
type fakeRestrictions struct {
	grants map[string][]model.UserID
	err    error
	reads  int
	asked  [][]string
}

func (f *fakeRestrictions) read(
	_ context.Context, userID model.UserID, pools []string,
) (map[string]bool, error) {
	f.reads++
	f.asked = append(f.asked, pools)
	if f.err != nil {
		return nil, f.err
	}
	restricted := map[string]bool{}
	for _, pool := range pools {
		grants, ok := f.grants[pool]
		if !ok {
			continue
		}
		restricted[pool] = false
		for _, id := range grants {
			if id == userID {
				restricted[pool] = true
			}
		}
	}
	return restricted, nil
}

var (
	fakeAlice = model.User{ID: 1, Username: "alice"}
	fakeBob   = model.User{ID: 2, Username: "bob"}
	fakeAdmin = model.User{ID: 3, Username: "admin", Admin: true}
)

func deniedMessage(user model.User, pool string) string {
	return fmt.Sprintf("user %q may not use resource pool %q: the pool is restricted; choose "+
		"another pool or ask an administrator for access (if resources.resource_pool was not "+
		"set, %q is the default pool for this workspace or the cluster)",
		user.Username, pool, pool)
}

func TestCheckerCanUseResourcePool(t *testing.T) {
	for _, tc := range []struct {
		name  string
		user  model.User
		pool  string
		code  codes.Code
		reads int
	}{
		{"a pool without a restriction is public", fakeAlice, "public", codes.OK, 1},
		{"restricted with no grants", fakeAlice, "admins-only", codes.PermissionDenied, 1},
		{"restricted with a grant", fakeAlice, "granted", codes.OK, 1},
		{"restricted with another user's grant", fakeBob, "granted", codes.PermissionDenied, 1},
		{"an admin reads nothing", fakeAdmin, "admins-only", codes.OK, 0},
		{"an empty pool name", fakeAlice, "", codes.Internal, 0},
		{"an empty pool name, admin", fakeAdmin, "", codes.Internal, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restrictions := &fakeRestrictions{grants: map[string][]model.UserID{
				"admins-only": nil, "granted": {fakeAlice.ID},
			}}
			err := NewChecker(restrictions.read).CanUseResourcePool(
				context.Background(), tc.user, tc.pool)
			require.Equal(t, tc.code, status.Code(err), "%v", err)
			require.Equal(t, tc.reads, restrictions.reads)
			if tc.reads > 0 {
				require.Equal(t, [][]string{{tc.pool}}, restrictions.asked)
			}
			if tc.code == codes.PermissionDenied {
				require.Equal(t, deniedMessage(tc.user, tc.pool), status.Convert(err).Message())
			}
		})
	}
}

func TestCheckerFailedReadIsNeverPublic(t *testing.T) {
	ctx := context.Background()
	restrictions := &fakeRestrictions{err: errors.New("connection refused")}
	checker := NewChecker(restrictions.read)

	err := checker.CanUseResourcePool(ctx, fakeAlice, "public")
	require.Equal(t, codes.Unavailable, status.Code(err))
	require.Equal(t,
		`could not check access to resource pool "public": connection refused; try again`,
		status.Convert(err).Message())
	_, err = checker.UsablePools(ctx, fakeAlice, []string{"public"})
	require.Equal(t, codes.Unavailable, status.Code(err))
	require.Equal(t, "could not check access to resource pools: connection refused; try again",
		status.Convert(err).Message())
	require.Equal(t, 2, restrictions.reads)

	// The admin predicate is checked first and reads no access table.
	require.NoError(t, checker.CanUseResourcePool(ctx, fakeAdmin, "public"))
	usable, err := checker.UsablePools(ctx, fakeAdmin, []string{"public", "admins-only"})
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"public": true, "admins-only": true}, usable)
	require.Equal(t, 2, restrictions.reads)
}

func TestCheckerUsablePools(t *testing.T) {
	ctx := context.Background()
	restrictions := &fakeRestrictions{grants: map[string][]model.UserID{
		"admins-only": nil, "granted": {fakeAlice.ID},
	}}
	checker := NewChecker(restrictions.read)
	pools := []string{"public", "admins-only", "granted"}

	usable, err := checker.UsablePools(ctx, fakeAlice, pools)
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"public": true, "admins-only": false, "granted": true}, usable)
	usable, err = checker.UsablePools(ctx, fakeBob, pools)
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"public": true, "admins-only": false, "granted": false}, usable)
	require.Equal(t, [][]string{pools, pools}, restrictions.asked, "one read per call")
	usable, err = checker.UsablePools(ctx, fakeBob, nil)
	require.NoError(t, err)
	require.Empty(t, usable)
}

func TestMemoDecidesEachPoolOnce(t *testing.T) {
	ctx := context.Background()
	restrictions := &fakeRestrictions{grants: map[string][]model.UserID{
		"admins-only": nil, "granted": {fakeAlice.ID},
	}}
	checker := NewChecker(restrictions.read)
	adminChecks := 0
	checker.isAdmin = func(_ context.Context, user model.User) (bool, error) {
		adminChecks++
		return user.Admin, nil
	}

	memo := checker.NewMemo(fakeAlice)
	for i := 0; i < 3; i++ {
		require.NoError(t, memo.CanUseResourcePool(ctx, "public"))
		require.NoError(t, memo.CanUseResourcePool(ctx, "granted"))
		err := memo.CanUseResourcePool(ctx, "admins-only")
		require.Equal(t, codes.PermissionDenied, status.Code(err), "%v", err)
		require.Equal(t, deniedMessage(fakeAlice, "admins-only"), status.Convert(err).Message())
		// An empty pool name is a bug of the caller: it is refused without a check.
		require.Equal(t, codes.Internal, status.Code(memo.CanUseResourcePool(ctx, "")))
	}
	require.Equal(t, 1, adminChecks)
	require.Equal(t, [][]string{{"public"}, {"granted"}, {"admins-only"}}, restrictions.asked)

	// Another request decides again.
	require.NoError(t, checker.NewMemo(fakeAlice).CanUseResourcePool(ctx, "public"))
	require.Equal(t, 2, adminChecks)
	require.Equal(t, 4, restrictions.reads)

	// An administrator is checked once and reads nothing.
	adminChecks = 0
	adminMemo := checker.NewMemo(fakeAdmin)
	require.NoError(t, adminMemo.CanUseResourcePool(ctx, "admins-only"))
	require.NoError(t, adminMemo.CanUseResourcePool(ctx, "granted"))
	require.Equal(t, 1, adminChecks)
	require.Equal(t, 4, restrictions.reads)
}

// countAdminChecks makes checker count its admin predicate's calls, and fail them with *err while
// it is set.
func countAdminChecks(checker *Checker, err *error) *int {
	checks := 0
	checker.isAdmin = func(_ context.Context, user model.User) (bool, error) {
		checks++
		if *err != nil {
			return false, *err
		}
		return user.Admin, nil
	}
	return &checks
}

func TestMemoConcurrentCallsDecideOnce(t *testing.T) {
	const callers = 8
	for _, test := range []struct {
		pool string
		code codes.Code
	}{
		{"granted", codes.OK},
		{"admins-only", codes.PermissionDenied},
	} {
		t.Run(test.pool, func(t *testing.T) {
			restrictions := &fakeRestrictions{grants: map[string][]model.UserID{
				"admins-only": nil, "granted": {fakeAlice.ID},
			}}
			// The read waits until every caller is about to call, so all of them are in
			// flight while the pool is read.
			var calling sync.WaitGroup
			calling.Add(callers)
			checker := NewChecker(func(
				ctx context.Context, userID model.UserID, pools []string,
			) (map[string]bool, error) {
				calling.Wait()
				return restrictions.read(ctx, userID, pools)
			})
			var adminErr error
			adminChecks := countAdminChecks(checker, &adminErr)
			memo := checker.NewMemo(fakeAlice)

			start := make(chan struct{})
			errs := make([]error, callers)
			var done sync.WaitGroup
			for i := 0; i < callers; i++ {
				done.Add(1)
				go func(i int) {
					defer done.Done()
					<-start
					calling.Done()
					errs[i] = memo.CanUseResourcePool(context.Background(), test.pool)
				}(i)
			}
			close(start)
			done.Wait()

			require.Equal(t, 1, *adminChecks)
			require.Equal(t, [][]string{{test.pool}}, restrictions.asked)
			require.Equal(t, test.code, status.Code(errs[0]), "%v", errs[0])
			for _, err := range errs {
				require.True(t, err == errs[0], "every caller gets the same result: %v", err)
			}
		})
	}
}

func TestMemoStableWithinRequest(t *testing.T) {
	ctx := context.Background()
	restrictions := &fakeRestrictions{grants: map[string][]model.UserID{"gpu": {fakeAlice.ID}}}
	checker := NewChecker(restrictions.read)

	// Allowed, then the grant is revoked.
	before := checker.NewMemo(fakeAlice)
	require.NoError(t, before.CanUseResourcePool(ctx, "gpu"))
	restrictions.grants["gpu"] = nil
	require.NoError(t, before.CanUseResourcePool(ctx, "gpu"))
	after := checker.NewMemo(fakeAlice)
	require.Equal(t, codes.PermissionDenied, status.Code(after.CanUseResourcePool(ctx, "gpu")))

	// Refused, then granted.
	restrictions.grants["gpu"] = []model.UserID{fakeAlice.ID}
	require.Equal(t, codes.PermissionDenied, status.Code(after.CanUseResourcePool(ctx, "gpu")))
	require.NoError(t, checker.NewMemo(fakeAlice).CanUseResourcePool(ctx, "gpu"))
	require.Equal(t, 3, restrictions.reads)
}

func TestMemoErrors(t *testing.T) {
	ctx := context.Background()
	restrictions := &fakeRestrictions{grants: map[string][]model.UserID{
		"a": {fakeAlice.ID}, "b": nil,
	}}
	failing := true
	checker := NewChecker(func(
		ctx context.Context, userID model.UserID, pools []string,
	) (map[string]bool, error) {
		if failing && pools[0] == "a" {
			restrictions.reads++
			return nil, errors.New("connection refused")
		}
		return restrictions.read(ctx, userID, pools)
	})
	var adminErr error
	adminChecks := countAdminChecks(checker, &adminErr)

	// A read error of pool a is kept for a; pool b is decided as usual.
	memo := checker.NewMemo(fakeAlice)
	for i := 0; i < 2; i++ {
		err := memo.CanUseResourcePool(ctx, "a")
		require.Equal(t, codes.Unavailable, status.Code(err), "%v", err)
		require.Equal(t,
			`could not check access to resource pool "a": connection refused; try again`,
			status.Convert(err).Message())
		require.Equal(t, codes.PermissionDenied, status.Code(memo.CanUseResourcePool(ctx, "b")))
		require.NoError(t, memo.CanUseResourcePool(ctx, "public"))
	}
	require.Equal(t, 3, restrictions.reads, "a failed read is not repeated within the request")

	// Once the reads work again, the old Memo keeps the error and a new Memo decides a.
	failing = false
	require.Equal(t, codes.Unavailable, status.Code(memo.CanUseResourcePool(ctx, "a")))
	require.NoError(t, checker.NewMemo(fakeAlice).CanUseResourcePool(ctx, "a"))
	require.Equal(t, 4, restrictions.reads)
	require.Equal(t, 2, *adminChecks)

	// A failed admin check fails every pool of the Memo, with one check and no read.
	adminErr = status.Error(codes.Internal, "authz down")
	memo = checker.NewMemo(fakeAlice)
	for _, pool := range []string{"a", "b", "public", "a"} {
		require.Equal(t, codes.Internal, status.Code(memo.CanUseResourcePool(ctx, pool)), pool)
	}
	require.Equal(t, 3, *adminChecks)
	require.Equal(t, 4, restrictions.reads)

	adminErr = nil
	require.NoError(t, checker.NewMemo(fakeAlice).CanUseResourcePool(ctx, "a"))
	require.Equal(t, 4, *adminChecks)
}
