import {useRef,useState} from 'react'
import {api,APIError,type Run} from './api'

type Scope={target_count:number}
type PreflightIssue={code:string;target?:string;detail:string}
type AdmissionPreview={
  preview_id:string;state:string;execution_started:boolean
  selection:{target:string;profile:{profile:string;version:number};visible_asset_refs:string[]}
  preflight:{can_queue:boolean;facility_status:string;cases:{key:string;blockers:PreflightIssue[];warnings:PreflightIssue[]}[]}
}
type RevisionPreview={
  run_id:string;conversation_id:string;task_id:string;source_run_id?:string
  proposal:{state:string;expected_revision:number;interpretation:{action:string;case_key:string;parameter?:string;value?:string;change_expected_actual_config?:boolean};
    preview:{revision:number;changes:{added:string[];removed:string[];changed:string[];order_changed:boolean};frozen_cases:string[];
      asset_impact?:{baseline:Scope;proposed:Scope;scope_changed:boolean}}}
}

function commandError(e:unknown, action:string) {
  return e instanceof APIError&&e.status!==undefined&&e.status<500
    ? e.status===409?'原状态已变化，未自动重试。请核对后重新发起。':e.message
    : action+'是否成功尚未核实；不会自动重试。'
}

export function BenchmarkConversation({conversationID,run,onAccepted,onApplied,archived,alreadyAccepted}:{
  conversationID:string;run:Run;onAccepted:(taskID:string)=>void;onApplied:()=>void;archived:boolean;alreadyAccepted:boolean
}) {
  const [admission,setAdmission]=useState<AdmissionPreview>()
  const [revision,setRevision]=useState<RevisionPreview>()
  const [receipt,setReceipt]=useState('')
  const [error,setError]=useState('')
  const [busy,setBusy]=useState(false)
  const request=useRef<string|undefined>(undefined)
  const lock=useRef(false)
  const suggestion=run.benchmark_suggestion
  const revisionSuggestion=run.benchmark_revision_suggestion
  if(!suggestion&&!revisionSuggestion)return null
  const base='/conversations/'+encodeURIComponent(conversationID)
  async function act(action:string,go:()=>Promise<void>) {
    if(lock.current||archived)return
    lock.current=true;setBusy(true);setError('')
    try{await go()}catch(e){
      if(e instanceof APIError&&e.status===409) {
        setAdmission(undefined);setRevision(undefined);onApplied()
      }
      setError(commandError(e,action))
    }finally{lock.current=false;setBusy(false)}
  }
  function previewAdmission() {
    void act('预览',async()=>{
      const value=await api<AdmissionPreview>(base+'/benchmark-suggestions/'+encodeURIComponent(run.id)+'/preview',{action:'preview_benchmark_suggestion'})
      if(value.state!=='preview_only'||value.execution_started||value.selection.target!==suggestion?.target)throw new Error('preview mismatch')
      setAdmission(value)
    })
  }
  function acceptAdmission() {
    if(!admission||!admission.preflight.can_queue||alreadyAccepted)return
    void act('受理',async()=>{
      const value=await api<{preview_id:string;task_id:string;state:string}>('/domain/benchmark-admission/preview/'+encodeURIComponent(admission.preview_id)+'/confirm',{action:'accept_benchmark_admission'})
      if(value.preview_id!==admission.preview_id||value.state!=='accepted'||!value.task_id)throw new Error('receipt mismatch')
      setReceipt('任务已进入队列，尚不代表执行已启动。');onAccepted(value.task_id)
    })
  }
  function previewRevision() {
    void act('预览',async()=>{
      request.current??=crypto.randomUUID()
      const value=await api<RevisionPreview>(base+'/benchmark-revision-preview',{
        action:'preview_benchmark_revision',source_run_id:run.id,client_request_id:request.current,
      })
      if(value.proposal.state!=='preview_only'||value.task_id!==revisionSuggestion?.task_id||value.conversation_id!==conversationID||
          value.source_run_id!==run.id||value.proposal.expected_revision!==revisionSuggestion.expected_revision)throw new Error('preview mismatch')
      setRevision(value)
    })
  }
  function applyRevision() {
    if(!revision)return
    void act('修订',async()=>{
      const value=await api<{preview_run_id:string;revision:number;state:string}>(base+'/benchmark-revision-preview/'+encodeURIComponent(revision.run_id)+'/confirm',{
        action:'apply_benchmark_revision',expected_revision:revision.proposal.expected_revision,
      })
      if(value.state!=='applied'||value.preview_run_id!==revision.run_id||value.revision!==revision.proposal.preview.revision)throw new Error('receipt mismatch')
      setReceipt('方案修订已应用，当前修订 '+value.revision+'。不是本次执行回执。');onApplied()
    })
  }
  const patch=revision?.proposal.preview
  const intent=revision?.proposal.interpretation
  return <section className="conversation-tasks" aria-label={suggestion?'基准受理':'基准方案修订'}>
    <h2>{suggestion?'基准受理':'基准方案修订'}</h2>
    {receipt?<p role="status">{receipt}</p>:suggestion
      ? alreadyAccepted?<p>该请求已受理，查看下方任务。</p>:<>
          <p>{suggestion.target} · {suggestion.profile.profile} v{suggestion.profile.version}</p>
          {!admission&&<button type="button" className="text-button" disabled={busy||archived} onClick={previewAdmission}>查看受理预览</button>}
          {admission&&<><p>已冻结资产目标数 {admission.selection.visible_asset_refs?.length??0}；不代表当前可用节点或 GPU 数。</p>
            <p>预览不启动执行。{admission.preflight.can_queue?'通过入队检查；不代表现场或容量已验证。':'当前不能排队，请先检查预检结果。'}</p>
            {(admission.preflight.cases??[]).flatMap(item=>[...(item.blockers??[]),...(item.warnings??[])].map((issue,index)=>
              <p className="muted" key={item.key+':'+issue.code+':'+index}>{item.key}：{issue.detail}{issue.target&&'（'+issue.target+'）'}</p>))}
            <button type="button" className="text-button" disabled={busy||archived||!admission.preflight.can_queue} onClick={acceptAdmission}>受理并加入队列</button></>}
        </>
      : <>
          <p>任务 {revisionSuggestion?.task_id} · 当前修订 {revisionSuggestion?.expected_revision}</p>
          {!patch&&<button type="button" className="text-button" disabled={busy||archived} onClick={previewRevision}>查看方案修订预览</button>}
          {patch&&intent&&<><p>方案拟修订为 {patch.revision}。此操作修改方案，不是本次跳过，也不直接启动或停止作业。</p>
            <ul>{Boolean(patch.changes.added?.length)&&<li>新增：{patch.changes.added.join('、')}</li>}
              {Boolean(patch.changes.removed?.length)&&<li>移除：{patch.changes.removed.join('、')}</li>}
              {Boolean(patch.changes.changed?.length)&&<li>修改：{patch.changes.changed.join('、')}{intent.parameter&&<>，{intent.parameter} → {intent.value}</>}</li>}
              {intent.change_expected_actual_config&&<li>同时修改实测要求。</li>}
              {patch.changes.order_changed&&<li>调整测试顺序。</li>}
              {Boolean(patch.frozen_cases?.length)&&<li>保持不变的已冻结测项：{patch.frozen_cases.join('、')}</li>}
              {patch.asset_impact&&<li>冻结资产目标数：{patch.asset_impact.baseline.target_count} → {patch.asset_impact.proposed.target_count}，不代表现场实时占用。</li>}</ul>
            <button type="button" className="text-button" disabled={busy||archived} onClick={applyRevision}>应用方案修订</button></>}
        </>}
    {busy&&<p role="status" className="muted">正在核对…</p>}
    {error&&<p role="alert" className="error">{error}</p>}
  </section>
}
