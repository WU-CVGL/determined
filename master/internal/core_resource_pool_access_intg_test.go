//go:build integration
// +build integration

package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/config"
	detContext "github.com/determined-ai/determined/master/internal/context"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/poolaccess"
	"github.com/determined-ai/determined/master/pkg/model"
)

const testUserHeader = "X-Test-User"

// poolAccessRoutes serves the resource pool access routes of a master with the given resource
// manager. A request authenticates as the user named by its X-Test-User header, and without one it
// is unauthenticated. Authorization is the cluster's, basic in these tests.
type poolAccessRoutes struct {
	t    *testing.T
	echo *echo.Echo
}

func newPoolAccessRoutes(
	t *testing.T, pgDB *db.PgDB, rmConfig *config.ResourceManagerWithPoolsConfig,
	users ...model.User,
) poolAccessRoutes {
	byUsername := map[string]model.User{}
	for _, u := range users {
		byUsername[u.Username] = u
	}
	originalUser := masterConfigRouteUser
	t.Cleanup(func() { masterConfigRouteUser = originalUser })
	masterConfigRouteUser = func(request *http.Request) (*model.User, *model.UserSession, error) {
		u, ok := byUsername[request.Header.Get(testUserHeader)]
		if !ok {
			return nil, nil, echo.ErrUnauthorized
		}
		return &u, &model.UserSession{}, nil
	}

	e := echo.New()
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			return next(&detContext.DetContext{Context: c})
		}
	})
	m := &Master{
		echo: e,
		db:   pgDB,
		config: &config.Config{ResourceConfig: config.ResourceConfig{
			RootManagerInternal: rmConfig.ResourceManager,
			RootPoolsInternal:   rmConfig.ResourcePools,
		}},
	}
	m.registerResourcePoolAccessRoutes()
	return poolAccessRoutes{t: t, echo: e}
}

func (r poolAccessRoutes) sendAs(
	user *model.User, method, path, contentType, body string,
) (int, []byte) {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		request.Header.Set(echo.HeaderContentType, contentType)
	}
	if user != nil {
		request.Header.Set(testUserHeader, user.Username)
	}
	recorder := httptest.NewRecorder()
	r.echo.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.Bytes()
}

// write sends a write as user and returns the pool's access.
func (r poolAccessRoutes) write(
	user model.User, method, path, body string,
) resourcePoolAccessItem {
	r.t.Helper()
	code, response := r.sendAs(&user, method, path, echo.MIMEApplicationJSON, body)
	require.Equal(r.t, http.StatusOK, code, string(response))
	var written resourcePoolAccessItem
	require.NoError(r.t, json.Unmarshal(response, &written), string(response))
	return written
}

// list returns the access items that the admin API lists, by pool name.
func (r poolAccessRoutes) list(user model.User) map[string]resourcePoolAccessItem {
	r.t.Helper()
	code, response := r.sendAs(&user, http.MethodGet, resourcePoolAccessPath, "", "")
	require.Equal(r.t, http.StatusOK, code, string(response))
	var listed resourcePoolAccessListResponse
	require.NoError(r.t, json.Unmarshal(response, &listed), string(response))
	items := map[string]resourcePoolAccessItem{}
	var names []string
	for _, item := range listed.ResourcePools {
		items[item.PoolName] = item
		names = append(names, item.PoolName)
	}
	require.IsIncreasing(r.t, names)
	return items
}

func (r poolAccessRoutes) requireError(
	user model.User, code int, message, method, path, contentType, body string,
) {
	r.t.Helper()
	gotCode, response := r.sendAs(&user, method, path, contentType, body)
	require.Equal(r.t, code, gotCode, string(response))
	var decoded map[string]interface{}
	require.NoError(r.t, json.Unmarshal(response, &decoded), string(response))
	require.Contains(r.t, decoded["message"], message)
}

func poolAccessPath(pool string, action ...string) string {
	return strings.Join(append([]string{resourcePoolAccessPath, url.PathEscape(pool)}, action...), "/")
}

func requirePoolAccess(t *testing.T, user model.User, pool string, allowed bool) {
	t.Helper()
	err := poolaccess.CanUseResourcePool(context.Background(), user, pool)
	if allowed {
		require.NoError(t, err)
		return
	}
	require.Equal(t, codes.PermissionDenied, status.Code(err), err)
}

func usernamesOf(users []resourcePoolAccessUser) []string {
	names := []string{}
	for _, u := range users {
		names = append(names, u.Username)
	}
	return names
}

func TestResourcePoolAccessRoutes(t *testing.T) {
	api, admin, _ := setupAPITest(t, nil)
	ctx := context.Background()
	pgDB := api.m.db
	alice := db.RequireMockUser(t, pgDB)
	bob := db.RequireMockUser(t, pgDB)
	carol := db.RequireMockUser(t, pgDB)
	_, err := db.Bun().NewRaw(`UPDATE users SET active = false WHERE id = ?`, carol.ID).Exec(ctx)
	require.NoError(t, err)

	suffix := uuid.NewString()
	compute := "acl-compute-" + suffix
	aux := "acl-aux-" + suffix
	known := "acl-known-" + suffix
	shared := "acl-workspace-" + suffix
	dynamic := "acl-dynamic-" + suffix
	missing := "acl-missing-" + suffix
	slashed := "acl-slashed/" + suffix
	t.Cleanup(func() {
		for _, pool := range []string{compute, aux, known, shared, dynamic, missing, slashed} {
			_, err := db.Bun().NewRaw(
				`DELETE FROM resource_pool_restrictions WHERE pool_name = ?`, pool).Exec(ctx)
			require.NoError(t, err)
			_, err = db.Bun().NewRaw(
				`DELETE FROM resource_pool_grants WHERE pool_name = ?`, pool).Exec(ctx)
			require.NoError(t, err)
		}
		_, err := db.Bun().NewRaw(
			`DELETE FROM dynamic_resource_pools WHERE pool_name = ?`, dynamic).Exec(ctx)
		require.NoError(t, err)
	})

	// A dynamic pool in any state is a pool, although the resource managers list Ready ones only.
	_, _, err = pgDB.CreateDynamicResourcePool(ctx, db.DynamicResourcePool{
		ClusterName: "agent-cluster", PoolName: dynamic, ConfigVersion: 1,
		IdempotencyKey: "acl-" + suffix,
		Config:         json.RawMessage(fmt.Sprintf(`{"pool_name":%q}`, dynamic)),
		ConfigHash:     "acl-hash-" + suffix,
	})
	require.NoError(t, err)

	// Workspace W has the shared pool as its default compute and aux pool.
	workspaceID, workspace := db.RequireMockWorkspaceID(t, pgDB, "")
	_, err = db.Bun().NewRaw(
		`UPDATE workspaces SET default_compute_pool = ?, default_aux_pool = ? WHERE id = ?`,
		shared, shared, workspaceID).Exec(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.Bun().NewRaw(`UPDATE workspaces SET default_compute_pool = NULL, `+
			`default_aux_pool = NULL WHERE id = ?`, workspaceID).Exec(ctx)
		require.NoError(t, err)
	})

	routes := newPoolAccessRoutes(t, pgDB, &config.ResourceManagerWithPoolsConfig{
		ResourceManager: &config.ResourceManagerConfig{AgentRM: &config.AgentResourceManagerConfig{
			ClusterName:                "agent-cluster",
			DefaultComputeResourcePool: compute,
			DefaultAuxResourcePool:     aux,
		}},
		ResourcePools: []config.ResourcePoolConfig{
			{PoolName: compute}, {PoolName: aux}, {PoolName: known}, {PoolName: shared},
		},
	}, admin, alice, bob)

	t.Run("authentication and authorization", func(t *testing.T) {
		code, _ := routes.sendAs(nil, http.MethodGet, resourcePoolAccessPath, "", "")
		require.Equal(t, http.StatusUnauthorized, code)
		code, _ = routes.sendAs(nil, http.MethodPut, poolAccessPath(known),
			echo.MIMEApplicationJSON, `{"mode":"restricted"}`)
		require.Equal(t, http.StatusUnauthorized, code)

		for _, request := range []struct{ method, path, body string }{
			{http.MethodGet, resourcePoolAccessPath, ""},
			{http.MethodPut, poolAccessPath(known), `{"mode":"restricted"}`},
			{
				http.MethodPost, poolAccessPath(known, "grant"),
				fmt.Sprintf(`{"usernames":[%q]}`, alice.Username),
			},
			{
				http.MethodPost, poolAccessPath(known, "revoke"),
				fmt.Sprintf(`{"usernames":[%q]}`, alice.Username),
			},
		} {
			code, response := routes.sendAs(
				&alice, request.method, request.path, echo.MIMEApplicationJSON, request.body)
			require.Equal(t, http.StatusForbidden, code, request.method+" "+request.path)
			require.Contains(t, string(response), "user does not have permission")
		}
		// Nothing was written.
		require.Equal(t, resourcePoolModePublic, routes.list(admin)[known].Mode)
		require.Empty(t, routes.list(admin)[known].Users)
	})

	t.Run("list known pools without records", func(t *testing.T) {
		items := routes.list(admin)
		require.Equal(t, resourcePoolAccessItem{
			PoolName: known, Mode: resourcePoolModePublic, Exists: true,
			WorkspaceDefaults:    []resourcePoolAccessWorkspaceDefault{},
			Users:                []resourcePoolAccessUser{},
			Warnings:             []string{},
			WarningsIfRestricted: []string{},
		}, items[known])
		require.True(t, items[compute].DefaultCompute)
		require.False(t, items[compute].DefaultAux)
		require.True(t, items[aux].DefaultAux)
		require.False(t, items[aux].DefaultCompute)
		require.True(t, items[dynamic].Exists)
		require.Equal(t, []resourcePoolAccessWorkspaceDefault{
			{WorkspaceID: workspaceID, Workspace: workspace, Kind: resourcePoolDefaultCompute},
			{WorkspaceID: workspaceID, Workspace: workspace, Kind: resourcePoolDefaultAux},
		}, items[shared].WorkspaceDefaults)
		require.NotContains(t, items, missing)
	})

	t.Run("restrict a name with no pool", func(t *testing.T) {
		written := routes.write(admin, http.MethodPut, poolAccessPath(missing),
			`{"mode":"restricted"}`)
		require.Equal(t, missing, written.PoolName)
		require.Equal(t, resourcePoolModeRestricted, written.Mode)
		require.False(t, written.Exists)
		require.NotNil(t, written.RestrictedAt)
		require.Equal(t, admin.Username, *written.RestrictedBy)
		require.Equal(t, []string{fmt.Sprintf("no resource pool named %q exists; the setting "+
			"applies to a pool created with this name", missing)}, written.Warnings)
		requirePoolAccess(t, alice, missing, false)
		requirePoolAccess(t, admin, missing, true)

		// The name is listed as an orphan, and making it public removes its last record.
		require.False(t, routes.list(admin)[missing].Exists)
		written = routes.write(admin, http.MethodPut, poolAccessPath(missing), `{"mode":"public"}`)
		require.Equal(t, resourcePoolModePublic, written.Mode)
		require.Nil(t, written.RestrictedAt)
		require.Nil(t, written.RestrictedBy)
		require.NotContains(t, routes.list(admin), missing)
		requirePoolAccess(t, alice, missing, true)
	})

	t.Run("restrict default pools", func(t *testing.T) {
		refused := fmt.Sprintf(
			"that omit resources.resource_pool are refused for users without a grant on %q", compute)
		written := routes.write(admin, http.MethodPut, poolAccessPath(compute),
			`{"mode":"restricted"}`)
		require.True(t, written.Exists)
		require.Equal(t, []string{fmt.Sprintf(
			"%q is the cluster's default compute pool: submissions %s", compute, refused,
		)}, written.Warnings)
		// Restricting again changes nothing and still warns.
		again := routes.write(admin, http.MethodPut, poolAccessPath(compute),
			`{"mode":"restricted"}`)
		require.Equal(t, written, again)
		// Making it public again has nothing to warn about.
		written = routes.write(admin, http.MethodPut, poolAccessPath(compute), `{"mode":"public"}`)
		require.Equal(t, resourcePoolModePublic, written.Mode)
		require.Empty(t, written.Warnings)

		refused = fmt.Sprintf(
			"that omit resources.resource_pool are refused for users without a grant on %q", aux)
		written = routes.write(admin, http.MethodPut, poolAccessPath(aux), `{"mode":"restricted"}`)
		require.Equal(t, []string{fmt.Sprintf(
			"%q is the cluster's default aux pool: submissions %s", aux, refused,
		)}, written.Warnings)
		// A grant on a restricted default warns too.
		written = routes.write(admin, http.MethodPost, poolAccessPath(aux, "grant"),
			fmt.Sprintf(`{"usernames":[%q]}`, alice.Username))
		require.Equal(t, []string{alice.Username}, usernamesOf(written.Users))
		require.Len(t, written.Warnings, 1)
		requirePoolAccess(t, alice, aux, true)
		requirePoolAccess(t, bob, aux, false)

		refused = fmt.Sprintf(
			"that omit resources.resource_pool are refused for users without a grant on %q", shared)
		written = routes.write(admin, http.MethodPut, poolAccessPath(shared),
			`{"mode":"restricted"}`)
		require.Equal(t, []resourcePoolAccessWorkspaceDefault{
			{WorkspaceID: workspaceID, Workspace: workspace, Kind: resourcePoolDefaultCompute},
			{WorkspaceID: workspaceID, Workspace: workspace, Kind: resourcePoolDefaultAux},
		}, written.WorkspaceDefaults)
		require.Equal(t, []string{
			fmt.Sprintf("%q is the default compute pool of workspace %q: submissions there %s",
				shared, workspace, refused),
			fmt.Sprintf("%q is the default aux pool of workspace %q: submissions there %s",
				shared, workspace, refused),
		}, written.Warnings)
	})

	t.Run("grant, then restrict", func(t *testing.T) {
		written := routes.write(admin, http.MethodPost, poolAccessPath(known, "grant"),
			fmt.Sprintf(`{"usernames":[%q,%q,%q]}`, alice.Username, carol.Username, alice.Username))
		require.Equal(t, resourcePoolModePublic, written.Mode)
		require.Empty(t, written.Warnings)
		require.ElementsMatch(t, []resourcePoolAccessUser{
			{ID: alice.ID, Username: alice.Username, Active: true},
			{ID: carol.ID, Username: carol.Username, Active: false},
		}, written.Users)
		requirePoolAccess(t, alice, known, true)
		requirePoolAccess(t, bob, known, true)

		written = routes.write(admin, http.MethodPut, poolAccessPath(known),
			`{"mode":"restricted"}`)
		require.Equal(t, resourcePoolModeRestricted, written.Mode)
		require.Len(t, written.Users, 2)
		requirePoolAccess(t, alice, known, true)
		requirePoolAccess(t, bob, known, false)

		// One unknown username refuses the request, naming every unknown one, and grants nothing.
		unknown1, unknown2 := "acl-nobody-"+uuid.NewString(), "acl-nobody-"+uuid.NewString()
		routes.requireError(admin, http.StatusNotFound,
			fmt.Sprintf("unknown users: %s, %s; nothing was changed", unknown1, unknown2),
			http.MethodPost, poolAccessPath(known, "grant"), echo.MIMEApplicationJSON,
			fmt.Sprintf(`{"usernames":[%q,%q,%q]}`, unknown1, bob.Username, unknown2))
		require.Len(t, routes.list(admin)[known].Users, 2)
		requirePoolAccess(t, bob, known, false)
		routes.requireError(admin, http.StatusNotFound, "nothing was changed",
			http.MethodPost, poolAccessPath(known, "revoke"), echo.MIMEApplicationJSON,
			fmt.Sprintf(`{"usernames":[%q,%q]}`, alice.Username, unknown1))
		require.Len(t, routes.list(admin)[known].Users, 2)

		// Making the pool public keeps its grants.
		written = routes.write(admin, http.MethodPut, poolAccessPath(known), `{"mode":"public"}`)
		require.Equal(t, resourcePoolModePublic, written.Mode)
		require.Len(t, written.Users, 2)

		// Revoking is idempotent.
		for i := 0; i < 2; i++ {
			written = routes.write(admin, http.MethodPost, poolAccessPath(known, "revoke"),
				fmt.Sprintf(`{"usernames":[%q]}`, alice.Username))
			require.Equal(t, []string{carol.Username}, usernamesOf(written.Users))
		}
		// Restricting again applies the remaining grants.
		routes.write(admin, http.MethodPut, poolAccessPath(known), `{"mode":"restricted"}`)
		requirePoolAccess(t, alice, known, false)
		requirePoolAccess(t, carol, known, true)
	})

	t.Run("a pool name with a slash", func(t *testing.T) {
		written := routes.write(admin, http.MethodPut, poolAccessPath(slashed),
			`{"mode":"restricted"}`)
		require.Equal(t, slashed, written.PoolName)
		require.Equal(t, resourcePoolModeRestricted, routes.list(admin)[slashed].Mode)
		requirePoolAccess(t, alice, slashed, false)
	})

	t.Run("invalid requests", func(t *testing.T) {
		path := poolAccessPath(known)
		grant := poolAccessPath(known, "grant")
		for _, test := range []struct {
			code                      int
			message                   string
			method, path, contentType string
			body                      string
		}{
			{
				http.StatusBadRequest, `unknown field "pool"`, http.MethodPut, path,
				echo.MIMEApplicationJSON, `{"mode":"public","pool":"x"}`,
			},
			{
				http.StatusBadRequest, `mode must be "public" or "restricted"`, http.MethodPut, path,
				echo.MIMEApplicationJSON, `{"mode":"admins"}`,
			},
			{
				http.StatusBadRequest, `mode must be "public" or "restricted"`, http.MethodPut, path,
				echo.MIMEApplicationJSON, `{}`,
			},
			{
				http.StatusBadRequest, "exactly one value", http.MethodPut, path,
				echo.MIMEApplicationJSON, `{"mode":"public"} {"mode":"restricted"}`,
			},
			{
				http.StatusBadRequest, "invalid JSON body", http.MethodPut, path,
				echo.MIMEApplicationJSON, `["public"]`,
			},
			{
				http.StatusUnsupportedMediaType, "Content-Type must be application/json",
				http.MethodPut, path, "text/plain", `{"mode":"public"}`,
			},
			{
				http.StatusUnsupportedMediaType, "Content-Type must be application/json",
				http.MethodPost, grant, "", `{"usernames":["x"]}`,
			},
			{
				http.StatusRequestEntityTooLarge, "exceeds 64 KiB", http.MethodPut, path,
				echo.MIMEApplicationJSON, `{"mode":"public"}` + strings.Repeat(" ", 64<<10),
			},
			{
				http.StatusBadRequest, "at least one user", http.MethodPost, grant,
				echo.MIMEApplicationJSON, `{"usernames":[]}`,
			},
			{
				http.StatusBadRequest, "must not be empty", http.MethodPost, grant,
				echo.MIMEApplicationJSON, `{"usernames":[""]}`,
			},
			{
				http.StatusBadRequest, `unknown field "users"`, http.MethodPost, grant,
				echo.MIMEApplicationJSON, `{"users":["x"]}`,
			},
		} {
			routes.requireError(admin, test.code, test.message, test.method, test.path,
				test.contentType, test.body)
		}
		// The pool kept its access.
		item := routes.list(admin)[known]
		require.Equal(t, resourcePoolModeRestricted, item.Mode)
		require.Equal(t, []string{carol.Username}, usernamesOf(item.Users))
	})

	t.Run("a write answers with the pool's list item", func(t *testing.T) {
		// A write reads only its pool; the list reads every pool. Both show the same item, with
		// the same warnings: a default pool, a workspace default, a dynamic pool, a name with a
		// dormant grant only.
		for _, pool := range []string{compute, aux, known, shared, dynamic, missing, slashed} {
			written := routes.write(admin, http.MethodPost, poolAccessPath(pool, "grant"),
				fmt.Sprintf(`{"usernames":[%q]}`, bob.Username))
			require.Contains(t, usernamesOf(written.Users), bob.Username, pool)
			require.Equal(t, routes.list(admin)[pool], written, pool)
		}
	})

	t.Run("the list carries each pool's warnings", func(t *testing.T) {
		code, response := routes.sendAs(&admin, http.MethodGet, resourcePoolAccessPath, "", "")
		require.Equal(t, http.StatusOK, code, string(response))
		var listed struct {
			ResourcePools []map[string]any `json:"resource_pools"`
		}
		require.NoError(t, json.Unmarshal(response, &listed), string(response))
		warnings := map[string][]any{}
		ifRestricted := map[string][]any{}
		for _, item := range listed.ResourcePools {
			name, _ := item["pool_name"].(string)
			require.IsType(t, []any{}, item["warnings"], name)
			require.IsType(t, []any{}, item["warnings_if_restricted"], name)
			warnings[name], _ = item["warnings"].([]any)
			ifRestricted[name], _ = item["warnings_if_restricted"].([]any)
		}
		refused := func(pool string) string {
			return fmt.Sprintf("that omit resources.resource_pool are refused for users "+
				"without a grant on %q", pool)
		}
		computeWarning := fmt.Sprintf("%q is the cluster's default compute pool: submissions %s",
			compute, refused(compute))
		auxWarning := fmt.Sprintf("%q is the cluster's default aux pool: submissions %s",
			aux, refused(aux))

		// The default compute pool is public again: it warns only once restricted.
		require.Empty(t, warnings[compute])
		require.Equal(t, []any{computeWarning}, ifRestricted[compute])
		// The default aux pool is restricted: it warns now.
		require.Equal(t, []any{auxWarning}, warnings[aux])
		require.Equal(t, []any{auxWarning}, ifRestricted[aux])
		require.Len(t, warnings[shared], 2)
		require.Equal(t, warnings[shared], ifRestricted[shared])
		require.Empty(t, warnings[dynamic])
		require.Empty(t, ifRestricted[dynamic])
		// A name with records but no pool warns in either mode.
		missingWarning := fmt.Sprintf("no resource pool named %q exists; the setting applies to "+
			"a pool created with this name", missing)
		require.Equal(t, []any{missingWarning}, warnings[missing])
		require.Equal(t, []any{missingWarning}, ifRestricted[missing])
	})
}
