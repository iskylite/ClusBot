import {useEffect,useId,useRef,useState} from 'react'
import {api,APIError} from './api'
import {displayDate} from './GeneralPreferences'
import {assistantDetails,isAssistantItem,parseSurfaces,previewText,trustedForm,type AssistantItem,type DetailRow,type Surface} from './assistantSurface'

function SurfaceView({surface,expanded,detailsID,toggle,canToggle}:{surface:Surface;expanded:boolean;detailsID:string;toggle:()=>void;canToggle:boolean}) {
  function firstText(id:string):string|undefined {
    const node=surface.nodes.get(id)!
    if(node.component==='Text')return id
    if(node.component==='Button')return
    for(const child of 'children' in node?node.children:[node.child]) {
      const match=firstText(child)
      if(match)return match
    }
  }
  const headingID=firstText('root')
  function render(id:string):React.ReactNode {
    const node=surface.nodes.get(id)!
    if(node.component==='Text')return id===headingID?<h3 key={id}>{node.text}</h3>:<p key={id}>{node.text}</p>
    if(node.component==='Button') {
      const label=surface.nodes.get(node.child)
      return <button key={id} type="button" className="text-button assistant-detail-toggle" aria-expanded={expanded} aria-controls={detailsID} disabled={!canToggle} onClick={toggle}>
        {expanded?'收起详情':label?.component==='Text'?label.text:'展开详情'}
      </button>
    }
    if(node.component==='Card')return <div key={id} className="assistant-surface-card">{render(node.child)}</div>
    return <div key={id} className={node.component==='Row'?'assistant-surface-row':'assistant-surface-column'}>{node.children.map(render)}</div>
  }
  return <>{render('root')}</>
}

function LocalDetails({items,navigate}:{items:DetailRow[];navigate:(path:string)=>void}) {
  return <ul className="assistant-details">{items.map((item,index)=><li key={index}>
    <dl>{item.fields.map(field=><div key={field.label}><dt>{field.label}</dt><dd>{field.value}</dd></div>)}</dl>
    {item.taskID&&<button type="button" className="text-button" onClick={()=>navigate('/tasks/'+item.taskID)}>查看任务</button>}
  </li>)}</ul>
}

type RoutingRole = {role:string; source:string; status:string; model_id?:string; business?:string; profile_revision?:number; candidates?:{model_id?:string;status:string;profile_id:string}[]}
const roleNames:Record<string,string> = {
  general_router:'通用路由',conversation_reply:'对话回复',task_worker:'任务执行',
  benchmark_admission:'基准受理',benchmark_revision:'基准方案修订',benchmark_execution:'基准执行建议',deterministic:'确定性处理',
}
const routingState:Record<string,string> = {locally_configured:'已配置',configured:'已配置',not_configured:'未配置',model_not_used:'无需模型',demo:'演示模式',unavailable:'不可用',profile_missing:'档案不存在',credential_missing:'缺少凭据'}
const routingSource:Record<string,string> = {role_policy:'角色策略',business_override:'业务覆盖',global_default:'继承默认',explicit_profile:'显式绑定',go:'内置引擎'}

function RoutingCard({result,navigate,canManageModels}:{result:unknown;navigate:(path:string)=>void;canManageModels:boolean}) {
  const value=result as {roles?:RoutingRole[]}|null
  const roles=Array.isArray(value?.roles)?value.roles.filter(role=>role&&typeof role.role==='string'):[]
  const [selected,setSelected] = useState('')
  const role=roles.find(item=>item.role===selected)??roles[0]
  if (!role) return <p className="muted">暂无可显示的模型路由。</p>
  return <section className="assistant-routing" aria-label="模型配置与路由">
    <header><h3>模型配置与路由</h3>{canManageModels&&<button type="button" className="outline" onClick={()=>navigate('/settings?tab=model#model-roles')}>调整路由</button>}</header>
    <div className="assistant-routing-grid">
      <div className="assistant-routing-table">
        <table><thead><tr><th scope="col">调用角色</th><th scope="col">生效模型</th><th scope="col">绑定来源</th><th scope="col">状态</th></tr></thead>
        <tbody>{roles.map(item=><tr key={item.role} className={item===role?'selected':undefined}>
          <th scope="row"><button type="button" aria-pressed={item===role} onClick={()=>setSelected(item.role)}>{roleNames[item.role]??item.role}</button></th>
          <td>{item.model_id||(item.role==='deterministic'?'系统内置引擎':'—')}</td><td>{routingSource[item.source]??item.source}</td><td>{routingState[item.status]??item.status}</td>
        </tr>)}</tbody></table>
      </div>
      <aside className="assistant-routing-focus">
        <strong>{roleNames[role.role]??role.role}{role.model_id?' · '+role.model_id:''}</strong>
        <dl><dt>绑定来源</dt><dd>{routingSource[role.source]??role.source}</dd>
          {role.profile_revision&&<><dt>档案版本</dt><dd>{role.profile_revision}</dd></>}</dl>
        {Boolean(role.candidates?.length)&&<ol aria-label="有序模型">{role.candidates?.map(candidate=><li key={candidate.profile_id}>
          {candidate.model_id||'档案不可用'} <small>{routingState[candidate.status]??candidate.status}</small>
        </li>)}</ol>}
      </aside>
    </div>
  </section>
}

function operationsLink(tool:string, result:unknown) {
  if(tool!=='get_cluster_trends'&&tool!=='get_token_usage_stats'&&tool!=='get_incident_workflow'&&tool!=='get_incident_step_logs') return
  const {incident_id:incident,range}=result as {incident_id?:string;range?:string} ?? {}
  const params=new URLSearchParams()
  if (typeof incident==='string'&&/^(?:[a-f0-9]{32}|inc-[0-9]{1,16})$/.test(incident))params.set('incident',incident)
  if (range==='24h'||range==='7d'||range==='30d')params.set('range',range)
  return '/operations'+(params.size?'?'+params:'')
}

export function AssistantCard({item,navigate,canManageModels=false}:{item:AssistantItem;navigate:(path:string)=>void;canManageModels?:boolean}) {
  const [current,setCurrent]=useState(item)
  const [expanded,setExpanded]=useState(false)
  const [pending,setPending]=useState(false)
  const [outcome,setOutcome]=useState('')
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
    if(!preview||lock.current||expired)return
    if(Date.now()>=preview.expires_at_unix_ms){setExpired(true);return}
    if(confirm&&!previewText({input:preview.input,...(preview.target===undefined?{}:{target:preview.target})}).safe)return
    lock.current=true;setPending(true);setOutcome('')
    try {
      if(confirm) {
        const result=await api<unknown>('/assistant/confirm',{confirmation_id:preview.confirmation_id})
        if(!isAssistantItem(result) || Array.isArray(result) || result.state!=='completed')throw new Error('未收到有效操作回执，请核对目标状态')
        if(mounted.current){setCurrent(result);setExpanded(false)}
      } else {
        await api('/assistant/confirm/'+encodeURIComponent(preview.confirmation_id),undefined,'DELETE')
        if(mounted.current)setOutcome('已取消')
      }
    } catch(error) {
      if(mounted.current)setOutcome(error instanceof APIError?error.message:'未收到操作回执，请核对目标状态后重新预览')
    } finally {
      if(mounted.current){setPending(false);setExpired(true)}
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
  const hasToggle=surfaces?.some(surface=>[...surface.nodes.values()].some(node=>node.component==='Button'))
  const request=preview?previewText({input:preview.input,...(preview.target===undefined?{}:{target:preview.target})}):undefined
  const result=current.state==='completed'?previewText(current.result):undefined
  const form=current.state==='form_required'?trustedForm(current.tool,current.form):undefined
  const operations=current.state==='completed'?operationsLink(current.tool,current.result):undefined
  if(current.state==='completed'&&current.tool==='get_current_model_routing'){
    return <article className="assistant-card"><RoutingCard result={current.result} navigate={navigate} canManageModels={canManageModels}/></article>
  }
  return <article className="assistant-card">
    {surfaces?surfaces.map(surface=><SurfaceView key={surface.id} surface={surface} expanded={expanded} detailsID={detailsID} canToggle={details.length>0} toggle={()=>setExpanded(value=>!value)}/>):
      <h3>{current.state==='completed'?'工具已返回结果':current.state==='confirmation_required'?'请核对操作后决定是否确认':'此操作需要在设置中完成'}</h3>}
    {current.state==='completed'&&<>
      {operations&&<button type="button" className="outline" onClick={()=>navigate(operations)}>在运维工作台查看</button>}
      {details.length>0&&!hasToggle&&<button type="button" className="text-button assistant-detail-toggle" aria-expanded={expanded} aria-controls={detailsID} onClick={()=>setExpanded(value=>!value)}>{expanded?'收起详情':'展开详情'}</button>}
      {details.length>0&&<div id={detailsID} hidden={!expanded}><LocalDetails items={details} navigate={navigate}/></div>}
      {!details.length&&result&&<details className="assistant-result"><summary>查看返回数据</summary><pre>{result.text}</pre></details>}
    </>}
    {preview&&request&&<>
      <dl className="assistant-preview-fields">{request.fields.map((field,index)=><div key={index}><dt>{field.label}</dt><dd>{field.value}</dd></div>)}</dl>
      <details><summary>查看完整参数</summary><pre className="assistant-preview">{request.text}</pre></details>
      <p className="muted">有效至 {displayDate(preview.expires_at_unix_ms,{hour:'2-digit',minute:'2-digit',second:'2-digit'})}</p>
      {!request.safe&&<p className="error">内容含敏感字段或无法完整显示，请在对应页面处理。</p>}
      {outcome?<p role="status">{outcome}</p>:expired?<p role="status">确认已失效，请重新预览。</p>:
        <footer><button type="button" className="outline" disabled={pending} onClick={()=>void decide(false)}>取消</button>
          <button type="button" className="primary" disabled={pending||!request.safe} onClick={()=>void decide(true)}>{pending?'正在处理…':'确认操作'}</button></footer>}
    </>}
    {current.state==='form_required'&&(form?<button type="button" className="outline" onClick={()=>navigate(form)}>打开设置</button>:<p className="muted">当前版本暂无可用的设置入口。</p>)}
  </article>
}
