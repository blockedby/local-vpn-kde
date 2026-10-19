#!/usr/bin/env python3
"""Opt-in source-profile acceptance in the dedicated disposable native VM.

Requires the configured installed application, lab-only Polkit authorization,
and passwordless lab sudo. Never run against a workstation or production VM.
Uses synthetic sources and never selects or connects a runtime server.
"""
import hashlib
import json
import os
import pathlib
import select
import subprocess

assert os.environ.get("VPNKIT_NATIVE_VM_LAB") == "1", "disposable-lab opt-in required"
assert pathlib.Path("/etc/hostname").read_text().strip() == "vpn-native-lab", "wrong lab"
os.umask(0o077)
private = pathlib.Path.home() / ".config/vpn-lab/source-profile-validation"
private.mkdir(parents=True, exist_ok=True, mode=0o700)


def digest(path):
    data = subprocess.check_output(["sudo", "-n", "cat", path])
    return hashlib.sha256(data).hexdigest()


paths = [
    "/var/lib/local-vpn-kde/vibe-vpn/current-node.json",
    "/etc/local-vpn-kde/sing-box/config.json",
]
before = [digest(path) for path in paths]
process = subprocess.Popen(
    ["/usr/bin/local-vpn-kde", "bridge"],
    stdin=subprocess.PIPE,
    stdout=subprocess.PIPE,
    stderr=subprocess.DEVNULL,
    text=True,
    bufsize=1,
)
sequence = 0
created = None
original = None
summary = []


def run(action, value=None, ok=True):
    global sequence
    sequence += 1
    request = {"id": "profile-lab-" + str(sequence), "action": action}
    if value is not None:
        request["value"] = value
    process.stdin.write(json.dumps(request) + "\n")
    process.stdin.flush()
    while True:
        assert select.select([process.stdout], [], [], 60)[0], "bridge request timed out"
        line = process.stdout.readline()
        assert line, "bridge exited without reply"
        reply = json.loads(line)
        if "ok" not in reply:
            continue
        assert reply["id"] == request["id"], "reply id mismatch"
        assert reply["ok"] == ok, "unexpected result for " + action
        summary.append({"action": action, "ok": reply["ok"], "expected_ok": ok})
        return reply


try:
    first = run("profiles/list")["profiles"]
    original = first["active"]
    assert first["items"], "missing existing subscription"
    sources = [
        ("Lab Hysteria2", "hy2://fixture@192.0.2.1#Lab%20Hysteria2"),
        ("Lab VLESS", "vless://fixture@192.0.2.2:443?security=tls#Lab%20VLESS"),
    ]
    for name, source in sources:
        added = run("profiles/add", json.dumps({"name": name, "source": source}))["profiles"]
        created = added["active"]
        profile = next(item for item in added["items"] if item["id"] == created)
        assert profile["kind"] == "server" and profile["count"] == 1, "invalid direct profile"
        serialized = json.dumps(added)
        assert "fixture@" not in serialized and "192.0.2." not in serialized, "source leaked"
        run("profiles/open", original)
        run("profiles/open", created)
        refreshed = run("servers/refresh")["catalog"]
        matching = [row for row in refreshed["servers"] if row.get("profile_id") == created]
        assert len(matching) == 1, "catalog source boundary lost"
        run("profiles/add", json.dumps({"name": name, "source": source}), ok=False)
        run(
            "profiles/add",
            json.dumps({"name": "Bad payload", "source": source, "config": "/tmp/escape"}),
            ok=False,
        )
        run("profiles/add", json.dumps({"name": "Bad payload", "source": source}) + " {}", ok=False)
        run("profiles/remove", created)
        created = None
        if original:
            run("profiles/open", original)

    # A valid prefix must not hide a second frame, even at the CLI boundary.
    bad = '{"action":"list"}{"action":"remove","id":"profile_missing"}'
    check = subprocess.run(
        [
            "sudo", "-n", "-u", "local-vpn-kde",
            "/usr/libexec/local-vpn-kde/vibe-vpn", "--config",
            "/etc/local-vpn-kde/vibe-vpn/config.yaml", "profiles",
        ],
        input=bad, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=15,
    )
    assert check.returncode != 0, "CLI accepted trailing frame"
    assert [digest(path) for path in paths] == before, "profile browsing changed runtime"
    final = run("profiles/list")["profiles"]
    assert final["active"] == original, "active source cleanup mismatch"
    assert len(final["items"]) == len(first["items"]), "source cleanup mismatch"
    (private / "summary.json").write_text(json.dumps(summary, indent=2))
    print("INSTALLED_NATIVE_PROFILE_BOUNDARIES=passed; protocols=hysteria2,vless; "
          "strict-input=passed; runtime-preserved=passed; cleanup=passed")
finally:
    if created:
        run("profiles/remove", created)
    if original:
        run("profiles/open", original)
    process.stdin.close()
    process.wait(timeout=35)
