import contextlib
from typing import Generator, List, Optional, Sequence

import pytest

import tests.config as conf
from determined.common import api
from determined.common.api import bindings, errors
from tests import api_utils, detproc
from tests.cluster import test_rbac, test_workspace_org
from tests.experiment import noop

DEFAULT_WID = 1  # default workspace ID


@contextlib.contextmanager
def setup_shells(
    session: api.Session, shells: List[bindings.v1LaunchShellRequest]
) -> Generator[List[bindings.v1Shell], None, None]:
    created: List[bindings.v1Shell] = []
    try:
        for sh_req in shells:
            r = bindings.post_LaunchShell(session, body=sh_req)
            created.append(r.shell)
        yield created

    finally:
        for sh in created:
            bindings.post_KillShell(session, shellId=sh.id)


def filter_out_ntsc(
    base: Sequence[bindings.v1Shell], target: Sequence[bindings.v1Shell]
) -> List[bindings.v1Shell]:
    """throw out target shells that are not in the base list"""
    accepted_ids = {sh.id for sh in base}
    return list(filter(lambda sh: sh.id in accepted_ids, target))


@pytest.mark.e2e_cpu_rbac
@api_utils.skipif_rbac_not_enabled()
def test_shell() -> None:
    u_viewer_ws0, _ = api_utils.create_test_user()
    u_editor_ws0, _ = api_utils.create_test_user()
    admin = api_utils.admin_session()

    with test_workspace_org.setup_workspaces(count=2) as workspaces:
        api_utils.assign_user_role(
            session=admin,
            user=u_viewer_ws0.username,
            role="Viewer",
            workspace=workspaces[0].name,
        )
        api_utils.assign_user_role(
            session=admin,
            user=u_editor_ws0.username,
            role="Editor",
            workspace=workspaces[1].name,
        )

        sh_reqs = [
            bindings.v1LaunchShellRequest(workspaceId=workspaces[0].id),
            bindings.v1LaunchShellRequest(workspaceId=workspaces[1].id),
            bindings.v1LaunchShellRequest(),
        ]
        with setup_shells(admin, sh_reqs) as shells:
            r = bindings.get_GetShells(admin)
            assert len(filter_out_ntsc(shells, r.shells)) == 3
            r = bindings.get_GetShells(admin, workspaceId=workspaces[0].id)
            assert len(filter_out_ntsc(shells, r.shells)) == 1
            r = bindings.get_GetShells(admin, workspaceId=workspaces[1].id)
            assert len(filter_out_ntsc(shells, r.shells)) == 1
            r = bindings.get_GetShells(admin, workspaceId=DEFAULT_WID)
            assert len(filter_out_ntsc(shells, r.shells)) == 1

            r = bindings.get_GetShells(u_viewer_ws0)
            assert len(r.shells) == 1
            r = bindings.get_GetShells(u_viewer_ws0, workspaceId=workspaces[0].id)
            assert len(r.shells) == 1
            with pytest.raises(errors.APIException) as e:
                r = bindings.get_GetShells(u_viewer_ws0, workspaceId=workspaces[1].id)
                assert e.value.status_code == 404

            # User with only view role on first workspace
            json_out = detproc.check_json(u_viewer_ws0, ["det", "shell", "ls", "--all", "--json"])
            assert len(json_out) == 1


@pytest.mark.e2e_cpu_rbac
@api_utils.skipif_rbac_not_enabled()
def test_ntsc_iface_access() -> None:
    def can_access_logs(sess: api.Session, ntsc_id: str) -> bool:
        try:
            list(bindings.get_TaskLogs(sess, taskId=ntsc_id))
            return True
        except errors.APIException as e:
            if e.status_code != 404 and "not found" not in e.message:
                # FIXME: the endpoint should respond with 404.
                raise e
            return False

    with test_rbac.create_workspaces_with_users(
        [
            [
                (0, ["Viewer", "Editor"]),
                (1, ["Viewer"]),
            ],
            [
                (2, ["Viewer"]),
                (0, ["Viewer"]),
            ],
            [
                (0, ["Editor"]),
            ],
        ]
    ) as (workspaces, creds):
        # launch one of each ntsc in the first workspace
        for typ in conf.ALL_NTSC:
            created_id = api_utils.launch_ntsc(creds[0], workspaces[0].id, typ).id

            # user 0
            assert can_access_logs(
                creds[0], created_id
            ), f"user 0 should be able to access {typ} logs"
            # user 0 should be able to get details.
            api.get_ntsc_details(creds[0], typ, created_id)
            # user 0 should be able to kill.
            api_utils.kill_ntsc(creds[0], typ, created_id)
            # user 0 should be able to set priority.
            api_utils.set_prio_ntsc(creds[0], typ, created_id, 1)
            # user 0 should be able to launch in workspace 0.
            api_utils.launch_ntsc(creds[0], workspaces[0].id, typ)

            # user 0 should not be able to launch in workspace 1.
            with pytest.raises(errors.ForbiddenException):
                api_utils.launch_ntsc(creds[0], workspaces[1].id, typ)

            # user 1
            assert can_access_logs(
                creds[1], created_id
            ), f"user 1 should be able to access {typ} logs"
            # user 1 should be able to get details.
            api.get_ntsc_details(creds[1], typ, created_id)
            with pytest.raises(errors.ForbiddenException) as fe:
                # user 1 should not be able to kill.
                api_utils.kill_ntsc(creds[1], typ, created_id)
            assert "access denied" in fe.value.message
            with pytest.raises(errors.ForbiddenException) as fe:
                # user 1 should not be able to set priority.
                api_utils.set_prio_ntsc(creds[1], typ, created_id, 1)
            assert "access denied" in fe.value.message
            # user 1 should not be able to launch in workspace 0.
            with pytest.raises(errors.ForbiddenException):
                api_utils.launch_ntsc(creds[1], workspaces[0].id, typ)
            # Don't leak workspace information to a user without access.
            with pytest.raises(errors.NotFoundException):
                api_utils.launch_ntsc(creds[1], workspaces[1].id, typ)

            # user 2
            assert not can_access_logs(
                creds[2], created_id
            ), f"user 2 should not be able to access {typ} logs"
            with pytest.raises(errors.APIException) as e:
                # user 2 should not be able to get details.
                api.get_ntsc_details(creds[2], typ, created_id)
            assert e.value.status_code == 404, f"user 2 should not be able to get details for {typ}"
            with pytest.raises(errors.APIException) as e:
                # user 2 should not be able to kill or know it exists.
                api_utils.kill_ntsc(creds[2], typ, created_id)
            assert e.value.status_code == 404, f"user 2 should not be able to kill {typ}"
            with pytest.raises(errors.APIException) as e:
                # user 2 should not be able to set priority or know it exists.
                api_utils.set_prio_ntsc(creds[2], typ, created_id, 1)
            assert e.value.status_code == 404, f"user 2 should not be able to set priority {typ}"
            with pytest.raises(errors.NotFoundException):
                api_utils.launch_ntsc(creds[2], workspaces[0].id, typ)
            # user 2 has view access to workspace 1 so gets forbidden instead of not found.
            with pytest.raises(errors.ForbiddenException):
                api_utils.launch_ntsc(creds[2], workspaces[1].id, typ)

            # test visibility
            created_id2 = api_utils.launch_ntsc(creds[0], workspaces[2].id, typ).id

            # none of the users should be able to get details
            for cred in [creds[1], creds[2]]:
                if cred == creds[2]:
                    with pytest.raises(errors.APIException) as e:
                        api.get_ntsc_details(cred, typ, created_id2)
                assert e.value.status_code == 404
                results = api_utils.list_ntsc(cred, typ)
                for r in results:
                    if r.id == created_id2:
                        pytest.fail(f"should not be able to see {typ} {r.id} in the list results")
                with pytest.raises(errors.APIException) as e:
                    api_utils.list_ntsc(cred, typ, workspace_id=workspaces[2].id)
                assert e.value.status_code == 404, f"{typ} should fail with 404"
                with pytest.raises(errors.APIException) as e:
                    api_utils.list_ntsc(cred, typ, workspace_id=12532459)
                assert e.value.status_code == 404, f"{typ} should fail with 404"

            # kill the ntsc
            api_utils.kill_ntsc(creds[0], typ, created_id)


def create_project_experiment(sess: api.Session, workspace_id: int) -> int:
    """Create a project in the workspace and a completed experiment in it, for TensorBoard."""
    pid = bindings.post_PostProject(
        sess,
        body=bindings.v1PostProjectRequest(name="test", workspaceId=workspace_id),
        workspaceId=workspace_id,
    ).project.id
    return noop.create_experiment(sess, [noop.Report({"x": 1})], project_id=pid).id


@pytest.mark.e2e_cpu_rbac
@api_utils.skipif_rbac_not_enabled()
def test_ntsc_proxy() -> None:
    def get_proxy(sess: api.Session, task_id: str) -> Optional[errors.APIException]:
        try:
            sess.get(f"proxy/{task_id}/")
            return None
        except errors.APIException as e:
            return e

    with test_rbac.create_workspaces_with_users(
        [
            [
                (0, ["Viewer", "Editor"]),
                (1, ["Viewer"]),
            ],
            [
                (2, ["Viewer"]),
            ],
        ]
    ) as (workspaces, creds):
        # A viewing session's primary service, its TensorBoard, has the task ID as its service ID.
        experiment_id = create_project_experiment(creds[0], workspaces[0].id)
        created_id = api_utils.launch_viewing_session(
            creds[0], workspaces[0].id, experiment_ids=[experiment_id]
        ).id
        try:
            api.wait_for_ntsc_state(
                creds[0],
                api.NTSC_Kind.shell,
                created_id,
                lambda s: s == bindings.taskv1State.RUNNING,
                timeout=300,
            )
            err = api.wait_for_task_ready(api_utils.admin_session(), created_id)
            assert err is None, f"viewing session should be ready {err}"
            assert (
                get_proxy(creds[0], created_id) is None
            ), "user 0 should be able to access the viewing session through proxy"
            assert (
                get_proxy(creds[1], created_id) is None
            ), "user 1 should be able to access the viewing session through proxy"
            view_err = get_proxy(creds[2], created_id)
            assert view_err is not None, "user 2 should not be able to access it through proxy"
            assert view_err.status_code == 404, "user 2 should error out with not found"
        finally:
            api_utils.kill_ntsc(creds[0], api.NTSC_Kind.shell, created_id)


@pytest.mark.e2e_cpu_rbac
@api_utils.skipif_rbac_not_enabled()
def test_shell_jupyter_proxy() -> None:
    # The jupyter service runs code as the shell's owner: only the owner and admins reach it.
    with test_rbac.create_workspaces_with_users(
        [
            [
                (0, ["Viewer", "Editor"]),
                (1, ["Viewer"]),
            ],
            [
                (2, ["Viewer"]),
            ],
        ]
    ) as (workspaces, creds):
        created_id = api_utils.launch_shell_with_jupyter(creds[0], workspaces[0].id).id
        try:
            service = api_utils.wait_for_service_ready(creds[0], created_id, "jupyter")
            assert "JupyterLab" in api_utils.get_jupyter_page(creds[0], created_id)

            # user 1 sees the shell and its services but neither the token nor the service.
            bindings.get_GetTaskServices(creds[1], taskId=created_id)
            with pytest.raises(errors.ForbiddenException):
                bindings.get_GetTaskServiceCredential(
                    creds[1], taskId=created_id, service="jupyter"
                )
            with pytest.raises(errors.APIException) as e:
                creds[1].get(service.url.lstrip("/"))
            assert e.value.status_code == 404, "user 1 should not reach the jupyter service"

            # user 2 does not see the shell.
            with pytest.raises(errors.APIException) as e:
                bindings.get_GetTaskServices(creds[2], taskId=created_id)
            assert e.value.status_code == 404
            with pytest.raises(errors.APIException) as e:
                creds[2].get(service.url.lstrip("/"))
            assert e.value.status_code == 404, "user 2 should not reach the jupyter service"
        finally:
            api_utils.kill_ntsc(creds[0], api.NTSC_Kind.shell, created_id)


@pytest.mark.e2e_cpu_rbac
@api_utils.skipif_rbac_not_enabled()
def test_tsb_listed() -> None:
    with test_rbac.create_workspaces_with_users(
        [
            [
                (0, ["Editor"]),
                (1, ["Viewer"]),
            ],
        ]
    ) as ([workspace], creds):
        editor_sess = creds[0]
        viewer_sess = creds[1]
        experiment_id = create_project_experiment(editor_sess, workspace.id)
        created_id = api_utils.launch_viewing_session(
            editor_sess, workspace.id, experiment_ids=[experiment_id]
        ).id
        try:
            # The viewing session is listed as a shell and by det tensorboard list.
            for sess in [editor_sess, viewer_sess]:
                shells = bindings.get_GetShells(sess, workspaceId=workspace.id).shells
                assert [sh.id for sh in shells] == [created_id], shells
                listed = detproc.check_output(sess, ["det", "tensorboard", "list", "-a", "-q"])
                assert created_id in listed.split(), listed
        finally:
            api_utils.kill_ntsc(editor_sess, api.NTSC_Kind.shell, created_id)


@pytest.mark.e2e_cpu_rbac
@api_utils.skipif_rbac_not_enabled()
def test_tsb_launch_on_trials() -> None:
    with test_rbac.create_workspaces_with_users(
        [
            [
                (0, ["Editor"]),
            ],
            [
                (1, ["Editor"]),
            ],
        ]
    ) as (workspaces, creds):
        pid = bindings.post_PostProject(
            creds[0],
            body=bindings.v1PostProjectRequest(name="test", workspaceId=workspaces[0].id),
            workspaceId=workspaces[0].id,
        ).project.id
        experiment_id = noop.create_experiment(
            api_utils.admin_session(), [noop.Report({"x": 1})], project_id=pid
        ).id

        trials = bindings.get_GetExperimentTrials(creds[0], experimentId=experiment_id).trials
        trial_ids = [t.id for t in trials]
        assert len(trial_ids) == 1, f"we should have 1 trial, but got {trial_ids}"

        created_id = api_utils.launch_viewing_session(
            creds[0], workspaces[0].id, trial_ids=trial_ids
        ).id
        api_utils.kill_ntsc(creds[0], api.NTSC_Kind.shell, created_id)

        # A user who may launch shells but not see the trial's artifacts is refused at launch.
        with pytest.raises(errors.NotFoundException):
            api_utils.launch_viewing_session(creds[1], workspaces[1].id, trial_ids=trial_ids)
        with pytest.raises(errors.NotFoundException):
            api_utils.launch_viewing_session(
                creds[1], workspaces[1].id, experiment_ids=[experiment_id]
            )
