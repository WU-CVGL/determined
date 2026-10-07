import json
import pathlib
from typing import Any, Dict, List

import pytest
from responses import matchers

from determined.cli import cli, errors, resource_pool
from tests.cli import util

MASTER = "http://localhost:8080"
DYNAMIC_POOLS_URL = f"{MASTER}/api/v1/resource-pools/dynamic"
# The master refuses dynamic-pool request bodies that are not labelled as JSON.
JSON_CONTENT_TYPE = matchers.header_matcher({"Content-Type": "application/json"})
ACCESS_URL = f"{MASTER}/api/v1/resource-pool-access"


def dynamic_pool_response(state: str = "Ready") -> dict:
    response = {
        "pool_name": "online-gpu",
        "cluster_name": "agent-cluster",
        "config_version": 1,
        "state": state,
        "config": {"pool_name": "online-gpu", "max_aux_containers_per_agent": 100},
        "created_at": "2026-09-20T00:00:00Z",
        "updated_at": "2026-09-20T00:00:00Z",
    }
    if state == "Failed":
        response["error"] = "scheduler initialization failed"
    return response


def test_create_dynamic_pool_posts_yaml_config_and_reports_failed_state(
    tmp_path: pathlib.Path, capsys: pytest.CaptureFixture[str]
) -> None:
    config_path = tmp_path / "pool.yaml"
    config_path.write_text(
        "pool_name: online-gpu\nmax_aux_containers_per_agent: 100\n", encoding="utf-8"
    )
    response = dynamic_pool_response("Failed")

    with util.standard_cli_rsps() as rsps:
        rsps.post(
            DYNAMIC_POOLS_URL,
            status=201,
            match=[
                JSON_CONTENT_TYPE,
                matchers.json_params_matcher(
                    {
                        "cluster_name": "agent-cluster",
                        "idempotency_key": "request-123",
                        "config": {
                            "pool_name": "online-gpu",
                            "max_aux_containers_per_agent": 100,
                        },
                    }
                ),
            ],
            json=response,
        )
        with pytest.raises(SystemExit) as failed_exit:
            cli.main(
                [
                    "resource-pool",
                    "create",
                    str(config_path),
                    "--idempotency-key",
                    "request-123",
                    "--cluster-name",
                    "agent-cluster",
                ]
            )
        assert failed_exit.value.code == 1

    captured = capsys.readouterr()
    output = captured.out
    assert "online-gpu" in output
    assert "Failed" in output
    assert "scheduler initialization failed" in output
    assert "success" not in output.lower()
    assert "failed" in captured.err.lower()


def test_create_dynamic_pool_json_output(
    tmp_path: pathlib.Path, capsys: pytest.CaptureFixture[str]
) -> None:
    config_path = tmp_path / "pool.json"
    config_path.write_text(json.dumps({"pool_name": "online-gpu"}), encoding="utf-8")
    response = dynamic_pool_response()

    with util.standard_cli_rsps() as rsps:
        rsps.post(DYNAMIC_POOLS_URL, status=201, match=[JSON_CONTENT_TYPE], json=response)
        cli.main(
            [
                "resource-pool",
                "create",
                str(config_path),
                "--idempotency-key",
                "request-json",
                "--json",
            ]
        )

    assert json.loads(capsys.readouterr().out) == response


def test_list_dynamic_pools_filters_cluster_and_prints_json(
    capsys: pytest.CaptureFixture[str],
) -> None:
    response = {"resource_pools": [dynamic_pool_response()]}
    with util.standard_cli_rsps() as rsps:
        rsps.get(
            DYNAMIC_POOLS_URL,
            status=200,
            match=[matchers.query_param_matcher({"cluster_name": "agent-cluster"})],
            json=response,
        )
        cli.main(
            [
                "resource-pool",
                "list-dynamic",
                "--cluster-name",
                "agent-cluster",
                "--json",
            ]
        )

    assert json.loads(capsys.readouterr().out) == response


def test_retry_dynamic_pool_posts_empty_body_and_renders_state(
    capsys: pytest.CaptureFixture[str],
) -> None:
    response = dynamic_pool_response()
    with util.standard_cli_rsps() as rsps:
        rsps.post(
            f"{DYNAMIC_POOLS_URL}/online-gpu/retry",
            status=200,
            match=[matchers.query_param_matcher({"cluster_name": "agent-cluster"})],
            json=response,
        )
        cli.main(
            [
                "resource-pool",
                "retry",
                "online-gpu",
                "--cluster-name",
                "agent-cluster",
            ]
        )

    output = capsys.readouterr().out
    assert "online-gpu" in output
    assert "Ready" in output


def test_update_dynamic_pool_puts_config_and_reports_failed_state(
    tmp_path: pathlib.Path, capsys: pytest.CaptureFixture[str]
) -> None:
    config_path = tmp_path / "pool.yaml"
    config_path.write_text(
        "pool_name: online-gpu\ndescription: updated\nagent_reconnect_wait: 10m\n",
        encoding="utf-8",
    )
    response = dynamic_pool_response("Failed")

    with util.standard_cli_rsps() as rsps:
        rsps.put(
            f"{DYNAMIC_POOLS_URL}/online-gpu",
            status=200,
            match=[
                matchers.query_param_matcher({"cluster_name": "agent-cluster"}),
                JSON_CONTENT_TYPE,
                matchers.json_params_matcher(
                    {
                        "expected_revision": 3,
                        "config": {
                            "pool_name": "online-gpu",
                            "description": "updated",
                            "agent_reconnect_wait": "10m",
                        },
                    }
                ),
            ],
            json=response,
        )
        with pytest.raises(SystemExit) as failed_exit:
            cli.main(
                [
                    "resource-pool",
                    "update",
                    "online-gpu",
                    str(config_path),
                    "--expected-revision",
                    "3",
                    "--cluster-name",
                    "agent-cluster",
                ]
            )
        assert failed_exit.value.code == 1

    captured = capsys.readouterr()
    assert "online-gpu" in captured.out
    assert "scheduler initialization failed" in captured.out
    assert "failed" in captured.err.lower()


def test_update_dynamic_pool_without_expected_revision_prints_json(
    tmp_path: pathlib.Path, capsys: pytest.CaptureFixture[str]
) -> None:
    config_path = tmp_path / "pool.json"
    config_path.write_text(json.dumps({"pool_name": "online-gpu"}), encoding="utf-8")
    response = dynamic_pool_response()
    response.update({"revision": 2, "active_revision": 1, "pending_restart": True})

    with util.standard_cli_rsps() as rsps:
        rsps.put(
            f"{DYNAMIC_POOLS_URL}/online-gpu",
            status=200,
            match=[
                matchers.query_param_matcher({}),
                JSON_CONTENT_TYPE,
                matchers.json_params_matcher({"config": {"pool_name": "online-gpu"}}),
            ],
            json=response,
        )
        cli.main(["resource-pool", "update", "online-gpu", str(config_path), "--json"])

    assert json.loads(capsys.readouterr().out) == response


def test_adopt_dynamic_pool_posts_master_yaml_entry(
    tmp_path: pathlib.Path, capsys: pytest.CaptureFixture[str]
) -> None:
    config_path = tmp_path / "static-gpu.yaml"
    config_path.write_text(
        "pool_name: static-gpu\ndescription: GPUs\nagent_reconnect_wait: 10m\n",
        encoding="utf-8",
    )
    response = dynamic_pool_response()
    response.update(
        {
            "pool_name": "static-gpu",
            "revision": 1,
            "active_revision": None,
            "defined_in_master_yaml": True,
            "pending_restart": True,
        }
    )

    with util.standard_cli_rsps() as rsps:
        rsps.post(
            f"{DYNAMIC_POOLS_URL}/static-gpu/adopt",
            status=201,
            match=[
                matchers.query_param_matcher({"cluster_name": "agent-cluster"}),
                JSON_CONTENT_TYPE,
                matchers.json_params_matcher(
                    {
                        "config": {
                            "pool_name": "static-gpu",
                            "description": "GPUs",
                            "agent_reconnect_wait": "10m",
                        }
                    }
                ),
            ],
            json=response,
        )
        cli.main(
            [
                "resource-pool",
                "adopt",
                "static-gpu",
                str(config_path),
                "--cluster-name",
                "agent-cluster",
            ]
        )

    lines = capsys.readouterr().out.splitlines()
    cells = [cell.strip() for cell in lines[2].split("|")]
    assert cells[:6] == ["static-gpu", "agent-cluster", "Ready", "1", "master.yaml", "True"]


def test_adopt_dynamic_pool_json_output(
    tmp_path: pathlib.Path, capsys: pytest.CaptureFixture[str]
) -> None:
    config_path = tmp_path / "static-gpu.json"
    config_path.write_text(json.dumps({"pool_name": "static-gpu"}), encoding="utf-8")
    response = dynamic_pool_response()

    with util.standard_cli_rsps() as rsps:
        rsps.post(
            f"{DYNAMIC_POOLS_URL}/static-gpu/adopt",
            status=200,
            match=[
                matchers.query_param_matcher({}),
                JSON_CONTENT_TYPE,
                matchers.json_params_matcher({"config": {"pool_name": "static-gpu"}}),
            ],
            json=response,
        )
        cli.main(["resource-pool", "adopt", "static-gpu", str(config_path), "--json"])

    assert json.loads(capsys.readouterr().out) == response


def test_list_dynamic_pools_renders_revisions(capsys: pytest.CaptureFixture[str]) -> None:
    pending_restart = dynamic_pool_response()
    pending_restart.update(
        {"revision": 3, "active_revision": 2, "pending_restart": True, "spec_version": 1}
    )
    from_master_yaml = dynamic_pool_response()
    from_master_yaml.update(
        {
            "pool_name": "adopted-gpu",
            "revision": 1,
            "active_revision": None,
            "defined_in_master_yaml": True,
            "pending_restart": True,
        }
    )
    pending = dynamic_pool_response("Pending")
    pending.update(
        {"pool_name": "new-gpu", "revision": 1, "active_revision": None, "pending_restart": False}
    )
    with util.standard_cli_rsps() as rsps:
        rsps.get(
            DYNAMIC_POOLS_URL,
            status=200,
            json={"resource_pools": [pending_restart, from_master_yaml, pending]},
        )
        cli.main(["resource-pool", "list-dynamic"])

    lines = capsys.readouterr().out.splitlines()
    header = lines[0].split("|")
    assert [column.strip() for column in header] == [
        "Name",
        "Cluster",
        "State",
        "Revision",
        "Active",
        "Pending restart",
        "Error",
    ]
    rows = {
        cells[0]: cells
        for cells in ([cell.strip() for cell in line.split("|")] for line in lines[2:])
    }
    assert rows["online-gpu"][2:6] == ["Ready", "3", "2", "True"]
    assert rows["adopted-gpu"][2:6] == ["Ready", "1", "master.yaml", "True"]
    assert rows["new-gpu"][2:6] == ["Pending", "1", "-", "False"]


def test_dynamic_pool_config_must_be_mapping(tmp_path: pathlib.Path) -> None:
    config_path = tmp_path / "pool.yaml"
    config_path.write_text("- not\n- a\n- mapping\n", encoding="utf-8")

    with config_path.open() as config_file, pytest.raises(
        errors.CliError, match="resource pool config must be a YAML or JSON mapping"
    ):
        resource_pool._load_dynamic_pool_config(config_file)


def test_dynamic_pool_create_help_and_required_idempotency_key(
    tmp_path: pathlib.Path, capsys: pytest.CaptureFixture[str]
) -> None:
    with pytest.raises(SystemExit) as help_exit:
        cli.main(["resource-pool", "create", "--help"])
    assert help_exit.value.code == 0
    assert "--idempotency-key" in capsys.readouterr().out

    config_path = tmp_path / "pool.yaml"
    config_path.write_text("pool_name: online-gpu\n", encoding="utf-8")
    with pytest.raises(SystemExit) as parse_exit:
        cli.main(["resource-pool", "create", str(config_path)])
    assert parse_exit.value.code == 2
    assert "--idempotency-key" in capsys.readouterr().err


def test_job_list_without_pool_reports_a_hidden_default_pool(
    capsys: pytest.CaptureFixture[str],
) -> None:
    # The pool list holds only the pools the user may use: a restricted default is not in it.
    fixture = pathlib.Path(__file__).resolve().parent.parent / "fixtures" / "resource_pool.json"
    pool = json.loads(fixture.read_text(encoding="utf-8"))["resourcePool"]
    pool.update(
        name="public-pool",
        defaultComputePool=False,
        defaultAuxPool=False,
        agentFluentImage="",
        clusterName="default",
        details={},
        resourceManagerMetadata={},
    )

    with util.standard_cli_rsps() as rsps:
        rsps.get(
            f"{MASTER}/api/v1/resource-pools",
            status=200,
            json={"resourcePools": [pool], "pagination": {"total": 1}},
        )
        with pytest.raises(SystemExit) as failed_exit:
            cli.main(["job", "list"])
        assert failed_exit.value.code == 1

    assert (
        "the default compute pool is not available to you; name a pool with -r"
        in capsys.readouterr().err
    )


def access_item(pool_name: str, **fields: Any) -> Dict[str, Any]:
    item: Dict[str, Any] = {
        "pool_name": pool_name,
        "mode": "public",
        "exists": True,
        "default_compute": False,
        "default_aux": False,
        "workspace_defaults": [],
        "users": [],
        "restricted_at": None,
        "restricted_by": None,
    }
    item.update(fields)
    return item


def written(pool_name: str, warnings: List[str], **fields: Any) -> Dict[str, Any]:
    return {**access_item(pool_name, **fields), "warnings": warnings}


def default_warning(pool_name: str) -> str:
    return (
        f'"{pool_name}" is the cluster\'s default compute pool: submissions that omit '
        f'resources.resource_pool are refused for users without a grant on "{pool_name}"'
    )


def test_access_list_renders_mode_exists_defaults_and_users(
    capsys: pytest.CaptureFixture[str],
) -> None:
    response = {
        "resource_pools": [
            access_item("cpu"),
            access_item(
                "gpu",
                mode="restricted",
                default_compute=True,
                default_aux=True,
                workspace_defaults=[
                    {"workspace_id": 2, "workspace": "vision", "kind": "compute"},
                    {"workspace_id": 2, "workspace": "vision", "kind": "aux"},
                ],
                users=[
                    {"id": 3, "username": "alice", "active": True, "admin": False},
                    {"id": 4, "username": "carol", "active": False, "admin": False},
                    {"id": 1, "username": "root", "active": True, "admin": True},
                ],
                restricted_at="2026-10-06T00:00:00Z",
                restricted_by="root",
            ),
            access_item("retired", mode="restricted", exists=False),
        ]
    }
    with util.standard_cli_rsps() as rsps:
        rsps.get(ACCESS_URL, status=200, json=response)
        cli.main(["rp", "access", "list"])

    lines = capsys.readouterr().out.splitlines()
    assert [column.strip() for column in lines[0].split("|")] == [
        "Pool",
        "Mode",
        "Exists",
        "Defaults",
        "Users",
    ]
    rows = {
        cells[0]: cells
        for cells in ([cell.strip() for cell in line.split("|")] for line in lines[2:])
    }
    assert rows == {
        "cpu": ["cpu", "public", "True", "", ""],
        "gpu": [
            "gpu",
            "restricted",
            "True",
            "cluster compute, cluster aux, vision compute, vision aux",
            "alice, carol (inactive), root (admin)",
        ],
        "retired": ["retired", "restricted", "False", "", ""],
    }


def test_access_list_json_prints_the_response(capsys: pytest.CaptureFixture[str]) -> None:
    response = {"resource_pools": [access_item("gpu", mode="restricted")]}
    with util.standard_cli_rsps() as rsps:
        rsps.get(ACCESS_URL, status=200, json=response)
        cli.main(["resource-pool", "access", "list", "--json"])

    assert json.loads(capsys.readouterr().out) == response


def test_access_set_puts_each_pool_and_prints_server_warnings(
    capsys: pytest.CaptureFixture[str],
) -> None:
    missing_warning = (
        'no resource pool named "new/pool" exists; the setting applies to a pool created with '
        "this name"
    )
    with util.standard_cli_rsps() as rsps:
        for path, pool_name, warnings in [
            ("gpu", "gpu", [default_warning("gpu")]),
            ("new%2Fpool", "new/pool", [missing_warning]),
        ]:
            rsps.put(
                f"{ACCESS_URL}/{path}",
                status=200,
                match=[JSON_CONTENT_TYPE, matchers.json_params_matcher({"mode": "restricted"})],
                json=written(pool_name, warnings, mode="restricted"),
            )
        cli.main(["rp", "access", "set", "gpu", "new/pool", "--mode", "restricted"])

    captured = capsys.readouterr()
    assert captured.out.splitlines() == [
        'resource pool "gpu": restricted',
        'resource pool "new/pool": restricted',
    ]
    assert captured.err.splitlines() == [
        f"warning: {default_warning('gpu')}",
        f"warning: {missing_warning}",
    ]


def test_access_set_continues_past_a_failure_and_exits_1(
    capsys: pytest.CaptureFixture[str],
) -> None:
    with util.standard_cli_rsps() as rsps:
        rsps.put(
            f"{ACCESS_URL}/broken",
            status=500,
            match=[matchers.json_params_matcher({"mode": "public"})],
            json={"message": "listing resource pool restrictions: connection refused"},
        )
        rsps.put(
            f"{ACCESS_URL}/gpu",
            status=200,
            match=[matchers.json_params_matcher({"mode": "public"})],
            json=written("gpu", []),
        )
        with pytest.raises(SystemExit) as failed_exit:
            cli.main(["rp", "access", "set", "broken", "gpu", "--mode", "public"])
        assert failed_exit.value.code == 1

    captured = capsys.readouterr()
    assert captured.out.splitlines() == ['resource pool "gpu": public']
    assert captured.err.splitlines() == [
        'resource pool "broken": listing resource pool restrictions: connection refused'
    ]


def test_access_set_requires_a_mode(capsys: pytest.CaptureFixture[str]) -> None:
    with pytest.raises(SystemExit) as parse_exit:
        cli.main(["rp", "access", "set", "gpu", "--mode", "admins"])
    assert parse_exit.value.code == 2
    assert "invalid choice" in capsys.readouterr().err
    with pytest.raises(SystemExit) as parse_exit:
        cli.main(["rp", "access", "set", "gpu"])
    assert parse_exit.value.code == 2
    assert "--mode" in capsys.readouterr().err


def test_access_grant_posts_usernames_and_prints_server_warnings(
    capsys: pytest.CaptureFixture[str],
) -> None:
    users = [
        {"id": 3, "username": "alice", "active": True, "admin": False},
        {"id": 5, "username": "bob", "active": True, "admin": False},
    ]
    with util.standard_cli_rsps() as rsps:
        rsps.post(
            f"{ACCESS_URL}/gpu/grant",
            status=200,
            match=[
                JSON_CONTENT_TYPE,
                matchers.json_params_matcher({"usernames": ["alice", "bob"]}),
            ],
            json=written(
                "gpu",
                [default_warning("gpu")],
                mode="restricted",
                default_compute=True,
                users=users,
            ),
        )
        cli.main(["rp", "access", "grant", "gpu", "alice", "bob"])

    captured = capsys.readouterr()
    assert captured.out.splitlines() == ['resource pool "gpu" (restricted): granted alice, bob']
    assert captured.err.splitlines() == [f"warning: {default_warning('gpu')}"]


def test_access_revoke_posts_usernames(capsys: pytest.CaptureFixture[str]) -> None:
    with util.standard_cli_rsps() as rsps:
        rsps.post(
            f"{ACCESS_URL}/gpu/revoke",
            status=200,
            match=[JSON_CONTENT_TYPE, matchers.json_params_matcher({"usernames": ["bob"]})],
            json=written("gpu", []),
        )
        cli.main(["rp", "access", "revoke", "gpu", "bob"])

    captured = capsys.readouterr()
    assert captured.out.splitlines() == ['resource pool "gpu" (public): revoked bob']
    assert captured.err == ""


def test_access_grant_prints_the_server_error_and_exits_1(
    capsys: pytest.CaptureFixture[str],
) -> None:
    with util.standard_cli_rsps() as rsps:
        rsps.post(
            f"{ACCESS_URL}/gpu/grant",
            status=404,
            json={"message": "unknown users: bob, carol; nothing was changed"},
        )
        with pytest.raises(SystemExit) as failed_exit:
            cli.main(["rp", "access", "grant", "gpu", "alice", "bob", "carol"])
        assert failed_exit.value.code == 1

    assert capsys.readouterr().err.strip() == (
        'resource pool "gpu": unknown users: bob, carol; nothing was changed'
    )


def test_access_revoke_prints_a_bad_request_message(capsys: pytest.CaptureFixture[str]) -> None:
    with util.standard_cli_rsps() as rsps:
        rsps.post(
            f"{ACCESS_URL}/gpu/revoke",
            status=400,
            json={"message": "usernames must not be empty"},
        )
        with pytest.raises(SystemExit) as failed_exit:
            cli.main(["rp", "access", "revoke", "gpu", ""])
        assert failed_exit.value.code == 1

    assert capsys.readouterr().err.strip() == ('resource pool "gpu": usernames must not be empty')
