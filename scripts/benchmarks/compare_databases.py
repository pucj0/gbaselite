"""Durable single-client comparison. Creates/removes only unique .tmp databases."""
from __future__ import annotations
import argparse,json,os,pathlib,random,shutil,socket,statistics,subprocess,sys,threading,time,traceback,uuid
ROOT=pathlib.Path(__file__).resolve().parents[2]
BASE=ROOT/'.tmp'/'db-comparison'
sys.path.insert(0,str(BASE/'packages'))
import psutil
OUT=pathlib.Path(os.environ.get('BENCH_OUTPUT_DIR',str(ROOT/'docs'/'报告'/'数据'/'四数据库对比-2026-09-08')))
GBASELITE=pathlib.Path(os.environ.get('BENCH_GBASELITE_EXE',str(ROOT/'.tmp/four-db-comparison/gbaselite.exe')))
MYSQL=pathlib.Path(os.environ.get('BENCH_MYSQL_BIN',r'D:\MySQL\MySQL Server 8.2\bin'))
PG=pathlib.Path(os.environ.get('BENCH_PG_BIN',str(BASE/'pgsql'/'bin')))
FLAGS=subprocess.CREATE_NO_WINDOW if os.name=='nt' else 0
def command(args,**kw):
    return subprocess.run([str(x) for x in args],check=True,creationflags=FLAGS,**kw)
def port():
    with socket.socket() as s:
        s.bind(('127.0.0.1',0)); return s.getsockname()[1]
def size(path):
    return sum(p.stat().st_size for p in path.rglob('*') if p.is_file())
class Monitor:
    def __init__(self,pid):
        self.root=psutil.Process(pid); self.done=threading.Event(); self.lock=threading.RLock()
        self.rss=self.private=self.samples=0; self.first={}; self.last={}
        self.tick(); self.idle_rss=self.rss; self.idle_private=self.private
        self.thread=threading.Thread(target=self.loop,daemon=True); self.thread.start()
    def tick(self):
        with self.lock:
            rss=private=0
            try: processes=[self.root]+self.root.children(recursive=True)
            except psutil.Error: return
            for p in processes:
                try:
                    m,c=p.memory_info(),p.cpu_times()
                    rss+=m.rss; private+=getattr(m,'private',m.vms)
                    v=c.user+c.system; self.first.setdefault(p.pid,v); self.last[p.pid]=v
                except psutil.Error: pass
            self.rss=max(self.rss,rss); self.private=max(self.private,private); self.samples+=1
    def cpu_seconds(self):
        self.tick()
        with self.lock:
            return sum(v-self.first[k] for k,v in self.last.items())
    def loop(self):
        while not self.done.wait(.1): self.tick()
    def stop(self):
        self.done.set(); self.thread.join(); self.tick()
        return dict(rss_start_bytes=self.idle_rss,private_start_bytes=self.idle_private,rss_peak_bytes=self.rss,private_peak_bytes=self.private,cpu_seconds=sum(v-self.first[k] for k,v in self.last.items()),samples=self.samples,sample_interval_ms=100)
def worker(engine,n):
    global pymysql,psycopg,sqlite3
    if engine in ('mysql','gbaselite'): import pymysql
    elif engine=='postgresql': import psycopg
    else: import sqlite3
    work=BASE/f'run-{engine}-{n}-{uuid.uuid4().hex[:8]}'; work.mkdir(parents=True)
    data=work/'data'; data.mkdir()
    pnum=port(); proc=conn=mon=None
    log=(work/'server.log').open('wb')
    r=dict(engine=engine,rows=n,timestamp=time.strftime('%Y-%m-%dT%H:%M:%S%z'),status='started',batch_rows=250,point_queries=200,query_repeats=3,payload_bytes=128,work_path=str(work),port=pnum,stages={})
    def save():
        OUT.mkdir(parents=True,exist_ok=True)
        (OUT/f'{engine}-{n}.json').write_text(json.dumps(r,ensure_ascii=False,indent=2),encoding='utf-8')
    last_affected=None
    def sql(q):
        nonlocal last_affected
        with conn.cursor() as c:
            c.execute(q); last_affected=c.rowcount
            return c.fetchall() if c.description else None
    def connect():
        if engine=='postgresql':
            return psycopg.connect(host='127.0.0.1',port=pnum,user='bench',dbname='postgres',autocommit=True,connect_timeout=2)
        return pymysql.connect(host='127.0.0.1',port=pnum,user='root',password=('bench-local-only' if engine=='gbaselite' else ''),autocommit=True,connect_timeout=2,read_timeout=300,write_timeout=300,charset='utf8mb4')
    try:
        if engine=='mysql':
            command([MYSQL/'mysqld.exe','--no-defaults','--initialize-insecure',f'--basedir={MYSQL.parent}',f'--datadir={data}'],stdout=log,stderr=log,timeout=180)
            args=[MYSQL/'mysqld.exe','--no-defaults',f'--basedir={MYSQL.parent}',f'--datadir={data}',f'--port={pnum}','--bind-address=127.0.0.1','--mysqlx=OFF','--skip-log-bin','--innodb-buffer-pool-size=67108864','--innodb-flush-log-at-trx-commit=1','--innodb-redo-log-capacity=104857600','--max-connections=16','--performance-schema=OFF']
        elif engine=='postgresql':
            command([PG/'initdb.exe','-D',data,'-U','bench','--encoding=UTF8','--locale=C','--auth-local=trust','--auth-host=trust'],stdout=log,stderr=log,timeout=180)
            args=[PG/'postgres.exe','-D',data,'-h','127.0.0.1','-p',str(pnum),'-c','shared_buffers=64MB','-c','work_mem=4MB','-c','max_connections=16','-c','fsync=on','-c','synchronous_commit=on','-c','full_page_writes=on','-c','max_parallel_workers_per_gather=0']
        elif engine=='gbaselite':
            cfg=work/'config.yaml'
            config=f"""server:
  host: 127.0.0.1
  port: {pnum}
  max_connections: 16
storage:
  mode: mvcc
  path: '{data.as_posix()}'
resources:
  memory_limit_mb: 64
  working_set_limit_mb: 64
  max_procs: 2
  sort_memory_mb: 4
  query_result_memory_mb: 16
  query_temp_mb: 256
  transaction_write_mb: 0
  query_temp_path: '{(work/'query-temp').as_posix()}'
  query_timeout_ms: 120000
auth:
  username: root
  password: bench-local-only
log:
  path: '{(work/'logs').as_posix()}'
"""
            cfg.write_text(config,encoding='utf-8'); r['configuration']=config
            args=[GBASELITE,'server','--config',cfg]
        else: args=[]
        r['server_command']=[str(x) for x in args]
        if engine!='sqlite':
            proc=subprocess.Popen([str(x) for x in args],stdout=log,stderr=log,cwd=work,creationflags=FLAGS)
            end=time.monotonic()+90
            while True:
                try: conn=connect(); break
                except Exception:
                    if proc.poll() is not None or time.monotonic()>end: raise
                    time.sleep(.2)
            if engine in ('mysql','gbaselite'):
                r['version']=str(sql('SELECT VERSION()')[0][0]); sql('CREATE DATABASE bench'); sql('USE bench')
            else:
                r['version']=sql('SELECT version()')[0][0]; sql("SET statement_timeout='120s'")
        else:
            conn=sqlite3.connect(data/'bench.sqlite',isolation_level=None,timeout=120)
            def sql(q):
                nonlocal last_affected
                c=conn.execute(q); last_affected=c.rowcount
                return c.fetchall() if c.description else None
            r['version']=sqlite3.sqlite_version
            for q in ['PRAGMA journal_mode=WAL','PRAGMA synchronous=FULL','PRAGMA cache_size=-65536','PRAGMA mmap_size=0']: sql(q)
            r['sqlite_settings']={x:sql('PRAGMA '+x)[0][0] for x in ['journal_mode','synchronous','cache_size','mmap_size']}
        r['empty_data_bytes']=size(data)
        mon=Monitor(proc.pid if proc else os.getpid())
        start=time.perf_counter(); cpu=time.process_time()
        sql('CREATE TABLE items(id '+('INTEGER' if engine=='sqlite' else 'BIGINT')+' PRIMARY KEY, v INT NOT NULL, payload VARCHAR(128) NOT NULL)')
        payload='x'*128; cpu_before=mon.cpu_seconds(); t=time.perf_counter()
        for base in range(0,n,250):
            values=','.join(f"({i},1,'{payload}')" for i in range(base+1,min(base+250,n)+1))
            sql('INSERT INTO items(id,v,payload) VALUES '+values)
            if base and base%100000==0: print(f'{engine} {n}: inserted {base}',flush=True)
        r['stages']['insert']=dict(seconds=time.perf_counter()-t,status='ok'); r['stages']['insert']['cpu_seconds']=mon.cpu_seconds()-cpu_before; r['data_after_insert_bytes']=size(data)
        check=sql('SELECT COUNT(*), SUM(v) FROM items')[0]
        assert int(check[0])==n and int(check[1])==n,str(check)
        rng=random.Random(20260908); lat=[]
        for j in range(220):
            k=rng.randint(1,n); t=time.perf_counter(); rows=sql(f'SELECT v FROM items WHERE id={k}'); dt=time.perf_counter()-t
            assert len(rows)==1 and int(rows[0][0])==1
            if j>=20: lat.append(dt*1000)
        lat.sort()
        r['stages']['point']=dict(status='ok',p50_ms=statistics.median(lat),p95_ms=lat[189],mean_ms=statistics.mean(lat),samples_ms=lat)
        count=min(100,n); lower=max(1,min(n//2,n-count+1))
        for name,q in [('aggregate','SELECT COUNT(*), SUM(v) FROM items'),('range',f'SELECT id,v FROM items WHERE id >= {lower} AND id < {lower+count} ORDER BY id')]:
            samples=[]
            try:
                for _ in range(3):
                    t=time.perf_counter(); rows=sql(q); samples.append(time.perf_counter()-t)
                    if name=='aggregate': assert len(rows)==1 and int(rows[0][0])==n and int(rows[0][1])==n,(name,rows)
                    else: assert [(int(a),int(b)) for a,b in rows]==[(i,1) for i in range(lower,lower+count)],name
                r['stages'][name]=dict(status='ok',seconds=statistics.median(samples),samples_seconds=samples,sql=q,contents_verified=True)
            except Exception as exc: r['stages'][name]=dict(status='error',error=str(exc),samples_seconds=samples,sql=q)
            save()
        cpu_before=mon.cpu_seconds()
        t=time.perf_counter()
        try:
            sql('UPDATE items SET v=v+1'); elapsed=time.perf_counter()-t
            update_cpu=mon.cpu_seconds()-cpu_before; affected=last_affected
            assert affected==n,('affected',affected,n)
            check=sql('SELECT COUNT(*), SUM(v) FROM items')[0]
            assert int(check[0])==n and int(check[1])==2*n,str(check)
            r['stages']['update']=dict(status='ok',seconds=elapsed,cpu_seconds=update_cpu,affected_rows=affected,verified=True)
        except Exception as exc:
            r['stages']['update']=dict(status='error',seconds=time.perf_counter()-t,error=str(exc))
            try:
                check=sql('SELECT COUNT(*), SUM(v) FROM items')[0]
                r['stages']['update']['after_error_count_sum']=[int(x) for x in check]
                r['stages']['update']['atomic_rollback_verified']=int(check[0])==n and int(check[1])==n
            except Exception as ve: r['stages']['update']['verification_error']=str(ve)
        r['workload_seconds']=time.perf_counter()-start; r['client_cpu_seconds']=time.process_time()-cpu
        r['resources']=mon.stop(); mon=None
        r['data_after_update_bytes']=size(data); r['status']='complete'; r['all_stages_ok']=all(v['status']=='ok' for v in r['stages'].values())
    except Exception as exc:
        r['status']='error'; r['error']=str(exc); r['traceback']=traceback.format_exc()
    finally:
        if mon: r['resources']=mon.stop()
        if conn:
            if engine=='mysql':
                try: sql('SHUTDOWN')
                except Exception: pass
            try: conn.close()
            except Exception: pass
        if proc:
            if engine=='postgresql':
                try: command([PG/'pg_ctl.exe','-D',data,'stop','-m','fast','-w'],stdout=log,stderr=log,timeout=45)
                except Exception: pass
            if proc.poll() is None:
                try: proc.wait(timeout=3)
                except subprocess.TimeoutExpired: proc.terminate(); proc.wait(timeout=30)
        log.close()
        r['server_log_tail']=(work/'server.log').read_text(encoding='utf-8',errors='replace')[-4000:]
        assert work.resolve().parent==BASE.resolve() and work.name.startswith('run-')
        try: shutil.rmtree(work); r['temporary_database_removed']=True
        except Exception as exc: r['cleanup_error']=str(exc)
        save()
    print(json.dumps({k:r[k] for k in ('engine','rows','status')}),flush=True)
    if r['status']!='complete': print(r.get('traceback'),flush=True)
    return 0 if r['status']=='complete' and r.get('all_stages_ok') and r.get('temporary_database_removed') else 1
if __name__=='__main__':
    ap=argparse.ArgumentParser(); ap.add_argument('--engine',choices=['gbaselite','mysql','sqlite','postgresql'],required=True); ap.add_argument('--rows',type=int)
    a=ap.parse_args()
    if a.rows:
        if a.rows not in [10,1000,10000,100000,1000000]: ap.error('unsupported scale')
        raise SystemExit(worker(a.engine,a.rows))
    for n in [10,1000,10000,100000,1000000]:
        completed=subprocess.run([sys.executable,__file__,'--engine',a.engine,'--rows',str(n)],creationflags=FLAGS)
        if completed.returncode: raise SystemExit(completed.returncode)

