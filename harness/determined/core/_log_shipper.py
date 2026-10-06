import datetime
import queue
import sys
import threading
import time
import types
from typing import Any, Callable, Dict, List, Optional, Set, TextIO, Tuple

from determined import core
from determined.common import api
from determined.common.api import errors


class _LogShipper:
    def __init__(
        self,
        *,
        session: api.Session,
        trial_id: int,
        task_id: str,
        distributed: Optional[core.DistributedContext] = None,
    ) -> None:
        self._session = session
        self._trial_id = trial_id
        self._task_id = task_id
        self._distributed = distributed

    def start(self) -> "_LogShipper":
        return self

    def close(
        self,
        exc_type: Optional[type],
        exc_val: Optional[BaseException],
        exc_tb: Optional[types.TracebackType],
    ) -> "_LogShipper":
        return self

    def __enter__(self) -> "_LogShipper":
        return self.start()

    def __exit__(
        self,
        exc_type: Optional[type],
        exc_val: Optional[BaseException],
        exc_tb: Optional[types.TracebackType],
    ) -> "_LogShipper":
        return self.close(exc_type, exc_val, exc_tb)


class _ManagedTrialLogShipper(_LogShipper):
    """
    Does nothing: the output of managed trials is shipped by ship_logs.py, which wraps the
    entrypoint of every task container.
    """

    pass


class _Interceptor:
    def __init__(self, original_io: TextIO, handler: Callable[[str], None]) -> None:
        self._original_io = original_io
        self._handler = handler

    def write(self, data: str) -> int:
        # Local output first: the remote copy must never hold it up.
        n = self._original_io.write(data)
        self._handler(data)
        return n

    def flush(self) -> None:
        self._original_io.flush()

    def __getattr__(self, attr: str) -> Any:
        return getattr(self._original_io, attr)


# Seconds between shipments of the lines collected so far.
SHIPPER_FLUSH_INTERVAL = 1
# Seconds one POST /task-logs may take.
SHIPPER_POST_TIMEOUT = 30
# Seconds close() waits for the last lines to be shipped.
SHIPPER_CLOSE_TIMEOUT = 10
# Lines per POST.
LOG_BATCH_MAX_SIZE = 1000
# Characters of output without a newline held back before they are shipped as one line.
LOG_LINE_MAX_SIZE = 16 * 1024
# Writes waiting for the sender. The remote copy of further writes is dropped.
SHIP_QUEUE_MAX_SIZE = 3 * LOG_BATCH_MAX_SIZE


# (time of the write, data), or None, which only wakes the sender up to close.
_QueueElement = Optional[Tuple[float, str]]


class _LogSender(threading.Thread):
    """
    Sends a copy of the output to the master's POST /task-logs from a background thread.

    The copy is best effort, so that local output continues and the program can exit whatever
    the master does. Writers never wait: a write that finds the queue full is left out of the copy.
    A failed POST drops its batch, and 401 or 403 (expired session, revoked permission) ends the
    copy for the rest of the run. Each kind of problem is reported once on the original stderr,
    which the interceptors do not capture.
    """

    def __init__(self, session: api.Session, logs_metadata: Dict, warnings_io: TextIO) -> None:
        self._queue = queue.Queue(maxsize=SHIP_QUEUE_MAX_SIZE)  # type: queue.Queue[_QueueElement]
        self._session = session
        self._logs_metadata = logs_metadata
        self._warnings_io = warnings_io
        self._closing = threading.Event()
        self._disabled = threading.Event()
        self._dropped_lock = threading.Lock()
        self._dropped_writes = 0
        self._dropped_lines = 0
        self._warned = set()  # type: Set[str]
        self._buf = ""
        self._buf_time = 0.0
        self._msgs = []  # type: List[Dict[str, Any]]

        super().__init__(daemon=True, name="LogSenderThread")

    def write(self, data: str) -> None:
        if not data or self._closing.is_set() or self._disabled.is_set():
            return
        try:
            self._queue.put_nowait((time.time(), data))
        except queue.Full:
            with self._dropped_lock:
                self._dropped_writes += 1
                self._dropped_lines += data.count("\n")

    def close(self) -> None:
        self._closing.set()
        try:
            self._queue.put_nowait(None)
        except queue.Full:
            pass  # The sender finds the queue non-empty and sees _closing soon anyway.
        # The only wait for the sender: a POST that hangs must not keep the program from exiting.
        self.join(SHIPPER_CLOSE_TIMEOUT)
        if self.is_alive():
            self._warn(
                f"gave up after {SHIPPER_CLOSE_TIMEOUT}s waiting to send the last output to the "
                "master; the master may not have all of it."
            )
        if self._dropped_writes:
            lines = _lines(self._dropped_lines) if self._dropped_lines else "parts of lines"
            self._warn(
                f"sending fell behind, so the master is missing {lines} of output printed here."
            )

    def run(self) -> None:
        try:
            deadline = time.monotonic() + SHIPPER_FLUSH_INTERVAL
            while not self._closing.is_set() and not self._disabled.is_set():
                try:
                    item = self._queue.get(timeout=max(0.0, deadline - time.monotonic()))
                except queue.Empty:
                    item = None
                if item is not None:
                    self._add(*item)
                if time.monotonic() >= deadline:
                    self._ship()
                    deadline = time.monotonic() + SHIPPER_FLUSH_INTERVAL

            # Closing: ship what is queued, then the last partial line.
            while not self._disabled.is_set():
                try:
                    item = self._queue.get_nowait()
                except queue.Empty:
                    break
                if item is not None:
                    self._add(*item)
            if self._buf:
                self._append(self._buf, self._buf_time)
                self._buf = ""
            self._ship()
        except Exception as e:
            self._disabled.set()
            self._warn(f"stopped sending output to the master after an error: {e!r}")

    def _add(self, t: float, data: str) -> None:
        self._buf += data
        self._buf_time = t
        if "\n" in data:
            lines = self._buf.split("\n")
            self._buf = lines.pop()
            for line in lines:
                self._append(line + "\n", t)
        # Output without newlines, like a progress bar, must not pile up.
        while len(self._buf) >= LOG_LINE_MAX_SIZE:
            self._append(self._buf[:LOG_LINE_MAX_SIZE], t)
            self._buf = self._buf[LOG_LINE_MAX_SIZE:]

    def _append(self, log: str, t: float) -> None:
        msg = dict(self._logs_metadata)
        msg["timestamp"] = datetime.datetime.fromtimestamp(t, datetime.timezone.utc).isoformat()
        msg["log"] = log
        self._msgs.append(msg)
        if len(self._msgs) >= LOG_BATCH_MAX_SIZE:
            self._ship()

    def _ship(self) -> None:
        msgs, self._msgs = self._msgs, []
        if not msgs or self._disabled.is_set():
            return
        try:
            self._session.post("task-logs", json=msgs, timeout=SHIPPER_POST_TIMEOUT)
        except (errors.UnauthenticatedException, errors.ForbiddenException) as e:
            # Retrying with dead credentials cannot help.
            self._disabled.set()
            self._warn(
                f"stopped sending output to the master for the rest of the run ({e}). Output "
                "is still printed here."
            )
        except Exception as e:
            status = getattr(e, "status_code", None)
            kind = type(e).__name__ + (f" (HTTP {status})" if status else "")
            if kind not in self._warned:
                self._warned.add(kind)
                self._warn(
                    f"dropped {_lines(len(msgs))} of output that could not be sent to the master "
                    f"({kind}: {e}). Later output is still sent; this error is not reported again."
                )

    def _warn(self, message: str) -> None:
        try:
            self._warnings_io.write(f"determined: {message}\n")
            self._warnings_io.flush()
        except Exception:
            pass


def _lines(n: int) -> str:
    return "1 line" if n == 1 else f"{n} lines"


class _UnmanagedTrialLogShipper(_LogShipper):
    def start(self) -> "_LogShipper":
        self._original_stdout, self._original_stderr = sys.stdout, sys.stderr

        logs_metadata = {"task_id": self._task_id}  # type: Dict[str, Any]
        if self._distributed:
            logs_metadata["rank_id"] = int(self._distributed.rank)

        self._log_sender = _LogSender(
            session=self._session,
            logs_metadata=logs_metadata,
            warnings_io=self._original_stderr,
        )

        sys.stdout = _Interceptor(sys.stdout, self._log_sender.write)  # type: ignore
        sys.stderr = _Interceptor(sys.stderr, self._log_sender.write)  # type: ignore

        self._log_sender.start()

        return self

    def close(
        self,
        exc_type: Optional[type] = None,
        exc_val: Optional[BaseException] = None,
        exc_tb: Optional[types.TracebackType] = None,
    ) -> "_LogShipper":
        sys.stdout, sys.stderr = self._original_stdout, self._original_stderr
        self._log_sender.close()

        return self
