import {clearDrafts,readDraft,writeDraft} from './draftStorage'
import {useCallback,useSyncExternalStore} from 'react'
import type {DeliveryUpdate,Material} from './api'
import type {ReadDraft} from './TaskRead'

export type TaskReference={taskID:string;title:string;planRevision?:number;passive:boolean}
export type IsolatedReplyRequest={mode:'only_this_message';profile_id:string;profile_revision:number;confirm_current_text_egress:true;client_request_id:string;text:string}
type Request = {key:string;scope:string;delivery:DeliveryUpdate;strategy?:'role_routing'|'deterministic_only';awaitingAdmission?:boolean}
type Draft = {
  text:string
  scope?:string
  materials:Material[]
  replaceMaterials?:boolean
  taskReference?:TaskReference
  isolatedRequest?:IsolatedReplyRequest
  criteria?:{text:string;filename:string;baseText:string;baseFilename:string;revision?:number}
  read?:ReadDraft
  reads?:ReadDraft[]
  request?:Request
}

const prefix='clusbot.composer.v1.'
const empty:Draft={text:'',materials:[]}
const drafts=new Map<string,Draft>()
const listeners=new Map<string,Set<()=>void>>()
let generation=0

function read(key:string):Draft {
  const cached=drafts.get(key)
  if(cached)return cached
  let draft=empty
  try {
    const raw=readDraft(prefix+key)
    if(raw) {
      const parsed=JSON.parse(raw) as Draft
      if(typeof parsed.text==='string' && Array.isArray(parsed.materials))draft=parsed
    }
  } catch { /* Storage may be disabled. In-memory drafts still work. */ }
  drafts.set(key,draft)
  return draft
}

function write(key:string,draft:Draft) {
  drafts.set(key,draft)
  try {
    if(draft===empty)writeDraft(prefix+key,null)
    else writeDraft(prefix+key,JSON.stringify(draft))
  } catch { /* Keep the in-memory copy when storage is unavailable. */ }
  listeners.get(key)?.forEach(listener=>listener())
}

export function useComposerDraft(key:string) {
  const subscribe=useCallback((listener:()=>void)=>{
    const subscribers=listeners.get(key)??new Set()
    subscribers.add(listener)
    listeners.set(key,subscribers)
    return ()=>{
      subscribers.delete(listener)
      if(subscribers.size===0)listeners.delete(key)
    }
  },[key])
  const snapshot=useCallback(()=>read(key),[key])
  return useSyncExternalStore(subscribe,snapshot)
}

export function changeComposerDraft(key:string,change:(draft:Draft)=>Partial<Draft>) {
  const draft=read(key)
  write(key,{...draft,...change(draft),request:undefined})
}

export function beginDraftRequest(key:string,scope:string,delivery:DeliveryUpdate,strategy:'role_routing'|'deterministic_only'='role_routing'):Request {
  const draft=read(key)
  if(draft.request)return draft.request
  const request={key:crypto.randomUUID(),scope,delivery,strategy}
  write(key,{...draft,request})
  return request
}

export function finishDraftRequest(key:string,requestID:string) {
  if(read(key).request?.key===requestID)write(key,empty)
}

export function awaitDraftAdmission(key:string,requestID:string) {
  const draft=read(key)
  if(draft.request?.key===requestID)write(key,{...draft,request:{...draft.request,awaitingAdmission:true}})
}

export function releaseDraftRequest(key:string,requestID?:string) {
  const draft=read(key)
  if(draft.request && (requestID===undefined || draft.request.key===requestID))write(key,{...draft,request:undefined})
}

export function discardComposerDraft(key:string) {write(key,empty)}

// Material snapshots can be deduplicated by the server across drafts. Keep one
// draft from deleting a snapshot still selected by another draft in this app.
export function hasOtherDraftMaterial(key:string,id:string) {
  const keys=new Set(drafts.keys())
  try {
    for(const stored of Object.keys(window.clusbotDrafts??sessionStorage))if(stored.startsWith(prefix))keys.add(stored.slice(prefix.length))
  } catch {return true}
  for(const other of keys)if(other!==key&&read(other).materials.some(material=>material.id===id))return true
  return false
}

export function hasPinnedIsolatedRequest(key:string,requestID:string) {
  return read(key).isolatedRequest?.client_request_id===requestID
}

export function draftGeneration() {return generation}

export function clearComposerDrafts() {
  generation++
  drafts.clear()
  try {
    clearDrafts(prefix)
  } catch { /* In-memory drafts are already cleared. */ }
  listeners.forEach(subscribers=>subscribers.forEach(listener=>listener()))
}
