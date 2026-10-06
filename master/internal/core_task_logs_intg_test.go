//go:build integration
// +build integration

package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	apiPkg "github.com/determined-ai/determined/master/internal/api"
	authz2 "github.com/determined-ai/determined/master/internal/authz"
	detContext "github.com/determined-ai/determined/master/internal/context"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/user"
	"github.com/determined-ai/determined/master/pkg/model"
)

// taskLogsServer serves the legacy POST /task-logs route behind the middleware that Master.Run
// installs: the extended context and useAuthenticationMiddleware.
func taskLogsServer(t *testing.T, apiSrv *apiServer) *httptest.Server {
	user.InitService(apiSrv.m.db, &apiSrv.m.config.InternalConfig.ExternalSessions)

	e := echo.New()
	e.HTTPErrorHandler = apiPkg.JSONErrorHandler
	e.Use(func(h echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error { return h(&detContext.DetContext{Context: c}) }
	})
	useAuthenticationMiddleware(e, nil)
	e.POST("/task-logs", apiPkg.Route(apiSrv.m.postTaskLogs))

	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	return srv
}

// shippedLogs is a batch as the harness's unmanaged log shipper (_LogSender) sends it: the same
// task_id, timestamp and rank on every line, and no id.
func shippedLogs(t *testing.T, taskID model.TaskID, lines ...string) []map[string]any {
	t.Helper()
	ts := time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00")
	logs := make([]map[string]any, 0, len(lines))
	for _, l := range lines {
		logs = append(logs, map[string]any{
			"task_id": string(taskID), "timestamp": ts, "rank": "0", "log": l + "\n",
		})
	}
	return logs
}

func jsonBody(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

// taskLogRows counts the task's rows in the task_logs table.
func taskLogRows(t *testing.T, taskID model.TaskID) int {
	t.Helper()
	n, err := db.Bun().NewSelect().Table("task_logs").Where("task_id = ?", taskID).
		Count(context.Background())
	require.NoError(t, err)
	return n
}

func sessionToken(t *testing.T, u model.User) string {
	t.Helper()
	token, err := user.StartSession(context.Background(), &u)
	require.NoError(t, err)
	return token
}

// postTaskLogsAs posts body to /task-logs with token as a bearer token, or with no credentials
// when token is empty, and returns the status code.
func postTaskLogsAs(t *testing.T, srv *httptest.Server, token, body string) int {
	t.Helper()
	r := browserRequest{
		method: http.MethodPost, path: "/task-logs", contentType: jsonContentType, body: body,
	}
	if token != "" {
		r.authorization = "Bearer " + token
	}
	resp, msg := r.send(t, srv)
	t.Logf("POST /task-logs: %d %s", resp.StatusCode, msg)
	return resp.StatusCode
}

func TestPostTaskLogsRouteNeedsSession(t *testing.T) {
	api, admin, _ := setupAPITest(t, nil)
	srv := taskLogsServer(t, api)
	_, task := createTestTrial(t, api, admin)
	body := jsonBody(t, shippedLogs(t, task.TaskID, "forged"))

	// No credentials, a token the master never issued, and a session cookie that is not one.
	require.Equal(t, http.StatusUnauthorized, postTaskLogsAs(t, srv, "", body))
	require.Equal(t, http.StatusUnauthorized, postTaskLogsAs(t, srv, "not-a-token", body))
	resp, _ := browserRequest{
		method: http.MethodPost, path: "/task-logs", contentType: jsonContentType, body: body,
		cookie: "not-a-token", fetchSite: sameOrigin,
	}.send(t, srv)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.Zero(t, taskLogRows(t, task.TaskID))

	// A signed-in user who may edit the task gets through.
	require.Equal(t, http.StatusOK, postTaskLogsAs(t, srv, sessionToken(t, admin), body))
	require.Equal(t, 1, taskLogRows(t, task.TaskID))
}

func TestPostTaskLogsRouteChecksTask(t *testing.T) {
	api, admin, _ := setupAPITest(t, nil) // Basic authorization.
	srv := taskLogsServer(t, api)
	owner := addPasswordUser(t, "Owner-password-1")
	other := addPasswordUser(t, "Other-password-1")
	ownerToken := sessionToken(t, owner)
	otherToken := sessionToken(t, other)
	adminToken := sessionToken(t, admin)

	_, task := createTestTrial(t, api, owner)
	_, task2 := createTestTrial(t, api, owner)
	_, adminTask := createTestTrial(t, api, admin)
	logs := shippedLogs(t, task.TaskID, "one", "two")

	// Another non-admin user can see the trial but not edit it.
	require.Equal(t, http.StatusForbidden, postTaskLogsAs(t, srv, otherToken, jsonBody(t, logs)))
	require.Zero(t, taskLogRows(t, task.TaskID))
	// Neither can the owner of one trial write to another user's.
	require.Equal(t, http.StatusForbidden, postTaskLogsAs(t, srv, ownerToken,
		jsonBody(t, shippedLogs(t, adminTask.TaskID, "forged"))))
	require.Zero(t, taskLogRows(t, adminTask.TaskID))

	// A task that does not exist.
	missing := model.TaskID(uuid.New().String())
	require.Equal(t, http.StatusNotFound, postTaskLogsAs(t, srv, ownerToken,
		jsonBody(t, shippedLogs(t, missing, "lost"))))
	require.Zero(t, taskLogRows(t, missing))

	// Batches that PostTaskLogs refuses are refused whole: logs of several tasks, in either
	// order, logs with an ID, null logs, an empty batch, and a body that is not a batch.
	mixed := append(shippedLogs(t, task.TaskID, "mine"), shippedLogs(t, task2.TaskID, "mine too")...)
	require.Equal(t, http.StatusBadRequest, postTaskLogsAs(t, srv, ownerToken, jsonBody(t, mixed)))
	mixed = append(shippedLogs(t, task.TaskID, "mine"), shippedLogs(t, adminTask.TaskID, "not")...)
	require.Equal(t, http.StatusBadRequest, postTaskLogsAs(t, srv, ownerToken, jsonBody(t, mixed)))
	mixed = append(shippedLogs(t, adminTask.TaskID, "not"), shippedLogs(t, task.TaskID, "mine")...)
	require.Equal(t, http.StatusForbidden, postTaskLogsAs(t, srv, ownerToken, jsonBody(t, mixed)))
	withID := shippedLogs(t, task.TaskID, "numbered")
	withID[0]["id"] = 1
	for _, body := range []string{
		jsonBody(t, withID), fmt.Sprintf(`[%s, null]`, jsonBody(t, logs[0])),
		`[]`, `null`, `{"task_id": "x"}`, `[`,
	} {
		require.Equal(t, http.StatusBadRequest, postTaskLogsAs(t, srv, ownerToken, body), body)
	}
	require.Zero(t, taskLogRows(t, task.TaskID))
	require.Zero(t, taskLogRows(t, task2.TaskID))
	require.Zero(t, taskLogRows(t, adminTask.TaskID))

	// The owner and an administrator may write.
	require.Equal(t, http.StatusOK, postTaskLogsAs(t, srv, ownerToken, jsonBody(t, logs)))
	require.Equal(t, 2, taskLogRows(t, task.TaskID))
	require.Equal(t, http.StatusOK, postTaskLogsAs(t, srv, adminToken, jsonBody(t, logs)))
	require.Equal(t, 4, taskLogRows(t, task.TaskID))

	// The session cookie works too, but only from the master's own pages.
	cookiePost := browserRequest{
		method: http.MethodPost, path: "/task-logs", contentType: jsonContentType,
		body: jsonBody(t, logs), cookie: ownerToken,
	}
	cookiePost.fetchSite = "cross-site"
	resp, _ := cookiePost.send(t, srv)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Equal(t, 4, taskLogRows(t, task.TaskID))
	cookiePost.fetchSite = sameOrigin
	resp, _ = cookiePost.send(t, srv)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, 6, taskLogRows(t, task.TaskID))
}

func TestPostTaskLogsRouteAuthZ(t *testing.T) {
	api, authZExp, _, curUser, _ := setupExpAuthTest(t, nil)
	srv := taskLogsServer(t, api)
	token := sessionToken(t, curUser)
	mockUserArg := mock.MatchedBy(func(u model.User) bool { return u.ID == curUser.ID })

	_, task := createTestTrial(t, api, curUser)
	body := jsonBody(t, shippedLogs(t, task.TaskID, "line"))

	// A user who cannot view the trial's experiment is told that the task does not exist.
	authZExp.On("CanGetExperiment", mock.Anything, mockUserArg, mock.Anything).
		Return(authz2.PermissionDeniedError{}).Once()
	require.Equal(t, http.StatusNotFound, postTaskLogsAs(t, srv, token, body))
	require.Zero(t, taskLogRows(t, task.TaskID))

	// A user who can view it but not edit it is forbidden.
	authZExp.On("CanGetExperiment", mock.Anything, mockUserArg, mock.Anything).Return(nil).Once()
	authZExp.On("CanEditExperiment", mock.Anything, mockUserArg, mock.Anything).
		Return(authz2.PermissionDeniedError{}).Once()
	require.Equal(t, http.StatusForbidden, postTaskLogsAs(t, srv, token, body))
	require.Zero(t, taskLogRows(t, task.TaskID))

	authZExp.On("CanGetExperiment", mock.Anything, mockUserArg, mock.Anything).Return(nil).Once()
	authZExp.On("CanEditExperiment", mock.Anything, mockUserArg, mock.Anything).Return(nil).Once()
	require.Equal(t, http.StatusOK, postTaskLogsAs(t, srv, token, body))
	require.Equal(t, 1, taskLogRows(t, task.TaskID))
	authZExp.AssertExpectations(t)
}
