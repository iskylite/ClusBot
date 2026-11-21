import {useCallback, useEffect, useRef, useState, type FormEvent} from 'react'
import {ArrowDown, ArrowUp, Plus, X} from 'lucide-react'
import {api, APIError} from './api'
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
const stateNames:Record<string,string>={locally_configured:'本地配置可用',configured:'本地配置可用',profile_missing:'模型已不存在',profile_revision_changed:'模型版本已变化',profile_disabled:'模型已停用',provider_disabled:'提供方已停用',profile_invalid:'模型配置无效',credential_missing:'凭据不可用',credential_changed:'凭据已变化',demo_profile_disabled:'演示模式不可用',unconfigured:'未配置',ready:'可用',selected:'已选择'}
const sourceNames:Record<string,string>={business_override:'业务覆盖',role_policy:'阶段策略',global_default:'全局默认',explicit_profile:'旧角色绑定',uses_existing_default:'默认模型'}
const err=(e:unknown)=>e instanceof APIError&&e.status===409?'配置版本已变化，未保存。请重新读取并核对后重试。':e instanceof Error?e.message:'请求未完成，请重试'

export function ModelRoutingSettings({models,providers,generation}:{models:CatalogModel[];providers:Provider[];generation:number}) {
  const [policies,setPolicies]=useState<Policy[]>()
  const [stage,setStage]=useState(stages[0].role)
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
  return <section id="model-routing" className="model-routing-panel" aria-labelledby="model-routing-title">
    <div className="model-panel-heading"><h2 id="model-routing-title">阶段模型策略</h2><button type="button" className="model-link" onClick={()=>void reload()}>重新读取</button></div>
    <p className="muted">按顺序选择候选模型；未设置时沿用下层配置。预览显示已保存的实际选择，不会调用模型。</p>
    <div className="model-edit-grid"><label>阶段<select value={stage} onChange={e=>{setStage(e.currentTarget.value);setPreview(undefined);setNotice('');setReset(n=>n+1)}}>{stages.map(s=><option value={s.role} key={s.role}>{s.title}</option>)}</select></label>
      <label>策略范围<select value={scope} onChange={e=>{setScope(e.currentTarget.value as 'default'|'business');setNotice('');setReset(n=>n+1)}}><option value="business">{spec.business==='conversation'?'对话':spec.business==='task'?'任务':'基准'}业务覆盖</option><option value="default">阶段默认</option></select></label></div>
    {preview&&<div className="model-routing-preview"><strong>当前生效</strong><span>{sourceNames[preview.source]??preview.source} · {preview.model_id??'未选择模型'} · {stateNames[preview.status]??preview.status}</span><small>仅检查本地配置；候补仅在输出前遇到网络、超时、限流或服务不可用时尝试，工具调用不自动候补。</small></div>}
    {policies&&<PolicyEditor key={stage+':'+business+':'+reset} spec={spec} business={business} initial={policy} models={models} providers={providers} onSaved={async()=>{await Promise.all([load(),loadPreview()]);setReset(n=>n+1);setNotice('阶段策略已保存。')}}/>}
    {notice&&<p className="model-feedback" role="status">{notice}</p>}
    {error&&<p className="error" role="alert">{error}</p>}
  </section>
}

function PolicyEditor({spec,business,initial,models,providers,onSaved}:{spec:Stage;business:string;initial?:Policy;models:CatalogModel[];providers:Provider[];onSaved:()=>Promise<void>}) {
  const [candidates,setCandidates]=useState<Candidate[]>(()=>initial?.candidates??[])
  const [allowCross,setAllowCross]=useState(initial?.allow_cross_provider??false)
  const [choice,setChoice]=useState(''),[consent,setConsent]=useState(false),[busy,setBusy]=useState(false),[error,setError]=useState(''),[notice,setNotice]=useState('')
  const available=models.filter(m=>m.enabled&&m.provider_enabled&&!candidates.some(c=>c.profile_id===m.id)&&providers.some(p=>p.id===m.provider_id&&p.enabled&&p.credential_status!=='missing'))
  const stale=candidates.some(c=>{const m=models.find(item=>item.id===c.profile_id);return !m||m.revision!==c.profile_revision||!m.enabled||!m.provider_enabled})
  function change(next:Candidate[]){setCandidates(next);setConsent(false);setNotice('');setError('')}
  function add(){const item=models.find(m=>m.id===choice);if(!item||candidates.length>=4)return;change([...candidates,{profile_id:item.id,profile_revision:item.revision}]);setChoice('')}
  function move(index:number,offset:number){const next=[...candidates],target=index+offset;if(target<0||target>=next.length)return;[next[index],next[target]]=[next[target],next[index]];change(next)}
  async function save(e:FormEvent<HTMLFormElement>){e.preventDefault();if(busy||candidates.length>0&&!consent)return;setBusy(true);setError('');setNotice('')
    try{await api('/settings/model/routing/policies/'+business+'/'+spec.role,{revision:initial?.revision??0,candidates:candidates.map(c=>({profile_id:c.profile_id,profile_revision:c.profile_revision})),allow_cross_provider:allowCross,confirm_egress:consent},'PUT');await onSaved()}catch(e){setError(err(e))}finally{setBusy(false)}
  }
  return <form className="model-policy" aria-label={`${spec.title}模型策略`} onSubmit={save}>
    <div className="model-panel-heading"><h3>候选顺序</h3><small>最多 4 个 · 修订 #{initial?.revision??0}</small></div>
    {candidates.length===0?<p className="muted">未设置候选，沿用下层配置。</p>:<ol className="model-candidate-list">{candidates.map((candidate,index)=>{
      const model=models.find(item=>item.id===candidate.profile_id),provider=providers.find(item=>item.id===model?.provider_id)
      const changed=!model||model.revision!==candidate.profile_revision
      return <li key={candidate.profile_id}><span><strong>{model?.name??candidate.model_id??'模型已不存在'}</strong><small>{provider?.name??''} · 修订 #{candidate.profile_revision}{changed?' · 版本已变化，请移除后重新添加':candidate.status&&candidate.status!=='locally_configured'?' · '+(stateNames[candidate.status]??candidate.status):''}</small></span><div><button type="button" className="model-icon-button" aria-label={`上移 ${model?.name??'候选模型'}`} disabled={busy||index===0} onClick={()=>move(index,-1)}><ArrowUp size={15}/></button><button type="button" className="model-icon-button" aria-label={`下移 ${model?.name??'候选模型'}`} disabled={busy||index===candidates.length-1} onClick={()=>move(index,1)}><ArrowDown size={15}/></button><button type="button" className="model-icon-button" aria-label={`移除 ${model?.name??'候选模型'}`} disabled={busy} onClick={()=>change(candidates.filter((_,i)=>i!==index))}><X size={15}/></button></div></li>
    })}</ol>}
    {candidates.length<4&&<div className="model-add-candidate"><label className="model-sr-only" htmlFor="model-policy-choice">添加候选模型</label><select id="model-policy-choice" value={choice} disabled={busy} onChange={e=>setChoice(e.currentTarget.value)}><option value="">选择候选模型</option>{available.map(m=><option key={m.id} value={m.id}>{providers.find(p=>p.id===m.provider_id)?.name} / {m.name} · 修订 #{m.revision}</option>)}</select><button type="button" className="model-button" disabled={!choice||busy} onClick={add}><Plus size={14}/>添加</button></div>}
    <label className="model-check"><input type="checkbox" checked={allowCross} disabled={busy} onChange={e=>{setAllowCross(e.currentTarget.checked);setConsent(false)}}/>允许跨提供方候补</label>
    {candidates.length>0&&<label className="model-check model-consent"><input type="checkbox" checked={consent} disabled={busy} onChange={e=>setConsent(e.currentTarget.checked)}/><span>我确认以上所有候选模型均可处理此阶段的数据：{spec.egress}。</span></label>}
    {stale&&<p className="error">候选模型已变化或不可用，请移除后重新添加。</p>}
    {error&&<p className="error" role="alert">{error}</p>}{notice&&<p className="model-feedback" role="status">{notice}</p>}
    <div className="model-form-actions"><button type="submit" className="model-button primary" disabled={busy||stale||Boolean(candidates.length&&!consent)}>{busy?'正在保存…':'保存阶段策略'}</button></div>
  </form>
}
