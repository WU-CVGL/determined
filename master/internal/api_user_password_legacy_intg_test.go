//go:build integration
// +build integration

package internal

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/user"
	"github.com/determined-ai/determined/master/pkg/model"
)

// Until 0.41.0, the body of POST /api/v1/users/{id}/password was the new password alone, a JSON
// string. Clients built before then still send it: the 0.40 web UI in a tab opened before the
// upgrade, and scripts that call the 0.40 SDK's bindings.post_SetUserPassword.
func TestSetUserPasswordLegacyStringBody(t *testing.T) {
	apiSrv, admin, _ := setupAPITest(t, nil)
	srv := browserSessionServer(t, apiSrv)
	u := addPasswordUser(t, "Legacy-password-1")
	legacy := func(caller model.User, newPassword string) (browserResponse, string) {
		token, err := user.StartSession(context.Background(), &caller)
		require.NoError(t, err)
		return browserRequest{
			method: http.MethodPost, path: fmt.Sprintf("/api/v1/users/%d/password", u.ID),
			contentType: jsonContentType, authorization: "Bearer " + token,
			body: fmt.Sprintf("%q", newPassword),
		}.send(t, srv)
	}

	// An administrator changes another user's password, as 0.40 scripts do.
	resp, body := legacy(admin, "Legacy-password-2")
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	requireLogin(t, apiSrv, u.Username, "Legacy-password-2", true)

	// Users who change their own password must now send their current one, which a 0.40 client
	// cannot; the master says so instead of failing to parse the body.
	resp, body = legacy(u, "Legacy-password-3")
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, body)
	require.Contains(t, body, "enter your current password")
	require.NotContains(t, body, "cannot unmarshal")
	requireLogin(t, apiSrv, u.Username, "Legacy-password-2", true)
}
