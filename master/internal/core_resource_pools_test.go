package internal

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/docker/docker/api/types/registry"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/cluster"
	detContext "github.com/determined-ai/determined/master/internal/context"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/rm/agentrm"
	"github.com/determined-ai/determined/master/pkg/model"
)

func TestValidateDynamicPoolRequestJSON(t *testing.T) {
	valid := []byte(`{
        "idempotency_key":"request-1",
        "config":{
            "pool_name":"online",
            "scheduler":{"type":"priority","default_priority":42},
            "task_container_defaults":{"force_pull_image":true,
                "registry_auth":{"username":"u","password":"p"}}
        }
    }`)
	require.NoError(t, validateDynamicPoolRequestJSON(valid, createDynamicPoolRequestFields))

	tests := []string{
		`{"unknown":true,"idempotency_key":"x","config":{"pool_name":"p"}}`,
		`{"idempotency_key":"x","config":{"pool_name":"p","unknown":true}}`,
		`{"idempotency_key":"x","config":{"pool_name":"p","scheduler":{"unknown":true}}}`,
		`{"idempotency_key":"x","config":{"pool_name":"p","task_container_defaults":{"unknown":true}}}`,
		`{"idempotency_key":"x","config":{"pool_name":"p","task_container_defaults":{"registry_auth":{"unknown":true}}}}`,
		`{"idempotency_key":"x","config":{"pool_name":"p","provider":{"type":"aws"}}}`,
		`{"idempotency_key":"x","expected_revision":1,"config":{"pool_name":"p"}}`,
		`{"idempotency_key":"x"}`,
	}
	for _, body := range tests {
		require.Error(t, validateDynamicPoolRequestJSON(
			[]byte(body), createDynamicPoolRequestFields,
		), body)
	}

	// An update names its pool in the path and may only add the revision it was based on.
	for _, body := range []string{
		`{"config":{"pool_name":"p"}}`,
		`{"expected_revision":3,"config":{"pool_name":"p"}}`,
		`{"expected_revision":null,"config":{"pool_name":"p","task_container_defaults":{"shm_size_bytes":1}}}`,
	} {
		require.NoError(t, validateDynamicPoolRequestJSON(
			[]byte(body), updateDynamicPoolRequestFields,
		), body)
	}
	for _, body := range []string{
		`{"cluster_name":"c","config":{"pool_name":"p"}}`,
		`{"idempotency_key":"x","config":{"pool_name":"p"}}`,
		`{"expected_revision":3}`,
		`{"config":{"pool_name":"p","unknown":true}}`,
		`{"config":{"pool_name":"p","task_container_defaults":{"registry_auth":{"unknown":true}}}}`,
	} {
		require.Error(t, validateDynamicPoolRequestJSON(
			[]byte(body), updateDynamicPoolRequestFields,
		), body)
	}
}

func TestDynamicPoolHTTPError(t *testing.T) {
	for _, test := range []struct {
		err  error
		code int
	}{
		{fmt.Errorf("%w: renaming is not supported", agentrm.ErrInvalidDynamicResourcePool), 400},
		{fmt.Errorf("%w: %q", agentrm.ErrStaticResourcePoolConflict, "p"), 409},
		{fmt.Errorf("%w; retry", agentrm.ErrDynamicResourcePoolInitializing), 409},
		{fmt.Errorf("%w: name", db.ErrDynamicResourcePoolConflict), 409},
		{db.ErrDynamicResourcePoolNotFailed, 409},
		{fmt.Errorf("%w: expected revision 1", db.ErrDynamicResourcePoolChanged), 409},
		{db.ErrDynamicResourcePoolNotFound, 404},
	} {
		var httpErr *echo.HTTPError
		require.ErrorAs(t, dynamicPoolHTTPError(test.err), &httpErr, test.err.Error())
		require.Equal(t, test.code, httpErr.Code, test.err.Error())
		require.Equal(t, test.err.Error(), httpErr.Message)
	}
	other := errors.New("database unavailable")
	require.Same(t, other, dynamicPoolHTTPError(other))
}

func TestPrintableDynamicResourcePoolRedactsRegistryCredentials(t *testing.T) {
	cfg := map[string]interface{}{
		"pool_name": "online",
		"task_container_defaults": model.TaskContainerDefaultsConfig{
			RegistryAuth: &registry.AuthConfig{
				Username:      "user",
				Password:      "password-secret",
				IdentityToken: "identity-secret",
				RegistryToken: "registry-secret",
			},
		},
	}
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	printed := printableDynamicResourcePool(db.DynamicResourcePool{Config: raw})
	require.NotContains(t, string(printed.Config), "password-secret")
	require.NotContains(t, string(printed.Config), "identity-secret")
	require.NotContains(t, string(printed.Config), "registry-secret")
	require.Contains(t, string(printed.Config), "********")

	printed = printableDynamicResourcePool(db.DynamicResourcePool{Config: json.RawMessage(`{`)})
	require.JSONEq(t, `{"redacted":true}`, string(printed.Config))
}

func TestPrintableDynamicResourcePoolRedactsSpec(t *testing.T) {
	spec := json.RawMessage(`{"pool_name":"online","task_container_defaults":{"registry_auth":{
        "username":"user","password":"password-secret","auth":"auth-secret",
        "identitytoken":"identity-secret","registrytoken":"registry-secret","email":""}},
        "max_aux_containers_per_agent":100}`)
	printed := printableDynamicResourcePool(db.DynamicResourcePool{
		Config: json.RawMessage(`{"pool_name":"online"}`), Spec: &spec,
	})
	// Only the keys that the spec holds are shown, and the stored spec is not modified.
	require.JSONEq(t, `{"pool_name":"online","task_container_defaults":{"registry_auth":{
        "username":"user","password":"********","auth":"********",
        "identitytoken":"********","registrytoken":"********","email":""}},
        "max_aux_containers_per_agent":100}`, string(*printed.Spec))
	require.Contains(t, string(spec), "password-secret")

	invalid := json.RawMessage(`[`)
	printed = printableDynamicResourcePool(db.DynamicResourcePool{
		Config: json.RawMessage(`{"pool_name":"online"}`), Spec: &invalid,
	})
	require.JSONEq(t, `{"redacted":true}`, string(*printed.Spec))
	printed = printableDynamicResourcePool(db.DynamicResourcePool{
		Config: json.RawMessage(`{"pool_name":"online"}`),
	})
	require.Nil(t, printed.Spec)
}

func TestDynamicResourcePoolViewPendingRestart(t *testing.T) {
	record := db.DynamicResourcePool{Config: json.RawMessage(`{"pool_name":"online"}`), Revision: 2}
	previous, current := int64(1), int64(2)
	for _, test := range []struct {
		name                string
		activeRevision      *int64
		definedInMasterYAML bool
		pendingRestart      bool
	}{
		{name: "not published"},
		{name: "runs the saved revision", activeRevision: &current},
		{name: "runs an earlier revision", activeRevision: &previous, pendingRestart: true},
		{name: "served from master.yaml", definedInMasterYAML: true, pendingRestart: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			view := newDynamicResourcePoolView(
				record, test.activeRevision, test.definedInMasterYAML,
			)
			require.Equal(t, test.pendingRestart, view.PendingRestart)
			require.Equal(t, test.definedInMasterYAML, view.DefinedInMasterYAML)
			require.Equal(t, test.activeRevision, view.ActiveRevision)
		})
	}

	raw, err := json.Marshal(newDynamicResourcePoolView(record, nil, false))
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &fields))
	require.JSONEq(t, `null`, string(fields["active_revision"]))
	require.JSONEq(t, `2`, string(fields["revision"]))
	require.JSONEq(t, `false`, string(fields["pending_restart"]))
	require.JSONEq(t, `false`, string(fields["defined_in_master_yaml"]))
	require.NotContains(t, fields, "spec")
	require.NotContains(t, fields, "spec_hash")
}

func TestDynamicPoolRouteExplicitlyAuthenticates(t *testing.T) {
	original := dynamicPoolRequestUser
	t.Cleanup(func() { dynamicPoolRequestUser = original })
	dynamicPoolRequestUser = func(
		*http.Request,
	) (*model.User, *model.UserSession, error) {
		return nil, nil, echo.ErrUnauthorized
	}

	e := echo.New()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/resource-pools/dynamic", nil)
	recorder := httptest.NewRecorder()
	ctx := &detContext.DetContext{Context: e.NewContext(request, recorder)}
	called := false
	handler := (&Master{}).dynamicPoolAuth(false)(func(echo.Context) error {
		called = true
		return nil
	})
	err := handler(ctx)
	var httpErr *echo.HTTPError
	require.ErrorAs(t, err, &httpErr)
	require.Equal(t, http.StatusUnauthorized, httpErr.Code)
	require.False(t, called)
}

func TestDynamicPoolRouteAuthorization(t *testing.T) {
	originalUser := dynamicPoolRequestUser
	originalAuthorize := authorizeDynamicPoolRequest
	t.Cleanup(func() {
		dynamicPoolRequestUser = originalUser
		authorizeDynamicPoolRequest = originalAuthorize
	})
	for _, test := range []struct {
		name       string
		active     bool
		admin      bool
		update     bool
		wantCode   int
		wantCalled bool
	}{
		{name: "admin create", active: true, admin: true, update: true, wantCalled: true},
		{name: "admin list", active: true, admin: true, wantCalled: true},
		{name: "read-only create", active: true, update: true, wantCode: http.StatusForbidden},
		{name: "viewer list", active: true, wantCode: http.StatusForbidden},
		{name: "inactive admin", admin: true, update: true, wantCode: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			dynamicPoolRequestUser = func(
				*http.Request,
			) (*model.User, *model.UserSession, error) {
				return &model.User{Active: test.active, Admin: test.admin}, &model.UserSession{}, nil
			}
			authorizeDynamicPoolRequest = func(
				request *http.Request, currentUser *model.User, update bool,
			) (error, error) {
				return authorizeDynamicPoolWithProvider(
					&cluster.MiscAuthZBasic{}, request, currentUser, update,
				)
			}
			e := echo.New()
			request := httptest.NewRequest(http.MethodGet, "/api/v1/resource-pools/dynamic", nil)
			ctx := &detContext.DetContext{Context: e.NewContext(request, httptest.NewRecorder())}
			called := false
			err := (&Master{}).dynamicPoolAuth(test.update)(func(echo.Context) error {
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

func TestDynamicPoolRoutesRequirePermissions(t *testing.T) {
	originalUser := dynamicPoolRequestUser
	originalAuthorize := authorizeDynamicPoolRequest
	t.Cleanup(func() {
		dynamicPoolRequestUser = originalUser
		authorizeDynamicPoolRequest = originalAuthorize
	})
	dynamicPoolRequestUser = func(*http.Request) (*model.User, *model.UserSession, error) {
		return &model.User{Active: true}, &model.UserSession{}, nil
	}
	var updates []bool
	authorizeDynamicPoolRequest = func(
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
	(&Master{echo: e}).registerDynamicResourcePoolRoutes()
	for _, test := range []struct {
		method string
		path   string
		update bool
	}{
		{http.MethodGet, "/api/v1/resource-pools/dynamic", false},
		{http.MethodPost, "/api/v1/resource-pools/dynamic", true},
		{http.MethodPut, "/api/v1/resource-pools/dynamic/online", true},
		{http.MethodPost, "/api/v1/resource-pools/dynamic/online/retry", true},
	} {
		updates = nil
		recorder := httptest.NewRecorder()
		e.ServeHTTP(recorder, httptest.NewRequest(test.method, test.path, nil))
		require.Equal(t, http.StatusForbidden, recorder.Code, test.method+" "+test.path)
		require.Equal(t, []bool{test.update}, updates, test.method+" "+test.path)
	}
}
