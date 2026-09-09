"""Paired before/after GBaseLite validation; uses the common durable SQL harness."""
import argparse,hashlib,json,os,pathlib,subprocess,sys,time
ROOT=pathlib.Path(__file__).resolve().parents[2]
parser=argparse.ArgumentParser(description=__doc__)
parser.add_argument('--output',default='docs/报告/数据/扫描优化-2026-09-08')
parser.add_argument('--before',default='.tmp/four-db-comparison/gbaselite.exe')
parser.add_argument('--after',default='.tmp/mvcc-perf/gbaselite.exe')
args=parser.parse_args()
OUT=ROOT/args.output
BINARIES={'before':ROOT/args.before,'after':ROOT/args.after}
if list(OUT.glob('round-*/*/*.json')):raise SystemExit('Existing data; refusing to overwrite measurements.')
OUT.mkdir(parents=True,exist_ok=True)
order=[]
for repeat in range(1,4):
 for n in [100000,1000000]:
  for variant in (['before','after'] if repeat%2 else ['after','before']):order.append(dict(repeat=repeat,rows=n,variant=variant))
meta={'start':time.strftime('%Y-%m-%dT%H:%M:%S%z'),'order':order,'binary_sha256':{k:hashlib.sha256(p.read_bytes()).hexdigest() for k,p in BINARIES.items()},'harness_sha256':hashlib.sha256((ROOT/'scripts/benchmarks/compare_databases.py').read_bytes()).hexdigest(),'design':'12 isolated serial TCP runs; 100k/1m x before/after x 3 rounds; AB/BA/AB; same durable harness and resource configuration'}
(OUT/'运行记录.json').write_text(json.dumps(meta,ensure_ascii=False,indent=2),encoding='utf-8')
failed=[]
for i,c in enumerate(order,1):
 print(f"CASE {i}/{len(order)} {c}",flush=True)
 env=os.environ.copy();env['BENCH_OUTPUT_DIR']=str(OUT/f"round-{c['repeat']}"/c['variant']);env['BENCH_GBASELITE_EXE']=str(BINARIES[c['variant']])
 r=subprocess.run([sys.executable,str(ROOT/'scripts/benchmarks/compare_databases.py'),'--engine','gbaselite','--rows',str(c['rows'])],env=env)
 if r.returncode:failed.append(c)
meta['end']=time.strftime('%Y-%m-%dT%H:%M:%S%z');meta['failed_cases']=failed
(OUT/'运行记录.json').write_text(json.dumps(meta,ensure_ascii=False,indent=2),encoding='utf-8')
if failed:raise SystemExit('Failed cases retained for analysis.')
