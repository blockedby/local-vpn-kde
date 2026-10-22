#!/usr/bin/env python3
"""Run only in the disposable Debian package container, without network/systemd."""
import fcntl
import json
import os
from pathlib import Path
import subprocess
import sys

assert os.environ.get("VPNKIT_DEBIAN_PACKAGE_LAB") == "1"
assert not Path("/run/systemd/system").exists()
package = sys.argv[1]
def run(*args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)
cli = "/usr/libexec/local-vpn-kde/local-vpn-kde.bin"
run("dpkg", "-i", package)
result = run("runuser", "-u", "builder", "--", "/usr/bin/local-vpn-kde", "ui", "--status-json", capture_output=True)
status = json.loads(result.stdout)
assert status["runtime"] == "native" and status["installed"] is True
run("runuser", "-u", "builder", "--", "/usr/bin/local-vpn-kde", "bridge", input=b"")
assert Path("/home/builder/.local/state/local-vpn-kde").stat().st_mode & 0o777 == 0o700
# Verify persistent POSIX locks, not lock-file existence, gate recovery.
marker = Path("/run/local-vpn-kde-package-blocked")
marker.write_text("native-package-transaction-v1\n")
marker.chmod(0o600)
for name in ("lock-frontend", "lock"):
    with open("/var/lib/dpkg/" + name, "r+") as lock:
        fcntl.lockf(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        blocked = subprocess.run([cli, "native-package", "--action", "recover"], capture_output=True)
        assert blocked.returncode != 0 and marker.exists(), (name, blocked.stdout, blocked.stderr)
run(cli, "native-package", "--action", "recover")
assert not marker.exists()
# Existing regression checks real compiled TUI and bridge with no system Bun.
run("python3", "/work/source/test/compiled-tui-installed-test.py")
run("dpkg", "-i", package)
retained = Path("/etc/local-vpn-kde/debian-lab-retain")
retained.write_text("preserve")
run("dpkg", "-r", "local-vpn-kde-native")
assert not Path("/usr/bin/local-vpn-kde").exists()
assert retained.read_text() == "preserve"
run("getent", "passwd", "local-vpn-kde", stdout=subprocess.DEVNULL)
run("dpkg", "--purge", "local-vpn-kde-native")
assert retained.read_text() == "preserve"
print("PASS: Debian install, compiled TUI, dpkg lock recovery, reinstall, remove/purge with private data retained")
