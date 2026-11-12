import {useEffect,useRef,useState,type FormEvent} from 'react'
import {Check,X} from 'lucide-react'
import {api,type Task} from './api'

function taskValues(task:Task) {
  return {title:task.title,goal:task.goal,scope_label:task.scope_label,
    acceptance_criteria:task.acceptance_criteria??'',required_filename:task.required_filename??''}
}

export function TaskEditor({task,blocked,reload,onSaved,onClose}:{task:Task;blocked:boolean;reload:()=>Promise<void>;onSaved:()=>Promise<void>;onClose:()=>void}) {
  const dialog=useRef<HTMLDialogElement>(null)
  const title=useRef<HTMLInputElement>(null)
  const [base,setBase]=useState(()=>({revision:task.revision,values:taskValues(task)}))
  const [draft,setDraft]=useState(()=>taskValues(task))
  const [saving,setSaving]=useState(false)
  const [error,setError]=useState('')
  const stale=task.revision!==base.revision
  const changes=Object.fromEntries((Object.keys(draft) as (keyof typeof draft)[])
    .filter(key=>draft[key].trim()!==base.values[key]).map(key=>[key,draft[key].trim()]))
  useEffect(()=>{
    const modal=dialog.current
    if(!modal)return
    modal.showModal()
    title.current?.focus()
    return()=>{if(modal.open)modal.close()}
  },[])
  function dismiss() {
    dialog.current?.close()
    onClose()
  }
  async function save(event:FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if(saving||blocked||stale||!Object.keys(changes).length)return
    setSaving(true);setError('')
    try {
      await api<Task>('/tasks/'+task.id,{revision:base.revision,...changes},'PATCH')
      await onSaved()
      dismiss()
    }catch(error){setError(error instanceof Error?error.message:'事项保存失败');await reload()}
    finally{setSaving(false)}
  }
  return <dialog className="new-dialog task-editor" aria-labelledby="task-editor-title" ref={dialog} onCancel={event=>{event.preventDefault();if(!saving)dismiss()}}>
    <header><h2 id="task-editor-title">编辑事项</h2><button type="button" className="icon-button" aria-label="关闭编辑" disabled={saving} onClick={dismiss}><X size={20}/></button></header>
    <form onSubmit={save}>
      <label>事项标题<input ref={title} maxLength={60} required readOnly={saving} value={draft.title} onChange={event=>setDraft({...draft,title:event.target.value})}/></label>
      <label>事项目标<textarea aria-label="事项目标" rows={3} maxLength={16000} required readOnly={saving} value={draft.goal} onChange={event=>setDraft({...draft,goal:event.target.value})}/></label>
      <label>工作范围<input maxLength={40} readOnly={saving} value={draft.scope_label} onChange={event=>setDraft({...draft,scope_label:event.target.value})}/></label>
      <label>验收条件<textarea aria-label="验收条件" rows={2} maxLength={1000} readOnly={saving} value={draft.acceptance_criteria} onChange={event=>setDraft({...draft,acceptance_criteria:event.target.value})}/></label>
      <label>交付文件名<span className="filename-input"><span>outputs/</span><input aria-label="交付文件名" maxLength={180} readOnly={saving} value={draft.required_filename} onChange={event=>setDraft({...draft,required_filename:event.target.value})}/></span></label>
      {stale ? <div className="task-edit-conflict"><p className="error" role="alert">事项已变更。载入最新版本会替换当前编辑内容。</p><button type="button" className="text-button" disabled={saving} onClick={()=>{
        const values=taskValues(task)
        setBase({revision:task.revision,values});setDraft(values);setError('')
      }}>载入最新版本</button></div> : error&&<p className="error" role="alert">{error}</p>}
      <div className="task-actions">
        <button type="button" className="outline" disabled={saving} onClick={dismiss}>取消</button>
        <button className="primary" disabled={saving||blocked||stale||!draft.title.trim()||!draft.goal.trim()||!Object.keys(changes).length}><Check size={16}/>保存修改</button>
      </div>
    </form>
  </dialog>
}
