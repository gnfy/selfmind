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
import socket
import sqlite3
import subprocess
import tempfile
import time
import urllib.error
import urllib.request


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


def http_request(base, token, path, payload=None):
    headers = {"Authorization": "Bearer " + token}
    if payload is not None:
        headers["Content-Type"] = "application/json"
    request = urllib.request.Request(
        base + path,
        data=json.dumps(payload).encode() if payload is not None else None,
        headers=headers,
    )
    try:
        with urllib.request.urlopen(request, timeout=35) as response:
            return response.status, json.load(response)
    except urllib.error.HTTPError as error:
        try:
            return error.code, json.loads(error.read().decode())
        except (ValueError, UnicodeError):
            return error.code, {}


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


def tool_intervals(database, lineages):
    intervals = {}
    for root_id, run_ids in lineages.items():
        completed = []
        for run_id in run_ids:
            started = {}
            for kind, raw, at in database.execute(
                "SELECT type, payload_json, created_at FROM task_events "
                "WHERE run_id = ? AND type IN ('tool.started', 'tool.completed') "
                "ORDER BY cursor", (run_id,)
            ):
                try:
                    payload = json.loads(raw or "{}")
                except ValueError:
                    continue
                if payload.get("tool") not in ("terminal", "exec_command"):
                    continue
                call_id = payload.get("tool_call_id")
                if not call_id:
                    continue
                if kind == "tool.started":
                    started[call_id] = at
                elif call_id in started and not payload.get("error"):
                    completed.append((started.pop(call_id), at))
        if not completed:
            raise RuntimeError("a work lineage never completed its terminal sleep: " + root_id)
        intervals[root_id] = max(completed, key=lambda item: item[1] - item[0])
    return intervals


def main():
    args = parse_args()
    source = Path(__file__).resolve().parent.parent
    config_path = args.config.expanduser().resolve()
    if not config_path.is_file():
        raise RuntimeError("config file not found")
    names = ("aster", "birch", "cedar")[: args.capacity]
    with tempfile.TemporaryDirectory(prefix="selfmind-parallel-soak-", dir=Path.home()) as temporary:
        root = Path(temporary)
        binary = root / "selfmind"
        subprocess.run(["go", "build", "-o", str(binary), "./cmd/selfmind"],
                       cwd=source, check=True, stdout=subprocess.DEVNULL)
        data_dir = root / "data"
        data_dir.mkdir()
        workspaces = [root / name for name in names]
        for workspace in workspaces:
            workspace.mkdir()
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
                    [str(binary), "--config", str(config_path), "gateway", "run",
                     "--addr", f"127.0.0.1:{port}"],
                    cwd=source, env=env, stdout=log, stderr=subprocess.STDOUT,
                )

            process = launch()
            base = f"http://127.0.0.1:{port}"
            request = lambda path, payload=None: http_request(base, token, path, payload)
            try:
                wait_for_health(request, process)
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
                    (f"For the {name.title()} task, use the terminal tool to run `sleep 20`, "
                     f"then answer in one sentence that {name.title()} is ready.")
                    for name in names
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
                intervals = tool_intervals(database, lineages)
                overlap = min(end for _, end in intervals.values()) - max(start for start, _ in intervals.values())
                if overlap < 5:
                    raise RuntimeError(f"terminal work did not overlap sufficiently: {overlap}s")
                print(json.dumps({"result": "PASS", "capacity": args.capacity,
                                  "peak_active": peak, "done_runs": len(run_ids),
                                  "terminal_overlap_seconds": overlap,
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
