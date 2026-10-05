# GPU topology-aware allocation and GPU health view (design, revision 4)

Opt-in topology-aware GPU set selection per task, plus a per-GPU topology and health view in the API, `det agent list` / `det agent describe` and the WebUI. The feature ships after 0.41.0.

Status: design for review. The implementation follows in the same PR once the design is approved. What changed in each revision is under "Revisions" near the end. The owner decisions, most of them still open, are under "Owner decisions".

- **Code references.** File:line references are at WU-CVGL/determined `wu/main` dd3be1fce unless another repository is named.
  - PR #35 (dynamic pools inherit, update, adopt; head 3ce0d87b9) and the 0.41.0 release (PR #36, f8fa7b904, stacked on #35) change only three of the cited files.
  - core.go: only `agentrm.New` at lines 1093 and 1138, with the same line count, so the cited core.go lines stay valid.
  - agentrm/agent_resource_manager.go: the cited code is unchanged, but its lines shift (for example :171 is :220 on the release).
  - .github/workflows/fork-release.yml: only the default release tag changes; the cited lines stay valid.
  - No other cited file changes, so this work does not conflict with #35 or #36.
- **Cluster facts.** Topology, P2P status, PCIe link width and generation, and NUMA nodes were measured on the cluster's nodes with `nvidia-smi topo -m`, `nvidia-smi topo -p2p r`, `nvidia-smi --query-gpu=index,pci.bus_id,name,pcie.link.gen.max,pcie.link.width.current,pcie.link.width.max` and sysfs. The facts this design uses are stated inline. Benchmarks and monitoring come from the public cluster-setup repository, WU-CVGL/cluster-setup at 5b02037: `docs/05_GPU_P2P_GeForce.md` (cited as docs/05:<line>) and the Prometheus and Grafana configuration under `services/`.
- **Owner requirements.** R1-R7 in section 0, stated inline.
- **go-nvml.** Facts were checked against tag v0.12.9-0 (`pkg/nvml/zz_generated.api.go`, `const.go`, `init.go`, `return.go`, `lib.go`, `nvml.go`).

---

## 0. Scope, promises and invariants

The feature has two parts that share one data path (agent NVML collection, then the agent message, then master state, then the API):

1. **Scheduling.** A per-task flag `resources.prefer_gpu_topology` (bool, unset = false). The owner decided that it stays off by default, because not every multi-GPU task is DDP.
   - Flag off: the scheduler behaves exactly as today.
   - Flag on: a soft preference. The task never waits for a better set, running work is never moved, and an agent whose topology is unknown still gets the task, with today's device choice.
   - Inside the agent that today's ranking picks, the flag chooses which free GPUs the task gets. That never changes any slot count.
   - The flag can also move the task to another agent, but only in a pool where every enabled, non-draining agent has the same number of slots and whose scheduler does not preempt, and only to an agent with the same number of free slots as today's choice. Within that scheduling pass this never changes whether any other task fits; requests that exclude nodes (log-pattern `exclude_node` policies) are the exception and are retried (section 8.3). In every other pool the agent is today's choice (section 7).
   - Across passes the guarantee is weaker. Which of two equally full agents holds the task can matter later, when a task on one of them finishes. Today's tie-break by allocation-id hash has the same property; the flag replaces that arbitrary choice with a topology-informed one.
2. **Visibility and health.** Every agent that has CUDA slots reports per GPU:
   - the UUID, PCI bus id and NUMA node;
   - the current and maximum PCIe link width and generation;
   - pairwise link level, NVLink count and P2P status;
   - NVML errors seen at collection.

   GPUs that are deliberately left out of an agent through its exclude list (D24) are reported the same way, but never become slots. They are shown as "excluded".

   The master adds recent critical XIDs from the cluster's DCGM-Exporter, when the Prometheus integration is configured, and classifies each GPU as healthy, link downgraded, error or unknown. The result is shown in the agent API, in `det agent list` (summary columns), in `det agent describe` (per-GPU detail) and on the WebUI resource-pool page, using the owner's colour encoding.

Owner requirements, all in scope:

- R1 P2P capability comes only from what the agent measures with NVML at process start, never from the GPU model (decided by the owner). After a driver change (g292 getting the patched open modules), an agent restart is enough, with no code or config change.
- R2 Topology in the API, the WebUI (cluster/agent views) and the CLI (`det agent list` summary plus per-GPU detail).
- R3 Per GPU, current and maximum PCIe link width and generation (NVML `DeviceGetCurr/MaxPcieLinkWidth`, `DeviceGetCurr/MaxPcieLinkGeneration`).
  - Downgrade means current width < maximum width.
  - A lower current generation is information only, because idle GPUs lower the link speed.
  - Width is collected once per agent start.
- R4 WebUI encoding (approved by the owner after a preview):
  - the tile fill keeps the slot-state palette (free, pending, active);
  - a separate LED-style health dot sits on every GPU tile: green = healthy, amber = width below max, red = error, hollow gray outline = unknown, each with a contrasting border, in theme status colours;
  - disabled is a striped tile with a "disabled" label, never a fill colour;
  - an info button next to the dot shows details on hover and on click;
  - unknown topology is stated as text;
  - a GPU excluded from the agent (D24) is a striped tile labelled "excluded", without a slot id.
- R5 A concrete definition of "errored" (section 3).
- R6 Downgrades and errors are shown in `det agent list` and in the per-GPU detail.
- R7 P2P capability is taken into account in the score (P2P-capable sets first; PCIe locality treated as neutral inside a NUMA node when P2P is unsupported), and the docs say so.

Invariants, each with a test (see Tests):

- **I1 Viability is unchanged.** `findFits` (fitting.go:72-94) returns a fit for a request exactly when it does today. The flag only chooses among viable candidates and devices. Callers that use findFits only for viability depend on this: fair_share.go:96/170/388 and the priority preemption checks at priority.go:149/236.
- **I2 Flag off, or SlotsNeeded < 2: the code path is identical to today.**
  - `candidateList.Less` (fitting.go:46-66) is not edited, and `findSharedAgentFit` sorts exactly as today.
  - `findFits` gains one bool parameter, the pool's "no preemption" flag (section 7.1). With the flag off, or SlotsNeeded < 2, it is never read.
  - The `allocateFreeDevices` message carries `preferTopology=false`, which calls the unedited `agentState.allocateFreeDevices` (agent_state.go:158-186).
  - The scheduler-tick retry rule (section 8.3) does nothing in a tick without an opted-in request.
- **I3 Count interchangeability, only where it is sufficient.** The cross-agent tie-break runs only when both hold:
  - the pool is homogeneous: every enabled, non-draining agent in the agent map the fit is given has the same `len(Devices)`;
  - the pool's scheduler does not preempt (`!SchedulerConfig.GetPreemption()`, scheduler_config.go:103-114; preemption is off by default, :32).

  Then the chosen agent has the same `numSlots()` and `numEmptySlots()` as the flag-off agent W. In a homogeneous pool the Scores, the slot constraints and the idle-agent grouping in fitting.go depend on `numEmptySlots()` alone, and HashDistance only picks among agents with equal values (section 7.2), so within the same scheduling pass every later request fits exactly when it fits with the flag off, and the free-count multiset after the pass is the same.
  - Otherwise the agent is W, and only the device set inside W is chosen. That changes no count.
  - The property the priority scheduler relies on is that every request its simulation put into `toAllocate` also fits in the real allocation. It holds after this change in exactly the cases where it holds today (section 7.2).
  - The opted-in task may land on a different agent than in the simulation, but only on one that is identical in count terms in a homogeneous, non-preempting pool. Its device set is chosen from the live state.
  - The guarantee covers one scheduling pass. Across passes, which agent holds the task can matter when another task finishes, as with today's hash tie-break (section 0).
  - Exception inside a pass: requests with `BlockedNodes` (section 7.2), covered by the retry in 8.3.
- **I4 `agentState.allocateFreeDevices` is not edited.** The explicit-set path is a new function that validates before any mutation.
- **I5 Unknown topology everywhere:** the same agent and today's device choice as with the flag off.

Explicitly not done:

- no DB persistence or migration of topology or health;
- no new master configuration keys, and one new agent option only, the GPU exclude list (D24); the XID lookup reuses the existing `integrations.task_resources` settings;
- no new agent-to-master message type (section 3.4 says why);
- no pool-level or task_container_defaults default and no master kill switch;
- no WebUI launch-form checkbox;
- no Kubernetes or dispatcher RM support, since they read only SlotsNeeded;
- no change to how tasks without the flag pick devices;
- no fixes for the pre-existing quirks listed in owner decision D17.

---

## 1. Per-task field (name, location per task type, merge behaviour)

Name: `resources.prefer_gpu_topology`. Type boolean or null, schema default null, read as false unless it is exactly true.

**a) Experiments**
- In schemas/expconf/v0/resources.json, add a property (alphabetical, between max_slots and priority; additionalProperties is false):
  `"prefer_gpu_topology": {"type": ["boolean","null"], "default": null}`
- In master/pkg/schemas/expconf/experiment_config.go `ResourcesConfigV0` (203-217), add `RawPreferGPUTopology *bool \`json:"prefer_gpu_topology,omitempty"\``.
  - `omitempty` limits the rollback damage to experiments that set the key (section 14.3).
- Add a hand-written helper next to the struct:
  `func (r ResourcesConfigV0) GPUTopologyPreferred() bool { return r.RawPreferGPUTopology != nil && *r.RawPreferGPUTopology }`
- Regenerate with `make -C master gen`. This updates zgen_resources_config_v0.go, which gets a getter that cannot panic because the default is null, and zgen_schemas.go. `make -C master check-gen` must be clean.

**b) Commands, notebooks, shells, TensorBoards (NTSC)**
- In master/pkg/model/experiment_config.go `ResourcesConfig` (85-97), add `PreferGPUTopology *bool \`json:"prefer_gpu_topology,omitempty"\``.
  - This is required: api_command.go:133-142 decodes the merged config with `DisallowUnknownFields`.
- In master/pkg/model/compat.go `ToExpconf` (28-46), add `RawPreferGPUTopology: r.PreferGPUTopology`.
- Do NOT copy the is_single_node ban (command_config.go:71). `det cmd run` with torchrun is a real DDP path.

**c) Generic tasks**
- GenericTaskConfig.Resources is `expconf.ResourcesConfig`, so (a) covers it. Decoding is strict (api_generic_tasks.go:128), so the schema property is required.
- Read the field nil-safely, because WithDefaults is never applied to generic task configs.

**d) Scheduler carrier.** master/internal/sproto/scheduler.go `FittingRequirements` gets `PreferGPUTopology bool`, set at the seven construction sites:

| Site | Value |
|---|---|
| trial.go:422 (restore) and :470 (new) | `t.config.Resources().GPUTopologyPreferred()` |
| command/command.go:171 | `{SingleAgent: true, PreferGPUTopology: c.Config.Resources.PreferGPUTopology != nil && *c.Config.Resources.PreferGPUTopology}` |
| api_generic_tasks.go:399, generic_task_resume.go:379, core.go:955 | the spec's `Resources.GPUTopologyPreferred()` |
| checkpoint_gc.go:181 | stays false |

- Restore requests are never fitted (resource_pool.go:170-230); they carry the flag only for consistency.
- `ValidateResourcesRequest` is unchanged, because a soft preference never changes feasibility.

**e) Merge and template behaviour** (no code needed)
- Experiments: user config, then the template merge (core_experiment.go:293-300; `schemas.Merge` takes the template's value only where the user's is nil), then WithDefaults, then the invariant config policies (task_config_policy.go:191-209, invariant wins).
  - An explicit user `false` overrides a template `true`.
  - A workspace or global invariant config can force the flag for experiments.
- NTSC: task_container_defaults, then the template (lenient), then the request config. The user's explicit value wins.
- Generic tasks: no templates.

**f) Clients:** no CLI or SDK code changes.
- `det e create --config resources.prefer_gpu_topology=true` works, and the same flag works for `det cmd run | shell start | notebook start | tensorboard start` and `det task create`, and in SDK config dicts.
- The WebUI full-config YAML mode passes the key through.
- A new client against an old master fails loudly: a schema error for experiments, an unknown-field 400 for NTSC.

---

## 2. Agent: topology, link and error collection with go-nvml

### 2.1 Hook and lifetime
- In agent/internal/agent.go, right after `detect.Detect` (115-120), call `topo := detect.DetectGPUTopology(devices, excluded)`. `excluded` is the list of GPUs left out by the exclude list (2.4), empty by default. The call never returns an error and never fails agent start.
- Put `GPUTopology: topo` into `AgentStarted` (160-165).
- Pass `topo` through `reconnectFlow` (call at 208, signature at 307, send at 352-357). A reconnect re-sends the value collected at process start; a new measurement happens on every agent process start. That covers R1 and the "width is decided at boot" premise of R3.
- **Timeout.** Collection runs in a goroutine, and the agent waits at most 60 s for it. On timeout it sends `UnknownReason: "NVML collection did not finish within 60s"` and starts normally.
  - The goroutine is left blocked, because a cgo call cannot be cancelled.
  - Today's `nvidia-smi` calls in detect have no timeout either (nvidia.go:33, :71), so a GPU that hangs NVML usually hangs detection first. The timeout only guarantees that the new code adds no hang of its own.

### 2.2 New files in agent/internal/detect
**topology.go** (no build tag): `DetectGPUTopology(devices, excluded []device.Device) *aproto.GPUTopology`
- No CUDA device (cpu, rocm, artificial slots): nil.
- Any UUID with prefix "MIG-": `&GPUTopology{UnknownReason: "MIG instances: GPU topology not collected"}`.
- Otherwise `collectWithTimeout(uuids, excludedUUIDs, 60*time.Second)`, using exactly the detected slot UUIDs plus the excluded UUIDs (2.4), and no other GPU. This respects `visible_gpus` and node01's current 7-UUID `--gpus device=...` passthrough, where in-container slots 0..6 are host GPUs 0,1,2,3,5,6,7.

**topology_nvml.go** (`//go:build linux && cgo`) imports `github.com/NVIDIA/go-nvml/pkg/nvml`.
- `collect(lib nvml.Interface, uuids, excludedUUIDs []string, numa func(bdf string) *int, now func() time.Time) *aproto.GPUTopology`
- `collectNVML = collect(nvml.New(), uuids, excludedUUIDs, readSysfsNUMA, time.Now)`
- Excluded GPUs go through the same steps as slot GPUs and get `Excluded: true` (section 4). Pairs are formed over all reported GPUs.

The collection steps:
1. `ret := lib.Init()`.
   - Not SUCCESS: return `UnknownReason: "NVML init: " + nvmlReturnString(ret)`, for example `NVML init: ERROR_LIBRARY_NOT_FOUND (12)`. go-nvml's Init returns ERROR_LIBRARY_NOT_FOUND when dlopen of libnvidia-ml.so.1 fails (init.go:20-24). All GPUs are then *unknown*, not *error*: a missing library is a deployment problem, not a GPU fault.
   - Otherwise `defer lib.Shutdown()`.
   - Set `CollectedAt = now()` and `DriverVersion` from `lib.SystemGetDriverVersion()` (best effort). This makes g292's later driver change visible.
2. Per UUID, apply the **return-code rule** to the *health calls*: the handle lookup, `GetPciInfo` and the four link width and generation calls.
   - SUCCESS: use the value;
   - ERROR_NOT_SUPPORTED: leave the field at its unknown zero value, with no error;
   - anything else (ERROR_GPU_IS_LOST, ERROR_UNKNOWN, ERROR_TIMEOUT, ...): leave the field unknown and append `"<call>: " + nvmlReturnString(ret)` to that GPU's `Error`, for example `GetCurrPcieLinkWidth: ERROR_GPU_IS_LOST (15)`.

   The calls:
   - `lib.DeviceGetHandleByUUID(uuid)` (health call). On failure, record the GPU with only UUID and Error, then go on to the next UUID.
   - `dev.GetPciInfo()` (health call): the bus id from the C char array, NUL-trimmed and normalised to the 4-hex-domain lowercase sysfs form ("00000000:A1:00.0" becomes "0000:a1:00.0").
   - NUMA node from `/sys/bus/pci/devices/<bdf>/numa_node`; -1 or a read error means unknown, with no error. Do NOT use `DeviceGetNumaNodeId`: on this cluster's nodes it is not supported (`nvidia-smi -q` shows "GPU NUMA ID N/A").
   - `dev.GetCurrPcieLinkWidth()`, `dev.GetMaxPcieLinkWidth()`, `dev.GetCurrPcieLinkGeneration()`, `dev.GetMaxPcieLinkGeneration()` (health calls).
     - Use the *device and system* maximum (`DeviceGetMaxPcieLinkGeneration`/`Width`, zz_generated.api.go:133-134), the same as `nvidia-smi pcie.link.gen.max` / `width.max`. It is not `DeviceGetGpuMaxPcieLinkGeneration` (:109).
     - So a card in a slot that is physically x8 has max 8 and is not called downgraded. The cluster's x8 cards (on node01, node05, node06 and node07) report max 16, so they are real downgrades.
   - **NVLink probe** (not a health call; it never sets `Error`). For each link in 0..`nvml.NVLINK_MAX_LINKS`-1 (18 at this tag, const.go:48): call `GetNvLinkState(link)`; if it is FEATURE_ENABLED, call `GetNvLinkRemotePciInfo(link)`, and if the remote bus id is one of our GPUs, count the link for that pair.
     - The loop stops at the first non-SUCCESS return from either call and logs it at Debug.
     - Why: NVML answers only for the links a device has. A GPU with NVLink hardware returns ERROR_INVALID_ARGUMENT (2) for higher link indices, and a GPU without NVLink returns ERROR_NOT_SUPPORTED for link 0. The RTX 3090 on nodes 01, 03 and 04 has four NVLink links and no bridge (`nvidia-smi topo -m` shows no NV# links), so links 0-3 are expected to answer FEATURE_DISABLED and link 4 ERROR_INVALID_ARGUMENT. Under the return-code rule that would turn every 3090 red. The Phase 1 check confirms what a 3090 reports (section 15).
3. Per unordered pair:
   - `devA.GetTopologyCommonAncestor(devB)`, mapped one to one (const.go:1193-1198):

     | NVML `GpuTopologyLevel` | Value | Wire `GPULinkLevel` |
     |---|---|---|
     | TOPOLOGY_INTERNAL | 0 | INTERNAL |
     | TOPOLOGY_SINGLE | 10 | PIX |
     | TOPOLOGY_MULTIPLE | 20 | PXB |
     | TOPOLOGY_HOSTBRIDGE | 30 | PHB |
     | TOPOLOGY_NODE | 40 | NODE |
     | TOPOLOGY_SYSTEM | 50 | SYS |
     | any other value | | "" (unknown) |

   - `devA.GetP2PStatus(devB, nvml.P2P_CAPS_INDEX_READ)`, mapped one to one by value (const.go:1206-1213):

     | NVML `GpuP2PStatus` | Value | Wire `GPUP2PStatus` |
     |---|---|---|
     | P2P_STATUS_OK | 0 | OK |
     | P2P_STATUS_CHIPSET_NOT_SUPPORED and P2P_STATUS_CHIPSET_NOT_SUPPORTED (both spellings exist) | 1 | CHIPSET_NOT_SUPPORTED |
     | P2P_STATUS_GPU_NOT_SUPPORTED | 2 | GPU_NOT_SUPPORTED |
     | P2P_STATUS_IOH_TOPOLOGY_NOT_SUPPORTED | 3 | TOPOLOGY_NOT_SUPPORTED |
     | P2P_STATUS_DISABLED_BY_REGKEY | 4 | DISABLED_BY_REGKEY |
     | P2P_STATUS_NOT_SUPPORTED | 5 | NOT_SUPPORTED |
     | P2P_STATUS_UNKNOWN | 6 | "" (unknown) |
     | any other value | | "" (unknown) |

     The switch is on the integer value, because the two spellings of value 1 cannot both be case labels.
   - A pairwise failure (any non-SUCCESS return) makes only that pair unknown and never sets a GPU's `Error`. GPU errors come only from the health calls.
   - **Zero-value trap:** TOPOLOGY_INTERNAL and P2P_STATUS_OK are both 0. A value is used only when its Return is SUCCESS.
4. Log one Info line, for example `GPU topology: 8 GPUs, NUMA 4+4, levels NODE/SYS, P2P OK, links below max width: slot 1 (x8 of x16), NVML errors: none, driver 610.57.04`. Log a Warn line with the reason when the topology is unknown or a GPU has an error.

**Formatting NVML returns.** Every NVML return that reaches a string (UnknownReason, `GPUInfo.Error`, log lines, the subcommand's JSON) goes through the two local helpers below (topology_nvml.go), never through `ret.String()`, `ret.Error()` or `%v`/`%s` of a Return.
- Why: go-nvml's `Return.String()` and `Error()` call a package variable (return.go:27-37). It starts as a built-in name table (`defaultErrorStringFunc`, return.go:42-102), but once the library is loaded go-nvml points it at NVML's own `nvmlErrorString` (lib.go:116-117), which returns prose such as "GPU is lost". In production every return after a successful dlopen would therefore print prose, including an Init failure such as ERROR_DRIVER_NOT_LOADED.
- `nvmlReturnName(ret) string`: the symbolic name from a local table of every `Return` constant in const.go:1504-1534 at v0.12.9-0, generated from const.go rather than copied from `defaultErrorStringFunc`, which lacks ERROR_NOT_READY (27), ERROR_GPU_NOT_FOUND (28) and ERROR_INVALID_STATE (29). A value outside the table gives `UNKNOWN_RETURN`.
- `nvmlReturnString(ret) string` = `nvmlReturnName(ret) + " (" + strconv.Itoa(int(ret)) + ")"`, for example `ERROR_GPU_IS_LOST (15)`.
- The agent collection, the `gpu-topology` subcommand and the tests all use these two functions.

**topology_nvml_stub.go** (`//go:build !linux || !cgo`): `collectNVML` returns `{UnknownReason: "agent built without NVML support (needs linux and cgo)"}`.
- This guard is required. With CGO_ENABLED=0 an unguarded go-nvml import does not compile, and the darwin/arm64 and other CGO_ENABLED=0 builds need the stub.

go-nvml names verified at v0.12.9-0:
- New/Interface, Init, Shutdown, SystemGetDriverVersion, DeviceGetHandleByUUID;
- GetPciInfo, GetCurr/MaxPcieLinkWidth, GetCurr/MaxPcieLinkGeneration;
- GetNvLinkState, GetNvLinkRemotePciInfo, GetTopologyCommonAncestor, GetP2PStatus;
- ERROR_INVALID_ARGUMENT=2, ERROR_NOT_SUPPORTED=3, ERROR_LIBRARY_NOT_FOUND=12, ERROR_GPU_IS_LOST=15;
- the `pkg/nvml/mock` package;
- cgo flags: `--unresolved-symbols=ignore-in-object-files` on Linux, plus `-ldl`.

NVML inside the agent container:
- Every agent's DeviceRequest carries the `utility` capability, and the NVIDIA container toolkit injects libnvidia-ml.so.1.
- Do NOT bundle libnvidia-ml in the image.
- That the NVML and sysfs NUMA reads work inside the container, and node01's slot-to-host mapping, are checked on a candidate image before merge (section 15, Phase 1).

### 2.3 Diagnostic subcommand
`determined-agent gpu-topology [--visible-gpus X] [--slot-type T] [--exclude-gpus U]`, in agent/cmd/determined-agent/gpu_topology.go, registered in root.go `AddCommand`.

1. It **always** calls `lib.Init()` first, before and independent of device detection, and prints `"nvml_init": nvmlReturnName(ret)` and `"nvml_init_code": int(ret)` (the name without the code suffix, since the code has its own field).
   - On SUCCESS it also prints `driver_version` and calls Shutdown.
   - The stub build prints `"nvml_init": "NOT_BUILT"` and `"nvml_init_code": -1`.
   - The local name table (2.2) makes the output the same whether or not the library loaded: a failed dlopen prints exactly `ERROR_LIBRARY_NOT_FOUND` and 12, and an Init failure after a successful dlopen prints its symbolic name too, not NVML's prose.
   - The topology part of the JSON carries the same `NAME (code)` strings as the agent.
2. It runs the same detection as the agent (the exclude list is applied afterwards, see 3) and the same collection, and prints `{"nvml_init", "nvml_init_code", "driver_version", "devices", "excluded", "topology"}` as JSON.
3. It exits 0 even when NVML is missing; it exits non-zero only on flag errors. An exclude entry that matches no GPU is printed as `"exclude_error"` instead of stopping the command, so the output still shows which UUIDs exist. The subcommand detects without the list and then applies the same split as the agent (2.4); on a mismatch it prints the detected devices unchanged and an empty `excluded`.

It is used three ways:
- by the release check (section 13), where step 1 makes a GPU-less runner exercise dlopen in the real image;
- for the candidate-image check (Phase 1);
- for operator debugging.

### 2.4 Excluded GPUs (owner decision D24)
Today a faulty GPU, such as node01's card at bus id 81:00.0, is hidden by starting the agent container with `docker run --gpus device=<UUIDs of the other GPUs>`. The agent cannot see that GPU at all, so no view shows it. With an exclude list the agent sees every GPU, reports the excluded ones, and never offers them as slots.

- **Option.** New agent option `exclude_gpus`: a comma-separated list of GPU UUIDs.
  - It is the config key `exclude_gpus`, the flag `--exclude-gpus` and the environment variable `DET_EXCLUDE_GPUS` (the existing flag-to-env naming, init.go:23-25). It is registered next to `visible-gpus` (init.go:103), and the field sits next to `VisibleGPUs` (options.go:58).
  - UUIDs only, never indices, so a changed numbering cannot bring the GPU back.
  - The agent container is started with all GPUs (`--gpus all` with the `gpu,utility` capabilities).
- **Detection.** `detect.Detect` (detect.go:19) gets the list. After CUDA detection it moves every device whose UUID is in the list to a separate `excluded` list, with a split helper that the `gpu-topology` subcommand also uses (2.3).
  - The remaining devices keep their nvidia-smi index as device ID (nvidia.go:97-107), as with `visible_gpus` today. node01's slots then become host indices 0-3 and 5-7, instead of 0-6 behind today's device list. Gaps are safe: task containers get their GPUs by UUID (containers/spec.go:85-97), and the harness uses `DET_SLOT_IDS` only for its length (harness/determined/_info.py:278).
  - **Fail closed.** An entry that matches no detected CUDA GPU stops agent start with an error that names the entry. A typo must never hand the faulty GPU to tasks.
- **Never a slot.** Excluded GPUs are not in `AgentStarted.Devices`, so the master never schedules them. Task containers get their GPUs by the UUIDs of their slots (containers/spec.go:85-97), so no task container can request an excluded GPU.
- **Collection.** The agent queries excluded GPUs with the same NVML calls as slot GPUs (2.2) and reports their topology, link width and generation, NVML errors and links to the slot GPUs.
  - The owner confirmed that querying the faulty card is fine as long as no workload runs on it.
  - The 60 s timeout (2.1) bounds only the NVML collection step.
  - Device detection runs before it and is not bounded. Its nvidia-smi calls (2.1; nvidia.go:33, :71) now also see the excluded card and have no timeout.
    - A hang there blocks agent start, with no limit.
    - An nvidia-smi error makes detectCudaGPUs return no devices (nvidia.go:76-79). The exclude entry then matches nothing, and the fail-closed rule stops the whole agent: node01 loses all 7 good slots, not only the faulty GPU.
    - Today's docker device list cannot fail this way, because the card is not visible inside the agent container.
    - Mitigation: the Phase 1 node01 check (section 15) runs the same detection with all GPUs and the exclude list.
    - Fallback: the docker device list, if that check hangs or reports `exclude_error`, or if the card gets worse later.
- **Changing the list** changes the agent's device set. Follow the existing rules for that: drain the agent first, because the master stops an agent whose devices changed on reconnect (agent_state.go:261-292).

---

## 3. GPU health: states and the definition of "errored"

### 3.1 States (computed once, in the master API layer, section 10.3)
Per GPU, with the earliest matching rule winning:

| State | Condition | WebUI dot | CLI |
|---|---|---|---|
| ERROR | the agent reported an NVML error for this GPU at its last start (`GPUInfo.Error` non-empty), OR DCGM reported a critical XID for this GPU's UUID within the lookback window (3.3) | red, filled | `error` |
| LINK_DEGRADED | current and max width both known and current < max | amber, filled | `narrow` |
| HEALTHY | topology known for the agent, current and max width both known and current == max | green, filled | `ok` |
| UNKNOWN | anything else: the agent did not report (old agent, master just restarted), NVML init failed or timed out, stub build, MIG, or width unknown for this GPU | hollow gray outline | `unknown` |

- The PCIe **generation never changes the state.** The details show `current Gen<c> of Gen<m> at agent start`, with the note that GPUs lower the link speed when idle. A lower generation at idle is normal; to judge the generation, look under load (`nvidia-smi -q -d PCIE` while a job runs).
- When the XID lookup is not configured or fails, the dot still follows the other rules: a GPU can be green. The details then say `XID check: not configured` or `XID check failed: <error>` (owner decision D11). This keeps "healthy" and "unknown" apart: unknown always means the link and topology data is missing.
- Excluded GPUs (D24) are classified with the same rules and show their own dot. Their state never affects any slot.

### 3.2 Errored, part 1: NVML at agent start (always on)
A GPU is errored when, during collection at agent start, one of its health calls (`DeviceGetHandleByUUID`, `GetPciInfo`, or one of the four link width and generation queries) returns anything other than SUCCESS or ERROR_NOT_SUPPORTED. For example, ERROR_GPU_IS_LOST (const.go:1519) means the GPU has fallen off the bus.

- The NVLink probe and the pairwise calls never make a GPU errored (2.2): NVML rejects link indices a device lacks, and a pairwise failure only makes that pair unknown.
- The details name the call, the symbolic code and its number (for example `GetCurrPcieLinkWidth: ERROR_GPU_IS_LOST (15)`), plus the collection time.
- Why this signal: it is the only per-GPU fault the agent sees without a new message type (3.4). It needs no extra code beyond the collection, and it uses the same `CollectedAt` as the link data.
- What it covers is honestly narrow. When `nvidia-smi` exits with an error, `detectCudaGPUs` returns no devices at all (nvidia.go:74-79), and with slot type `auto` the agent falls back to CPU (detect.go:78-100). So a GPU that is already lost before the agent starts usually never reaches NVML collection.
  - The agent then registers fewer or no GPU slots.
  - On a reconnect, the device-count check stops the agent (agent_state.go:274-275, agent.go:614-626).
  - DCGM's existing `gpu-missing` Grafana alert covers that case.
- Part 1 catches GPUs that `nvidia-smi` still lists but NVML cannot query.

### 3.3 Errored, part 2: recent critical XIDs from DCGM-Exporter (master side, when configured)
**Source.** The cluster already scrapes DCGM-Exporter into Prometheus:
- job `dcgm`, label `det_cluster=cvgl`, `node`, `gpu`, `gpu_uuid` (cluster-setup services/prometheus/prometheus.yml:72-106);
- it alerts on `DCGM_EXP_XID_ERRORS_COUNT` with the class "every code except the application class 13, 31, 43, 45" (cluster-setup services/grafana/provisioning/alerting/gpu-health.yaml, rule `gpu-xid-critical`).

The master already queries that Prometheus for task charts:
- `integrations.task_resources.prometheus_url` / `det_cluster` (master/internal/config/task_resources.go);
- `queryTaskPrometheus`, a range query with a 10 s client timeout and an 8 MiB body limit (core_task_resources.go:29-44, 293);
- series labels `job="dcgm",det_cluster=...,gpu_uuid!=""` (core_task_resources.go:265-267).

So this source is cheap (one PromQL range query, reusing the existing client and labels) and reliable (it is the signal the cluster's own XID alert trusts).

**Query** (new `master/internal/gpu_health_xid.go`):
```
max by (gpu_uuid, xid) (
  max_over_time(
    DCGM_EXP_XID_ERRORS_COUNT{job="dcgm", det_cluster="<det_cluster>", gpu_uuid!="",
                              xid!="", xid!="0", xid!~"13|31|43|45"}[5m]
  )
) > 0
```
- Range: [now − 24 h, now], step 300 s, which is 289 points, inside `taskResourceMaxPoints` (1440).
  - Why `max_over_time(...[5m])`: a range query evaluates the expression only at the step timestamps, and a bare selector takes the latest sample at or before each of them. `DCGM_EXP_XID_ERRORS_COUNT` counts XIDs in a sliding 5-minute window, but its samples arrive at the scrape interval, so an event whose non-zero samples all fall between two evaluation points 300 s apart can be missed at the window boundary. With a 5-minute range at a 300 s step, consecutive windows tile the whole 24 h, so every non-zero sample is seen by some step.
  - Never use `DCGM_FI_DEV_XID_ERRORS`: it holds the last code until another arrives or the exporter restarts (gpu-health.yaml header).
- Per (gpu_uuid, xid), `first_seen` and `last_seen` are the first and last step timestamps whose value is > 0. A step can trail the event by up to about 10 minutes (the exporter's 5-minute window plus the 5-minute range), and the details say "around".
- The excluded codes are a Go constant with a comment: application-caused XIDs, the same class as the cluster alert. Owner decision D10.

**When it runs:**
- only if `Integrations.TaskResources.Enabled()`;
- only for `GetAgent`, and for `GetAgents` with `exclude_slots=false`. The WebUI cluster store polls with `excludeSlots` defaulting to true (apiConfig.ts:519) and never triggers it. The pool page and `det agent list` do trigger it.
- It shares one in-process result cache with a 30 s TTL (one query for all agents) and a 5 s context timeout.
  - The pool page polls every 5 s (usePolling.ts:33, ResourcepoolDetail.tsx:106/152), which gives at most one Prometheus query per 30 s per master.
  - On error, the cached failure is kept for 30 s too, and `GpuTopology.xid_check_error` carries the message. Nothing fails.
- The join is by UUID. It therefore works even when the agent's topology is unknown, because GPU UUIDs come from the slots (device.Device.UUID). Excluded GPUs (D24) are joined by the UUID the agent reports for them, so their XIDs show as well.

### 3.4 Why there is no agent-side runtime health (and no new agent message)
NVML event subscription for critical XIDs (`EventSetCreate`, `DeviceRegisterEvents`, `EventTypeXidCriticalError`=8 at this tag) would need the agent to send health updates while it runs. That requires a new agent-to-master message.

An older master decodes such a message into a `MasterMessage` whose fields are all nil. It then reaches the `default:` case of `HandleIncomingWebsocketMessage`, which is `check.Panic` (agent.go:685-687, check.go:25-28) and panics the master. A mixed rollout (new agent, old master) would crash the master. Runtime health therefore comes from the master side (DCGM), and the agent message is extended only with `json:",omitempty"` fields of the existing `AgentStarted`.

### 3.5 Out of reach (stated in docs and in the details popover)
- **GPUs hidden from the agent container.** Today node01's faulty GPU at 81:00.0 is hidden with `docker run --gpus device=<UUIDs>`, so the agent cannot see it and no view shows it. DCGM sees it (the host exporter has all 8 GPUs), but mapping DCGM's `node` label (the node's DNS name) to an agent id is environment-specific. The host-level `gpu-missing` and XID alerts in Grafana cover it. With the exclude list (D24) the agent reports the GPU instead: it is shown as "excluded" with its link data, NVML errors and XIDs.
- Clusters without `integrations.task_resources`: only part 1 applies.
- A link that retrains, or a GPU lost, while the agent runs: the link data and the NVML errors are a snapshot taken at agent start. A later XID is caught by part 2, and the next agent start re-measures.
- Application-class XIDs (13, 31, 43, 45) are not errors here, although on this cluster Xid 31 also appeared in the UVM BAR1 fault (docs/05:93). That fault was followed by Xid 154, which is counted.

---

## 4. Wire format (agent to master, JSON)

New master/pkg/aproto/gpu_topology.go, shared by the agent and the master (no proto). The zero value of every enum is "", meaning unknown. JSON tags are snake_case.
- `type GPULinkLevel string`: "" | INTERNAL | PIX | PXB | PHB | NODE | SYS
- `type GPUP2PStatus string`: "" | OK | CHIPSET_NOT_SUPPORTED | GPU_NOT_SUPPORTED | TOPOLOGY_NOT_SUPPORTED | DISABLED_BY_REGKEY | NOT_SUPPORTED
- `GPUInfo{UUID; PCIBusID; NUMANode *int; PCIeLinkWidth int; PCIeLinkWidthMax int; PCIeLinkGen int; PCIeLinkGenMax int; Error string; Excluded bool}`. All fields except UUID are `omitempty`, and 0 or nil means unknown. `Excluded` marks a GPU left out by the exclude list (D24); it is never in `AgentStarted.Devices`.
- `GPULink{UUIDA, UUIDB string (UUIDA < UUIDB); Level GPULinkLevel; NVLinks int; P2P GPUP2PStatus}`. P2P is the READ capability, matching `nvidia-smi topo -p2p r`.
- `GPUTopology{UnknownReason string; CollectedAt time.Time; DriverVersion string; GPUs []GPUInfo; Links []GPULink}` (`omitempty` on each).
- master_message.go `AgentStarted` (79-84) gets `GPUTopology *GPUTopology \`json:",omitempty"\``. The other fields have no tags, so the JSON key is "GPUTopology".

Compatibility:
- ws.go:142 decodes with plain `json.Unmarshal`, so an old master ignores the field from a new agent.
- An old agent sends nil, which means unknown.
- An unknown enum string from a newer agent degrades to unknown.
- There is no version gate (agentrm has no agent-version check), and no new message type (3.4).

---

## 5. Master agent state, restore and reconnect

New master/internal/rm/agentrm/gpu_topology.go holds an immutable `gpuTopology` built from the wire value and the agent's `AgentStarted.Devices`:
- Map UUID to device.ID for CUDA devices only. Store per-GPU info by device.ID: bus id, NUMA, width and gen (current and max), error.
- Store pair keys in `map[[2]device.ID]pairKey` (lo, hi).
- Drop links whose UUIDs are not in the device list, normalise reversed A/B, and treat a missing pair as unknown.
- Excluded GPUs (D24): keep each GPU marked `Excluded` whose UUID is not in the device list, and its links, in a separate display-only part keyed by UUID. They get no device.ID, no pair key, and never reach `selectDevices`. Drop any other GPU whose UUID is not in the device list. A GPU marked excluded whose UUID is in the device list is treated as a slot GPU and logged at Warn; a correct agent never sends one.
- `reason string` is non-empty when the topology is unknown:
  - wire nil with CUDA devices: "agent <version> does not report GPU topology";
  - `wire.UnknownReason`: passed through;
  - never set since the master started: "not reported since the master started".
- Topology stays out of `device.Device`. Device is a map key (agent_state.go:49), is stored in snapshots and the proto, and is compared on reconnect (agent_state.go:261-292).

agent_state.go:
- Add the field `gpuTopology *gpuTopology` and `setGPUTopology(t *aproto.GPUTopology, devices []device.Device, version string)`. It replaces the pointer wholesale and never mutates it.
- In `deepCopy` (199-214), add `gpuTopology: a.gpuTopology`. Without this line every scheduler copy has no topology and the feature is silently off.
- Not in `snapshot()` or `newAgentStateFromSnapshot`: no DB change, no migration.

There is **one set site**: agent.go `HandleIncomingWebsocketMessage`, after the `if a.started { checkAgentStartedDevicesMatch; checkAgentResourcePoolMatch } else { a.agentStarted(...) }` block (614-643) and before `a.started = true` (645). It calls `a.agentState.setGPUTopology(...)` and logs one Info summary (Warn with the reason, or with GPUs in error). This single site covers:
- fresh registration;
- a reconnect within `agent_reconnect_wait`;
- an agent process restart, for example after a reboot with a new driver. The devices are equal, so P2P, width, generation and errors are refreshed with no code or config change (R1, g292).
- a master restart. The snapshot restore leaves topology nil until the agent's reconnect AgentStarted arrives (agent.go:147-163 restore; the agent is re-enabled at 406-414 and listeners are notified at 433, before AgentStarted). In that window of a few seconds the agent counts as unknown. Opted-in tasks still allocate there, with the default device choice.

A device mismatch keeps today's shutdown path; the topology is never compared.

---

## 6. Score and set selection (pure functions in gpu_topology.go)

### 6.1 pairKey
A tuple compared lexicographically; smaller is better. `w` = 32 − min(current width of A, current width of B), with unknown width (0) counting as 32, the worst.

| Tier | Meaning | Sub-key 1 | Sub-key 2 |
|---|---|---|---|
| 0 | NVLink (NVLinks > 0) | 64 − NVLinks | 0 |
| 1 | PCIe P2P (P2P == OK and level known) | `w` (width first) | level ordinal: INTERNAL 0, PIX 1, PXB 2, PHB 3, NODE 4, SYS 5 |
| 2 | host-staged (P2P known and not OK, level known) | 0 if level ≤ NODE (same NUMA node), 1 if SYS | `w` |
| 3 | unknown (otherwise) | 0 | 0 |
| 4 | either GPU has an agent-reported NVML error (D6) | 0 | 0 |

- Tier 1 is the recommended width-first variant (D2). The fallback swaps sub-keys 1 and 2 (level first, width second). That is a one-line comparator change, and it still avoids the x8 GPU on node07 for 4-GPU sets.
- Tier 2 uses width as its second sub-key, because width matters without P2P on Genoa (see the evidence in 6.2).
- XIDs are not in the key: they live only in the master API layer, not in agentState.

### 6.2 Evidence (docs/05 Appendix A, NCCL all-reduce busbw at 1 GiB, GB/s)

**An x8 GPU in a set, measured on one node.**
- node06 (Genoa, GPU6 at x8): `NCCL 8 GPUs / GPU4-7 (with GPU6)` 13.0 / 13.0 against `NCCL GPU0-3` 25.6 (docs/05:350-357). Both 4-GPU sets are on one socket, so width alone halves the bandwidth.
- node07 is supporting data: `NCCL 8 GPUs (with GPU1)` 13.0 against `NCCL GPU4-7` 25.7 (docs/05:368-369). Its GPU0-3 set was not measured.
- docs/05:58 and :240 say the same as a rule: "A GPU at PCIe x8 caps every ring that contains it at about 13 GB/s, with or without P2P".

**Width before level, with P2P.**
- Measured for pairs on Milan (node05, GPU1 and GPU4 at x8): the cross-socket all-x16 pair `3,7` gives 19.2, against the same-socket pairs that contain an x8 GPU, `0,1 / 4,5`, at 12.8 / 12.8 (docs/05:337-338).
- On Rome, across nodes of the same CPU family: node02's cross-socket x16 pairs give 19.2-19.4 (docs/05:290), against node01's same-socket pair with an x8 GPU, `4,5`, at 12.6 (docs/05:273).
- On Genoa, on one node: node06's cross-socket x16 pairs give 23.5 / 23.6 (docs/05:359), against any set with GPU6 at 13.0.
- **For 4-GPU sets this is inferred only.** No 2+2 cross-socket all-x16 set has been measured. Benchmark gates (b) node01 and (c) node05 in section 15 decide D2.

**Socket (NUMA) locality with P2P.**
- Rome: same-socket pairs 24.5-24.8 against cross-socket pairs 19.2-19.4, about 27% (node02, docs/05:289-290).
- Genoa: 25.1-25.2 against 23.5-23.6, about 7% (node06, docs/05:358-359).

**Without P2P (tier 2).**
- On Rome, NUMA locality dominates and width does not matter:
  - stock same-socket pairs 3.4 against cross-socket pairs 1.2-1.3 on node02, about 2.7x (docs/05:289-290);
  - node01 stock x16 pair `0,1` 3.6 against x8 pair `4,5` 3.6 (docs/05:272-273).
- On Genoa host staging is fast enough that width does matter:
  - node07 stock (SHM): cross-socket pair `0,4` 8.7 with GPU0 at x8 in that boot, against `3,7` 16.4 (docs/05:371, :374);
  - same-socket x16 pairs 18.5 / 18.3 (docs/05:370).
- Hence tier 2 is NUMA class first, then width. The costs are asymmetric (about 2.7x on Rome against about 1.9x on Genoa), and locality-first is never catastrophic. This applies today to g292 only, whose GPUs are all x16.

**PIX without P2P.** Two GPUs behind one switch share one uplink to the host, so switch locality inside a NUMA node is treated as neutral in tier 2. This is inferred and is benchmark (d) on g292.

### 6.3 Set key and selection
- `setKey`: the C(n,2) pair keys of a set, sorted worst first. Sets compare lexicographically (worst pair, then second worst, ...). Exact ties go to the lexicographically smallest sorted device-ID list.
- `selectDevices(eligible []device.Device, g *gpuTopology, n int) ([]device.Device, setKey)`:
  - `eligible` is sorted by device ID. For node choice (section 7) it is the agent's free devices: a.Devices entries with a nil container, the same eligibility as today's loop. In the real reservation (section 8) it additionally excludes slots that are not enabled (draining), read under the agent lock.
  - If n < 2, g is nil or unknown, or len(eligible) < n: return (nil, unknownKey(n)). nil means "use the default allocation"; unknownKey is C(n,2) tier-3 keys.
  - If C(len(eligible), n) > 20000: return (nil, unknownKey(n)) with a Debug log. On this cluster the maximum is C(8,4) = 70, and every agent with at most 16 free GPUs is fully enumerated.
  - Otherwise enumerate the combinations in ID-lexicographic order and keep only a strictly better key, so the ID tie-break falls out of the order. Return the set in ID order.

### 6.4 Expected choices on the cluster
Slot IDs, with node01 as deployed today: its slots 0-6 are host GPUs 0,1,2,3,5,6,7, with x8 at slots 3 and 4. Each cell was recomputed with this key from the node's measured levels, P2P status and link widths:

| Node | n=2 | n=3 | n=4 width-first | n=4 level-first |
|---|---|---|---|---|
| node02/03/04/08 (all x16) | {0,1} | {0,1,2} | {0,1,2,3} | {0,1,2,3} |
| node06 (x8: 6) | {0,1} | {0,1,2} | {0,1,2,3} | {0,1,2,3} |
| node07 (x8: 1) | {0,2} | {0,2,3} | {4,5,6,7} | {4,5,6,7} |
| node05 (x8: 1, 4) | {0,2} | {0,2,3} | {0,2,3,5} (cross socket, all x16) | {0,1,2,3} (contains x8 slot 1) |
| node01 (x8: slots 3, 4) | {0,1} | {0,1,2} | {0,1,2,5} (cross socket, all x16) | {0,1,2,3} (contains x8 slot 3) |

- With the exclude list (D24), node01's slot ids become the host indices 0-3 and 5-7, with x8 at slots 3 and 5. The same sets are then {0,1}, {0,1,2}, {0,1,2,6} (width-first) and {0,1,2,3} (level-first). The excluded GPU is never in a set.
- node02 with free {0,1,4,5,6}, n=3: {4,5,6}.
- g292 (P2P GNS, all NODE or PIX in one NUMA node, all x16):
  - n=2 gives {0,1}; with slot 0 busy it gives {1,2}, because PIX is neutral without P2P.
  - After P2P is enabled and the agent restarts, slot 0 busy gives {2,3} (PIX).
- Prior art worth comparing: NVIDIA's k8s-device-plugin preferred allocation (go-gpuallocator). It is not a dependency.

---

## 7. Node choice and the scheduler simulation

### 7.1 Rule: topology breaks ties only among count-interchangeable agents, in homogeneous pools that do not preempt
`findFits` (fitting.go:72-94) gains one parameter, `noPreemption bool`, and passes it to `findSharedAgentFit` (fitting.go:222-246). There:
1. Build the candidates and sort them **exactly as today**: hard constraints at :227, then `sort.Sort(candidates)` at :242. W = candidates[0] is today's winner.
2. Only if `req.FittingRequirements.PreferGPUTopology && req.SlotsNeeded >= 2`:
   - W is the result unless the cross-agent tie-break below runs and finds a better agent. Either way the result is marked for within-agent set selection (step 3, section 8).
   - The cross-agent tie-break runs only if `noPreemption && homogeneous(agents)`:
     - `homogeneous(agents)`: every agent in the map with `enabled && !draining` has the same `len(Devices)`. Disabled and draining agents have `numEmptySlots() == 0` (agent_state.go:98-106) and take no request that needs slots, so they are left out.
     - It is computed from the map the function is given. The simulation's deep copies carry `enabled`, `draining` and `Devices` (agent_state.go:199-214), so the simulation and the real pass decide it from the same tick-start state.
   - When it runs:
     - the interchangeable set C = the candidates with `numSlots() == W.numSlots()` and `numEmptySlots() == W.numEmptySlots()` (in a homogeneous pool, the candidates with W's Score);
     - for each c in C, `c.topoKey = selectDevices(free(c), c.Agent.gpuTopology, n)`;
     - pick the c with the smallest topoKey. Ties keep today's order, so W wins exact ties, including the all-unknown case (I5).
3. The winner gets `Slots = SlotsNeeded` and `preferTopology = true` (new `fittingState` fields `topoKey` and `preferTopology`). There is no `Devices` field: the set is chosen at reservation (section 8).

The new argument at each `findFits` call site:

| Call site | Argument |
|---|---|
| resource_pool.go:390 (`allocateResources`, the real pass) | `!rp.config.Scheduler.GetPreemption()` |
| priority.go:149, :236, :263 (the simulation) | `!p.preemptionEnabled` |
| fair_share.go:96, :170, :388 | `false`: `GetPreemption()` is always true for the deprecated fair-share scheduler (scheduler_config.go:106-107) |

- `rp.config.Scheduler` is the config the pool's scheduler was built from (agent_resource_manager.go:671-678), so the real pass and the priority simulation see the same value. In priority.go and fair_share.go only these arguments change; their logic does not.
- On this cluster the master config (cluster-setup services/system-configurations/etc/determined/master.yaml) sets `type: priority`, `fitting_policy: best` and no `preemption`, so the tie-break is live in every homogeneous pool.
- A slot disabled with `det slot disable` (no drain) leaves `Devices` (agent_state.go:418-422). That agent's `numSlots()` drops, the pool stops being homogeneous, and the cross-agent tie-break is off until the slot is enabled again. A slot drained through the REST API stays in `Devices` and changes nothing here. GPUs left out by an exclude list (D24) are not in `Devices` either, so an agent with an exclude list in a pool of otherwise equal agents also turns the tie-break off.

`findDedicatedAgentFits` (107-220) is unchanged. Multi-agent fits take every free slot of fully idle agents, so there is no choice inside an agent. The task log says the flag had no effect.

### 7.2 Why this rule (C2, V1)
- **Today's property, stated precisely.** The priority scheduler simulates on deep copies:
  - `deepCopyAgents` at priority.go:104;
  - `findFits` then `addTaskToAgents` at :263-268, which picks devices on the copy at :319-325;
  - then `allocateResources` fits again on `rp.agentStatesCache` (resource_pool.go:375-377, 389-395).
  - Device choice iterates a Go map (agent_state.go:166-173), so the simulation and the real state end up with different free *sets*. They have the same free *counts*, and today's ranking (fitting.go:46-66: Score, HashDistance, agent id) uses only counts and ids. So the simulation and the real allocation agree on every agent.
  - That holds when every simulated placement is also in `toAllocate`. When backfilling drops a non-preemptible task (priority.go:128-137), or a task is reserved while it waits for preemptions (:149-161), the simulation reserved slots that reality does not use. Then they agree only in the weaker sense that reality has more room.
- **A topology-first rule breaks this** (review finding C2; it follows from the code). Pool with X and Y, 8 GPUs each, 4 per socket, all free; one tick, same priority: N1 (non-opted, 6), A (opted, 2), N2 (non-opted, 8).
  - In the simulation, N1 leaves X with {0,1}, so A goes to X and N2 to Y.
  - In reality, N1 leaves X with {0,4}. Topology-first would send A to Y, and N2 then finds 6 free on Y and 2 on X: findFits returns nil and `allocateResources` returns false (resource_pool.go:397-398).
- **Equal counts are not enough when agents differ in size** (V1). Requiring equal numSlots and numEmptySlots does not make the rule safe for mixed agent sizes, because later steps look at more than the multiset of counts:
  - `candidateList.Less` breaks Score ties by HashDistance and then agent id (fitting.go:46-66). Under BestFit, equal Score means equal numEmptySlots (fitting_methods.go:44) but not equal numSlots; under WorstFit (:58) it means neither. So which *size* of agent a later request lands on depends on which agents are left with which counts, by identity.
  - `findDedicatedAgentFits` groups idle agents by numEmptySlots (fitting.go:121-131) and needs enough agents in one group (:174-187), so whether a multi-agent request fits depends on how many idle agents of each size remain.
  - Counterexample (BestFit, one pass): A and B have 8 slots with 4 used; S and T have 4 slots and are idle. Requests: O (2, opted), R (4), M (8, multi-agent). All four agents have 4 free, so for O they tie on Score and HashDistance decides; say W = A, so the interchangeable set is {A, B}. Say R's hash order is B, S, A, T.
    - Flag off: O goes to A. R goes to B (B, S and T have 4 free; B comes first). S and T stay idle, and M takes both. M fits.
    - Flag on, O goes to B: R goes to S (A, S and T have 4 free; S comes before A). Only T is idle, and M fails.
  - WorstFit has the same problem: an 8-slot A and B with 4 used and a 4-slot S with 2 used all score 0.5. With O (2, opted, W = A, tie-break picks B), R (2, hash order A, S, B) and M (4), M fits with the flag off and fails with it on.
- **In a homogeneous pool counts are enough.** For an enabled, non-draining agent, `numSlots() == len(Devices)` and `numEmptySlots() == numSlots() − numUsedSlots()` (agent_state.go:86-106). If every such agent has N slots:
  - the BestFit and WorstFit Scores (fitting_methods.go:41-64) and `slotsSatisfied` (:20-22) are functions of numEmptySlots alone, and an agent passes `agentSlotUnusedSatisfied` (:31-33, idle) exactly when its numEmptySlots is N;
  - disabled and draining agents have numEmptySlots 0 and take nothing;
  - so a shared fit always takes an agent whose numEmptySlots is fixed by the multiset of numEmptySlots, and the hash only picks which of several agents with that value; a dedicated fit takes idle agents, all of size N, and fits exactly when enough of them exist (fitting.go:174-187; the heterogeneous path at :190-217 sees a single group);
  - by induction over the requests of a pass, the states with the flag on and off stay permutations of each other, and each request fits in one exactly when it fits in the other. The opted-in choice inside C is one more such permutation. The same argument makes every request in the simulation's `toAllocate` fit in the real pass whenever it does today;
  - zero-slot requests change no slot count, and each needs only one free zero-slot unit (`maxZeroSlotContainersSatisfied`, :24-29), so where one lands never decides whether the next one fits.
- **Preemption: the tie-break is skipped, not left to the retry.** With preemption, the scheduler frees slots on the agent that runs the task it preempts (`removeTaskFromAgents` at priority.go:233, which looks the agent up at :336-346). Which equally full agent holds an opted-in task then decides what a later preemption frees.
  - Counterexample (homogeneous, one pass): A and B have 8 slots. A runs Q (4 slots, not preemptible), B runs P (4 slots, preemptible, lower priority than U). O (2, opted) and U (8) are pending with the same priority, O queued first. Say W = A.
    - Flag off: O goes to A. U preempts P, B becomes idle, and U gets B once P has exited.
    - Flag on, O goes to B: freeing P leaves O on B, so U does not fit, nothing is preempted, and U waits.
  - The bounded retry (8.3) cannot repair this. A release is sent at once (`releaseResource`, resource_pool.go:484-487) and cannot be taken back, and re-running the scheduler on the same state gives the same answer. Skipping is the safer option, and it costs nothing by default: preemption is off by default (scheduler_config.go:32) and off on this cluster.
  - Within-agent set selection stays on in preempting pools. Preemption frees whole containers by ID, so which GPUs inside the agent the opted-in task holds changes no count.
  - The gate must apply in the simulation as well as in the real pass. If only the real pass skipped the tie-break, the simulation could place O on another agent than reality does and base its preemption decisions on that. Hence the `findFits` argument at all call sites.
- **The simulation needs no logic change.** priority.go only passes `!p.preemptionEnabled` to its three `findFits` calls. It keeps using the default device choice in `addTaskToAgents`. The real reservation picks its set under the agent lock (section 8). For later opted-in requests in the same tick, a different choice inside C is again count-neutral in a homogeneous pool.
  - This rule deliberately does not make the simulation and the real pass pick the *same* agent and devices for an opted-in task. Device sets cannot match anyway, because non-opted tasks take map-order devices separately in each pass. Pinning the agent would need a plan carried across the `Scheduler` interface (D4 alternative). What later fits need is equal counts in a homogeneous pool, and the rule guarantees that.
- **Across passes.** The argument above covers one scheduling pass. Later, when a task on one of the two agents finishes, which of them holds the opted-in task decides which one becomes emptier. For example, with A and B both at 4 of 8 used, putting O (2) on B rather than A leaves B at 6 used and A idle once A's task finishes, instead of A at 2 and B at 4 used; an 8-slot task then fits only in the first case. Had B's task finished first, the opposite would hold. Today's tie-break by allocation-id hash has exactly this property, and neither choice can know which task finishes first. The flag replaces one arbitrary choice with a topology-informed one; it does not systematically help or hurt packing.
- **Residual edge case:** identity-specific constraints.
  - `agentPermittedSatisfied` (BlockedNodes from log-pattern `exclude_node` policies, fitting_methods.go:16-18) makes a later request care which of two interchangeable agents is fuller. That request can then fail its real fit after an opted-in placement.
  - The retry rule in 8.3 covers it: the next tick re-simulates from the real state.
- **What the rule gives up.** A topology-first rule, which ranks the topology key before the packing score, could send an opted-in task to an emptier agent whose best set is better. In the C2 case above that strands N2, a *non-opted* task, even with a perfectly consistent simulation; with topology-first plus simulation pinning (D4 alternative) N2 still waits. Topology-first therefore changes default-path behaviour for other tasks in the pool.
  - With the gated rule, the node choice changes only between equally packed agents of the same size, in pools whose enabled agents all have the same size and whose scheduler does not preempt.
  - Today that is node03 and node04 in pool 48c96t_512_3090 (8 GPUs each), the only multi-agent pool (g292 will be its own pool). Within-agent set selection still applies on every node.

---

## 8. Reserving the set, draining slots, and the retry after a failed reservation

### 8.1 The set is chosen at reservation time, under the agent lock
- agent.go: the `allocateFreeDevices` message (107-110) gets `preferTopology bool`. In `AllocateFreeDevices` (168-182), still under `a.mu`:
  ```
  if msg.preferTopology && msg.slots >= 2 {
      eligible := a.agentState.freeEnabledDevices()      // nil container AND slot enabled (not draining)
      if devs, key := selectDevices(eligible, a.agentState.gpuTopology, msg.slots); devs != nil {
          got, err := a.agentState.allocateSelectedDevices(devs, msg.containerID)
          return allocateFreeDevicesResponse{devices: got, topoKey: key, preferred: true}, err
      }
  }
  devices, err := a.agentState.allocateFreeDevices(msg.slots, msg.containerID)   // unchanged path
  ```
- resource_pool.go `allocateResources` (423-426) passes `preferTopology: fit.preferTopology`.
- New `agentState.allocateSelectedDevices(devs, cid)` is all or nothing:
  1. Validate first: devs is non-empty, has no duplicates, and every device is present in a.Devices with a nil container.
  2. Only then set `containerState[cid] = &cproto.Container{ID: cid, Devices: sorted}` and mark each device `&cid`.
  3. Return the devices sorted by ID. They become DET_SLOT_IDS and the DeviceRequests order.

  The existing function leaks `containerState[cid]` on "not enough devices" (agent_state.go:160/175). It stays untouched (I4, D17).
- Effect: the set comes from the live state at reservation time, not from the tick's cache. A set computed during the tick could be invalidated by a slot disable between the cache at resource_pool.go:362 and the reservation; choosing at reservation removes that race and any need for a validation-failure path.
- The node choice (section 7) still ranks agents by the cache's view. A small mismatch there costs at most a slightly worse set on an interchangeable agent.

### 8.2 Draining slots (C7)
- Today a slot disabled with drain stays in `a.Devices` (agent_state.go:418-423), so once its container exits it counts as free and gets new work.
  - The master sends `StartContainer` before `agentState.startContainer` checks the slot (agent.go:224-228), and that check only logs (agent_state.go:345-346).
  - Random device choice picks such a slot sometimes. A best-set choice would pick it every time it is in the best set.
- `freeEnabledDevices()` runs under `a.mu`, so it may read `slotStates`. The scheduler goroutine may not: `deepCopy` shares `slotStates` (agent_state.go:208-209). It excludes slots whose `enabled.enabled()` is false.
- If fewer than n eligible devices remain, `selectDevices` returns nil and the unchanged default allocation runs. That is today's behaviour, which may include the draining slot.
- Default-path tasks keep the quirk (D17). Note: a slot can be drained only through the REST API (`DisableSlotRequest.drain`); `det slot disable` has no `--drain` (harness/determined/cli/agent.py:288-291).

### 8.3 Retry after a failed reservation (C3)
- A failed reservation is not retried on the next tick (500 ms) by itself:
  - `schedulerTick` runs the scheduler only `if rp.reschedule` (resource_pool.go:360) and clears the flag unconditionally at :383.
  - `allocateResources` only logs and returns false (:397-398, :427-432).
  - The flag is set only by new requests, releases, group changes, provisioner errors, agent updates, job position recovery and job stop (:121, :235, :290, :298, :306, :356, :559, :666, :686).
  - `PatchSlotState` (agent.go:547-560) never calls `notifyListeners`, so a slot disable never triggers a reschedule.
- The fix, in `schedulerTick` only, with a new `resourcePool` field `topologyRetries int`:
  ```
  retry := false                                             // before `if rp.reschedule` (:360): read after the block
  if rp.reschedule {
      ...
      optInAllocated := false
      for _, req := range toAllocate {                       // replaces :375-377
          optIn := req.FittingRequirements.PreferGPUTopology
          ok := rp.allocateResources(req)
          if !ok && (optIn || optInAllocated) { retry = true }
          if ok && optIn { optInAllocated = true }
      }
      ...
  }
  rp.reschedule = false                                      // :383, unchanged
  if retry && rp.topologyRetries < 3 { rp.reschedule = true; rp.topologyRetries++ }
  if !retry { rp.topologyRetries = 0 }
  ```
- In a tick without an opted-in request, `retry` stays false and the behaviour is byte-identical (I2). The cap of 3 consecutive ticks stops a persistent failure, such as a DB persistence error, from re-running the scheduler every 500 ms forever. After the cap, the task waits for the next ordinary trigger, as today.

---

## 9. Task-log line showing the chosen set

- In resource_pool.go, after `rmevents.Publish(req.AllocationID, allocated.Clone())` (:469), and only for opted-in requests with SlotsNeeded ≥ 2, publish a `sproto.ContainerLog` (ContainerID = resources[0].containerID, AgentID, Timestamp, Level "INFO", AuxMessage).
  - The allocation subscribed before `pool.Allocate` (agent_resource_manager.go:171), and allocation.go:263-264 turns ContainerLog into a task log.
- Texts:
  - set chosen: `GPU topology preference: agent cvgl-node07, slots 4,5,6,7; worst pair NODE, P2P OK, x16`
  - unknown: `GPU topology preference: topology of agent X unknown (<reason>); slots chosen as usual`
  - fallback: `GPU topology preference: fewer than N enabled free GPUs on agent X; slots chosen as usual`
  - multi-agent: `GPU topology preference has no effect: the task uses whole agents`
- The same text goes to `rp.syslog` at Info.

---

## 10. API: proto, assembly, XID join and health classification

### 10.1 Proto (proto/src/determined/agent/v1/agent.proto)
Under buf DEFAULT+COMMENTS lint, every message, field and enum value is commented.

```
message GpuTopology {
  string unknown_reason = 1;                       // "" = known
  google.protobuf.Timestamp collected_at = 2;      // agent clock, at agent start
  string driver_version = 3;
  repeated GpuInfo gpus = 4;                       // one per CUDA slot, ordered by device_id; then excluded GPUs by pci_bus_id
  repeated GpuLink links = 5;                      // device_a < device_b between slots
  bool xid_checked = 6;                            // the DCGM lookup ran and succeeded
  string xid_check_error = 7;                      // "" = ok or not configured
}
message GpuInfo {
  int32 device_id = 1; string uuid = 2; string pci_bus_id = 3;
  int32 numa_node = 4;                             // -1 unknown
  int32 pcie_link_width = 5; int32 pcie_link_width_max = 6;   // 0 unknown
  int32 pcie_link_gen = 7;   int32 pcie_link_gen_max = 8;     // 0 unknown; gen is informational
  string error = 9;                                // NVML error at collection
  repeated GpuXid recent_xids = 10;
  GpuHealth health = 11;
  bool excluded = 12;                              // left out by the agent's exclude list (D24); device_id is -1
}
message GpuXid { int32 xid = 1; google.protobuf.Timestamp first_seen = 2; google.protobuf.Timestamp last_seen = 3; }
message GpuLink { int32 device_a = 1; int32 device_b = 2; GpuLinkLevel level = 3; int32 nvlinks = 4; GpuP2pStatus p2p_status = 5;
                  string uuid_a = 6; string uuid_b = 7; }   // set on every link
enum GpuLinkLevel { GPU_LINK_LEVEL_UNSPECIFIED = 0; GPU_LINK_LEVEL_INTERNAL = 1; _PIX = 2; _PXB = 3; _PHB = 4; _NODE = 5; _SYS = 6; }
enum GpuP2pStatus { GPU_P2P_STATUS_UNSPECIFIED = 0; _OK = 1; _CHIPSET_NOT_SUPPORTED = 2; _GPU_NOT_SUPPORTED = 3; _TOPOLOGY_NOT_SUPPORTED = 4; _DISABLED_BY_REGKEY = 5; _NOT_SUPPORTED = 6; }
enum GpuHealth { GPU_HEALTH_UNSPECIFIED = 0 /* unknown */; GPU_HEALTH_HEALTHY = 1; GPU_HEALTH_LINK_DEGRADED = 2; GPU_HEALTH_ERROR = 3; }
```

- Excluded GPUs (D24) have `excluded = true` and `device_id = -1`, so a client can never confuse one with slot 0. A link with an excluded end has -1 as that end's device id and is ordered by uuid_a < uuid_b. Clients join slot GPUs by device_id and excluded GPUs by UUID.
- `Agent` gets `GpuTopology gpu_topology = 12;`. Tag 12 is the next free one (agent.proto:39-67); buf breaking FILE treats it as additive. It is unset for agents without CUDA slots. Keep it out of `devicev1.Device`.
- Regenerate with the PR #27/#34 chain:
  - `make -C proto build` (protoc ≥ 24 and the tool versions pinned in proto/get-deps.sh) produces proto/pkg/agentv1/agent.pb.go;
  - `make -C bindings` produces harness/determined/common/api/bindings.py and webui/react/src/services/api-ts-sdk/api.ts.

### 10.2 Assembly in agentrm
- `model.AgentSummary` (master/pkg/model/agent.go:15-25) gets `GPUTopology *agentv1.GpuTopology \`json:"gpu_topology,omitempty"\``, matching the snake_case tags of every other field; `ToProto` (76-99) copies it.
- agentrm/agent.go `summarize` (745-768) fills it with `a.agentState.gpuTopologyProto()`, under `a.mu`.
  - It builds a fresh message per call. `gpus` has one entry per CUDA slot (device_id and uuid from `slotStates`, so the shape is the same even when the topology is unknown), with the NVML fields filled when known.
  - Excluded GPUs (D24) follow the slot GPUs, from the display-only part of `gpuTopology` (section 5). They exist only when the agent reported them.
  - It sets `unknown_reason` when the topology is unknown, and `links` with device_a < device_b.
  - `health` is left UNSPECIFIED here and classified in 10.3.
  - It is nil for agents without CUDA slots.

### 10.3 API layer (master/internal/api_agents.go)
- `GetAgents` (46-56):
  - when `ExcludeSlots`, set `agent.GpuTopology = nil` and skip the XID lookup;
  - otherwise, if any agent has a GpuTopology and the integration is enabled, fetch XIDs once (cached, section 3.3) and attach `recent_xids` by UUID, setting `xid_checked` or `xid_check_error`;
  - then call `classifyGPUHealth(gpu, topologyKnown)` for every GPU, slot or excluded (rules in 3.1).
- `GetAgent` (62-85) does the same. It has no `exclude_slots`.
- RBAC: `authz.ObfuscateAgent` (obfuscate.go:69-110) sets `agent.GpuTopology = nil`. It runs before the join (api_agents.go:33-43), so obfuscated agents trigger no lookup. Under basic authz this changes nothing.
- One classification function for both clients means the CLI and the WebUI never disagree about a dot.

---

## 11. CLI (harness/determined/cli/agent.py)

### 11.1 `det agent list` (list_agents, 23-63)
Two new columns after "Slots", computed client-side from `gpu_topology`. They are also in `--json` as `gpu_topology` and `gpu_health`.
- **GPU Topology**: NUMA group sizes of the slot GPUs, then the distinct levels best to worst, then the P2P summary.
- **GPU Health**:
  - `ok` when every GPU is healthy and none is excluded;
  - otherwise the non-healthy GPUs grouped by state in the order error, narrow, unknown, as slot ids;
  - narrow GPUs show `cur of max` width;
  - excluded GPUs (D24) come last as `excluded: <bus id>`, with their own state in parentheses when it is not ok. They have no slot id.

| Agent | GPU Topology | GPU Health |
|---|---|---|
| node02 | `4+4 NODE/SYS p2p` | `ok` |
| node01 | `4+3 NODE/SYS p2p` | `narrow: 3,4 (x8 of x16)` |
| node01 with the exclude list (D24) | `4+3 NODE/SYS p2p` | `narrow: 3,5 (x8 of x16); excluded: 81:00.0` (plus `(error)` if that GPU is in error) |
| node07 | `4+4 NODE/SYS p2p` | `narrow: 1 (x8 of x16)` |
| a GPU with a recent XID 79 | `4+4 NODE/SYS p2p` | `error: 5; narrow: 1 (x8 of x16)` |
| g292 now | `8 PIX/NODE no-p2p(GNS)` | `ok` |
| topology unknown | `unknown: <reason>` | `unknown` (or `error: ...` if XIDs exist) |
| CPU agent, or an older master without the field | blank | blank |

### 11.2 `det agent describe AGENT_ID [--json]` (new, via `bindings.get_GetAgent`)
- A header with:
  - id, pools, version, enabled/draining, driver version and collection time;
  - the topology summary, or the unknown reason;
  - the XID check status (`checked`, `not configured`, `failed: <err>`).
- A per-GPU table with these columns, with one row per slot and then one row per excluded GPU (D24):
  - Slot (`-` for an excluded GPU);
  - State: FREE | allocation id | DISABLED | DRAINING | EXCLUDED;
  - Health;
  - UUID, PCI Bus ID, NUMA;
  - Width cur/max;
  - Gen cur/max, labelled "at agent start, informational";
  - Details: NVML error text and time; XIDs with first/last seen; for narrow GPUs, "x8 of x16: NCCL rings and peer copies through this GPU get about half the bandwidth".
- An nvidia-smi-style level matrix over slot IDs (excluded GPUs labelled by bus id), and a P2P matrix only when not all pairs are OK.
- Bus IDs let operators map node01's renumbered slots to host indices (today's docker device list).
- `--json` prints the raw `gpu_topology`.

---

## 12. WebUI

Placement: the resource-pool page's existing "Topology" section.
- pages/ResourcePool/ClusterTopology.tsx:61-71 renders one `NodeElement` per agent, from `getAgents({excludeSlots:false})` at ResourcepoolDetail.tsx:106, polled at :152 and shown at :330-332.
- For an agent with `gpuTopology`, render a new `GpuTopology` panel (pages/ResourcePool/GpuTopology.tsx + .module.scss) in place of the bar-style slot strip.
- Agents without it keep today's `NodeElement`.

Data:
- types.ts `Agent` (244-252) gets `gpuTopology?: GpuTopology` plus new interfaces. `Resource` gets `draining?: boolean`.
- services/decoder.ts `jsonToAgents` (129-180) maps `agent.gpuTopology` and `slot.draining`. Today the decoder drops `draining` (163-170).
- GPUs are joined to slots by `device_id` and slot id, not by UUID. Excluded GPUs (D24) have no slot and are drawn from `gpus` alone.

Encoding, as required by R4 and approved by the owner after a preview:
- **Tile fill = slot state**, using the palette `SlotAllocationBar` already uses (SlotAllocationBar.tsx:115-143, `getStateColorThemeVar(SlotState.*)` from hew):
  - no container, or container state TERMINATED: `SlotState.Free` (gray);
  - container state RUNNING: `SlotState.Running` (active);
  - container state ASSIGNED, PULLING or STARTING, or a state the decoder leaves undefined (decoder.ts:142-150): `SlotState.Pending`.

  The fill never shows health.
  - Why TERMINATED is Free: the master clears a slot's container reference when the container reports TERMINATED (agent_state.go:318-320) and drops its state (:324-326), and `getSlotSummary` reports a container only through that reference (:388-402). So the API normally shows such a slot with no container, which is Free. A slot that still carries a TERMINATED container (only possible from an agent snapshot restored at a master restart, newAgentStateFromSnapshot :598-630) is drawn the same way, because its container no longer runs.
  - The mapping is its own function, not `SlotAllocationBar`'s tally: that tally counts every non-RUNNING container state, TERMINATED included, as pending (SlotAllocationBar.tsx:101-102).
- **Disabled**: a slot with `enabled=false` (`det slot disable`, or an agent that is disabled or draining, which patches every slot, agent.go:506-510/528-533) is drawn with a diagonal stripe overlay (`repeating-linear-gradient` over the state fill) and the label "disabled", or "draining" when `draining`. It is never a fill colour.
- **Excluded** (D24): a GPU with `excluded=true` is a tile without a slot id, with the Free fill, the same stripe overlay and the label "excluded". It has a health dot and an info button like a slot tile. The popover says "Left out by the agent's exclude list. No task runs on this GPU." The tile sits in its NUMA box and switch group like any other GPU.
- **Health dot**: a 10 px LED-style dot in the tile's top-right corner.

  | Health | Dot fill | Dot ring |
  |---|---|---|
  | healthy | `var(--theme-status-success)` | 2 px `var(--theme-surface)` |
  | link downgraded | `var(--theme-status-warning)` | 2 px `var(--theme-surface)` |
  | error | `var(--theme-status-critical)` | 2 px `var(--theme-surface)` |
  | unknown | `var(--theme-surface)` (hollow) | 2 px `var(--theme-status-inactive-strong)` border |

  - Every dot also gets a 1 px outer outline `var(--theme-surface-on-weak)`, so it stays visible on any fill, including the active fill, and in dark mode. All tokens exist (webui/react/src/utils/colors.ts).
  - Each dot has `aria-label` "GPU health: <state>".
- **Info button**: hew `Icon name="info"` next to the dot.
  - Hover or focus opens a hew `Tooltip`. A click pins the same content in a hew `Dropdown` content popover (the pattern of components/MultiSortMenu.tsx:285-296).
  - The content:
    - slot, state, UUID, bus id, NUMA;
    - "PCIe x<cur> of x<max>, Gen<c> of Gen<m> (collected at <time>, at agent start; GPUs lower the generation when idle)";
    - for narrow GPUs: "A link below its maximum width caps every NCCL ring and peer copy through this GPU at about half the bandwidth (x8 of x16)", with a link to the fork docs section on GPU topology (section 14);
    - for errors: the NVML call and code at <time>, and each XID with first and last seen;
    - for unknown: the reason;
    - the XID check status.
- **Layout**:
  - the summary line (the same strings as the CLI);
  - NUMA boxes, containing "switch" groups (connected components of PIX/PXB links), containing GPU tiles;
  - a collapsible pairwise matrix (level text; non-OK P2P marked with its abbreviation; unknown "?") that scrolls horizontally on narrow screens;
  - a text legend under the section ("● healthy ● link below max width ● error ○ unknown; striped = disabled, draining or excluded"), so colour is never the only signal.
- **Unknown topology**: the text "GPU topology unknown: <reason>", with tiles still drawn from the slots, hollow dots, no grouping and no matrix.
- New `utils/gpuTopology.ts` holds pure helpers: summary strings (shared fixtures with the CLI tests), NUMA/switch grouping, the matrix, and the slot-state-to-palette mapping.

---

## 13. Build and release workflow

- **go.mod/go.sum**: add `github.com/NVIDIA/go-nvml` at **v0.12.9-0** (a go 1.20 module; fine with the release Go 1.22.12, fork-release.yml:147-150).
  - It needs testify 1.10.0 (from 1.9.0, go.mod:49).
  - v0.13.4-0 would pull testify 1.12.1 and go.yaml.in/yaml/v3. D15.
  - Run the full Go suite after `go mod tidy`.
- **.github/workflows/fork-release.yml, "Build Linux binaries" (213-222)**: drop the job-wide `CGO_ENABLED: '0'` and set it per command.
  - master and gotmpl: `CGO_ENABLED=0 go build ...` (unchanged output).
  - agent: `CGO_ENABLED=1 go build -trimpath -tags netgo,osusergo -ldflags "-X main.version=${VERSION}" ...`. With cgo on, the agent's only other cgo users are `net` and `os/user`, and these tags keep both pure Go.
  - Then assert, failing the job:
    1. `go version -m .fork-build/bin/determined-agent` contains `build	CGO_ENABLED=1` (this catches a silent stub build), and the master shows `CGO_ENABLED=0`.
    2. `readelf -d` NEEDED for the agent lists only glibc libraries (libc, libdl, libpthread, ld-linux).
    3. **glibc requirement against the image.** This is the real constraint: the binary's highest required `GLIBC_` symbol version must not exceed the image's glibc.
       ```
       need=$(objdump -T .fork-build/bin/determined-agent | grep -o 'GLIBC_[0-9.]*' | sed 's/GLIBC_//' | sort -uV | tail -1)
       have=$(docker run --rm "$BASE_IMAGE" ldd --version | head -1 | awk '{print $NF}')
       [[ $(printf '%s\n%s\n' "$need" "$have" | sort -V | head -1) == "$need" ]] || { echo "agent needs glibc $need > $BASE_IMAGE $have" >&2; exit 1; }
       ```
       This checks the real constraint, rather than requiring the runner and the base image to be the same Ubuntu release, which is only a proxy.
- **After the image build (:241)**, run the binary *in the image*:
  - `docker run --rm --entrypoint /usr/bin/determined-agent "$AGENT_IMAGE" version` must succeed. A binary linked against a newer glibc fails here with `GLIBC_x.y not found`.
  - `out=$(docker run --rm --entrypoint /usr/bin/determined-agent "$AGENT_IMAGE" gpu-topology)` must exit 0, and `jq -e '.nvml_init == "ERROR_LIBRARY_NOT_FOUND" and .nvml_init_code == 12' <<<"$out"`. Because the subcommand calls `Init()` before detection (2.3), this runs the dynamically linked cgo wrapper and its dlopen-failure path in the real image on the GPU-less runner. The stub prints `NOT_BUILT` and fails the check.
- Add comments at :29 (`runs-on: ubuntu-22.04`) and :36 (`BASE_IMAGE: ubuntu:22.04`): the agent is now dynamically linked, and the glibc check above enforces the coupling.
  - Alternative (D14): build the agent inside a container started from `$BASE_IMAGE`, with gcc and the pinned Go tarball. That removes the dependence on the runner image entirely.
- **agent/Makefile `build` (59-64)**: `CGO_ENABLED=$(AGENT_CGO_ENABLED) go build -tags netgo,osusergo ...` with `AGENT_CGO_ENABLED ?= 1`. Setting it to 0 builds the stub. Dev builds now need gcc.
- **agent/.goreleaser.yml**: unchanged. The fork release does not use it, and its cross targets get the stub.
- **tools/fork/check.sh**: add a `topology` mode, also run by `quick`, with:
  - `CGO_ENABLED=0 go build ./agent/cmd/determined-agent`
  - `GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -o /dev/null ./agent/cmd/determined-agent`
  - `CGO_ENABLED=0 go test ./agent/internal/detect` (stub tests)
  - `go test ./agent/internal/detect ./master/internal/rm/agentrm ./master/pkg/aproto` (cgo, mock and scheduler tests)
- **tools/fork/Dockerfile.agent**: unchanged (`FROM ${BASE_IMAGE}`, no NVIDIA env, no driver libs). tools/fork/smoke.sh's CPU agent keeps exercising the nil-topology path.

---

## 14. Docs and release note (timeless: no measured numbers, no live state; results go in the PR)

- **docs/reference/experiment-config-reference.rst**, resources section next to `is_single_node`, entry `prefer_gpu_topology`. It covers:
  - what it ranks: P2P-capable links before host-staged ones; links below their maximum width last; then NVLink, the same PCIe switch, the same host bridge, the same NUMA node, and across NUMA nodes. Word this in NUMA terms: NVML's NODE/SYS are NUMA-based, and they equal CPU sockets only with NPS1, as on this cluster;
  - without P2P, NUMA locality comes first and width second;
  - P2P capability and link width are what the agent measured when it started, so restart agents after a driver or hardware change;
  - it is soft: it never waits and never moves tasks;
  - it always chooses the GPUs inside the agent the scheduler picks as usual;
  - it can move the task to another agent only when that agent has the same number of free GPUs as the usual choice, every enabled agent in the pool has the same number of GPUs, and the pool's scheduler does not preempt. A slot disabled with `det slot disable` makes that agent smaller, which turns this off until the slot is enabled again. Then, within that scheduling pass, it does not change whether any other task fits (tasks that exclude nodes through a log-pattern policy are retried). Like the usual tie-break between equally full agents, the choice can matter later, when other tasks finish;
  - it applies only to tasks of 2 or more slots on one agent; multi-agent tasks use whole agents;
  - agent resource manager only;
  - the interplay with templates and config policies;
  - the task-log line.
- **docs/reference/job-config-reference.rst**: the same entry for commands, notebooks, shells and TensorBoards.
- **docs/reference/deploy/master-config-reference.rst**:
  - next to `fitting_policy`: the opt-in tie-break among interchangeable agents, active only in pools whose enabled agents all have the same number of slots and whose scheduler does not preempt (`preemption: true` on the priority scheduler, or the fair-share scheduler, turns it off);
  - under `integrations.task_resources`: it also enables the XID part of GPU health (the query class, the 24 h window, the excluded codes).
- **docs/reference/deploy/agent-config-reference.rst**, new section "GPU topology and health":
  - NVML via libnvidia-ml.so.1, injected by the NVIDIA container toolkit; the agent container needs the `utility` capability, as with `--gpus`;
  - the collected fields, and collection at agent start;
  - stub builds (non-Linux or CGO_ENABLED=0) report unknown;
  - the four health states and their exact rules; downgrade = current width < max width; generation is informational;
  - NVML codes appear as symbolic name and number (for example `ERROR_GPU_IS_LOST (15)`); the NVLink probe never marks a GPU as errored;
  - what "error" covers and what is out of reach (section 3.5);
  - `determined-agent gpu-topology`;
  - the `exclude_gpus` option (D24): GPU UUIDs only; start the agent container with all GPUs; an entry that matches no GPU stops agent start; excluded GPUs are reported and shown as "excluded" but never become slots; changing the list changes the agent's slots, so drain first; an older agent ignores the option, so go back to a container-level device list before rolling the agent image back.
  The WebUI info popover links here.
- **docs/release-notes/gpu-topology-placement.rst** (`:orphan:`):
  - New Features: the per-task field; agents report GPU topology, PCIe link width and generation and NVML errors; the agent's GPU exclude list; GPU health with optional DCGM XIDs; the `det agent list` columns, `det agent describe`, and the WebUI pool page.
  - Upgrade notes: the agent binary is now dynamically linked against the image's glibc; the rollback restrictions in 14.3, including the exclude list on older agents.

### 14.3 Rollback (C5)
A master downgrade to a release without this feature (0.41.0 or earlier) after experiments, templates or policies used the key:
- **Stored experiment configs.** Every experiment whose stored config contains `prefer_gpu_topology`, including an explicit `false`, becomes unparsable. `ActiveExperimentConfig` (postgres_experiments.go:848-860) calls `ParseAnyExperimentConfigYAML`, which validates with `schemas.SaneBytes` against the old schema, where additionalProperties is false (expconf/parse.go:38-47).
  - Restore then fails ("cannot restore experiment %d with unparsable config", restore.go:62-65).
  - Every other path that re-parses the stored config fails the same way.
  - `omitempty` limits this to experiments that set the key.
- **Experiment templates.** They are decoded strictly: core_experiment.go:293-299 calls `templates.UnmarshalTemplateConfig(..., true)`, and templates/service.go:60-64 then adds `yaml.DisallowUnknownFields`. So creating an experiment from a template that contains the key (including `false`) is rejected until the key is removed from the template.
- **Invariant config policies.** They are decoded leniently (`json.Unmarshal` into `ExperimentConfigV0`, task_config_policy.go:193 and :207, then `schemas.Merge`). A policy that forces the flag is silently ignored, with no error.
- **NTSC and generic-task snapshots.** They are bun JSON columns (command/models.go:24, :27), decoded with encoding/json, and drop the key leniently. New NTSC requests with the key get an unknown-field 400 from the old master (api_command.go:133-142).
- **Agents.** A new agent works with an old master (the field is ignored, section 4). An old agent with a new master shows "unknown".
- **Agent image rollback with an exclude list** (D24). An older agent does not know `exclude_gpus` and ignores it. Started with all GPUs, it registers the excluded GPU as a slot, and tasks can run on it. Before rolling an agent image back, go back to the docker device list (2.4).

The release note states all four master cases.

---

## 15. Validation plan

**Phase 0: benchmarks before the selection code, baseline first (needs no code).** Per node in a maintenance window:
- drain with `det agent disable --drain` and re-enable afterwards (docs/05 steps 1/7);
- run cluster-setup scripts/gpu-p2p/tests/run_host.sh with `STAGES=nccl` and `GPU_SETS` as below, 1 GiB busbw, 3 runs, median, `NCCL_VARIANTS="p2p-sys"` unless noted.

| Run | Node(s) | GPU_SETS / variant | Purpose |
|---|---|---|---|
| a | node02 or 03/04 (Rome, all x16) | "0,1 0,4 0,1,2,3 0,1,4,5 0,2,4,6"; also `NCCL_VARIANTS=no-p2p` | 2+2 cross-socket 4-GPU sets (never measured); checks the tier-2 locality rule |
| b | node01, `GPUS` = the 7 agent GPUs (7-GPU numbering) | "0,1,2,3 0,1,2,5 0,1,2,6" | decisive for width-first on 4-GPU sets: same-socket set with x8 against all-x16 cross-socket set |
| c | node05 (Milan) | "0,1,2,3 0,2,3,5 0,2,3,6 2,3 3,7" | the same for 4-GPU sets under Milan's asymmetric cross-socket P2P (pairs are already measured: 19.2 against 12.8) |
| d | g292, default/SHM | "0,1 0,2 0,1,2,3 0,2,4,6" | PIX against NODE without P2P (tier-2 neutrality); repeat with p2p-sys after the patched driver |
| e | node02 and node01 | one representative DDP training (fixed 500 steps, samples/s, 3 runs): same-socket against cross-socket set; x8 set against all-x16 set | busbw overstates gains for compute-bound jobs |

- Decision rule for D2: choose width-first if (b) and (c) show the all-x16 cross-socket 4-GPU set beating the same-socket set with an x8 GPU; otherwise level-first with width second.
- Post the tables in the PR.

**Phase 1: candidate image, read only.** On each node, run `docker run --rm --gpus all --entrypoint /usr/bin/determined-agent <candidate> gpu-topology`; on node01, use the agent's 7-UUID device request. Diff the output against host `nvidia-smi topo -m`, `nvidia-smi topo -p2p r`, `nvidia-smi --query-gpu=index,pci.bus_id,pcie.link.gen.max,pcie.link.width.current,pcie.link.width.max` and sysfs NUMA. Check that:
- node01 slots 0-6 = host 0,1,2,3,5,6,7;
- on node01 with `--gpus all` and `--exclude-gpus <UUID of 81:00.0>`, the command finishes, the output has no `exclude_error`, the GPU at 81:00.0 is reported as excluded with its link data, the devices are host 0-3 and 5-7, and querying it causes no fault on the host (D24, 2.4);
- x8 is reported at node01 slots 3, 4, node05 slots 1, 4, node06 slot 6 and node07 slot 1, each with max 16;
- sysfs NUMA is readable inside the container;
- on an RTX 3090 node (node03 or node04), host `nvidia-smi nvlink -s` lists the links the GPUs have and shows none active, and the candidate reports NVLinks 0 for every pair and no `error` on any GPU (the NVLink probe, 2.2);
- `nvml_init` reads `SUCCESS`, and any NVML code in the topology part is a symbolic name with its number; NVML prose anywhere means a return was formatted with `ret.String()` (2.2).

**Phase 2: after deploy, functional.**
- **Visibility.** On the pool pages:
  - nodes 01/05/06/07 show amber dots on exactly those slots, and the other GPUs show green;
  - a node disabled with `det agent disable` shows striped tiles labelled "disabled";
  - `det agent list` and `det agent describe` match the WebUI;
  - in a test master without `integrations.task_resources`, the details say `XID check: not configured`;
  - after node01 is drained and restarted with all GPUs and the exclude list (D24): the pool page shows a striped "excluded" tile without a slot id, `det agent list` and `det agent describe` list the GPU as excluded, and a 7-GPU task on node01 gets no device request with the excluded UUID (`docker inspect` of its container).
- **Selection.**
  - On node02, disable slots to get a fixed free set (for example `det slot disable cvgl-node02 0`, and 1 and 4).
  - Run `det cmd run --config resources.resource_pool=48c96t_512_4090 --config resources.slots=2 --config resources.prefer_gpu_topology=true 'nvidia-smi topo -m; echo $DET_SLOT_IDS'`, and the same with slots=4. Check the set and the task-log line, plus a control without the flag.
  - Re-enable the slots afterwards.
- **Node choice.** In pool 48c96t_512_3090, make node03 and node04 equally full (the same number of free GPUs), with only cross-socket free GPUs on node03. An opted-in 2-GPU task must land on node04 even if BestFit and the hash prefer node03. Then make node03 fuller: the opted-in task must go to node03, as without the flag (packing wins).
  - Shape the free sets with running placeholder tasks, not with `det slot disable`: a disabled slot leaves the agent's device list, which makes the pool non-homogeneous and turns the cross-agent tie-break off (7.1).
  - Gate check: disable one free slot on node03 with `det slot disable`, then make the free counts equal again (one more placeholder GPU on node04). The opted-in task must now go where the flag-off control goes. Re-enable the slot afterwards.

---

## 16. PR shape
One PR after 0.41.0, with commits in this order:
1. aproto wire, agent collection (topology, link, NVML errors, timeout), the GPU exclude list (D24), the `gpu-topology` subcommand, and the build/workflow and check.sh changes.
2. Master state, reconnect, the API proto and regenerated bindings, assembly and health classification (NVML part).
3. The DCGM XID lookup (gpu_health_xid.go, cache, docs). It is self-contained and can be dropped without touching anything else (D9).
4. CLI and WebUI.
5. The config field, selection, interchangeable node choice, reservation under the agent lock, the retry rule, the task-log line, and docs and release note.

The candidate-image check (Phase 1) runs before merge.

---

## Revisions

Findings of the design review are numbered C1-C8 (revision 2), and findings of the verification pass V1-V5 (revision 3). The body cites them where they shaped a rule.

- **Revision 1.** Initial design: topology ranked before the packing score in node choice, the GPU set computed during the scheduler tick and validated at reservation, and the owner requirements known at the time.
- **Revision 2** (design review):
  - C1 Missing owner requirements. Added the link generation (information only), downgrade = current width < maximum width per GPU (replacing a rule relative to the agent's widest link), the WebUI health encoding, and a definition of "errored" with its coverage (2.2, 3, 10-12).
  - C2 Topology-first node choice broke the agreement between the priority scheduler's simulation and the real allocation (the N1, A, N2 case in 7.2). Topology now only breaks ties among agents with the same numSlots and numEmptySlots as today's winner.
  - C3 A failed reservation is not retried on the next tick by itself. Added a bounded retry in schedulerTick (8.3). The set is now chosen under the agent lock at reservation (8.1), which removes the stale-set race.
  - C4 The release check could pass without loading NVML. `gpu-topology` now calls `Init()` before detection, and CI asserts `ERROR_LIBRARY_NOT_FOUND`/12 in the real image (2.3, 13).
  - C5 The rollback risk was understated. Templates with the key reject experiment creation, and policies that force it are silently ignored (14.3).
  - C6 Evidence corrected. node06's x8 set is the measured case and node05's 4-GPU sets are inferred. The claim "width does not matter without P2P" holds on Rome but not on Genoa, so tier 2 uses width second (6.1, 6.2).
  - C7 A draining slot would be chosen every time it is in the best set. The opted-in reservation now skips slots that are not enabled (8.2).
  - C8 The glibc coupling relied on the runner image. Replaced by an objdump GLIBC check against the base image and runs inside the image (13, D14).
  - Minor: docs in NUMA terms; `det slot disable` has no `--drain`; the `refreshAgentStateCacheFor` nil dereference added to D17.
- **Revision 3** (verification):
  - V1 Equal counts were not enough with mixed agent sizes (BestFit and WorstFit counterexamples) or with preemption. The cross-agent tie-break now runs only in homogeneous pools whose scheduler does not preempt, gated through a new `findFits` argument at every call site. The guarantee is restated as one that holds within a scheduling pass (0, 7, D4, 14, Tests).
  - V2 The NVML-to-wire mapping was garbled. Replaced by two one-to-one tables and a table test (2.2).
  - V3 The NVLink probe fell under the error rule and would have turned every RTX 3090 red. It now stops at the first non-SUCCESS return and never sets an error (2.2, 3.2).
  - V4 NVML returns would print as NVML prose in production. Every return now goes through a local name table plus the code (2.2, 2.3).
  - V5 The XID range query could miss events at the window boundary. It now uses `max_over_time(...[5m])` at a 300 s step (3.3).
  - Minor: the scope of the retry variable; the JSON tag of `AgentSummary.GPUTopology`; TERMINATED slots drawn as Free.
- **Revision 4** (this document):
  - D24, decided by the owner: GPUs left out of an agent are reported and shown as "excluded", and never scheduled (2.4, and briefly in 3-5, 10-16, Tests and Risks).
  - Owner decisions recorded: the flag is off by default, P2P capability comes only from the agent's measurement, and the WebUI encoding is approved.
  - Prepared for public review: cluster facts stated inline, cluster-setup references pinned to a commit, and release references updated for 0.41.0 (#35, #36).

---

## Files
- agent/internal/agent.go
- agent/internal/options/options.go, options_test.go; agent/cmd/determined-agent/init.go (the exclude list, D24)
- agent/internal/detect/detect.go, nvidia.go, detect_test.go (the exclude list, D24)
- agent/internal/detect/topology.go, topology_nvml.go, topology_nvml_stub.go, topology_test.go, topology_nvml_test.go, topology_nvml_stub_test.go
- agent/cmd/determined-agent/gpu_topology.go, gpu_topology_test.go, root.go
- agent/Makefile; go.mod; go.sum
- .github/workflows/fork-release.yml; tools/fork/check.sh; tools/fork/smoke.sh
- master/pkg/aproto/gpu_topology.go, gpu_topology_test.go, master_message.go
- master/internal/rm/agentrm/gpu_topology.go, gpu_topology_test.go, agent_state.go, agent_state_test.go, agent.go, agent_test.go, fitting.go, fitting_test.go, resource_pool.go, resource_pool_test.go, priority.go and fair_share.go (only the new `findFits` argument), priority_test.go
- master/internal/sproto/scheduler.go
- master/internal/gpu_health_xid.go, gpu_health_xid_test.go, api_agents.go, api_agents_test.go
- schemas/expconf/v0/resources.json; schemas/test_cases/v0/experiment.yaml, merging.yaml
- master/pkg/schemas/expconf/experiment_config.go, zgen_resources_config_v0.go; master/pkg/schemas/zgen_schemas.go
- master/pkg/model/experiment_config.go, compat.go, agent.go, agent_test.go
- master/internal/trial.go, command/command.go, api_generic_tasks.go, generic_task_resume.go, core.go
- master/internal/authz/obfuscate.go, obfuscate_test.go
- proto/src/determined/agent/v1/agent.proto; proto/pkg/agentv1/agent.pb.go
- harness/determined/common/api/bindings.py; harness/determined/cli/agent.py; harness/tests/cli/test_agent.py
- webui/react/src/services/api-ts-sdk/api.ts, types.ts, services/decoder.ts
- webui/react/src/utils/gpuTopology.ts, gpuTopology.test.ts
- webui/react/src/pages/ResourcePool/GpuTopology.tsx, GpuTopology.module.scss, GpuTopology.test.tsx, ClusterTopology.tsx
- docs/reference/experiment-config-reference.rst, job-config-reference.rst, deploy/master-config-reference.rst, deploy/agent-config-reference.rst; docs/release-notes/gpu-topology-placement.rst

## Tests

**Agent collection**
- topology_nvml_test.go (linux&&cgo, go-nvml `pkg/nvml/mock`):
  - **TestCollectReturnCodeRule**: the mock returns (TOPOLOGY_INTERNAL, ERROR_NOT_SUPPORTED) for the common ancestor, (P2P_STATUS_OK, ERROR_UNKNOWN) for P2P, NOT_SUPPORTED for the max link generation, and GPU_IS_LOST for the current width of GPU 3. Expect: level and P2P unknown; gen max 0 with no error; GPU 3's Error containing "GetCurrPcieLinkWidth: ERROR_GPU_IS_LOST (15)"; no other GPU in error. Fails if values are read without checking SUCCESS (the zero-value trap), or if NOT_SUPPORTED is treated as an error.
  - **TestCollectNode07Like**: 8 mocked GPUs with made-up UUIDs and node07's bus ids ("00000000:21:00.0" style), NODE/SYS levels, P2P OK, GPU1 width 8 of 16, gen 1 of 4, and NUMA from an injected sysfs reader. Expect 8 GPUInfo with width and gen as given, 28 links with UUIDA<UUIDB, bus ids normalised ("0000:21:00.0"), and CollectedAt and DriverVersion set. Fails without the gen fields, the level/P2P mapping or the normalisation.
  - **TestCollectOnlyDetectedUUIDs**: node01-like, 7 detected UUIDs. Handles are requested only for those 7 (the mock records calls) and there are 21 links; the GPU at 81:00.0, which was neither detected nor excluded, is never touched. Fails if the collector enumerates all NVML devices or keys by NVML index.
  - **TestCollectExcludedGPUs** (D24): node01-like, 7 slot UUIDs plus the UUID of 81:00.0 as excluded, and a ninth mocked GPU that is neither. Expect 8 GPUInfo, `Excluded` set only on 81:00.0, 28 links, and no handle request for the ninth GPU. An NVML error on the excluded GPU sets only its own `Error`.
  - **TestCollectLibraryNotFound**: Init returns ERROR_LIBRARY_NOT_FOUND. Expect UnknownReason "NVML init: ERROR_LIBRARY_NOT_FOUND (12)", no GPU Error, no panic, Shutdown not called. Fails if an init failure is reported as a GPU error (red) or aborts agent start.
  - **TestCollectHandleLookupFails**: DeviceGetHandleByUUID returns ERROR_NOT_FOUND for one UUID. That GPU has Error set and the other 7 are collected normally.
  - **TestCollectNVLinkCount**: 4 enabled links to the peer plus 1 to a non-GPU bus gives NVLinks=4.
  - **TestCollectNVLinkProbeNoError** (RTX 3090-like): `GetNvLinkState` returns FEATURE_DISABLED for links 0-3 and ERROR_INVALID_ARGUMENT for link 4. Expect no Error on any GPU, NVLinks=0 for every pair, and no call for link 5 or higher (the mock records calls). A second case returns ERROR_NOT_SUPPORTED for link 0 with the same expectations. Fails if the NVLink probe is subject to the return-code rule.
  - **TestMapTopologyLevelAndP2PStatus** (table): every row of both tables in 2.2, including both spellings of P2P value 1, P2P_STATUS_UNKNOWN, and an out-of-range value of each type giving "". Fails if INTERNAL maps to PIX, NODE to SYS, or IOH_TOPOLOGY_NOT_SUPPORTED to anything but TOPOLOGY_NOT_SUPPORTED.
  - **TestNVMLReturnFormatting** (table): every `Return` constant of const.go gives its symbolic name, including ERROR_NOT_READY, ERROR_GPU_NOT_FOUND and ERROR_INVALID_STATE; `nvmlReturnString(ERROR_GPU_IS_LOST)` is "ERROR_GPU_IS_LOST (15)"; an unlisted value gives "UNKNOWN_RETURN (<n>)".
  - **TestCollectErrorsIgnoreNVMLProse**: the mock's `ErrorStringFunc` returns NVML-style prose ("GPU is lost"), and GPU 3's current width returns ERROR_GPU_IS_LOST. Expect Error "GetCurrPcieLinkWidth: ERROR_GPU_IS_LOST (15)", with no prose. `Return.String()` itself cannot be switched to prose in a unit test (go-nvml's `errorStringFunc` is unexported), so the Phase 1 check covers that path on real GPUs: `nvml_init` must read `SUCCESS`, not NVML's prose.
  - **TestCollectTimeout**: a mock whose Init blocks makes collectWithTimeout return UnknownReason "...did not finish within" after the (test-shortened) timeout. Fails if collection can block agent start.
- topology_test.go (no tag): TestDetectGPUTopologyNoCUDA gives nil for CPU and artificial devices; TestDetectGPUTopologyMIG gives UnknownReason for "MIG-" UUIDs.
- detect_test.go **TestDetectExcludeGPUs** (D24), with the nvidia-smi output injected through a small seam in detectCudaGPUs: 8 GPUs and one excluded UUID give 7 devices with IDs 0-3 and 5-7 plus one excluded device; an entry that matches no GPU returns an error that names it; an empty list gives today's result. Fails if a typo can leave the GPU in the device list.
- options_test.go: `DET_EXCLUDE_GPUS`, `--exclude-gpus` and the config key `exclude_gpus` all set the option.
- topology_nvml_stub_test.go (`!linux || !cgo`, run with `CGO_ENABLED=0`): the stub's UnknownReason. Fails (the package does not build) if go-nvml is imported without the tag.
- cmd test **TestGPUTopologySubcommandInitsFirst**: with an injected library whose Init returns ERROR_LIBRARY_NOT_FOUND and a detector that returns CPU devices, the JSON has `nvml_init` "ERROR_LIBRARY_NOT_FOUND", code 12, and exit 0. Fails if Init is skipped when no CUDA device is detected (C4).
- cmd test **TestGPUTopologySubcommandExcludeGPUs** (D24): with an injected detector that returns 8 CUDA GPUs and a mock library, `--exclude-gpus <unknown UUID>` exits 0, and the JSON has `exclude_error` naming the entry, the 8 devices and an empty `excluded`. With the UUID of one of the 8 GPUs, the JSON has 7 devices, 1 entry in `excluded` and no `exclude_error`. Fails if the subcommand stops on an unknown entry as the agent does, or splits differently from the agent.

**Wire**
- aproto TestAgentStartedWireCompat: AgentStarted with a nil GPUTopology marshals byte-identical to a golden copy of today's message, and old-agent JSON unmarshals to nil. A GPUInfo with `Excluded` false marshals without the `excluded` key.

**Master state and selection**
- TestNewGPUTopologyMapsUUIDsToDeviceIDs: node01-like devices with IDs 0-6 and the UUIDs of host 0,1,2,3,5,6,7. Expect pairKey(3,4) = P2P SYS x8, foreign UUIDs dropped, reversed A/B accepted, and an unknown enum string treated as unknown.
- **TestGPUTopologyKeepsExcludedForDisplay** (D24): an excluded GPU whose UUID is not in Devices is kept by UUID with its links, has no pair key, and is never returned by selectDevices. A GPU marked excluded whose UUID is in Devices is treated as a slot. A GPU that is neither in Devices nor marked excluded is dropped.
- TestPairKeyOrder (table): NVLink(4) < NVLink(2) < P2P x16 PIX < ... < P2P x16 SYS < P2P x8 NODE (width-first) < host-staged NODE x16 == host-staged PIX x16 < host-staged NODE x8 < host-staged SYS x16 < unknown < errored GPU. Fails if a tier, the host-staged width tie-break or the error tier is missing or misordered.
- TestSelectDevicesClusterCases: fixtures in the test file, transcribed from each node's `nvidia-smi topo -m`, `nvidia-smi topo -p2p r` and link-width output, reproduce every cell of the table in 6.4, including the node01 sets with the exclude list, plus the g292 free {1..7} cases (GNS gives {1,2}; P2P OK gives {2,3}). Fails with a level-only key and ID tie-break (node07 n=4 would give {0,1,2,3}), or if P2P is not taken per pair.
- TestSelectDevicesLeximax; TestSelectDevicesUnknownAndGates (nil/unknown gives (nil, unknownKey); n<2 gives nil; len(eligible)<n gives nil); TestSelectDevicesCap (20 free, n=10 gives nil quickly); BenchmarkSelectDevices8Choose4.

**Node choice and consistency (C2, V1)**

In these tests "noPreemption" is the new `findFits` argument. Where an ungated tie-break must be shown to differ, agent A's topology is unknown and B's is known, so a tie-break that ran would pick B. The sequence tests apply each fit with `addTaskToAgents` before fitting the next request. HashDistance is md5 of the allocation and agent ids, so each test finds ids that give the stated orders by searching a small candidate set at test time instead of hardcoding them.
- fitting_test.go **TestTopologyTieBreakAmongInterchangeable**: BestFit, noPreemption; node03/04-like A and B, both 8 slots with 4 free, and no other agent. A has free {0,1,4,5}, so every 4-set crosses sockets; B has free {0,1,2,3}, one socket. Request n=4, with ids and hashes chosen so that today's winner W is A. The flag picks B; flag off picks A. Same counts.
- **TestTopologyNeverOverridesPacking**: A has 2 free (cross socket), B has 6 free (same-socket pairs); an opted-in 2-slot request goes to A, the same as flag off. Fails with topology-first.
- **TestTopologyHomogeneityGate**: the setup of TestTopologyTieBreakAmongInterchangeable plus an agent X.
  - X enabled with 4 slots, all used: the pool is not homogeneous, so the opted-in request goes to A (W) with `preferTopology=true`.
  - X disabled, or draining: X is ignored, the pool is homogeneous, and the request goes to B.
  - X enabled with 8 slots, 7 used, and its free slot disabled with `patchSlotState`, so X keeps 7 entries in `Devices`: not homogeneous, and the request goes to A.
  - Fails if homogeneity counts disabled or draining agents, or is not checked.
- **TestTopologyTieBreakOffWithPreemption**: the setup of TestTopologyTieBreakAmongInterchangeable with noPreemption=false. The request goes to A with `preferTopology=true`, so the set inside A is still chosen at reservation.
- **TestTopologyMixedSizesSequence** (BestFit, noPreemption, one pass on one agent map): A and B 8 slots with 4 used, S and T 4 slots idle; requests [O(2, opted), R(4), M(8, multi-agent)], with ids chosen so that W for O is A and R's hash order is B, S, A, T. Expect the flag-off outcome with the flag on: O on A, R on B, M on S and T. Fails without the homogeneity gate (O goes to B, R to S, and M finds no fit).
- **TestTopologyWorstFitMixedSizes** (WorstFit, noPreemption): A and B 8 slots with 4 used, S 4 slots with 2 used, all at Score 0.5; requests [O(2, opted), R(2), M(4)], with W for O = A and R's hash order A, S, B. Expect the flag-off outcome: O on A, R on S, M on B. Fails without the gate (O on B, R on A, M finds no fit).
- **TestFindFitsDefaultInvariants**: (I1) findFits is equally long with the flag on and off over a table of requests and agent sets; (I2) flag off, or flag on with n=1, gives the identical agent and preferTopology=false; (I5) with no agent having topology, the same agent id as flag off. **TestFindFitsMultiAgentUnchanged**: an opted-in 16-slot request on two idle 8-GPU agents gives two fits with preferTopology=false.
- priority_test.go **TestOptInKeepsSimulationAndAllocationConsistent**: preemption off; for 200 seeds, run `Schedule` plus `allocateResources` on a real resourcePool with mock agent handlers that use map-order device choice, over [N1(6, non-opted), A(2, opted), N2(8, non-opted)] on two idle 8-GPU agents, and a 3-agent variant with an extra opted-in task. Assert that every request in toAllocate is allocated, and that the final (numSlots, numEmptySlots) multiset equals the flag-off run. Fails (for some seeds) with topology-first.
- priority_test.go **TestOptInWithPreemptionMatchesFlagOff**: priority scheduler with `preemption: true`; A and B 8 slots; A runs Q (4, not preemptible), B runs P (4, preemptible, lower priority than U); pending O (2, opted) and U (8) with the same priority, O queued first; ids chosen so that W for O is A. Expect the flag-off result with the flag on: O placed on A in the simulation and in `allocateResources`, and `toRelease` = [P]. Fails if the tie-break runs under preemption (O goes to B, nothing is released, and U waits).

**Reservation, draining, retry (C3, C7)**
- agent_state_test.go TestAllocateSelectedDevices: reserves exactly {5,7} in ID order. A busy, absent or duplicated device gives an error with no containerState entry and no device marked. TestDeepCopyKeepsGPUTopology.
- agent_test.go **TestAllocateFreeDevicesPreferTopology**: a node02-like agent with a draining free slot 1 and free {0,1,2,3}; n=2 with preferTopology reserves {0,2}, never slot 1. With only {1,5} free and n=2 it falls back to the default allocation. With unknown topology it calls the unchanged path.
- resource_pool_test.go **TestRetryAfterFailedOptInReservation**: a failed opted-in allocation, or a non-opted failure after an opted-in success in the same tick, leaves `rp.reschedule == true` after schedulerTick, at most 3 consecutive times. A failed non-opted allocation in a tick without opted-in requests leaves it false (identical to today). TestAllocateResourcesOptedInPublishesTaskLog: a ContainerLog event carrying the set and the worst pair is published via rmevents.
- agent_test.go TestAgentStartedRefreshesGPUTopology: fresh T1; a reconnect with the same devices and T2 (P2P GNS to OK, width 8 to 16) replaces it; changed devices keep today's shutdown path.

**Health and XID**
- api_agents_test.go **TestClassifyGPUHealth** (table):
  - Error set gives ERROR;
  - an XID gives ERROR;
  - width 8 of 16 gives LINK_DEGRADED;
  - 8 of 16 plus an XID gives ERROR (precedence);
  - 16 of 16 gives HEALTHY regardless of gen 1 of 4;
  - width 0 gives UNKNOWN;
  - unknown topology gives UNKNOWN;
  - unknown topology plus an XID gives ERROR;
  - XID check failed with 16 of 16 gives HEALTHY;
  - an excluded GPU (D24) with an XID gives ERROR on its own entry and changes no slot's state.
- api_agents_test.go TestGetAgentExcludedGPUs (D24): an excluded GPU comes after the slots with `device_id` -1 and `excluded` true; a link with an excluded end carries both UUIDs and -1 for that end; XIDs are joined to it by UUID.
- gpu_health_xid_test.go:
  - the query builder emits the exact PromQL of 3.3: `max by (gpu_uuid, xid) (max_over_time(...[5m])) > 0` with job, det_cluster, gpu_uuid!="", xid!="", xid!="0" and the excluded 13|31|43|45, at step 300 over 24 h. Fails if `max_over_time` or the `[5m]` range is missing;
  - the parser derives first and last seen from the step timestamps with a value > 0;
  - the cache serves a second call within 30 s without a query;
  - a Prometheus error sets `xid_check_error` and returns normally within the timeout;
  - GetAgents with `exclude_slots=true` makes no query (a fake records calls);
  - integration disabled gives no query and `xid_checked=false` with an empty error.
- model/agent_test.go: ToProto carries gpu_topology; `json.Marshal` of an AgentSummary has the key `gpu_topology`, and omits it when nil. obfuscate_test.go: ObfuscateAgent sets it to nil and the join is skipped.

**Config**
- schemas test_cases: a sanity-valid `prefer_gpu_topology: true`, a sanity-invalid string value, and merging template true plus user false giving false.
- An expconf test: a defaulted config without the key marshals with no "prefer_gpu_topology" key.
- A model test: strict decode of `{"resources":{"prefer_gpu_topology":true}}` into CommandConfig succeeds and ToExpconf carries it.
- The GPUTopologyPreferred table (nil, false, true).
- The allocation-request capture tests assert `FittingRequirements.PreferGPUTopology` at each site.

**Clients**
- harness/tests/cli/test_agent.py (mocked API): `det agent list` prints the GPU Topology and GPU Health strings of the table in 11.1 (node02, node01, node01 with the exclude list, node07, an XID case, g292, unknown, blank for CPU or an old master). `det agent describe` prints the per-GPU table with Health, Width and Gen and the Details text, and an excluded GPU as a row with Slot `-` and State EXCLUDED, the level matrix, and the P2P matrix only when not all pairs are OK. `--json` includes the raw gpu_topology.
- WebUI vitest:
  - utils/gpuTopology.test.ts: summary strings (the CLI fixtures), NUMA/switch grouping (g292 gives 4 PIX groups in 1 NUMA node), and the state-to-palette map (no container gives Free; TERMINATED gives Free; RUNNING gives Running; ASSIGNED/PULLING/STARTING and an undefined state give Pending).
  - GpuTopology.test.tsx:
    - one tile per health state renders a dot with the right class and aria-label, and the hollow dot for unknown;
    - a disabled slot renders the striped class and the "disabled" label, and a draining slot "draining", with the fill unchanged;
    - an excluded GPU (D24) renders a striped tile labelled "excluded", without a slot id, with its health dot and info button;
    - hovering the info button shows the tooltip with width/gen, the x8 explanation, the error and XID times, and the XID check status; clicking pins it;
    - unknown topology renders the reason text with hollow dots;
    - the legend text is present.

**Release and smoke**
- Release workflow assertions (CI):
  - `go version -m` shows CGO_ENABLED=1 for the agent and 0 for the master;
  - readelf NEEDED lists glibc only;
  - objdump max GLIBC ≤ the `$BASE_IMAGE` glibc;
  - in-image `version` succeeds;
  - in-image `gpu-topology` exits 0 with `ERROR_LIBRARY_NOT_FOUND`/12.
- Smoke (tools/fork/smoke.sh, CPU-only agent): `det cmd run --config resources.prefer_gpu_topology=true ...` completes, and `det agent list` shows blank topology and health columns.

## Owner decisions

Decided by the owner:

- The per-task flag is off by default (unset = false), because not every multi-GPU task is DDP (D1, D16).
- P2P capability comes only from what the agent measures, never from the GPU model (R1, D3).
- The WebUI encoding is approved after a preview: slot-state fill, a separate LED health dot (green, amber, red, hollow gray), striped disabled and draining tiles, and an info button with a hover tooltip and a click popover (R4, D13).
- D24: GPUs that are deliberately left out of an agent are shown as "excluded" tiles.

The list below gives the recommended option first. Every item is open for the owner unless it says otherwise.

1. **D1 Field name and type** (open; the default off is decided).
    - Recommended: `resources.prefer_gpu_topology`, a plain bool (unset = false) for experiments, NTSC and generic tasks.
    - Alternative: an enum `gpu_placement: default|prefer_topology`, which leaves room for a hard "require" later.
2. **D2 Width against level for P2P pairs** (open).
    - Recommended: width first (min current width of the pair), then level, gated on Phase-0 benchmarks (b) and (c).
    - Measured: node06 x8 set 13.0 against 25.6, and node05 cross-socket x16 pair 19.2 against same-socket x8 pair 12.8. 4-GPU cross-socket sets are inferred.
    - Fallback: level first, width second, a one-line comparator change that still avoids node07's x8 GPU.
3. **D3 Pairs without P2P** (g292 requirement; open for the ranking; P2P from measurement only is decided).
    - Recommended: P2P-capable pairs (NVML P2P READ == OK, measured at agent start) rank above host-staged pairs. Host-staged pairs rank by NUMA class (PIX/PXB/PHB/NODE count as one "same NUMA node" class; switch locality neutral), then by width.
    - After the patched driver and an agent restart, g292's PIX pairs win automatically.
    - Confirm PIX against NODE without P2P with benchmark (d).
4. **D4 Node choice** (open).
    - Recommended: topology breaks ties only among agents with the same numSlots and numEmptySlots as today's winner, and only in pools where every enabled, non-draining agent has the same number of slots and the scheduler does not preempt. Elsewhere the agent is today's choice and only the set inside it is chosen.
    - What it guarantees: within one scheduling pass it never changes whether another task fits (requests with BlockedNodes aside, which are retried), and the simulation and the allocation stay consistent. The priority scheduler's logic is untouched; its three `findFits` calls pass the preemption flag. Across passes the choice matters like today's hash tie-break.
    - On this cluster it moves tasks only between node03 and node04 (pool 48c96t_512_3090), and only while neither has a disabled slot.
    - Alternative (a): within-agent set selection only, never a different agent. Simplest, with no pool conditions, but an opted-in task on node03/node04 can get a cross-socket set while the equally full other node offers a same-socket one.
    - Alternative (b): topology before the packing score, with the simulation's agent choice pinned for opted-in tasks. That needs a plan carried from priority.go to allocateResources, and it changes packing for non-opted tasks in the same pool (N2 in 7.2 waits).
5. **D5 Multi-agent tasks** (open). Recommended: unchanged; whole agents leave no choice, and the task log says so.
6. **D6 GPUs with an agent-reported NVML error in opted-in sets** (open). Recommended: rank them last (tier 4), for opted-in tasks only, and never exclude them; operators should disable the slot. Alternative: ignore errors in the score.
7. **D7 Unknown topology** (old agent, NVML failure or timeout, stub, MIG, just after a master restart; open). Recommended: still allocate with today's device choice, with no ranking penalty beyond the tie-break, and never exclude the agent.
8. **D8 Definition of "errored"** (open).
    - Recommended: the NVML handle lookup, `GetPciInfo` or one of the four PCIe link width and generation queries fails at agent start (anything other than SUCCESS or NOT_SUPPORTED), OR a DCGM XID outside the application class (13, 31, 43, 45) within the last 24 h.
    - The NVML init failure of a whole agent is "unknown", not "error". The NVLink probe and the pairwise topology and P2P queries never make a GPU errored.
    - Out of reach: GPUs hidden from the agent container (node01's 81:00.0 as deployed today; with D24 an excluded GPU is covered), runtime faults without DCGM, link retrains after start.
9. **D9 XID lookup in this PR** (open).
    - Recommended: yes, as its own commit, gated on the existing `integrations.task_resources`, with a 30 s cache and a 5 s timeout, never failing the API.
    - It is the only signal that turns a dot red for a GPU that faults while the agent runs.
    - Alternative: a follow-up PR, in which case red comes from NVML at start only.
10. **D10 XID window and excluded codes** (open).
    - Recommended: 24 h lookback; exclude 13, 31, 43, 45 (the cluster alert's application class), as a code constant.
    - Alternatives: 1 h, or "since the agent's collection time", which a reboot clears.
11. **D11 Dot when the XID check is unavailable** (open). Recommended: follow the NVML data (green possible) and state the XID status in the details. Alternative: hollow gray for every GPU, which hides downgrade information on clusters without the integration.
12. **D12 NVLink** (open). Recommended: in the wire format, the ordering (NVLink first, more links better) and the collection, mock-tested only. The collection is a probe that stops at the first non-SUCCESS return and never marks a GPU errored. The cluster has no active NVLink (the RTX 3090s have links but no bridge).
13. **D13 Visibility scope** (open for the scope; the WebUI encoding is decided).
    - Recommended, all in this PR: the API field; the `det agent list` GPU Topology and GPU Health columns; `det agent describe`; the WebUI panel in the pool page's Topology section with the R4 encoding.
    - A launch-form checkbox (shown when slots ≥ 2) is a later small PR.
14. **D14 Agent build** (open).
    - Recommended: keep building on the `ubuntu-22.04` runner, with the objdump GLIBC ≤ image check and the in-image runs.
    - Alternative: build the agent inside a `$BASE_IMAGE` container (gcc plus the pinned Go tarball), which removes the runner dependence. Switch to it when ubuntu-22.04 runners are deprecated.
15. **D15 go-nvml version** (open). Recommended: v0.12.9-0, with the smaller dependency bump (testify 1.10.0) and all needed APIs verified at that tag. Alternative: v0.13.4-0 (testify 1.12.1, go.yaml.in/yaml/v3).
16. **D16 Defaults and switches** (open; the default off is decided).
    - Recommended: no pool-level or task_container_defaults default, no master kill switch, no new master config keys (the agent's exclude list, D24, is the only new option). Groups can force the flag with templates or invariant policies (not silently on a downgraded master; see 14.3).
    - Rollback is users not setting the flag, or the previous agent image (with the device list back first if an exclude list is in use, 14.3).
17. **D17 Pre-existing quirks** (open). Recommended: separate small PRs, not this one, to keep I2/I4:
    1. allocateFreeDevices leaves `containerState[cid]` on "not enough devices" (agent_state.go:160/175);
    2. a slot disabled with drain stays allocatable for default-path tasks (agent_state.go:418-423); the opt-in path now avoids it;
    3. agent/internal/agent.go:119 formats `devices` instead of `err`;
    4. `refreshAgentStateCacheFor` dereferences a nil state when `a.State()` fails (resource_pool.go:509-513);
    5. `PatchSlotState` does not notify listeners, so a slot disable does not trigger a reschedule (agent.go:547-560).
18. **D18 Per-allocation task-log line** (open). Recommended: yes. It is the only user-visible feedback for a soft preference and needs no new types.
19. **D19 `determined-agent gpu-topology` diagnostic subcommand** (open). Recommended: yes. Release CI and the Phase-1 candidate-image check depend on it.
20. **D20 Non-opted device choice** (open). Recommended: keep today's map-order choice ("exactly as today"). It fragments sockets and limits how often opted-in tasks find a same-socket set; socket-aware packing for default tasks is a separate decision.
21. **D21 Non-amd64 and non-Linux agents** (open). Recommended: accept the stub (topology unknown). The fork releases linux/amd64 only.
22. **D22 PR shape** (open). Recommended: one PR after 0.41.0 with the five ordered commits of section 16. Alternative: visibility and health first (commits 1-4) as their own PR, so the data runs on the cluster before scheduling relies on it.
23. **D23 Follow-ups outside this repo** (open). Recommended after release:
    - mention the flag in cluster-setup docs/05 "Using P2P in jobs" (timeless text), and link the cluster-setup GPU topology notes (`docs/06_GPU_Topology.md`, added by WU-CVGL/cluster-setup#14, not in 5b02037) to the WebUI view;
    - replace the "Leaving out a faulty GPU" procedure in cluster-setup services/determined/README.md (today a `docker run --gpus device=<UUIDs>` list) with all GPUs plus the exclude list (D24);
    - check that the cluster's job-launch tooling passes `resources.prefer_gpu_topology` through.
24. **D24 Excluded GPUs** (decided by the owner).
    - Show GPUs that are deliberately left out of an agent, such as node01's faulty GPU at 81:00.0, as "excluded" tiles.
    - Today such a GPU is hidden with `docker run --gpus device=<UUIDs>` and is therefore invisible to the agent and every view. That setup keeps working.
    - Mechanism: the agent is started with access to all GPUs plus an exclude list of GPU UUIDs (`exclude_gpus`, `--exclude-gpus`, `DET_EXCLUDE_GPUS`). It reports the excluded GPUs with their topology and link data, but never registers them as slots (2.4, 4, 5). The API marks them `excluded` with no slot id (10). The WebUI draws them as striped tiles labelled "excluded" without a slot id (12), and the CLI lists them (11).
    - The owner confirmed that the faulty card is fine as long as no workload runs on it, so the agent may query it with NVML. The 60 s timeout bounds only the NVML collection step. Detection's nvidia-smi now also sees the card and has no timeout; a hang there blocks agent start and an nvidia-smi error stops the whole agent, with the docker device list as the fallback (2.4).
    - Safety: an entry that matches no GPU stops agent start, and an agent image rollback needs the device list back first (14.3).

## Risks
- **Master downgrade** (14.3):
  - stored experiment configs with the key, including `false`, cannot be restored or re-parsed;
  - templates containing the key reject experiment creation;
  - invariant policies forcing it are silently ignored.
  - omitempty limits the first case to experiments that set the key. The release note states all of this.
- **cgo/NVML crash risk.** A fault inside libnvidia-ml at agent start takes the node's agent down (restart loop), and a cgo segfault cannot be recovered. The 60 s timeout covers NVML collection hangs, not crashes. Recovery is the previous agent image tag. go-nvml is widely used (NVIDIA's k8s device plugin, DCGM tooling).
- **Exclude list** (D24).
  - An older agent image ignores `exclude_gpus`. Started with all GPUs, it registers the excluded GPU as a slot, and tasks can run on it. Go back to the docker device list before rolling an agent image back (14.3).
  - A typo in the list stops agent start instead of exposing the GPU (2.4).
  - The agent queries the excluded GPU at every start, first with nvidia-smi in detection, then with NVML. The 60 s timeout covers a hang in the NVML collection only, not a crash inside NVML (see the crash risk above). The owner confirmed that querying node01's faulty card is safe.
  - Detection's nvidia-smi has no timeout and now also lists the excluded card (2.4). A hang there blocks agent start. An nvidia-smi error leaves no detected GPU, so the fail-closed check stops the whole agent and node01 loses all 7 good slots. The Phase 1 node01 check runs the same detection; the docker device list is the fallback.
- **glibc coupling.** The agent is now dynamically linked. The objdump check fails the release before publishing if the build host's glibc symbols exceed the image's, and the in-image `version`/`gpu-topology` runs prove the binary starts.
  - Builds still depend on the `ubuntu-22.04` runner label. GitHub supports at most two GA Ubuntu images and deprecates the oldest once a new one is GA (actions/runner-images README; the retirement date was not checked). When it goes, switch to D14's container build.
  - Dev builds now need gcc; CGO_ENABLED=0 still builds the stub.
- **Dependency.** go-nvml v0.12.9-0 bumps testify 1.9.0 to 1.10.0. Run the full Go test suite after `go mod tidy`. NVML's P2P enum has the typo spelling CHIPSET_NOT_SUPPORED, so map by value.
- **Unverified in-container NVML.** NVML, sysfs NUMA and link reads inside the det-agent container, and node01's slot-to-host mapping, have not been checked directly. Phase 1 closes this before merge.
- **"P2P OK" is not proof.** NVML P2P READ == OK does not prove that BAR1 P2P works (docs/05 caveat). A node with a broken P2P setup could get sets that rank as P2P but run host-staged.
- **Snapshots, not live data.** Width, generation and NVML errors are snapshots at agent start. A link that retrains, or a GPU lost while the agent runs, is not seen until the next start, except through a DCGM XID. Width can change between boots (docs/05:240, :374); the agent re-measures at each start.
- **Narrow error coverage.** NVML-at-start errors rarely fire, because a GPU lost before start usually makes `nvidia-smi` fail, and the agent then registers fewer or no GPUs (nvidia.go:74-79). Red therefore depends mostly on the DCGM XID lookup. Without `integrations.task_resources`, runtime faults are not shown.
- **XID semantics.**
  - The 24 h window means a GPU fixed by a reboot stays red until the window passes; the details show the times.
  - Application-class XIDs are excluded, so a driver bug that surfaces as Xid 31 alone is not shown.
  - The 5-minute counter window and the 5-minute `max_over_time` range make first and last seen approximate: a reported time can trail the event by up to about 10 minutes.
  - A Prometheus outage only adds a note.
- **Load from the lookup.** Prometheus is queried from the agent API path. It is bounded by the cache (one query per 30 s per master), a 5 s timeout and gating on `exclude_slots=false`, and a slow Prometheus can add up to 5 s to one uncached call.
- **Limited node choice.** Node choice changes only between equally full agents of the same size, and only in pools whose enabled agents all have the same size and whose scheduler does not preempt. An opted-in task can therefore get a cross-socket set on a fuller agent while an emptier agent offers a same-socket set, and in a mixed-size or preempting pool it never changes agent at all. A `det slot disable` on node03 or node04 turns the node choice off for that pool until the slot is enabled again. That is the price of not changing which other tasks fit (D4).
- **Across passes.** Which of two equally full agents holds an opted-in task can decide, when a task on one of them later finishes, whether a large task fits. Today's hash tie-break has the same property; the flag does not make it systematically better or worse.
- **BlockedNodes edge case.** A later request whose BlockedNodes distinguish two interchangeable agents can fail its real fit in the same tick. The bounded retry (3 ticks) re-schedules it; after the cap it waits for the next ordinary trigger, as today.
- **The size of the benefit varies.**
  - With P2P, socket locality is worth about 27% on Rome and about 7% on Genoa (busbw).
  - The large wins are avoiding x8 GPUs (about 2x) and keeping NUMA locality without P2P on Rome (about 2.7x).
  - Compute-bound jobs gain less than busbw suggests; run benchmark (e) before advertising the flag.
  - Cross-socket all-x16 4-GPU sets are unmeasured (gates b, c).
- **Fragmentation.** Non-opted tasks still take random free GPUs (map order), so opted-in tasks often find only cross-socket sets on busy nodes. The preference is soft and never waits, so it cannot fix this (D20).
- **Restart window.** Right after a master restart, agents count as unknown until their reconnect AgentStarted arrives (seconds). Opted-in tasks scheduled then get the default device choice, and the WebUI shows hollow dots. The same applies during a mixed rollout with old agents.
- **Large agents.** Agents with more than 16 free GPUs can exceed the 20000-subset cap for some n and fall back to the default choice. This is not reachable on the current 8-GPU nodes.
- **Where the flag has no effect.** It is silently ignored on Kubernetes and dispatcher RMs and for multi-agent fits; the docs say "agent resource manager only". Under RBAC, users without the sensitive-agent permission see no topology or health (ObfuscateAgent clears it).
- **Agent start time.** NVML Init and the first handle lookups initialise GPUs that have persistence mode off, which adds seconds to agent start.
- **Proto toolchain.** Proto regeneration needs the pinned toolchain (protoc ≥ 24 and the versions in proto/get-deps.sh), and bindings.py and api.ts produce large generated diffs (as in PRs #27/#34).
