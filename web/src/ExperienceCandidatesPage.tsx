import { useEffect, useState } from 'react'
import { ArrowLeft, LoaderCircle } from 'lucide-react'
import { api, type ExperienceCandidate } from './api'

const statusLabel = {pending:'待审阅',approved:'已保留',rejected:'已驳回',unavailable:'来源不可用'}

export function ExperienceCandidatesPage({navigate}:{navigate:(path:string)=>void}) {
  const [items,setItems]=useState<ExperienceCandidate[]>()
  const [busy,setBusy]=useState('')
  const [error,setError]=useState('')
  useEffect(()=>{
    let active=true
    api<ExperienceCandidate[]>('/experience-candidates').then(result=>{if(active)setItems(result)})
      .catch(e=>{if(active)setError(e instanceof Error?e.message:'加载候选失败')})
    return()=>{active=false}
  },[])

  async function review(item:ExperienceCandidate, decision:'approved'|'rejected') {
    if(busy)return
    setBusy(item.id)
    setError('')
    try {
      const result=await api<ExperienceCandidate>('/experience-candidates/'+item.id+'/review',{revision:item.revision,decision})
      setItems(previous=>previous?.map(entry=>entry.id===result.id?result:entry))
    } catch(e) {
      setError(e instanceof Error?e.message:'审阅失败')
      const latest=await api<ExperienceCandidate[]>('/experience-candidates').catch(()=>undefined)
      if(latest)setItems(latest)
    } finally {setBusy('')}
  }

  return <main className="main memory-list">
    <button type="button" className="text-button back" onClick={()=>navigate('/memory')}><ArrowLeft size={16}/>个人记忆</button>
    <header className="page-heading"><h1>经验候选</h1><button type="button" className="outline"
      onClick={()=>navigate('/memory/skill-proposals')}>Skill 提案</button></header>
    {error && <p className="error" role="alert">{error}</p>}
    {!items && !error && <LoaderCircle className="spin" aria-label="正在加载候选"/>}
    {items?.length===0 && <p className="muted">验收成果或标记纠正消息后，候选会出现在这里。</p>}
    {items?.map(item=><section className="work-goal" key={item.id}>
      <h2>{item.kind==='user_correction'?'用户纠正':'已验收成果'} <small className="muted">{statusLabel[item.status]}</small></h2>
      {item.goal && <p>{item.goal}</p>}
      {item.scope_label && <p className="muted">适用于同一范围和目标 · {item.scope_label}</p>}
      {item.status!=='unavailable' && !item.scope_label && <p className="muted">仅适用于原事项</p>}
      {item.acceptance_criteria && <p>{item.acceptance_criteria}</p>}
      {!!item.format_cues?.length && <p>{item.format_cues.join('、')}</p>}
      {item.correction && <p>{item.correction}{item.correction_truncated?'…':''}</p>}
      {item.status!=='unavailable' && <div className="task-actions">
        <button type="button" className="text-button" onClick={()=>navigate(item.message_id
          ? '/tasks/'+item.task_id+'#message-'+item.message_id
          : '/artifacts/'+item.artifact_id+'?task_id='+item.task_id)}>{item.message_id?'查看原消息':'查看验收成果'}</button>
        {item.counter_artifact_id && <button type="button" className="text-button"
          onClick={()=>navigate('/artifacts/'+item.counter_artifact_id+'?task_id='+item.task_id)}>查看反例</button>}
        {item.status==='pending' && <>
          <button type="button" className="outline" disabled={!!busy} onClick={()=>void review(item,'rejected')}>驳回</button>
          <button type="button" className="outline" disabled={!!busy} onClick={()=>void review(item,'approved')}>保留候选</button>
        </>}
      </div>}
    </section>)}
  </main>
}
