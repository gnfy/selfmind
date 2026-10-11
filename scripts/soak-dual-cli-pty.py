#!/usr/bin/env python3
"""Exercise two actual CLI terminals against an isolated local fake provider.

Run: PYTHONDONTWRITEBYTECODE=1 python3 scripts/soak-dual-cli-pty.py
Requires Go and POSIX PTYs (macOS/Linux/WSL). No credentials or real IM are used.
This proves client/daemon mechanics, not model judgement or terminal graphics.
"""

import codecs
import argparse
import errno
import fcntl
import json
import os
from pathlib import Path
import pty
import re
import secrets
import select
import signal
import socket
import sqlite3
import struct
import subprocess
import sys
import tempfile
import termios
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parent))
import soak_http  # noqa: E402


LABELS = ("PTYALPHA", "PTYBETA", "PTYCANCELAPPROVAL", "PTYCANCELCLARIFY",
          "PTYINPUTAPPROVAL", "PTYINPUTCLARIFY")
ALPHA_FINAL = "ALPHAFINALSTART\n\n" + "\n".join(
    f"ALPHALINE{i:02d}: This is the full committed answer for the originating terminal; synthetic evidence retained."
    for i in range(18)) + "\n\nALPHAFINALEND"
BETA_FINAL = "BETAFINALSTART\n\nThe independent read completed.\n\nBETAFINALEND"


def tool_call(call_id, name, args):
    return {"id": call_id, "type": "function",
            "function": {"name": name, "arguments": json.dumps(args)}}


class FakeModel(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    requests = []
    lock = threading.Lock()
    finish_beta = threading.Event()
    beta_held = threading.Event()

    def log_message(self, *_):
        pass

    def do_POST(self):
        payload = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))))
        with self.lock:
            self.requests.append(payload)
        messages = payload.get("messages", [])
        names = {(tool.get("function") or {}).get("name") for tool in payload.get("tools", [])}
        if "selfmind_model_check" in names:
            calls = [] if any(m.get("role") == "tool" for m in messages) else [
                tool_call("probe", "selfmind_model_check", {"value": "ping"})]
            return self.answer(payload, "OK" if not calls else "", calls)
        paired = {m.get("tool_call_id") for m in messages if m.get("role") == "tool"}
        # A stale clarification chooser changes ordinary "1" into its first
        # choice ("plain"). Probe that observable keyboard arbitration rather
        # than accepting a word that behaves the same in both UI modes.
        if "PTYDIGITACCEPTED-finish" in paired:
            return self.answer(payload, "PTYDIGITACCEPTEDDONE", [])
        last_user = next((str(m.get("content", "")) for m in reversed(messages)
                          if m.get("role") == "user"), "")
        last_user = last_user.split("[/SelfMind daemon context]\n\n", 1)[-1].strip()
        if last_user == "1":
            return self.answer(payload, "", [tool_call("PTYDIGITACCEPTED-finish", "finish_run", {
                "status": "done", "summary": "Ordinary digit input accepted."})])
        label = next((label for m in reversed(messages) if m.get("role") == "user"
                      for label in LABELS if re.search(r"\b" + label + r"\b", str(m.get("content", "")))), "")
        if not label:
            return self.answer(payload, "Synthetic auxiliary result.", [])
        if label + "-finish" in paired:
            final = ALPHA_FINAL if label == "PTYALPHA" else BETA_FINAL if label == "PTYBETA" else label + "DONE"
            return self.answer(payload, final, [], hold=label == "PTYBETA")
        if label == "PTYCANCELCLARIFY" and "clarify-prepare" in paired and "cancel-question" not in paired:
            return self.answer(payload, "", [tool_call("cancel-question", "clarify", {
                "question": "CLARIFYPANELTOKEN: choose the synthetic format", "choices": ["plain", "json"]})])
        if paired:
            return self.answer(payload, "", [tool_call(label + "-finish", "finish_run", {
                "status": "done", "summary": "Alpha complete." if label == "PTYALPHA" else "Beta complete." if label == "PTYBETA" else "Input accepted.",
                "done": ["Synthetic local operation completed."]})])
        if label == "PTYALPHA":
            return self.answer(payload, "ALPHALIVESTREAM: read before the requested write.", [
                tool_call("alpha-read", "read_file", {"path": "input.txt"}),
                tool_call("alpha-write", "write_file", {"path": "receipt.txt", "content": "approved synthetic effect"})])
        if label == "PTYBETA":
            return self.answer(payload, "BETALIVESTREAM: independent file read.", [
                tool_call("beta-read", "read_file", {"path": "input.txt"})])
        if label == "PTYCANCELAPPROVAL":
            return self.answer(payload, "", [tool_call("cancel-write", "write_file", {
                "path": "cancelled.txt", "content": "must never be written"})])
        if label == "PTYCANCELCLARIFY":
            # The question must start in an asynchronous foreground continuation.
            # A synchronous parent's HTTP completion clears its chooser already;
            # using that path would miss stale panels on daemon-resumed work.
            return self.answer(payload, "", [tool_call("clarify-prepare", "write_file", {
                "path": "prepare.txt", "content": "approved preparation before a question"})])
        return self.answer(payload, "", [tool_call(label + "-finish", "finish_run", {
            "status": "done", "summary": "Input accepted."})])

    def answer(self, payload, content, calls, hold=False):
        reason = "tool_calls" if calls else "stop"
        if not payload.get("stream"):
            body = json.dumps({"choices": [{"message": {"role": "assistant", "content": content,
                                "tool_calls": calls}, "finish_reason": reason}]}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Connection", "close")
        self.end_headers()

        def frame(delta, finish=None):
            data = {"choices": [{"index": 0, "delta": delta, "finish_reason": finish}]}
            self.wfile.write(("data: " + json.dumps(data) + "\n\n").encode())
            self.wfile.flush()

        try:
            for offset in range(0, len(content), 80):
                frame({"content": content[offset:offset + 80]})
                if hold and offset == 0:
                    self.beta_held.set()
                    if not self.finish_beta.wait(30):
                        raise RuntimeError("beta terminal was not released by the harness")
                time.sleep(0.02)
            if calls:
                frame({"tool_calls": [dict(call, index=i) for i, call in enumerate(calls)]})
            frame({}, reason)
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass


class Terminal:
    """Actual process attached to a PTY; capture bytes, not a mocked UI model."""

    def __init__(self, binary, config, workspace, channel, env):
        self.master, slave = pty.openpty()
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 40, 140, 0, 0))
        self.raw = bytearray()
        self.lock = threading.Lock()
        self.closed = threading.Event()
        child_env = dict(env, TERM="xterm-256color", NO_COLOR="1", SELFMIND_CHANNEL=channel)
        self.process = subprocess.Popen([str(binary), "--config", str(config), "--resume", channel],
                                        cwd=workspace, env=child_env, stdin=slave, stdout=slave,
                                        stderr=slave, start_new_session=True)
        os.close(slave)
        self.reader = threading.Thread(target=self.read, daemon=True)
        self.reader.start()

    def read(self):
        decoder = codecs.getincrementaldecoder("utf-8")("replace")
        while not self.closed.is_set():
            try:
                if not select.select([self.master], [], [], 0.1)[0]:
                    continue
                data = os.read(self.master, 65536)
                if not data:
                    break
                with self.lock:
                    self.raw.extend(data)
                    if len(self.raw) > 4 * 1024 * 1024:
                        raise RuntimeError("terminal output exceeded the harness bound")
                # Bubble Tea can query terminal cursor/background. Replies only
                # emulate terminal capabilities; all UI behavior is the real CLI.
                chunk = decoder.decode(data)
                if "\x1b[6n" in chunk:
                    self.write(b"\x1b[1;1R")
                if "\x1b]11;?" in chunk:
                    self.write(b"\x1b]11;rgb:0000/0000/0000\x1b\\")
            except OSError as error:
                if error.errno not in (errno.EIO, errno.EBADF):
                    raise
                break

    def write(self, data):
        os.write(self.master, data)

    def submit(self, content):
        self.write(content.encode() + b"\r")

    def text(self):
        with self.lock:
            text = bytes(self.raw).decode("utf-8", "replace")
        text = re.sub(r"\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)", "", text)
        return re.sub(r"\x1b\[[0-?]*[ -/]*[@-~]", "", text)

    def close(self):
        self.closed.set()
        if self.process.poll() is None:
            os.killpg(self.process.pid, signal.SIGTERM)
            try:
                self.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                os.killpg(self.process.pid, signal.SIGKILL)
                self.process.wait(timeout=5)
        os.close(self.master)
        self.reader.join(timeout=1)


def until(predicate, seconds, label):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        value = predicate()
        if value:
            return value
        time.sleep(0.1)
    raise RuntimeError(label)


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def isolated_environment(root, port, token):
    env = {key: value for key, value in os.environ.items()
           if key in ("PATH", "TMPDIR", "LANG", "TZ") or key.startswith("LC_")}
    # HOME is removed only from child environments, never reassigned. The CLI
    # crash-report fallback is then in its temporary cwd, not the user's home.
    env.pop("HOME", None)
    env.update({"SELFMIND_WORKERS": "2", "SELF_STORAGE_DATA_DIR": str(root / "data"),
                "SELF_GATEWAY_ADDR": f"127.0.0.1:{port}", "SELF_GATEWAY_URL": f"http://127.0.0.1:{port}",
                "SELF_DAEMON_ADDR": f"127.0.0.1:{port}", "SELF_DAEMON_URL": f"http://127.0.0.1:{port}",
                "SELF_GATEWAY_TOKEN": token, "SELF_DAEMON_TOKEN": token, "SELF_TENANT_ID": "default",
                "SELF_GATEWAY_MAX_ACTIVE_WORK_RUNS": "2", "SELF_GATEWAY_WEIXIN_ENABLED": "false",
                "SELF_GATEWAY_WECHAT_ENABLED": "false", "SELF_GATEWAY_FEISHU_ENABLED": "false",
                "SELF_GATEWAY_QQ_ENABLED": "false", "SELF_GATEWAY_TELEGRAM_TOKEN": "",
                "SELF_CRON_ENABLED": "false", "SELF_UPDATES_ENABLED": "false",
                "SELF_HISTORY_PERSISTENCE": "none", "PYTHONDONTWRITEBYTECODE": "1"})
    return env


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, default=Path(__file__).resolve().parent.parent,
                        help="read-only source tree to build (defaults to this script's repository)")
    source = parser.parse_args().source.resolve()
    model = ThreadingHTTPServer(("127.0.0.1", 0), FakeModel)
    threading.Thread(target=model.serve_forever, daemon=True).start()
    try:
        # Execution leases cannot live beneath /tmp because the sandbox binds
        # its private temp directory there. This owned directory is removed.
        with tempfile.TemporaryDirectory(prefix="selfmind-dual-pty-", dir=Path.home()) as temporary:
            root = Path(temporary)
            binary = root / "selfmind"
            subprocess.run(["go", "build", "-o", str(binary), "./cmd/selfmind"], cwd=source, check=True)
            config = root / "config.yaml"
            config.write_text(f"""providers:
  custom:
    pty-test:
      base_url: http://127.0.0.1:{model.server_port}/v1
      protocol: openai-compatible
      auth: none
models:
  primary:
    provider: pty-test
    model: pty-test-model
  auxiliary:
    enabled: false
evolution:
  skills_dir: {root / 'skills'}
cron:
  enabled: false
history:
  persistence: none
updates:
  enabled: false
tui:
  theme: mono
""")
            config.chmod(0o600)
            (root / "data").mkdir()
            workspaces = [root / "alpha", root / "beta"]
            for workspace in workspaces:
                workspace.mkdir()
                (workspace / "input.txt").write_text("before parking: synthetic local evidence")
            port, token = free_port(), secrets.token_urlsafe(24)
            env = isolated_environment(root, port, token)
            terminals = []
            with (root / "daemon.log").open("w") as log:
                daemon = subprocess.Popen([str(binary), "--config", str(config), "gateway", "run",
                                           "--addr", f"127.0.0.1:{port}"], cwd=root, env=env,
                                          stdout=log, stderr=subprocess.STDOUT)
                db = None
                try:
                    base = f"http://127.0.0.1:{port}"
                    def healthy():
                        if daemon.poll() is not None:
                            raise RuntimeError("isolated daemon exited: " + (root / "daemon.log").read_text()[-2000:])
                        try:
                            return soak_http.request_json(base, token, "/v1/gateway/status")[1].get("state") == "running"
                        except (OSError, ValueError):
                            return False
                    until(healthy, 40, "isolated daemon did not start")
                    control_socket = soak_http.daemon_socket(base, token)
                    identities = []
                    for workspace in workspaces:
                        code, reply = soak_http.request_json(base, token, "/v1/workspaces/register", {
                            "platform": "cli", "platform_user_id": "local", "local_path": str(workspace),
                            "name": workspace.name}, control_socket)
                        if code != 200:
                            raise RuntimeError("temporary workspace registration failed: " + repr(reply))
                        identities.append(reply["workspace"])
                    (root / "onboarding.json").write_text(json.dumps({
                        "version": 2, "background_mode": "on-demand", "gateway_verified_at": "2026-10-01T00:00:00Z",
                        "workspace_id": identities[0]["id"], "workspace_name": "alpha", "workspace_path": str(workspaces[0]),
                        "workspace_trusted": True, "approval_mode": "read-only", "first_task_completed": True}))
                    db = sqlite3.connect(f"file:{root / 'data' / 'control.db'}?mode=ro", uri=True)
                    for workspace, channel in zip(workspaces, ("pty-alpha", "pty-beta")):
                        terminal = Terminal(binary, config, workspace, channel, env)
                        terminals.append(terminal)
                        until(lambda: "Ask SelfMind" in terminal.text(), 15, "actual CLI did not reach its composer: " + channel)
                        terminal.submit("/mode read-only")
                        until(lambda: re.search(r"Approval mode set to:? read-only", terminal.text()), 5, "CLI mode command did not complete")
                    alpha, beta = terminals
                    alpha.submit("PTYALPHA read input.txt then write receipt.txt after approval")
                    approval = until(lambda: db.execute("SELECT id,run_id FROM approval_requests WHERE requested_channel='pty-alpha' AND status='pending' AND waiter_state='parked'").fetchone(), 15, "alpha did not checkpoint and park its approval")
                    until(lambda: "ALPHALIVESTREAM" in alpha.text(), 5, "original CLI lost its live stream")
                    until(lambda: "Other session:" in beta.text(), 5, "other CLI lost the concise lifecycle notice")
                    beta.submit("new PTYBETA independently read input.txt")
                    until(lambda: FakeModel.beta_held.is_set() and "BETALIVESTREAM" in beta.text(), 15,
                          "other CLI was blocked by alpha's panel")
                    if db.execute("SELECT status FROM approval_requests WHERE id=?", (approval[0],)).fetchone()[0] != "pending":
                        raise RuntimeError("other CLI's ordinary keyboard input answered alpha's approval")
                    (workspaces[0] / "input.txt").write_text("after parking: changed local evidence")
                    beta.submit("/approve " + approval[0])
                    until(lambda: "ALPHAFINALEND" in alpha.text(), 20, "exact answer did not deliver the original window's full final answer")
                    if "ALPHALIVESTREAM" in beta.text() or "ALPHAFINALSTART" in beta.text() or "ALPHAFINALEND" in beta.text():
                        raise RuntimeError("alpha's detail stream or final answer leaked into beta")
                    FakeModel.finish_beta.set()
                    until(lambda: "BETAFINALEND" in beta.text() and db.execute(
                        "SELECT id FROM runs WHERE channel='pty-beta' AND input_summary LIKE 'new PTYBETA%' AND status='done'").fetchone(),
                        10, "beta final did not complete")
                    if "BETALIVESTREAM" in alpha.text() or "BETAFINALSTART" in alpha.text():
                        raise RuntimeError("beta's private stream leaked into alpha")
                    for index in range(18):
                        if f"ALPHALINE{index:02d}" not in alpha.text():
                            raise RuntimeError("original CLI truncated its full final answer")
                    if (workspaces[0] / "receipt.txt").read_text() != "approved synthetic effect" or (workspaces[1] / "receipt.txt").exists():
                        raise RuntimeError("approval continuation lost its frozen directory")
                    with FakeModel.lock:
                        resumed = [request for request in FakeModel.requests if any(m.get("tool_call_id") == "alpha-write" for m in request.get("messages", []))]
                    if not resumed or any("before parking" not in str(request["messages"]) or "after parking" in str(request["messages"]) for request in resumed):
                        raise RuntimeError("approval continuation replayed or discarded its prior read")
                    for kind, marker, probe in (("approval", "PTYCANCELAPPROVAL", "PTYINPUTAPPROVAL"),
                                                ("clarification", "PTYCANCELCLARIFY", "PTYINPUTCLARIFY")):
                        alpha.submit(marker + " request a synthetic human decision")
                        if kind == "clarification":
                            preparation = until(lambda: db.execute(
                                "SELECT a.id FROM approval_requests a JOIN runs r ON r.id=a.run_id "
                                "WHERE a.status='pending' AND a.waiter_state='parked' AND r.input_summary LIKE 'PTYCANCELCLARIFY%'").fetchone(),
                                15, "clarification preparation did not park")
                            beta.submit("/approve " + preparation[0])
                        table = "approval_requests" if kind == "approval" else "clarify_requests"
                        wait_sql = "waiter_state='parked'" if kind == "approval" else "parked_at>0"
                        ask = until(lambda: db.execute(f"SELECT id,run_id FROM {table} WHERE status='pending' AND {wait_sql}").fetchone(), 15, "cancellation probe did not park: " + kind)
                        if kind == "clarification":
                            until(lambda: "CLARIFYPANELTOKEN" in alpha.text() and "Options:" in alpha.text(), 5,
                                  "original CLI did not show the clarification chooser")
                        beta.submit("/stop " + ask[1])
                        until(lambda: db.execute("SELECT status FROM runs WHERE id=?", (ask[1],)).fetchone()[0] == "cancelled", 5, "exact parked human stop did not cancel: " + kind)
                        # Submit through the former asking window. A stale armed
                        # panel consumes these keys/Enter as an old human answer.
                        time.sleep(0.3)
                        next_input = "1" if kind == "clarification" else probe + " ordinary independent input"
                        expected = "PTYDIGITACCEPTEDDONE" if kind == "clarification" else probe + "DONE"
                        alpha.submit(next_input)
                        until(lambda: expected in alpha.text(), 10, "cancelled " + kind + " panel still owned the original CLI keyboard")
                        fresh = db.execute("SELECT id,resumes_run_id FROM runs WHERE channel='pty-alpha' AND input_summary=? ORDER BY started_at DESC LIMIT 1", (next_input,)).fetchone()
                        if not fresh or fresh[0] == ask[1] or fresh[1]:
                            raise RuntimeError("post-cancellation input did not start independent work: " + kind)
                    if (workspaces[0] / "cancelled.txt").exists():
                        raise RuntimeError("cancelled unapproved effect ran")
                    print(json.dumps({"result": "PASS", "real_cli_ptys": 2, "fake_provider": True,
                                      "original_stream_and_full_final": True, "other_window_lifecycle_only": True,
                                      "cross_window_answer_exact": True, "prior_results_and_frozen_root": True,
                                      "cancelled_panels_release_keyboard": ["approval", "clarification"]}))
                except Exception:
                    with FakeModel.lock:
                        print("synthetic provider calls: " + repr([
                            [m.get("tool_call_id") for m in request.get("messages", []) if m.get("role") == "tool"]
                            for request in FakeModel.requests]), file=sys.stderr)
                        print("synthetic provider last inputs: " + repr([
                            str(next((m.get("content") for m in reversed(request.get("messages", []))
                                      if m.get("role") == "user"), ""))[-160:]
                            for request in FakeModel.requests][-5:]), file=sys.stderr)
                    if db is not None:
                        print("isolated runs: " + repr(db.execute(
                            "SELECT channel,input_summary,status,resumes_run_id FROM runs ORDER BY started_at").fetchall()), file=sys.stderr)
                    for index, terminal in enumerate(terminals):
                        print(f"PTY {index} synthetic tail: {terminal.text()[-6000:]}", file=sys.stderr)
                    print("isolated daemon tail: " + (root / "daemon.log").read_text()[-2000:], file=sys.stderr)
                    raise
                finally:
                    FakeModel.finish_beta.set()
                    for terminal in terminals:
                        terminal.close()
                    if db is not None:
                        db.close()
                    daemon.terminate()
                    try:
                        daemon.wait(timeout=8)
                    except subprocess.TimeoutExpired:
                        daemon.kill()
                        daemon.wait(timeout=5)
    finally:
        model.shutdown()
        model.server_close()


if __name__ == "__main__":
    main()
