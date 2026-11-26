import {useEffect,useRef,useState} from 'react'
import {Activity,Check,CircleHelp,Cpu,GitCommitHorizontal,HardDrive,Pause,Play,Server,ShieldCheck} from 'lucide-react'
import {api,APIError,type Detail,type Run,type RunRecord} from './api'
import {benchmarkReadiness,useBenchmarkProgress,useBenchmarkTaskDispatch} from './BenchmarkExecution'
import {benchmarkSurfacePath,benchmarkPreviewSurfaces,benchmarkSyncActualAction,checkedBenchmarkPreview,previewBenchmarkSurface,readBenchmarkRevisionApplied,
  readBenchmarkRevisionSnapshot,type RevisionPreview,type Surface,type SurfaceAction} from './assistantSurface'
import {AssistantSurfaceView} from './AssistantSurfaceView'
import './assistant-cards.css'
import './BenchmarkDispatchControl.css'

export type {RevisionPreview} from './assistantSurface'

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

export function BenchmarkConversation({conversationID,run,onApplied,onRestore,archived,initialRevision,initialStale,initialAppliedRevision}:{
  conversationID:string;run:Run;onApplied:()=>void;onRestore:(taskID:string,title:string)=>Promise<void>;archived:boolean
  initialRevision?:RevisionPreview;initialStale?:boolean;initialAppliedRevision?:number
}) {
  const [revision,setRevision]=useState<RevisionPreview|undefined>(initialRevision)
  const [revisionCollapsed,collapseRevision]=useState(false)
  const [applyState,setApplyState]=useState<'ready'|'uncertain'|'retry'>('ready')
  const revisionHeading=useRef<HTMLHeadingElement>(null)
  const focusRevision=useRef(false)
  useEffect(()=>{
    if(revision&&!revisionCollapsed&&focusRevision.current){
      focusRevision.current=false
      revisionHeading.current?.focus({preventScroll:true})
    }
  },[revision,revisionCollapsed])
  const [receipt,setReceipt]=useState(initialAppliedRevision?'方案修订已应用，提交为修订 #'+initialAppliedRevision+'。不是本次执行回执。':'')
  const [dispatchReceipt,setDispatchReceipt]=useState<{controlVersion:number;text:string}>()
  const [error,setError]=useState('')
  const [busy,setBusy]=useState(false)
  const [uncertain,setUncertain]=useState(false)
  const [interrupted,setInterrupted]=useState(false)
  const [previewSurfaces,setPreviewSurfaces]=useState<{preview:Surface;sync:Surface}>()
  const [syncChoiceOpen,setSyncChoiceOpen]=useState(false)
  const [surfaceEpoch,refreshSurface]=useState(0)
  const [stale,setStale]=useState(Boolean(initialStale))
  const request=useRef<{key:string;sync:boolean}|undefined>(undefined)
  const lock=useRef(false)
  const revisionSuggestion=run.benchmark_revision_suggestion
  const taskID=revisionSuggestion?.task_id??''
  const {progress,error:progressError,refresh:refreshProgress}=useBenchmarkProgress(taskID,Boolean(taskID))
  const taskDispatch=useBenchmarkTaskDispatch(progress,refreshProgress,result=>{
    // Execution controls have their own receipt and must not erase a pending
    // revision readback or replace a persisted plan-change acknowledgement.
    setDispatchReceipt({controlVersion:result.control_version,
      text:result.action==='pause'?'已暂停后续派发，已派发作业继续核对。':'已恢复后续派发，执行前仍会核对条件。'})
    onApplied()
  })
  useEffect(()=>{
    if(dispatchReceipt&&progress?.dispatch_control&&progress.dispatch_control.control_version>dispatchReceipt.controlVersion)
      setDispatchReceipt(undefined)
  },[dispatchReceipt,progress?.dispatch_control?.control_version])
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
    let active=true
    setPreviewSurfaces(undefined)
    if(!revisionSuggestion||archived||initialStale||initialAppliedRevision)return
    const path=benchmarkSurfacePath(conversationID,run.id)
    if(!path)return
    // Loading a conversation, including historical messages, must not ask a
    // model, create a preview, dispatch a benchmark, or apply a revision.
    void api<unknown>(path).then(receipt=>{
      const surfaces=benchmarkPreviewSurfaces(receipt,conversationID,run.id,taskID,revisionSuggestion.expected_revision)
      if(!surfaces)throw new Error('无法核对修订操作。')
      if(active){setPreviewSurfaces(surfaces);setError('')}
    }).catch(()=>{if(active)setError('未能加载修订操作，没有调用模型。')})
    return()=>{active=false}
  },[conversationID,run.id,taskID,revisionSuggestion?.expected_revision,archived,initialStale,initialAppliedRevision,surfaceEpoch])
  if(!revisionSuggestion)return null
  const base='/conversations/'+encodeURIComponent(conversationID)
  async function act(action:string,go:()=>Promise<void>) {
    if(lock.current||archived)return
    lock.current=true;setBusy(true);setError('')
    try{await go()}catch(e){
      if(e instanceof APIError&&e.status===409) {
        setRevision(undefined);setApplyState('ready')
        if(e.code==='a2ui_surface_stale')refreshSurface(n=>n+1)
        else if(e.code==='benchmark_revision_request_interrupted')setInterrupted(true)
        else {setStale(true);void refreshProgress()}
      } else if(e instanceof APIError&&e.code==='benchmark_revision_actual_consent_required'&&!request.current?.sync) {
        setSyncChoiceOpen(true)
      } else if((!(e instanceof APIError)||e.status===undefined||e.status>=500)) {
        if(action==='预览'&&revisionSuggestion)setUncertain(true)
        if(action==='修订'){
          setApplyState('uncertain')
          setError('尚未收到应用回执。请核对原预演，不会自动重试。')
          return
        }
      }
      setError(commandError(e,action))
    }finally{lock.current=false;setBusy(false)}
  }
  function previewRevision(action:SurfaceAction) {
    void act('预览',async()=>{
      if(!revisionSuggestion)return
      const sync=action.action.name===benchmarkSyncActualAction
      if(!request.current||request.current.sync!==sync)
        request.current={key:(sync?'revision-sync-actual-':'revision-preview-')+run.id,sync}
      else if(interrupted)request.current={key:crypto.randomUUID(),sync}
      const {key}=request.current
      setInterrupted(false)
      const value=await previewOnce(conversationID+':'+key,()=>previewBenchmarkSurface(
        api,conversationID,run.id,taskID,revisionSuggestion.expected_revision,action,key))
      // A replay intentionally omits the old local occupancy snapshot.
      // Recover the immutable receipt and refresh read-only facts, not the model.
      if(!value.proposal.preview.asset_impact){
        const applied=await readBenchmarkRevisionApplied(api,value)
        if(applied!==undefined){showApplied(applied);return}
        showRevision(await readBenchmarkRevisionSnapshot(api,value))
      } else showRevision(value)
    })
  }
  function checkRevisionRequest() {
    if(!request.current)return
    const {key,sync}=request.current
    void act('核对',async()=>{
      let state:{state:string;preview_run_id?:string;can_start_new_request:boolean}
      try {state=await api(base+'/benchmark-revision-requests/'+encodeURIComponent(key))}
      catch(e){
        if(e instanceof APIError&&e.status===404){setUncertain(false);setError('未查到预览回执，可以按原请求重试。');return}
        throw e
      }
      if(state.state==='completed'&&state.preview_run_id){
        if(!revisionSuggestion)throw new Error('无法核对原建议')
        const raw=await api<unknown>(base+'/benchmark-revision-preview/'+encodeURIComponent(state.preview_run_id))
        const value=checkedBenchmarkPreview(raw,conversationID,run.id,taskID,revisionSuggestion.expected_revision)
        if(value.run_id!==state.preview_run_id||(value.requested_action===benchmarkSyncActualAction)!==sync)throw new Error('预演回执不匹配')
        showRevision(value);setUncertain(false)
      } else if(state.state==='interrupted'&&state.can_start_new_request){
        setUncertain(false);setInterrupted(true);setError('上次预览未完成，可以重新预览。')
      } else setError('预览仍在处理中，请稍后再核对。')
    })
  }
  function restoreDraft() {
    void act('恢复草稿',async()=>{await onRestore(taskID,taskDetail?.task?.title??'基准任务');setError('已按最新方案版本恢复草稿，请核对后发送。')})
  }
  function showRevision(value:RevisionPreview) {
    focusRevision.current=true
    collapseRevision(false)
    setRevision(value)
  }
  function showApplied(version:number) {
    setApplyState('ready')
    setReceipt('方案修订已应用，提交为修订 #'+version+'。不是本次执行回执。')
    void refreshProgress()
    onApplied()
  }
  function checkAppliedRevision() {
    if(!revision||applyState!=='uncertain')return
    const original=revision
    void act('核对修订结果',async()=>{
      const stored=await readBenchmarkRevisionApplied(api,original)
      if(stored!==undefined){showApplied(stored);return}
      // Absence alone is not proof the first request failed or never arrived.
      // Permit only a deliberate retry of the same stored, still-valid draft.
      try {
        setRevision(await readBenchmarkRevisionSnapshot(api,original))
      } catch(e) {
        if(e instanceof APIError&&e.status===409){
          const committed=await readBenchmarkRevisionApplied(api,original)
          if(committed!==undefined){showApplied(committed);return}
        }
        throw e
      }
      setApplyState('retry')
    })
  }
  function applyRevision() {
    if(!revision?.proposal.preview.asset_impact||applyState==='uncertain'||taskDispatch.busy)return
    void act('修订',async()=>{
      const value=await api<{preview_run_id:string;revision:number;state:string}>(base+'/benchmark-revision-preview/'+encodeURIComponent(revision.run_id)+'/confirm',{
        action:'apply_benchmark_revision',expected_revision:revision.proposal.expected_revision,
      })
      if(value.state!=='applied'||value.preview_run_id!==revision.run_id||value.revision!==revision.proposal.preview.revision)throw new Error('receipt mismatch')
      showApplied(value.revision)
    })
  }
  const patch=revision?.proposal.preview
  const intent=revision?.proposal.interpretation
  const parameter=revision?.proposal.parameter_change
  const impact=patch?.asset_impact
  const activeCase=progress?.cases.find(item=>(item.phase==='submitting'||item.phase==='following')&&(item.job_id||item.operation))??progress?.cases.find(item=>item.job_id||item.operation)
  const holds=progress?.cases.reduce((total,item)=>total+item.target_holds,0)??0
  const verified=progress?.cases.filter(item=>item.phase==='verified').length??0
  const model=taskRun?.contexts?.filter(item=>item.manifest?.requested_model||item.model_receipt).at(-1)
  const modelName=model?.model_receipt?.state==='reported'?model.model_receipt.reported_model:model?.manifest?.requested_model
  return <section className="conversation-tasks benchmark-conversation" aria-label="基准方案修订">
    <header className="benchmark-conversation-heading"><h2><Activity size={15} aria-hidden="true"/>{taskDetail?.task?.title??'基准方案修订'}{revisionSuggestion&&<> · {taskID.slice(0,8)}</>}</h2>
      {revisionSuggestion&&<span className="benchmark-revision-badge">方案修订 #{revisionSuggestion.expected_revision}</span>}
      {patch&&!receipt&&applyState==='ready'&&<button type="button" className="outline" disabled={busy} aria-expanded={!revisionCollapsed}
        onClick={()=>collapseRevision(value=>!value)}>{revisionCollapsed?'展开本次预演':'收起预演'}</button>}
      {modelName&&<span className="benchmark-model-badge">{model?.model_receipt?.state==='reported'?'已回报':'请求模型'} · {modelName}</span>}
      {progress?.dispatch_control?.available&&!progressError&&<button type="button" className="outline benchmark-dispatch-button" disabled={busy||taskDispatch.busy||archived} onClick={()=>void taskDispatch.apply()}>
        {progress.dispatch_control.paused?<Play size={13} aria-hidden="true"/>:<Pause size={13} aria-hidden="true"/>}{taskDispatch.busy?'正在核对…':progress.dispatch_control.paused?'恢复后续派发':'暂停后续派发'}
      </button>}</header>
    {revisionSuggestion&&progressError&&<p role="alert" className="error">未能核对最新基准进度：{progressError}</p>}
    {revisionSuggestion&&progress?.dispatch_control?.unavailable_reason&&<p className="muted">{progress.dispatch_control.unavailable_reason}</p>}
    {taskDispatch.notice?.error&&<p role="alert" className="error">{taskDispatch.notice.text}</p>}
    {dispatchReceipt&&<p role="status">{dispatchReceipt.text}</p>}
    {revisionSuggestion&&progress&&!progressError&&<div className="benchmark-status-grid">
      <div><span><CircleHelp size={12} aria-hidden="true"/>任务控制</span><strong>{benchmarkReadiness(progress)}</strong></div>
      <div><span><Cpu size={12} aria-hidden="true"/>远端作业</span><strong>{activeCase?(activeCase.job_id?'作业 '+activeCase.job_id+' · ':'')+(({submitting:'正在核对提交',following:'等待远端结果',verified:'执行证据已核实',incomplete:'核验未完成'} as Record<string,string>)[activeCase.phase]??activeCase.key):'暂无远端作业'}</strong></div>
      <div><span><Server size={12} aria-hidden="true"/>资源占用</span><strong>{holds?`持有记录 ${holds} 项`:'当前无本地占用记录'}</strong></div>
      <div><span><CircleHelp size={12} aria-hidden="true"/>结果证据</span><strong>{verified}/{progress.cases.length} 测项已核实</strong></div>
    </div>}
    {receipt?<p role="status">{receipt}</p>:<>
          {!taskDetail?.task&&<p>任务 {revisionSuggestion?.task_id}{patch&&<> · 拟修订为 {patch.revision}</>}</p>}
          {!patch&&<div className="benchmark-conversation-actions">
            {uncertain?<button type="button" className="outline" disabled={busy||archived} onClick={checkRevisionRequest}>核对预览结果</button>:
              !stale&&!archived&&(previewSurfaces?<div>
                <AssistantSurfaceView key={previewSurfaces.preview.id} surface={previewSurfaces.preview} busy={busy} onAgentAction={previewRevision}/>
                <details open={syncChoiceOpen} onToggle={event=>setSyncChoiceOpen(event.currentTarget.open)}>
                  <summary>执行参数与实测核对值</summary>
                  <p>只预演原要求中的一处参数修改。核对值随该参数更新，不改验收阈值，不运行测试。</p>
                  <AssistantSurfaceView key={previewSurfaces.sync.id} surface={previewSurfaces.sync} busy={busy} onAgentAction={previewRevision}/>
                </details></div>:
                error?<button type="button" className="outline" onClick={()=>refreshSurface(n=>n+1)}>重新加载修订操作</button>:<span className="muted">正在加载修订操作…</span>)}
            {(stale||error)&&<button type="button" className="outline" disabled={busy||archived} onClick={restoreDraft}>重取并编辑草稿</button>}
          </div>}
          {patch&&intent&&!revisionCollapsed&&<><div className="benchmark-revision-panels">
            <div className="benchmark-revision-panel"><h3 ref={revisionHeading} tabIndex={-1}><HardDrive size={12} aria-hidden="true"/>1. 涉及资产</h3>
              {impact?<><p><strong>{impact.baseline.target_refs.slice(0,3).join('、')}{impact.baseline.target_count>3?'等':''} · {impact.baseline.target_count} 个冻结资产目标</strong>{impact.scope_changed?
                <> → {impact.proposed.target_count} 个目标（范围缩小）</>:'（范围不变）'}。</p>
                {impact.removed_refs.length>0&&<p>不再列入方案：{impact.removed_refs.slice(0,3).join('、')}{impact.removed_refs.length>3?`等 ${impact.removed_refs.length} 个`:''}</p>}
                <p className="muted">节点与加速卡数量未核实。</p>
                <details><summary>查看冻结目标与测项范围</summary>
                  <p>原方案：{impact.baseline.target_refs.join('、')||'无'}</p><p>修改后：{impact.proposed.target_refs.join('、')||'无'}</p>
                  {impact.removed_refs.length>3&&<p>移出范围：{impact.removed_refs.join('、')}</p>}
                  <ul>{impact.changed_cases.map(item=><li key={item.case_key}>{item.case_key}：{item.before_refs.join('、')||'无'} → {item.after_refs.join('、')||'无'}</li>)}</ul>
                </details></>:<p>未能核对本次预演的资产范围，不能应用。</p>}</div>
            <div className="benchmark-revision-panel"><h3><GitCommitHorizontal size={12} aria-hidden="true"/>2. 本次变更</h3>
              <ul>{Boolean(patch.changes.added?.length)&&<li>新增：{patch.changes.added.join('、')}</li>}
                {Boolean(patch.changes.removed?.length)&&<li>移除：{patch.changes.removed.join('、')}</li>}
                {Boolean(patch.changes.changed?.length)&&<li>修改：{patch.changes.changed.join('、')}{intent.parameter&&parameter&&<> · {intent.parameter}：{parameter.before} → {parameter.after}</>}</li>}
                {parameter?.expected_after&&<li>同名实测核对值：{parameter.expected_before} → {parameter.expected_after}。验收指标和阈值不变。</li>}
                {patch.changes.order_changed&&<li>调整测试顺序。</li>}
                {!patch.changes.added?.length&&!patch.changes.removed?.length&&!patch.changes.changed?.length&&!intent.change_expected_actual_config&&!patch.changes.order_changed&&<li>没有方案内容变更。</li>}</ul></div>
            <div className="benchmark-revision-panel"><h3><ShieldCheck size={12} aria-hidden="true"/>3. 执行影响</h3>
              <p>{Boolean(patch.frozen_cases?.length)&&<>已冻结：{patch.frozen_cases.join('、')}。 </>}不启动或停止作业，也不是本次跳过。</p>
              {impact&&<>{impact.current_local_holds.length>0?<><p>预演时的本地占用（不代表远端状态）：</p>
                <ul>{impact.current_local_holds.map(item=><li key={item.case_key}>{item.case_key}：{item.target_refs.join('、')}</li>)}</ul></>:
                <p>预演时没有本地占用记录，不代表现场空闲。</p>}</>}</div>
          </div><div className="benchmark-conversation-actions">
            {impact&&<button type="button" className={applyState==='uncertain'?'outline':'primary'} disabled={busy||taskDispatch.busy||archived}
              onClick={applyState==='uncertain'?checkAppliedRevision:applyRevision}>
              {applyState==='uncertain'?<CircleHelp size={13} aria-hidden="true"/>:<Check size={13} aria-hidden="true"/>}
              {applyState==='uncertain'?'核对修订结果':applyState==='retry'?'按原预演重试':'应用修订'}
            </button>}
          </div>{applyState==='retry'&&<p role="status">暂未找到应用回执，原预演仍有效。可按原预演重试，不会创建新请求。</p>}</>}
        </>}
    {busy&&<p className="muted">正在核对…</p>}
    {stale&&!error&&<p role="alert" className="error">方案版本或测项状态已变化，请重取并编辑草稿。</p>}
    {error&&<p role="alert" className="error">{error}</p>}
  </section>
}
