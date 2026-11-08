"""
tensorboard_service.py runs TensorBoard for the tensorboard service of a shell, with the python
that has TensorBoard, which the unit tensorboard.sh picks, on the log directory that it gives as
its argument, absolute and created. The launcher of a TensorBoard task runs it as well, with its
own python, on the directory that it fetches the logs into.

TensorBoard refuses every request that does not carry exactly one X-Determined-Service-Token
header equal to DET_TENSORBOARD_TOKEN, which the master's proxy adds once it has authorized the
request: it answers 401 at once, without reading the request's body, closes the connection after
the response and never passes the request on. The check wraps TensorBoard's whole WSGI
application, inside TensorBoard's own server, so that no port of the container, in any network
mode, serves TensorBoard without the token. The header does not reach TensorBoard, and nothing of
the token is logged.

TensorBoard gets the config's arguments, DET_TENSORBOARD_ARGS, a JSON array of strings, and then
the service's own flags, which win over them: --logdir, --port DET_TENSORBOARD_PORT, on every IPv4
address, --path_prefix DET_TENSORBOARD_PATH_PREFIX, --load_fast=false, which keeps TensorBoard's
data server, a listener without the check, from starting, and --reuse_port=false, so that no other
process can share the port. --bind_all is false, whatever its default, which some builds set to
true. With DET_TENSORBOARD_SOURCES, --reload_multifile=true and
--reload_multifile_inactive_secs=86400, TensorBoard's default, so that TensorBoard polls every
event file of a run, which the loader may publish in any order.

Once TensorBoard's server has bound the port, and only then, a probe asks it on 127.0.0.1 under the
prefix, without and with the token, every half second, and writes "tensorboard: http ready" to
the descriptor DET_TENSORBOARD_READY_FD, 3 if unset, once TensorBoard refuses the first and answers
the second. While TensorBoard holds
the port, no other process can listen on 127.0.0.1 at that port, so the probe sends the token to
TensorBoard only. It follows no redirect and takes no proxy. A TensorBoard that answers without
the token is never ready, and the probe says so once.

TensorBoard 2.9 and later is supported. With an earlier release, or what the program needs
missing, it says why in one line and exits 127, which makes the service UNAVAILABLE. Otherwise it
exits with TensorBoard's status, which a stop's SIGTERM or SIGINT makes 0, or 130 for a SIGINT that
comes before TensorBoard serves.

The program imports only the standard library and tensorboard, so that any python with TensorBoard
runs it.
"""

import hmac
import http.client
import inspect
import json
import os
import re
import sys
import threading
import time
from typing import IO, Any, Callable, Dict, Iterable, List, Mapping, Optional, Tuple

TOKEN_ENV = "DET_TENSORBOARD_TOKEN"
TOKEN_HEADER_NAME = "X-Determined-Service-Token"
# The token's header in the WSGI environment.
TOKEN_HEADER = "HTTP_X_DETERMINED_SERVICE_TOKEN"
# The descriptor that the probe writes READY_LINE to: DET_TENSORBOARD_READY_FD, or READY_FD, which
# the unit hands over.
READY_FD_ENV = "DET_TENSORBOARD_READY_FD"
READY_FD = 3
READY_LINE = "tensorboard: http ready\n"
PROBE_INTERVAL = 0.5
PROBE_TIMEOUT = 5
MIN_RELEASE = (2, 9)
UNAVAILABLE_STATUS = 127
UNAUTHORIZED = b"Unauthorized\n"

WSGIApp = Callable[[Dict[str, Any], Callable[..., Any]], Iterable[bytes]]


class Unavailable(Exception):
    """What the service needs is missing; the message says what, in one line."""


def release(version: str) -> Tuple[int, int]:
    """The major and minor release of a TensorBoard version, as (2, 9) for "2.9.0"."""
    match = re.match(r"([0-9]+)\.([0-9]+)", version)
    if match is None:
        raise Unavailable(f'cannot tell the release of TensorBoard: "{version}"')
    return int(match.group(1)), int(match.group(2))


def user_args(environ: Mapping[str, str]) -> List[str]:
    """The config's arguments, DET_TENSORBOARD_ARGS, a JSON array of strings, unset for none."""
    try:
        args = json.loads(environ.get("DET_TENSORBOARD_ARGS") or "[]")
    except ValueError:
        args = None
    if not isinstance(args, list) or not all(isinstance(arg, str) for arg in args):
        raise Unavailable("DET_TENSORBOARD_ARGS is not a JSON array of strings")
    return args


def ready_fd(environ: Mapping[str, str]) -> int:
    """The descriptor of the ready line: DET_TENSORBOARD_READY_FD, READY_FD if unset."""
    value = environ.get(READY_FD_ENV, "")
    if not value:
        return READY_FD
    if not re.fullmatch(r"[0-9]+", value) or int(value) < 3:
        raise Unavailable(f'{READY_FD_ENV} is not a descriptor: "{value}"')
    return int(value)


def tensorboard_argv(args: List[str]) -> List[str]:
    """TensorBoard's command line: the config's arguments."""
    return ["tensorboard", *args]


def service_flags(environ: Mapping[str, str], logdir: str) -> Dict[str, Any]:
    """
    The service's flags, which TensorBoard's configure sets after it has parsed the command line
    and before it checks the flags, so that they win over the config's arguments.
    """
    flags: Dict[str, Any] = {
        "logdir": logdir,
        "port": int(environ["DET_TENSORBOARD_PORT"]),
        "path_prefix": environ["DET_TENSORBOARD_PATH_PREFIX"].rstrip("/"),
        "host": "0.0.0.0",
        "bind_all": False,
        "load_fast": "false",
        "reuse_port": False,
    }
    if environ.get("DET_TENSORBOARD_SOURCES"):
        flags["reload_multifile"] = True
        flags["reload_multifile_inactive_secs"] = 86400
    return flags


def check_token(app: WSGIApp, token: bytes) -> WSGIApp:
    """
    app behind the token: a request without exactly one token header equal to token gets 401 at
    once and never reaches app; app gets the others without the header. Two headers reach a WSGI
    environment joined by a comma, which a token never has. The 401 reads nothing of the body and
    has "Connection: close", which makes TensorBoard's server close the connection after it, so
    that the unread body is never read as a request.
    """

    def checked(environ: Dict[str, Any], start_response: Callable[..., Any]) -> Iterable[bytes]:
        value = environ.pop(TOKEN_HEADER, None)
        if not isinstance(value, str) or not hmac.compare_digest(
            value.encode("latin-1", "replace"), token
        ):
            start_response(
                "401 Unauthorized",
                [
                    ("Content-Type", "text/plain; charset=utf-8"),
                    ("Content-Length", str(len(UNAUTHORIZED))),
                    ("Connection", "close"),
                ],
            )
            return [UNAUTHORIZED]
        return app(environ, start_response)

    return checked


def server_factory(
    program: Any, token: bytes, bound: Callable[[Any, Any], None]
) -> Callable[[WSGIApp, Any], Any]:
    """
    The server_class of TensorBoard: its own default server, of the application behind token.
    bound gets the server and the flags once the server has bound its port.
    """
    default = getattr(program, "create_port_scanning_werkzeug_server", None)
    if default is None:
        default = program.WerkzeugServer

    def create(wsgi_app: WSGIApp, flags: Any) -> Any:
        server = default(check_token(wsgi_app, token), flags)
        bound(server, flags)
        return server

    return create


def http_status(port: int, path: str, headers: Mapping[str, str]) -> Optional[int]:
    """
    The status of a GET of path on 127.0.0.1 at port, without a proxy or a redirect, or None if
    the port does not answer.
    """
    connection = http.client.HTTPConnection("127.0.0.1", port, timeout=PROBE_TIMEOUT)
    try:
        connection.request("GET", path, headers=dict(headers))
        response = connection.getresponse()
        response.read()
        return response.status
    except (OSError, http.client.HTTPException):
        return None
    finally:
        connection.close()


def probe(port: int, path: str, token: str, ready: IO[str]) -> None:
    """
    Ask TensorBoard, whose server has bound port, until it refuses a request without the token and
    answers one with it, and then write READY_LINE to ready; or, if it answers without the token,
    say so and return.
    """
    try:
        while True:
            refused = http_status(port, path, {})
            if refused == 200:
                print(
                    "tensorboard service: TensorBoard answers without the token",
                    file=sys.stderr,
                    flush=True,
                )
                return
            if refused == 401 and http_status(port, path, {TOKEN_HEADER_NAME: token}) == 200:
                ready.write(READY_LINE)
                ready.flush()
                return
            time.sleep(PROBE_INTERVAL)
    except OSError:
        # The readiness check has gone.
        pass
    finally:
        try:
            ready.close()
        except OSError:
            pass


def start_probe(token: bytes, ready: IO[str]) -> Callable[[Any, Any], None]:
    """The bound of server_factory: probe the server's port in a thread."""

    def bound(server: Any, flags: Any) -> None:
        threading.Thread(
            target=probe,
            args=(server.server_port, flags.path_prefix + "/", token.decode("utf-8"), ready),
            daemon=True,
        ).start()

    return bound


def run(argv: List[str], environ: Mapping[str, str]) -> int:
    if len(argv) != 2:
        raise Unavailable("usage: tensorboard_service.py LOGDIR")
    logdir = argv[1]
    token = environ.get(TOKEN_ENV, "").encode("utf-8")
    if not token:
        raise Unavailable(f"{TOKEN_ENV} is not set")
    try:
        from tensorboard import default, program, version
        from tensorboard.plugins import base_plugin
    except ImportError:
        raise Unavailable("tensorboard is not installed") from None
    tensorboard_version = str(version.VERSION)
    if release(tensorboard_version) < MIN_RELEASE:
        raise Unavailable(f"TensorBoard {tensorboard_version} is older than 2.9")
    if "server_class" not in inspect.signature(program.TensorBoard.__init__).parameters:
        raise Unavailable(f"TensorBoard {tensorboard_version} takes no server_class")
    args = user_args(environ)
    fd = ready_fd(environ)
    try:
        os.set_inheritable(fd, False)
        ready = os.fdopen(fd, "w")
    except OSError:
        raise Unavailable(f"descriptor {fd} is not open") from None
    print(f"tensorboard service: TensorBoard {tensorboard_version} reads {logdir}", file=sys.stderr)

    try:
        from tensorboard import main_lib
    except ImportError:
        pass
    else:
        main_lib.global_init()
    tensorboard = program.TensorBoard(
        plugins=default.get_plugins(),
        server_class=server_factory(program, token, start_probe(token, ready)),
    )
    try:
        tensorboard.configure(tensorboard_argv(args), **service_flags(environ, logdir))
    except base_plugin.FlagsError as e:
        print(f"Error: {e}", file=sys.stderr)
        return 1
    return int(tensorboard.main())


def main() -> int:
    try:
        return run(sys.argv, os.environ)
    except Unavailable as e:
        print(f"tensorboard service: {e}", file=sys.stderr)
        return UNAVAILABLE_STATUS
    except KeyboardInterrupt:
        return 130


if __name__ == "__main__":
    sys.exit(main())
