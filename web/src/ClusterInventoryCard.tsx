import { useEffect, useId, useRef, useState } from 'react'
import { Activity, Network, Search, X } from 'lucide-react'
import { api, APIError } from './api'
import './ClusterInventoryCard.css'

type Health = 'healthy' | 'degraded' | 'unhealthy' | 'unknown'
type HealthCounts = Record<Health,number>
type InventoryNode = {object_ref:string; identity:string; state:string; updated_at?:number; health?:Health; health_at?:number; pod_ref?:string; pod_name?:string}
type InventoryPod = {object_ref:string; identity:string; total_nodes:number; health:HealthCounts; status:Health}
type InventoryException = {object_ref:string; identity:string; state:string; health:Health; health_at:number; pod_ref?:string; pod_name?:string}
type InventorySummary = {total_nodes:number; health:HealthCounts; health_percent:number|null; topology_unknown_nodes:number; pods:InventoryPod[]; exception_total:number; exceptions:InventoryException[]; computed_at:number}
export type InventoryPage = {items:InventoryNode[]; next_cursor?:string; summary?:InventorySummary}

const healthOrder:Health[] = ['healthy','degraded','unhealthy','unknown']
const healthNames:Record<Health,string> = {healthy:'健康',degraded:'降级',unhealthy:'不健康',unknown:'未知'}
const healthMarks:Record<Health,string> = {healthy:'',degraded:'!',unhealthy:'×',unknown:'?'}
const number = new Intl.NumberFormat('zh-CN')
const record = (value:unknown):value is Record<string,unknown> => typeof value==='object' && value!==null && !Array.isArray(value)
const count = (value:unknown):value is number => typeof value==='number' && Number.isSafeInteger(value) && value>=0
const isHealth = (value:unknown):value is Health => typeof value==='string' && healthOrder.some(item=>item===value)
const isHealthCounts = (value:unknown):value is HealthCounts => record(value) && healthOrder.every(key=>count(value[key]))
const isNode = (value:unknown):value is InventoryNode => record(value) && typeof value.object_ref==='string' && typeof value.identity==='string' && typeof value.state==='string' && (value.health===undefined || isHealth(value.health)) && (value.health_at===undefined || count(value.health_at)) && (value.pod_ref===undefined || typeof value.pod_ref==='string') && (value.pod_name===undefined || typeof value.pod_name==='string')
const isPod = (value:unknown):value is InventoryPod => record(value) && typeof value.object_ref==='string' && typeof value.identity==='string' && count(value.total_nodes) && isHealthCounts(value.health) && isHealth(value.status)
const isException = (value:unknown):value is InventoryException => record(value) && isNode(value) && isHealth(value.health) && count(value.health_at) && (value.pod_ref===undefined || typeof value.pod_ref==='string') && (value.pod_name===undefined || typeof value.pod_name==='string')
const isSummary = (value:unknown):value is InventorySummary => record(value) && count(value.total_nodes) && isHealthCounts(value.health) && (value.health_percent===null || (typeof value.health_percent==='number' && Number.isFinite(value.health_percent) && value.health_percent>=0 && value.health_percent<=100)) && count(value.topology_unknown_nodes) && Array.isArray(value.pods) && value.pods.every(isPod) && count(value.exception_total) && Array.isArray(value.exceptions) && value.exceptions.every(isException) && count(value.computed_at)
export const isClusterInventoryPage = (value:unknown):value is InventoryPage => record(value) && Array.isArray(value.items) && value.items.every(isNode) && (value.next_cursor===undefined || typeof value.next_cursor==='string') && (value.summary===undefined || isSummary(value.summary))

function timeLabel(value:number) {
  const date=new Date(value)
  return Number.isFinite(date.getTime()) ? new Intl.DateTimeFormat('zh-CN',{month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit'}).format(date) : '时间未知'
}
function countsLabel(counts:HealthCounts) {
  return healthOrder.map(key=>`${healthNames[key]} ${number.format(counts[key])}`).join('，')
}
function errorText(error:unknown) {return error instanceof Error ? error.message : '请求未完成，请稍后重试'}

/** Summary is always server-computed across the whole authorized inventory. */
export function ClusterInventoryCard({data,variant='assistant'}:{data:InventoryPage;variant?:'assistant'|'page'}) {
  const [page,setPage]=useState(data)
  const [cursor,setCursor]=useState('')
  const [history,setHistory]=useState<string[]>([])
  const [loading,setLoading]=useState(false)
  const [error,setError]=useState('')
  const request=useRef(0)
  const topologyDialog=useRef<HTMLDialogElement>(null)
  const podRequest=useRef(0)
  const [selectedPod,setSelectedPod]=useState<InventoryPod>()
  const [podPage,setPodPage]=useState<InventoryPage>()
  const [podCursor,setPodCursor]=useState('')
  const [podHistory,setPodHistory]=useState<string[]>([])
  const [podLoading,setPodLoading]=useState(false)
  const [podError,setPodError]=useState('')
  const topologyTitle=useId()

  async function load(next:string,nextHistory:string[]) {
    const current=++request.current
    setLoading(true);setError('')
    try {
      const params=new URLSearchParams({limit:'24'});if(next)params.set('after',next)
      const result=await api<unknown>('/domain/cluster-inventory?'+params)
      if(!isClusterInventoryPage(result) || !result.summary)throw new Error('节点汇总数据格式无效，请重试')
      if(current===request.current){setPage(result);setCursor(next);setHistory(nextHistory)}
    } catch(e) {if(current===request.current)setError(errorText(e))} finally {if(current===request.current)setLoading(false)}
  }

  async function loadPod(pod:InventoryPod,next:string,nextHistory:string[]) {
    const current=++podRequest.current
    setSelectedPod(pod);setPodPage(undefined);setPodCursor(next);setPodHistory(nextHistory);setPodLoading(true);setPodError('')
    try {
      const params=new URLSearchParams({limit:'24',pod_ref:pod.object_ref});if(next)params.set('after',next)
      const result=await api<unknown>('/domain/cluster-inventory?'+params)
      if(!isClusterInventoryPage(result) || !result.summary || result.items.some(node=>node.pod_ref!==pod.object_ref))throw new Error('POD 节点数据格式无效，请重试')
      const currentPod=result.summary.pods.find(item=>item.object_ref===pod.object_ref)
      if(!currentPod)throw new Error('POD 已不在当前汇总中，请重试')
      if(current===podRequest.current){
        setPage(previous=>({...previous,summary:result.summary}));setSelectedPod(currentPod);setPodPage(result)
      }
    } catch(e) {
      if(current===podRequest.current){
        if(e instanceof APIError&&(e.status===403||e.status===404)){setSelectedPod(undefined);setPodError('该 POD 已不可查看，正在刷新集群汇总。');void load('',[])}
        else setPodError(errorText(e))
      }
    } finally {if(current===podRequest.current)setPodLoading(false)}
  }

  function openTopology(pod?:InventoryPod) {
    if(pod)void loadPod(pod,'',[])
    else {podRequest.current++;setSelectedPod(undefined);setPodPage(undefined);setPodLoading(false);setPodError('')}
    if(!topologyDialog.current?.open)topologyDialog.current?.showModal()
  }

  useEffect(()=>{
    request.current++
    podRequest.current++
    setPage(data);setCursor('');setHistory([]);setError('')
    setSelectedPod(undefined);setPodPage(undefined);setPodError('');setPodLoading(false)
    if(topologyDialog.current?.open)topologyDialog.current.close()
    if(!data.summary)void load('',[])
    else setLoading(false)
    return ()=>{request.current++;podRequest.current++}
  },[data])

  const summary=page.summary
  const availablePod=summary?.pods.find(pod=>pod.object_ref===selectedPod?.object_ref)
  const podLabel=(pod:InventoryPod)=>{
    const name=pod.identity || pod.object_ref
    return summary?.pods.some(other=>other.object_ref!==pod.object_ref&&(other.identity || other.object_ref)===name) ? `${name} · ${pod.object_ref}` : name
  }
  return <section className={`${variant==='page'?'ops-card':'ops-assistant-card'} cluster-card`} aria-label="集群与超节点 POD 总览">
    <header className="ops-card-heading cluster-heading">
      <h2><Activity size={15} aria-hidden="true"/>集群与超节点 POD 总览{summary && <span>（{number.format(summary.total_nodes)} 节点 · {number.format(summary.pods.length)} POD）</span>}</h2>
      {summary && <div className="cluster-badges"><span className={`ops-badge ${summary.health_percent==null?'':'green'}`}>健康率 {summary.health_percent==null?'未知':`${summary.health_percent.toFixed(1)}%`}</span>{summary.exception_total>0 && <span className="ops-badge cluster-attention">{number.format(summary.exception_total)} 待关注</span>}</div>}
    </header>
    {!summary && (loading ? <p className="ops-state">正在加载节点汇总…</p> : <p className="ops-state">暂无节点汇总</p>)}
    {summary && (summary.total_nodes===0 ? <p className="ops-state">暂无已授权节点</p> : <>
      <ul className="cluster-health" aria-label="已授权节点最近健康记录分布">{healthOrder.map(key=><li key={key} className={key}><i aria-hidden="true"/>{healthMarks[key]&&<b aria-hidden="true">{healthMarks[key]}</b>}{healthNames[key]} {number.format(summary.health[key])} <span>{(summary.health[key]*100/summary.total_nodes).toFixed(1)}%</span></li>)}</ul>
      {summary.pods.length>0 ? <ul className="cluster-pods" aria-label="POD 可见成员的健康状态">{summary.pods.map(pod=><li key={pod.object_ref}><button type="button" className={`cluster-pod ${pod.status}`} title={`${podLabel(pod)}（${pod.object_ref}） · ${healthNames[pod.status]}，${countsLabel(pod.health)}`} onClick={()=>openTopology(pod)}><span className="cluster-pod-name">{podLabel(pod)}</span>{healthMarks[pod.status]&&<span className="cluster-pod-mark" aria-hidden="true">{healthMarks[pod.status]}</span>}<span className="ops-sr-only">，标识 {pod.object_ref}，{healthNames[pod.status]}，{number.format(pod.total_nodes)} 个可见节点，{countsLabel(pod.health)}。查看成员</span></button></li>)}</ul> : <p className="cluster-empty">暂无可确认且有权查看的 POD 关系</p>}
      {summary.exceptions.length>0 && <div className="cluster-exceptions" tabIndex={0} role="region" aria-label="待关注节点"><ul>{summary.exceptions.map(node=><li key={node.object_ref}><span><strong>{node.pod_name ? `${node.pod_name} / ` : ''}{node.identity || node.object_ref}</strong> · {node.state || '状态未知'}{node.health_at>0 && ` · 记录于 ${timeLabel(node.health_at)}`}</span><span className={`cluster-tag ${node.health}`}>{healthNames[node.health]}</span></li>)}</ul>{summary.exception_total>summary.exceptions.length && <p>显示前 {number.format(summary.exceptions.length)} 项，共 {number.format(summary.exception_total)} 项待关注</p>}</div>}
      <p className="cluster-note">仅统计已授权节点的最近健康记录，可能并非实时（汇总于 {timeLabel(summary.computed_at)}）；POD 仅包含有权查看的节点。{summary.topology_unknown_nodes>0 && ` ${number.format(summary.topology_unknown_nodes)} 个节点的 POD 归属未确认。`}</p>
      <div className="cluster-footer"><details className="cluster-details"><summary>查看节点状态</summary><ul className="cluster-nodes">{page.items.map(node=><li key={node.object_ref}><strong>{node.identity || node.object_ref}</strong><span>{node.state || '状态未知'} · {healthNames[node.health || 'unknown']}</span></li>)}</ul><div className="ops-node-footer"><span>本页 {number.format(page.items.length)} 个节点</span><div><button type="button" disabled={loading || !history.length} onClick={()=>void load(history.at(-1)||'',history.slice(0,-1))}>上一页</button><button type="button" disabled={loading || !page.next_cursor} onClick={()=>page.next_cursor&&void load(page.next_cursor,[...history,cursor])}>下一页</button></div></div></details>
        <div className="cluster-actions"><button type="button" onClick={()=>openTopology()}><Network size={13} aria-hidden="true"/>查看全局拓扑</button><button type="button" disabled={!availablePod} title={availablePod?podLabel(availablePod):'选择 POD 后查看成员'} onClick={()=>availablePod&&openTopology(availablePod)}><Search size={13} aria-hidden="true"/>下钻 {availablePod?podLabel(availablePod):'POD'} 明细</button></div></div>
    </>)}
    {error && <p className="cluster-error" role="alert">{error} <button type="button" onClick={()=>void load(cursor,history)}>重试</button></p>}
    <dialog className="cluster-topology" ref={topologyDialog} aria-labelledby={topologyTitle} onClose={()=>{podRequest.current++;setPodLoading(false);setPodPage(undefined)}}>
      <header><div><h2 id={topologyTitle}>集群拓扑</h2><p>已登记且当前可查看的 POD 与节点</p></div><button type="button" autoFocus className="cluster-topology-close" aria-label="关闭集群拓扑" onClick={()=>topologyDialog.current?.close()}><X size={17}/></button></header>
      <div className="cluster-topology-body"><div className="cluster-topology-groups"><h3>POD</h3><ul>{summary?.pods.map(pod=><li key={pod.object_ref}><button type="button" aria-pressed={selectedPod?.object_ref===pod.object_ref} title={pod.object_ref} onClick={()=>void loadPod(pod,'',[])}><span>{podLabel(pod)}</span><small>{number.format(pod.total_nodes)} 节点 · {healthNames[pod.status]}</small></button></li>)}</ul>{Boolean(summary?.topology_unknown_nodes)&&<p>{number.format(summary!.topology_unknown_nodes)} 个节点归属未确认</p>}</div>
        <section className="cluster-topology-members" aria-label="POD 节点明细" aria-live="polite">{selectedPod&&<h3>{podLabel(selectedPod)} · {number.format(selectedPod.total_nodes)} 节点</h3>}{podLoading?<p className="ops-state" role="status">正在加载节点…</p>:podError?<p className="cluster-error" role="alert">{podError}{selectedPod&&<> <button type="button" onClick={()=>void loadPod(selectedPod,podCursor,podHistory)}>重试</button></>}</p>:podPage?<><ul className="cluster-topology-nodes">{podPage.items.map(node=><li key={node.object_ref}><strong>{node.identity || node.object_ref}</strong><span>{node.state || '状态未知'} · {healthNames[node.health || 'unknown']}{node.health_at?` · 记录于 ${timeLabel(node.health_at)}`:''}</span></li>)}</ul><div className="ops-node-footer"><span>本页 {number.format(podPage.items.length)} 个节点</span><div><button type="button" disabled={!podHistory.length} onClick={()=>selectedPod&&void loadPod(selectedPod,podHistory.at(-1)||'',podHistory.slice(0,-1))}>上一页</button><button type="button" disabled={!podPage.next_cursor} onClick={()=>selectedPod&&podPage.next_cursor&&void loadPod(selectedPod,podPage.next_cursor,[...podHistory,podCursor])}>下一页</button></div></div></>:<p className="ops-state">选择 POD 查看已授权节点</p>}</section>
      </div>
    </dialog>
  </section>
}
