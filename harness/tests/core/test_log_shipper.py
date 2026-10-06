import json

import responses
from responses import matchers

from determined import core
from determined.common import api

MASTER_URL = "http://test_master:8080"


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
