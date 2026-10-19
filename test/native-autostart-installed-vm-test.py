#!/usr/bin/env python3
"""Host orchestrator: six REAL disposable-guest reboots and session autostart.

VPNKIT_NATIVE_VM_LAB=1 python3 test/native-autostart-installed-vm-test.py \
  --ssh /path/to/vm-lab/ssh.sh

Requires exclusive use of vpn-native-lab, installed/configured native package,
lab-only Polkit authorization, passwordless sudo, working HTTPS and SSH wrapper.
The headless guest has no desktop: explicitly starting graphical-session.target
models desktop login AFTER reboot. This does not prove display-manager login.
Temporary lingering keeps that synthetic session alive between SSH commands.
No host network mutation; reboot commands are sent only after guest identity checks.
On success/failure restore saved autostart, runtime connection and linger intent.
A killed orchestrator can leave the disposable guest in the current test state.
"""
import argparse
import json
import os
import shlex
import subprocess
import time

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--ssh', required=True, help='dedicated lab ssh.sh wrapper')
args = parser.parse_args()
if os.environ.get('VPNKIT_NATIVE_VM_LAB') != '1':
    parser.error('VPNKIT_NATIVE_VM_LAB=1 required')
ssh = os.path.abspath(args.ssh)
unit = 'local-vpn-kde-native-autostart.service'
session_unit = 'vpnkit-lab-graphical-session.target'
cli = '/usr/bin/local-vpn-kde'


def remote(command, timeout=180, check=True):
    # Re-check inside EVERY remote execution, including destructive commands.
    guarded = 'test "$(cat /etc/hostname)" = vpn-native-lab && test "$(id -un)" = lab && ' + '{ ' + command + '; }'
    result = subprocess.run([ssh, guarded], capture_output=True, text=True, timeout=timeout)
    if check and result.returncode:
        raise RuntimeError('guest operation failed (private SSH output suppressed)')
    return result


def run_cli(*arguments):
    return remote(shlex.join([cli, *arguments])).stdout


def snapshot():
    script = r'''
import hashlib,json,pathlib,subprocess
r=lambda a: subprocess.run(a,capture_output=True,text=True).stdout.strip()
c=json.loads(subprocess.check_output(['sudo','-n','cat','/etc/local-vpn-kde/sing-box/config.json']))
print(json.dumps({'boot':pathlib.Path('/proc/sys/kernel/random/boot_id').read_text().strip(),
'gateway':r(['systemctl','is-active','local-vpn-kde-gateway'])=='active',
'connected':any(x=='vpnkit-native:vpn' for x in r(['nmcli','-t','-f','NAME,TYPE','connection','show','--active']).splitlines()),
'route_tun':'tun' in r(['ip','route','get','1.1.1.1']),
'config_hash':hashlib.sha256(json.dumps(c,sort_keys=True).encode()).hexdigest(),
'mode':'smart' if any(x.get('outbound')=='direct-out' for x in c['route']['rules']) else 'strict'}))
'''
    return json.loads(remote('python3 -c ' + shlex.quote(script)).stdout)


def reboot(old_boot):
    remote('sudo -n systemctl reboot', timeout=30, check=False)
    deadline = time.monotonic() + 180
    while time.monotonic() < deadline:
        time.sleep(3)
        try:
            state = snapshot()
            if state['boot'] != old_boot:
                return state
        except (RuntimeError, subprocess.TimeoutExpired, json.JSONDecodeError):
            pass
    raise RuntimeError('guest did not return with a different kernel boot ID')


def https():
    remote('curl --silent --fail --max-time 25 https://www.gstatic.com/generate_204 >/dev/null', timeout=35)


saved = json.loads(run_cli('autostart', '--runtime', 'native', '--action', 'status'))
before = snapshot()
linger = remote('loginctl show-user lab -p Linger --value').stdout.strip()
assert linger in ('yes', 'no'), 'unknown linger state'
results = []
fixture_owned = False
try:
    remote('sudo -n loginctl enable-linger lab')
    fixture = '[Unit]\nDescription=Disposable VPN lab graphical session\nWants=graphical-session.target\nBefore=graphical-session.target\n'
    path = '~/.config/systemd/user/' + session_unit
    remote('test ! -e ' + path + ' && mkdir -p ~/.config/systemd/user && printf %s ' + shlex.quote(fixture) + ' >' + path + ' && systemctl --user daemon-reload')
    fixture_owned = True
    for mode, action in ((mode, action) for mode in ('strict', 'smart') for action in ('off', 'gateway', 'connect')):
        if snapshot()['mode'] != mode:
            run_cli('native-desktop', '--action', 'toggle-mode', '--yes')
        run_cli('native-desktop', '--action', 'stop', '--yes')
        remote('systemctl --user stop ' + session_unit + ' graphical-session.target')
        run_cli('autostart', '--runtime', 'native', '--action', action)
        baseline = snapshot()
        assert not baseline['gateway'] and not baseline['connected'], 'saving autostart started runtime'
        booted = reboot(baseline['boot'])
        assert not booted['gateway'] and not booted['connected'], 'runtime started before graphical session'
        remote('systemctl --user start --no-block ' + session_unit)
        deadline = time.monotonic() + 180
        while True:
            current = snapshot()
            if current['gateway'] == (action != 'off') and current['connected'] == (action == 'connect'):
                break
            assert time.monotonic() < deadline, 'autostart runtime state timed out: ' + action
            time.sleep(2)
        assert current['config_hash'] == baseline['config_hash'], 'configuration changed over reboot'
        assert current['mode'] == mode, 'policy mode not persistent'
        assert current['route_tun'] == (action == 'connect'), 'desktop route does not match expected VPN state'
        time.sleep(5)
        settled = snapshot()
        assert settled['gateway'] == current['gateway'] and settled['connected'] == current['connected'], 'autostart state did not settle'
        https()
        if action == 'off':
            disabled = remote('systemctl --user is-enabled ' + unit, check=False).stdout.strip()
            inactive = remote('systemctl --user is-active ' + unit, check=False).stdout.strip()
            assert disabled == 'disabled' and inactive == 'inactive', 'off unit remains enabled or active'
        if action != 'off':
            # Exercise sing-box inside the namespace even for service-only mode.
            remote('sudo -n ip netns exec vpnkit-native curl --silent --fail --max-time 25 --socks5-hostname 127.0.0.1:2080 https://www.gstatic.com/generate_204 >/dev/null', timeout=35)
            active = remote('systemctl --user is-active ' + unit).stdout.strip()
            assert active == 'active', 'autostart unit did not complete successfully'
        results.append({'mode': mode, 'case': action, 'real_reboot': True, 'mode_persisted': True, 'https': True,
                        'gateway': current['gateway'], 'desktop_vpn': current['connected']})
        print(json.dumps(results[-1]), flush=True)
finally:
    # Attempt every independent restoration step, even when one fails. Do not
    # obscure the original assertion/connection failure with cleanup failures.
    import sys
    original_error = sys.exc_info()[0] is not None
    cleanup_failures = []
    def cleanup(operation):
        try:
            operation()
        except Exception:
            cleanup_failures.append('guest restoration operation failed')
    original = 'connect' if saved.get('connect') else 'gateway' if saved.get('gateway') else 'off'
    cleanup(lambda: run_cli('autostart', '--runtime', 'native', '--action', original))
    cleanup(lambda: remote('systemctl --user stop graphical-session.target'))
    cleanup(lambda: run_cli('native-desktop', '--action', 'stop', '--yes'))
    def restore_mode():
        if snapshot()['mode'] != before['mode']:
            run_cli('native-desktop', '--action', 'toggle-mode', '--yes')
            run_cli('native-desktop', '--action', 'stop', '--yes')
    cleanup(restore_mode)
    if before['gateway']:
        cleanup(lambda: run_cli('native-desktop', '--action', 'start' if before['connected'] else 'backend-start', '--yes'))
    if fixture_owned:
        cleanup(lambda: remote('systemctl --user stop ' + session_unit + ' && unlink ~/.config/systemd/user/' + session_unit + ' && systemctl --user daemon-reload'))
    if linger == 'no':
        cleanup(lambda: remote('sudo -n loginctl disable-linger lab'))
    if cleanup_failures:
        print(json.dumps({'cleanup': 'incomplete', 'failed_operations': len(cleanup_failures)}), flush=True)
        if not original_error:
            raise RuntimeError('guest restoration incomplete')
print(json.dumps({'native_autostart_real_reboot': 'passed', 'cases': len(results),
                  'session': 'headless graphical-session.target activation; display manager untested'}))
