import {accountDraftPrefix,clearDrafts,readDraft,writeDraft} from './draftStorage'
import {useCallback,useSyncExternalStore} from 'react'
import type {DeliveryUpdate,Material} from './api'
import type {ReadDraft} from './TaskRead'

export type TaskReference={taskID:string;title:string;planRevision?:number;passive:boolean;purpose?:'revision'}
export type IsolatedReplyRequest={mode:'only_this_message';profile_id:string;profile_revision:number;confirm_current_text_egress:true;client_request_id:string;text:string}
export type PendingAdmission={key:string;text:string;createdAt:number}
type Request = {key:string;scope:string;delivery:DeliveryUpdate;strategy?:'role_routing'|'deterministic_only'}
type Draft = {
  text:string
  benchmarkPlan?:string
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

const prefix=accountDraftPrefix+'composer.'
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

export function restoreRevisionDraft(key:string,text:string,reference:TaskReference) {
  const draft=read(key)
  if(draft.request||(draft.text.trim()&&draft.text!==text))return false
  write(key,{...draft,text,taskReference:reference})
  return true
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

const admissionPrefix=accountDraftPrefix+'admission.'
const emptyAdmissions:PendingAdmission[]=[]
const pendingAdmissions=new Map<string,PendingAdmission[]>()
const admissionListeners=new Map<string,Set<()=>void>>()

function readAdmissions(key:string):PendingAdmission[] {
  const cached=pendingAdmissions.get(key)
  if(cached)return cached
  let list=emptyAdmissions
  try {
    const raw=readDraft(admissionPrefix+key)
    if(raw) {
      const parsed=JSON.parse(raw)
      if(Array.isArray(parsed))list=parsed.filter((item):item is PendingAdmission=>Boolean(item&&typeof item.key==='string'&&typeof item.text==='string'&&typeof item.createdAt==='number'))
    }
  } catch { /* Storage may be disabled. In-memory admissions still work. */ }
  pendingAdmissions.set(key,list)
  return list
}

function writeAdmissions(key:string,list:PendingAdmission[]) {
  const next=list.length===0?emptyAdmissions:list
  pendingAdmissions.set(key,next)
  try {
    if(next.length===0)writeDraft(admissionPrefix+key,null)
    else writeDraft(admissionPrefix+key,JSON.stringify(next))
  } catch { /* Keep the in-memory copy when storage is unavailable. */ }
  admissionListeners.get(key)?.forEach(listener=>listener())
}

export function usePendingAdmissions(key:string) {
  const subscribe=useCallback((listener:()=>void)=>{
    const subscribers=admissionListeners.get(key)??new Set()
    subscribers.add(listener)
    admissionListeners.set(key,subscribers)
    return ()=>{
      subscribers.delete(listener)
      if(subscribers.size===0)admissionListeners.delete(key)
    }
  },[key])
  const snapshot=useCallback(()=>readAdmissions(key),[key])
  return useSyncExternalStore(subscribe,snapshot)
}

export function enqueuePendingAdmission(key:string,admission:PendingAdmission) {
  const current=readAdmissions(key)
  if(current.some(item=>item.key===admission.key))return
  writeAdmissions(key,[...current,admission])
}

export function removePendingAdmission(key:string,requestID:string) {
  const current=readAdmissions(key)
  if(!current.some(item=>item.key===requestID))return
  writeAdmissions(key,current.filter(item=>item.key!==requestID))
}

export function clearComposerDrafts() {
  generation++
  drafts.clear()
  pendingAdmissions.clear()
  try {
    clearDrafts(prefix)
    clearDrafts(admissionPrefix)
  } catch { /* In-memory drafts are already cleared. */ }
  listeners.forEach(subscribers=>subscribers.forEach(listener=>listener()))
  admissionListeners.forEach(subscribers=>subscribers.forEach(listener=>listener()))
}
