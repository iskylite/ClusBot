import {useCallback,useEffect,useId,useRef,useState,type FormEvent} from 'react'
import {api,type MCPConfig} from './api'
import {parseSurfaces,type Surface,type SurfaceAction} from './assistantSurface'
import {MCPSetupNextSteps} from './MCPSetupNextSteps'
import {AssistantSurfaceView} from './AssistantSurfaceView'

type Transport='stdio'|'http'
type Step={kind:'choose';surface:Surface}|{kind:'form';transport:Transport;surfaceID:string;revision:number;proof:string;request:string}|
  {kind:'saved';connection:string;revision:number;status:string;surface:Surface;bound?:string;bindingStatus?:string}|{kind:'message';text:string}
type DraftFields={name:string;command:string;args:string;env:string;endpoint:string;token_env:string}
type DraftPayload={request_key:string;form_proof:string;surface_id:string;revision:number;name:string;config:MCPConfig}
type DraftPreview={draft:DraftPayload;id:string;target:string;args:number;refs:number}
const emptyDraft:DraftFields={name:'',command:'',args:'',env:'',endpoint:'',token_env:''}
const object=(x:unknown):x is Record<string,unknown>=>x!==null&&typeof x==='object'&&!Array.isArray(x)
const hex=(x:unknown):x is string=>typeof x==='string'&&/^[a-f\d]{32}$/.test(x)
const positive=(x:unknown):x is number=>typeof x==='number'&&Number.isSafeInteger(x)&&x>0
function saved(x:Record<string,unknown>):Step|undefined{
  const surface=parseSurfaces(x.surface)
  if(surface?.length===1&&surface[0].id.startsWith('mcp_')&&x.state==='saved'&&hex(x.connection_id)&&positive(x.revision)&&typeof x.status==='string'&&['untested','ready','failed'].includes(x.status))
    return {kind:'saved',connection:x.connection_id,revision:x.revision,status:x.status,surface:surface[0],bound:typeof x.bound_tool==='string'?x.bound_tool:undefined,bindingStatus:typeof x.binding_status==='string'?x.binding_status:undefined}
}
export function MCPSetupCard({conversation,run}:{conversation:string;run:string}){
  const [step,setStep]=useState<Step>()
  const [busy,setBusy]=useState(false)
  const [error,setError]=useState('')
  const [preview,setPreview]=useState<DraftPreview>()
  const draftFields=useRef<DraftFields>(emptyDraft)
  const formLabel=useId()
  const previewHeading=useRef<HTMLHeadingElement>(null)
  const editName=useRef<HTMLInputElement>(null)
  const errorFocus=useRef<HTMLParagraphElement>(null)
  const returning=useRef(false)
  const gate=useRef(false)
  const generation=useRef(0)
  const attempt=useRef<{fingerprint:string;key:string}|undefined>(undefined)
  const base='/conversations/'+encodeURIComponent(conversation)+'/mcp-setup/'+encodeURIComponent(run)
  const scoped=useCallback((x:unknown):x is Record<string,unknown>=>object(x)&&x.conversation_id===conversation&&x.run_id===run,[conversation,run])
  const reload=useCallback(async()=>{
    const epoch=++generation.current
    draftFields.current=emptyDraft
    setError('');setStep(undefined);setPreview(undefined)
    try{
      const response=await api(base)
      if(!scoped(response))throw new Error('未收到原会话的接入记录。')
      const complete=saved(response)
      const surface=response.state==='choose_transport'?parseSurfaces(response.surface):undefined
      if(!complete&&(surface?.length!==1||!surface[0].id.startsWith('mcp_')||!['stdio','http'].every(name=>{
        const node=surface[0].nodes.get(name)
        return node?.component==='Button'&&node.action.event.name==='choose_mcp_transport'&&node.action.event.context?.transport===name
      })))throw new Error('接入选择无效，请重新打开。')
      if(epoch===generation.current)setStep(complete??{kind:'choose',surface:surface![0]})
    }catch(e){if(epoch===generation.current)setError(e instanceof Error?e.message:'无法读取接入记录')}
  },[base,scoped])
  useEffect(()=>{void reload();return()=>{generation.current++}},[reload])
  useEffect(()=>{
    if(preview)previewHeading.current?.focus()
    else if(returning.current){returning.current=false;editName.current?.focus()}
  },[preview])
  useEffect(()=>{if(error&&step?.kind==='form')errorFocus.current?.focus()},[error,step?.kind])
  async function choose(action:SurfaceAction){
    if(gate.current||step?.kind!=='choose')return
    const transport=action.action.context.transport
    if((transport!=='http'&&transport!=='stdio')||action.action.name!=='choose_mcp_transport'||action.action.sourceComponentId!==transport||action.action.surfaceId!==step.surface.id)return
    const epoch=generation.current
    const fingerprint=step.surface.id+':'+transport
    if(attempt.current?.fingerprint!==fingerprint)attempt.current={fingerprint,key:crypto.randomUUID()}
    const key=attempt.current.key
    gate.current=true;setBusy(true);setError('')
    try {
      const result=await api(base+'/actions',action,'POST',{'X-ClusBot-Request-Key':key})
      if(!scoped(result)||result.transport!==transport)throw new Error('未收到原会话的接入决定。')
      if(epoch!==generation.current)return
      if(result.state==='open_trusted_form'&&hex(result.form_proof)&&result.request_key===key&&positive(result.revision)&&result.revision===action.action.context.revision){
        setStep({kind:'form',transport,surfaceID:step.surface.id,proof:result.form_proof,revision:result.revision,request:key})
      }else if(result.state==='ask_for_details'||result.state==='cannot_prepare'){
        setStep({kind:'message',text:result.state==='ask_for_details'?'助手需要进一步确认连接方式，尚未创建连接。':'助手暂不能准备接入，尚未创建连接。'})
      }else throw new Error('助手没有返回受支持的接入决定。')
    }catch(e){if(epoch===generation.current)setError(e instanceof Error?e.message:'接入决定不可用')}
    finally{gate.current=false;if(epoch===generation.current)setBusy(false)}
  }
  async function validateDraft(event:FormEvent<HTMLFormElement>){
    event.preventDefault()
    if(gate.current||step?.kind!=='form'||preview)return
    const controls=new FormData(event.currentTarget)
    const value=(name:string)=>String(controls.get(name)??'')
    draftFields.current={name:value('name'),command:value('command'),args:value('args'),env:value('env'),endpoint:value('endpoint'),token_env:value('token_env')}
    const fields=draftFields.current
    const lines=(text:string)=>text.split('\n').filter(Boolean)
    const config:MCPConfig=step.transport==='http'?{transport:'http',endpoint:fields.endpoint.trim(),token_env:fields.token_env.trim()}:
      {transport:'stdio',command:fields.command.trim(),args:lines(fields.args),env:lines(fields.env).map(x=>x.trim()).filter(Boolean)}
    const draft:DraftPayload={request_key:step.request,form_proof:step.proof,surface_id:step.surfaceID,revision:step.revision,
      name:fields.name.trim(),config}
    const target=config.transport==='http'?config.endpoint:config.command
    const args=config.args?.length??0
    const refs=config.transport==='http'?(config.token_env?1:0):config.env?.length??0
    const epoch=generation.current
    gate.current=true;setBusy(true);setError('')
    try{
      const result=await api(base+'/preview',draft)
      if(!scoped(result)||result.state!=='configuration_reviewed'||!hex(result.preview_id)||result.request_key!==draft.request_key||
        result.revision!==draft.revision||result.name!==draft.name||result.transport!==config.transport||
        typeof target!=='string'||result.target!==target||result.argument_count!==args||result.credential_reference_count!==refs){
        throw new Error('未收到与当前配置匹配的预检结果。')
      }
      if(epoch===generation.current)setPreview({draft,id:result.preview_id,target,args,refs})
    }catch(e){if(epoch===generation.current)setError(e instanceof Error?e.message:'未收到配置预检结果，没有登记连接。')}
    finally{gate.current=false;if(epoch===generation.current)setBusy(false)}
  }
  async function saveReviewedDraft(event:FormEvent<HTMLFormElement>){
    event.preventDefault()
    if(gate.current||step?.kind!=='form'||!preview)return
    const epoch=generation.current
    gate.current=true;setBusy(true);setError('')
    try{
      const result=await api(base+'/connection',{...preview.draft,preview_id:preview.id})
      const next=scoped(result)?saved(result):undefined
      if(!next)throw new Error('未收到匹配的登记回执，请重新核对本次接入。')
      if(epoch===generation.current){draftFields.current=emptyDraft;setPreview(undefined);setStep(next)}
    }catch(e){if(epoch===generation.current)setError(e instanceof Error?e.message:'登记结果暂未确认，请重新读取接入记录。')}
    finally{gate.current=false;if(epoch===generation.current)setBusy(false)}
  }
  return <article className="assistant-card" aria-label="会话 MCP 接入" aria-busy={busy}>
    {step?.kind==='choose'&&<AssistantSurfaceView surface={step.surface} busy={busy} onAgentAction={choose}/>}
    {step?.kind==='form'&&!preview&&<form className="connection-form memory-form" onSubmit={validateDraft}
      method="post" action={'/api/v1'+base+'/preview'} autoComplete="off" aria-labelledby={formLabel} onInput={()=>{if(error)setError('')}}>
      <h3 id={formLabel}>{step.transport==='stdio'?'添加本地 MCP 程序':'添加远程 MCP 服务'}</h3>
      <p>先核对格式和名称，不登记、不启动程序或联网。凭据只填环境变量名，不填值，也不会发送给模型。</p>
      <fieldset disabled={busy}><legend>连接配置</legend>
      <label>连接名称<input ref={editName} name="name" defaultValue={draftFields.current.name} required maxLength={60}/></label>
      {step.transport==='http'?<><label>服务地址<input name="endpoint" type="url" defaultValue={draftFields.current.endpoint}
        required maxLength={2048} autoCorrect="off" autoCapitalize="none" spellCheck={false}/></label>
      <label>令牌所在环境变量（可选）<input name="token_env" defaultValue={draftFields.current.token_env} maxLength={80}
        autoCorrect="off" autoCapitalize="none" spellCheck={false} pattern="[A-Za-z_][A-Za-z0-9_]*"/></label></>:
      <><label>程序绝对路径<input name="command" defaultValue={draftFields.current.command} required maxLength={1024}
        autoCorrect="off" autoCapitalize="none" spellCheck={false}/></label><label>参数（每行一个）<textarea name="args" defaultValue={draftFields.current.args} rows={2}/>
      </label><label>环境变量名（每行一个，不填值）<textarea name="env" defaultValue={draftFields.current.env} rows={2}
        autoCorrect="off" autoCapitalize="none" spellCheck={false}/></label></>}
      <button type="submit" className="primary">核对连接配置</button></fieldset></form>}
    {step?.kind==='form'&&preview&&<form className="connection-form memory-form" onSubmit={saveReviewedDraft}
      method="post" action={'/api/v1'+base+'/connection'} aria-labelledby={formLabel}>
      <h3 ref={previewHeading} id={formLabel} tabIndex={-1}>确认登记连接</h3>
      <p>格式与名称已核对，尚未登记。没有测试程序、服务或凭据是否可用。</p>
      <dl>
        <dt>连接名称</dt><dd>{preview.draft.name}</dd>
        <dt>{preview.draft.config.transport==='stdio'?'本地程序':'服务地址'}</dt><dd><code>{preview.target}</code></dd>
        {preview.draft.config.transport==='stdio'&&<><dt>参数</dt><dd>{preview.args} 个</dd></>}
        <dt>凭据引用</dt><dd>{preview.refs} 个</dd>
      </dl>
      <p>确认只登记连接。握手、读取工具目录和授权仍需分别操作。</p>
      <button type="button" className="outline" disabled={busy} onClick={()=>{returning.current=true;setPreview(undefined);setError('')}}>返回修改</button>
      <button type="submit" className="primary" disabled={busy}>确认登记连接</button>
    </form>}
    {step?.kind==='saved'&&<>
      <p role="status">{step.status==='ready'?'已取得工具目录。':step.status==='failed'?'上次握手失败。':'已登记连接，尚未握手。'}{!step.bound?'此连接还未绑定到当前会话。':step.bindingStatus==='locally_ready'?'本会话已绑定只读工具：'+step.bound:'原绑定已失效，请重新核对。'}</p>
      <MCPSetupNextSteps key={step.surface.id} surface={step.surface} conversation={conversation} run={run} connectionRevision={step.revision} reload={reload}/>
    </>}
    {step?.kind==='message'&&<p role="status">{step.text}</p>}
    {error&&<p role="alert" className="error" ref={errorFocus} tabIndex={-1}>{error}</p>}
    {!step&&!error&&<p role="status">正在读取本次接入…</p>}
    {(error||step?.kind==='message')&&<button type="button" className="text-button" onClick={()=>void reload()} disabled={busy}>重新核对本次接入</button>}
  </article>
}
