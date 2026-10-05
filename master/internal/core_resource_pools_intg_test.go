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

func TestUpdateDynamicResourcePoolRoute(t *testing.T) {
	pgDB, cleanup := db.MustResolveNewPostgresDatabase(t)
	defer cleanup()
	db.MustMigrateTestPostgres(t, pgDB, "file://../static/migrations", "up")
	ctx := context.Background()

	originalUser := dynamicPoolRequestUser
	originalAuthorize := authorizeDynamicPoolRequest
	t.Cleanup(func() {
		dynamicPoolRequestUser = originalUser
		authorizeDynamicPoolRequest = originalAuthorize
	})
	dynamicPoolRequestUser = func(*http.Request) (*model.User, *model.UserSession, error) {
		return &model.User{Active: true, Admin: true}, &model.UserSession{}, nil
	}
	authorizeDynamicPoolRequest = func(*http.Request, *model.User, bool) (error, error) {
		return nil, nil
	}

	rmConfig := &config.ResourceManagerWithPoolsConfig{
		ResourceManager: &config.ResourceManagerConfig{AgentRM: &config.AgentResourceManagerConfig{
			ClusterName:                "agent-cluster",
			DefaultComputeResourcePool: "default",
			DefaultAuxResourcePool:     "default",
			Scheduler:                  config.DefaultSchedulerConfig(),
		}},
		ResourcePools: []config.ResourcePoolConfig{{
			PoolName: "default", MaxAuxContainersPerAgent: 100,
		}},
	}
	masterDefaults := *model.DefaultTaskContainerDefaults()
	resourceManager, err := agentrm.New(
		ctx, pgDB, echo.New(), rmConfig, nil, nil, &masterDefaults,
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

	send := func(method, path, body string) (int, map[string]interface{}) {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		recorder := httptest.NewRecorder()
		e.ServeHTTP(recorder, request)
		var response map[string]interface{}
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response), recorder.Body.String())
		return recorder.Code, response
	}
	const path = "/api/v1/resource-pools/dynamic"
	requireError := func(code int, message string, method, path, body string) {
		t.Helper()
		gotCode, response := send(method, path, body)
		require.Equal(t, code, gotCode, response)
		require.Contains(t, response["message"], message)
	}

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
	_, _, err = pgDB.CreateDynamicResourcePool(ctx, db.DynamicResourcePool{
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
