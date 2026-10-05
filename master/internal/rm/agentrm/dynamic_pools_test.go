package agentrm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/registry"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/config/provconfig"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/rm/tasklist"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

func testDynamicPoolRM() *ResourceManager {
	return &ResourceManager{
		syslog: logrus.New().WithField("component", "dynamic-pool-test"),
		config: &config.AgentResourceManagerConfig{
			ClusterName: "agent-cluster",
			Scheduler:   config.DefaultSchedulerConfig(),
		},
		agentService: &agents{
			agents: tasklist.NewRegistry[aproto.ID, *agent](),
		},
	}
}

func TestNormalizeDynamicResourcePoolConfig(t *testing.T) {
	rm := testDynamicPoolRM()
	defaults := *model.DefaultTaskContainerDefaults()
	defaults.ForcePullImage = true

	normalized, err := rm.NormalizeDynamicResourcePoolConfig(config.ResourcePoolConfig{
		PoolName:                 "online-pool_1",
		MaxAuxContainersPerAgent: 100,
	}, defaults)
	require.NoError(t, err)
	require.NotNil(t, normalized.Scheduler)
	require.NotSame(t, rm.config.Scheduler, normalized.Scheduler)
	require.NotNil(t, normalized.TaskContainerDefaults)
	require.True(t, normalized.TaskContainerDefaults.ForcePullImage)

	// Effective values and pointer graphs are frozen independently of later YAML changes.
	*rm.config.Scheduler.Priority.DefaultPriority = 99
	defaults.ForcePullImage = false
	require.Equal(t, config.DefaultSchedulingPriority,
		*normalized.Scheduler.Priority.DefaultPriority)
	require.True(t, normalized.TaskContainerDefaults.ForcePullImage)
}

func TestNormalizeDynamicResourcePoolConfigRejectsUnsupported(t *testing.T) {
	rm := testDynamicPoolRM()
	defaults := *model.DefaultTaskContainerDefaults()

	for _, name := range []string{"", " leading", "path/name", "control\nname"} {
		_, err := rm.NormalizeDynamicResourcePoolConfig(config.ResourcePoolConfig{
			PoolName: name,
		}, defaults)
		require.ErrorIs(t, err, ErrInvalidDynamicResourcePool)
	}

	_, err := rm.NormalizeDynamicResourcePoolConfig(config.ResourcePoolConfig{
		PoolName: "provider-pool",
		Provider: &provconfig.Config{},
	}, defaults)
	require.ErrorIs(t, err, ErrInvalidDynamicResourcePool)
	require.Contains(t, err.Error(), "provider")

	_, err = rm.NormalizeDynamicResourcePoolConfig(config.ResourcePoolConfig{
		PoolName: "removed-scheduler",
		Scheduler: &config.SchedulerConfig{
			RoundRobin:    &config.RoundRobinSchedulerConfig{},
			FittingPolicy: "best",
		},
	}, defaults)
	require.ErrorIs(t, err, ErrInvalidDynamicResourcePool)
	require.Contains(t, err.Error(), "round robin")
}

func TestUpdateDynamicResourcePoolRejectsInvalidSpecBeforeReading(t *testing.T) {
	// The resource manager has no database: every rejection happens before the record is read.
	manager := testDynamicPoolRM()
	for _, test := range []struct{ spec, message string }{
		{`{"pool_name":"renamed"}`, "renaming is not supported"},
		{`{"pool_name":"online","unknown":true}`, "unknown field"},
		{`{"pool_name":"online","provider":{"type":"aws"}}`, "provider"},
		{`{"pool_name":"online","task_container_defaults":{"shm_size_bytes":-1}}`, "shm_size_bytes"},
		{
			`{"pool_name":"online","task_container_defaults":{"registry_auth":{"password":"********"}}}`,
			"registry_auth.password is the redacted placeholder",
		},
		{
			`{"pool_name":"online","task_container_defaults":{"registry_auth":{"auth":"********"}}}`,
			"registry_auth.auth is the redacted placeholder",
		},
		{
			`{"pool_name":"online","task_container_defaults":` +
				`{"registry_auth":{"identitytoken":"********"}}}`,
			"registry_auth.identitytoken is the redacted placeholder",
		},
		{
			`{"pool_name":"online","task_container_defaults":` +
				`{"registry_auth":{"registrytoken":"********"}}}`,
			"registry_auth.registrytoken is the redacted placeholder",
		},
	} {
		expectedRevision := int64(1)
		_, err := manager.UpdateDynamicResourcePool(
			context.Background(), "online", &expectedRevision, json.RawMessage(test.spec),
			*model.DefaultTaskContainerDefaults(),
		)
		require.ErrorIs(t, err, ErrInvalidDynamicResourcePool, test.spec)
		require.ErrorContains(t, err, test.message, test.spec)
	}
}

func TestValidateDynamicPoolIdempotencyKey(t *testing.T) {
	require.NoError(t, validateDynamicPoolIdempotencyKey("operation-123"))
	require.NoError(t, validateDynamicPoolIdempotencyKey("readopt:pool"))
	for _, key := range []string{"", " \t", strings.Repeat("x", 513), "adopt:pool"} {
		require.ErrorIs(t, validateDynamicPoolIdempotencyKey(key), ErrInvalidDynamicResourcePool)
	}
	require.ErrorContains(t, validateDynamicPoolIdempotencyKey("adopt:pool"),
		`prefix "adopt:" is reserved for adopted pools`)
}

// staticPoolConfig decodes a resource pool the way the master decodes a master.yaml entry.
func staticPoolConfig(t *testing.T, entry string) config.ResourcePoolConfig {
	var cfg config.ResourcePoolConfig
	require.NoError(t, json.Unmarshal([]byte(entry), &cfg))
	return cfg
}

func TestAdoptStaticResourcePoolRejectsBeforeWriting(t *testing.T) {
	// The resource manager has no database: every rejection happens before the record is written.
	manager := testDynamicPoolRM()
	static := staticPoolConfig(t,
		`{"pool_name":"static","description":"gpu","agent_reconnect_wait":"10m"}`)
	withProvider := static
	withProvider.Provider = &provconfig.Config{}
	for _, test := range []struct {
		static  config.ResourcePoolConfig
		spec    string
		message string
	}{
		{withProvider, `{"pool_name":"static"}`, "pools with a provider cannot be adopted"},
		{
			static,
			`{"pool_name":"other","description":"gpu","agent_reconnect_wait":"10m"}`,
			`config.pool_name "other" differs from "static"`,
		},
		{
			static,
			`{"pool_name":"static","description":"gpu"}`,
			`differs from the master.yaml entry of "static" in agent_reconnect_wait; ` +
				"copy the master.yaml entry verbatim, including agent_reconnect_wait",
		},
		{
			static,
			`{"pool_name":"static","agent_reconnect_wait":"10m","max_aux_containers_per_agent":7}`,
			"in description, max_aux_containers_per_agent;",
		},
		{
			static,
			`{"pool_name":"static","agent_reconnect_wait":"10m","description":"gpu",` +
				`"scheduler":{"type":"priority"}}`,
			"in scheduler;",
		},
		{static, `{"pool_name":"static","unknown":true}`, "unknown field"},
		{static, `{"pool_name":"static","provider":{"type":"aws"}}`, "provider"},
	} {
		_, _, err := manager.AdoptStaticResourcePool(
			context.Background(), test.static, json.RawMessage(test.spec),
			*model.DefaultTaskContainerDefaults(),
		)
		require.ErrorIs(t, err, ErrInvalidDynamicResourcePool, test.spec)
		require.ErrorContains(t, err, test.message, test.spec)
	}
}

func TestStaticResourcePoolDifferences(t *testing.T) {
	static := staticPoolConfig(t, `{"pool_name":"static","agent_reconnect_wait":"10m",
		"task_container_defaults":{"shm_size_bytes":17179869184}}`)
	// Keys that master.yaml leaves out decode to the same defaults as keys a spec spells out.
	for _, spec := range []string{
		`{"pool_name":"static","agent_reconnect_wait":"10m",
			"task_container_defaults":{"shm_size_bytes":17179869184}}`,
		`{"task_container_defaults":{"shm_size_bytes":17179869184,"network_mode":"bridge"},
			"max_aux_containers_per_agent":100,"agent_reconnect_wait":"600s","pool_name":"static"}`,
	} {
		differences, err := staticResourcePoolDifferences(staticPoolConfig(t, spec), static)
		require.NoError(t, err)
		require.Empty(t, differences, spec)
	}
	differences, err := staticResourcePoolDifferences(staticPoolConfig(t,
		`{"pool_name":"static","task_container_defaults":{"shm_size_bytes":1}}`), static)
	require.NoError(t, err)
	require.Equal(t, []string{"agent_reconnect_wait", "task_container_defaults"}, differences)
}

func TestCheckStaticPoolCollision(t *testing.T) {
	manager := testDynamicPoolRM()
	static := staticPoolConfig(t,
		`{"pool_name":"static","description":"gpu","agent_reconnect_wait":"10m"}`)
	snapshot, err := manager.NormalizeDynamicResourcePoolConfig(
		static, *model.DefaultTaskContainerDefaults(),
	)
	require.NoError(t, err)
	adopted := testSpecRecord(t, "static",
		`{"agent_reconnect_wait":"10m","description":"gpu","pool_name":"static"}`, snapshot)
	adopted.ClusterName = "agent-cluster"

	// The master.yaml entry serves an adopted pool while the entry equals the saved spec.
	require.NoError(t, checkStaticPoolCollision(adopted, "agent-cluster", static))

	const hint = "if the pool was adopted, its master.yaml entry must equal the saved spec or " +
		"be removed"
	edited := static
	edited.Description = "edited"
	legacy := adopted
	legacy.Spec, legacy.SpecVersion, legacy.SpecHash = nil, nil, nil
	unsupported := adopted
	specVersion := 2
	unsupported.SpecVersion = &specVersion
	for _, test := range []struct {
		name    string
		record  db.DynamicResourcePool
		cluster string
		static  config.ResourcePoolConfig
		reason  string
	}{
		{"edited entry", adopted, "agent-cluster", edited, "its saved spec differs in description"},
		{
			"other resource manager", adopted, "other-cluster", static,
			`it is saved for resource manager "agent-cluster"`,
		},
		{"saved without a spec", legacy, "agent-cluster", static, "it was saved without a spec"},
		{"unsupported spec", unsupported, "agent-cluster", static, "unsupported spec version 2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := checkStaticPoolCollision(test.record, test.cluster, test.static)
			require.ErrorContains(t, err, fmt.Sprintf(
				`dynamic resource pool "static" conflicts with static pool in resource manager %q`,
				test.cluster,
			))
			require.ErrorContains(t, err, test.reason)
			require.ErrorContains(t, err, hint)
		})
	}
}

func TestDecodeStoredDynamicResourcePool(t *testing.T) {
	rm := testDynamicPoolRM()
	cfg, err := rm.NormalizeDynamicResourcePoolConfig(config.ResourcePoolConfig{
		PoolName:                 "persisted-pool",
		MaxAuxContainersPerAgent: 100,
	}, *model.DefaultTaskContainerDefaults())
	require.NoError(t, err)
	raw, hash, err := marshalDynamicResourcePoolConfig(cfg)
	require.NoError(t, err)
	record := db.DynamicResourcePool{
		PoolName:      cfg.PoolName,
		ConfigVersion: dynamicResourcePoolConfigVersion,
		Config:        raw,
		ConfigHash:    hash,
	}
	decoded, inherit, err := decodeStoredDynamicResourcePool(record)
	require.NoError(t, err)
	require.False(t, inherit)
	require.Equal(t, cfg.PoolName, decoded.PoolName)

	record.ConfigVersion++
	_, _, err = decodeStoredDynamicResourcePool(record)
	require.ErrorContains(t, err, "unsupported config version")
	record.ConfigVersion = dynamicResourcePoolConfigVersion
	record.ConfigHash = "corrupt"
	_, _, err = decodeStoredDynamicResourcePool(record)
	require.ErrorContains(t, err, "hash mismatch")

	var object map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &object))
	object["unexpected"] = true
	record.Config, err = json.Marshal(object)
	require.NoError(t, err)
	_, _, err = decodeStoredDynamicResourcePool(record)
	require.ErrorContains(t, err, "unknown field")
}

func TestCanonicalDynamicPoolSpec(t *testing.T) {
	canonical, hash, err := canonicalDynamicPoolSpec(json.RawMessage(
		`{"pool_name":"p","agent_reconnect_wait":"10m","description":"d"}`,
	))
	require.NoError(t, err)
	require.Equal(t, `{"agent_reconnect_wait":"10m","description":"d","pool_name":"p"}`,
		string(canonical))
	sum := sha256.Sum256(canonical)
	require.Equal(t, hex.EncodeToString(sum[:]), hash)

	for _, equivalent := range []string{
		"{ \"description\" : \"d\",\n\t\"pool_name\":\"p\", \"agent_reconnect_wait\":\"10m\" }",
		`{"pool_name":"p","agent_reconnect_wait":"10m","description":"d",
		  "scheduler":null,"task_container_defaults":null,"provider":null}`,
	} {
		equivalentCanonical, equivalentHash, err := canonicalDynamicPoolSpec(
			json.RawMessage(equivalent),
		)
		require.NoError(t, err, equivalent)
		require.Equal(t, string(canonical), string(equivalentCanonical), equivalent)
		require.Equal(t, hash, equivalentHash, equivalent)
	}

	// Nested nulls are values a pool may set, and number literals keep their exact digits.
	nested, nestedHash, err := canonicalDynamicPoolSpec(json.RawMessage(
		`{"pool_name":"p","task_container_defaults":{"work_dir":null,` +
			`"shm_size_bytes":9007199254740993,"add_capabilities":["B","A"]}}`,
	))
	require.NoError(t, err)
	require.Equal(t, `{"pool_name":"p","task_container_defaults":{"add_capabilities":["B","A"],`+
		`"shm_size_bytes":9007199254740993,"work_dir":null}}`, string(nested))
	require.NotEqual(t, hash, nestedHash)

	_, changedHash, err := canonicalDynamicPoolSpec(json.RawMessage(
		`{"pool_name":"p","agent_reconnect_wait":"11m","description":"d"}`,
	))
	require.NoError(t, err)
	require.NotEqual(t, hash, changedHash)

	for _, invalid := range []string{
		`null`, `[]`, `"p"`, `{"pool_name":"p"} {}`, `{"pool_name":"p","unknown":true}`,
		`{"pool_name":"p","task_container_defaults":{"unknown":true}}`,
		`{"pool_name":"p","provider":{"type":"aws"}}`,
	} {
		_, _, err = canonicalDynamicPoolSpec(json.RawMessage(invalid))
		require.Error(t, err, invalid)
	}
}

func testSpecRecord(
	t *testing.T, poolName string, spec string, snapshot config.ResourcePoolConfig,
) db.DynamicResourcePool {
	raw, hash, err := marshalDynamicResourcePoolConfig(snapshot)
	require.NoError(t, err)
	canonical, specHash, err := canonicalDynamicPoolSpec(json.RawMessage(spec))
	require.NoError(t, err)
	specVersion := dynamicResourcePoolSpecVersion
	return db.DynamicResourcePool{
		PoolName:      poolName,
		ConfigVersion: dynamicResourcePoolConfigVersion,
		Config:        raw,
		ConfigHash:    hash,
		Spec:          &canonical,
		SpecVersion:   &specVersion,
		SpecHash:      &specHash,
		Revision:      1,
	}
}

func TestDecodeStoredDynamicResourcePoolSpecRow(t *testing.T) {
	rm := testDynamicPoolRM()
	stale, err := rm.NormalizeDynamicResourcePoolConfig(config.ResourcePoolConfig{
		PoolName: "spec-pool", Description: "stale snapshot", MaxAuxContainersPerAgent: 7,
	}, *model.DefaultTaskContainerDefaults())
	require.NoError(t, err)
	record := testSpecRecord(
		t, "spec-pool", `{"pool_name":"spec-pool","agent_reconnect_wait":"10m"}`, stale,
	)
	// A spec row runs its spec even when the snapshot no longer matches it.
	record.ConfigHash = "stale"

	cfg, inherit, err := decodeStoredDynamicResourcePool(record)
	require.NoError(t, err)
	require.True(t, inherit)
	require.Equal(t, "spec-pool", cfg.PoolName)
	require.Empty(t, cfg.Description)
	require.Nil(t, cfg.Scheduler)
	require.Nil(t, cfg.TaskContainerDefaults)
	require.Equal(t, 100, cfg.MaxAuxContainersPerAgent)
	require.Equal(t, model.Duration(10*time.Minute), cfg.AgentReconnectWait)

	unsupported := record
	specVersion := 2
	unsupported.SpecVersion = &specVersion
	_, _, err = decodeStoredDynamicResourcePool(unsupported)
	require.ErrorContains(t, err, "unsupported spec version 2")
	unsupported = record
	unsupported.ConfigVersion = 2
	_, _, err = decodeStoredDynamicResourcePool(unsupported)
	require.ErrorContains(t, err, "unsupported config version 2")

	for _, test := range []struct{ spec, message string }{
		{
			`{"pool_name":"spec-pool","task_container_defaults":{"registry_auth":{"unknown":"x"}}}`,
			"unknown field",
		},
		{`{"pool_name":"other-pool"}`, "does not match record name"},
		{`{"pool_name":"spec-pool","provider":{"type":"aws"}}`, "provider"},
		{`{"pool_name":"spec-pool","scheduler":{"type":"round_robin"}}`, "round robin"},
		{`{"pool_name":"spec-pool","max_aux_containers_per_agent":-1}`, ">= 0"},
	} {
		invalid := record
		raw := json.RawMessage(test.spec)
		invalid.Spec = &raw
		_, _, err = decodeStoredDynamicResourcePool(invalid)
		require.ErrorContains(t, err, test.message, test.spec)
	}
}

func TestSpecRowRegistersInheriting(t *testing.T) {
	manager := testDynamicPoolRM()
	registry, err := newPoolRegistry(nil)
	require.NoError(t, err)
	manager.registry = registry
	for _, spec := range []string{
		`{"pool_name":"inherits"}`,
		`{"pool_name":"overrides","task_container_defaults":{"add_capabilities":["CAP_X"]}}`,
	} {
		var named struct {
			PoolName string `json:"pool_name"`
		}
		require.NoError(t, json.Unmarshal([]byte(spec), &named))
		record := testSpecRecord(t, named.PoolName, spec, config.ResourcePoolConfig{
			PoolName: named.PoolName,
		})
		cfg, inherit, err := decodeStoredDynamicResourcePool(record)
		require.NoError(t, err)
		require.NoError(t, registry.addStoredDynamicDesired(cfg, inherit, record.Revision))
		require.NoError(t, registry.publishReady(cfg.PoolName, &resourcePool{config: &cfg}))
	}

	// Master defaults and the RM scheduler change after the pools were registered.
	masterDefaults := *model.DefaultTaskContainerDefaults()
	masterDefaults.ShmSizeBytes = 16 << 30
	masterDefaults.ForcePullImage = true
	*manager.config.Scheduler.Priority.DefaultPriority = 7

	inherited, err := manager.TaskContainerDefaults("inherits", masterDefaults)
	require.NoError(t, err)
	require.Equal(t, masterDefaults, inherited)

	// A pool-level block is parsed with its own defaults, so it resets shm_size_bytes to 4GiB
	// unless the block repeats it, exactly like a master.yaml pool.
	overridden, err := manager.TaskContainerDefaults("overrides", masterDefaults)
	require.NoError(t, err)
	var override model.TaskContainerDefaultsConfig
	require.NoError(t, json.Unmarshal([]byte(`{"add_capabilities":["CAP_X"]}`), &override))
	expected, err := masterDefaults.Merge(override)
	require.NoError(t, err)
	require.Equal(t, expected, overridden)
	require.EqualValues(t, 4<<30, overridden.ShmSizeBytes)
	require.True(t, overridden.ForcePullImage)
	require.Equal(t, []string{"CAP_X"}, overridden.AddCapabilities)

	scheduler, ok := manager.ResourcePoolSchedulerConfig("inherits")
	require.True(t, ok)
	require.Equal(t, 7, *scheduler.Priority.DefaultPriority)
}

func TestSpecSnapshotReadableByLegacyDecoder(t *testing.T) {
	manager := testDynamicPoolRM()
	masterDefaults := *model.DefaultTaskContainerDefaults()
	masterDefaults.ShmSizeBytes = 16 << 30
	for _, spec := range []string{
		`{"pool_name":"plain"}`,
		`{"pool_name":"reconnect","description":"d","agent_reconnect_wait":"10m"}`,
		`{"pool_name":"scheduled","scheduler":{"type":"priority","default_priority":10}}`,
		`{"pool_name":"overrides","task_container_defaults":{"shm_size_bytes":1024,
			"registry_auth":{"username":"u","password":"p"},"add_capabilities":["CAP_X"]}}`,
	} {
		prepared, err := manager.prepareDynamicPoolSpec(json.RawMessage(spec), masterDefaults)
		require.NoError(t, err, spec)

		// This is the record as a master without spec support reads it.
		legacy := db.DynamicResourcePool{
			PoolName:      prepared.config.PoolName,
			ConfigVersion: dynamicResourcePoolConfigVersion,
			Config:        prepared.snapshot.Config,
			ConfigHash:    prepared.snapshot.ConfigHash,
		}
		decoded, inherit, err := decodeStoredDynamicResourcePool(legacy)
		require.NoError(t, err, spec)
		require.False(t, inherit)

		normalized, err := manager.NormalizeDynamicResourcePoolConfig(
			prepared.config, masterDefaults,
		)
		require.NoError(t, err)
		require.Equal(t, normalized, decoded, spec)
		raw, hash, err := marshalDynamicResourcePoolConfig(normalized)
		require.NoError(t, err)
		require.Equal(t, string(raw), string(prepared.snapshot.Config))
		require.Equal(t, hash, prepared.snapshot.ConfigHash)
	}
}

// Snapshots are decoded strictly by masters without spec support. A field added to one of these
// types without omitempty, or renamed, makes those masters refuse to start, so a change here must
// keep every snapshot readable by the oldest master that a cluster may roll back to.
func TestDynamicPoolJSONFieldSetsMatch0401(t *testing.T) {
	fieldTags := func(value interface{}) []string {
		typ := reflect.TypeOf(value)
		tags := make([]string, 0, typ.NumField())
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			tag := field.Tag.Get("json")
			if tag == "" {
				tag = field.Name
			}
			if union := field.Tag.Get("union"); union != "" {
				tag += " union:" + union
			}
			tags = append(tags, tag)
		}
		return tags
	}
	for _, test := range []struct {
		value interface{}
		tags  []string
	}{
		{config.ResourcePoolConfig{}, []string{
			"pool_name", "description", "provider", "scheduler,omitempty",
			"max_aux_containers_per_agent", "task_container_defaults", "agent_reattach_enabled",
			"agent_reconnect_wait", "max_cpu_containers_per_agent,omitempty",
		}},
		{config.SchedulerConfig{}, []string{
			"- union:type,fair_share", "- union:type,priority", "- union:type,round_robin",
			"fitting_policy", "allow_heterogeneous_fits",
		}},
		{config.FairShareSchedulerConfig{}, []string{}},
		{config.PrioritySchedulerConfig{}, []string{"preemption", "default_priority"}},
		{config.RoundRobinSchedulerConfig{}, []string{}},
		{model.TaskContainerDefaultsConfig{}, []string{
			"dtrain_network_interface,omitempty", "nccl_port_range,omitempty",
			"gloo_port_range,omitempty", "shm_size_bytes,omitempty", "network_mode,omitempty",
			"cpu_pod_spec", "gpu_pod_spec", "checkpoint_gc_pod_spec", "image,omitempty",
			"registry_auth,omitempty", "force_pull_image,omitempty",
			"environment_variables,omitempty", "add_capabilities", "drop_capabilities", "devices",
			"bind_mounts", "work_dir", "slurm", "pbs", "startup_hook", "log_policies",
			"preemption_timeout,omitempty", "kubernetes",
		}},
		{registry.AuthConfig{}, []string{
			"username,omitempty", "password,omitempty", "auth,omitempty", "email,omitempty",
			"serveraddress,omitempty", "identitytoken,omitempty", "registrytoken,omitempty",
		}},
		{model.KubernetesTaskContainerDefaults{}, []string{"max_slots_per_pod"}},
		{expconf.SlurmConfigV0{}, []string{
			"slots_per_node,omitempty", "gpu_type,omitempty", "sbatch_args,omitempty",
		}},
		{expconf.PbsConfigV0{}, []string{"slots_per_node,omitempty", "pbsbatch_args,omitempty"}},
		{expconf.LogPolicyV0{}, []string{"name,omitempty", "pattern,omitempty", "action,omitempty"}},
		{expconf.LogActionV0{}, []string{"Type"}},
		{model.RuntimeItem{}, []string{"cpu,omitempty", "cuda,omitempty", "rocm,omitempty"}},
		{model.RuntimeItems{}, []string{"cpu,omitempty", "cuda,omitempty", "rocm,omitempty"}},
		{model.DeviceConfig{}, []string{"host_path", "container_path", "mode"}},
		{model.BindMount{}, []string{"host_path", "container_path", "read_only", "propagation"}},
	} {
		require.Equal(t, test.tags, fieldTags(test.value), reflect.TypeOf(test.value).String())
	}
}

func TestDynamicPoolReadyWriteFailureDoesNotPublish(t *testing.T) {
	originalSetState := setDynamicResourcePoolState
	originalStop := stopPreparedDynamicResourcePool
	t.Cleanup(func() {
		setDynamicResourcePoolState = originalSetState
		stopPreparedDynamicResourcePool = originalStop
	})

	registry, err := newPoolRegistry(nil)
	require.NoError(t, err)
	rm := testDynamicPoolRM()
	rm.registry = registry
	stopped := 0
	stopPreparedDynamicResourcePool = func(pool *resourcePool) {
		stopped++
		pool.stop()
	}
	setDynamicResourcePoolState = func(
		_ *db.PgDB,
		_ context.Context,
		poolName string,
		state db.DynamicResourcePoolState,
		errText *string,
	) (db.DynamicResourcePool, error) {
		record := db.DynamicResourcePool{PoolName: poolName, State: state, Error: errText}
		if state == db.DynamicResourcePoolReady {
			return record, errors.New("database unavailable")
		}
		return record, nil
	}

	cfg, err := rm.NormalizeDynamicResourcePoolConfig(config.ResourcePoolConfig{
		PoolName:                 "write-failure",
		MaxAuxContainersPerAgent: 100,
	}, *model.DefaultTaskContainerDefaults())
	require.NoError(t, err)
	raw, hash, err := marshalDynamicResourcePoolConfig(cfg)
	require.NoError(t, err)
	record, err := rm.initializeDynamicResourcePool(context.Background(), db.DynamicResourcePool{
		PoolName:      cfg.PoolName,
		ConfigVersion: dynamicResourcePoolConfigVersion,
		Config:        raw,
		ConfigHash:    hash,
		State:         db.DynamicResourcePoolPending,
	}, cfg, false)
	require.ErrorIs(t, err, ErrDynamicResourcePoolPersistence)
	require.Equal(t, db.DynamicResourcePoolFailed, record.State)
	require.Equal(t, 1, stopped)
	require.False(t, rm.IsDynamicResourcePoolReady(cfg.PoolName))
}
