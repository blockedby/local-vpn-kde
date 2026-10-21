#!/usr/bin/env python3
"""Switch real installed profiles/protocols in the disposable native VM.

Requires a healthy connected selected real server and private lab-owned mode0600
file VPNKIT_NATIVE_LAB_SERVER_FILE containing the authorized alternate direct
server URI. Never prints or persists that URI. Adds one profile, selects/probes
its real server, restores original selection/profile, and removes the fixture.
Requires exclusive VM access, installed app, lab Polkit and passwordless sudo.
"""
import json
import os
import pathlib
import queue
import stat
import subprocess
import threading

assert os.environ.get("VPNKIT_NATIVE_VM_LAB") == "1", "disposable-lab opt-in required"
assert pathlib.Path("/etc/hostname").read_text().strip() == "vpn-native-lab", "wrong lab"
os.umask(0o077)
source_path = pathlib.Path(os.environ["VPNKIT_NATIVE_LAB_SERVER_FILE"])
info = source_path.lstat()
assert stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid() and stat.S_IMODE(info.st_mode) == 0o600, "private lab server file required"
source = source_path.read_text().strip()
assert source.startswith(("hy2://", "hysteria2://")), "Hysteria2 alternate source required"
proc = subprocess.Popen(["/usr/bin/local-vpn-kde", "bridge"], stdin=subprocess.PIPE,
                        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True, bufsize=1)
replies = queue.Queue()
threading.Thread(target=lambda: [replies.put(json.loads(line)) for line in proc.stdout], daemon=True).start()
sequence = 0
summary = []
created = original_profile = original_server = None


def run(action, value=None):
    global sequence
    sequence += 1
    request = {"id": "switch-lab-" + str(sequence), "action": action}
    if value is not None:
        request["value"] = value
    proc.stdin.write(json.dumps(request) + "\n")
    proc.stdin.flush()
    while True:
        reply = replies.get(timeout=240)
        assert reply.get("id") == request["id"]
        if "ok" in reply:
            assert reply["ok"], "installed action failed: " + action
            summary.append({"action": action, "outcome": "passed"})
            return reply


def runtime_protocols():
    config = json.loads(subprocess.check_output(["sudo", "-n", "cat", "/var/lib/local-vpn-kde/sing-box/config.json"]))
    return sorted(outbound["type"] for outbound in config.get("outbounds", [])
                  if outbound.get("type") in ("vless", "hysteria2"))


def connected():
    status = run("status")["status"]
    assert status["gateway_state"] in ("healthy", "running") and status["networkmanager_active"] == "yes"
    routes = json.loads(subprocess.check_output(["ip", "-j", "route", "get", "1.1.1.1"]))
    assert routes and routes[0].get("dev", "").startswith("tun"), "desktop route bypasses VPN"
    subprocess.run(["curl", "--silent", "--fail", "--max-time", "25", "https://www.gstatic.com/generate_204"],
                   check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


try:
    connected()
    original_protocols = runtime_protocols()
    assert original_protocols, "original selected runtime protocol missing"
    initial = run("profiles/list")["profiles"]
    original_profile = initial["active"]
    original_server = run("servers/current")["catalog"]["server"]["server_id"]
    added = run("profiles/add", json.dumps({"name": "Lab real Hysteria2 switch", "source": source}))["profiles"]
    del source
    created = added["active"]
    run("servers/refresh")
    alternate = [row for row in run("servers/list")["catalog"]["servers"] if row.get("profile_id") == created]
    assert len(alternate) == 1, "alternate source must produce one server"
    alternate_id = alternate[0]["server_id"]
    run("servers/availability", json.dumps({"id": alternate_id, "url": "https://www.gstatic.com/generate_204"}))
    run("servers/select", alternate_id)
    assert runtime_protocols() == ["hysteria2"], "alternate protocol was not actually applied"
    assert run("servers/current")["catalog"]["server"]["server_id"] == alternate_id
    connected()
    run("profiles/open", original_profile)
    run("servers/select", original_server)
    assert runtime_protocols() == original_protocols, "original protocol was not restored"
    assert run("servers/current")["catalog"]["server"]["server_id"] == original_server
    connected()
    run("profiles/remove", created)
    created = None
    assert run("profiles/list")["profiles"] == initial, "original profile collection changed"
    private = pathlib.Path.home() / ".config/vpn-lab/server-switch-validation"
    private.mkdir(parents=True, exist_ok=True, mode=0o700)
    summary.append({"protocol_transition": [original_protocols, ["hysteria2"], original_protocols]})
    (private / "summary.json").write_text(json.dumps(summary, indent=2))
    print("INSTALLED_NATIVE_REAL_SERVER_PROFILE_SWITCH=passed; alternate=hysteria2; desktop-tun-https=passed; original-selection-profile=restored")
finally:
    try:
        if original_profile:
            run("profiles/open", original_profile)
        if original_server:
            run("servers/select", original_server)
            connected()
    finally:
        try:
            if created:
                run("profiles/remove", created)
        finally:
            proc.stdin.close()
            proc.wait(timeout=35)
