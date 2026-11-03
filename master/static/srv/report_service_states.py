"""
report_service_states.py reports the states of the services of an interactive task to the master,
for the interactive task supervisor, which starts it before the services and stops it when they
have stopped.

It reads "<service> <state>" lines from the FIFO that the supervisor and the services' readiness
checks write, and keeps the current snapshot of every service's state. Once every service has a
state, it posts the snapshot (ReportTaskServices) whenever it changes, with a new sequence number,
and again every DET_SERVICE_STATE_REPORT_SECONDS (30), so that a master that restarted learns the
states. After a failed post it posts the current snapshot again with a short backoff; it never
posts a past snapshot. The first failed post after a successful one is logged.

The line "- -" makes it post the current snapshot once more if the master does not have it yet,
and exit. It also exits when the FIFO has no writer left, as when the supervisor is gone.
"""

import os
import select
import sys
import time
from typing import Any, Dict, List, Optional

from determined.common import api
from determined.common.api import authentication, certs

STATES = ("STARTING", "READY", "EXITED", "UNAVAILABLE")
DEFAULT_REPORT_SECONDS = 30
TIMEOUT_SECONDS = 5
FINAL_TIMEOUT_SECONDS = 1
BACKOFF_SECONDS = 1
MAX_BACKOFF_SECONDS = 5


def report_seconds() -> int:
    try:
        seconds = int(os.environ.get("DET_SERVICE_STATE_REPORT_SECONDS", ""))
    except ValueError:
        return DEFAULT_REPORT_SECONDS
    return seconds if seconds > 0 else DEFAULT_REPORT_SECONDS


class Reporter:
    def __init__(self, services: List[str]) -> None:
        self.services = services
        self.states: Dict[str, str] = {}
        # The sequence of the last snapshot posted, and of the last one that the master took;
        # changed says that the current snapshot has not been posted yet and needs a new sequence.
        self.sequence = 0
        self.posted = 0
        self.changed = False
        self.failing = False
        self.session: Optional[api.BaseSession] = None

    def update(self, name: str, state: str) -> bool:
        """Take the state of a service; return whether that made a new snapshot to post."""
        if name not in self.services or state not in STATES or self.states.get(name) == state:
            return False
        self.states[name] = state
        if len(self.states) < len(self.services):
            return False
        self.changed = True
        return True

    def unposted(self) -> bool:
        return self.changed or self.sequence > self.posted

    def post(self, timeout: int) -> bool:
        if self.changed:
            self.sequence += 1
            self.changed = False
        body: Dict[str, Any] = {
            "sequence": self.sequence,
            "services": [
                {"name": name, "state": f"STATE_{self.states[name]}"} for name in self.services
            ],
        }
        try:
            if self.session is None:
                master_url = api.canonicalize_master_url(os.environ["DET_MASTER"])
                cert = certs.default_load(master_url)
                # This only runs on-cluster, so it is expected the username and session token are
                # present in the environment.
                self.session = authentication.login_from_task(master_url, cert=cert).with_retry(0)
            allocation_id = os.environ["DET_ALLOCATION_ID"]
            self.session.post(
                f"/api/v1/allocations/{allocation_id}/services", json=body, timeout=timeout
            )
        except Exception as e:
            if not self.failing:
                self.failing = True
                error = f"{type(e).__name__}: {e}".replace("\n", " ")
                print(
                    f"interactive task: reporting the services' states failed: {error}",
                    file=sys.stderr,
                    flush=True,
                )
            return False
        self.failing = False
        self.posted = self.sequence
        return True


def main(fifo: str, services: List[str]) -> None:
    interval = report_seconds()
    reporter = Reporter(services)
    fd = os.open(fifo, os.O_RDONLY | os.O_NONBLOCK)
    pending = b""
    # When to post the current snapshot; None until there is one.
    due: Optional[float] = None
    backoff = BACKOFF_SECONDS
    while True:
        timeout = None if due is None else max(0.0, due - time.monotonic())
        readable, _, _ = select.select([fd], [], [], timeout)
        if readable:
            try:
                data = os.read(fd, 4096)
            except BlockingIOError:
                continue
            if not data:
                return
            lines = (pending + data).split(b"\n")
            pending = lines.pop()
            for line in lines:
                fields = line.decode(errors="replace").split()
                if fields == ["-", "-"]:
                    if reporter.unposted():
                        reporter.post(FINAL_TIMEOUT_SECONDS)
                    return
                if len(fields) == 2 and reporter.update(fields[0], fields[1]):
                    due = time.monotonic()
                    backoff = BACKOFF_SECONDS
        if due is not None and time.monotonic() >= due:
            if reporter.post(TIMEOUT_SECONDS):
                due = time.monotonic() + interval
                backoff = BACKOFF_SECONDS
            else:
                due = time.monotonic() + backoff
                backoff = min(backoff * 2, MAX_BACKOFF_SECONDS)


if __name__ == "__main__":
    if len(sys.argv) < 3:
        print(f"usage: {sys.argv[0]} FIFO SERVICE...", file=sys.stderr)
        sys.exit(2)
    main(sys.argv[1], sys.argv[2:])
