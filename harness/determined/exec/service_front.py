"""
The front of a task's service: an HTTP reverse proxy on the service's published port that passes
on only the requests that carry the service's token, which only the master's proxy has, to the
service, which listens inside the container.

- A request must have the header X-Determined-Service-Token with the token, the value of the
  environment variable that --token-env names; any other request gets 401 with a one-line body.
  The header does not reach the service.
- GET, HEAD and POST pass on with their path and query as they came, their headers but the
  hop-by-hop ones, and their bodies, which a POST must give with Content-Length. Responses pass
  back as they came, Content-Encoding and Content-Length included; one without Content-Length
  ends when the connection closes. There are no WebSockets.
- The front logs no requests.

The front uses the standard library only, so that it also runs as a file, outside the package.
"""

import argparse
import hmac
import http.client
import http.server
import os
import signal
import socket
import sys
from typing import Any, Iterator, List, Optional, Set, Tuple

TOKEN_HEADER = "X-Determined-Service-Token"

_HOP_BY_HOP = frozenset(
    [
        "connection",
        "keep-alive",
        "proxy-authenticate",
        "proxy-authorization",
        "proxy-connection",
        "te",
        "trailer",
        "trailers",
        "transfer-encoding",
        "upgrade",
    ]
)

_CHUNK = 64 * 1024


def _hop_by_hop(connection_values: List[str]) -> Set[str]:
    """The hop-by-hop header names, with the ones that the Connection headers list."""
    names = set(_HOP_BY_HOP)
    for value in connection_values:
        names.update(name.strip().lower() for name in value.split(",") if name.strip())
    return names


def _address(text: str) -> Tuple[str, int]:
    host, sep, port = text.rpartition(":")
    if not sep or not port.isdigit():
        raise argparse.ArgumentTypeError(f"not a host:port: {text!r}")
    return host, int(port)


class _Server(http.server.ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, listen: Tuple[str, int], upstream: Tuple[str, int], token: bytes) -> None:
        super().__init__(listen, _Handler)
        self.upstream = upstream
        self.token = token

    def handle_error(self, request: Any, client_address: Any) -> None:
        if isinstance(sys.exc_info()[1], (ConnectionError, socket.timeout)):
            return
        super().handle_error(request, client_address)


class _Handler(http.server.BaseHTTPRequestHandler):
    # HTTP/1.0: every connection carries one request, and a response without Content-Length ends
    # when it closes.
    server: _Server
    # A connection that sends or reads nothing for this many seconds closes.
    timeout = 300

    def do_GET(self) -> None:
        self._forward()

    def do_HEAD(self) -> None:
        self._forward()

    def do_POST(self) -> None:
        self._forward()

    def log_message(self, format: str, *args: object) -> None:
        pass

    def _answer(self, status: int, line: str) -> None:
        body = (line + "\n").encode("utf-8")
        self.send_response_only(status)
        self.send_header("Content-Type", "text/plain; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def _authorized(self) -> bool:
        values = self.headers.get_all(TOKEN_HEADER) or []
        if len(values) != 1:
            return False
        # The parser decoded the header as Latin-1, so encoding it back gives its bytes.
        return hmac.compare_digest(values[0].strip().encode("latin-1"), self.server.token)

    def _body(self, length: int) -> Iterator[bytes]:
        while length > 0:
            data = self.rfile.read(min(length, _CHUNK))
            if not data:
                raise ConnectionError("the request body ended early")
            length -= len(data)
            yield data

    def _forward(self) -> None:
        if not self._authorized():
            self._answer(401, "Unauthorized")
            return

        body: Optional[Iterator[bytes]] = None
        if "chunked" in self.headers.get("Transfer-Encoding", "").lower():
            self._answer(411, "Length Required")
            return
        length = self.headers.get("Content-Length")
        if length is not None:
            if not length.strip().isdigit():
                self._answer(400, "Bad Request")
                return
            body = self._body(int(length))

        drop = _hop_by_hop(self.headers.get_all("Connection") or [])
        drop.add(TOKEN_HEADER.lower())
        headers = [(k, v) for k, v in self.headers.items() if k.lower() not in drop]
        has_host = any(k.lower() == "host" for k, _ in headers)

        upstream = http.client.HTTPConnection(*self.server.upstream)
        try:
            try:
                upstream.putrequest(
                    self.command, self.path, skip_host=has_host, skip_accept_encoding=True
                )
                for name, value in headers:
                    upstream.putheader(name, value)
                upstream.endheaders()
                for data in body or ():
                    upstream.send(data)
                response = upstream.getresponse()
            except (OSError, http.client.HTTPException):
                self._answer(502, "Bad Gateway")
                return
            self._relay(response)
        finally:
            upstream.close()

    def _relay(self, response: http.client.HTTPResponse) -> None:
        self.send_response_only(response.status, response.reason)
        drop = _hop_by_hop(response.headers.get_all("Connection") or [])
        if response.chunked:
            # http.client takes the chunks apart; the body then ends with the connection.
            drop.add("content-length")
        for name, value in response.getheaders():
            if name.lower() not in drop:
                self.send_header(name, value)
        self.send_header("Connection", "close")
        self.end_headers()
        if self.command == "HEAD":
            return
        while True:
            data = response.read1(_CHUNK)
            if not data:
                break
            self.wfile.write(data)
        self.wfile.flush()


def main(argv: Optional[List[str]] = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0].strip())
    parser.add_argument("--listen", required=True, type=_address, help="host:port")
    parser.add_argument("--upstream", required=True, type=_address, help="host:port")
    parser.add_argument("--token-env", required=True, help="the variable that holds the token")
    args = parser.parse_args(argv)

    token = os.environ.get(args.token_env, "")
    if not token:
        print(f"service front: {args.token_env} is not set", file=sys.stderr)
        return 1
    server = _Server(args.listen, args.upstream, token.encode("utf-8"))
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        return 128 + signal.SIGINT
    finally:
        server.server_close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
