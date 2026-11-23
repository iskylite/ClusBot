import {useEffect,useRef,useState} from 'react'
import {ArrowRight,Check,Info,ShieldAlert,Upload,X} from 'lucide-react'
import {api,APIError} from './api'
import type {CatalogModel} from './ModelSettings'

type Candidate={profile_id:string;profile_revision:number}
type Policy={revision:number;candidates:Candidate[];role:string;business:string;allow_cross_provider:boolean}
const scopes:Record<string,string>={conversation:'对话',task:'任务',benchmark:'基准',default:'默认'}
const roles=new Set(['general_router','conversation_reply','task_worker','benchmark_admission','benchmark_revision','benchmark_execution'])
const fields:Record<string,string>={
  submitted_text:'提交文字',recent_history:'近期对话',task_candidates:'候选任务',pending_intents:'待处理意图',benchmark_candidate:'基准候选方案',
  task_messages_history_and_requirements:'任务消息和要求',selected_text_materials:'文本材料',selected_images_if_supported:'图片（模型支持时）',
  matching_user_memories:'记忆',workflow_input_artifacts:'工作流产物',applicable_skill_and_experience:'Skill 和经验',
  authorized_read_tool_definitions_and_results:'授权只读工具',current_run_model_tool_round_history:'本次工具往返记录',
  published_target:'已发布目标',published_profile_and_version:'已发布方案与版本',editable_case_keys:'可编辑测项',
  editable_parameter_keys:'可编辑参数',pending_case_keys:'待派发测项',pending_case_positions:'待派发顺序',
}
export type BindingRole={role:string;source:string;business?:string;model_id?:string;egress_fields?:string[]}

export function AssistantRoleBinding({role,label,models,providers,initialProfileID,close,saved,openPolicies}:{
  role:BindingRole;label:string;models:CatalogModel[];providers?:{id:string;credential_status:string}[];initialProfileID?:string;
  close:()=>void;saved:()=>Promise<void>;openPolicies:()=>void;
}){
  const business=role.source==='role_policy'?'default':role.business??''
  const eligible=roles.has(role.role)&&Object.hasOwn(scopes,business)
  const [policy,setPolicy]=useState<Policy|null>()
  const [choice,setChoice]=useState(initialProfileID??'')
  const [busy,setBusy]=useState(false)
  const [error,setError]=useState('')
  const lock=useRef(false)
  const available=(model:CatalogModel)=>model.enabled&&model.provider_enabled&&
    (model.credential_configured||providers?.some(provider=>provider.id===model.provider_id&&provider.credential_status==='not_required'))
  const next=models.find(model=>model.id===choice&&available(model))
  const shared=(role.egress_fields??[]).map(field=>fields[field]).filter(Boolean)

  useEffect(()=>{
    if(!eligible)return
    let active=true
    api<{policies:Policy[]}>('/settings/model/routing/policies').then(value=>{
      if(active)setPolicy(value.policies.find(item=>item.business===business&&item.role===role.role)??null)
    }).catch(()=>{if(active)setError('无法读取角色绑定。')})
    return()=>{active=false}
  },[eligible,business,role.role])

  async function confirm(){
    if(lock.current||policy===undefined||!next||!eligible||!shared.length)return
    const candidates=policy?.candidates??[]
    const remaining=candidates.slice(1).filter(item=>item.profile_id!==next.id)
    lock.current=true;setBusy(true);setError('')
    try{
      const current=await api<CatalogModel[]>('/settings/model/catalog')
      const latest=current.find(model=>model.id===next.id)
      if(!latest||latest.revision!==next.revision||latest.provider_revision!==next.provider_revision||!available(latest)){
        setPolicy(undefined)
        throw new Error('模型或提供方配置已变化，请重新打开并核对。')
      }
      await api('/settings/model/routing/policies/'+business+'/'+role.role,{
        revision:policy?.revision??0,
        candidates:[{profile_id:next.id,profile_revision:next.revision},...remaining],
        allow_cross_provider:policy?.allow_cross_provider??false,confirm_egress:true,
      },'PUT')
      await saved()
    }catch(error){
      if(error instanceof APIError&&error.status===409)setPolicy(undefined)
      setError(error instanceof Error?error.message:'无法保存角色绑定。')
    }finally{lock.current=false;setBusy(false)}
  }

  return <section className="assistant-card assistant-card-confirmation assistant-role-binding" aria-label="角色绑定确认">
    <div className="assistant-card-title"><h3><ShieldAlert size={15}/>确认变更 · {label}绑定</h3>{eligible&&<span className="assistant-expiry">{scopes[business]}范围</span>}</div>
    {eligible&&<div className="assistant-role-transition">
      <code>{role.model_id||'当前未绑定'}</code><ArrowRight size={15} aria-hidden="true"/>
      <select aria-label="新绑定模型" value={choice} onChange={event=>setChoice(event.target.value)} disabled={busy||policy===undefined}>
        <option value="">选择新模型</option>
        {models.filter(available).map(model=><option key={model.id} value={model.id}>{model.name}</option>)}
      </select>
    </div>}
    <p className="assistant-role-note"><Info size={13} aria-hidden="true"/>{role.role==='task_worker'?'仅对新任务生效':'仅对新的调用生效'}，已派发的执行保留启动时的模型配置。</p>
    {shared.length>0&&<p className="assistant-role-note"><Upload size={13} aria-hidden="true"/>发送数据：{shared.join(' · ')}</p>}
    {Boolean(policy?.candidates[1])&&<p>保留其余候补模型。<button type="button" className="text-button" onClick={openPolicies}>查看完整候补链</button></p>}
    {error&&<p className="error" role="alert">{error}</p>}
    <footer><button type="button" className="outline" onClick={close} disabled={busy}><X size={13}/>取消</button>
      <button type="button" className="primary" disabled={busy||!next||policy===undefined||!shared.length} onClick={()=>void confirm()}><Check size={13}/>{busy?'正在处理…':'确认绑定'}</button></footer>
  </section>
}
