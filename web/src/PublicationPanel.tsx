import { useRef, useState, type FormEvent } from 'react'
import { CircleAlert, FileCheck2, FileOutput, FilePenLine, LoaderCircle } from 'lucide-react'
import { api, type Artifact, type Publication, type Task } from './api'
import { ApprovalExpiry } from './ApprovalExpiry'

export function PublicationPanel({ task, artifact, publication, reload }: {
  task: Task
  artifact: Artifact
  publication?: Publication
  reload: () => Promise<void>
}) {
  const [editing, setEditing] = useState(false)
  const [name, setName] = useState(task.required_filename || artifact.name)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const requestKey = useRef('')
  const locked = useRef(false)

  async function send(path: string, body: unknown, requested = false) {
    if (locked.current) return
    locked.current = true
    setBusy(true)
    setError('')
    try {
      await api(path, body)
      if (requested) {
        setEditing(false)
        requestKey.current = ''
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : '操作未完成')
    } finally {
      await reload()
      locked.current = false
      setBusy(false)
    }
  }

  function prepare(event: FormEvent) {
    event.preventDefault()
    requestKey.current ||= crypto.randomUUID()
    void send('/tasks/' + task.id + '/publications', {
      artifact_id: artifact.id, filename: name.trim(), client_request_id: requestKey.current, revision: task.revision,
    }, true)
  }

  const errorNotice = error && <p className="error" role="alert">{error}</p>
  if (publication?.status === 'succeeded') {
    return <div className="publication-receipt"><FileCheck2 size={18}/><div><strong>工作区文件</strong><code>outputs/{publication.filename}</code></div></div>
  }
  if (publication?.status === 'pending') {
    const overwrite = Boolean(publication.previous_sha256)
    const diff = publication.previous_content !== undefined && artifact.content !== undefined
      ? <OverwriteDiff before={publication.previous_content} after={artifact.content}/>
      : null
    const ActionIcon = overwrite ? FilePenLine : FileOutput
    return <section className="publication-card" aria-label="写入确认">
      <div className="publication-heading"><ActionIcon size={19}/><h3>{overwrite?'确认覆盖文件':'确认新建文件'}</h3></div>
      <code>outputs/{publication.filename}</code>
      {overwrite ? (diff ?? <p role="status">正在读取文件差异…</p>)
        : <p>本次写入使用上方的成果版本。</p>}
      <ApprovalExpiry at={publication.expires_at}/>
      {errorNotice}
      <div className="publication-actions">
        <button className="outline" disabled={busy} onClick={() => void send('/publications/' + publication.id + '/decision', { decision:'deny', revision:task.revision })}>拒绝</button>
        <button className="primary" disabled={busy||(overwrite&&!diff)} onClick={() => void send('/publications/' + publication.id + '/decision', { decision:'approve', revision:task.revision })}>{busy ? <LoaderCircle size={16} className="spin"/> : '允许此次写入'}</button>
      </div>
    </section>
  }
  if (publication?.status === 'dispatched' || publication?.status === 'unknown') {
    return <section className="publication-card" aria-label="写入核对">
      <div className="publication-heading"><CircleAlert size={19}/><h3>核对写入结果</h3></div>
      <code>outputs/{publication.filename}</code>
      <p>{publication.detail || '写入结果待核对'}</p>
      {errorNotice}
      <button className="outline" disabled={busy} onClick={() => void send('/publications/' + publication.id + '/reconcile', {})}>核对文件</button>
    </section>
  }
  if(publication?.status==='expired'&&!editing) {
    return <section className="publication-card publication-expired" aria-label="写入确认已过期">
      <div className="publication-heading"><CircleAlert size={19} aria-hidden="true"/><h3>{publication.detail||'写入确认已过期'}</h3></div>
      <code>outputs/{publication.filename}</code>
      {errorNotice}
      <div className="publication-actions"><button className="outline" type="button" disabled={busy} onClick={()=>{
        setName(task.required_filename||publication.filename);requestKey.current='';setError('');setEditing(true)
      }}>重新发起写入</button></div>
    </section>
  }
  return <div className="publication-options">
    {editing ? <form className="publication-form" onSubmit={prepare}>
      <label>保存为<span className="filename-input"><span>outputs/</span><input aria-label="文件名" maxLength={100} required value={name} readOnly={Boolean(task.required_filename)} disabled={busy} onChange={e => {setName(e.target.value); requestKey.current = ''}}/></span></label>
      <div className="publication-actions"><button type="button" className="text-button" disabled={busy} onClick={() => {setEditing(false);setError('')}}>取消</button><button className="outline" disabled={busy || !name.trim()}>检查目标</button></div>
    </form> : <button className="text-button" onClick={() => setEditing(true)}><FileOutput size={17}/>保存到工作区</button>}
    {publication?.status==='conflict' && <p className="error" role="alert">{publication.detail}</p>}
    {errorNotice}
  </div>
}

function OverwriteDiff({before,after}:{before:string;after:string}) {
  if (before===after) return <p>文件内容与当前成果一致。</p>
  const oldLines = before.split('\n')
  const newLines = after.split('\n')
  let start = 0
  let suffix = 0
  const shared = Math.min(oldLines.length,newLines.length)
  while (start<shared && oldLines[start]===newLines[start]) start++
  while (suffix<shared-start && oldLines[oldLines.length-1-suffix]===newLines[newLines.length-1-suffix]) suffix++
  const removed = oldLines.slice(start,oldLines.length-suffix)
  const added = newLines.slice(start,newLines.length-suffix)
  return <div className="publication-diff" role="region" aria-label="文件差异" tabIndex={0}>
    {removed.length>0 && <div><strong>原文件</strong><pre className="diff-removed">{removed.join('\n')||'（空行）'}</pre></div>}
    {added.length>0 && <div><strong>替换为</strong><pre className="diff-added">{added.join('\n')||'（空行）'}</pre></div>}
  </div>
}
