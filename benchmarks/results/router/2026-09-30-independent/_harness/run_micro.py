from pathlib import Path
import subprocess,os,time
p=Path(__file__).parent
env=os.environ|{'GOMAXPROCS':'4'}
pattern='Benchmark(UniformHost|HostSelector|RouteMatrix|Match$|MatchParallel$|Compile$)'
for i in range(8):
 for v in (['before','after'] if i%2==0 else ['after','before']):
  print(i,v,time.strftime('%T'),flush=True)
  with (p/'results'/f'micro-{v}-{i}.txt').open('w') as f:
   subprocess.run(['taskset','-c','4,6,8,10',str(p/f'{v}.test'),'-test.run=^$','-test.bench='+pattern,'-test.benchmem','-test.benchtime=300ms','-test.cpu=4','-test.count=1'],env=env,stdout=f,stderr=subprocess.STDOUT,check=True)
