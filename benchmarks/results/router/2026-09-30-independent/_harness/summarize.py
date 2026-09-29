from pathlib import Path
import re,json,statistics as st,random,csv
p=Path(__file__).parent;r=p/'results'
rng=random.Random(20260929)
def summary(a,b):
 ratios=[y/x for x,y in zip(a,b)];samples=sorted(st.median(rng.choices(ratios,k=len(ratios))) for _ in range(20000))
 return {'before':st.median(a),'after':st.median(b),'change_pct':(st.median(b)/st.median(a)-1)*100,'paired_ratio_ci95':[samples[500],samples[19499]],'before_range':[min(a),max(a)],'after_range':[min(b),max(b)],'rounds':len(a)}
data={}
for f in sorted(r.glob('micro-*-*.txt')):
 _,v,rep=f.stem.split('-')
 for line in f.read_text().splitlines():
  m=re.match(r'(Benchmark\S+)\s+\d+\s+([\d.]+) ns/op\s+(\d+) B/op\s+(\d+) allocs/op',line)
  if m:data.setdefault(m[1],{}).setdefault(v,{})[int(rep)]=list(map(float,m.groups()[1:]))
rows=[]
for name,d in sorted(data.items()):
 if set(d)!= {'before','after'}:continue
 reps=sorted(set(d['before'])&set(d['after']))
 a=[d['before'][i] for i in reps];b=[d['after'][i] for i in reps]
 out={'name':name,**summary([x[0] for x in a],[x[0] for x in b]),'before_B':st.median(x[1] for x in a),'after_B':st.median(x[1] for x in b),'before_alloc':st.median(x[2] for x in a),'after_alloc':st.median(x[2] for x in b)};rows.append(out)
(r/'micro-summary.json').write_text(json.dumps(rows,indent=2))
with (r/'micro-summary.md').open('w') as f:
 f.write('| Case | Before ns/op | After ns/op | Change | paired after/before 95% bootstrap CI | B/op | alloc/op | n |\n|---|---:|---:|---:|---|---|---|---:|\n')
 for x in rows:f.write(f"| {x['name']} | {x['before']:.2f} | {x['after']:.2f} | {x['change_pct']:+.1f}% | {x['paired_ratio_ci95'][0]:.3f}–{x['paired_ratio_ci95'][1]:.3f} | {x['before_B']:g}→{x['after_B']:g} | {x['before_alloc']:g}→{x['after_alloc']:g} | {x['rounds']} |\n")
if (r/'e2e.jsonl').exists():
 raw=[json.loads(l) for l in (r/'e2e.jsonl').read_text().splitlines()]
 out=[]
 for scenario in sorted({x['scenario'] for x in raw}):
  a=sorted([x for x in raw if x['scenario']==scenario and x['version']=='before'],key=lambda x:x['round']);b=sorted([x for x in raw if x['scenario']==scenario and x['version']=='after'],key=lambda x:x['round'])
  if len(a)!=len(b):continue
  out.append({'scenario':scenario,'errors':sum(x['errors'] for x in a+b),'requests':sum(x['requests'] for x in a+b),**{k:summary([x[k] for x in a],[x[k] for x in b]) for k in ['rps','p50_ms','p95_ms','p99_ms','gateway_cpu_us_per_request']}})
 (r/'e2e-summary.json').write_text(json.dumps(out,indent=2))
 print(json.dumps(out,indent=2))
print('micro cases',len(rows),'min rounds',min([x['rounds'] for x in rows],default=0))
