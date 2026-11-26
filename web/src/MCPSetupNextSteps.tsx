import {useEffect,useRef,useState,type FormEvent} from 'react'
import {api} from './api'
import {AssistantSurfaceView} from './AssistantSurfaceView'
import type {Surface,SurfaceAction} from './assistantSurface'

type Operation='probe'|'permissions'|'binding'
type Tool={name:string;version:string;allowed:boolean;read_only:boolean;can_read:boolean}
type ProbeTarget={name:string;transport:'http'|'stdio';target:string;argument_count:number}
type Form={step:Operation;proof:string;request:string;revision:number;tools:Tool[];bindingRevision?:number;current?:string;probeTarget?:ProbeTarget}
const object=(v:unknown):v is Record<string,unknown>=>v!==null&&typeof v==='object'&&!Array.isArray(v)
const integer=(v:unknown):v is number=>typeof v==='number'&&Number.isSafeInteger(v)&&v>=0
const probeTarget=(v:unknown):v is ProbeTarget=>object(v)&&typeof v.name==='string'&&v.name.length>0&&v.name.length<=120&&
  (v.transport==='http'||v.transport==='stdio')&&typeof v.target==='string'&&v.target.length>0&&v.target.length<=2048&&integer(v.argument_count)&&v.argument_count<=24
const tool=(v:unknown):v is Tool=>object(v)&&typeof v.name==='string'&&v.name.length>0&&v.name.length<=200&&
  typeof v.version==='string'&&/^[a-f\d]{64}$/.test(v.version)&&typeof v.allowed==='boolean'&&typeof v.read_only==='boolean'&&typeof v.can_read==='boolean'

export function MCPSetupNextSteps({surface,conversation,run,connectionRevision,reload}:{surface:Surface;conversation:string;run:string;connectionRevision:number;reload:()=>Promise<void>}){
  const [form,setForm]=useState<Form>()
  const [selected,setSelected]=useState('')
  const [mode,setMode]=useState('')
  const [message,setMessage]=useState('')
  const [error,setError]=useState('')
  const [busy,setBusy]=useState(false)
  const gate=useRef(false)
  const live=useRef(true)
  const request=useRef<{sig:string;key:string}|undefined>(undefined)
  const path='/conversations/'+encodeURIComponent(conversation)+'/mcp-setup/'+encodeURIComponent(run)
  useEffect(()=>{live.current=true;return()=>{live.current=false}},[])
  const selectedTool=form?.tools.find(t=>t.name===selected)
  async function choose(a:SurfaceAction){
    const step=a.action.context.step
    if(gate.current||a.action.name!=='request_mcp_setup_step'||a.action.surfaceId!==surface.id||a.action.sourceComponentId!==step||
      a.action.context.connection_revision!==connectionRevision||(step!=='probe'&&step!=='binding'&&step!=='permissions'))return
    const sig=surface.id+':'+step
    if(request.current?.sig!==sig)request.current={sig,key:crypto.randomUUID()}
    const key=request.current.key
    gate.current=true;setBusy(true);setError('');setMessage('')
    try{
      const r=await api(path+'/actions',a,'POST',{'X-ClusBot-Request-Key':key})
      if(!object(r)||r.conversation_id!==conversation||r.run_id!==run||r.step!==step)throw new Error('无法核对此操作的会话和步骤。')
      if(!live.current)return
      if(r.state==='ask_for_details'||r.state==='cannot_prepare'){setMessage('助手暂未准备本步骤的表单，没有执行操作。');return}
      if(r.state!=='open_trusted_form'||r.request_key!==key||typeof r.form_proof!=='string'||!/^[a-f\d]{32}$/.test(r.form_proof)||
        !integer(r.revision)||r.revision!==a.action.context.revision||r.connection_revision!==connectionRevision||
        (step==='probe'&&!probeTarget(r.probe_target))||(step!=='probe'&&(!Array.isArray(r.tools)||r.tools.length>1024||!r.tools.every(tool)))||
        (step==='binding'&&!integer(r.binding_revision)))throw new Error('未收到此操作的有效受信表单。')
      setForm({step,proof:r.form_proof,revision:r.revision,request:key,tools:step==='probe'?[]:r.tools as Tool[],bindingRevision:step==='binding'?r.binding_revision as number:undefined,current:typeof r.current_tool==='string'?r.current_tool:undefined,probeTarget:probeTarget(r.probe_target)?r.probe_target:undefined})
      setSelected('');setMode('')
    }catch(e){if(live.current)setError(e instanceof Error?e.message:'无法准备本步骤。')}
    finally{gate.current=false;if(live.current)setBusy(false)}
  }
  async function apply(e:FormEvent){
    e.preventDefault()
    if(gate.current||!form||form.step!=='probe'&&!selectedTool||form.step==='permissions'&&!mode)return
    gate.current=true;setBusy(true);setError('')
    try{
      const payload={step:form.step,surface_id:surface.id,revision:form.revision,connection_revision:connectionRevision,form_proof:form.proof,request_key:form.request,
        ...(selectedTool?{tool:selectedTool.name,tool_version:selectedTool.version}:{}),...(form.step==='permissions'?{mode}:{}),...(form.step==='binding'?{binding_revision:form.bindingRevision}:{})}
      const result=await api(path+'/operation',payload)
      if(!object(result)||result.state!=='saved'||result.conversation_id!==conversation||result.run_id!==run)throw new Error('未收到与操作匹配的状态，请重新核对。')
      if(live.current)await reload()
    }catch(e){if(live.current){setForm(undefined);setError(e instanceof Error?e.message:'未收到确定回执，请重新读取连接记录。')}}
    finally{gate.current=false;if(live.current)setBusy(false)}
  }
  return <>
    {!form&&<AssistantSurfaceView surface={surface} busy={busy} onAgentAction={choose}/>}
    {form&&<form className="connection-form memory-form" onSubmit={apply}><fieldset disabled={busy}>
      <h3>{form.step==='probe'?'握手并读取工具目录':form.step==='permissions'?'修改实例工具授权':'绑定本会话只读工具'}</h3>
      {form.step==='probe'?<><p>{form.probeTarget?.name}：{form.probeTarget?.transport==='stdio'?'将启动本地程序':'将连接服务'} <code>{form.probeTarget?.target}</code>。{form.probeTarget?.transport==='stdio'&&'使用已保存的 '+form.probeTarget.argument_count+' 个参数。'}</p><p>仅进行握手、读取工具目录，不调用业务工具。</p></>:
      <><label>工具<select aria-label="工具" required value={selected} onChange={e=>{setSelected(e.target.value);setMode('')}}><option value="">请选择</option>
        {form.tools.map(t=><option key={t.name} value={t.name}>{t.name}{form.step==='permissions'?' · '+(t.read_only?'只读自动':t.allowed?'受控':'未授权'):''}</option>)}</select></label>
        {form.step==='permissions'?<><label>权限<select aria-label="权限" required value={mode} onChange={e=>setMode(e.target.value)}><option value="">请选择权限</option>
          <option value="controlled">允许受控调用</option>{selectedTool?.can_read&&<option value="read_only">允许只读自动调用</option>}<option value="revoked">撤销权限</option></select></label>
          <p>这会更改全实例的工具授权，不是仅授予当前会话。授予只读自动调用前，请核实工具没有副作用。</p></>:
        <p>{form.current?'当前已选 '+form.current+'，确认后将替换该选择。':'只影响当前会话，不会授予新权限或调用工具。'}</p>}
      </>}
      <button className="outline" type="button" onClick={()=>{setForm(undefined);request.current=undefined}}>返回</button>
      <button className="primary" type="submit" disabled={form.step!=='probe'&&!selectedTool||form.step==='permissions'&&!mode}>
        {form.step==='probe'?'开始握手':form.step==='permissions'?'确认修改实例授权':'确认绑定本会话'}</button>
    </fieldset></form>}
    {message&&<p role="status">{message}</p>}
    {error&&<><p role="alert" className="error">{error}</p><button type="button" className="text-button" disabled={busy} onClick={()=>void reload()}>重新读取连接状态</button></>}
  </>
}
