"""Exercise a dedicated, empty AgentENV cluster with at least two Conch nodes.

Install e2b==2.46.4 and websockets==15.0.1, then set E2B_API_KEY,
E2B_API_URL, E2B_SANDBOX_URL and E2B_TEMPLATE_ID. The guest template needs
Python 3 and the normal envd command dependencies. No guest Python packages
are required. Configure the same sandbox proxy domain on Gateway and nodes.
"""

import json
import os
import time
import uuid

import httpx
from e2b import PtySize, Sandbox, SandboxQuery
from websockets.sync.client import connect


# An ordinary guest application, installed through the public SDK file API.
APPLICATION = r'''
import base64, hashlib, json, struct, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from socketserver import TCPServer

class Server(ThreadingHTTPServer):
    def server_bind(self):
        # A test HTTP server does not need reverse DNS for its bind address.
        TCPServer.server_bind(self)
        self.server_name = "localhost"
        self.server_port = self.server_address[1]

class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def do_GET(self):
        if self.path == "/ws":
            key = self.headers["Sec-WebSocket-Key"]
            accept = base64.b64encode(hashlib.sha1((key +
                "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()).decode()
            self.send_response(101)
            self.send_header("Upgrade", "websocket")
            self.send_header("Connection", "Upgrade")
            self.send_header("Sec-WebSocket-Accept", accept)
            self.end_headers()
            while True:
                header = self.rfile.read(2)
                if len(header) != 2:
                    return
                opcode, length = header[0] & 15, header[1] & 127
                if length == 126:
                    length = struct.unpack("!H", self.rfile.read(2))[0]
                elif length == 127:
                    length = struct.unpack("!Q", self.rfile.read(8))[0]
                if length > 1048576:
                    return
                mask = self.rfile.read(4) if header[1] & 128 else b""
                data = self.rfile.read(length)
                if mask:
                    data = bytes(v ^ mask[i % 4] for i, v in enumerate(data))
                opcode = 10 if opcode == 9 else opcode
                if len(data) < 126:
                    size = bytes([len(data)])
                elif len(data) < 65536:
                    size = b"\x7e" + struct.pack("!H", len(data))
                else:
                    size = b"\x7f" + struct.pack("!Q", len(data))
                self.wfile.write(bytes([128 | opcode]) + size + data)
                self.wfile.flush()
                if opcode == 8:
                    return
        elif self.path == "/stream":
            self.close_connection = True
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Connection", "close")
            self.end_headers()
            for index in range(3):
                self.wfile.write(f"data: {index}\n\n".encode())
                self.wfile.flush()
                time.sleep(0.4)
        else:
            body = json.dumps({"path": self.path,
                "marker": self.headers.get("X-Conch-Marker")}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
    def log_message(self, *args):
        pass

Server(("0.0.0.0", 8080), Handler).serve_forever()
'''


def record(stage, **details):
    print(json.dumps({"stage": stage, **details}), flush=True)


def eventually(probe, accepts, description, timeout=20):
    deadline = time.monotonic() + timeout
    while True:
        value = probe()
        if accepts(value):
            return value
        if time.monotonic() >= deadline:
            raise AssertionError(f"{description}: {value}")
        time.sleep(0.2)


def idle(nodes):
    return all(
        node["sandboxCount"] == 0
        and node["sandboxStartingCount"] == 0
        and node["metrics"]["allocatedCPU"] == 0
        and node["metrics"]["allocatedMemoryBytes"] == 0
        for node in nodes
    )


def guest_checks(box):
    assert box.get_info().sandbox_id == box.sandbox_id
    result = box.commands.run(
        'printf "%s" "$CONCH_E2E_VALUE"; printf error-output >&2'
    )
    assert result.stdout == "from-create" and result.stderr == "error-output"
    assert result.exit_code == 0
    content = "Conch AgentENV 文件传输\n" * 512
    filename = "/tmp/e2e file %23.txt"
    box.files.write(filename, content)
    assert box.files.read(filename) == content
    assert "e2e file %23.txt" in [entry.name for entry in box.files.list("/tmp")]
    pty = box.pty.create(size=PtySize(rows=24, cols=80), timeout=30)
    box.pty.resize(pty.pid, size=PtySize(rows=35, cols=100))
    box.pty.send_stdin(pty.pid, b"printf 'PTY_OK\\n'; stty size; exit\n")
    output = []
    result = pty.wait(on_pty=output.append)
    assert result.exit_code == 0
    assert b"PTY_OK" in b"".join(output) and b"35 100" in b"".join(output)
    record("guest", sandbox_id=box.sandbox_id, commands=True, files=True, pty=True)


def proxy_checks(box, client, api_url):
    box.files.write("/tmp/conch-e2e-app.py", APPLICATION)
    application = box.commands.run(
        "python3 /tmp/conch-e2e-app.py >/tmp/conch-e2e-app.log 2>&1",
        background=True,
        timeout=0,
    )
    routing = {"e2b-sandbox-id": box.sandbox_id, "e2b-sandbox-port": "8080"}
    eventually(
        lambda: client.get("/app-ready", headers=routing),
        lambda response: response.status_code == 200,
        "guest HTTP application readiness",
    )
    host = box.get_host(8080)
    response = client.get(
        "/encoded%2Fpath?q=a%2Fb",
        headers={
            "Host": host,
            "X-Conch-Marker": "through-gateway",
            "e2b-sandbox-id": str(uuid.uuid4()),
            "e2b-sandbox-port": "1",
        },
    )
    response.raise_for_status()
    assert response.json() == {
        "path": "/encoded%2Fpath?q=a%2Fb", "marker": "through-gateway"
    }
    response = client.get("/sandboxes", headers={"Host": host})
    response.raise_for_status()
    assert response.json()["path"] == "/sandboxes"
    started = time.monotonic()
    first = None
    lines = []
    with client.stream("GET", "/stream", headers={"Host": host}) as response:
        response.raise_for_status()
        for line in response.iter_lines():
            if line:
                if first is None:
                    first = time.monotonic() - started
                lines.append(line)
    assert lines == ["data: 0", "data: 1", "data: 2"]
    assert first is not None and first < 0.8, "SSE buffered until stream completion"
    websocket_url = api_url.replace("https://", "wss://", 1).replace("http://", "ws://", 1)
    with connect(websocket_url + "/ws", additional_headers=routing, proxy=None) as websocket:
        for message in ["Conch WebSocket 文本", b"\x00\x01\xff"]:
            websocket.send(message)
            assert websocket.recv() == message
    application.disconnect()
    record("proxy", host_header_precedence=True, escaped_path=True,
           sse_first_seconds=first, websocket=True)


def main():
    for variable in ["E2B_API_KEY", "E2B_API_URL", "E2B_SANDBOX_URL", "E2B_TEMPLATE_ID"]:
        if not os.getenv(variable):
            raise SystemExit(f"Set {variable} before running this dedicated-cluster test")
    api_url = os.environ["E2B_API_URL"].rstrip("/")
    template = os.environ["E2B_TEMPLATE_ID"]
    suite = str(uuid.uuid4())
    boxes = []
    with httpx.Client(base_url=api_url, timeout=30,
                      headers={"X-API-Key": os.environ["E2B_API_KEY"]}) as client:
        def nodes():
            response = client.get("/nodes")
            response.raise_for_status()
            return response.json()

        initial = nodes()
        assert len(initial) >= 2 and idle(initial), "Use an empty cluster with at least two nodes"
        assert client.get("/sandboxes", headers={"X-API-Key": "invalid"}).status_code == 401
        assert client.post("/sandboxes", json={"templateID": template, "secure": True}).status_code == 501
        try:
            for _ in range(2):
                box = Sandbox.create(template, secure=False, timeout=300,
                                     metadata={"suite": suite},
                                     envs={"CONCH_E2E_VALUE": "from-create"},
                                     request_timeout=120)
                boxes.append(box)
                record("create", sandbox_id=box.sandbox_id)
            observed = eventually(nodes, lambda values: sum(n["sandboxCount"] for n in values) == 2
                                  and sum(n["sandboxCount"] > 0 for n in values) == 2,
                                  "two-node scheduling and heartbeat roster")
            for node in observed:
                if node["sandboxCount"]:
                    assert node["metrics"]["allocatedCPU"] > 0
                    assert node["metrics"]["allocatedMemoryBytes"] > 0
            paginator = Sandbox.list(query=SandboxQuery(metadata={"suite": suite}), limit=1)
            listed = []
            while paginator.has_next:
                listed.extend(s.sandbox_id for s in paginator.next_items())
            assert len(listed) == 2 and set(listed) == {box.sandbox_id for box in boxes}
            for box in boxes:
                guest_checks(box)
                # pause/connect are implemented; the E2B resume and fork
                # endpoints stay unimplemented.
                assert client.post(f"/sandboxes/{box.sandbox_id}/resume", json={}).status_code == 501
                assert client.post(f"/sandboxes/{box.sandbox_id}/fork", json={}).status_code == 501
                assert client.post(f"/sandboxes/{box.sandbox_id}/connect",
                                   json={"timeout": 600}).status_code == 200
                assert client.get(f"/sandboxes/{box.sandbox_id}").json()["state"] == "running"
                assert client.post(f"/sandboxes/{box.sandbox_id}/refreshes",
                                   json={"duration": 60}).status_code == 204
                assert client.post(f"/sandboxes/{box.sandbox_id}/timeout",
                                   json={"timeout": 600}).status_code == 204
            proxy_checks(boxes[0], client, api_url)
        finally:
            for box in boxes:
                box.kill()
        eventually(nodes, idle, "deleted sandbox roster and allocations return to zero")
        expiring = Sandbox.create(template, secure=False, timeout=2, request_timeout=120)
        try:
            eventually(lambda: client.get(f"/sandboxes/{expiring.sandbox_id}").status_code,
                       lambda status: status == 404, "initial timeout deletes sandbox", timeout=15)
        finally:
            expiring.kill()
        eventually(nodes, idle, "expired sandbox roster and allocations return to zero")
        record("complete", two_nodes=True, initial_timeout=True, ttl=True, unimplemented=True, api_key=True)


if __name__ == "__main__":
    main()
