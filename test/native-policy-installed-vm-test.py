#!/usr/bin/env python3
"""Exercise real policy transitions and rule download failure in the lab VM only."""
import json, os, pathlib, select, subprocess
assert os.environ.get('VPNKIT_NATIVE_VM_LAB')=='1'
assert pathlib.Path('/etc/hostname').read_text().strip()=='vpn-native-lab'
os.umask(0o077)
proc=subprocess.Popen(['/usr/bin/local-vpn-kde','bridge'],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,text=True,bufsize=1)
sequence=0

def run(action,ok=True):
 global sequence
 sequence+=1
 proc.stdin.write(json.dumps({'id':str(sequence),'action':action})+'\n');proc.stdin.flush()
 while True:
  assert select.select([proc.stdout],[],[],150)[0], 'policy operation timed out'
  line=proc.stdout.readline();assert line,'bridge exited'
  reply=json.loads(line)
  if 'ok' not in reply:continue
  assert reply['ok']==ok,'unexpected policy result: '+action
  return reply

def config():
 return json.loads(subprocess.check_output(['sudo','-n','cat','/etc/local-vpn-kde/sing-box/config.json']))
def mode():
 return 'smart' if any(r.get('outbound')=='direct-out' for r in config()['route']['rules']) else 'strict'
def healthy():
 assert subprocess.run(['systemctl','is-active','--quiet','local-vpn-kde-gateway']).returncode==0
 subprocess.run(['curl','--silent','--fail','--max-time','15','https://www.gstatic.com/generate_204'],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
def write_template(data):
 subprocess.run(['sudo','-n','tee',template],input=data,check=True,stdout=subprocess.DEVNULL)

template='/usr/share/local-vpn-kde/config/sing-box/config.tun.json.template'
original=subprocess.check_output(['sudo','-n','cat',template])
try:
 healthy()
 if mode()=='smart':run('toggle-mode')
 assert mode()=='strict';healthy()
 run('toggle-mode');assert mode()=='smart';healthy()
 servers=config()['dns']['servers']
 assert any(s.get('tag')=='direct-dns' and s.get('type')=='udp' and s.get('server')=='8.8.8.8' for s in servers)
 run('toggle-mode');assert mode()=='strict';healthy()
 print('native_strict_smart_strict_with_https=passed',flush=True)
 # A closed loopback port deterministically fails actual rule-set download,
 # while leaving the working selected VPN server and DNS untouched.
 rules=[{'type':'remote','tag':tag,'format':'binary','url':'https://127.0.0.1:9/unavailable.srs'} for tag in ['geoip-ru','geosite-category-ru']]
 assert b'{{RU_RULE_SETS_JSON}}' in original
 broken=original.replace(b'{{RU_RULE_SETS_JSON}}',json.dumps(rules)[1:-1].encode())
 write_template(broken)
 run('toggle-mode',ok=False)
 assert mode()=='strict','failed candidate remained published'
 healthy()
 print('native_rule_download_failure_restores_strict_and_https=passed',flush=True)
finally:
 write_template(original)
 proc.stdin.close()
 proc.wait(timeout=20)
