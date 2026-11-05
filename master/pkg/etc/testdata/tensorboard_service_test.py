"""
Tests of tensorboard_service.py, the tensorboard unit's program, with the stand-in tensorboard
package of tensorboard_service_test.go, whose directory is TENSORBOARD_STAND_IN, on the
PYTHONPATH with the program's. TestTensorBoardServiceProgram runs them.
"""

import io
import os
import subprocess
import sys
import types
import unittest
from typing import Any, Dict, Iterable, List, Optional, Tuple
from unittest import mock

import tensorboard_service

TOKEN = "test-token"


class Response:
    def __init__(self) -> None:
        self.status: Optional[str] = None
        self.headers: List[Tuple[str, str]] = []

    def start_response(self, status: str, headers: List[Tuple[str, str]]) -> None:
        self.status = status
        self.headers = headers


class App:
    """A WSGI application that records the environment of each request that reaches it."""

    def __init__(self) -> None:
        self.environs: List[Dict[str, Any]] = []

    def __call__(self, environ: Dict[str, Any], start_response: Any) -> Iterable[bytes]:
        self.environs.append(dict(environ))
        start_response("200 OK", [("Content-Length", "2")])
        return [b"ok"]


def request(app: Any, token: Optional[str], **environ: Any) -> Tuple[Response, bytes]:
    env: Dict[str, Any] = {"REQUEST_METHOD": "GET", "PATH_INFO": "/", **environ}
    if token is not None:
        env[tensorboard_service.TOKEN_HEADER] = token
    response = Response()
    body = b"".join(app(env, response.start_response))
    return response, body


class CheckTokenTest(unittest.TestCase):
    def test_refuses(self) -> None:
        # Two headers reach the environment joined by a comma.
        for token in [None, "", "wrong", TOKEN + "x", TOKEN[:-1], TOKEN + "," + TOKEN, "tést"]:
            with self.subTest(token=token):
                app = App()
                response, body = request(
                    tensorboard_service.check_token(app, TOKEN.encode()), token
                )
                self.assertEqual("401 Unauthorized", response.status)
                self.assertEqual(b"Unauthorized\n", body)
                self.assertIn(("Content-Length", str(len(body))), response.headers)
                self.assertEqual([], app.environs, "the request does not reach TensorBoard")

    def test_drains_a_refused_body(self) -> None:
        for length, read in [(10, 10), (1 << 20, tensorboard_service.DRAIN_LIMIT)]:
            with self.subTest(length=length):
                stream = io.BytesIO(b"x" * length)
                checked = tensorboard_service.check_token(App(), TOKEN.encode())
                response, _ = request(
                    checked,
                    None,
                    REQUEST_METHOD="POST",
                    CONTENT_LENGTH=str(length),
                    **{"wsgi.input": stream},
                )
                self.assertEqual("401 Unauthorized", response.status)
                self.assertEqual(read, stream.tell())

    def test_passes_on_without_the_header(self) -> None:
        app = App()
        response, body = request(
            tensorboard_service.check_token(app, TOKEN.encode()), TOKEN, PATH_INFO="/x/"
        )
        self.assertEqual("200 OK", response.status)
        self.assertEqual(b"ok", body)
        self.assertEqual(1, len(app.environs))
        self.assertNotIn(tensorboard_service.TOKEN_HEADER, app.environs[0])
        self.assertEqual("/x/", app.environs[0]["PATH_INFO"])


class ServerFactoryTest(unittest.TestCase):
    def test_default_server_of_the_checked_app(self) -> None:
        for name in ["create_port_scanning_werkzeug_server", "WerkzeugServer"]:
            with self.subTest(name=name):
                made: List[Tuple[Any, Any]] = []

                def server(wsgi_app: Any, flags: Any, made: List[Tuple[Any, Any]] = made) -> str:
                    made.append((wsgi_app, flags))
                    return "server"

                program = types.SimpleNamespace(**{name: server})
                app = App()
                flags = object()
                factory = tensorboard_service.server_factory(program, TOKEN.encode())
                self.assertEqual("server", factory(app, flags))
                self.assertEqual(1, len(made))
                self.assertIs(flags, made[0][1])
                self.assertEqual("401 Unauthorized", request(made[0][0], None)[0].status)
                self.assertEqual("200 OK", request(made[0][0], TOKEN)[0].status)
                self.assertEqual(1, len(app.environs))


class FlagsTest(unittest.TestCase):
    environ = {
        "DET_TENSORBOARD_PORT": "6006",
        "DET_TENSORBOARD_PATH_PREFIX": "/proxy/task-1:6006/",
    }
    flag_names = ["logdir", "port", "path_prefix", "host", "bind_all", "load_fast"]

    def configure(self, args: List[str], bind_all: str) -> Any:
        from tensorboard import default, program

        with mock.patch.dict(os.environ, {"TB_STAND_IN_BIND_ALL": bind_all}):
            tb = program.TensorBoard(plugins=default.get_plugins())
            tb.configure(
                tensorboard_service.tensorboard_argv(args),
                **tensorboard_service.service_flags(self.environ, "/logs"),
            )
        return tb.flags

    def test_service_flags_win(self) -> None:
        # The config refuses these arguments; the service's flags win over them all the same.
        args = ["--load_fast=true", "--port", "1", "--hos=127.0.0.2", "--path_prefix=/x"]
        for bind_all in ["true", "false"]:
            with self.subTest(bind_all=bind_all):
                flags = self.configure(args, bind_all)
                self.assertEqual(
                    {
                        "logdir": "/logs",
                        "port": 6006,
                        "path_prefix": "/proxy/task-1:6006",
                        "host": "0.0.0.0",
                        "bind_all": False,
                        "load_fast": "false",
                    },
                    {k: getattr(flags, k) for k in self.flag_names},
                )

    def test_argv(self) -> None:
        self.assertEqual(["tensorboard"], tensorboard_service.tensorboard_argv([]))
        self.assertEqual(
            ["tensorboard", "--reload_interval=5", ""],
            tensorboard_service.tensorboard_argv(["--reload_interval=5", ""]),
        )

    def test_user_args(self) -> None:
        for value, want in [(None, []), ("", []), ("[]", []), ('["a", "b c"]', ["a", "b c"])]:
            environ = {} if value is None else {"DET_TENSORBOARD_ARGS": value}
            self.assertEqual(want, tensorboard_service.user_args(environ))
        for value in ['["a", 1]', '{"a": "b"}', '"a"', "[", "null"]:
            with self.subTest(value=value):
                with self.assertRaisesRegex(
                    tensorboard_service.Unavailable,
                    "^DET_TENSORBOARD_ARGS is not a JSON array of strings$",
                ):
                    tensorboard_service.user_args({"DET_TENSORBOARD_ARGS": value})

    def test_release(self) -> None:
        self.assertEqual((2, 9), tensorboard_service.release("2.9.0"))
        self.assertEqual((2, 18), tensorboard_service.release("2.18.0"))
        self.assertEqual((10, 0), tensorboard_service.release("10.0a1"))
        for version in ["unknown", "2", ""]:
            with self.assertRaises(tensorboard_service.Unavailable):
                tensorboard_service.release(version)


class MainTest(unittest.TestCase):
    """The program's refusals, each one line and 127, before TensorBoard starts."""

    def run_program(self, *args: str, **env: str) -> "subprocess.CompletedProcess[str]":
        environ = {
            k: v
            for k, v in os.environ.items()
            if not k.startswith("DET_TENSORBOARD_") and not k.startswith("TB_STAND_IN_")
        }
        environ.update(
            {
                "DET_TENSORBOARD_PORT": "6006",
                "DET_TENSORBOARD_PATH_PREFIX": "/proxy/task-1:6006/",
                "DET_TENSORBOARD_TOKEN": TOKEN,
                "ORDER_FILE": os.devnull + "/order",
            }
        )
        environ.update(env)
        return subprocess.run(
            [sys.executable, tensorboard_service.__file__, *args],
            env=environ,
            capture_output=True,
            text=True,
            timeout=30,
        )

    def test_unavailable(self) -> None:
        stand_in = os.environ["TENSORBOARD_STAND_IN"]
        without = os.pathsep.join(
            p for p in os.environ["PYTHONPATH"].split(os.pathsep) if p != stand_in
        )
        cases: List[Tuple[str, List[str], Dict[str, str], str]] = [
            ("no logdir", [], {}, "usage: tensorboard_service.py LOGDIR"),
            (
                "no token",
                ["/logs"],
                {"DET_TENSORBOARD_TOKEN": ""},
                "DET_TENSORBOARD_TOKEN is not set",
            ),
            ("not installed", ["/logs"], {"PYTHONPATH": without}, "tensorboard is not installed"),
            (
                "before 2.9",
                ["/logs"],
                {"TB_STAND_IN_VERSION": "2.8.0"},
                "TensorBoard 2.8.0 is older than 2.9",
            ),
            (
                "no release",
                ["/logs"],
                {"TB_STAND_IN_VERSION": "unknown"},
                'cannot tell the release of TensorBoard: "unknown"',
            ),
            (
                "no server_class",
                ["/logs"],
                {"TB_STAND_IN_NO_SERVER_CLASS": "yes"},
                "TensorBoard 2.9.0 takes no server_class",
            ),
            (
                "args",
                ["/logs"],
                {"DET_TENSORBOARD_ARGS": '["--a", 1]'},
                "DET_TENSORBOARD_ARGS is not a JSON array of strings",
            ),
        ]
        for name, args, env, line in cases:
            with self.subTest(name):
                result = self.run_program(*args, **env)
                self.assertEqual(127, result.returncode, result.stderr)
                self.assertEqual("tensorboard service: " + line + "\n", result.stderr)
                self.assertEqual("", result.stdout)


if __name__ == "__main__":
    unittest.main(verbosity=2)
