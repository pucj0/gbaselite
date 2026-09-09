"""Render only the fresh, complete 60-case four-database experiment."""
import argparse, csv, hashlib, json, math, pathlib, statistics, sys
ROOT = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / '.tmp/db-comparison/packages'))
import matplotlib
matplotlib.use('Agg')
import matplotlib.pyplot as plt
from matplotlib.font_manager import FontProperties, fontManager
parser=argparse.ArgumentParser(description=__doc__)
parser.add_argument('--output',default='docs/报告')
args=parser.parse_args()
OUT = ROOT / args.output
DATA = OUT / '数据/四数据库对比-2026-09-08'
ENGINES = ['gbaselite', 'mysql', 'postgresql', 'sqlite']
SIZES = [10, 1000, 10000, 100000, 1000000]
LABELS = dict(zip(ENGINES, ['GBaseLite MVCC', 'MySQL', 'PostgreSQL', 'SQLite（嵌入式）']))
COLORS = ['#cf5936', '#2779b3', '#7359a8', '#138979']
raw = {}
for e in ENGINES:
    for n in SIZES:
        raw[e,n] = [json.loads((DATA / f'round-{i}/{e}-{n}.json').read_text(encoding='utf-8-sig')) for i in range(1,4)]
        for r in raw[e,n]:
            assert r['status'] == 'complete' and r['all_stages_ok'] and r['temporary_database_removed'], (e,n,r)
            assert r['stages']['update']['verified'] and r['stages']['update']['affected_rows'] == n
meta = json.loads((DATA/'运行记录.json').read_text(encoding='utf-8-sig'))
assert 'end' in meta and not meta['failed_cases']
env = json.loads((DATA/'环境.json').read_text(encoding='utf-8-sig'))
METRICS = {
 'insert_s': ('INSERT 总耗时', '秒', lambda r:r['stages']['insert']['seconds']),
 'insert_rows_s': ('INSERT 吞吐', '行/秒', lambda r:r['rows']/r['stages']['insert']['seconds']),
 'update_s': ('原子 UPDATE', '秒', lambda r:r['stages']['update']['seconds']),
 'point_p50_ms': ('点查 P50', '毫秒', lambda r:r['stages']['point']['p50_ms']),
 'point_p95_ms': ('点查 P95', '毫秒', lambda r:r['stages']['point']['p95_ms']),
 'point_p99_ms': ('点查 P99', '毫秒', lambda r:sorted(r['stages']['point']['samples_ms'])[197]),
 'range_ms': ('范围读取', '毫秒', lambda r:r['stages']['range']['seconds']*1000),
 'aggregate_ms': ('COUNT + SUM', '毫秒', lambda r:r['stages']['aggregate']['seconds']*1000),
 'insert_cpu_s': ('INSERT CPU', 'CPU秒', lambda r:r['stages']['insert']['cpu_seconds']),
 'update_cpu_s': ('UPDATE CPU', 'CPU秒', lambda r:r['stages']['update']['cpu_seconds']),
 'cpu_s': ('整个工作负载 CPU', 'CPU秒', lambda r:r['resources']['cpu_seconds']),
 'cpu_core_pct': ('平均单核等效 CPU', '%', lambda r:r['resources']['cpu_seconds']/r['workload_seconds']*100),
 'rss_mib': ('采样 RSS 峰值', 'MiB', lambda r:r['resources']['rss_peak_bytes']/2**20),
 'private_mib': ('采样 Private 峰值', 'MiB', lambda r:r['resources']['private_peak_bytes']/2**20),
 'empty_mib': ('空实例目录', 'MiB', lambda r:r['empty_data_bytes']/2**20),
 'insert_disk_mib': ('插入后目录', 'MiB', lambda r:r['data_after_insert_bytes']/2**20),
 'update_disk_mib': ('更新后目录', 'MiB', lambda r:r['data_after_update_bytes']/2**20),
 'growth_mib': ('更新后相对空库增长', 'MiB', lambda r:(r['data_after_update_bytes']-r['empty_data_bytes'])/2**20),
}
def samples(e,n,k): return [METRICS[k][2](r) for r in raw[e,n]]
def med(e,n,k): return statistics.median(samples(e,n,k))
def fmt(v): return f'{v:,.4f}' if abs(v)<1 else f'{v:,.2f}'
def table(keys):
    lines=['| 行数 | 数据库 | '+' | '.join(METRICS[k][0]+' / '+METRICS[k][1] for k in keys)+' |', '| ---: | --- | '+' | '.join('---:' for _ in keys)+' |']
    for n in SIZES:
        for e in ENGINES: lines.append('| '+f'{n:,} | {LABELS[e]} | '+' | '.join(fmt(med(e,n,k)) for k in keys)+' |')
    return '\n'.join(lines)
with (DATA/'三轮指标汇总.csv').open('w',encoding='utf-8-sig',newline='') as f:
    w=csv.writer(f);w.writerow(['engine','rows','metric','unit','round_1','round_2','round_3','median','min','max'])
    for n in SIZES:
        for e in ENGINES:
            for k,(_,unit,_) in METRICS.items():
                a=samples(e,n,k);w.writerow([e,n,k,unit,*a,statistics.median(a),min(a),max(a)])
font=pathlib.Path('C:/Windows/Fonts/msyh.ttc')
if font.exists():
    fontManager.addfont(str(font));plt.rcParams['font.family']=FontProperties(fname=str(font)).get_name()
plt.rcParams.update({'font.size':11,'axes.spines.top':False,'axes.spines.right':False,'axes.grid':True,'grid.alpha':.2,'figure.facecolor':'#f5f7fb','axes.facecolor':'white','svg.fonttype':'path','axes.unicode_minus':False})
IMAGES=OUT/'图片';IMAGES.mkdir(parents=True,exist_ok=True)
def save(fig,name):
    for ext in ['png','svg']:fig.savefig(IMAGES/f'{name}.{ext}',dpi=160,facecolor=fig.get_facecolor())
    plt.close(fig)
def chart(keys,name,title,note):
    fig,axs=plt.subplots(2,3,figsize=(18,11))
    for ax,k in zip(axs.flat,keys):
        for e,c in zip(ENGINES,COLORS):
            ys=[med(e,n,k) for n in SIZES]
            ax.plot(SIZES,ys,'o-',color=c,label=LABELS[e],lw=2,ms=5)
            ax.fill_between(SIZES,[min(samples(e,n,k)) for n in SIZES],[max(samples(e,n,k)) for n in SIZES],color=c,alpha=.12)
        ax.set_xscale('log');ax.set_yscale('symlog',linthresh=.02) if 'cpu' in k else ax.set_yscale('log')
        if 'cpu' in k: ax.set_ylim(bottom=0)
        ax.set_xticks(SIZES,labels=['10','1千','1万','10万','100万']);ax.minorticks_off()
        ax.set_title(METRICS[k][0],loc='left',weight='bold');ax.set_ylabel(METRICS[k][1]);ax.set_xlabel('数据行数')
    fig.suptitle(title,x=.065,y=.97,ha='left',fontsize=24,weight='bold')
    handles,labels=axs[0,0].get_legend_handles_labels();fig.legend(handles,labels,loc='upper left',bbox_to_anchor=(.065,.925),ncol=4,frameon=False)
    fig.text(.065,.042,'2026-09-08 · 单客户端 · 持久化提交 · 128B文本 · 三轮中位数，阴影为最小至最大 · 非等资源极限排名',fontsize=10)
    fig.text(.065,.018,note,fontsize=10)
    fig.subplots_adjust(left=.065,right=.975,top=.82,bottom=.14,hspace=.55,wspace=.32)
    save(fig,name)
chart(['insert_s','update_s','point_p95_ms','point_p99_ms','range_ms','aggregate_ms'],'SQL性能对比','四数据库 SQL 性能实测 | 10 至 100 万行','耗时越低越好；双对数轴。SQLite直接嵌入，其他三个经本机TCP。范围读取10行或100行；查询属于热读。')
chart(['rss_mib','private_mib','insert_cpu_s','update_cpu_s','insert_disk_mib','update_disk_mib'],'资源占用对比','四数据库资源实测 | 内存、CPU 与文件占用','RSS为100ms采样；PG进程树RSS可能重复计共享页。CPU为累计CPU秒；SQLite含Python。目录含日志、预分配和旧版本。')
# Linear bars keep the million-row headline directly readable.
fig,axs=plt.subplots(2,3,figsize=(18,10))
for ax,k in zip(axs.flat,['insert_s','update_s','aggregate_ms','rss_mib','private_mib','cpu_s']):
    vals=[med(e,1000000,k) for e in ENGINES]
    ax.barh(range(4),vals,color=COLORS,height=.55)
    ax.set_yticks(range(4),labels=[LABELS[e] for e in ENGINES]);ax.invert_yaxis()
    ax.set_xlim(0,max(vals)*1.34)
    for i,v in enumerate(vals):ax.text(v+max(vals)*.025,i,fmt(v),va='center',fontsize=11)
    ax.set_title(METRICS[k][0],loc='left',weight='bold');ax.set_xlabel(METRICS[k][1])
fig.suptitle('百万行实测摘要 | 三轮中位数',x=.055,y=.965,ha='left',fontsize=25,weight='bold')
fig.text(.055,.035,'2026-09-08 · 数值越低越好 · 单客户端同步持久化 · SQLite嵌入式，其余本机TCP · 非等总资源配额',fontsize=11)
fig.subplots_adjust(left=.12,right=.965,top=.87,bottom=.13,hspace=.55,wspace=.63)
save(fig,'百万行性能摘要')
cap=[
 ['访问方式','MySQL协议子集','MySQL协议','PostgreSQL协议','进程内API'],
 ['复杂SQL','INNER/LEFT、分组；有限','JOIN / 分组 / 窗口','JOIN / 分组 / 窗口','JOIN / 分组 / 窗口'],
 ['事务并发','快照隔离、乐观冲突','InnoDB MVCC','MVCC','每个文件单写者'],
 ['索引','主键/联合覆盖；规则选路','多种索引与规划','丰富索引与规划','B树索引与规划'],
 ['JSON对象','JSON_OBJECT子集','原生JSON及函数','json / jsonb及函数','JSON函数及JSONB'],
 ['复制与选主','实验性固定三节点Raft','复制 / Group Replication','流复制；另配切换编排','内核无集群选主'],
 ['恢复运维','单机备份/恢复；无PITR','备份及日志恢复工具','备份 / WAL / PITR','在线Backup API'],
 ['本次验证','单机性能，未测HA','单机性能，未开复制','单机性能，未开复制','单机嵌入式性能'],
]
fig,ax=plt.subplots(figsize=(18,8));ax.axis('off')
t=ax.table(cellText=cap,colLabels=['能力',*LABELS.values()],cellLoc='left',loc='center',colWidths=[.12,.23,.22,.23,.20]);t.auto_set_font_size(False);t.set_fontsize(11);t.scale(1,2.65)
for (row,col),cell in t.get_celld().items():
    cell.set_edgecolor('#dce3ed');cell.set_facecolor('#e8eef7' if row==0 else ('#fff4ee' if col==1 else '#ffffff'));cell.PAD=.08
    if row==0:cell.set_text_props(weight='bold')
fig.suptitle('功能与适用性对比 | 能力清单，不是性能评分',x=.035,ha='left',y=.96,fontsize=23,weight='bold')
fig.text(.035,.065,'GBaseLite仅指本次MVCC后端；其他后端支持范围不同。复制、备份与容灾能力均需正确配置和独立验证。',fontsize=11)
fig.text(.035,.025,'依据：仓库实现与使用文档、各产品官方文档。百万行测试不等于TB级、高并发或生产高可用认证。',fontsize=11)
fig.subplots_adjust(left=.03,right=.975,top=.88,bottom=.13);save(fig,'功能与适用性对比')
capmd='| 能力 | '+' | '.join(LABELS.values())+' |\n| --- | --- | --- | --- | --- |\n'+'\n'.join('| '+' | '.join(r)+' |' for r in cap)
versions='\n'.join(f'- **{LABELS[e]}**：`{raw[e,10][0]["version"]}`。' for e in ENGINES)
summary='\n'.join(f'| {LABELS[e]} | '+ ' | '.join(fmt(med(e,1000000,k)) for k in ['insert_s','update_s','rss_mib','private_mib','update_disk_mib'])+' |' for e in ENGINES)
variability='\n'.join(f'| {LABELS[e]} | '+ ' | '.join(f'{fmt(min(samples(e,1000000,k)))}—{fmt(max(samples(e,1000000,k)))}' for k in ['insert_s','update_s'])+' |' for e in ENGINES)
text=(ROOT/'scripts/benchmarks/report_template.zh-CN.md').read_text(encoding='utf-8')
findings=['| 百万行指标 | 本次最低中位数 | GBaseLite | GBaseLite / 最低值 |','| --- | --- | ---: | ---: |']
for k in ['insert_s','update_s','point_p95_ms','range_ms','aggregate_ms','cpu_s','rss_mib','private_mib','update_disk_mib']:
    winner=min(ENGINES,key=lambda e:med(e,1000000,k));a=med(winner,1000000,k);b=med('gbaselite',1000000,k)
    findings.append(f'| {METRICS[k][0]} / {METRICS[k][1]} | {LABELS[winner]}：{fmt(a)} | {fmt(b)} | {b/a:.2f}倍 |' if a else f'| {METRICS[k][0]} | 低于计时精度 | {fmt(b)} | — |')
findings='各指标分别比较，不合成为总分；最低值只代表本次访问方式和配置。\n\n'+'\n'.join(findings)
host_env=f"主机：{env['os']['Caption']} {env['os']['Version']}，{env['cpu']['Name']}（{env['cpu']['NumberOfCores']}核、{env['cpu']['NumberOfLogicalProcessors']}逻辑处理器），可见物理内存约{env['os']['TotalVisibleMemorySize']/2**20:.1f} GiB。E盘：{env['benchmark_drive']['FriendlyName']}，{env['benchmark_drive']['BusType']}；{env['go']}。源码基于提交`{env['git_head']}`的未提交工作区；程序哈希与源码哈希另行记录，不能仅凭该提交号复现候选。"
subs={'HOST_ENV':host_env,'CURRENT_FINDINGS':findings,'VERSIONS':versions,'SUMMARY':summary,'VARIABILITY':variability,'WRITE_TABLE':table(['insert_s','insert_rows_s','update_s']),'READ_TABLE':table(['point_p50_ms','point_p95_ms','point_p99_ms','range_ms','aggregate_ms']),'CPU_TABLE':table(['insert_cpu_s','update_cpu_s','cpu_s','cpu_core_pct']),'MEMORY_TABLE':table(['rss_mib','private_mib']),'DISK_TABLE':table(['empty_mib','insert_disk_mib','update_disk_mib','growth_mib']),'CAPABILITIES':capmd,'START':meta['start'],'END':meta['end'],'BINARY_HASH':meta['binary_sha256'],'HARNESS_HASH':meta['harness_sha256'],'G_RSS':fmt(med('gbaselite',1000000,'rss_mib')),'G_UPDATE_RATIO':fmt(med('gbaselite',1000000,'update_s')/med('sqlite',1000000,'update_s'))}
for k,v in subs.items():text=text.replace('{{'+k+'}}',str(v))
assert '{{' not in text
(OUT/'四数据库对比报告.md').write_text(text,encoding='utf-8')
(DATA/'结果校验.json').write_text(json.dumps({'cases':60,'engines':4,'scales':SIZES,'rounds':3,'all_stages_ok':True,'all_update_counts_verified':True,'all_temporary_databases_removed':True,'scope':'COUNT/SUM and affected rows; range result content; no crash/restart/HA or full payload validation'},ensure_ascii=False,indent=2),encoding='utf-8')
manifest=[]
for p in sorted(OUT.rglob('*')):
    if p.is_file() and p.name!='SHA256SUMS.txt':manifest.append(hashlib.sha256(p.read_bytes()).hexdigest()+'  '+p.relative_to(OUT).as_posix())
(OUT/'SHA256SUMS.txt').write_text('\n'.join(manifest)+'\n',encoding='utf-8')
print('Rendered 60 verified cases, 4 figures, Markdown, CSV and SHA256 manifest.')
