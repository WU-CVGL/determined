"""
check_ready_logs.py accepts a task's logs as STDIN, runs a regex to determine and report readiness.
Callers should be aware it may terminate early, and stop reading from STDIN.
"""

import argparse
import os
import re
import sys
import time
from typing import Optional, Pattern

import requests

from determined.common import api
from determined.common.api import authentication, certs, errors

BACKOFF_SECONDS = 5
TIMEOUT_SECONDS = 10


def retryable(e: Exception) -> bool:
    # The session raises MasterNotFoundException for a connection failure, a plain
    # BadRequestException for any other failed request, such as a read timeout, and APIException
    # with the status for an HTTP error; it lets an SSL error through as it is. The other classes
    # it raises, for a 401, 403 or 404, are answers that a retry does not change.
    if isinstance(e, errors.APIException):
        return e.status_code >= 500
    return (
        isinstance(e, (errors.MasterNotFoundException, requests.exceptions.SSLError))
        or type(e) is errors.BadRequestException
    )


def post_ready(sess: api.Session, allocation_id: str, state: str):
    # Since the service is virtually inaccessible by the user unless
    # the call completes, we may as well try forever or just wait for
    # them to kill us.
    while True:
        try:
            sess.post(
                f"/api/v1/allocations/{allocation_id}/{state}", timeout=TIMEOUT_SECONDS
            )
            return
        except Exception as e:
            if not retryable(e):
                raise
            time.sleep(BACKOFF_SECONDS)


def report_ready(report_to: str, service: str) -> None:
    # The interactive task supervisor reads the services' states from the FIFO report_to and posts
    # them. Without a reader, or with a full FIFO, the state is lost rather than waited for.
    try:
        fd = os.open(report_to, os.O_WRONLY | os.O_NONBLOCK)
    except OSError:
        return
    try:
        os.write(fd, f"{service} READY\n".encode())
    except OSError:
        pass
    finally:
        os.close(fd)


def main(
    ready: Pattern,
    waiting: Optional[Pattern] = None,
    service: Optional[str] = None,
    post: bool = True,
    report_to: Optional[str] = None,
):
    sess = None
    allocation_id = ""
    if post:
        master_url = api.canonicalize_master_url(os.environ["DET_MASTER"])
        cert = certs.default_load(master_url)
        # This only runs on-cluster, so it is expected the username and session token are present
        # in the environment.
        sess = authentication.login_from_task(master_url, cert=cert)
        allocation_id = str(os.environ["DET_ALLOCATION_ID"])
    for line in sys.stdin:
        if ready.match(line):
            # The service's state does not wait for the master.
            if service:
                if report_to:
                    report_ready(report_to, service)
                print(f"[{service}] ready", file=sys.stderr, flush=True)
            if sess is not None:
                post_ready(sess, allocation_id, "ready")
            return
        if waiting and waiting.match(line):
            if sess is not None:
                post_ready(sess, allocation_id, "waiting")
            return


if __name__ == "__main__":
    parser = argparse.ArgumentParser(
        description="Read STDIN for a match and mark a task as ready"
    )
    parser.add_argument(
        "--ready-regex", type=str, help="the pattern to match task ready", required=True
    )
    parser.add_argument(
        "--waiting-regex", type=str, help="the pattern to match task waiting"
    )
    parser.add_argument(
        "--service", type=str, help="the service whose readiness the task log gets"
    )
    parser.add_argument(
        "--no-post", action="store_true", help="do not report readiness to the master"
    )
    parser.add_argument(
        "--report-to", type=str, help="the FIFO to write the service's readiness to"
    )
    args = parser.parse_args()

    ready_regex = re.compile(args.ready_regex)
    waiting_regex = re.compile(args.waiting_regex) if args.waiting_regex else None
    main(ready_regex, waiting_regex, args.service, not args.no_post, args.report_to)
