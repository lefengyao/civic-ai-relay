"""End-to-end heartbeat checks with a local fake upstream and optional Nginx."""

import base64
import http.client
import http.server
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.request


ROOT = Path(__file__).resolve().parents[2]
ADMIN_KEY = "test-admin-key"


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def request_json(base, path, payload=None, admin=False, method=None):
    headers = {"X-Admin-Key": ADMIN_KEY} if admin else {}
    method = method or ("POST" if payload is not None else "GET")
    data = None
    if payload is not None:
        headers["Content-Type"] = "application/json"
        data = json.dumps(payload).encode()
    request = urllib.request.Request(base + path, data=data, headers=headers, method=method)
    with urllib.request.urlopen(request, timeout=3) as response:
        return json.load(response)


class FakeUpstream(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        if self.path != "/v1/chat/completions":
            self.send_error(404)
            return
        self.rfile.read(int(self.headers.get("Content-Length", "0")))
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        self.wfile.flush()
        time.sleep(0.2)
        try:
            self.wfile.write(b'data: {"choices":[{"delta":{"content":"ok"}}]}\n\n')
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass

    def log_message(self, *_args):
        pass


class GatewayHeartbeatTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory(prefix="civic-stream-test-")
        cls.directory = Path(cls.temp.name)
        cls.upstream = http.server.ThreadingHTTPServer(("127.0.0.1", 0), FakeUpstream)
        cls.upstream_thread = threading.Thread(target=cls.upstream.serve_forever, daemon=True)
        cls.upstream_thread.start()
        cls.gateway_port = free_port()
        cls.gateway_url = f"http://127.0.0.1:{cls.gateway_port}"
        config = cls.directory / "relay.env"
        config.write_text(
            "\n".join(
                [
                    f"ADMIN_API_KEY={ADMIN_KEY}",
                    "RELAY_ENCRYPTION_KEY=" + base64.b64encode(bytes(32)).decode(),
                    f"DB_PATH={cls.directory / 'relay.db'}",
                    "HOST=127.0.0.1",
                    f"PORT={cls.gateway_port}",
                    "GROUP_MONITOR_INTERVAL=0",
                    "MODEL_AUTO_SYNC=false",
                    "MAX_STREAM_DURATION=0",
                    "UPSTREAM_HEADER_TIMEOUT=1s",
                    "SSE_HEARTBEAT_INTERVAL=10ms",
                ]
            )
            + "\n",
            encoding="utf-8",
        )
        binary = cls.directory / ("civic-relay.exe" if os.name == "nt" else "civic-relay")
        try:
            subprocess.run(["go", "build", "-o", str(binary), "./cmd/civic-relay"], cwd=ROOT, check=True, timeout=90, capture_output=True)
            cls.gateway_log = (cls.directory / "gateway.log").open("w", encoding="utf-8")
            cls.gateway = subprocess.Popen(
                [str(binary)], cwd=ROOT, env={**os.environ, "CIVIC_RELAY_CONFIG_FILE": str(config)},
                stdout=cls.gateway_log, stderr=subprocess.STDOUT,
            )
            deadline = time.monotonic() + 8
            while time.monotonic() < deadline:
                if cls.gateway.poll() is not None:
                    raise RuntimeError("gateway exited before readiness")
                try:
                    request_json(cls.gateway_url, "/healthz")
                    break
                except (urllib.error.URLError, TimeoutError):
                    time.sleep(0.05)
            else:
                raise RuntimeError("gateway did not become ready")

            provider = request_json(cls.gateway_url, "/admin/api/providers", {
                "name": "fake", "base_url": f"http://127.0.0.1:{cls.upstream.server_port}", "api_key": "fake",
            }, admin=True)["data"]
            request_json(cls.gateway_url, "/admin/api/models", {
                "provider_id": provider["ID"], "public_name": "m", "upstream_name": "m",
                "input_price_microyuan": 1, "output_price_microyuan": 1, "enabled": True,
            }, admin=True)
            group = request_json(cls.gateway_url, "/admin/api/groups", {
                "name": "g", "provider_ids": [provider["ID"]],
            }, admin=True)["data"]
            cls.token = request_json(cls.gateway_url, "/admin/api/keys", {
                "name": "k", "max_concurrency": 1, "group_ids": [group["ID"]],
            }, admin=True)["token"]
        except Exception:
            cls.tearDownClass()
            raise

    @classmethod
    def tearDownClass(cls):
        for name in ("nginx", "gateway"):
            process = getattr(cls, name, None)
            if process is not None and process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)
        if hasattr(cls, "gateway_log"):
            cls.gateway_log.close()
        if hasattr(cls, "upstream"):
            cls.upstream.shutdown()
            cls.upstream.server_close()
            cls.upstream_thread.join(timeout=5)
        if hasattr(cls, "temp"):
            cls.temp.cleanup()

    @classmethod
    def stream(cls, base):
        request = urllib.request.Request(
            base + "/v1/chat/completions",
            data=b'{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}',
            headers={"Authorization": "Bearer " + cls.token, "Content-Type": "application/json"},
            method="POST",
        )
        started = time.monotonic()
        with urllib.request.urlopen(request, timeout=3) as response:
            lines = []
            first_comment_at = None
            while True:
                try:
                    line = response.readline().decode("utf-8")
                except (OSError, http.client.IncompleteRead) as exc:
                    partial = getattr(exc, "partial", b"")
                    if partial:
                        lines.append(partial.decode("utf-8", errors="replace"))
                    break
                if not line:
                    break
                lines.append(line)
                if line.startswith(":") and first_comment_at is None:
                    first_comment_at = time.monotonic() - started
                if "data: [DONE]" in line:
                    break
            return first_comment_at, "".join(lines), response.headers

    def test_direct_gateway_sends_heartbeat_while_upstream_thinks(self):
        first, body, headers = self.stream(self.gateway_url)
        self.assertIsNotNone(first, body)
        self.assertLess(first, 0.15, body)
        self.assertIn(": relay-heartbeat", body)
        self.assertIn("data: [DONE]", body)
        self.assertEqual(headers.get("X-Accel-Buffering"), "no")

    def test_nginx_short_idle_timeout_with_heartbeat(self):
        nginx = shutil.which("nginx")
        if nginx is None:
            self.skipTest("Nginx is not installed; run this proxy check in Linux release CI")
        port = free_port()
        proxy_url = f"http://127.0.0.1:{port}"
        config = self.directory / "nginx.conf"
        config.write_text(
            "events { worker_connections 64; }\n"
            "http { access_log off; server {\n"
            f"listen 127.0.0.1:{port};\n"
            "location / { proxy_http_version 1.1; proxy_set_header Connection \"\";\n"
            f"proxy_pass {self.gateway_url};\n"
            "proxy_buffering off; proxy_cache off; proxy_read_timeout 50ms; } } }\n",
            encoding="utf-8",
        )
        subprocess.run([nginx, "-t", "-p", str(self.directory), "-c", str(config)], check=True, timeout=5, capture_output=True)
        self.nginx = subprocess.Popen([nginx, "-p", str(self.directory), "-c", str(config), "-g", "daemon off; master_process off;"])
        deadline = time.monotonic() + 3
        while time.monotonic() < deadline:
            try:
                request_json(proxy_url, "/healthz")
                break
            except (urllib.error.URLError, TimeoutError):
                time.sleep(0.05)
        else:
            self.fail("Nginx did not become ready")
        first, body, _ = self.stream(proxy_url)
        self.assertIsNotNone(first, body)
        self.assertIn(": relay-heartbeat", body)
        self.assertIn("data: [DONE]", body)

        request_json(self.gateway_url, "/admin/api/config", {
            "settings": {"SSE_HEARTBEAT_INTERVAL": "0"},
        }, admin=True, method="PUT")
        _, disconnected, _ = self.stream(proxy_url)
        self.assertNotIn("data: [DONE]", disconnected)
        deadline = time.monotonic() + 2
        while time.monotonic() < deadline:
            rows = request_json(self.gateway_url, "/admin/api/requests", admin=True)["data"]
            if rows and rows[0]["status"] == "aborted":
                break
            time.sleep(0.02)
        else:
            self.fail(f"proxy disconnect was not settled as aborted: {rows[:1]}")
        _, retry, _ = self.stream(self.gateway_url)
        self.assertIn("data: [DONE]", retry)


if __name__ == "__main__":
    unittest.main()
