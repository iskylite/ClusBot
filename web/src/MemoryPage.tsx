import { useEffect, useState, type FormEvent } from 'react'
import { ArrowLeft, Bookmark, Check, ChevronRight, ExternalLink, LoaderCircle, Pin, Plus, Search, Trash2 } from 'lucide-react'
import { api, type Memory, type MemoryKind, type MemorySource } from './api'
import {discardMemoryDraft,readMemoryDraft,writeMemoryDraft,type MemoryFields} from './memoryDrafts'

const date = new Intl.DateTimeFormat('zh-CN',{month:'long',day:'numeric'})
const category: Record<MemoryKind,string> = {preference:'偏好',fact:'个人事项'}
const message = (error:unknown) => error instanceof Error ? error.message : '操作未完成'

export function MemoryPage({selected,sourceId,navigate}:{selected?:string;sourceId?:string;navigate:(path:string)=>void}) {
  const [items,setItems]=useState<Memory[]>()
  const [source,setSource]=useState<MemorySource>()
  const [query,setQuery]=useState('')
  const [kind,setKind]=useState('')
  const [error,setError]=useState('')
  const [sourceError,setSourceError]=useState('')
  const [notice,setNotice]=useState('')
  const fresh=selected==='new'
  const item=selected ? items?.find(entry=>entry.id===selected) : items?.[0]
  const draftKey=fresh?'new/'+(sourceId??''):item?.id??selected??''
  const filtered=items?.filter(entry=>(!kind || entry.kind===kind) && (entry.title+'\n'+entry.content).toLowerCase().includes(query.trim().toLowerCase()))

  async function reload() {
    const entries=await api<Memory[]>('/memories')
    setItems(entries)
    return entries
  }
  useEffect(()=>{
    let cancelled=false
    api<Memory[]>('/memories').then(entries=>{if(!cancelled)setItems(entries)}).catch(e=>{if(!cancelled)setError(message(e))})
    return()=>{cancelled=true}
  },[])
  useEffect(()=>{
    setSource(undefined)
    setSourceError('')
    if(!fresh || !sourceId)return
    let cancelled=false
    api<MemorySource>('/memories/source/'+sourceId).then(value=>{if(!cancelled)setSource(value)}).catch(e=>{if(!cancelled)setSourceError(message(e))})
    return()=>{cancelled=true}
  },[fresh,sourceId])

  function select(path:string) {
    setNotice('')
    navigate(path)
  }
  function onSaved(saved:Memory) {
    setItems(previous=>[...(previous??[]).filter(entry=>entry.id!==saved.id),saved]
      .sort((left,right)=>Number(right.pinned)-Number(left.pinned)||right.updated_at-left.updated_at))
    setNotice('已保存')
    navigate('/memory/'+saved.id)
  }
  function onDeleted(id:string) {
    setItems(previous=>previous?.filter(entry=>entry.id!==id))
    setNotice('已删除')
    navigate('/memory')
  }
  const canEdit=(!fresh && (item || readMemoryDraft(draftKey)?.original)) || (fresh && (!sourceId || String(source?.message_id)===sourceId || readMemoryDraft(draftKey)))
  return <>
    <main className={'main memory-list '+(selected?'memory-list-hidden':'')}>
      <header className="page-heading"><h1>个人记忆</h1><div className="task-actions">
        <button className="outline" onClick={()=>select('/memory/candidates')}>经验候选</button>
        <button className="outline" onClick={()=>select('/memory/skill-proposals')}>Skill 提案</button>
        <button className="primary" onClick={()=>select('/memory/new')}><Plus size={17}/>新增记忆</button>
      </div></header>
      <div className="memory-toolbar">
        <label className="search"><Search size={17}/><input aria-label="搜索记忆" placeholder="搜索记忆" value={query} onChange={event=>setQuery(event.target.value)}/></label>
        <select aria-label="记忆类别筛选" value={kind} onChange={event=>setKind(event.target.value)}>
          <option value="">全部类别</option><option value="preference">偏好</option><option value="fact">个人事项</option>
        </select>
      </div>
      {error && <p className="error" role="alert">{error}</p>}
      {!items && !error && <LoaderCircle className="spin" aria-label="正在加载记忆"/>}
      {filtered?.map(entry=><button key={entry.id} className={'memory-row '+(!fresh && item?.id===entry.id?'selected':'')} onClick={()=>select('/memory/'+entry.id)}>
        <span className="memory-icon">{entry.pinned?<Pin size={17}/>:<Bookmark size={17}/>}</span>
        <span className="memory-row-content"><strong>{entry.title}</strong><small>{entry.content}</small></span>
        <span className="memory-row-date">{date.format(entry.updated_at)}</span><ChevronRight size={16} className="memory-row-arrow"/>
      </button>)}
      {filtered?.length===0 && <div className="empty"><span className="empty-icon"><Bookmark size={26}/></span><h2>{items?.length?'没有匹配的记忆':'还没有记忆'}</h2>
        {!items?.length && <p>保存常用偏好和需要长期保留的事项。</p>}
      </div>}
      {notice==='已删除' && <p className="memory-saved" role="status"><Check size={16}/>已删除</p>}
    </main>
    <aside className={'detail-pane memory-detail '+(selected?'memory-detail-open':'')}>
      <div className="detail-content">
        <button className="text-button memory-back" onClick={()=>select('/memory')}><ArrowLeft size={16}/>个人记忆</button>
        {canEdit ? <MemoryEditor key={draftKey} draftKey={draftKey} item={fresh?undefined:item} source={fresh?source:undefined}
          onSaved={onSaved} onDeleted={onDeleted} reload={reload} navigate={select} notice={notice}/>
          : sourceError ? <p className="error" role="alert">{sourceError}</p>
          : fresh || !items ? <LoaderCircle className="spin" aria-label="正在加载记忆"/>
          : <div className="pane-empty"><Bookmark size={26}/><p>{selected?'这条记忆已删除':'选择一条记忆'}</p></div>}
      </div>
    </aside>
  </>
}

type Draft = MemoryFields
const draftFrom = (item?:Memory, source?:MemorySource):Draft => ({
  kind:item?.kind??'preference',title:item?.title??'',
  content:item?.content??Array.from(source?.text??'').slice(0,1000).join(''),pinned:item?.pinned??false,
})

function MemoryEditor({draftKey,item,source,onSaved,onDeleted,reload,navigate,notice}:{
  draftKey:string;item?:Memory;source?:MemorySource;onSaved:(item:Memory)=>void;onDeleted:(id:string)=>void
  reload:()=>Promise<Memory[]>;navigate:(path:string)=>void;notice:string
}) {
  const [restored]=useState(()=>readMemoryDraft(draftKey))
  const [original,setOriginal]=useState(restored?.original??item)
  const [draft,setDraft]=useState(()=>restored?.fields??draftFrom(item,source))
  const [busy,setBusy]=useState(false)
  const [error,setError]=useState('')
  const [confirmDelete,setConfirmDelete]=useState(false)
  const [changed,setChanged]=useState(Boolean(restored))
  const draftSource=restored?.source??source
  const origin=original?.source??draftSource

  function update(value:Partial<Draft>) {
    const fields={...draft,...value}
    setDraft(fields)
    setChanged(true)
    writeMemoryDraft(draftKey,{fields,original,source:draftSource})
  }
  function reset(value=original) {
    setOriginal(value)
    setDraft(draftFrom(value,draftSource))
    setChanged(false)
    discardMemoryDraft(draftKey)
    setError('')
    setConfirmDelete(false)
  }
  async function save(event:FormEvent) {
    event.preventDefault()
    if(busy || !draft.title.trim() || !draft.content.trim())return
    setBusy(true)
    setError('')
    try {
      const result=await api<Memory>(original?'/memories/'+original.id:'/memories',
        {...draft,revision:original?.revision,source_message:original?undefined:draftSource?.message_id},original?'PATCH':'POST')
      reset(result)
      onSaved(result)
    } catch(e) {setError(message(e))}
    finally {setBusy(false)}
  }
  async function captureCorrection() {
    if(busy || !draftSource)return
    setBusy(true)
    setError('')
    try {
      await api('/experience-candidates/from-correction',{message_id:draftSource.message_id})
      navigate('/memory/candidates')
    } catch(e) {setError(message(e))}
    finally {setBusy(false)}
  }
  async function remove() {
    if(busy || !original)return
    setBusy(true)
    setError('')
    try {
      await api('/memories/'+original.id,{revision:original.revision},'DELETE')
      discardMemoryDraft(draftKey)
      onDeleted(original.id)
    } catch(e) {setError(message(e));setConfirmDelete(false)}
    finally {setBusy(false)}
  }
  async function loadLatest() {
    if(!original || (changed && !window.confirm('载入最新版本并放弃当前草稿？')))return
    setBusy(true)
    try {
      const latest=(await reload()).find(entry=>entry.id===original.id)
      if(latest) reset(latest)
      else setError('这条记忆已删除。')
    } catch(e) {setError(message(e))}
    finally {setBusy(false)}
  }
  return <form className="memory-form" onSubmit={save}>
    <header className="page-heading"><h2>{original?'编辑记忆':'新增记忆'}</h2>
      {original && <button type="button" className="text-button memory-delete" aria-label="删除记忆" onClick={()=>setConfirmDelete(true)} disabled={busy}><Trash2 size={17}/>删除</button>}
    </header>
    <label>标题<input aria-label="标题" required value={draft.title} placeholder="例如：周报格式" maxLength={60} readOnly={busy} onChange={event=>update({title:event.target.value})}/></label>
    <label>类别<select aria-label="类别" value={draft.kind} onChange={event=>update({kind:event.target.value as MemoryKind})} disabled={busy}>
      {Object.entries(category).map(([key,label])=><option key={key} value={key}>{label}</option>)}</select>
      <small>{draft.kind==='preference'?'用作新请求的默认偏好。':'请求与标题关键词匹配时使用。'}</small>
    </label>
    <label>内容<textarea aria-label="内容" rows={7} required value={draft.content} maxLength={1000} readOnly={busy} onChange={event=>update({content:event.target.value})}/></label>
    {source?.text && Array.from(source.text).length>1000 && <small>已预填前 1000 字，可从来源查看原文。</small>}
    <label className="memory-pin"><input type="checkbox" checked={draft.pinned} disabled={busy} onChange={event=>update({pinned:event.target.checked})}/>置顶</label>
    <dl className="memory-meta"><dt>来源</dt><dd>{origin?<button type="button" className="text-button" onClick={()=>navigate(origin.path+'#message-'+origin.message_id)}>{origin.title}<ExternalLink size={14}/></button>:original?.source_deleted?'原对话已删除':'手动添加'}</dd>
      {original && <><dt>最近更新</dt><dd>{date.format(original.updated_at)}</dd></>}
    </dl>
    {error && <div><p className="error" role="alert">{error}</p>{original && <button type="button" className="text-button" disabled={busy} onClick={()=>void loadLatest()}>载入最新版本</button>}</div>}
    {confirmDelete && <div className="memory-delete-panel">
      <p>删除这条记忆？</p><button type="button" className="outline" disabled={busy} onClick={()=>setConfirmDelete(false)}>取消</button>
      <button type="button" className="outline memory-delete" disabled={busy} onClick={()=>void remove()}>确认删除</button>
    </div>}
    <div className="memory-form-actions">
      {!original && draftSource?.path.startsWith('/tasks/') && <button type="button" className="text-button" disabled={busy}
        onClick={()=>void captureCorrection()}>标记原消息为纠正</button>}
      {notice==='已保存' && !changed && <span className="memory-saved" role="status"><Check size={16}/>已保存</span>}
      {changed && <button type="button" className="text-button" onClick={()=>reset(item)} disabled={busy}>{original?'取消修改':'清空草稿'}</button>}
      <button className="primary" disabled={busy || confirmDelete || !draft.title.trim() || !draft.content.trim() || Boolean(original && !changed)}>{busy?<LoaderCircle size={16} className="spin"/>:<Check size={16}/>}保存记忆</button>
    </div>
  </form>
}
