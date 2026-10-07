//go:build integration

package internal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/config"
	detContext "github.com/determined-ai/determined/master/internal/context"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/rm"
	"github.com/determined-ai/determined/master/internal/rm/agentrm"
	"github.com/determined-ai/determined/master/pkg/model"
)

// dynamicPoolRouteTest serves the dynamic pool routes of a master with one agent resource manager
// to an administrator.
type dynamicPoolRouteTest struct {
	t               *testing.T
	echo            *echo.Echo
	resourceManager *agentrm.ResourceManager
}

func newDynamicPoolRouteTest(
	t *testing.T, pgDB *db.PgDB, rmConfig *config.ResourceManagerWithPoolsConfig,
) dynamicPoolRouteTest {
	originalUser := masterConfigRouteUser
	originalAuthorize := authorizeMasterConfigRoute
	t.Cleanup(func() {
		masterConfigRouteUser = originalUser
		authorizeMasterConfigRoute = originalAuthorize
		// The test's own database replaced the shared one in Bun and is dropped when the test
		// ends, so the next setupAPITest must connect to the shared database again.
		thePgDB = nil
	})
	masterConfigRouteUser = func(*http.Request) (*model.User, *model.UserSession, error) {
		return &model.User{Active: true, Admin: true}, &model.UserSession{}, nil
	}
	authorizeMasterConfigRoute = func(*http.Request, *model.User, bool) (error, error) {
		return nil, nil
	}

	masterDefaults := *model.DefaultTaskContainerDefaults()
	resourceManager, err := agentrm.New(
		context.Background(), pgDB, echo.New(), rmConfig, nil, nil, &masterDefaults,
	)
	require.NoError(t, err)
	t.Cleanup(resourceManager.StopDynamicPoolWorker)

	e := echo.New()
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			return next(&detContext.DetContext{Context: c})
		}
	})
	m := &Master{
		echo: e,
		db:   pgDB,
		config: &config.Config{
			TaskContainerDefaults: masterDefaults,
			ResourceConfig: config.ResourceConfig{
				RootManagerInternal: rmConfig.ResourceManager,
				RootPoolsInternal:   rmConfig.ResourcePools,
			},
		},
		allRms: map[string]rm.ResourceManager{"agent-cluster": resourceManager},
	}
	m.registerDynamicResourcePoolRoutes()
	return dynamicPoolRouteTest{t: t, echo: e, resourceManager: resourceManager}
}

func (r dynamicPoolRouteTest) send(method, path, body string) (int, map[string]interface{}) {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	recorder := httptest.NewRecorder()
	r.echo.ServeHTTP(recorder, request)
	var response map[string]interface{}
	require.NoError(r.t, json.Unmarshal(recorder.Body.Bytes(), &response), recorder.Body.String())
	return recorder.Code, response
}

func (r dynamicPoolRouteTest) requireError(code int, message string, method, path, body string) {
	r.t.Helper()
	gotCode, response := r.send(method, path, body)
	require.Equal(r.t, code, gotCode, response)
	require.Contains(r.t, response["message"], message)
}

// testDynamicPoolRouteRMConfig returns an agent RM whose master.yaml pools are default and pools.
func testDynamicPoolRouteRMConfig(
	pools ...config.ResourcePoolConfig,
) *config.ResourceManagerWithPoolsConfig {
	return &config.ResourceManagerWithPoolsConfig{
		ResourceManager: &config.ResourceManagerConfig{AgentRM: &config.AgentResourceManagerConfig{
			ClusterName:                "agent-cluster",
			DefaultComputeResourcePool: "default",
			DefaultAuxResourcePool:     "default",
			Scheduler:                  config.DefaultSchedulerConfig(),
		}},
		ResourcePools: append([]config.ResourcePoolConfig{{
			PoolName: "default", MaxAuxContainersPerAgent: 100,
		}}, pools...),
	}
}

const dynamicPoolsPath = "/api/v1/resource-pools/dynamic"

func TestUpdateDynamicResourcePoolRoute(t *testing.T) {
	pgDB, cleanup := db.MustResolveNewPostgresDatabase(t)
	defer cleanup()
	db.MustMigrateTestPostgres(t, pgDB, "file://../static/migrations", "up")
	ctx := context.Background()

	routes := newDynamicPoolRouteTest(t, pgDB, testDynamicPoolRouteRMConfig())
	resourceManager, send, requireError := routes.resourceManager, routes.send, routes.requireError
	const path = dynamicPoolsPath

	code, created := send(http.MethodPost, path,
		`{"idempotency_key":"online-operation","config":{"pool_name":"online"}}`)
	require.Equal(t, http.StatusCreated, code, created)
	require.Eventually(t, func() bool {
		return resourceManager.IsDynamicResourcePoolReady("online")
	}, 10*time.Second, 20*time.Millisecond)

	code, updated := send(http.MethodPut, path+"/online?cluster_name=agent-cluster",
		`{"expected_revision":1,"config":{"pool_name":"online","description":"after"}}`)
	require.Equal(t, http.StatusOK, code, updated)
	require.EqualValues(t, 2, updated["revision"])
	require.EqualValues(t, 1, updated["active_revision"])
	require.Equal(t, true, updated["pending_restart"])
	require.Equal(t, "Ready", updated["state"])
	require.Equal(t, "after", updated["config"].(map[string]interface{})["description"])
	require.Equal(t, map[string]interface{}{"pool_name": "online", "description": "after"},
		updated["spec"])

	// The saved spec is answered without a write, even for a stale revision.
	code, same := send(http.MethodPut, path+"/online",
		`{"expected_revision":1,"config":{"description":"after","pool_name":"online"}}`)
	require.Equal(t, http.StatusOK, code, same)
	require.Equal(t, updated, same)
	code, listed := send(http.MethodGet, path, "")
	require.Equal(t, http.StatusOK, code, listed)
	require.Equal(t, []interface{}{updated}, listed["resource_pools"])

	requireError(http.StatusConflict, "current revision is 2", http.MethodPut, path+"/online",
		`{"expected_revision":1,"config":{"pool_name":"online","description":"stale"}}`)
	requireError(http.StatusBadRequest, "renaming is not supported", http.MethodPut,
		path+"/online", `{"config":{"pool_name":"renamed"}}`)
	requireError(http.StatusBadRequest, "redacted placeholder", http.MethodPut, path+"/online",
		`{"config":{"pool_name":"online","task_container_defaults":{"registry_auth":`+
			`{"username":"u","password":"********"}}}}`)
	requireError(http.StatusBadRequest, `unknown field "cluster_name"`, http.MethodPut,
		path+"/online", `{"cluster_name":"agent-cluster","config":{"pool_name":"online"}}`)
	requireError(http.StatusBadRequest, "config is required", http.MethodPut, path+"/online",
		`{"expected_revision":2}`)
	requireError(http.StatusNotFound, "not found", http.MethodPut, path+"/missing",
		`{"config":{"pool_name":"missing"}}`)
	requireError(http.StatusNotFound, `resource manager "other-cluster" not found`,
		http.MethodPut, path+"/online?cluster_name=other-cluster",
		`{"config":{"pool_name":"online"}}`)

	// A pool configured in master.yaml must be adopted before it can be updated.
	requireError(http.StatusNotFound, `"default" is configured in master.yaml; adopt it first`,
		http.MethodPut, path+"/default", `{"config":{"pool_name":"default"}}`)

	// The worker is stopped so that the following records stay as written.
	resourceManager.StopDynamicPoolWorker()
	code, created = send(http.MethodPost, path,
		`{"idempotency_key":"pending-operation","config":{"pool_name":"pending"}}`)
	require.Equal(t, http.StatusCreated, code, created)
	requireError(http.StatusConflict, "initializing", http.MethodPut, path+"/pending",
		`{"config":{"pool_name":"pending","description":"changed"}}`)

	// A saved pool that master.yaml still defines is served from master.yaml until its entry is
	// removed, so updating it is refused.
	_, _, err := pgDB.CreateDynamicResourcePool(ctx, db.DynamicResourcePool{
		ClusterName: "agent-cluster", PoolName: "default", ConfigVersion: 1,
		IdempotencyKey: "default-operation",
		Config:         json.RawMessage(`{"pool_name":"default"}`),
		ConfigHash:     "default-hash",
	})
	require.NoError(t, err)
	requireError(http.StatusConflict,
		`pool "default" is still defined in master.yaml; remove it and restart before updating`,
		http.MethodPut, path+"/default", `{"config":{"pool_name":"default"}}`)
}

func TestAdoptDynamicResourcePoolRoute(t *testing.T) {
	pgDB, cleanup := db.MustResolveNewPostgresDatabase(t)
	defer cleanup()
	db.MustMigrateTestPostgres(t, pgDB, "file://../static/migrations", "up")

	var static config.ResourcePoolConfig
	require.NoError(t, json.Unmarshal(
		[]byte(`{"pool_name":"static-gpu","agent_reconnect_wait":"10m"}`), &static,
	))
	routes := newDynamicPoolRouteTest(t, pgDB, testDynamicPoolRouteRMConfig(static))
	send, requireError := routes.send, routes.requireError
	const path = dynamicPoolsPath

	code, adopted := send(http.MethodPost, path+"/static-gpu/adopt?cluster_name=agent-cluster",
		`{"config":{"pool_name":"static-gpu","agent_reconnect_wait":"10m"}}`)
	require.Equal(t, http.StatusCreated, code, adopted)
	require.Equal(t, "Ready", adopted["state"])
	require.EqualValues(t, 1, adopted["revision"])
	require.Nil(t, adopted["active_revision"])
	require.Equal(t, true, adopted["defined_in_master_yaml"])
	require.Equal(t, true, adopted["pending_restart"])
	require.Equal(t, map[string]interface{}{
		"pool_name": "static-gpu", "agent_reconnect_wait": "10m",
	}, adopted["spec"])
	require.Equal(t, "10m0s", adopted["config"].(map[string]interface{})["agent_reconnect_wait"])

	code, replayed := send(http.MethodPost, path+"/static-gpu/adopt",
		`{"config":{"agent_reconnect_wait":"10m","pool_name":"static-gpu"}}`)
	require.Equal(t, http.StatusOK, code, replayed)
	require.Equal(t, adopted, replayed)
	code, listed := send(http.MethodGet, path, "")
	require.Equal(t, http.StatusOK, code, listed)
	require.Equal(t, []interface{}{adopted}, listed["resource_pools"])

	requireError(http.StatusBadRequest, "copy the master.yaml entry verbatim, including "+
		"agent_reconnect_wait", http.MethodPost, path+"/static-gpu/adopt",
		`{"config":{"pool_name":"static-gpu"}}`)
	requireError(http.StatusConflict, "idempotency key already names a different desired config",
		http.MethodPost, path+"/static-gpu/adopt",
		`{"config":{"pool_name":"static-gpu","agent_reconnect_wait":"600s"}}`)
	requireError(http.StatusBadRequest, `unknown field "idempotency_key"`, http.MethodPost,
		path+"/static-gpu/adopt", `{"idempotency_key":"x","config":{"pool_name":"static-gpu"}}`)
	requireError(http.StatusNotFound,
		`"missing" is not configured in master.yaml for resource manager "agent-cluster"`,
		http.MethodPost, path+"/missing/adopt", `{"config":{"pool_name":"missing"}}`)
	requireError(http.StatusNotFound, `resource manager "other-cluster" not found`,
		http.MethodPost, path+"/static-gpu/adopt?cluster_name=other-cluster",
		`{"config":{"pool_name":"static-gpu","agent_reconnect_wait":"10m"}}`)
	requireError(http.StatusConflict,
		`pool "static-gpu" is still defined in master.yaml; remove it and restart before updating`,
		http.MethodPut, path+"/static-gpu",
		`{"config":{"pool_name":"static-gpu","agent_reconnect_wait":"10m"}}`)

	// A pool that was created as a dynamic pool has no master.yaml entry to adopt.
	code, created := send(http.MethodPost, path,
		`{"idempotency_key":"online-operation","config":{"pool_name":"online"}}`)
	require.Equal(t, http.StatusCreated, code, created)
	requireError(http.StatusNotFound, `"online" is not configured in master.yaml`,
		http.MethodPost, path+"/online/adopt", `{"config":{"pool_name":"online"}}`)
	requireError(http.StatusBadRequest, `prefix "adopt:" is reserved for adopted pools`,
		http.MethodPost, path,
		`{"idempotency_key":"adopt:created","config":{"pool_name":"created"}}`)
}
