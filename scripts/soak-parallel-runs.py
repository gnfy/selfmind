#!/usr/bin/env python3
"""Isolated live-provider check for same-person parallel Runs and IM routing.

Usage: python3 scripts/soak-parallel-runs.py --config ~/.selfmind/config.yaml \
    --capacity 3 --third-im [--provider google --model gemini-3.8-flash]

The script never changes the user's daemon, config, or control database. It
builds the current source into a temporary runtime under the home directory,
overrides runtime paths via SELF_* environment variables, and removes the
runtime after checking durable Run events. It does use the configured provider.
"""

import argparse
import concurrent.futures
import json
import os
from pathlib import Path
import secrets
import shlex
import shutil
import socket
import sqlite3
import subprocess
import tempfile
import time
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent))
import soak_http  # noqa: E402


def parse_args():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", type=Path, required=True)
    parser.add_argument("--capacity", type=int, choices=(2, 3), default=2)
    parser.add_argument("--provider", default="", help="optional primary provider override")
    parser.add_argument("--model", default="", help="optional primary model override")
    parser.add_argument("--third-im", action="store_true", help="start Cedar from the single IM chat (requires capacity 3)")
    parser.add_argument("--restart", action="store_true", help="kill the daemon after two local effects, then verify exact recovery")
    args = parser.parse_args()
    if args.third_im and args.capacity != 3:
        parser.error("--third-im requires --capacity 3")
    if args.restart and (args.capacity != 2 or args.third_im):
        parser.error("--restart requires --capacity 2 without --third-im")
    return args


def http_request(base, token, path, payload=None, socket_path=None):
    """One request; CLI-identity requests travel on the daemon's control socket."""
    return soak_http.request_json(base, token, path, payload, socket_path, timeout=35)


def wait_for_health(request, process):
    until = time.monotonic() + 45
    while time.monotonic() < until:
        if process.poll() is not None:
            raise RuntimeError("isolated daemon exited during startup")
        try:
            if request("/v1/gateway/status")[1].get("state") == "running":
                return
        except (OSError, ValueError):
            pass
        time.sleep(0.2)
    raise RuntimeError("isolated daemon did not become healthy")


PROBE_RECEIPT_PREFIX = "SELF_MIND_PROCESS_PROBE "
EXECUTION_TOOLS = {"terminal", "exec_command", "verify", "execute_command", "shell",
                   "execute_code", "watch_external"}
WRITE_TOOLS = {"write_file", "edit", "edit_file", "patch", "apply_patch"}
PROBE_SOURCE = '''import json
import os
from pathlib import Path
import sys
import time

nonce, duration = sys.argv[1:]
receipt_path = Path(".selfmind-soak-probe-" + nonce + ".json")
# Reserve this receipt before doing work: a replay must not replace evidence.
started = time.monotonic_ns()
with receipt_path.open("x") as receipt_file:
    time.sleep(float(duration))
    receipt = {"nonce": nonce, "pid": os.getpid(), "cwd": os.getcwd(),
               "start_ns": started, "end_ns": time.monotonic_ns()}
    encoded = json.dumps(receipt, separators=(",", ":"))
    receipt_file.write(encoded)
print("SELF_MIND_PROCESS_PROBE " + encoded, flush=True)
'''


def prepare_process_probe(workspace, name, duration=20):
    workspace = workspace.resolve()
    script_name = ".selfmind-soak-probe.py"
    (workspace / script_name).write_text(PROBE_SOURCE)
    nonce = name + "-" + secrets.token_hex(8)
    return {"workspace": workspace, "nonce": nonce,
            "receipt_path": workspace / (".selfmind-soak-probe-" + nonce + ".json"),
            "command": shlex.join(["python3", script_name, nonce, str(duration)])}


def execution_or_mutation(payload):
    classes = set(payload.get("operation_classes") or [])
    return bool(payload.get("process")
                or classes.intersection({"write", "delete", "exec", "exec.in_turn", "exec.delegated"})
                or payload.get("tool") in EXECUTION_TOOLS | WRITE_TOOLS
                or (payload.get("tool_category") == "filesystem"
                    and payload.get("tool_read_only") is not True)
                or ((payload.get("tool_origin") == "external" or payload.get("tool_category") == "mcp")
                    and payload.get("tool_read_only") is not True))


def process_intervals(database, lineages, probes):
    """Bind independent process clocks to exact successful runtime receipts.

    Tool event timestamps include approval and lane waiting, so they are never
    execution evidence. Reject extra execution/mutations whose actual intervals
    this deliberately narrow workload cannot measure.
    """
    intervals = {}
    for root_id, run_ids in lineages.items():
        probe = probes[root_id]
        try:
            receipt = json.loads(probe["receipt_path"].read_text())
        except (OSError, ValueError) as error:
            raise RuntimeError("missing process probe receipt: " + root_id) from error
        if (receipt.get("nonce") != probe["nonce"]
                or receipt.get("cwd") != str(probe["workspace"])
                or any(type(receipt.get(key)) is not int or receipt[key] <= 0
                       for key in ("pid", "start_ns", "end_ns"))
                or receipt["end_ns"] <= receipt["start_ns"]):
            raise RuntimeError("invalid process probe receipt: " + root_id)
        completed = []
        for run_id in run_ids:
            started = {}
            for kind, raw in database.execute(
                "SELECT type, payload_json FROM task_events "
                "WHERE run_id = ? AND type IN ('tool.started', 'tool.completed') "
                "ORDER BY cursor", (run_id,)
            ):
                payload = json.loads(raw or "{}")
                call_id = payload.get("tool_call_id")
                if kind == "tool.started":
                    started[call_id] = payload
                    continue
                invocation = started.pop(call_id, {})
                if payload.get("invoked") is False:
                    continue  # Explicitly refused/parked before entering the body.
                process = payload.get("process") or {}
                if payload.get("tool") not in ("terminal", "exec_command"):
                    if execution_or_mutation(payload):
                        raise RuntimeError("unmeasured mutation/execution in " + run_id)
                    continue
                arguments = invocation.get("args") or "{}"
                if isinstance(arguments, str):
                    arguments = json.loads(arguments)
                command = arguments.get("command", "")
                if (not call_id or invocation.get("tool") != payload.get("tool")
                        or shlex.split(command) != shlex.split(probe["command"])):
                    raise RuntimeError("unmeasured terminal execution in " + run_id)
                if (payload.get("error") or payload.get("invoked") is not True
                        or process.get("started") is not True
                        or type(process.get("exit_code")) is not int or process["exit_code"] != 0
                        or process.get("sandbox_mode") not in ("host", "isolated")):
                    raise RuntimeError("process probe lacks successful typed process evidence: " + run_id)
                output = payload.get("result", "")
                try:
                    emitted = json.loads(output.split(PROBE_RECEIPT_PREFIX, 1)[1].splitlines()[0])
                except (IndexError, ValueError) as error:
                    raise RuntimeError("process probe receipt missing from exact tool result: " + run_id) from error
                if emitted != receipt:
                    raise RuntimeError("process probe receipt does not match exact tool result: " + run_id)
                completed.append({**receipt, "sandbox_mode": process["sandbox_mode"],
                                  "run_id": run_id, "tool_call_id": call_id})
            if any(execution_or_mutation(item) for item in started.values()):
                raise RuntimeError("unmeasured unfinished execution/mutation invocation in " + run_id)
        if len(completed) != 1:
            raise RuntimeError("a work lineage must have exactly one successful process probe: " + root_id)
        intervals[root_id] = completed[0]
    return intervals


def validate_process_intervals(intervals, minimum_overlap_seconds=5):
    measured = list(intervals.values())
    for index, left in enumerate(measured):
        for right in measured[index + 1:]:
            overlap = min(left["end_ns"], right["end_ns"]) - max(left["start_ns"], right["start_ns"])
            if "host" in (left["sandbox_mode"], right["sandbox_mode"]) and overlap > 0:
                raise RuntimeError("host process overlapped another mutation/terminal probe")
    overlap_seconds = max(0, min(item["end_ns"] for item in measured)
                          - max(item["start_ns"] for item in measured)) / 10**9
    if all(item["sandbox_mode"] == "isolated" for item in measured):
        if overlap_seconds < minimum_overlap_seconds:
            raise RuntimeError(f"isolated process probes did not overlap sufficiently: {overlap_seconds}s")
        policy = "isolated-overlap"
    else:
        policy = "host-exclusive" if all(item["sandbox_mode"] == "host" for item in measured) else "mixed-host-exclusive"
    return {"execution_policy": policy, "process_overlap_seconds": overlap_seconds}


def isolate_configuration(source_config, runtime_root):
    # Readiness and model transactions live beside config.yaml, separately
    # from control.db. Keep both inside the throwaway runtime.
    isolated_config = runtime_root / "config.yaml"
    shutil.copyfile(source_config, isolated_config)
    isolated_config.chmod(0o600)
    return isolated_config


def main():
    args = parse_args()
    source = Path(__file__).resolve().parent.parent
    config_path = args.config.expanduser().resolve()
    if not config_path.is_file():
        raise RuntimeError("config file not found")
    names = ("aster", "birch", "cedar")[: args.capacity]
    with tempfile.TemporaryDirectory(prefix="selfmind-parallel-soak-", dir=Path.home()) as temporary:
        root = Path(temporary)
        isolated_config = isolate_configuration(config_path, root)
        binary = root / "selfmind"
        subprocess.run(["go", "build", "-o", str(binary), "./cmd/selfmind"],
                       cwd=source, check=True, stdout=subprocess.DEVNULL)
        data_dir = root / "data"
        data_dir.mkdir()
        workspaces = [root / name for name in names]
        for workspace in workspaces:
            workspace.mkdir()
        probes = [] if args.restart else [
            prepare_process_probe(workspace, name) for name, workspace in zip(names, workspaces)]
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            port = sock.getsockname()[1]
        token = secrets.token_urlsafe(24)
        env = os.environ.copy()
        env.update({
            "SELFMIND_WORKERS": str(args.capacity),
            "SELF_STORAGE_DATA_DIR": str(data_dir),
            "SELF_GATEWAY_ADDR": f"127.0.0.1:{port}",
            "SELF_GATEWAY_TOKEN": token,
            "SELF_DAEMON_TOKEN": "",
            "SELF_GATEWAY_MAX_ACTIVE_WORK_RUNS": str(args.capacity),
            "SELF_EXEC_SANDBOX_ALLOW_NETWORK": "false",
            "SELF_GATEWAY_WEIXIN_ENABLED": "false",
            "SELF_GATEWAY_WECHAT_ENABLED": "false",
            "SELF_GATEWAY_FEISHU_ENABLED": "false",
            "SELF_GATEWAY_QQ_ENABLED": "false",
            "SELF_GATEWAY_TELEGRAM_TOKEN": "",
        })
        if args.provider:
            env["SELF_MODELS_PRIMARY_PROVIDER"] = args.provider
        if args.model:
            env["SELF_MODELS_PRIMARY_MODEL"] = args.model
        log_path = root / "gateway.log"
        with log_path.open("w") as log:
            def launch():
                return subprocess.Popen(
                    [str(binary), "--config", str(isolated_config), "gateway", "run",
                     "--addr", f"127.0.0.1:{port}"],
                    cwd=source, env=env, stdout=log, stderr=subprocess.STDOUT,
                )

            process = launch()
            base = f"http://127.0.0.1:{port}"
            control = {"socket": None}
            request = lambda path, payload=None: http_request(base, token, path, payload, control["socket"])
            try:
                wait_for_health(request, process)
                control["socket"] = soak_http.daemon_socket(base, token)
                if args.third_im:
                    code, setup = request("/v1/message", {
                        "platform": "cli", "platform_user_id": "local",
                        "channel": "cli-setup", "client_cwd": str(workspaces[2]),
                        "content": "/ws", "async": False})
                    if code != 200 or setup.get("error"):
                        raise RuntimeError("could not establish Cedar as the IM default workspace")
                prompts = [
                    (f"For the {name.title()} local marker task, use terminal to run "
                     "`printf x >> marker.txt; sleep 25` in the current workspace, "
                     "then report the file status.") if args.restart else
                    (f"For the {name.title()} task, use the terminal tool exactly once to run "
                     f"`{probes[index]['command']}` in the current workspace. "
                     "Preserve the provided probe and its receipt. No extra terminal calls are needed. "
                     f"After it finishes, answer in one sentence that {name.title()} is ready.")
                    for index, name in enumerate(names)
                ]
                cli_names = names[:2] if args.third_im else names
                payloads = [
                    {"platform": "cli", "platform_user_id": "local",
                     "display_name": "Parallel Soak", "channel": f"cli-{name}",
                     "client_cwd": str(workspace), "content": prompt,
                     "async": True, "approval_mode": "full-auto"}
                    for name, workspace, prompt in zip(cli_names, workspaces, prompts)
                ]
                with concurrent.futures.ThreadPoolExecutor(max_workers=len(cli_names)) as pool:
                    futures = [pool.submit(request, "/v1/message", payload) for payload in payloads]
                    peak = 0
                    until = time.monotonic() + 40
                    while time.monotonic() < until:
                        peak = max(peak, request("/v1/gateway/status")[1].get("active_run_count", 0))
                        if peak == len(cli_names) and all(f.done() for f in futures):
                            break
                        time.sleep(0.1)
                    admissions = [future.result() for future in futures]
                if peak != len(cli_names) or any(code != 200 or not body.get("accepted") for code, body in admissions):
                    raise RuntimeError("parallel CLI admission failed: " +
                                       repr([(code, body.get("error")) for code, body in admissions]))
                people = {body["identity"]["person_id"] for _, body in admissions}
                if len(people) != 1:
                    raise RuntimeError("two CLI sessions resolved to different people")
                if args.restart:
                    deadline = time.monotonic() + 50
                    while time.monotonic() < deadline:
                        if all((workspace / "marker.txt").exists() for workspace in workspaces):
                            break
                        time.sleep(0.1)
                    else:
                        raise RuntimeError("the local effects did not occur before the crash deadline")
                    process.kill()
                    process.wait(timeout=5)
                    process = launch()
                    wait_for_health(request, process)
                    control["socket"] = soak_http.daemon_socket(base, token)
                    database = sqlite3.connect(data_dir / "control.db")
                    original = dict(database.execute(
                        "SELECT channel, id FROM runs WHERE execution_class = 'work' AND resumes_run_id = ''"
                    ).fetchall())
                    parents = {original.get("cli-aster", ""), original.get("cli-birch", "")}
                    if "" in parents or len(parents) != 2:
                        raise RuntimeError("restart lost the two original Run identities")
                    deadline = time.monotonic() + 180
                    next_report = time.monotonic() + 30
                    while time.monotonic() < deadline:
                        runs = database.execute(
                            "SELECT id, status, resumes_run_id FROM runs WHERE execution_class = 'work'"
                        ).fetchall()
                        by_id = {run_id: (status, parent) for run_id, status, parent in runs}
                        children = [(run_id, status, parent) for run_id, status, parent in runs if parent]
                        queue = database.execute(
                            "SELECT class, status, reply_to_run_id FROM task_queue "
                            "WHERE class IN ('recovery', 'finalization')"
                        ).fetchall()
                        active = request("/v1/gateway/status")[1].get("active_run_count", 0)
                        leaves = {run_id for run_id, _, _ in runs} - {parent for _, _, parent in children}
                        def root_of(run_id):
                            seen = set()
                            while run_id in by_id and by_id[run_id][1] and run_id not in seen:
                                seen.add(run_id)
                                run_id = by_id[run_id][1]
                            return run_id if run_id in by_id and not by_id[run_id][1] else ""
                        recovery = [(status, parent) for kind, status, parent in queue if kind == "recovery"]
                        if (len(children) >= 2 and len(leaves) == 2
                                and {root_of(run_id) for run_id in leaves} == parents
                                and all(by_id[run_id][0] == "done" for run_id in leaves)
                                and len(recovery) == 2 and {parent for _, parent in recovery} == parents
                                and all(status == "done" for _, status, _ in queue)
                                and active == 0):
                            break
                        if time.monotonic() >= next_report:
                            print(json.dumps({"recovery_progress": True,
                                              "children": [status for _, status, _ in children],
                                              "queue": [status for _, status, _ in queue],
                                              "active": active}), flush=True)
                            next_report += 30
                        time.sleep(0.5)
                    else:
                        raise RuntimeError("recovery did not settle: " + repr({
                            "runs": runs, "queue": queue, "active": active,
                            "markers": [(workspace / "marker.txt").read_text() for workspace in workspaces],
                        }))
                    marker_values = [(workspace / "marker.txt").read_text() for workspace in workspaces]
                    if marker_values != ["x", "x"]:
                        raise RuntimeError("an uncertain local effect was repeated after restart")
                    old_statuses = [status for (status,) in database.execute(
                        "SELECT status FROM runs WHERE id IN (?, ?)", tuple(parents)
                    ).fetchall()]
                    if old_statuses != ["interrupted", "interrupted"]:
                        raise RuntimeError("original Runs lost their interrupted history")
                    print(json.dumps({"result": "PASS", "crash_after_effect": True,
                                      "original_runs": 2, "recovered_runs": len(children),
                                      "provider_waits": len([1 for kind, _, _ in queue if kind == "finalization"]),
                                      "duplicate_effects": 0, "recovery_queue": "done"}))
                    database.close()
                    return
                code, _ = request("/v1/accounts/bind", {
                    "person_id": next(iter(people)), "platform": "weixin",
                    "platform_user_id": "soak-im", "display_name": "Soak IM"})
                if code != 200:
                    raise RuntimeError("isolated IM account binding failed")
                if args.third_im:
                    code, third = request("/v1/message", {
                        "platform": "weixin", "platform_user_id": "soak-im",
                        "channel": "one-chat", "content": prompts[2],
                        "async": True, "approval_mode": "full-auto"})
                    if code != 200 or not third.get("accepted"):
                        raise RuntimeError("the independent Cedar request was not accepted from IM")
                    until = time.monotonic() + 30
                    while time.monotonic() < until:
                        peak = max(peak, request("/v1/gateway/status")[1].get("active_run_count", 0))
                        if peak == args.capacity:
                            break
                        time.sleep(0.1)
                if peak != args.capacity:
                    raise RuntimeError("the third independent work Run never ran alongside both CLI Runs")
                database = sqlite3.connect(data_dir / "control.db")
                by_channel = dict(database.execute(
                    "SELECT channel, id FROM runs WHERE execution_class = 'work' AND resumes_run_id = ''"
                ).fetchall())
                run_ids = [by_channel.get("cli-" + name, "") for name in cli_names]
                if args.third_im:
                    run_ids.append(by_channel.get("one-chat", ""))
                if not all(run_ids) or len(set(run_ids)) != args.capacity:
                    raise RuntimeError("CLI sessions did not create distinct Runs")
                code, status = request("/v1/message", {
                    "platform": "weixin", "platform_user_id": "soak-im",
                    "channel": "one-chat", "content": "/status", "async": False})
                if code != 200 or not all(name.title() in status.get("content", "") for name in names):
                    raise RuntimeError("IM could not view every active Run")
                code, stranger = request("/v1/message", {
                    "platform": "weixin", "platform_user_id": "unbound-stranger",
                    "channel": "other-chat", "content": "/status", "async": False})
                if code != 200 or any(name.title() in stranger.get("content", "") for name in names):
                    raise RuntimeError("another person's IM status exposed this work")
                code, supplement = request("/v1/message", {
                    "platform": "weixin", "platform_user_id": "soak-im",
                    "channel": "one-chat", "content": "For the Birch task, add a final verification note.",
                    "async": True})
                target = (supplement.get("turn") or {}).get("run_id", "")
                if code != 200 or not supplement.get("accepted") or target != run_ids[1]:
                    raise RuntimeError("IM supplement did not route to the Birch Run")
                until = time.monotonic() + 300
                next_report = time.monotonic() + 30
                while time.monotonic() < until:
                    rows = database.execute(
                        "SELECT id, status, COALESCE(resumes_run_id, '') FROM runs WHERE execution_class = 'work'"
                    ).fetchall()
                    by_id = {run_id: (status, parent) for run_id, status, parent in rows}
                    def root_of(run_id):
                        seen = set()
                        while run_id in by_id and by_id[run_id][1] and run_id not in seen:
                            seen.add(run_id)
                            run_id = by_id[run_id][1]
                        return run_id if run_id in by_id and not by_id[run_id][1] else ""
                    lineages = {root: [] for root in run_ids}
                    for run_id in by_id:
                        root = root_of(run_id)
                        if root in lineages:
                            lineages[root].append(run_id)
                    parents = {parent for _, _, parent in rows if parent}
                    leaves = {root: [(run_id, by_id[run_id][0]) for run_id in children if run_id not in parents]
                              for root, children in lineages.items()}
                    pending_queue = database.execute(
                        "SELECT class, status FROM task_queue WHERE status IN ('queued','started')"
                    ).fetchall()
                    active = request("/v1/gateway/status")[1].get("active_run_count", 0)
                    if (all(len(items) == 1 and items[0][1] == "done" for items in leaves.values())
                            and not pending_queue and active == 0):
                        break
                    if time.monotonic() >= next_report:
                        print(json.dumps({"progress": True, "leaves": leaves,
                                          "pending_queue": pending_queue, "active": active}), flush=True)
                        next_report += 30
                    time.sleep(0.2)
                else:
                    raise RuntimeError("work lineages did not settle: " + repr({
                        "leaves": leaves, "pending_queue": pending_queue, "active": active}))
                intervals = process_intervals(database, lineages, dict(zip(run_ids, probes)))
                execution = validate_process_intervals(intervals)
                print(json.dumps({"result": "PASS", "capacity": args.capacity,
                                  "peak_admitted_active_runs": peak, "done_runs": len(run_ids),
                                  "process_intervals": intervals, **execution,
                                  "im_supplement": "birch", "stranger_isolated": True,
                                  "third_from_im": args.third_im}))
                database.close()
            finally:
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)


if __name__ == "__main__":
    main()
