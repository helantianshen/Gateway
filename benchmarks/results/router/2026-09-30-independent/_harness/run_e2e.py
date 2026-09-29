from pathlib import Path
import subprocess,os,time,json,urllib.request,socket,signal
p=Path(__file__).parent
results=p/'results'
subprocess.run(['go','build','-o',str(p/'load'),str(p/'load.go')],check=True)
def port():
 with socket.socket() as s:s.bind(('127.0.0.1',0));return s.getsockname()[1]
def ready(proc,url):
 for _ in range(600):
  if proc.poll() is not None:raise RuntimeError('server exited')
  try:
   with urllib.request.urlopen(url,timeout=.3) as r:
    if r.status==200:return
  except Exception:pass
  time.sleep(.1)
 raise RuntimeError('startup timeout')
def stop(proc):
 proc.send_signal(signal.SIGTERM)
 try:proc.wait(timeout=10)
 except subprocess.TimeoutExpired:proc.kill();proc.wait()
def cpu(pid):
 fields=Path(f'/proc/{pid}/stat').read_text().split();return (int(fields[13])+int(fields[14]))/os.sysconf('SC_CLK_TCK')
up=port();pub=port();admin=port()
upstream=subprocess.Popen(['taskset','-c','12,14',str(p/'load'),'-mode=upstream',f'-addr=127.0.0.1:{up}'],env=os.environ|{'GOMAXPROCS':'2'},stdout=subprocess.DEVNULL,stderr=(results/'upstream.log').open('w'))
try:
 ready(upstream,f'http://127.0.0.1:{up}/')
 for n,wild,c in [(1,False,32),(10,False,32),(1000,False,32),(10000,False,32),(10000,True,32),(10000,False,1)]:
  scenario=f'{n}-'+('any-static' if n==1 else ('wildcard' if wild else 'exact'))+f'-c{c}'
  cfg=p/f'{scenario}.yaml'
  lines=['api_version: v1','upstreams:','  - id: bench','    endpoints:','      - id: fixed',f'        url: http://127.0.0.1:{up}','        weight: 100','routes:']
  for i in range(n):lines += [f'  - id: r{i}',f'    host: "'+('*.' if wild else '')+f'h{i:05d}.example.com"','    method: GET','    path: /users/:id','    upstream: bench']
  if n==1:
   lines=[('    host: ""' if x.startswith('    host:') else '    path: /users/42' if x=='    path: /users/:id' else x) for x in lines]
  lines += ['policies:','  request_timeout: 3s','  rate: 0','  burst: 0'];cfg.write_text('\n'.join(lines)+'\n')
  for rep in range(6):
   for v in (['before','after'] if rep%2==0 else ['after','before']):
    print(scenario,rep,v,time.strftime('%T'),flush=True)
    env=os.environ|{'GOMAXPROCS':'4','GATEWAY_CONFIG_FILE':str(cfg),'GATEWAY_PUBLIC_ADDR':f'127.0.0.1:{pub}','GATEWAY_ADMIN_ADDR':f'127.0.0.1:{admin}'}
    proc=subprocess.Popen(['taskset','-c','4,6,8,10',str(p/f'{v}.gateway')],env=env,stdout=subprocess.DEVNULL,stderr=(results/f'server-{scenario}-{rep}-{v}.log').open('w'))
    try:
     ready(proc,f'http://127.0.0.1:{admin}/readyz')
     cmd=['taskset','-c','0,2',str(p/'load'),f'-addr=127.0.0.1:{pub}',f'-hosts={n}',f'-c={c}',f'-wildcard={str(wild).lower()}']
     clientenv=os.environ|{'GOMAXPROCS':'2'}
     subprocess.run(cmd+['-duration=2s'],env=clientenv,check=True,stdout=subprocess.DEVNULL)
     cpustart=cpu(proc.pid);upstart=cpu(upstream.pid)
     raw=subprocess.check_output(cmd+['-duration=10s'],env=clientenv,text=True)
     row=json.loads(raw);row.update(version=v,round=rep,scenario=scenario,gateway_cpu_s=cpu(proc.pid)-cpustart,upstream_cpu_s=cpu(upstream.pid)-upstart)
     row['gateway_cpu_us_per_request']=row['gateway_cpu_s']*1e6/row['requests']
     with (results/'e2e.jsonl').open('a') as f:f.write(json.dumps(row)+'\n')
    finally:stop(proc)
 # 直接上游测试用于观察负载发生器的吞吐余量
 for rep in range(3):
  raw=subprocess.check_output(['taskset','-c','0,2',str(p/'load'),f'-addr=127.0.0.1:{up}','-hosts=10000','-c=32','-duration=10s'],env=os.environ|{'GOMAXPROCS':'2'},text=True)
  with (results/'upstream-direct.jsonl').open('a') as f:f.write(raw)
finally:stop(upstream)
