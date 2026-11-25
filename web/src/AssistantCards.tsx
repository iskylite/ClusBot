import {lazy,Suspense,useCallback,useEffect,useId,useRef,useState,type ReactNode} from 'react'
import {ArrowDownToLine,Check,GitBranch,Info,KeyRound,Layers,PanelRight,Pencil,Server,Settings2,ShieldAlert,Star,X,Zap} from 'lucide-react'
import {api} from './api'
import {AssistantRoleBinding,type BindingRole} from './AssistantRoleBinding'
import {DiscoveryPanel,type DiscoveryStatus} from './DiscoveryPanel'
import {AssistantProviderCredentials} from './AssistantProviderCredentials'
import {AssistantRecommendation} from './AssistantRecommendation'
import {OperationEvidenceCard} from './OperationEvidenceCard'
import type {CatalogModel} from './ModelSettings'
import {displayDate} from './GeneralPreferences'
import {assistantDetails,isAssistantItem,parseSurfaces,previewText,trustedForm,sendSurfaceAction,type AssistantCardItem,type DetailRow,type DiscoveryConversationSeed,type Surface,type SurfaceAction,type AssistantReply,type RoleBindingPrefill,type ProviderCredentialsPrefill} from './assistantSurface'
import {AssistantSurfaceView} from './AssistantSurfaceView'
import './assistant-cards.css'

const OperationsAssistantCard=lazy(()=>import('./OperationsPage').then(module=>({
  default:({tool,result,navigate,fallback}:{tool:string;result:unknown;navigate:(path:string)=>void;fallback:ReactNode})=>
    module.supportsOperationsAssistantCard(tool,result)?<module.OperationsAssistantCard tool={tool} result={result} navigate={navigate}/>:<>{fallback}</>,
})))
const operationsTools=new Set(['get_cluster_inventory','get_cluster_trends','get_token_usage_stats','get_incident_workflow','get_incident_step_logs'])

function LocalDetails({items,navigate}:{items:DetailRow[];navigate:(path:string)=>void}) {
  return <ul className="assistant-details">{items.map((item,index)=><li key={index}>
    <dl>{item.fields.map(field=><div key={field.label}><dt>{field.label}</dt><dd>{field.value}</dd></div>)}</dl>
    {item.taskID&&<button type="button" className="text-button" onClick={()=>navigate('/tasks/'+item.taskID)}>查看任务</button>}
  </li>)}</ul>
}

type RoutingRole = BindingRole & {status:string; profile_id?:string; default_profile_id?:string; profile_revision?:number; remote_observation?:string; candidates?:{model_id?:string;status:string;profile_id:string}[]}
const roleNames:Record<string,string> = {
  general_router:'通用路由',conversation_reply:'对话回复',task_worker:'任务执行',
  benchmark_admission:'基准受理',benchmark_revision:'基准方案修订',benchmark_execution:'基准执行建议',deterministic:'确定性处理',
}
const routingState:Record<string,string> = {
  locally_configured:'本地已配置',configured:'本地已配置',not_configured:'未配置',model_not_used:'无需模型',
  demo:'演示模式',local_demo:'本地演示',demo_role_unavailable:'演示模式不支持此角色',demo_profile_disabled:'演示模式未启用专用档案',
  unavailable:'不可用',settings_unavailable:'设置不可用',default_unavailable:'默认模型不可用',
  profile_missing:'档案不存在',profile_revision_changed:'档案版本已变更',profile_invalid:'档案不可用',profile_disabled:'档案已停用',
  provider_disabled:'提供方已停用',credential_missing:'缺少凭据',credential_changed:'凭据已变化',
  invalid_configuration:'配置不可用',default_destination_mismatch:'与默认模型连接不一致',
}
const routingSource:Record<string,string> = {role_policy:'角色策略',business_override:'业务覆盖',global_default:'继承默认',explicit_profile:'显式绑定',go:'内置引擎'}

type RoutingProvider = {id:string;name:string;base_url:string;enabled:boolean;auth_mode?:string;credential_status:string;model_count:number}
type RoutingTab='roles'|'providers'|'profiles'|'effective'
const routingTabs:{id:RoutingTab;label:string;icon:typeof GitBranch}[]=[
  {id:'roles',label:'角色路由',icon:GitBranch},
  {id:'providers',label:'提供方与凭据',icon:Server},
  {id:'profiles',label:'模型档案',icon:Layers},
  {id:'effective',label:'生效预览与回执',icon:Check},
]
const support=(value?:boolean)=>value===undefined?'未知':value?'支持':'不支持'
const reasoningLabels:Record<string,string>={low:'低',medium:'中',high:'高'}
const providerCredentials:Record<string,string>={configured:'凭据已配置',not_required:'无需凭据',missing:'缺少凭据'}

function RoutingCard({result,navigate,canManageModels,modelConcurrency}:{result:unknown;navigate:(path:string)=>void;canManageModels:boolean;modelConcurrency?:number}) {
  const [updated,setUpdated]=useState<{roles:RoutingRole[]}>()
  const [bindingRole,setBindingRole]=useState<RoutingRole>()
  const [editingCredentials,setEditingCredentials]=useState(false)
  const [bindingNotice,setBindingNotice]=useState('')
  const [testing,setTesting]=useState(false)
  const value=updated??result as {roles?:RoutingRole[]}|null
  const roles=Array.isArray(value?.roles)?value.roles.filter(role=>role&&typeof role.role==='string'):[]
  const [selected,setSelected] = useState('')
  const [showAll,setShowAll]=useState(false)
  const [tab,setTab]=useState<RoutingTab>('roles')
  const [catalog,setCatalog]=useState<{providers:RoutingProvider[];profiles:CatalogModel[]}>()
  const [error,setError]=useState('')
  useEffect(()=>{
    if(!canManageModels)return
    let active=true
    Promise.all([api<RoutingProvider[]>('/settings/model/providers'),api<CatalogModel[]>('/settings/model/catalog')])
      .then(([providers,profiles])=>{if(active)setCatalog({providers,profiles})})
      .catch(error=>{if(active)setError(error instanceof Error?error.message:'无法读取模型配置')})
    return()=>{active=false}
  },[canManageModels])
  const tableID=useId()
  const role=roles.find(item=>item.role===selected)??roles.find(item=>item.role==='task_worker')??roles[0]
  if (!role) return <p className="muted">暂无可显示的模型路由。</p>
  const activeProfile=catalog?.profiles.find(item=>item.id===(role.profile_id||role.default_profile_id))
  const activeProvider=catalog?.providers.find(item=>item.id===activeProfile?.provider_id)
  const defaultModel=roles.find(item=>item.source==='global_default'&&item.model_id)?.model_id
  const attributes=activeProfile?.attributes
  const reasoningLevels=attributes?.reasoning_levels?.map(level=>reasoningLabels[level]??level).join('/')
  const compactNumber=(value?:number)=>value===undefined?'未知':value>=1000&&value%1000===0?`${value/1000}K`:new Intl.NumberFormat('zh-CN').format(value)
  const credential=activeProvider?.auth_mode==='api_key'&&activeProvider.credential_status==='configured'?'API Key ••••••••':
    activeProvider?.credential_status==='configured'?'凭据已配置':activeProvider?.credential_status==='not_required'?'无需凭据':'凭据缺失'
  const providerAddress=activeProvider?.base_url.replace(/^https?:\/\//,'')
  async function tryReply() {
    if(!activeProfile||testing)return
    setTesting(true);setBindingNotice('')
    try {
      const result=await api<{duration_ms:number;revision:number}>('/settings/model/catalog/'+encodeURIComponent(activeProfile.id)+'/test',{revision:activeProfile.revision})
      if(result.revision!==activeProfile.revision)throw new Error('试答回执版本无法核对')
      setBindingNotice(`试答通过 · ${result.duration_ms} 毫秒`)
    } catch(error) {setBindingNotice(error instanceof Error?error.message:'试答未完成')}
    finally{setTesting(false)}
  }
  return <>{!bindingRole&&!editingCredentials&&<>
    <p className="assistant-routing-summary">已读取当前模型配置（{catalog&&<>{catalog.providers.length} 个提供方 · {catalog.profiles.length} 个档案 · </>}{roles.length} 个阶段）{defaultModel&&<>，全局默认 {defaultModel}</>}。</p>
    <article className="assistant-card assistant-card-routing"><section className={'assistant-routing'+(showAll?' assistant-routing-show-all':'')} aria-label="模型配置与路由">
    <header><h3><GitBranch size={15} aria-hidden="true"/>模型配置与路由</h3><div className="assistant-routing-header-actions">
      {defaultModel&&<span><Star size={13} aria-hidden="true"/>默认：{defaultModel}</span>}
      {canManageModels&&<button type="button" className="outline" onClick={()=>navigate('/settings?tab=model')}>切换默认</button>}
    </div></header>
    <div className="assistant-routing-tabs" aria-label="模型配置视图">{routingTabs.filter(item=>canManageModels||item.id==='roles'||item.id==='effective').map(({id,label,icon:Icon})=>
      <button type="button" key={id} aria-pressed={tab===id} onClick={()=>setTab(id)}><Icon size={12} aria-hidden="true"/>{label}
        {id==='roles'?' '+roles.length:id==='providers'&&catalog?' '+catalog.providers.length:id==='profiles'&&catalog?' '+catalog.profiles.length:''}
      </button>)}</div>
    <div className="assistant-routing-mobile-heading"><strong>{roleNames[role.role]??role.role}{role.model_id?' · '+role.model_id:''}</strong>
      <button type="button" className="outline" aria-expanded={showAll} aria-controls={tableID} onClick={()=>setShowAll(value=>!value)}>{showAll?'收起阶段':`全部 ${roles.length} 阶段`}</button>
    </div>
    <div className={'assistant-routing-grid'+(tab!=='roles'?' assistant-routing-inactive':'')}>
      <div className="assistant-routing-table" id={tableID}>
        <table><thead><tr><th scope="col">调用角色</th><th scope="col">生效模型</th><th scope="col">绑定来源</th><th scope="col">状态</th></tr></thead>
        <tbody>{roles.map(item=><tr key={item.role} className={item===role?'selected':undefined}>
          <th scope="row"><button type="button" aria-pressed={item===role} onClick={()=>setSelected(item.role)}>{roleNames[item.role]??item.role}</button></th>
          <td>{item.model_id||(item.role==='deterministic'?'系统内置引擎':'—')}</td><td><span className="assistant-routing-source">{routingSource[item.source]??item.source}</span></td><td><span className={['locally_configured','configured'].includes(item.status)?'assistant-routing-ready':undefined}>{routingState[item.status]??item.status}</span></td>
        </tr>)}</tbody></table>
        {Boolean(catalog?.providers.length)&&<ul className="assistant-routing-providers" aria-label="提供方配置状态">
          {catalog?.providers.map(provider=><li key={provider.id} data-missing={provider.enabled&&provider.credential_status==='missing'}>
            <Server size={11} aria-hidden="true"/><strong>{provider.name}</strong><span>{provider.enabled?providerCredentials[provider.credential_status]??'凭据状态未知':'已停用'}</span>
          </li>)}
        </ul>}
      </div>
      <aside className="assistant-routing-focus">
        <div className="assistant-routing-focus-title"><strong>{roleNames[role.role]??role.role}{role.model_id?' · '+role.model_id:''}</strong>
          {activeProfile&&<span data-enabled={activeProfile.enabled}>{activeProfile.enabled?'已启用':'已停用'}</span>}</div>
        {activeProvider&&<p className="assistant-provider-note">{providerAddress} · {credential}</p>}
        {attributes&&<dl className="assistant-routing-capabilities">
          {[
            ['上下文',compactNumber(attributes.context_tokens)],
            ['最大输出',compactNumber(attributes.max_output_tokens)],
            ['推理档',reasoningLevels||support(attributes.reasoning)],
            ['工具调用',support(attributes.tool_calling)],
            ['结构化',support(attributes.structured_output)],
            ['图片输入',support(attributes.vision)],
            ['文件输入',support(attributes.file_input)],
            ['来源',routingSource[role.source]??role.source],
          ].map(([label,detail])=><div key={label}><dt>{label}</dt><dd>{detail??'未知'}</dd></div>)}
        </dl>}
        {Boolean(role.candidates?.length)&&<ol aria-label="有序模型">{role.candidates?.map(candidate=><li key={candidate.profile_id}>
          {candidate.model_id||'档案不可用'} <small>{routingState[candidate.status]??candidate.status}</small>
        </li>)}</ol>}
        <div className="assistant-routing-mobile-summary">
          {attributes&&<p>{compactNumber(attributes.context_tokens)} 上下文 · {compactNumber(attributes.max_output_tokens)} 输出 · 工具调用{support(attributes.tool_calling)} · 结构化{support(attributes.structured_output)}{reasoningLevels?' · 推理 '+reasoningLevels : ''}</p>}
          {activeProvider&&<p>{providerAddress} · {credential}</p>}
          {!attributes&&!activeProvider&&<p>{routingState[role.status]??role.status} · {routingSource[role.source]??role.source}</p>}
        </div>
      </aside>
    </div>
    {tab!=='roles'&&<div className="assistant-routing-tab-content">
      {tab==='effective'?<dl>{roles.map(item=><div key={item.role}><dt>{roleNames[item.role]??item.role}</dt><dd>{routingState[item.status]??item.status} · {item.model_id??routingSource[item.source]??item.source}</dd></div>)}</dl>:
        error?<p className="error" role="alert">{error}</p>:!catalog?<p role="status">读取模型配置…</p>:
        tab==='providers'?catalog.providers.length?<dl>{catalog.providers.map(item=><div key={item.id}><dt>{item.name}</dt><dd>{item.base_url} · {item.enabled?'已启用':'已停用'} · {item.model_count} 个模型</dd></div>)}</dl>:<p>暂无提供方</p>:
        catalog.profiles.length?<dl>{catalog.profiles.map(item=><div key={item.id}><dt>{item.name}</dt><dd>{item.model_id} · {item.enabled?'已启用':'已停用'} · 修订 {item.revision}</dd></div>)}</dl>:<p>暂无模型档案</p>}
    </div>}
    {canManageModels&&<div className="assistant-routing-mobile-actions"><button type="button" className="outline" onClick={()=>setEditingCredentials(true)}><KeyRound size={13} aria-hidden="true"/>编辑凭据</button>
      <button type="button" className="primary" disabled={!catalog||role.role==='deterministic'} onClick={()=>{setBindingNotice('');setBindingRole(role)}}><Settings2 size={13} aria-hidden="true"/>调整角色绑定</button></div>}
    <footer className="assistant-routing-footer"><p className="assistant-routing-note" role={bindingNotice?'status':undefined}>{bindingNotice}</p>
      {canManageModels&&<div>{activeProfile&&<><button type="button" className="outline" disabled={testing||!activeProfile.enabled||!activeProfile.provider_enabled} onClick={()=>void tryReply()}><Zap size={13} aria-hidden="true"/>{testing?'正在试答…':'试答探测'}</button>
        <button type="button" className="outline" onClick={()=>navigate('/settings?'+new URLSearchParams({tab:'model',provider:activeProfile.provider_id,model:activeProfile.id}))}><Pencil size={13} aria-hidden="true"/>编辑档案</button></>}
        <button type="button" className="outline" onClick={()=>setEditingCredentials(true)}><KeyRound size={13} aria-hidden="true"/>编辑凭据</button>
        <button type="button" className="primary" disabled={!catalog||role.role==='deterministic'} onClick={()=>{setBindingNotice('');setBindingRole(role)}}>调整角色绑定</button></div>}
    </footer>
  </section></article></>}{editingCredentials&&<AssistantProviderCredentials navigate={navigate} providerID={activeProvider?.id} onClose={()=>{
    setEditingCredentials(false)
    void Promise.all([api<RoutingProvider[]>('/settings/model/providers'),api<CatalogModel[]>('/settings/model/catalog')])
      .then(([providers,profiles])=>{setCatalog({providers,profiles});setError('')})
      .catch(()=>setError('无法刷新模型配置。'))
  }}/>} {bindingRole&&catalog&&<AssistantRoleBinding key={bindingRole.role} role={bindingRole} label={roleNames[bindingRole.role]??bindingRole.role}
    models={catalog.profiles} providers={catalog.providers} close={()=>setBindingRole(undefined)} openPolicies={()=>navigate('/settings?tab=model#model-routing')}
    saved={async()=>{setBindingRole(undefined);try{setUpdated(await api<{roles:RoutingRole[]}>('/assistant/model-routing'));setBindingNotice('角色绑定已更新。')}catch{setBindingNotice('角色绑定已保存，暂时无法刷新模型路由。')}}}/>}</>
}

function RoleBindingPrefillCard({prefill,navigate}:{prefill:RoleBindingPrefill;navigate:(path:string)=>void}){
  const [data,setData]=useState<{roles:RoutingRole[];profiles:CatalogModel[];providers:RoutingProvider[]}>()
  const [roleID,setRoleID]=useState(prefill.role??'')
  const [error,setError]=useState('')
  const [outcome,setOutcome]=useState('')
  useEffect(()=>{
    let active=true
    Promise.all([api<{roles:RoutingRole[]}>('/assistant/model-routing'),api<CatalogModel[]>('/settings/model/catalog'),api<RoutingProvider[]>('/settings/model/providers')])
      .then(([routing,profiles,providers])=>{if(active)setData({roles:routing.roles,profiles,providers})})
      .catch(()=>{if(active)setError('无法读取当前模型配置，请重新打开。')})
    return()=>{active=false}
  },[])
  if(outcome)return <p role="status">{outcome}</p>
  if(error)return <p className="error" role="alert">{error}</p>
  if(!data)return <p role="status">正在读取当前模型配置…</p>
  const role=data.roles.find(item=>item.role===roleID&&item.role!=='deterministic')
  const profile=data.profiles.find(item=>item.id===prefill.profile_id)
  const initialProfile=profile&&profile.revision===prefill.profile_revision&&profile.provider_revision===prefill.provider_revision&&
    profile.enabled&&profile.provider_enabled&&(profile.credential_configured||data.providers.some(item=>item.id===profile.provider_id&&item.credential_status==='not_required'))?profile:undefined
  if(!role)return <section className="assistant-card assistant-card-confirmation" aria-label="选择模型角色">
    <h3>选择要调整的角色</h3>
    <label>角色 <select value="" onChange={event=>setRoleID(event.target.value)}>
      <option value="">请选择</option>
      {data.roles.filter(item=>item.role!=='deterministic'&&Object.hasOwn(roleNames,item.role)).map(item=><option key={item.role} value={item.role}>{roleNames[item.role]}</option>)}
    </select></label>
    <footer><button type="button" className="outline" onClick={()=>setOutcome('已取消')}>取消</button></footer>
  </section>
  return <>
    {prefill.profile_id&&!initialProfile&&<p role="status">模型或提供方配置已变化或暂不可用，请重新选择。</p>}
    {!prefill.profile_id&&<p role="status">未能唯一确定模型，请选择。</p>}
    <AssistantRoleBinding key={role.role} role={role} label={roleNames[role.role]??role.role} models={data.profiles} providers={data.providers}
      initialProfileID={initialProfile?.id} close={()=>setOutcome('已取消')} openPolicies={()=>navigate('/settings?tab=model#model-routing')}
      saved={async()=>{setOutcome('角色绑定已更新。')}}/>
  </>
}

export function DiscoveryResult({initial,initialSurfaces,navigate,principalID,conversationID,conversationReadOnly,openConversation}:{
  initial?:DiscoveryStatus;initialSurfaces?:Surface[];navigate:(path:string)=>void;principalID?:string;conversationID?:string;conversationReadOnly?:boolean;openConversation?:(seed:DiscoveryConversationSeed)=>void
}){
  const [status,setStatus]=useState(initial)
  const [error,setError]=useState('')
  const refresh=useCallback(()=>{
    api<DiscoveryStatus>('/domain/discovery/setup-status').then(value=>{setStatus(value);setError('')})
      .catch(()=>setError('发现计划暂时无法刷新。'))
  },[])
  useEffect(()=>{if(!initial)refresh()},[initial,refresh])
  return <>{error&&<p role="alert" className="error">{error}</p>}<DiscoveryPanel status={status??{state:'source_unavailable'}} refresh={refresh}
    principalID={principalID} conversationID={conversationID} conversationReadOnly={conversationReadOnly} initialSurfaces={initialSurfaces} openConversation={openConversation}
    renderConfirmation={(item,onSettled,confirm)=><AssistantCard item={item} navigate={navigate} conversationReadOnly={conversationReadOnly}
      onConfirmationSettled={onSettled} confirmDiscovery={confirm}/>}/></>
}

function operationsLink(tool:string, result:unknown) {
  if(tool!=='get_cluster_trends'&&tool!=='get_token_usage_stats'&&tool!=='get_incident_workflow'&&tool!=='get_incident_step_logs') return
  const {incident_id:incident,range}=result as {incident_id?:string;range?:string} ?? {}
  const params=new URLSearchParams()
  if (typeof incident==='string'&&/^(?:[a-f0-9]{32}|inc-[0-9]{1,16})$/.test(incident))params.set('incident',incident)
  if (range==='24h'||range==='7d'||range==='30d')params.set('range',range)
  return '/operations'+(params.size?'?'+params:'')
}

export function AssistantCard({item,navigate,canManageModels=false,modelConcurrency,openTask,principalID,conversationID,conversationReadOnly,openDiscoveryConversation,onConfirmationSettled,confirmDiscovery}:{item:AssistantCardItem;navigate:(path:string)=>void;canManageModels?:boolean;modelConcurrency?:number;openTask?:(taskID:string,trigger:HTMLButtonElement)=>void;principalID?:string;conversationID?:string;conversationReadOnly?:boolean;openDiscoveryConversation?:(seed:DiscoveryConversationSeed)=>void;onConfirmationSettled?:(item?:AssistantReply)=>void;confirmDiscovery?:(previewID:string)=>Promise<AssistantReply>}) {
  const [current,setCurrent]=useState(item)
  const [expanded,setExpanded]=useState(false)
  const [pending,setPending]=useState(false)
  const [outcome,setOutcome]=useState('')
  const [agentError,setAgentError]=useState('')
  const [agentPending,setAgentPending]=useState(false)
  const agentLock=useRef(false)
  const [expired,setExpired]=useState(false)
  const lock=useRef(false)
  const mounted=useRef(true)
  const detailsID=useId()
  useEffect(()=>{mounted.current=true;return()=>{mounted.current=false}},[])
  const preview=!Array.isArray(current)&&current.state==='confirmation_required'?current:undefined
  useEffect(()=>{
    if(!preview)return
    const remaining=preview.expires_at_unix_ms-Date.now()
    if(remaining<=0){setExpired(true);return}
    const timer=window.setTimeout(()=>setExpired(true),Math.min(remaining,2147483647))
    return()=>window.clearTimeout(timer)
  },[preview])
  async function decide(confirm:boolean) {
    if(!preview||conversationReadOnly||lock.current||expired)return
    if(Date.now()>=preview.expires_at_unix_ms){setExpired(true);return}
    const scopedAdmission=preview.input.tool==='admit_discovery_candidate'
    if(confirm&&scopedAdmission&&!confirmDiscovery)return
    if(confirm&&!previewText({input:preview.input,...(preview.target===undefined?{}:{target:preview.target})}).safe)return
    lock.current=true;setPending(true);setOutcome('')
    try {
      if(confirm) {
        const result=scopedAdmission?await confirmDiscovery!(preview.confirmation_id):await api<unknown>('/assistant/confirm',{confirmation_id:preview.confirmation_id})
        if(!isAssistantItem(result) || Array.isArray(result) || result.state!=='completed'||result.tool!==preview.input.tool)
          throw new Error('未收到与本次操作匹配的回执，请核对目标状态')
        if(mounted.current){setCurrent(result);setExpanded(false);onConfirmationSettled?.(result)}
      } else {
        await api('/assistant/confirm/'+encodeURIComponent(preview.confirmation_id),undefined,'DELETE')
        if(mounted.current){setOutcome('已取消');onConfirmationSettled?.()}
      }
    } catch(error) {
      if(mounted.current)setOutcome(error instanceof Error?error.message:'未收到操作回执，请先核对原请求。')
    } finally {
      if(mounted.current){setPending(false);setExpired(true)}
    }
  }
  async function dispatchSurface(action:SurfaceAction) {
    if(!conversationID||conversationReadOnly||agentLock.current)return
    agentLock.current=true;setAgentPending(true);setAgentError('')
    try {
      const receipt=await sendSurfaceAction(api,conversationID,action,crypto.randomUUID())
      if(mounted.current){setCurrent(receipt);setExpanded(false)}
    } catch(error) {
      if(mounted.current)setAgentError(error instanceof Error?error.message:'暂未收到操作回执，请重新核对。')
    } finally {
      agentLock.current=false
      if(mounted.current)setAgentPending(false)
    }
  }
  if(Array.isArray(current))return <article className="assistant-card">
    <h3>助手工具</h3><p>输入 /助手 后接问题，或使用工具名称和参数。</p>
    <details><summary>查看工具目录（{current.length}）</summary><ul className="assistant-tool-list">{current.map(tool=><li key={tool.name}>
      <strong>{tool.label||tool.name}</strong><code>{tool.name}</code><p>{tool.description}</p>{tool.params?.length?<small>需要：{tool.params.join('、')}</small>:null}
    </li>)}</ul></details>
  </article>
  const surfaces='surface' in current?parseSurfaces(current.surface):undefined
  const details=current.state==='completed'?assistantDetails(current.tool,current.result):[]
  const hasToggle=surfaces?.some(surface=>[...surface.nodes.values()].some(node=>node.component==='Button'&&node.action.event.name==='toggle_details'))
  const operation=preview?.input.tool
  const ownPause=operation==='pause_task'
  const discoveryAdmission=operation==='admit_discovery_candidate'
  const confirmationLabel=operation==='admin_pause_task'||ownPause?'确认暂停':
    operation==='accept_task'||operation==='admin_accept_task'?'确认验收':
    operation==='set_default_model'?'确认切换':discoveryAdmission?'确认纳管':'确认操作'
  const taskNotice=operation==='admin_pause_task'||ownPause?'暂停请求会阻止后续派发，已派发的作业仍须核对。':
    operation==='admin_accept_task'||operation==='accept_task'?'验收将记录本地结果。':
    discoveryAdmission?'确认后才将候选写入资产台账。':''
  const request=preview?previewText({input:preview.input,...(preview.target===undefined?{}:{target:preview.target})}):undefined
  const params=preview?.input.params as {owner?:unknown;id?:unknown}|undefined
  const inputBody=preview?.input.body as {artifact_id?:unknown}|undefined
  const target=preview?.target&&typeof preview.target==='object'?preview.target as {acceptance_criteria?:unknown;status?:unknown}:undefined
  const taskTitle=typeof preview?.target==='object'&&preview.target!==null&&'title' in preview.target&&typeof preview.target.title==='string'?preview.target.title:undefined
  const validSegment=(value:unknown):value is string=>typeof value==='string'&&/^[a-z0-9_-]{1,80}$/i.test(value)
  const acceptance=operation==='admin_accept_task'||operation==='accept_task'
  const crossPause=operation==='admin_pause_task'
  const ownTaskID=ownPause&&request?.safe&&typeof params?.id==='string'&&/^[a-f0-9]{32}$/.test(params.id)?params.id:undefined
  const reportLink=operation==='admin_accept_task'&&request?.safe&&validSegment(params?.owner)&&validSegment(params?.id)&&validSegment(inputBody?.artifact_id)?
    '/api/v1/admin/tasks/'+encodeURIComponent(params.owner)+'/'+encodeURIComponent(params.id)+'/artifacts/'+encodeURIComponent(inputBody.artifact_id)+'/content':undefined
  const result=current.state==='completed'?previewText(current.result):undefined
  const form=current.state==='form_required'?trustedForm(current.tool,current.form):undefined
  if(current.state==='form_required'&&current.tool==='role_binding_prefill'&&isAssistantItem(current)){
    return canManageModels?<RoleBindingPrefillCard prefill={current.prefill as RoleBindingPrefill} navigate={navigate}/>:<p role="status">当前账号无权管理模型设置。</p>
  }
  if(current.state==='form_required'&&current.tool==='provider_credentials_prefill'&&isAssistantItem(current)){
    const prefill=current.prefill as ProviderCredentialsPrefill
    return canManageModels?<AssistantProviderCredentials navigate={navigate} providerID={prefill.provider_id} providerRevision={prefill.provider_revision} requireSelection/>:<p role="status">当前账号无权管理模型设置。</p>
  }
  if(current.state==='form_required'&&current.tool==='configure_model_provider'&&form&&canManageModels){
    return <AssistantProviderCredentials navigate={navigate}/>
  }
  const operations=current.state==='completed'?operationsLink(current.tool,current.result):undefined
  if(current.state==='completed'&&current.tool==='get_discovery_setup_status'&&current.result&&typeof current.result==='object'&&
    'state' in current.result&&typeof current.result.state==='string'){
    return <DiscoveryResult initial={current.result as DiscoveryStatus} navigate={navigate} principalID={principalID} conversationID={conversationID} conversationReadOnly={conversationReadOnly}
      openConversation={openDiscoveryConversation}/>
  }
  if(current.state==='completed'&&current.tool==='propose_operation_options'){
    return <AssistantRecommendation result={current.result}/>
  }
  if(current.state==='completed'&&current.tool==='get_operation_evidence'){
    return <OperationEvidenceCard result={current.result}/>
  }
  if(current.state==='completed'&&current.tool==='get_current_model_routing'){
    return <RoutingCard result={current.result} navigate={navigate} canManageModels={canManageModels} modelConcurrency={modelConcurrency}/>
  }
  if(current.state==='completed'&&current.tool==='admit_discovery_candidate'&&current.outcome==='admitted'){
    const receipt=current.result as {asset:string;check_task_id?:string}
    return <article className="assistant-card"><h3>资产已纳管</h3><p role="status">{receipt.asset}</p>
      {receipt.check_task_id&&<p>已登记核查任务，尚不代表检查通过。</p>}</article>
  }
  const card=<article className={'assistant-card'+(preview?' assistant-card-confirmation':'')}>
    {preview&&(crossPause||acceptance)?<div className="assistant-card-title">
      <h3>{crossPause?'跨账号任务暂停确认':(taskTitle?taskTitle+' · ':'')+'独立验收'}</h3>
      {request&&<details className="assistant-target-details"><summary className="assistant-expiry" title={'有效至 '+displayDate(preview.expires_at_unix_ms,{hour:'2-digit',minute:'2-digit',second:'2-digit'})}>{validSegment(params?.owner)?`owner: ${params.owner} · `:''}{validSegment(params?.id)?params.id.slice(0,8):'核验目标'}</summary><pre className="assistant-preview">{request.text}</pre></details>}
    </div>:preview&&ownPause?<div className="assistant-card-title">
      <h3><ShieldAlert size={15} aria-hidden="true"/>{taskTitle?taskTitle+' · ':''}暂停后续派发</h3>
      <span className="assistant-expiry" title={'有效至 '+displayDate(preview.expires_at_unix_ms,{hour:'2-digit',minute:'2-digit',second:'2-digit'})}>{expired?'已失效':'待确认执行'}</span>
    </div>:surfaces?surfaces.map((surface,index)=><AssistantSurfaceView key={surface.id} surface={surface} expanded={expanded} detailsID={detailsID} canToggle={details.length>0} toggle={()=>setExpanded(value=>!value)} confirmation={Boolean(preview)&&index===0} expires={preview?.expires_at_unix_ms} busy={agentPending} onAgentAction={conversationID&&!conversationReadOnly&&!preview?dispatchSurface:undefined}/>):
      <div className="assistant-card-title"><h3>{preview&&<ShieldAlert size={15} aria-hidden="true"/>}{current.state==='completed'?'工具已返回结果':preview?'请核对操作后决定是否确认':'此操作需要在设置中完成'}</h3>
        {preview&&<span className="assistant-expiry">有效至 {displayDate(preview.expires_at_unix_ms,{hour:'2-digit',minute:'2-digit',second:'2-digit'})}</span>}</div>}
    {agentError&&<p className="error" role="alert">{agentError}</p>}
    {current.state==='completed'&&<>
      {operations&&<button type="button" className="outline" onClick={()=>navigate(operations)}>在运维工作台查看</button>}
      {details.length>0&&!hasToggle&&<button type="button" className="text-button assistant-detail-toggle" aria-expanded={expanded} aria-controls={detailsID} onClick={()=>setExpanded(value=>!value)}>{expanded?'收起详情':'展开详情'}</button>}
      {details.length>0&&<div id={detailsID} hidden={!expanded}><LocalDetails items={details} navigate={navigate}/></div>}
      {!details.length&&result&&<details className="assistant-result"><summary>查看返回数据</summary><pre>{result.text}</pre></details>}
    </>}
    {preview&&request&&<>
      {crossPause&&taskTitle&&<p className="assistant-confirmation-line"><Info size={12} aria-hidden="true"/>{taskTitle}</p>}
      {!crossPause&&!acceptance&&<dl className="assistant-preview-fields">{request.fields.map((field,index)=><div key={index}><dt>{field.label}</dt><dd>{field.value}</dd></div>)}</dl>}
      {!crossPause&&!acceptance&&<details><summary>查看完整参数</summary><pre className="assistant-preview">{request.text}</pre></details>}
      {crossPause&&<><p className="assistant-confirmation-line"><Info size={12} aria-hidden="true"/>仅阻止新派发，不代表远端作业已停止。</p>
        <p className="assistant-confirmation-line"><Info size={12} aria-hidden="true"/>私人对话正文保持不可见，后续执行仍归所属账号。</p></>}
      {acceptance&&<>{typeof target?.acceptance_criteria==='string'&&target.acceptance_criteria&&<p className="assistant-confirmation-line"><Info size={12} aria-hidden="true"/>验收条件：{target.acceptance_criteria}</p>}
        <p className="assistant-confirmation-line"><Info size={12} aria-hidden="true"/>仅记录本地验收，不触发外部邮件外发。</p></>}
      {taskNotice&&!crossPause&&!acceptance&&<p className="muted">{taskNotice}</p>}
      {!request.safe&&<p className="error">内容含敏感字段或无法完整显示，请在对应页面处理。</p>}
      {outcome?<p role="status">{outcome}</p>:expired?<p role="status">确认已失效，请重新预览。</p>:
        <footer>{reportLink&&<a className="outline" href={reportLink} download><ArrowDownToLine size={13}/>下载报告</a>}
          {ownTaskID&&openTask&&<button type="button" className="outline assistant-open-task" onClick={event=>openTask(ownTaskID,event.currentTarget)}><PanelRight size={13} aria-hidden="true"/>打开详情抽屉</button>}
          <button type="button" className="outline" disabled={pending||conversationReadOnly} onClick={()=>void decide(false)}><X size={13} aria-hidden="true"/>取消</button>
          <button type="button" className="primary" disabled={pending||!request.safe||conversationReadOnly||discoveryAdmission&&!confirmDiscovery} onClick={()=>void decide(true)}><Check size={13} aria-hidden="true"/>{pending?'正在处理…':confirmationLabel}</button></footer>}
    </>}
    {current.state==='form_required'&&(form?<button type="button" className="outline" onClick={()=>navigate(form)}>打开设置</button>:<p className="muted">当前版本暂无可用的设置入口。</p>)}
  </article>
  if(current.state==='completed'&&operationsTools.has(current.tool)){
    return <Suspense fallback={card}><OperationsAssistantCard tool={current.tool} result={current.result} navigate={navigate} fallback={card}/></Suspense>
  }
  return card
}
