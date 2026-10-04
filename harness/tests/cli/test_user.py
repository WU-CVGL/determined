import contextlib
from typing import Iterator
from unittest import mock

import pytest
import responses
from responses import matchers, registries

from determined.cli import cli
from determined.common import api
from determined.common.api import bindings
from tests.cli import util


@contextlib.contextmanager
def relogin_cli_rsps(new_token: str) -> Iterator[responses.RequestsMock]:
    """Like util.standard_cli_rsps, but also expects the CLI to store a new token for det-user."""
    with contextlib.ExitStack() as es:
        es.enter_context(util.setenv_optional("DET_USER", "det-user"))
        es.enter_context(util.setenv_optional("DET_USER_TOKEN", "det-token"))
        mts = es.enter_context(util.MockTokenStore(strict=False))
        mts.get_active_user(retval="det-user")
        mts.get_token("det-user", retval="det-token")
        mts.set_token("det-user", new_token)
        mts.set_active("det-user")
        rsps = es.enter_context(
            responses.RequestsMock(
                registry=registries.OrderedRegistry, assert_all_requests_are_fired=True
            )
        )
        util.expect_get_info(rsps)
        rsps.get("http://localhost:8080/api/v1/me", status=200)
        yield rsps


def test_user_create_password_flag_does_not_prompt() -> None:
    with util.standard_cli_rsps() as rsps:
        userobj = bindings.v1User(active=True, admin=True, username="det-user", id=10017)
        rsps.post(
            "http://localhost:8080/api/v1/users",
            status=200,
            match=[
                matchers.json_params_matcher(
                    params={
                        "isHashed": True,
                        "user": {
                            "username": "test-user-1",
                        },
                        "password": api.salt_and_hash("5DCAB140-f49b-4260-a451-fad6a10017ca"),
                    },
                    strict_match=False,
                ),
            ],
            json={"user": userobj.to_json()},  # doesn't really match, but doesn't matter yet
        )
        cli.main(
            ["user", "create", "test-user-1", "--password", "5DCAB140-f49b-4260-a451-fad6a10017ca"]
        )


def test_user_create_remote_account_does_not_require_password() -> None:
    with util.standard_cli_rsps() as rsps:
        userobj = bindings.v1User(active=True, admin=True, username="det-user", id=10018)
        rsps.post(
            "http://localhost:8080/api/v1/users",
            status=200,
            match=[
                matchers.json_params_matcher(
                    params={
                        "isHashed": True,
                        "user": {
                            "username": "test-user-2",
                            "remote": True,
                        },
                    },
                    strict_match=False,
                ),
            ],
            json={"user": userobj.to_json()},
        )
        cli.main(["user", "create", "test-user-2", "--remote"])


@mock.patch("getpass.getpass")
def test_user_create_interactive_password(mock_getpass: mock.MagicMock) -> None:
    mock_getpass.side_effect = lambda *_: "8CBAAB59-21c5-45cb-b058-6e2f3ceaf03e"
    with util.standard_cli_rsps() as rsps:
        userobj = bindings.v1User(active=True, admin=True, username="det-user", id=10019)
        rsps.post(
            "http://localhost:8080/api/v1/users",
            status=200,
            match=[
                matchers.json_params_matcher(
                    params={
                        "isHashed": True,
                        "user": {
                            "username": "test-user-3",
                        },
                        "password": api.salt_and_hash("8CBAAB59-21c5-45cb-b058-6e2f3ceaf03e"),
                    },
                    strict_match=False,
                ),
            ],
            json={"user": userobj.to_json()},  # doesn't really match, but doesn't matter yet
        )
        cli.main(["user", "create", "test-user-3"])


@mock.patch("getpass.getpass")
def test_user_create_fails_with_empty_password(mock_getpass: mock.MagicMock) -> None:
    mock_getpass.side_effect = lambda *_: ""
    with pytest.raises(SystemExit):
        cli.main(["user", "create", "test-user-4"])


@mock.patch("getpass.getpass")
def test_user_change_password(mock_getpass: mock.MagicMock) -> None:
    mock_getpass.side_effect = lambda *_: "ce93AA76-2f62-4f29-ab5d-c56a3375e702"
    with util.standard_cli_rsps() as rsps:
        userobj = bindings.v1User(active=True, admin=False, username="det-user", id=101)
        rsps.get(
            "http://localhost:8080/api/v1/users/tgt-user/by-username",
            status=200,
            json={"user": userobj.to_json()},
        )

        patchobj = bindings.v1PatchUser(
            isHashed=True, password=api.salt_and_hash("ce93AA76-2f62-4f29-ab5d-c56a3375e702")
        )
        rsps.patch(
            "http://localhost:8080/api/v1/users/101",
            status=200,
            match=[
                matchers.json_params_matcher(patchobj.to_json(True)),
            ],
            json={"user": userobj.to_json()},
        )

        cli.main(["user", "change-password", "tgt-user"])

    # Changing another user's password does not ask for the current password.
    assert mock_getpass.call_count == 2


@mock.patch("getpass.getpass")
def test_user_change_own_password_sends_current_password(mock_getpass: mock.MagicMock) -> None:
    current, new = "9d1Cc0a1-current-password", "7F0e8b2D-new-password"
    mock_getpass.side_effect = [current, new, new]
    with relogin_cli_rsps("new-token") as rsps:
        userobj = bindings.v1User(active=True, admin=False, username="det-user", id=104)
        rsps.get(
            "http://localhost:8080/api/v1/users/det-user/by-username",
            status=200,
            json={"user": userobj.to_json()},
        )
        patchobj = bindings.v1PatchUser(
            isHashed=True,
            password=api.salt_and_hash(new),
            oldPassword=api.salt_and_hash(current),
        )
        rsps.patch(
            "http://localhost:8080/api/v1/users/104",
            status=200,
            match=[matchers.json_params_matcher(patchobj.to_json(True))],
            json={"user": userobj.to_json()},
        )
        # Changing the password ends the user's sessions, so the CLI signs in again.
        rsps.post(
            "http://localhost:8080/api/v1/auth/login",
            status=200,
            match=[
                matchers.json_params_matcher(
                    {"username": "det-user", "password": api.salt_and_hash(new)},
                    strict_match=False,
                )
            ],
            json={"token": "new-token", "user": userobj.to_json()},
        )

        cli.main(["user", "change-password"])

    prompts = [c.args[0] for c in mock_getpass.call_args_list]
    assert prompts[0] == "Current password for user 'det-user': "


@mock.patch("getpass.getpass")
def test_user_change_own_password_blank_current_password(mock_getpass: mock.MagicMock) -> None:
    # Users without a password confirm an empty current password.
    new = "2a7B0c4E-new-password"
    mock_getpass.side_effect = ["", new, new]
    with relogin_cli_rsps("new-token") as rsps:
        userobj = bindings.v1User(active=True, admin=False, username="det-user", id=105)
        rsps.get(
            "http://localhost:8080/api/v1/users/det-user/by-username",
            status=200,
            json={"user": userobj.to_json()},
        )
        rsps.patch(
            "http://localhost:8080/api/v1/users/105",
            status=200,
            match=[
                matchers.json_params_matcher(
                    {"isHashed": True, "password": api.salt_and_hash(new), "oldPassword": ""}
                )
            ],
            json={"user": userobj.to_json()},
        )
        rsps.post(
            "http://localhost:8080/api/v1/auth/login",
            status=200,
            json={"token": "new-token", "user": userobj.to_json()},
        )

        cli.main(["user", "change-password", "det-user"])


@mock.patch("getpass.getpass")
def test_user_change_own_password_wrong_current_password(
    mock_getpass: mock.MagicMock, capsys: pytest.CaptureFixture
) -> None:
    new = "5c3D9e1F-new-password"
    mock_getpass.side_effect = ["wrong", new, new]
    with util.standard_cli_rsps() as rsps:
        userobj = bindings.v1User(active=True, admin=False, username="det-user", id=106)
        rsps.get(
            "http://localhost:8080/api/v1/users/det-user/by-username",
            status=200,
            json={"user": userobj.to_json()},
        )
        rsps.patch(
            "http://localhost:8080/api/v1/users/106",
            status=403,
            json={"error": {"error": "the current password is incorrect"}},
        )
        with pytest.raises(SystemExit):
            cli.main(["user", "change-password"])
    assert "The current password is incorrect" in capsys.readouterr().err


@mock.patch("getpass.getpass")
def test_user_edit_own_username_asks_for_current_password(mock_getpass: mock.MagicMock) -> None:
    # Renaming yourself needs your current password, like changing it.
    mock_getpass.side_effect = ["current-password"]
    with util.standard_cli_rsps() as rsps:
        userobj = bindings.v1User(active=True, admin=False, username="det-user", id=107)
        rsps.get(
            "http://localhost:8080/api/v1/users/det-user/by-username",
            status=200,
            json={"user": userobj.to_json()},
        )
        rsps.patch(
            "http://localhost:8080/api/v1/users/107",
            status=200,
            match=[
                matchers.json_params_matcher(
                    {
                        "username": "new-name",
                        "displayName": "New Name",
                        "oldPassword": api.salt_and_hash("current-password"),
                        "isHashed": True,
                    }
                )
            ],
            json={"user": userobj.to_json()},
        )
        cli.main(
            ["user", "edit", "det-user", "--username", "new-name", "--display-name", "New Name"]
        )
    prompts = [c.args[0] for c in mock_getpass.call_args_list]
    assert prompts == ["Current password for user 'det-user': "]


@mock.patch("getpass.getpass")
def test_user_edit_own_username_wrong_current_password(
    mock_getpass: mock.MagicMock, capsys: pytest.CaptureFixture
) -> None:
    mock_getpass.side_effect = ["wrong"]
    with util.standard_cli_rsps() as rsps:
        userobj = bindings.v1User(active=True, admin=False, username="det-user", id=108)
        rsps.get(
            "http://localhost:8080/api/v1/users/det-user/by-username",
            status=200,
            json={"user": userobj.to_json()},
        )
        rsps.patch(
            "http://localhost:8080/api/v1/users/108",
            status=403,
            json={"error": {"error": "the current password is incorrect"}},
        )
        with pytest.raises(SystemExit):
            cli.main(["user", "edit", "det-user", "--username", "new-name"])
    assert "The current password is incorrect" in capsys.readouterr().err


@mock.patch("getpass.getpass")
def test_user_edit_other_username_or_own_display_name_does_not_prompt(
    mock_getpass: mock.MagicMock,
) -> None:
    mock_getpass.side_effect = AssertionError("unexpected password prompt")
    with util.standard_cli_rsps() as rsps:
        other = bindings.v1User(active=True, admin=False, username="tgt-user", id=109)
        rsps.get(
            "http://localhost:8080/api/v1/users/tgt-user/by-username",
            status=200,
            json={"user": other.to_json()},
        )
        rsps.patch(
            "http://localhost:8080/api/v1/users/109",
            status=200,
            match=[matchers.json_params_matcher({"username": "new-name"})],
            json={"user": other.to_json()},
        )
        cli.main(["user", "edit", "tgt-user", "--username", "new-name"])
    with util.standard_cli_rsps() as rsps:
        me = bindings.v1User(active=True, admin=False, username="det-user", id=110)
        rsps.get(
            "http://localhost:8080/api/v1/users/det-user/by-username",
            status=200,
            json={"user": me.to_json()},
        )
        rsps.patch(
            "http://localhost:8080/api/v1/users/110",
            status=200,
            match=[matchers.json_params_matcher({"displayName": "New Name"})],
            json={"user": me.to_json()},
        )
        cli.main(["user", "edit", "det-user", "--display-name", "New Name"])


@mock.patch("getpass.getpass")
def test_user_rename_self_asks_for_current_password(mock_getpass: mock.MagicMock) -> None:
    mock_getpass.side_effect = ["current-password"]
    with util.standard_cli_rsps() as rsps:
        userobj = bindings.v1User(active=True, admin=False, username="det-user", id=111)
        rsps.get(
            "http://localhost:8080/api/v1/users/det-user/by-username",
            status=200,
            json={"user": userobj.to_json()},
        )
        rsps.patch(
            "http://localhost:8080/api/v1/users/111",
            status=200,
            match=[
                matchers.json_params_matcher(
                    {
                        "username": "new-name",
                        "oldPassword": api.salt_and_hash("current-password"),
                        "isHashed": True,
                    }
                )
            ],
            json={"user": userobj.to_json()},
        )
        rsps.get(
            "http://localhost:8080/api/v1/users/111",
            status=200,
            json={"user": userobj.to_json()},
        )
        cli.main(["user", "rename", "det-user", "new-name"])


@mock.patch("getpass.getpass")
def test_user_change_password_blank_fails(mock_getpass: mock.MagicMock) -> None:
    mock_getpass.side_effect = lambda *_: ""
    with util.standard_cli_rsps() as rsps:
        userobj = bindings.v1User(active=True, admin=False, username="det-user", id=102)
        rsps.get(
            "http://localhost:8080/api/v1/users/tgt-user/by-username",
            status=200,
            json={"user": userobj.to_json()},
        )
        with pytest.raises(SystemExit):
            cli.main(["user", "change-password", "tgt-user"])


@mock.patch("getpass.getpass")
def test_user_change_password_weak_fails(mock_getpass: mock.MagicMock) -> None:
    mock_getpass.side_effect = lambda *_: "password"
    with util.standard_cli_rsps() as rsps:
        userobj = bindings.v1User(active=True, admin=False, username="det-user", id=103)
        rsps.get(
            "http://localhost:8080/api/v1/users/tgt-user/by-username",
            status=200,
            json={"user": userobj.to_json()},
        )
        with pytest.raises(SystemExit):
            cli.main(["user", "change-password", "tgt-user"])


@mock.patch("determined.cli.cli.die")
def test_user_edit_no_fields(mock_die: mock.MagicMock) -> None:
    with util.standard_cli_rsps() as rsps:
        userobj = bindings.v1User(active=True, admin=False, username="det-user", id=101)
        rsps.get(
            "http://localhost:8080/api/v1/users/det-user/by-username",
            status=200,
            json={"user": userobj.to_json()},
        )

        # No edited field should result in error
        cli.main(["user", "edit", "det-user"])
        mock_die.assert_has_calls(
            [mock.call("No field provided. Use 'det user edit -h' for usage.", exit_code=1)]
        )


@responses.activate()
@mock.patch("determined.common.api.authentication.TokenStore", util.MockTokenStore(strict=False))
@mock.patch("getpass.getpass", lambda *_: "newpass")
@mock.patch("determined.cli.cli.die")
def test_login_dies_with_invalid_credentials_error_message(mock_die: mock.MagicMock) -> None:
    util.expect_get_info()
    responses.post(
        "http://localhost:8080/api/v1/auth/login",
        status=401,
    )
    cli.main(["user", "login", "test-user"])
    mock_die.assert_has_calls(
        [
            mock.call(
                "Failed to log in user: Invalid username/password combination. Please try again."
            )
        ]
    )
