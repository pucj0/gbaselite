"""Validate report evidence, local references, source snapshots and image outputs."""
import argparse,csv,hashlib,json,pathlib,re
p=argparse.ArgumentParser();p.add_argument('--output',default='docs/报告');args=p.parse_args()
root=pathlib.Path(__file__).resolve().parents[2];out=(root/args.output).resolve();data=out/'数据/四数据库对比-2026-09-08'
meta=json.loads((data/'运行记录.json').read_text(encoding='utf-8-sig'))
assert meta.get('end') and not meta['failed_cases']
engines=['gbaselite','mysql','postgresql','sqlite'];scales=[10,1000,10000,100000,1000000]
raw={}
for i in range(1,4):
 for e in engines:
  for n in scales:
   r=json.loads((data/f'round-{i}/{e}-{n}.json').read_text(encoding='utf-8'))
   assert r['engine']==e and r['rows']==n
   assert r['status']=='complete' and r['all_stages_ok'] and r['temporary_database_removed']
   assert r['stages']['update']['affected_rows']==n and r['stages']['update']['verified']
   assert len(r['stages']['point']['samples_ms'])==200
   for k in ['insert','update','range','aggregate']:assert r['stages'][k]['seconds']>0
   raw[i,e,n]=r
for e in engines:assert len({r['version'] for (i,en,n),r in raw.items() if en==e})==1
rows=list(csv.DictReader((data/'三轮指标汇总.csv').open(encoding='utf-8-sig')))
assert len(rows)==4*5*18
for r in rows:
 vals=[float(r[f'round_{i}']) for i in range(1,4)]
 assert float(r['median'])==sorted(vals)[1] and float(r['min'])==min(vals) and float(r['max'])==max(vals)
 if r['metric'] in ['insert_s','update_s']:
  stage=r['metric'].split('_')[0]
  for i in range(1,4):assert vals[i-1]==raw[i,r['engine'],int(r['rows'])]['stages'][stage]['seconds']
text=(out/'四数据库对比报告.md').read_text(encoding='utf-8');assert '{{' not in text
for link in re.findall(r'\]\(([^)]+)\)',text):
 if '://' in link or link.startswith('#'):continue
 assert (out/link.split('#')[0]).exists(),link
for line in (out/'SHA256SUMS.txt').read_text(encoding='utf-8').splitlines():
 digest,rel=line.split('  ',1);assert hashlib.sha256((out/rel).read_bytes()).hexdigest()==digest,rel
for r in json.loads((data/'源码/源码哈希.json').read_text(encoding='utf-8')):
 assert hashlib.sha256((data/'源码'/r['path']).read_bytes()).hexdigest()==r['sha256'],r['path']
assert hashlib.sha256((data/'源码/scripts/benchmarks/compare_databases.py').read_bytes()).hexdigest()==meta['harness_sha256']
for name in ['百万行性能摘要','SQL性能对比','资源占用对比','功能与适用性对比']:
 b=(out/'图片'/f'{name}.png').read_bytes();assert b[:8]==b'\x89PNG\r\n\x1a\n'
 assert int.from_bytes(b[16:20],'big')>=2000 and int.from_bytes(b[20:24],'big')>=1000
 assert (out/'图片'/f'{name}.svg').stat().st_size>1000
print('Verified: 60 runs, 360 metric rows, source hashes, manifest, references, 4 PNG/SVG figures.')
