//go:build integration
// +build integration

package user

import (
	stdContext "context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/pkg/errors"

	"github.com/determined-ai/determined/master/internal/api"
	authz2 "github.com/determined-ai/determined/master/internal/authz"
	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/context"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/master/pkg/etc"

	"github.com/determined-ai/determined/master/pkg/model"
)

// Mocks don't like being initialized more than once?
var (
	pgDB               *db.PgDB
	userAuthzSingleton *mocks.UserAuthZ = &mocks.UserAuthZ{}
	notFoundUsername                    = "usernotfound99999"
)

func init() {
	AuthZProvider.Register("mock", userAuthzSingleton)
	config.GetMasterConfig().Security.AuthZ = config.AuthZConfig{Type: "mock"}
}

func setup(t *testing.T) (*Service, func(), *mocks.UserAuthZ, echo.Context) {
	e := echo.New()
	c := e.NewContext(nil, nil)
	ctx := &context.DetContext{Context: c}
	ctx.SetUser(model.User{})

	var cl func()
	pgDB, cl = db.MustResolveTestPostgres(t)
	db.MustMigrateTestPostgres(t, pgDB, "file://../../static/migrations")
	require.NoError(t, etc.SetRootPath("../../static/srv"))

	externalSessions := &model.ExternalSessions{}
	InitService(pgDB, externalSessions)
	return GetService(), cl, userAuthzSingleton, ctx
}

func TestAddUserExec(t *testing.T) {
	_, closeDB, _, _ := setup(t) //nolint: dogsled
	defer closeDB()

	ctx := stdContext.TODO()
	u := &model.User{Username: uuid.New().String()}
	_, err := AddUserTx(ctx, db.Bun(), u)
	require.NoError(t, err)

	actual := &model.User{}
	require.NoError(t, db.Bun().NewSelect().Model(actual).Where("id = ?", u.ID).Scan(ctx))
	require.Equal(t, u.Username, actual.Username)

	actualGroup := &struct {
		bun.BaseModel `bun:"table:groups,alias:groups"`
		Name          string `bun:"group_name,notnull"  json:"name"`
	}{}
	require.NoError(t, db.Bun().NewSelect().Model(actualGroup).Where("user_id = ?", u.ID).Scan(ctx))
	require.Equal(t, fmt.Sprintf("%d%s", u.ID, PersonalGroupPostfix), actualGroup.Name)

	groupMember := &struct {
		bun.BaseModel `bun:"table:user_group_membership"`
		UserID        model.UserID `bun:"user_id,notnull"`
	}{
		UserID: u.ID,
	}
	require.NoError(t, db.Bun().NewSelect().Model(groupMember).Where("user_id = ?", u.ID).Scan(ctx))
	require.Equal(t, u.ID, groupMember.UserID)
}

func TestAuthzUserList(t *testing.T) {
	svc, closeDB, authzUser, ctx := setup(t)
	defer closeDB()

	// Error passes through.
	expectedErr := fmt.Errorf("filterUserListError")
	authzUser.On("FilterUserList", mock.Anything, model.User{}, mock.Anything).
		Return(nil, expectedErr).Once()
	_, err := svc.getUsers(ctx)
	require.Equal(t, expectedErr, err)

	// Nil error returns whatever FilterUserList returns.
	users := []model.FullUser{
		{Username: "a"},
		{Username: "b"},
	}
	authzUser.On("FilterUserList", mock.Anything, model.User{}, mock.Anything).
		Return(users, nil).Once()
	actualUsers, err := svc.getUsers(ctx)
	require.NoError(t, err)
	require.Equal(t, users, actualUsers)
}

func TestAuthzPatchUser(t *testing.T) {
	svc, closeDB, authzUser, ctx := setup(t)
	defer closeDB()
	cases := []struct {
		expectedCall string
		args         []any
		body         string
	}{
		{
			"CanSetUsersPassword",
			[]any{mock.Anything, model.User{}, mock.Anything},
			`{"password":"new"}`,
		},
		{
			"CanSetUsersActive",
			[]any{mock.Anything, model.User{}, mock.Anything, false},
			`{"active":false}`,
		},
		{
			"CanSetUsersAdmin",
			[]any{mock.Anything, model.User{}, mock.Anything, true},
			`{"admin":true}`,
		},
		{
			"CanSetUsersAgentUserGroup",
			[]any{
				mock.Anything,
				model.User{},
				mock.Anything,
				model.AgentUserGroup{GID: 3, UID: 3, User: "uname", Group: "gname"},
			},
			`{"agent_user_group":{"gid":3,"uid":3,"user":"uname","group":"gname"}}`,
		},
	}
	for _, testCase := range cases {
		// If we can view the user we get the can set function error.
		ctx.SetParamNames("username")
		ctx.SetParamValues("admin")
		ctx.SetRequest(httptest.NewRequest(http.MethodPatch, "/",
			strings.NewReader(testCase.body)))
		expectedErr := errors.Wrap(forbiddenError, testCase.expectedCall+"Error")
		authzUser.On(testCase.expectedCall, testCase.args...).
			Return(fmt.Errorf(testCase.expectedCall + "Error")).Once()
		authzUser.On("CanGetUser", mock.Anything, model.User{}, mock.Anything).
			Return(nil).Once()

		_, err := svc.patchUser(ctx)
		require.Equal(t, expectedErr.Error(), err.Error())

		// If CanGetUser returns an error we get that error.
		ctx.SetParamNames("username")
		ctx.SetParamValues("admin")
		ctx.SetRequest(httptest.NewRequest(http.MethodPatch, "/",
			strings.NewReader(testCase.body)))
		authzUser.On(testCase.expectedCall, testCase.args...).
			Return(fmt.Errorf(testCase.expectedCall + "Error")).Once()
		cantGetUserError := fmt.Errorf("cantGetUserError")
		authzUser.On("CanGetUser", mock.Anything, model.User{}, mock.Anything).
			Return(cantGetUserError).Once()

		_, err = svc.patchUser(ctx)
		require.Equal(t, cantGetUserError, err)

		// If we can't view the user we get the same error as user not being found.
		ctx.SetRequest(httptest.NewRequest(http.MethodPatch, "/",
			strings.NewReader(testCase.body)))
		authzUser.On(testCase.expectedCall, testCase.args...).
			Return(fmt.Errorf(testCase.expectedCall + "Error")).Once()
		authzUser.On("CanGetUser", mock.Anything, model.User{}, mock.Anything).
			Return(authz2.PermissionDeniedError{}).Once()

		_, err = svc.patchUser(ctx)
		require.Equal(t, api.NotFoundErrs("user", "admin", false).Error(), err.Error())

		ctx.SetParamNames("username")
		ctx.SetParamValues(notFoundUsername)
		ctx.SetRequest(httptest.NewRequest(http.MethodPatch, "/",
			strings.NewReader(testCase.body)))
		_, err = svc.patchUser(ctx)
		require.Equal(t, api.NotFoundErrs("user", notFoundUsername, false).Error(), err.Error())
	}
}

func TestAuthzPatchUsername(t *testing.T) {
	svc, closeDB, authzUser, ctx := setup(t)
	defer closeDB()

	// If we can view the user we get canSetUsersUsername error.
	ctx.SetParamNames("username")
	ctx.SetParamValues("admin")
	expectedErr := errors.Wrap(forbiddenError, "canSetUsersUsernameError")
	ctx.SetRequest(httptest.NewRequest("", "/", strings.NewReader(`{"username":"x"}`)))
	authzUser.On("CanSetUsersUsername", mock.Anything, model.User{}, mock.Anything).
		Return(fmt.Errorf("canSetUsersUsernameError")).Once()
	authzUser.On("CanGetUser", mock.Anything, model.User{}, mock.Anything).Return(nil).Once()

	_, err := svc.patchUsername(ctx)
	require.Equal(t, expectedErr.Error(), err.Error())

	// If we get an error from canGetUser we return that error.
	ctx.SetRequest(httptest.NewRequest("", "/", strings.NewReader(`{"username":"x"}`)))
	authzUser.On("CanSetUsersUsername", mock.Anything, model.User{}, mock.Anything).
		Return(fmt.Errorf("canSetUsersUsernameError")).Once()
	cantGetUserError := fmt.Errorf("cantGetUserError")
	authzUser.On("CanGetUser", mock.Anything, model.User{}, mock.Anything).
		Return(cantGetUserError).Once()

	_, err = svc.patchUsername(ctx)
	require.Equal(t, cantGetUserError, err)

	// If we can't view the user we get the same error as the user not existing.
	ctx.SetRequest(httptest.NewRequest("", "/", strings.NewReader(`{"username":"x"}`)))
	authzUser.On("CanSetUsersUsername", mock.Anything, model.User{}, mock.Anything).
		Return(fmt.Errorf("canSetUsersUsernameError")).Once()
	authzUser.On("CanGetUser", mock.Anything, model.User{}, mock.Anything).
		Return(authz2.PermissionDeniedError{}).Once()

	_, err = svc.patchUsername(ctx)
	require.Equal(t, db.ErrNotFound.Error(), err.Error())

	ctx.SetRequest(httptest.NewRequest("", "/", strings.NewReader(`{"username":"x"}`)))
	ctx.SetParamNames("username")
	ctx.SetParamValues(notFoundUsername)
	_, err = svc.patchUsername(ctx)
	require.Equal(t, db.ErrNotFound.Error(), err.Error())
}

func TestAuthzPostUser(t *testing.T) {
	svc, closeDB, authzUser, ctx := setup(t)
	defer closeDB()
	ctx.SetParamNames("username")
	ctx.SetParamValues("admin")
	ctx.SetRequest(httptest.NewRequest("", "/", strings.NewReader(
		`{"username":"x","agent_user_group":{"uid":1,"gid":2,"user":"u","group":"g"}}`)))

	agentGroup := &model.AgentUserGroup{
		UID:   1,
		GID:   2,
		User:  "u",
		Group: "g",
	}
	expectedErr := errors.Wrap(forbiddenError, "canCreateUserError")
	authzUser.On("CanCreateUser", mock.Anything, model.User{}, model.User{Username: "x"}, agentGroup).
		Return(fmt.Errorf("canCreateUserError")).Once()

	_, err := svc.postUser(ctx)
	require.Equal(t, expectedErr.Error(), err.Error())
}

func TestAuthzPostUserDuplicate(t *testing.T) {
	userString := fmt.Sprintf("{\"username\":\"%s\"}", uuid.New().String())
	svc, closeDB, authzUser, ctx := setup(t)
	defer closeDB()
	ctx.SetRequest(httptest.NewRequest("", "/", strings.NewReader(userString)))

	authzUser.On("CanCreateUser", mock.Anything, model.User{}, mock.Anything, mock.Anything).Return(nil)

	// post user once
	_, err := svc.postUser(ctx)
	require.NoError(t, err)

	// post user a second time, expect an error.
	_, closeDB, _, ctx2 := setup(t)
	defer closeDB()
	ctx2.SetRequest(httptest.NewRequest("", "/", strings.NewReader(userString)))

	expectedErr := api.ErrUserExists

	_, err = svc.postUser(ctx2)
	require.Contains(t, expectedErr.Error(), err.Error())
}

func TestAuthzGetUserImage(t *testing.T) {
	svc, closeDB, authzUser, ctx := setup(t)
	defer closeDB()

	// If we can get the user return the error from canGetUsersImageError.
	ctx.SetParamNames("username")
	ctx.SetParamValues("admin")
	expectedErr := errors.Wrap(forbiddenError, "canGetUsersImageError")
	authzUser.On("CanGetUsersImage", mock.Anything, model.User{}, mock.Anything).
		Return(fmt.Errorf("canGetUsersImageError")).Once()
	authzUser.On("CanGetUser", mock.Anything, model.User{}, mock.Anything).Return(nil).Once()

	_, err := svc.getUserImage(ctx)
	require.Equal(t, expectedErr.Error(), err.Error())

	// If we get an error from canGetUser we return that error.
	authzUser.On("CanGetUsersImage", mock.Anything, model.User{}, mock.Anything).
		Return(fmt.Errorf("canGetUsersImageError")).Once()
	cantGetUserError := fmt.Errorf("cantGetUserError")
	authzUser.On("CanGetUser", mock.Anything, model.User{}, mock.Anything).
		Return(cantGetUserError).Once()

	_, err = svc.getUserImage(ctx)
	require.Equal(t, cantGetUserError, err)

	// If we can't view the user return the same error as the user not existing.
	authzUser.On("CanGetUsersImage", mock.Anything, model.User{}, mock.Anything).
		Return(fmt.Errorf("canGetUsersImageError"))
	authzUser.On("CanGetUser", mock.Anything, model.User{},
		mock.Anything).Return(authz2.PermissionDeniedError{}).Once()

	_, err = svc.getUserImage(ctx)
	require.Equal(t, db.ErrNotFound.Error(), err.Error())

	ctx.SetParamNames("username")
	ctx.SetParamValues(notFoundUsername)
	_, err = svc.getUserImage(ctx)
	require.Equal(t, db.ErrNotFound.Error(), err.Error())
}

func TestPatchUserOwnPasswordNeedsCurrentPassword(t *testing.T) {
	svc, closeDB, authzUser, ctx := setup(t)
	defer closeDB()

	hashedOld := ReplicateClientSideSaltAndHash("Old-password-1")
	u := model.User{Username: uuid.New().String(), Active: true}
	require.NoError(t, u.UpdatePasswordHash(hashedOld))
	id, err := Add(stdContext.TODO(), &u, nil)
	require.NoError(t, err)
	u.ID = id
	dc := ctx.(*context.DetContext)
	dc.SetUser(u)
	defer dc.SetUser(model.User{})

	hashedNew := ReplicateClientSideSaltAndHash("New-password-1")
	cases := []struct {
		body string
		code int
	}{
		{fmt.Sprintf(`{"password":%q}`, hashedNew), http.StatusBadRequest},
		{fmt.Sprintf(`{"password":%q,"old_password":"wrong"}`, hashedNew), http.StatusForbidden},
		{fmt.Sprintf(`{"password":%q,"old_password":%q}`, hashedNew, hashedOld), 0},
	}
	for _, tc := range cases {
		ctx.SetParamNames("username")
		ctx.SetParamValues(u.Username)
		ctx.SetRequest(httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(tc.body)))
		authzUser.On("CanSetUsersPassword", mock.Anything, u, mock.Anything).Return(nil).Once()

		_, err := svc.patchUser(ctx)
		if tc.code == 0 {
			require.NoError(t, err)
			continue
		}
		var httpErr *echo.HTTPError
		require.ErrorAs(t, err, &httpErr)
		require.Equal(t, tc.code, httpErr.Code)
	}

	updated, err := ByUsername(stdContext.TODO(), u.Username)
	require.NoError(t, err)
	require.True(t, updated.ValidatePassword(hashedNew))
}

// legacyPatch calls a legacy PATCH /users/:username route handler as curUser.
func legacyPatch(
	ctx echo.Context, curUser model.User, username, body string,
	handler func(echo.Context) (interface{}, error),
) error {
	dc := ctx.(*context.DetContext)
	dc.SetUser(curUser)
	defer dc.SetUser(model.User{})
	ctx.SetParamNames("username")
	ctx.SetParamValues(username)
	ctx.SetRequest(httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(body)))
	_, err := handler(ctx)
	return err
}

func requireHTTPCode(t *testing.T, code int, err error) {
	t.Helper()
	var httpErr *echo.HTTPError
	require.ErrorAs(t, err, &httpErr)
	require.Equal(t, code, httpErr.Code, httpErr.Error())
}

func TestLegacyPatchUserAdminChangesOtherPassword(t *testing.T) {
	svc, closeDB, authzUser, ctx := setup(t)
	defer closeDB()

	hashedOld := ReplicateClientSideSaltAndHash("Old-password-1")
	u := model.User{Username: uuid.New().String(), Active: true}
	require.NoError(t, u.UpdatePasswordHash(hashedOld))
	id, err := Add(stdContext.TODO(), &u, nil)
	require.NoError(t, err)
	u.ID = id
	admin := model.User{ID: id + 100000, Username: "admin-" + uuid.New().String(), Admin: true}

	// An administrator changes another user's password without that user's current password.
	hashedNew := ReplicateClientSideSaltAndHash("New-password-1")
	authzUser.On("CanSetUsersPassword", mock.Anything, admin, mock.Anything).Return(nil).Once()
	require.NoError(t, legacyPatch(ctx, admin, u.Username,
		fmt.Sprintf(`{"password":%q}`, hashedNew), svc.patchUser))
	updated, err := ByUsername(stdContext.TODO(), u.Username)
	require.NoError(t, err)
	require.True(t, updated.ValidatePassword(hashedNew))
}

func TestLegacyPatchUsernameSelfNeedsCurrentPassword(t *testing.T) {
	svc, closeDB, authzUser, ctx := setup(t)
	defer closeDB()

	hashed := ReplicateClientSideSaltAndHash("Password-1")
	u := model.User{Username: uuid.New().String(), Active: true}
	require.NoError(t, u.UpdatePasswordHash(hashed))
	id, err := Add(stdContext.TODO(), &u, nil)
	require.NoError(t, err)
	u.ID = id

	newName := uuid.New().String()
	cases := []struct {
		body string
		code int
	}{
		{fmt.Sprintf(`{"username":%q}`, newName), http.StatusBadRequest},
		{fmt.Sprintf(`{"username":%q,"old_password":"wrong"}`, newName), http.StatusForbidden},
		// Like the legacy routes' passwords, the current password is salted and hashed.
		{fmt.Sprintf(`{"username":%q,"old_password":"Password-1"}`, newName), http.StatusForbidden},
	}
	for _, tc := range cases {
		authzUser.On("CanSetUsersUsername", mock.Anything, u, mock.Anything).Return(nil).Once()
		requireHTTPCode(t, tc.code, legacyPatch(ctx, u, u.Username, tc.body, svc.patchUsername))
		_, err = ByUsername(stdContext.TODO(), u.Username)
		require.NoError(t, err, "the user was renamed")
	}

	authzUser.On("CanSetUsersUsername", mock.Anything, u, mock.Anything).Return(nil).Once()
	require.NoError(t, legacyPatch(ctx, u, u.Username,
		fmt.Sprintf(`{"username":%q,"old_password":%q}`, newName, hashed), svc.patchUsername))
	renamed, err := ByUsername(stdContext.TODO(), newName)
	require.NoError(t, err)
	require.Equal(t, u.ID, renamed.ID)

	// An administrator renames another user without a current password.
	admin := model.User{ID: id + 100000, Username: "admin-" + uuid.New().String(), Admin: true}
	adminName := uuid.New().String()
	authzUser.On("CanSetUsersUsername", mock.Anything, admin, mock.Anything).Return(nil).Once()
	require.NoError(t, legacyPatch(ctx, admin, newName,
		fmt.Sprintf(`{"username":%q}`, adminName), svc.patchUsername))
	renamed, err = ByUsername(stdContext.TODO(), adminName)
	require.NoError(t, err)
	require.Equal(t, u.ID, renamed.ID)
}
