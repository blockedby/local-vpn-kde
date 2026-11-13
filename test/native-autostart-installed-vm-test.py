#!/usr/bin/env python3
"""Host orchestrator: REAL active-runtime disposable-guest reboots and session autostart.

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
parser.add_argument('--case-group', choices=('all', 'runtime', 'holder', 'smart-connect'), default='all', help='focused rerun of active/off runtime cases, namespace-holder cases, or smart connected normal reboot')
parser.add_argument('--repeat', type=int, default=1, help='repeat selected cases (1 to 100)')
parser.add_argument('--qmp-host', help='dedicated hypervisor SSH alias; enables abrupt QMP reset cases')
parser.add_argument('--qmp-control-socket', help='optional existing SSH multiplex socket for hypervisor')
args = parser.parse_args()
if not 1 <= args.repeat <= 100:
    parser.error('--repeat must be between 1 and 100')
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
jdata=subprocess.run(['sudo','-n','cat','/var/lib/local-vpn-kde-network/journal.json'],capture_output=True,text=True)
j=json.loads(jdata.stdout) if jdata.returncode==0 else {}
print(json.dumps({'boot':pathlib.Path('/proc/sys/kernel/random/boot_id').read_text().strip(),
'gateway':r(['systemctl','is-active','local-vpn-kde-gateway'])=='active',
'connected':any(x=='vpnkit-native:vpn' for x in r(['nmcli','-t','-f','NAME,TYPE','connection','show','--active']).splitlines()),
'route_tun':'tun' in r(['ip','route','get','1.1.1.1']),
'config_hash':hashlib.sha256(json.dumps(c,sort_keys=True).encode()).hexdigest(),
'journal_version':j.get('Version'),'journal_boot':j.get('BootID'),
'mode':'smart' if any(x.get('outbound')=='direct-out' for x in c['route']['rules']) else 'strict'}))
'''
    return json.loads(remote('python3 -c ' + shlex.quote(script)).stdout)


def reboot(old_boot, abrupt=False):
    if abrupt:
        import pathlib
        helper = pathlib.Path(__file__).with_name('native-vm-hard-reset.py').read_text()
        command = ['ssh']
        if args.qmp_control_socket:
            command += ['-S', os.path.abspath(args.qmp_control_socket)]
        command += [args.qmp_host, 'VPNKIT_NATIVE_VM_LAB=1 python3 -']
        subprocess.run(command, input=helper, text=True, check=True, timeout=30)
    else:
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


def failure_evidence():
    # Collect only fixed fields and categories; journals, routes, resolver
    # addresses and configuration may contain private endpoints. This runs
    # before cleanup and never transfers those raw inputs off the guest.
    script = r'''
import json,os,pathlib,re,subprocess,tempfile
def run(args):
 p=subprocess.run(args,capture_output=True,text=True,timeout=30)
 return p.returncode,p.stdout
os.umask(0o077)
evidence=pathlib.Path(tempfile.mkdtemp(prefix='vpnkit-reboot-evidence-',dir='/var/tmp'))
def capture(name,args):
 try:
  code,out=run(args)
  (evidence/name).write_text('exit='+str(code)+'\n'+out)
 except (OSError,subprocess.TimeoutExpired):
  (evidence/name).write_text('capture unavailable\n')
for name,command in {
 'system-units.txt':['systemctl','show','local-vpn-kde-network.service','local-vpn-kde-gateway.service','NetworkManager.service','network-online.target'],
 'autostart-unit.txt':['systemctl','--user','show','local-vpn-kde-native-autostart.service'],
 'system-journal.txt':['sudo','-n','journalctl','-b','-n','1000','-u','local-vpn-kde-network.service','-u','local-vpn-kde-gateway.service','-u','NetworkManager.service','-u','network-online.target','--no-pager','-o','short-monotonic'],
 'autostart-journal.txt':['journalctl','--user','-b','-n','300','-u','local-vpn-kde-native-autostart.service','--no-pager','-o','short-monotonic'],
 'routes.txt':['ip','-4','route','show','table','all'],
 'rules.txt':['ip','-4','rule','show'],
 'namespace-routes.txt':['sudo','-n','ip','netns','exec','vpnkit-native','ip','-4','route','show','table','all'],
 'namespace-rules.txt':['sudo','-n','ip','netns','exec','vpnkit-native','ip','-4','rule','show'],
 'resolver.txt':['cat','/etc/resolv.conf'],
 'namespace-resolver.txt':['cat','/etc/netns/vpnkit-native/resolv.conf'],
}.items(): capture(name,command)
def props(args):
 _,out=run(args)
 allowed={'ActiveState','SubState','Result','ExecMainStatus','ActiveEnterTimestampMonotonic','InactiveEnterTimestampMonotonic','ExecMainStartTimestampMonotonic','ExecMainExitTimestampMonotonic'}
 return {k:v for line in out.splitlines() if '=' in line for k,v in [line.split('=',1)] if k in allowed and re.fullmatch(r'[a-zA-Z0-9_-]*',v)}
properties=['-p','ActiveState','-p','SubState','-p','Result','-p','ExecMainStatus','-p','ActiveEnterTimestampMonotonic','-p','InactiveEnterTimestampMonotonic','-p','ExecMainStartTimestampMonotonic','-p','ExecMainExitTimestampMonotonic']
units={}
for unit in ('local-vpn-kde-network.service','local-vpn-kde-gateway.service','NetworkManager.service','network-online.target'):
 units[unit]=props(['systemctl','show',unit,*properties])
units['autostart']=props(['systemctl','--user','show','local-vpn-kde-native-autostart.service',*properties])
_,journal=run(['sudo','-n','journalctl','-b','-u','local-vpn-kde-network.service','-u','local-vpn-kde-gateway.service','-u','NetworkManager.service','-u','network-online.target','--no-pager','-o','cat'])
codes=sorted(set(re.findall(r'native_gateway_start_failure=([a-z0-9-]+):',journal)))
signals={name:bool(re.search(pattern,journal,re.I)) for name,pattern in {'dns':r'\bdns\b|name resolution|no such host','timeout':r'timed? ?out|deadline exceeded','failed':r'\bfailed\b'}.items()}
_,routes=run(['ip','-4','route','show','table','all'])
_,rules=run(['ip','-4','rule','show'])
resolver=pathlib.Path('/etc/resolv.conf').read_text(errors='replace')
_,status=run(['/usr/bin/local-vpn-kde','autostart','--runtime','native','--action','status'])
try: result=json.loads(status)
except ValueError: result={}
safe_status={k:result.get(k) for k in ('last_result','last_reason','last_gateway_reason')}
dns_ok=run(['getent','ahostsv4','www.gstatic.com'])[0]==0
https_ok=run(['curl','--silent','--fail','--max-time','10','https://www.gstatic.com/generate_204','-o','/dev/null'])[0]==0
upstream_script="""
import json,socket
try:
 c=json.load(open('/etc/local-vpn-kde/sing-box/config.json'))
 node=next(o for o in c['outbounds'] if o.get('tag')=='selected-native-out')
 host,port=node['server'],int(node['server_port'])
 addresses=socket.getaddrinfo(host,port,socket.AF_INET,socket.SOCK_STREAM)
 if node.get('type') not in ('vless','trojan','shadowsocks'):
  print('dns-resolved-transport-unproven')
 else:
  s=socket.socket(socket.AF_INET,socket.SOCK_STREAM);s.settimeout(5)
  s.setsockopt(socket.SOL_SOCKET,socket.SO_BINDTODEVICE,b'eth0\\0')
  s.connect(addresses[0][4]);s.close();print('direct-tcp-connected')
except Exception: print('direct-probe-failed-or-unsupported')
"""
_,upstream=run(['sudo','-n','python3','-c',upstream_script])
upstream=upstream.strip()
if upstream not in ('dns-resolved-transport-unproven','direct-tcp-connected','direct-probe-failed-or-unsupported'): upstream='unavailable'
print(json.dumps({'private_guest_evidence':str(evidence),'units':units,'gateway_codes':codes,'journal_signals':signals,'route_count':len(routes.splitlines()),'rule_count':len(rules.splitlines()),'tun_route_present':bool(re.search(r'\bdev tun\d+\b',routes)),'resolver_nameserver_count':sum(line.lstrip().startswith('nameserver ') for line in resolver.splitlines()),'autostart':safe_status,'guest_dns_probe':dns_ok,'guest_https_probe':https_ok,'upstream_direct_probe':upstream}))
'''
    result = remote('python3 -c ' + shlex.quote(script), timeout=90, check=False)
    try:
        print(json.dumps({'failure_evidence': json.loads(result.stdout)}), flush=True)
    except json.JSONDecodeError:
        print(json.dumps({'failure_evidence': 'unavailable'}), flush=True)


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
    cases = [(mode, action, kind) for mode in ('strict', 'smart')
             for action in ('off', 'gateway', 'connect')
             for kind in (('normal',) if action == 'off' or not args.qmp_host else ('normal', 'abrupt'))]
    # Match the observed user failure: a test-owned namespace holder makes
    # normal shutdown refuse deletion; the following normal reboot clears only
    # volatile kernel state, leaving the durable journal for automatic recovery.
    cases += [(mode, 'connect', 'holder-normal') for mode in ('strict', 'smart')]
    if args.case_group == 'smart-connect':
        cases = [('smart', 'connect', 'normal')]
    elif args.case_group != 'all':
        cases = [case for case in cases if (case[2] == 'holder-normal') == (args.case_group == 'holder')]
    for mode, action, kind in cases * args.repeat:
        if snapshot()['mode'] != mode:
            run_cli('native-desktop', '--action', 'toggle-mode', '--yes')
        remote('systemctl --user stop ' + session_unit + ' graphical-session.target')
        run_cli('autostart', '--runtime', 'native', '--action', action)
        if action == 'off':
            run_cli('native-desktop', '--action', 'stop', '--yes')
        elif action == 'gateway':
            run_cli('native-desktop', '--action', 'disconnect', '--yes')
            run_cli('native-desktop', '--action', 'backend-start', '--yes')
        else:
            run_cli('native-desktop', '--action', 'start', '--yes')
        baseline = snapshot()
        assert baseline['gateway'] == (action != 'off') and baseline['connected'] == (action == 'connect'), 'runtime not active before reboot'
        if kind == 'holder-normal':
            holder_script = r"""
import json,os,pathlib,subprocess
unit='vpnkit-lab-namespace-holder.service'
state=subprocess.run(['systemctl','show',unit,'-p','LoadState','--value'],capture_output=True,text=True).stdout.strip()
assert state=='not-found', 'test holder unit already exists'
subprocess.run(['systemd-run','--unit='+unit,'--collect','--property=Type=exec','--','ip','netns','exec','vpnkit-native','sleep','600'],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
pid=int(subprocess.check_output(['systemctl','show',unit,'-p','MainPID','--value']))
start=pathlib.Path('/proc/'+str(pid)+'/stat').read_text().split()[21]
pathlib.Path('/run/vpnkit-lab-namespace-holder.json').write_text(json.dumps({'pid':pid,'start':start,'unit':unit}))
import time
for attempt in range(100):
 if pathlib.Path('/proc/'+str(pid)+'/cmdline').read_bytes()==b'sleep\x00600\x00' and pathlib.Path('/proc/'+str(pid)+'/ns/net').stat().st_ino==pathlib.Path('/run/netns/vpnkit-native').stat().st_ino: break
 time.sleep(0.05)
else: raise AssertionError('test holder did not enter native namespace')
"""
            remote('sudo -n python3 -c ' + shlex.quote(holder_script))
            verify_holder = r"""
import json,pathlib
j=json.loads(pathlib.Path('/run/vpnkit-lab-namespace-holder.json').read_text());p=pathlib.Path('/proc/'+str(j['pid']))
assert (p/'stat').read_text().split()[21]==j['start']
assert (p/'cmdline').read_bytes()==b'sleep\x00600\x00'
assert (p/'ns/net').stat().st_ino==pathlib.Path('/run/netns/vpnkit-native').stat().st_ino
"""
            remote('sudo -n python3 -c ' + shlex.quote(verify_holder))
            result = remote(shlex.join([cli, 'native-desktop', '--action', 'stop', '--yes']), check=False)
            remote('sudo -n python3 -c ' + shlex.quote(verify_holder))
            # systemctl's stop job can succeed while ExecStop fails. Require
            # the actual network unit failure and preserved holder, not the
            # outer desktop command's exit status.
            network_result = remote('systemctl show local-vpn-kde-network.service -p Result --value').stdout.strip()
            assert network_result == 'exit-code', 'held network unit did not refuse cleanup'
            guard_log = remote('sudo -n journalctl -b -u local-vpn-kde-network.service -n 12 --no-pager -o cat').stdout
            assert 'native namespace is held by another process; cleanup was not performed' in guard_log, 'stop failed for a different reason than owned holder protection'
            remote('sudo -n test -e /run/netns/vpnkit-native && sudo -n test -s /var/lib/local-vpn-kde-network/journal.json')
        at_reboot = snapshot()
        booted = reboot(baseline['boot'], abrupt=kind == 'abrupt')
        assert not booted['gateway'] and not booted['connected'], 'runtime started before graphical session'
        remote('systemctl --user start --no-block ' + session_unit)
        deadline = time.monotonic() + 180
        if action != 'off':
            # NM lists an activating VPN before its tunnel routes are ready.
            # The autostart oneshot completes only after its owned connection
            # and full-tunnel routes have passed the product's readiness proof.
            while True:
                state = remote('systemctl --user show ' + unit + ' -p ActiveState --value').stdout.strip()
                if state == 'active':
                    break
                if state == 'failed' or time.monotonic() >= deadline:
                    failure_evidence()
                    raise AssertionError('autostart unit failed or timed out before readiness: ' + action)
                time.sleep(1)
        current = snapshot()
        assert current['gateway'] == (action != 'off') and current['connected'] == (action == 'connect'), 'runtime state does not match completed autostart: ' + action
        if action != 'off':
            assert current['journal_version'] == 5 and current['journal_boot'] == current['boot'], 'active journal is not bound to current kernel boot'
        assert current['config_hash'] == baseline['config_hash'], 'configuration changed over reboot'
        assert current['mode'] == mode, 'policy mode not persistent'
        assert current['route_tun'] == (action == 'connect'), 'desktop route does not match expected VPN state'
        time.sleep(5)
        settled = snapshot()
        assert settled['gateway'] == current['gateway'] and settled['connected'] == current['connected'], 'autostart state did not settle'
        assert settled['route_tun'] == current['route_tun'], 'desktop route changed after autostart completed'
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
        results.append({'mode': mode, 'case': action, 'real_reboot': True, 'reboot_kind': kind, 'runtime_active_before_fault': baseline['gateway'], 'gateway_active_at_reboot': at_reboot['gateway'], 'desktop_vpn_at_reboot': at_reboot['connected'], 'namespace_holder_at_reboot': kind == 'holder-normal', 'mode_persisted': True, 'current_boot_journal': action != 'off', 'holder_guard_verified': kind == 'holder-normal', 'holder_cli_stop_exit': result.returncode if kind == 'holder-normal' else None, 'https': True,
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
    holder_cleanup = r"""
import json,os,pathlib,signal
p=pathlib.Path('/run/vpnkit-lab-namespace-holder.json')
if p.exists():
 d=json.loads(p.read_text()); proc=pathlib.Path('/proc/'+str(d['pid']))
 if proc.exists() and (proc/'stat').read_text().split()[21]==d['start']:
  assert (proc/'cmdline').read_bytes()==b'sleep\x00600\x00', 'test holder identity changed'
  assert d['unit']=='vpnkit-lab-namespace-holder.service'
  import subprocess
  subprocess.run(['systemctl','stop',d['unit']],check=True)
 p.unlink()
"""
    cleanup(lambda: remote('sudo -n python3 -c ' + shlex.quote(holder_cleanup)))

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
