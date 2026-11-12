declare global {
  interface Window {
    clusbotDrafts?: Record<string,string>
    webkit?: {messageHandlers?: {drafts?: {postMessage(value:{key:string;value:string|null}):void}}}
  }
}

export function readDraft(key:string) {
  return window.clusbotDrafts ? window.clusbotDrafts[key] : sessionStorage.getItem(key)
}

export function writeDraft(key:string,value:string|null) {
  const drafts=window.clusbotDrafts
  if(drafts) {
    window.webkit?.messageHandlers?.drafts?.postMessage({key,value})
    if(value===null)delete drafts[key]
    else drafts[key]=value
  } else if(value===null)sessionStorage.removeItem(key)
  else sessionStorage.setItem(key,value)
}

export function clearDrafts(prefix:string) {
  const keys=Object.keys(window.clusbotDrafts??sessionStorage)
  for(const key of keys)if(key.startsWith(prefix))writeDraft(key,null)
}
