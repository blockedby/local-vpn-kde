#!/usr/bin/env python3
"""Real installed HTTPS subscription refresh/cache E2E in disposable VM only.

Requires exclusive vpn-native-lab access, installed configured native app with
a running gateway/namespace and selected server, lab
Polkit authorization, passwordless sudo, openssl and update-ca-trust (Arch)
or update-ca-certificates (Debian).
Installs one temporary lab CA, serves loopback HTTPS, adds one synthetic profile,
and removes both in finally. Never opens, exports, or rewrites real sources.
"""
import json
import os
import pathlib
import queue
import select
import shutil
import subprocess
import tempfile
import threading

assert os.environ.get("VPNKIT_NATIVE_VM_LAB") == "1", "disposable-lab opt-in required"
assert pathlib.Path("/etc/hostname").read_text().strip() == "vpn-native-lab", "wrong lab"
os.umask(0o077)
mode = "first"
requests = []
# Serve inside the same real network namespace used by installed operations.
# Root is used only for entering the disposable namespace; no worker shortcut.
SERVER = r"""
import http.server, json, pathlib, ssl, sys
cert, key, control = sys.argv[1:]
class Handler(http.server.BaseHTTPRequestHandler):
 def do_GET(self):
  mode = pathlib.Path(control).read_text()
  print(mode, flush=True)
  links = {'first': b'vless://fixture@192.0.2.11:443?security=tls#Cache%20First\n',
           'second': b'hy2://fixture@192.0.2.12:443#Cache%20Second\n',
           'invalid': b'this is not a subscription\n', 'empty': b''}
  self.send_response(503 if mode == 'failure' else 200)
  self.end_headers()
  self.wfile.write(links.get(mode, b'unavailable'))
 def log_message(self, *_args): pass
server = http.server.HTTPServer(('127.0.0.1', 0), Handler)
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(cert, key)
server.socket = ctx.wrap_socket(server.socket, server_side=True)
server.timeout = 0.2
print(server.server_port, flush=True)
while pathlib.Path(control).read_text() != 'shutdown': server.handle_request()
server.server_close()
"""


proc = None
server = None
created = None
original = None
sequence = 0
summary = []
replies = queue.Queue()


def run(action, value=None):
    global sequence
    sequence += 1
    req = {"id": "cache-lab-" + str(sequence), "action": action}
    if value is not None:
        req["value"] = value
    proc.stdin.write(json.dumps(req) + "\n")
    proc.stdin.flush()
    while True:
        reply = replies.get(timeout=90)
        assert reply.get("id") == req["id"], "unexpected response ID"
        if "ok" in reply:
            assert reply["ok"], "unexpected bridge failure: " + action
            summary.append({"action": action, "outcome": "passed", "fixture_mode": mode})
            return reply


def profile():
    return next(item for item in run("profiles/list")["profiles"]["items"] if item["id"] == created)


def rows():
    return [row for row in run("servers/list")["catalog"]["servers"] if row.get("profile_id") == created]


def runtime_hashes():
    import hashlib
    return [hashlib.sha256(subprocess.check_output(["sudo", "-n", "cat", path])).hexdigest()
            for path in ("/var/lib/local-vpn-kde/vibe-vpn/current-node.json",
                         "/etc/local-vpn-kde/sing-box/config.json",
                         "/var/lib/local-vpn-kde/sing-box/config.json")]


with tempfile.TemporaryDirectory(prefix="native-cache-https-") as temporary:
    base = pathlib.Path(temporary)
    cert, key = base / "lab.crt", base / "lab.key"
    control = base / "mode"
    control.write_text(mode)
    if shutil.which("update-ca-trust"):
        anchor = "/etc/ca-certificates/trust-source/anchors/" + base.name + ".crt"
        update_trust = ["sudo", "-n", "update-ca-trust"]
    else:
        assert shutil.which("update-ca-certificates"), "no supported system CA trust updater"
        anchor = "/usr/local/share/ca-certificates/" + base.name + ".crt"
        update_trust = ["sudo", "-n", "update-ca-certificates"]
    subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                    "-keyout", str(key), "-out", str(cert), "-subj", "/CN=Native cache disposable lab",
                    "-addext", "subjectAltName=IP:127.0.0.1", "-addext", "basicConstraints=critical,CA:TRUE"],
                   check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    before = runtime_hashes()
    try:
        subprocess.run(["sudo", "-n", "install", "-m", "0644", str(cert), anchor], check=True)
        subprocess.run(update_trust, check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        server = subprocess.Popen(["sudo", "-n", "ip", "netns", "exec", "vpnkit-native",
                                   "python3", "-u", "-c", SERVER, str(cert), str(key), str(control)],
                                  stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True, bufsize=1)
        assert select.select([server.stdout], [], [], 30)[0], "HTTPS fixture startup timed out"
        port = int(server.stdout.readline().strip())
        threading.Thread(target=lambda: [requests.append(line.strip()) for line in server.stdout], daemon=True).start()
        proc = subprocess.Popen(["/usr/bin/local-vpn-kde", "bridge"], stdin=subprocess.PIPE,
                                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True, bufsize=1)
        threading.Thread(target=lambda: [replies.put(json.loads(line)) for line in proc.stdout], daemon=True).start()
        initial = run("profiles/list")["profiles"]
        original = initial["active"]
        added = run("profiles/add", json.dumps({"name": "Lab HTTPS cache lifecycle",
                    "source": "https://127.0.0.1:" + str(port) + "/subscription"}))["profiles"]
        created = added["active"]
        assert profile()["kind"] == "subscription"
        run("servers/refresh")
        first = rows()
        assert len(first) == 1 and first[0]["display_name"] == "Cache First"
        assert profile()["count"] == 1 and not profile()["stale"]
        mode = "second"
        control.write_text(mode)
        run("servers/refresh")
        last_good = rows()
        assert len(last_good) == 1 and last_good[0]["display_name"] == "Cache Second"
        assert first[0]["server_id"] != last_good[0]["server_id"], "successful refresh did not replace source cache"
        for mode in ("failure", "invalid", "empty"):
            control.write_text(mode)
            run("servers/refresh")
            assert profile()["stale"] and profile()["count"] == 1
            assert rows() == last_good, "failed fetch discarded or changed last-good cache"
        mode = "second"
        control.write_text(mode)
        run("servers/refresh")
        assert not profile()["stale"] and rows() == last_good, "source did not recover"
        assert all(value in requests for value in ("first", "second", "failure", "invalid", "empty")), "fixture transport bypassed"
        run("profiles/remove", created)
        created = None
        if original:
            run("profiles/open", original)
        assert run("profiles/list")["profiles"] == initial, "original profiles changed"
        assert runtime_hashes() == before, "subscription refresh changed selected runtime"
        private = pathlib.Path.home() / ".config/vpn-lab/subscription-cache-validation"
        private.mkdir(parents=True, exist_ok=True, mode=0o700)
        (private / "summary.json").write_text(json.dumps(summary, indent=2))
        print("INSTALLED_NATIVE_HTTPS_SUBSCRIPTION_CACHE=passed; refresh=replace; failures=http503,invalid,empty; last-good=preserved; recovery=passed; runtime=preserved")
    finally:
        try:
            if proc:
                try:
                    if created:
                        run("profiles/remove", created)
                finally:
                    if original:
                        run("profiles/open", original)
        finally:
            try:
                if proc:
                    proc.stdin.close()
                    proc.wait(timeout=35)
            finally:
                try:
                    if server:
                        control.write_text("shutdown")
                        server.wait(timeout=15)
                finally:
                    subprocess.run(["sudo", "-n", "rm", "-f", anchor], check=True)
                    subprocess.run(update_trust, check=True,
                                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
