import {useEffect,useId,useRef,useState,type FormEvent,type ReactNode} from 'react'
import {CalendarCheck,Info,Server} from 'lucide-react'
import {api,APIError} from './api'
import {displayDate} from './GeneralPreferences'
import {AssistantSurfaceView} from './AssistantSurfaceView'
import {a2uiPath,confirmDiscoveryAdmission,discoveryConfirmationRef,parseSurfaceReceipt,parseSurfaces,readSurfaceAction,recoverDiscoveryConfirmation,sendSurfaceAction,type AssistantReply,type DiscoveryConfirmationRef,type DiscoveryConversationSeed,type Surface,type SurfaceAction} from './assistantSurface'
import {forgetDiscoveryConfirmation,pendingDiscoveryConfirmations,rememberDiscoveryConfirmation} from './discoveryConfirmation'
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

export function DiscoveryPanel({status,refresh,principalID,conversationID,conversationReadOnly,initialSurfaces,openConversation,renderConfirmation}:{
  status:DiscoveryStatus;refresh:()=>void;principalID?:string;conversationID?:string;conversationReadOnly?:boolean;initialSurfaces?:Surface[];
  openConversation?:(seed:DiscoveryConversationSeed)=>void;
  renderConfirmation:(item:AssistantReply,onSettled:(item?:AssistantReply)=>void,confirm?:((previewID:string)=>Promise<AssistantReply>))=>ReactNode
}) {
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
  const [showCandidates,setShowCandidates]=useState(Boolean(initialSurfaces))
  const [showPlan,setShowPlan]=useState(false)
  const [confirmStop,setConfirmStop]=useState(false)
  const [review,setReview]=useState<AssistantReply>()
  const [preparedConfirmation,setPreparedConfirmation]=useState<DiscoveryConfirmationRef>()
  const [finalLookup,setFinalLookup]=useState<DiscoveryConfirmationRef>()
  const [candidateSurfaces,setCandidateSurfaces]=useState(initialSurfaces)
  const [surfaceError,setSurfaceError]=useState('')
  const [surfaceBusy,setSurfaceBusy]=useState(false)
  const surfaceLock=useRef(false)
  const mounted=useRef(false)
  const lastAction=useRef<{fingerprint:string;key:string;action:SurfaceAction}|undefined>(undefined)
  const bootstrapKey=useRef<string|undefined>(undefined)
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
  const reviewHeading=useRef<HTMLDivElement>(null)
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
  useEffect(()=>{
    mounted.current=true
    return()=>{mounted.current=false}
  },[])
  useEffect(()=>{if(review)reviewHeading.current?.focus()},[review])
  const surfacePath=a2uiPath(conversationID,'discovery-candidates')
  useEffect(()=>{
    if(!showCandidates||!surfacePath||conversationReadOnly||(version===0&&initialSurfaces))return
    let active=true
    setCandidateSurfaces(undefined);setSurfaceError('')
    api<unknown>(surfacePath).then(value=>{
      const surfaces=parseSurfaceReceipt(value,conversationID)
      if(!surfaces)throw new Error('候选交互数据无效，请重新读取。')
      if(active)setCandidateSurfaces(surfaces)
    }).catch(e=>{if(active)setSurfaceError(errorText(e))})
    return()=>{active=false}
  },[showCandidates,surfacePath,conversationID,conversationReadOnly,initialSurfaces,version])

  async function execute(action:()=>Promise<unknown>,target:'candidate'|'plan'|'confirm') {
    if(busy||conversationReadOnly)return
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
  async function dispatch(action:SurfaceAction) {
    if(!conversationID||conversationReadOnly||surfaceLock.current)return
    surfaceLock.current=true;setSurfaceBusy(true);setSurfaceError('')
    const {name,surfaceId,sourceComponentId,context}=action.action
    const fingerprint=JSON.stringify([name,surfaceId,sourceComponentId,context])
    if(lastAction.current?.fingerprint!==fingerprint)lastAction.current={fingerprint,key:crypto.randomUUID(),action}
    const request=lastAction.current
    try {
      const result=await sendSurfaceAction(api,conversationID,request.action,request.key)
      if(mounted.current){
        lastAction.current=undefined
        setPreparedConfirmation(result.state==='confirmation_required'?discoveryConfirmationRef(request.action,request.key):undefined)
        setReview(result)
      }
    } catch(e) {
      if(mounted.current)setSurfaceError(errorText(e))
    } finally {
      surfaceLock.current=false
      if(mounted.current)setSurfaceBusy(false)
    }
  }
  async function createDiscoveryConversation() {
    if(!openConversation||conversationID||conversationReadOnly||surfaceLock.current)return
    surfaceLock.current=true;setSurfaceBusy(true);setSurfaceError('')
    const key=bootstrapKey.current??=crypto.randomUUID()
    try {
      const result=await api<{conversation_id?:string;run_id?:string;surface?:unknown}>('/a2ui/discovery-candidates',{},'POST',{'X-ClusBot-Request-Key':key})
      const surfaces=parseSurfaceReceipt(result,result?.conversation_id)
      if(!surfaces||!a2uiPath(result?.conversation_id,'actions')||!/^[a-f\d]{32}$/.test(result?.run_id??''))throw new Error('无法核对这次候选查看的会话。请重试。')
      if(mounted.current)openConversation({conversationID:result.conversation_id!,runID:result.run_id!,surfaces})
    } catch(e) {
      if(mounted.current)setSurfaceError(errorText(e))
    } finally {
      surfaceLock.current=false
      if(mounted.current)setSurfaceBusy(false)
    }
  }
  async function readback() {
    if(finalLookup&&conversationID&&principalID&&!conversationReadOnly&&!surfaceLock.current) {
      surfaceLock.current=true;setSurfaceBusy(true);setSurfaceError('')
      try{
        const result=await recoverDiscoveryConfirmation(api,conversationID,finalLookup)
        if(result.state==='admitted') {
          forgetDiscoveryConfirmation(principalID,conversationID,finalLookup)
          if(mounted.current){setFinalLookup(undefined);onSettled(result.receipt)}
        } else if(mounted.current) {
          setSurfaceError('原请求尚没有已核实的纳管结果。这次只读取了状态，没有重新发起纳管。')
        }
      }catch(e){
        if(mounted.current)setSurfaceError(errorText(e))
      } finally {
        surfaceLock.current=false
        if(mounted.current)setSurfaceBusy(false)
      }
      return
    }
    const request=lastAction.current
    if(!request||!conversationID||conversationReadOnly||surfaceLock.current)return
    surfaceLock.current=true;setSurfaceBusy(true);setSurfaceError('')
    try {
      const result=await readSurfaceAction(api,conversationID,request.action,request.key)
      if(mounted.current){lastAction.current=undefined;setReview(result)}
    } catch(e) {
      if(mounted.current)setSurfaceError(errorText(e))
    } finally {
      surfaceLock.current=false
      if(mounted.current)setSurfaceBusy(false)
    }
  }
  async function confirmPrepared(previewID:string):Promise<AssistantReply> {
    if(!principalID||!conversationID||!preparedConfirmation||conversationReadOnly||surfaceLock.current||
      review?.state!=='confirmation_required'||review.confirmation_id!==previewID)throw new Error('无法核对本次纳管预览。')
    const pointer=preparedConfirmation
    // Store only a lookup pointer; the approval itself is never persisted or replayed.
    rememberDiscoveryConfirmation(principalID,conversationID,pointer)
    surfaceLock.current=true;setSurfaceBusy(true);setSurfaceError('');setFinalLookup(pointer)
    try{
      const result=await confirmDiscoveryAdmission(api,conversationID,pointer,previewID)
      forgetDiscoveryConfirmation(principalID,conversationID,pointer)
      if(mounted.current)setFinalLookup(undefined)
      return result
    } finally {
      surfaceLock.current=false
      if(mounted.current)setSurfaceBusy(false)
    }
  }
  function backToCandidates() {
    if(surfaceLock.current)return
    setReview(undefined);setPreparedConfirmation(undefined);setFinalLookup(undefined);setSurfaceError('');lastAction.current=undefined
    setVersion(value=>value+1);candidateTrigger.current?.focus()
  }
  function onSettled(result?:AssistantReply) {
    if(!result){backToCandidates();return}
    setReview(result);refresh();setVersion(value=>value+1)
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
      <button type="button" ref={candidateTrigger} className="outline" aria-expanded={showCandidates} aria-controls={candidatesId} disabled={surfaceBusy} onClick={()=>{
          if(!showCandidates&&!conversationID&&openConversation){void createDiscoveryConversation();return}
          setShowCandidates(value=>!value)
        }}>
        <Server size={13} aria-hidden="true"/>{showCandidates?'收起候选资产':'核对候选资产'}
      </button>
      <button type="button" ref={planTrigger} className={status.plan?'outline':'primary'} disabled={unavailable||conversationReadOnly} aria-expanded={showPlan} aria-controls={planId} onClick={()=>{setShowPlan(value=>!value);setConfirmStop(false)}}>
        <CalendarCheck size={13} aria-hidden="true"/>{showPlan?'收起计划':status.plan?'查看计划':'设置发现计划'}
      </button>
    </div>
    {surfaceError&&<p className="error" role="alert">{surfaceError}</p>}
    {(finalLookup||surfaceError&&lastAction.current)&&
      <button type="button" className="text-button" disabled={surfaceBusy||conversationReadOnly} onClick={()=>void readback()}>核对原请求</button>}
    {surfaceBusy&&<p role="status" className="muted">正在向助手核对候选…</p>}
    {showCandidates&&<section className="discovery-detail" id={candidatesId} aria-label="候选资产详情">
      <div className="discovery-detail-heading"><h4>候选资产</h4>{!review&&<button type="button" className="text-button" disabled={surfaceBusy} onClick={()=>{refresh();setVersion(value=>value+1)}}>重新读取</button>}</div>
      {!review&&<>
        {conversationReadOnly||!surfacePath?<p role="status">当前会话只能查看候选，不能纳管。</p>:
          !candidateSurfaces&&!surfaceError?<p className="muted" role="status">正在读取候选交互…</p>:null}
        {candidateSurfaces&&!conversationReadOnly?candidateSurfaces.map(surface=><AssistantSurfaceView key={surface.id} surface={surface} busy={surfaceBusy} onAgentAction={dispatch}/>):
          candidates&&candidates.length>0&&<ul className="discovery-items">{candidates.map(row=><li key={row.id}>
            <div><strong>{row.serial||row.address}</strong><small>{row.serial?`${row.address} · `:''}{row.source} / {row.scope} · {candidateStates[row.state]??row.state}</small></div>
          </li>)}</ul>}
      </>}
      {review&&<div ref={reviewHeading} tabIndex={-1}>
        {review.state==='confirmation_required'||(review.state==='completed'&&review.tool==='admit_discovery_candidate')?
          renderConfirmation(review,onSettled,preparedConfirmation&&!finalLookup?confirmPrepared:undefined):
          review.state==='completed'&&parseSurfaces(review.surface)?.map(surface=><AssistantSurfaceView key={surface.id} surface={surface} busy={surfaceBusy} onAgentAction={conversationReadOnly?undefined:dispatch}/>)}
        {review.state==='completed'&&review.outcome==='show_admission_preview'&&<p role="status">已核对原审阅结果。这次核对没有执行纳管，也没有生成新的确认入口。</p>}
        <button type="button" className="text-button" disabled={surfaceBusy} onClick={backToCandidates}>返回候选列表</button>
      </div>}
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

function RecoveredAdmission({principalID,conversationID,pointer}:{principalID:string;conversationID:string;pointer:DiscoveryConfirmationRef}) {
  const lock=useRef(false)
  const [pending,setPending]=useState(false)
  const [message,setMessage]=useState('')
  const [asset,setAsset]=useState('')
  async function verify() {
    if(lock.current||asset)return
    lock.current=true;setPending(true);setMessage('')
    try{
      const original=await recoverDiscoveryConfirmation(api,conversationID,pointer)
      if(original.state==='admitted'){
        setAsset(original.receipt.result.asset)
        forgetDiscoveryConfirmation(principalID,conversationID,pointer)
      } else setMessage('尚未核实到已完成的纳管。这次只读取了状态，没有重新发起操作。')
    }catch(error){
      setMessage(errorText(error))
    }finally{
      lock.current=false;setPending(false)
    }
  }
  return <article className="assistant-card">
    <h3>{asset?'已核实原纳管结果':'原纳管结果待核对'}</h3>
    {asset?<p role="status">已纳管 · {asset}</p>:
      <button type="button" className="outline" disabled={pending} onClick={()=>void verify()}>{pending?'正在读取原回执…':'核对原请求'}</button>}
    {message&&<p role="status">{message}</p>}
  </article>
}

// Only called when the signed-in account can read this conversation. Reloads
// never restore a confirmation token or automatically submit an operation.
export function DiscoveryConfirmationRecovery({principalID,conversationID}:{principalID:string;conversationID:string}) {
  const [entries]=useState(()=>pendingDiscoveryConfirmations(principalID,conversationID))
  if(!entries.length)return null
  return <div className="assistant-reply-cards">{entries.map(pointer=>
    <RecoveredAdmission key={pointer.key} principalID={principalID} conversationID={conversationID} pointer={pointer}/>)}</div>
}
