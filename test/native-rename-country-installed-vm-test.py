#!/usr/bin/env python3
"""Installed bridge acceptance for server names and offline countries; lab VM only.

Requires a configured native VPN with a loaded server catalog. Renames one
server through the installed bridge, checks persistence across a new bridge
process, then resets it. Changes no connection. Private catalog data stays in
memory: stdout carries only counts and case outcomes, never names or hosts.
"""
import json
import os
from pathlib import Path
import re
import subprocess

assert os.environ.get("VPNKIT_NATIVE_VM_LAB") == "1", "lab opt-in required"
assert Path("/etc/hostname").read_text().strip() == "vpn-native-lab", "wrong guest"
assert os.geteuid() != 0, "run as the lab desktop user"
os.umask(0o077)
state = Path(os.environ.get("XDG_STATE_HOME", str(Path.home() / ".local/state"))) / "local-vpn-kde"
names_file = state / "server-names.json"
LABEL = "Lab rename check"


class Bridge:
    def __init__(self):
        self.proc = subprocess.Popen(["/usr/bin/local-vpn-kde", "bridge"], stdin=subprocess.PIPE,
                                     stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True, bufsize=1)
        self.next = 0

    def request(self, action, value=None):
        self.next += 1
        message = {"id": f"t{self.next}", "action": action}
        if value is not None:
            message["value"] = value if isinstance(value, str) else json.dumps(value)
        self.proc.stdin.write(json.dumps(message) + "\n")
        self.proc.stdin.flush()
        for line in self.proc.stdout:
            reply = json.loads(line)
            if reply.get("id") == message["id"] and "ok" in reply:
                return reply
        raise AssertionError("bridge closed before replying")

    def close(self):
        self.proc.stdin.close()
        self.proc.wait(timeout=20)


def servers(bridge):
    reply = bridge.request("servers/list")
    assert reply["ok"], "catalog unavailable"
    return reply["catalog"]["servers"]


bridge = Bridge()
target = None
try:
    rows = servers(bridge)
    assert rows, "requires a loaded catalog"
    codes = [row.get("country") for row in rows]
    assert all(code is None or re.fullmatch(r"[A-Z]{2}", code) for code in codes), "malformed country"
    assert all("original_name" not in row for row in rows if row["display_name"] != LABEL), "unexpected override"
    target = next(row for row in rows if not row.get("selected"))["server_id"] if len(rows) > 1 else rows[0]["server_id"]
    before = next(row["display_name"] for row in rows if row["server_id"] == target)

    reply = bridge.request("servers/rename", {"id": target, "name": "  Lab   rename check "})
    assert reply["ok"] and reply["reason"] == "ok", "rename refused"
    row = next(row for row in servers(bridge) if row["server_id"] == target)
    assert row["display_name"] == LABEL and row["original_name"] == before, "override not applied"
    assert names_file.is_file() and names_file.stat().st_mode & 0o777 == 0o600, "names file mode"
    stored = json.loads(names_file.read_text())
    assert set(stored) <= {"schema", "names"} and stored["names"].get(target) == LABEL, "names file content"
    print(json.dumps({"case": "rename_applied_and_stored_privately", "result": "passed"}), flush=True)

    reply = bridge.request("servers/rename", {"id": target, "name": "\u0007\u202e"})
    assert not reply["ok"] and reply["reason"] == "invalid-name", "control-only name accepted"
    bridge.close()
    bridge = Bridge()
    row = next(row for row in servers(bridge) if row["server_id"] == target)
    assert row["display_name"] == LABEL, "override lost across bridge restart"
    print(json.dumps({"case": "rename_survives_new_bridge_and_rejects_empty_result", "result": "passed"}), flush=True)

    reply = bridge.request("servers/ping", target)
    measured = reply.get("catalog", {}).get("server") or {}
    assert measured.get("display_name", LABEL) == LABEL, "measurement reply lost the override"
    assert measured.get("country") is None or re.fullmatch(r"[A-Z]{2}", measured["country"]), "malformed country"
    print(json.dumps({"case": "measurement_reply_keeps_override", "result": "passed",
                      "ping_ok": bool(reply["ok"]), "country_known": bool(measured.get("country"))}), flush=True)
finally:
    if target:
        reply = bridge.request("servers/rename", {"id": target, "name": ""})
        assert reply["ok"], "reset refused"
        row = next(row for row in servers(bridge) if row["server_id"] == target)
        assert "original_name" not in row and row["display_name"] != LABEL, "reset not applied"
    bridge.close()

known = sum(1 for code in [row.get("country") for row in rows] if code)
print(json.dumps({"installed_native_rename_country": "passed", "servers": len(rows),
                  "countries_known_before_measurement": known, "reset": "passed"}))
