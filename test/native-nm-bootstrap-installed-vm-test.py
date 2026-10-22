#!/usr/bin/env python3
"""Real installed NM bootstrap/refusal acceptance; exclusive disposable VM only.

Requires a working connected native VPN, lab sudo/Polkit and the package under
test. Temporarily renames the original owned NM profile and removes its private
capability, then exercises installed start without a manual import. Keeps the
original UUID and restores its name/capability and connection in finally.
Never run on a workstation. Private profile/settings data stay in memory.
"""
import hashlib
import json
import os
from pathlib import Path
import queue
import subprocess
import tempfile
import threading
import time
import uuid

assert os.environ.get("VPNKIT_NATIVE_VM_LAB") == "1", "lab opt-in required"
assert Path("/etc/hostname").read_text().strip() == "vpn-native-lab", "wrong guest"
assert os.geteuid() != 0, "run as the lab desktop user"
os.umask(0o077)
state = Path(os.environ.get("XDG_STATE_HOME", str(Path.home()/".local/state")))/"local-vpn-kde"
capdir = state/"state-native"
assert capdir.is_dir() and not capdir.is_symlink() and capdir.stat().st_uid == os.getuid()
capnames = ("networkmanager-state", "networkmanager-uuid", "networkmanager-profile-fingerprint")
saved = {name: (capdir/name).read_bytes() if (capdir/name).exists() else None for name in capnames}
assert saved["networkmanager-state"], "requires an owned original profile"
original_uuid = saved["networkmanager-state"].decode().split()[0]
uuid.UUID(original_uuid)
suffix = uuid.uuid4().hex[:12]
backup_name = "vpnkit-bootstrap-backup-"+suffix
tampered_name = "vpnkit-bootstrap-tampered-"+suffix
foreign_name = "vpnkit-bootstrap-foreign-"+suffix
proc = subprocess.Popen(["/usr/bin/local-vpn-kde", "bridge"], stdin=subprocess.PIPE,
                        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True, bufsize=1)
replies = queue.Queue()
threading.Thread(target=lambda: [replies.put(json.loads(line)) for line in proc.stdout], daemon=True).start()
sequence = 0
generated = None
foreign = None
renamed_original = False
restore_active = False
summary = []
secondary = None
material_skips = ["secondary rival /1 Internet egress not tested"]


def record(case):
    summary.append(case)
    print(json.dumps({"case": case, "result": "passed"}), flush=True)


class SecondaryVPN:
    """Independent TLS OpenVPN peer; its full-tunnel overlap is route-only.

    The peer namespace intentionally has no Internet egress. A local HTTP echo
    verifies the real tunnel; restored rival /1 routes are not an Internet test.
    """
    def __init__(self):
        self.ns = "vpnkit-peer-"+suffix
        self.host = "vph"+suffix[:8]
        self.peer = "vpp"+suffix[:8]
        self.unit = "vpnkit-peer-"+suffix
        self.echo = self.unit+"-echo"
        self.identity = None
        self.transport = None
        # NM's OpenVPN service has a private /tmp. Keep its fixture credentials
        # in the same user-private home boundary as real imported VPN profiles.
        fixture_root = Path.home()/".config/vpn-lab"
        fixture_root.mkdir(mode=0o700, parents=True, exist_ok=True)
        self.tmp = tempfile.TemporaryDirectory(prefix="vpnkit-peer-", dir=fixture_root)
        self.path = Path(self.tmp.name)

    def root(self, *args):
        return subprocess.check_output(["sudo", "-n", *args], stderr=subprocess.DEVNULL)

    def start(self):
        assert not json.loads(subprocess.check_output(["ip", "-j", "route", "show", "exact", "198.19.1.0/30"])), "fixture subnet occupied"
        for label in ("ca", "server", "client"):
            args = ["openssl", "req", "-new", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1", "-nodes", "-keyout", str(self.path/(label+".key")), "-out", str(self.path/(label+".csr")), "-subj", "/CN=vpnkit-lab-"+label]
            if label == "ca":
                args += ["-x509", "-days", "1"]
                args[args.index(str(self.path/"ca.csr"))] = str(self.path/"ca.crt")
            subprocess.run(args, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=30)
            if label != "ca":
                ext = self.path/(label+".ext")
                ext.write_text("basicConstraints=CA:FALSE\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage="+("serverAuth" if label == "server" else "clientAuth")+"\n")
                subprocess.run(["openssl", "x509", "-req", "-in", str(self.path/(label+".csr")), "-CA", str(self.path/"ca.crt"), "-CAkey", str(self.path/"ca.key"), "-CAcreateserial", "-out", str(self.path/(label+".crt")), "-days", "1", "-extfile", str(ext)], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=30)
        server = self.path/"server.conf"
        server.write_text("\n".join(["dev lab-server", "dev-type tun", "disable-dco", "proto udp", "local 198.19.1.2", "port 33194", "topology subnet", "server 10.93.0.0 255.255.255.0", "dh none", "data-ciphers AES-256-GCM:AES-128-GCM", "keepalive 2 10", "verb 1"]+[key+" "+str(self.path/value) for key, value in (("ca", "ca.crt"), ("cert", "server.crt"), ("key", "server.key"))])+"\n")
        self.root("ip", "netns", "add", self.ns)
        self.root("ip", "link", "add", self.host, "type", "veth", "peer", "name", self.peer)
        self.root("ip", "link", "set", self.peer, "netns", self.ns)
        self.root("ip", "address", "add", "198.19.1.1/30", "dev", self.host)
        self.root("ip", "link", "set", self.host, "up")
        for args in (("address", "add", "198.19.1.2/30", "dev", self.peer), ("link", "set", self.peer, "up"), ("link", "set", "lo", "up")):
            self.root("ip", "netns", "exec", self.ns, "ip", *args)
        # Bind NM's VPN transport to this real connected veth. Otherwise NM
        # pins the remote /32 through eth0 and breaks the peer data channel.
        self.transport = str(uuid.uuid4())
        nm("connection", "add", "type", "ethernet", "ifname", self.host,
           "con-name", "vpnkit-peer-transport-"+suffix, "connection.uuid", self.transport,
           "connection.autoconnect", "no", "ipv4.method", "manual", "ipv4.addresses", "198.19.1.1/30",
           "ipv4.never-default", "yes", "ipv6.method", "disabled")
        nm("--wait", "15", "connection", "up", "uuid", self.transport)
        self.root("systemd-run", "--quiet", "--collect", "--unit", self.unit, "ip", "netns", "exec", self.ns, "openvpn", "--config", str(server))
        client = self.path/("vpnkit-secondary-"+suffix+".ovpn")
        client.write_text("\n".join(["client", "dev tun", "proto udp", "remote 198.19.1.2 33194", "nobind", "remote-cert-tls server", "data-ciphers AES-256-GCM:AES-128-GCM", "route-nopull", "verb 1"]+[key+" "+str(self.path/value) for key, value in (("ca", "ca.crt"), ("cert", "client.crt"), ("key", "client.key"))])+"\n")
        prior = inventory()
        nm("connection", "import", "type", "openvpn", "file", str(client))
        created = inventory()-prior
        assert len(created) == 1, "secondary import inventory"
        self.identity = created.pop()
        nm("connection", "modify", "uuid", self.identity, "connection.id", "vpnkit-secondary-"+suffix, "connection.autoconnect", "no", "ipv4.never-default", "yes", "ipv6.method", "disabled", "ipv4.route-metric", "50")
        nm("--wait", "30", "connection", "up", "uuid", self.identity, "ifname", self.host)
        # Echo is in the peer netns, so this cannot succeed via a host local route.
        echo_code = "import socket;s=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind(('10.93.0.1',33195));s.listen();\nwhile True:\n c,_=s.accept();c.recv(4096);c.sendall(b'HTTP/1.0 200 OK\\r\\nContent-Length: 19\\r\\n\\r\\nvpnkit-secondary-ok');c.close()"
        self.root("systemd-run", "--quiet", "--collect", "--unit", self.echo, "ip", "netns", "exec", self.ns, "python3", "-c", echo_code)
        deadline = time.monotonic()+10
        while True:
            check = subprocess.run(["curl", "--silent", "--fail", "--noproxy", "*", "--max-time", "2", "http://10.93.0.1:33195"], capture_output=True)
            if check.returncode == 0 and check.stdout == b"vpnkit-secondary-ok":
                break
            if time.monotonic() >= deadline:
                raise AssertionError("secondary tunnel echo failed")
            time.sleep(.2)
        self.root("ip", "route", "add", "0.0.0.0/1", "via", "10.93.0.1", "dev", self.device(), "metric", "50")
        self.root("ip", "route", "add", "128.0.0.0/1", "via", "10.93.0.1", "dev", self.device(), "metric", "50")

    def device(self):
        return mapped_device(self.identity)

    def stop(self):
        failures = []
        for args in (("systemctl", "stop", self.echo), ("systemctl", "stop", self.unit)):
            subprocess.run(["sudo", "-n", *args], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        if self.identity and self.identity in inventory():
            try:
                assert name(self.identity) == "vpnkit-secondary-"+suffix
                nm("connection", "delete", "uuid", self.identity)
            except BaseException:
                failures.append("secondary profile")
        if self.transport and self.transport in inventory():
            try:
                assert name(self.transport) == "vpnkit-peer-transport-"+suffix
                nm("connection", "delete", "uuid", self.transport)
            except BaseException:
                failures.append("secondary transport")
        for args in (("ip", "link", "delete", self.host), ("ip", "netns", "delete", self.ns)):
            subprocess.run(["sudo", "-n", *args], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.tmp.cleanup()
        assert not failures and self.ns not in subprocess.check_output(["ip", "netns", "list"]).decode(), "secondary fixture cleanup failed"


def run(action, ok=True):
    global sequence
    sequence += 1
    request_id = "nm-bootstrap-"+str(sequence)
    proc.stdin.write(json.dumps({"id": request_id, "action": action})+"\n")
    proc.stdin.flush()
    deadline = time.monotonic()+240
    while True:
        reply = replies.get(timeout=max(.01, deadline-time.monotonic()))
        assert reply.get("id") == request_id, "unexpected bridge reply"
        if "ok" not in reply:
            continue
        assert reply["ok"] == ok, "unexpected result: "+action
        return reply


def nm(*args):
    return subprocess.check_output(["sudo", "-n", "nmcli", *args], stderr=subprocess.DEVNULL)


def name(identity):
    return nm("-g", "connection.id", "connection", "show", "uuid", identity).decode().strip()


def profile_digest(identity):
    settings = nm("-f", "connection,vpn,ipv4,ipv6", "connection", "show", "uuid", identity)
    # NM updates this observation whenever a connection activates; it is not a
    # user setting, and restoring the original connection necessarily updates it.
    stable = b"\n".join(line for line in settings.splitlines() if not line.startswith(b"connection.timestamp:"))
    return hashlib.sha256(stable).hexdigest()


def inventory():
    return set(nm("-g", "UUID", "connection", "show").decode().splitlines())


def mapped_device(identity):
    # GENERAL.DEVICES identifies the VPN transport's underlay, not its TUN.
    addresses = set(nm("-g", "IP4.ADDRESS", "connection", "show", "uuid", identity).decode().splitlines())
    links = json.loads(subprocess.check_output(["ip", "-j", "-4", "address", "show"]))
    matches = [link["ifname"] for link in links if link["ifname"].startswith("tun") and any(str(a.get("local"))+"/"+str(a.get("prefixlen")) in addresses for a in link.get("addr_info", []))]
    assert len(matches) == 1, "ambiguous VPN TUN mapping"
    return matches[0]


def capability():
    value = (capdir/"networkmanager-state").read_text().split()[0]
    uuid.UUID(value)
    return value


def private_identity():
    paths = ["/var/lib/local-vpn-kde/vibe-vpn/current-node.json",
             "/var/lib/local-vpn-kde/vibe-vpn/source-profiles.json",
             "/etc/local-vpn-kde/vibe-vpn/sub_url"]
    return [hashlib.sha256(subprocess.check_output(["sudo", "-n", "cat", path])).hexdigest() for path in paths]


def connected(require_split=True):
    s = run("status")["status"]
    assert s.get("runtime") == "native" and s.get("installed") is True
    assert s.get("gateway_state") == "healthy" and s.get("networkmanager_active") == "yes"
    routes = json.loads(subprocess.check_output(["ip", "-j", "-4", "route", "show"]))
    if require_split:
        for destination in ("0.0.0.0/1", "128.0.0.0/1"):
            assert any(r.get("dst") == destination and r.get("dev", "").startswith("tun") for r in routes), "missing full-tunnel route"
    subprocess.run(["curl", "--silent", "--fail", "--noproxy", "*", "--max-time", "20",
                    "https://www.gstatic.com/generate_204"], check=True,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def delete_tracked(identity, expected_names):
    if identity and identity in inventory():
        assert identity != original_uuid and name(identity) in expected_names, "fixture ownership changed"
        nm("connection", "delete", "uuid", identity)


def restore_capability():
    for key, data in saved.items():
        target = capdir/key
        assert not target.is_symlink(), "capability replaced by symlink"
        if data is None:
            target.unlink(missing_ok=True)
        else:
            temporary = capdir/(key+".lab-restore-"+suffix)
            with temporary.open("xb") as out:
                out.write(data)
                out.flush()
                os.fsync(out.fileno())
            os.replace(temporary, target)


try:
    connected(require_split=False)  # The pre-upgrade profile can use a /0 route.
    restore_active = True
    # Apply the supported owned-profile metric migration before the fixture
    # snapshot. The new coexistence policy deliberately changes old metric 50
    # to 0; restoring the old priority would undo the feature under test.
    run("disconnect")
    run("start")
    connected()
    assert nm("-g", "ipv4.route-metric", "connection", "show", "uuid", original_uuid).decode().strip() == "0"
    assert name(original_uuid) == "vpnkit-native", "original ownership mismatch"
    original_settings = profile_digest(original_uuid)
    identities = private_identity()
    auto = run("autostart/read")["autostart"]
    run("stop")
    nm("connection", "modify", "uuid", original_uuid, "connection.id", backup_name)
    renamed_original = True
    for key in capnames:
        (capdir/key).unlink(missing_ok=True)
    assert "vpnkit-native" not in nm("-g", "NAME", "connection", "show").decode().splitlines()
    run("start")  # No networkmanager import command is permitted in this runner.
    generated = capability()
    assert generated != original_uuid and name(generated) == "vpnkit-native"
    connected()
    run("start")
    assert capability() == generated, "repeated start replaced the profile"
    connected()
    record("absent_profile_import_connect_and_idempotence")
    run("stop")  # Exercise the cold gateway lifecycle with a stale capability.
    stale_uuid = generated
    delete_tracked(stale_uuid, {"vpnkit-native"})
    run("start")
    generated = capability()
    assert generated not in (original_uuid, stale_uuid), "stale capability was not replaced"
    connected()
    record("stale_capability_missing_uuid_import_connect")
    run("disconnect")
    secondary = SecondaryVPN()
    secondary.start()
    rival_device = secondary.device()
    baseline_routes = [json.loads(subprocess.check_output(["ip", "-j", "route", "get", address]))[0]["dev"] for address in ("8.8.8.8", "200.1.1.1")]
    assert baseline_routes == [rival_device, rival_device], "rival /1 fixture did not win before native"
    run("start")
    connected()
    active_ids = set(nm("-g", "UUID", "connection", "show", "--active").decode().splitlines())
    assert {generated, secondary.identity} <= active_ids, "one VPN was disconnected"
    native_device = mapped_device(generated)
    for address in ("8.8.8.8", "200.1.1.1"):
        route = json.loads(subprocess.check_output(["ip", "-j", "route", "get", address]))[0]
        assert route["dev"] == native_device and native_device != rival_device, "native did not win both /1 halves"
    peer_route = json.loads(subprocess.check_output(["ip", "-j", "route", "get", "198.19.1.2"]))[0]
    assert peer_route["dev"] == secondary.host, "secondary transport recursively entered a VPN"
    underlay = json.loads(subprocess.check_output(["sudo", "-n", "ip", "-j", "route", "get", "8.8.8.8", "from", "172.30.90.2", "iif", "vpnkit-host"]))[0]
    assert underlay["dev"] not in (native_device, rival_device, secondary.host), "native underlay recursively entered a VPN"
    run("disconnect")
    assert secondary.identity in set(nm("-g", "UUID", "connection", "show", "--active").decode().splitlines())
    restored_routes = [json.loads(subprocess.check_output(["ip", "-j", "route", "get", address]))[0]["dev"] for address in ("8.8.8.8", "200.1.1.1")]
    assert restored_routes == baseline_routes, "previous VPN routes were not restored"
    secondary.stop()
    secondary = None
    record("real_secondary_nm_vpn_native_overlap_priority_and_restoration")
    # An existing owned UUID with a changed name must never be silently adopted.
    nm("connection", "modify", "uuid", generated, "connection.id", tampered_name)
    tampered = profile_digest(generated)
    run("start", ok=False)
    assert profile_digest(generated) == tampered, "tampered profile changed"
    assert run("status")["status"].get("networkmanager_active") != "yes"
    nm("connection", "modify", "uuid", generated, "connection.id", "vpnkit-native")
    record("tampered_owned_profile_refused_unchanged")
    prior = inventory()
    nm("connection", "clone", "uuid", generated, foreign_name)
    created = inventory()-prior
    assert len(created) == 1, "unexpected clone inventory"
    foreign = created.pop()
    nm("connection", "modify", "uuid", foreign, "connection.id", "vpnkit-native")
    both = {item: profile_digest(item) for item in (generated, foreign)}
    run("start", ok=False)
    assert {item: profile_digest(item) for item in both} == both, "collision profiles changed"
    assert run("status")["status"].get("networkmanager_active") != "yes"
    record("foreign_same_name_collision_refused_unchanged")
finally:
    cleanup_errors = []
    def cleanup_step(label, operation):
        try:
            operation()
        except BaseException:
            cleanup_errors.append(label)
    cleanup_step("foreign clone", lambda: delete_tracked(foreign, {"vpnkit-native", foreign_name}))
    def recover_fixture_identity():
        global generated
        if (capdir/"networkmanager-state").exists():
            candidate = capability()
            if candidate != original_uuid:
                generated = candidate
        if generated and generated in inventory() and name(generated) == tampered_name:
            nm("connection", "modify", "uuid", generated, "connection.id", "vpnkit-native")
    cleanup_step("fixture identity", recover_fixture_identity)
    if secondary is not None:
        cleanup_step("secondary VPN", secondary.stop)
    if renamed_original:
        cleanup_step("stop fixture", lambda: run("stop"))
        cleanup_step("generated profile", lambda: delete_tracked(generated, {"vpnkit-native"}))
        cleanup_step("original name", lambda: nm("connection", "modify", "uuid", original_uuid, "connection.id", "vpnkit-native"))
        cleanup_step("original capability", restore_capability)
        cleanup_step("restore original connection", lambda: run("start"))
        cleanup_step("restored full tunnel", connected)
        cleanup_step("original settings", lambda: profile_digest(original_uuid) == original_settings or (_ for _ in ()).throw(AssertionError()))
        cleanup_step("private identity", lambda: private_identity() == identities or (_ for _ in ()).throw(AssertionError()))
        cleanup_step("autostart", lambda: run("autostart/read")["autostart"] == auto or (_ for _ in ()).throw(AssertionError()))
    elif restore_active:
        cleanup_step("restore initial active connection", lambda: run("start"))
    proc.stdin.close()
    try:
        proc.wait(timeout=30)
    except subprocess.TimeoutExpired:
        proc.terminate()
        proc.wait(timeout=5)
    assert not cleanup_errors, "lab cleanup failed: "+", ".join(cleanup_errors)
print(json.dumps({"installed_native_nm_bootstrap": "passed", "cases": summary,
                  "material_skips": material_skips,
                  "original_uuid_settings_private_identity_autostart_restored": True}))
