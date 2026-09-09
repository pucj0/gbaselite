"""Fresh four-engine, five-scale, three-round comparison."""
import argparse,hashlib,json,os,pathlib,random,subprocess,sys,time
ROOT=pathlib.Path(__file__).resolve().parents[2]
parser=argparse.ArgumentParser(description=__doc__)
parser.add_argument('--output',default='docs/报告/数据/四数据库对比-2026-09-08')
parser.add_argument('--binary',default='.tmp/four-db-comparison/gbaselite.exe')
args=parser.parse_args()
OUT=ROOT/args.output
BINARY=ROOT/args.binary
if list(OUT.glob('round-*/*.json')):
 raise SystemExit('Existing measurement data found. Move it aside before a fresh run; refusing to overwrite evidence.')
OUT.mkdir(parents=True,exist_ok=True)
rng=random.Random(20260910);order=[]
for repeat in range(1,4):
 cases=[(e,n) for e in ['gbaselite','mysql','postgresql','sqlite'] for n in [10,1000,10000,100000,1000000]]
 rng.shuffle(cases);order.extend([dict(repeat=repeat,engine=e,rows=n) for e,n in cases])
meta=dict(start=time.strftime('%Y-%m-%dT%H:%M:%S%z'),order=order,rounds=3,
 binary_sha256=hashlib.sha256(BINARY.read_bytes()).hexdigest(),
 harness_sha256=hashlib.sha256((ROOT/'scripts/benchmarks/compare_databases.py').read_bytes()).hexdigest(),
 design='60 fresh isolated runs; randomized serial order; durable single client; TCP servers vs embedded SQLite; 250-row inserts; 128-byte payload; one atomic whole-table UPDATE')
(OUT/'运行记录.json').write_text(json.dumps(meta,ensure_ascii=False,indent=2),encoding='utf-8')
failures=[]
for i,c in enumerate(order,1):
 env=os.environ.copy();env['BENCH_OUTPUT_DIR']=str(OUT/f"round-{c['repeat']}");env['BENCH_GBASELITE_EXE']=str(BINARY)
 print(f"CASE {i}/60 ROUND {c['repeat']} {c['engine']} rows={c['rows']}",flush=True)
 r=subprocess.run([sys.executable,str(ROOT/'scripts/benchmarks/compare_databases.py'),'--engine',c['engine'],'--rows',str(c['rows'])],env=env)
 if r.returncode:failures.append(c)
meta['end']=time.strftime('%Y-%m-%dT%H:%M:%S%z');meta['failed_cases']=failures
(OUT/'运行记录.json').write_text(json.dumps(meta,ensure_ascii=False,indent=2),encoding='utf-8')
if failures:raise SystemExit('One or more cases failed; preserve and report their failures.')
