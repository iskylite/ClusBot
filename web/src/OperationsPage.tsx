import { useEffect, useMemo, useRef, useState } from 'react'
import { Activity, ChevronRight, CircleHelp, GitBranch, RefreshCw, Terminal, TrendingUp, X } from 'lucide-react'
import { Bar, BarChart, CartesianGrid, Line, LineChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts'
import { api } from './api'
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
type InventoryNode = {object_ref:string; identity:string; state:string; updated_at:number}
type InventoryPage = {items:InventoryNode[]; next_cursor?:string}
type TokenBucket = {date:string; input_tokens:number|null; output_tokens:number|null; total_tokens:number|null; requests:number; unreported_requests:number}
type TokenModel = {model:string; input_tokens:number|null; output_tokens:number|null; total_tokens:number|null; percent:number|null; requests:number; unreported_requests:number}
type TokenStats = {range:Range; total_tokens:number|null; input_tokens:number|null; output_tokens:number|null; requests:number; unreported_requests?:number; daily_buckets:TokenBucket[]; by_model:TokenModel[]}
type Incident = {incident_id:string; symptoms:string; state:string; updated_at:number}
type IncidentAction = {action_id:string; timestamp:number; summary:string; status:string; has_logs:boolean}
type Stage = {step_key:string; label:string; status:string; summary:string; policy_note?:string; impact_note?:string; actions:IncidentAction[]}
type Workflow = {incident_id:string; state:string; symptoms:string; impact_scope:string; active_step:string; stages:Stage[]}
type StepLogs = {incident_id:string; step_key:string; source:string; streaming:boolean; lines:string[]}

const ranges:Range[] = ['24h','7d','30d']
const rangeNames:Record<Range,string> = {'24h':'近 24 小时','7d':'近 7 日','30d':'近 30 日'}
const faultNames:Record<string,string> = {nhc:'节点健康检查',oob_alert:'带外告警',network:'网络',service:'服务',other:'其他'}
const faultColors = ['#ef4444','#f59e0b','#10b981','#3b82f6','#818cf8']
const modelColors = ['#4f6df5','#6257df','#1f98d1','#31a783','#e19b36','#9e70b4']
const number = new Intl.NumberFormat('zh-CN')
const compact = new Intl.NumberFormat('zh-CN',{notation:'compact',maximumFractionDigits:1})
const errorMessage = (error:unknown) => error instanceof Error ? error.message : '请求未完成，请稍后重试'
const tokenNumber = (value:number|null|undefined) => value == null ? '未知' : compact.format(value)

function timeLabel(value:number, range:Range, full=false) {
  const date = new Date(value)
  if (!Number.isFinite(date.getTime())) return '时间未知'
  const month = date.getUTCMonth()+1
  const day = date.getUTCDate()
  const hour = String(date.getUTCHours()).padStart(2,'0')
  if (range === '24h') return full ? `${month}月${day}日 ${hour}:00 UTC` : `${hour}:00`
  return full ? `${date.getUTCFullYear()}年${month}月${day}日 UTC` : `${month}/${day}`
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
    open:'待处理',investigating:'调查中',mitigated:'已缓解',resolved:'已恢复',closed:'已关闭'} as Record<string,string>)[status] || status || '未知'
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

export function OperationsPage({initialIncident,initialRange}:{isAdmin:boolean;initialIncident?:string;initialRange?:string}) {
  const [range,setRange] = useState<Range>(ranges.includes(initialRange as Range) ? initialRange as Range : '7d')
  const [reload,setReload] = useState(0)
  const [trend,setTrend] = useState<Loadable<ClusterTrends>>({loading:true})
  const [tokens,setTokens] = useState<Loadable<TokenStats>>({loading:true})
  const [inventory,setInventory] = useState<Loadable<InventoryPage>>({loading:true})
  const [inventoryCursor,setInventoryCursor] = useState('')
  const [inventoryHistory,setInventoryHistory] = useState<string[]>([])
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
    const query = new URLSearchParams({limit:'24'})
    if(inventoryCursor) query.set('after',inventoryCursor)
    setInventory({loading:true})
    api<InventoryPage>(`/domain/cluster-inventory?${query}`).then(data=>{if(current)setInventory({loading:false,data})}).catch(error=>{if(current)setInventory({loading:false,error:errorMessage(error)})})
    return ()=>{current=false}
  },[inventoryCursor,reload])
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
    setWorkflow({loading:true})
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
  const tokenBuckets = tokens.data?.daily_buckets || []
  const models = tokens.data?.by_model || []
  const selectedStage = workflow.data?.stages.find(stage=>stage.step_key===stepKey)
  function openStage(key:string){setShowLogs(false);setStepKey(key)}
  function changeStage(key:string){setShowLogs(false);setStepKey(key)}
  function retry(){setReload(value=>value+1)}

  return <section className="operations-page" aria-label="趋势分析与故障流程">
    <div className="ops-heading"><div><h1>趋势分析与故障流程</h1><p>查看已授权节点的状态、模型用量和故障处置进展</p></div></div>
    <div className="ops-cards">
      <section className="ops-card" aria-labelledby="ops-inventory-title">
        <header className="ops-card-heading"><h2 id="ops-inventory-title"><Activity size={16}/>集群节点状态</h2>
          {last && <span className="ops-badge">已授权 {number.format(last.total_nodes)} 个节点</span>}
        </header>
        <SectionState loading={inventory.loading} error={inventory.error} empty={inventory.data?.items.length===0?'暂无已授权节点':undefined} retry={retry}/>
        {inventory.data && inventory.data.items.length>0 && <><div className="ops-node-grid">{inventory.data.items.map(node=><div className={`ops-node ${node.state==='在役'?'in-service':node.state==='维护中'?'maintenance':''}`} key={node.object_ref} title={node.object_ref}>
          <strong>{node.identity || node.object_ref}</strong><span>{node.state || '状态未知'}</span>
        </div>)}</div><div className="ops-node-footer"><span>显示当前页节点的实际状态</span><div>
          <button type="button" disabled={!inventoryHistory.length} onClick={()=>{setInventoryCursor(inventoryHistory.at(-1) || '');setInventoryHistory(history=>history.slice(0,-1))}}>上一页</button>
          <button type="button" disabled={!inventory.data.next_cursor} onClick={()=>{setInventoryHistory(history=>[...history,inventoryCursor]);setInventoryCursor(inventory.data?.next_cursor || '')}}>下一页</button>
        </div></div></>}
      </section>
      <section className="ops-card" aria-labelledby="ops-trend-title">
        <header className="ops-card-heading"><h2 id="ops-trend-title"><TrendingUp size={16}/>集群在役率与故障趋势 <span>（{rangeNames[range]}）</span></h2>
          <div className="ops-range" aria-label="选择统计时间范围">{ranges.map(value=><button type="button" key={value} className={range===value?'selected':''} aria-pressed={range===value} onClick={()=>setRange(value)}>{value}</button>)}</div>
        </header>
        <SectionState loading={trend.loading} error={trend.error} retry={retry}/>
        {trend.data && <><div className="ops-trend-grid">
          <div className="ops-chart-panel"><div className="ops-chart-title"><h3>在役率趋势 (%)</h3><span className="ops-badge green">当前 {last?.in_service_percent == null ? '未知' : `${last.in_service_percent.toFixed(1)}%`}</span></div>
            {hasRate ? <div className="ops-chart" role="img" aria-label="已授权节点的在役率趋势，缺失状态显示为空缺"><ResponsiveContainer width="100%" height="100%"><LineChart data={chartBuckets} margin={{top:10,right:10,bottom:0,left:-24}}>
              <CartesianGrid vertical={false} stroke="var(--ops-grid)"/><XAxis dataKey="start_at" tickFormatter={value=>timeLabel(Number(value),range)} tick={{fill:'var(--ops-muted)'}} tickLine={false} axisLine={false} minTickGap={22} fontSize={10}/><YAxis domain={[0,100]} tickFormatter={value=>`${value}%`} tick={{fill:'var(--ops-muted)'}} tickLine={false} axisLine={false} fontSize={10}/>
              <Tooltip content={<ChartTip range={range}/>}/><Line type="linear" dataKey="in_service_percent" name="在役率" stroke="var(--ops-blue)" strokeWidth={2} dot={{r:2.5,fill:'var(--ops-blue)'}} activeDot={{r:4}} connectNulls={false} isAnimationActive={false}/>
            </LineChart></ResponsiveContainer></div> : <p className="ops-chart-empty">暂无可计算的在役率</p>}
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
          {tokens.data && <span className="ops-badge">已报告 {tokenNumber(tokens.data.total_tokens)} Tokens · {number.format(tokens.data.requests)} 次调用</span>}
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
            <ul>{models.map((model,index)=><li key={model.model}><span className="ops-model-name"><i style={{background:modelColors[index%modelColors.length]}}/><strong>{model.model}</strong> <b>{model.percent == null || !tokens.data?.total_tokens ? '占比未知' : `${model.percent}%`}</b></span><span>{tokenNumber(model.total_tokens)} <small>（输入 {tokenNumber(model.input_tokens)} / 输出 {tokenNumber(model.output_tokens)}{model.unreported_requests ? `；${number.format(model.unreported_requests)} 次未完整报告` : ''}）</small></span></li>)}</ul>
          </> : <p className="ops-state">此时段暂无模型用量</p>}</div>
        </div>}
        {tokens.data?.unreported_requests ? <p className="ops-footnote">{number.format(tokens.data.unreported_requests)} 次调用未完整报告用量，数值与占比仅包含已报告部分。</p> : null}
      </section>

      <section className="ops-card" aria-labelledby="ops-flow-title">
        <header className="ops-card-heading"><h2 id="ops-flow-title"><GitBranch size={16}/>故障处置流程</h2>
          {incidents.data && (incidents.data.length>0 || incidentID) && <label className="ops-incident-select"><span>事件</span><select value={incidentID} onChange={event=>setIncidentID(event.target.value)}>{incidentID && !incidents.data.some(item=>item.incident_id===incidentID) && <option value={incidentID}>{incidentID}</option>}{incidents.data.map(item=><option key={item.incident_id} value={item.incident_id}>{item.incident_id} · {item.symptoms}</option>)}</select></label>}
        </header>
        <SectionState loading={incidents.loading} error={incidents.error} empty={incidents.data?.length===0 && !incidentID?'暂无故障事件':undefined} retry={retry}/>
        {incidentID && <SectionState loading={workflow.loading} error={workflow.error} retry={()=>setWorkflowReload(value=>value+1)}/>}
        {workflow.data && <><div className="ops-workflow" aria-label="选择阶段查看详情">{workflow.data.stages.map((stage,index)=><div className="ops-step-wrap" key={stage.step_key}>
          <button type="button" className={`ops-step ${stage.status}`} onClick={()=>openStage(stage.step_key)}><strong>{stage.label}</strong><span>{stage.summary || statusText(stage.status)}</span></button>{index<workflow.data!.stages.length-1 && <ChevronRight className="ops-step-arrow" size={14} aria-hidden="true"/>}
        </div>)}</div><p className="ops-footnote">点击阶段查看执行动作与阶段日志。</p></>}
      </section>
    </div>
    <dialog className="ops-drawer" ref={drawer} aria-labelledby="ops-drawer-title" onClose={()=>{setStepKey('');setShowLogs(false)}}>
      {workflow.data && selectedStage && <><div className="ops-drawer-header"><div><h2 id="ops-drawer-title">事件 {workflow.data.incident_id} · 流程详情</h2><span className="ops-badge blue">{selectedStage.label} · {statusText(selectedStage.status)}</span></div><form method="dialog"><button className="ops-close" aria-label="关闭流程详情"><X size={18}/></button></form></div>
        <div className="ops-drawer-body"><nav className="ops-stage-nav" aria-label="故障处置阶段">{workflow.data.stages.map(stage=><button type="button" key={stage.step_key} className={stage.step_key===stepKey?'selected':''} aria-current={stage.step_key===stepKey?'step':undefined} onClick={()=>changeStage(stage.step_key)}>{stage.label}</button>)}</nav>
          {selectedStage.policy_note && <div className="ops-detail-block"><h3>触发条件与策略</h3><p>{selectedStage.policy_note}</p></div>}
          <div className="ops-detail-block"><h3>阶段状态</h3><p>{selectedStage.summary || statusText(selectedStage.status)}</p></div>
          <div className="ops-actions-heading"><h3>{selectedStage.label} · 执行动作 ({selectedStage.actions.length})</h3><button type="button" onClick={()=>setShowLogs(value=>!value)}><Terminal size={13}/>{showLogs?'收起日志':'查看阶段日志'}</button></div>
          {selectedStage.actions.length ? <div className="ops-action-list">{selectedStage.actions.map(action=><div className="ops-action" key={action.action_id}><div><time>{actionTime(action.timestamp)}</time><span className={`ops-action-status ${action.status}`}>{statusText(action.status)}</span></div><p>{action.summary}</p></div>)}</div> : <p className="ops-state">此阶段暂无执行动作</p>}
          {showLogs && <div className="ops-terminal"><div className="ops-terminal-header"><span><i/>阶段日志{logs.data?.streaming?' · 阶段进行中':''}</span><button type="button" onClick={()=>setLogReload(value=>value+1)} aria-label="刷新阶段日志"><RefreshCw size={13}/> 刷新</button></div>
            <SectionState loading={logs.loading} error={logs.error} empty={logs.data?.lines.length===0?'暂无日志':undefined} retry={()=>setLogReload(value=>value+1)}/>
            {logs.data && logs.data.lines.length>0 && <pre>{logs.data.lines.join('\n')}</pre>}
            {logs.data?.source && <small>{logs.data.source}</small>}
          </div>}
          <div className="ops-detail-block"><h3>关联资产与当前影响</h3><p>{textValue(selectedStage.impact_note || workflow.data.impact_scope)}</p></div>
        </div></>}
    </dialog>
  </section>
}
