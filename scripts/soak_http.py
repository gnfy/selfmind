"""HTTP helpers shared by the soak scripts.

The daemon grants CLI identity only to the person's own terminal, which it
verifies from kernel peer credentials on its local control socket. A request
that claims `platform: "cli"` therefore cannot travel over TCP: these helpers
send such a request over that socket and every other request over TCP, as an
integration would. A soak script is the daemon's parent, never its descendant,
so the daemon accepts it as the person's CLI.
"""

import http.client
import json
import socket
import urllib.error
import urllib.request

# Endpoints that act with the person's own authority whatever platform the body
# names. Binding an account gives it the person's whole identity.
CLI_ONLY_PATHS = ("/v1/accounts/bind",)


def claims_cli(path, payload):
    """Report whether a request needs the person's CLI identity."""
    if isinstance(payload, dict) and str(payload.get("platform", "")).strip().lower() == "cli":
        return True
    route, _, query = path.partition("?")
    return route in CLI_ONLY_PATHS or "platform=cli" in query.split("&")


class UnixConnection(http.client.HTTPConnection):
    """HTTP over the daemon's local control socket."""

    def __init__(self, socket_path, timeout):
        super().__init__("localhost", timeout=timeout)
        self.socket_path = socket_path

    def connect(self):
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        sock.settimeout(self.timeout)
        sock.connect(self.socket_path)
        self.sock = sock


def fetch(base, token, path, payload=None, socket_path=None, timeout=35):
    """Send one request and return (status, body bytes)."""
    headers = {"Authorization": "Bearer " + token}
    body = None
    method = "GET"
    if payload is not None:
        headers["Content-Type"] = "application/json"
        body = json.dumps(payload).encode()
        method = "POST"
    if claims_cli(path, payload):
        if not socket_path:
            raise RuntimeError(
                "this request claims CLI identity, which only the daemon's local control "
                "socket carries, but no socket path is known")
        connection = UnixConnection(socket_path, timeout)
        try:
            connection.request(method, path, body=body, headers=headers)
            response = connection.getresponse()
            return response.status, response.read()
        finally:
            connection.close()
    request = urllib.request.Request(base + path, data=body, headers=headers, method=method)
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            return response.status, response.read()
    except urllib.error.HTTPError as error:
        return error.code, error.read()


def request_json(base, token, path, payload=None, socket_path=None, timeout=35):
    """Send one request and return (status, decoded JSON object or {})."""
    status, data = fetch(base, token, path, payload, socket_path, timeout)
    try:
        return status, json.loads(data.decode())
    except (ValueError, UnicodeError):
        return status, {}


def daemon_socket(base, token):
    """Ask a running daemon, over TCP, where its local control socket is."""
    _, body = request_json(base, token, "/v1/gateway/status")
    path = (body.get("runtime") or {}).get("local_control_socket", "")
    if not path:
        raise RuntimeError("the daemon reports no local control socket")
    return path
