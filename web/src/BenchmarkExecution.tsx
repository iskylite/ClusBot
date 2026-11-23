import {useCallback,useEffect,useRef,useState} from 'react'
import {api,APIError} from './api'

export type BenchmarkProgress = {
  task_id:string; profile:string; version:number; plan_revision:number; state:string; stop_requested:boolean
  dispatch_control?:{paused:boolean;available:boolean;control_version:number;unavailable_reason?:string}
  asset_scope:{source:string;target_count:number;target_refs:string[]}
  cases:BenchmarkCase[]
}

type BenchmarkCase = {
  key:string; phase:string; step_id:string; job_id?:string; target_holds:number
  operation?:{id:string;state:string}; wait?:{id:string;version:number;state:string;recheck_at?:number}
  targets:{ref:string;frozen_revision:number;ownership_epoch:number;local_hold:string}[]
  execution_control?:{action:string;state:string;plan_revision:number}
  report?:{reason?:string}
}

type BenchmarkReport = {
  plan_revision:number; asset_acceptance:string
  cases:{step_id:string;case_key:string;phase:string;tool_outcome:string;required_evidence:string;performance:string;reason?:string}[]
}

type SkipReceipt = {
  task_id:string; step_id:string; action:'skip_this_run'; state:'skipped_this_run'; plan_revision:number
}

export function buildSkipRequest(taskID:string,stepID:string,caseKey:string,revision:number,key:string,conversationID?:string) {
  if (conversationID) {
    return {path:'/conversations/'+encodeURIComponent(conversationID)+'/messages',chat:true as const,
      body:{text:'本次跳过 '+caseKey,client_request_id:key,
        benchmark_execution_target:{task_id:taskID,expected_plan_revision:revision}}}
  }
  return {path:'/domain/benchmarks/'+encodeURIComponent(taskID)+'/cases/'+encodeURIComponent(stepID)+'/execution-control',chat:false as const,
    body:{action:'skip_this_run',expected_plan_revision:revision,client_request_id:key,reason:'用户在任务详情选择本次跳过'}}
}

export function benchmarkReadiness(p:BenchmarkProgress) {
  if (p.state==='cancelled') return '已确认停止'
  if (p.stop_requested) return '停止请求已登记，正在核对'
  if (p.dispatch_control?.paused || p.state==='paused') return '已暂停后续派发'
  if (p.cases.some(c=>c.operation?.state==='reconcile')) return '外部执行结果待核实'
  switch (p.state) {
    case 'pending': return '等待执行'
    case 'running': return '正在办理'
    case 'waiting': return '等待条件满足'
    case 'paused': return '已暂停新派发'
    case 'succeeded': return '流程已结束'
    case 'failed': return '流程已结束，未满足必测条件'
    case 'finalizing': return '正在收尾'
    default: return '状态待核实'
  }
}

function caseStatus(item:BenchmarkCase) {
  if (item.execution_control?.action==='skip_this_run' && item.execution_control.state==='skipped_this_run') return '本次未执行（按请求跳过）'
  if (item.operation?.state==='reconcile') return '外部结果待核实'
  switch (item.phase) {
    case 'queued': return '未派发'
    case 'submitting': return '正在核对提交'
    case 'following': return '等待远端结果'
    case 'verified': return '执行证据已核实'
    case 'incomplete': return '核验未完成'
    case 'skipped': return '未执行'
    default: return '状态待核实'
  }
}

function outcome(value:string) {
  return ({
    not_started:'未执行',in_progress:'进行中',outcome_unknown:'结果待核对',completed:'已结束',
    failed:'未成功',cancelled:'已确认取消',not_evaluated:'未评估',covered:'齐全',incomplete:'不齐全',
    passed:'已达阈值',below_threshold:'未达阈值',not_applicable:'不适用',
    met:'已达阈值',not_met:'未达阈值',not_configured:'未配置阈值',unknown:'待核实',
  } as Record<string,string>)[value] ?? '待核实'
}

export function useBenchmarkProgress(taskID:string, enabled=true) {
  const [result,setResult]=useState<{taskID:string;value:BenchmarkProgress|null;error:string;unavailable:boolean}>()
  const sequence=useRef(0)
  const refresh=useCallback(async()=>{
    if (!enabled)return
    const request=++sequence.current
    try {
      const value=await api<BenchmarkProgress>('/domain/benchmarks/'+encodeURIComponent(taskID))
      if (sequence.current===request)setResult({taskID,value,error:'',unavailable:false})
    } catch(e) {
      if (sequence.current!==request)return
      if(e instanceof APIError&&e.status===404) {
        setResult({taskID,value:null,error:'',unavailable:true})
        return
      }
      setResult(previous=>({taskID,value:previous?.taskID===taskID && !(e instanceof APIError&&e.status===403)?previous.value:null,
        unavailable:false,error:e instanceof Error?e.message:'暂时无法核对最新状态'}))
    }
  },[enabled,taskID])
  useEffect(()=>{
    if(!enabled)return
    void refresh()
    return()=>{sequence.current++}
  },[enabled,refresh])
  const current=result?.taskID===taskID?result:undefined
  const poll=Boolean(current?.value&&!['succeeded','failed','cancelled'].includes(current.value.state))
  useEffect(()=>{
    if (!enabled || !poll)return
    const id=window.setInterval(()=>{if(!document.hidden)void refresh()},3500)
    return()=>window.clearInterval(id)
  },[enabled,poll,refresh])
  return {progress:current?.value??undefined,unavailable:current?.unavailable??false,checking:enabled&&!current,error:current?.error??'',refresh}
}

type TaskDispatchReceipt = {task_id:string;action:'pause'|'resume';state:'paused'|'resumed';plan_revision:number;control_version:number}

export function useBenchmarkTaskDispatch(progress:BenchmarkProgress|undefined,refresh:()=>Promise<void>,onApplied?:(receipt:TaskDispatchReceipt)=>void) {
  const [busy,setBusy]=useState(false)
  const [notice,setNotice]=useState<{text:string;error?:boolean}>()
  const lock=useRef(false)
  const pending=useRef<{target:string;key:string}|undefined>(undefined)
  const noticeVersion=useRef<number|undefined>(undefined)
  useEffect(()=>{
    if(noticeVersion.current!==undefined&&progress?.dispatch_control&&progress.dispatch_control.control_version>noticeVersion.current) {
      noticeVersion.current=undefined;setNotice(undefined)
    }
  },[progress?.dispatch_control?.control_version])
  async function apply() {
    const control=progress?.dispatch_control
    if(!progress||!control?.available||lock.current)return
    const action=control.paused?'resume':'pause'
    const target=JSON.stringify([progress.task_id,action,progress.plan_revision,control.control_version])
    const key=pending.current?.target===target?pending.current.key:crypto.randomUUID()
    pending.current={target,key};lock.current=true;setBusy(true);setNotice(undefined);noticeVersion.current=undefined
    const base='/domain/benchmarks/'+encodeURIComponent(progress.task_id)+'/dispatch-control'
    const valid=(receipt:TaskDispatchReceipt)=>receipt.task_id===progress.task_id&&receipt.action===action&&
      receipt.state===(action==='pause'?'paused':'resumed')&&receipt.plan_revision===progress.plan_revision&&receipt.control_version===control.control_version+1
    let receipt:TaskDispatchReceipt|undefined
    try {
      receipt=await api<TaskDispatchReceipt>(base,{action,expected_plan_revision:progress.plan_revision,
        expected_control_version:control.control_version,client_request_id:key})
      if(!valid(receipt))throw new Error('回执无法核对')
    } catch(error) {
      if(error instanceof APIError&&error.status!==undefined&&error.status<500) {
        pending.current=undefined
        setNotice({text:error.status===409?'任务或控制状态已变化，正在刷新；请核对后重试。':error.message,error:true})
      } else {
        try {
          const found=await api<TaskDispatchReceipt>(base+'?'+new URLSearchParams({client_request_id:key}))
          if(valid(found))receipt=found
        } catch { /* Preserve the original request key until its outcome is known. */ }
        if(!receipt||!valid(receipt)) {
          receipt=undefined
          setNotice({text:'请求结果尚未核实；再次操作会沿用同一请求。',error:true})
        }
      }
    }
    if(receipt) {
      pending.current=undefined
      noticeVersion.current=receipt.control_version
      setNotice({text:action==='pause'?'已暂停后续派发，已派发作业继续核对。':'已恢复后续派发，执行前仍会核对条件。'})
      onApplied?.(receipt)
    }
    try {await refresh()} finally {lock.current=false;setBusy(false)}
  }
  return {apply,busy,notice}
}

export function BenchmarkExecution({progress,refresh,onChange,compact=false,stale=false,conversationID,conversationBusy=false}:{
  progress:BenchmarkProgress;refresh:()=>Promise<void>;onChange?:()=>void;compact?:boolean;stale?:boolean;conversationID?:string;conversationBusy?:boolean
}) {
  const [busyStep,setBusyStep]=useState<string>()
  const [notice,setNotice]=useState<{text:string;error?:boolean}>()
  const [showReport,setShowReport]=useState(false)
  const [report,setReport]=useState<BenchmarkReport>()
  const [reportError,setReportError]=useState('')
  const [reportBusy,setReportBusy]=useState(false)
  const [reportGeneration,setReportGeneration]=useState(0)
  const taskDispatch=useBenchmarkTaskDispatch(progress,refresh,()=>onChange?.())
  const lock=useRef(false)
  const inFlightKeys=useRef(new Map<string,string>())
  const reportSignature=JSON.stringify([progress.plan_revision,progress.state,progress.cases.map(c=>[c.step_id,c.phase,c.operation?.state])])
  useEffect(()=>{
    if (!showReport)return
    let current=true
    setReport(undefined);setReportError('');setReportBusy(true)
    api<BenchmarkReport>('/domain/benchmarks/'+encodeURIComponent(progress.task_id)+'/report')
      .then(value=>{if(current)setReport(value)})
      .catch(e=>{if(current)setReportError(e instanceof Error?e.message:'无法读取执行核验')})
      .finally(()=>{if(current)setReportBusy(false)})
    return()=>{current=false}
  },[progress.task_id,reportSignature,reportGeneration,showReport])

  const revision=progress.plan_revision
  const canSkip=(item:BenchmarkCase)=> !stale && !progress.stop_requested &&
    ['pending','running','paused'].includes(progress.state) && item.phase==='queued' && !item.operation && !item.job_id && !item.target_holds
  async function skipOnce(item:BenchmarkCase) {
    if(lock.current||conversationBusy||!canSkip(item))return
    lock.current=true;setBusyStep(item.step_id);setNotice(undefined)
    const request={task:progress.task_id,step:item.step_id,revision}
    const target=JSON.stringify([request.task,request.step,revision])
    const key=inFlightKeys.current.get(target)??crypto.randomUUID()
    inFlightKeys.current.set(target,key)
    try {
      const submission=buildSkipRequest(request.task,request.step,item.key,request.revision,key,conversationID)
      const receipt=submission.chat
        ? (await api<{benchmark_execution_control?:SkipReceipt}>(submission.path,submission.body)).benchmark_execution_control
        : await api<SkipReceipt>(submission.path,submission.body)
      if(!receipt||receipt.task_id!==request.task||receipt.step_id!==request.step||receipt.action!=='skip_this_run'||
          receipt.state!=='skipped_this_run'||receipt.plan_revision!==request.revision) throw new Error('收到的回执无法核对')
      inFlightKeys.current.delete(target)
      setNotice({text:'本次跳过已登记；不会移除必测项，也不计为通过。'})
    } catch(error) {
      if(error instanceof APIError && error.status!==undefined && error.status<500) {
        inFlightKeys.current.delete(target)
        setNotice({text:error.status===409?'任务或测项状态已变化，未自动重试，正在重新核对。':error.message,error:true})
      } else {
        setNotice({text:'这次请求是否已受理尚未核实，正在刷新；不会自动重发。',error:true})
      }
    } finally {
      try {await refresh();onChange?.()} finally {lock.current=false;setBusyStep(undefined)}
    }
  }

  const verified=progress.cases.filter(c=>c.phase==='verified').length
  return <section className="benchmark-execution" aria-label="基准执行">
    <header className="benchmark-execution-title"><div><h3>基准执行</h3><p className="muted">方案 {progress.profile} v{progress.version} · 本次修订 {revision}</p></div>
      <span className="muted">证据已核实 {verified} / {progress.cases.length}</span>
      {progress.dispatch_control?.available&&<button type="button" className="outline" disabled={stale||taskDispatch.busy} onClick={()=>void taskDispatch.apply()}>{taskDispatch.busy?'正在核对…':progress.dispatch_control.paused?'恢复后续派发':'暂停后续派发'}</button>}
    </header>
    {progress.dispatch_control?.unavailable_reason&&<p className="muted">{progress.dispatch_control.unavailable_reason}</p>}
    {taskDispatch.notice&&<p role={taskDispatch.notice.error?'alert':'status'} className={taskDispatch.notice.error?'error':'muted'}>{taskDispatch.notice.text}</p>}
    <details className="benchmark-targets"><summary>资产目标数 {progress.asset_scope.target_count}</summary>
      <p className="muted">这是任务冻结的资产目标引用数，不表示 GPU 数量或实时容量。</p>
      <ul>{(progress.asset_scope.target_refs??[]).map(ref=><li key={ref}>{ref}</li>)}</ul>
    </details>
    <ul className="benchmark-cases">{progress.cases.map(item=><li className="benchmark-case" key={item.step_id}>
      <div className="benchmark-case-head"><div><strong>{item.key}</strong><small>{caseStatus(item)}</small></div>
        {canSkip(item)&&<button type="button" className="text-button" disabled={Boolean(busyStep)||conversationBusy}
          onClick={()=>void skipOnce(item)}>{busyStep===item.step_id?'正在提交…':'本次跳过'}</button>}</div>
      {(item.job_id||item.target_holds>0)&&<p className="muted">{item.job_id&&<>作业 {item.job_id}</>}{Boolean(item.job_id&&item.target_holds)&&' · '}{item.target_holds>0&&<>仍持有 {item.target_holds} 个资产目标</>}</p>}
      {item.wait?.state==='open'&&!!item.wait.recheck_at&&<p className="muted">计划复查时间 {new Date(item.wait.recheck_at).toLocaleString('zh-CN')}</p>}
      {item.report?.reason&&<p className="muted">{item.report.reason}</p>}
    </li>)}</ul>
    {conversationBusy&&progress.cases.some(canSkip)&&<p className="muted">当前对话仍在处理，结束后可提交本次跳过。</p>}
    {notice&&<p role={notice.error?'alert':'status'} className={notice.error?'error':'muted'}>{notice.text}</p>}
    <button type="button" className="text-button" aria-expanded={showReport} onClick={()=>setShowReport(!showReport)}>{showReport?'收起执行回执':'查看执行回执'}</button>
    {showReport&&<div className="benchmark-receipts" aria-busy={reportBusy}>
      {reportBusy&&<p className="muted">正在读取执行回执…</p>}
      {reportError&&<p className="error" role="alert">{reportError} <button type="button" className="text-button" onClick={()=>setReportGeneration(n=>n+1)}>重新读取</button></p>}
      {report?.plan_revision===revision&&<><p className="muted">资产验收：{outcome(report.asset_acceptance)}。这里不代替资产验收。</p>
        <ul>{report.cases.map(item=><li key={item.step_id}><strong>{item.case_key}</strong>
          <span>工具：{outcome(item.tool_outcome)} · 必测证据：{outcome(item.required_evidence)} · 性能：{outcome(item.performance)}</span>
          {item.reason&&<small>{item.reason}</small>}
        </li>)}</ul></>}
    </div>}
    {!compact&&progress.cases.length===0&&<p className="muted">尚无可展示的测项。</p>}
  </section>
}
