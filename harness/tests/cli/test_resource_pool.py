import json
import pathlib

import pytest
from responses import matchers

from determined.cli import cli, errors, resource_pool
from tests.cli import util

MASTER = "http://localhost:8080"
DYNAMIC_POOLS_URL = f"{MASTER}/api/v1/resource-pools/dynamic"
# The master refuses dynamic-pool request bodies that are not labelled as JSON.
JSON_CONTENT_TYPE = matchers.header_matcher({"Content-Type": "application/json"})


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
