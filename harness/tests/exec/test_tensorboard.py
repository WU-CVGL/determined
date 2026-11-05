import json
import os
import pathlib
import subprocess
import sys
from typing import Optional
from unittest import mock

import pytest

from determined.exec import tensorboard

TOKEN = "test-token"


@pytest.fixture
def task_env(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("TENSORBOARD_PORT", "2701")
    monkeypatch.setenv("DET_TASK_ID", "task-1")
    monkeypatch.setenv("DET_TENSORBOARD_TOKEN", TOKEN)


def test_service_env() -> None:
    environ = {"TENSORBOARD_PORT": "2701", "DET_TASK_ID": "task-1", "DET_TENSORBOARD_TOKEN": TOKEN}
    env = tensorboard.service_env(environ, ["--samples_per_plugin=images=100", 'a "b"'])
    assert env == {
        **environ,
        "DET_TENSORBOARD_PORT": "2701",
        "DET_TENSORBOARD_PATH_PREFIX": "/proxy/task-1/",
        "DET_TENSORBOARD_ARGS": '["--samples_per_plugin=images=100", "a \\"b\\""]',
    }
    assert json.loads(tensorboard.service_env(environ, [])["DET_TENSORBOARD_ARGS"]) == []


@pytest.mark.usefixtures("task_env")
def test_start_service_runs_the_program() -> None:
    with mock.patch.object(tensorboard.subprocess, "Popen") as popen:
        process, ready = tensorboard.start_service("/tmp/logs", ["--reload_interval=5"])
    assert process is popen.return_value
    # The launcher keeps the read end only.
    assert os.read(ready, 1) == b""
    os.close(ready)
    popen.assert_called_once()
    args, kwargs = popen.call_args
    assert args == ([sys.executable, tensorboard.SERVICE_PROGRAM, "/tmp/logs"],)
    assert kwargs["env"] == tensorboard.service_env(os.environ, ["--reload_interval=5"])
    assert kwargs["env"]["DET_TENSORBOARD_TOKEN"] == TOKEN
    assert kwargs["close_fds"] is False
    assert callable(kwargs["preexec_fn"])


@pytest.mark.parametrize("occupied", [False, True])
@pytest.mark.usefixtures("task_env")
def test_start_service_passes_the_ready_line(tmp_path: pathlib.Path, occupied: bool) -> None:
    # The program writes its ready line to descriptor 3, which the launcher reads from the pipe,
    # whether or not the launcher has a descriptor 3 of its own. The launcher runs in a python of
    # its own, which starts with the standard streams only.
    out = tmp_path / "out.json"
    program = tmp_path / "program.py"
    program.write_text(
        "import json, os, sys, time\n"
        "json.dump({'argv': sys.argv[1:], 'prefix': os.environ['DET_TENSORBOARD_PATH_PREFIX']},\n"
        f"          open({str(out)!r}, 'w'))\n"
        "time.sleep(1)\n"
        "os.write(3, b'tensorboard: http')\n"
        "os.write(3, b' ready\\n')\n"
        "time.sleep(30)\n"
    )
    launcher = (
        "import os, sys\n"
        "from determined.exec import tensorboard\n"
        f"pipe = os.pipe() if {occupied} else None\n"
        f"tensorboard.SERVICE_PROGRAM = {str(program)!r}\n"
        "p, ready = tensorboard.start_service('/tmp/logs', [])\n"
        "received = bytearray()\n"
        "ticks = 0\n"
        "while not tensorboard.check_service_ready(p, ready, received):\n"
        "    ticks += 1\n"
        "    assert ticks < 30\n"
        "p.kill()\n"
        "p.wait()\n"
    )
    # The launcher imports the determined of this test, wherever pytest runs.
    harness = str(pathlib.Path(tensorboard.__file__).parents[2])
    env = {**os.environ, "PYTHONPATH": harness}
    subprocess.run([sys.executable, "-c", launcher], check=True, timeout=60, env=env)
    assert json.loads(out.read_text()) == {"argv": ["/tmp/logs"], "prefix": "/proxy/task-1/"}


def ready_pipe(data: bytes, close: bool) -> int:
    ready, ready_w = os.pipe()
    os.write(ready_w, data)
    if close:
        os.close(ready_w)
    return ready


@pytest.mark.parametrize(
    "data,close,expected",
    [
        (b"tensorboard: http ready\n", False, True),
        (b"tensorboard: http ready\n", True, True),
        (b"tensorboard: http", False, False),
        (b"", False, False),
    ],
)
def test_check_service_ready(data: bytes, close: bool, expected: bool) -> None:
    p = mock.Mock(spec=subprocess.Popen)
    with mock.patch.object(tensorboard, "TICK_INTERVAL", 0.01):
        assert tensorboard.check_service_ready(p, ready_pipe(data, close), bytearray()) is expected
    p.wait.assert_not_called()


def test_check_service_ready_joins_what_it_received() -> None:
    p = mock.Mock(spec=subprocess.Popen)
    received = bytearray(b"tensorboard: http")
    with mock.patch.object(tensorboard, "TICK_INTERVAL", 0.01):
        assert tensorboard.check_service_ready(p, ready_pipe(b" ready\n", False), received)


def test_check_service_ready_fails_when_the_program_closes_the_pipe() -> None:
    # The program closes the pipe without the line once TensorBoard answers without the token.
    p = mock.Mock(spec=subprocess.Popen)
    p.wait.side_effect = subprocess.TimeoutExpired("tensorboard", 0.01)
    with mock.patch.object(tensorboard, "TICK_INTERVAL", 0.01):
        with pytest.raises(RuntimeError, match="without the token"):
            tensorboard.check_service_ready(p, ready_pipe(b"", True), bytearray())


def test_check_service_ready_fails_when_the_program_exits() -> None:
    p = mock.Mock(spec=subprocess.Popen)
    p.wait.return_value = 127
    p.poll.return_value = 127
    with mock.patch.object(tensorboard, "TICK_INTERVAL", 0.01):
        with pytest.raises(RuntimeError, match=r"died, exit code\(127\)"):
            tensorboard.check_service_ready(p, ready_pipe(b"", True), bytearray())


@pytest.mark.parametrize(
    "token,program,expected",
    [("t", True, True), ("", True, False), (None, True, False), ("t", False, False)],
)
def test_check_launch(
    tmp_path: pathlib.Path, token: Optional[str], program: bool, expected: bool
) -> None:
    path = tmp_path / "tensorboard_service.py"
    if program:
        path.write_text("")
    environ = {} if token is None else {"DET_TENSORBOARD_TOKEN": token}
    with mock.patch.object(tensorboard, "SERVICE_PROGRAM", str(path)):
        assert tensorboard.check_launch(environ) is expected
