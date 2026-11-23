import {useEffect,useId,useRef,useState,type FormEvent} from 'react'
import {CalendarCheck,Info,Server} from 'lucide-react'
import {api,APIError} from './api'
import {displayDate} from './GeneralPreferences'
import './assistant-cards.css'
import './DiscoveryPanel.css'

export type DiscoveryStatus = {
  state:string
  plan?:{id:string;source:string;scope:string;next_at_unix_ms:number}
  last_run?:{status:string;scheduled_at_unix_ms:number;observation_count?:number}
}
type Candidate={id:string;source:string;scope:string;serial?:string;address:string;seen_at:number;state:string;asset?:string}
type Grant={id:string;source:string;scope:string;expiry:number}
type PendingPlan={id:string;source:string;scope:string;period_seconds:number;first_at_unix_ms:number}
const pendingEvent='clusbot:discovery-plan-pending'

function readPendingPlan(key:string):PendingPlan|null {
  try{
    const value=JSON.parse(window.sessionStorage.getItem(key)??'null') as PendingPlan|null
    return value&&/^discovery-[a-f0-9]{32}$/.test(value.id)&&typeof value.source==='string'&&typeof value.scope==='string'&&
      Number.isSafeInteger(value.period_seconds)&&Number.isSafeInteger(value.first_at_unix_ms)?value:null
  }catch{return null}
}
const states:Record<string,string> = {
  source_unavailable:'发现来源不可用',no_active_plan:'没有启用的发现计划',waiting_for_result:'等待运行结果',
  no_observations:'本次运行没有观测',observations_available:'已有观测',
  run_failed:'最近一次运行失败',run_skipped:'最近一次运行已跳过',
}
const candidateStates:Record<string,string> = {observed:'待核对',conflict:'存在冲突',admitted:'已纳管'}
const errorText=(error:unknown)=>error instanceof Error?error.message:'发现操作未完成'
const date=(value:number)=>displayDate(value,{year:'numeric',month:'numeric',day:'numeric',hour:'2-digit',minute:'2-digit'})
function periodText(seconds:number) {
  if(!Number.isInteger(seconds)||seconds<60||seconds>604800)return '周期待设置'
  if(seconds%86400===0)return `每 ${seconds/86400} 天`
  if(seconds%3600===0)return `每 ${seconds/3600} 小时`
  if(seconds%60===0)return `每 ${seconds/60} 分钟`
  return `每 ${seconds} 秒`
}

export function DiscoveryPanel({status,refresh}:{status:DiscoveryStatus;refresh:()=>void}) {
  const pendingKey='clusbot:discovery-plan:'+window.location.pathname
  const [pendingPlan,setPendingPlan]=useState(()=>readPendingPlan(pendingKey))
  const pendingRef=useRef(pendingPlan)
  const planLock=useRef(false)
  const [canRetry,setCanRetry]=useState(false)
  const [retryNeedsNewTime,setRetryNeedsNewTime]=useState(false)
  const [candidates,setCandidates]=useState<Candidate[]>()
  const [candidateError,setCandidateError]=useState('')
  const [source,setSource]=useState(pendingPlan?.source??'')
  const [scope,setScope]=useState(pendingPlan?.scope??'')
  const [period,setPeriod]=useState(pendingPlan?.period_seconds??21600)
  const [grant,setGrant]=useState<Grant>()
  const [showCandidates,setShowCandidates]=useState(false)
  const [showPlan,setShowPlan]=useState(false)
  const [confirmStop,setConfirmStop]=useState(false)
  const [admitting,setAdmitting]=useState<Candidate>()
  const [error,setError]=useState('')
  const [notice,setNotice]=useState('')
  const [busy,setBusy]=useState(false)
  const [version,setVersion]=useState(0)
  const [focusTarget,setFocusTarget]=useState<'candidate'|'plan'|'confirm'|'error'>()
  const candidatesId=useId()
  const planId=useId()
  const candidateTrigger=useRef<HTMLButtonElement>(null)
  const planTrigger=useRef<HTMLButtonElement>(null)
  const planConfirm=useRef<HTMLButtonElement>(null)
  const admissionHeading=useRef<HTMLHeadingElement>(null)
  const errorMessage=useRef<HTMLParagraphElement>(null)

  useEffect(()=>{
    function sync(event:Event){
      const detail=(event as CustomEvent<{key:string;source:string;value:PendingPlan|null}>).detail
      if(detail?.key!==pendingKey)return
      pendingRef.current=detail.value;setPendingPlan(detail.value);setCanRetry(false)
      if(!detail.value&&detail.source!==candidatesId)refresh()
    }
    window.addEventListener(pendingEvent,sync)
    return()=>window.removeEventListener(pendingEvent,sync)
  },[pendingKey,refresh,candidatesId])

  useEffect(()=>{
    let active=true
    setCandidateError('')
    api<Candidate[]>('/domain/discovery/candidates').then(rows=>{if(active)setCandidates(rows)})
      .catch(e=>{if(active){setCandidates(undefined);setCandidateError(errorText(e))}})
    return()=>{active=false}
  },[version])

  useEffect(()=>{
    if(busy||!focusTarget)return
    const target=focusTarget==='candidate'?candidateTrigger:focusTarget==='plan'?planTrigger:focusTarget==='confirm'?planConfirm:errorMessage
    target.current?.focus()
    setFocusTarget(undefined)
  },[busy,focusTarget])
  useEffect(()=>{if(admitting)admissionHeading.current?.focus()},[admitting])

  async function execute(action:()=>Promise<unknown>,target:'candidate'|'plan'|'confirm') {
    if(busy)return
    setError('')
    setBusy(true)
    try {await action();refresh();setVersion(value=>value+1);setFocusTarget(target)}
    catch(e){setError(errorText(e));setFocusTarget('error')}
    finally{setBusy(false)}
  }
  function grantAccess() {
    const expiry=Date.now()+24*3600_000
    void execute(async()=>{
      const result=await api<{authorization_id:string}>('/domain/discovery/authorizations',{source,scope,expires_at_unix_ms:expiry})
      setGrant({id:result.authorization_id,source,scope,expiry})
    },'confirm')
  }
  function publish(event:FormEvent) {
    event.preventDefault()
    if(busy||pendingRef.current)return
    if(grant?.source!==source||grant.scope!==scope||grant.expiry<=Date.now()){
      setError('授权已过期或范围已变化，请重新授权。')
      setFocusTarget('error')
      return
    }
    const request={id:'discovery-'+crypto.randomUUID().replaceAll('-',''),source,scope,period_seconds:period,first_at_unix_ms:Date.now()+60_000}
    storePendingPlan(request)
    sendPlan(request,false)
  }
  function storePendingPlan(request:PendingPlan){
    pendingRef.current=request;setPendingPlan(request)
    try{window.sessionStorage.setItem(pendingKey,JSON.stringify(request))}catch{ /* Keep the pending request in memory. */ }
    window.dispatchEvent(new CustomEvent(pendingEvent,{detail:{key:pendingKey,source:candidatesId,value:request}}))
  }
  function sendPlan(request:PendingPlan,retry:boolean){
    if(planLock.current)return
    planLock.current=true
    setError('');setNotice('');setBusy(true)
    void api<{id:string}>('/domain/discovery/plans',request).then(receipt=>{
      if(receipt.id!==request.id)throw new Error('无法核对发现计划回执')
      clearPendingPlan();refresh();setVersion(value=>value+1);setShowPlan(false);setFocusTarget('plan')
    }).catch(error=>{
      if(!retry&&error instanceof APIError&&error.status!==undefined&&error.status>=400&&error.status<500&&error.status!==409){
        clearPendingPlan();setError(errorText(error))
      }else setError('启用结果尚未核实，请先核对原请求。')
      setCanRetry(false)
      setFocusTarget('error')
    }).finally(()=>{planLock.current=false;setBusy(false)})
  }
  function clearPendingPlan(){
    const id=pendingRef.current?.id
    pendingRef.current=null;setPendingPlan(null);setCanRetry(false)
    try{
      const stored=JSON.parse(window.sessionStorage.getItem(pendingKey)??'null') as PendingPlan|null
      if(stored?.id===id)window.sessionStorage.removeItem(pendingKey)
    }catch{ /* In-memory state is already cleared. */ }
    window.dispatchEvent(new CustomEvent(pendingEvent,{detail:{key:pendingKey,source:candidatesId,value:null}}))
  }
  function checkPendingPlan(){
    if(!pendingPlan||busy||planLock.current)return
    planLock.current=true
    setError('');setCanRetry(false);setBusy(true)
    void api<{plan_id:string}>('/domain/discovery/plans/'+encodeURIComponent(pendingPlan.id)+'/observations?limit=1')
      .then(receipt=>{
        if(receipt.plan_id!==pendingPlan.id)throw new Error('无法核对发现计划')
        clearPendingPlan();refresh();setVersion(value=>value+1);setShowPlan(false)
        setNotice('发现计划已创建，请核对当前状态。');setFocusTarget('plan')
      }).catch(error=>{
        if(error instanceof APIError&&error.status===404){
          const expired=pendingPlan.first_at_unix_ms<=Date.now()
          setCanRetry(true);setRetryNeedsNewTime(expired)
          setError(expired?'尚未找到原请求；首次触发时间已过，可重新设置后重试。':'尚未找到原请求，可使用原请求重试。')
        }else setError('尚未核实原请求，请稍后再次核对。')
        setFocusTarget('error')
      })
      .finally(()=>{planLock.current=false;setBusy(false)})
  }
  function retryPendingPlan(){
    if(!pendingPlan||busy||!canRetry)return
    if(!retryNeedsNewTime&&pendingPlan.first_at_unix_ms<=Date.now()){
      setCanRetry(false);setError('首次触发时间已过，请重新核对。');setFocusTarget('error');return
    }
    const request=retryNeedsNewTime?{...pendingPlan,first_at_unix_ms:Date.now()+60_000}:pendingPlan
    storePendingPlan(request)
    sendPlan(request,true)
  }
  function admit(event:FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if(!admitting?.serial)return
    const fields=new FormData(event.currentTarget)
    const row=admitting
    void execute(async()=>{
      await api('/domain/discovery/candidates/'+encodeURIComponent(row.id)+'/admit',{
        confirmed_serial:row.serial,expected_seen_at:row.seen_at,owner:fields.get('owner'),
        location:fields.get('location'),role:fields.get('role'),
      })
      setAdmitting(undefined)
    },'candidate')
  }

  const pending=candidates?.filter(row=>row.state==='observed'||row.state==='conflict').length
  const admitted=candidates?.filter(row=>row.state==='admitted').length
  const conflicts=candidates?.filter(row=>row.state==='conflict').length??0
  const examples=candidates?.slice(0,2).map(row=>row.serial||row.address).join('、')
  const grantReady=Boolean(grant&&grant.source===source&&grant.scope===scope&&grant.expiry>Date.now())
  const unavailable=status.state==='source_unavailable'

  return <section className="discovery-panel" aria-label="资产发现与定时计划">
    <header><h3>资产发现与定时计划</h3>
      <span className={'discovery-status'+(candidates?' discovery-status-count':'')}>
        {candidateError?'候选读取失败':candidates?`${candidates.length===500?'已读取':'已发现'} ${candidates.length} 项`:'正在读取候选'}
      </span>
    </header>
    <div className="discovery-columns">
      <section className="discovery-section">
        <h4>候选资产{pending!==undefined?`（${pending} 项待核对）`:''}</h4>
        {candidateError?<p className="discovery-summary-line">{candidateError}</p>:
          !candidates?<p className="discovery-summary-line" role="status">正在读取候选资产…</p>:
          candidates.length===0?<p className="discovery-summary-line">暂无候选资产</p>:<>
            <p className="discovery-summary-line"><Info size={12} aria-hidden="true"/>{examples}{candidates.length>2?` 等 ${candidates.length} 项`:''}</p>
            <p className="discovery-summary-line"><Info size={12} aria-hidden="true"/>{conflicts?`${conflicts} 项存在身份或地址冲突，需人工核对`:`待核对 ${pending} 项 · 已纳管 ${admitted} 项`}</p>
          </>}
      </section>
      <section className="discovery-section">
        <h4>{status.plan?'定时发现计划':'定时发现计划预览'}</h4>
        {status.plan?<>
          <p className="discovery-summary-line"><Info size={12} aria-hidden="true"/>来源：{status.plan.source} · 范围：{status.plan.scope}</p>
          <p className="discovery-summary-line"><Info size={12} aria-hidden="true"/>下次触发：{date(status.plan.next_at_unix_ms)}</p>
          {(status.state==='run_failed'||status.state==='run_skipped')&&<p className="discovery-summary-line"><Info size={12} aria-hidden="true"/>{states[status.state]}</p>}
          {status.last_run?.observation_count!==undefined&&<p className="discovery-summary-line"><Info size={12} aria-hidden="true"/>最近一次：{status.last_run.observation_count} 条观测</p>}
        </>:<>
          <p className="discovery-summary-line"><Info size={12} aria-hidden="true"/>{unavailable?'发现来源不可用':`周期预览：${periodText(period)}（未启用）`}</p>
          <p className="discovery-summary-line"><Info size={12} aria-hidden="true"/>{unavailable?'请先配置可用的发现来源':source&&scope?`来源：${source} · 范围：${scope}`:'来源与范围待设置'}</p>
        </>}
      </section>
    </div>
    {pendingPlan&&<div className="discovery-summary-line discovery-pending" role="status"><span>发现计划的启用结果待核对。</span> <button type="button" className="text-button" disabled={busy} onClick={checkPendingPlan}>核对原请求</button>
      {canRetry&&<button type="button" className="text-button" disabled={busy} onClick={retryPendingPlan}>{retryNeedsNewTime?'从现在起约 1 分钟触发并重试':'重试原请求'}</button>}</div>}
    {notice&&<p className="discovery-summary-line" role="status">{notice}</p>}
    <div className="discovery-actions discovery-main-actions">
      <button type="button" ref={candidateTrigger} className="outline" aria-expanded={showCandidates} aria-controls={candidatesId} onClick={()=>{setShowCandidates(value=>!value);setAdmitting(undefined)}}>
        <Server size={13} aria-hidden="true"/>{showCandidates?'收起候选资产':'核对候选资产'}
      </button>
      <button type="button" ref={planTrigger} className={status.plan?'outline':'primary'} disabled={unavailable} aria-expanded={showPlan} aria-controls={planId} onClick={()=>{setShowPlan(value=>!value);setConfirmStop(false)}}>
        <CalendarCheck size={13} aria-hidden="true"/>{showPlan?'收起计划':status.plan?'查看计划':'设置发现计划'}
      </button>
    </div>
    {showCandidates&&<section className="discovery-detail" id={candidatesId} aria-label="候选资产详情">
      <div className="discovery-detail-heading"><h4>候选资产</h4><button type="button" className="text-button" onClick={()=>{refresh();setVersion(value=>value+1)}}>重新读取</button></div>
      {candidateError&&<p className="error" role="alert">读取失败：{candidateError}</p>}
      {!candidates&&!candidateError&&<p className="muted" role="status">正在读取候选资产…</p>}
      {candidates?.length===0&&<p className="muted">暂无候选资产</p>}
      {candidates&&candidates.length>0&&<ul className="discovery-items">
        {candidates.map(row=><li key={row.id}>
          <div><strong>{row.serial||row.address}</strong><small>{row.serial?`${row.address} · `:''}{row.source} / {row.scope} · {candidateStates[row.state]??row.state}</small></div>
          {row.state==='observed'&&<button type="button" className="outline" disabled={!row.serial||busy} onClick={()=>setAdmitting(row)}>核对并纳管</button>}
          {row.state==='observed'&&!row.serial&&<small>缺少序列号，无法纳管</small>}
        </li>)}
      </ul>}
      {admitting&&<form className="discovery-form" onSubmit={admit}>
        <h4 ref={admissionHeading} tabIndex={-1}>核对候选：{admitting.serial}</h4>
        <p>地址：{admitting.address} · 候选 ID：{admitting.id}</p>
        <p>来源：{admitting.source} · 范围：{admitting.scope} · 观测时间：{date(admitting.seen_at)}</p>
        <div className="discovery-fields">
          <label>责任人<input name="owner" required maxLength={256}/></label>
          <label>位置<input name="location" required maxLength={256}/></label>
          <label>角色<input name="role" required maxLength={256}/></label>
        </div>
        <div className="discovery-actions"><button type="button" className="outline" onClick={()=>{setAdmitting(undefined);candidateTrigger.current?.focus()}}>取消</button>
          <button type="submit" className="primary" disabled={busy}>确认纳管</button></div>
      </form>}
    </section>}
    {showPlan&&<section className="discovery-detail" id={planId} aria-label="发现计划详情">
      {status.plan?<>
        <h4>发现计划</h4>
        <p>计划 ID：{status.plan.id}</p>
        <p>{states[status.state]??status.state}{status.last_run?.observation_count!==undefined&&` · 最近一次 ${status.last_run.observation_count} 条观测`}</p>
        <p>当前状态未提供计划周期</p>
        <div className="discovery-actions">{confirmStop?<>
          <button type="button" className="outline" onClick={()=>setConfirmStop(false)}>取消</button>
          <button type="button" className="primary" disabled={busy} onClick={()=>void execute(async()=>{await api('/domain/discovery/plans/'+encodeURIComponent(status.plan!.id),undefined,'DELETE');setShowPlan(false);setConfirmStop(false)},'plan')}>确认停止计划</button>
        </>:<button type="button" className="outline" disabled={busy} onClick={()=>setConfirmStop(true)}>停止计划</button>}</div>
      </>:<form className="discovery-form" onSubmit={publish}>
        <h4>设置发现计划</h4>
        <div className="discovery-fields">
          <label>发现来源<input required value={source} onChange={e=>setSource(e.currentTarget.value)} maxLength={128} disabled={busy||Boolean(pendingPlan)}/></label>
          <label>发现范围<input required value={scope} onChange={e=>setScope(e.currentTarget.value)} maxLength={128} disabled={busy||Boolean(pendingPlan)}/></label>
          <label>周期（秒）<input required type="number" min={60} max={604800} step={1} value={period} onChange={e=>setPeriod(Number(e.currentTarget.value))} disabled={busy||Boolean(pendingPlan)}/></label>
        </div>
        <p>计划预览：{periodText(period)}发现一次，启用约 1 分钟后首次触发。</p>
        <p>授权所填来源与范围进行发现，有效期为 24 小时；到期后不会执行发现。</p>
        {grantReady&&<p role="status">已授权至 {date(grant!.expiry)} · 授权 ID：{grant!.id}</p>}
        <div className="discovery-actions"><button type="button" className="outline" disabled={busy||Boolean(pendingPlan)||!source||!scope} onClick={grantAccess}>授权发现 24 小时</button>
          <button type="submit" ref={planConfirm} className="primary" disabled={busy||Boolean(pendingPlan)||!grantReady}>确认启用计划</button></div>
      </form>}
    </section>}
    {error&&<p className="error" ref={errorMessage} role="alert" tabIndex={-1}>{error}</p>}
  </section>
}
