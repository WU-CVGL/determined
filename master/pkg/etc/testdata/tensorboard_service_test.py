"""
Tests of tensorboard_service.py, the tensorboard unit's program, with the stand-in tensorboard
package of tensorboard_service_test.go, whose directory is TENSORBOARD_STAND_IN, on the
PYTHONPATH with the program's. TestTensorBoardServiceProgram runs them with python3 -S, so that
the stand-in is the only tensorboard package.
"""

import contextlib
import fcntl
import io
import os
import select
import socket
import subprocess
import sys
import tempfile
import threading
import types
import unittest
from typing import Any, Dict, Iterable, Iterator, List, Optional, Tuple
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
                self.assertIn(("Connection", "close"), response.headers)
                self.assertEqual([], app.environs, "the request does not reach TensorBoard")

    def test_reads_no_refused_body(self) -> None:
        stream = io.BytesIO(b"x" * 10)
        response, _ = request(
            tensorboard_service.check_token(App(), TOKEN.encode()),
            "wrong",
            REQUEST_METHOD="POST",
            CONTENT_LENGTH="10",
            **{"wsgi.input": stream},
        )
        self.assertEqual("401 Unauthorized", response.status)
        self.assertEqual(0, stream.tell())

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
                bound: List[Tuple[Any, ...]] = []

                def server(
                    wsgi_app: Any,
                    flags: Any,
                    made: List[Tuple[Any, Any]] = made,
                    bound: List[Tuple[Any, ...]] = bound,
                ) -> str:
                    self.assertEqual([], bound, "bound comes after the server")
                    made.append((wsgi_app, flags))
                    return "server"

                program = types.SimpleNamespace(**{name: server})
                app = App()
                flags = object()
                factory = tensorboard_service.server_factory(
                    program, TOKEN.encode(), lambda *args, bound=bound: bound.append(args)
                )
                self.assertEqual("server", factory(app, flags))
                self.assertEqual(1, len(made))
                self.assertIs(flags, made[0][1])
                self.assertEqual([("server", flags)], bound)
                self.assertEqual("401 Unauthorized", request(made[0][0], None)[0].status)
                self.assertEqual("200 OK", request(made[0][0], TOKEN)[0].status)
                self.assertEqual(1, len(app.environs))


class Recorder:
    """A WSGI application that records each request's token header and answers status."""

    def __init__(self, status: str, headers: Optional[List[Tuple[str, str]]] = None) -> None:
        self.status = status
        self.headers = headers or []
        self.tokens: List[Optional[str]] = []

    def __call__(self, environ: Dict[str, Any], start_response: Any) -> Iterable[bytes]:
        self.tokens.append(environ.get(tensorboard_service.TOKEN_HEADER))
        start_response(self.status, [("Content-Length", "0"), *self.headers])
        return [b""]


class Echo:
    """A WSGI application that answers the length of a request's body and records its request."""

    def __init__(self) -> None:
        self.environs: List[Dict[str, Any]] = []

    def __call__(self, environ: Dict[str, Any], start_response: Any) -> Iterable[bytes]:
        self.environs.append(dict(environ))
        body = environ["wsgi.input"].read(int(environ.get("CONTENT_LENGTH") or 0))
        answer = str(len(body)).encode()
        start_response("200 OK", [("Content-Length", str(len(answer)))])
        return [answer]


@contextlib.contextmanager
def serve(app: Any) -> Iterator[Any]:
    """
    A server of app on 127.0.0.1, the stand-in's model of TensorBoard's: HTTP/1.1 with keep-alive,
    which leaves a body that app does not read on the connection.
    """
    from tensorboard import program

    flags = types.SimpleNamespace(bind_all=False, host="127.0.0.1", port=0)
    server = program.create_port_scanning_werkzeug_server(app, flags)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield server
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


def read_response(sock: socket.socket) -> Tuple[str, List[Tuple[str, str]], bytes]:
    """The status line, the headers and the body of the next response on sock."""
    data = b""
    while b"\r\n\r\n" not in data:
        chunk = sock.recv(65536)
        if not chunk:
            raise AssertionError(f"the connection closed before a response: {data!r}")
        data += chunk
    head, body = data.split(b"\r\n\r\n", 1)
    status, *lines = head.decode("latin-1").split("\r\n")
    headers = [(k, v.strip()) for k, v in (line.split(":", 1) for line in lines)]
    length = int({k.lower(): v for k, v in headers}["content-length"])
    while len(body) < length:
        chunk = sock.recv(65536)
        if not chunk:
            break
        body += chunk
    return status, headers, body


class ConnectionTest(unittest.TestCase):
    """The token check in a server that speaks HTTP/1.1 with keep-alive, through a socket."""

    path = "/proxy/task-1:6006/"

    def assert_closed(self, sock: socket.socket) -> None:
        try:
            self.assertEqual(b"", sock.recv(65536))
        except ConnectionResetError:
            pass

    def test_refuses_a_withheld_body_at_once(self) -> None:
        app = Echo()
        with serve(tensorboard_service.check_token(app, TOKEN.encode())) as server:
            with socket.create_connection(("127.0.0.1", server.server_port), timeout=2) as sock:
                sock.sendall(
                    f"POST {self.path} HTTP/1.1\r\nHost: localhost\r\n"
                    "Content-Length: 1\r\n\r\n".encode()
                )
                status, headers, body = read_response(sock)
                self.assertEqual("HTTP/1.1 401 Unauthorized", status)
                self.assertIn(("Connection", "close"), headers)
                self.assertEqual(b"Unauthorized\n", body)
                self.assert_closed(sock)
        self.assertEqual([], app.environs)

    def test_never_reads_a_refused_body_as_a_request(self) -> None:
        app = Echo()
        smuggled = (
            f"GET {self.path} HTTP/1.1\r\nHost: localhost\r\n"
            f"{tensorboard_service.TOKEN_HEADER_NAME}: {TOKEN}\r\n\r\n"
        ).encode()
        with serve(tensorboard_service.check_token(app, TOKEN.encode())) as server:
            with socket.create_connection(("127.0.0.1", server.server_port), timeout=2) as sock:
                sock.sendall(
                    f"POST {self.path} HTTP/1.1\r\nHost: localhost\r\n"
                    f"{tensorboard_service.TOKEN_HEADER_NAME}: wrong\r\n"
                    f"Content-Length: {1 << 20}\r\n\r\n".encode() + smuggled
                )
                status, headers, _ = read_response(sock)
                self.assertEqual("HTTP/1.1 401 Unauthorized", status)
                self.assertIn(("Connection", "close"), headers)
                self.assert_closed(sock)
        self.assertEqual([], app.environs, "the body is never served as a request")

    def test_passes_on_a_body(self) -> None:
        app = Echo()
        body = b"x" * (1 << 20)
        with serve(tensorboard_service.check_token(app, TOKEN.encode())) as server:
            with socket.create_connection(("127.0.0.1", server.server_port), timeout=10) as sock:
                for _ in range(2):
                    sock.sendall(
                        f"POST {self.path} HTTP/1.1\r\nHost: localhost\r\n"
                        f"{tensorboard_service.TOKEN_HEADER_NAME}: {TOKEN}\r\n"
                        f"Content-Length: {len(body)}\r\n\r\n".encode() + body
                    )
                    status, headers, answer = read_response(sock)
                    self.assertEqual("HTTP/1.1 200 OK", status)
                    self.assertNotIn("connection", [k.lower() for k, _ in headers])
                    self.assertEqual(str(len(body)).encode(), answer)
        self.assertEqual(2, len(app.environs), "the connection takes the next request")
        for environ in app.environs:
            self.assertNotIn(tensorboard_service.TOKEN_HEADER, environ)


class ProbeTest(unittest.TestCase):
    path = "/proxy/task-1:6006/"

    def probe(self, app: Any) -> Tuple[str, str]:
        """What the probe of a server of app writes to its descriptor and to stderr."""
        read, write = os.pipe()
        ready = os.fdopen(write, "w")
        with os.fdopen(read) as out:
            with serve(app) as server:
                with mock.patch.object(tensorboard_service, "PROBE_INTERVAL", 0.01):
                    with mock.patch("sys.stderr", new_callable=io.StringIO) as stderr:
                        tensorboard_service.probe(server.server_port, self.path, TOKEN, ready)
            self.assertTrue(ready.closed)
            return out.read(), stderr.getvalue()

    def test_ready(self) -> None:
        app = App()
        written, stderr = self.probe(tensorboard_service.check_token(app, TOKEN.encode()))
        self.assertEqual("tensorboard: http ready\n", written)
        self.assertEqual("", stderr)
        self.assertEqual(1, len(app.environs))
        self.assertEqual(self.path, app.environs[0]["PATH_INFO"])

    def test_not_ready_without_the_check(self) -> None:
        app = Recorder("200 OK")
        written, stderr = self.probe(app)
        self.assertEqual("", written)
        self.assertEqual("tensorboard service: TensorBoard answers without the token\n", stderr)
        self.assertEqual([None], app.tokens)

    def test_status_follows_no_redirect(self) -> None:
        other = Recorder("200 OK")
        with serve(other) as target:
            location = f"http://127.0.0.1:{target.server_port}/"
            moved = Recorder("302 Found", [("Location", location)])
            with serve(moved) as server:
                with mock.patch.dict(os.environ, {"http_proxy": location, "no_proxy": ""}):
                    status = tensorboard_service.http_status(
                        server.server_port, self.path, {"X-Determined-Service-Token": TOKEN}
                    )
        self.assertEqual(302, status)
        self.assertEqual([TOKEN], moved.tokens)
        self.assertEqual([], other.tokens, "neither the redirect nor a proxy gets the request")

    def test_status_without_a_server(self) -> None:
        with serve(App()) as server:
            port = server.server_port
        self.assertIsNone(tensorboard_service.http_status(port, self.path, {}))


class FlagsTest(unittest.TestCase):
    environ = {
        "DET_TENSORBOARD_PORT": "6006",
        "DET_TENSORBOARD_PATH_PREFIX": "/proxy/task-1:6006/",
    }
    flag_names = ["logdir", "port", "path_prefix", "host", "bind_all", "load_fast", "reuse_port"]

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
        args = [
            "--load_fast=true",
            "--port",
            "1",
            "--hos=127.0.0.2",
            "--path_prefix=/x",
            "--reuse_port=true",
        ]
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
                        "reuse_port": False,
                    },
                    {k: getattr(flags, k) for k in self.flag_names},
                )

    def test_reload_multifile_with_sources(self) -> None:
        # Only a service that reads sources polls every event file of a run.
        self.assertNotIn(
            "reload_multifile", tensorboard_service.service_flags(self.environ, "/logs")
        )
        environ = {**self.environ, "DET_TENSORBOARD_SOURCES": ""}
        self.assertNotIn("reload_multifile", tensorboard_service.service_flags(environ, "/logs"))
        environ["DET_TENSORBOARD_SOURCES"] = "/run/determined/tensorboard/sources.json"
        flags = tensorboard_service.service_flags(environ, "/logs")
        self.assertIs(True, flags["reload_multifile"])
        from tensorboard import default, program

        tb = program.TensorBoard(plugins=default.get_plugins())
        tb.configure(tensorboard_service.tensorboard_argv(["--reload_multifile=false"]), **flags)
        self.assertIs(True, tb.flags.reload_multifile)

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

    def test_ready_fd(self) -> None:
        for value, want in [(None, 3), ("", 3), ("3", 3), ("7", 7), ("12", 12)]:
            environ = {} if value is None else {"DET_TENSORBOARD_READY_FD": value}
            self.assertEqual(want, tensorboard_service.ready_fd(environ))
        for value in ["x", "-1", "0", "2", " 7", "7.0"]:
            with self.subTest(value=value):
                with self.assertRaisesRegex(
                    tensorboard_service.Unavailable,
                    f'^DET_TENSORBOARD_READY_FD is not a descriptor: "{value}"$',
                ):
                    tensorboard_service.ready_fd({"DET_TENSORBOARD_READY_FD": value})

    def test_release(self) -> None:
        self.assertEqual((2, 9), tensorboard_service.release("2.9.0"))
        self.assertEqual((2, 18), tensorboard_service.release("2.18.0"))
        self.assertEqual((10, 0), tensorboard_service.release("10.0a1"))
        for version in ["unknown", "2", ""]:
            with self.assertRaises(tensorboard_service.Unavailable):
                tensorboard_service.release(version)


class MainTest(unittest.TestCase):
    """The program's refusals, each one line and 127, before TensorBoard starts."""

    def program_env(self, **env: str) -> Dict[str, str]:
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
        return environ

    def run_program(self, *args: str, **env: str) -> "subprocess.CompletedProcess[str]":
        environ = self.program_env(**env)
        return subprocess.run(
            [sys.executable, "-S", tensorboard_service.__file__, *args],
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
            ("no descriptor", ["/logs"], {}, "descriptor 3 is not open"),
            (
                "no given descriptor",
                ["/logs"],
                {"DET_TENSORBOARD_READY_FD": "7"},
                "descriptor 7 is not open",
            ),
            (
                "bad descriptor",
                ["/logs"],
                {"DET_TENSORBOARD_READY_FD": "1"},
                'DET_TENSORBOARD_READY_FD is not a descriptor: "1"',
            ),
        ]
        for name, args, env, line in cases:
            with self.subTest(name):
                result = self.run_program(*args, **env)
                self.assertEqual(127, result.returncode, result.stderr)
                self.assertEqual("tensorboard service: " + line + "\n", result.stderr)
                self.assertEqual("", result.stdout)

    def test_ready_line_on_the_given_descriptor(self) -> None:
        # The program writes its ready line to DET_TENSORBOARD_READY_FD, not to descriptor 3.
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            port = sock.getsockname()[1]
        read, pipe_write = os.pipe()
        write = fcntl.fcntl(pipe_write, fcntl.F_DUPFD_CLOEXEC, 10)
        os.close(pipe_write)
        with tempfile.TemporaryDirectory() as tmp, os.fdopen(read, "rb") as ready:
            environ = self.program_env(
                DET_TENSORBOARD_PORT=str(port),
                DET_TENSORBOARD_READY_FD=str(write),
                ORDER_FILE=os.path.join(tmp, "order"),
                TB_STAND_IN_PYTHON=sys.executable,
            )
            try:
                p = subprocess.Popen(
                    [sys.executable, "-S", tensorboard_service.__file__, "/logs"],
                    env=environ,
                    pass_fds=(write,),
                    stderr=subprocess.PIPE,
                    text=True,
                )
            finally:
                os.close(write)
            try:
                readable, _, _ = select.select([ready], [], [], 30)
                written = ready.read() if readable else b""
            finally:
                p.terminate()
                _, stderr = p.communicate(timeout=30)
        self.assertEqual(b"tensorboard: http ready\n", written, stderr)


if __name__ == "__main__":
    unittest.main(verbosity=2)
