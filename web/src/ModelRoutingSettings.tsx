import {useCallback, useEffect, useRef, useState, type FormEvent} from 'react'
import {ArrowDown, ArrowUp, X} from 'lucide-react'
import {api, APIError} from './api'
import {FeedbackCallout} from './Feedback'
import type {CatalogModel} from './ModelSettings'

type Provider={id:string;name:string;credential_status:string;enabled:boolean}
type Candidate={profile_id:string;profile_revision:number;provider_id?:string;model_id?:string;status?:string}
type Policy={business:string;role:string;revision:number;allow_cross_provider:boolean;candidates:Candidate[]}
type Preview={source:string;status:string;model_id?:string;candidates?:Candidate[];fallback_conditions?:string[];fallback_limitations?:string[]}
type Stage={role:string;business:string;title:string;egress:string}
const stages:Stage[]=[
  {role:'general_router',business:'conversation',title:'对话路由',egress:'提交文字、近期对话、候选任务、待处理意图和基准候选'},
  {role:'conversation_reply',business:'conversation',title:'普通对话',egress:'提交文字、近期对话、所选文字和图片（模型支持时）、匹配的用户记忆'},
  {role:'task_worker',business:'task',title:'任务工作',egress:'任务消息与要求、所选文字和图片（模型支持时）、用户记忆、工作流输入、适用技能与经验、已授权的只读工具定义及结果、当前模型工具往返记录'},
  {role:'benchmark_admission',business:'benchmark',title:'基准受理',egress:'提交文字、已发布目标、已发布方案及版本'},
  {role:'benchmark_revision',business:'benchmark',title:'基准方案修订',egress:'提交文字、可编辑测项和参数名'},
  {role:'benchmark_execution',business:'benchmark',title:'基准执行建议',egress:'提交文字、待派发测项 ID 及顺序'},
]
const stateNames:Record<string,string>={locally_configured:'本地配置可用',configured:'本地配置可用',profile_missing:'模型已不存在',profile_revision_changed:'模型版本已变化',profile_disabled:'模型已停用',provider_disabled:'提供方已停用',profile_invalid:'模型配置无效',credential_missing:'凭据不可用',credential_changed:'凭据已变化',demo_profile_disabled:'演示模式不可用',not_configured:'未配置',settings_unavailable:'模型设置不可用',default_unavailable:'默认模型不可用',policy_unavailable:'阶段策略不可用',invalid_configuration:'模型配置无效',default_destination_mismatch:'模型与默认服务不一致',demo_role_unavailable:'演示模式不可用',unconfigured:'未配置',ready:'可用',selected:'已选择'}
const sourceNames:Record<string,string>={business_override:'业务覆盖',role_policy:'阶段策略',global_default:'全局默认',explicit_profile:'旧角色绑定',uses_existing_default:'默认模型'}
const err=(e:unknown)=>e instanceof APIError&&e.status===409?'配置版本已变化，未保存。请重新读取并核对后重试。':e instanceof Error?e.message:'请求未完成，请重试'

function sameCandidates(a:Candidate[],b:Candidate[]){
  return a.length===b.length && a.every((item,index)=>item.profile_id===b[index].profile_id && item.profile_revision===b[index].profile_revision)
}

export function ModelRoutingSettings({models,providers,generation,onPolicySaved}:{models:CatalogModel[];providers:Provider[];generation:number;onPolicySaved?:()=>Promise<void>}) {
  const [policies,setPolicies]=useState<Policy[]>()
  const [stage,setStage]=useState(()=>{
    const requested=new URLSearchParams(location.search).get('stage')
    return stages.some(item=>item.role===requested)?requested!:'conversation_reply'
  })
  const [scope,setScope]=useState<'default'|'business'>('business')
  const [preview,setPreview]=useState<Preview>()
  const [error,setError]=useState(''),[notice,setNotice]=useState('')
  const [reset,setReset]=useState(0)
  const previewRequest=useRef(0)
  const spec=stages.find(x=>x.role===stage)!
  const business=scope==='default'?'default':spec.business
  const load=useCallback(async()=>{const value=await api<{policies:Policy[]}>('/settings/model/routing/policies');setPolicies(value.policies??[])},[])
  const loadPreview=useCallback(async()=>{const request=++previewRequest.current;const value=await api<Preview>('/settings/model/routing/preview',{role:spec.role,business:spec.business});if(previewRequest.current===request)setPreview(value)},[spec])
  useEffect(()=>{let alive=true;void Promise.all([load(),loadPreview()]).catch(e=>{if(alive)setError(err(e))});return()=>{alive=false;previewRequest.current++}},[load,loadPreview,generation])
  const policy=policies?.find(p=>p.role===stage&&p.business===business)
  async function reload(){try{await Promise.all([load(),loadPreview()]);setReset(n=>n+1);setError('')}catch(e){setError(err(e))}}
  return <section id="model-routing" className="model-routing-layout" aria-labelledby="model-routing-title">
    <aside className="model-routing-sidebar" aria-label="使用场景">
      <div className="model-panel-heading"><h2 id="model-routing-title">使用场景 ({stages.length})</h2></div>
      <div className="model-scenario-list">
        {stages.map(s=>{
          const activePolicy=policies?.find(p=>p.role===s.role&&p.business===s.business)??policies?.find(p=>p.role===s.role&&p.business==='default')
          const firstModel=activePolicy?.candidates[0]?models.find(m=>m.id===activePolicy.candidates[0].profile_id)?.name??activePolicy.candidates[0].model_id:undefined
          return <button key={s.role} type="button" className={'model-scenario-item'+(stage===s.role?' selected':'')} onClick={()=>{setStage(s.role);setPreview(undefined);setNotice('');setReset(n=>n+1)}}>
            <strong>{s.title}</strong>
            <small>{firstModel?`主模型：${firstModel}`:'沿用全局默认'}</small>
          </button>
        })}
      </div>
    </aside>
    <div className="model-routing-panel">
      <div className="model-panel-heading"><h2>{spec.title} · 模型优先级</h2><button type="button" className="model-link" onClick={()=>void reload()}>重新读取</button></div>
      <p className="muted">未指定模型时自动沿用「全局默认模型」；按顺序配置多个模型时，异常将自动顺延至备用模型。</p>
      {preview&&<div className="model-routing-preview"><strong>当前生效</strong><span>{sourceNames[preview.source]??preview.source} · {preview.model_id??'未选择模型'} · {stateNames[preview.status]??preview.status}</span></div>}
      {policies&&<PolicyEditor key={stage+':'+business+':'+reset} spec={spec} business={business} scope={scope} onScopeChange={next=>{setScope(next);setNotice('');setReset(n=>n+1)}} initial={policy} models={models} providers={providers} onSaved={async()=>{await Promise.all([load(),loadPreview()]);if(onPolicySaved)await onPolicySaved();setReset(n=>n+1);setNotice('策略已保存。')}}/>}
      {notice&&<FeedbackCallout tone="success" compact role="status" message={notice} onDismiss={()=>setNotice('')}/>}
      {error&&<FeedbackCallout tone="error" compact role="alert" message={error} onDismiss={()=>setError('')}/>}
    </div>
  </section>
}

function PolicyEditor({spec,business,scope,onScopeChange,initial,models,providers,onSaved}:{spec:Stage;business:string;scope:'default'|'business';onScopeChange:(next:'default'|'business')=>void;initial?:Policy;models:CatalogModel[];providers:Provider[];onSaved:()=>Promise<void>}) {
  const [candidates,setCandidates]=useState<Candidate[]>(()=>initial?.candidates??[])
  const [allowCross,setAllowCross]=useState(initial?.allow_cross_provider??false)
  const [consent,setConsent]=useState(false),[busy,setBusy]=useState(false),[error,setError]=useState('')
  const available=models.filter(m=>m.enabled&&m.provider_enabled&&!candidates.some(c=>c.profile_id===m.id)&&providers.some(p=>p.id===m.provider_id&&p.enabled&&p.credential_status!=='missing'))
  const stale=candidates.some(c=>{const m=models.find(item=>item.id===c.profile_id);return !m||m.revision!==c.profile_revision||!m.enabled||!m.provider_enabled})
  const initialCandidates=initial?.candidates??[]
  const initialAllowCross=initial?.allow_cross_provider??false
  const isDirty=!sameCandidates(candidates,initialCandidates) || (candidates.length>0 && allowCross!==initialAllowCross)
  const consentChecked=isDirty ? consent : (initialCandidates.length>0 && !stale)
  function change(next:Candidate[]){setCandidates(next);setConsent(false);setError('')}
  function addById(id:string){const item=models.find(m=>m.id===id);if(!item||candidates.length>=4)return;change([...candidates,{profile_id:item.id,profile_revision:item.revision}])}
  function move(index:number,offset:number){const next=[...candidates],target=index+offset;if(target<0||target>=next.length)return;[next[index],next[target]]=[next[target],next[index]];change(next)}
  async function save(e:FormEvent<HTMLFormElement>){e.preventDefault();if(busy||!isDirty||stale||(candidates.length>0&&!consent))return;setBusy(true);setError('')
    try{await api('/settings/model/routing/policies/'+business+'/'+spec.role,{revision:initial?.revision??0,candidates:candidates.map(c=>({profile_id:c.profile_id,profile_revision:c.profile_revision})),allow_cross_provider:allowCross,confirm_egress:consent},'PUT');await onSaved()}catch(e){setError(err(e))}finally{setBusy(false)}
  }
  return <form className="model-policy" aria-label={`${spec.title}模型策略`} onSubmit={save}>
    <div className="model-panel-heading"><h3>优先级顺序</h3><small>最多 4 个</small></div>
    {candidates.length===0?<p className="muted">未指定模型，当前沿用全局默认。</p>:<ol className="model-candidate-list">{candidates.map((candidate,index)=>{
      const model=models.find(item=>item.id===candidate.profile_id),provider=providers.find(item=>item.id===model?.provider_id)
      const changed=!model||model.revision!==candidate.profile_revision
      const tierLabel=index===0?'主模型':`备用 ${index}`
      return <li key={candidate.profile_id}><span><strong><em className={'model-tier-tag'+(index===0?' primary':'')}>{tierLabel}</em>{provider?.name?`${provider.name} / `:''}{model?.name??candidate.model_id??'模型已不存在'}</strong><small>{changed?'版本已变化，请移除后重新添加':candidate.status&&candidate.status!=='locally_configured'?(stateNames[candidate.status]??candidate.status):'就绪'}</small></span><div><button type="button" className="model-icon-button" aria-label={`上移 ${model?.name??'候选模型'}`} disabled={busy||index===0} onClick={()=>move(index,-1)}><ArrowUp size={15}/></button><button type="button" className="model-icon-button" aria-label={`下移 ${model?.name??'候选模型'}`} disabled={busy||index===candidates.length-1} onClick={()=>move(index,1)}><ArrowDown size={15}/></button><button type="button" className="model-icon-button" aria-label={`移除 ${model?.name??'候选模型'}`} disabled={busy} onClick={()=>change(candidates.filter((_,i)=>i!==index))}><X size={15}/></button></div></li>
    })}</ol>}
    {candidates.length<4&&(available.length>0 ? (
      <div className="model-add-candidate"><label className="model-sr-only" htmlFor="model-policy-choice">添加模型到优先级列表</label>
        <select id="model-policy-choice" value="" disabled={busy} onChange={e=>{const id=e.currentTarget.value;if(id)addById(id)}}>
          <option value="">添加模型到优先级列表…</option>
          {available.map(m=><option key={m.id} value={m.id}>{providers.find(p=>p.id===m.provider_id)?.name} / {m.name}</option>)}
        </select>
      </div>
    ) : candidates.length===0 ? (
      <FeedbackCallout tone="warning" compact role="status" message="当前没有已启用且凭据就绪的模型，请先在「提供方与模型」中启用提供方和模型。"/>
    ) : null)}
    <label className="model-check"><input type="checkbox" checked={allowCross} disabled={busy} onChange={e=>{setAllowCross(e.currentTarget.checked);setConsent(false)}}/>允许跨提供方自动顺延</label>
    <details className="model-scope-advanced">
      <summary>高级：策略生效范围（{scope==='business'?'当前业务专属':'全部业务通用'}）</summary>
      <label>策略范围<select value={scope} onChange={e=>onScopeChange(e.currentTarget.value as 'default'|'business')}><option value="business">{spec.business==='conversation'?'对话':spec.business==='task'?'任务':'基准'}业务覆盖（推荐）</option><option value="default">阶段默认（全部业务通用）</option></select></label>
    </details>
    {candidates.length>0&&<label className="model-check model-consent"><input type="checkbox" checked={consentChecked} disabled={busy||!isDirty} onChange={e=>setConsent(e.currentTarget.checked)}/><span>我确认以上所有模型均可处理此阶段的数据：{spec.egress}。</span></label>}
    {stale&&<FeedbackCallout tone="error" compact role="alert" message="候选模型已变化或不可用，请移除后重新添加。"/>}
    {error&&<FeedbackCallout tone="error" compact role="alert" message={error} onDismiss={()=>setError('')}/>}
    <div className="model-form-actions">
      {isDirty && candidates.length>0 && !consent && <span className="model-policy-hint">请先勾选上方数据范围确认</span>}
      <button type="submit" className="model-button primary" disabled={busy||stale||!isDirty||Boolean(candidates.length&&!consent)}>{busy?'正在保存…':'保存策略'}</button>
    </div>
  </form>
}
