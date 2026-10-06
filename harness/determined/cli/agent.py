import argparse
import collections
import itertools
import operator
import os
import sys
import typing
from typing import Any, Callable, Dict, List, NamedTuple, Optional, Tuple

import determined.cli.render
from determined import cli
from determined.cli import errors, render
from determined.cli import task as cli_task
from determined.common import check
from determined.common.api import bindings

NO_PERMISSIONS = "NO PERMISSIONS"

# Link levels from best to worst, as NVML's GetTopologyCommonAncestor reports them.
GPU_LINK_LEVELS = ["INTERNAL", "PIX", "PXB", "PHB", "NODE", "SYS"]

# Short P2P status codes for the P2P matrix, as nvidia-smi topo -p2p prints them where it can.
GPU_P2P_STATUS_CODES = {
    bindings.v1GpuP2pStatus.OK: "OK",
    bindings.v1GpuP2pStatus.CHIPSET_NOT_SUPPORTED: "CNS",
    bindings.v1GpuP2pStatus.GPU_NOT_SUPPORTED: "GNS",
    bindings.v1GpuP2pStatus.TOPOLOGY_NOT_SUPPORTED: "TNS",
    bindings.v1GpuP2pStatus.DISABLED_BY_REGKEY: "DIS",
    bindings.v1GpuP2pStatus.NOT_SUPPORTED: "NS",
}
GPU_P2P_STATUS_LEGEND = (
    "CNS chipset not supported, GNS GPU not supported, TNS topology not supported, "
    "DIS disabled by registry key, NS not supported, ? unknown"
)

GPU_HEALTH_WORDS = {
    bindings.v1GpuHealth.OK: "ok",
    bindings.v1GpuHealth.LINK_BELOW_MAX: "narrow",
    bindings.v1GpuHealth.ERROR: "error",
}

# Shown with a narrow link. The width is the one measured at agent start; the agent docs explain
# what it does and does not mean.
GPU_NARROW_LINK_TEXT = "A lower link width lowers this link's bandwidth cap."
GPU_EXCLUDED_TEXT = "Left out by the agent's exclude list. No task runs on this GPU."
# An agent without NVIDIA GPUs, a master without GPU topology, or a user who may not view it.
GPU_TOPOLOGY_NOT_REPORTED = (
    "not reported (no NVIDIA GPUs, an older master, or no permission to view agent details)"
)


def local_id(address: str) -> str:
    return os.path.basename(address)


def gpu_link_level_name(level: bindings.v1GpuLinkLevel) -> str:
    """The level without its enum prefix, for example "NODE", or "" when unknown."""
    if level == bindings.v1GpuLinkLevel.UNSPECIFIED:
        return ""
    return str(level.value)[len("GPU_LINK_LEVEL_") :]


def gpu_p2p_status_name(status: bindings.v1GpuP2pStatus) -> str:
    """The status without its enum prefix, for example "GPU_NOT_SUPPORTED", or "" when unknown."""
    if status == bindings.v1GpuP2pStatus.UNSPECIFIED:
        return ""
    return str(status.value)[len("GPU_P2P_STATUS_") :]


def gpu_health_word(health: bindings.v1GpuHealth) -> str:
    """The CLI word of a GPU's health as the master classified it: ok, narrow, error or unknown."""
    return GPU_HEALTH_WORDS.get(health, "unknown")


def short_pci_bus_id(bus_id: str) -> str:
    """A bus id without the PCI domain 0000, for example "81:00.0" for "0000:81:00.0"."""
    return bus_id[len("0000:") :] if bus_id.startswith("0000:") else bus_id


def gpu_label(gpu: bindings.v1GpuInfo) -> str:
    """How an excluded GPU is named: its short bus id, or its UUID when the bus id is unknown."""
    return short_pci_bus_id(gpu.pciBusId) if gpu.pciBusId else gpu.uuid


def _gpu_slots(topo: bindings.v1GpuTopology) -> List[bindings.v1GpuInfo]:
    return sorted((g for g in topo.gpus if not g.excluded), key=lambda g: g.deviceId)


class GpuPairLink(NamedTuple):
    """The link of two GPUs, and whether the first GPU of the lookup is end A of the link."""

    link: bindings.v1GpuLink
    first_is_a: bool


def gpu_link_lookup(
    topo: bindings.v1GpuTopology,
) -> Callable[[bindings.v1GpuInfo, bindings.v1GpuInfo], Optional[GpuPairLink]]:
    """Looks up the link of two GPUs by UUID, in either order.

    The one lookup of the summary and the matrices, slots and excluded GPUs alike; slot IDs are
    only labels. The WebUI has the same lookup (utils/gpuTopology.ts).
    """
    links = {(link.uuidA, link.uuidB): link for link in topo.links}

    def lookup(a: bindings.v1GpuInfo, b: bindings.v1GpuInfo) -> Optional[GpuPairLink]:
        forward = links.get((a.uuid, b.uuid))
        if forward is not None:
            return GpuPairLink(forward, True)
        backward = links.get((b.uuid, a.uuid))
        if backward is not None:
            return GpuPairLink(backward, False)
        return None

    return lookup


def gpu_p2p_directions(
    pair: GpuPairLink,
) -> Tuple[bindings.v1GpuP2pCaps, bindings.v1GpuP2pCaps]:
    """The P2P statuses from the first GPU of the lookup to the second, and back."""
    link = pair.link
    if pair.first_is_a:
        return link.p2pAToB, link.p2pBToA
    return link.p2pBToA, link.p2pAToB


def gpu_first_not_ok_status(pair: GpuPairLink) -> Optional[bindings.v1GpuP2pStatus]:
    """The first known status other than OK.

    The order is A->B READ, A->B WRITE, B->A READ, B->A WRITE, where A is the GPU the lookup
    started from.
    """
    forward, backward = gpu_p2p_directions(pair)
    for status in (forward.read, forward.write, backward.read, backward.write):
        if status not in (bindings.v1GpuP2pStatus.OK, bindings.v1GpuP2pStatus.UNSPECIFIED):
            return status
    return None


def gpu_topology_summary(topo: Optional[bindings.v1GpuTopology]) -> str:
    """The GPU Topology column of `det agent list`.

    The NUMA group sizes of the slots, the distinct levels between slots from best to worst, and
    the P2P state of the pairs of slots, for example "4+4 NODE/SYS p2p 12/28 (3 unknown)".
    Excluded GPUs do not count. The WebUI shows the same string (utils/gpuTopology.ts).
    """
    if topo is None:
        return ""
    if topo.unknownReason:
        return f"unknown: {topo.unknownReason}"
    slots = _gpu_slots(topo)
    if not slots:
        return "no slots"

    numa_sizes = collections.Counter(g.numaNode for g in slots)
    # Known NUMA nodes in order, then the slots whose NUMA node is unknown (-1).
    numa_order = sorted(numa_sizes, key=lambda n: (n < 0, n))
    parts = ["+".join(str(numa_sizes[n]) for n in numa_order)]

    lookup = gpu_link_lookup(topo)
    # Each pair from its lower slot id, so the first status of a pair reads from that slot.
    pairs = [lookup(a, b) for a, b in itertools.combinations(slots, 2)]
    levels = {gpu_link_level_name(p.link.level) for p in pairs if p is not None}
    known_levels = [lv for lv in GPU_LINK_LEVELS if lv in levels]
    if known_levels:
        parts.append("/".join(known_levels))
    if not pairs:
        return " ".join(parts)

    usable = unknown = not_usable = 0
    first_status = ""
    for pair in pairs:
        if pair is not None and pair.link.p2p == bindings.v1GpuP2p.USABLE:
            usable += 1
        elif pair is not None and pair.link.p2p == bindings.v1GpuP2p.NOT_USABLE:
            not_usable += 1
            if not first_status:
                status = gpu_first_not_ok_status(pair)
                first_status = gpu_p2p_status_name(status) if status is not None else "unknown"
        else:
            unknown += 1

    if usable == len(pairs):
        p2p_part = "p2p"
    elif unknown == len(pairs):
        p2p_part = "p2p?"
    elif usable == 0 and not_usable > 0:
        p2p_part = f"no-p2p({first_status})"
    else:
        p2p_part = f"p2p {usable}/{len(pairs)}"
    if 0 < unknown < len(pairs):
        p2p_part += f" ({unknown} unknown)"
    parts.append(p2p_part)
    return " ".join(parts)


def gpu_health_summary(topo: Optional[bindings.v1GpuTopology]) -> str:
    """The GPU Health column of `det agent list`.

    "ok" when every GPU is ok and none is excluded. Otherwise the slots that are not ok, grouped
    as error, narrow and unknown, then the excluded GPUs with their state when it is not ok, for
    example "narrow: 3,5 (x8 of x16); excluded: 81:00.0". The widths are the ones measured at agent
    start. The WebUI shows the same string (utils/gpuTopology.ts).
    """
    if topo is None:
        return ""
    slots = _gpu_slots(topo)
    excluded = [g for g in topo.gpus if g.excluded]
    words = {g.deviceId: gpu_health_word(g.health) for g in slots}
    if not excluded and all(w == "ok" for w in words.values()):
        return "ok"

    parts = []
    with_error = [g for g in slots if words[g.deviceId] == "error"]
    if with_error:
        parts.append("error: " + ",".join(str(g.deviceId) for g in with_error))
    narrow: Dict[str, List[int]] = {}
    for g in slots:
        if words[g.deviceId] == "narrow":
            width = f"x{g.pcieLinkWidth} of x{g.pcieLinkWidthMax}"
            narrow.setdefault(width, []).append(g.deviceId)
    if narrow:
        parts.append(
            "narrow: "
            + ", ".join(f"{','.join(map(str, ids))} ({width})" for width, ids in narrow.items())
        )
    unknown = [g for g in slots if words[g.deviceId] == "unknown"]
    if unknown and len(unknown) == len(slots):
        parts.append("unknown")
    elif unknown:
        parts.append("unknown: " + ",".join(str(g.deviceId) for g in unknown))
    if excluded:
        labels = []
        for g in excluded:
            word = gpu_health_word(g.health)
            labels.append(gpu_label(g) + ("" if word == "ok" else f" ({word})"))
        parts.append("excluded: " + ", ".join(labels))
    return "; ".join(parts)


def list_agents(args: argparse.Namespace) -> None:
    sess = cli.setup_session(args)
    resp = bindings.get_GetAgents(sess)

    agents = [
        collections.OrderedDict(
            [
                ("id", local_id(a.id)),
                ("version", a.version),
                ("registered_time", render.format_time(a.registeredTime)),
                ("num_slots", len(a.slots) if a.slots is not None else ""),
                ("gpu_topology", gpu_topology_summary(a.gpuTopology)),
                ("gpu_health", gpu_health_summary(a.gpuTopology)),
                ("num_containers", len(a.containers) if a.containers is not None else ""),
                (
                    "resource_pools",
                    ", ".join(a.resourcePools) if a.resourcePools is not None else "",
                ),
                ("enabled", a.enabled),
                ("draining", a.draining),
                ("addresses", ", ".join(a.addresses) if a.addresses is not None else ""),
            ]
        )
        for a in sorted(resp.agents or [], key=operator.attrgetter("id"))
    ]

    if args.json:
        determined.cli.render.print_json(agents)
        return

    headers = [
        "Agent ID",
        "Version",
        "Registered Time",
        "Slots",
        "GPU Topology",
        "GPU Health",
        "Containers",
        "Resource Pool",
        "Enabled",
        "Draining",
        "Addresses",
    ]
    values = [a.values() for a in agents]
    render.tabulate_or_csv(headers, values, args.csv)


def list_slots(args: argparse.Namespace) -> None:
    sess = cli.setup_session(args)
    task_res = bindings.get_GetTasks(sess)
    resp = bindings.get_GetAgents(sess)

    allocations = task_res.allocationIdToSummary

    c_names = (
        {
            r.containerId: {"name": a.name, "allocation_id": a.allocationId}
            for a in allocations.values()
            for r in (a.resources or {})
            if r.containerId
        }
        if allocations
        else {}
    )

    def device_type_string(deviceType: typing.Optional[bindings.devicev1Type]) -> str:
        if deviceType == bindings.devicev1Type.CUDA:
            return "cuda"
        if deviceType == bindings.devicev1Type.ROCM:
            return "rocm"
        if deviceType == bindings.devicev1Type.CPU:
            return "cpu"
        return "unknown"

    def get_task_name(containers: Dict[str, Any], slot: bindings.v1Slot) -> str:
        if not slot.container:
            return "FREE"

        if slot.container.permissionDenied:
            return NO_PERMISSIONS

        container_id = slot.container.id

        if slot.container and container_id in containers:
            return str(containers[container_id]["name"])

        if slot.container and (
            "determined-master-deployment" in container_id
            or "determined-db-deployment" in container_id
        ):
            return f"Determined System Task: {container_id}"

        if slot.container and ("dispatcherrm-inuse-slot-placeholder" in container_id):
            return ""  # slot:task relationship not tracked on HPC clusters, so just show ""

        return f"Non-Determined Task: {container_id}"

    slots = [
        collections.OrderedDict(
            [
                ("agent_id", local_id(agent.id)),
                (
                    "resource_pools",
                    ", ".join(agent.resourcePools) if agent.resourcePools is not None else "",
                ),
                ("slot_id", local_id(slot.id or "")),
                ("enabled", slot.enabled),
                ("draining", slot.draining),
                (
                    "allocation_id",
                    c_names[slot.container.id]["allocation_id"]
                    if slot.container and slot.container.id in c_names
                    else ("OCCUPIED" if slot.container else "FREE"),
                ),
                ("task_name", get_task_name(c_names, slot)),
                ("type", device_type_string((slot.device or bindings.v1Device()).type)),
                ("device", (slot.device or bindings.v1Device()).brand),
            ]
        )
        for agent in sorted(resp.agents or [], key=operator.attrgetter("id"))
        for _key, slot in (agent.slots or {}).items()
    ]

    headers = [
        "Agent ID",
        "Resource Pool",
        "Slot ID",
        "Enabled",
        "Draining",
        "Allocation ID",
        "Task Name",
        "Type",
        "Device",
    ]

    if args.json:
        determined.cli.render.print_json(slots)
        return

    values = [s.values() for s in slots]

    render.tabulate_or_csv(headers, values, args.csv)


def _link_text(cur: int, cur_max: int, prefix: str) -> str:
    """A link width as "<cur> of <max>", with ? for an unknown value (0)."""

    def value(v: int) -> str:
        return f"{prefix}{v}" if v > 0 else f"{prefix}?"

    return f"{value(cur)} of {value(cur_max)}"


def _link_gen_text(gen_max: int, unknown: str = "Gen?") -> str:
    """A GPU's PCIe link generation: the highest that the GPU and its slot support.

    `unknown` is shown when it is unknown (0). The current generation drops while a GPU is idle, so
    it is left out, as in the WebUI.
    """
    return f"Gen{gen_max}" if gen_max > 0 else unknown


def _cur_max(cur: int, cur_max: int, prefix: str) -> str:
    def value(v: int) -> str:
        return f"{prefix}{v}" if v > 0 else "?"

    return f"{value(cur)}/{value(cur_max)}"


def _gpu_details(
    gpu: bindings.v1GpuInfo, topo: bindings.v1GpuTopology, collected_at: str
) -> List[str]:
    """The facts of a GPU's health, each on its own line, as in the WebUI's details.

    The PCIe link and the NVML errors, both measured at agent start, and the collection time (the
    agent's clock).
    """
    known = not topo.unknownReason
    lines = []
    if gpu.excluded:
        lines.append(GPU_EXCLUDED_TEXT)
    if gpu.pcieLinkWidth or gpu.pcieLinkWidthMax or gpu.pcieLinkGenMax:
        link = (
            f"{_link_text(gpu.pcieLinkWidth, gpu.pcieLinkWidthMax, 'x')}, "
            f"{_link_gen_text(gpu.pcieLinkGenMax)}"
        )
    else:
        link = "unknown"
    lines.append(f"PCIe link: {link}")
    if gpu.health == bindings.v1GpuHealth.LINK_BELOW_MAX:
        lines.append(GPU_NARROW_LINK_TEXT)
    if not known:
        nvml_errors = "not collected"
    else:
        nvml_errors = gpu.nvmlError or "none"
    lines.append(f"NVML errors: {nvml_errors}")
    lines.append(f"Collected at: {collected_at}")
    return lines


def _gpu_matrix_labels(topo: bindings.v1GpuTopology) -> List[Tuple[str, bindings.v1GpuInfo]]:
    """The GPUs of the matrices: slots by id, then excluded GPUs by bus id (or UUID)."""
    return [(str(g.deviceId), g) for g in _gpu_slots(topo)] + [
        (gpu_label(g), g) for g in topo.gpus if g.excluded
    ]


def _print_gpu_matrices(topo: bindings.v1GpuTopology) -> None:
    labels = _gpu_matrix_labels(topo)
    if len(labels) < 2:
        return
    lookup = gpu_link_lookup(topo)

    level_rows = []
    all_usable = True
    for label_a, a in labels:
        row = [label_a]
        for _, b in labels:
            if a.uuid == b.uuid:
                row.append("X")
                continue
            pair = lookup(a, b)
            level = gpu_link_level_name(pair.link.level) if pair is not None else ""
            cell = level or "?"
            if pair is not None and pair.link.nvlinks > 0:
                cell += f"+NV{pair.link.nvlinks}"
            row.append(cell)
            if pair is None or pair.link.p2p != bindings.v1GpuP2p.USABLE:
                all_usable = False
        level_rows.append(row)
    headers = [""] + [label for label, _ in labels]
    print("\nLink levels (slot ids; excluded GPUs by bus id):")
    render.tabulate_or_csv(headers, level_rows, False)

    if all_usable:
        print("\nP2P: usable for every pair (READ and WRITE are OK in both directions).")
        return
    p2p_rows = []
    for label_a, a in labels:
        row = [label_a]
        for _, b in labels:
            if a.uuid == b.uuid:
                row.append("X")
                continue
            pair = lookup(a, b)
            if pair is None:
                row.append("?/?")
                continue
            caps, _ = gpu_p2p_directions(pair)
            read = GPU_P2P_STATUS_CODES.get(caps.read, "?")
            write = GPU_P2P_STATUS_CODES.get(caps.write, "?")
            row.append(f"{read}/{write}")
        p2p_rows.append(row)
    print("\nP2P READ/WRITE from the row's GPU to the column's GPU:")
    render.tabulate_or_csv(headers, p2p_rows, False)
    print(GPU_P2P_STATUS_LEGEND)


def describe_agent(args: argparse.Namespace) -> None:
    sess = cli.setup_session(args)
    agent = bindings.get_GetAgent(sess, agentId=args.agent_id).agent
    topo = agent.gpuTopology
    if args.json:
        determined.cli.render.print_json(topo.to_json() if topo is not None else None)
        return

    header = [
        ("Agent ID", local_id(agent.id)),
        ("Resource Pools", ", ".join(agent.resourcePools or [])),
        ("Version", agent.version or ""),
        ("Enabled", agent.enabled),
        ("Draining", agent.draining),
    ]
    if topo is None:
        header.append(("GPU Topology", GPU_TOPOLOGY_NOT_REPORTED))
        for key, value in header:
            print(f"{key + ':':<17}{value}")
        return

    collected_at = "unknown"
    if topo.collectedAt:
        collected_at = render.format_time(topo.collectedAt) or collected_at
    header += [
        ("Driver Version", topo.driverVersion or "unknown"),
        ("Collected At", collected_at),
        ("GPU Topology", gpu_topology_summary(topo)),
        ("GPU Health", gpu_health_summary(topo)),
    ]
    for key, value in header:
        print(f"{key + ':':<17}{value}")

    slots = {}
    for slot in (agent.slots or {}).values():
        try:
            slots[int(local_id(slot.id or ""))] = slot
        except ValueError:
            continue

    def state(gpu: bindings.v1GpuInfo) -> str:
        if gpu.excluded:
            return "EXCLUDED"
        # As in the WebUI: the scheduler gives a disabled or draining agent no new work, also when
        # a slot of it is enabled on its own or has no slot record. An agent without the fields
        # counts as enabled.
        agent_off = "DRAINING" if agent.draining else "DISABLED" if agent.enabled is False else ""
        slot = slots.get(gpu.deviceId)
        if slot is None:
            return f"? ({agent_off})" if agent_off else "?"
        off = ""
        if slot.draining or agent_off == "DRAINING":
            off = "DRAINING"
        elif not slot.enabled or agent_off == "DISABLED":
            off = "DISABLED"
        if slot.container:
            # A disabled or draining slot can still run a task: show both.
            if slot.container.id and not slot.container.permissionDenied:
                occupant = slot.container.id
            else:
                occupant = "OCCUPIED"
            return f"{occupant} ({off})" if off else occupant
        return off or "FREE"

    gpus = _gpu_slots(topo) + [g for g in topo.gpus if g.excluded]
    rows = [
        [
            "-" if g.excluded else g.deviceId,
            state(g),
            gpu_health_word(g.health),
            g.uuid,
            g.pciBusId or "?",
            g.numaNode if g.numaNode >= 0 else "?",
            _cur_max(g.pcieLinkWidth, g.pcieLinkWidthMax, "x"),
            _link_gen_text(g.pcieLinkGenMax, "?"),
        ]
        for g in gpus
    ]
    print()
    render.tabulate_or_csv(
        ["Slot", "State", "Health", "UUID", "PCI Bus ID", "NUMA", "Width cur/max", "Gen max"],
        rows,
        False,
    )

    print("\nDetails:")
    for g in gpus:
        name = f"Excluded GPU {g.pciBusId or g.uuid}" if g.excluded else f"Slot {g.deviceId}"
        print(f"  {name} ({gpu_health_word(g.health)}):")
        for line in _gpu_details(g, topo, collected_at):
            print(f"    {line}")

    if not topo.unknownReason:
        _print_gpu_matrices(topo)


def patch_agent(enabled: bool) -> Callable[[argparse.Namespace], None]:
    def patch(args: argparse.Namespace) -> None:
        sess = cli.setup_session(args)
        check.check_false(args.all and args.agent_id)
        action = "enable" if enabled else "disable"

        if not (args.all or args.agent_id):
            raise errors.CliError(
                "Please pass agent id or --all option. "
                f"See `det agent {action} --help` for details."
            )

        if args.agent_id:
            agent_ids = [args.agent_id]
        else:
            resp = bindings.get_GetAgents(sess)
            agent_ids = sorted(local_id(a.id) for a in resp.agents or [])

        drain_mode = None if enabled else args.drain

        for agent_id in agent_ids:
            path = f"api/v1/agents/{agent_id}/{action}"

            payload = None
            if not enabled and drain_mode:
                payload = {
                    "drain": drain_mode,
                }

            sess.post(path, json=payload)
            status = "Disabled" if not enabled else "Enabled"
            print(f"{status} agent {agent_id}.", file=sys.stderr)

        # When draining, check if there're any tasks currently running on
        # these slots, and list them.
        if drain_mode:
            rsp = bindings.get_GetTasks(sess)
            tasks_data = {
                k: t
                for (k, t) in (
                    rsp.allocationIdToSummary.items()
                    if rsp.allocationIdToSummary is not None
                    else {}
                )
                if any(a in agent_ids for r in (t.resources or []) for a in (r.agentDevices or {}))
            }

            if not (args.json or args.csv):
                if tasks_data:
                    print("Tasks still in progress on draining nodes.")
                else:
                    print("No tasks in progress on draining nodes.")

            cli_task.render_tasks(args, tasks_data)

    return patch


def patch_slot(enabled: bool) -> Callable[[argparse.Namespace], None]:
    def patch(args: argparse.Namespace) -> None:
        sess = cli.setup_session(args)
        if enabled:
            bindings.post_EnableSlot(sess, agentId=args.agent_id, slotId=args.slot_id)
        else:
            bindings.post_DisableSlot(
                sess,
                agentId=args.agent_id,
                slotId=args.slot_id,
                body=bindings.v1DisableSlotRequest(),
            )

        status = "Disabled" if not enabled else "Enabled"
        print("{} slot {} of agent {}".format(status, args.slot_id, args.agent_id))

    return patch


def agent_id_completer(_1: str, parsed_args: argparse.Namespace, _2: Any) -> List[str]:
    resp = bindings.get_GetAgents(cli.setup_session(parsed_args))
    return [a.id for a in resp.agents or []]


# fmt: off

args_description = [
    cli.Cmd("a|gent", None, "manage agents", [
        cli.Cmd("list ls", list_agents, "list agents", [
            cli.Group(
                cli.Arg("--csv", action="store_true", help="print as CSV"),
                cli.Arg("--json", action="store_true", help="print as JSON"),
            ),
        ], is_default=True),
        cli.Cmd("describe", describe_agent, "describe an agent's GPUs: topology, P2P and health", [
            cli.Arg("agent_id", help="agent ID", completer=agent_id_completer),
            cli.Arg("--json", action="store_true", help="print the agent's GPU topology as JSON"),
        ]),
        cli.Cmd("enable", patch_agent(True), "enable agent", [
            cli.Group(
                cli.Arg("agent_id", help="agent ID", nargs="?", completer=agent_id_completer),
                cli.Arg("--all", action="store_true", help="enable all agents"),
            )
        ]),
        cli.Cmd("disable", patch_agent(False), "disable agent", [
            cli.Group(
                cli.Arg("agent_id", help="agent ID", nargs="?", completer=agent_id_completer),
                cli.Arg("--all", action="store_true", help="disable all agents"),
            ),
            cli.Arg(
                "--drain", action="store_true",
                help="enter drain mode, allowing the tasks currently running on "
                "the disabled agents to finish. will also print these tasks, if any"
            ),
            cli.Group(
                cli.Arg("--csv", action="store_true", help="print as CSV"),
                cli.Arg("--json", action="store_true", help="print as JSON"),
            ),
        ]),
    ]),
    cli.Cmd("s|lot", None, "manage slots", [
        cli.Cmd("list ls", list_slots, "list slots in cluster", [
            cli.Group(
                cli.Arg("--csv", action="store_true", help="print as CSV"),
                cli.Arg("--json", action="store_true", help="print as JSON"),
            ),
        ], is_default=True),
        cli.Cmd("enable", patch_slot(True), "enable slot on agent", [
            cli.Arg("agent_id", help="agent ID", completer=agent_id_completer),
            cli.Arg("slot_id", type=int, help="slot ID"),
        ]),
        cli.Cmd("disable", patch_slot(False), "disable slot on agent", [
            cli.Arg("agent_id", help="agent ID", completer=agent_id_completer),
            cli.Arg("slot_id", type=int, help="slot ID"),
        ]),
    ]),
]  # type: List[Any]

# fmt: on
