import { useEffect, useMemo, useRef, useState } from 'react'
import { ArrowDownToLine, ChevronRight, CircleCheck, CircleDot, CircleHelp, Clock3, GitBranch, RefreshCw, Terminal, TrendingUp, X } from 'lucide-react'
import { Area, AreaChart, Bar, BarChart, CartesianGrid, Cell, LabelList, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts'
import { api, APIError } from './api'
import { ClusterInventoryCard, isClusterInventoryPage, type InventoryPage } from './ClusterInventoryCard'
import './operations.css'

type Range = '24h' | '7d' | '30d'
type Loadable<T> = { loading:boolean; data?:T; error?:string }
type TrendBucket = {
  start_at:number; end_at:number; total_nodes:number; in_service_nodes:number
  unknown_nodes:number; in_service_percent:number|null; fault_counts:Record<string,number>
}
type ClusterTrends = {
  range:Range; timezone:string; buckets:TrendBucket[]; fault_categories:string[]
}
type TokenBucket = {date:string; input_tokens:number|null; output_tokens:number|null; total_tokens:number|null; requests:number; unreported_requests:number}
type TokenModel = {model:string; brand?:string; input_tokens:number|null; output_tokens:number|null; total_tokens:number|null; percent:number|null; requests:number; unreported_requests:number}
type TokenStats = {range:Range; total_tokens:number|null; input_tokens:number|null; output_tokens:number|null; requests:number; unreported_requests?:number; daily_buckets:TokenBucket[]; by_model:TokenModel[]}
type Incident = {incident_id:string; symptoms:string; state:string; updated_at:number}
type IncidentAction = {action_id:string; timestamp:number; summary:string; status:string; has_logs:boolean; log_preview?:string}
type Stage = {step_key:string; label:string; status:string; summary:string; policy_note?:string; proposal_risk_note?:string; impact_note?:string; actions:IncidentAction[]}
type Workflow = {incident_id:string; state:string; symptoms:string; impact_scope:string; active_step:string; stages:Stage[]}
type StepLogs = {incident_id:string; step_key:string; source:string; streaming:boolean; lines:string[]}
type TicketOption = {ticket_id:string; object_ref:string; state:string}
type IncidentActionOptions = {tickets:TicketOption[]; operations:{operation_id:string;object_ref:string;state:string;reason?:string}[]; assets:{object_ref:string;identity:string;state:string}[]; next_cursor?:string; ticket_available:boolean; ticket_reason?:string; recheck_available:boolean}
type IncidentPreview = {preview_id:string; incident_id:string; object_ref:string; expires_at:number; description?:string; ticket_id?:string}

const ranges:Range[] = ['24h','7d','30d']
const rangeNames:Record<Range,string> = {'24h':'近 24 小时','7d':'近 7 日','30d':'近 30 日'}
const faultNames:Record<string,string> = {nhc:'节点健康检查',oob_alert:'带外告警',network:'网络',service:'服务',other:'其他'}
const faultColors = ['#ef4444','#f59e0b','#10b981','#3b82f6','#818cf8']
const modelColors = ['#4f6df5','#6257df','#1f98d1','#31a783','#e19b36','#9e70b4']
const brandLogos=import.meta.glob('./ai-brands/*.svg',{eager:true,query:'?url',import:'default'}) as Record<string,string>
const number = new Intl.NumberFormat('zh-CN')
const compact = new Intl.NumberFormat('en-US',{notation:'compact',maximumFractionDigits:2})
const errorMessage = (error:unknown) => error instanceof Error ? error.message : '请求未完成，请稍后重试'
const tokenNumber = (value:number|null|undefined) => value == null ? '未知' : compact.format(value)
function reportedTokens<T extends {input_tokens:number|null;output_tokens:number|null;total_tokens:number|null;requests:number;unreported_requests?:number}>(entry:T):T {
  if (!entry.unreported_requests || entry.unreported_requests < entry.requests) return entry
  return {...entry, input_tokens:entry.input_tokens || null, output_tokens:entry.output_tokens || null, total_tokens:entry.total_tokens || null}
}

function ModelMark({brand,color}:{brand?:string;color:string}) {
  const logo=brand&&brandLogos['./ai-brands/'+brand+'.svg']
  return logo ? <span className="ops-model-logo" aria-hidden="true" style={{background:color,maskImage:'url('+JSON.stringify(logo)+')'}}/> : <i aria-hidden="true" style={{background:color}}/>
}

function rateDomain(buckets:TrendBucket[]):[number,number] {
  const values=buckets.map(bucket=>bucket.in_service_percent).filter((value):value is number=>value!=null && Number.isFinite(value))
  if(!values.length)return [0,100]
  const low=Math.min(...values), high=Math.max(...values)
  const padding=Math.max(.5,(high-low)*.2)
  let min=Math.max(0,Math.floor(low-padding)), max=Math.min(100,Math.ceil(high+padding))
  if(max-min<2){min=Math.max(0,max-2);max=Math.min(100,min+2)}
  return [min,max]
}

function timeLabel(value:number, range:Range, full=false) {
  const date = new Date(value)
  if (!Number.isFinite(date.getTime())) return '时间未知'
  const month = date.getUTCMonth()+1
  const day = date.getUTCDate()
  const hour = String(date.getUTCHours()).padStart(2,'0')
  if (range === '24h') return full ? `${month}月${day}日 ${hour}:00 UTC` : `${hour}:00`
  return full ? `${date.getUTCFullYear()}年${month}月${day}日 UTC` : range==='7d' ? `${day}日` : `${month}/${day}`
}
function actionTime(value:number) {
  if (!value) return '时间未知'
  const date = new Date(value)
  return Number.isFinite(date.getTime()) ? new Intl.DateTimeFormat('zh-CN',{hour:'2-digit',minute:'2-digit',second:'2-digit'}).format(date) : '时间未知'
}
function textValue(value:string|undefined) {
  if (!value || value === '{}' || value === '[]') return '未记录'
  return value
}
function statusText(status:string) {
  return ({completed:'已完成',active:'进行中',conditional:'按条件执行',pending:'待处理',blocked:'受阻',
    verified:'已验证',failed:'失败',proposed:'已提出',admitted:'已准入',dispatched:'已派发',
    open:'待处理',investigating:'调查中',mitigated:'已缓解',resolved:'已恢复',closed:'已关闭',
    registered:'已登记',running:'进行中',reconcile:'待核对',cancelled:'已取消',succeeded:'已完成',rejected:'已拒绝',executed:'已执行'} as Record<string,string>)[status] || status || '未知'
}
function SectionState({loading,error,empty,retry}:{loading:boolean;error?:string;empty?:string;retry?:()=>void}) {
  if (loading) return <p className="ops-state" role="status">正在加载…</p>
  if (error) return <div className="ops-state ops-error" role="alert"><span>{error}</span>{retry && <button type="button" onClick={retry}>重试</button>}</div>
  return empty ? <p className="ops-state">{empty}</p> : null
}
function ChartTip({active,payload,label,range}:{active?:boolean;payload?:readonly {name?:string;value?:number|string;color?:string;payload?:{unreported_requests?:number}}[];label?:string|number;range?:Range}) {
  if (!active || !payload?.length) return null
  return <div className="ops-tooltip"><strong>{range ? timeLabel(Number(label),range,true) : `${label} UTC`}</strong>
    {payload.map((item,index)=><div key={`${item.name}-${index}`}><i style={{background:item.color}}/>{item.name}：{item.value == null ? '未知' : number.format(Number(item.value))}{item.name === '在役率' ? '%' : ''}</div>)}
    {payload[0]?.payload?.unreported_requests ? <div>{payload[0].payload.unreported_requests} 次未完整报告</div> : null}
  </div>
}

function IncidentActions({incident, symptoms, showLogs, toggleLogs, refresh}:{incident:string; symptoms:string; showLogs:boolean; toggleLogs:()=>void; refresh:()=>void}) {
  const [options,setOptions]=useState<IncidentActionOptions>()
  const [optionsError,setOptionsError]=useState('')
  const [hidden,setHidden]=useState(false)
  const [mode,setMode]=useState<'ticket'|'recheck'>('ticket')
  const [page,setPage]=useState<InventoryPage>()
  const [cursor,setCursor]=useState('')
  const [history,setHistory]=useState<string[]>([])
  const [asset,setAsset]=useState('')
  const [description,setDescription]=useState(symptoms)
  const [ticket,setTicket]=useState('')
  const [preview,setPreview]=useState<IncidentPreview>()
  const [issued,setIssued]=useState(false)
  const [pending,setPending]=useState(false)
  const [error,setError]=useState('')
  const [notice,setNotice]=useState('')
  const dialog=useRef<HTMLDialogElement>(null)
  const lock=useRef(false)
  const optionsRequest=useRef(0)
  const base=`/incidents/${encodeURIComponent(incident)}`

  async function loadOptions() {
    const request=++optionsRequest.current
    try {
      const result=await api<IncidentActionOptions>(`${base}/ticket/options`)
      if(request!==optionsRequest.current)return
      setOptions(result);setPage({items:result.assets,next_cursor:result.next_cursor});setCursor('');setHistory([]);setOptionsError('');setHidden(false)
    } catch(e) {
      if(request!==optionsRequest.current)return
      if(e instanceof APIError && (e.status===403 || e.status===404 || e.code==='account_feature_unavailable')){setHidden(true);return}
      setOptionsError(errorMessage(e))
    }
  }
  useEffect(()=>{setOptions(undefined);setHidden(false);setNotice('');void loadOptions();return()=>{optionsRequest.current++}},[incident])

  async function loadAssets(next:string,nextHistory:string[]) {
    try {
      const query=new URLSearchParams();if(next)query.set('after',next)
      const result=await api<IncidentActionOptions>(`${base}/ticket/options?${query}`)
      if(!Array.isArray(result.assets))throw new Error('资产列表格式无效，请重试')
      setOptions(result);setPage({items:result.assets,next_cursor:result.next_cursor});setCursor(next);setHistory(nextHistory)
    } catch(e){setError(errorMessage(e))}
  }
  function open(action:'ticket'|'recheck') {
    if(!issued || mode!==action){setPreview(undefined);setIssued(false);setAsset('');setTicket('');setDescription(symptoms)}
    setMode(action);setError('')
    if(action==='ticket')void loadAssets('',[])
    dialog.current?.showModal()
  }
  async function submit() {
    if(lock.current)return
    lock.current=true;setPending(true);setError('')
    try {
      if(!preview) {
        const body=mode==='ticket'?{object_ref:asset,description}:{ticket_id:ticket}
        const result=await api<IncidentPreview>(`${base}/${mode==='ticket'?'ticket':'recheck'}/preview`,body)
        if(!result.preview_id || result.incident_id!==incident || !result.object_ref)throw new Error('确认信息不完整，请重试')
        setPreview(result)
      } else {
        setIssued(true)
        if(mode==='ticket') {
          const result=await api<{operation_id:string;state:string;reason?:string}>(`${base}/ticket/confirm`,{preview_id:preview.preview_id})
          if(!result.operation_id)throw new Error('未收到有效回执，请重试同一确认')
          setNotice(`工单请求：${statusText(result.state)}${result.reason?`（${result.reason}）`:''}。`)
        } else {
          const result=await api<{object_ref:string;incident_acceptance:string}>(`${base}/recheck/confirm`,{preview_id:preview.preview_id})
          if(result.object_ref!==preview.object_ref || !['pending','changed'].includes(result.incident_acceptance))throw new Error('未收到有效回执，请重试同一确认')
          setNotice(result.incident_acceptance==='pending'?`资产 ${result.object_ref} 复检通过；事件仍待验收。`:`资产 ${result.object_ref} 的复检记录已确认；事件状态已变化，请刷新查看。`)
        }
        setPreview(undefined);setIssued(false);dialog.current?.close();refresh();void loadOptions()
      }
    } catch(e) {
      if(preview && e instanceof APIError && e.status===409) {
        setIssued(false)
        setError(`${errorMessage(e)}。请返回修改并重新预览。`)
      } else {
        setError(preview ? `${errorMessage(e)}。可重试同一确认，或关闭后核对当前状态。` : errorMessage(e))
      }
    } finally { lock.current=false;setPending(false) }
  }

  const eligible=options?.tickets.filter(item=>item.state==='closed' || item.state==='verified') || []
  return <>
    {notice&&<p className="ops-action-notice" role="status">{notice}</p>}
    {options?.operations?.map(op=><p className="ops-action-notice" key={op.operation_id}>资产 {op.object_ref} 的工单请求：{statusText(op.state)}{op.reason?`（${op.reason}）`:''}</p>)}
    {optionsError&&<p className="ops-inline-error" role="alert">{optionsError}</p>}
    <footer className="ops-drawer-footer"><button type="button" aria-label={showLogs?'收起阶段日志':'查看阶段日志'} aria-expanded={showLogs} onClick={toggleLogs}><Terminal size={13}/>{showLogs?'收起日志':'原始日志'}</button>
      {!hidden&&<button type="button" aria-label="刷新工单状态" title="刷新工单状态" onClick={()=>{refresh();void loadOptions()}}><RefreshCw size={13}/></button>}
      {!hidden&&<><button type="button" className="ops-ticket-button" disabled={!options?.ticket_available} title={options?.ticket_reason || ''} onClick={()=>open('ticket')}>转维修工单</button>
      <button type="button" className="ops-verify-button" disabled={!options?.recheck_available || !eligible.length} title={!options?.recheck_available?'尚未配置独立健康验证服务':!eligible.length?'没有已关闭的可复检工单':''} onClick={()=>open('recheck')}>标记复检通过</button></>}
    </footer>
    <dialog className="ops-action-modal" ref={dialog} aria-labelledby="incident-action-title">
      <form onSubmit={event=>{event.preventDefault();void submit()}}>
        <h2 id="incident-action-title">{mode==='ticket'?'转维修工单':'确认资产复检'}</h2>
        {!preview ? mode==='ticket' ? <>
          <p>选择已授权资产并确认故障描述，提交后进入工单派发流程。</p>
          <label>维修资产<select required value={asset} onChange={event=>setAsset(event.target.value)}><option value="">请选择资产</option>{asset && !page?.items.some(item=>item.object_ref===asset) && <option value={asset}>{asset}</option>}{page?.items.map(item=><option key={item.object_ref} value={item.object_ref}>{item.identity || item.object_ref} · {item.object_ref} · {item.state}</option>)}</select></label>
          <div className="ops-action-pages"><button type="button" disabled={!history.length || pending} onClick={()=>void loadAssets(history.at(-1)||'',history.slice(0,-1))}>上一页</button><button type="button" disabled={!page?.next_cursor || pending} onClick={()=>page?.next_cursor&&void loadAssets(page.next_cursor,[...history,cursor])}>下一页</button></div>
          <label>故障描述<textarea required maxLength={8000} value={description} onChange={event=>setDescription(event.target.value)}/></label>
        </> : <><p>选择已关闭工单。系统将通过独立健康服务重新检查设备；本次仅确认该资产，事件仍待验收。</p>
          <label>工单<select required value={ticket} onChange={event=>setTicket(event.target.value)}><option value="">请选择工单</option>{eligible.map(item=><option key={item.ticket_id} value={item.ticket_id}>{item.object_ref} · {item.ticket_id}</option>)}</select></label></>
        : <><p>{mode==='ticket'?'请核对后确认提交。故障描述由操作者提供。':'系统将查询独立健康服务，只有检查通过才记录该资产复检。事件仍待验收。'}</p>
          <dl><dt>事件</dt><dd>{preview.incident_id}</dd><dt>资产</dt><dd>{preview.object_ref}</dd>{preview.ticket_id&&<><dt>工单</dt><dd>{preview.ticket_id}</dd></>}{preview.description&&<><dt>故障描述</dt><dd>{preview.description}</dd></>}</dl>
        </>}
        {error&&<p className="ops-inline-error" role="alert">{error}</p>}
        <div className="ops-modal-actions"><button type="button" disabled={pending} onClick={()=>dialog.current?.close()}>取消</button>{preview&&!issued&&<button type="button" disabled={pending} onClick={()=>{setPreview(undefined);setError('');void loadOptions()}}>返回修改</button>}<button type="submit" className="ops-modal-primary" disabled={pending}>{pending?'处理中…':preview?'确认':'预览'}</button></div>
      </form>
    </dialog>
  </>
}

export function OperationsPage({initialIncident,initialRange}:{isAdmin:boolean;initialIncident?:string;initialRange?:string}) {
  const [range,setRange] = useState<Range>(ranges.includes(initialRange as Range) ? initialRange as Range : '7d')
  const [reload,setReload] = useState(0)
  const [trend,setTrend] = useState<Loadable<ClusterTrends>>({loading:true})
  const [tokens,setTokens] = useState<Loadable<TokenStats>>({loading:true})
  const [inventory,setInventory] = useState<Loadable<InventoryPage>>({loading:true})
  const [incidents,setIncidents] = useState<Loadable<Incident[]>>({loading:true})
  const [incidentID,setIncidentID] = useState(initialIncident && /^(?:[a-f0-9]{32}|inc-[0-9]{1,16})$/.test(initialIncident) ? initialIncident : '')
  const [workflow,setWorkflow] = useState<Loadable<Workflow>>({loading:false})
  const [workflowReload,setWorkflowReload] = useState(0)
  const [stepKey,setStepKey] = useState('')
  const [showLogs,setShowLogs] = useState(false)
  const [logs,setLogs] = useState<Loadable<StepLogs>>({loading:false})
  const [logReload,setLogReload] = useState(0)
  const drawer = useRef<HTMLDialogElement>(null)

  useEffect(()=>{
    let current = true
    setTrend({loading:true})
    setTokens({loading:true})
    api<ClusterTrends>(`/domain/cluster-trends?range=${range}`).then(data=>{if(current)setTrend({loading:false,data})}).catch(error=>{if(current)setTrend({loading:false,error:errorMessage(error)})})
    api<TokenStats>(`/assistant/token-usage-stats?range=${range}`).then(data=>{if(current)setTokens({loading:false,data})}).catch(error=>{if(current)setTokens({loading:false,error:errorMessage(error)})})
    return ()=>{current=false}
  },[range,reload])
  useEffect(()=>{
    let current = true
    setInventory({loading:true})
    api<InventoryPage>('/domain/cluster-inventory?limit=24').then(data=>{if(current)setInventory({loading:false,data})}).catch(error=>{if(current)setInventory({loading:false,error:errorMessage(error)})})
    return ()=>{current=false}
  },[reload])
  useEffect(()=>{
    let current = true
    setIncidents({loading:true})
    api<Incident[]>('/incidents').then(data=>{
      if(!current)return
      setIncidents({loading:false,data})
      setIncidentID(previous=>previous || data[0]?.incident_id || '')
    }).catch(error=>{if(current)setIncidents({loading:false,error:errorMessage(error)})})
    return ()=>{current=false}
  },[reload])
  useEffect(()=>{
    if(!incidentID){setWorkflow({loading:false});return}
    let current = true
    setWorkflow(previous=>previous.data?.incident_id===incidentID?{...previous,loading:true,error:undefined}:{loading:true})
    api<Workflow>(`/incidents/${encodeURIComponent(incidentID)}/workflow`).then(data=>{if(current)setWorkflow({loading:false,data})}).catch(error=>{if(current)setWorkflow({loading:false,error:errorMessage(error)})})
    return ()=>{current=false}
  },[incidentID,workflowReload])
  useEffect(()=>{
    if(!stepKey || !showLogs || !incidentID){setLogs({loading:false});return}
    let current = true
    setLogs({loading:true})
    api<StepLogs>(`/incidents/${encodeURIComponent(incidentID)}/steps/${encodeURIComponent(stepKey)}/logs`).then(data=>{if(current)setLogs({loading:false,data})}).catch(error=>{if(current)setLogs({loading:false,error:errorMessage(error)})})
    return ()=>{current=false}
  },[incidentID,stepKey,showLogs,logReload])
  useEffect(()=>{
    if(stepKey && drawer.current && !drawer.current.open) drawer.current.showModal()
  },[stepKey])

  const buckets = trend.data?.buckets || []
  const last = buckets.at(-1)
  const chartBuckets = useMemo(()=>buckets.map(bucket=>({
    ...bucket,
    ...Object.fromEntries(Object.entries(bucket.fault_counts || {}).map(([key,value])=>[`fault_${key}`,value])),
  })),[buckets])
  const hasRate = buckets.some(bucket=>bucket.in_service_percent != null)
  const faultTotal = buckets.reduce((sum,bucket)=>sum+Object.values(bucket.fault_counts || {}).reduce((n,value)=>n+value,0),0)
  const categories = trend.data?.fault_categories || []
  const tokenBuckets = (tokens.data?.daily_buckets || []).map(reportedTokens)
  const models = (tokens.data?.by_model || []).map(reportedTokens)
  const selectedStage = workflow.data?.stages.find(stage=>stage.step_key===stepKey)
  function openStage(key:string){setShowLogs(false);setStepKey(key)}
  function changeStage(key:string){setShowLogs(false);setStepKey(key)}
  function retry(){setReload(value=>value+1)}

  return <section className="operations-page" aria-label="趋势分析与故障流程">
    <div className="ops-heading"><div><h1>趋势分析与故障流程</h1><p>查看已授权节点的状态、模型用量和故障处置进展</p></div></div>
    <div className="ops-cards">
      {(inventory.loading || inventory.error) && <section className="ops-card"><SectionState loading={inventory.loading} error={inventory.error} retry={retry}/></section>}
      {inventory.data && <ClusterInventoryCard data={inventory.data} variant="page"/>}
      <section className="ops-card" aria-labelledby="ops-trend-title">
        <header className="ops-card-heading"><h2 id="ops-trend-title"><TrendingUp size={16}/>集群在役率与故障趋势 <span>（{rangeNames[range]}）</span></h2>
          <div className="ops-range" aria-label="选择统计时间范围">{ranges.map(value=><button type="button" key={value} className={range===value?'selected':''} aria-pressed={range===value} onClick={()=>setRange(value)}>{value}</button>)}</div>
        </header>
        <SectionState loading={trend.loading} error={trend.error} retry={retry}/>
        {trend.data && <><div className="ops-trend-grid">
          <div className="ops-chart-panel"><div className="ops-chart-title"><h3>在役率趋势 (%)</h3><span className="ops-badge green">当前 {last?.in_service_percent == null ? '未知' : `${last.in_service_percent.toFixed(1)}%`}</span></div>
            {hasRate ? <div className="ops-chart" role="img" aria-label="已授权节点的在役率趋势，缺失状态显示为空缺"><ResponsiveContainer width="100%" height="100%"><AreaChart data={chartBuckets} margin={{top:10,right:10,bottom:0,left:-24}}>
              <CartesianGrid vertical={false} stroke="var(--ops-grid)"/><XAxis dataKey="start_at" tickFormatter={value=>timeLabel(Number(value),range)} tick={{fill:'var(--ops-muted)'}} tickLine={false} axisLine={false} minTickGap={22} fontSize={10}/><YAxis domain={rateDomain(buckets)} tickCount={3} tickFormatter={value=>`${Number(value).toFixed(1)}%`} tick={{fill:'var(--ops-muted)'}} tickLine={false} axisLine={false} fontSize={10}/>
              <Tooltip content={<ChartTip range={range}/>}/><Area type="linear" dataKey="in_service_percent" name="在役率" stroke="var(--ops-blue)" fill="var(--ops-blue)" fillOpacity={.1} baseValue={rateDomain(buckets)[0]} strokeWidth={2} dot={{r:2.5,fill:'var(--ops-blue)'}} activeDot={{r:4}} connectNulls={false} isAnimationActive={false}/>
            </AreaChart></ResponsiveContainer></div> : <p className="ops-chart-empty">暂无可计算的在役率</p>}
            <table className="ops-sr-only"><caption>在役率趋势数据</caption><thead><tr><th scope="col">时间</th><th scope="col">在役率</th><th scope="col">在役节点</th><th scope="col">节点总数</th><th scope="col">状态未知</th></tr></thead><tbody>{buckets.map(bucket=><tr key={bucket.start_at}><th scope="row">{timeLabel(bucket.start_at,range,true)}</th><td>{bucket.in_service_percent == null?'未知':`${bucket.in_service_percent.toFixed(1)}%`}</td><td>{bucket.in_service_nodes}</td><td>{bucket.total_nodes}</td><td>{bucket.unknown_nodes}</td></tr>)}</tbody></table>
            <p className="ops-chart-note">{last?.total_nodes ? `${number.format(last.in_service_nodes)} / ${number.format(last.total_nodes)} 个节点在役${last.unknown_nodes ? `，${number.format(last.unknown_nodes)} 个状态未知` : ''}` : '当前没有可统计的已授权节点'}</p>
          </div>
          <div className="ops-chart-panel"><div className="ops-chart-title"><h3>新增故障</h3><span className="ops-muted">{number.format(faultTotal)} 条</span></div>
            {faultTotal > 0 ? <div className="ops-chart" role="img" aria-label="按观测类型堆叠的新增故障趋势"><ResponsiveContainer width="100%" height="100%"><BarChart data={chartBuckets} margin={{top:10,right:4,bottom:0,left:-24}}>
              <CartesianGrid vertical={false} stroke="var(--ops-grid)"/><XAxis dataKey="start_at" tickFormatter={value=>timeLabel(Number(value),range)} tick={{fill:'var(--ops-muted)'}} tickLine={false} axisLine={false} minTickGap={22} fontSize={10}/><YAxis allowDecimals={false} tick={{fill:'var(--ops-muted)'}} tickLine={false} axisLine={false} fontSize={10}/><Tooltip content={<ChartTip range={range}/>}/>
              {categories.map((category,index)=><Bar key={category} dataKey={`fault_${category}`} name={faultNames[category] || category} stackId="fault" fill={faultColors[index%faultColors.length]} isAnimationActive={false}/>)}</BarChart></ResponsiveContainer></div> : <p className="ops-chart-empty">暂无可归因的故障记录</p>}
            <table className="ops-sr-only"><caption>新增故障趋势数据</caption><thead><tr><th scope="col">时间</th>{categories.map(category=><th scope="col" key={category}>{faultNames[category] || category}</th>)}</tr></thead><tbody>{buckets.map(bucket=><tr key={bucket.start_at}><th scope="row">{timeLabel(bucket.start_at,range,true)}</th>{categories.map(category=><td key={category}>{bucket.fault_counts[category] ?? '未知'}</td>)}</tr>)}</tbody></table>
            <div className="ops-legend">{categories.map((category,index)=><span key={category}><i style={{background:faultColors[index%faultColors.length]}}/>{faultNames[category] || category}</span>)}</div>
          </div>
        </div><p className="ops-footnote">时间按 UTC 统计；故障按观测类型分类，仅包含可关联到已授权节点的记录。历史状态不完整时在役率显示为未知。</p></>}
      </section>

      <section className="ops-card" aria-labelledby="ops-token-title">
        <header className="ops-card-heading"><h2 id="ops-token-title"><CircleHelp size={16}/>模型调用与 Token 用量统计 <span>（{rangeNames[range]}）</span></h2>
          {tokens.data && <span className="ops-badge">已报告 {tokenNumber(reportedTokens(tokens.data).total_tokens)} Tokens · {number.format(tokens.data.requests)} 次调用</span>}
        </header>
        <SectionState loading={tokens.loading} error={tokens.error} retry={retry}/>
        {tokens.data && <div className="ops-token-grid">
          <div className="ops-chart-panel"><div className="ops-chart-title"><h3>已报告 Token 消耗趋势</h3><span className="ops-muted">UTC 日</span></div>
            {tokenBuckets.some(bucket=>(bucket.input_tokens || 0)>0 || (bucket.output_tokens || 0)>0 || bucket.requests>(bucket.unreported_requests || 0)) ? <div className="ops-token-chart" role="img" aria-label="按日显示已报告的输入与输出 Token"><ResponsiveContainer width="100%" height="100%"><BarChart data={tokenBuckets} margin={{top:8,right:4,bottom:0,left:-25}}>
              <CartesianGrid vertical={false} stroke="var(--ops-grid)"/><XAxis dataKey="date" tickFormatter={value=>String(value).slice(5)} tick={{fill:'var(--ops-muted)'}} tickLine={false} axisLine={false} minTickGap={15} fontSize={10}/><YAxis tickFormatter={value=>compact.format(Number(value))} tick={{fill:'var(--ops-muted)'}} tickLine={false} axisLine={false} fontSize={10}/><Tooltip content={<ChartTip/>}/><Bar dataKey="input_tokens" name="输入" fill="#90bdfb" isAnimationActive={false}/><Bar dataKey="output_tokens" name="输出" fill="var(--ops-blue)" isAnimationActive={false}/>
            </BarChart></ResponsiveContainer></div> : <p className="ops-chart-empty">此时段暂无已报告的 Token 数值</p>}
            <table className="ops-sr-only"><caption>已报告 Token 消耗趋势数据</caption><thead><tr><th scope="col">UTC 日期</th><th scope="col">输入</th><th scope="col">输出</th><th scope="col">未完整报告的调用</th></tr></thead><tbody>{tokenBuckets.map(bucket=><tr key={bucket.date}><th scope="row">{bucket.date}</th><td>{bucket.input_tokens == null?'未知':number.format(bucket.input_tokens)}</td><td>{bucket.output_tokens == null?'未知':number.format(bucket.output_tokens)}</td><td>{bucket.unreported_requests ?? '未知'}</td></tr>)}</tbody></table>
            <div className="ops-legend"><span><i style={{background:'#90bdfb'}}/>输入</span><span><i style={{background:'var(--ops-blue)'}}/>输出</span></div>
          </div>
          <div className="ops-models"><h3>已报告用量的模型分布</h3>{models.length ? <>{(tokens.data.total_tokens || 0)>0 && models.every(model=>model.percent != null) && <div className="ops-model-bar" aria-hidden="true">{models.map((model,index)=><span key={model.model} style={{width:`${model.percent}%`,background:modelColors[index%modelColors.length]}}/>)}</div>}
            <ul>{models.map((model,index)=><li key={model.model}><span className="ops-model-name"><ModelMark brand={model.brand} color={modelColors[index%modelColors.length]}/><strong>{model.model}</strong> <b>{model.total_tokens == null || model.percent == null || !tokens.data?.total_tokens ? '占比未知' : `${model.percent}%`}</b></span><span>{tokenNumber(model.total_tokens)} <small>（输入 {tokenNumber(model.input_tokens)} / 输出 {tokenNumber(model.output_tokens)}{model.unreported_requests ? `；${number.format(model.unreported_requests)} 次未完整报告` : ''}）</small></span></li>)}</ul>
          </> : <p className="ops-state">此时段暂无模型用量</p>}</div>
        </div>}
        {tokens.data?.unreported_requests ? <p className="ops-footnote">{number.format(tokens.data.unreported_requests)} 次调用未完整报告用量，数值与占比仅包含已报告部分。</p> : null}
      </section>

      <section className="ops-card" aria-labelledby="ops-flow-title">
        <header className="ops-card-heading"><h2 id="ops-flow-title"><GitBranch size={16}/>故障处置流程</h2>
          {incidents.data && (incidents.data.length>0 || incidentID) && <label className="ops-incident-select"><span>事件</span><select value={incidentID} onChange={event=>{drawer.current?.close();setStepKey('');setShowLogs(false);setIncidentID(event.target.value)}}>{incidentID && !incidents.data.some(item=>item.incident_id===incidentID) && <option value={incidentID}>{incidentID}</option>}{incidents.data.map(item=><option key={item.incident_id} value={item.incident_id}>{item.incident_id} · {item.symptoms}</option>)}</select></label>}
        </header>
        <SectionState loading={incidents.loading} error={incidents.error} empty={incidents.data?.length===0 && !incidentID?'暂无故障事件':undefined} retry={retry}/>
        {incidentID && <SectionState loading={workflow.loading} error={workflow.error} retry={()=>setWorkflowReload(value=>value+1)}/>}
        {workflow.data && <><div className="ops-workflow" aria-label="选择阶段查看详情">{workflow.data.stages.map((stage,index)=><div className="ops-step-wrap" key={stage.step_key}>
          <button type="button" className={`ops-step ${stage.status}`} onClick={()=>openStage(stage.step_key)}><strong>{stage.label}</strong><span>{stage.summary || statusText(stage.status)}</span></button>{index<workflow.data!.stages.length-1 && <ChevronRight className="ops-step-arrow" size={14} aria-hidden="true"/>}
        </div>)}</div><p className="ops-footnote">点击阶段查看记录与日志。</p></>}
      </section>
    </div>
    <dialog className="ops-drawer" ref={drawer} aria-labelledby="ops-drawer-title" onClose={()=>{setStepKey('');setShowLogs(false)}}>
      {workflow.data && selectedStage && <><div className="ops-drawer-header"><div className="ops-drawer-title"><h2 id="ops-drawer-title">事件 {workflow.data.incident_id} · 流程详情</h2><span className="ops-badge blue">{selectedStage.label} · {statusText(selectedStage.status)}</span></div><form method="dialog"><button className="ops-close" aria-label="关闭流程详情"><X size={18}/></button></form></div>
        <div className="ops-drawer-body"><nav className="ops-stage-nav" aria-label="故障处置阶段">{workflow.data.stages.map(stage=><button type="button" key={stage.step_key} className={stage.step_key===stepKey?'selected':''} aria-current={stage.step_key===stepKey?'step':undefined} onClick={()=>changeStage(stage.step_key)}>{stage.label}</button>)}</nav>
          {selectedStage.policy_note && <div className="ops-detail-block"><h3>触发条件与策略</h3><p>{selectedStage.policy_note}</p></div>}
          {selectedStage.proposal_risk_note && <div className="ops-detail-block"><h3>提案风险边界</h3><p>{selectedStage.proposal_risk_note}</p></div>}
          <div className="ops-actions-heading"><h3>{selectedStage.label} · 阶段记录 ({selectedStage.actions.length})</h3><button type="button" aria-label={showLogs?'收起阶段日志':'查看阶段日志'} aria-expanded={showLogs} onClick={()=>setShowLogs(value=>!value)}><Terminal size={13}/>{showLogs?'收起日志':'日志'}</button></div>
          {selectedStage.actions.length ? <div className="ops-action-list">{selectedStage.actions.map(action=><div className="ops-action" key={action.action_id}><div><time>{actionTime(action.timestamp)}</time><span className={`ops-action-status ${action.status}`}>{statusText(action.status)}</span></div><p>{action.summary}</p></div>)}</div> : <p className="ops-state">此阶段暂无记录</p>}
          {showLogs && <div className="ops-terminal"><div className="ops-terminal-header"><span><i/>阶段日志{logs.data?.streaming?' · 阶段进行中':''}</span><button type="button" onClick={()=>setLogReload(value=>value+1)} aria-label="刷新阶段日志"><RefreshCw size={13}/> 刷新</button></div>
            <SectionState loading={logs.loading} error={logs.error} empty={logs.data?.lines.length===0?'暂无日志':undefined} retry={()=>setLogReload(value=>value+1)}/>
            {logs.data && logs.data.lines.length>0 && <pre>{logs.data.lines.join('\n')}</pre>}
            {logs.data?.source && <small>{logs.data.source}</small>}
          </div>}
          <div className="ops-detail-block"><h3>关联资产与当前影响</h3><p>{textValue(selectedStage.impact_note || workflow.data.impact_scope)}</p></div>
        </div><IncidentActions incident={workflow.data.incident_id} symptoms={workflow.data.symptoms} showLogs={showLogs} toggleLogs={()=>setShowLogs(value=>!value)} refresh={()=>setWorkflowReload(value=>value+1)}/></>}
    </dialog>
  </section>
}

// Assistant cards render only fields returned by the corresponding read tool.
const isRecord = (value:unknown):value is Record<string,unknown> => typeof value==='object' && value!==null && !Array.isArray(value)
const isNumber = (value:unknown):value is number => typeof value==='number' && Number.isFinite(value)
const isNullableNumber = (value:unknown) => value===null || isNumber(value)
const isRange = (value:unknown):value is Range => value==='24h' || value==='7d' || value==='30d'
const isTrends = (value:unknown):value is ClusterTrends => isRecord(value) && isRange(value.range) && Array.isArray(value.fault_categories) && value.fault_categories.every(item=>typeof item==='string') && Array.isArray(value.buckets) && value.buckets.every(item=>isRecord(item) && isNumber(item.start_at) && isNumber(item.total_nodes) && isNumber(item.in_service_nodes) && isNumber(item.unknown_nodes) && isNullableNumber(item.in_service_percent) && isRecord(item.fault_counts) && Object.values(item.fault_counts).every(isNumber))
const isTokens = (value:unknown):value is TokenStats => isRecord(value) && isRange(value.range) && isNullableNumber(value.total_tokens) && isNumber(value.requests) && (value.unreported_requests===undefined || isNumber(value.unreported_requests)) && Array.isArray(value.daily_buckets) && value.daily_buckets.every(item=>isRecord(item) && typeof item.date==='string' && isNullableNumber(item.input_tokens) && isNullableNumber(item.output_tokens) && (item.total_tokens===undefined || isNullableNumber(item.total_tokens))) && Array.isArray(value.by_model) && value.by_model.every(item=>isRecord(item) && typeof item.model==='string' && isNullableNumber(item.total_tokens) && isNullableNumber(item.input_tokens) && isNullableNumber(item.output_tokens) && isNullableNumber(item.percent))
const isWorkflow = (value:unknown):value is Workflow => isRecord(value) && typeof value.incident_id==='string' && /^(?:[a-f0-9]{32}|inc-[0-9]{1,16})$/.test(value.incident_id) && typeof value.impact_scope==='string' && Array.isArray(value.stages) && value.stages.every(stage=>isRecord(stage) && typeof stage.step_key==='string' && /^[a-z0-9_-]{1,80}$/i.test(stage.step_key) && typeof stage.label==='string' && typeof stage.status==='string' && typeof stage.summary==='string' && (stage.policy_note===undefined || typeof stage.policy_note==='string') && (stage.impact_note===undefined || typeof stage.impact_note==='string') && (stage.proposal_risk_note===undefined || typeof stage.proposal_risk_note==='string') && Array.isArray(stage.actions) && stage.actions.every(action=>isRecord(action) && typeof action.action_id==='string' && isNumber(action.timestamp) && typeof action.summary==='string' && typeof action.status==='string' && (action.log_preview===undefined || typeof action.log_preview==='string')))
const isStepLogs = (value:unknown):value is StepLogs => isRecord(value) && typeof value.incident_id==='string' && typeof value.step_key==='string' && typeof value.source==='string' && typeof value.streaming==='boolean' && Array.isArray(value.lines) && value.lines.every(line=>typeof line==='string')

export function supportsOperationsAssistantCard(tool:string,result:unknown) {
  return (tool==='get_cluster_inventory' && isClusterInventoryPage(result)) || (tool==='get_cluster_trends' && isTrends(result)) ||
    (tool==='get_token_usage_stats' && isTokens(result)) || (tool==='get_incident_workflow' && isWorkflow(result)) ||
    (tool==='get_incident_step_logs' && isStepLogs(result))
}

export function OperationsAssistantCard({tool,result}:{tool:string;result:unknown;navigate?:(path:string)=>void}) {
  if(tool==='get_cluster_inventory' && isClusterInventoryPage(result))return <ClusterInventoryCard data={result}/>
  if(tool==='get_cluster_trends' && isTrends(result))return <TrendsAssistantCard data={result}/>
  if(tool==='get_token_usage_stats' && isTokens(result))return <TokensAssistantCard data={result}/>
  if(tool==='get_incident_workflow' && isWorkflow(result))return <WorkflowAssistantCard data={result}/>
  if(tool==='get_incident_step_logs' && isStepLogs(result))return <section className="ops-assistant-card ops-assistant-logs"><header className="ops-card-heading"><h2><Terminal size={15}/>阶段日志</h2></header><LogOutput data={result}/></section>
  return null
}

function csvCell(value:string|number|null|undefined) {
  if(value==null)return '""'
  let text=String(value)
  // CSV quoting does not stop spreadsheets from interpreting text as a formula.
  if(typeof value==='string' && (/^[\u0000-\u0020\u007f-\u009f]*[=+\-@＝＋－＠]/u.test(text) || /^[\t\r\n]/.test(text)))text=`'${text}`
  return `"${text.replace(/"/g,'""')}"`
}

function exportTrends(data:ClusterTrends) {
  const timestamp=(value:unknown)=>{
    if(!isNumber(value))return ''
    const date=new Date(value)
    return Number.isFinite(date.getTime()) ? date.toISOString() : ''
  }
  const categories=data.fault_categories
  const rows:(string|number|null|undefined)[][]=[
    ['统计范围','时间段开始 (UTC)','时间段结束 (UTC)','节点总数','在役节点','状态未知节点','在役率 (%)',
      ...categories.map(category=>`新增故障：${Object.hasOwn(faultNames,category)?faultNames[category]:category}`)],
    ...data.buckets.map(bucket=>[data.range,timestamp(bucket.start_at),timestamp(bucket.end_at),bucket.total_nodes,bucket.in_service_nodes,
      bucket.unknown_nodes,bucket.in_service_percent,...categories.map(category=>isNumber(bucket.fault_counts[category])?bucket.fault_counts[category]:null)]),
  ]
  const url=URL.createObjectURL(new Blob(['\ufeff',rows.map(row=>row.map(csvCell).join(',')).join('\r\n'),'\r\n'],{type:'text/csv;charset=utf-8'}))
  const link=document.createElement('a')
  link.href=url;link.download=`集群趋势-${data.range}.csv`;link.click()
  window.setTimeout(()=>URL.revokeObjectURL(url),1000)
}

function TrendsAssistantCard({data:initialData}:{data:ClusterTrends}) {
  const [data,setData]=useState(initialData)
  const [pending,setPending]=useState(false)
  const [error,setError]=useState('')
  async function changeRange(range:Range) {
    if(pending || range===data.range)return
    setPending(true);setError('')
    try {const result=await api<unknown>(`/domain/cluster-trends?range=${range}`);if(!isTrends(result))throw new Error('趋势数据格式无效，请重试');setData(result)}
    catch(e){setError(errorMessage(e))}finally{setPending(false)}
  }
  const last=data.buckets.at(-1)
  const buckets=data.buckets.map(bucket=>({...bucket,fault_total:Object.values(bucket.fault_counts).reduce((sum,value)=>sum+value,0),...Object.fromEntries(Object.entries(bucket.fault_counts).map(([key,value])=>[`fault_${key}`,value]))}))
  const hasRate=data.buckets.some(bucket=>bucket.in_service_percent!=null)
  const faults=data.buckets.reduce((sum,bucket)=>sum+Object.values(bucket.fault_counts).reduce((n,value)=>n+value,0),0)
  return <section className="ops-assistant-card" aria-label="集群在役率与故障趋势">
    <header className="ops-card-heading"><h2 title="UTC 时间；仅包含可关联到已授权节点的故障记录。历史状态不完整时在役率显示为未知。"><TrendingUp size={15}/>集群在役率与故障趋势 <span>（{rangeNames[data.range]}{last ? ` · ${number.format(last.total_nodes)} 个节点` : ''}）</span></h2><div className="ops-assistant-header-actions"><div className="ops-range" aria-label="选择统计时间范围">{ranges.map(range=><button type="button" key={range} disabled={pending} className={data.range===range?'selected':''} aria-pressed={data.range===range} onClick={()=>void changeRange(range)}>{range}</button>)}</div></div></header>
    <div className="ops-trend-grid">
      <div className="ops-chart-panel"><div className="ops-chart-title"><h3>在役率走势 (%)</h3><span className="ops-badge green">当前 {last?.in_service_percent==null?'未知':`${last.in_service_percent.toFixed(1)}%`}</span></div>
        {hasRate ? <div className="ops-chart"><ResponsiveContainer width="100%" height="100%"><AreaChart data={buckets} margin={{top:8,right:5,bottom:0,left:-18}}><CartesianGrid vertical={false} stroke="var(--ops-grid)"/><XAxis dataKey="start_at" tickFormatter={value=>timeLabel(Number(value),data.range)} tick={{fill:'var(--ops-muted)'}} tickLine={false} axisLine={false} minTickGap={20} interval={data.range==='7d'&&data.buckets.length<=7?0:'preserveEnd'} fontSize={9}/><YAxis domain={rateDomain(data.buckets)} tickCount={3} tickFormatter={value=>`${Number(value).toFixed(1)}%`} tick={{fill:'var(--ops-muted)'}} tickLine={false} axisLine={false} fontSize={9}/><Tooltip content={<ChartTip range={data.range}/>}/><Area type="linear" dataKey="in_service_percent" name="在役率" stroke="var(--ops-blue)" fill="var(--ops-blue)" fillOpacity={.1} baseValue={rateDomain(data.buckets)[0]} strokeWidth={2} dot={{r:2,fill:'var(--ops-blue)'}} activeDot={{r:4}} connectNulls={false} isAnimationActive={false}/></AreaChart></ResponsiveContainer></div> : <p className="ops-chart-empty">暂无可计算的在役率</p>}
      </div>
      <div className="ops-chart-panel"><div className="ops-chart-title"><h3>{data.range==='24h'?'每小时新增故障':'每日新增故障'}</h3><div className="ops-legend">{data.fault_categories.map((category,index)=>data.buckets.some(bucket=>(bucket.fault_counts[category]||0)>0)&&<span key={category}><i style={{background:faultColors[index%faultColors.length]}}/>{faultNames[category]||category}</span>)}</div></div>
        {faults ? <div className="ops-chart"><ResponsiveContainer width="100%" height="100%"><BarChart data={buckets} margin={{top:8,right:5,bottom:0,left:-28}}><CartesianGrid vertical={false} stroke="var(--ops-grid)"/><XAxis dataKey="start_at" tickFormatter={value=>timeLabel(Number(value),data.range)} tick={{fill:'var(--ops-muted)'}} tickLine={false} axisLine={false} minTickGap={20} interval={data.range==='7d'&&data.buckets.length<=7?0:'preserveEnd'} fontSize={9}/><YAxis allowDecimals={false} tick={{fill:'var(--ops-muted)'}} tickLine={false} axisLine={false} fontSize={9}/><Tooltip content={<ChartTip range={data.range}/>}/>{data.fault_categories.map((category,index)=><Bar key={category} dataKey={`fault_${category}`} name={faultNames[category]||category} stackId="fault" fill={faultColors[index%faultColors.length]} isAnimationActive={false}>{data.range==='7d'&&data.buckets.length<=7&&<LabelList valueAccessor={entry=>{const bucket=entry.payload as typeof buckets[number];return (bucket.fault_counts[category]||0)>0&&data.fault_categories.slice(index+1).every(next=>(bucket.fault_counts[next]||0)===0)?bucket.fault_total:undefined}} position="top" fill="var(--ops-muted)" fontSize={9}/>}</Bar>)}</BarChart></ResponsiveContainer></div> : <p className="ops-chart-empty">暂无可归因的故障记录</p>}
      </div>
    </div>
    <p className="ops-sr-only">UTC 时间；仅包含可关联到已授权节点的故障记录。历史状态不完整时在役率显示为未知。</p>
    <table className="ops-sr-only"><caption>集群趋势数据</caption><thead><tr><th scope="col">时间</th><th scope="col">在役率</th><th scope="col">新增故障</th></tr></thead><tbody>{data.buckets.map(bucket=><tr key={bucket.start_at}><th scope="row">{timeLabel(bucket.start_at,data.range,true)}</th><td>{bucket.in_service_percent==null?'未知':`${bucket.in_service_percent.toFixed(1)}%`}</td><td>{number.format(Object.values(bucket.fault_counts).reduce((sum,value)=>sum+value,0))}</td></tr>)}</tbody></table>
    {error&&<p className="ops-inline-error" role="alert">{error}</p>}
    <footer className="ops-trend-actions"><button type="button" className="outline ops-trend-export" disabled={pending} onClick={()=>exportTrends(data)}><ArrowDownToLine size={13} aria-hidden="true"/>导出 CSV</button></footer>
  </section>
}

function TokensAssistantCard({data:stats}:{data:TokenStats}) {
  const data=reportedTokens(stats)
  const models=data.by_model.map(reportedTokens)
  const dailyTotals=data.daily_buckets.map(reportedTokens).map(bucket=>({...bucket,chart_total:bucket.total_tokens ?? (bucket.input_tokens!=null && bucket.output_tokens!=null ? bucket.input_tokens+bucket.output_tokens : null)}))
  const hasUsage=dailyTotals.some(bucket=>bucket.chart_total!=null)
  const peak=Math.max(0,...dailyTotals.map(bucket=>bucket.chart_total??0))
  return <section className="ops-assistant-card" aria-label="模型调用与 Token 用量统计">
    <header className="ops-card-heading"><h2><CircleHelp size={15}/>模型调用与 Token 用量统计 <span>（{rangeNames[data.range]}）</span></h2><div className="ops-assistant-header-actions"><span className="ops-badge">{tokenNumber(data.total_tokens)} Tokens · {number.format(data.requests)} 次调用</span></div></header>
    <div className="ops-token-grid"><div className="ops-chart-panel"><div className="ops-chart-title"><h3>{rangeNames[data.range]}消耗趋势</h3>{hasUsage&&<span className="ops-token-peak">峰值 {tokenNumber(peak)}</span>}</div>
      {hasUsage ? <div className="ops-token-chart"><ResponsiveContainer width="100%" height="100%"><BarChart data={dailyTotals} margin={{top:5,right:3,bottom:0,left:3}}><XAxis dataKey="date" tickFormatter={value=>String(value).slice(8)} tick={{fill:'var(--ops-muted)'}} tickLine={false} axisLine={false} minTickGap={12} height={12} fontSize={9}/><Tooltip content={<ChartTip/>}/><Bar dataKey="chart_total" name="总量" isAnimationActive={false}>{dailyTotals.map(bucket=><Cell key={bucket.date} fill={bucket.chart_total===peak&&peak>0?'var(--ops-blue)':'#90bdfb'}/>)}</Bar></BarChart></ResponsiveContainer></div> : <p className="ops-chart-empty">暂无已报告的 Token 数值</p>}
    </div><div className="ops-models">{models.length ? <>{(data.total_tokens||0)>0 && models.every(model=>model.percent!=null) && <div className="ops-model-bar" aria-hidden="true">{models.map((model,index)=><span key={model.model} style={{width:`${model.percent}%`,background:modelColors[index%modelColors.length]}}/>)}</div>}<ul tabIndex={0} aria-label="模型用量明细">{models.map((model,index)=><li key={model.model}><span className="ops-model-name"><ModelMark brand={model.brand} color={modelColors[index%modelColors.length]}/><strong>{model.model}</strong> <b>{model.total_tokens==null || model.percent==null || !data.total_tokens?'占比未知':`${model.percent}%`}</b></span><span>{tokenNumber(model.total_tokens)} <small>（输入 {tokenNumber(model.input_tokens)} / 输出 {tokenNumber(model.output_tokens)}）</small></span></li>)}</ul></> : <p className="ops-state">暂无模型用量</p>}</div></div>
    {data.unreported_requests ? <p className="ops-footnote">{number.format(data.unreported_requests)} 次调用未完整报告用量，统计仅包含已报告部分。</p> : null}
    <table className="ops-sr-only"><caption>已报告 Token 消耗趋势</caption><thead><tr><th scope="col">UTC 日期</th><th scope="col">输入</th><th scope="col">输出</th></tr></thead><tbody>{dailyTotals.map(bucket=><tr key={bucket.date}><th scope="row">{bucket.date}</th><td>{tokenNumber(bucket.input_tokens)}</td><td>{tokenNumber(bucket.output_tokens)}</td></tr>)}</tbody></table>
  </section>
}

function WorkflowAssistantCard({data:initialData}:{data:Workflow}) {
  const [data,setData]=useState(initialData)
  const [stepKey,setStepKey]=useState('')
  const [showLogs,setShowLogs]=useState(false)
  const [previewAction,setPreviewAction]=useState('')
  const [logs,setLogs]=useState<Loadable<StepLogs>>({loading:false})
  const [logReload,setLogReload]=useState(0)
  const selected=data.stages.find(stage=>stage.step_key===stepKey)
  useEffect(()=>{
    if(!selected || !showLogs){setLogs({loading:false});return}
    let alive=true;setLogs({loading:true})
    api<unknown>(`/incidents/${encodeURIComponent(data.incident_id)}/steps/${encodeURIComponent(selected.step_key)}/logs`).then(result=>{if(alive)setLogs(isStepLogs(result)?{loading:false,data:result}:{loading:false,error:'日志数据格式无效'})}).catch(error=>{if(alive)setLogs({loading:false,error:errorMessage(error)})})
    return()=>{alive=false}
  },[data.incident_id,selected?.step_key,showLogs,logReload])
  useEffect(()=>{
    if(!stepKey)return
    const close=(event:KeyboardEvent)=>{if(event.key==='Escape' && !document.querySelector('dialog[open]'))setStepKey('')}
    document.addEventListener('keydown',close);return()=>document.removeEventListener('keydown',close)
  },[stepKey])
  function selectStep(key:string){setShowLogs(false);setPreviewAction('');setStepKey(key)}
  function refreshWorkflow(){api<unknown>(`/incidents/${encodeURIComponent(data.incident_id)}/workflow`).then(value=>{if(isWorkflow(value))setData(value)}).catch(()=>{})}
  return <section className="ops-assistant-card" aria-label="故障处置流程">
    <header className="ops-card-heading"><h2><GitBranch size={15}/>故障处置流程 <span>（{data.incident_id} · 可点击节点查看）</span></h2>{selected&&<span className="ops-badge blue">选中：{selected.label}</span>}</header>
    {data.stages.length ? <div className="ops-workflow" aria-label="选择阶段查看详情">{data.stages.map((stage,index)=>{const Icon=stage.status==='completed'?CircleCheck:stage.status==='active'?CircleDot:Clock3;return <div className="ops-step-wrap" key={stage.step_key}><button type="button" className={`ops-step ${stage.status}`} aria-pressed={stepKey===stage.step_key} onClick={()=>selectStep(stage.step_key)}><strong><Icon size={12} aria-hidden="true"/>{stage.label}</strong><span>{stage.summary || statusText(stage.status)}</span></button>{index<data.stages.length-1&&<ChevronRight className="ops-step-arrow" size={14} aria-hidden="true"/>}</div>})}</div> : <p className="ops-state">暂无处置阶段</p>}
    {selected&&<footer className="ops-assistant-footer"><span className="ops-selection-note">已选节点：{selected.label}{selected.summary?`（${selected.summary}）`:''} · 点击节点切换右侧阶段详情</span><div><button type="button" onClick={()=>setStepKey('')}>收起详情</button></div></footer>}
    {selected&&<aside className="ops-assistant-dock" aria-label={`事件 ${data.incident_id} 的流程详情`}>
      <div className="ops-drawer-header"><div className="ops-drawer-heading"><div className="ops-drawer-title"><h2>事件 {data.incident_id} · 流程详情</h2><span className="ops-badge blue">{selected.label} · {statusText(selected.status)}</span></div><button type="button" className="ops-close" aria-label="关闭流程详情" onClick={()=>setStepKey('')}><X size={18}/></button></div><nav className="ops-stage-nav" aria-label="故障处置阶段">{data.stages.map(stage=><button type="button" key={stage.step_key} className={stage.step_key===stepKey?'selected':''} aria-current={stage.step_key===stepKey?'step':undefined} onClick={()=>selectStep(stage.step_key)}>{stage.label}</button>)}</nav></div>
      <div className="ops-drawer-body">
        {selected.policy_note&&<div className="ops-detail-block"><h3>触发条件与策略</h3><p>{selected.policy_note}</p></div>}
        {selected.proposal_risk_note && <div className="ops-detail-block"><h3>提案风险边界</h3><p>{selected.proposal_risk_note}</p></div>}
        <div className="ops-actions-heading"><h3>{selected.label} · 阶段记录 ({selected.actions.length})</h3></div>
        {selected.actions.length ? <div className="ops-action-list">{selected.actions.map(action=><div className={`ops-action ${previewAction===action.action_id?'expanded':''}`} key={action.action_id}><div className="ops-action-header"><time>{actionTime(action.timestamp)}</time><span className={`ops-action-status ${action.status}`}>{statusText(action.status)}</span>{action.log_preview&&<button type="button" aria-expanded={previewAction===action.action_id} onClick={()=>setPreviewAction(value=>value===action.action_id?'':action.action_id)}>{previewAction===action.action_id?'收起日志':'日志'}</button>}</div><p>{action.summary}</p>{previewAction===action.action_id&&action.log_preview&&<div className="ops-terminal ops-action-preview"><div className="ops-terminal-header"><span><i/>日志预览</span></div><pre>{action.log_preview}</pre></div>}</div>)}</div> : <p className="ops-state">此阶段暂无记录</p>}
        {showLogs&&<div className="ops-terminal"><div className="ops-terminal-header"><span><i/>阶段日志{logs.data?.streaming?' · 阶段进行中':''}</span><button type="button" onClick={()=>setLogReload(value=>value+1)}><RefreshCw size={13}/>刷新</button></div><SectionState loading={logs.loading} error={logs.error} retry={()=>setLogReload(value=>value+1)}/>{logs.data&&<LogOutput data={logs.data}/>}</div>}
        <div className="ops-detail-block"><h3>关联资产与当前影响</h3><p>{textValue(selected.impact_note || data.impact_scope)}</p></div>
      </div>
      <IncidentActions incident={data.incident_id} symptoms={data.symptoms} showLogs={showLogs} toggleLogs={()=>setShowLogs(value=>!value)} refresh={refreshWorkflow}/>
    </aside>}
  </section>
}

function LogOutput({data}:{data:StepLogs}) {
  return <div className="ops-log-output">{data.lines.length ? <pre>{data.lines.join('\n')}</pre> : <p className="ops-state">暂无日志</p>}{data.source&&<small>{data.source}</small>}</div>
}
