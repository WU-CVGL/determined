package internal

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/config"
	detContext "github.com/determined-ai/determined/master/internal/context"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
)

func TestResourcePoolAccessRoutesRequirePermissions(t *testing.T) {
	originalUser := masterConfigRouteUser
	originalAuthorize := authorizeMasterConfigRoute
	t.Cleanup(func() {
		masterConfigRouteUser = originalUser
		authorizeMasterConfigRoute = originalAuthorize
	})
	masterConfigRouteUser = func(*http.Request) (*model.User, *model.UserSession, error) {
		return &model.User{Active: true}, &model.UserSession{}, nil
	}
	var updates []bool
	authorizeMasterConfigRoute = func(
		_ *http.Request, _ *model.User, update bool,
	) (error, error) {
		updates = append(updates, update)
		return errors.New("permission denied"), nil
	}

	e := echo.New()
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			return next(&detContext.DetContext{Context: c})
		}
	})
	(&Master{echo: e}).registerResourcePoolAccessRoutes()
	// Reading needs the permission to read the master configuration, and every write the
	// permission to update it.
	for _, test := range []struct {
		method string
		path   string
		update bool
	}{
		{http.MethodGet, "/api/v1/resource-pool-access", false},
		{http.MethodPut, "/api/v1/resource-pool-access/gpu", true},
		{http.MethodPost, "/api/v1/resource-pool-access/gpu/grant", true},
		{http.MethodPost, "/api/v1/resource-pool-access/gpu/revoke", true},
	} {
		updates = nil
		recorder := httptest.NewRecorder()
		e.ServeHTTP(recorder, httptest.NewRequest(test.method, test.path, nil))
		require.Equal(t, http.StatusForbidden, recorder.Code, test.method+" "+test.path)
		require.Equal(t, []bool{test.update}, updates, test.method+" "+test.path)
	}
}

func TestResourceManagerDefaultPools(t *testing.T) {
	for _, test := range []struct {
		name         string
		rmConfig     *config.ResourceManagerConfig
		compute, aux string
	}{
		{name: "none"},
		{name: "no type", rmConfig: &config.ResourceManagerConfig{}},
		{
			name: "agent",
			rmConfig: &config.ResourceManagerConfig{AgentRM: &config.AgentResourceManagerConfig{
				DefaultComputeResourcePool: "gpu", DefaultAuxResourcePool: "cpu",
			}},
			compute: "gpu", aux: "cpu",
		},
		{
			name: "kubernetes",
			rmConfig: &config.ResourceManagerConfig{
				KubernetesRM: &config.KubernetesResourceManagerConfig{
					DefaultComputeResourcePool: "gpu", DefaultAuxResourcePool: "cpu",
				},
			},
			compute: "gpu", aux: "cpu",
		},
		{
			name: "slurm",
			rmConfig: &config.ResourceManagerConfig{
				DispatcherRM: &config.DispatcherResourceManagerConfig{
					DefaultComputeResourcePool: ptrs.Ptr("gpu"),
				},
			},
			compute: "gpu",
		},
		{
			name: "pbs",
			rmConfig: &config.ResourceManagerConfig{
				PbsRM: &config.DispatcherResourceManagerConfig{
					DefaultAuxResourcePool: ptrs.Ptr("cpu"),
				},
			},
			aux: "cpu",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			compute, aux := resourceManagerDefaultPools(test.rmConfig)
			require.Equal(t, test.compute, compute)
			require.Equal(t, test.aux, aux)
		})
	}
}

func TestResourcePoolAccessWarnings(t *testing.T) {
	refused := `that omit resources.resource_pool are refused for users without a grant on "gpu"`
	defaults := resourcePoolAccessItem{
		PoolName: "gpu", Exists: true, DefaultCompute: true, DefaultAux: true,
		WorkspaceDefaults: []resourcePoolAccessWorkspaceDefault{
			{WorkspaceID: 4, Workspace: "vision", Kind: resourcePoolDefaultCompute},
			{WorkspaceID: 4, Workspace: "vision", Kind: resourcePoolDefaultAux},
		},
	}
	defaultWarnings := []string{
		`"gpu" is the cluster's default compute pool: submissions ` + refused,
		`"gpu" is the cluster's default aux pool: submissions ` + refused,
		`"gpu" is the default compute pool of workspace "vision": submissions there ` + refused,
		`"gpu" is the default aux pool of workspace "vision": submissions there ` + refused,
	}
	missing := `no resource pool named "gpu" exists; the setting applies to a pool created with ` +
		`this name`
	orphan := defaults
	orphan.Exists = false

	for _, test := range []struct {
		name     string
		item     resourcePoolAccessItem
		mode     string
		warnings []string
	}{
		{"a public pool", defaults, resourcePoolModePublic, []string{}},
		{"a restricted pool, each default it is", defaults, resourcePoolModeRestricted, defaultWarnings},
		{"no pool, public", orphan, resourcePoolModePublic, []string{missing}},
		{
			"no pool, restricted", orphan, resourcePoolModeRestricted,
			append([]string{missing}, defaultWarnings...),
		},
		{
			"a restricted pool that is no default",
			resourcePoolAccessItem{PoolName: "gpu", Exists: true},
			resourcePoolModeRestricted,
			[]string{},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			// The warnings follow the mode asked for, not the item's own.
			for _, itemMode := range []string{resourcePoolModePublic, resourcePoolModeRestricted} {
				item := test.item
				item.Mode = itemMode
				require.Equal(t, test.warnings, resourcePoolAccessWarnings(item, test.mode),
					"item mode %s", itemMode)
			}
		})
	}
}
