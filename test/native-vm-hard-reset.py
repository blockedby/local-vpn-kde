#!/usr/bin/env python3
"""Run ONLY on the dedicated lab hypervisor; abruptly reset its owned QEMU guest.
No guest shutdown is requested. Checks exact container ownership, mounts,
QEMU arguments and QMP guest name before issuing system_reset.
"""
import json
import os
import pathlib
import socket
import subprocess
assert os.environ.get('VPNKIT_NATIVE_VM_LAB') == '1'
name = 'vpn-native-lab-vm-1'
info = json.loads(subprocess.check_output(['docker', 'inspect', name]))[0]
labels = info['Config']['Labels']
assert info['Name'] == '/' + name and info['State']['Running']
assert labels['com.docker.compose.project'] == 'vpn-native-lab'
assert labels['com.docker.compose.project.working_dir'] == '/opt/vpn-native-lab'
assert any(m['Source'] == '/opt/vpn-native-lab/state' and m['Destination'] == '/state' for m in info['Mounts'])
processes = subprocess.check_output(['docker', 'top', name, '-eo', 'pid,args'], text=True)
assert 'qemu-system-x86_64' in processes and '-name vpn-native-lab' in processes and 'file=/state/guest.qcow2' in processes
path = pathlib.Path('/opt/vpn-native-lab/state/qmp.sock')
assert path.is_socket() and path.stat().st_uid == 0
with socket.socket(socket.AF_UNIX) as connection:
    connection.settimeout(10)
    connection.connect(str(path))
    stream = connection.makefile('rwb', buffering=0)
    assert 'QMP' in json.loads(stream.readline())
    def command(action):
        stream.write(json.dumps({'execute': action}).encode() + b'\n')
        while True:
            result = json.loads(stream.readline())
            assert 'error' not in result, 'QMP command failed'
            if 'return' in result:
                return result['return']
    command('qmp_capabilities')
    assert command('query-name')['name'] == 'vpn-native-lab'
    command('system_reset')
print('DEDICATED_NATIVE_QEMU_ABRUPT_RESET=requested; guest_shutdown=not-requested')
