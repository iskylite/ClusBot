import {useRef,useState,type FormEvent} from 'react'
import {api,APIError,type BenchmarkExecutionReceipt,type BenchmarkExecutionSuggestion,type Run} from './api'

type Target={taskID:string;title:string;planRevision?:number}

export function BenchmarkExecutionConversation({conversationID,target,run,disabled,canSuggest,onChange}:{
  conversationID:string;target:Target;run?:Run;disabled:boolean;canSuggest:boolean;onChange:()=>void
}) {
  const [open,setOpen]=useState(false)
  const [text,setText]=useState('')
  const [busy,setBusy]=useState(false)
  const [error,setError]=useState('')
  const [rejected,setRejected]=useState<string>()
  const [sent,setSent]=useState<{runID:string;suggestion:BenchmarkExecutionSuggestion}>()
  const [receipt,setReceipt]=useState<BenchmarkExecutionReceipt>()
  const locked=useRef(false)
  const request=useRef<{text:string;key:string}|undefined>(undefined)
  const base='/conversations/'+encodeURIComponent(conversationID)
  const current=run?.benchmark_execution_suggestion?.task_id===target.taskID
    ? {runID:run.id,suggestion:run.benchmark_execution_suggestion} : undefined
  const candidate=sent??current
  const completed=receipt??(run?.id===candidate?.runID?run?.benchmark_execution_control:undefined)
  const canConfirm=Boolean(candidate?.suggestion.state==='confirmation_required'&&candidate.suggestion.plan_revision===target.planRevision&&!completed&&rejected!==candidate.runID)
  async function perform(fn:()=>Promise<void>,failure:(e:unknown)=>void) {
    if(locked.current||disabled)return
    locked.current=true;setBusy(true);setError('')
    try{await fn()}catch(e){failure(e)}finally{locked.current=false;setBusy(false)}
  }
  async function suggest(event:FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const value=text.trim()
    if(!canSuggest||!value||!target.planRevision)return
    if(new TextEncoder().encode(value).length>1024){setError('请缩短说明，仅描述本次要跳过的一个测项。');return}
    await perform(async()=>{
      if(request.current?.text!==value)request.current={text:value,key:crypto.randomUUID()}
      const result=await api<{run_id:string;benchmark_execution_suggestion?:BenchmarkExecutionSuggestion;benchmark_execution_control?:unknown}>(base+'/messages',{
        text:value,client_request_id:request.current.key,interpret_benchmark_execution:true,
        benchmark_execution_target:{task_id:target.taskID,expected_plan_revision:target.planRevision},
      })
      const proposed=result.benchmark_execution_suggestion
      if(!result.run_id||!proposed?.step_id||!proposed.case_key||proposed.task_id!==target.taskID||proposed.plan_revision!==target.planRevision||
          proposed.state!=='confirmation_required'||result.benchmark_execution_control)throw new Error('unexpected response')
      request.current=undefined
      setSent({runID:result.run_id,suggestion:proposed});setText('');setOpen(false);setReceipt(undefined);setRejected(undefined);onChange()
    },e=>{
      if(e instanceof APIError&&e.status!==undefined&&e.status<500)request.current=undefined
      setError(e instanceof APIError&&e.status!==undefined&&e.status<500?e.message:'尚未核实是否生成建议。没有自动重发。')
    })
  }
  async function confirm() {
    if(!candidate||!canConfirm)return
    await perform(async()=>{
      const outcome=await api<BenchmarkExecutionReceipt>(base+'/benchmark-execution-suggestions/'+encodeURIComponent(candidate.runID)+'/confirm',{action:'skip_this_run'})
      const expected=candidate.suggestion
      if(outcome.task_id!==expected.task_id||outcome.step_id!==expected.step_id||outcome.plan_revision!==expected.plan_revision||
          outcome.action!=='skip_this_run'||outcome.state!=='skipped_this_run')throw new Error('unexpected response')
      setReceipt(outcome);onChange()
    },e=>{
      const conflict=e instanceof APIError&&e.status===409
      if(conflict){setRejected(candidate.runID);onChange()}
      setError(conflict?'方案或测项状态已变化。这次没有执行，也不会自动重试。请查看任务最新状态。':
        e instanceof APIError&&e.status!==undefined&&e.status<500?e.message:'尚未核实是否已执行；没有自动重发。')
    })
  }
  const title='本次跳过建议 · '+target.title
  return <section className="conversation-tasks" aria-label={title}>
    {candidate&&<div>
      <h2>{completed?'执行回执':'本次跳过建议'}</h2>
      <p>{target.title} · {candidate.suggestion.case_key} · 方案修订 {candidate.suggestion.plan_revision}</p>
      {completed?<p role="status">已登记本次跳过。不移除必测项，不计为通过。</p>:<>
        <p>仅为建议，尚未执行。不修改方案，也不取消远端作业。</p>
        {canConfirm&&<button type="button" className="text-button" disabled={disabled||busy} onClick={()=>void confirm()}>确认本次跳过</button>}
      </>}
    </div>}
    {canSuggest&&!open&&target.planRevision!==undefined&&<button type="button" className="text-button" disabled={disabled||busy}
      onClick={()=>{setOpen(true);setError('')}}>描述要跳过的测项</button>}
    {canSuggest&&open&&<form className="composer" aria-label={title} onSubmit={event=>void suggest(event)}>
      <p>{target.title} · 方案修订 {target.planRevision}。只生成本次跳过建议，待你确认。</p>
      <textarea autoFocus name="skip-description" aria-label="描述本次要跳过的一个测项" maxLength={1024} value={text}
        readOnly={busy} onChange={event=>setText(event.currentTarget.value)} rows={2}/>
      <div className="composer-tools">
        <button type="button" className="text-button" disabled={busy} onClick={()=>{setOpen(false);setError('')}}>退出跳过模式</button>
        <button type="submit" className="primary" disabled={disabled||busy||!text.trim()}>生成跳过建议</button>
      </div>
    </form>}
    {busy&&<p role="status" className="muted">正在核对…</p>}
    {error&&<p role="alert" className="error">{error}</p>}
  </section>
}
