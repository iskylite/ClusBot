import {Activity} from 'lucide-react'

const healthLabels={healthy:'健康',degraded:'降级',unhealthy:'不健康',unknown:'未知'} as const
const number = new Intl.NumberFormat('zh-CN')
const date = new Intl.DateTimeFormat('zh-CN',{month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit'})
type HealthKey=keyof typeof healthLabels
const healthKeys=Object.keys(healthLabels) as HealthKey[]
type Evidence={scope:'pod'|'global';pod?:{object_ref:string;identity:string};total_nodes:number;health:Record<HealthKey,number>;computed_at:number}

function isObject(value:unknown):value is Record<string,unknown> {
  return Boolean(value && typeof value==='object' && !Array.isArray(value))
}

function isEvidence(value:unknown):value is Evidence {
  if(!isObject(value)||!isObject(value.health)||(value.scope!=='pod'&&value.scope!=='global'))return false
  const counts=value.health
  if(!healthKeys.every(key=>typeof counts[key]==='number'&&Number.isSafeInteger(counts[key])&&counts[key]>=0))return false
  if(typeof value.total_nodes!=='number'||!Number.isSafeInteger(value.total_nodes)||value.total_nodes<0||
      typeof value.computed_at!=='number'||!Number.isFinite(new Date(value.computed_at).getTime()))return false
  return value.scope==='global'||(isObject(value.pod)&&typeof value.pod.object_ref==='string'&&typeof value.pod.identity==='string')
}

export function OperationEvidenceCard({result}:{result:unknown}) {
  if(!isEvidence(result))return <section className="assistant-card"><p role="status">未获取可核对的节点健康数据。</p></section>
  const scope=result.scope==='global'?'全局可见节点':result.pod?.identity||result.pod?.object_ref
  return <section className="assistant-card" aria-label="本地健康记录">
    <div className="assistant-card-title"><h3><Activity size={15} aria-hidden="true"/>{scope} · 最近健康记录</h3></div>
    <p>{number.format(result.total_nodes)} 个节点 · {healthKeys.map(key=>healthLabels[key]+' '+number.format(result.health[key])).join(' · ')}</p>
    <p className="muted">本地汇总于 {date.format(result.computed_at)} · 实时状态待核对</p>
  </section>
}
