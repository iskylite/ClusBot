import { useEffect, useRef, useState } from 'react'
import { FileText, LoaderCircle, Paperclip, Search, Upload } from 'lucide-react'
import { api, type MaterialFile } from './api'

export function MaterialPicker({limitReached,disabled,choose,upload,replacement,allowImages}:{
  allowImages:boolean; limitReached:boolean; disabled:boolean; choose:(path:string)=>Promise<void>; upload:(file:File)=>Promise<void>;
  replacement?:{enabled:boolean;change:(enabled:boolean)=>void}
}) {
  const input=useRef<HTMLInputElement>(null)
  const [open,setOpen]=useState(false)
  const [query,setQuery]=useState('')
  const [files,setFiles]=useState<MaterialFile[]>([])
  const [loading,setLoading]=useState(false)
  const [error,setError]=useState('')
  useEffect(()=>{
    if(!open)return
    let cancelled=false
    setLoading(true)
    setError('')
    const debounce=window.setTimeout(()=>{
      api<MaterialFile[]>('/workspace/files?'+new URLSearchParams({q:query}))
        .then(result=>{if(!cancelled)setFiles(result)})
        .catch(e=>{if(!cancelled)setError(e instanceof Error?e.message:'无法读取材料列表')})
        .finally(()=>{if(!cancelled)setLoading(false)})
    },query?150:0)
    return()=>{cancelled=true;clearTimeout(debounce)}
  },[open,query])

  return <div className="material-picker">
    <button type="button" className={'text-button'+(replacement?.enabled?' replacing':'')} aria-expanded={open}
      disabled={disabled||(limitReached&&!replacement)} onClick={()=>{setOpen(!open);setQuery('')}}>
      <Paperclip size={16}/>{replacement?.enabled?'仅用本轮材料':'选取材料'}</button>
    {open && <div className="material-menu">
      <input type="file" ref={input} hidden aria-label="上传文件" accept={allowImages ? ".md,.txt,image/png,image/jpeg" : ".md,.txt"} onChange={async event=>{
        const file=event.target.files?.[0]
        event.target.value=''
        if(file){await upload(file);setOpen(false)}
      }}/>
      <div className="material-menu-actions">
        <button type="button" className="text-button upload-material" disabled={disabled||limitReached}
          onClick={()=>input.current?.click()}><Upload size={16}/>上传本地文件</button>
        {replacement && <label className="material-replacement">
          <input type="checkbox" checked={replacement.enabled} disabled={disabled} onChange={event=>replacement.change(event.target.checked)}/>
          替换此前材料
        </label>}
      </div>
      <label className="material-search"><Search size={16}/><input autoFocus={!replacement} aria-label="搜索材料" placeholder="搜索文件名" value={query} onChange={e=>setQuery(e.target.value)}/></label>
      {loading ? <LoaderCircle size={17} className="spin" aria-label="正在读取材料"/> : error ? <p className="error" role="alert">{error}</p> :
        files.length===0 ? <p className="muted">{query?'没有匹配的材料':'将文本或 Markdown 文件放入工作区的 materials 目录。'}</p> :
        <ul>{files.map(file=><li key={file.path}>
          <button type="button" disabled={disabled||limitReached} onClick={async()=>{await choose(file.path);setOpen(false)}}>
            <FileText size={16}/><span>{file.path.slice('materials/'.length)}</span><small>{Math.max(1,Math.ceil(file.size/1024))} KB</small>
          </button>
        </li>)}</ul>}
    </div>}
  </div>
}
