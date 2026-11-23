import {useCallback,useEffect,useId,useRef,useState,type KeyboardEvent,type ReactNode} from 'react'
import {ArrowDownToLine,ChevronRight,FileText,LoaderCircle,MessageSquareQuote,Pause,Pencil,Play} from 'lucide-react'
import {activeRun,api,awaitingPublication,type Artifact,type Detail,type RunRecord,type Task} from './api'
import {benchmarkReadiness,useBenchmarkProgress,useBenchmarkTaskDispatch} from './BenchmarkExecution'
import type {TaskReference} from './composerDrafts'
import {FormattedText} from './ExecutionText'
import {displayDate} from './GeneralPreferences'
import {RunHistory,RunModelProvenance} from './RunHistory'
import {TaskEditor} from './TaskEditor'
import {ReadEvidence} from './TaskRead'
import './ChatTaskDrawer.css'

type Tab='overview'|'runs'|'artifacts'|'receipts'
const tabs:{id:Tab;label:string}[]=[
  {id:'overview',label:'概览'}, {id:'runs',label:'运行'},
  {id:'artifacts',label:'产物'}, {id:'receipts',label:'回执'},
]
const publicationLabels:Record<string,string>={
  pending:'待确认',dispatched:'正在写入',unknown:'结果待核对',succeeded:'已写入',denied:'已拒绝',
  conflict:'文件版本已变化',failed:'写入失败',expired:'确认已过期',
}
const caseLabels:Record<string,string>={
  queued:'未派发',submitting:'正在核对提交',following:'等待远端结果',verified:'执行证据已核实',
  incomplete:'核验未完成',skipped:'未执行',
}
const number=new Intl.NumberFormat('zh-CN',{maximumFractionDigits:1})
const when=(value:number)=>displayDate(value,{month:'numeric',day:'numeric',hour:'2-digit',minute:'2-digit'})
const errorText=(error:unknown)=>error instanceof Error?error.message:'请求未完成'

function statusText(task:Task,data:Detail) {
  if(task.status_reason==='stop_requested')return '停止请求处理中'
  if(task.status_reason==='pause_requested')return '暂停请求处理中'
  if(task.status_reason==='benchmark_dispatch_paused')return '已暂停后续派发'
  if(awaitingPublication(data.publication))return ({pending:'待确认写入',dispatched:'正在保存成果',unknown:'写入待核对'} as Record<string,string>)[data.publication?.status??'']||task.reason
  if(task.status==='active')return task.status_reason==='waiting_dependencies'?'等待依赖成果':task.status_reason==='queued'?'等待执行':'正在处理'
  if(task.status==='waiting_user')return data.run?.status==='input_required'?'等你补充':task.required_filename&&(data.publication?.status!=='succeeded'||data.publication.filename!==task.required_filename)?'待保存成果':'等你验收'
  if(task.status==='interrupted')return task.status_reason==='paused'?'已暂停':task.requirements_changed?'待按新要求执行':'执行中断'
  return task.status==='completed'?'已完成':'已停止'
}

function artifactPath(task:Task,id:string) {
  const query=new URLSearchParams({task_id:task.id,scope_label:task.scope_label})
  return '/artifacts/'+encodeURIComponent(id)+'?'+query
}

export function ChatTaskDrawer({taskID,children,onChange,navigate,sourceConversationID,onReference,closeAction,readOnly=false}:{
  taskID:string;children?:ReactNode;onChange:()=>void;navigate:(path:string)=>void
  sourceConversationID?:string;onReference?:(task:TaskReference)=>void;closeAction?:ReactNode;readOnly?:boolean
}) {
  const [data,setData]=useState<Detail>()
  const [error,setError]=useState('')
  const [record,setRecord]=useState<RunRecord>()
  const [recordError,setRecordError]=useState('')
  const [tab,setTab]=useState<Tab>('overview')
  const [selectedRun,setSelectedRun]=useState('')
  const [detailsOpen,setDetailsOpen]=useState(false)
  const [preview,setPreview]=useState(false)
  const [editing,setEditing]=useState(false)
  const [pending,setPending]=useState(false)
  const [actionError,setActionError]=useState('')
  const sequence=useRef(0)
  const list=useRef<HTMLDivElement>(null)
  const id=useId()
  const {progress:benchmark,unavailable:ordinary,error:benchmarkError,refresh:refreshBenchmark}=useBenchmarkProgress(taskID)
  const reload=useCallback(async()=>{
    const request=++sequence.current
    try {
      const next=await api<Detail>('/tasks/'+encodeURIComponent(taskID))
      if(request===sequence.current){setData(next);setError('')}
    }catch(error){if(request===sequence.current)setError(errorText(error))}
  },[taskID])
  const taskDispatch=useBenchmarkTaskDispatch(benchmark,refreshBenchmark,()=>{void reload();onChange()})
  useEffect(()=>{void reload();return()=>{sequence.current++}},[reload])
  useEffect(()=>{
    if(!activeRun(data?.run)&&!awaitingPublication(data?.publication)&&!detailsOpen)return
    const timer=window.setInterval(()=>{if(!document.hidden)void reload()},2500)
    return()=>window.clearInterval(timer)
  },[data?.run?.status,data?.publication?.status,detailsOpen,reload])
  const task=data?.task
  const run=data?.run
  useEffect(()=>{
    if(!run?.id){setRecord(undefined);setRecordError('');return}
    let cancelled=false
    setRecord(undefined);setRecordError('')
    api<RunRecord>('/tasks/'+encodeURIComponent(taskID)+'/runs/'+encodeURIComponent(run.id))
      .then(result=>{if(!cancelled)setRecord(result)})
      .catch(error=>{if(!cancelled)setRecordError(errorText(error))})
    return()=>{cancelled=true}
  },[taskID,run?.id,run?.status,run?.execution_generation,task?.revision])

  function chooseTab(next:Tab) {
    setTab(next)
    if(next==='runs'&&!selectedRun&&run?.id)setSelectedRun(run.id)
  }
  function navigateTabs(event:KeyboardEvent<HTMLDivElement>) {
    const items=Array.from(list.current?.querySelectorAll<HTMLButtonElement>('[role="tab"]')??[])
    const current=items.indexOf(document.activeElement as HTMLButtonElement)
    if(current<0)return
    let next=current
    if(event.key==='ArrowRight')next=(current+1)%items.length
    else if(event.key==='ArrowLeft')next=(current+items.length-1)%items.length
    else if(event.key==='Home')next=0
    else if(event.key==='End')next=items.length-1
    else return
    event.preventDefault();items[next]?.focus()
  }
  async function pause() {
    if(!task||pending)return
    setPending(true);setActionError('')
    try {await api('/tasks/'+encodeURIComponent(taskID)+'/pause',{revision:task.revision});await Promise.all([reload(),refreshBenchmark()]);onChange()}
    catch(error){setActionError(errorText(error));await reload()}
    finally{setPending(false)}
  }
  if(!data||!task)return <div className="chat-task-view chat-task-view--loading"><div className="chat-task-view__close">{closeAction}</div>{error?<p role="alert">{error}</p>:<LoaderCircle className="spin" aria-label="正在加载任务"/>}</div>
  const status=benchmark&&!benchmarkError?benchmarkReadiness(benchmark):statusText(task,data)
  const currentStage=benchmark?.cases.findIndex(item=>item.phase==='submitting'||item.phase==='following'||item.operation?.state==='reconcile')??-1
  const busy=activeRun(run)
  const blocked=Boolean(busy||awaitingPublication(data.publication)||data.unknown_read_run_id||pending)
  const controlling=task.status_reason==='pause_requested'||task.status_reason==='stop_requested'
  const canReference=onReference&&(task.conversation_id===sourceConversationID||Boolean(benchmark&&!benchmarkError))
  const allArtifacts=[...(data.artifact?[data.artifact]:[]),...(data.artifact_versions??[]).filter(item=>item.id!==data.artifact?.id)]
  const reads=data.reads??(data.read?[data.read]:[])
  const usage=data.task_usage??run?.usage
  const model=record?.contexts?.filter(entry=>entry.manifest?.requested_model||entry.model_receipt).at(-1)
  const report=model?.model_receipt
  const reportText=!report?'未记录':report.state==='reported'&&report.reported_model?report.reported_model:report.state==='not_reported'?'未回报模型标识':report.state==='conflicting'?'回报互相冲突':'无法核对'
  const duration=usage?(usage.active_ms<1000?`${usage.active_ms} 毫秒`:`${number.format(usage.active_ms/1000)} 秒`):''
  const lowerBound=Boolean(busy||usage?.time_partial||usage?.untracked_runs)
  const showUsage=Boolean(model||usage&&(usage.model_requests||usage.active_ms||usage.untracked_runs))
  const hasModelReceipt=Boolean(record?.contexts?.some(entry=>entry.manifest?.requested_model||entry.model_receipt))
  const hasReceipt=Boolean(data.publication||data.acceptance||reads.length||hasModelReceipt)
  const artifactRow=(artifact:Artifact,current=false)=><div className="chat-task-artifact" key={artifact.id}>
    <div className="chat-task-artifact__name"><FileText size={15} aria-hidden="true"/><span>{artifact.name}</span></div>
    {current&&artifact.content!==undefined?<button type="button" className="chat-task-small-button" onClick={()=>setPreview(!preview)}>{preview?'收起':'预览'}</button>:
      <button type="button" className="chat-task-small-button" onClick={()=>navigate(artifactPath(task,artifact.id))}>查看</button>}
    <a className="chat-task-download" href={'/api/v1/artifacts/'+encodeURIComponent(artifact.id)+'/content'} aria-label={'下载 '+artifact.name} title="下载"><ArrowDownToLine size={15} aria-hidden="true"/></a>
  </div>
  return <div className="chat-task-view">
    <header className="chat-task-view__header">
      <div className="chat-task-view__close">{closeAction}</div>
      <div className="chat-task-view__badges"><span className="chat-task-view__id" title={task.id}>{task.id.slice(0,8)}</span><span className={'chat-task-view__status chat-task-view__status--'+task.status}>{status}</span></div>
      <h2>{task.title}</h2>
    </header>
    <div className="chat-task-tabs" role="tablist" aria-label="任务详情" ref={list} onKeyDown={navigateTabs}>
      {tabs.map(item=><button type="button" role="tab" id={id+'-'+item.id} key={item.id} aria-selected={tab===item.id} tabIndex={tab===item.id?0:-1} onClick={()=>chooseTab(item.id)}>{item.label}</button>)}
    </div>
    <div className="chat-task-view__body">
      <div role="tabpanel" aria-labelledby={id+'-'+tab} tabIndex={0}>
        {tab==='overview'&&<div className="chat-task-sections">
          <section className="chat-task-section"><h3>{benchmark&&!benchmarkError?'当前执行阶段'+(currentStage>=0?`（${currentStage+1}/${benchmark.cases.length}）`:''):'当前进展'}</h3>
            {benchmark&&!benchmarkError&&benchmark.cases.length>0?<ol className="chat-task-stages">{benchmark.cases.map(item=><li key={item.step_id}><span>{item.key}</span><span className="chat-task-stage-status" data-phase={item.phase}>{item.operation?.state==='reconcile'?'外部结果待核实':caseLabels[item.phase]??'状态待核实'}</span></li>)}</ol>:<p>{task.reason||status}</p>}
          </section>
          {showUsage&&<section className="chat-task-section"><h3>模型回执与 Token 用量</h3>
            {model?.manifest?.requested_model&&<p>请求模型：{model.manifest.requested_model}</p>}
            {model&&<p>供应商回报：{reportText}</p>}
            {usage&&Boolean(usage.model_requests||usage.active_ms||usage.untracked_runs)&&<p>累计消耗：{usage.model_requests>0?<>输入 {usage.input_tokens===null?'未知':number.format(usage.input_tokens)} · 输出 {usage.output_tokens===null?'未知':number.format(usage.output_tokens)} tokens · </>:null}{lowerBound?'至少 ':''}活跃 {duration}{usage.unreported_requests>0?' · 部分用量未报告':''}{usage.untracked_runs>0?' · 部分历史缺少记录':''}</p>}
          </section>}
          {data.artifact&&<section className="chat-task-section"><h3>已生成产物</h3>{artifactRow(data.artifact,true)}{preview&&data.artifact.content!==undefined&&<FormattedText text={data.artifact.content} framed/>}</section>}
          {!readOnly&&children&&<details className="chat-task-more" open={detailsOpen} onToggle={event=>setDetailsOpen(event.currentTarget.open)}><summary>查看完整工作与交流<ChevronRight size={15} aria-hidden="true"/></summary>{detailsOpen&&children}</details>}
        </div>}
        {tab==='runs'&&<div className="chat-task-tab-content"><RunHistory taskID={taskID} latest={run} selected={selectedRun} select={setSelectedRun} revision={task.revision} archived={data.conversation.archived} openArtifact={artifactID=>navigate(artifactPath(task,artifactID))}/></div>}
        {tab==='artifacts'&&<div className="chat-task-tab-content"><section className="chat-task-section"><h3>产物</h3>{allArtifacts.length?allArtifacts.map(item=>artifactRow(item,item.id===data.artifact?.id)):<p>暂无产物</p>}{preview&&data.artifact?.content!==undefined&&<FormattedText text={data.artifact.content} framed/>}{allArtifacts.length>=20&&<button type="button" className="chat-task-all" onClick={()=>navigate('/artifacts?'+new URLSearchParams({task_id:task.id,scope_label:task.scope_label}))}>查看全部成果<ChevronRight size={15} aria-hidden="true"/></button>}</section></div>}
        {tab==='receipts'&&<div className="chat-task-tab-content chat-task-receipts">
          {data.publication&&<section className="chat-task-section"><h3>写入回执</h3><p>outputs/{data.publication.filename}</p><p>{publicationLabels[data.publication.status]??data.publication.status}{data.publication.detail?' · '+data.publication.detail:''}</p></section>}
          {data.acceptance&&<section className="chat-task-section"><h3>验收回执</h3><p>已验收 · {when(data.acceptance.accepted_at)}</p>{data.acceptance.filename&&<p>outputs/{data.acceptance.filename}</p>}</section>}
          {reads.map(read=><ReadEvidence key={read.position} read={read} showSource={reads.length>1} distinguish={reads.some(other=>other.position!==read.position&&other.connection_name===read.connection_name&&other.tool_name===read.tool_name)} openRun={runID=>{setSelectedRun(runID);setTab('runs')}}/>)}
          {hasModelReceipt&&<section className="chat-task-section"><RunModelProvenance contexts={record?.contexts}/></section>}
          {recordError&&<p className="error" role="alert">无法读取执行回执：{recordError}</p>}
          {!hasReceipt&&!recordError&&<p className="chat-task-empty">暂无回执</p>}
        </div>}
      </div>
    </div>
    {!readOnly&&(ordinary||canReference||benchmark?.dispatch_control?.available)&&<footer className="chat-task-view__footer">
      {ordinary&&<button type="button" className="chat-task-action" disabled={blocked} onClick={()=>setEditing(true)}><Pencil size={14} aria-hidden="true"/>修订</button>}
      {canReference&&<button type="button" className="chat-task-action" onClick={()=>onReference({taskID:task.id,title:task.title,passive:task.conversation_id===sourceConversationID,planRevision:benchmarkError?undefined:benchmark?.plan_revision})}><MessageSquareQuote size={14} aria-hidden="true"/>引用</button>}
      {benchmark&&!benchmarkError&&onReference&&<button type="button" className="chat-task-action" onClick={()=>onReference({taskID:task.id,title:task.title,passive:false,purpose:'revision',planRevision:benchmark.plan_revision})}><Pencil size={14} aria-hidden="true"/>修订方案</button>}
      {ordinary&&task.status==='active'&&<button type="button" className="chat-task-action chat-task-action--pause" disabled={pending||controlling} onClick={()=>void pause()}><Pause size={14} aria-hidden="true"/>暂停</button>}
      {benchmark?.dispatch_control?.available&&!benchmarkError&&<button type="button" className="chat-task-action chat-task-action--pause" disabled={taskDispatch.busy||controlling} onClick={()=>void taskDispatch.apply()}>
        {benchmark.dispatch_control.paused?<Play size={14} aria-hidden="true"/>:<Pause size={14} aria-hidden="true"/>}{taskDispatch.busy?'正在核对…':benchmark.dispatch_control.paused?'恢复后续派发':'暂停后续派发'}
      </button>}
    </footer>}
    {taskDispatch.notice&&<p className={taskDispatch.notice.error?'chat-task-action-error':'chat-task-action-notice'} role={taskDispatch.notice.error?'alert':'status'}>{taskDispatch.notice.text}</p>}
    {benchmark&&!benchmarkError&&benchmark.dispatch_control?.unavailable_reason&&<p className="chat-task-action-notice">{benchmark.dispatch_control.unavailable_reason}</p>}
    {(actionError||error)&&<p className="chat-task-action-error" role="alert">{actionError||error}</p>}
    {editing&&<div onKeyDown={event=>{if(event.key==='Escape')event.stopPropagation()}}><TaskEditor task={task} blocked={blocked} reload={reload} onSaved={async()=>{await reload();onChange()}} onClose={()=>setEditing(false)}/></div>}
  </div>
}
