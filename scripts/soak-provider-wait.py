#!/usr/bin/env python3
"""Exercise a real daemon's durable 429 wait across restart with a local model.

No user daemon, credentials, provider, or workspace is changed. The fake
OpenAI-compatible endpoint returns one 429, then a normal answer. Run from the
repository with: python3 scripts/soak-provider-wait.py
"""

import json
import os
from pathlib import Path
import secrets
import socket
import sqlite3
import subprocess
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import urllib.error
import urllib.request


class ModelHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    requests = []
    work_requests = []
    lock = threading.Lock()

    def log_message(self, *_):
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        payload = json.loads(self.rfile.read(length))
        with self.lock:
            self.requests.append(payload)
            probe = any((tool.get("function") or {}).get("name") == "selfmind_model_check"
                        for tool in payload.get("tools", []))
            if not probe:
                self.work_requests.append(payload)
            ordinal = len(self.work_requests)
        if probe:
            if any(message.get("role") == "tool" for message in payload.get("messages", [])):
                body = b'{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}]}'
            else:
                body = (b'{"choices":[{"message":{"content":null,"tool_calls":['
                        b'{"id":"check-1","type":"function","function":{'
                        b'"name":"selfmind_model_check","arguments":"{\\"value\\":\\"ping\\"}"}}]},'
                        b'"finish_reason":"tool_calls"}]}')
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
        elif ordinal == 1:
            body = b'{"error":{"message":"temporary rate limit","type":"rate_limit"}}'
            self.send_response(429)
            self.send_header("Content-Type", "application/json")
            self.send_header("Retry-After", "8")
        elif payload.get("stream"):
            body = (b'data: {"choices":[{"index":0,"delta":{"content":"Recovered."},'
                    b'"finish_reason":null}]}\n\n'
                    b'data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}\n\n'
                    b'data: [DONE]\n\n')
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
        else:
            body = b'{"choices":[{"message":{"content":"Recovered."},"finish_reason":"stop"}]}'
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(body)


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def request(base, token, path, payload=None):
    headers = {"Authorization": "Bearer " + token}
    if payload is not None:
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(base + path,
                                 data=json.dumps(payload).encode() if payload is not None else None,
                                 headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=20) as response:
            return response.status, json.load(response)
    except urllib.error.HTTPError as error:
        return error.code, json.loads(error.read())


def until(predicate, seconds, label):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        result = predicate()
        if result:
            return result
        time.sleep(0.1)
    raise RuntimeError(label)


def main():
    source = Path(__file__).resolve().parent.parent
    model = ThreadingHTTPServer(("127.0.0.1", 0), ModelHandler)
    thread = threading.Thread(target=model.serve_forever, daemon=True)
    thread.start()
    try:
        with tempfile.TemporaryDirectory(prefix="selfmind-provider-wait-", dir=Path.home()) as temporary:
            root = Path(temporary)
            binary = root / "selfmind"
            subprocess.run(["go", "build", "-o", str(binary), "./cmd/selfmind"], cwd=source, check=True)
            config = root / "config.yaml"
            config.write_text(f"""providers:
  custom:
    wait-test:
      base_url: http://127.0.0.1:{model.server_port}/v1
      protocol: openai-compatible
      auth: none
models:
  primary:
    provider: wait-test
    model: wait-test-model
  auxiliary:
    enabled: false
""")
            data_dir = root / "data"
            data_dir.mkdir()
            workspace = root / "workspace"
            workspace.mkdir()
            port = free_port()
            token = secrets.token_urlsafe(24)
            env = os.environ.copy()
            env.update({
                "SELFMIND_WORKERS": "2", "SELF_STORAGE_DATA_DIR": str(data_dir),
                "SELF_GATEWAY_ADDR": f"127.0.0.1:{port}", "SELF_GATEWAY_TOKEN": token,
                "SELF_DAEMON_TOKEN": "", "SELF_GATEWAY_MAX_ACTIVE_WORK_RUNS": "2",
                "SELF_GATEWAY_WEIXIN_ENABLED": "false", "SELF_GATEWAY_WECHAT_ENABLED": "false",
                "SELF_GATEWAY_FEISHU_ENABLED": "false", "SELF_GATEWAY_QQ_ENABLED": "false",
                "SELF_GATEWAY_TELEGRAM_TOKEN": "",
            })
            log_path = root / "gateway.log"
            with log_path.open("w") as log:
                def launch():
                    return subprocess.Popen([str(binary), "--config", str(config), "gateway", "run",
                                             "--addr", f"127.0.0.1:{port}"], cwd=source, env=env,
                                            stdout=log, stderr=subprocess.STDOUT)

                process = launch()
                base = f"http://127.0.0.1:{port}"
                def healthy():
                    if process.poll() is not None:
                        raise RuntimeError("isolated daemon exited: " + log_path.read_text()[-2000:])
                    try:
                        return request(base, token, "/v1/gateway/status")[1].get("state") == "running"
                    except (OSError, ValueError):
                        return False
                try:
                    until(healthy, 30, "isolated daemon did not start")
                    code, accepted = request(base, token, "/v1/message", {
                        "platform": "cli", "platform_user_id": "local", "channel": "wait-a",
                        "client_cwd": str(workspace), "content": "Answer the first request in one sentence.",
                        "async": True})
                    if code != 200 or not accepted.get("accepted"):
                        raise RuntimeError("first work was not accepted: " + repr(accepted))
                    database = sqlite3.connect(data_dir / "control.db")
                    def parked():
                        row = database.execute("SELECT id FROM runs WHERE channel='wait-a' "
                                               "AND status='waiting_external'").fetchone()
                        if not row:
                            return None
                        queue = database.execute("SELECT status FROM task_queue WHERE "
                                                 "idempotency_key=?", ("provider-wait:" + row[0],)).fetchone()
                        return row[0] if queue and queue[0] == "queued" else None
                    try:
                        parent = until(parked, 15, "429 did not park the exact Run and queue")
                    except RuntimeError:
                        print(json.dumps({"diagnostic": "park_failure",
                                          "runs": database.execute("SELECT channel,status FROM runs").fetchall(),
                                          "queue": database.execute("SELECT status,idempotency_key FROM task_queue").fetchall(),
                                          "model_requests": len(ModelHandler.requests),
                                          "work_requests": len(ModelHandler.work_requests),
                                          "daemon_log_tail": log_path.read_text()[-1600:]}), flush=True)
                        raise
                    process.kill()
                    process.wait(timeout=5)
                    process = launch()
                    until(healthy, 30, "daemon did not restart")
                    code, second = request(base, token, "/v1/message", {
                        "platform": "cli", "platform_user_id": "local", "channel": "wait-b",
                        "client_cwd": str(workspace), "content": "Answer the second request in one sentence.",
                        "async": True})
                    if code != 200 or not second.get("accepted"):
                        raise RuntimeError("second work was not accepted after restart: " + repr(second))
                    def settled():
                        rows = database.execute("SELECT id,status,resumes_run_id FROM runs WHERE channel='wait-a'").fetchall()
                        queue = database.execute("SELECT status,run_id FROM task_queue WHERE "
                                                 "idempotency_key=?", ("provider-wait:" + parent,)).fetchone()
                        children = [row for row in rows if row[2] == parent]
                        return children if queue and queue[0] == "done" and children and children[0][1] == "done" else None
                    children = until(settled, 40, "parked Run did not finish its exact child after restart")

                    started = json.loads(database.execute(
                        "SELECT payload_json FROM task_events WHERE run_id=? AND type='run.started'",
                        (children[0][0],)).fetchone()[0])
                    if started.get("origin") != "provider_wait" or started.get("presentation") != "foreground":
                        raise RuntimeError("provider continuation lost its originating foreground: " + repr(started))
                    saved = database.execute("SELECT content FROM channel_messages WHERE id=?",
                                             ("msg_run_" + children[0][0] + "_assistant",)).fetchone()[0]
                    for session in ("wait-a", "wait-b"):
                        url = (base + "/v1/events/stream?platform=cli&platform_user_id=local"
                               + "&session=" + session + "&cursor=0&once=true")
                        replay = urllib.request.Request(url, headers={"Authorization": "Bearer " + token})
                        with urllib.request.urlopen(replay, timeout=10) as response:
                            events = [json.loads(line[6:]) for line in response.read().decode().splitlines()
                                      if line.startswith("data: ")]
                        terminal = next(event for event in events if event.get("run_id") == children[0][0]
                                        and event.get("type") == "run.finished")
                        final_answer = terminal.get("payload", {}).get("final_answer")
                        if session == "wait-a" and final_answer != saved:
                            raise RuntimeError("reconnect lost the committed final answer")
                        if session == "wait-b" and final_answer is not None:
                            raise RuntimeError("another CLI received the full final answer")
                    with ModelHandler.lock:
                        prompts = [r.get("messages", []) for r in ModelHandler.work_requests]
                    if len(prompts) < 3 or any("Continue this exact work from its durable model-call checkpoint"
                                               in str(messages) for messages in prompts):
                        raise RuntimeError("provider retry included internal scheduling text")
                    print(json.dumps({"result": "PASS", "parked_run": parent,
                                      "exact_child": children[0][0], "model_requests": len(prompts),
                                      "restart": True, "source_queue": "done",
                                      "foreground_preserved": True, "answer_replay_isolated": True}))
                    database.close()
                finally:
                    process.terminate()
                    try:
                        process.wait(timeout=10)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=5)
    finally:
        model.shutdown()
        model.server_close()


if __name__ == "__main__":
    main()
