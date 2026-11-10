import copy
import json
import pathlib
from typing import Any, Dict, List, Optional

import pytest
import responses

from determined.cli import cli, render
from tests.cli import util
from tests.fixtures import api_responses

MASTER = "http://localhost:8080"
FIXTURES = pathlib.Path(__file__).resolve().parents[1] / "fixtures"
TASK_ID = "1a2b3c4d-0000-4000-8000-000000000001"

HIDDEN = {"viewTitle": False, "viewLogs": False, "viewConfig": False, "manageSharing": False}
SHOWN = {"viewTitle": True, "viewLogs": True, "viewConfig": True, "manageSharing": False}

# The gateway's error bodies: unary calls, and the first chunk of a refused stream.
UNARY_LOGS = {"error": {"code": 7, "reason": "PermissionDenied", "error": "logs not shared"}}
UNARY_CONFIG = {"error": {"code": 7, "reason": "PermissionDenied", "error": "config not shared"}}
STREAM_LOGS = {
    "error": {
        "grpcCode": 7,
        "httpCode": 403,
        "message": "logs not shared",
        "httpStatus": "Forbidden",
    }
}

# The error that ends a stream whose logs the master refuses while it runs, as a follow stream
# received it in an end-to-end check.
STREAM_LOGS_REVOKED = {
    "error": {
        "grpcCode": 7,
        "httpCode": 403,
        "message": (
            "1 error occurred:\n\t* failed to fetch batch: rpc error: code = PermissionDenied "
            "desc = logs not shared\n\n"
        ),
        "httpStatus": "Forbidden",
    }
}


def log_line(n: int) -> Dict[str, Any]:
    return {
        "result": {
            "id": str(n),
            "level": "LOG_LEVEL_INFO",
            "log": f"tick-{n}\n",
            "message": f"tick-{n}\n",
            "taskId": TASK_ID,
            "timestamp": "2026-11-09T22:20:49Z",
        }
    }


def with_caps(obj: Dict[str, Any], caps: Optional[Dict[str, bool]]) -> Dict[str, Any]:
    out = copy.deepcopy(obj)
    out.pop("capabilities", None)
    if caps is not None:
        out["capabilities"] = caps
    return out


def table_rows(out: str) -> List[List[str]]:
    lines = [ln for ln in out.splitlines() if "|" in ln]
    return [[c.strip() for c in ln.split("|")] for ln in lines[1:]]


def run_refused(capsys: pytest.CaptureFixture, args: List[str]) -> str:
    with pytest.raises(SystemExit) as e:
        cli.main(args)
    assert e.value.code == 1
    captured = capsys.readouterr()
    err: str = captured.err.strip()
    assert captured.out == ""
    assert "Traceback" not in err
    assert len(err.splitlines()) == 1
    return err


def pagination(n: int) -> Dict[str, int]:
    return {"offset": 0, "limit": n, "startIndex": 0, "endIndex": n, "total": n}


def test_experiment_list_omits_hidden_title(capsys: pytest.CaptureFixture) -> None:
    exps = api_responses.sample_get_experiments().to_json()["experiments"][:3]
    exps[0] = with_caps(exps[0], HIDDEN)
    exps[0]["name"] = ""
    exps[1] = with_caps(exps[1], SHOWN)
    exps[2] = with_caps(exps[2], None)
    with util.standard_cli_rsps() as rsps:
        rsps.get(
            f"{MASTER}/api/v1/experiments",
            json={"experiments": exps, "pagination": pagination(3)},
        )
        cli.main(["experiment", "list"])
    rows = table_rows(capsys.readouterr().out)
    names = {int(r[0]): r[2] for r in rows}
    assert names[exps[0]["id"]] == render.OMITTED_VALUE
    assert names[exps[1]["id"]] == exps[1]["name"]
    assert names[exps[2]["id"]] == exps[2]["name"]


def test_experiment_config_not_shared(capsys: pytest.CaptureFixture) -> None:
    resp = api_responses.sample_get_experiment().to_json()
    resp["experiment"] = with_caps(resp["experiment"], HIDDEN)
    resp["experiment"]["config"] = None
    resp["config"] = None
    exp_id = resp["experiment"]["id"]
    with util.standard_cli_rsps() as rsps:
        rsps.get(f"{MASTER}/api/v1/experiments/{exp_id}", json=resp)
        err = run_refused(capsys, ["experiment", "config", str(exp_id)])
    assert err == "Config not shared"


def test_experiment_config_shown(capsys: pytest.CaptureFixture) -> None:
    resp = api_responses.sample_get_experiment().to_json()
    resp["experiment"] = with_caps(resp["experiment"], None)
    resp["experiment"]["config"] = {"name": "visible"}
    exp_id = resp["experiment"]["id"]
    with util.standard_cli_rsps() as rsps:
        rsps.get(f"{MASTER}/api/v1/experiments/{exp_id}", json=resp)
        cli.main(["experiment", "config", str(exp_id)])
    assert capsys.readouterr().out == "name: visible\n"


def test_experiment_describe_omits_hidden_title(capsys: pytest.CaptureFixture) -> None:
    resp = api_responses.sample_get_experiment().to_json()
    resp["experiment"] = with_caps(resp["experiment"], HIDDEN)
    resp["experiment"].update({"name": "", "description": "", "labels": []})
    exp_id = resp["experiment"]["id"]
    with util.standard_cli_rsps() as rsps:
        rsps.get(f"{MASTER}/api/v1/experiments/{exp_id}", json=resp)
        rsps.get(
            f"{MASTER}/api/v1/experiments/{exp_id}/trials",
            json={"trials": [], "pagination": pagination(0)},
        )
        cli.main(["experiment", "describe", str(exp_id)])
    rows = table_rows(capsys.readouterr().out)
    # Name, Description and Labels of the experiment table.
    assert [rows[0][5], rows[0][6], rows[0][9]] == [render.OMITTED_VALUE] * 3


def job_json(n: int, caps: Optional[Dict[str, bool]], job_type: str, name: str) -> Dict[str, Any]:
    job: Dict[str, Any] = {
        "summary": {"state": "STATE_QUEUED", "jobsAhead": n},
        "type": job_type,
        "submissionTime": "2026-11-10T08:00:00Z",
        "username": "alice",
        "userId": 2,
        "resourcePool": "default",
        "isPreemptible": True,
        "priority": 42,
        "weight": 1,
        "entityId": str(10 + n),
        "jobId": f"job-{n}",
        "requestedSlots": 1,
        "allocatedSlots": 0,
        "name": name,
        "workspaceId": 1,
    }
    if caps is not None:
        job["capabilities"] = caps
    return {"full": job}


def test_job_list_omits_hidden_title(capsys: pytest.CaptureFixture) -> None:
    pool = json.loads((FIXTURES / "resource_pool.json").read_text())["resourcePool"]
    pool.update(agentFluentImage="", clusterName="", details={}, resourceManagerMetadata={})
    jobs = [
        job_json(0, HIDDEN, "TYPE_EXPERIMENT", ""),
        job_json(1, HIDDEN, "TYPE_GENERIC", ""),
        job_json(2, SHOWN, "TYPE_EXPERIMENT", "visible-exp"),
        job_json(3, None, "TYPE_COMMAND", "old-master"),
    ]
    with util.standard_cli_rsps() as rsps:
        rsps.get(
            f"{MASTER}/api/v1/resource-pools",
            json={"resourcePools": [pool], "pagination": pagination(1)},
        )
        rsps.get(
            f"{MASTER}/api/v1/job-queues-v2",
            json={"jobs": jobs, "pagination": pagination(4)},
        )
        cli.main(["job", "list"])
    names = {r[1]: r[3] for r in table_rows(capsys.readouterr().out)}
    assert names == {
        "job-0": f"{render.OMITTED_VALUE} (10)",
        "job-1": render.OMITTED_VALUE,
        "job-2": "visible-exp (12)",
        "job-3": "old-master",
    }


def allocation_json(n: int, caps: Optional[Dict[str, bool]], name: str) -> Dict[str, Any]:
    out: Dict[str, Any] = {
        "taskId": f"task-{n}",
        "allocationId": f"task-{n}.alloc",
        "name": name,
        "registeredTime": f"2026-11-10T08:0{n}:00Z",
        "resourcePool": "default",
        "slotsNeeded": 1,
        "resources": [{"containerId": f"container-{n}", "agentDevices": {}}],
        "schedulerType": "fair_share",
    }
    if caps is not None:
        out["capabilities"] = caps
    return out


ALLOCATIONS = [
    allocation_json(0, HIDDEN, ""),
    allocation_json(1, SHOWN, "visible-task"),
    allocation_json(2, None, "old-master"),
]


def test_task_list_omits_hidden_title(capsys: pytest.CaptureFixture) -> None:
    with util.standard_cli_rsps() as rsps:
        rsps.get(
            f"{MASTER}/api/v1/tasks",
            json={"allocationIdToSummary": {a["allocationId"]: a for a in ALLOCATIONS}},
        )
        cli.main(["task", "list"])
    names = {r[0]: r[2] for r in table_rows(capsys.readouterr().out)}
    assert names == {
        "task-0": render.OMITTED_VALUE,
        "task-1": "visible-task",
        "task-2": "old-master",
    }


def test_slot_list_omits_hidden_title(capsys: pytest.CaptureFixture) -> None:
    slots = {
        f"/agents/node01/slots/{n}": {
            "id": str(n),
            "device": {"id": n, "brand": "GPU", "type": "TYPE_CUDA"},
            "enabled": True,
            "draining": False,
            "container": {
                "id": f"container-{n}",
                "parent": "",
                "state": "STATE_RUNNING",
                "devices": [],
                "permissionDenied": False,
            },
        }
        for n in range(3)
    }
    agent = {
        "id": "node01",
        "registeredTime": "2026-11-10T08:00:00Z",
        "slots": slots,
        "containers": {},
        "resourcePools": ["default"],
        "addresses": ["10.0.0.1"],
        "enabled": True,
        "draining": False,
        "version": "0.43.1",
        "slotStats": {"typeStats": {}, "brandStats": {}},
    }
    with util.standard_cli_rsps() as rsps:
        rsps.get(
            f"{MASTER}/api/v1/tasks",
            json={"allocationIdToSummary": {a["allocationId"]: a for a in ALLOCATIONS}},
        )
        rsps.get(f"{MASTER}/api/v1/agents", json={"agents": [agent]})
        cli.main(["slot", "list"])
    names = {r[2]: r[6] for r in table_rows(capsys.readouterr().out)}
    assert names == {"0": render.OMITTED_VALUE, "1": "visible-task", "2": "old-master"}


def generic_task_json(n: int, caps: Optional[Dict[str, bool]], name: str) -> Dict[str, Any]:
    out: Dict[str, Any] = {
        "taskId": f"task-{n}",
        "jobId": f"job-{n}",
        "name": name,
        "description": "",
        "userId": 2,
        "username": "alice",
        "state": "GENERIC_TASK_STATE_ACTIVE",
        "slots": 1,
        "noPause": True,
        "projectId": 1,
        "workspaceId": 1,
        "resourcePool": "default",
        "startTime": "2026-11-10T08:00:00Z",
    }
    if caps is not None:
        out["capabilities"] = caps
    return out


def test_task_list_generic_omits_hidden_title(capsys: pytest.CaptureFixture) -> None:
    tasks = [
        generic_task_json(0, HIDDEN, ""),
        generic_task_json(1, SHOWN, "visible-task"),
        generic_task_json(2, None, "old-master"),
    ]
    with util.standard_cli_rsps() as rsps:
        rsps.get(
            f"{MASTER}/api/v1/generic-tasks", json={"tasks": tasks, "pagination": pagination(3)}
        )
        cli.main(["task", "list-generic", "--all"])
    names = {r[0]: r[1] for r in table_rows(capsys.readouterr().out)}
    assert names == {
        "task-0": render.OMITTED_VALUE,
        "task-1": "visible-task",
        "task-2": "old-master",
    }


def test_task_list_generic_json_is_as_sent(capsys: pytest.CaptureFixture) -> None:
    with util.standard_cli_rsps() as rsps:
        rsps.get(
            f"{MASTER}/api/v1/generic-tasks",
            json={"tasks": [generic_task_json(0, HIDDEN, "")], "pagination": pagination(1)},
        )
        cli.main(["task", "list-generic", "--all", "--json"])
    assert json.loads(capsys.readouterr().out)[0]["name"] == ""


def shell_json(n: int, caps: Optional[Dict[str, bool]], description: str) -> Dict[str, Any]:
    out: Dict[str, Any] = {
        "id": f"1a2b3c4d-0000-4000-8000-00000000000{n}",
        "description": description,
        "state": "STATE_RUNNING",
        "startTime": f"2026-11-10T08:0{n}:00Z",
        "username": "alice",
        "userId": 2,
        "resourcePool": "default",
        "exitStatus": "",
        "workspaceId": 1,
        "jobId": f"job-{n}",
        "config": None if caps == HIDDEN else {"description": description},
    }
    if caps is not None:
        out["capabilities"] = caps
    return out


def test_shell_list_omits_hidden_title(capsys: pytest.CaptureFixture) -> None:
    shells = [
        shell_json(0, HIDDEN, ""),
        shell_json(1, SHOWN, "visible"),
        shell_json(2, None, "old"),
    ]
    workspace = api_responses.sample_get_workspace().workspace.to_json()
    workspace["id"] = 1
    with util.standard_cli_rsps() as rsps:
        rsps.get(f"{MASTER}/api/v1/shells", json={"shells": shells})
        rsps.get(
            f"{MASTER}/api/v1/workspaces",
            json={"workspaces": [workspace], "pagination": pagination(1)},
        )
        cli.main(["shell", "list", "--all"])
    descriptions = {r[0][-1]: r[2] for r in table_rows(capsys.readouterr().out)}
    assert descriptions == {"0": render.OMITTED_VALUE, "1": "visible", "2": "old"}


def test_shell_config_not_shared(capsys: pytest.CaptureFixture) -> None:
    shell = shell_json(0, HIDDEN, "")
    with util.standard_cli_rsps() as rsps:
        rsps.get(f"{MASTER}/api/v1/shells/{shell['id']}", json={"shell": shell, "config": None})
        err = run_refused(capsys, ["shell", "config", shell["id"]])
    assert err == "Config not shared"


def expect_primary_service(rsps: responses.RequestsMock, task_id: str, primary: str) -> None:
    rsps.get(
        f"{MASTER}/api/v1/tasks/{task_id}/services",
        json={"taskId": task_id, "userId": 2, "workspaceId": 1, "primary": primary, "services": []},
    )


def test_tensorboard_list_omits_hidden_title(capsys: pytest.CaptureFixture) -> None:
    sessions = [
        shell_json(0, HIDDEN, ""),
        shell_json(1, SHOWN, "visible"),
        shell_json(2, None, "old"),
    ]
    workspace = api_responses.sample_get_workspace().workspace.to_json()
    workspace["id"] = 1
    with util.standard_cli_rsps() as rsps:
        rsps.get(f"{MASTER}/api/v1/shells", json={"shells": sessions})
        for session in sessions:
            expect_primary_service(rsps, session["id"], "tensorboard")
        rsps.get(
            f"{MASTER}/api/v1/workspaces",
            json={"workspaces": [workspace], "pagination": pagination(1)},
        )
        cli.main(["tensorboard", "list", "--all"])
    descriptions = {r[0][-1]: r[2] for r in table_rows(capsys.readouterr().out)}
    assert descriptions == {"0": render.OMITTED_VALUE, "1": "visible", "2": "old"}


def test_tensorboard_config_not_shared(capsys: pytest.CaptureFixture) -> None:
    session = shell_json(0, HIDDEN, "")
    with util.standard_cli_rsps() as rsps:
        expect_primary_service(rsps, session["id"], "tensorboard")
        rsps.get(f"{MASTER}/api/v1/shells/{session['id']}", json={"shell": session, "config": None})
        err = run_refused(capsys, ["tensorboard", "config", session["id"]])
    assert err == "Config not shared"


def test_task_logs_not_shared(capsys: pytest.CaptureFixture) -> None:
    with util.standard_cli_rsps() as rsps:
        rsps.get(f"{MASTER}/api/v1/tasks/{TASK_ID}/logs", status=403, json=STREAM_LOGS)
        err = run_refused(capsys, ["task", "logs", TASK_ID])
    assert err == "logs not shared"


def test_trial_logs_not_shared(capsys: pytest.CaptureFixture) -> None:
    with util.standard_cli_rsps() as rsps:
        rsps.get(f"{MASTER}/api/v1/trials/7/logs", status=403, json=STREAM_LOGS)
        err = run_refused(capsys, ["trial", "logs", "7"])
    assert err == "logs not shared"


def test_task_logs_follow_stream_ends_not_shared(capsys: pytest.CaptureFixture) -> None:
    # A follow stream that the master ends when the logs are no longer shared.
    with util.standard_cli_rsps() as rsps:
        rsps.get(
            f"{MASTER}/api/v1/tasks/{TASK_ID}/logs",
            body=json.dumps(STREAM_LOGS) + "\n",
            status=200,
        )
        err = run_refused(capsys, ["task", "logs", "-f", TASK_ID])
    assert err == "logs not shared"


def test_task_logs_follow_stream_revoked_while_running(capsys: pytest.CaptureFixture) -> None:
    # A follow stream that delivered lines, then ended when the logs were no longer shared.
    body = "".join(
        json.dumps(obj) + "\n" for obj in [log_line(1), log_line(2), STREAM_LOGS_REVOKED]
    )
    with util.standard_cli_rsps() as rsps:
        rsps.get(f"{MASTER}/api/v1/tasks/{TASK_ID}/logs", body=body, status=200)
        with pytest.raises(SystemExit) as e:
            cli.main(["task", "logs", "-f", TASK_ID])
    assert e.value.code == 1
    captured = capsys.readouterr()
    assert "tick-1" in captured.out and "tick-2" in captured.out
    assert "Task log stream ended" not in captured.out
    assert captured.err.strip() == "logs not shared"


@pytest.mark.parametrize(
    "message",
    [
        # The refusal text inside a line, not at its end.
        "1 error occurred:\n\t* code = PermissionDenied desc = logs not shared, then more\n\n",
        # The refusal text as a mere part of a line.
        "1 error occurred:\n\t* failed to fetch batch: why logs not shared\n\n",
        # Another refusal of the stream.
        "1 error occurred:\n\t* failed to fetch batch: rpc error: code = PermissionDenied "
        "desc = denied\n\n",
    ],
)
def test_task_logs_follow_stream_other_error_unchanged(
    capsys: pytest.CaptureFixture, message: str
) -> None:
    error = copy.deepcopy(STREAM_LOGS_REVOKED)
    error["error"]["message"] = message
    body = "".join(json.dumps(obj) + "\n" for obj in [log_line(1), error])
    with util.standard_cli_rsps() as rsps:
        rsps.get(f"{MASTER}/api/v1/tasks/{TASK_ID}/logs", body=body, status=200)
        with pytest.raises(SystemExit) as e:
            cli.main(["task", "logs", "-f", TASK_ID])
    assert e.value.code == 1
    captured = capsys.readouterr()
    assert "Task log stream ended" in captured.out
    assert captured.err.startswith("Failed on operation get_TaskLogs: Stream Error during")


def test_task_logs_other_refusal_unchanged(capsys: pytest.CaptureFixture) -> None:
    body = {"error": {"code": 7, "reason": "PermissionDenied", "error": "denied"}}
    with util.standard_cli_rsps() as rsps:
        rsps.get(f"{MASTER}/api/v1/tasks/{TASK_ID}/logs", status=403, json=body)
        with pytest.raises(SystemExit) as e:
            cli.main(["task", "logs", TASK_ID])
    assert e.value.code == 1
    captured = capsys.readouterr()
    assert "Forbidden(denied)" in captured.err
    assert "Task log stream ended" in captured.out


def test_experiment_logs_not_shared(capsys: pytest.CaptureFixture) -> None:
    trials = api_responses.sample_get_experiment_trials().to_json()
    exp_id = trials["trials"][0]["experimentId"]
    first_trial_id = min(t["id"] for t in trials["trials"])
    with util.standard_cli_rsps() as rsps:
        rsps.get(f"{MASTER}/api/v1/experiments/{exp_id}/trials", json=trials)
        rsps.get(f"{MASTER}/api/v1/trials/{first_trial_id}/logs", status=403, json=STREAM_LOGS)
        err = run_refused(capsys, ["experiment", "logs", str(exp_id)])
    assert err == "logs not shared"


@pytest.mark.parametrize(
    "args",
    [["task", "config", TASK_ID], ["task", "fork", TASK_ID]],
)
def test_generic_task_config_not_shared(capsys: pytest.CaptureFixture, args: List[str]) -> None:
    with util.standard_cli_rsps() as rsps:
        if args[1] == "config":
            rsps.get(f"{MASTER}/api/v1/tasks/{TASK_ID}/config", status=403, json=UNARY_CONFIG)
        else:
            rsps.post(f"{MASTER}/api/v1/generic-tasks", status=403, json=UNARY_CONFIG)
        err = run_refused(capsys, args)
    assert err == "config not shared"


def test_unary_logs_refusal(capsys: pytest.CaptureFixture) -> None:
    # The same text from a unary body, as an older gateway sends it.
    with util.standard_cli_rsps() as rsps:
        rsps.get(f"{MASTER}/api/v1/tasks/{TASK_ID}/logs", status=403, json=UNARY_LOGS)
        err = run_refused(capsys, ["task", "logs", TASK_ID])
    assert err == "logs not shared"


@pytest.mark.parametrize(
    "args,method,path,body,resp",
    [
        (["set", "description", "d"], "PATCH", "", {"description": "d"}, {}),
        (["set", "name", "n"], "PATCH", "", {"name": "n"}, {}),
    ],
)
def test_experiment_title_edits_change_one_field(
    args: List[str],
    method: str,
    path: str,
    body: Dict[str, Any],
    resp: Dict[str, Any],
) -> None:
    # The title may not be shared with the editor: an edit changes its one field and never
    # writes back what the master omitted.
    with util.standard_cli_rsps() as rsps:
        match = [responses.matchers.json_params_matcher({"id": 1, **body})]
        rsps.add(method, f"{MASTER}/api/v1/experiments/1{path}", json=resp, match=match)
        cli.main(["experiment", args[0], args[1], "1", args[2]])


@pytest.mark.parametrize("action,label", [("add", "new"), ("remove", "old")])
@pytest.mark.parametrize("shared", [True, False])
def test_experiment_label_edits(action: str, label: str, shared: bool) -> None:
    resp = api_responses.sample_get_experiment().to_json()
    exp_id = resp["experiment"]["id"]
    resp["experiment"] = with_caps(resp["experiment"], SHOWN if shared else HIDDEN)
    resp["experiment"]["labels"] = ["old"] if shared else []
    with util.standard_cli_rsps() as rsps:
        rsps.get(f"{MASTER}/api/v1/experiments/{exp_id}", json=resp)
        if shared:
            # The labels the user sees are the stored ones; any label routes in a patch.
            labels = ["old", "new"] if action == "add" else []
            rsps.patch(
                f"{MASTER}/api/v1/experiments/{exp_id}",
                json={},
                match=[responses.matchers.json_params_matcher({"id": exp_id, "labels": labels})],
            )
        else:
            # The labels are omitted: the master changes the stored ones.
            method = "PUT" if action == "add" else "DELETE"
            rsps.add(
                method,
                f"{MASTER}/api/v1/experiments/{exp_id}/labels/{label}",
                json={"labels": []},
            )
        cli.main(["experiment", "label", action, str(exp_id), label])


@pytest.mark.parametrize("action,method", [("add", "PUT"), ("remove", "DELETE")])
def test_experiment_label_edits_hidden_title_slash(action: str, method: str) -> None:
    # A label with a slash takes more than one path segment, which the label routes accept.
    resp = api_responses.sample_get_experiment().to_json()
    exp_id = resp["experiment"]["id"]
    resp["experiment"] = with_caps(resp["experiment"], HIDDEN)
    resp["experiment"]["labels"] = []
    with util.standard_cli_rsps() as rsps:
        rsps.get(f"{MASTER}/api/v1/experiments/{exp_id}", json=resp)
        rsps.add(
            method,
            f"{MASTER}/api/v1/experiments/{exp_id}/labels/train/validation",
            json={"labels": []},
        )
        cli.main(["experiment", "label", action, str(exp_id), "train/validation"])
        sent = [c.request for c in rsps.calls if c.request.method == method]
    assert [r.path_url for r in sent] == [f"/api/v1/experiments/{exp_id}/labels/train/validation"]
