import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shlex
import socketserver
import sqlite3
import subprocess
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


import sys
sys.path.insert(0, str(Path(__file__).resolve().parent))

spec = importlib.util.spec_from_file_location(
    "parallel_soak", Path(__file__).with_name("soak-parallel-runs.py"))
soak = importlib.util.module_from_spec(spec)
spec.loader.exec_module(soak)


class ConfigurationIsolationTest(unittest.TestCase):
    def test_startup_state_is_adjacent_to_the_isolated_config(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            owner = root / "owner"
            runtime = root / "runtime"
            owner.mkdir()
            runtime.mkdir()
            config = owner / "config.yaml"
            state = owner / "model-state.json"
            config.write_text("models:\n  primary:\n    model: owner-model\n")
            state.write_text('{"running":"owner-model"}')
            before = [hashlib.sha256(p.read_bytes()).digest()
                      for p in (config, state)]
            isolated = soak.isolate_configuration(config, runtime)
            self.assertEqual(isolated.read_bytes(), config.read_bytes())
            self.assertEqual(isolated.stat().st_mode & 0o777, 0o600)
            # Model-change persistence derives its state path from config,
            # independently of SELF_STORAGE_DATA_DIR.
            isolated.with_name("model-state.json").write_text(
                '{"running":"temporary-provider"}')
            self.assertEqual(before, [hashlib.sha256(p.read_bytes()).digest()
                                     for p in (config, state)])


class ProcessExecutionEvidenceTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.database = sqlite3.connect(":memory:")
        self.addCleanup(self.database.close)
        self.database.execute("CREATE TABLE task_events (run_id TEXT, type TEXT, "
                              "payload_json TEXT, created_at REAL, cursor INTEGER)")
        self.lineages = {"aster": ["aster-child"], "birch": ["birch"]}
        self.probes = {}
        for name in self.lineages:
            workspace = Path(self.directory.name) / name
            workspace.mkdir()
            self.probes[name] = soak.prepare_process_probe(workspace, name, duration=0.02)

    def evidence(self, name, mode, start, end, *, process=None, error=""):
        probe = self.probes[name]
        receipt = {"nonce": probe["nonce"], "pid": 321 + len(name),
                   "cwd": str(probe["workspace"]), "start_ns": start * 10**9,
                   "end_ns": end * 10**9}
        probe["receipt_path"].write_text(json.dumps(receipt))
        run_id = self.lineages[name][-1]
        call_id = name + "-call"
        started = {"tool": "terminal", "tool_call_id": call_id,
                   "args": json.dumps({"command": probe["command"]})}
        completed = {"tool": "terminal", "tool_call_id": call_id, "invoked": True,
                     "process": process if process is not None else
                     {"started": True, "exit_code": 0, "sandbox_mode": mode},
                     "result": soak.PROBE_RECEIPT_PREFIX + json.dumps(receipt),
                     "tool_read_only": False, "operation_classes": ["exec.in_turn"]}
        if error:
            completed["error"] = error
        # Both invocation intervals include the same approval/lane wait. Only
        # the independent receipt tells us when either process actually ran.
        for kind, payload, at in (("tool.started", started, 0),
                                  ("tool.completed", completed, 40)):
            self.database.execute("INSERT INTO task_events VALUES (?, ?, ?, ?, ?)",
                                  (run_id, kind, json.dumps(payload), at,
                                   self.database.execute("SELECT COUNT(*) FROM task_events").fetchone()[0]))

    def check(self, minimum_overlap_seconds=5):
        intervals = soak.process_intervals(self.database, self.lineages, self.probes)
        return soak.validate_process_intervals(intervals, minimum_overlap_seconds)

    def test_waiting_intervals_cannot_prove_isolated_process_overlap(self):
        self.evidence("aster", "isolated", 10, 20)
        self.evidence("birch", "isolated", 20, 30)
        with self.assertRaisesRegex(RuntimeError, "isolated.*overlap"):
            self.check()

    def test_serial_host_processes_pass_despite_overlapping_invocation_waits(self):
        self.evidence("aster", "host", 10, 20)
        self.evidence("birch", "host", 20, 30)
        self.assertEqual(self.check()["execution_policy"], "host-exclusive")

    def test_real_isolated_overlap_passes_for_changed_paths_and_child_runs(self):
        self.evidence("aster", "isolated", 10, 25)
        self.evidence("birch", "isolated", 15, 30)
        result = self.check()
        self.assertEqual(result["execution_policy"], "isolated-overlap")
        self.assertEqual(result["process_overlap_seconds"], 10)

    def test_host_overlap_with_host_or_isolated_is_refused(self):
        for other_mode in ("host", "isolated"):
            with self.subTest(other_mode=other_mode):
                self.database.execute("DELETE FROM task_events")
                self.evidence("aster", "host", 10, 25)
                self.evidence("birch", other_mode, 15, 30)
                with self.assertRaisesRegex(RuntimeError, "host.*overlap"):
                    self.check()

    def test_mixed_modes_accept_only_host_exclusion(self):
        self.evidence("aster", "host", 10, 20)
        self.evidence("birch", "isolated", 20, 30)
        self.assertEqual(self.check()["execution_policy"], "mixed-host-exclusive")

    def test_third_isolated_process_must_share_the_actual_overlap(self):
        workspace = Path(self.directory.name) / "cedar"
        workspace.mkdir()
        self.lineages["cedar"] = ["cedar"]
        self.probes["cedar"] = soak.prepare_process_probe(workspace, "cedar", duration=0.02)
        self.evidence("aster", "isolated", 10, 25)
        self.evidence("birch", "isolated", 15, 30)
        self.evidence("cedar", "isolated", 25, 40)
        with self.assertRaisesRegex(RuntimeError, "isolated.*overlap"):
            self.check()

    def test_read_only_observation_and_pre_body_parking_are_not_execution(self):
        self.evidence("aster", "host", 10, 20)
        self.evidence("birch", "host", 20, 30)
        for cursor, payload in enumerate((
                {"tool": "read_file", "tool_call_id": "read", "invoked": True,
                 "tool_read_only": True, "tool_category": "filesystem", "operation_classes": ["observe"]},
                {"tool": "terminal", "tool_call_id": "parked", "invoked": False}), 99):
            self.database.execute("INSERT INTO task_events VALUES (?, ?, ?, ?, ?)",
                                  ("aster-child", "tool.completed", json.dumps(payload), 5, cursor))
        self.assertEqual(self.check()["execution_policy"], "host-exclusive")

    def test_success_requires_typed_started_exit_and_sandbox_facts(self):
        for process in ({}, {"started": True, "sandbox_mode": "host"},
                        {"started": False, "exit_code": 0, "sandbox_mode": "host"},
                        {"started": True, "exit_code": False, "sandbox_mode": "host"},
                        {"started": True, "exit_code": 1, "sandbox_mode": "host"},
                        {"started": True, "exit_code": 0, "sandbox_mode": "auto"}):
            with self.subTest(process=process):
                self.database.execute("DELETE FROM task_events")
                self.evidence("aster", "host", 10, 20, process=process)
                self.evidence("birch", "host", 20, 30)
                with self.assertRaises(RuntimeError):
                    self.check()

    def test_probe_is_bound_to_exact_successful_call_and_workspace(self):
        for field, value in (("tool_call_id", "different-call"),
                             ("result", "the model says it succeeded"),
                             ("error", "process failed"), ("invoked", False)):
            with self.subTest(field=field):
                self.database.execute("DELETE FROM task_events")
                self.evidence("aster", "host", 10, 20)
                self.evidence("birch", "host", 20, 30)
                cursor, raw = self.database.execute(
                    "SELECT cursor, payload_json FROM task_events "
                    "WHERE run_id='aster-child' AND type='tool.completed'").fetchone()
                payload = json.loads(raw)
                payload[field] = value
                self.database.execute("UPDATE task_events SET payload_json=? WHERE cursor=?",
                                      (json.dumps(payload), cursor))
                with self.assertRaises(RuntimeError):
                    self.check()
        self.database.execute("DELETE FROM task_events")
        self.evidence("aster", "host", 10, 20)
        self.evidence("birch", "host", 20, 30)
        path = self.probes["aster"]["receipt_path"]
        receipt = json.loads(path.read_text())
        receipt["cwd"] = str(self.probes["birch"]["workspace"])
        path.write_text(json.dumps(receipt))
        with self.assertRaisesRegex(RuntimeError, "receipt"):
            self.check()

    def test_extra_mutation_or_unmeasured_terminal_cannot_report_pass(self):
        for tool, classes in (("write_file", ["write"]), ("patch", ["write"]),
                              ("terminal", ["observe"]), ("execute_code", ["exec.in_turn"])):
            with self.subTest(tool=tool):
                self.database.execute("DELETE FROM task_events")
                self.evidence("aster", "host", 10, 20)
                self.evidence("birch", "host", 20, 30)
                payload = {"tool": tool, "tool_call_id": "unmeasured", "invoked": True,
                           "tool_read_only": False, "operation_classes": classes}
                self.database.execute("INSERT INTO task_events VALUES (?, ?, ?, ?, ?)",
                                      ("birch", "tool.completed", json.dumps(payload), 22, 99))
                with self.assertRaisesRegex(RuntimeError, "unmeasured"):
                    self.check()

    def test_unfinished_or_completed_external_mutations_are_unmeasured(self):
        for kind in ("tool.started", "tool.completed"):
            for classes in (["write", "exec.in_turn"], ["network"]):
                with self.subTest(kind=kind, classes=classes):
                    self.database.execute("DELETE FROM task_events")
                    self.evidence("aster", "host", 10, 20)
                    self.evidence("birch", "host", 20, 30)
                    payload = {"tool": "mcp_update_remote_workspace", "tool_call_id": "unmeasured",
                               "tool_origin": "external", "tool_category": "mcp",
                               "tool_read_only": False, "operation_classes": classes}
                    self.database.execute("INSERT INTO task_events VALUES (?, ?, ?, ?, ?)",
                                          ("birch", kind, json.dumps(payload), 15, 99))
                    with self.assertRaisesRegex(RuntimeError, "unmeasured"):
                        self.check()

    def test_read_only_external_network_observation_is_allowed(self):
        self.evidence("aster", "host", 10, 20)
        self.evidence("birch", "host", 20, 30)
        payload = {"tool": "mcp_read_remote_workspace", "tool_call_id": "read", "invoked": True,
                   "tool_origin": "external", "tool_category": "mcp",
                   "tool_read_only": True, "operation_classes": ["network", "observe"]}
        self.database.execute("INSERT INTO task_events VALUES (?, ?, ?, ?, ?)",
                              ("birch", "tool.completed", json.dumps(payload), 15, 99))
        self.assertEqual(self.check()["execution_policy"], "host-exclusive")

    def test_probe_records_the_actual_child_process_clock_and_cwd(self):
        probe = self.probes["aster"]
        completed = subprocess.run(shlex.split(probe["command"]), cwd=probe["workspace"],
                                   capture_output=True, text=True, check=True)
        receipt = json.loads(probe["receipt_path"].read_text())
        self.assertEqual(receipt["cwd"], str(probe["workspace"]))
        self.assertEqual(receipt["nonce"], probe["nonce"])
        self.assertGreater(receipt["pid"], 0)
        self.assertGreaterEqual(receipt["end_ns"] - receipt["start_ns"], 20_000_000)
        self.assertEqual(json.loads(completed.stdout.removeprefix(soak.PROBE_RECEIPT_PREFIX)), receipt)
        repeated = subprocess.run(shlex.split(probe["command"]), cwd=probe["workspace"],
                                  capture_output=True, text=True)
        self.assertNotEqual(repeated.returncode, 0, "a replay cannot overwrite its first receipt")


def _recording_handler(label, seen):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def _answer(self):
            length = int(self.headers.get("Content-Length", "0"))
            body = self.rfile.read(length) if length else b""
            seen.append({"via": label, "method": self.command, "path": self.path,
                         "auth": self.headers.get("Authorization"),
                         "type": self.headers.get("Content-Type"),
                         "body": json.loads(body) if body else None})
            reply = json.dumps({"via": label, "runtime": {"local_control_socket": "/reported/control.sock"}}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(reply)))
            self.end_headers()
            self.wfile.write(reply)

        do_GET = do_POST = _answer

    return Handler


class CLIIdentityTransportTest(unittest.TestCase):
    """The daemon grants CLI identity only on its local control socket."""

    def setUp(self):
        self.seen = []
        self.tcp = ThreadingHTTPServer(("127.0.0.1", 0), _recording_handler("tcp", self.seen))
        threading.Thread(target=self.tcp.serve_forever, daemon=True).start()
        self.addCleanup(self.tcp.server_close)
        self.addCleanup(self.tcp.shutdown)
        # Unix socket paths are short on macOS; a /tmp directory keeps this one valid.
        self.directory = tempfile.mkdtemp(prefix="smsk", dir="/tmp")
        self.socket_path = os.path.join(self.directory, "control.sock")
        self.unix = socketserver.ThreadingUnixStreamServer(self.socket_path, _recording_handler("socket", self.seen))
        threading.Thread(target=self.unix.serve_forever, daemon=True).start()
        self.addCleanup(self.unix.server_close)
        self.addCleanup(self.unix.shutdown)
        self.addCleanup(lambda: __import__("shutil").rmtree(self.directory, ignore_errors=True))
        self.base = "http://127.0.0.1:%d" % self.tcp.server_port

    def test_requests_that_claim_cli_identity_use_the_socket(self):
        for path, payload in (
            ("/v1/message", {"platform": "cli", "content": "hi"}),
            ("/v1/message", {"platform": " CLI ", "content": "hi"}),
            ("/v1/accounts/bind", {"person_id": "p", "platform": "weixin"}),
            ("/v1/events/stream?platform=cli&cursor=0", None),
        ):
            self.seen.clear()
            soak = importlib.import_module("soak_http")
            status, body = soak.request_json(self.base, "tok", path, payload, self.socket_path)
            self.assertEqual(status, 200)
            self.assertEqual(body["via"], "socket", path)
            self.assertEqual(self.seen[0]["auth"], "Bearer tok")
            if payload is not None:
                self.assertEqual(self.seen[0]["type"], "application/json")

    def test_other_identities_and_lifecycle_probes_use_tcp(self):
        soak = importlib.import_module("soak_http")
        for path, payload in (
            ("/v1/message", {"platform": "weixin", "platform_user_id": "x", "content": "hi"}),
            ("/v1/message", {"platform": "http", "content": "hi"}),
            ("/v1/gateway/status", None),
        ):
            status, body = soak.request_json(self.base, "tok", path, payload, self.socket_path)
            self.assertEqual((status, body["via"]), (200, "tcp"), path)

    def test_a_cli_claim_without_a_socket_never_falls_back_to_tcp(self):
        soak = importlib.import_module("soak_http")
        with self.assertRaises(RuntimeError):
            soak.request_json(self.base, "tok", "/v1/message", {"platform": "cli"}, None)
        self.assertEqual(self.seen, [], "a refused CLI claim must send nothing over TCP")

    def test_the_socket_path_comes_from_the_daemons_own_status(self):
        soak = importlib.import_module("soak_http")
        self.assertEqual(soak.daemon_socket(self.base, "tok"), "/reported/control.sock")


if __name__ == "__main__":
    unittest.main()
