import copy
import json
import pathlib
from typing import Any, Dict, List, Optional

import pytest

from determined.cli import agent, cli
from determined.common.api import bindings
from tests.cli import util

MASTER = "http://localhost:8080"

# Shared with webui/react/src/utils/gpuTopology.test.ts, so the CLI and the WebUI show the same
# summary strings.
FIXTURE = pathlib.Path(__file__).resolve().parents[1] / "fixtures" / "gpu_topology_cases.json"
CASES: List[Dict[str, Any]] = json.loads(FIXTURE.read_text(encoding="utf-8"))["cases"]


def topology_case(name_prefix: str) -> Optional[Dict[str, Any]]:
    matches = [c for c in CASES if c["name"].startswith(name_prefix)]
    assert len(matches) == 1, name_prefix
    topology: Optional[Dict[str, Any]] = copy.deepcopy(matches[0]["gpuTopology"])
    return topology


def slot_json(
    slot_id: int,
    enabled: bool = True,
    draining: bool = False,
    container_id: Optional[str] = None,
) -> Dict[str, Any]:
    slot: Dict[str, Any] = {
        "id": str(slot_id),
        "device": {"id": slot_id, "brand": "NVIDIA GeForce RTX 3090", "type": "TYPE_CUDA"},
        "enabled": enabled,
        "draining": draining,
    }
    if container_id is not None:
        slot["container"] = {
            "id": container_id,
            "parent": "",
            "state": "STATE_RUNNING",
            "devices": [],
            "permissionDenied": False,
        }
    return slot


def agent_json(
    agent_id: str,
    topology: Optional[Dict[str, Any]],
    slots: Optional[Dict[int, Dict[str, Any]]] = None,
    enabled: bool = True,
    draining: bool = False,
) -> Dict[str, Any]:
    if slots is None:
        slot_ids = [g["deviceId"] for g in (topology or {}).get("gpus", []) if not g["excluded"]]
        slots = {i: slot_json(i) for i in slot_ids}
    out: Dict[str, Any] = {
        "id": agent_id,
        "registeredTime": "2026-10-05T08:00:00Z",
        "slots": {f"/agents/{agent_id}/slots/{i}": s for i, s in slots.items()},
        "containers": {},
        "resourcePools": ["pool"],
        "addresses": ["10.0.0.1"],
        "enabled": enabled,
        "draining": draining,
        "version": "0.41.1",
        "slotStats": {"typeStats": {}, "brandStats": {}},
    }
    if topology is not None:
        out["gpuTopology"] = topology
    return out


@pytest.mark.parametrize("case", CASES, ids=[c["name"] for c in CASES])
def test_gpu_summaries_match_shared_fixture(case: Dict[str, Any]) -> None:
    raw = case["gpuTopology"]
    topo = bindings.v1GpuTopology.from_json(raw) if raw is not None else None
    assert agent.gpu_topology_summary(topo) == case["topology"]
    assert agent.gpu_health_summary(topo) == case["health"]


@pytest.mark.parametrize(
    "name_prefix,expected",
    [
        ("no-p2p:", "3 PHB no-p2p(TOPOLOGY_NOT_SUPPORTED) (1 unknown)"),
        # Each of the four positions of the lowest NOT_USABLE pair holds a different status, so
        # another status order, or a reversed pair read without swapping, names another status.
        ("no-p2p order:", "3 PHB no-p2p(TOPOLOGY_NOT_SUPPORTED)"),
    ],
)
def test_gpu_summaries_ignore_link_order_and_direction(name_prefix: str, expected: str) -> None:
    """A reversed pair keeps its P2P status order: A is always the lower slot id."""
    raw = topology_case(name_prefix)
    assert raw is not None
    assert agent.gpu_topology_summary(bindings.v1GpuTopology.from_json(raw)) == expected
    for link in raw["links"]:
        link["deviceA"], link["deviceB"] = link["deviceB"], link["deviceA"]
        link["uuidA"], link["uuidB"] = link["uuidB"], link["uuidA"]
        link["p2pAToB"], link["p2pBToA"] = link["p2pBToA"], link["p2pAToB"]
    raw["links"].reverse()
    topo = bindings.v1GpuTopology.from_json(raw)
    assert agent.gpu_topology_summary(topo) == expected


def test_list_agents_gpu_columns(capsys: pytest.CaptureFixture) -> None:
    agents = [
        agent_json("node02", topology_case("node02:")),
        agent_json("cpu-agent", None, slots={0: slot_json(0)}),
        agent_json("node01", topology_case("node01 with the exclude list")),
    ]
    with util.standard_cli_rsps() as rsps:
        rsps.get(f"{MASTER}/api/v1/agents", status=200, json={"agents": agents})
        cli.main(["agent", "list"])
    out = capsys.readouterr().out
    header = out.splitlines()[0]
    columns = [c.strip() for c in header.split("|")]
    assert columns[3:6] == ["Slots", "GPU Topology", "GPU Health"]
    rows = {line.split("|")[0].strip(): line for line in out.splitlines()[2:]}
    assert "| 4+4 NODE/SYS p2p" in rows["node02"]
    assert "| ok " in rows["node02"]
    assert "| 4+3 NODE/SYS p2p" in rows["node01"]
    assert "| narrow: 3,5 (x8 of x16 at start); excluded: 81:00.0 |" in rows["node01"]
    cpu_columns = [c.strip() for c in rows["cpu-agent"].split("|")]
    assert cpu_columns[4:6] == ["", ""]

    with util.standard_cli_rsps() as rsps:
        rsps.get(f"{MASTER}/api/v1/agents", status=200, json={"agents": agents})
        cli.main(["agent", "list", "--json"])
    listed = {a["id"]: a for a in json.loads(capsys.readouterr().out)}
    assert listed["node02"]["gpu_topology"] == "4+4 NODE/SYS p2p"
    assert listed["node02"]["gpu_health"] == "ok"
    assert listed["node01"]["gpu_health"] == "narrow: 3,5 (x8 of x16 at start); excluded: 81:00.0"
    assert listed["cpu-agent"]["gpu_topology"] == ""
    assert listed["cpu-agent"]["gpu_health"] == ""


def test_describe_agent(capsys: pytest.CaptureFixture) -> None:
    topology = topology_case("node01 with the exclude list")
    assert topology is not None
    slots = {i: slot_json(i) for i in (0, 1, 2, 3, 5, 6, 7)}
    slots[0] = slot_json(0, container_id="container-a")
    slots[1] = slot_json(1, enabled=False)
    slots[2] = slot_json(2, enabled=False, draining=True, container_id="container-b")
    slots[3] = slot_json(3, container_id="")
    # Only GetAgent is mocked: the mock fails on any other request, such as GetTasks.
    with util.standard_cli_rsps() as rsps:
        rsps.get(
            f"{MASTER}/api/v1/agents/node01",
            status=200,
            json={"agent": agent_json("node01", topology, slots=slots)},
        )
        cli.main(["agent", "describe", "node01"])
    out = capsys.readouterr().out
    lines = out.splitlines()

    assert "Agent ID:        node01" in lines
    assert "Driver Version:  610.57.04" in lines
    assert "Collected At:    2026-10-05 08:00:00+0000 (agent clock, at agent start)" in lines
    assert "GPU Topology:    4+3 NODE/SYS p2p" in lines
    assert "GPU Health:      narrow: 3,5 (x8 of x16 at start); excluded: 81:00.0" in lines

    def row(first: str) -> List[str]:
        found = [
            [c.strip() for c in line.split("|")]
            for line in lines
            if "|" in line and line.split("|")[0].strip() == first
        ]
        assert found, first
        return found[0]

    assert row("0")[1:4] == ["container-a", "ok", topology["gpus"][0]["uuid"]]
    assert row("1")[1] == "DISABLED"
    assert row("2")[1] == "DRAINING"
    assert row("3")[1:3] == ["OCCUPIED", "narrow"]
    assert row("5")[1] == "FREE"
    assert row("3")[4:] == ["0000:61:00.0", "0", "x8/x16", "Gen4/Gen4"]
    excluded = row("-")
    assert excluded[1:5] == ["EXCLUDED", "ok", topology["gpus"][-1]["uuid"], "0000:81:00.0"]

    assert "  Slot 3 (narrow):" in lines
    details = lines[lines.index("  Slot 3 (narrow):") + 1 :][:5]
    assert details == [
        "    Link at agent start: x8 of x16, Gen4 of Gen4"
        " (an observation, not a confirmed fault)",
        "    " + agent.GPU_NARROW_LINK_TEXT,
        "    NVML errors at agent start: none",
        "    Recent critical XIDs: not collected",
        "    Collected at: 2026-10-05 08:00:00+0000 (agent clock, at agent start)",
    ]
    excluded_details = lines.index("  Excluded GPU 0000:81:00.0 (ok):")
    assert lines[excluded_details + 1] == "    " + agent.GPU_EXCLUDED_TEXT

    # The level matrix labels the excluded GPU by its bus id; every pair is usable.
    start = lines.index("Link levels (slot ids; excluded GPUs by bus id):")
    matrix = [[c.strip() for c in line.split("|")] for line in lines[start + 1 : start + 11]]
    assert matrix[0] == ["", "0", "1", "2", "3", "5", "6", "7", "81:00.0"]
    assert matrix[6] == ["5", "SYS", "SYS", "SYS", "SYS", "X", "NODE", "NODE", "NODE"]
    assert matrix[9] == ["81:00.0", "SYS", "SYS", "SYS", "SYS", "NODE", "NODE", "NODE", "X"]
    assert "P2P: usable for every pair (READ and WRITE are OK in both directions)." in lines
    assert "P2P READ/WRITE" not in out


def test_describe_agent_p2p_matrix(capsys: pytest.CaptureFixture) -> None:
    topology = topology_case("no-p2p:")
    with util.standard_cli_rsps() as rsps:
        rsps.get(
            f"{MASTER}/api/v1/agents/a",
            status=200,
            json={"agent": agent_json("a", topology)},
        )
        cli.main(["agent", "describe", "a"])
    out = capsys.readouterr().out
    lines = out.splitlines()
    start = lines.index("P2P READ/WRITE from the row's GPU to the column's GPU:")
    matrix = [[c.strip() for c in line.split("|")] for line in lines[start + 1 : start + 6]]
    assert matrix[0] == ["", "0", "1", "2"]
    assert matrix[2] == ["0", "X", "OK/?", "CNS/CNS"]
    assert matrix[3] == ["1", "TNS/CNS", "X", "OK/?"]
    assert matrix[4] == ["2", "CNS/CNS", "OK/OK", "X"]
    assert agent.GPU_P2P_STATUS_LEGEND in lines


def test_describe_agent_unknown_topology(capsys: pytest.CaptureFixture) -> None:
    topology = topology_case("NVML init failed")
    assert topology is not None
    excluded_uuid = topology["gpus"][-1]["uuid"]
    with util.standard_cli_rsps() as rsps:
        rsps.get(
            f"{MASTER}/api/v1/agents/a",
            status=200,
            json={"agent": agent_json("a", topology)},
        )
        cli.main(["agent", "describe", "a"])
    lines = capsys.readouterr().out.splitlines()
    assert "GPU Topology:    unknown: NVML init: ERROR_LIBRARY_NOT_FOUND (12)" in lines
    assert f"GPU Health:      unknown; excluded: {excluded_uuid} (unknown)" in lines
    assert "Collected At:    unknown" in lines
    details = lines[lines.index("  Slot 0 (unknown):") + 1 :][:4]
    assert details == [
        "    Link at agent start: unknown",
        "    NVML errors at agent start: not collected",
        "    Recent critical XIDs: not collected",
        "    Collected at: unknown",
    ]
    assert f"  Excluded GPU {excluded_uuid} (unknown):" in lines
    assert not any(line.startswith("Link levels") for line in lines)


def test_describe_agent_json(capsys: pytest.CaptureFixture) -> None:
    topology = topology_case("g292 today")
    with util.standard_cli_rsps() as rsps:
        rsps.get(
            f"{MASTER}/api/v1/agents/g292",
            status=200,
            json={"agent": agent_json("g292", topology)},
        )
        cli.main(["agent", "describe", "g292", "--json"])
    assert json.loads(capsys.readouterr().out) == topology

    with util.standard_cli_rsps() as rsps:
        rsps.get(
            f"{MASTER}/api/v1/agents/cpu",
            status=200,
            json={"agent": agent_json("cpu", None, slots={0: slot_json(0)})},
        )
        cli.main(["agent", "describe", "cpu", "--json"])
    assert json.loads(capsys.readouterr().out) is None


def test_describe_agent_without_topology(capsys: pytest.CaptureFixture) -> None:
    with util.standard_cli_rsps() as rsps:
        rsps.get(
            f"{MASTER}/api/v1/agents/cpu",
            status=200,
            json={"agent": agent_json("cpu", None, slots={0: slot_json(0)})},
        )
        cli.main(["agent", "describe", "cpu"])
    lines = capsys.readouterr().out.splitlines()
    assert f"GPU Topology:    {agent.GPU_TOPOLOGY_NOT_REPORTED}" in lines
    assert not any("Slot" in line for line in lines)
