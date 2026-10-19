#!/usr/bin/env python3
"""Run only in the dedicated disposable guest; never on the host/VPS itself."""
import json
import os
from pathlib import Path
import subprocess
import sys
import threading
import time

if os.environ.get('VPNKIT_NATIVE_VM_LAB') != '1' or Path('/etc/hostname').read_text().strip() != 'vpn-native-lab':
    raise SystemExit('dedicated VM opt-in required')
if len(sys.argv) != 2 or not Path(sys.argv[1]).is_file():
    raise SystemExit('provide a local candidate package')
os.umask(0o077)

def read(args):
    return subprocess.check_output(args, text=True).strip()

def snapshot():
    pid = read(['systemctl','show','local-vpn-kde-gateway','-p','MainPID','--value'])
    assert int(pid) > 1
    return {
        'gateway':pid,
        'children':read(['pgrep','-P',pid]),
        'namespace':read(['stat','-Lc','%i','/run/netns/vpnkit-native']),
        'connections':sorted(read(['nmcli','-t','-f','UUID,TYPE','connection','show','--active']).splitlines()),
        'route':read(['ip','route','get','1.1.1.1']).split('cache')[0].strip(),
        'pinned':read(['sha256sum','/var/lib/local-vpn-kde-runtime/sing-box','/var/lib/local-vpn-kde-runtime/vibe-vpn']),
    }

before=snapshot()
assert 'tun' in before['route'], 'guest must be connected through VPN'
samples=[]
stop=threading.Event()
def traffic():
    while not stop.is_set():
        result=subprocess.run(['curl','--silent','--fail','--max-time','8','https://www.gstatic.com/generate_204'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
        samples.append(result.returncode == 0)
        stop.wait(.2)
thread=threading.Thread(target=traffic)
thread.start()
try:
    deadline=time.monotonic()+30
    while len(samples)<3 and time.monotonic()<deadline:time.sleep(.1)
    assert len(samples)>=3 and all(samples), 'baseline HTTPS failed'
    with open('/tmp/native-hot-upgrade-private.log','w') as log:
        result=subprocess.run(['sudo','-n','pacman','-U','--noconfirm',sys.argv[1]],stdout=log,stderr=subprocess.STDOUT)
    assert result.returncode==0, 'package upgrade failed; private log retained in guest'
    time.sleep(3)
    assert snapshot()==before, 'runtime identity, route or connection changed'
    assert not Path('/run/local-vpn-kde-package-blocked').exists(), 'upgrade inhibitor remains'
finally:
    stop.set();thread.join(timeout=10)
assert samples and all(samples), 'HTTPS interrupted during upgrade'
print(json.dumps({'hot_upgrade':'passed','https_samples':len(samples),'https_failures':samples.count(False),'runtime_pids_namespace_routes':'unchanged'}))
