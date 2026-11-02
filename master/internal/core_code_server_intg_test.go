//go:build integration
// +build integration

package internal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/api"
	"github.com/determined-ai/determined/master/internal/config"
	detContext "github.com/determined-ai/determined/master/internal/context"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/user"
)

// codeServerAuthServer serves the code-server route behind the middleware of Master.Run.
func codeServerAuthServer(t *testing.T, apiSrv *apiServer, conf config.CodeServerConfig) *httptest.Server {
	user.InitService(apiSrv.m.db, &apiSrv.m.config.InternalConfig.ExternalSessions)
	e := echo.New()
	e.HTTPErrorHandler = api.JSONErrorHandler
	e.Use(func(h echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error { return h(&detContext.DetContext{Context: c}) }
	})
	useAuthenticationMiddleware(e, nil)
	require.NoError(t, registerCodeServerRoutes(e, conf))
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	return srv
}

func TestCodeServerRouteAuthentication(t *testing.T) {
	apiSrv, _, _ := setupAPITest(t, nil)
	ctx := context.Background()

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		_, _ = w.Write(testCodeServerContent("amd64"))
	}))
	t.Cleanup(upstream.Close)
	conf := config.CodeServerConfig{
		Version:     testCodeServerVersion,
		Dir:         t.TempDir(),
		URLTemplate: upstream.URL + "/{version}/{arch}",
	}
	srv := codeServerAuthServer(t, apiSrv, conf)
	path := "/det/code-server/4.144.0/" + testCodeServerAMD64

	// get returns the status of a GET request with the given header names and values.
	get := func(srv *httptest.Server, path string, header ...string) int {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
		require.NoError(t, err)
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		return resp.StatusCode
	}

	// The middleware exempts /det/ for the WebUI; the route still refuses requests without a
	// session, also for paths that it does not serve.
	for _, p := range []string{path, path + ".sha256", "/det/code-server/4.143.0/x"} {
		require.Equal(t, http.StatusUnauthorized, get(srv, p), p)
		require.Equal(t, http.StatusUnauthorized, get(srv, p, "Authorization", "Bearer not-a-token"), p)
	}
	require.Equal(t, int32(0), upstreamCalls.Load())

	// A task's DET_USER_TOKEN is a session of its owner, which passes. Its DET_SESSION_TOKEN
	// authenticates the allocation to the gRPC API only.
	owner := db.RequireMockUser(t, apiSrv.m.db)
	userToken, err := getTaskSessionToken(ctx, &owner)
	require.NoError(t, err)
	task := db.RequireMockTask(t, apiSrv.m.db, &owner.ID)
	allocation := db.RequireMockAllocation(t, apiSrv.m.db, task.TaskID)
	allocationToken, err := db.StartAllocationSession(ctx, allocation.AllocationID, &owner)
	require.NoError(t, err)

	require.Equal(t, http.StatusUnauthorized, get(srv, path, "Authorization", "Bearer "+allocationToken))
	require.Equal(t, int32(0), upstreamCalls.Load())

	require.Equal(t, http.StatusOK, get(srv, path, "Authorization", "Bearer "+userToken))
	require.Equal(t, int32(1), upstreamCalls.Load())
	require.Equal(t, http.StatusNotFound,
		get(srv, "/det/code-server/4.143.0/x", "Authorization", "Bearer "+userToken))

	// The browser's session cookie passes too.
	require.Equal(t, http.StatusOK,
		get(srv, path+".sha256", "Cookie", user.SessionCookieName+"="+userToken))
	_, err = os.Stat(filepath.Join(conf.Dir, testCodeServerAMD64+".sha256"))
	require.NoError(t, err)

	// Without the section, authenticated requests get 404 and anonymous ones 401.
	disabled := codeServerAuthServer(t, apiSrv, config.DefaultCodeServerConfig())
	require.Equal(t, http.StatusUnauthorized, get(disabled, path))
	require.Equal(t, http.StatusNotFound, get(disabled, path, "Authorization", "Bearer "+userToken))
}
