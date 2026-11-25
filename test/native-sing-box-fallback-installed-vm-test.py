#!/usr/bin/env python3
"""Installed sing-box fallback acceptance; disposable vpn-native-lab guest only.

Prerequisites: installed native app, passwordless lab sudo/Polkit, configured
working subscription, selected working server, active VPN and Internet access.
Replaces the bundled sing-box with a stub that fails like a missing shared
library, then expects the gateway to start on the system sing-box and a speed
test to work. Then breaks the system binary too and expects a clear startup
failure. Both binaries are restored and the connection is re-established in a
finally block. Prints only case outcomes, never server data.
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
assert os.geteuid() != 0, "run as the lab desktop user"
os.umask(0o077)
BUNDLED = "/usr/libexec/local-vpn-kde/sing-box"
SYSTEM = "/usr/bin/sing-box"
GATEWAY = "local-vpn-kde-gateway.service"
STUB = "#!/bin/sh\necho 'sing-box: error while loading shared libraries: libsimdutf.so.0: cannot open shared object file' >&2\nexit 127\n"


def sudo(*args, **kwargs):
    return subprocess.run(["sudo", "-n", *args], check=True, **kwargs)


def journal_since(since):
    return subprocess.run(["sudo", "-n", "journalctl", "-u", GATEWAY, "--since", "@" + str(since), "-o", "cat", "--no-pager"],
                          check=True, capture_output=True, text=True).stdout


def wait_journal(since, needle, timeout=90):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if needle in journal_since(since):
            return
        time.sleep(1)
    raise AssertionError("journal line not seen: " + needle.split("=")[0])


class Bridge:
    def __init__(self):
        self.proc = subprocess.Popen(["/usr/bin/kvpn", "bridge"], stdin=subprocess.PIPE,
                                     stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True, bufsize=1)
        self.replies = queue.Queue()
        threading.Thread(target=lambda: [self.replies.put(json.loads(line)) for line in self.proc.stdout],
                         daemon=True).start()
        self.sequence = 0

    def run(self, action, value=None, ok=True):
        self.sequence += 1
        request = {"id": "singbox-lab-" + str(self.sequence), "action": action}
        if value is not None:
            request["value"] = value
        self.proc.stdin.write(json.dumps(request) + "\n")
        self.proc.stdin.flush()
        deadline = time.monotonic() + 240
        while True:
            reply = self.replies.get(timeout=max(0.01, deadline - time.monotonic()))
            if reply.get("id") == request["id"] and "ok" in reply:
                assert reply["ok"] == ok, "unexpected outcome: " + action
                return reply

    def close(self):
        self.proc.stdin.close()
        self.proc.wait(timeout=20)


def connected(bridge, timeout=120):
    deadline = time.monotonic() + timeout
    while True:
        try:
            status = bridge.run("status")["status"]
            assert status["gateway_state"] in ("healthy", "running"), "gateway not ready"
            assert status["networkmanager_active"] == "yes", "VPN not connected"
            route = json.loads(subprocess.check_output(["ip", "-j", "route", "get", "1.1.1.1"]))
            assert route and route[0].get("dev", "").startswith("tun"), "desktop route bypasses VPN"
            target = os.environ.get("VPNKIT_NATIVE_LAB_HTTPS_TARGET", "https://www.gstatic.com/generate_204")
            subprocess.run(["curl", "--silent", "--fail", "--max-time", "20", target],
                           check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            return
        except (AssertionError, subprocess.CalledProcessError):
            if time.monotonic() > deadline:
                raise
            time.sleep(3)


def restart_gateway():
    since = int(time.time())
    sudo("systemctl", "reset-failed", GATEWAY)
    subprocess.run(["sudo", "-n", "systemctl", "restart", GATEWAY], check=False,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    return since


def install_stub(path):
    stub = subprocess.run(["mktemp", "/var/tmp/singbox-stub.XXXXXX"], check=True, capture_output=True, text=True).stdout.strip()
    pathlib.Path(stub).write_text(STUB)
    try:
        sudo("install", "-m755", "-o", "root", "-g", "root", stub, path)
    finally:
        os.unlink(stub)


subprocess.run([SYSTEM, "version"], check=True, stdout=subprocess.DEVNULL)
backup = {}
for path in (BUNDLED, SYSTEM):
    backup[path] = subprocess.run(["sudo", "-n", "mktemp", "/var/tmp/singbox-backup.XXXXXX"], check=True,
                                  capture_output=True, text=True).stdout.strip()
    sudo("cp", "--preserve=mode,ownership,timestamps", path, backup[path])
bridge = Bridge()
try:
    connected(bridge)
    server_id = bridge.run("servers/current")["catalog"]["server"]["server_id"]

    install_stub(BUNDLED)
    since = restart_gateway()
    wait_journal(since, "native_sing_box_selected=system version=")
    assert " bundled=cannot-run" in journal_since(since), "bundled problem not reported"
    bridge.run("start")
    connected(bridge)
    print(json.dumps({"case": "broken_bundled_starts_on_system_sing_box", "result": "passed"}), flush=True)

    # The selected server carries the working connection, so its speed test must
    # start sing-box through the helper's fallback path and succeed.
    bridge.run("servers/speed", server_id)
    print(json.dumps({"case": "speed_test_with_broken_bundled", "result": "passed"}), flush=True)

    install_stub(SYSTEM)
    since = restart_gateway()
    wait_journal(since, "native_gateway_start_failure=sing-box-unavailable")
    print(json.dumps({"case": "no_usable_sing_box_reports_clear_failure", "result": "passed"}), flush=True)
finally:
    cleanup_errors = []
    for path, saved in backup.items():
        try:
            sudo("install", "-m755", "-o", "root", "-g", "root", saved, path)
            sudo("rm", "-f", "--", saved)
        except subprocess.CalledProcessError:
            cleanup_errors.append("restore " + ("bundled" if path == BUNDLED else "system"))
    try:
        since = restart_gateway()
        wait_journal(since, "native_sing_box_selected=")
        bridge.run("start")
        connected(bridge)
    except Exception as error:  # report every cleanup failure, never private output
        cleanup_errors.append("reconnect: " + type(error).__name__)
    bridge.close()
    if cleanup_errors:
        raise AssertionError("cleanup failed: " + ", ".join(cleanup_errors))
selected = [line for line in journal_since(since).splitlines() if line.startswith("native_sing_box_selected=")][-1]
print(json.dumps({"installed_native_sing_box_fallback": "passed", "restored_selection": selected.split()[0]}))
