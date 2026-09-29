from pathlib import Path
import time,json,os
p=Path(__file__).parent
with (p/'results/process-samples.jsonl').open('w') as out:
 for _ in range(1200):
  rows=[]
  for d in Path('/proc').glob('[0-9]*'):
   try:
    exe=(d/'exe').readlink()
    if exe.parent!=p or exe.name not in ['load','before.gateway','after.gateway']:continue
    stat=(d/'stat').read_text().split()
    rows.append({'pid':int(d.name),'exe':exe.name,'args':(d/'cmdline').read_bytes().replace(b'\0',b' ').decode(),'cpu_s':(int(stat[13])+int(stat[14]))/os.sysconf('SC_CLK_TCK'),'rss_bytes':int(stat[23])*os.sysconf('SC_PAGE_SIZE')})
   except (OSError,ValueError):pass
  out.write(json.dumps({'time':time.time(),'processes':rows})+'\n');out.flush();time.sleep(1)
