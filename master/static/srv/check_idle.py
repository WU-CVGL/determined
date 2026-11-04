import enum
import logging
import os
import socket
import time
from typing import Optional

import requests
from urllib3.util import Timeout

from determined.common import api
from determined.common.api import authentication, certs


class IdleType(enum.Enum):
    KERNELS_OR_TERMINALS = 1
    KERNEL_CONNECTIONS = 2
    ACTIVITY = 3


REPORT_IDLE_INTERVAL = 30
# The check reports at a quarter of the task's idle timeout, but not more often than this.
MIN_REPORT_INTERVAL = 1
# The check gives up on a JupyterLab request after at most this long.
JUPYTER_TIMEOUT_SECONDS = 3
MASTER_TIMEOUT_SECONDS = 10

last_activity = None


def wait_for_jupyter(addr):
    """
    Avoid logging enormous stacktraces when the requests library attempts to connect to a server
    that isn't accepting connections yet.  This is expected as jupyter startup time might take
    longer than a second, and we don't want to generate scary logs for expected behavior.
    """
    i = 0
    while True:
        with socket.socket() as s:
            try:
                s.connect(addr)
                # Connection worked, we're done here.
                return
            except ConnectionError:
                if (i + 1) % 10 == 0:
                    # Every 10 seconds without reaching jupyter, start telling the user.
                    # This is beyond the range of expected startup times.
                    logging.warning(f"jupyter is still not reachable at {addr}")
            time.sleep(1)
            i += 1


class Timing:
    """How often a round of checks starts and how long its requests may take, in seconds."""

    def __init__(
        self,
        interval: float = REPORT_IDLE_INTERVAL,
        jupyter: float = JUPYTER_TIMEOUT_SECONDS,
        master: float = MASTER_TIMEOUT_SECONDS,
    ):
        self.interval = interval
        self.jupyter = jupyter
        self.master = master


def report_timing(idle_timeout: Optional[str]) -> Timing:
    """
    The timing for the task's idle timeout T in seconds: a round of checks starts every T/4, at
    least every second and at most every 30 s; a JupyterLab request times out after T/4, at most
    3 s, and the report to the master, which is not retried, after T/4, at most 10 s. Each of these
    timeouts bounds its request from connecting to the answer, not each phase of it. A round starts
    at most an interval after the previous report, since a report takes at most an interval, and it
    reports a busy or running kernel after one request: two such reports are at most interval +
    request + report <= 3T/4 apart, for any T of at least 4 s. Activity found in the terminals
    takes a second request, so at most T. At the shortest timeout, 10 s, each of these is 2.5 s, so
    a JupyterLab that answers within 2 s is reported in time.
    """
    timing = Timing()
    if idle_timeout is None:
        return timing
    try:
        seconds = float(idle_timeout)
    except ValueError:
        seconds = 0
    if not seconds > 0:
        logging.warning("invalid idle timeout '%s', ignoring it", idle_timeout)
        return timing
    quarter = seconds / 4
    timing.interval = min(REPORT_IDLE_INTERVAL, max(MIN_REPORT_INTERVAL, quarter))
    timing.jupyter = min(JUPYTER_TIMEOUT_SECONDS, quarter)
    timing.master = min(MASTER_TIMEOUT_SECONDS, quarter)
    return timing


class Unreadable(Exception):
    pass


def is_idle(
    request_address: str, token: str, mode: IdleType, timeout: float
) -> Optional[bool]:
    """
    Whether JupyterLab is idle, or None when its state cannot be read within timeout. It reads only
    what the mode needs and decides as soon as it can.
    """
    auth_header = {"Authorization": f"token {token}"}
    answers = {}

    def get(path: str):
        if path not in answers:
            try:
                response = requests.get(
                    f"{request_address}{path}",
                    headers=auth_header,
                    verify=False,
                    timeout=Timeout(total=timeout),
                )
                # An error body is not a list of kernels.
                response.raise_for_status()
                answers[path] = response.json()
            except Exception as e:
                raise Unreadable(path) from e
        return answers[path]

    try:
        if mode == IdleType.KERNELS_OR_TERMINALS:
            # A session has a kernel, so the sessions are not read.
            return not (get("/api/kernels") or get("/api/terminals"))
        elif mode == IdleType.KERNEL_CONNECTIONS:
            # Unfortunately, the terminals API doesn't return a connection count.
            return all(k["connections"] == 0 for k in get("/api/kernels"))
        elif mode == IdleType.ACTIVITY:
            global last_activity

            kernels = get("/api/kernels")
            no_busy_kernels = all(k["execution_state"] != "busy" for k in kernels)
            if not no_busy_kernels:
                return False
            terminals = get("/api/terminals")
            old_last_activity = last_activity
            if kernels or terminals:
                last_activity = max(x["last_activity"] for x in kernels + terminals)

            return no_busy_kernels and (last_activity == old_last_activity)
    except Unreadable:
        logging.warning("Cannot get notebook kernel status", exc_info=True)
        return None


def main():
    requests.packages.urllib3.disable_warnings()
    port = os.environ["NOTEBOOK_PORT"]
    token = os.environ["DET_NOTEBOOK_TOKEN"]
    # JupyterLab serves under the base URL that the master sets for the jupyter service.
    base_url = os.environ["NOTEBOOK_BASE_URL"]
    notebook_server = f"https://127.0.0.1:{port}{base_url.rstrip('/')}"
    timing = report_timing(os.environ.get("DET_IDLE_TIMEOUT_SECONDS"))
    master_url = api.canonicalize_master_url(os.environ["DET_MASTER"])
    cert = certs.default_load(master_url)
    # A retried report would come later than its timeout; the next round reports again.
    sess = authentication.login_from_task(master_url, cert=cert).with_retry(0)
    try:
        idle_type = IdleType[os.environ["NOTEBOOK_IDLE_TYPE"].upper()]
    except KeyError:
        logging.warning(
            "unknown idle type '%s', using default value",
            os.environ["NOTEBOOK_IDLE_TYPE"],
        )
        idle_type = IdleType.KERNELS_OR_TERMINALS

    wait_for_jupyter(("127.0.0.1", int(port)))

    while True:
        # The interval counts from the start of the round, so that its requests take part of it.
        start = time.monotonic()
        try:
            idle = is_idle(notebook_server, token, idle_type, timing.jupyter)
            # Only a busy JupyterLab is reported to the task's activity record: an unreadable one
            # is not activity.
            if idle is False:
                sess.post(
                    f"/api/v1/allocations/{os.environ['DET_ALLOCATION_ID']}/activity",
                    json={"active": True},
                    timeout=Timeout(total=timing.master),
                )
        except Exception:
            logging.warning("ignoring error communicating with master", exc_info=True)
        time.sleep(max(0.0, timing.interval - (time.monotonic() - start)))


if __name__ == "__main__":
    main()
