import {accountDraftPrefix,clearDrafts,readDraft,writeDraft} from './draftStorage'
import type {Memory,MemorySource} from './api'

export type MemoryFields=Pick<Memory,'kind'|'title'|'content'|'pinned'>
export type MemoryDraft={fields:MemoryFields;original?:Memory;source?:MemorySource}
const prefix=accountDraftPrefix+'memory.'
const drafts=new Map<string,MemoryDraft>()

export function readMemoryDraft(key:string):MemoryDraft|undefined {
  const cached=drafts.get(key)
  if(cached)return cached
  try {
    const raw=readDraft(prefix+key)
    if(!raw)return
    const value=JSON.parse(raw) as MemoryDraft
    const fields=value.fields
    if(!fields || !['preference','fact'].includes(fields.kind) || typeof fields.title!=='string' || typeof fields.content!=='string' || typeof fields.pinned!=='boolean')return
    if(value.original && (!value.original.id || !Number.isInteger(value.original.revision)))return
    drafts.set(key,value)
    return value
  }catch { /* Keep editable in-memory drafts if storage is unavailable. */ }
}

export function writeMemoryDraft(key:string,value:MemoryDraft) {
  drafts.set(key,value)
  try {writeDraft(prefix+key,JSON.stringify(value))}catch { /* Navigation still retains the in-memory draft. */ }
}

export function discardMemoryDraft(key:string) {
  drafts.delete(key)
  try {writeDraft(prefix+key,null)}catch { /* The in-memory copy is already removed. */ }
}

export function clearMemoryDrafts() {
  drafts.clear()
  try {
    clearDrafts(prefix)
  }catch { /* Nothing remains in memory. */ }
}
