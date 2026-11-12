import {useEffect,useState} from 'react'
import {ArrowDownToLine, ChevronDown, History, Plug, Plus, X} from 'lucide-react'
import {api,type MCPResultReference,type TaskRead,type TaskReadRequest,type TaskToolOption} from './api'

export type ReadDraft = {option:TaskToolOption;args:string}

const optionKey=(option:TaskToolOption)=>option.connection_id+':'+option.tool.name

export function TaskReadPicker({drafts,change,disabled}:{drafts:ReadDraft[];change:(drafts:ReadDraft[])=>void;disabled:boolean}) {
  const [open,setOpen]=useState(false)
  const [adding,setAdding]=useState(false)
  const [options,setOptions]=useState<TaskToolOption[]>()
  const [error,setError]=useState('')
  useEffect(()=>{
    if(!open)return
    let cancelled=false
    setError('')
    api<TaskToolOption[]>('/task-tools').then(items=>{if(!cancelled)setOptions(items)})
      .catch(error=>{if(!cancelled)setError(error instanceof Error?error.message:'无法读取工具列表')})
    return()=>{cancelled=true}
  },[open])
  function choose(index:number,key:string) {
    const option=options?.find(item=>optionKey(item)===key)
    if(!option) {
      change(drafts.filter((_,i)=>i!==index))
      setAdding(false)
      return
    }
    const next=drafts.slice()
    next[index]={option,args:'{}'}
    change(next)
    if(index===drafts.length)setAdding(false)
  }
  const rows=open&&(adding||drafts.length===0)?[...drafts,undefined]:drafts
  const label=drafts.length>1?`已选 ${drafts.length} 个读取工具`:drafts.length?'已选读取工具':'读取工具'
  return <>
    <button type="button" className="text-button task-read-toggle" aria-expanded={open} disabled={disabled}
      onClick={()=>{setOpen(!open);setAdding(false)}}><Plug size={16}/>{label}</button>
    {(open || drafts.length>0) && <div className="task-read-picker">
      {rows.map((draft,index)=>{
        const suffix=index===0?'':` ${index+1}`
        const selected=draft?optionKey(draft.option):''
        return <div className="task-read-source" key={index}>
          {open ? <>
            <label>读取工具<select aria-label={'选择读取工具'+suffix} value={selected} disabled={disabled}
              onChange={event=>choose(index,event.target.value)}>
              <option value="">请选择工具</option>
              {draft && !options?.some(option=>optionKey(option)===selected) && <option value={selected}>{draft.option.connection_name} · {draft.option.tool.title||draft.option.tool.name}</option>}
              {options?.map(option=><option key={optionKey(option)} value={optionKey(option)}>{option.connection_name} · {option.tool.title||option.tool.name}</option>)}
            </select></label>
            {draft && <>
              <details><summary>参数定义</summary><pre>{JSON.stringify(draft.option.tool.input_schema,null,2)}</pre></details>
              <label>读取参数<textarea aria-label={'读取参数'+suffix} rows={2} value={draft.args} maxLength={24576} readOnly={disabled}
                onChange={event=>change(drafts.map((item,i)=>i===index?{...item,args:event.target.value}:item))}/></label>
            </>}
          </> : <span>{draft?.option.connection_name} · {draft?.option.tool.title||draft?.option.tool.name}</span>}
          {draft && <button type="button" className="text-button" aria-label={'移除读取工具'+suffix} disabled={disabled}
            onClick={()=>change(drafts.filter((_,i)=>i!==index))}><X size={15}/>移除读取工具</button>}
        </div>
      })}
      {open && error && <p className="error" role="alert">{error}</p>}
      {open && options?.length===0 && <p className="muted">在「工具与连接」授权读取工具后即可选用。</p>}
      {open && !adding && drafts.length>0 && drafts.length<8 && Boolean(options?.length) &&
        <button type="button" className="text-button" disabled={disabled} onClick={()=>setAdding(true)}><Plus size={15}/>添加读取工具</button>}
    </div>}
  </>
}

export function readRequest(draft:ReadDraft):TaskReadRequest {
  let args:unknown
  try {args=JSON.parse(draft.args)} catch {throw new Error('读取参数须为有效的 JSON 对象')}
  if(!args || typeof args!=='object' || Array.isArray(args)) throw new Error('读取参数须为有效的 JSON 对象')
  const {option}=draft
  return {connection_id:option.connection_id,revision:option.revision,tool_name:option.tool.name,tool_version:option.tool.version,arguments:args as Record<string,unknown>}
}

const readLabels:Record<string,string>={queued:'等待读取',checking:'正在检查连接',dispatched:'正在读取',completed:'读取完成',reused:'沿用上次结果',failed:'读取失败',tool_error:'工具返回错误',unknown:'未收到完整结果',not_run:'工具未调用'}
const receivedTime=new Intl.DateTimeFormat('zh-CN',{month:'numeric',day:'numeric',hour:'numeric',minute:'numeric',second:'numeric'})

export function ToolResultDownload({reference}:{reference:MCPResultReference}) {
  return <a className="text-button" href={'/api/v1/tool-results/'+reference.id+'/content'} download>
    <ArrowDownToLine size={16} aria-hidden="true"/>下载读取结果
  </a>
}

export function ReadEvidence({read,showSource,distinguish,openRun}:{read:TaskRead;showSource?:boolean;distinguish?:boolean;openRun?:(id:string)=>void}) {
  const sourceRunID=read.source_run_id
  const hasData=Boolean(read.result?.text?.trim()) || read.result?.structured!==undefined
  let label=read.status==='unknown'&&read.acknowledged?'已确认未取得结果':readLabels[read.status]
  if((read.status==='completed'||read.status==='reused') && read.result) {
    if(read.result.truncated) label=(read.status==='reused'?'沿用的':'')+(read.result.reference?'预览已截断':'结果已截断')
    else if(!hasData) label=read.status==='reused'?'沿用的读取无数据':'读取无数据'
  }
  if(read.result?.validation_error) label='结果校验失败'
  if(sourceRunID) label='先前'+label
  const tone=read.status==='unknown'||read.result?.truncated?'warning':read.status==='failed'||read.status==='tool_error'?'failed':undefined
  const title=(showSource?read.connection_name+' · ':'')+(read.tool_title||read.tool_name)+(distinguish?' '+read.position:'')
  return <details className="read-evidence">
    <summary><Plug size={16} aria-hidden="true"/><strong>{title}</strong>
      <span className={tone?'evidence-state '+tone:undefined} aria-live="polite">{label}</span>
      <ChevronDown className="read-evidence-chevron" size={14} aria-hidden="true"/>
    </summary>
    <div className="read-evidence-body">
      <p>{read.connection_name} · <code>{read.tool_name}</code></p>
      {(read.received_at!==undefined||read.result?.reference)&&<div className="tool-result-meta">
        {read.received_at!==undefined&&<small>接收于 {receivedTime.format(read.received_at)}</small>}
        {read.result?.reference&&<ToolResultDownload reference={read.result.reference}/>}
      </div>}
      {read.detail && <p className="error">{read.detail}</p>}
      {read.result && <>
        {read.result.truncated&&!read.result.reference&&<p className="muted">结果已截断</p>}
        {read.result.media_omitted && <p className="muted">非文本内容未展示</p>}
        {read.result.text && <pre>{read.result.text}</pre>}
        {read.result.structured!==undefined && <pre>{JSON.stringify(read.result.structured,null,2)}</pre>}
        {!hasData && <p className="muted">未返回文本或结构化数据</p>}
      </>}
    {sourceRunID&&openRun&&<button type="button" className="text-button read-source-link" onClick={()=>openRun(sourceRunID)}>
      <History size={15} aria-hidden="true"/>查看原轮次
    </button>}</div>
  </details>
}
