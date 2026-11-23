import {useEffect,useRef,useState} from 'react'
import {Activity,Check,CircleHelp,Cpu,GitCommitHorizontal,HardDrive,Pause,Play,Server,ShieldCheck} from 'lucide-react'
import {api,APIError,type Detail,type Run,type RunRecord} from './api'
import {benchmarkReadiness,useBenchmarkProgress,useBenchmarkTaskDispatch} from './BenchmarkExecution'
import './assistant-cards.css'
import './BenchmarkDispatchControl.css'

type Scope={target_count:number;target_refs?:string[]}
type PreflightIssue={code:string;target?:string;detail:string}
type AdmissionPreview={
  preview_id:string;state:string;execution_started:boolean
  selection:{target:string;profile:{profile:string;version:number};visible_asset_refs:string[]}
  preflight:{can_queue:boolean;facility_status:string;cases:{key:string;blockers:PreflightIssue[];warnings:PreflightIssue[]}[]}
}
export type RevisionPreview={
  run_id:string;conversation_id:string;task_id:string;source_run_id?:string
  proposal:{state:string;expected_revision:number;interpretation:{action:string;case_key:string;parameter?:string;value?:string;change_expected_actual_config?:boolean};
    preview:{revision:number;changes:{added:string[];removed:string[];changed:string[];order_changed:boolean};frozen_cases:string[];
      asset_impact?:{baseline:Scope;proposed:Scope;scope_changed:boolean}}}
}

// The same message can move from the initial snapshot into paged history while
// its card is mounted. Share that one in-flight request across both mounts.
const revisionPreviewFlights=new Map<string,Promise<RevisionPreview>>()
function previewOnce(identity:string,load:()=>Promise<RevisionPreview>) {
  const existing=revisionPreviewFlights.get(identity)
  if(existing)return existing
  const pending=load()
  revisionPreviewFlights.set(identity,pending)
  void pending.then(()=>revisionPreviewFlights.delete(identity),()=>revisionPreviewFlights.delete(identity))
  return pending
}

function commandError(e:unknown, action:string) {
  return e instanceof APIError&&e.status!==undefined&&e.status<500
    ? e.status===409?'原状态已变化，未自动重试。请核对后重新发起。':e.message
    : e instanceof Error&&action==='恢复草稿'?e.message:action+'是否成功尚未核实；不会自动重试。'
}

export function BenchmarkConversation({conversationID,run,onAccepted,onApplied,onRestore,archived,alreadyAccepted,initialRevision,initialStale,initialAppliedRevision}:{
  conversationID:string;run:Run;onAccepted:(taskID:string)=>void;onApplied:()=>void;onRestore:(taskID:string,title:string)=>Promise<void>;archived:boolean;alreadyAccepted:boolean
  initialRevision?:RevisionPreview;initialStale?:boolean;initialAppliedRevision?:number
}) {
  const [admission,setAdmission]=useState<AdmissionPreview>()
  const [revision,setRevision]=useState<RevisionPreview|undefined>(initialRevision)
  const [receipt,setReceipt]=useState(initialAppliedRevision?'方案修订已应用，当前修订 '+initialAppliedRevision+'。不是本次执行回执。':'')
  const [dispatchReceiptVersion,setDispatchReceiptVersion]=useState<number>()
  const [error,setError]=useState('')
  const [busy,setBusy]=useState(false)
  const [uncertain,setUncertain]=useState(false)
  const [interrupted,setInterrupted]=useState(false)
  const [retrySame,setRetrySame]=useState(false)
  const [stale,setStale]=useState(Boolean(initialStale))
  const request=useRef<string|undefined>(undefined)
  const lock=useRef(false)
  const autoPreview=useRef(false)
  const suggestion=run.benchmark_suggestion
  const revisionSuggestion=run.benchmark_revision_suggestion
  const taskID=revisionSuggestion?.task_id??''
  const {progress,error:progressError,refresh:refreshProgress}=useBenchmarkProgress(taskID,Boolean(taskID))
  const taskDispatch=useBenchmarkTaskDispatch(progress,refreshProgress,result=>{
    setRevision(undefined)
    setDispatchReceiptVersion(result.control_version)
    setReceipt(result.action==='pause'?'已暂停后续派发，已派发作业继续核对。':'已恢复后续派发，执行前仍会核对条件。')
    onApplied()
  })
  useEffect(()=>{
    if(dispatchReceiptVersion!==undefined&&progress?.dispatch_control&&progress.dispatch_control.control_version>dispatchReceiptVersion) {
      setDispatchReceiptVersion(undefined);setReceipt('')
    }
  },[dispatchReceiptVersion,progress?.dispatch_control?.control_version])
  const [taskDetail,setTaskDetail]=useState<Detail>()
  const [taskRun,setTaskRun]=useState<RunRecord>()
  useEffect(()=>{
    if(!taskID)return
    let active=true
    setTaskDetail(undefined);setTaskRun(undefined)
    api<Detail>('/tasks/'+encodeURIComponent(taskID)).then(detail=>{
      if(!active)return
      setTaskDetail(detail)
      if(detail.run?.id)void api<RunRecord>('/tasks/'+encodeURIComponent(taskID)+'/runs/'+encodeURIComponent(detail.run.id))
        .then(value=>{if(active)setTaskRun(value)}).catch(()=>undefined)
    }).catch(()=>undefined)
    return()=>{active=false}
  },[taskID])
  useEffect(()=>{
    if(!revisionSuggestion||archived||initialRevision||initialStale||initialAppliedRevision||autoPreview.current)return
    autoPreview.current=true
    previewRevision()
  },[run.id,archived])
  if(!suggestion&&!revisionSuggestion)return null
  const base='/conversations/'+encodeURIComponent(conversationID)
  async function act(action:string,go:()=>Promise<void>) {
    if(lock.current||archived)return
    lock.current=true;setBusy(true);setError('')
    try{await go()}catch(e){
      if(e instanceof APIError&&e.status===409) {
        setAdmission(undefined);setRevision(undefined)
        if(e.code==='benchmark_revision_request_interrupted')setInterrupted(true)
        else {setStale(true);void refreshProgress()}
      } else if(action==='预览'&&revisionSuggestion&&(!(e instanceof APIError)||e.status===undefined||e.status>=500)) {
        setUncertain(true)
      }
      setError(commandError(e,action))
    }finally{lock.current=false;setBusy(false)}
  }
  function previewAdmission() {
    void act('预览',async()=>{
      const value=await api<AdmissionPreview>(base+'/benchmark-suggestions/'+encodeURIComponent(run.id)+'/preview',{action:'preview_benchmark_suggestion'})
      if(value.state!=='preview_only'||value.execution_started||value.selection.target!==suggestion?.target)throw new Error('preview mismatch')
      setAdmission(value)
    })
  }
  function acceptAdmission() {
    if(!admission||!admission.preflight.can_queue||alreadyAccepted)return
    void act('受理',async()=>{
      const value=await api<{preview_id:string;task_id:string;state:string}>('/domain/benchmark-admission/preview/'+encodeURIComponent(admission.preview_id)+'/confirm',{action:'accept_benchmark_admission'})
      if(value.preview_id!==admission.preview_id||value.state!=='accepted'||!value.task_id)throw new Error('receipt mismatch')
      setReceipt('任务已进入队列，尚不代表执行已启动。');onAccepted(value.task_id)
    })
  }
  function previewRevision(restart=false) {
    void act('预览',async()=>{
      if(restart){request.current=crypto.randomUUID();setInterrupted(false)}
      setRetrySame(false)
      request.current??='revision-preview-'+run.id
      const value=await previewOnce(conversationID+':'+request.current,()=>api<RevisionPreview>(base+'/benchmark-revision-preview',{
        action:'preview_benchmark_revision',source_run_id:run.id,client_request_id:request.current,
      }))
      if(value.proposal.state!=='preview_only'||value.task_id!==revisionSuggestion?.task_id||value.conversation_id!==conversationID||
          value.source_run_id!==run.id||value.proposal.expected_revision!==revisionSuggestion.expected_revision)throw new Error('preview mismatch')
      setRevision(value)
    })
  }
  function checkRevisionRequest() {
    if(!request.current)return
    void act('核对',async()=>{
      let state:{state:string;preview_run_id?:string;can_start_new_request:boolean}
      try {state=await api(base+'/benchmark-revision-requests/'+encodeURIComponent(request.current!))}
      catch(e){
        if(e instanceof APIError&&e.status===404){setUncertain(false);setRetrySame(true);setError('未查到预览回执，可以按原请求重试。');return}
        throw e
      }
      if(state.state==='completed'&&state.preview_run_id){
        const value=await api<RevisionPreview>(base+'/benchmark-revision-preview/'+encodeURIComponent(state.preview_run_id))
        if(value.task_id!==taskID||value.conversation_id!==conversationID||value.source_run_id!==run.id||
          value.proposal.expected_revision!==revisionSuggestion?.expected_revision)throw new Error('preview mismatch')
        setRevision(value);setUncertain(false)
      } else if(state.state==='interrupted'&&state.can_start_new_request){
        setUncertain(false);setInterrupted(true);setError('上次预览未完成，可以重新预览。')
      } else setError('预览仍在处理中，请稍后再核对。')
    })
  }
  function restoreDraft() {
    void act('恢复草稿',async()=>{await onRestore(taskID,taskDetail?.task?.title??'基准任务');setError('已按最新方案版本恢复草稿，请核对后发送。')})
  }
  function applyRevision() {
    if(!revision)return
    void act('修订',async()=>{
      const value=await api<{preview_run_id:string;revision:number;state:string}>(base+'/benchmark-revision-preview/'+encodeURIComponent(revision.run_id)+'/confirm',{
        action:'apply_benchmark_revision',expected_revision:revision.proposal.expected_revision,
      })
      if(value.state!=='applied'||value.preview_run_id!==revision.run_id||value.revision!==revision.proposal.preview.revision)throw new Error('receipt mismatch')
      setReceipt('方案修订已应用，当前修订 '+value.revision+'。不是本次执行回执。');onApplied()
    })
  }
  const patch=revision?.proposal.preview
  const intent=revision?.proposal.interpretation
  const activeCase=progress?.cases.find(item=>(item.phase==='submitting'||item.phase==='following')&&(item.job_id||item.operation))??progress?.cases.find(item=>item.job_id||item.operation)
  const holds=progress?.cases.reduce((total,item)=>total+item.target_holds,0)??0
  const verified=progress?.cases.filter(item=>item.phase==='verified').length??0
  const model=taskRun?.contexts?.filter(item=>item.manifest?.requested_model||item.model_receipt).at(-1)
  const modelName=model?.model_receipt?.state==='reported'?model.model_receipt.reported_model:model?.manifest?.requested_model
  return <section className="conversation-tasks benchmark-conversation" aria-label={suggestion?'基准受理':'基准方案修订'}>
    <header className="benchmark-conversation-heading"><h2><Activity size={15} aria-hidden="true"/>{suggestion?'基准受理':taskDetail?.task?.title??'基准方案修订'}{revisionSuggestion&&<> · {taskID.slice(0,8)}</>}</h2>
      {revisionSuggestion&&<span className="benchmark-revision-badge">rev.{revisionSuggestion.expected_revision}</span>}
      {modelName&&<span className="benchmark-model-badge">{model?.model_receipt?.state==='reported'?'已回报':'请求模型'} · {modelName}</span>}
      {progress?.dispatch_control?.available&&!progressError&&<button type="button" className="outline benchmark-dispatch-button" disabled={busy||taskDispatch.busy||archived} onClick={()=>void taskDispatch.apply()}>
        {progress.dispatch_control.paused?<Play size={13} aria-hidden="true"/>:<Pause size={13} aria-hidden="true"/>}{taskDispatch.busy?'正在核对…':progress.dispatch_control.paused?'恢复后续派发':'暂停后续派发'}
      </button>}</header>
    {revisionSuggestion&&progressError&&<p role="alert" className="error">未能核对最新基准进度：{progressError}</p>}
    {revisionSuggestion&&progress?.dispatch_control?.unavailable_reason&&<p className="muted">{progress.dispatch_control.unavailable_reason}</p>}
    {taskDispatch.notice?.error&&<p role="alert" className="error">{taskDispatch.notice.text}</p>}
    {revisionSuggestion&&progress&&!progressError&&<div className="benchmark-status-grid">
      <div><span><CircleHelp size={12} aria-hidden="true"/>任务控制</span><strong>{benchmarkReadiness(progress)}</strong></div>
      <div><span><Cpu size={12} aria-hidden="true"/>远端作业</span><strong>{activeCase?(activeCase.job_id?'作业 '+activeCase.job_id+' · ':'')+(({submitting:'正在核对提交',following:'等待远端结果',verified:'执行证据已核实',incomplete:'核验未完成'} as Record<string,string>)[activeCase.phase]??activeCase.key):'暂无远端作业'}</strong></div>
      <div><span><Server size={12} aria-hidden="true"/>资源占用</span><strong>{holds?`持有记录 ${holds} 项`:'当前无本地占用记录'}</strong></div>
      <div><span><CircleHelp size={12} aria-hidden="true"/>结果证据</span><strong>{verified}/{progress.cases.length} 测项已核实</strong></div>
    </div>}
    {receipt?<p role="status">{receipt}</p>:suggestion
      ? alreadyAccepted?<p>该请求已受理，查看下方任务。</p>:<>
          <p>{suggestion.target} · {suggestion.profile.profile} v{suggestion.profile.version}</p>
          {!admission&&<div className="benchmark-conversation-actions"><button type="button" className="outline" disabled={busy||archived} onClick={previewAdmission}>查看受理预览</button></div>}
          {admission&&<><p>已冻结资产目标数 {admission.selection.visible_asset_refs?.length??0}；不代表当前可用节点或 GPU 数。</p>
            <p>预览不启动执行。{admission.preflight.can_queue?'通过入队检查；不代表现场或容量已验证。':'当前不能排队，请先检查预检结果。'}</p>
            {(admission.preflight.cases??[]).flatMap(item=>[...(item.blockers??[]),...(item.warnings??[])].map((issue,index)=>
              <p className="muted" key={item.key+':'+issue.code+':'+index}>{item.key}：{issue.detail}{issue.target&&'（'+issue.target+'）'}</p>))}
            <div className="benchmark-conversation-actions"><button type="button" className="primary" disabled={busy||archived||!admission.preflight.can_queue} onClick={acceptAdmission}><Check size={13} aria-hidden="true"/>受理并加入队列</button></div></>}
        </>
      : <>
          {!taskDetail?.task&&<p>任务 {revisionSuggestion?.task_id}{patch&&<> · 拟修订为 {patch.revision}</>}</p>}
          {!patch&&<div className="benchmark-conversation-actions">
            {uncertain?<button type="button" className="outline" disabled={busy||archived} onClick={checkRevisionRequest}>核对预览结果</button>:
              !stale&&<button type="button" className="outline" disabled={busy||archived} onClick={()=>previewRevision(interrupted)}>{interrupted?'重新预览':retrySame?'重试预览':'查看方案修订预览'}</button>}
            {(stale||error)&&<button type="button" className="outline" disabled={busy||archived} onClick={restoreDraft}>重取并编辑草稿</button>}
          </div>}
          {patch&&intent&&<><div className="benchmark-revision-panels">
            <div className="benchmark-revision-panel"><h3><HardDrive size={12} aria-hidden="true"/>1. 涉及资产</h3>
              {patch.asset_impact?<p title="冻结目标数不代表现场实时容量">{patch.asset_impact.baseline.target_refs?.join('、')||'冻结资产目标'} · {patch.asset_impact.baseline.target_count} 个目标{patch.asset_impact.scope_changed?` → ${patch.asset_impact.proposed.target_count} 个目标`:'（范围不变）'}</p>:
                <p>{progress?`${progress.asset_scope.target_count} 个冻结资产目标`:'资产范围待核对'}</p>}</div>
            <div className="benchmark-revision-panel"><h3><GitCommitHorizontal size={12} aria-hidden="true"/>2. 本次变更</h3>
              <ul>{Boolean(patch.changes.added?.length)&&<li>新增：{patch.changes.added.join('、')}</li>}
                {Boolean(patch.changes.removed?.length)&&<li>移除：{patch.changes.removed.join('、')}</li>}
                {Boolean(patch.changes.changed?.length)&&<li>修改：{patch.changes.changed.join('、')}{intent.parameter&&<>，{intent.parameter} → {intent.value}</>}</li>}
                {intent.change_expected_actual_config&&<li>同时修改实测要求。</li>}
                {patch.changes.order_changed&&<li>调整测试顺序。</li>}
                {!patch.changes.added?.length&&!patch.changes.removed?.length&&!patch.changes.changed?.length&&!intent.change_expected_actual_config&&!patch.changes.order_changed&&<li>没有方案内容变更。</li>}</ul></div>
            <div className="benchmark-revision-panel"><h3><ShieldCheck size={12} aria-hidden="true"/>3. 执行影响</h3><p>{Boolean(patch.frozen_cases?.length)&&<>已冻结：{patch.frozen_cases.join('、')}。 </>}不启动或停止作业，也不是本次跳过。</p></div>
          </div><div className="benchmark-conversation-actions"><button type="button" className="outline" disabled={busy} onClick={()=>setRevision(undefined)}>收起预览</button>
            <button type="button" className="primary" disabled={busy||archived} onClick={applyRevision}><Check size={13} aria-hidden="true"/>应用修订</button></div></>}
        </>}
    {busy&&<p role="status" className="muted">正在核对…</p>}
    {stale&&!error&&<p role="alert" className="error">方案版本或测项状态已变化，请重取并编辑草稿。</p>}
    {error&&<p role="alert" className="error">{error}</p>}
  </section>
}
