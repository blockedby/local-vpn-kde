#!/usr/bin/env python3
"""Real compiled native TUI + installed Go bridge acceptance, disposable VM only.

Requires exclusive vpn-native-lab, healthy configured native VPN, a populated
active selected real server, Python pyte (QA-only dependency),
and lab Polkit/passwordless sudo. Docker must be absent. No source/Bun UI runner,
backend mock, server selection, or host networking changes. Adds a temporary
one-server profile from the selected private link in memory; restores/removes it.
Raw terminal frames are private; stdout contains only sanitized case outcomes.
--expect-broken reproduces the old validator failure before upgrading.
"""
import codecs
import errno
import fcntl
import hashlib
import json
import os
import pathlib
import pty
import re
import select
import shutil
import signal
import struct
import subprocess
import sys
import termios
import time
import pyte

assert os.environ.get('VPNKIT_NATIVE_VM_LAB') == '1'
assert pathlib.Path('/etc/hostname').read_text().strip() == 'vpn-native-lab'
assert not shutil.which('docker') and not pathlib.Path('/var/run/docker.sock').exists(), 'Docker-free guest required'
os.umask(0o077)
private = pathlib.Path.home()/'.config/vpn-lab/compiled-speed-batch-validation'
private.mkdir(parents=True, exist_ok=True, mode=0o700)
summary = []


def cli(action, value=None):
    result = subprocess.run(['/usr/bin/local-vpn-kde', 'bridge'], input=json.dumps({'id':'qa','action':action, 'value':value})+'\n',
                            capture_output=True, text=True, timeout=45, check=True)
    reply = next(json.loads(line) for line in result.stdout.splitlines() if '"ok"' in line)
    assert reply['ok'], 'installed CLI request failed: '+action
    return reply


def snapshot():
    paths = ['/var/lib/local-vpn-kde/vibe-vpn/current-node.json', '/etc/local-vpn-kde/sing-box/config.json', '/var/lib/local-vpn-kde/sing-box/config.json']
    status = cli('status')['status']
    assert status.get('runtime') == 'native' and status.get('installed') and status.get('networkmanager_active') == 'yes'
    assert status['gateway_state'] in ('healthy','running')
    return {'hashes':[hashlib.sha256(subprocess.check_output(['sudo','-n','cat',p])).hexdigest() for p in paths],
            'gateway_pid':subprocess.check_output(['systemctl','show','local-vpn-kde-gateway','-p','MainPID','--value'],text=True).strip(),
            'profile':cli('profiles/list')['profiles']['active']}


before = snapshot()
original_profiles = cli('profiles/list')['profiles']
created = process = master = screen = tracer = None
expected_count = 0
raw = bytearray()
seen_speed = False


def setup():
    global created, expected_count, master, process, screen, stream, decoder, active_index, expected_display_name
    if os.environ.get('VPNKIT_NATIVE_LAB_SERVER_FILE'):
        import stat
        path=pathlib.Path(os.environ['VPNKIT_NATIVE_LAB_SERVER_FILE']);info=path.lstat()
        assert stat.S_ISREG(info.st_mode) and info.st_uid==os.getuid() and stat.S_IMODE(info.st_mode)==0o600
        source=path.read_text().strip()
    else:
        selected = json.loads(subprocess.check_output(['sudo','-n','cat','/var/lib/local-vpn-kde/vibe-vpn/current-node.json']))
        source=selected['link'];del selected
    source=source.split('#',1)[0]+'#Lab%20compiled%20real%20node'
    added = cli('profiles/add',json.dumps({'name':'Lab compiled speed acceptance','source':source}))['profiles']
    del source
    created = added['active']
    profiles = cli('profiles/list')['profiles']
    active_index = next(i for i,p in enumerate(profiles['items']) if p['id']==profiles['active'])
    assert active_index < 4, 'active profile must be visible on first page'
    expected_count = profiles['items'][active_index]['count']
    assert 0 < expected_count <= 8, 'requires a populated bounded lab profile'
    candidates=[row for row in cli('servers/list')['catalog']['servers'] if row.get('profile_id')==created]
    assert len(candidates)==1, 'temporary source did not produce one real server'
    expected_display_name=candidates[0]['display_name'][:40]
    (private/'expected-batch.json').write_text(json.dumps({'id_hash':hashlib.sha256(candidates[0]['server_id'].encode()).hexdigest(),'server_id':candidates[0]['server_id'],'url':'https://www.gstatic.com/generate_204'}))
    preflight=cli('servers/speed-batch',json.dumps({'ids':[candidates[0]['server_id']],'url':'https://www.gstatic.com/generate_204'}))['catalog']['servers'][0]
    assert preflight['status']=='ready' and preflight['availability']=='ready' and preflight.get('downloaded_bytes',0)>0, 'single-node real speed preflight failed'
    master, slave = pty.openpty()
    try:
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 40, 140, 0, 0))
        process = subprocess.Popen(['/usr/bin/local-vpn-kde','ui'], stdin=slave, stdout=slave, stderr=slave,
                                   env={**os.environ,'TERM':'xterm-256color','COLORTERM':'truecolor'}, start_new_session=True)
    finally:
        os.close(slave)
    screen = pyte.Screen(140,40)
    stream = pyte.Stream(screen)
    decoder = codecs.getincrementaldecoder('utf-8')('replace')


def restore_profiles():
    global created
    if created:
        cli('profiles/open',before['profile'])
        cli('profiles/remove',created)
        created=None
    assert cli('profiles/list')['profiles']==original_profiles, 'temporary profile cleanup mismatch'


def frame():
    return '\n'.join(screen.display) if screen is not None else ''


def pump(seconds=.1):
    global seen_speed
    until = time.monotonic()+seconds
    while time.monotonic()<until:
        if not select.select([master],[],[],min(.05,max(.001,until-time.monotonic())))[0]:
            continue
        try:data=os.read(master,65536)
        except OSError as error:
            if error.errno==errno.EIO:break
            raise
        if not data:break
        raw.extend(data)
        if b'\x1b[6n' in data:os.write(master,b'\x1b[1;1R')
        stream.feed(decoder.decode(data))
        seen_speed |= any('замер' in line and 'Скорость + сайт' not in line for line in screen.display)


def wait(predicate, label, timeout=150, allow_unavailable=False):
    deadline=time.monotonic()+timeout
    while True:
        pump()
        text=frame()
        if not allow_unavailable:
            assert 'Служба управления недоступна' not in text, 'compiled TUI lost backend: '+label
        assert process.poll() is None, 'compiled TUI exited: '+label
        if predicate(text):return text
        assert time.monotonic()<deadline, 'compiled TUI timeout: '+label


def key(value):
    os.write(master,value.encode())
    pump(.2)


def site(url):
    key('e')
    wait(lambda text:'HTTPS' in text or 'Адрес сайта' in text,'site editor')
    # End + backspaces is independent of editor Ctrl+A/selection bindings.
    key('\x1b[F'+'\x7f'*200+url+'\r')
    wait(lambda text:'Ping всех' in text and re.search(r'Сайт:\s*'+re.escape(url)+r'\s*(?:\n|$)',text),'saved exact site')


def completed(errors=None):
    pattern=r'Завершено:\s*(\d+)/(\d+)\s*·\s*ошибок\s*(\d+)'
    text=wait(lambda text:re.search(pattern,text),'batch complete')
    result=re.search(pattern,text)
    assert int(result[1])==expected_count and int(result[2])==expected_count, 'compiled batch incomplete'
    assert errors is None or int(result[3])==errors, 'compiled batch completed with unexpected measurement failures'
    return text


def record(result):
    summary.append(result)
    (private/'progress.json').write_text(json.dumps(summary,indent=2))


def second_action():
    key('d');key('u')
    wait(lambda text:'Состояние компонентов обновлено.' in text,'second diagnostic action')
    assert 'Docker' not in frame(), 'native diagnostics displays Docker wording'
    key('v')
    wait(lambda text:'Ping всех' in text,'return to server table')


try:
    setup()
    wait(lambda text:'VPNKIT' in text and 'NATIVE' in text,'native UI startup',30)
    if '--trace-requests' in sys.argv:
        trace_path=private/'ui-private-writes.trace';trace_path.write_text('')
        threads=[part for tid in pathlib.Path('/proc/'+str(process.pid)+'/task').iterdir() for part in ('-p',tid.name)]
        tracer=subprocess.Popen(['sudo','-n','timeout','30','strace',*threads,'-e','trace=write,writev,sendto,sendmsg','-s','65535','-o',str(trace_path)],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
        memory={key:int(value.split()[0]) for line in pathlib.Path('/proc/meminfo').read_text().splitlines() for key,value in [line.split(':',1)] if key in ('MemAvailable','SwapFree')}
        rss=next(line.split()[1] for line in pathlib.Path('/proc/'+str(process.pid)+'/status').read_text().splitlines() if line.startswith('VmRSS:'))
        kernel=subprocess.check_output(['sudo','-n','journalctl','-k','-b','--no-pager','-o','cat'],text=True)
        print(json.dumps({'ui_alive_mem_available_kib':memory['MemAvailable'],'swap_free_kib':memory['SwapFree'],'ui_rss_kib':int(rss),'kernel_oom_kill_lines':sum('Killed process' in line or 'Out of memory:' in line for line in kernel.splitlines())}),flush=True)
        pump(.5)
    assert 'Docker' not in frame(), 'native UI displays Docker wording'
    key('v')
    wait(lambda text:'Lab compiled speed acceptance' in text,'loaded profile list',30)
    key(str(active_index+1))
    wait(lambda text:'Ping всех' in text and 'Lab compiled speed acceptance' in text and '1 серверов' in text and 'Lab compiled real node' in text,'loaded fixture server table',30)
    site('https://www.gstatic.com/generate_204')
    seen_speed=False
    if '--trace-requests' in sys.argv:
        expected=json.loads((private/'expected-batch.json').read_text())
        parity=cli('servers/speed-batch',json.dumps({'ids':[expected['server_id']],'url':expected['url']}))['catalog']['servers'][0]
        print(json.dumps({'raw_while_compiled_ui_alive':{key:parity.get(key) for key in ('status','ping_status','availability','downloaded_bytes')}}),flush=True)
    key('t')
    if '--expect-broken' in sys.argv:
        wait(lambda text:'Служба управления недоступна' in text,'old speed validator failure',40,True)
        key('d');key('u')
        wait(lambda text:'Служба управления недоступна' in text,'old bridge remains unusable',30,True)
        record({'baseline_compiled_speed_stage_backend_loss':'reproduced','docker_installed':False})
    else:
        completed(errors=0)
        assert seen_speed, 'compiled UI never rendered the real speed stage'
        assert 'Docker' not in frame(), 'native batch displays Docker wording'
        second_action()
        record({'compiled_speed_site_complete':'passed','real_speed_stage_rendered':True,'second_action':'passed','docker_installed':False})
        # Failed website evidence must not destroy the bridge/throughput pass.
        site('https://127.0.0.1:9/unavailable')
        seen_speed=False
        key('t');completed(errors=expected_count)
        assert seen_speed, 'failed site suppressed the real throughput stage'
        failed_rows=[row for row in cli('servers/list')['catalog']['servers'] if row.get('profile_id')==created]
        assert len(failed_rows)==expected_count
        assert all(row['availability']=='failed' and row['status']=='ready' and row.get('downloaded_bytes',0)>0 and row.get('download_mbps',0)>0 for row in failed_rows), 'failed site lost independent throughput result'
        second_action()
        record({'site_failure_without_backend_loss':'passed','second_action':'passed'})
        # Cancel only once a real throughput stage is visible, then reuse bridge.
        site('https://www.gstatic.com/generate_204')
        seen_speed=False
        key('t')
        wait(lambda text:seen_speed,'active speed cancellation')
        key('k')
        wait(lambda text:'Остановлено:' in text,'cancelled speed batch')
        second_action()
        record({'active_speed_cancel_without_backend_loss':'passed','second_action':'passed'})
    if '--expect-broken' not in sys.argv:
        def descendants(pid):
            path=pathlib.Path('/proc/'+str(pid)+'/task/'+str(pid)+'/children')
            try:children=[int(value) for value in path.read_text().split()]
            except FileNotFoundError:return []
            return children+[child for parent in children for child in descendants(parent)]
        bridges=[]
        for pid in descendants(process.pid):
            try:command=pathlib.Path('/proc/'+str(pid)+'/cmdline').read_bytes().split(b'\0')
            except FileNotFoundError:continue
            if command[:2]==[b'/usr/libexec/local-vpn-kde/local-vpn-kde.bin',b'bridge']:
                bridges.append(pid)
        assert len(bridges)==1, 'cannot identify UI-owned bridge for controlled failure'
        os.kill(bridges[0],signal.SIGTERM)
        key('h');key('u')
        wait(lambda text:'Служба управления недоступна' in text,'controlled status transport failure',30,True)
        assert 'NATIVE' in frame() and 'Docker' not in frame(), 'transport failure changed native identity to Docker'
        assert 'Статус недоступен' in frame(), 'failed transport retained stale healthy status'
        key('v');pump(.3)
        assert 'NATIVE' in frame() and 'Docker' not in frame(), 'unavailable native server controls display Docker'
        assert 'Статус недоступен' in frame(), 'server screen retained stale healthy status'
        record({'controlled_transport_failure_native_identity':'preserved','false_docker_wording':False})
    key('q')
    process.wait(timeout=45)
    assert process.returncode==0, 'compiled UI did not exit cleanly'
    restore_profiles()
    assert snapshot()==before, 'TUI acceptance changed runtime or active source'
    (private/'summary.json').write_text(json.dumps(summary,indent=2))
    print(json.dumps({'installed_compiled_tui_speed_batch':'baseline-reproduced' if '--expect-broken' in sys.argv else 'passed','cases':summary,'runtime':'unchanged'}))
except BaseException:
    if created:
        try:
            rows=[{key:row.get(key) for key in ('status','ping_status','availability','download_mbps','download_seconds','downloaded_bytes')} for row in cli('servers/list')['catalog']['servers'] if row.get('profile_id')==created]
            (private/'failure-measurements.json').write_text(json.dumps(rows,indent=2))
        except Exception:pass
    (private/'failure-screen.txt').write_text(frame())
    (private/'failure-terminal.bin').write_bytes(raw)
    raise
finally:
    if process is not None and process.poll() is None:
        os.write(master,b'\x03')
        try:process.wait(timeout=35)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid,signal.SIGKILL);process.wait()
    if master is not None:os.close(master)
    if tracer is not None:tracer.wait(timeout=35)
    restore_profiles()
