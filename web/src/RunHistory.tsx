import {useEffect,useRef,useState} from 'react'
import {ChevronDown,ChevronRight,FileCheck2,FileText,History,LoaderCircle,MessageCircle} from 'lucide-react'
import {activeRun,api,type Run,type RunPage,type RunRecord} from './api'
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
  const [record,setRecord]=useState<RunRecord>()
  const [listError,setListError]=useState('')
  const [recordError,setRecordError]=useState('')
  const [loadingMore,setLoadingMore]=useState(false)
  const [retry,setRetry]=useState(0)
  const historyRef=useRef<HTMLDivElement>(null)
  const open=expanded||Boolean(selected)
  const runID=selected||latest?.id||page?.items[0]?.id||''
  const recordRevision=runID===latest?.id?revision:0
  const recordStatus=runID===latest?.id?latest.status:undefined
  const path='/tasks/'+taskID+'/runs'

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
    setRecordError('')
    api<RunRecord>(path+'/'+encodeURIComponent(runID)).then(result=>{if(!cancelled)setRecord(result)})
      .catch(error=>{if(!cancelled)setRecordError(message(error))})
    return()=>{cancelled=true}
  },[open,path,runID,recordRevision,recordStatus,retry])

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
        {recordError?<><p className="error" role="alert">{recordError}</p><button type="button" className="text-button" onClick={()=>setRetry(retry+1)}>重新加载详情</button></>
          :record?.id===runID?<RunEvidence record={record} archived={archived} openArtifact={openArtifact} openRun={select}/>
          :<LoaderCircle size={17} className="spin" aria-label="正在加载执行详情"/>}
      </div>}
    </div>}
  </div>
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
    <UsageDetails current={record.usage} running={activeRun(record)} title="本轮用量"/>
    {!archived&&(messages.length>0||partial)&&<details className="run-messages">
      <summary><MessageCircle size={16} aria-hidden="true"/>当轮交流</summary>
      {record.inherited_input&&<p className="muted">沿用上一轮输入</p>}
      <Messages messages={messages} anchors={false}/>
      {partial&&<><p className="muted">未完成的回复</p><FormattedText text={record.response}/></>}
    </details>}
  </>
}
