#!/usr/bin/env python3
"""Installed speed/site batch acceptance in the exclusive disposable native VM.

Run --baseline BEFORE upgrading to compare two old ping/site/speed triplets with
one new combined pass over the same opaque server IDs and HTTPS target. Stores
only opaque IDs, hashes and timings in the guest's private validation directory.
Default mode needs that baseline and the candidate package installed. Adds one
synthetic unreachable profile, removes it in finally, and restores active source.
Never changes the selected runtime server, routes or private runtime configs.
"""
import hashlib
import json
import os
import pathlib
import queue
import subprocess
import sys
import threading
import time

assert os.environ.get("VPNKIT_NATIVE_VM_LAB") == "1", "disposable-lab opt-in required"
assert pathlib.Path("/etc/hostname").read_text().strip() == "vpn-native-lab", "wrong lab"
os.umask(0o077)
private = pathlib.Path.home() / ".config/vpn-lab/speed-batch-validation"
private.mkdir(parents=True, exist_ok=True, mode=0o700)
baseline_path = private / "baseline.json"
target = os.environ.get("VPNKIT_NATIVE_LAB_HTTPS_TARGET", "https://www.gstatic.com/generate_204")
proc = subprocess.Popen(["/usr/bin/local-vpn-kde", "bridge"], stdin=subprocess.PIPE,
                        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True, bufsize=1)
replies = queue.Queue()
threading.Thread(target=lambda: [replies.put(json.loads(line)) for line in proc.stdout], daemon=True).start()
sequence = 0


def read(args):
    return subprocess.check_output(args, text=True).strip()


def snapshot():
    paths = ["/var/lib/local-vpn-kde/vibe-vpn/current-node.json",
             "/etc/local-vpn-kde/sing-box/config.json",
             "/var/lib/local-vpn-kde/sing-box/config.json"]
    hashes = [hashlib.sha256(subprocess.check_output(["sudo", "-n", "cat", path])).hexdigest()
              for path in paths]
    pid = read(["systemctl", "show", "local-vpn-kde-gateway", "-p", "MainPID", "--value"])
    assert int(pid) > 1
    route = read(["ip", "route", "get", "1.1.1.1"]).split("cache")[0].strip()
    assert "tun" in route, "guest route bypasses VPN"
    return {"hashes": hashes, "gateway": pid, "children": read(["pgrep", "-P", pid]),
            "namespace": read(["stat", "-Lc", "%i", "/run/netns/vpnkit-native"]), "route": route}


def send(action, value=None):
    global sequence
    sequence += 1
    request = {"id": "speed-lab-" + str(sequence), "action": action, "progress": True}
    if value is not None:
        request["value"] = value
    proc.stdin.write(json.dumps(request) + "\n")
    proc.stdin.flush()
    return request["id"]


def receive(request_id, action, ok=True, on_event=None):
    started = time.monotonic()
    events = []
    while True:
        reply = replies.get(timeout=max(.01, 180 - (time.monotonic() - started)))
        assert reply.get("id") == request_id, "unexpected request ID"
        if "ok" in reply:
            assert ok is None or reply["ok"] == ok, "unexpected result for " + action + ": " + str(reply.get("reason"))
            return reply, events, time.monotonic() - started
        if reply.get("event") == "server-check":
            events.append((time.monotonic() - started, reply))
            if on_event:
                on_event(reply)


def run(action, value=None, ok=True, on_event=None):
    return receive(send(action, value), action, ok, on_event)


def batch(ids, site=target, **kwargs):
    return run("servers/speed-batch", json.dumps({"ids": ids, "url": site}), **kwargs)


def assert_speed(row):
    assert row["status"] == "ready" and row.get("downloaded_bytes", 0) > 0
    assert row.get("download_mbps", 0) > 0 and 0 < row.get("download_seconds", 0) < 4


original = None
created = None
before = snapshot()
try:
    status = run("status")[0]["status"]
    assert status["runtime"] == "native" and status["installed"] and status["networkmanager_active"] == "yes"
    current = run("servers/current")[0]["catalog"]["server"]
    selected = current["server_id"]
    profiles = run("profiles/list")[0]["profiles"]
    original = profiles["active"]
    catalog = run("servers/list")[0]["catalog"]["servers"]
    if "--baseline" in sys.argv:
        candidates = [selected] + [r["server_id"] for r in catalog if r["server_id"] != selected][:5]
        ids = []
        for server_id in candidates:
            ping = run("servers/ping", server_id, ok=None)[0]
            if ping["catalog"]["server"].get("ping_status") != "ready":
                continue
            site = run("servers/availability", json.dumps({"id": server_id, "url": target}), ok=None)[0]
            if site["catalog"]["server"].get("availability") == "ready":
                ids.append(server_id)
            if len(ids) == 2:
                break
        assert len(ids) == 2, "requires two working existing nodes"
        started = time.monotonic()
        for server_id in ids:
            run("servers/ping", server_id)
            run("servers/availability", json.dumps({"id": server_id, "url": target}))
            assert_speed(run("servers/speed", server_id)[0]["catalog"]["server"])
        elapsed = time.monotonic() - started
        assert snapshot() == before, "baseline probes changed runtime"
        baseline = {"ids": ids, "selected": selected, "target": target, "elapsed": elapsed,
                    "package": read(["pacman", "-Q", "local-vpn-kde-native"]), "runtime": before}
        baseline_path.write_text(json.dumps(baseline))
        print(json.dumps({"baseline": "passed", "nodes": 2, "old_ping_site_speed_seconds": round(elapsed, 3),
                          "runtime": "unchanged", "package": baseline["package"]}))
    else:
        baseline = json.loads(baseline_path.read_text())
        assert baseline["target"] == target and baseline["selected"] == selected
        assert before == baseline["runtime"], "hot upgrade changed running VPN"
        ids = baseline["ids"]
        reply, events, elapsed = batch(ids)
        rows = reply["catalog"]["servers"]
        assert len(rows) == 2
        for row in rows:
            assert_speed(row)
            assert row["ping_status"] == "ready" and row["availability"] == "ready"
        starts = [(t, e) for t, e in events if e["stage"] == "speed"]
        completed = {e["server_id"]: e for _, e in events if e["stage"] == "complete"}
        assert len(starts) == len(completed) == 2, "missing real speed progress"
        for (t, e), (next_t, _) in zip(starts, starts[1:]):
            assert next_t - t >= completed[e["server_id"]]["download_seconds"] - .25, "overlapping download windows"
        # Independent website failure must not suppress working throughput.
        failed_site = batch([ids[0]], "https://127.0.0.1:9/unavailable")[0]["catalog"]["servers"][0]
        assert_speed(failed_site)
        assert failed_site["availability"] == "failed"
        # A synthetic direct profile is catalog-only; no active proxy selection.
        added = run("profiles/add", json.dumps({"name": "Lab speed unreachable",
                    "source": "vless://fixture@127.0.0.1:9?security=none&type=tcp#Lab%20speed%20unreachable"}))[0]["profiles"]
        created = added["active"]
        refreshed = run("servers/refresh")[0]["catalog"]["servers"]
        bad = next(r["server_id"] for r in refreshed if r.get("profile_id") == created)
        run("profiles/open", original)
        mixed = batch([bad, ids[0]])[0]["catalog"]["servers"]
        assert mixed[0]["ping_status"] == "failed" and mixed[0]["status"] == "failed"
        assert_speed(mixed[1])
        # Cancel only after an actual completed row was durably published.
        request_id = send("servers/speed-batch", json.dumps({"ids": ids, "url": target}))
        durable = {}
        def cancel_after_complete(event):
            if event["stage"] == "complete" and not durable:
                durable[event["server_id"]] = event
                proc.stdin.write(json.dumps({"id": request_id, "action": "cancel"}) + "\n")
                proc.stdin.flush()
        canceled, _, cancel_seconds = receive(request_id, "active speed batch cancellation", ok=False, on_event=cancel_after_complete)
        assert durable and canceled["reason"] == "cancelled"
        saved = run("servers/list")[0]["catalog"]["servers"]
        for server_id, event in durable.items():
            row = next(r for r in saved if r["server_id"] == server_id)
            assert row["downloaded_bytes"] == event["downloaded_bytes"], "cancel lost durable completion"
        assert_speed(batch([ids[0]])[0]["catalog"]["servers"][0])  # Locks/proxies reusable.
        assert run("servers/current")[0]["catalog"]["server"]["server_id"] == selected
        assert snapshot() == before, "batch altered main route, running proxy or private config"
        summary = {"installed_speed_batch": "passed", "nodes": 2, "mixed_bad_server": "passed",
                   "site_failure_independent": "passed", "cancel_preserved_completed": "passed",
                   "download_start_spacing": "serialized", "runtime": "unchanged",
                   "old_ping_site_speed_seconds": round(baseline["elapsed"], 3),
                   "combined_ping_site_speed_seconds": round(elapsed, 3), "cancel_pass_seconds": round(cancel_seconds, 3),
                   "package": read(["pacman", "-Q", "local-vpn-kde-native"])}
        (private / "summary.json").write_text(json.dumps(summary, indent=2))
        print(json.dumps(summary))
finally:
    try:
        if created:
            run("profiles/remove", created)
        if original:
            run("profiles/open", original)
        assert snapshot() == before, "cleanup changed running VPN"
    finally:
        proc.stdin.close()
        proc.wait(timeout=35)
