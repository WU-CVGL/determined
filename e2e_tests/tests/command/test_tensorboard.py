import contextlib
import json
import pathlib
import re
import subprocess
import time
from typing import Any, Dict, Iterator, List, Optional, Set

import pytest

from determined.common import api, util
from determined.common.api import bindings, errors
from determined.experimental import client
from tests import api_utils, detproc
from tests import experiment as exp
from tests.experiment import noop

num_trials = 1


SHARED_FS_CONFIG = {
    "checkpoint_storage": {
        "type": "shared_fs",
        "host_path": "/tmp",
        "storage_path": "test-tensorboard",
    },
}


def s3_config(secrets: Dict[str, str], prefix: Optional[str] = None) -> Dict[str, Any]:
    config = {
        "checkpoint_storage": {
            "type": "s3",
            "access_key": secrets["INTEGRATIONS_S3_ACCESS_KEY"],
            "secret_key": secrets["INTEGRATIONS_S3_SECRET_KEY"],
            "bucket": secrets["INTEGRATIONS_S3_BUCKET"],
        },
    }
    if prefix is not None:
        config["checkpoint_storage"]["prefix"] = prefix

    return config


LOADER_PREFIX = "tensorboard loader: "


@contextlib.contextmanager
def tensorboard_session(sess: api.Session, args: List[str]) -> Iterator[str]:
    """
    Starts a TensorBoard viewing session with `det tensorboard start <args> --detach`, yields its
    task ID, and kills it with `det shell kill`, since `det tensorboard kill` does not see sessions.
    The kill may fail only for a session that has ended by itself, such as one whose image lacks
    TensorBoard.
    """
    task_id = detproc.check_output(sess, ["det", "tensorboard", "start", *args, "--detach"]).strip()
    try:
        yield task_id
    finally:
        kill = detproc.run(
            sess,
            ["det", "shell", "kill", task_id],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        if kill.returncode != 0:
            task = bindings.get_GetTask(sess, taskId=task_id).task
            assert task.endTime is not None, kill.stderr


def tensorboard_url(sess: api.Session, task_id: str) -> str:
    """
    Waits until the session is ready and returns the path of its tensorboard service on the
    master, which ends with a slash.
    """
    err = api.wait_for_task_ready(sess, task_id)
    assert err is None, err
    services = bindings.get_GetTaskServices(sess, taskId=task_id)
    assert services.primary == "tensorboard", services.to_json()
    tensorboard = [s for s in services.services if s.name == "tensorboard"]
    assert len(tensorboard) == 1, services.to_json()
    url = tensorboard[0].url
    assert url.endswith("/"), url
    return url


def get_runs(sess: api.Session, url: str) -> List[str]:
    runs = sess.get(f"{url.lstrip('/')}data/runs").json()
    assert isinstance(runs, list), runs
    return runs


def wait_for_runs(
    sess: api.Session, task_id: str, expected: Set[str], timeout: float = 180
) -> List[str]:
    """
    Waits until TensorBoard lists every run of expected and returns all the runs that it lists.
    """
    url = tensorboard_url(sess, task_id)
    deadline = time.time() + timeout
    runs: List[str] = []
    while time.time() < deadline:
        runs = get_runs(sess, url)
        if expected <= set(runs):
            return runs
        time.sleep(2)
    raise AssertionError(f"TensorBoard lists {runs}, not all of {sorted(expected)}")


def task_log(sess: api.Session, task_id: str) -> List[str]:
    return [tl.message for tl in api.task_logs(sess, task_id)]


def loader_lines(sess: api.Session, task_id: str) -> List[str]:
    return [line for line in task_log(sess, task_id) if LOADER_PREFIX in line]


def trial_runs(exp_ref: client.Experiment) -> Set[str]:
    return {f"experiment/{exp_ref.id}/trial/{t.id}" for t in exp_ref.get_trials()}


def completed_experiment(
    sess: api.Session, config: Optional[Dict[str, Any]] = None
) -> client.Experiment:
    exp_ref = noop.create_experiment(sess, [noop.Report({"x": 1})], config=config)
    assert exp_ref.wait(interval=0.01) == client.ExperimentState.COMPLETED
    return exp_ref


@pytest.mark.slow
@pytest.mark.e2e_cpu
def test_start_tensorboard_for_shared_fs_experiment() -> None:
    """
    Start a TensorBoard viewing session on an experiment with shared_fs storage and check that
    TensorBoard lists its trial, without a loader error.
    """
    sess = api_utils.user_session()
    exp_ref = completed_experiment(sess, SHARED_FS_CONFIG)

    with tensorboard_session(sess, [str(exp_ref.id)]) as task_id:
        wait_for_runs(sess, task_id, trial_runs(exp_ref))
        config = bindings.get_GetShell(sess, shellId=task_id).config
        assert config["services"]["tensorboard"]["experiment_ids"] == [exp_ref.id], config
        lines = loader_lines(sess, task_id)
        assert lines, "no loader line in the task log"
        for line in lines:
            assert line.rstrip().endswith("loading 1 sources from shared_fs storage"), lines


@pytest.mark.slow
@pytest.mark.e2e_gpu
@pytest.mark.tensorflow2
@pytest.mark.parametrize("prefix", [None, "my/test/prefix"])
def test_start_tensorboard_for_s3_experiment(
    secrets: Dict[str, str], prefix: Optional[str]
) -> None:
    """
    Start a TensorBoard viewing session on an experiment with s3 storage, check that TensorBoard
    lists its trial, and that neither the task log nor the session's config holds the secret key.
    """
    sess = api_utils.user_session()
    exp_ref = completed_experiment(sess, s3_config(secrets, prefix))
    secret_key = secrets["INTEGRATIONS_S3_SECRET_KEY"]

    with tensorboard_session(sess, [str(exp_ref.id)]) as task_id:
        wait_for_runs(sess, task_id, trial_runs(exp_ref))
        log = task_log(sess, task_id)
        assert any(line.rstrip().endswith("loading 1 sources from s3 storage") for line in log)
        assert not any(secret_key in line for line in log)
        config = detproc.check_output(sess, ["det", "shell", "config", task_id])
        assert secret_key not in config


@pytest.mark.slow
@pytest.mark.e2e_cpu
@pytest.mark.e2e_slurm
@pytest.mark.e2e_pbs
def test_start_tensorboard_for_multi_experiment() -> None:
    """
    Start a TensorBoard viewing session on two experiments and the trials of a third, plus a
    trial of a listed experiment, and check that TensorBoard lists each trial once.
    """
    sess = api_utils.user_session()
    exp1 = completed_experiment(sess, SHARED_FS_CONFIG)
    exp2 = completed_experiment(sess, SHARED_FS_CONFIG)
    exp3 = completed_experiment(
        sess, {**SHARED_FS_CONFIG, "searcher": {"name": "random", "max_trials": 2}}
    )
    trial_ids = [str(t.id) for t in exp3.get_trials()] + [str(exp1.get_trials()[0].id)]
    expected = trial_runs(exp1) | trial_runs(exp2) | trial_runs(exp3)
    assert len(expected) == 4, expected

    args = [str(exp1.id), str(exp2.id), "-t", *trial_ids]
    with tensorboard_session(sess, args) as task_id:
        runs = wait_for_runs(sess, task_id, expected)
        assert sorted(runs) == sorted(expected), runs


@pytest.mark.e2e_cpu
def test_start_tensorboard_for_mixed_storage() -> None:
    """
    Starting a TensorBoard viewing session on experiments with different checkpoint storage is
    refused.
    """
    sess = api_utils.user_session()
    other = {"checkpoint_storage": {**SHARED_FS_CONFIG["checkpoint_storage"]}}
    other["checkpoint_storage"]["storage_path"] = "test-tensorboard-other"
    exp1 = completed_experiment(sess, SHARED_FS_CONFIG)
    exp2 = completed_experiment(sess, other)

    detproc.check_error(
        sess,
        ["det", "tensorboard", "start", str(exp1.id), str(exp2.id), "--detach"],
        "use different checkpoint storage",
    )


@pytest.mark.slow
@pytest.mark.e2e_cpu
def test_tensorboard_shows_live_growth() -> None:
    """
    Start a TensorBoard viewing session while an experiment reports metrics, and check that the
    scalars TensorBoard shows only grow, without doubled steps, up to the experiment's last step.
    """
    sess = api_utils.user_session()
    steps = 60
    actions: List[noop.Action] = []
    for _ in range(steps):
        actions += [noop.Report({"x": 1}), noop.Sleep(2)]
    exp_ref = noop.create_experiment(sess, actions, config=SHARED_FS_CONFIG)
    exp.wait_for_experiment_workload_progress(sess, exp_ref.id)
    run = f"experiment/{exp_ref.id}/trial/{exp.experiment_first_trial(sess, exp_ref.id)}"

    def scalar_steps(url: str) -> List[int]:
        resp = sess.get(
            f"{url.lstrip('/')}data/plugin/scalars/scalars",
            params={"run": run, "tag": "Determined/x"},
        )
        return [int(point[1]) for point in resp.json()]

    with tensorboard_session(sess, [str(exp_ref.id)]) as task_id:
        wait_for_runs(sess, task_id, {run})
        url = tensorboard_url(sess, task_id)
        first = scalar_steps(url)
        time.sleep(30)
        second = scalar_steps(url)
        for points in (first, second):
            assert points == sorted(set(points)), points
        assert set(first) <= set(second), (first, second)

        assert exp_ref.wait(interval=1) == client.ExperimentState.COMPLETED
        deadline = time.time() + 60
        last = second
        while time.time() < deadline and (not last or last[-1] != steps):
            time.sleep(5)
            last = scalar_steps(url)
        assert last == sorted(set(last)), last
        assert last and last[-1] == steps, last


@pytest.mark.e2e_cpu
def test_start_tensorboard_with_custom_image() -> None:
    """
    Start a TensorBoard viewing session with a custom image and check the session's config.
    """
    sess = api_utils.user_session()
    exp_ref = noop.create_experiment(sess)
    args = [str(exp_ref.id), "--config", "environment.image=python:3.8.16"]
    try:
        with tensorboard_session(sess, args) as task_id:
            res = detproc.check_output(sess, ["det", "shell", "config", task_id])
            config = util.yaml_safe_load(res)
            assert (
                config["environment"]["image"]["cpu"] == "python:3.8.16"
                and config["environment"]["image"]["cuda"] == "python:3.8.16"
                and config["environment"]["image"]["rocm"] == "python:3.8.16"
            ), config
    finally:
        exp_ref.kill()


@pytest.mark.e2e_cpu
def test_tensorboard_does_not_inherit_image_pull_secrets() -> None:
    """
    Start a TensorBoard viewing session on an experiment with image_pull_secrets and check that
    the session does not inherit them.
    """
    sess = api_utils.user_session()
    exp_secrets = [{"name": "ips"}]
    config = {"environment": {"pod_spec": {"spec": {"imagePullSecrets": exp_secrets}}}}
    exp_ref = noop.create_experiment(sess, config=config)
    try:
        with tensorboard_session(sess, [str(exp_ref.id)]) as task_id:
            res = detproc.check_output(sess, ["det", "shell", "config", task_id])
            config = util.yaml_safe_load(res)
            pod_spec = config["environment"].get("pod_spec") or {}
            assert "imagePullSecrets" not in (pod_spec.get("spec") or {}), config
    finally:
        exp_ref.kill()


@pytest.mark.e2e_cpu
def test_tensorboard_old_type() -> None:
    """
    A TensorBoard launched through the API as before is listed, configured and killed by the
    tensorboard commands.
    """
    sess = api_utils.user_session()
    exp_ref = completed_experiment(sess, SHARED_FS_CONFIG)
    tsb = bindings.post_LaunchTensorboard(
        sess, body=bindings.v1LaunchTensorboardRequest(experimentIds=[exp_ref.id])
    ).tensorboard
    try:
        listed = detproc.check_output(sess, ["det", "tensorboard", "list", "-q"])
        assert tsb.id in listed.split(), listed
        config = util.yaml_safe_load(
            detproc.check_output(sess, ["det", "tensorboard", "config", tsb.id])
        )
        assert isinstance(config, dict), config
    finally:
        detproc.check_call(sess, ["det", "tensorboard", "kill", tsb.id])


@pytest.mark.e2e_cpu
def test_start_tensorboard_without_detach() -> None:
    """
    Without --detach, `det tensorboard start` prints the launch line, waits until the session is
    ready, and prints its TensorBoard page.
    """
    sess = api_utils.user_session()
    exp_ref = completed_experiment(sess, SHARED_FS_CONFIG)
    task_id = None
    try:
        with detproc.Popen(
            sess,
            ["det", "tensorboard", "start", str(exp_ref.id), "--no-browser"],
            stdout=subprocess.PIPE,
        ) as p:
            assert p.stdout is not None
            output = []
            for raw in p.stdout:
                line = raw.decode()
                output.append(line)
                m = re.search(r"Launched tensorboard \(id: (.*), name: (.*)\)\.", line)
                if m:
                    task_id = m.group(1)
            assert p.wait() == 0, output
        assert task_id is not None, output
        url = [line for line in output if "Tensorboard is running at: " in line]
        assert len(url) == 1, output
        assert f"/tasks/{task_id}/interactive?tab=tensorboard" in url[0], output
    finally:
        if task_id is not None:
            detproc.check_call(sess, ["det", "shell", "kill", task_id])


@pytest.mark.e2e_cpu
def test_kill_tensorboard_session() -> None:
    """
    After `det shell kill`, the session's TensorBoard answers 404 and the task log has the
    supervisor's exit line.
    """
    sess = api_utils.user_session()
    exp_ref = completed_experiment(sess, SHARED_FS_CONFIG)
    task_id = detproc.check_output(
        sess, ["det", "tensorboard", "start", str(exp_ref.id), "--detach"]
    ).strip()
    killed = False
    try:
        url = tensorboard_url(sess, task_id)
        get_runs(sess, url)
        detproc.check_call(sess, ["det", "shell", "kill", task_id])
        killed = True

        def not_found() -> bool:
            try:
                get_runs(sess, url)
            except errors.NotFoundException:
                return True
            except errors.APIException:
                # The proxy can answer otherwise while the task stops.
                return False
            return False

        deadline = time.time() + 60
        while time.time() < deadline and not not_found():
            time.sleep(1)
        with pytest.raises(errors.NotFoundException):
            get_runs(sess, url)

        deadline = time.time() + 60
        log: List[str] = []
        while time.time() < deadline:
            log = task_log(sess, task_id)
            if any("[tensorboard] exited" in line for line in log):
                break
            time.sleep(2)
        assert any("[tensorboard] exited" in line for line in log), log
    finally:
        if not killed:
            detproc.check_call(sess, ["det", "shell", "kill", task_id])


@pytest.mark.e2e_cpu
def test_delete_tensorboard_for_experiment() -> None:
    """
    Start a random experiment, start a TensorBoard instance pointed to
    the experiment, delete tensorboard and verify deletion.
    """
    sess = api_utils.user_session()
    exp_ref = noop.create_experiment(sess, [noop.Report({"x": 1})])
    assert exp_ref.wait(interval=0.01) == client.ExperimentState.COMPLETED

    command = ["det", "e", "delete-tb-files", str(exp_ref.id)]
    detproc.check_call(sess, command)

    # Ensure Tensorboard files are deleted.
    assert exp_ref.config
    host_path = exp_ref.config["checkpoint_storage"]["host_path"]
    storage_path = exp_ref.config["checkpoint_storage"]["storage_path"]
    cluster_id = sess.get("info").json()["cluster_id"]
    tb_path = (
        pathlib.Path(host_path)
        / storage_path
        / cluster_id
        / "tensorboard"
        / "experiment"
        / str(exp_ref.id)
    )
    assert not tb_path.exists()


@pytest.mark.e2e_cpu
def test_tensorboard_directory_storage() -> None:
    """
    A TensorBoard viewing session on an experiment with directory storage lists its trial when it
    has the experiment's mount, and none without it.
    """
    sess = api_utils.user_session()
    bind_mounts = [{"host_path": "/tmp", "container_path": "/tmp/somepath"}]
    config = {
        "checkpoint_storage": {
            "type": "directory",
            "container_path": "/tmp/somepath",
        },
        "bind_mounts": bind_mounts,
    }
    exp_ref = completed_experiment(sess, config)

    args = [str(exp_ref.id), f"--config=bind_mounts={json.dumps(bind_mounts)}"]
    with tensorboard_session(sess, args) as task_id:
        wait_for_runs(sess, task_id, trial_runs(exp_ref))

    with tensorboard_session(sess, [str(exp_ref.id)]) as task_id:
        url = tensorboard_url(sess, task_id)
        time.sleep(10)
        assert get_runs(sess, url) == []
