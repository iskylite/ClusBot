import {Download,File,FileText,Image,X} from 'lucide-react'
import {useEffect,useId,useRef,useState,type ReactNode} from 'react'
import {createPortal} from 'react-dom'
import Markdown from 'react-markdown'
import {api,type Material} from './api'
import {markdownComponents,markdownPlugins} from './ExecutionText'

export function MaterialTypeIcon({material,size=16}:{material:Material;size?:number}) {
  const Icon=material.path.startsWith('images/')?Image:
    material.upload||material.path.startsWith('materials/')?FileText:File
  return <Icon size={size} aria-hidden="true"/>
}

export function MessageContent({reference,body,attachments,command}:{reference?:ReactNode;body?:ReactNode;attachments?:ReactNode;command?:ReactNode}) {
  return <div className="message-content">{(reference||attachments||command)&&<div className="message-context">{reference}{attachments}{command}</div>}{body&&<div className="message-content-body">{body}</div>}</div>
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

type Preview={name:string;preview_kind:string;bound_to_message:boolean;total_lines?:number;truncated?:boolean;columns?:string[];rows?:string[][];lines?:string[];image_data_url?:string}
const materialName=(item:Material)=>item.upload?.name??item.path.split('/').pop()??item.path

function MaterialPreview({item,items,floating,onSelect,onClose}:{item:Material;items:Material[];floating:boolean;onSelect:(item:Material)=>void;onClose:()=>void}) {
  const dialog=useRef<HTMLDialogElement>(null)
  const title=useId()
  const [preview,setPreview]=useState<Preview>()
  const [error,setError]=useState('')
  const [loading,setLoading]=useState(true)
  useEffect(()=>{
    const element=dialog.current
    if(element&&!floating&&!element.open)element.showModal()
    element?.querySelector<HTMLButtonElement>('.material-preview-close')?.focus({preventScroll:true})
    return()=>{if(!floating&&element?.open)element.close()}
  },[floating])
  useEffect(()=>{
    let active=true, request=0
    async function load(){
      const token=++request
      setPreview(undefined);setLoading(true)
      try {
        const result=await api<Preview>('/materials/'+encodeURIComponent(item.id)+'/preview')
        if(active&&token===request){setPreview(result);setError('')}
      } catch(e){
        if(active&&token===request){setPreview(undefined);setError(e instanceof Error?e.message:'材料预览不可用')}
      } finally{if(active&&token===request)setLoading(false)}
    }
    void load()
    const revalidate=()=>{if(document.visibilityState==='visible')void load();else{request++;setPreview(undefined);setLoading(true)}}
    const offline=()=>{request++;setPreview(undefined);setLoading(false);setError('网络不可用，请联网后重新打开预览')}
    window.addEventListener('focus',revalidate)
    window.addEventListener('offline',offline)
    document.addEventListener('visibilitychange',revalidate)
    return()=>{active=false;request++;window.removeEventListener('focus',revalidate);window.removeEventListener('offline',offline);document.removeEventListener('visibilitychange',revalidate)}
  },[item.id])
  const image=preview?.image_data_url&&/^data:image\/(?:png|jpeg);base64,[a-z0-9+/=]+$/i.test(preview.image_data_url)?preview.image_data_url:undefined
  const visibleRows=preview?.preview_kind==='csv'?preview.rows?.length:preview?.lines?.length
  const totalRows=preview?.total_lines===undefined?undefined:preview.preview_kind==='csv'?Math.max(0,preview.total_lines-1):preview.total_lines
  const content=<dialog ref={dialog} className={'material-preview'+(floating?' material-preview-floating':'')} open={floating||undefined} aria-labelledby={title}
    onClose={onClose} onKeyDown={event=>{if(floating&&event.key==='Escape'){event.preventDefault();onClose()}}}>
    <header><div className="material-preview-title"><MaterialTypeIcon material={item} size={16}/><h2 id={title}>{preview?.name??materialName(item)}</h2>
      {preview&&<small>{preview.bound_to_message?'历史材料':'待发送'}{visibleRows&&totalRows?' · 第 1–'+visibleRows+' / '+totalRows+' 行':''}</small>}</div>
      <div className="material-preview-actions">
        {items.length>1&&<select aria-label="切换预览材料" value="" onChange={event=>{const next=items.find(candidate=>candidate.id===event.currentTarget.value);if(next)onSelect(next)}}>
          <option value="" disabled>切换：{materialName(items.find(candidate=>candidate.id!==item.id)??item)}</option>
          {items.filter(candidate=>candidate.id!==item.id).map(candidate=><option key={candidate.id} value={candidate.id}>{materialName(candidate)}</option>)}
        </select>}
        <a className="icon-button" aria-label="下载材料" href={'/api/v1/materials/'+encodeURIComponent(item.id)+'/download'} download={materialName(item)}><Download size={15}/></a>
        <button type="button" className="icon-button material-preview-close" aria-label="关闭预览" onClick={()=>floating?onClose():dialog.current?.close()}><X size={16}/></button>
      </div></header>
    {loading?<p role="status">正在读取预览…</p>:error?<p className="error" role="alert">{error}</p>:preview&&<div className="material-preview-body">
      {preview.preview_kind==='image'?(image?<img src={image} alt={preview.name}/>:<p>图片预览不可用</p>):preview.preview_kind==='csv'&&preview.columns?
        <div className="material-preview-table"><table><thead><tr>{preview.columns.map((column,index)=><th scope="col" key={index}>{column}</th>)}</tr></thead><tbody>{preview.rows?.map((row,index)=><tr key={index}>{row.map((cell,i)=><td key={i}>{cell}</td>)}</tr>)}</tbody></table></div>:
        preview.preview_kind==='markdown'?<div className="formatted"><Markdown skipHtml disallowedElements={['img','a']} unwrapDisallowed remarkPlugins={markdownPlugins} components={markdownComponents}>{(preview.lines??[]).join('\n')}</Markdown></div>:
        <pre>{(preview.lines??[]).join('\n')}</pre>}
    </div>}
  </dialog>
  return floating?createPortal(content,document.body):content
}

type DraftActions={disabled:boolean;remove:(id:string)=>void;choosePart:(id:string,part:number)=>void}
export function MessageAttachments({items,previewItems,draft}:{items:Material[];previewItems?:Material[];draft?:DraftActions}) {
  const [selectedID,setSelectedID]=useState<string>()
  const [floating,setFloating]=useState(false)
  const trigger=useRef<HTMLButtonElement>(null)
  const seen=new Set<string>()
  const choices=[...items,...(previewItems??[])].filter(item=>{
    if(seen.has(item.id))return false
    seen.add(item.id)
    return true
  })
  const selected=choices.find(item=>item.id===selectedID)
  function closePreview(){
    setSelectedID(undefined)
    trigger.current?.focus({preventScroll:true})
  }
  if(!items.length)return null
  return <section className="message-attachments" aria-label="附件" data-draft={Boolean(draft)||undefined}>
    <ul className="message-attachment-grid">{items.map(item=>{
      const filename=materialName(item)
      const upload=item.upload
      return <li className="message-attachment" data-previewing={selected?.id===item.id||undefined} key={upload?.id??item.id}>
        <button type="button" className="message-attachment-open" aria-label={'预览 '+filename} title={'预览 '+filename} aria-haspopup="dialog" onClick={event=>{
          trigger.current=event.currentTarget
          setFloating(Boolean(event.currentTarget.closest('[data-section="chat"]')&&!event.currentTarget.closest('dialog')))
          setSelectedID(item.id)
        }}>
          <MaterialTypeIcon material={item}/><strong>{filename}</strong><small> · {Math.max(1,Math.ceil(item.size/1024))} KB{draft&&(selected?.id===item.id?' · 预览中':' · 点击预览')}</small>
        </button>
        {upload&&upload.parts>1&&(draft?<select aria-label={'选择 '+filename+' 的片段'} disabled={draft.disabled} value={upload.part}
          onChange={event=>draft.choosePart(upload.id,Number(event.currentTarget.value))}>
          {Array.from({length:upload.parts},(_,index)=><option key={index} value={index+1}>第 {index+1} / {upload.parts} 段</option>)}
        </select>:<small>第 {upload.part} / {upload.parts} 段</small>)}
        {draft&&<button type="button" className="message-content-action" aria-label={'移除 '+filename} disabled={draft.disabled} onClick={()=>draft.remove(item.id)}><X size={15} aria-hidden="true"/></button>}
      </li>
    })}</ul>
    {selected&&<MaterialPreview item={selected} items={choices} floating={floating} onSelect={item=>setSelectedID(item.id)} onClose={closePreview}/>}
  </section>
}
