import {useCallback, useEffect, useId, useRef, useState, type FormEvent} from 'react'
import {Activity, Braces, Check, ChevronDown, ChevronRight, FileText, Image, Info, KeyRound, Plus, RefreshCw, Server, Sparkles, Star, Trash2, Wrench, Zap} from 'lucide-react'
import {api, APIError, type DiscoveredModel, type ModelProtocol} from './api'
import {ModelRoutingSettings} from './ModelRoutingSettings'
import './ModelSettings.css'

type Attributes = DiscoveredModel['attributes']
type AttributeKey = Exclude<keyof Attributes,'sources'>
type Provider = {id:string; name:string; protocol:ModelProtocol; base_url:string; auth_mode:'api_key'|'environment'|'none'; credential_status:string; enabled:boolean; revision:number; model_count:number}
export type CatalogModel = {id:string; name:string; provider_id:string; provider_revision:number; enabled:boolean; provider_enabled:boolean; model_id:string; revision:number; attributes?:Attributes; credential_configured:boolean}
type DefaultModel = {source:string; revision:number; profile_id?:string; profile_revision?:number; status:string}
type Diagnostic = {connection:string; authentication:string; http_status?:number; revision:number}
type Discovered = DiscoveredModel
type ConversationRoute = {source:string;status:string;model_id?:string;remote_observation:string}

const brandLogos=import.meta.glob('./ai-brands/*.svg',{eager:true,query:'?url',import:'default'}) as Record<string,string>
const providerBrand=(provider:Provider)=>{
  const value=(provider.name+' '+provider.base_url).toLowerCase()
  return ['deepseek','openai','anthropic','ollama','gemini','qwen','moonshot','minimax','mistral','meta','zhipu','grok','kimi'].find(name=>new RegExp('(^|[^a-z0-9])'+name+'([^a-z0-9]|$)').test(value))
}
function ProviderMark({provider}:{provider:Provider}){
  const brand=providerBrand(provider)
  const logo=brand&&brandLogos['./ai-brands/'+brand+'.svg']
  return logo?<img className={'model-brand model-brand-'+brand} src={logo} alt=""/>:<Server className="model-brand" size={15} aria-hidden="true"/>
}

const root = '/settings/model'
const errorMessage = (e:unknown) => e instanceof APIError && e.status === 409
  ? '配置版本已变化，未保存。请重新读取后核对并重试。'
  : e instanceof Error ? e.message : '请求未完成，请重试'
const credentialLabel:Record<string,string> = {configured:'凭据已配置',missing:'凭据缺失',not_required:'无需凭据'}
const statusLabel:Record<string,string> = {selected:'已选择',profile_missing:'模型已不存在',profile_disabled:'模型已停用',provider_disabled:'提供方已停用',credential_missing:'凭据缺失',invalid_configuration:'模型配置无效'}
const routeStatus:Record<string,string> = {configured:'本地配置可用',locally_configured:'本地配置可用',local_demo:'演示模式',not_configured:'未配置',settings_unavailable:'模型设置不可用',default_unavailable:'默认模型不可用',policy_unavailable:'阶段策略不可用',profile_missing:'模型已不存在',profile_disabled:'模型已停用',provider_disabled:'提供方已停用',credential_missing:'凭据不可用',credential_changed:'凭据已变化，请重新绑定',profile_revision_changed:'模型版本已变化，请重新绑定',profile_invalid:'模型配置无效',invalid_configuration:'模型配置无效',default_destination_mismatch:'模型与默认服务不一致',demo_profile_disabled:'演示模式不可用',demo_role_unavailable:'演示模式不可用'}
const routeSource:Record<string,string> = {business_override:'业务覆盖',role_policy:'阶段策略',global_default:'全局默认',explicit_profile:'角色绑定'}

export function ModelSettings({onChanged,mode='catalog'}:{onChanged?:()=>Promise<void>;mode?:'catalog'|'routing'}) {
  const [providers,setProviders]=useState<Provider[]>()
  const [models,setModels]=useState<CatalogModel[]>([])
  const [defaultModel,setDefaultModel]=useState<DefaultModel>()
  const [selected,setSelected]=useState(()=>new URLSearchParams(location.search).get('provider')??'')
  const [requestedModel]=useState(()=>new URLSearchParams(location.search).get('model'))
  const [expandedModel,setExpandedModel]=useState<string|null>(null)
  const [modelDirty,setModelDirty]=useState(false)
  const [modelEditorBusy,setModelEditorBusy]=useState(false)
  const [editing,setEditing]=useState<Provider|true>()
  const [newModel,setNewModel]=useState<Discovered|false>(false)
  const [discovered,setDiscovered]=useState<Discovered[]>()
  const [diagnostic,setDiagnostic]=useState<Diagnostic>()
  const [conversationRoute,setConversationRoute]=useState<ConversationRoute|null>()
  const [error,setError]=useState('')
  const [notice,setNotice]=useState('')
  const [busy,setBusy]=useState('')
  const [generation,setGeneration]=useState(0)
  const load=useCallback(async()=>{
    const [p,m,d,route]=await Promise.all([
      api<Provider[]>(root+'/providers'),api<CatalogModel[]>(root+'/catalog'),api<DefaultModel>(root+'/default'),
      api<ConversationRoute>(root+'/routing/preview',{role:'conversation_reply',business:'conversation'}).catch(()=>null),
    ])
    setProviders(p);setModels(m);setDefaultModel(d);setConversationRoute(route)
    setSelected(old=>p.some(item=>item.id===old)?old:p[0]?.id??'')
    setGeneration(n=>n+1)
    if(onChanged)void onChanged().catch(()=>undefined)
  },[onChanged])
  useEffect(()=>{let alive=true;void load().catch(e=>{if(alive)setError(errorMessage(e))});return()=>{alive=false}},[load])
  const provider=providers?.find(item=>item.id===selected)
  const visible=models.filter(item=>item.provider_id===selected)
  const firstOpenModel=visible.find(item=>item.id===requestedModel)??visible[0]
  const openModel=expandedModel===null?firstOpenModel?.id:expandedModel
  async function run(key:string, action:()=>Promise<void>) {
    if(busy)return
    setBusy(key);setError('');setNotice('')
    try{await action()}catch(e){setError(errorMessage(e))}finally{setBusy('')}
  }
  function discardModelDraft(){return !modelEditorBusy&&(!modelDirty||window.confirm('当前模型有未保存的修改，切换后将丢弃。确认继续？'))}
  function selectProvider(id:string){
    if(!discardModelDraft())return
    setSelected(id);setExpandedModel(null);setModelDirty(false);setEditing(undefined);setNewModel(false);setDiscovered(undefined);setDiagnostic(undefined);setNotice('');setError('')
  }
  function openProviderEditor(item:Provider|true){
    if(!discardModelDraft())return
    setEditing(item);setExpandedModel('');setModelDirty(false);setNewModel(false);setError('')
  }
  function openNewModel(item:Discovered){
    if(!discardModelDraft())return
    setExpandedModel('');setModelDirty(false);setNewModel(item)
  }
  function toggleEditor(id:string){
    if(modelEditorBusy)return
    if(openModel===id&&!newModel){setExpandedModel('');setModelDirty(false);return}
    if(!discardModelDraft())return
    setNewModel(false);setExpandedModel(id);setModelDirty(false)
  }
  function updateProvider(item:Provider,enabled:boolean){
    void run('provider',async()=>{
      await api(root+'/providers/'+encodeURIComponent(item.id),{name:item.name,protocol:item.protocol,base_url:item.base_url,auth_mode:item.auth_mode,enabled,revision:item.revision},'PUT')
      await load();setDiagnostic(undefined);setNotice(enabled?'提供方已启用。':'提供方已停用。')
    })
  }
  function deleteProvider(item:Provider){
    if(!window.confirm(`确定删除提供方“${item.name}”？`))return
    void run('provider',async()=>{await api(root+'/providers/'+encodeURIComponent(item.id),{revision:item.revision},'DELETE');await load();setEditing(undefined);setNotice('提供方已删除。')})
  }
  function discover(){
    if(!provider)return
    void run('discover',async()=>{const result=await api<Discovered[]>(root+'/providers/'+encodeURIComponent(provider.id)+'/discover',{revision:provider.revision});setDiscovered(result);setNotice(result.length?'请选择要加入目录的模型。':'服务没有返回模型。')})
  }
  function diagnose(){
    if(!provider)return
    void run('diagnose',async()=>{const result=await api<Diagnostic>(root+'/providers/'+encodeURIComponent(provider.id)+'/diagnose',{revision:provider.revision});setDiagnostic(result)})
  }
  function selectDefault(model:CatalogModel){
    if(!defaultModel)return
    if(!window.confirm(`设为全局默认后，未指定模型的阶段可能将对话和任务内容交由“${model.name}”处理。确认继续？`))return
    void run('default',async()=>{
      const input={source:'profile',revision:defaultModel.revision,profile_id:model.id,profile_revision:model.revision}
      const value=await api<DefaultModel>(root+'/default',input,'PUT');setDefaultModel(value);setConversationRoute(await api<ConversationRoute>(root+'/routing/preview',{role:'conversation_reply',business:'conversation'}).catch(()=>null));setGeneration(n=>n+1);if(onChanged)void onChanged().catch(()=>undefined);setNotice('全局默认模型已更新；如有阶段策略，以阶段策略为准。')
    })
  }
  function toggleModel(model:CatalogModel,enabled:boolean){
    if(!provider)return
    void run('model',async()=>{
      await api(root+'/catalog/'+encodeURIComponent(model.id),{name:model.name,provider_id:provider.id,provider_revision:provider.revision,model_id:model.model_id,attributes:model.attributes??{},enabled,revision:model.revision},'PUT')
      await load();setNotice(enabled?'模型已启用。':'模型已停用。')
    })
  }
  function deleteModel(model:CatalogModel){
    if(!window.confirm(`确定删除模型“${model.name}”？`))return
    void run('model',async()=>{await api(root+'/catalog/'+encodeURIComponent(model.id),{revision:model.revision},'DELETE');await load();setNotice('模型已删除。')})
  }
  if(mode==='routing')return <>
    {!providers&&!error&&<p className="muted" role="status">正在读取模型…</p>}
    {providers&&<ModelRoutingSettings models={models} providers={providers} generation={generation} onPolicySaved={load}/>}
    {error&&<p className="error" role="alert">{error} <button type="button" className="model-link" onClick={()=>void load().then(()=>setError('')).catch(e=>setError(errorMessage(e)))}>重新读取</button></p>}
  </>
  return <>
    <section className="model-catalog" aria-label="模型提供方和目录">
      <div className="model-provider-panel">
        <div className="model-panel-heading"><h2>服务提供方 ({providers?.length??0})</h2><button className="model-button primary" type="button" disabled={Boolean(busy)||modelEditorBusy} onClick={()=>openProviderEditor(true)}><Plus size={14}/>添加</button></div>
        {!providers&&!error&&<p className="muted" role="status">正在读取提供方…</p>}
        {providers?.length===0&&<p className="muted">还没有提供方。</p>}
        <div className="model-provider-list">{providers?.map(item=><div key={item.id} className={'model-provider-card'+(selected===item.id?' selected':'')}>
          <button type="button" className="model-provider-select" disabled={Boolean(busy)||modelEditorBusy} aria-pressed={selected===item.id} onClick={()=>selectProvider(item.id)}>
            <span className="model-provider-line"><span className="model-provider-name"><ProviderMark provider={item}/><strong title={item.name}>{item.name}</strong></span></span>
            <span className="model-url" title={item.base_url}>{item.base_url.replace(/^https?:\/\//,'')}</span>
            <span className="model-provider-line">
              <span>{item.auth_mode==='api_key'?`API Key${item.credential_status==='configured'?' · ••••••••':''}`:item.auth_mode==='environment'?'环境变量':'无凭据'}</span>
              <span className={'model-status '+(item.credential_status==='missing'?'warning':'')}>{credentialLabel[item.credential_status]??item.credential_status}</span>
            </span>
          </button>
          <span className="model-provider-count">{item.model_count} 个模型</span>
          <label className="model-switch" title={item.enabled?'停用提供方':'启用提供方'}><input type="checkbox" checked={item.enabled} disabled={Boolean(busy)||modelEditorBusy} onChange={e=>updateProvider(item,e.currentTarget.checked)}/><span aria-hidden="true"/><span className="model-sr-only">启用 {item.name}</span></label>
        </div>)}</div>
      </div>
      <div className="model-catalog-panel">
        <div className="model-routing-preview model-routing-bar" role="status">
          <div>
            <strong>当前对话生效</strong>
            <span>{conversationRoute===undefined?'正在读取…':conversationRoute===null?<>无法读取当前路由。 <button type="button" className="model-link" onClick={()=>void load().catch(e=>setError(errorMessage(e)))}>重新读取</button></>:`${conversationRoute.model_id??'未选择模型'} · ${routeStatus[conversationRoute.status]??conversationRoute.status}${routeSource[conversationRoute.source]?' · '+routeSource[conversationRoute.source]:''}`}</span>
          </div>
          <button type="button" className="model-button" onClick={()=>{const target='/settings?tab=model&stage=conversation_reply#model-routing';history.pushState(null,'',target);window.dispatchEvent(new Event('clusbot:navigate'))}}>调整策略 →</button>
        </div>
        {editing ? <ProviderForm key={editing===true?'new':editing.id+':'+editing.revision} provider={editing===true?undefined:editing} onCancel={()=>setEditing(undefined)} onSaved={async item=>{await load();setSelected(item.id);setEditing(undefined);setDiagnostic(undefined);setDiscovered(undefined);setNotice('提供方已保存。')}} onDelete={deleteProvider} onReload={load}/>: provider ? <>
          <div className="model-panel-heading model-catalog-heading"><h2>{provider.name} · 模型清单 ({visible.length})</h2><div className="model-toolbar">
            <button className="model-button" type="button" disabled={Boolean(busy)||modelEditorBusy||!provider.enabled} onClick={diagnose}><Activity size={14}/>诊断连接</button>
            <button className="model-button" type="button" disabled={Boolean(busy)||modelEditorBusy} onClick={()=>openProviderEditor(provider)}><KeyRound size={14}/>编辑连接/凭据</button>
            <button className="model-button primary" type="button" disabled={Boolean(busy)||modelEditorBusy||!provider.enabled} onClick={discover}><RefreshCw size={14}/>自动拉取模型</button>
          </div></div>
          {diagnostic&&<p className="model-feedback" role="status">连接：{diagnostic.connection==='reachable'?'可达':diagnostic.connection==='failed'?'失败':'未检查'}；认证：{({credential_missing:'凭据缺失',not_used:'无需凭据',rejected:'被拒绝',not_rejected:'未被拒绝',unknown:'未知'} as Record<string,string>)[diagnostic.authentication]??diagnostic.authentication}{diagnostic.http_status?` · HTTP ${diagnostic.http_status}`:''}。诊断不代表模型试答成功。</p>}
          {discovered&&discovered.length>0&&<div className="model-discovery"><strong>拉取结果</strong><ul>{discovered.map(item=><li key={item.id}><span>{item.name&&item.name!==item.id?<>{item.name} · </>:null}{item.id}</span>{visible.some(m=>m.model_id===item.id)?<small>已在目录中</small>:<button type="button" className="model-button" disabled={modelEditorBusy} onClick={()=>openNewModel(item)}>添加</button>}</li>)}</ul></div>}
          {defaultModel&&defaultModel.status!=='selected'&&<p className="error" role="status">全局默认：{statusLabel[defaultModel.status]??defaultModel.status}</p>}
          {newModel&&<ModelForm key={'new:'+newModel.id} provider={provider} discovered={newModel} onBusyChange={setModelEditorBusy} onDirtyChange={setModelDirty} onCancel={()=>{setNewModel(false);setModelDirty(false)}} onSaved={async saved=>{setNewModel(false);setModelDirty(false);setExpandedModel(saved.id);await load();setNotice('模型已添加。')}}/>}
          {visible.length===0&&!newModel&&<p className="muted">还没有模型，可手动添加或从服务拉取。</p>}
          <div className="model-catalog-list">{visible.map(model=><ModelCard key={model.id+':'+model.revision} open={openModel===model.id&&!newModel} onOpenChange={()=>toggleEditor(model.id)} onDirtyChange={setModelDirty} onEditorBusyChange={setModelEditorBusy} model={model} provider={provider} isDefault={defaultModel?.source==='profile'&&defaultModel.profile_id===model.id} busy={Boolean(busy)||modelEditorBusy} onToggle={toggleModel} onDelete={deleteModel} onDefault={()=>selectDefault(model)} onSaved={async()=>{await load();setModelDirty(false);setNotice('模型已保存。')}} onReload={load}/>)}</div>
          {!newModel&&<div className="model-list-actions"><button type="button" className="model-button" disabled={Boolean(busy)||modelEditorBusy} onClick={()=>openNewModel({id:'',attributes:{}})}><Plus size={14}/>手动添加模型</button></div>}
        </>:<p className="muted">选择或添加一个服务提供方。</p>}
        {notice&&<p className="model-feedback" role="status">{notice}</p>}
        {error&&<p className="error" role="alert">{error} <button type="button" className="model-link" onClick={()=>void load().then(()=>{setError('');setNotice('已重新读取，请核对配置。')}).catch(e=>setError(errorMessage(e)))}>重新读取</button></p>}
      </div>
    </section>
  </>
}

function ProviderForm({provider,onCancel,onSaved,onDelete,onReload}:{provider?:Provider;onCancel:()=>void;onSaved:(p:Provider)=>Promise<void>;onDelete:(p:Provider)=>void;onReload:()=>Promise<void>}) {
  const [name,setName]=useState(provider?.name??'')
  const [protocol,setProtocol]=useState<ModelProtocol>(provider?.protocol??'responses')
  const [baseURL,setBaseURL]=useState(provider?.base_url??'')
  const [auth,setAuth]=useState<Provider['auth_mode']>(provider?.auth_mode??'api_key')
  const [env,setEnv]=useState('')
  const [enabled,setEnabled]=useState(provider?.enabled??true)
  const [busy,setBusy]=useState(false),[error,setError]=useState('')
  const keyInput=useRef<HTMLInputElement>(null)
  const keyRequired=auth==='api_key'&&(!provider||provider.auth_mode!=='api_key'||provider.base_url!==baseURL||provider.protocol!==protocol)
  async function save(e:FormEvent<HTMLFormElement>){
    e.preventDefault();if(busy)return
    const key=keyInput.current?.value.trim()??''
    if(keyInput.current)keyInput.current.value=''
    setBusy(true);setError('')
    try{
      const value=await api<Provider>(root+'/providers'+(provider?'/'+encodeURIComponent(provider.id):''),{name,protocol,base_url:baseURL,auth_mode:auth,enabled,revision:provider?.revision??0,...(auth==='environment'&&env.trim()?{api_key_ref:'env:'+env.trim()}:{}),...(auth==='api_key'&&key?{api_key:key}:{})},provider?'PUT':'POST')
      await onSaved(value)
    }catch(e){setError(errorMessage(e))}finally{setBusy(false)}
  }
  return <form className="model-editor" aria-label={provider?'编辑提供方连接':'添加服务提供方'} onSubmit={save}>
    <div className="model-panel-heading"><h2>{provider?'编辑连接/凭据':'添加服务提供方'}</h2>{provider&&<button type="button" className="model-link danger" disabled={busy} onClick={()=>onDelete(provider)}><Trash2 size={14}/>删除提供方</button>}</div>
    <div className="model-edit-grid"><label>名称<input value={name} onChange={e=>setName(e.currentTarget.value)} required maxLength={60}/></label><label>协议<select value={protocol} onChange={e=>setProtocol(e.currentTarget.value as ModelProtocol)}><option value="responses">Responses</option><option value="chat_completions">Chat Completions</option></select></label></div>
    <label>服务地址<input type="url" value={baseURL} onChange={e=>setBaseURL(e.currentTarget.value)} required maxLength={2048} placeholder="https://example.com/v1"/></label>
    <label>认证方式<select value={auth} onChange={e=>setAuth(e.currentTarget.value as Provider['auth_mode'])}><option value="api_key">API Key</option><option value="environment">环境变量</option><option value="none">本机无凭据</option></select></label>
    {auth==='api_key'&&<label>API Key<input ref={keyInput} type="password" autoComplete="new-password" spellCheck={false} maxLength={4096} required={keyRequired} placeholder={keyRequired?'输入 API Key':'留空以保留现有密钥'}/></label>}
    {auth==='environment'&&<label>密钥环境变量<input value={env} onChange={e=>setEnv(e.currentTarget.value)} required={!provider||provider.auth_mode!=='environment'} pattern="[A-Za-z_][A-Za-z0-9_]*" maxLength={80} autoComplete="off" placeholder={provider?.auth_mode==='environment'?'留空以保留现有引用':'例如 MODEL_API_KEY'}/></label>}
    <label className="model-check"><input type="checkbox" checked={enabled} onChange={e=>setEnabled(e.currentTarget.checked)}/>启用提供方</label>
    {error&&<p className="error" role="alert">{error} {provider&&<button type="button" className="model-link" onClick={()=>void onReload().then(onCancel).catch(e=>setError(errorMessage(e)))}>重新读取</button>}</p>}
    <div className="model-form-actions"><button type="button" className="model-button" disabled={busy} onClick={onCancel}>取消</button><button type="submit" className="model-button primary" disabled={busy}>{busy?'正在保存…':'保存连接'}</button></div>
  </form>
}

function ModelCard({model,provider,isDefault,busy,onToggle,onDelete,onDefault,onSaved,onReload,open,onOpenChange,onDirtyChange,onEditorBusyChange}:{model:CatalogModel;provider:Provider;isDefault:boolean;busy:boolean;open:boolean;onOpenChange:()=>void;onDirtyChange:(dirty:boolean)=>void;onEditorBusyChange:(busy:boolean)=>void;onToggle:(m:CatalogModel,e:boolean)=>void;onDelete:(m:CatalogModel)=>void;onDefault:()=>void;onSaved:()=>Promise<void>;onReload:()=>Promise<void>}) {
  const [enabledDraft,setEnabledDraft]=useState(model.enabled)
  const [editorBusy,setEditorBusy]=useState(false)
  useEffect(()=>{if(!open)setEnabledDraft(model.enabled)},[open,model.enabled])
  const updateBusy=useCallback((value:boolean)=>{setEditorBusy(value);onEditorBusyChange(value)},[onEditorBusyChange])
  const closeEditor=()=>{setEnabledDraft(model.enabled);onOpenChange()}
  const levels=model.attributes?.reasoning_levels?.map(level=>({low:'低',medium:'中',high:'高'} as Record<string,string>)[level]??level).join('/')
  const reasoning=model.attributes?.reasoning===true ? open&&levels?`推理 ${levels}`:'推理 ✓' : model.attributes?.reasoning===false?'推理 ×':''
  const tools=model.attributes?.tool_calling===true?'工具 ✓':model.attributes?.tool_calling===false?'工具 ×':''
  const summary = [model.attributes?.context_tokens ? `${Math.round(model.attributes.context_tokens/1000)}K` : '', ...(open?[tools,reasoning]:[reasoning,tools]), model.attributes?.vision === true ? '识图 ✓' : model.attributes?.vision === false ? '识图 ×' : '', !model.enabled?'已停用':''].filter(Boolean).join(' · ')
  return <div className={'model-card'+(open?' expanded':'')}>
    <div className="model-card-head"><button type="button" className="model-card-toggle" aria-expanded={open} title={open?'收起并取消未保存的修改':'展开模型配置'} disabled={busy||editorBusy} onClick={()=>{if(open)closeEditor();else onOpenChange()}}>{open?<ChevronDown size={15}/>:<ChevronRight size={15}/>}<strong>{model.name}</strong>{open&&<small className="model-revision">配置修订 #{model.revision}<Info size={11} aria-hidden="true"/></small>}<ProviderMark provider={provider}/>{isDefault&&<span className="model-default-tag">全局默认</span>}<small className="model-card-summary">{summary}</small></button>
      {open&&<span className="model-enabled-label">启用</span>}<label className="model-switch"><input type="checkbox" checked={open?enabledDraft:model.enabled} disabled={busy||editorBusy} onChange={e=>{if(open)setEnabledDraft(e.currentTarget.checked);else onToggle(model,e.currentTarget.checked)}}/><span aria-hidden="true"/><span className="model-sr-only">启用 {model.name}</span></label></div>
    {open&&<ModelForm provider={provider} model={model} enabledOverride={enabledDraft} onBusyChange={updateBusy} onDirtyChange={onDirtyChange} onCancel={closeEditor} onSaved={onSaved} onDelete={()=>onDelete(model)} onDefault={onDefault} isDefault={isDefault} onReload={onReload}/>}
  </div>
}

function ModelForm({provider,model,discovered,onCancel,onSaved,onDelete,onDefault,isDefault,onReload,enabledOverride,onBusyChange,onDirtyChange}:{provider:Provider;model?:CatalogModel;discovered?:Discovered;onCancel:()=>void;onSaved:(saved:CatalogModel)=>Promise<void>;onDelete?:()=>void;onDefault?:()=>void;isDefault?:boolean;onReload?:()=>Promise<void>;enabledOverride?:boolean;onBusyChange?:(busy:boolean)=>void;onDirtyChange?:(dirty:boolean)=>void}) {
  const initial=()=>({name:model?.name??discovered?.name??Array.from(discovered?.id??'').slice(0,60).join(''),model_id:model?.model_id??discovered?.id??'',attributes:{...(model?.attributes??discovered?.attributes??{})},enabled:model?.enabled??true})
  const [draft,setDraft]=useState(initial),[levels,setLevels]=useState(()=>draft.attributes.reasoning_levels?.join(', ')??'')
  const formId=useId()
  const [busy,setBusy]=useState(''),[error,setError]=useState(''),[notice,setNotice]=useState('')
  useEffect(()=>{onBusyChange?.(Boolean(busy));return()=>onBusyChange?.(false)},[busy,onBusyChange])
  const attributes:Attributes={...draft.attributes,reasoning_levels:[...new Set(levels.split(/[,，]/).map(x=>x.trim()).filter(Boolean))]}
  if(attributes.reasoning_effort&&!attributes.reasoning_levels?.includes(attributes.reasoning_effort))attributes.reasoning_effort=undefined
  const currentDraft={...draft,enabled:enabledOverride??draft.enabled}
  const standardLevels={low:'低',medium:'中',high:'高'}
  const levelOptions=[...new Set([...Object.keys(standardLevels),...(initial().attributes.reasoning_levels??[]),...(attributes.reasoning_levels??[])])]
  const dirty=JSON.stringify({...currentDraft,attributes})!==JSON.stringify({...initial(),attributes:{...initial().attributes,reasoning_levels:initial().attributes.reasoning_levels??[]}})
  useEffect(()=>{onDirtyChange?.(dirty)},[dirty,onDirtyChange])
  function attr<K extends AttributeKey>(key:K,value:Attributes[K]){setDraft(old=>({...old,attributes:{...old.attributes,[key]:value,sources:{...old.attributes.sources,[key]:'manual'}}}));setNotice('')}
  function editLevels(value:string){setLevels(value);setDraft(old=>({...old,attributes:{...old.attributes,sources:{...old.attributes.sources,reasoning_levels:'manual'}}}));setNotice('')}
  function source(key:AttributeKey){
    const value=attributes[key]
    const origin=attributes.sources?.[key]
    return origin==='provider'?'服务声明':origin==='manual'?'手动':value!==undefined&&(!Array.isArray(value)||value.length>0)?'来源未记录':'未知'
  }
  async function save(e:FormEvent<HTMLFormElement>){e.preventDefault();if(busy)return;setBusy('save');setError('');setNotice('')
    try{const saved=await api<CatalogModel>(root+'/catalog'+(model?'/'+encodeURIComponent(model.id):''),{...currentDraft,attributes,provider_id:provider.id,provider_revision:provider.revision,revision:model?.revision??0},model?'PUT':'POST');await onSaved(saved)}catch(e){setError(errorMessage(e))}finally{setBusy('')}
  }
  async function test(){if(!model||busy)return;setBusy('test');setError('');setNotice('')
    try{const result=await api<{duration_ms:number;revision:number}>(root+'/catalog/'+encodeURIComponent(model.id)+'/test',{revision:model.revision});if(result.revision!==model.revision)throw new Error('试答回执版本无法核对');setNotice(`已保存版本试答通过 · ${result.duration_ms} 毫秒。此结果仅在当前页面保留，不会切换对话路由。`)}catch(e){setError(errorMessage(e))}finally{setBusy('')}
  }
  const title=model?`编辑模型 ${model.name}`:discovered?.id?`添加 ${discovered.name??discovered.id}`:'添加模型'
  return <form className="model-editor model-card-editor" aria-label={title} onSubmit={save}>
    {!model&&<h3>{title}</h3>}
    <div className="model-edit-grid four"><label>显示名称<input value={draft.name} required maxLength={60} onChange={e=>setDraft({...draft,name:e.currentTarget.value})}/></label><label>模型 ID<input value={draft.model_id} required maxLength={256} spellCheck={false} autoCapitalize="none" onChange={e=>{setDraft({...draft,model_id:e.currentTarget.value,attributes:{}});setLevels('');setNotice('')}}/></label>
      {([['context_tokens','上下文 (tokens)'],['max_output_tokens','最大输出']] as const).map(([key,label])=><label key={key}><span>{label} <small className="model-attribute-source">{source(key)}</small></span><input type="number" min={1} step={1} placeholder="未知" value={attributes[key]??''} onChange={e=>attr(key,e.currentTarget.value?e.currentTarget.valueAsNumber:undefined)}/></label>)}</div>
    <div className="model-reasoning-row">
      <div className="model-reasoning-toggle"><Sparkles size={13} aria-hidden="true"/><strong>支持思考 / 推理</strong><label className="model-switch"><input type="checkbox" aria-label="支持思考 / 推理" aria-checked={attributes.reasoning===undefined?'mixed':attributes.reasoning} ref={node=>{if(node)node.indeterminate=attributes.reasoning===undefined}} checked={attributes.reasoning===true} onChange={e=>{const enabled=e.currentTarget.checked;attr('reasoning',enabled);if(!enabled){editLevels('');attr('reasoning_effort',undefined)}}}/><span aria-hidden="true"/></label><small className="model-attribute-source">{source('reasoning')}</small></div>
      <div className="model-levels" role="group" aria-labelledby={formId+'-levels'}><span id={formId+'-levels'}>支持等级 <small className="model-attribute-source">{source('reasoning_levels')}</small></span>{levelOptions.map(level=><label key={level}><input type="checkbox" disabled={attributes.reasoning===false} checked={Boolean(attributes.reasoning_levels?.includes(level))} onChange={e=>{const next=e.currentTarget.checked?[...(attributes.reasoning_levels??[]),level]:(attributes.reasoning_levels??[]).filter(value=>value!==level);editLevels(next.join(', '))}}/><span>{(standardLevels as Record<string,string>)[level]??level}</span></label>)}</div>
      <div className="model-effort" role="group" aria-label="默认强度"><span>默认强度 <small className="model-attribute-source">{attributes.reasoning_effort?source('reasoning_effort'):'服务默认'}</small></span><div className="model-effort-buttons">
        {Object.entries(standardLevels).map(([level,label])=><button type="button" key={level} aria-pressed={attributes.reasoning_effort===level}
          title={attributes.reasoning_effort===level?'再次选择可恢复服务默认':undefined}
          disabled={attributes.reasoning===false||!attributes.reasoning_levels?.includes(level)}
          onClick={()=>attr('reasoning_effort',attributes.reasoning_effort===level?undefined:level)}>{label}</button>)}
      </div></div>
      <details className="model-custom-levels"><summary aria-label="自定义档位与模型操作" title="自定义档位与模型操作">⋯</summary>
        <label>推理强度档位<input value={levels} maxLength={528} disabled={attributes.reasoning===false} onChange={e=>editLevels(e.currentTarget.value)} placeholder="例如 low, medium, high"/></label>
        <label>自定义默认强度<select value={attributes.reasoning_effort??''} disabled={attributes.reasoning===false} onChange={e=>attr('reasoning_effort',e.currentTarget.value||undefined)}><option value="">服务默认</option>{attributes.reasoning_levels?.map(x=><option key={x} value={x}>{x}</option>)}</select></label>
        <button type="button" className="model-link" onClick={()=>attr('reasoning',undefined)}>将推理支持设为未知</button>
        {onDelete&&<button type="button" className="model-link danger" disabled={Boolean(busy)} onClick={onDelete}><Trash2 size={14}/>删除模型</button>}
      </details>
    </div>
    <div className="model-capabilities">{([['tool_calling','工具调用',Wrench],['structured_output','结构化输出',Braces],['vision','识图 (Vision)',Image],['file_input','文件输入',FileText]] as const).map(([key,label,Icon])=><div key={key} className="model-capability" role="group" aria-labelledby={formId+key+'-label'}><span className="model-capability-label" id={formId+key+'-label'}><Icon size={13} aria-hidden="true"/>{label} <small className="model-attribute-source">{source(key)}</small></span><div className="model-capability-options">{([['','未知'],['true','支持'],['false','不支持']] as const).map(([value,text])=><label key={value}><input type="radio" name={formId+key} value={value} checked={(attributes[key]===undefined?'':String(attributes[key]))===value} onChange={()=>attr(key,value===''?undefined:value==='true')}/><span>{text}</span></label>)}</div></div>)}</div>
    {!model&&<label className="model-check"><input type="checkbox" checked={draft.enabled} onChange={e=>setDraft({...draft,enabled:e.currentTarget.checked})}/>启用模型</label>}
    {notice&&<p className="model-feedback" role="status">{notice}</p>}{error&&<p className="error" role="alert">{error} {onReload&&<button type="button" className="model-link" onClick={()=>void onReload().then(onCancel).catch(e=>setError(errorMessage(e)))}>重新读取</button>}</p>}
    <div className="model-form-actions">{model&&<button type="button" className="model-button" disabled={Boolean(busy)||dirty||!model.enabled||!provider.enabled} onClick={test}><Zap size={14}/>{busy==='test'?'正在试答…':'试答测试'}</button>}{onDefault&&<button type="button" className="model-button" disabled={Boolean(busy)||dirty||isDefault||!model?.enabled||!provider.enabled} onClick={onDefault}><Star size={14}/>{isDefault?'全局默认':'设为全局默认'}</button>}{!model&&<button type="button" className="model-button" disabled={Boolean(busy)} onClick={onCancel}>取消</button>}<button type="submit" className="model-button primary" disabled={Boolean(busy)||Boolean(model&&!dirty)}><Check size={14}/>{busy==='save'?'正在保存…':'保存修改'}</button></div>
  </form>
}
