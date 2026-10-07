package internal

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/cluster"
	detContext "github.com/determined-ai/determined/master/internal/context"
	"github.com/determined-ai/determined/master/pkg/model"
)

func TestMasterConfigRouteExplicitlyAuthenticates(t *testing.T) {
	original := masterConfigRouteUser
	t.Cleanup(func() { masterConfigRouteUser = original })
	masterConfigRouteUser = func(
		*http.Request,
	) (*model.User, *model.UserSession, error) {
		return nil, nil, echo.ErrUnauthorized
	}

	e := echo.New()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	recorder := httptest.NewRecorder()
	ctx := &detContext.DetContext{Context: e.NewContext(request, recorder)}
	called := false
	handler := requireMasterConfigAccess(false)(func(echo.Context) error {
		called = true
		return nil
	})
	err := handler(ctx)
	var httpErr *echo.HTTPError
	require.ErrorAs(t, err, &httpErr)
	require.Equal(t, http.StatusUnauthorized, httpErr.Code)
	require.False(t, called)
}

func TestMasterConfigRouteAuthorization(t *testing.T) {
	originalUser := masterConfigRouteUser
	originalAuthorize := authorizeMasterConfigRoute
	t.Cleanup(func() {
		masterConfigRouteUser = originalUser
		authorizeMasterConfigRoute = originalAuthorize
	})
	for _, test := range []struct {
		name       string
		active     bool
		admin      bool
		update     bool
		wantCode   int
		wantCalled bool
	}{
		{name: "admin update", active: true, admin: true, update: true, wantCalled: true},
		{name: "admin read", active: true, admin: true, wantCalled: true},
		{name: "non-admin update", active: true, update: true, wantCode: http.StatusForbidden},
		{name: "non-admin read", active: true, wantCode: http.StatusForbidden},
		{name: "inactive admin", admin: true, update: true, wantCode: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			masterConfigRouteUser = func(
				*http.Request,
			) (*model.User, *model.UserSession, error) {
				return &model.User{Active: test.active, Admin: test.admin}, &model.UserSession{}, nil
			}
			authorizeMasterConfigRoute = func(
				request *http.Request, currentUser *model.User, update bool,
			) (error, error) {
				return authorizeMasterConfigRouteWithProvider(
					&cluster.MiscAuthZBasic{}, request, currentUser, update,
				)
			}
			e := echo.New()
			request := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
			ctx := &detContext.DetContext{Context: e.NewContext(request, httptest.NewRecorder())}
			called := false
			err := requireMasterConfigAccess(test.update)(func(echo.Context) error {
				called = true
				return nil
			})(ctx)
			require.Equal(t, test.wantCalled, called)
			if test.wantCode == 0 {
				require.NoError(t, err)
				return
			}
			var httpErr *echo.HTTPError
			require.ErrorAs(t, err, &httpErr)
			require.Equal(t, test.wantCode, httpErr.Code)
		})
	}
}

func TestDecodeJSONBody(t *testing.T) {
	type target struct {
		Mode string `json:"mode"`
	}
	for _, test := range []struct {
		name        string
		contentType string
		body        string
		code        int
		message     string
		mode        string
	}{
		{name: "one value", contentType: "application/json", body: `{"mode":"a"}`, mode: "a"},
		{
			name: "a charset", contentType: "application/json; charset=utf-8",
			body: `{"mode":"a"}`, mode: "a",
		},
		{
			name: "not labeled as JSON", contentType: "text/plain", body: `{"mode":"a"}`,
			code: http.StatusUnsupportedMediaType, message: "Content-Type must be application/json",
		},
		{
			name: "no content type", body: `{"mode":"a"}`,
			code: http.StatusUnsupportedMediaType, message: "Content-Type must be application/json",
		},
		{
			name: "too large", contentType: "application/json",
			body: `{"mode":"a"}` + strings.Repeat(" ", 1<<10),
			code: http.StatusRequestEntityTooLarge, message: "request body exceeds 1 KiB",
		},
		{
			name: "an unknown field", contentType: "application/json", body: `{"mode":"a","x":1}`,
			code: http.StatusBadRequest, message: `invalid JSON body: json: unknown field "x"`,
		},
		{
			name: "two values", contentType: "application/json", body: `{"mode":"a"} {"mode":"b"}`,
			code: http.StatusBadRequest, message: "JSON body must contain exactly one value",
		},
		{
			name: "trailing garbage", contentType: "application/json", body: `{"mode":"a"} x`,
			code: http.StatusBadRequest, message: "invalid JSON body",
		},
		{
			name: "not an object", contentType: "application/json", body: `["a"]`,
			code: http.StatusBadRequest, message: "invalid JSON body",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPut, "/api/v1/test", strings.NewReader(test.body))
			if test.contentType != "" {
				request.Header.Set(echo.HeaderContentType, test.contentType)
			}
			c := echo.New().NewContext(request, httptest.NewRecorder())
			var decoded target
			err := decodeJSONBody(c, 1<<10, &decoded)
			if test.code == 0 {
				require.NoError(t, err)
				require.Equal(t, test.mode, decoded.Mode)
				return
			}
			var httpErr *echo.HTTPError
			require.ErrorAs(t, err, &httpErr)
			require.Equal(t, test.code, httpErr.Code)
			require.Contains(t, httpErr.Message, test.message)
		})
	}
}
