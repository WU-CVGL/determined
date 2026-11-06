import contextlib
import logging
import queue
import threading
import time
from typing import Any, Callable, Dict, Generator, Iterator, List, Optional
from unittest import mock

import pytest

from determined.exec import tensorboard
from determined.tensorboard.fetchers import base

TIMEOUT = 10


class FlakyFetcher(base.Fetcher):
    """A fetcher of files whose fetch fails until storage is set."""

    def __init__(self, files: List[str]) -> None:
        self.storage_paths = [""]
        self._file_records = {}  # type: Dict[str, Any]
        self.files = files
        self.storage = threading.Event()
        self.lock = threading.Lock()
        self.fetched = []  # type: List[str]
        self.failures = []  # type: List[threading.Thread]

    def _list(self, storage_path: str) -> Generator[str, None, None]:
        for filepath in self.files:
            if filepath not in self._file_records:
                self._file_records[filepath] = 1
                yield filepath

    def _fetch(self, filepath: str, new_file_callback: Callable) -> None:
        if not self.storage.is_set():
            with self.lock:
                self.failures.append(threading.current_thread())
            raise OSError(f"storage unreachable: {filepath}")
        with self.lock:
            self.fetched.append(filepath)
        new_file_callback()


class RecordingQueue(queue.Queue):
    """A queue that records the threads that put on it."""

    def __init__(self, maxsize: int) -> None:
        super().__init__(maxsize=maxsize)
        self.putters = set()  # type: set

    def put(self, item: Any, block: bool = True, timeout: Optional[float] = None) -> None:
        self.putters.add(threading.current_thread())
        super().put(item, block, timeout)


def wait_for(condition: Callable[[], bool]) -> None:
    deadline = time.monotonic() + TIMEOUT
    while not condition():
        assert time.monotonic() < deadline, "timed out"
        time.sleep(0.01)


@contextlib.contextmanager
def fetch_threads(
    fetcher: base.Fetcher, work_queue: queue.Queue, n: int
) -> Iterator[List[tensorboard.TBFetchThread]]:
    threads = [
        tensorboard.TBFetchThread(
            fetcher=fetcher, work_queue=work_queue, new_file_callback=lambda: None, daemon=True
        )
        for _ in range(n)
    ]
    for t in threads:
        t.start()
    try:
        yield threads
    finally:
        for t in threads:
            t.stop_event.set()
        for t in threads:
            t.join(TIMEOUT)


@pytest.fixture
def fast() -> Iterator[None]:
    with mock.patch.multiple(
        tensorboard, TICK_INTERVAL=0.01, FETCH_RETRY_PAUSE=0.001, FETCH_RETRY_MAX_PAUSE=0.004
    ):
        yield


@pytest.mark.usefixtures("fast")
def test_fetch_threads_resume_after_an_outage_with_a_full_queue() -> None:
    # All fetch threads fail at once while the queue is full; once storage is back, every file is
    # fetched, and no fetch thread puts on the queue (where it would block for good).
    files = [f"f{i}" for i in range(2 * tensorboard.WORK_QUEUE_MAX_SIZE)]
    fetcher = FlakyFetcher(files)
    work_queue = RecordingQueue(maxsize=tensorboard.WORK_QUEUE_MAX_SIZE)

    def produce() -> None:
        for filepath in fetcher.list_all_generator():
            work_queue.put(filepath)

    producer = threading.Thread(target=produce, daemon=True)
    with mock.patch.object(tensorboard, "FETCH_ATTEMPTS", 10**9):
        with fetch_threads(fetcher, work_queue, tensorboard.NUM_FETCH_THREADS) as threads:
            producer.start()
            wait_for(lambda: set(fetcher.failures) == set(threads) and work_queue.full())
            fetcher.storage.set()
            wait_for(lambda: sorted(fetcher.fetched) == sorted(files))
            producer.join(TIMEOUT)
            assert not producer.is_alive()
            assert work_queue.empty()
    assert work_queue.putters == {producer}


@pytest.mark.usefixtures("fast")
def test_a_file_that_fails_every_attempt_is_listed_again(caplog: pytest.LogCaptureFixture) -> None:
    fetcher = FlakyFetcher(["a"])
    work_queue = queue.Queue()  # type: queue.Queue
    caplog.set_level(logging.WARNING, logger="determined")
    with mock.patch.object(tensorboard, "FETCH_ATTEMPTS", 3):
        with fetch_threads(fetcher, work_queue, 1):
            assert list(fetcher.list_all_generator()) == ["a"]
            assert list(fetcher.list_all_generator()) == []
            work_queue.put("a")
            wait_for(lambda: "a" not in fetcher._file_records)
            assert len(fetcher.failures) == 3
            records = [r for r in caplog.records if r.name == "determined"]
            assert [r.levelno for r in records] == [logging.WARNING] * 2 + [logging.ERROR]
            assert records[-1].getMessage() == (
                "Failed to fetch TensorBoard file a: storage unreachable: a"
            )
            assert all(r.exc_info is None for r in records)

            fetcher.storage.set()
            for filepath in fetcher.list_all_generator():
                work_queue.put(filepath)
            wait_for(lambda: fetcher.fetched == ["a"])
    assert len(fetcher.failures) == 3


def test_stop_ends_a_thread_that_pauses_between_attempts() -> None:
    fetcher = FlakyFetcher(["a"])
    work_queue = queue.Queue()  # type: queue.Queue
    work_queue.put("a")
    with mock.patch.multiple(tensorboard, TICK_INTERVAL=0.01, FETCH_RETRY_PAUSE=60):
        with fetch_threads(fetcher, work_queue, 1) as (thread,):
            wait_for(lambda: len(fetcher.failures) == 1)
            start = time.monotonic()
            thread.stop_event.set()
            thread.join(TIMEOUT)
            assert not thread.is_alive()
            assert time.monotonic() - start < 1
    assert len(fetcher.failures) == 1
