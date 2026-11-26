import {useCallback,useEffect,useId,useRef,useState} from 'react'
import {api} from './api'
import {AssistantSurfaceView} from './AssistantSurfaceView'
import {parseSurfaces,type Surface,type SurfaceAction} from './assistantSurface'
import {TaskRecordsDisclosure,type TaskRecordLink} from './TaskRecordsDisclosure'

type CardSpec={
  route:'task-card'|'asset-card'|'attention-card'|'artifact-card'|'skill-card'|'memory-card'|'report-card'|'schedule-card';title:string;action:string;target:'task_id'|'asset_slot'|'attention_slot'|'artifact_slot'|'skill_slot'|'memory_slot'|'report_slot'|'schedule_slot';signature:RegExp;button:RegExp
  mismatch:string;missing:string;checking:string;confirmed:string
}
const tasks:CardSpec={
  route:'task-card',title:'任务清单',action:'query_task_progress',target:'task_id',
  signature:/^taskcard_[a-f\d]+_[a-f\d]{16}_[a-f\d]{32}$/,button:/^progress_[a-f\d]{32}$/,
  mismatch:'任务清单或回执与原对话不符。',missing:'未收到这段对话的任务清单。',
  checking:'正在核对任务进度…',confirmed:'已收到这项任务的核对回执。',
}
const assets:CardSpec={
  route:'asset-card',title:'资产台账',action:'query_asset_ledger',target:'asset_slot',
  signature:/^assetcard_[a-f\d]+_[a-f\d]{16}_[a-f\d]{32}$/,button:/^asset_[a-f\d]{32}$/,
  mismatch:'资产台账或回执与原对话不符。',missing:'未收到这段对话的资产台账。',
  checking:'正在核对资产台账…',confirmed:'已收到这项资产的台账核对回执。',
}

const attention:CardSpec={
  route:'attention-card',title:'待处理事项',action:'query_attention_reason',target:'attention_slot',
  signature:/^attentioncard_[a-f\d]+_[a-f\d]{16}_[a-f\d]{32}$/,button:/^attention_[a-f\d]{32}$/,
  mismatch:'待处理清单或回执与原对话不符。',missing:'未收到这段对话的待处理清单。',
  checking:'正在核对待处理事项…',confirmed:'已收到这项待处理事项的核对回执。',
}

const artifacts:CardSpec={
  route:'artifact-card',title:'成果清单',action:'preview_artifact_text',target:'artifact_slot',
  signature:/^artifactcard_[a-f\d]+_[a-f\d]{16}_[a-f\d]{32}$/,button:/^artifact_[a-f\d]{32}$/,
  mismatch:'成果清单或回执与原对话不符。',missing:'未收到这段对话的成果清单。',
  checking:'正在核对成果…',confirmed:'已收到这项成果的预览回执。',
}

const skills:CardSpec={
  route:'skill-card',title:'Skill 绑定预览',action:'preview_conversation_skill',target:'skill_slot',
  signature:/^skillcard_[a-f\d]+_[a-f\d]{16}_[a-f\d]{32}$/,button:/^skillpreview_[a-f\d]{32}$/,
  mismatch:'Skill 预览与原对话不符。',missing:'未收到这段对话的 Skill 预览清单。',
  checking:'正在核对 Skill…',confirmed:'已取得预览回执，未保存会话配置。',
}

const memories:CardSpec={
  route:'memory-card',title:'记忆来源',action:'review_memory_provenance',target:'memory_slot',
  signature:/^memorycard_[a-f\d]+_[a-f\d]{16}_[a-f\d]{32}$/,button:/^memorysource_[a-f\d]{32}$/,
  mismatch:'记忆来源与原回复不符。',missing:'未收到这条回复的记忆来源。',
  checking:'正在核对记忆来源…',confirmed:'已收到核对结果。没有修改个人记忆。',
}

const reports:CardSpec={
  route:'report-card',title:'基准报告对比',action:'preview_benchmark_comparison',target:'report_slot',
  signature:/^reportcard_[a-f\d]+_[a-f\d]{16}_[a-f\d]{32}$/,button:/^reportcompare_[a-f\d]{32}$/,
  mismatch:'报告对比与原对话不符。',missing:'未收到这段对话的报告对比。',
  checking:'正在核对两份报告…',confirmed:'已收到核对结果。未启动测试或发布报告。',
}

const schedules:CardSpec={
  route:'schedule-card',title:'定时发现状态',action:'preview_discovery_schedule',target:'schedule_slot',
  signature:/^schedulecard_[a-f\d]+_[a-f\d]{16}_[a-f\d]{32}$/,button:/^scheduleview_[a-f\d]{32}$/,
  mismatch:'定时发现状态与原对话不符。',missing:'未收到这段对话的定时发现状态。',
  checking:'正在核对计划和回执…',confirmed:'已收到核对结果。没有修改计划或发起探测。',
}

const approvalPreviewAction='preview_pending_approval'
const supportedAction=(spec:CardSpec,action:string,component:string)=>
  action===spec.action&&spec.button.test(component)||
  spec.route==='attention-card'&&action===approvalPreviewAction&&/^approvalview_[a-f\d]{32}$/.test(component)

type SkillConfirmForm={
  run_id:string;slot:string;revision:number;expires_at_unix_ms:number;proof:string;label:string
}
type SkillBindingReceipt={
  conversation_id:string;run_id:string;slot:string;binding_revision:number;confirmed_at_unix_ms:number
}
type CardView={surface:Surface;recordTasks:TaskRecordLink[];skillConfirmations:SkillConfirmForm[];skillReceipt?:SkillBindingReceipt}

function checkedSkillReceipt(value:unknown,conversation:string,run:string):SkillBindingReceipt {
  if(!value||typeof value!=='object')throw new Error('无法核对 Skill 绑定回执。请先重新读取。')
  const receipt=value as SkillBindingReceipt
  if(receipt.conversation_id!==conversation||receipt.run_id!==run||typeof receipt.slot!=='string'||!/^([1-8])$/.test(receipt.slot)||
    !Number.isSafeInteger(receipt.binding_revision)||receipt.binding_revision<1||
    !Number.isSafeInteger(receipt.confirmed_at_unix_ms)||receipt.confirmed_at_unix_ms<1)
    throw new Error('无法核对 Skill 绑定回执。请先重新读取。')
  return receipt
}

function checkedReadCard(value:unknown,conversation:string,run:string,spec:CardSpec):CardView {
  if(!value||typeof value!=='object')throw new Error(spec.missing)
  const response=value as Record<string,unknown>
  const surfaces=parseSurfaces(response.surface)
  if(response.conversation_id!==conversation||response.run_id!==run||surfaces?.length!==1||
    !spec.signature.test(surfaces[0].id)||[...surfaces[0].nodes.values()].some(node=>
      node.component==='Button'&&!supportedAction(spec,node.action.event.name,node.id))){
    throw new Error(spec.mismatch)
  }
  const records=response.record_tasks??[]
  if(!Array.isArray(records)||records.length>10||(records.length>0&&spec.route!=='task-card'))throw new Error(spec.mismatch)
  const taskIDs=new Set<string>()
  const recordTasks=records.map((raw:unknown)=>{
    if(!raw||typeof raw!=='object')throw new Error(spec.mismatch)
    const ref=raw as TaskRecordLink
    if(typeof ref.task_id!=='string'||!(/^[a-f\d]{32}$/).test(ref.task_id)||taskIDs.has(ref.task_id)||
      surfaces[0].nodes.get('receipt_'+ref.task_id)?.component!=='Text'||typeof ref.label!=='string'||!ref.label||ref.label.length>400)
      throw new Error(spec.mismatch)
    taskIDs.add(ref.task_id)
    return ref
  })
  const forms=response.binding_confirmations??[]
  if(!Array.isArray(forms)||forms.length>8||(forms.length>0&&spec.route!=='skill-card'))
    throw new Error(spec.mismatch)
  const seen=new Set<string>()
  const skillConfirmations=forms.map((raw:unknown)=>{
    if(!raw||typeof raw!=='object')throw new Error(spec.mismatch)
    const f=raw as SkillConfirmForm
    if(f.run_id!==run||typeof f.slot!=='string'||!/^([1-8])$/.test(f.slot)||seen.has(f.slot)||
      !Number.isSafeInteger(f.revision)||f.revision<0||!Number.isSafeInteger(f.expires_at_unix_ms)||
      typeof f.proof!=='string'||!(/^[a-f\d]{32}$/).test(f.proof)||typeof f.label!=='string'||!f.label||f.label.length>360)
      throw new Error(spec.mismatch)
    seen.add(f.slot)
    return f
  })
  const receipt=response.binding_receipt
  if(receipt!=null&&(spec.route!=='skill-card'||skillConfirmations.length>0))throw new Error(spec.mismatch)
  const skillReceipt=receipt==null?undefined:checkedSkillReceipt(receipt,conversation,run)
  return {surface:surfaces[0],recordTasks,skillConfirmations,skillReceipt}
}

type CardLocation={conversation:string;run:string}
export function TaskListProgressCard(props:CardLocation){return <PersonalReadCard {...props} spec={tasks}/>}
export function AssetLedgerCard(props:CardLocation){return <PersonalReadCard {...props} spec={assets}/>}
export function AttentionListCard(props:CardLocation){return <PersonalReadCard {...props} spec={attention}/>}
export function ArtifactPreviewCard(props:CardLocation){return <PersonalReadCard {...props} spec={artifacts}/>}
export function SkillBindingPreviewCard(props:CardLocation){return <PersonalReadCard {...props} spec={skills}/>}
export function MemoryProvenanceCard(props:CardLocation){return <PersonalReadCard {...props} spec={memories}/>}
export function BenchmarkComparisonCard(props:CardLocation){return <PersonalReadCard {...props} spec={reports}/>}
export function DiscoveryScheduleCard(props:CardLocation){return <PersonalReadCard {...props} spec={schedules}/>}

function PersonalReadCard({conversation,run,spec}:CardLocation&{spec:CardSpec}){
  const [view,setView]=useState<CardView>()
  const formPrefix=useId()
  const surface=view?.surface
  const [busy,setBusy]=useState(false)
  const [error,setError]=useState('')
  const [notice,setNotice]=useState('')
  const lock=useRef(false)
  const epoch=useRef(0)
  const base='/conversations/'+encodeURIComponent(conversation)+'/'+spec.route+'/'+encodeURIComponent(run)
  const restore=useCallback(async()=>{
    const current=++epoch.current
    setError('');setNotice('')
    try{
      const value=checkedReadCard(await api(base),conversation,run,spec)
      if(epoch.current===current)setView(value)
    }catch(e){
      if(epoch.current===current){setView(undefined);setError(e instanceof Error?e.message:'无法读取'+spec.title+'。')}
    }
  },[base,conversation,run,spec])
  useEffect(()=>{setView(undefined);setBusy(false);void restore();return()=>{epoch.current++}},[restore])
  useEffect(()=>{
    const assetLedger=spec.route==='asset-card'
    if(!assetLedger&&spec.route!=='memory-card'&&spec.route!=='report-card'&&spec.route!=='schedule-card')return
    // A cached ledger is not continuing authority. Invalidate outstanding
    // replies when it becomes invisible or loses its connection, then
    // retrieve the current authorized view without resending an action.
    const recheck=()=>{
      const offline=assetLedger&&!navigator.onLine
      if(document.visibilityState==='hidden'||offline){
        epoch.current++;setView(undefined);setBusy(false);setNotice('')
        if(assetLedger)setError(offline?'当前离线，重新联网后核对资产台账。':'')
      }else void restore()
    }
    document.addEventListener('visibilitychange',recheck)
    if(assetLedger){
      window.addEventListener('offline',recheck)
      window.addEventListener('online',recheck)
    }
    return()=>{
      document.removeEventListener('visibilitychange',recheck)
      if(assetLedger){
        window.removeEventListener('offline',recheck)
        window.removeEventListener('online',recheck)
      }
    }
  },[spec,restore])
  async function inspect(action:SurfaceAction) {
    const {name,sourceComponentId}=action.action
    if(lock.current||surface?.id!==action.action.surfaceId||!supportedAction(spec,name,sourceComponentId))return
    const button=surface.nodes.get(sourceComponentId)
    if(button?.component!=='Button'||button.action.event.name!==name||button.action.event.context?.[spec.target]!==action.action.context[spec.target])return
    const current=epoch.current
    lock.current=true
    setBusy(true);setError('');setNotice('')
    try {
      const preview=name===approvalPreviewAction
      const value=checkedReadCard(await api(base+(preview?'/approval-preview':'/actions'),action,'POST',{'X-ClusBot-Request-Key':crypto.randomUUID()}),conversation,run,spec)
      if(epoch.current===current){setView(value);setNotice(preview?'已收到审批预览回执，未提交决定。':spec.confirmed)}
    }catch(e){
      if(epoch.current===current){setView(undefined);setError(e instanceof Error?e.message:'未收到回执，请先重新读取。')}
    }finally {
      lock.current=false
      if(epoch.current===current)setBusy(false)
    }
  }
  async function confirmSkill(f:SkillConfirmForm) {
    if(lock.current||spec.route!=='skill-card'||!view?.skillConfirmations.includes(f))return
    const current=epoch.current
    lock.current=true
    setBusy(true);setError('');setNotice('')
    try{
      const {label:_,...preview_confirmation}=f
      const result=checkedSkillReceipt(await api('/conversations/'+encodeURIComponent(conversation)+'/skill-binding',{
        action:'confirm_skill_preview',preview_confirmation,
      },'PUT'),conversation,run)
      const updated=checkedReadCard(await api(base),conversation,run,spec)
      if(result.slot!==f.slot||updated.skillReceipt?.slot!==f.slot||updated.skillReceipt.binding_revision!==result.binding_revision||
        updated.skillReceipt.confirmed_at_unix_ms!==result.confirmed_at_unix_ms)
        throw new Error('无法核对 Skill 绑定回执。请先重新读取。')
      if(epoch.current===current){
        setView(updated)
        setNotice('已收到绑定回执。不会改变已开始或已完成的任务。')
      }
    }catch(e){
      if(epoch.current===current){
        setView(undefined)
        setError(e instanceof Error?e.message:'没有收到绑定回执，请先重新读取。')
      }
    }finally{
      lock.current=false
      if(epoch.current===current)setBusy(false)
    }
  }
  return <article className="assistant-card" data-read-card={spec.route} aria-label={'对话'+spec.title}>
    <div aria-busy={busy}>
      {surface?<AssistantSurfaceView key={surface.id} surface={surface} busy={busy} onAgentAction={error?undefined:inspect}/>:!error&&<p>正在读取{spec.title}…</p>}
      {view?.recordTasks.map(task=><TaskRecordsDisclosure key={task.task_id} conversation={conversation} run={run} task={task}/>)}
      {view?.skillConfirmations.map(f=><form key={f.slot} method="post" action={'/api/v1/conversations/'+encodeURIComponent(conversation)+'/skill-binding'}
        aria-labelledby={formPrefix+'-'+f.slot} onSubmit={event=>{event.preventDefault();void confirmSkill(f)}}>
        <p id={formPrefix+'-'+f.slot}>选用 {f.label}，仅用于此会话之后范围匹配的任务。</p>
        <footer><button type="submit" className="primary" disabled={busy}>确认绑定：{f.label}</button></footer>
      </form>)}
    </div>
    {busy&&<p>{spec.checking}</p>}
    <div role="status">{notice&&<p>{notice}</p>}</div>
    {error&&<><p role="alert" className="error">{error}</p><button type="button" className="text-button" disabled={busy} onClick={()=>void restore()}>重新读取{spec.title}与回执</button></>}
  </article>
}
