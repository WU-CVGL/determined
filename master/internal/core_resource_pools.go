package internal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/determined-ai/determined/master/internal/cluster"
	"github.com/determined-ai/determined/master/internal/config"
	detContext "github.com/determined-ai/determined/master/internal/context"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/rm/agentrm"
	"github.com/determined-ai/determined/master/internal/user"
	"github.com/determined-ai/determined/master/pkg/model"
)

const maxDynamicPoolRequestBytes = 1 << 20

var dynamicPoolRequestUser = func(
	request *http.Request,
) (*model.User, *model.UserSession, error) {
	return user.GetService().UserAndSessionFromRequest(request)
}

var authorizeDynamicPoolRequest = func(
	request *http.Request, currentUser *model.User, update bool,
) (permErr error, err error) {
	return authorizeDynamicPoolWithProvider(
		cluster.AuthZProvider.Get(), request, currentUser, update,
	)
}

func authorizeDynamicPoolWithProvider(
	provider cluster.MiscAuthZ,
	request *http.Request,
	currentUser *model.User,
	update bool,
) (permErr error, err error) {
	if update {
		return provider.CanUpdateMasterConfig(request.Context(), currentUser)
	}
	return provider.CanGetMasterConfig(request.Context(), currentUser)
}

type createDynamicResourcePoolRequest struct {
	ClusterName    string `json:"cluster_name,omitempty"`
	IdempotencyKey string `json:"idempotency_key"`
	// Config is kept as sent: only the keys the administrator wrote are saved as the pool's spec.
	Config json.RawMessage `json:"config"`
}

// updateDynamicResourcePoolRequest replaces a pool's whole spec rather than patching it: keys that
// it leaves out are no longer set, as when a master.yaml entry is edited.
type updateDynamicResourcePoolRequest struct {
	ExpectedRevision *int64          `json:"expected_revision,omitempty"`
	Config           json.RawMessage `json:"config"`
}

// The top-level fields that each request body may hold.
var (
	createDynamicPoolRequestFields = map[string]bool{
		"cluster_name": true, "idempotency_key": true, "config": true,
	}
	updateDynamicPoolRequestFields = map[string]bool{"expected_revision": true, "config": true}
)

// dynamicResourcePoolView is a dynamic pool record as the API returns it.
type dynamicResourcePoolView struct {
	db.DynamicResourcePool
	// ActiveRevision is the revision that the published runtime pool runs. It is null for a pool
	// that is not published and for a pool served from master.yaml.
	ActiveRevision      *int64 `json:"active_revision"`
	DefinedInMasterYAML bool   `json:"defined_in_master_yaml"`
	// PendingRestart reports that the running master does not run the saved revision.
	PendingRestart bool `json:"pending_restart"`
}

type dynamicResourcePoolListResponse struct {
	ResourcePools []dynamicResourcePoolView `json:"resource_pools"`
}

func (m *Master) registerDynamicResourcePoolRoutes() {
	group := m.echo.Group("/api/v1/resource-pools/dynamic")
	group.POST("", m.createDynamicResourcePool, m.dynamicPoolAuth(true))
	group.GET("", m.listDynamicResourcePools, m.dynamicPoolAuth(false))
	group.PUT("/:name", m.updateDynamicResourcePool, m.dynamicPoolAuth(true))
	group.POST("/:name/retry", m.retryDynamicResourcePool, m.dynamicPoolAuth(true))
}

// dynamicPoolAuth authenticates direct /api/v1 Echo routes explicitly. Generic Echo auth exempts
// /api/v1 because normal routes there are authenticated by gRPC interceptors; these routes do not
// pass through gRPC.
func (m *Master) dynamicPoolAuth(update bool) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			currentUser, session, err := dynamicPoolRequestUser(c.Request())
			switch {
			case errors.Is(err, db.ErrNotFound):
				return echo.NewHTTPError(http.StatusUnauthorized, "invalid authentication")
			case err != nil:
				var httpErr *echo.HTTPError
				if errors.As(err, &httpErr) && httpErr.Code == http.StatusUnauthorized {
					return httpErr
				}
				return err
			case !currentUser.Active:
				return echo.NewHTTPError(http.StatusForbidden, "user not active")
			}

			ctx := c.(*detContext.DetContext)
			ctx.SetUser(*currentUser)
			ctx.SetUserSession(*session)
			permErr, err := authorizeDynamicPoolRequest(c.Request(), currentUser, update)
			if err != nil {
				return err
			}
			if permErr != nil {
				return echo.NewHTTPError(http.StatusForbidden, permErr.Error())
			}
			return next(c)
		}
	}
}

func (m *Master) createDynamicResourcePool(c echo.Context) error {
	var request createDynamicResourcePoolRequest
	if err := decodeStrictBoundedJSON(c, &request, createDynamicPoolRequestFields); err != nil {
		return err
	}
	resourceManager, clusterName, err := m.selectDynamicAgentRM(request.ClusterName)
	if err != nil {
		return err
	}
	var named struct {
		PoolName string `json:"pool_name"`
	}
	if err = json.Unmarshal(request.Config, &named); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid config: %v", err))
	}
	if staticCluster, ok := m.staticResourcePoolCluster(named.PoolName); ok {
		return echo.NewHTTPError(http.StatusConflict, fmt.Sprintf(
			"resource pool name %q conflicts with static pool in resource manager %q",
			named.PoolName, staticCluster,
		))
	}
	record, created, err := resourceManager.CreateDynamicResourcePool(
		c.Request().Context(), request.IdempotencyKey, request.Config,
		m.config.TaskContainerDefaults,
	)
	if err != nil {
		return dynamicPoolHTTPError(err)
	}
	if record.ClusterName != clusterName {
		return echo.NewHTTPError(http.StatusInternalServerError, "resource manager mismatch")
	}
	statusCode := http.StatusOK
	if created {
		statusCode = http.StatusCreated
	}
	return c.JSON(statusCode, m.dynamicResourcePoolView(record))
}

func (m *Master) listDynamicResourcePools(c echo.Context) error {
	clusterName := c.QueryParam("cluster_name")
	if clusterName != "" {
		if _, ok := m.allRms[clusterName]; !ok {
			return echo.NewHTTPError(
				http.StatusNotFound, fmt.Sprintf("resource manager %q not found", clusterName),
			)
		}
	}
	records, err := m.db.ListDynamicResourcePools(c.Request().Context(), clusterName)
	if err != nil {
		return err
	}
	views := make([]dynamicResourcePoolView, 0, len(records))
	for _, record := range records {
		views = append(views, m.dynamicResourcePoolView(record))
	}
	return c.JSON(http.StatusOK, dynamicResourcePoolListResponse{ResourcePools: views})
}

func (m *Master) updateDynamicResourcePool(c echo.Context) error {
	var request updateDynamicResourcePoolRequest
	if err := decodeStrictBoundedJSON(c, &request, updateDynamicPoolRequestFields); err != nil {
		return err
	}
	resourceManager, _, err := m.selectDynamicAgentRM(c.QueryParam("cluster_name"))
	if err != nil {
		return err
	}
	poolName := c.Param("name")
	if _, ok := m.staticResourcePoolCluster(poolName); ok {
		_, err = m.db.DynamicResourcePoolByName(c.Request().Context(), poolName)
		switch {
		case err == nil:
			return echo.NewHTTPError(http.StatusConflict, fmt.Sprintf(
				"pool %q is still defined in master.yaml; remove it and restart before updating",
				poolName,
			))
		case errors.Is(err, db.ErrDynamicResourcePoolNotFound):
			return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf(
				"%q is configured in master.yaml; adopt it first", poolName,
			))
		default:
			return err
		}
	}
	record, err := resourceManager.UpdateDynamicResourcePool(
		c.Request().Context(), poolName, request.ExpectedRevision, request.Config,
		m.config.TaskContainerDefaults,
	)
	if err != nil {
		return dynamicPoolHTTPError(err)
	}
	return c.JSON(http.StatusOK, m.dynamicResourcePoolView(record))
}

func (m *Master) retryDynamicResourcePool(c echo.Context) error {
	if err := requireEmptyBody(c); err != nil {
		return err
	}
	resourceManager, _, err := m.selectDynamicAgentRM(c.QueryParam("cluster_name"))
	if err != nil {
		return err
	}
	record, err := resourceManager.RetryDynamicResourcePool(
		c.Request().Context(), c.Param("name"),
	)
	if err != nil {
		return dynamicPoolHTTPError(err)
	}
	return c.JSON(http.StatusOK, m.dynamicResourcePoolView(record))
}

// dynamicResourcePoolView adds what the running master does with a record. A durable Ready can
// be written immediately before publication, so an unpublished Ready pool is shown as Pending.
func (m *Master) dynamicResourcePoolView(record db.DynamicResourcePool) dynamicResourcePoolView {
	var activeRevision *int64
	if resourceManager, ok := m.allRms[record.ClusterName].(*agentrm.ResourceManager); ok {
		if record.State == db.DynamicResourcePoolReady &&
			!resourceManager.IsDynamicResourcePoolReady(record.PoolName) {
			record.State = db.DynamicResourcePoolPending
		}
		if revision, active := resourceManager.ActiveDynamicResourcePoolRevision(
			record.PoolName,
		); active {
			activeRevision = &revision
		}
	}
	_, definedInMasterYAML := m.staticResourcePoolCluster(record.PoolName)
	return newDynamicResourcePoolView(record, activeRevision, definedInMasterYAML)
}

func newDynamicResourcePoolView(
	record db.DynamicResourcePool, activeRevision *int64, definedInMasterYAML bool,
) dynamicResourcePoolView {
	return dynamicResourcePoolView{
		DynamicResourcePool: printableDynamicResourcePool(record),
		ActiveRevision:      activeRevision,
		DefinedInMasterYAML: definedInMasterYAML,
		PendingRestart: definedInMasterYAML ||
			activeRevision != nil && *activeRevision != record.Revision,
	}
}

func (m *Master) selectDynamicAgentRM(
	requestedCluster string,
) (*agentrm.ResourceManager, string, error) {
	if requestedCluster != "" {
		resourceManager, ok := m.allRms[requestedCluster]
		if !ok {
			return nil, "", echo.NewHTTPError(
				http.StatusNotFound,
				fmt.Sprintf("resource manager %q not found", requestedCluster),
			)
		}
		agentRM, ok := resourceManager.(*agentrm.ResourceManager)
		if !ok {
			return nil, "", echo.NewHTTPError(
				http.StatusBadRequest,
				fmt.Sprintf("resource manager %q is not an agent resource manager", requestedCluster),
			)
		}
		return agentRM, requestedCluster, nil
	}

	var selected *agentrm.ResourceManager
	var clusterName string
	for name, resourceManager := range m.allRms {
		agentRM, ok := resourceManager.(*agentrm.ResourceManager)
		if !ok {
			continue
		}
		if selected != nil {
			return nil, "", echo.NewHTTPError(
				http.StatusBadRequest,
				"cluster_name is required when multiple agent resource managers are configured",
			)
		}
		selected = agentRM
		clusterName = name
	}
	if selected == nil {
		return nil, "", echo.NewHTTPError(
			http.StatusBadRequest, "dynamic pools require an agent resource manager",
		)
	}
	return selected, clusterName, nil
}

func (m *Master) staticResourcePoolCluster(poolName string) (string, bool) {
	for _, rmConfig := range m.config.ResourceManagers() {
		for _, pool := range rmConfig.ResourcePools {
			if pool.PoolName == poolName {
				return rmConfig.ResourceManager.ClusterName(), true
			}
		}
	}
	return "", false
}

func decodeStrictBoundedJSON(
	c echo.Context, target interface{}, allowedFields map[string]bool,
) error {
	contentType := c.Request().Header.Get(echo.HeaderContentType)
	if !strings.HasPrefix(strings.ToLower(contentType), echo.MIMEApplicationJSON) {
		return echo.NewHTTPError(http.StatusUnsupportedMediaType, "Content-Type must be application/json")
	}
	c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, maxDynamicPoolRequestBytes)
	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "request body exceeds 1 MiB")
		}
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("reading JSON body: %v", err))
	}
	if err = validateDynamicPoolRequestJSON(body, allowedFields); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return echo.NewHTTPError(http.StatusBadRequest, "JSON body must contain exactly one value")
		}
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
	}
	return nil
}

// ResourcePoolConfig and TaskContainerDefaultsConfig implement custom JSON unmarshaling, so the
// standard DisallowUnknownFields option cannot see unknown nested fields. Validate the API schema
// at each configuration object boundary before invoking those unmarshallers. Kubernetes pod specs
// remain ordinary Kubernetes JSON objects and are validated by their own decoder.
func validateDynamicPoolRequestJSON(body []byte, allowedFields map[string]bool) error {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	if err := rejectUnknownJSONFields(request, allowedFields, "request"); err != nil {
		return err
	}
	configRaw, ok := request["config"]
	if !ok {
		return fmt.Errorf("config is required")
	}
	return agentrm.ValidateDynamicResourcePoolConfigJSON(configRaw)
}

func rejectUnknownJSONFields(
	object map[string]json.RawMessage, allowed map[string]bool, path string,
) error {
	for field := range object {
		if !allowed[field] {
			return fmt.Errorf("unknown field %q in %s", field, path)
		}
	}
	return nil
}

func requireEmptyBody(c echo.Context) error {
	c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, maxDynamicPoolRequestBytes)
	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "request body exceeds 1 MiB")
		}
		return err
	}
	if len(bytes.TrimSpace(body)) != 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "retry request body must be empty")
	}
	return nil
}

func dynamicPoolHTTPError(err error) error {
	switch {
	case errors.Is(err, agentrm.ErrInvalidDynamicResourcePool):
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	case errors.Is(err, agentrm.ErrStaticResourcePoolConflict),
		errors.Is(err, agentrm.ErrDynamicResourcePoolInitializing),
		errors.Is(err, db.ErrDynamicResourcePoolConflict),
		errors.Is(err, db.ErrDynamicResourcePoolNotFailed),
		errors.Is(err, db.ErrDynamicResourcePoolChanged):
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	case errors.Is(err, db.ErrDynamicResourcePoolNotFound):
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	default:
		return err
	}
}

func printableDynamicResourcePool(record db.DynamicResourcePool) db.DynamicResourcePool {
	if record.Spec != nil {
		spec := printableDynamicPoolSpec(*record.Spec)
		record.Spec = &spec
	}
	var cfg config.ResourcePoolConfig
	if err := json.Unmarshal(record.Config, &cfg); err != nil {
		record.Config = json.RawMessage(`{"redacted":true}`)
		return record
	}
	if cfg.TaskContainerDefaults != nil && cfg.TaskContainerDefaults.RegistryAuth != nil {
		auth := *cfg.TaskContainerDefaults.RegistryAuth
		if auth.Password != "" {
			auth.Password = agentrm.RedactedDynamicPoolCredential
		}
		if auth.Auth != "" {
			auth.Auth = agentrm.RedactedDynamicPoolCredential
		}
		if auth.IdentityToken != "" {
			auth.IdentityToken = agentrm.RedactedDynamicPoolCredential
		}
		if auth.RegistryToken != "" {
			auth.RegistryToken = agentrm.RedactedDynamicPoolCredential
		}
		cfg.TaskContainerDefaults.RegistryAuth = &auth
	}
	printable, err := json.Marshal(cfg.Printable())
	if err == nil {
		record.Config = printable
	}
	return record
}

// printableDynamicPoolSpec redacts registry credentials in a spec. It edits the decoded object,
// so the spec keeps only the keys the administrator wrote.
func printableDynamicPoolSpec(spec json.RawMessage) json.RawMessage {
	decoder := json.NewDecoder(bytes.NewReader(spec))
	decoder.UseNumber()
	var object map[string]interface{}
	if err := decoder.Decode(&object); err != nil {
		return json.RawMessage(`{"redacted":true}`)
	}
	if defaults, ok := object["task_container_defaults"].(map[string]interface{}); ok {
		if auth, ok := defaults["registry_auth"].(map[string]interface{}); ok {
			for _, field := range []string{"password", "auth", "identitytoken", "registrytoken"} {
				if value, exists := auth[field]; exists && value != nil && value != "" {
					auth[field] = agentrm.RedactedDynamicPoolCredential
				}
			}
		}
	}
	printable, err := json.Marshal(object)
	if err != nil {
		return json.RawMessage(`{"redacted":true}`)
	}
	return printable
}
