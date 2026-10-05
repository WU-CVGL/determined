# GPU topology view and topology-aware GPU sets (design)

Agents measure each GPU's topology, P2P status, PCIe link and NVML errors when they start. The agent API, `det agent list`, `det agent describe` and the WebUI resource-pool page show the result. An opt-in per-task flag then chooses a well-connected GPU set inside the agent that the scheduler picks. The feature ships after 0.41.0.

Status: design for review. The implementation lands as separate PRs (section 11). Owner decisions are in section 15.

**References**
- Code: WU-CVGL/determined `wu/main` at dd3be1fce, unless another repository is named. Among the cited files, the 0.41.0 release (#36, f8fa7b904, stacked on #35) changes only three: `master/internal/core.go` (two `agentrm.New` calls, same line count), `agentrm/agent_resource_manager.go` (the cited code is unchanged, but :171 becomes :220) and `.github/workflows/fork-release.yml` (only the default release tag). The cited lines stay valid otherwise.
- go-nvml: tag v0.12.9-0, `pkg/nvml`.
- NCCL: NVIDIA/nccl at 12df1a11afad322be5a204a2db890161cbf8131d, `src/graph/paths.cc`.
- Cluster: WU-CVGL/cluster-setup at 5b02037. `docs/05_GPU_P2P_GeForce.md` is cited as docs/05:<line>.

**Cluster facts used in this document.** They were read on the hosts with `nvidia-smi topo -m`, `nvidia-smi topo -p2p r`, `nvidia-smi --query-gpu=index,pci.bus_id,pcie.link.gen.max,pcie.link.width.current,pcie.link.width.max` and sysfs.
- Nodes 01-08 have 8 GPUs, 4 per CPU socket, one NUMA node per socket (NPS1). Pairs inside a socket are NODE, pairs across sockets are SYS. There is no PIX, PXB or active NVLink. P2P READ is OK for every pair. P2P WRITE has not been read yet (Phase 1, section 12).
- Links read at x8 of x16: node01 host GPUs 3 and 5, node05 GPUs 1 and 4, node06 GPU 6, node07 GPU 1.
- node01's host GPU at bus 81:00.0 is faulty. Today it is hidden from the agent with a docker device list, so node01's slots 0-6 are host GPUs 0,1,2,3,5,6,7, with x8 at slots 3 and 4.
- The RTX 3090 nodes (01, 03, 04) have NVLink hardware but no bridge.
- g292 (joins later) has one NUMA node and 8 GPUs in four PIX pairs ({0,1}, {2,3}, {4,5}, {6,7}), NODE between pairs, all x16. Its P2P READ is GPU_NOT_SUPPORTED for every pair. A patched driver may enable P2P later.

---

## 1. Scope

### 1.1 What v1 does
- **PR A, collection and display.** The agent collects topology, P2P status, PCIe link observations and NVML errors with go-nvml at start (section 2). The master keeps them in agent state (section 4) and serves them in the agent API. `det agent list`, `det agent describe` and the WebUI show them with the approved LED and tile encoding (section 5). PR A also adds the agent's GPU exclude list (D24, section 2.6).
- **PR B, in-agent GPU set selection.** The per-task flag `resources.prefer_gpu_topology` chooses which free GPUs a task gets inside the agent that the scheduler picks (section 6). The scheduler picks the agent exactly as today.
- **Before PR B:** two small fixes for draining slots, P1 and P2 (section 6.1).
- **Later, each as its own PR:** recent critical XIDs from DCGM-Exporter (section 9), width-aware ranking (section 10), and choosing the agent by topology, which is a scheduler change (section 6.6).

### 1.2 Owner requirements
- R1: P2P capability comes only from what the agent measures with NVML at process start, never from the GPU model. After a driver change (g292 with the patched modules), an agent restart is enough.
- R2: Topology in the API, the WebUI and the CLI (`det agent list` summary and per-GPU detail).
- R3: Per GPU, the current and maximum PCIe link width and generation. These are observations at agent start (section 3.2).
- R4: The WebUI encoding approved after a preview (section 5.4).
- R5: A concrete definition of "error" (section 3.1).
- R6: Link observations and errors in `det agent list` and in the per-GPU detail.
- R7: P2P is part of the score. P2P-usable pairs rank first. Without P2P, PCIe locality inside a NUMA node is neutral. The docs say so.

### 1.3 Invariants (each has a test, section 13)
- I1 Scheduling is unchanged. `findFits` (master/internal/rm/agentrm/fitting.go:72-94), `candidateList.Less` (fitting.go:46-66), priority.go and fair_share.go are not edited. The flag is read only after the fit is chosen: in `allocateResources` and in the agent's reservation.
- I2 With the flag off, or fewer than 2 slots, the reservation runs today's code.
- I3 An opted-in reservation succeeds exactly when today's reservation would. Both take n devices from the same eligible set (P1) under the same agent lock. Slot counts, and so every other fit, stay as with the flag off.
- I4 Unknown topology means today's device choice from the same eligible set. So does a report without rankable information, where every pair among the free GPUs is unknown (tier 3 of S1). Partial information is used: its unknown pairs rank at tier 3 (S2).

### 1.4 Not done
- no DB persistence of topology or health, and no migration;
- no new master configuration key; one new agent option, the exclude list (D24);
- no new agent-to-master message type (section 3.3);
- no pool default, no task_container_defaults default, no master kill switch (D16);
- no WebUI launch-form checkbox;
- no Kubernetes or dispatcher RM support (they read only SlotsNeeded);
- no change to how tasks without the flag pick devices (D20).

---

## 2. Agent collection (PR A)

### 2.1 Hook and lifetime
- In agent/internal/agent.go, right after `detect.Detect` (115-120), call `detect.DetectGPUTopology(devices, excluded)`. It never returns an error and never fails agent start.
- Put the result into `AgentStarted` (160-165). Pass it through `reconnectFlow` (call at 208, signature at 307, send at 352-357). A reconnect re-sends the value from process start. Every process start measures again (R1).
- **N5 Timeout.** Collection runs in a goroutine on its own copy of the inventory (N6). The agent waits at most 60 s, then sends the inventory with `UnknownReason: "NVML collection did not finish within 60s"` and starts normally. The goroutine stays blocked, because a cgo call cannot be cancelled, and its copy is discarded. Detection's own `nvidia-smi` calls have no timeout (agent/internal/detect/nvidia.go:33, :71), so N5 only keeps the new code from adding a hang.

### 2.2 Files and NVML calls
- `detect/topology.go` (no build tag): `DetectGPUTopology(devices, excluded []device.Device) *aproto.GPUTopology`. It returns nil only when there is neither a CUDA device nor an excluded GPU.
- **N6 Inventory first.** Before any NVML call it builds `GPUs` from detection: one entry per CUDA slot, then one per excluded GPU, each with `UUID` and `Excluded` set. Every return path keeps this inventory: NVML init failure, the N5 timeout, the stub build and MIG ("MIG-" UUIDs, `UnknownReason: "MIG instances: GPU topology not collected"`) send it with `UnknownReason` and no telemetry. A collection fills each entry's telemetry fields only as far as its calls succeed (N1). Collection never adds or drops an entry, so it queries exactly the detected slot UUIDs and the excluded UUIDs, never another GPU. This respects `visible_gpus` and a docker device list.
- `detect/topology_nvml.go` (`//go:build linux && cgo`) imports `github.com/NVIDIA/go-nvml/pkg/nvml`: `collect(lib nvml.Interface, inventory []aproto.GPUInfo, numa func(bdf string) *int, now func() time.Time) *aproto.GPUTopology`.
- `detect/topology_nvml_stub.go` (`//go:build !linux || !cgo`) returns the inventory with `UnknownReason: "agent built without NVML support (needs linux and cgo)"`. The guard is required: with CGO_ENABLED=0 an unguarded go-nvml import does not compile (D21).
- NVML inside the agent container: every agent DeviceRequest has the `utility` capability, so the NVIDIA container toolkit injects libnvidia-ml.so.1. The image does not bundle it.

Steps:
1. `lib.Init()`. If it fails, return the inventory with `UnknownReason: "NVML init: " + nvmlReturnString(ret)`, for example `NVML init: ERROR_LIBRARY_NOT_FOUND (12)`, which go-nvml returns when dlopen fails (init.go:20-24). All GPUs are then unknown, not in error: a missing library is a deployment problem. Otherwise `defer lib.Shutdown()`, and set `CollectedAt = now()` and `DriverVersion` (best effort).
2. Per UUID, the health calls: `DeviceGetHandleByUUID`, `GetPciInfo`, `GetCurrPcieLinkWidth`, `GetMaxPcieLinkWidth`, `GetCurrPcieLinkGeneration`, `GetMaxPcieLinkGeneration`. **N1, the return-code rule:**
   - SUCCESS: use the value;
   - ERROR_NOT_SUPPORTED: leave the field unknown (zero), with no error;
   - anything else: leave the field unknown and append `"<call>: " + nvmlReturnString(ret)` to the GPU's `NVMLError`, for example `GetCurrPcieLinkWidth: ERROR_GPU_IS_LOST (15)`. A failed handle lookup leaves the entry with only its inventory fields and the error.

   Details:
   - The bus id from `GetPciInfo` is NUL-trimmed and normalised to the sysfs form ("00000000:A1:00.0" becomes "0000:a1:00.0").
   - NUMA comes from `/sys/bus/pci/devices/<bdf>/numa_node`. -1 or a read error is unknown, with no error. `DeviceGetNumaNodeId` is not used: these nodes do not support it ("GPU NUMA ID N/A").
   - The maximum is the device-and-system maximum (`DeviceGetMaxPcieLinkWidth`/`Generation`, zz_generated.api.go:133-134), the same as `nvidia-smi pcie.link.width.max`. It is not `DeviceGetGpuMaxPcieLinkGeneration` (:109). A card in a slot that is physically x8 then reports max 8.
3. **N2, the NVLink probe.** It never sets an error. For each link from 0 to `NVLINK_MAX_LINKS`-1 (18, const.go:48), call `GetNvLinkState`. If it is FEATURE_ENABLED, call `GetNvLinkRemotePciInfo` and count the link for the pair when the remote bus id is one of ours. Stop at the first non-SUCCESS return and log it at Debug. Why: NVML answers only for links a device has. An RTX 3090 without a bridge is expected to answer FEATURE_DISABLED for links 0-3 and ERROR_INVALID_ARGUMENT for link 4; a GPU without NVLink answers ERROR_NOT_SUPPORTED for link 0. Under N1 that would turn every 3090 red.
4. Per unordered pair, `GetTopologyCommonAncestor`, mapped by value (const.go:1193-1198): INTERNAL 0 to INTERNAL, SINGLE 10 to PIX, MULTIPLE 20 to PXB, HOSTBRIDGE 30 to PHB, NODE 40 to NODE, SYSTEM 50 to SYS, any other value to unknown. Then the P2P queries of N3 (section 2.3).
   - A pairwise failure makes only that value unknown. It never sets a GPU's error.
   - Zero-value trap: TOPOLOGY_INTERNAL and P2P_STATUS_OK are both 0. A value is used only when its return is SUCCESS.
5. Log one Info line, for example `GPU topology: 8 GPUs, NUMA 4+4, levels NODE/SYS, P2P usable, width below max at start: slot 1 (x8 of x16), NVML errors: none, driver 610.57.04`. Log at Warn when the topology is unknown or a GPU has an NVML error.

### 2.3 N3: P2P needs READ and WRITE in both directions
- For each ordered pair (A, B), call `A.GetP2PStatus(B, P2P_CAPS_INDEX_READ)` and `A.GetP2PStatus(B, P2P_CAPS_INDEX_WRITE)` (const.go:1221-1222). That is four queries per unordered pair, 112 on an 8-GPU node.
- All four raw statuses go on the wire, mapped by value (const.go:1206-1213): 0 OK; 1 CHIPSET_NOT_SUPPORTED (go-nvml has two spellings for 1, so the switch is on the integer); 2 GPU_NOT_SUPPORTED; 3 TOPOLOGY_NOT_SUPPORTED (IOH_TOPOLOGY_NOT_SUPPORTED); 4 DISABLED_BY_REGKEY; 5 NOT_SUPPORTED; 6 (P2P_STATUS_UNKNOWN) and any other value: unknown. A query that does not return SUCCESS is unknown.
- One direction is usable when READ == OK and WRITE == OK. The pair value comes from one function in aproto that the master, the API and the subcommand share:
  - USABLE: all four statuses are OK, so both directions are usable;
  - NOT_USABLE: at least one of the four is a known status other than OK;
  - unknown: otherwise, that is, a failed query or P2P_STATUS_UNKNOWN and no known non-OK status.
- Why: NCCL treats a GPU pair as P2P-capable only when NVML reports both READ and WRITE as OK (src/graph/paths.cc:414-435). For NVLink-connected GPUs without both, it reports an error instead. So NVLinks > 0 does not bypass N3: a pair ranks as NVLink only when it is also USABLE (section 6.3).
- The cluster has read only READ so far. Phase 1 compares WRITE.

### 2.4 N4: formatting NVML returns
- Every NVML return that reaches a string (UnknownReason, `NVMLError`, logs, the subcommand's JSON) goes through two local helpers. Never `ret.String()`, `ret.Error()` or `%v`. Why: go-nvml's `Return.String()` and `Error()` call a package variable (return.go:27-37). It starts as a built-in table (return.go:42-102), but once the library loads it points at NVML's own `nvmlErrorString` (lib.go:116-117), which returns prose such as "GPU is lost".
- `nvmlReturnName(ret)`: the symbolic name from a local table generated from every `Return` constant in const.go:1504-1534. The built-in table lacks ERROR_NOT_READY (27), ERROR_GPU_NOT_FOUND (28) and ERROR_INVALID_STATE (29). An unlisted value gives `UNKNOWN_RETURN`.
- `nvmlReturnString(ret)` = name + " (" + code + ")", for example `ERROR_GPU_IS_LOST (15)`.

### 2.5 Diagnostic subcommand (D19)
`determined-agent gpu-topology [--visible-gpus X] [--slot-type T] [--exclude-gpus U]`, in agent/cmd/determined-agent/gpu_topology.go, added in root.go (`AddCommand`, :19).
- It calls `lib.Init()` first, independent of detection, and prints `nvml_init` (the symbolic name) and `nvml_init_code`. The stub prints `NOT_BUILT` and -1. On SUCCESS it also prints `driver_version`.
- It then runs the agent's detection, the exclude split and the collection, and prints `{nvml_init, nvml_init_code, driver_version, devices, excluded, topology}` as JSON, with the raw P2P statuses and the derived pair value.
- It exits 0 even without NVML, and non-zero only on flag errors. An exclude entry that matches no GPU is printed as `exclude_error` instead of stopping the command, with the detected devices unchanged and an empty `excluded`.
- It is used by the release check (section 8), the Phase 1 check and operators.

### 2.6 Excluded GPUs (D24)
Today a faulty GPU is hidden by starting the agent container with `docker run --gpus device=<UUIDs of the other GPUs>`. The agent cannot see it, so no view shows it. With the exclude list the agent sees every GPU, reports the excluded ones, and never offers them as slots.

- **X1 Option.** The config key `exclude_gpus` in the agent YAML, or the flag `--exclude-gpus`: a comma-separated list of GPU UUIDs, never indices. There is no environment variable (X3 says why). `registerString` binds `DET_<FLAG>` automatically (agent/cmd/determined-agent/init.go:35-40), so the option is registered with a variant that binds only the flag and the config key. The flag sits next to `visible-gpus` (init.go:103) and the field next to `VisibleGPUs` (agent/internal/options/options.go:58). The agent container is started with all GPUs.
- **X2 Detection, fail closed.** `detect.Detect` (detect.go:19) moves every CUDA device whose UUID is listed into a separate `excluded` list, with a helper the subcommand shares. The others keep their nvidia-smi index as device ID (nvidia.go:97-107), as with `visible_gpus`, so node01's slots become host indices 0-3 and 5-7. Gaps are safe: task containers get their GPUs by UUID (agent/internal/containers/spec.go:85-97), and the harness reads `DET_SLOT_IDS` (harness/determined/_info.py:278) and uses only their number. An entry that matches no detected CUDA GPU stops agent start with an error that names it, so a typo never hands the faulty GPU to tasks.
  - Excluded GPUs are not in `AgentStarted.Devices`, so the master never schedules them and no task container can request one.
  - With slot type `auto` (the default), excluded GPUs count as found CUDA GPUs: an agent whose GPUs are all excluded has no slots and does not fall back to ROCm or CPU slots (detect.go:78-100).
  - Excluded GPUs are in the inventory with `Excluded` (N6), even when NVML fails, and get the same NVML calls (section 2.2). The owner confirmed that querying the faulty card is fine while no workload runs on it.
  - Detection now also sees the excluded card. Its `nvidia-smi` call has no timeout (nvidia.go:71), so a hang there blocks agent start. An `nvidia-smi` error returns no devices (nvidia.go:74-79); the entry then matches nothing, and fail-closed stops the whole agent, so node01 would lose all 7 good slots. Phase 1 runs this detection on node01. The fallback is the docker device list.
  - Changing the list changes the agent's devices. Drain first: the master stops an agent whose devices changed on reconnect (master/internal/rm/agentrm/agent_state.go:261-292).
- **X3 Older agent images.**
  - The agent decodes its YAML config with `yaml.DisallowUnknownFields` (agent/cmd/determined-agent/run.go:114-123). An older agent therefore refuses to start with `exclude_gpus` in its YAML.
  - An older agent also refuses the unknown flag `--exclude-gpus`: cobra returns an error and the agent exits (main.go:39-40).
  - An older agent would silently ignore an unknown environment variable, register the faulty GPU as a slot, and run tasks on it. That is why there is no environment variable.
  - Rolling back the agent image: first restore the docker `--gpus device=<UUIDs>` list and remove the option. Until then an older agent refuses to start. Phase 1 checks this refusal once.

---

## 3. GPU health (PR A)

### 3.1 H1: states and what the LED means
The master API layer computes the state once per GPU (section 5.2). The first matching row wins.

| State | Condition | Dot | CLI |
|---|---|---|---|
| ERROR | an NVML health call failed at the agent's last start (`nvml_error` is set) | red | `error` |
| LINK_BELOW_MAX | at agent start, current and max width were both known and current < max | amber | `narrow` |
| OK | topology known; at agent start both widths were known and equal | green | `ok` |
| UNKNOWN | anything else: no report (older agent, just after a master restart), NVML init failed or timed out, stub build, MIG, or width unknown | hollow gray | `unknown` |

- Green means no NVML error at agent start and link width at max at agent start. It is not a verified healthy GPU. The XID PR adds "and no recently reported critical XID" (section 9).
- The link generation never changes the state.
- An excluded GPU gets its own state by the same rules. It never affects a slot.
- The details (CLI and WebUI) list four facts separately:
  1. Link at agent start: `x<cur> of x<max>, Gen<c> of Gen<m>`, marked as an observation (section 3.2).
  2. NVML errors at agent start: `none`, or each call with its code.
  3. Recent critical XIDs: `not collected` until the XID PR (section 9).
  4. Data time and query status: the agent's `collected_at`; with the XID PR, also the XID query time and status.

### 3.2 Link width is an observation
- Width and generation are read once at agent start and carry `collected_at`. They are an observation, not a confirmed fault. NVIDIA documents that the current link generation and width "may be reduced when the GPU is not in use" (`nvidia-smi --help-query-gpu`, fields `pcie.link.gen.current` and `pcie.link.width.current`). The cluster also saw a width change between boots (docs/05:240, :374).
- So amber means "link width below max at agent start". The UI text says: "A lower link width lowers this link's bandwidth cap. Actual collective throughput depends on the workload."
- v1 ranking does not use width (section 6.3). Phase 0 run f compares idle and under-load readings before the width PR (section 10).

### 3.3 Coverage, and why the agent sends no runtime health
- ERROR covers GPUs that `nvidia-smi` still lists but NVML cannot query. It rarely fires. When `nvidia-smi` fails, detection returns no devices (nvidia.go:74-79), and with slot type `auto` the agent falls back to CPU (detect.go:78-100). A GPU lost before start therefore usually never reaches collection: the agent registers fewer slots, and on a reconnect the device check stops it (agent_state.go:274-275). The cluster's DCGM `gpu-missing` alert covers that case.
- Link data and NVML errors are a snapshot at agent start. A GPU lost, or a link retrained, while the agent runs shows only at the next start.
- Runtime health from the agent (NVML XID events) would need a new agent-to-master message. An older master decodes an unknown message into a `MasterMessage` whose fields are all nil, reaches `check.Panic` in the `default:` case (master/internal/rm/agentrm/agent.go:685-687, master/pkg/check/check.go:25-28), and panics. A mixed rollout would crash the master. So v1 only adds an `omitempty` field to `AgentStarted`, and runtime XIDs come later from the master side (section 9).

---

## 4. Wire format and master state (PR A)

### 4.1 Wire (agent to master, JSON)
New master/pkg/aproto/gpu_topology.go, shared by agent and master. The zero value of every enum is "", meaning unknown. JSON tags are snake_case. Every field except UUID is `omitempty`, and 0 or nil means unknown.
- `GPULinkLevel string`: INTERNAL | PIX | PXB | PHB | NODE | SYS.
- `GPUP2PStatus string`: OK | CHIPSET_NOT_SUPPORTED | GPU_NOT_SUPPORTED | TOPOLOGY_NOT_SUPPORTED | DISABLED_BY_REGKEY | NOT_SUPPORTED.
- `GPUP2PCaps{Read, Write GPUP2PStatus}`: one direction.
- `GPUInfo{UUID; PCIBusID; NUMANode *int; PCIeLinkWidth, PCIeLinkWidthMax, PCIeLinkGen, PCIeLinkGenMax int; NVMLError string; Excluded bool}`.
- `GPULink{UUIDA, UUIDB string (UUIDA < UUIDB); Level; NVLinks int; P2PAToB, P2PBToA GPUP2PCaps}`.
- `GPUTopology{UnknownReason; CollectedAt time.Time; DriverVersion; GPUs []GPUInfo; Links []GPULink}`.
- `P2PUsability(GPULink) GPUP2PUsability` (USABLE, NOT_USABLE, "" unknown): the N3 rule.
- `AgentStarted` (master_message.go:79-84) gets `GPUTopology *GPUTopology \`json:",omitempty"\``.

Compatibility:
- The master decodes agent messages with plain `json.Unmarshal` (master/pkg/ws/ws.go:142), so an older master ignores the field.
- An older agent sends nil, which means unknown.
- An enum string unknown to the master degrades to unknown.
- No version gate and no new message type.

### 4.2 Master state
New master/internal/rm/agentrm/gpu_topology.go holds an immutable `gpuTopology` built from the wire value and `AgentStarted.Devices`:
- It maps UUIDs to device IDs for CUDA devices only, and stores per-GPU info and per-pair values by device ID.
- It drops links whose UUIDs are not slots, normalises a reversed A/B (swapping the two directions with it), and treats a missing pair as unknown.
- It keeps excluded GPUs (marked `Excluded`, UUID not a slot) and their links in a display-only part keyed by UUID, also when the report is unknown (N6). They never reach `selectDevices`. A GPU marked excluded whose UUID is a slot is treated as a slot and logged at Warn.
- Its unknown reason is the wire's `UnknownReason`, "agent <version> does not report GPU topology" (wire nil with CUDA devices), or "not reported since the master started".
- Topology stays out of `device.Device`, which is a map key (agent_state.go:49) and is compared on reconnect.

agent_state.go gets the field `gpuTopology *gpuTopology` and `setGPUTopology(...)`, which replaces the pointer and never mutates it. `deepCopy` (199-214) leaves it out: its copies feed the scheduler's fit and the pool's count queries (`GetResourceSummary`, `ValidateResources`, `CapacityCheck`), all through `agents.list` or `refreshAgentStateCacheFor` via `agent.State` (agent.go:195-204), which use counts only. Selection (S3) and `summarize` (section 5.2) read the live state under `a.mu`. It is not in `snapshot()`, so there is no DB change.

There is one set site: master/internal/rm/agentrm/agent.go `HandleIncomingWebsocketMessage`, after the `if a.started { ...match checks } else { a.agentStarted(...) }` block (614-643) and before `a.started = true` (645). It covers a fresh registration, a reconnect, and an agent restart with the same devices, which refreshes P2P, width and errors (R1). After a master restart the snapshot restore (agent.go:147-163) leaves the topology nil until the agent's AgentStarted arrives, a few seconds; meanwhile the agent counts as unknown and its excluded GPUs are not shown. A device mismatch keeps today's shutdown path.

---

## 5. API, CLI and WebUI (PR A)

### 5.1 Proto (proto/src/determined/agent/v1/agent.proto)
Every message, field and enum value gets a comment (buf DEFAULT+COMMENTS lint).
```
message GpuTopology {
  string unknown_reason = 1;                   // "" = known
  google.protobuf.Timestamp collected_at = 2;  // agent clock, at agent start
  string driver_version = 3;
  repeated GpuInfo gpus = 4;                   // slots by device_id, then excluded GPUs by pci_bus_id, then uuid ("" bus id first)
  repeated GpuLink links = 5;
}
message GpuInfo {
  int32 device_id = 1;                         // -1 for an excluded GPU
  string uuid = 2;
  string pci_bus_id = 3;
  int32 numa_node = 4;                         // -1 unknown
  int32 pcie_link_width = 5;                   // at agent start; 0 unknown
  int32 pcie_link_width_max = 6;
  int32 pcie_link_gen = 7;                     // at agent start; 0 unknown
  int32 pcie_link_gen_max = 8;
  string nvml_error = 9;                       // NVML health-call errors at agent start
  GpuHealth health = 10;
  bool excluded = 11;                          // left out by the exclude list (D24)
}
message GpuP2pCaps { GpuP2pStatus read = 1; GpuP2pStatus write = 2; }
message GpuLink {
  int32 device_a = 1; int32 device_b = 2;      // -1 for an excluded end
  string uuid_a = 3; string uuid_b = 4;
  GpuLinkLevel level = 5;
  int32 nvlinks = 6;
  GpuP2pCaps p2p_a_to_b = 7; GpuP2pCaps p2p_b_to_a = 8;   // raw NVML statuses
  GpuP2p p2p = 9;                              // derived by N3
}
enum GpuLinkLevel { GPU_LINK_LEVEL_UNSPECIFIED = 0; _INTERNAL = 1; _PIX = 2; _PXB = 3; _PHB = 4; _NODE = 5; _SYS = 6; }
enum GpuP2pStatus { GPU_P2P_STATUS_UNSPECIFIED = 0; _OK = 1; _CHIPSET_NOT_SUPPORTED = 2; _GPU_NOT_SUPPORTED = 3;
                    _TOPOLOGY_NOT_SUPPORTED = 4; _DISABLED_BY_REGKEY = 5; _NOT_SUPPORTED = 6; }
enum GpuP2p { GPU_P2P_UNSPECIFIED = 0; GPU_P2P_USABLE = 1; GPU_P2P_NOT_USABLE = 2; }
enum GpuHealth { GPU_HEALTH_UNSPECIFIED = 0; GPU_HEALTH_OK = 1; GPU_HEALTH_LINK_BELOW_MAX = 2; GPU_HEALTH_ERROR = 3; }
```
- Links between slots have device_a < device_b. A link with an excluded end has -1 for that end and is ordered by uuid_a < uuid_b. Clients join slots by device_id and excluded GPUs by UUID.
- `Agent` gets `GpuTopology gpu_topology = 12;`, the next free tag (the message uses 1-11, with 5 reserved; agent.proto:39-67). It is unset only for agents with neither CUDA slots nor excluded GPUs, so an agent whose GPUs are all excluded still has it. It stays out of `devicev1.Device`.
- Regenerate with `make -C proto build` (protoc >= 24 and the versions pinned in proto/get-deps.sh), then `make -C bindings` (bindings.py and api.ts).

### 5.2 Assembly and API
- `model.AgentSummary` (master/pkg/model/agent.go:15-25) gets `GPUTopology *agentv1.GpuTopology \`json:"gpu_topology,omitempty"\``, and `ToProto` (76-99) copies it.
- `summarize` (master agentrm/agent.go:745-768) fills it under `a.mu` from a new `agentState.gpuTopologyProto()`. There is one entry per CUDA slot, with device_id and uuid from `slotStates`, so the shape is the same when the topology is unknown. Excluded GPUs follow, from the inventory (N6), also when the topology is unknown. It is nil only for agents with neither CUDA slots nor excluded GPUs.
- master/internal/api_agents.go: `GetAgents` (46-56) drops `gpu_topology` when `exclude_slots` is set. Otherwise it, and `GetAgent` (62-85), call `classifyGPUHealth` (H1) for every GPU. One function means the CLI and the WebUI never disagree.
- RBAC: `authz.ObfuscateAgent` (master/internal/authz/obfuscate.go:69) sets `gpu_topology` to nil.

### 5.3 CLI (harness/determined/cli/agent.py)
`det agent list` (`list_agents`, 23-63) gets two columns after "Slots", also in `--json` as `gpu_topology` and `gpu_health`:
- **GPU Topology:** the NUMA group sizes of the slots, the distinct levels from best to worst, and the P2P state of the pairs of slots (excluded GPUs do not count):
  - `p2p` when every pair is USABLE;
  - `no-p2p(<status>)` when no pair is USABLE and at least one is NOT_USABLE. `<status>` is the first known non-OK status of the NOT_USABLE pair with the lowest slot ids, in the order A->B READ, A->B WRITE, B->A READ, B->A WRITE, where A is the lower slot id;
  - `p2p?` when every pair is unknown;
  - otherwise usable pairs over all pairs, for example `p2p 12/28`;
  - ` (<k> unknown)` follows when some but not all pairs are unknown, for example `p2p 12/28 (3 unknown)`;
  - no P2P part with one slot, and `no slots` for an agent whose GPUs are all excluded.

  The P2P matrix of `det agent describe` shows each pair.
- **GPU Health:** `ok` when every GPU is OK and none is excluded. Otherwise the non-OK GPUs by slot id, grouped as `error`, `narrow`, `unknown`. Narrow GPUs show `x<cur> of x<max> at start`. Excluded GPUs come last as `excluded: <bus id>` (the UUID when the bus id is unknown), with their state in parentheses when it is not ok.

| Agent | GPU Topology | GPU Health |
|---|---|---|
| node02 | `4+4 NODE/SYS p2p` | `ok` |
| node01 today | `4+3 NODE/SYS p2p` | `narrow: 3,4 (x8 of x16 at start)` |
| node01 with the exclude list | `4+3 NODE/SYS p2p` | `narrow: 3,5 (x8 of x16 at start); excluded: 81:00.0` |
| g292 today | `8 PIX/NODE no-p2p(GPU_NOT_SUPPORTED)` | `ok` |
| P2P usable inside each socket only | `4+4 NODE/SYS p2p 12/28` | `ok` |
| topology unknown | `unknown: <reason>` | `unknown` |
| CPU agent, or an older master | blank | blank |

The `p2p` rows assume Phase 1 finds WRITE OK where READ is OK.

`det agent describe AGENT_ID [--json]` (new, via `bindings.get_GetAgent`):
- a header with id, pools, version, enabled or draining, driver version, `collected_at`, and the topology summary or the unknown reason;
- one row per slot, then one per excluded GPU: Slot (`-` when excluded), State (FREE, allocation id, DISABLED, DRAINING, EXCLUDED), Health, UUID, PCI bus id, NUMA, Width cur/max, Gen cur/max, and Details with the four facts of H1;
- a level matrix (excluded GPUs labelled by bus id), and a P2P matrix with READ and WRITE per direction when not every pair is USABLE;
- `--json` prints the raw `gpu_topology`.

Bus ids let operators map node01's slots to host indices.

### 5.4 WebUI
- **Placement.** The resource-pool page's "Topology" section (webui/react/src/pages/ResourcePool/ClusterTopology.tsx:61-71), fed by `getAgents({ excludeSlots: false })` (pages/ResourcePool/ResourcepoolDetail.tsx:106, polled at :152). For an agent with `gpuTopology`, a new `GpuTopology` panel replaces the bar-style slot strip. Other agents keep `NodeElement`.
- **Data.** types.ts `Agent` (244-252) gets `gpuTopology?`, and `Resource` gets `draining?`. services/decoder.ts `jsonToAgents` (129-180) maps both; today it drops `draining`. GPUs join slots by `device_id`. Excluded GPUs come from `gpus` alone.
- **Tile fill = slot state**, with the palette `SlotAllocationBar` uses (`getStateColorThemeVar(SlotState.*)`): no container, or TERMINATED, is Free; RUNNING is Running; ASSIGNED, PULLING, STARTING or an undefined state is Pending. The fill never shows health. The mapping is its own function, because `SlotAllocationBar` counts every non-RUNNING container as pending (SlotAllocationBar.tsx:101-102).
- **Disabled** (`enabled=false`): diagonal stripes over the fill, labelled "disabled", or "draining" when `draining`. Never a fill colour.
- **Excluded:** a tile without a slot id, with the Free fill, the stripes, the label "excluded", a health dot and an info button. Popover: "Left out by the agent's exclude list. No task runs on this GPU."
- **Health dot:** a 10 px LED dot in the tile's top-right corner. OK uses `--theme-status-success`, LINK_BELOW_MAX `--theme-status-warning`, ERROR `--theme-status-critical`, each with a 2 px `--theme-surface` ring. UNKNOWN is hollow: `--theme-surface` fill with a 2 px `--theme-status-inactive-strong` border. Every dot has a 1 px `--theme-surface-on-weak` outline, so it stays visible on any fill and in dark mode (tokens in utils/colors.ts), and the `aria-label` "GPU health: <state>".
- **Info button** (hew `Icon name="info"`): hover or focus shows a hew `Tooltip`; a click pins the same content in a hew `Dropdown` popover (the pattern of components/MultiSortMenu.tsx:285-296). Content: slot, state, UUID, bus id, NUMA, then the four facts of H1. For amber it adds the text of section 3.2 and a link to the agent docs (section 7.1).
- **Layout:** the summary line (the CLI strings); NUMA boxes that contain switch groups (connected PIX/PXB components) that contain tiles; a collapsible pairwise matrix (level text, non-usable P2P marked, unknown "?") that scrolls horizontally on narrow screens; and a text legend ("● ok ● link below max at start ● error ○ unknown; striped = disabled, draining or excluded"), so colour is never the only signal.
- **Unknown topology:** the text "GPU topology unknown: <reason>", tiles from the inventory (N6): the slot tiles by `device_id` and the excluded tiles, all with hollow dots, no grouping and no matrix.
- Pure helpers live in utils/gpuTopology.ts and share fixtures with the CLI tests.

---

## 6. GPU set selection (PR B)

### 6.1 Prerequisite: the draining-slot fixes (small separate PRs, merged first)
- **P1 A draining slot can be allocated again.** A slot drained through the REST API stays in `a.Devices`: `updateSlotDeviceView` removes only disabled slots that are not draining (agent_state.go:418-422). Once no container holds the device, it counts in `numEmptySlots()` (agent_state.go:99-106) and `allocateFreeDevices` (158-186) can pick it. The master then sends `StartContainer` (agent.go:224) before `startContainer` notices the disabled slot, and that check only logs (agent_state.go:345-346).
- **P1 contract: one allocatable set.** A device is allocatable exactly when its slot is enabled and not draining and no container holds it. The scheduler's counts and the free entries of `a.Devices` both describe this set. The fix PR tests that none of these makes a draining slot allocatable; today each does:
  1. draining an idle slot (free at once);
  2. draining a running slot whose task then exits (`deallocateContainer`, agent_state.go:189-196, frees it);
  3. draining a reserved slot whose container has not started, then cancelling the reservation (`deallocateContainer` from `resourcesReleased`, resource_pool.go:260 and :274, or from the rollback in `allocateResources`, :409).
- **P2 Slot state changes do not notify the scheduler.** `PatchSlotState` (agent.go:547-559) never calls `notifyListeners`, unlike agent enable and disable (agent.go:511, :543). So enabling or disabling a slot does not set `rp.reschedule` (resource_pool.go:556-559).
- **What PR B relies on.** After the P1 fix, the free entries of `a.Devices` (nil container) are exactly the eligible slots, so the scheduler's counts and the set selection use the same set. The fix PR chooses the mechanism, for example removing a drained slot's device once no container holds it, as a plain disable does at once.
- PR B selects only from this eligible set. It adds no draining special case and has no fallback to a path that may pick a draining slot.

### 6.2 Per-task flag
`resources.prefer_gpu_topology`: boolean or null, schema default null, read as false unless it is exactly true (D1). Off by default (decided).
- **Experiments.** A property in schemas/expconf/v0/resources.json, between max_slots and priority (additionalProperties is false). `ResourcesConfigV0` (master/pkg/schemas/expconf/experiment_config.go:203-217) gets `RawPreferGPUTopology *bool \`json:"prefer_gpu_topology,omitempty"\``; `omitempty` keeps the key out of stored configs that do not set it (section 7.2). A helper `GPUTopologyPreferred()` reads it nil-safely. Regenerate with `make -C master gen`; `check-gen` must be clean.
- **Commands, notebooks, shells, TensorBoards.** master/pkg/model/experiment_config.go `ResourcesConfig` (85-97) gets `PreferGPUTopology *bool` with the same tag. This is required, because the merged config is decoded with `DisallowUnknownFields` (master/internal/api_command.go:133-142). `ToExpconf` (master/pkg/model/compat.go:28-46) copies it. The `is_single_node` ban (master/pkg/model/command_config.go:71) is not copied: `det cmd run` with torchrun is a DDP path.
- **Generic tasks** use `expconf.ResourcesConfig` and decode strictly (master/internal/api_generic_tasks.go:128). Read the field nil-safely: WithDefaults is never applied to them.
- **Carrier.** `sproto.FittingRequirements` gets `PreferGPUTopology bool`, set at trial.go:422 and :470, command/command.go:171, api_generic_tasks.go:399, generic_task_resume.go:379 and core.go:955 (all under master/internal). checkpoint_gc.go:181 stays false.
- **Merge.** An explicit user value wins over a template (core_experiment.go:293-300, `schemas.Merge`). An invariant config policy can force it for experiments (configpolicy/task_config_policy.go:191-209).
- **Clients** need no change. `--config resources.prefer_gpu_topology=true` works for `det e create`, `det cmd run`, `det shell start`, `det notebook start`, `det tensorboard start` and `det task create`. A new client against an older master fails loudly: a schema error for experiments, an unknown-field 400 for the others.

### 6.3 S1: pair key
A tuple compared lexicographically; smaller is better. v1 does not use link width (decided, D2). It ranks by topology and P2P only, with one exception while D6 is open: tier 4 ranks a pair last when either GPU had an NVML error at agent start.

| Tier | Pair | Sub-key |
|---|---|---|
| 0 | NVLinks > 0 and P2P USABLE | 64 - NVLinks |
| 1 | P2P USABLE, level known | level: INTERNAL 0, PIX 1, PXB 2, PHB 3, NODE 4, SYS 5 |
| 2 | P2P NOT_USABLE, level known | 0 if the level is NODE or closer (same NUMA node), 1 if SYS |
| 3 | anything else (unknown) | 0 |
| 4 | either GPU had an NVML error at agent start (D6) | 0 |

- Tier 2 treats PIX, PXB, PHB and NODE as one class. Without P2P, two GPUs behind one switch share one uplink to the host, so switch locality is neutral (D3, Phase 0 run d).
- Evidence on this cluster (NCCL all-reduce busbw at 1 GiB): with P2P, same-socket pairs beat cross-socket pairs by about 27% on Rome (24.5-24.8 against 19.2-19.4 GB/s, docs/05:289-290) and about 7% on Genoa (25.1-25.2 against 23.5-23.6, docs/05:358-359). Without P2P on Rome, same-socket pairs give 3.4 against 1.2-1.3 (docs/05:289-290).

### 6.4 S2: set key and selection
- `setKey`: the C(n,2) pair keys of a set, sorted worst first. Sets compare lexicographically: worst pair, then second worst, and so on. Exact ties go to the smallest sorted list of device IDs.
- `selectDevices(free []device.Device, g *gpuTopology, n int) ([]device.Device, setKey)` returns nil, meaning today's choice (I4), when n < 2, g is nil or unknown, len(free) < n, or every pair among `free` is unknown (tier 3), so the report has no rankable information there. If any pair is known, or a GPU had an NVML error (tier 4), it selects, and the unknown pairs rank at tier 3.
- Otherwise it enumerates every subset of size n in ID order and keeps only a strictly better key, so the ID tie-break falls out of the order. Brute force is enough: C(8,4) = 70 subsets on this cluster. Above 20000 subsets, which needs more than 16 free GPUs, it returns nil and logs at Debug.

### 6.5 S3: reservation under the agent lock
- resource_pool.go `allocateResources` (423-426) passes `preferTopology: req.FittingRequirements.PreferGPUTopology && len(fits) == 1`. A multi-agent fit takes whole idle agents, so there is no choice (D5).
- master agentrm/agent.go: the `allocateFreeDevices` message (107-110) gets `preferTopology`. `AllocateFreeDevices` (168-182) does, under `a.mu`:
  ```
  if msg.preferTopology && msg.slots >= 2 {
      free := a.agentState.freeDevices()   // nil container in a.Devices, sorted by ID: the P1 eligible set
      if devs, key := selectDevices(free, a.agentState.gpuTopology, msg.slots); devs != nil {
          got, err := a.agentState.allocateSelectedDevices(devs, msg.containerID)
          return allocateFreeDevicesResponse{devices: got, topoKey: key, preferred: true}, err
      }
  }
  devices, err := a.agentState.allocateFreeDevices(msg.slots, msg.containerID)   // today's path
  ```
- `allocateSelectedDevices(devs, cid)` validates first: the list is non-empty, has no duplicates, and every device is in `a.Devices` with a nil container. Only then does it mark the devices and set `containerState[cid]`. It returns the devices sorted by ID; they become `DET_SLOT_IDS` and the order of the DeviceRequests.
- The set comes from live state under the agent lock, not from the tick's cache. It fails only where today's path fails (fewer than n free), so I3 holds and no retry is needed.
- The priority scheduler's simulation (priority.go:263-268) still takes devices in map order on its copies. Its free GPU sets already differ from the real ones today; both sides depend only on counts, which the flag does not change.

### 6.6 Why v1 does not choose the agent by topology
An earlier draft let the flag choose between equally full agents. A review of revision 4 gave a counterexample. The priority scheduler simulates on copies that take devices in map order (priority.go:104, :263-268, :319-325), so on two equally full agents the simulation and the real allocation leave different free GPU sets. A topology tie-break can then put an opted-in task O on agent B in reality where the simulation put it on agent A. A later request in the same pass that blocks A (BlockedNodes from a log-pattern `exclude_node` policy, fitting_methods.go:16-18) can only use B. It fits in the simulation, where B does not hold O, and fails for real, where B does. A retry on a later tick cannot undo the allocation already made. So choosing the agent by topology needs the scheduler itself to plan with topology. That is a separate scheduler change (section 11).

### 6.7 Expected choices on the cluster
All slots free, with slot IDs as deployed (node01's slots 0-6 are host GPUs 0,1,2,3,5,6,7). This assumes Phase 1 finds WRITE OK wherever READ is OK.

| Nodes | n=2 | n=3 | n=4 |
|---|---|---|---|
| node01 to node08 | {0,1} | {0,1,2} | {0,1,2,3} |

- Every 8-GPU node has its first 4 slots on one socket (node01's agent 4+3), all NODE, so the lowest IDs win the ties. With the exclude list, node01's slots are 0-3 and 5-7 and the sets are the same.
- v1 does not avoid x8 GPUs. The 4-GPU set contains one on node01 (slot 3), node05 (GPU 1) and node07 (GPU 1).
- node02 with free {0,1,4,5,6} and n=3 gives {4,5,6}, on one socket.
- g292 today (no P2P, one NUMA node): every pair ties, so n=2 gives {0,1}, and with slot 0 busy {1,2}. After P2P becomes USABLE and the agent restarts, slot 0 busy gives {2,3} (PIX), and n=4 gives {0,1,2,3} (two PIX pairs).

### 6.8 Task-log line (D18)
After `rmevents.Publish(req.AllocationID, ...)` (resource_pool.go:469), for an opted-in request with 2 or more slots, publish a `sproto.ContainerLog`. The allocation subscribed before `pool.Allocate` (agent_resource_manager.go:171), and master/internal/task/allocation.go:263-264 turns it into a task log. The same text goes to `rp.syslog` at Info.
- set chosen: `GPU topology preference: agent <id>, slots 4,5,6,7; worst pair NODE, P2P usable`
- unknown: `GPU topology preference: topology of agent <id> unknown (<reason>); slots chosen as usual`, with the reason "every pair of free GPUs unknown" for the last case of S2
- multi-agent: `GPU topology preference has no effect: the task uses whole agents`

---

## 7. Docs and rollback

### 7.1 Docs (timeless; measured results go in PRs)
- **PR A.** docs/reference/deploy/agent-config-reference.rst, new section "GPU topology and health": NVML through libnvidia-ml.so.1, injected by the NVIDIA container toolkit (the `utility` capability); the fields, read at agent start; P2P READ and WRITE (N3); stub builds report unknown; H1 and section 3.2; NVML codes as name and number; coverage (section 3.3); `determined-agent gpu-topology`; the `exclude_gpus` option with X1-X3. The WebUI popover links here.
- **PR B.** docs/reference/experiment-config-reference.rst (next to `is_single_node`) and job-config-reference.rst: `prefer_gpu_topology`. What it ranks (S1), in NUMA terms: NVML's NODE and SYS are NUMA-based and equal sockets only with NPS1. It ignores link width. It is soft: it never waits and never moves tasks, and it chooses GPUs only inside the agent the scheduler picks. It applies to tasks of 2 or more slots on one agent, with the agent resource manager only. Also: templates and policies, the task-log line, and that agents must restart after a driver change.
- **Each PR** adds a release note under docs/release-notes/ (`:orphan:`). PR A's note says the agent binary is now dynamically linked against the image's glibc, and gives X3. PR B's note gives section 7.2.

### 7.2 Rollback of the master after the flag was used
- Stored experiment configs that contain the key, even `false`, become unparsable. `ActiveExperimentConfig` (master/internal/db/postgres_experiments.go:848-860) validates against the old schema (master/pkg/schemas/expconf/parse.go:38-47), so restore fails (master/internal/restore.go:62-65). `omitempty` limits this to experiments that set the key.
- Templates decode strictly (core_experiment.go:293-299, master/internal/templates/service.go:60-64). Creating an experiment from a template with the key fails until the key is removed.
- Invariant config policies decode leniently (task_config_policy.go:193, :207). A policy that forces the flag is silently ignored.
- NTSC and generic-task snapshots (master/internal/command/models.go:24, :27) drop the key. New NTSC requests with it get an unknown-field 400.

Agents: a new agent works with an older master, which ignores the field. An older agent with a new master shows unknown. For an agent image rollback with an exclude list, see X3.

---

## 8. Build and release (PR A)
- **go.mod:** add `github.com/NVIDIA/go-nvml` v0.12.9-0 (D15). It needs testify 1.10.0 (from 1.9.0, go.mod:49). Run the full Go suite after `go mod tidy`.
- **.github/workflows/fork-release.yml, "Build Linux binaries" (213-222):** drop the job-wide `CGO_ENABLED: '0'`. master and gotmpl keep `CGO_ENABLED=0`. The agent builds with `CGO_ENABLED=1 go build -trimpath -tags netgo,osusergo ...`; the tags keep `net` and `os/user` pure Go. The job then fails unless:
  1. `go version -m` shows `CGO_ENABLED=1` for the agent (this catches a silent stub build) and `CGO_ENABLED=0` for the master;
  2. `readelf -d` lists only glibc libraries as NEEDED for the agent;
  3. the agent's highest `GLIBC_` symbol version (`objdump -T`) is not above the glibc of `$BASE_IMAGE` (`ldd --version` in the image);
  4. after the image build (:241), `determined-agent version` runs in the image, and `determined-agent gpu-topology` exits 0 with `nvml_init == "ERROR_LIBRARY_NOT_FOUND"` and `nvml_init_code == 12`. This runs the cgo wrapper and its dlopen-failure path in the real image on the GPU-less runner. The stub prints `NOT_BUILT` and fails the check.
- Comments at :29 (`runs-on: ubuntu-22.04`) and :36 (`BASE_IMAGE`) explain the coupling (D14).
- **agent/Makefile `build` (59-64):** `CGO_ENABLED=$(AGENT_CGO_ENABLED)` with default 1 and the same tags. Dev builds now need gcc; 0 builds the stub.
- **tools/fork/check.sh:** a `topology` mode, also run by `quick`: a CGO_ENABLED=0 agent build, a darwin/arm64 CGO_ENABLED=0 build, the stub tests with CGO_ENABLED=0, and the cgo, mock and agentrm tests.
- agent/.goreleaser.yml and tools/fork/Dockerfile.agent are unchanged. tools/fork/smoke.sh's CPU agent exercises the nil-topology path.

---

## 9. Later: recent critical XIDs (separate PR)
- **Meaning.** The data is "recently reported critical XIDs, and whether the Prometheus query succeeded". It does not mean "this GPU is monitored" or "healthy": an empty successful result can also mean the exporter is down, the GPU is missing from it, or the labels do not match.
- **Source.** The cluster's DCGM-Exporter in Prometheus (cluster-setup services/prometheus/prometheus.yml:72-106: job `dcgm`, label `gpu_uuid`), with the class of the cluster's `gpu-xid-critical` alert (services/grafana/provisioning/alerting/gpu-health.yaml): every code except the application class 13, 31, 43, 45 (D10). The master already queries that Prometheus for task charts (`integrations.task_resources`; `queryTaskPrometheus`, master/internal/core_task_resources.go:293, with the series labels at :265-267), so no new config key is needed.
- **Query.** `max by (gpu_uuid, xid) (max_over_time(DCGM_EXP_XID_ERRORS_COUNT{job="dcgm", det_cluster="<c>", gpu_uuid!="", xid!="", xid!="0", xid!~"13|31|43|45"}[5m])) > 0` over 24 h at a 300 s step. The 5-minute `max_over_time` makes consecutive steps cover every sample. `DCGM_FI_DEV_XID_ERRORS` is never used, because it holds the last code. First and last seen are step times, so the details say "around".
- **Fields** (new proto tags): `GpuTopology.xid_query_status` (NOT_CONFIGURED, OK, FAILED), `xid_query_error`, `xid_queried_at`; `GpuInfo.recent_xids` (xid, first_seen, last_seen).
- **Health.** ERROR also when a recent critical XID is reported. The details fill facts 3 and 4 of H1. A failed or unconfigured query leaves the dot to the NVML data and says so in fact 4 (D11).
- **Load.** Only `GetAgent`, and `GetAgents` with `exclude_slots=false`, run the query. The WebUI cluster store polls with `excludeSlots` defaulting to true (webui/react/src/services/apiConfig.ts:519). One shared result cache with a 30 s TTL (failures cached too) and a 5 s timeout. The pool page polls every 5 s (hooks/usePolling.ts:33), so a master sends at most one query per 30 s. The join is by UUID, so it also works for unknown topology and excluded GPUs. `ObfuscateAgent` runs first, so obfuscated agents trigger no query.
- **Limits.** A GPU fixed by a reboot stays red for the window. A driver bug that shows only as Xid 31 is not counted.

## 10. Later: width-aware ranking
- It comes only after Phase 0 run f shows how the x8 readings behave idle and under load, and after runs b and c.
- This cluster's evidence, not a general rule: an x8 GPU caps the NCCL rings through it at about 13 GB/s (docs/05:58, :240). On node06 the one-socket 4-GPU set that contains its x8 GPU gives 13.0, against 25.6 for the other socket's all-x16 set (docs/05:350-357). On node05 the cross-socket all-x16 pair 3,7 gives 19.2 against 12.8 for same-socket pairs with an x8 GPU (docs/05:337-338). Without P2P on Genoa, width mattered too: node07's pair 0,4 with GPU0 at x8 in that boot gave 8.7 against 16.4 (docs/05:371, :374).
- Runs b and c decide whether width ranks before or after the level for 4-GPU sets.

---

## 11. Implementation order
1. **P1 and P2** (section 6.1), each a small PR with its own tests (D17).
2. **PR A, collection and display:** aproto wire; agent collection (N1-N5); the exclude list (X1-X3); the subcommand; build and release; master state and reconnect; proto, bindings and assembly; health (H1); CLI; WebUI; agent docs and release note. Phase 1 runs on its candidate image before merge.
3. **PR B, in-agent set selection,** after P1 and P2: the config field and carrier; S1-S3; the task-log line; config docs and release note. Phase 0 runs a, d and e come before merge, and Phase 2 checks its real allocations.
4. **Later, each its own PR:** the XID lookup (section 9); width-aware ranking (section 10), gated by Phase 0 runs b, c and f; choosing the agent by topology, a scheduler change (section 6.6).

Files:
- **PR A:** agent/internal/agent.go; agent/internal/options/options.go and its test; agent/cmd/determined-agent/init.go, root.go, gpu_topology.go and its test; agent/internal/detect/detect.go, nvidia.go, topology.go, topology_nvml.go, topology_nvml_stub.go and their tests; agent/Makefile; go.mod, go.sum; .github/workflows/fork-release.yml; tools/fork/check.sh, smoke.sh; master/pkg/aproto/gpu_topology.go, master_message.go and tests; master/internal/rm/agentrm/gpu_topology.go, agent_state.go, agent.go and tests; master/internal/api_agents.go, authz/obfuscate.go and tests; master/pkg/model/agent.go and test; proto/src/determined/agent/v1/agent.proto, proto/pkg/agentv1/agent.pb.go; harness/determined/common/api/bindings.py, harness/determined/cli/agent.py, harness/tests/cli/test_agent.py; webui/react/src/services/api-ts-sdk/api.ts, types.ts, services/decoder.ts, utils/gpuTopology.ts, pages/ResourcePool/GpuTopology.tsx, GpuTopology.module.scss, ClusterTopology.tsx and tests; docs/reference/deploy/agent-config-reference.rst; a release note.
- **PR B:** schemas/expconf/v0/resources.json; schemas/test_cases/v0/experiment.yaml, merging.yaml; master/pkg/schemas/expconf/experiment_config.go and the generated files; master/pkg/model/experiment_config.go, compat.go; master/internal/sproto/scheduler.go; master/internal/trial.go, command/command.go, api_generic_tasks.go, generic_task_resume.go, core.go; master/internal/rm/agentrm/gpu_topology.go, agent.go, agent_state.go, resource_pool.go and tests; docs/reference/experiment-config-reference.rst, job-config-reference.rst; a release note.

---

## 12. Validation plan

**Phase 0: benchmarks, needing no code.** Per node in a maintenance window: drain with `det agent disable --drain` and re-enable afterwards. Run cluster-setup scripts/gpu-p2p/tests/run_host.sh with `STAGES=nccl`, 1 GiB busbw, 3 runs, median, and `NCCL_VARIANTS="p2p-sys"` unless noted. Post the tables in the PR.
- Runs a, d and e gate PR B: they check the topology and P2P ranking it uses (S1).
- Runs b, c and f gate the width PR (section 10), not PR B.

| Run | Node | Sets or check | Purpose |
|---|---|---|---|
| a | node02, 03 or 04 (Rome, all x16) | "0,1 0,4 0,1,2,3 0,1,4,5 0,2,4,6"; also `NCCL_VARIANTS=no-p2p` | same-socket against 2+2 cross-socket 4-GPU sets; the tier 2 rule |
| b | node01, with the agent's 7-GPU numbering | "0,1,2,3 0,1,2,5 0,1,2,6" | later width-aware ranking (section 10) |
| c | node05 (Milan) | "0,1,2,3 0,2,3,5 0,2,3,6 2,3 3,7" | later width-aware ranking (section 10) |
| d | g292, default variant | "0,1 0,2 0,1,2,3 0,2,4,6"; again with p2p-sys after the patched driver | PIX against NODE without P2P (D3) |
| e | node02 and node01 | one DDP training, fixed 500 steps, samples/s, 3 runs: a same-socket set against a cross-socket set | busbw overstates gains for compute-bound jobs |
| f | nodes 01, 05, 06, 07 | `nvidia-smi -q -d PCIE` on each x8 GPU and one x16 GPU, once idle and once under a running NCCL test | whether the width read at agent start is stable (section 3.2) |

**Phase 1: PR A's candidate image, read only.** On each node run `docker run --rm --gpus all --entrypoint /usr/bin/determined-agent <candidate> gpu-topology`; on node01 use the agent's 7-UUID device list. Compare with the host's `nvidia-smi topo -m`, `nvidia-smi topo -p2p r`, `nvidia-smi topo -p2p w`, the `--query-gpu` fields above and sysfs NUMA. Check that:
- the READ and WRITE statuses match the host for every ordered pair, and note any pair where WRITE is not OK;
- node01's slots 0-6 are host GPUs 0,1,2,3,5,6,7;
- on node01 with `--gpus all` and `--exclude-gpus <UUID of 81:00.0>`, the command finishes, prints no `exclude_error`, reports 81:00.0 as excluded with its link data, reports devices for host GPUs 0-3 and 5-7, and causes no fault on the host (X2);
- the x8 links show where the host shows them, each with max 16;
- sysfs NUMA is readable inside the container;
- on node03 or node04 (RTX 3090), every pair has NVLinks 0 and no GPU has an NVML error (N2), while host `nvidia-smi nvlink -s` shows no active link;
- `nvml_init` reads `SUCCESS`, and every NVML code is a symbolic name with its number, never prose (N4);
- once: the current agent image refuses to start with `exclude_gpus` in its YAML, and with `--exclude-gpus` (X3).

**Phase 2: after deploy.**
- PR A: amber dots exactly on the x8 GPUs of nodes 01, 05, 06 and 07, with details that say "at agent start", and green elsewhere. A node disabled with `det agent disable` shows striped "disabled" tiles. `det agent list` and `det agent describe` match the WebUI. After node01 is drained and restarted with all GPUs and the exclude list, the pool page shows an "excluded" tile without a slot id, and the container of a 7-GPU task on node01 has no device request with the excluded UUID (`docker inspect`).
- PR B: on node02, disable slots to get a fixed free set (for example `det slot disable cvgl-node02 0`, then 1 and 4). Run `det cmd run --config resources.resource_pool=48c96t_512_4090 --config resources.slots=2 --config resources.prefer_gpu_topology=true 'nvidia-smi topo -m; echo $DET_SLOT_IDS'`, and the same with slots=4. Check the set and the task-log line against a run without the flag. Re-enable the slots afterwards.

---

## 13. Tests
Each test names the rule it covers.

**PR A, agent** (go-nvml `pkg/nvml/mock` unless noted)
- TestCollectReturnCodeRule (N1): pairwise failures leave values unknown; NOT_SUPPORTED gives unknown without an error; GPU_IS_LOST on GPU 3's current width gives `NVMLError` "GetCurrPcieLinkWidth: ERROR_GPU_IS_LOST (15)" on GPU 3 only. Catches the zero-value trap.
- TestCollectP2PReadWrite (N3): the mock records four calls per pair (READ and WRITE, both directions). All OK gives USABLE. READ OK with WRITE NOT_SUPPORTED in one direction gives NOT_USABLE and keeps all four raw statuses. A failed WRITE query with the rest OK gives unknown.
- TestP2PUsability (N3, table) and TestMapTopologyLevelAndP2PStatus (table, including both spellings of value 1, P2P_STATUS_UNKNOWN and out-of-range values).
- TestCollectNode07Like: 8 GPUs with NODE/SYS levels, x8 of x16 on GPU 1, gen 1 of 4, NUMA from an injected reader. Expect 28 links with UUIDA < UUIDB, normalised bus ids, and CollectedAt and DriverVersion set.
- TestCollectOnlyDetectedUUIDs and TestCollectExcludedGPUs (X2): handles are requested only for detected and excluded UUIDs; only excluded GPUs carry `Excluded`; an NVML error on an excluded GPU sets only its own.
- TestCollectLibraryNotFound, TestCollectHandleLookupFails, TestCollectTimeout (N5): each keeps the inventory (N6).
- TestCollectNVLinkCount and TestCollectNVLinkProbeNoError (N2): RTX 3090-like answers give no error, NVLinks 0, and no call above link 4.
- TestNVMLReturnFormatting and TestCollectErrorsIgnoreNVMLProse (N4). `Return.String()` cannot be switched to prose in a unit test, so Phase 1 covers it.
- TestDetectGPUTopologyNoCUDA (nil only without CUDA devices and excluded GPUs), TestDetectGPUTopologyMIG; the stub test, run with CGO_ENABLED=0.
- TestDetectExcludeGPUs (X2): 8 GPUs and one excluded UUID give IDs 0-3 and 5-7 plus one excluded device; an unknown entry gives an error that names it; an empty list gives today's result; all GPUs excluded with slot type `auto` gives no slots, not CPU slots.
- options_test.go (X1): the config key and the flag set the option; `DET_EXCLUDE_GPUS` does not.
- TestGPUTopologySubcommandInitsFirst and TestGPUTopologySubcommandExcludeGPUs (section 2.5).

**PR A, master and clients**
- TestAgentStartedWireCompat: a nil GPUTopology marshals byte-identical to a golden copy of today's message, and older-agent JSON gives nil.
- TestNewGPUTopologyMapsUUIDsToDeviceIDs (reversed A/B swaps the directions), TestGPUTopologyKeepsExcludedForDisplay (also when unknown), TestAgentStartedRefreshesGPUTopology (section 4.2).
- TestClassifyGPUHealth (H1, table): the generation never changes the state; an excluded GPU never changes a slot's state. TestGetAgentExcludedGPUs. The model and obfuscate tests.
- harness/tests/cli/test_agent.py: the rows of the table in section 5.3; a mixed agent (one pair with READ OK and WRITE NOT_SUPPORTED in one direction, one pair unknown, the rest USABLE) gives `p2p 26/28 (1 unknown)`; and `describe`.
- WebUI vitest: utils/gpuTopology.test.ts (summaries, grouping, the state-to-palette map) and GpuTopology.test.tsx (dots and aria labels, stripes, the excluded tile, the popover with the four facts and the text of section 3.2, unknown topology, the legend).
- The release assertions (section 8), and the smoke run on the CPU agent.

**PR A, inventory without telemetry (N6).** The agent (`DetectGPUTopology`), the master (`gpuTopologyProto`), the CLI and the WebUI each run these cases:

| Case | Agent sends | Shown |
|---|---|---|
| 8 GPUs detected, 1 excluded; NVML init fails, collection times out, or stub build | `UnknownReason` and 8 entries: 7 slot UUIDs and 1 `Excluded` UUID, no telemetry | 7 slot tiles and 1 excluded tile, all UNKNOWN; CLI `unknown: <reason>` and `unknown; excluded: <UUID> (unknown)` |
| 8 GPUs detected, all excluded, slot type `auto` | no slots; 8 `Excluded` entries, with telemetry where NVML works | `gpu_topology` set; 8 excluded tiles; CLI `no slots` and `excluded: <8 bus ids>` |

**PR B**
- TestPairKeyOrder (S1): NVLink(4) < NVLink(2) < usable PIX < ... < usable SYS < not usable in one NUMA node (PIX equals NODE) < not usable SYS < unknown < NVML error. A pair with NVLinks that is not USABLE is not tier 0. Width never changes a key.
- TestSelectDevicesClusterCases: fixtures transcribed from each node's host output reproduce section 6.7.
- TestSelectDevicesLeximax, TestSelectDevicesGates (I4: every free pair unknown returns nil; one known pair selects), TestSelectDevicesCap, BenchmarkSelectDevices8Choose4 (S2).
- TestAllocateSelectedDevices: reserves exactly the given set in ID order; a busy, absent or duplicated device gives an error and changes nothing.
- TestAllocateFreeDevicesPreferTopology (S3, I3, I4): over random agent states, including drained slots after P1, the opted-in and the default reservation succeed or fail together; unknown topology runs today's path; a multi-agent fit passes `preferTopology=false`.
- TestFindFitsIgnoresFlag (I1, I2): `findFits` returns the same fits with the flag on and off.
- Config: schema test cases (valid true, invalid string, template true plus user false gives false); a defaulted config without the key marshals without it; strict NTSC decode and `ToExpconf`; the `GPUTopologyPreferred` table; the FittingRequirements capture tests.
- TestAllocateResourcesOptedInPublishesTaskLog (section 6.8).

---

## 14. Risks
Only risks not already stated with their rule.
- **cgo crash.** A fault inside libnvidia-ml at agent start takes the agent down in a restart loop; N5 covers hangs only. Recovery is the previous agent image, after X3's steps if an exclude list is set.
- **"P2P usable" is not proof.** NVML can report OK while BAR1 P2P is broken (docs/05 caveat). A node with a broken setup gets sets that rank as P2P but run host-staged.
- **Few red dots in v1.** Red comes only from NVML at agent start, which rarely fires (section 3.3). Runtime faults show only after the XID PR.
- **Benefit varies.** Socket locality with P2P is worth about 27% on Rome and 7% on Genoa in busbw (section 6.3), and compute-bound jobs gain less (run e). v1 does not avoid x8 GPUs (section 6.7).
- **Fragmentation.** Tasks without the flag still take free GPUs in map order, so on busy nodes opted-in tasks often find only cross-socket sets (D20).
- **Restart window.** Right after a master restart, agents are unknown for a few seconds (section 4.2), and opted-in tasks then get today's choice.
- **glibc and runner.** The agent is dynamically linked. Builds depend on the ubuntu-22.04 runner until D14's container build.
- **Agent start time.** NVML init on GPUs without persistence mode adds seconds to agent start.
- **Proto toolchain.** Regeneration needs the pinned toolchain and produces large generated diffs.
- **No effect** on Kubernetes and dispatcher RMs or for multi-agent fits. Under RBAC, users without the sensitive-agent permission see no topology.

---

## 15. Owner decisions
Decided by the owner: the flag is off by default; P2P comes only from the agent's measurement (R1); the WebUI encoding (R4); and the rows marked "decided".

| ID | Topic | Status and choice | Section |
|---|---|---|---|
| D1 | Field name and type | open; recommended plain bool `resources.prefer_gpu_topology` (alternative: an enum `gpu_placement`) | 6.2 |
| D2 | Link width in the ranking | decided: not in v1; a later option | 6.3, 10 |
| D3 | Pairs without P2P | open; recommended: below usable pairs, by NUMA class only, switch locality neutral; run d | 6.3 |
| D4 | Choosing the agent | decided: not in v1; the agent is today's choice | 6.6 |
| D5 | Multi-agent tasks | open; recommended: unchanged | 6.5 |
| D6 | GPUs with an NVML error in opted-in sets | open; recommended: rank last, never exclude. The third review supports it | 6.3 |
| D7 | Unknown topology | open; recommended: today's device choice, also when every pair is unknown | I4 |
| D8 | Definition of "error" | decided for v1: an NVML health call failed at agent start; XIDs later | 3.1, 9 |
| D9 | XID lookup | decided: a later separate PR | 9 |
| D10 | XID window and codes | open; recommended: 24 h, excluding 13, 31, 43, 45 | 9 |
| D11 | Dot when XID data is missing | decided: the dot follows the reported data; the query status is shown separately | 3.1, 9 |
| D12 | NVLink | open; recommended: in the wire and the order, mock-tested, P2P USABLE required | 2.2, 6.3 |
| D13 | Visibility scope | decided: API, `det agent list`, `det agent describe` and WebUI in PR A | 5 |
| D14 | Agent build | open; recommended: ubuntu-22.04 runner with the GLIBC check (alternative: build inside `$BASE_IMAGE`) | 8 |
| D15 | go-nvml version | open; recommended: v0.12.9-0 | 8 |
| D16 | Defaults and switches | open; recommended: no pool default, no kill switch, no master config key | 1.4 |
| D17 | Pre-existing quirks | decided: P1 and P2 are fixed first. Open: the others, each its own small PR | 6.1 |
| D18 | Task-log line | open; recommended: yes | 6.8 |
| D19 | Diagnostic subcommand | open; recommended: yes | 2.5 |
| D20 | Device choice without the flag | open; recommended: unchanged | 1.4 |
| D21 | Agents built without `linux && cgo` | open; recommended: the stub (CGO_ENABLED=0, non-Linux, a cross build, or a native build with no C compiler found, where Go disables cgo by default). A linux/arm64 build with cgo uses NVML. The fork releases only linux/amd64, with cgo | 2.2, 8 |
| D22 | PR shape | decided: P1 and P2, then PR A, then PR B, then the later PRs | 11 |
| D23 | Follow-ups in cluster-setup | open; recommended: document the flag and the exclude list (with X3's rollback step), and check that the launch tooling passes the flag through | |
| D24 | Excluded GPUs | decided; option, safety and compatibility in X1-X3 | 2.6 |

The other quirks of D17: `allocateFreeDevices` leaves `containerState[cid]` behind on "not enough devices" (agent_state.go:160, :175); agent/internal/agent.go:119 formats `devices` instead of `err`; `refreshAgentStateCacheFor` dereferences a nil state when `a.State()` fails (resource_pool.go:509-513).

---

## History
Revisions 1-4 and their reviews are in the "Revisions" section of this file at commit 55ddc4619: review findings C1-C8 shaped revision 2, verification findings V1-V5 revision 3, and revision 4 added D24. After a review of revision 4, v1 dropped choosing the agent by topology and width-aware ranking, moved the XID lookup to a later PR, requires P2P READ and WRITE, treats link width as an observation, and corrected the exclude list's compatibility (X3).
After the third review, excluded GPUs stay visible without telemetry (N6), the P2P summary counts usable pairs, Phase 0's width runs gate only the width PR, and P1's contract and I4 are stated exactly.
