//go:build integration
// +build integration

package internal

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	detContext "github.com/determined-ai/determined/master/internal/context"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/grpcutil"
	"github.com/determined-ai/determined/master/internal/user"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/userv1"
)

// addPasswordUser creates an active, non-admin user with the given plaintext password.
func addPasswordUser(t *testing.T, password string) model.User {
	u := model.User{Username: uuid.New().String(), Active: true}
	require.NoError(t, u.UpdatePasswordHash(user.ReplicateClientSideSaltAndHash(password)))
	id, err := user.Add(context.Background(), &u, nil)
	require.NoError(t, err)
	u.ID = id
	return u
}

// sessionContext logs u in and returns a context that carries the new session token.
func sessionContext(t *testing.T, u model.User) context.Context {
	token, err := user.StartSession(context.Background(), &u)
	require.NoError(t, err)
	return metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("x-user-token", fmt.Sprintf("Bearer %s", token)))
}

func requireCode(t *testing.T, code codes.Code, err error) {
	t.Helper()
	require.Error(t, err)
	require.Equal(t, code, status.Code(err), err.Error())
}

func requireLogin(t *testing.T, api *apiServer, username, password string, ok bool) {
	t.Helper()
	_, err := api.Login(context.Background(), &apiv1.LoginRequest{
		Username: username, Password: password,
	})
	if ok {
		require.NoError(t, err)
	} else {
		require.Error(t, err)
	}
}

func TestSetUserPasswordRequiresCurrentPasswordForSelf(t *testing.T) {
	api, admin, adminCtx := setupAPITest(t, nil)
	const oldPassword = "Old-password-1"
	u := addPasswordUser(t, oldPassword)
	req := func(old *string, newPassword string) *apiv1.SetUserPasswordRequest {
		return &apiv1.SetUserPasswordRequest{
			UserId: int32(u.ID), Password: newPassword, OldPassword: old,
		}
	}

	ctx := sessionContext(t, u)
	_, err := api.SetUserPassword(ctx, req(nil, "New-password-1"))
	requireCode(t, codes.InvalidArgument, err)
	_, err = api.SetUserPassword(ctx, req(ptrs.Ptr("wrong"), "New-password-1"))
	requireCode(t, codes.PermissionDenied, err)
	// A blank current password is a wrong password for a user who has one.
	_, err = api.SetUserPassword(ctx, req(ptrs.Ptr(""), "New-password-1"))
	requireCode(t, codes.PermissionDenied, err)
	requireLogin(t, api, u.Username, oldPassword, true)

	_, err = api.SetUserPassword(ctx, req(ptrs.Ptr(oldPassword), "New-password-1"))
	require.NoError(t, err)
	requireLogin(t, api, u.Username, oldPassword, false)
	requireLogin(t, api, u.Username, "New-password-1", true)

	// An administrator needs no current password to change another user's password.
	_, err = api.SetUserPassword(adminCtx, req(nil, "New-password-2"))
	require.NoError(t, err)
	requireLogin(t, api, u.Username, "New-password-2", true)

	// Administrators changing their own password need it like everyone else. The test
	// administrator has no password, so an empty string is its current password.
	adminReq := &apiv1.SetUserPasswordRequest{UserId: int32(admin.ID), Password: "Admin-password-1"}
	_, err = api.SetUserPassword(adminCtx, adminReq)
	requireCode(t, codes.InvalidArgument, err)
	adminReq.OldPassword = ptrs.Ptr("not-blank")
	_, err = api.SetUserPassword(adminCtx, adminReq)
	requireCode(t, codes.PermissionDenied, err)
	adminReq.OldPassword = ptrs.Ptr("")
	_, err = api.SetUserPassword(adminCtx, adminReq)
	require.NoError(t, err)
	requireLogin(t, api, admin.Username, "Admin-password-1", true)
}

func TestPatchUserRequiresCurrentPasswordForSelf(t *testing.T) {
	api, _, adminCtx := setupAPITest(t, nil)
	const oldPassword = "Old-password-1"
	u := addPasswordUser(t, oldPassword)
	patch := func(newPassword string, old *string, hashed bool) *apiv1.PatchUserRequest {
		if hashed {
			newPassword = user.ReplicateClientSideSaltAndHash(newPassword)
		}
		return &apiv1.PatchUserRequest{UserId: int32(u.ID), User: &userv1.PatchUser{
			Password: ptrs.Ptr(newPassword), OldPassword: old, IsHashed: hashed,
		}}
	}
	hash := user.ReplicateClientSideSaltAndHash

	ctx := sessionContext(t, u)
	_, err := api.PatchUser(ctx, patch("New-password-1", nil, true))
	requireCode(t, codes.InvalidArgument, err)
	_, err = api.PatchUser(ctx, patch("New-password-1", ptrs.Ptr(hash("wrong")), true))
	requireCode(t, codes.PermissionDenied, err)
	// The current password is hashed like the new one: a plaintext one with is_hashed is wrong.
	_, err = api.PatchUser(ctx, patch("New-password-1", ptrs.Ptr(oldPassword), true))
	requireCode(t, codes.PermissionDenied, err)
	requireLogin(t, api, u.Username, oldPassword, true)

	_, err = api.PatchUser(ctx, patch("New-password-1", ptrs.Ptr(hash(oldPassword)), true))
	require.NoError(t, err)
	requireLogin(t, api, u.Username, "New-password-1", true)

	// Changing the password ends the user's sessions, so sign in again.
	ctx = sessionContext(t, u)
	_, err = api.PatchUser(ctx, patch("New-password-2", ptrs.Ptr("New-password-1"), false))
	require.NoError(t, err)
	requireLogin(t, api, u.Username, "New-password-2", true)

	// Other fields need no current password.
	ctx = sessionContext(t, u)
	_, err = api.PatchUser(ctx, &apiv1.PatchUserRequest{UserId: int32(u.ID), User: &userv1.PatchUser{
		DisplayName: ptrs.Ptr(uuid.New().String()),
	}})
	require.NoError(t, err)

	// An administrator needs no current password to change another user's password.
	_, err = api.PatchUser(adminCtx, patch("New-password-3", nil, false))
	require.NoError(t, err)
	requireLogin(t, api, u.Username, "New-password-3", true)
}

func TestChangeOwnPasswordBlankAndRemoteUsers(t *testing.T) {
	api, _, _ := setupAPITest(t, nil)

	// A user with a blank password sends an empty current password.
	blank := model.User{Username: uuid.New().String(), Active: true}
	id, err := user.Add(context.Background(), &blank, nil)
	require.NoError(t, err)
	blank.ID = id
	ctx := sessionContext(t, blank)
	_, err = api.PatchUser(ctx, &apiv1.PatchUserRequest{UserId: int32(id), User: &userv1.PatchUser{
		Password: ptrs.Ptr(user.ReplicateClientSideSaltAndHash("New-password-1")), IsHashed: true,
	}})
	requireCode(t, codes.InvalidArgument, err)
	_, err = api.PatchUser(ctx, &apiv1.PatchUserRequest{UserId: int32(id), User: &userv1.PatchUser{
		Password:    ptrs.Ptr(user.ReplicateClientSideSaltAndHash("New-password-1")),
		OldPassword: ptrs.Ptr(""),
		IsHashed:    true,
	}})
	require.NoError(t, err)
	requireLogin(t, api, blank.Username, "New-password-1", true)

	// Remote users sign in through SSO and cannot set a password for themselves, and neither can
	// users whose password disables password sign-in.
	for _, remoteFlag := range []bool{true, false} {
		remote := model.User{
			Username: uuid.New().String(), Active: true, Remote: remoteFlag,
			PasswordHash: model.NoPasswordLogin,
		}
		id, err = user.Add(context.Background(), &remote, nil)
		require.NoError(t, err)
		remote.ID = id
		ctx = sessionContext(t, remote)
		for _, old := range []*string{nil, ptrs.Ptr("")} {
			_, err = api.SetUserPassword(ctx, &apiv1.SetUserPasswordRequest{
				UserId: int32(id), Password: "New-password-1", OldPassword: old,
			})
			requireCode(t, codes.InvalidArgument, err)
			require.Contains(t, err.Error(), "remote users")
		}
	}
}

func TestPatchUserRenameSelfRequiresCurrentPassword(t *testing.T) {
	api, _, adminCtx := setupAPITest(t, nil)
	const password = "Old-password-1"
	u := addPasswordUser(t, password)
	rename := func(username string, old *string, hashed bool) *apiv1.PatchUserRequest {
		return &apiv1.PatchUserRequest{UserId: int32(u.ID), User: &userv1.PatchUser{
			Username: ptrs.Ptr(username), OldPassword: old, IsHashed: hashed,
		}}
	}
	ctx := sessionContext(t, u)

	// Renaming yourself locks you out of the name you sign in with, like a new password.
	newName := uuid.New().String()
	_, err := api.PatchUser(ctx, rename(newName, nil, false))
	requireCode(t, codes.InvalidArgument, err)
	require.Contains(t, err.Error(), "to change your own username")
	_, err = api.PatchUser(ctx, rename(newName, ptrs.Ptr("wrong"), false))
	requireCode(t, codes.PermissionDenied, err)
	requireLogin(t, api, u.Username, password, true)
	requireLogin(t, api, newName, password, false)

	// Sending the current username is not a rename (the web UI's user editor does that).
	_, err = api.PatchUser(ctx, rename(u.Username, nil, false))
	require.NoError(t, err)

	_, err = api.PatchUser(ctx, rename(newName, ptrs.Ptr(password), false))
	require.NoError(t, err)
	requireLogin(t, api, newName, password, true)

	// The current password is hashed like a password when is_hashed is set.
	newerName := uuid.New().String()
	_, err = api.PatchUser(ctx, rename(newerName, ptrs.Ptr(password), true))
	requireCode(t, codes.PermissionDenied, err)
	_, err = api.PatchUser(ctx, rename(newerName,
		ptrs.Ptr(user.ReplicateClientSideSaltAndHash(password)), true))
	require.NoError(t, err)
	requireLogin(t, api, newerName, password, true)

	// An administrator needs no current password to rename another user.
	adminName := uuid.New().String()
	_, err = api.PatchUser(adminCtx, rename(adminName, nil, false))
	require.NoError(t, err)
	requireLogin(t, api, adminName, password, true)
}

// TestAllocationTokenNeedsCurrentPassword checks that the session token of a task, which its
// container holds and any code running in it can read, does not let that code change the password
// or the username of the task's owner without the owner's current password, although it
// authenticates as the owner.
func TestAllocationTokenNeedsCurrentPassword(t *testing.T) {
	api, _, _ := setupAPITest(t, nil)
	srv := browserSessionServer(t, api)
	const password = "Old-password-1"
	u := addPasswordUser(t, password)
	ctx := context.Background()

	// A running task of u's, and the token that its container authenticates with.
	task := db.RequireMockTask(t, api.m.db, &u.ID)
	allocationID := model.AllocationID(fmt.Sprintf("%s.1", task.TaskID))
	require.NoError(t, db.AddAllocation(ctx, &model.Allocation{
		AllocationID: allocationID, TaskID: task.TaskID,
		StartTime: ptrs.Ptr(time.Now().UTC()), State: ptrs.Ptr(model.AllocationStateRunning),
	}))
	token, err := db.StartAllocationSession(ctx, allocationID, &u)
	require.NoError(t, err)
	taskCtx := metadata.NewIncomingContext(ctx,
		metadata.Pairs("x-allocation-token", "Bearer "+token))
	requireTaskToken := func(username string) {
		t.Helper()
		curUser, session, err := grpcutil.GetUser(taskCtx)
		require.NoError(t, err)
		require.Nil(t, session, "a user session")
		require.Equal(t, u.ID, curUser.ID)
		require.Equal(t, username, curUser.Username)
	}
	requireTaskToken(u.Username)

	id := int32(u.ID) //nolint:gosec // The IDs of test users are small.
	newName := uuid.New().String()
	setPassword := func(old *string) error {
		_, err := api.SetUserPassword(taskCtx, &apiv1.SetUserPasswordRequest{
			UserId: id, Password: "New-password-1", OldPassword: old,
		})
		return err
	}
	patchPassword := func(old *string) error {
		_, err := api.PatchUser(taskCtx, &apiv1.PatchUserRequest{
			UserId: id, User: &userv1.PatchUser{
				Password: ptrs.Ptr("New-password-1"), OldPassword: old,
			},
		})
		return err
	}
	rename := func(old *string) error {
		_, err := api.PatchUser(taskCtx, &apiv1.PatchUserRequest{
			UserId: id, User: &userv1.PatchUser{Username: ptrs.Ptr(newName), OldPassword: old},
		})
		return err
	}
	for name, change := range map[string]func(*string) error{
		"SetUserPassword": setPassword, "PatchUser password": patchPassword, "PatchUser username": rename,
	} {
		err := change(nil)
		require.Equal(t, codes.InvalidArgument, status.Code(err), "%s: %v", name, err)
		for _, wrong := range []string{"wrong", ""} {
			err = change(ptrs.Ptr(wrong))
			require.Equal(t, codes.PermissionDenied, status.Code(err), "%s %q: %v", name, wrong, err)
		}
	}

	// The same through the gateway, with the header that the harness sends the token in.
	send := func(method, path, body string) (int, string) {
		req, err := http.NewRequestWithContext(ctx, method, srv.URL+path, strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", jsonContentType)
		req.Header.Set("Grpc-Metadata-x-allocation-token", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { require.NoError(t, resp.Body.Close()) }()
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, string(b)
	}
	passwordPath := fmt.Sprintf("/api/v1/users/%d/password", u.ID)
	userPath := fmt.Sprintf("/api/v1/users/%d", u.ID)
	for _, req := range []struct{ method, path, change string }{
		{http.MethodPost, passwordPath, `"password": "New-password-1"`},
		{http.MethodPatch, userPath, `"password": "New-password-1"`},
		{http.MethodPatch, userPath, fmt.Sprintf(`"username": %q`, newName)},
	} {
		code, body := send(req.method, req.path, "{"+req.change+"}")
		require.Equal(t, http.StatusBadRequest, code, "%v: %s", req, body)
		require.Contains(t, body, "enter your current password", req)
		code, body = send(req.method, req.path, "{"+req.change+`, "old_password": "wrong"}`)
		require.Equal(t, http.StatusForbidden, code, "%v: %s", req, body)
	}

	// Nothing changed.
	requireLogin(t, api, u.Username, password, true)
	requireLogin(t, api, u.Username, "New-password-1", false)
	requireLogin(t, api, newName, password, false)
	stored, err := user.ByID(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, u.Username, stored.Username)
	requireTaskToken(u.Username)

	// With the current password, the task can make these changes, as its owner could. A new
	// password ends the owner's sessions, but not the sessions of their tasks, which keep running.
	userCtx := sessionContext(t, u)
	require.NoError(t, setPassword(ptrs.Ptr(password)))
	requireLogin(t, api, u.Username, "New-password-1", true)
	_, _, err = grpcutil.GetUser(userCtx)
	requireCode(t, codes.Unauthenticated, err)
	requireTaskToken(u.Username)
	require.NoError(t, rename(ptrs.Ptr("New-password-1")))
	requireLogin(t, api, newName, "New-password-1", true)
	requireTaskToken(newName)
}

// requireProxySignIn checks that the proxied services send a browser whose session cookie holds
// token to sign in again.
func requireProxySignIn(t *testing.T, token string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://gpu.example/proxy/abc/", nil)
	req.AddCookie(&http.Cookie{Name: user.SessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.SetParamNames("service")
	c.SetParamValues("abc")
	done, err := processProxyAuthentication(&detContext.DetContext{Context: c})
	require.True(t, done)
	require.NoError(t, err)
	require.Equal(t, http.StatusSeeOther, rec.Code)
	require.True(t, strings.HasPrefix(rec.Header().Get("Location"), "/det/login?redirect="),
		rec.Header().Get("Location"))
}

// TestExpiredRemoteSessionIsUnauthenticated checks that an expired session of a remote (SSO) user,
// which ByToken reports apart from other ended sessions, is refused as unauthenticated everywhere.
func TestExpiredRemoteSessionIsUnauthenticated(t *testing.T) {
	api, _, _ := setupAPITest(t, nil)
	srv := browserSessionServer(t, api)
	u := addPasswordUser(t, "Old-password-1")
	ctx := context.Background()
	_, err := db.Bun().NewUpdate().Table("users").Set("remote = true").
		Where("id = ?", u.ID).Exec(ctx)
	require.NoError(t, err)
	token, err := user.StartSession(ctx, &u)
	require.NoError(t, err)
	_, err = db.Bun().NewUpdate().Table("user_sessions").
		Set("expiry = ?", time.Now().UTC().Add(-time.Hour)).
		Where("user_id = ?", u.ID).Exec(ctx)
	require.NoError(t, err)
	_, _, err = user.ByToken(ctx, token, &model.ExternalSessions{})
	require.ErrorIs(t, err, user.ErrRemoteUserTokenExpired)

	for _, r := range []browserRequest{
		{method: http.MethodGet, path: "/users/me", authorization: "Bearer " + token},
		{method: http.MethodGet, path: "/users/me", cookie: token},
		{method: http.MethodGet, path: "/api/v1/me", cookie: token},
	} {
		resp, body := r.send(t, srv)
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "%s %s", r.path, body)
	}
	requireProxySignIn(t, token)
}

func TestPasswordChangeRevokesAccessTokens(t *testing.T) {
	api, _, adminCtx := setupAPITest(t, nil)
	srv := browserSessionServer(t, api)
	u := addPasswordUser(t, "Old-password-1")
	newToken := func() string {
		resp, err := api.PostAccessToken(adminCtx, &apiv1.PostAccessTokenRequest{UserId: int32(u.ID)})
		require.NoError(t, err)
		return resp.Token
	}
	requireValid := func(token string, valid bool) {
		t.Helper()
		_, _, err := user.ByToken(context.Background(), token, &model.ExternalSessions{})
		if valid {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, user.ErrAccessTokenRevoked)
		}

		// The legacy routes refuse a revoked token as unauthenticated, like the gateway, whether
		// it comes in the Authorization header or the session cookie.
		want := http.StatusOK
		if !valid {
			want = http.StatusUnauthorized
		}
		for _, r := range []browserRequest{
			{method: http.MethodGet, path: "/users/me", authorization: "Bearer " + token},
			{method: http.MethodGet, path: "/users/me", cookie: token},
			{method: http.MethodGet, path: "/api/v1/me", authorization: "Bearer " + token},
		} {
			resp, body := r.send(t, srv)
			require.Equal(t, want, resp.StatusCode, "%s %s", r.path, body)
		}
		if !valid {
			requireProxySignIn(t, token)
		}
	}

	// Changes other than the password leave access tokens alone.
	token := newToken()
	_, err := api.PatchUser(adminCtx, &apiv1.PatchUserRequest{UserId: int32(u.ID), User: &userv1.PatchUser{
		DisplayName: ptrs.Ptr(uuid.New().String()),
	}})
	require.NoError(t, err)
	_, err = api.PatchUser(adminCtx, &apiv1.PatchUserRequest{UserId: int32(u.ID), User: &userv1.PatchUser{
		Username: ptrs.Ptr(uuid.New().String()),
	}})
	require.NoError(t, err)
	requireValid(token, true)

	// A new password revokes them, however it is set: a stolen session may have created them, and
	// changing the password is how users take their account back.
	_, err = api.SetUserPassword(adminCtx, &apiv1.SetUserPasswordRequest{
		UserId: int32(u.ID), Password: "New-password-1",
	})
	require.NoError(t, err)
	requireValid(token, false)

	token = newToken()
	_, err = api.PatchUser(sessionContext(t, u), &apiv1.PatchUserRequest{
		UserId: int32(u.ID), User: &userv1.PatchUser{
			Password: ptrs.Ptr("New-password-2"), OldPassword: ptrs.Ptr("New-password-1"),
		},
	})
	require.NoError(t, err)
	requireValid(token, false)

	// Deactivating the user still revokes them.
	_, err = api.PatchUser(adminCtx, &apiv1.PatchUserRequest{UserId: int32(u.ID), User: &userv1.PatchUser{
		Active: wrapperspb.Bool(true),
	}})
	require.NoError(t, err)
	token = newToken()
	_, err = api.PatchUser(adminCtx, &apiv1.PatchUserRequest{UserId: int32(u.ID), User: &userv1.PatchUser{
		Active: wrapperspb.Bool(false),
	}})
	require.NoError(t, err)
	requireValid(token, false)
}
