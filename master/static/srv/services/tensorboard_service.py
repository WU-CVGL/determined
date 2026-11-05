"""
tensorboard_service.py runs TensorBoard for the tensorboard service of a shell, with the python
that has TensorBoard, which the unit tensorboard.sh picks, on the log directory that it gives as
its argument, absolute and created.

TensorBoard refuses every request that does not carry exactly one X-Determined-Service-Token
header equal to DET_TENSORBOARD_TOKEN, which the master's proxy adds once it has authorized the
request: it answers 401 and never passes the request on. The check wraps TensorBoard's whole WSGI
application, inside TensorBoard's own server, so that no port of the container, in any network
mode, serves TensorBoard without the token. The header does not reach TensorBoard, and nothing of
the token is logged.

TensorBoard gets the config's arguments, DET_TENSORBOARD_ARGS, a JSON array of strings, and then
the service's own flags, which win over them: --logdir, --port DET_TENSORBOARD_PORT, on every IPv4
address, --path_prefix DET_TENSORBOARD_PATH_PREFIX and --load_fast=false, which keeps TensorBoard's
data server, a listener without the check, from starting. --bind_all is false, whatever its
default, which some builds set to true.

TensorBoard 2.9 and later is supported. With an earlier release, or what the program needs
missing, it says why in one line and exits 127, which makes the service UNAVAILABLE. Otherwise it
exits with TensorBoard's status, which a stop's SIGTERM or SIGINT makes 0, or 130 for a SIGINT that
comes before TensorBoard serves.

The program imports only the standard library and tensorboard, so that any python with TensorBoard
runs it.
"""

import hmac
import inspect
import json
import os
import re
import sys
from typing import Any, Callable, Dict, Iterable, List, Mapping, Tuple

TOKEN_ENV = "DET_TENSORBOARD_TOKEN"
# The token's header, X-Determined-Service-Token, in the WSGI environment.
TOKEN_HEADER = "HTTP_X_DETERMINED_SERVICE_TOKEN"
MIN_RELEASE = (2, 9)
UNAVAILABLE_STATUS = 127
# How much of a refused request's body is read, so that its connection can take the next request.
DRAIN_LIMIT = 64 * 1024
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


def tensorboard_argv(args: List[str]) -> List[str]:
    """TensorBoard's command line: the config's arguments."""
    return ["tensorboard", *args]


def service_flags(environ: Mapping[str, str], logdir: str) -> Dict[str, Any]:
    """
    The service's flags, which TensorBoard's configure sets after it has parsed the command line
    and before it checks the flags, so that they win over the config's arguments.
    """
    return {
        "logdir": logdir,
        "port": int(environ["DET_TENSORBOARD_PORT"]),
        "path_prefix": environ["DET_TENSORBOARD_PATH_PREFIX"].rstrip("/"),
        "host": "0.0.0.0",
        "bind_all": False,
        "load_fast": "false",
    }


def drain(environ: Dict[str, Any]) -> None:
    """Read a refused request's body, up to DRAIN_LIMIT."""
    try:
        length = min(int(environ.get("CONTENT_LENGTH") or 0), DRAIN_LIMIT)
    except ValueError:
        return
    stream = environ.get("wsgi.input")
    while length > 0 and stream is not None:
        chunk = stream.read(length)
        if not chunk:
            break
        length -= len(chunk)


def check_token(app: WSGIApp, token: bytes) -> WSGIApp:
    """
    app behind the token: a request without exactly one token header equal to token gets 401 and
    never reaches app; app gets the others without the header. Two headers reach a WSGI
    environment joined by a comma, which a token never has.
    """

    def checked(environ: Dict[str, Any], start_response: Callable[..., Any]) -> Iterable[bytes]:
        value = environ.pop(TOKEN_HEADER, None)
        if not isinstance(value, str) or not hmac.compare_digest(
            value.encode("latin-1", "replace"), token
        ):
            drain(environ)
            start_response(
                "401 Unauthorized",
                [
                    ("Content-Type", "text/plain; charset=utf-8"),
                    ("Content-Length", str(len(UNAUTHORIZED))),
                ],
            )
            return [UNAUTHORIZED]
        return app(environ, start_response)

    return checked


def server_factory(program: Any, token: bytes) -> Callable[[WSGIApp, Any], Any]:
    """The server_class of TensorBoard: its own default server, of the application behind token."""
    default = getattr(program, "create_port_scanning_werkzeug_server", None)
    if default is None:
        default = program.WerkzeugServer

    def create(wsgi_app: WSGIApp, flags: Any) -> Any:
        return default(check_token(wsgi_app, token), flags)

    return create


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
    print(f"tensorboard service: TensorBoard {tensorboard_version} reads {logdir}", file=sys.stderr)

    try:
        from tensorboard import main_lib
    except ImportError:
        pass
    else:
        main_lib.global_init()
    tensorboard = program.TensorBoard(
        plugins=default.get_plugins(), server_class=server_factory(program, token)
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
