import pytest

from determined.common.api import bindings
from tests import api_utils
from tests import command as cmd


@pytest.mark.slow
@pytest.mark.e2e_cpu
@pytest.mark.e2e_slurm
@pytest.mark.e2e_pbs
def test_basic_shell_jupyter_start_and_kill() -> None:
    sess = api_utils.user_session()
    with cmd.interactive_command(
        sess, ["shell", "start", "--detach", "--service", "jupyter"]
    ) as shell:
        assert shell.task_id is not None
        api_utils.wait_for_service_ready(sess, shell.task_id, "jupyter")


@pytest.mark.e2e_cpu
@pytest.mark.e2e_multi_k8s
def test_shell_jupyter_proxy() -> None:
    sess = api_utils.user_session()
    shell_id = api_utils.launch_shell_with_jupyter(sess, 1).id
    try:
        assert "JupyterLab" in api_utils.get_jupyter_page(sess, shell_id)
    finally:
        bindings.post_KillShell(sess, shellId=shell_id)
