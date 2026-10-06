import datetime
import io
import json
import re
import sys
import threading
import time
from typing import Any, Callable, Dict, List, Tuple
from unittest import mock

import pytest
import responses
from responses import matchers

from determined import core
from determined.common import api
from determined.common.api import errors
from determined.core import _log_shipper

MASTER_URL = "http://test_master:8080"

# Every scenario runs under this limit, so that a shipper that blocks fails the test instead of
# hanging the suite.
SCENARIO_TIMEOUT = 10


@responses.activate
def test_unmanaged_log_shipper_sends_session() -> None:
    # The master accepts POST /task-logs only from a signed-in user who may edit the task, so the
    # unmanaged shipper must send its user's session token with every batch.
    responses.post(
        f"{MASTER_URL}/task-logs",
        match=[matchers.header_matcher({"Authorization": "Bearer user-token"})],
        json="",
    )
    session = api.Session(master=MASTER_URL, username="user", token="user-token", cert=None)
    shipper = core._UnmanagedTrialLogShipper(session=session, trial_id=1, task_id="1.task")

    shipper.start()
    try:
        print("hello from an unmanaged trial")
    finally:
        shipper.close()

    assert len(responses.calls) == 1
    request = responses.calls[0].request
    assert request.headers["Authorization"] == "Bearer user-token"
    # One task per batch and no log IDs, as the master requires.
    assert request.body is not None
    logs = json.loads(request.body)
    assert [log["log"] for log in logs] == ["hello from an unmanaged trial\n"]
    assert {log["task_id"] for log in logs} == {"1.task"}
    assert all("id" not in log for log in logs)


@pytest.fixture
def streams(monkeypatch: pytest.MonkeyPatch) -> Tuple[io.StringIO, io.StringIO]:
    """Stand-ins for the original stdout and stderr, and a quick shipper."""
    monkeypatch.setattr(_log_shipper, "SHIPPER_FLUSH_INTERVAL", 0.05)
    monkeypatch.setattr(_log_shipper, "SHIPPER_CLOSE_TIMEOUT", 2)
    return io.StringIO(), io.StringIO()


def _within(
    seconds: float, scenario: Callable[[], None], streams: Tuple[io.StringIO, io.StringIO]
) -> None:
    """Run the scenario with the stand-in streams; fail if it does not finish in time."""
    failures = []  # type: List[BaseException]

    def target() -> None:
        try:
            scenario()
        except BaseException as e:
            failures.append(e)

    thread = threading.Thread(target=target, daemon=True)
    # Swapped here, not in the fixture: pytest sets its own capture streams for the test call.
    saved = sys.stdout, sys.stderr
    sys.stdout, sys.stderr = streams
    try:
        thread.start()
        thread.join(seconds)
    finally:
        sys.stdout, sys.stderr = saved
    assert not thread.is_alive(), f"the scenario did not finish within {seconds}s"
    if failures:
        raise failures[0]


def _wait_for(condition: Callable[[], object], seconds: float = 5) -> None:
    deadline = time.monotonic() + seconds
    while not condition():
        assert time.monotonic() < deadline, "timed out waiting for the shipper"
        time.sleep(0.01)


def _fake_session() -> mock.Mock:
    return mock.Mock(spec=api.Session)


def _batches(session: mock.Mock) -> List[List[Dict[str, Any]]]:
    return [c.kwargs["json"] for c in session.post.call_args_list]


def _shipped_logs(session: mock.Mock) -> List[str]:
    return [log["log"] for batch in _batches(session) for log in batch]


def _count(err: io.StringIO, text: str) -> int:
    return err.getvalue().count(text)


def _shipper(session: Any, **kwargs: Any) -> core._UnmanagedTrialLogShipper:
    return core._UnmanagedTrialLogShipper(session=session, trial_id=1, task_id="1.task", **kwargs)


def test_post_error_with_room_in_queue(
    streams: Tuple[io.StringIO, io.StringIO], monkeypatch: pytest.MonkeyPatch
) -> None:
    out, err = streams
    monkeypatch.setattr(_log_shipper, "SHIP_QUEUE_MAX_SIZE", 10)
    session = _fake_session()
    session.post.side_effect = errors.MasterNotFoundException("connection refused")
    shipper = _shipper(session)

    def scenario() -> None:
        shipper.start()
        print("first")
        _wait_for(lambda: session.post.call_count >= 1)
        # More writes than the queue holds: the sender must keep taking them.
        for i in range(100):
            print(f"line {i}")
        shipper.close()

    _within(SCENARIO_TIMEOUT, scenario, streams)

    assert out.getvalue() == "first\n" + "".join(f"line {i}\n" for i in range(100))
    assert _count(err, "determined: dropped") == 1
    assert "MasterNotFoundException" in err.getvalue()
    assert session.post.call_count >= 2, "later batches must still be sent"
    assert not any("determined:" in log for log in _shipped_logs(session))


def test_post_error_with_full_queue(
    streams: Tuple[io.StringIO, io.StringIO], monkeypatch: pytest.MonkeyPatch
) -> None:
    out, err = streams
    monkeypatch.setattr(_log_shipper, "SHIP_QUEUE_MAX_SIZE", 10)
    release = threading.Event()

    def post(*args: Any, **kwargs: Any) -> None:
        if session.post.call_count == 1:
            release.wait(SCENARIO_TIMEOUT)
            raise errors.MasterNotFoundException("connection reset")

    session = _fake_session()
    session.post.side_effect = post
    shipper = _shipper(session)

    def scenario() -> None:
        shipper.start()
        print("first")
        # The sender is stuck in its first POST while the writes fill the queue.
        _wait_for(lambda: session.post.call_count == 1)
        for i in range(100):
            print(f"line {i}")
        assert shipper._log_sender._queue.full()
        release.set()
        shipper.close()

    _within(SCENARIO_TIMEOUT, scenario, streams)

    assert out.getvalue() == "first\n" + "".join(f"line {i}\n" for i in range(100))
    assert _count(err, "determined: dropped 1 line of output") == 1
    dropped = re.findall(
        r"determined: sending fell behind, so the master is missing (\d+) lines? of output",
        err.getvalue(),
    )
    assert len(dropped) == 1 and 0 < int(dropped[0]) <= 100
    # The warnings went to the original stderr only, never into the copy for the master.
    assert not any("determined:" in log for log in _shipped_logs(session))
    assert not any(
        "determined:" in item[1] for item in list(shipper._log_sender._queue.queue) if item
    )


def test_post_that_never_returns(
    streams: Tuple[io.StringIO, io.StringIO], monkeypatch: pytest.MonkeyPatch
) -> None:
    out, err = streams
    monkeypatch.setattr(_log_shipper, "SHIPPER_CLOSE_TIMEOUT", 0.2)
    hang = threading.Event()
    session = _fake_session()
    session.post.side_effect = lambda *args, **kwargs: hang.wait()
    shipper = _shipper(session)

    def scenario() -> None:
        shipper.start()
        print("first")
        _wait_for(lambda: session.post.call_count == 1)
        for i in range(10000):
            print(f"line {i}")
        start = time.monotonic()
        shipper.close()
        assert time.monotonic() - start < 2

    try:
        _within(SCENARIO_TIMEOUT, scenario, streams)
    finally:
        hang.set()

    assert out.getvalue().count("\n") == 10001
    assert _count(err, "determined: gave up after 0.2s") == 1
    # The request itself has a finite timeout.
    timeout = session.post.call_args.kwargs["timeout"]
    assert timeout == _log_shipper.SHIPPER_POST_TIMEOUT and 0 < timeout < 600


@pytest.mark.parametrize("status", [401, 403])
@responses.activate
def test_auth_error_stops_forwarding(status: int, streams: Tuple[io.StringIO, io.StringIO]) -> None:
    out, err = streams
    responses.post(f"{MASTER_URL}/task-logs", status=status, json={"message": "no"})
    session = api.Session(master=MASTER_URL, username="user", token="expired", cert=None)
    shipper = _shipper(session)

    def scenario() -> None:
        shipper.start()
        print("first")
        _wait_for(lambda: len(responses.calls) == 1)
        print("second")
        time.sleep(10 * _log_shipper.SHIPPER_FLUSH_INTERVAL)
        shipper.close()

    _within(SCENARIO_TIMEOUT, scenario, streams)

    assert out.getvalue() == "first\nsecond\n"
    assert len(responses.calls) == 1, "no retries with dead credentials"
    assert _count(err, "determined: stopped sending output to the master") == 1


@responses.activate
def test_server_error_drops_one_batch(streams: Tuple[io.StringIO, io.StringIO]) -> None:
    out, err = streams
    # Responses are used in the order they were added.
    responses.post(f"{MASTER_URL}/task-logs", status=500, body="database is down")
    responses.post(f"{MASTER_URL}/task-logs", json="")
    session = api.Session(master=MASTER_URL, username="user", token="token", cert=None)
    shipper = _shipper(session)

    def scenario() -> None:
        shipper.start()
        print("first")
        _wait_for(lambda: len(responses.calls) == 1)
        print("second")
        shipper.close()

    _within(SCENARIO_TIMEOUT, scenario, streams)

    assert out.getvalue() == "first\nsecond\n"
    assert len(responses.calls) == 2
    body = responses.calls[1].request.body
    assert body is not None
    assert [log["log"] for log in json.loads(body)] == ["second\n"]
    assert _count(err, "determined: dropped 1 line of output") == 1
    assert "HTTP 500" in err.getvalue()


def test_one_write_ships_in_batches(streams: Tuple[io.StringIO, io.StringIO]) -> None:
    session = _fake_session()
    shipper = _shipper(session)
    lines = [f"line {i}\n" for i in range(2501)]

    def scenario() -> None:
        shipper.start()
        sys.stdout.write("".join(lines))
        shipper.close()

    _within(SCENARIO_TIMEOUT, scenario, streams)

    assert [len(batch) for batch in _batches(session)] == [1000, 1000, 501]
    assert _shipped_logs(session) == lines


def test_output_without_newline(
    streams: Tuple[io.StringIO, io.StringIO], monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(_log_shipper, "LOG_LINE_MAX_SIZE", 10)
    session = _fake_session()
    shipper = _shipper(session)

    def scenario() -> None:
        shipper.start()
        sys.stdout.write("x" * 25)
        sys.stdout.write("y" * 3)
        _wait_for(lambda: len(_shipped_logs(session)) == 2)
        assert len(shipper._log_sender._buf) < 10
        shipper.close()

    _within(SCENARIO_TIMEOUT, scenario, streams)

    # Held-back output goes out in pieces of at most LOG_LINE_MAX_SIZE, the rest on close.
    assert _shipped_logs(session) == ["x" * 10, "x" * 10, "x" * 5 + "y" * 3]


def test_lines_have_their_own_timestamps(streams: Tuple[io.StringIO, io.StringIO]) -> None:
    session = _fake_session()
    shipper = _shipper(session)

    def scenario() -> None:
        shipper.start()
        print("early")
        time.sleep(0.1)
        print("late")
        shipper.close()

    _within(SCENARIO_TIMEOUT, scenario, streams)

    logs = [log for batch in _batches(session) for log in batch]
    assert [log["log"] for log in logs] == ["early\n", "late\n"]
    early, late = (datetime.datetime.fromisoformat(log["timestamp"]) for log in logs)
    assert early.tzinfo is not None
    assert late - early >= datetime.timedelta(seconds=0.1)


@responses.activate
def test_rank_is_sent_as_rank_id(streams: Tuple[io.StringIO, io.StringIO]) -> None:
    responses.post(f"{MASTER_URL}/task-logs", json="")
    session = api.Session(master=MASTER_URL, username="user", token="token", cert=None)
    distributed = core.DummyDistributedContext()
    distributed.rank = 2
    shipper = _shipper(session, distributed=distributed)

    def scenario() -> None:
        shipper.start()
        print("from rank 2")
        shipper.close()

    _within(SCENARIO_TIMEOUT, scenario, streams)

    assert len(responses.calls) == 1
    body = responses.calls[0].request.body
    assert body is not None
    (log,) = json.loads(body)
    # model.TaskLog decodes the rank from "rank_id", as an integer.
    assert log["rank_id"] == 2 and type(log["rank_id"]) is int
    assert "rank" not in log
