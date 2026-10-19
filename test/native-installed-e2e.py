#!/usr/bin/env python3
"""Run installed native acceptance sequentially in the exclusive disposable VM.

Requires the configured vpn-native-lab guest, an active working native connection,
passwordless lab sudo/Polkit, and an SSH wrapper accepting one remote command.
The guest must support reboot (QEMU must not use -no-reboot).
This controls only the dedicated guest; never use a workstation SSH wrapper.
"""
import argparse
import os
from pathlib import Path
import shlex
import stat
import subprocess
import sys

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--ssh', required=True)
parser.add_argument('--package', type=Path, help='optional candidate Arch package to hot-upgrade first')
parser.add_argument('--server-file', type=Path, help='optional private mode0600 alternate Hysteria2 source file')
parser.add_argument('--skip-reboot', action='store_true', help='explicitly omit the six reboot cases')
args = parser.parse_args()
if os.environ.get('VPNKIT_NATIVE_VM_LAB') != '1':
    parser.error('VPNKIT_NATIVE_VM_LAB=1 required')
ssh = str(Path(args.ssh).resolve())
root = Path(__file__).resolve().parent
guard = 'test "$(cat /etc/hostname)" = vpn-native-lab && test "$(id -un)" = lab && '


def remote(command, **kwargs):
    return subprocess.run([ssh, guard + command], check=True, **kwargs)


def guest_test(name, extra='', environment=''):
    print('RUN ' + name, flush=True)
    # Scripts consume no stdin; supply public source directly, with no checkout
    # mounts, private sources, guest files or credentials copied back to the host.
    remote('VPNKIT_NATIVE_VM_LAB=1 ' + environment + ' python3 - ' + extra,
           input=(root / name).read_bytes())


remote('pacman -Q local-vpn-kde-native')
if args.package:
    with args.package.open('rb') as package:
        path = remote('mktemp /var/tmp/native-e2e-package.XXXXXX', capture_output=True, text=True).stdout.strip()
        if not path.startswith('/var/tmp/native-e2e-package.') or '\n' in path:
            raise RuntimeError('unexpected guest temporary path')
        try:
            remote('cat > ' + shlex.quote(path), stdin=package)
            guest_test('native-hot-upgrade-vm-test.py', shlex.quote(path))
            remote('pacman -Q local-vpn-kde-native')
        finally:
            remote('rm -f -- ' + shlex.quote(path))
for script in ('native-policy-installed-vm-test.py', 'native-profiles-installed-vm-test.py',
               'native-subscription-cache-installed-vm-test.py', 'native-lifecycle-installed-vm-test.py'):
    guest_test(script)
if args.server_file:
    info = args.server_file.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o600:
        raise RuntimeError('alternate source must be an owned regular mode0600 file')
    path = remote('mktemp /var/tmp/native-e2e-source.XXXXXX', capture_output=True, text=True).stdout.strip()
    if not path.startswith('/var/tmp/native-e2e-source.') or '\n' in path:
        raise RuntimeError('unexpected guest temporary path')
    try:
        with args.server_file.open('rb') as source:
            remote('cat > ' + shlex.quote(path), stdin=source)
        guest_test('native-server-switch-installed-vm-test.py',
                   environment='VPNKIT_NATIVE_LAB_SERVER_FILE=' + shlex.quote(path))
    finally:
        remote('rm -f -- ' + shlex.quote(path))
else:
    print('SKIP real alternate server switch (provide --server-file)', flush=True)
if args.skip_reboot:
    print('SKIP real reboot acceptance (--skip-reboot)', flush=True)
else:
    subprocess.run([sys.executable, str(root / 'native-autostart-installed-vm-test.py'), '--ssh', ssh], check=True)
print('installed_native_e2e=passed; reboot=' + ('skipped' if args.skip_reboot else 'passed'), flush=True)
