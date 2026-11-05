import contextlib
import json
import os
import pathlib
import subprocess
import sys
from typing import Iterator, Optional
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
    env = tensorboard.service_env(environ, ["--samples_per_plugin=images=100", 'a "b"'], 7)
    assert env == {
        **environ,
        "DET_TENSORBOARD_PORT": "2701",
        "DET_TENSORBOARD_PATH_PREFIX": "/proxy/task-1/",
        "DET_TENSORBOARD_ARGS": '["--samples_per_plugin=images=100", "a \\"b\\""]',
        "DET_TENSORBOARD_READY_FD": "7",
    }
    assert json.loads(tensorboard.service_env(environ, [], 7)["DET_TENSORBOARD_ARGS"]) == []


@pytest.mark.usefixtures("task_env")
def test_start_service_runs_the_program() -> None:
    with mock.patch.object(tensorboard.subprocess, "Popen") as popen:
        process, ready = tensorboard.start_service("/tmp/logs", ["--reload_interval=5"])
    try:
        assert process is popen.return_value
        # The launcher keeps the read end only.
        assert os.read(ready, 1) == b""
    finally:
        os.close(ready)
    popen.assert_called_once()
    args, kwargs = popen.call_args
    assert args == ([sys.executable, tensorboard.SERVICE_PROGRAM, "/tmp/logs"],)
    ready_fd = int(kwargs["env"]["DET_TENSORBOARD_READY_FD"])
    assert kwargs == {
        "env": tensorboard.service_env(os.environ, ["--reload_interval=5"], ready_fd),
        "pass_fds": (ready_fd,),
    }
    assert kwargs["env"]["DET_TENSORBOARD_TOKEN"] == TOKEN


@pytest.mark.skipif(not os.path.isdir("/proc/self/fd"), reason="needs /proc")
@pytest.mark.usefixtures("task_env")
def test_start_service_passes_the_ready_line(tmp_path: pathlib.Path) -> None:
    # The program writes its ready line to the descriptor that the launcher names, which the
    # launcher reads from the pipe; the program gets no other descriptor of the launcher's.
    out = tmp_path / "out.json"
    program = tmp_path / "program.py"
    program.write_text(
        "import json, os, sys, time\n"
        "fd = int(os.environ['DET_TENSORBOARD_READY_FD'])\n"
        "fds = {int(n): os.readlink(f'/proc/self/fd/{n}') for n in os.listdir('/proc/self/fd')\n"
        "       if os.path.exists(f'/proc/self/fd/{n}')}\n"
        "json.dump({'argv': sys.argv[1:], 'prefix': os.environ['DET_TENSORBOARD_PATH_PREFIX'],\n"
        "           'fd': fd, 'fds': fds}, open(" + repr(str(out)) + ", 'w'))\n"
        "time.sleep(1)\n"
        "os.write(fd, b'tensorboard: http')\n"
        "os.write(fd, b' ready\\n')\n"
        "time.sleep(30)\n"
    )
    extra_path = tmp_path / "extra"
    extra = os.open(extra_path, os.O_WRONLY | os.O_CREAT)
    os.set_inheritable(extra, True)
    try:
        with mock.patch.object(tensorboard, "SERVICE_PROGRAM", str(program)):
            p, ready = tensorboard.start_service("/tmp/logs", [])
        try:
            received = bytearray()
            ticks = 0
            while not tensorboard.check_service_ready(p, ready, received):
                ticks += 1
                assert ticks < 30
        finally:
            os.close(ready)
            p.kill()
            p.wait()
    finally:
        os.close(extra)
    seen = json.loads(out.read_text())
    assert seen["argv"] == ["/tmp/logs"]
    assert seen["prefix"] == "/proxy/task-1/"
    fds = {int(fd): target for fd, target in seen["fds"].items()}
    assert fds[seen["fd"]].startswith("pipe:")
    assert set(fds) - {0, 1, 2} == {seen["fd"]}, fds
    assert str(extra_path) not in fds.values()


@contextlib.contextmanager
def ready_pipe(data: bytes, close: bool) -> Iterator[int]:
    """A pipe with data, its write end closed if close; both ends are closed at the end."""
    ready, ready_w = os.pipe()
    try:
        os.write(ready_w, data)
        if close:
            os.close(ready_w)
            ready_w = -1
        yield ready
    finally:
        os.close(ready)
        if ready_w >= 0:
            os.close(ready_w)


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
    with mock.patch.object(tensorboard, "TICK_INTERVAL", 0.01), ready_pipe(data, close) as ready:
        assert tensorboard.check_service_ready(p, ready, bytearray()) is expected
    p.wait.assert_not_called()


def test_check_service_ready_joins_what_it_received() -> None:
    p = mock.Mock(spec=subprocess.Popen)
    received = bytearray(b"tensorboard: http")
    with mock.patch.object(tensorboard, "TICK_INTERVAL", 0.01), ready_pipe(b" ready\n", False) as r:
        assert tensorboard.check_service_ready(p, r, received)


def test_check_service_ready_fails_when_the_program_closes_the_pipe() -> None:
    p = mock.Mock(spec=subprocess.Popen)
    p.wait.side_effect = subprocess.TimeoutExpired("tensorboard", 0.01)
    with mock.patch.object(tensorboard, "TICK_INTERVAL", 0.01), ready_pipe(b"", True) as ready:
        with pytest.raises(
            RuntimeError, match="^TensorBoard closed its readiness channel before reporting ready.$"
        ):
            tensorboard.check_service_ready(p, ready, bytearray())


def test_check_service_ready_fails_when_the_program_exits() -> None:
    p = mock.Mock(spec=subprocess.Popen)
    p.wait.return_value = 127
    p.poll.return_value = 127
    with mock.patch.object(tensorboard, "TICK_INTERVAL", 0.01), ready_pipe(b"", True) as ready:
        with pytest.raises(RuntimeError, match=r"died, exit code\(127\)"):
            tensorboard.check_service_ready(p, ready, bytearray())


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
