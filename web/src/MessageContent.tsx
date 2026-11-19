import {File,FileText,Image,X} from 'lucide-react'
import type {ReactNode} from 'react'
import type {Material} from './api'

export function MaterialTypeIcon({material,size=16}:{material:Material;size?:number}) {
  // These namespaces are assigned by the server after capture or byte validation.
  const Icon=material.path.startsWith('images/')?Image:
    material.upload||material.path.startsWith('materials/')?FileText:File
  return <Icon size={size} aria-hidden="true"/>
}

export function MessageContent({reference,body,attachments}:{reference?:ReactNode;body?:ReactNode;attachments?:ReactNode}) {
  return <div className="message-content">{reference}{body&&<div className="message-content-body">{body}</div>}{attachments}</div>
}

export function MessageReference({label,onOpen,onRemove}:{
  label:string;onOpen?:(button:HTMLButtonElement)=>void;onRemove?:()=>void
}) {
  return <div className="message-reference">
    {onOpen?<button type="button" className="message-reference-label" title={label} aria-haspopup="dialog" onClick={e=>onOpen(e.currentTarget)}>{label}</button>
      :<span className="message-reference-label" title={label}>{label}</span>}
    {onRemove&&<button type="button" className="message-content-action" aria-label="移除任务引用" onClick={onRemove}><X size={14} aria-hidden="true"/></button>}
  </div>
}

type DraftActions={disabled:boolean;remove:(id:string)=>void;choosePart:(id:string,part:number)=>void}
export function MessageAttachments({items,draft}:{items:Material[];draft?:DraftActions}) {
  if(!items.length)return null
  const card=(item:Material)=>{
    const filename=item.upload?.name??item.path.split('/').pop()??item.path
    const upload=item.upload
    return <li className="message-attachment" key={upload?.id??item.id} title={filename+' · '+item.sha256}>
      <MaterialTypeIcon material={item} size={20}/>
      <div className="message-attachment-copy">
        <strong>{filename}</strong>
        {upload&&upload.parts>1 ? draft
          ? <select aria-label={'选择 '+filename+' 的片段'} disabled={draft.disabled} value={upload.part}
            onChange={event=>draft.choosePart(upload.id,Number(event.currentTarget.value))}>
            {Array.from({length:upload.parts},(_,index)=><option key={index} value={index+1}>第 {index+1} / {upload.parts} 段</option>)}
          </select>
          : <small>第 {upload.part} / {upload.parts} 段</small>
          : <small>{Math.max(1,Math.ceil(item.size/1024))} KB</small>}
      </div>
      {draft&&<button type="button" className="message-content-action" aria-label={'移除 '+filename} disabled={draft.disabled} onClick={()=>draft.remove(item.id)}>
        <X size={15} aria-hidden="true"/>
      </button>}
    </li>
  }
  return <section className="message-attachments" aria-label="附件">
    <ul className="message-attachment-grid">{items.slice(0,3).map(card)}</ul>
    {items.length>3&&<details><summary>另有 {items.length-3} 个附件</summary>
      <ul className="message-attachment-grid">{items.slice(3).map(card)}</ul></details>}
  </section>
}
