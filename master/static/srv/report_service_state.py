"""
report_service_state.py reports the state of a service of an interactive task to the master, once
and with a timeout. The interactive task supervisor runs it for each state of each service and
again periodically. On failure it prints one line and exits 1.
"""

import argparse
import os
import sys

from determined.common import api
from determined.common.api import authentication, certs

TIMEOUT_SECONDS = 5

STATES = ["STARTING", "READY", "EXITED", "UNAVAILABLE"]


def main(service: str, state: str) -> None:
    master_url = api.canonicalize_master_url(os.environ["DET_MASTER"])
    cert = certs.default_load(master_url)
    # This only runs on-cluster, so it is expected the username and session token are present in
    # the environment.
    sess = authentication.login_from_task(master_url, cert=cert).with_retry(0)
    allocation_id = os.environ["DET_ALLOCATION_ID"]
    sess.post(
        f"/api/v1/allocations/{allocation_id}/services/{service}/state",
        json={"state": f"STATE_{state}"},
        timeout=TIMEOUT_SECONDS,
    )


if __name__ == "__main__":
    parser = argparse.ArgumentParser(
        description="Report the state of a service of an interactive task to the master"
    )
    parser.add_argument("service", help="the name of the service")
    parser.add_argument("state", choices=STATES, help="the state of the service")
    args = parser.parse_args()
    try:
        main(args.service, args.state)
    except Exception as e:
        print(f"{type(e).__name__}: {e}".replace("\n", " "), file=sys.stderr)
        sys.exit(1)
