#!/usr/bin/env python3
"""Installed bridge E2E; exclusive access to disposable vpn-native-lab required.

Prerequisites: installed native app, lab Polkit rules/passwordless sudo, configured
working subscription, selected working server, active VPN and Internet access.
Optional VPNKIT_NATIVE_LAB_HTTPS_TARGET overrides the public availability target.
Only opaque IDs and action/outcome pairs are recorded. Original source profiles
and selection are preserved; finally restores the initial running connection.
While stopped, injects invalid sing-box JSON to exercise backend start failure;
the original private config bytes are restored in an inner finally block.
"""
import json
import os
import pathlib
import queue
import subprocess
import threading
import time

assert os.environ.get("VPNKIT_NATIVE_VM_LAB") == "1", "disposable-lab opt-in required"
assert pathlib.Path("/etc/hostname").read_text().strip() == "vpn-native-lab", "wrong lab"
os.umask(0o077)
proc = subprocess.Popen(["/usr/bin/local-vpn-kde", "bridge"], stdin=subprocess.PIPE,
                        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True, bufsize=1)
replies = queue.Queue()
threading.Thread(target=lambda: [replies.put(json.loads(line)) for line in proc.stdout], daemon=True).start()
sequence = 0
summary = []


def send(action, value=None):
    global sequence
    sequence += 1
    request = {"id": "lifecycle-lab-" + str(sequence), "action": action, "progress": True}
    if value is not None:
        request["value"] = value
    proc.stdin.write(json.dumps(request) + "\n")
    proc.stdin.flush()
    return request["id"]


def receive(request_id, action, ok=True):
    deadline = time.monotonic() + 240
    while True:
        reply = replies.get(timeout=max(0.01, deadline - time.monotonic()))
        assert reply.get("id") == request_id, "unexpected bridge request ID"
        if "ok" not in reply:
            continue
        assert reply["ok"] == ok, "unexpected outcome: " + action
        summary.append({"action": action, "ok": reply["ok"], "reason": reply.get("reason")})
        return reply


def run(action, value=None, ok=True):
    return receive(send(action, value), action, ok)


def connected():
    status = run("status")["status"]
    assert status["runtime"] == "native" and status["installed"] is True
    assert status["gateway_state"] in ("healthy", "running"), "gateway not ready"
    assert status["networkmanager_active"] == "yes", "VPN not connected"
    route = json.loads(subprocess.check_output(["ip", "-j", "route", "get", "1.1.1.1"]))
    assert route and route[0].get("dev", "").startswith("tun"), "desktop route bypasses VPN"
    target = os.environ.get("VPNKIT_NATIVE_LAB_HTTPS_TARGET", "https://www.gstatic.com/generate_204")
    subprocess.run(["curl", "--silent", "--fail", "--max-time", "20", target],
                   check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


restore = False
try:
    connected()
    restore = True
    run("start")  # Already-connected start must be idempotent.
    connected()
    profiles = run("profiles/list")["profiles"]
    source = run("subscription/read")["value"]
    assert source, "missing configured desktop subscription"
    run("subscription", "not-a-subscription", ok=False)
    assert run("subscription/read")["value"] == source, "invalid setup changed private subscription"
    del source
    current = run("servers/current")["catalog"].get("server")
    assert current and current["selected"], "requires selected working server"
    server_id = current["server_id"]
    catalog = run("servers/list")["catalog"]
    assert any(row["server_id"] == server_id for row in catalog["servers"])
    for action in ("servers/select", "servers/ping", "servers/speed"):
        run(action, "not-an-opaque-id", ok=False)
    for action in ("servers/select", "servers/ping", "servers/speed"):
        run(action, "srv_" + "A" * 27, ok=False)  # Real backend rejects an unknown opaque ID.
    assert run("servers/current")["catalog"]["server"]["server_id"] == server_id
    run("servers/availability", json.dumps({"id": server_id, "url": "http://example.invalid"}), ok=False)
    run("servers/check-batch", json.dumps({"ids": [server_id, server_id], "url": "https://example.com"}), ok=False)
    run("servers/ping", server_id)
    target = os.environ.get("VPNKIT_NATIVE_LAB_HTTPS_TARGET", "https://www.gstatic.com/generate_204")
    run("servers/availability", json.dumps({"id": server_id, "url": target}))
    failed_probe = run("servers/availability", json.dumps({"id": server_id, "url": "https://127.0.0.1:9/unavailable"}), ok=False)
    assert failed_probe["catalog"]["server"]["availability"] == "failed", "failed HTTPS probe lost evidence"
    assert run("servers/current")["catalog"]["server"]["server_id"] == server_id
    batch = run("servers/check-batch", json.dumps({"ids": [server_id], "url": target}))["catalog"]
    assert len(batch["servers"]) == 1 and batch["servers"][0]["availability"] == "ready"
    speed = run("servers/speed", server_id)["catalog"]["server"]
    assert speed.get("downloaded_bytes", 0) > 0 and speed.get("download_mbps", 0) > 0
    request_id = send("servers/check-batch", json.dumps({"ids": [server_id], "url": "https://192.0.2.254/unavailable"}))
    # Observe the real installed worker before canceling, rather than canceling
    # a request while it is still queued at the bridge gate.
    while True:
        event = replies.get(timeout=60)
        assert event.get("id") == request_id
        assert "ok" not in event, "check finished before active cancellation"
        if event.get("event") == "server-check" and event.get("stage") == "start":
            break
    proc.stdin.write(json.dumps({"id": request_id, "action": "cancel"}) + "\n")
    proc.stdin.flush()
    canceled = receive(request_id, "servers/check-batch active cancel", ok=False)
    assert canceled["reason"] == "cancelled", "operation was not canceled"
    # A subsequent actual probe establishes worker/lock resources are reusable.
    run("servers/availability", json.dumps({"id": server_id, "url": target}))
    run("servers/select", server_id)
    assert run("servers/current")["catalog"]["server"]["server_id"] == server_id
    connected()
    diag = run("diagnostics")
    assert diag["status"]["diagnostics"] == "available"
    run("disconnect")
    status = run("status")["status"]
    assert status["networkmanager_active"] == "no" and status["gateway_state"] in ("healthy", "running")
    run("disconnect")  # Idempotence through the installed service boundary.
    run("start")
    connected()
    run("stop")
    status = run("status")["status"]
    assert status["networkmanager_active"] == "no" and status["gateway_state"] not in ("healthy", "running")
    run("stop")
    # Exercise an actual installed service start failure, preserving the private
    # working configuration in memory and restoring its exact bytes afterward.
    config_path = "/etc/local-vpn-kde/sing-box/config.json"
    config_bytes = subprocess.check_output(["sudo", "-n", "cat", config_path])
    try:
        subprocess.run(["sudo", "-n", "tee", config_path], input=b"{invalid-json\n",
                       check=True, stdout=subprocess.DEVNULL)
        run("backend/start", ok=False)
        failed = run("status")["status"]
        assert failed["networkmanager_active"] == "no", "failed backend start connected desktop VPN"
        assert failed["gateway_state"] not in ("healthy", "running"), "invalid backend reported healthy"
    finally:
        subprocess.run(["sudo", "-n", "tee", config_path], input=config_bytes,
                       check=True, stdout=subprocess.DEVNULL)
        del config_bytes
    run("backend/start")
    status = run("status")["status"]
    assert status["gateway_state"] in ("healthy", "running") and status["networkmanager_active"] == "no"
    run("start")
    connected()
    assert run("profiles/list")["profiles"] == profiles, "lifecycle changed source profiles"
    assert run("servers/current")["catalog"]["server"]["server_id"] == server_id
    private = pathlib.Path.home() / ".config/vpn-lab/lifecycle-validation"
    private.mkdir(parents=True, exist_ok=True, mode=0o700)
    (private / "summary.json").write_text(json.dumps(summary, indent=2))
    print("INSTALLED_NATIVE_LIFECYCLE_SERVERS_DIAGNOSTICS=passed; probes=ping,availability,batch,speed; cancel=passed; backend-start-failure-recovery=passed; preserved=profiles,selection")
finally:
    try:
        if restore:
            current_status = run("status")["status"]
            if current_status["networkmanager_active"] != "yes" or current_status["gateway_state"] not in ("healthy", "running"):
                run("start")
            connected()
    finally:
        proc.stdin.close()
        proc.wait(timeout=35)
