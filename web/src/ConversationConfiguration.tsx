import {useCallback,useEffect,useId,useRef,useState,type FormEvent} from 'react'
import {Blocks,BookOpen,Check,Plug,X} from 'lucide-react'
import {api,APIError,type TaskToolOption} from './api'
import './assistant-cards.css'
import './ConversationConfiguration.css'

type SkillSelection={conversation_id:string;binding_revision:number;binding:null|{skill_id:string;scope:string;version:number;validity:string}}
type ToolRef={connection_id:string;connection_revision:number;tool_name:string;tool_version:string}
type ToolSelection={conversation_id:string;binding_revision:number;selection:ToolRef|null;local_validity?:string}
type SkillVersion={skill_id:string;scope_label:string;version:number;instructions:string;active:boolean;approved_at?:number}
type Option={key:string;label:string;badge:string;detail:string;body:Record<string,unknown>}
type Selection={key:string;label:string;badge:string;detail:string;validity:string}
type Snapshot={revision:number;value:Selection|null}
type Spec={title:string;empty:string;defaultLabel:string;path:string;clear:string;catalog:string;
  project:(body:unknown,id:string)=>Snapshot;options:(body:unknown)=>Option[]}
type SaveResult='saved'|'not_saved'|'unknown'

function snapshot(revision:number,value:Selection|null):Snapshot {
  if(!Number.isSafeInteger(revision)||revision<0)throw new Error('无法核对会话配置版本，请更新应用或重新连接。')
  return {revision,value}
}
const skillKey=(id:string,version:number)=>JSON.stringify([id,version])
const toolKey=(t:ToolRef)=>JSON.stringify([t.connection_id,t.connection_revision,t.tool_name,t.tool_version])
const skill:Spec={
  title:'Skill',empty:'没有可选的已批准启用版本。',defaultLabel:'沿用默认 Skill',
  path:'/skill-binding',clear:'clear_skill',catalog:'/skills',
  project:(body,id)=>{
    const result=body as SkillSelection|undefined
    if(result?.conversation_id!==id||!('binding' in result))throw new Error('无法核对当前 Skill 绑定')
    const b=result.binding
    return snapshot(result.binding_revision,b?{key:skillKey(b.skill_id,b.version),label:b.scope,badge:'v'+b.version,detail:'已批准并启用',validity:b.validity}:null)
  },
  options:body=>(Array.isArray(body)?body as SkillVersion[]:[]).filter(v=>v.active&&v.approved_at&&v.instructions)
    .map(v=>({key:skillKey(v.skill_id,v.version),label:v.scope_label,badge:'v'+v.version,detail:v.instructions,body:{action:'bind_skill',skill_id:v.skill_id,version:v.version}})),
}
const mcp:Spec={
  title:'MCP 只读工具',empty:'没有可选的已授权工具。',defaultLabel:'沿用默认工具范围',
  path:'/mcp-selection',clear:'clear_mcp_read',catalog:'/task-tools',
  project:(body,id)=>{
    const result=body as ToolSelection|undefined
    if(result?.conversation_id!==id||!('selection' in result))throw new Error('无法核对当前 MCP 绑定')
    const t=result.selection
    return snapshot(result.binding_revision,t?{key:toolKey(t),label:t.tool_name,badge:'只读已授权',detail:t.tool_name+' · 连接版本 '+t.connection_revision,validity:result.local_validity??'unavailable'}:null)
  },
  options:body=>(Array.isArray(body)?body as TaskToolOption[]:[]).filter(o=>o.tool?.allowed&&!o.tool.schema_error)
    .map(o=>{
      const t={connection_id:o.connection_id,connection_revision:o.revision,tool_name:o.tool.name,tool_version:o.tool.version}
      return {key:toolKey(t),label:o.connection_name,badge:'只读已授权',
        detail:(o.tool.title||o.tool.name)+' · 连接版本 '+o.revision,body:{action:'bind_mcp_read',...t}}
    }),
}

function useBinding(spec:Spec,id:string,open:boolean){
  const path='/conversations/'+encodeURIComponent(id)+spec.path
  const [server,setServer]=useState<Snapshot>()
  const [options,setOptions]=useState<Option[]>()
  const [choice,setChoice]=useState('')
  const [stale,setStale]=useState(true)
  const [error,setError]=useState('')
  const lock=useRef(false)
  const generation=useRef(0)
  const reload=useCallback(async(preserveChoice=false)=>{
    if(lock.current)return
    const token=++generation.current
    setStale(true);setError('')
    try{
      const [state,catalog]=await Promise.allSettled([api(path),api(spec.catalog)])
      if(token!==generation.current)return
      if(state.status==='rejected')throw state.reason
      const actual=spec.project(state.value,id)
      setServer(actual)
      if(!preserveChoice)setChoice(actual.value?.key??'')
      if(catalog.status==='rejected')throw catalog.reason
      setOptions(spec.options(catalog.value));setStale(false)
    }catch(e){if(token===generation.current)setError(e instanceof Error?e.message:'暂时无法读取配置')}
  },[id,path,spec])
  useEffect(()=>{if(open)void reload();return()=>{generation.current++}},[open,reload])

  const current=server?.value
  const selected=options?.find(o=>o.key===choice)
  const changed=server!==undefined&&choice!==(current?.key??'')
  const choiceAvailable=!choice||Boolean(selected)||choice===current?.key
  async function save():Promise<SaveResult>{
    if(stale||lock.current||!server||!changed||!choiceAvailable)return 'not_saved'
    const option=choice?selected:null
    if(choice&&!option)return 'not_saved'
    lock.current=true;generation.current++;setError('')
    let attempted=false
    try{
      const actual=spec.project(await api(path),id)
      if(actual.revision!==server.revision||JSON.stringify(actual.value)!==JSON.stringify(server.value)){
        setServer(actual);setStale(true)
        setError('服务端当前选择已变化，未保存。已保留你的选择，请重新核对。')
        return 'not_saved'
      }
      const request={...(option?.body??{action:spec.clear}),expected_binding_revision:actual.revision}
      attempted=true
      const value=spec.project(await api(path,request,'PUT'),id)
      const targetKey=option?.key??null
      const expectedReceipt=actual.revision+(targetKey!==(actual.value?.key??null)?1:0)
      if(value.revision!==expectedReceipt||(value.value?.key??null)!==targetKey)throw new Error('无法核对配置回执')
      setServer(value);setChoice(value.value?.key??'')
      return 'saved'
    }catch(e){
      setStale(true)
      const rejected=e instanceof APIError&&e.status!==undefined&&e.status>=400&&e.status<500
      setError(e instanceof APIError&&e.code==='conversation_binding_conflict'?'绑定已被其他操作修改，重新核对后再保存。':
        e instanceof APIError&&e.status===409?'配置已变化或会话正忙，重新核对后再试。':
        e instanceof Error?e.message:'没有收到配置回执，请先重新核对。')
      return attempted&&!rejected?'unknown':'not_saved'
    }finally{lock.current=false}
  }
  return {server,options,choice,setChoice,selected,changed,choiceAvailable,stale,error,reload,save}
}

type Binding=ReturnType<typeof useBinding>
function BindingCard({spec,binding,disabled,onChange}:{spec:Spec;binding:Binding;disabled:boolean;onChange:()=>void}){
  const field=useId()
  const detailID=useId()
  const badgeID=useId()
  const {server,options,choice,selected,stale,error}=binding
  const current=server?.value
  const selectedCurrent=Boolean(choice&&current?.key===choice)
  const currentReady=current?.validity==='valid'||current?.validity==='locally_ready'
  const unavailable=selectedCurrent&&!currentReady
  const display=selected??(selectedCurrent?current:undefined)
  const Icon=spec===skill?BookOpen:Plug
  const detail=unavailable?(current?.validity==='revoked'?'已撤销，请重新选择或恢复默认。':'当前选择不可用，请重新选择或恢复默认。'):
    display?.detail??(spec===mcp?'未限定工具，沿用默认授权范围。':'未显式绑定，沿用默认范围。')
  return <div className="conversation-binding-card">
    <div className="conversation-binding-card-row">
      <Icon size={14} aria-hidden="true"/>
      <label className="v31-screen-reader" htmlFor={field}>{spec.title}</label>
      <select id={field} name={spec===skill?'skill':'mcp'} value={choice} aria-describedby={display&&!unavailable?detailID+' '+badgeID:detailID}
        disabled={disabled||stale||!options} onChange={event=>{binding.setChoice(event.currentTarget.value);onChange()}}>
        <option value="">{spec.defaultLabel}</option>
        {choice&&!selected&&<option value={choice} disabled>{selectedCurrent?current?.label:'此前选择已不可用'}</option>}
        {options?.map(o=><option key={o.key} value={o.key}>{o.label}</option>)}
      </select>
      {display&&!unavailable&&<span id={badgeID} className="conversation-binding-card-badge">{display.badge}</span>}
    </div>
    <p id={detailID} className="conversation-binding-card-detail">{server===undefined&&!error?'正在读取配置…':detail}</p>
    {options?.length===0&&!error&&<p className="conversation-binding-card-detail">{spec.empty}</p>}
    {error&&<p role="alert" className="error">{error} <button type="button" className="text-button" disabled={disabled} onClick={()=>void binding.reload(true)}>重新核对</button></p>}
  </div>
}

export function ConversationConfiguration({id,open,panelID,disabled,archived,onClose}:{id:string;open:boolean;panelID:string;disabled:boolean;archived:boolean;onClose:()=>void}){
  const heading=useRef<HTMLHeadingElement>(null)
  const [busy,setBusy]=useState(false)
  const [notice,setNotice]=useState('')
  const lock=useRef(false)
  const skillBinding=useBinding(skill,id,open)
  const mcpBinding=useBinding(mcp,id,open)
  useEffect(()=>{if(open){setNotice('');heading.current?.focus()}},[open])
  async function submit(event:FormEvent){
    event.preventDefault()
    if(disabled||lock.current||skillBinding.stale||mcpBinding.stale||!skillBinding.choiceAvailable||!mcpBinding.choiceAvailable)return
    const pending=[{name:'Skill',binding:skillBinding},{name:'MCP',binding:mcpBinding}].filter(item=>item.binding.changed)
    if(!pending.length){setNotice('绑定范围未变化。');return}
    lock.current=true;setBusy(true);setNotice('')
    const results:SaveResult[]=[]
    try{
      for(const item of pending){
        const result=await item.binding.save()
        results.push(result)
        if(result!=='saved')break
      }
      if(results.every(result=>result==='saved')&&results.length===pending.length)setNotice('绑定范围已保存。')
      else setNotice(pending.map((item,index)=>item.name+' '+(results[index]==='saved'?'已保存':results[index]==='unknown'?'保存结果未知':'未保存')).join('；')+'。请重新核对。')
    }finally{lock.current=false;setBusy(false)}
  }
  return <section className="conversation-config" hidden={!open} id={panelID} aria-labelledby={panelID+'-heading'}
    onKeyDown={e=>{if(e.key==='Escape'&&!e.defaultPrevented&&!busy){e.preventDefault();onClose()}}}>
    <header className="settings-section-heading"><h2 id={panelID+'-heading'} ref={heading} tabIndex={-1}><Blocks size={15} aria-hidden="true"/>会话能力绑定 <span className="conversation-config-badge">以后此会话</span></h2></header>
    {disabled&&<p role="status">{archived?'会话已归档，恢复后才能修改。':'会话正在处理，结束后才能修改。'}</p>}
    <form aria-labelledby={panelID+'-heading'} aria-busy={busy} onSubmit={event=>void submit(event)}>
      <div className="conversation-config-fields">
        <BindingCard spec={skill} binding={skillBinding} disabled={disabled||busy} onChange={()=>setNotice('')}/>
        <BindingCard spec={mcp} binding={mcpBinding} disabled={disabled||busy} onChange={()=>setNotice('')}/>
      </div>
      {notice&&<p className="conversation-binding-notice" role="status">{notice}</p>}
      <div className="conversation-binding-actions">
        <button type="button" className="outline" disabled={busy} onClick={onClose}><X size={13} aria-hidden="true"/>取消</button>
        <button type="submit" className="primary" disabled={disabled||busy||skillBinding.stale||mcpBinding.stale||!skillBinding.choiceAvailable||!mcpBinding.choiceAvailable}>
          <Check size={13} aria-hidden="true"/>{busy?'正在保存…':'确认绑定范围'}
        </button>
      </div>
    </form>
  </section>
}
