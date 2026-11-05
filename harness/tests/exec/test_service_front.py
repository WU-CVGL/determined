import gzip
import http.client
import http.server
import socket
import threading
from typing import Any, Dict, Iterator, List, Optional, Tuple

import pytest

from determined.exec import service_front

TOKEN = "s3cret-token"


class _Upstream(http.server.ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self) -> None:
        super().__init__(("127.0.0.1", 0), _UpstreamHandler)
        self.requests: List[Dict[str, Any]] = []


class _UpstreamHandler(http.server.BaseHTTPRequestHandler):
    """A stand-in service: /gzip answers a gzip-encoded body, /chunked a chunked one, and any other
    path its own request line; it records every request."""

    server: _Upstream
    protocol_version = "HTTP/1.1"

    def _record(self) -> bytes:
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length) if length else b""
        self.server.requests.append(
            {
                "method": self.command,
                "path": self.path,
                "headers": dict(self.headers.items()),
                "body": body,
            }
        )
        return body

    def do_GET(self) -> None:
        self._record()
        if self.path == "/gzip":
            body = gzip.compress(b"<svg>icons</svg>" * 100)
            self.send_response(200)
            self.send_header("Content-Type", "image/svg+xml")
            self.send_header("Content-Encoding", "gzip")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        if self.path == "/chunked":
            self.send_response(200)
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()
            for part in (b"one ", b"two"):
                self.wfile.write(b"%x\r\n%s\r\n" % (len(part), part))
            self.wfile.write(b"0\r\n\r\n")
            return
        body = f"{self.command} {self.path}".encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Set-Cookie", "a=1")
        self.send_header("Set-Cookie", "b=2")
        self.send_header("Connection", "X-Upstream-Hop")
        self.send_header("X-Upstream-Hop", "dropped")
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    do_HEAD = do_GET

    def do_POST(self) -> None:
        body = self._record()
        self.send_response(201)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, format: str, *args: Any) -> None:
        pass


def _serve(server: http.server.HTTPServer) -> None:
    threading.Thread(target=server.serve_forever, daemon=True).start()


@pytest.fixture
def front() -> Iterator[Tuple[_Upstream, int]]:
    upstream = _Upstream()
    _serve(upstream)
    server = service_front._Server(
        ("127.0.0.1", 0), ("127.0.0.1", upstream.server_address[1]), TOKEN.encode()
    )
    _serve(server)
    try:
        yield upstream, server.server_address[1]
    finally:
        server.shutdown()
        server.server_close()
        upstream.shutdown()
        upstream.server_close()


def _request(
    port: int,
    method: str,
    path: str,
    headers: Optional[Dict[str, str]] = None,
    body: Optional[bytes] = None,
) -> http.client.HTTPResponse:
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=10)
    conn.request(method, path, body=body, headers=headers or {})
    return conn.getresponse()


def test_tensorboard_front_refuses_without_the_token(front: Tuple[_Upstream, int]) -> None:
    upstream, port = front
    for headers in (
        {},
        {service_front.TOKEN_HEADER: "wrong"},
        {service_front.TOKEN_HEADER: TOKEN + "x"},
        {service_front.TOKEN_HEADER: ""},
        {"Authorization": f"Bearer {TOKEN}"},
        {"Cookie": f"{service_front.TOKEN_HEADER}={TOKEN}"},
    ):
        for method in ("GET", "POST"):
            response = _request(port, method, f"/p/?token={TOKEN}", headers, body=b"x")
            assert response.status == 401, headers
            assert response.read() == b"Unauthorized\n"
    assert upstream.requests == []


def test_tensorboard_front_passes_requests_with_the_token(front: Tuple[_Upstream, int]) -> None:
    upstream, port = front
    path = "/proxy/task:2900/data/plugin/scalars/tags?run=a%2Fb&x=1"
    response = _request(
        port,
        "GET",
        path,
        {
            service_front.TOKEN_HEADER: TOKEN,
            "X-Custom": "kept",
            "Connection": "X-Hop",
            "X-Hop": "dropped",
        },
    )
    assert response.status == 200
    assert response.read() == f"GET {path}".encode()
    assert response.headers.get_all("Set-Cookie") == ["a=1", "b=2"]
    assert response.headers.get("X-Upstream-Hop") is None
    (seen,) = upstream.requests
    assert seen["path"] == path
    assert seen["headers"]["X-Custom"] == "kept"
    assert service_front.TOKEN_HEADER not in seen["headers"]
    assert "X-Hop" not in seen["headers"]
    assert seen["headers"]["Host"] == f"127.0.0.1:{port}"

    response = _request(port, "HEAD", "/head", {service_front.TOKEN_HEADER: TOKEN})
    assert response.status == 200
    assert response.headers["Content-Length"] == str(len(b"HEAD /head"))
    assert response.read() == b""


def test_tensorboard_front_keeps_the_encoding(front: Tuple[_Upstream, int]) -> None:
    _, port = front
    response = _request(port, "GET", "/gzip", {service_front.TOKEN_HEADER: TOKEN})
    assert response.status == 200
    assert response.headers["Content-Encoding"] == "gzip"
    body = response.read()
    assert response.headers["Content-Length"] == str(len(body))
    assert gzip.decompress(body) == b"<svg>icons</svg>" * 100

    response = _request(port, "GET", "/chunked", {service_front.TOKEN_HEADER: TOKEN})
    assert response.status == 200
    assert response.headers.get("Transfer-Encoding") is None
    assert response.read() == b"one two"


def test_tensorboard_front_passes_a_post_body(front: Tuple[_Upstream, int]) -> None:
    upstream, port = front
    body = b"a=1&b=" + b"x" * 200_000
    response = _request(
        port,
        "POST",
        "/proxy/task:2900/data/plugin/x",
        {service_front.TOKEN_HEADER: TOKEN, "Content-Type": "application/x-www-form-urlencoded"},
        body=body,
    )
    assert response.status == 201
    assert response.read() == body
    (seen,) = upstream.requests
    assert seen["method"] == "POST"
    assert seen["body"] == body
    assert seen["headers"]["Content-Type"] == "application/x-www-form-urlencoded"


def test_tensorboard_front_without_an_upstream() -> None:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        closed = s.getsockname()[1]
    server = service_front._Server(("127.0.0.1", 0), ("127.0.0.1", closed), TOKEN.encode())
    _serve(server)
    try:
        response = _request(
            server.server_address[1], "GET", "/", {"X-Determined-Service-Token": TOKEN}
        )
        assert response.status == 502
        assert response.read() == b"Bad Gateway\n"
    finally:
        server.shutdown()
        server.server_close()


def test_tensorboard_front_requires_the_token(
    monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture
) -> None:
    monkeypatch.delenv("FRONT_TEST_TOKEN", raising=False)
    argv = [
        "--listen",
        "127.0.0.1:0",
        "--upstream",
        "127.0.0.1:1",
        "--token-env",
        "FRONT_TEST_TOKEN",
    ]
    assert service_front.main(argv) == 1
    assert capsys.readouterr().err == "service front: FRONT_TEST_TOKEN is not set\n"


def test_tensorboard_front_stops_on_sigint(
    monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture
) -> None:
    def interrupted(self: service_front._Server, poll_interval: float = 0.5) -> None:
        raise KeyboardInterrupt

    monkeypatch.setenv("FRONT_TEST_TOKEN", TOKEN)
    monkeypatch.setattr(service_front._Server, "serve_forever", interrupted)
    argv = [
        "--listen",
        "127.0.0.1:0",
        "--upstream",
        "127.0.0.1:1",
        "--token-env",
        "FRONT_TEST_TOKEN",
    ]
    assert service_front.main(argv) == 130
    assert capsys.readouterr().err == ""
