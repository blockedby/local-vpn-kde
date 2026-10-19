#!/usr/bin/env python3
"""PTY acceptance for the installed compiled UI in a disposable container."""
import errno
import os
import pty
import select
import signal
import subprocess
import time

assert not os.path.exists('/usr/share/local-vpn-kde/scripts/vpnkit/tui/node_modules')
assert not os.path.exists('/usr/share/local-vpn-kde/scripts/vpnkit/tui/index.ts')
# Compile regression must run without the system Bun or sing-box executable.
for binary in ('/usr/bin/bun', '/usr/bin/sing-box'):
    if os.path.exists(binary):
        os.rename(binary, binary + '.portable-lab-disabled')

subprocess.run(['/usr/libexec/local-vpn-kde/sing-box', 'version'], check=True, stdout=subprocess.DEVNULL)

def descendants(pid):
    try:
        with open(f'/proc/{pid}/task/{pid}/children') as source:
            children = [int(value) for value in source.read().split()]
    except FileNotFoundError:
        return []
    result = []
    for child in children:
        try:
            with open(f'/proc/{child}/cmdline', 'rb') as source:
                result.append(source.read())
        except FileNotFoundError:
            continue
        result.extend(descendants(child))
    return result

for arguments in (['--demo'], []):
    master, slave = pty.openpty()
    child = subprocess.Popen(['runuser', '-u', 'builder', '--', '/usr/bin/local-vpn-kde', 'ui', *arguments],
                             stdin=slave, stdout=slave, stderr=slave,
                             env={**os.environ, 'TERM': 'xterm-256color'}, start_new_session=True)
    os.close(slave)
    output = bytearray()
    try:
        deadline = time.monotonic() + 8
        while time.monotonic() < deadline:
            if select.select([master], [], [], 0.1)[0]:
                try:
                    data = os.read(master, 65536)
                except OSError as error:
                    if error.errno == errno.EIO:
                        break
                    raise
                output.extend(data)
                # OpenTUI's renderer waits for terminal capability responses.
                if b'\x1b[6n' in data:
                    os.write(master, b'\x1b[1;1R')
            if b'VPNKIT' in output:
                break
            if child.poll() is not None:
                break
        assert b'VPNKIT' in output, output.decode(errors='replace')[-3000:]
        assert child.poll() is None, output.decode(errors='replace')[-3000:]
        if not arguments:
            commands = descendants(child.pid)
            expected = b'/usr/libexec/local-vpn-kde/local-vpn-kde.bin\0bridge\0--repo\0/usr/share/local-vpn-kde/'
            assert any(command.startswith(expected) for command in commands), commands
        os.write(master, b'\x03')
        child.wait(timeout=8)
        assert child.returncode == 0, (child.returncode, output.decode(errors='replace')[-2000:])
    finally:
        if child.poll() is None:
            os.killpg(child.pid, signal.SIGKILL)
            child.wait()
        os.close(master)
print('compiled_tui_without_bun_or_node_modules=passed')
