import {useState,type ReactNode} from 'react'
import {ShieldAlert} from 'lucide-react'
import {displayDate} from './GeneralPreferences'
import {createSurfaceAction,resolveSurfaceText,surfaceFieldBytes,surfaceFieldLength,type Surface,type SurfaceAction} from './assistantSurface'

export function AssistantSurfaceView({surface,expanded=false,detailsID,toggle,canToggle=false,confirmation,expires,busy,onAgentAction}:{
  surface:Surface;expanded?:boolean;detailsID?:string;toggle?:()=>void;canToggle?:boolean
  confirmation?:boolean;expires?:number;busy?:boolean;onAgentAction?:(action:SurfaceAction)=>void
}) {
  const [edited,setEdited]=useState<Map<string,string>>(()=>new Map())
  const data=new Map(surface.data)
  for(const [key,value] of edited)if(data.has(key))data.set(key,value)
  const submitNode=[...surface.nodes.values()].find(node=>node.component==='Button'&&node.action.event.name==='request_discovery_admission')
  function dispatch(id:string) {
    const action=createSurfaceAction(surface,id,data)
    if(action&&!busy)onAgentAction?.(action)
  }
  function firstText(id:string):string|undefined {
    const node=surface.nodes.get(id)!
    if(node.component==='Text')return id
    if(node.component==='Button'||node.component==='TextField')return
    for(const child of 'children' in node?node.children:[node.child]) {
      const match=firstText(child)
      if(match)return match
    }
  }
  const headingID=firstText('root')
  function render(id:string):ReactNode {
    const node=surface.nodes.get(id)!
    if(node.component==='Text'){
      const text=resolveSurfaceText(node.text,data)
      return id===headingID?<div key={id} className="assistant-card-title">
        <h3>{confirmation&&<ShieldAlert size={15} aria-hidden="true"/>}{text}</h3>
        {confirmation&&expires!==undefined&&<span className="assistant-expiry">有效至 {displayDate(expires,{hour:'2-digit',minute:'2-digit',second:'2-digit'})}</span>}
      </div>:<p key={id}>{text}</p>
    }
    if(node.component==='TextField'){
      const current=resolveSurfaceText(node.value,data)
      const tooLong=surfaceFieldLength(current.trim())>surfaceFieldBytes
      return <label key={id} className="acct-field">{node.label}
        <input type="text" value={current} maxLength={surfaceFieldBytes} required aria-invalid={tooLong||undefined} disabled={!onAgentAction||busy}
          onChange={event=>{const value=event.currentTarget.value;setEdited(previous=>new Map(previous).set(node.value['@path'],value))}}/>
        {tooLong&&<small role="status">内容过长，请控制在 {surfaceFieldBytes} 字节内。</small>}
      </label>
    }
    if(node.component==='Button') {
      const label=surface.nodes.get(node.child)
      const text=label?.component==='Text'?resolveSurfaceText(label.text,data):''
      if(node.action.event.name==='toggle_details')return <button key={id} type="button" className="text-button assistant-detail-toggle" aria-expanded={expanded} aria-controls={detailsID} disabled={!canToggle} onClick={toggle}>
        {expanded?'收起详情':text}
      </button>
      const submitting=node.id===submitNode?.id
      return <button key={id} type={submitting?'submit':'button'} className={node.variant==='primary'?'primary':node.variant==='default'?'outline':'text-button'}
        disabled={!onAgentAction||busy||(submitting&&!createSurfaceAction(surface,id,data))}
        onClick={submitting?undefined:()=>dispatch(id)}>{text}</button>
    }
    if(node.component==='Card')return <div key={id} className="assistant-surface-card">{render(node.child)}</div>
    return <div key={id} className={node.component==='Row'?'assistant-surface-row':'assistant-surface-column'}>{node.children.map(render)}</div>
  }
  return submitNode?<form onSubmit={event=>{event.preventDefault();dispatch(submitNode.id)}} aria-busy={busy}>{render('root')}</form>:<>{render('root')}</>
}
