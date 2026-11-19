import {useEffect,useRef,useState} from 'react'
import {ChevronDown,ChevronRight,FileCheck2,FileText,History,LoaderCircle,MessageCircle} from 'lucide-react'
import {activeRun,api,type Run,type RunPage,type RunRecord,type RunModelContext} from './api'
import {FormattedText,Messages,UsageDetails} from './ExecutionText'
import {ReadEvidence} from './TaskRead'

const runLabels:Record<string,string>={
  queued:'等待执行',running:'正在执行',completed:'已执行',input_required:'请求补充',interrupted:'执行中断',failed:'执行失败',cancelled:'已停止',
}
const publicationLabels:Record<string,string>={
  pending:'待确认',dispatched:'正在写入',unknown:'结果待核对',succeeded:'已写入',denied:'已拒绝',
  conflict:'文件版本已变化',failed:'写入失败',expired:'确认已过期',
}
const runTime=new Intl.DateTimeFormat('zh-CN',{year:'numeric',month:'numeric',day:'numeric',hour:'2-digit',minute:'2-digit',second:'2-digit'})
const message=(error:unknown)=>error instanceof Error?error.message:'无法加载执行记录'

export function RunHistory({taskID,latest,selected,select,revision,archived,openArtifact}:{
  taskID:string;latest?:Run;selected:string;select:(runID:string)=>void;revision:number;archived:boolean;openArtifact:(id:string)=>void
}) {
  const [expanded,setExpanded]=useState(false)
  const [page,setPage]=useState<RunPage>()
  const [record,setRecord]=useState<{key:string;value:RunRecord}>()
  const [listError,setListError]=useState('')
  const [recordError,setRecordError]=useState<{key:string;text:string}>()
  const [loadingMore,setLoadingMore]=useState(false)
  const [retry,setRetry]=useState(0)
  const historyRef=useRef<HTMLDivElement>(null)
  const open=expanded||Boolean(selected)
  const runID=selected||latest?.id||page?.items[0]?.id||''
  const recordRevision=runID===latest?.id?revision:0
  const recordStatus=runID===latest?.id?latest.status:undefined
  const path='/tasks/'+taskID+'/runs'
  const recordKey=JSON.stringify([path,runID,recordRevision,recordStatus,runID===latest?.id?latest.execution_generation:null,retry])
  const errorForRun=recordError?.key===recordKey?recordError.text:''

  useEffect(()=>{
    if(selected)historyRef.current?.scrollIntoView({block:'start'})
  },[selected])

  useEffect(()=>{
    if(!open)return
    let cancelled=false
    setListError('')
    api<RunPage>(path).then(result=>{if(!cancelled)setPage(result)})
      .catch(error=>{if(!cancelled)setListError(message(error))})
    return()=>{cancelled=true}
  },[open,path,latest?.id,latest?.status,latest?.execution_generation,retry])

  useEffect(()=>{
    if(!open||!runID)return
    let cancelled=false
    setRecordError(undefined)
    api<RunRecord>(path+'/'+encodeURIComponent(runID)).then(result=>{
      if(cancelled)return
      if(result?.id!==runID)throw new Error('收到的执行记录不匹配')
      setRecord({key:recordKey,value:result})
    }).catch(error=>{if(!cancelled)setRecordError({key:recordKey,text:message(error)})})
    return()=>{cancelled=true}
  },[open,path,runID,recordKey])

  async function loadMore() {
    const after=page?.next_cursor
    if(!after||loadingMore)return
    setLoadingMore(true)
    setListError('')
    try {
      const next=await api<RunPage>(path+'?'+new URLSearchParams({after}))
      setPage(current=>current?.next_cursor===after?{items:[...current.items,...next.items],next_cursor:next.next_cursor}:current)
    } catch(error) {setListError(message(error))}
    finally {setLoadingMore(false)}
  }

  return <div className="run-history">
    <button type="button" className="text-button run-history-toggle" aria-expanded={open} aria-controls="task-run-history" onClick={()=>{
      setExpanded(!open)
      if(open&&selected)select('')
    }}><History size={16} aria-hidden="true"/>执行记录<ChevronDown size={14} aria-hidden="true"/></button>
    {open&&<div id="task-run-history" className="run-history-grid" ref={historyRef}>
      <div className="run-list" aria-label="历次执行">
        {!page&&!listError&&<LoaderCircle size={17} className="spin" aria-label="正在加载执行列表"/>}
        {page?.items.length===0&&<p className="muted">暂无执行记录</p>}
        {page?.items.map(item=><button type="button" key={item.id} className={'run-list-item '+(item.id===runID?'selected':'')}
          aria-current={item.id===runID?'true':undefined} onClick={()=>select(item.id)}>
          <span><strong>{!archived&&item.input_excerpt?item.input_excerpt:runLabels[item.status]??item.status}</strong>
            <small>{runTime.format(item.created_at)}{!archived&&item.input_excerpt?' · '+(runLabels[item.status]??item.status):''}</small></span>
          <ChevronRight size={15} aria-hidden="true"/>
        </button>)}
        {page?.next_cursor&&<button type="button" className="text-button run-more" disabled={loadingMore} onClick={()=>void loadMore()}>{loadingMore?'正在加载…':'加载更早记录'}</button>}
        {listError&&<><p className="error" role="alert">{listError}</p><button type="button" className="text-button" onClick={()=>setRetry(retry+1)}>重新加载</button></>}
      </div>
      {runID&&<div className="run-record" aria-label="执行详情">
        {errorForRun?<><p className="error" role="alert">{errorForRun}</p><button type="button" className="text-button" onClick={()=>setRetry(retry+1)}>重新加载详情</button></>
          :record?.key===recordKey?<RunEvidence key={runID} record={record.value} archived={archived} openArtifact={openArtifact} openRun={select}/>
          :<LoaderCircle size={17} className="spin" aria-label="正在加载执行详情"/>}
      </div>}
    </div>}
  </div>
}

export function RunModelProvenance({contexts}:{contexts?:RunModelContext[]}) {
  const entries=(contexts??[]).filter(c=>Boolean(c.manifest?.requested_model||c.model_receipt))
  if(!entries.length)return null
  return <details className="run-messages run-model-provenance">
    <summary>模型来源（{entries.length} 条记录）</summary>
    <p className="muted">请求的模型可能是别名。供应商回报仅为对方声明，不是独立核验的模型身份。</p>
    <ol>{entries.map((entry,index)=>{
      const receipt=entry.model_receipt
      const protocol=receipt?.protocol==='responses'?'Responses':receipt?.protocol==='chat_completions'?'Chat Completions':''
      return <li key={entry.execution_generation+':'+entry.recorded_at+':'+index}>
        <small>执行代际 {entry.execution_generation}{Number.isFinite(entry.recorded_at)?' · '+runTime.format(entry.recorded_at):''}{protocol&&' · '+protocol}</small>
        <dl>
          <dt>请求的模型</dt><dd>{entry.manifest?.requested_model||'未记录'}</dd>
          <dt>供应商回报</dt><dd>{!receipt?'没有回执记录':receipt.state==='not_reported'?'未回报模型标识':
            receipt.state==='conflicting'?'回报互相冲突，无法确认':
            receipt.state==='reported'&&receipt.reported_model?receipt.reported_model:'回执无法核对'}</dd>
        </dl>
      </li>
    })}</ol>
  </details>
}

function RunEvidence({record,archived,openArtifact,openRun}:{record:RunRecord;archived:boolean;openArtifact:(id:string)=>void;openRun:(id:string)=>void}) {
  const reads=record.reads??[]
  const messages=record.inherited_input?[record.inherited_input,...record.messages]:record.messages
  const partial=!activeRun(record)&&record.status!=='completed'&&record.response&&!record.messages.some(message=>message.role==='assistant')
  return <>
    <div className="run-record-heading"><h3>{runLabels[record.status]??record.status}</h3><time dateTime={new Date(record.created_at).toISOString()}>{runTime.format(record.created_at)}</time></div>
    {record.error&&<p className="error">{record.error}</p>}
    {reads.map(read=><ReadEvidence key={read.position} read={read} showSource openRun={openRun} distinguish={reads.some(other=>other.position!==read.position&&other.connection_name===read.connection_name&&other.tool_name===read.tool_name)}/>)}
    {record.artifacts?.map(artifact=><button type="button" className="history-row run-artifact" key={artifact.id} onClick={()=>openArtifact(artifact.id)}>
      <FileText size={16} aria-hidden="true"/><span>{artifact.name}{artifact.accepted_at&&<small>已验收</small>}</span><ChevronRight size={15} aria-hidden="true"/>
    </button>)}
    {record.publications?.map(publication=><div className="run-receipt" key={publication.id}>
      <FileCheck2 size={16} aria-hidden="true"/><div><strong>outputs/{publication.filename}</strong><small>{publicationLabels[publication.status]??publication.status}{publication.detail&&['failed','conflict','unknown'].includes(publication.status)&&' · '+publication.detail}</small></div>
    </div>)}
    <RunModelProvenance contexts={record.contexts}/>
    <UsageDetails current={record.usage} running={activeRun(record)} title="本轮用量"/>
    {!archived&&(messages.length>0||partial)&&<details className="run-messages">
      <summary><MessageCircle size={16} aria-hidden="true"/>当轮交流</summary>
      {record.inherited_input&&<p className="muted">沿用上一轮输入</p>}
      <Messages messages={messages} anchors={false}/>
      {partial&&<><p className="muted">未完成的回复</p><FormattedText text={record.response}/></>}
    </details>}
  </>
}
