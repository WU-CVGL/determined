import enum
import logging
import os
import socket
import time
from typing import Optional

import requests

from determined.common import api
from determined.common.api import authentication, certs


class IdleType(enum.Enum):
    KERNELS_OR_TERMINALS = 1
    KERNEL_CONNECTIONS = 2
    ACTIVITY = 3


REPORT_IDLE_INTERVAL = 30
# A shell's task reports at a fraction of its idle timeout, but not more often than this.
MIN_REPORT_INTERVAL = 1
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


def report_interval(idle_timeout: Optional[str]) -> float:
    """
    The time from the start of one round of checks to the next: a third of the task's idle timeout
    in seconds, when it is set, so that a busy JupyterLab is reported more often than the timeout.
    """
    if idle_timeout is None:
        return REPORT_IDLE_INTERVAL
    try:
        seconds = float(idle_timeout)
    except ValueError:
        seconds = 0
    if not seconds > 0:
        logging.warning("invalid idle timeout '%s', ignoring it", idle_timeout)
        return REPORT_IDLE_INTERVAL
    return min(REPORT_IDLE_INTERVAL, max(MIN_REPORT_INTERVAL, seconds / 3))


def is_idle(request_address: str, token: str, mode: IdleType) -> Optional[bool]:
    """Whether JupyterLab is idle, or None when its state cannot be read."""
    auth_header = {"Authorization": f"token {token}"}

    def get(path: str):
        return requests.get(
            f"{request_address}{path}",
            headers=auth_header,
            verify=False,
            timeout=JUPYTER_TIMEOUT_SECONDS,
        ).json()

    try:
        kernels = get("/api/kernels")
        terminals = get("/api/terminals")
        sessions = get("/api/sessions")
    except Exception:
        logging.warning("Cannot get notebook kernel status", exc_info=True)
        return None

    if mode == IdleType.KERNELS_OR_TERMINALS:
        return len(kernels) == 0 and len(terminals) == 0 and len(sessions) == 0
    elif mode == IdleType.KERNEL_CONNECTIONS:
        # Unfortunately, the terminals API doesn't return a connection count.
        return all(k["connections"] == 0 for k in kernels)
    elif mode == IdleType.ACTIVITY:
        global last_activity

        old_last_activity = last_activity
        if kernels or terminals:
            last_activity = max(x["last_activity"] for x in kernels + terminals)
        no_busy_kernels = all(k["execution_state"] != "busy" for k in kernels)

        return no_busy_kernels and (last_activity == old_last_activity)


def main():
    requests.packages.urllib3.disable_warnings()
    port = os.environ["NOTEBOOK_PORT"]
    task_id = os.environ["DET_TASK_ID"]
    token = os.environ["DET_NOTEBOOK_TOKEN"]
    # JupyterLab serves under the base URL that the jupyter unit gives it.
    base_url = os.environ.get("NOTEBOOK_BASE_URL") or f"/proxy/{task_id}/"
    notebook_server = f"https://127.0.0.1:{port}{base_url.rstrip('/')}"
    # A shell reports to its task's activity record, a notebook to the notebook API.
    report_task = os.environ.get("DET_ACTIVITY_REPORT") == "task"
    interval = REPORT_IDLE_INTERVAL
    if report_task:
        interval = report_interval(os.environ.get("DET_IDLE_TIMEOUT_SECONDS"))
    master_url = api.canonicalize_master_url(os.environ["DET_MASTER"])
    cert = certs.default_load(master_url)
    sess = authentication.login_from_task(master_url, cert=cert)
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
            idle = is_idle(notebook_server, token, idle_type)
            if report_task:
                # Only a busy JupyterLab is reported: an unreadable one is not activity.
                if idle is False:
                    sess.post(
                        f"/api/v1/allocations/{os.environ['DET_ALLOCATION_ID']}/activity",
                        json={"active": True},
                        timeout=MASTER_TIMEOUT_SECONDS,
                    )
            else:
                sess.put(
                    f"/api/v1/notebooks/{task_id}/report_idle",
                    json={"idle": bool(idle)},
                    timeout=MASTER_TIMEOUT_SECONDS,
                )
        except Exception:
            logging.warning("ignoring error communicating with master", exc_info=True)
        time.sleep(max(0.0, interval - (time.monotonic() - start)))


if __name__ == "__main__":
    main()
