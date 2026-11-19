import {useCallback,useEffect,useId,useRef,useState,type FormEvent} from 'react'
import {api,APIError,type TaskToolOption} from './api'

type SkillSelection={conversation_id:string;binding:null|{skill_id:string;scope:string;version:number;validity:string}}
type ToolRef={connection_id:string;connection_revision:number;tool_name:string;tool_version:string}
type ToolSelection={conversation_id:string;selection:ToolRef|null;local_validity?:string}
type SkillVersion={skill_id:string;scope_label:string;version:number;instructions:string;active:boolean;approved_at?:number}
type Option={key:string;label:string;body:Record<string,unknown>}
type Selection={key:string;label:string;validity:string}
type Spec={title:string;choose:string;empty:string;path:string;clear:string;catalog:string;
  project:(body:unknown,id:string)=>Selection|null;options:(body:unknown)=>Option[]}

const skillKey=(id:string,version:number)=>JSON.stringify([id,version])
const toolKey=(t:ToolRef)=>JSON.stringify([t.connection_id,t.connection_revision,t.tool_name,t.tool_version])
const skill:Spec={
  title:'Skill',choose:'选择已启用的 Skill 版本',empty:'没有可选的已批准启用版本。',
  path:'/skill-binding',clear:'clear_skill',catalog:'/skills',
  project:(body,id)=>{
    const result=body as SkillSelection|undefined
    if(result?.conversation_id!==id||!('binding' in result))throw new Error('无法核对当前 Skill 绑定')
    const b=result.binding
    return b?{key:skillKey(b.skill_id,b.version),label:b.scope+' · v'+b.version,validity:b.validity}:null
  },
  options:body=>(Array.isArray(body)?body as SkillVersion[]:[]).filter(v=>v.active&&v.approved_at&&v.instructions)
    .map(v=>({key:skillKey(v.skill_id,v.version),label:v.scope_label+' · v'+v.version,body:{action:'bind_skill',skill_id:v.skill_id,version:v.version}})),
}
const mcp:Spec={
  title:'MCP 只读工具',choose:'选择已就绪且已获授权的工具',empty:'没有可选的已授权工具。可在工具与连接中管理。',
  path:'/mcp-selection',clear:'clear_mcp_read',catalog:'/task-tools',
  project:(body,id)=>{
    const result=body as ToolSelection|undefined
    if(result?.conversation_id!==id||!('selection' in result))throw new Error('无法核对当前 MCP 绑定')
    const t=result.selection
    return t?{key:toolKey(t),label:t.tool_name+' · 连接版本 '+t.connection_revision,validity:result.local_validity??'unavailable'}:null
  },
  options:body=>(Array.isArray(body)?body as TaskToolOption[]:[]).filter(o=>o.tool?.allowed&&!o.tool.schema_error)
    .map(o=>{
      const t={connection_id:o.connection_id,connection_revision:o.revision,tool_name:o.tool.name,tool_version:o.tool.version}
      return {key:toolKey(t),label:o.connection_name+' · '+(o.tool.title||o.tool.name),body:{action:'bind_mcp_read',...t}}
    }),
}

function BindingEditor({spec,id,open,disabled}:{spec:Spec;id:string;open:boolean;disabled:boolean}){
  const title=useId()
  const path='/conversations/'+encodeURIComponent(id)+spec.path
  const [current,setCurrent]=useState<Selection|null>()
  const [options,setOptions]=useState<Option[]>()
  const [choice,setChoice]=useState('')
  const [busy,setBusy]=useState(false)
  const [stale,setStale]=useState(true)
  const [error,setError]=useState('')
  const [notice,setNotice]=useState('')
  const lock=useRef(false)
  const generation=useRef(0)
  const reload=useCallback(async()=>{
    if(lock.current)return
    const token=++generation.current
    setStale(true);setError('')
    try{
      const [state,catalog]=await Promise.allSettled([api(path),api(spec.catalog)])
      if(token!==generation.current)return
      if(state.status==='rejected')throw state.reason
      setCurrent(spec.project(state.value,id))
      if(catalog.status==='rejected')throw catalog.reason
      setOptions(spec.options(catalog.value));setStale(false)
    }catch(e){if(token===generation.current)setError(e instanceof Error?e.message:'暂时无法读取配置')}
  },[id,path,spec])
  useEffect(()=>{if(open)void reload();return()=>{generation.current++}},[open,reload])
  const selected=options?.find(o=>o.key===choice)
  const ready=current?.validity==='valid'||current?.validity==='locally_ready'
  async function save(option:Option|null){
    if(disabled||stale||lock.current||current===undefined||(!option&&!current))return
    lock.current=true;generation.current++;setBusy(true);setNotice('');setError('')
    let recheck=false
    try{
      const actual=spec.project(await api(path),id)
      if(JSON.stringify(actual)!==JSON.stringify(current)){
        setCurrent(actual);setStale(true);recheck=true
        setNotice('服务端当前选择已变化。未保存，已保留你的选择，请重新核对。')
        return
      }
      const value=spec.project(await api(path,option?.body??{action:spec.clear},'PUT'),id)
      if(option?(value?.key!==option.key):value!==null)throw new Error('无法核对配置回执')
      setCurrent(value);setChoice('');setNotice(option?'已保存为本会话的选择。':'已清除本会话的选择。')
    }catch(e){
      setStale(true)
      setError(e instanceof APIError&&e.status===409?'配置已变化或会话正忙。没有自动重发，核对当前状态后再试。':e instanceof Error?e.message:'没有收到配置回执，请先重新核对。')
    }finally{
      lock.current=false;setBusy(false)
      if(recheck)void reload()
    }
  }
  function submit(event:FormEvent){
    event.preventDefault()
    if(selected&&(!ready||selected.key!==current?.key))void save(selected)
  }
  return <form className="settings-fields" onSubmit={submit} aria-labelledby={title} aria-busy={busy}>
    <h3 id={title}>{spec.title}</h3>
    {stale&&current!==undefined&&!busy&&<p className="muted">当前显示的是上次读取结果；核对完成前无法修改。</p>}
    {current===undefined&&!error?<p role="status">正在读取当前配置…</p>:current?
      <p>当前：{current.label} · {ready?'有效（本地检查）':current.validity==='revoked'?'已撤销':'不可用，请重新选择或解除'}</p>:
      current===null?<p className="muted">未显式绑定。</p>:null}
    {options&&options.length>0?
      <label className="settings-field"><span>{spec.choose}</span>
        <select value={choice} disabled={disabled||stale||busy} name="selection" onChange={event=>{setChoice(event.currentTarget.value);setNotice('')}}>
          <option value="">请选择</option>
          {choice&&!selected&&<option value={choice} disabled>此前选择已不可用</option>}
          {options.map(o=><option key={o.key} value={o.key}>{o.label}</option>)}
        </select></label>:options?<p className="muted">{spec.empty}</p>:null}
    <div className="settings-actions">
      {options&&options.length>0&&<button type="submit" className="outline"
        disabled={disabled||stale||busy||!selected||Boolean(ready&&selected.key===current?.key)}>保存选择</button>}
      {current&&<button type="button" className="text-button" disabled={disabled||stale||busy} onClick={()=>void save(null)}>解除绑定</button>}
      {stale&&error&&<button type="button" className="text-button" disabled={busy} onClick={()=>void reload()}>重新核对</button>}
    </div>
    {notice&&<p role="status">{notice}</p>}
    {error&&<p role="alert" className="error">{error}</p>}
  </form>
}

export function ConversationConfiguration({id,open,panelID,disabled,archived,onClose}:{id:string;open:boolean;panelID:string;disabled:boolean;archived:boolean;onClose:()=>void}){
  const heading=useRef<HTMLHeadingElement>(null)
  const [seen,setSeen]=useState(open)
  useEffect(()=>{if(open){setSeen(true);heading.current?.focus()}},[open])
  return <section className="conversation-config" hidden={!open} id={panelID} aria-labelledby={panelID+'-heading'}
    onKeyDown={e=>{if(e.key==='Escape'&&!e.defaultPrevented){e.preventDefault();onClose()}}}>
    <header className="settings-section-heading"><h2 id={panelID+'-heading'} ref={heading} tabIndex={-1}>本会话配置</h2>
      <button type="button" className="text-button" onClick={onClose}>收起</button></header>
    <p className="muted">影响此会话后续办理，不改变已记录的运行，也不授予权限、启动工具或执行 Skill。</p>
    {disabled&&<p role="status">{archived?'会话已归档，恢复后才能修改。':'会话正在处理，结束后才能修改。'}</p>}
    {(open||seen)&&<div className="conversation-config-fields">
      <BindingEditor spec={skill} id={id} open={open} disabled={disabled}/>
      <BindingEditor spec={mcp} id={id} open={open} disabled={disabled}/>
    </div>}
  </section>
}
