import {accountDraftKey,accountDraftPrefix,readDraft,writeDraft} from './draftStorage'
import {a2uiPath,isDiscoveryConfirmationRef,type DiscoveryConfirmationRef} from './assistantSurface'

const limit=12
function storageKey(principalID:string,conversationID:string) {
  return accountDraftPrefix+'discovery-confirmation.'+accountDraftKey(principalID,conversationID)
}
export function pendingDiscoveryConfirmations(principalID:string,conversationID:string):DiscoveryConfirmationRef[] {
  if(!principalID||!a2uiPath(conversationID,'actions'))return []
  try{
    const raw=readDraft(storageKey(principalID,conversationID))
    const items=raw&&JSON.parse(raw) as unknown
    if(!Array.isArray(items)||items.length>limit||!items.every(isDiscoveryConfirmationRef)||
      new Set(items.map(item=>item.key)).size!==items.length)return []
    return items
  }catch{return []}
}
export function rememberDiscoveryConfirmation(principalID:string,conversationID:string,pointer:DiscoveryConfirmationRef) {
  if(!principalID||!a2uiPath(conversationID,'actions')||!isDiscoveryConfirmationRef(pointer))throw new Error('无法安全记录原纳管请求。未执行纳管。')
  const items=pendingDiscoveryConfirmations(principalID,conversationID)
  const prior=items.find(item=>item.key===pointer.key)
  if(prior){
    if(prior.candidateID!==pointer.candidateID||prior.revision!==pointer.revision)throw new Error('发现不匹配的原纳管请求。未执行纳管。')
    return
  }
  if(items.length>=limit)throw new Error('请先核对原有的待查纳管结果。未执行本次纳管。')
  try{
    writeDraft(storageKey(principalID,conversationID),JSON.stringify([...items,pointer]))
  }catch{throw new Error('无法保存纳管回执的核对标识。未执行纳管。')}
}
export function forgetDiscoveryConfirmation(principalID:string,conversationID:string,pointer:DiscoveryConfirmationRef) {
  const items=pendingDiscoveryConfirmations(principalID,conversationID)
  const remaining=items.filter(item=>item.key!==pointer.key||item.candidateID!==pointer.candidateID||item.revision!==pointer.revision)
  if(remaining.length===items.length)return
  try{writeDraft(storageKey(principalID,conversationID),remaining.length?JSON.stringify(remaining):null)}
  catch{/* Verified terminal results can safely be looked up again. */}
}
