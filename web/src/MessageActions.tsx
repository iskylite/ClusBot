import {useState,type RefObject} from 'react'
import {Copy,Quote} from 'lucide-react'
import './MessageActions.css'

export type QuoteMessage=(id:number,text:string)=>string|void

function selectedBodyText(body:HTMLElement|null) {
  const selection=window.getSelection()
  if(!body||!selection||selection.rangeCount!==1||selection.isCollapsed)return ''
  const range=selection.getRangeAt(0)
  if(!body.contains(range.startContainer)||!body.contains(range.endContainer))return ''
  const text=range.toString()
  return text.trim()?text:''
}

export function MessageActions({body,text,quote}:{
  body:RefObject<HTMLDivElement|null>;text:string;quote?:(text:string)=>string|void
}) {
  const [status,setStatus]=useState('')
  // Keep an active body selection in place when the user clicks an action.
  const keepSelection=(event:React.MouseEvent)=>{
    if(event.button===0&&selectedBodyText(body.current))event.preventDefault()
  }
  async function copy(){
    const value=selectedBodyText(body.current)||text
    try {
      if(!navigator.clipboard)throw new Error('Clipboard unavailable')
      await navigator.clipboard.writeText(value)
      setStatus('已复制')
    } catch {setStatus('复制失败，请手动选择正文复制。')}
  }
  return <section className="chat-message-actions" aria-label="消息操作">
    {quote&&<button type="button" className="chat-message-action" onMouseDown={keepSelection}
      onClick={()=>setStatus(quote(selectedBodyText(body.current)||text)||'已放入草稿')}><Quote size={14} aria-hidden="true"/>引用回复</button>}
    <button type="button" className="chat-message-action" onMouseDown={keepSelection} onClick={()=>void copy()}><Copy size={14} aria-hidden="true"/>复制</button>
    <span className="chat-message-action-status" role="status">{status}</span>
  </section>
}

// Only append visible Markdown text; no context or task reference is inferred.
// The source chip is a local navigation aid, not an API message reference.
export type DraftQuote={messageID:number;inserted:string}

export function insertDraftQuote(draft:string,messageID:number,text:string) {
  const quote=text.replaceAll('\r\n','\n').replaceAll('\r','\n').split('\n').map(line=>'> '+line).join('\n')
  const inserted=(draft?'\n\n':'')+quote+'\n\n'
  return {text:draft+inserted,source:{messageID,inserted}}
}

export function removeDraftQuote(draft:string,source:DraftQuote) {
  // Only remove an intact insertion. If it has been edited or there is
  // ambiguity, removing its chip must leave the user's draft untouched.
  const start=draft.indexOf(source.inserted)
  if(start<0||draft.lastIndexOf(source.inserted)!==start)return draft
  return draft.slice(0,start)+draft.slice(start+source.inserted.length)
}
