"""Run pinned Java compatibility tests against an isolated local MVCC server."""
import json,os,pathlib,shutil,socket,subprocess,sys,time,uuid
ROOT=pathlib.Path(__file__).resolve().parents[2]
BASE=ROOT/'.tmp/java-compat'
sys.path.insert(0,str(ROOT/'.tmp/db-comparison/packages'))
import pymysql
FLAGS=subprocess.CREATE_NO_WINDOW if os.name=='nt' else 0
work=(BASE/('run-'+uuid.uuid4().hex[:8])).resolve();work.mkdir(parents=True)
assert work.parent==BASE.resolve()
with socket.socket() as s:s.bind(('127.0.0.1',0));port=s.getsockname()[1]
config=f"""server:
  host: 127.0.0.1
  port: {port}
  max_connections: 16
storage:
  mode: mvcc
  path: '{(work/'data').as_posix()}'
  local_wal: true
resources:
  memory_limit_mb: 64
  max_procs: 2
  sort_memory_mb: 4
  query_result_memory_mb: 16
  query_timeout_ms: 120000
auth:
  username: root
  password: compat-local-only
log:
  path: '{(work/'logs').as_posix()}'
"""
(work/'config.yaml').write_text(config,encoding='utf-8')
log=(BASE/'server-java.log').open('wb');proc=None;code=1
try:
 proc=subprocess.Popen([str(BASE/'gbaselite.exe'),'server','--config',str(work/'config.yaml')],cwd=ROOT,stdout=log,stderr=log,creationflags=FLAGS)
 for _ in range(100):
  if proc.poll() is not None:raise RuntimeError('Temporary server exited; see server-java.log')
  try:
   conn=pymysql.connect(host='127.0.0.1',port=port,user='root',password='compat-local-only',autocommit=True,connect_timeout=1)
   with conn.cursor() as cursor:cursor.execute('CREATE DATABASE test')
   conn.close();break
  except pymysql.Error:time.sleep(.1)
 else:raise RuntimeError('Temporary server startup timed out')
 env=os.environ.copy()
 if not env.get('JAVA_HOME'):
  java=shutil.which('java')
  if java:env['JAVA_HOME']=str(pathlib.Path(java).resolve().parent.parent)
 env['GBASE_TEST_URL']=f'jdbc:mysql://127.0.0.1:{port}/test?useSSL=false&allowPublicKeyRetrieval=true&useServerPrepStmts=true&cachePrepStmts=true&useLocalSessionState=false'
 mvn=pathlib.Path(env.get('MAVEN_CMD',str(BASE/'apache-maven-3.9.9/bin/mvn.cmd')))
 args=['cmd.exe','/c',str(mvn),'-B','-q','-Dmaven.repo.local='+str(BASE/'m2'),'-f',str(ROOT/'scripts/java-compat/pom.xml'),'test']
 with (BASE/'integration.log').open('wb') as output:code=subprocess.run(args,cwd=ROOT,env=env,stdout=output,stderr=output,creationflags=FLAGS,timeout=300).returncode
 (BASE/'integration.json').write_text(json.dumps(dict(exit_code=code,java=17,spring_boot='3.5.0',mybatis_plus='3.5.12',connector_j='9.3.0',isolated=True),indent=2),encoding='utf-8')
 print('Java integration exit:',code,'log:',BASE/'integration.log')
finally:
 if proc is not None and proc.poll() is None:proc.terminate();proc.wait(timeout=20)
 log.close()
 if work.parent!=BASE.resolve() or not work.name.startswith('run-'):raise RuntimeError('Unsafe cleanup target')
 shutil.rmtree(work)
sys.exit(code)
