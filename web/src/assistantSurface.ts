// A bounded A2UI 1.0 Basic Catalog subset. A surface can describe a permitted
// event, but cannot choose transport, call a business API, or confirm a write.
const catalog = 'https://a2ui.org/specification/v1_0/catalogs/basic/catalog.json'
const idPattern = /^[A-Za-z0-9_-]{1,80}$/

type Binding = {'@path':string}
type Scalar = string|number|boolean|null
type EventValue = Scalar|Binding
type SurfaceData = Map<string,Scalar>
type AgentEvent = {name:string;context?:Record<string,EventValue>}
type ContainerNode = {id:string;component:'Column'|'Row';children:string[]}
type TextNode = {id:string;component:'Text';text:string|Binding}
type CardNode = {id:string;component:'Card';child:string}
type TextFieldNode = {id:string;component:'TextField';label:string;value:Binding}
type ButtonNode = {id:string;component:'Button';child:string;variant:'borderless'|'default'|'primary';action:{event:AgentEvent}}
export type SurfaceNode = ContainerNode|TextNode|CardNode|ButtonNode|TextFieldNode
export type Surface = {id:string;nodes:Map<string,SurfaceNode>;data:SurfaceData}
export type SurfaceAction = {
  version:'1.0'
  action:{name:string;surfaceId:string;sourceComponentId:string;timestamp:string;context:Record<string,Scalar>}
}
export type DiscoveryConversationSeed={conversationID:string;runID:string;surfaces:Surface[]}

const object = (value:unknown):value is Record<string,unknown> => typeof value==='object' && value!==null && !Array.isArray(value)
const keysAre = (value:Record<string,unknown>,keys:string[]) => Object.keys(value).length===keys.length && keys.every(key=>Object.hasOwn(value,key))
const allowedKeys = (value:Record<string,unknown>,required:string[],optional:string[]) => required.every(key=>Object.hasOwn(value,key)) && Object.keys(value).every(key=>required.includes(key)||optional.includes(key))
const validID = (value:unknown):value is string => typeof value==='string' && idPattern.test(value)
const safeKey = (key:string) => /^[a-zA-Z][a-zA-Z0-9_-]{0,63}$/.test(key) && !['constructor','prototype'].includes(key) &&
  !/(?:password|passwd|secret|credential|token|api[_-]?key|authorization)/i.test(key)
const validPath = (path:unknown):path is string => typeof path==='string' && path.startsWith('/') &&
  path.length<=200 && path.slice(1).split('/').every(safeKey)
const binding = (value:unknown):value is Binding => object(value) && keysAre(value,['@path']) && validPath(value['@path'])
const scalar = (value:unknown):value is Scalar => value===null || typeof value==='boolean' ||
  (typeof value==='number' && Number.isFinite(value)) || (typeof value==='string' && value.length<=4000)

// The product catalog specifies which fields each event may send. Do not
// forward userMessage, whole data models, functions or arbitrary destinations.
const reviewFields=['owner','location','role'] as const
export const benchmarkAdmissionAction='preview_benchmark_admission'
const benchmarkRevisionAction='preview_benchmark_revision'
export const benchmarkSyncActualAction='preview_benchmark_parameter_with_actual'
const revisionFields=['source_run_id','task_id','revision'] as const
const eventFields:Readonly<Record<string,readonly string[]>> = Object.freeze({
  query_task_progress:['task_id','revision'],
  query_asset_ledger:['asset_slot','revision'],
  query_attention_reason:['attention_slot','revision'],
  preview_pending_approval:['attention_slot','revision'],
  preview_conversation_skill:['skill_slot','revision'],
  preview_artifact_text:['artifact_slot','revision'],
  review_memory_provenance:['memory_slot','revision'],
  preview_benchmark_comparison:['report_slot','revision'],
  preview_discovery_schedule:['schedule_slot','revision'],
  review_operation_advice_choice:['advice_slot','revision'],
  choose_mcp_transport:['transport','revision'],
  request_mcp_setup_step:['step','revision','connection_revision'],
  review_discovery_candidate:['candidate_id','revision'],
  [benchmarkAdmissionAction]:['source_run_id','revision'],
  [benchmarkRevisionAction]:revisionFields,
  [benchmarkSyncActualAction]:revisionFields,
  request_discovery_admission:['candidate_id','revision',...reviewFields],
})
const editablePaths=new Set<string>(reviewFields.map(key=>'/form/'+key))
const validRevision=(value:unknown):value is number => typeof value==='number'&&Number.isSafeInteger(value)&&value>0
const numberedSlot=(key:string)=>key==='asset_slot'||key==='attention_slot'||key==='artifact_slot'||key==='advice_slot'||key==='skill_slot'||key==='memory_slot'||key==='report_slot'||key==='schedule_slot'
const validReadSlot=(value:unknown,key:string):value is string => typeof value==='string'&&
  ((key==='report_slot'||key==='schedule_slot')?value==='1':(key==='memory_slot'?/^(?:[1-9]|1[0-6])$/:/^(?:[1-9]|10)$/).test(value))
const validCandidateID=(value:unknown):value is string => typeof value==='string'&&/^[a-f\d]{64}$/.test(value)
export const surfaceFieldBytes=256
export const surfaceFieldLength=(value:string)=>new TextEncoder().encode(value).length
const validField=(value:unknown):value is string => typeof value==='string'&&!!value.trim()&&surfaceFieldLength(value.trim())<=surfaceFieldBytes&&
  !/[\x00-\x1F\x7F]/.test(value)

function parseEvent(value:unknown):AgentEvent|undefined {
  if(!object(value)||!allowedKeys(value,['name'],['context'])||typeof value.name!=='string')return
  if(value.name==='toggle_details')return keysAre(value,['name'])?{name:'toggle_details'}:undefined
  if(!Object.hasOwn(eventFields,value.name)||!object(value.context)||!keysAre(value.context,[...eventFields[value.name]]))return
  const context:Record<string,EventValue>=Object.create(null)
  for(const [key,input] of Object.entries(value.context)) {
    if(numberedSlot(key)) {
      if(!validReadSlot(input,key))return
    } else if(key==='candidate_id') {
      if(!validCandidateID(input))return
    } else if(key==='step') {
      if(input!=='probe'&&input!=='permissions'&&input!=='binding')return
    } else if(key==='transport') {
      if(input!=='stdio'&&input!=='http')return
    } else if(key==='task_id'||key==='source_run_id') {
      if(typeof input!=='string'||!/^[a-f\d]{32}$/.test(input))return
    } else if(key==='revision'||key==='connection_revision') {
      if(!validRevision(input))return
    } else if(!binding(input)||input['@path']!=='/form/'+key)return
    context[key]=input as EventValue
  }
  return {name:value.name,context}
}

export function resolveSurfaceText(input:string|Binding,data:SurfaceData) {
  if(typeof input==='string')return input
  const value=data.get(input['@path'])
  return typeof value==='string'?value:''
}

export function createSurfaceAction(surface:Surface,sourceComponentID:string,data:SurfaceData,now=new Date()):SurfaceAction|undefined {
  const source=surface.nodes.get(sourceComponentID)
  if(source?.component!=='Button'||!Number.isFinite(now.getTime()))return
  const {name,context}=source.action.event
  if(!context||!Object.hasOwn(eventFields,name))return
  // Revalidate after resolving the latest editable values. The server
  // independently owns the action, candidate and presentation revision.
  const resolved:Record<string,Scalar>={}
  for(const key of eventFields[name]) {
    const field=context[key]
    const value=binding(field)?data.get(field['@path']):field
    if(key==='revision'||key==='connection_revision') {
      if(!validRevision(value))return
      resolved[key]=value
    } else if(numberedSlot(key)) {
      if(!validReadSlot(value,key))return
      resolved[key]=value
    } else {
      if(!validField(value))return
      resolved[key]=value.trim()
    }
  }
  return {version:'1.0',action:{name,surfaceId:surface.id,sourceComponentId:source.id,timestamp:now.toISOString(),context:resolved}}
}

export type AssistantTool = {name:string;description:string;mode:'read'|'confirm'|'form';params?:string[];label?:string}
export type RoleBindingPrefill={role?:string;profile_id?:string;profile_revision?:number;provider_revision?:number}
export type ProviderCredentialsPrefill={provider_id?:string;provider_revision?:number}
export type AssistantReply = {state:'completed';tool:string;result:unknown;surface?:unknown;outcome?:string} |
  {state:'confirmation_required';confirmation_id:string;expires_at_unix_ms:number;input:Record<string,unknown>;target?:unknown;surface?:unknown} |
  {state:'form_required';tool:string;form:string;prefill?:RoleBindingPrefill|ProviderCredentialsPrefill}
type AssistantFailure = {state:'failed';tool:string;message:string}
export type AssistantCardItem = AssistantReply|AssistantTool[]
export type AssistantItem = AssistantCardItem|{state:'batch_completed';items:(AssistantReply|AssistantFailure)[]}

function isAssistantReply(value:unknown):value is AssistantReply {
  if(!object(value))return false
  if(value.state==='completed')return typeof value.tool==='string' && value.tool.length>0 && Object.hasOwn(value,'result')
  if(value.state==='form_required'){
    if(value.tool==='role_binding_prefill'||value.tool==='provider_credentials_prefill'){
      if(!object(value.prefill))return false
      const p=value.prefill
      const revision=(v:unknown)=>typeof v==='number'&&Number.isSafeInteger(v)&&v>0
      const recordID=(v:unknown)=>typeof v==='string'&&/^[a-f\d]{32}$/.test(v)
      if(value.tool==='role_binding_prefill'){
        return value.form==='model_role_binding' && Object.keys(p).every(key=>['role','profile_id','profile_revision','provider_revision'].includes(key)) &&
          (p.role===undefined||typeof p.role==='string'&&['general_router','conversation_reply','task_worker','benchmark_admission','benchmark_revision','benchmark_execution'].includes(p.role)) &&
          (p.profile_id===undefined?p.profile_revision===undefined&&p.provider_revision===undefined:recordID(p.profile_id)&&revision(p.profile_revision)&&revision(p.provider_revision))
      }
      return value.form==='model_provider_credentials' && Object.keys(p).every(key=>['provider_id','provider_revision'].includes(key)) &&
        (p.provider_id===undefined?p.provider_revision===undefined:recordID(p.provider_id)&&revision(p.provider_revision))
    }
    return typeof value.tool==='string' && typeof value.form==='string'
  }
  return value.state==='confirmation_required' && typeof value.confirmation_id==='string' && /^[a-f\d]{48}$/.test(value.confirmation_id) && typeof value.expires_at_unix_ms==='number' && Number.isSafeInteger(value.expires_at_unix_ms) && value.expires_at_unix_ms>=0 && value.expires_at_unix_ms<=8640000000000000 && object(value.input) && typeof value.input.tool==='string'
}

// Only a recognizable receipt may consume the composer's draft.
export function isAssistantItem(value:unknown):value is AssistantItem {
  if(Array.isArray(value))return value.length<=200 && value.every(item=>object(item) && typeof item.name==='string' && /^[a-z0-9_]{1,80}$/.test(item.name) && typeof item.description==='string' && ['read','confirm','form'].includes(String(item.mode)) && (item.label===undefined||typeof item.label==='string') && (item.params===undefined||(Array.isArray(item.params)&&item.params.every((p:unknown)=>typeof p==='string'))))
  if(object(value)&&value.state==='batch_completed'){
    return Array.isArray(value.items)&&value.items.length>=2&&value.items.length<=6&&value.items.every(item=>
      isAssistantReply(item)||(object(item)&&item.state==='failed'&&typeof item.tool==='string'&&typeof item.message==='string'&&item.message.length>0&&item.message.length<=1000))
  }
  return isAssistantReply(value)
}

export function assistantReplyCards(item:AssistantItem):(AssistantCardItem|AssistantFailure)[] {
  return !Array.isArray(item)&&item.state==='batch_completed'?item.items:[item]
}

export function a2uiPath(conversationID:string|undefined,kind:'actions'|'discovery-candidates') {
  return typeof conversationID==='string'&&/^[a-f\d]{32}$/.test(conversationID)?'/conversations/'+conversationID+'/a2ui/'+kind:undefined
}

export function parseSurfaceReceipt(value:unknown,conversationID?:string) {
  return object(value)&&(conversationID===undefined||value.conversation_id===conversationID)?parseSurfaces(value.surface):undefined
}

type SurfaceTransport=(path:string,data:SurfaceAction,method:'POST',headers:Record<string,string>)=>Promise<unknown>
export type DiscoveryConfirmationRef={key:string;candidateID:string;revision:number}
type DiscoveryAdmissionResult={id:string;state:'admitted';asset:string;revision:number;check_task_id?:string}
type AdmittedReceipt=Extract<AssistantReply,{state:'completed'}>&{result:DiscoveryAdmissionResult;outcome:'admitted'}
type ConfirmationLookup={state:'admitted';receipt:AdmittedReceipt}|{state:'not_confirmed'}
const admissionTool='admit_discovery_candidate'
const validRequestKey=(value:unknown):value is string => typeof value==='string'&&/^[A-Za-z0-9_-]{8,100}$/.test(value)
const confirmationID=(value:unknown):value is string => typeof value==='string'&&/^[a-f\d]{48}$/.test(value)
const localID=(value:unknown):value is string => typeof value==='string'&&/^[a-f\d]{32}$/.test(value)

export function isDiscoveryConfirmationRef(value:unknown):value is DiscoveryConfirmationRef {
  return object(value)&&keysAre(value,['key','candidateID','revision'])&&validRequestKey(value.key)&&validCandidateID(value.candidateID)&&validRevision(value.revision)
}
export function discoveryConfirmationRef(action:SurfaceAction,key:string):DiscoveryConfirmationRef|undefined {
  if(!validAction(action)||action.action.name!=='request_discovery_admission')return
  const pointer={key,candidateID:action.action.context.candidate_id,revision:action.action.context.revision}
  return isDiscoveryConfirmationRef(pointer)?pointer:undefined
}
function confirmationActionPath(conversationID:string,pointer:DiscoveryConfirmationRef) {
  const path=a2uiPath(conversationID,'actions')
  if(!path||!isDiscoveryConfirmationRef(pointer))throw new Error('无法核对原纳管操作。')
  return path+'/'+encodeURIComponent(pointer.key)
}
function admittedReceipt(value:unknown,conversationID:string,pointer:DiscoveryConfirmationRef):AdmittedReceipt|undefined {
  if(!object(value)||value.conversation_id!==conversationID||!localID(value.run_id)||Object.hasOwn(value,'confirmation_id')||!isAssistantReply(value)||value.state!=='completed'||
    value.tool!==admissionTool||value.outcome!=='admitted'||!object(value.result))return
  const result=value.result
  if(result.id!==pointer.candidateID||result.revision!==pointer.revision||result.state!=='admitted'||
    typeof result.asset!=='string'||!/^[a-zA-Z0-9_-]{1,64}:discovered:[a-f\d]{32}$/.test(result.asset)||
    result.check_task_id!==undefined&&!localID(result.check_task_id))return
  return {state:'completed',tool:admissionTool,result:{id:result.id,revision:result.revision,asset:result.asset,state:'admitted',
    ...(typeof result.check_task_id==='string'?{check_task_id:result.check_task_id}:{})},outcome:'admitted'}
}
export async function confirmDiscoveryAdmission(transport:(path:string,body:{confirmation_id:string})=>Promise<unknown>,
  conversationID:string,pointer:DiscoveryConfirmationRef,previewID:string):Promise<AdmittedReceipt> {
  if(!confirmationID(previewID))throw new Error('纳管确认凭据无效。')
  const path=confirmationActionPath(conversationID,pointer)
  const receipt=admittedReceipt(await transport(path+'/confirm',{confirmation_id:previewID}),conversationID,pointer)
  if(!receipt)throw new Error('未收到与原纳管操作匹配的确认结果。')
  return receipt
}
export async function recoverDiscoveryConfirmation(transport:(path:string)=>Promise<unknown>,
  conversationID:string,pointer:DiscoveryConfirmationRef):Promise<ConfirmationLookup> {
  const path=confirmationActionPath(conversationID,pointer)
  const value=await transport(path)
  const receipt=admittedReceipt(value,conversationID,pointer)
  if(receipt)return {state:'admitted',receipt}
  if(object(value)&&value.conversation_id===conversationID&&value.state==='completed'&&value.tool==='request_discovery_admission'&&
    value.outcome==='show_admission_preview'&&!Object.hasOwn(value,'confirmation_id'))return {state:'not_confirmed'}
  throw new Error('当前没有可核对的原纳管回执。')
}
function validAction(value:unknown):value is SurfaceAction {
  if(!object(value)||!keysAre(value,['version','action'])||value.version!=='1.0'||!object(value.action))return false
  const a=value.action
  if(!keysAre(a,['name','surfaceId','sourceComponentId','timestamp','context'])||typeof a.name!=='string'||!Object.hasOwn(eventFields,a.name)||
    !validID(a.surfaceId)||!validID(a.sourceComponentId)||typeof a.timestamp!=='string'||!/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$/.test(a.timestamp)||
    !Number.isFinite(Date.parse(a.timestamp))||!object(a.context)||!keysAre(a.context,[...eventFields[a.name]]))return false
  return Object.entries(a.context).every(([key,data])=>key==='candidate_id'?validCandidateID(data):key==='revision'?validRevision(data):validField(data)&&data===data.trim())
}
export async function sendSurfaceAction(transport:SurfaceTransport,conversationID:string,body:SurfaceAction,requestKey:string):Promise<AssistantReply> {
  const path=a2uiPath(conversationID,'actions')
  if(!path||!validRequestKey(requestKey)||!validAction(body)||!['review_discovery_candidate','request_discovery_admission'].includes(body.action.name))throw new Error('无法核对本次会话和操作。')
  const receipt=await transport(path,body,'POST',{'X-ClusBot-Request-Key':requestKey})
  return checkSurfaceReply(receipt,conversationID,body)
}

export function benchmarkSurfacePath(conversation:string,run:string) {
  return localID(conversation)&&localID(run)?'/conversations/'+conversation+'/a2ui/benchmark-revision/'+run:undefined
}
export function benchmarkPreviewSurfaces(receipt:unknown,conversation:string,run:string,task:string,revision:number) {
  const surfaces=parseSurfaceReceipt(receipt,conversation)
  if(surfaces?.length!==2)return
  function matches(surface:Surface,name:string,id:string) {
    const button=surface.nodes.get(id)
    const event=button?.component==='Button'?button.action.event:undefined
    return event?.name===name&&event.context?.source_run_id===run&&event.context.task_id===task&&event.context.revision===revision
  }
  const preview=surfaces.find(surface=>matches(surface,benchmarkRevisionAction,'preview_revision'))
  const sync=surfaces.find(surface=>matches(surface,benchmarkSyncActualAction,'preview_with_actual'))
  if(preview&&sync&&preview.id!==sync.id)return {preview,sync}
}
type BenchmarkImpactScope={target_count:number;target_refs:string[]}
type BenchmarkParameterChange={before:string;after:string;expected_before?:string;expected_after?:string}
type BenchmarkCaseAssetImpact={case_key:string;change:'removed'|'changed';before_refs:string[];after_refs:string[]}
type BenchmarkRevisionAssetImpact={
  baseline:BenchmarkImpactScope;proposed:BenchmarkImpactScope;scope_changed:boolean
  removed_refs:string[];changed_cases:BenchmarkCaseAssetImpact[];current_local_holds:{case_key:string;target_refs:string[]}[]
}
export type RevisionPreview={
  run_id:string;conversation_id:string;task_id:string;source_run_id?:string;requested_action?:typeof benchmarkSyncActualAction
  proposal:{state:string;expected_revision:number;interpretation:{action:string;case_key:string;parameter?:string;value?:string;change_expected_actual_config?:boolean};
    parameter_change?:BenchmarkParameterChange
    preview:{revision:number;changes:{added:string[];removed:string[];changed:string[];order_changed:boolean};frozen_cases:string[];
      asset_impact?:BenchmarkRevisionAssetImpact}}
}
const stringsOrEmpty=(value:unknown):string[]|undefined=>value===null?[]:Array.isArray(value)&&value.length<=4096&&value.every(v=>typeof v==='string'&&v.length<=512)?value:undefined
function uniqueAssetRefs(value:unknown):string[]|undefined {
  const refs=stringsOrEmpty(value)
  if(!refs||refs.some(ref=>!ref||ref.trim()!==ref)||new Set(refs).size!==refs.length)return
  return refs
}
const requiredRefs=(refs:string[],scope:ReadonlySet<string>)=>refs.every(ref=>scope.has(ref))
function previewScope(value:unknown):BenchmarkImpactScope|undefined {
  if(!object(value)||value.source!=='frozen_targets'||typeof value.target_count!=='number'||!Number.isSafeInteger(value.target_count)||value.target_count<0)return
  const refs=uniqueAssetRefs(value.target_refs)
  if(!refs||refs.length!==value.target_count)return
  return {target_count:value.target_count,target_refs:refs}
}
function revisionAssetImpact(value:unknown,revision:number,caseKey:string,action:unknown,frozen:string[]):BenchmarkRevisionAssetImpact|undefined {
  if(!object(value)||value.source!=='frozen_targets'||value.base_revision!==revision||value.scope_limit!=='within_original_task'||
    typeof value.scope_changed!=='boolean'||!Array.isArray(value.changed_cases)||value.changed_cases.length!==1||
    !Array.isArray(value.current_local_holds)||value.current_local_holds.length>64)return
  const baseline=previewScope(value.baseline),proposed=previewScope(value.proposed)
  if(!baseline||!proposed)return
  const before=new Set(baseline.target_refs),after=new Set(proposed.target_refs)
  const removed=baseline.target_refs.filter(ref=>!after.has(ref))
  if(!requiredRefs(proposed.target_refs,before)||value.scope_changed!==(removed.length>0))return
  const row=value.changed_cases[0],change=action==='remove_case'?'removed':'changed'
  if(!object(row)||row.case_key!==caseKey||row.change!==change)return
  const beforeRefs=uniqueAssetRefs(row.before_refs),afterRefs=uniqueAssetRefs(row.after_refs)
  if(!beforeRefs?.length||!afterRefs||!requiredRefs(beforeRefs,before)||!requiredRefs(afterRefs,after)||
    (change==='removed'?afterRefs.length!==0||!requiredRefs(removed,new Set(beforeRefs)):
      removed.length!==0||afterRefs.length!==beforeRefs.length||!requiredRefs(afterRefs,new Set(beforeRefs))))return
  const holds:BenchmarkRevisionAssetImpact['current_local_holds']=[]
  const heldCases=new Set<string>()
  for(const item of value.current_local_holds){
    if(!object(item)||typeof item.case_key!=='string'||!frozen.includes(item.case_key)||heldCases.has(item.case_key))return
    const refs=uniqueAssetRefs(item.target_refs)
    if(!refs?.length||!requiredRefs(refs,before))return
    heldCases.add(item.case_key);holds.push({case_key:item.case_key,target_refs:refs})
  }
  return {baseline,proposed,scope_changed:value.scope_changed,removed_refs:removed,
    changed_cases:[{case_key:caseKey,change,before_refs:beforeRefs,after_refs:afterRefs}],current_local_holds:holds}
}
// Validate the displayed diff before offering an apply button, including
// results fetched after a dropped HTTP acknowledgement.
export function checkedBenchmarkPreview(receipt:unknown,conversation:string,sourceRun:string|undefined,task:string,revision:number):RevisionPreview {
  const invalid=()=>new Error('没有收到与原任务匹配的预演回执。')
  if(!object(receipt)||!localID(receipt.run_id)||receipt.conversation_id!==conversation||receipt.source_run_id!==sourceRun||
    receipt.task_id!==task||!object(receipt.proposal)||receipt.proposal.state!=='preview_only'||receipt.proposal.expected_revision!==revision)throw invalid()
  const {interpretation:intent,preview,parameter_change:parameter}=receipt.proposal
  const sync=receipt.requested_action===benchmarkSyncActualAction
  if((receipt.requested_action!==undefined&&!sync)||(sync&&!sourceRun))throw invalid()
  if(!object(intent)||!['remove_case','set_parameter'].includes(String(intent.action))||typeof intent.case_key!=='string'||!intent.case_key||
    !object(preview)||preview.revision!==revision+1||!object(preview.changes)||typeof preview.changes.order_changed!=='boolean')throw invalid()
  const added=stringsOrEmpty(preview.changes.added),removed=stringsOrEmpty(preview.changes.removed),changed=stringsOrEmpty(preview.changes.changed),frozen=stringsOrEmpty(preview.frozen_cases)
  if(!added||!removed||!changed||!frozen||added.length!==0||preview.changes.order_changed||
    (intent.action==='remove_case'?(removed.length!==1||removed[0]!==intent.case_key||changed.length!==0):
    (changed.length!==1||changed[0]!==intent.case_key||removed.length!==0||typeof intent.parameter!=='string'||!intent.parameter||typeof intent.value!=='string'||!intent.value))||
    (intent.change_expected_actual_config!==undefined&&typeof intent.change_expected_actual_config!=='boolean')||
    (sync&&(intent.action!=='set_parameter'||intent.change_expected_actual_config!==true))||
    (intent.change_expected_actual_config===true&&(sourceRun!==undefined&&!sync||intent.action!=='set_parameter')))throw invalid()
  const safe:RevisionPreview={run_id:receipt.run_id,conversation_id:conversation,source_run_id:sourceRun,task_id:task,
    ...(sync?{requested_action:benchmarkSyncActualAction}:{}),
    proposal:{state:'preview_only',expected_revision:revision,interpretation:{action:String(intent.action),case_key:intent.case_key,
      ...(intent.action==='set_parameter'?{parameter:String(intent.parameter),value:String(intent.value)}:{}),
      ...(intent.change_expected_actual_config===true?{change_expected_actual_config:true}:{})},
      preview:{revision:revision+1,changes:{added,removed,changed,order_changed:false},frozen_cases:frozen}}}
  if(intent.action==='set_parameter'){
    if(!object(parameter)||!validField(parameter.before)||!validField(parameter.after)||parameter.before===parameter.after||
      parameter.after!==intent.value||(intent.change_expected_actual_config===true?
        parameter.expected_before!==parameter.before||parameter.expected_after!==parameter.after:
        parameter.expected_before!==undefined||parameter.expected_after!==undefined))throw invalid()
    safe.proposal.parameter_change={before:parameter.before,after:parameter.after,
      ...(intent.change_expected_actual_config===true?{expected_before:parameter.before,expected_after:parameter.after}:{})}
  } else if(parameter!==undefined)throw invalid()
  if(preview.asset_impact!==undefined&&preview.asset_impact!==null) {
    const impact=revisionAssetImpact(preview.asset_impact,revision,intent.case_key,intent.action,frozen)
    if(!impact)throw invalid()
    safe.proposal.preview.asset_impact=impact
  }
  return safe
}
function benchmarkPreviewPath(preview:RevisionPreview) {
  if(!localID(preview.conversation_id)||!localID(preview.run_id))throw new Error('无法核对原修订预演。')
  return '/conversations/'+preview.conversation_id+'/benchmark-revision-preview/'+preview.run_id
}
type RevisionRead=(path:string)=>Promise<unknown>
export async function readBenchmarkRevisionApplied(read:RevisionRead,preview:RevisionPreview):Promise<number|undefined> {
  const meta=await read(benchmarkPreviewPath(preview)+'?metadata=1')
  if(!object(meta)||meta.task_id!==preview.task_id||meta.source_run_id!==preview.source_run_id||
    meta.expected_revision!==preview.proposal.expected_revision||
    (meta.applied_revision!==undefined&&meta.applied_revision!==preview.proposal.preview.revision))
    throw new Error('没有收到与原预演对应的修订回执。')
  return meta.applied_revision as number|undefined
}
export async function readBenchmarkRevisionSnapshot(read:RevisionRead,preview:RevisionPreview) {
  const fresh=checkedBenchmarkPreview(await read(benchmarkPreviewPath(preview)),preview.conversation_id,preview.source_run_id,
    preview.task_id,preview.proposal.expected_revision)
  const prior=preview.proposal, next=fresh.proposal
  if(fresh.run_id!==preview.run_id||fresh.requested_action!==preview.requested_action||!next.preview.asset_impact||
    JSON.stringify(prior.interpretation)!==JSON.stringify(next.interpretation)||
    JSON.stringify(prior.parameter_change)!==JSON.stringify(next.parameter_change)||
    JSON.stringify(prior.preview.changes)!==JSON.stringify(next.preview.changes))
    throw new Error('无法重新核对原方案及资产范围。')
  return fresh
}
export async function previewBenchmarkSurface(transport:SurfaceTransport,conversation:string,sourceRun:string,task:string,revision:number,
  action:SurfaceAction,key:string):Promise<RevisionPreview> {
  const path=benchmarkSurfacePath(conversation,sourceRun)
  if(!path||!localID(task)||!validRevision(revision)||!validRequestKey(key)||!validAction(action)||
    (action.action.name!==benchmarkRevisionAction&&action.action.name!==benchmarkSyncActualAction)||action.action.context.source_run_id!==sourceRun||
    action.action.context.task_id!==task||action.action.context.revision!==revision)throw new Error('无法核对修订的会话、任务或版本。')
  const receipt=await transport(path,action,'POST',{'X-ClusBot-Request-Key':key})
  const preview=checkedBenchmarkPreview(receipt,conversation,sourceRun,task,revision)
  if((preview.requested_action===benchmarkSyncActualAction)!==(action.action.name===benchmarkSyncActualAction))
    throw new Error('预演回执与本次用户选择不符。')
  return preview
}
export async function readSurfaceAction(transport:(path:string)=>Promise<unknown>,conversationID:string,body:SurfaceAction,requestKey:string):Promise<AssistantReply> {
  const path=a2uiPath(conversationID,'actions')
  if(!path||!validRequestKey(requestKey)||!validAction(body)||!['review_discovery_candidate','request_discovery_admission'].includes(body.action.name))throw new Error('无法核对本次会话和操作。')
  const receipt=await transport(path+'/'+encodeURIComponent(requestKey))
  return checkSurfaceReply(receipt,conversationID,body,true)
}

function checkSurfaceReply(receipt:unknown,conversationID:string,body:SurfaceAction,readBack=false):AssistantReply {
  if(object(receipt)&&receipt.conversation_id===conversationID&&isAssistantReply(receipt)) {
    if(body.action.name==='review_discovery_candidate'&&receipt.state==='completed'&&receipt.tool==='review_discovery_candidate'){
      const surfaces=parseSurfaces(receipt.surface)
      if(surfaces) {
        const remoteEvents=surfaces.flatMap(surface=>[...surface.nodes.values()].flatMap(node=>node.component==='Button'&&node.action.event.name!=='toggle_details'?[node.action.event]:[]))
        if(receipt.outcome==='show_review'&&remoteEvents.length===1){
          const event=remoteEvents[0]
          if(event.name==='request_discovery_admission'&&event.context?.candidate_id===body.action.context.candidate_id&&event.context.revision===body.action.context.revision)return receipt
        }
        if((receipt.outcome==='cannot_prepare'||receipt.outcome==='ask_for_details')&&!remoteEvents.length)return receipt
      }
    }
    if(body.action.name==='request_discovery_admission'&&receipt.state==='completed'&&receipt.tool==='request_discovery_admission'&&
      (receipt.outcome==='cannot_prepare'||receipt.outcome==='ask_for_details'||(readBack&&receipt.outcome==='show_admission_preview'))){
      const surfaces=parseSurfaces(receipt.surface)
      if(surfaces?.every(surface=>[...surface.nodes.values()].every(node=>node.component!=='Button'||node.action.event.name==='toggle_details')))return receipt
    }
    if(!readBack&&body.action.name==='request_discovery_admission'&&receipt.state==='confirmation_required'&&receipt.input.tool==='admit_discovery_candidate') {
      const {params,body:input}=receipt.input
      const {candidate_id,revision,owner,location,role}=body.action.context
      if(object(params)&&params.id===candidate_id&&object(input)&&input.expected_seen_at===revision&&validField(input.confirmed_serial)&&
        input.owner===owner&&input.location===location&&input.role===role)return receipt
    }
  }
  throw new Error('没有收到与本次操作匹配的回执。请刷新候选后核对。')
}

export function parseSurfaces(value:unknown):Surface[]|undefined {
  if(!Array.isArray(value) || !value.length || value.length>16)return
  const surfaces=new Map<string,Surface>()
  let textLength=0
  let dataLength=0
  function putData(model:SurfaceData,path:string,value:unknown,depth=0):boolean {
    if(depth>6)return false
    if(path && scalar(value)) {
      if(typeof value==='string')dataLength+=value.length
      if(dataLength>24000||model.size>=128)return false
      model.set(path,value)
      return true
    }
    if(!object(value))return false
    const entries=Object.entries(value)
    if(entries.length>64)return false
    return entries.every(([key,child])=>safeKey(key) && putData(model,path+'/'+key,child,depth+1))
  }
  function replaceData(model:SurfaceData,path:string,value:unknown):boolean {
    for(const key of model.keys())if(!path||key===path||key.startsWith(path+'/')||path.startsWith(key+'/'))model.delete(key)
    return value===null?Boolean(path):putData(model,path,value)
  }
  function putNodes(surface:Surface,components:unknown):boolean {
    if(!Array.isArray(components)||!components.length||components.length>100)return false
    const nodes=surface.nodes, updated=new Set<string>()
    for(const item of components) {
      if(!object(item) || !validID(item.id) || updated.has(item.id))return false
      updated.add(item.id)
      if(item.component==='Text' && keysAre(item,['id','component','text']) && (typeof item.text==='string'&&item.text.length<=4000 || binding(item.text))) {
        if(typeof item.text==='string')textLength+=item.text.length
        nodes.set(item.id,{id:item.id,component:'Text',text:item.text})
      } else if((item.component==='Column' || item.component==='Row') && keysAre(item,['id','component','children']) && Array.isArray(item.children) && item.children.length<=50 && item.children.every(validID)) {
        nodes.set(item.id,{id:item.id,component:item.component,children:item.children})
      } else if(item.component==='Card' && keysAre(item,['id','component','child']) && validID(item.child)) {
        nodes.set(item.id,{id:item.id,component:'Card',child:item.child})
      } else if(item.component==='TextField'&&allowedKeys(item,['id','component','label','value'],['variant'])&&(item.variant===undefined||item.variant==='shortText')&&typeof item.label==='string'&&item.label.trim().length>0&&item.label.length<=128&&binding(item.value)&&editablePaths.has(item.value['@path'])) {
        textLength+=item.label.length
        nodes.set(item.id,{id:item.id,component:'TextField',label:item.label,value:item.value})
      } else if(item.component==='Button' && allowedKeys(item,['id','component','child','action'],['variant']) && validID(item.child) && object(item.action) && keysAre(item.action,['event'])) {
        const event=parseEvent(item.action.event)
        const variant=item.variant??'default'
        if(!event||variant!=='borderless'&&variant!=='default'&&variant!=='primary')return false
        if(event.name==='toggle_details'&&variant!=='borderless')return false
        nodes.set(item.id,{id:item.id,component:'Button',child:item.child,variant,action:{event}})
      } else return false
      if(textLength>24000||nodes.size>100)return false
    }
    return true
  }
  for(const message of value) {
    if(!object(message) || message.version!=='1.0')return
    if(keysAre(message,['version','createSurface'])) {
      const create=message.createSurface
      if(!object(create)||!allowedKeys(create,['surfaceId','catalogId'],['components','dataModel','sendDataModel'])||
        !validID(create.surfaceId)||create.catalogId!==catalog||surfaces.has(create.surfaceId)||
        create.sendDataModel!==undefined&&create.sendDataModel!==false)return
      const surface:Surface={id:create.surfaceId,nodes:new Map(),data:new Map()}
      if(create.dataModel!==undefined&&!replaceData(surface.data,'',create.dataModel))return
      if(create.components!==undefined&&!putNodes(surface,create.components))return
      surfaces.set(surface.id,surface)
      continue
    }
    if(keysAre(message,['version','updateDataModel'])) {
      const update=message.updateDataModel
      if(!object(update)||!allowedKeys(update,['surfaceId','value'],['path'])||!validID(update.surfaceId)||
        update.path!==undefined&&update.path!=='/'&&!validPath(update.path))return
      const surface=surfaces.get(update.surfaceId)
      if(!surface||!replaceData(surface.data,typeof update.path==='string'&&update.path!=='/'?update.path:'',update.value))return
      continue
    }
    if(!keysAre(message,['version','updateComponents']))return
    const update=message.updateComponents
    if(!object(update)||!keysAre(update,['surfaceId','components'])||!validID(update.surfaceId))return
    const surface=surfaces.get(update.surfaceId)
    if(!surface||!putNodes(surface,update.components))return
  }
  for(const {nodes,data} of surfaces.values()) {
    const visited=new Set<string>()
    const editable=new Set<string>()
    function visit(id:string,depth:number):boolean {
      if(depth>12||visited.has(id))return false
      const node=nodes.get(id)
      if(!node)return false
      visited.add(id)
      if(node.component==='Text')return typeof node.text==='string'||typeof data.get(node.text['@path'])==='string'
      if(node.component==='TextField'){
        const path=node.value['@path']
        if(editable.has(path)||typeof data.get(path)!=='string')return false
        editable.add(path)
        return true
      }
      if(node.component==='Button'&&nodes.get(node.child)?.component!=='Text')return false
      return ('children' in node?node.children:[node.child]).every(child=>visit(child,depth+1))
    }
    if(!visit('root',0)||visited.size!==nodes.size)return
    for(const node of nodes.values())if(node.component==='Button'&&node.action.event.context){
      for(const value of Object.values(node.action.event.context))if(binding(value)&&!editable.has(value['@path']))return
    }
  }
  return [...surfaces.values()]
}

const fields:Record<string,[string,string][]> = {
  list_tasks:[['title','标题'],['status','状态'],['scope_label','范围'],['id','任务 ID']],
  list_attention:[['kind','类型'],['state','状态'],['object_ref','对象'],['action','待处理'],['task_id','任务 ID'],['id','事项 ID']],
  list_discovery_candidates:[['source','来源'],['scope','范围'],['serial','序列号'],['address','地址'],['state','状态'],['id','候选 ID']],
}
export type DetailRow = {fields:{label:string;value:string}[];taskID?:string}
export function assistantDetails(tool:unknown,result:unknown):DetailRow[] {
  if(typeof tool!=='string' || !Object.hasOwn(fields,tool))return []
  const items=tool==='list_attention' && object(result)?result.items:result
  if(!Array.isArray(items))return []
  return items.slice(0,30).flatMap(item=>{
    if(!object(item))return []
    const entries=fields[tool].flatMap(([key,label])=>{
      const value=item[key]
      return typeof value==='string' && value.trim()?[{label,value:value.length>240?value.slice(0,240)+'…':value}]:[]
    })
    const taskID=tool==='list_tasks'?item.id:tool==='list_attention'?item.task_id:undefined
    return entries.length?[{fields:entries,taskID:typeof taskID==='string' && /^[a-f\d]{32}$/.test(taskID)?taskID:undefined}]:[]
  })
}

// Never enable a write when a complete, non-secret preview cannot be shown.
type PreviewField = {label:string;value:string}
const previewLabels:Record<string,string> = {
  tool:'操作',params:'目标',query:'筛选',body:'内容',target:'核验目标',
  id:'对象 ID',owner:'所属账号',task_id:'任务 ID',run_id:'执行 ID',artifact_id:'成果 ID',
  title:'标题',text:'任务内容',goal:'目标',scope_label:'工作范围',revision:'版本',
  acceptance_criteria:'验收条件',required_filename:'交付文件',decision:'决定',
  object_ref:'对象',fault_evidence:'故障依据',status:'状态',state:'状态',
  confirmed_serial:'候选序列号',expected_seen_at:'观测时间戳',serial:'序列号',address:'地址',scope:'范围',location:'位置',role:'用途',
  profile:'方案',version:'方案版本',steps:'步骤',depends_on:'依赖',optional:'可选',
}
const operationLabels:Record<string,string> = {
  create_task:'创建任务',edit_task:'修改任务',pause_task:'暂停任务',cancel_task:'取消任务',
  resume_task:'继续任务',accept_task:'验收任务',create_workflow:'创建工作计划',
  request_task_publication:'申请保存成果',decide_publication:'处理保存申请',
  decide_approval:'处理审批',request_ticket:'提交工单',admit_discovery_candidate:'纳管候选资产',
}
export function previewText(value:unknown):{text:string;safe:boolean;fields:PreviewField[]} {
  let safe=true, count=0
  const secret=/(?:password|passwd|secret|credential|token|api[_-]?key|(?:private|access|signing)[_-]?key|authorization|密码|密钥|凭据)/i
  function clean(input:unknown,depth:number):unknown {
    if(++count>500 || depth>8){safe=false;return '内容过多，无法完整预览'}
    if(input===null || typeof input==='boolean' || typeof input==='number')return input
    if(typeof input==='string') {
      if(input.length>4000){safe=false;return input.slice(0,4000)+'…'}
      return input
    }
    if(Array.isArray(input)) {
      if(input.length>100)safe=false
      return input.slice(0,100).map(item=>clean(item,depth+1))
    }
    if(object(input)) {
      const entries=Object.entries(input)
      if(entries.length>100)safe=false
      return Object.fromEntries(entries.slice(0,100).map(([key,val])=>{
      if(secret.test(key) && val!==undefined && val!==null && val!==''){safe=false;return [key,'[敏感内容已隐藏]']}
      return [key,clean(val,depth+1)]
      }))
    }
    safe=false
    return '无法预览'
  }
  const cleaned=clean(value,0)
  const fields:PreviewField[]=[]
  function collect(input:unknown,path:string[]) {
    if(Array.isArray(input)) {
      if(!input.length)fields.push({label:path.join(' · '),value:'空列表'})
      input.forEach((item,index)=>collect(item,[...path,String(index+1)]))
      return
    }
    if(object(input)) {
      const entries=Object.entries(input).filter(([key])=>key!=='client_request_id' && key!=='request_key')
      if(!entries.length && path.length)fields.push({label:path.join(' · '),value:'无'})
      for(const [key,val] of entries) {
        if(key==='input'){collect(val,path);continue}
        const label=previewLabels[key]??key
        const next=(key==='body'&&path.length===0)?path:[...path,label]
        collect(val,next)
      }
      return
    }
    let output=input===null?'无':input===true?'是':input===false?'否':String(input)
    if(path.at(-1)==='操作')output=operationLabels[output]??output
    fields.push({label:path.join(' · ')||'内容',value:output||'空字符串'})
  }
  collect(cleaned,[])
  const text=JSON.stringify(cleaned,null,2)??''
  if(text.length>24000)return {text:text.slice(0,24000)+'\n…',safe:false,fields}
  return {text,safe,fields}
}

const formTargets:Record<string,string> = {
  configure_model_provider:'/settings#model-default',
  configure_model_profile:'/settings#model-profiles',
  configure_model_role:'/settings#model-roles',
  configure_mcp_permission:'/connections',
}
export function trustedForm(tool:unknown,target:unknown):string|undefined {
  if(tool==='manage_accounts' && target==='account_management')return '/account#users'
  if(tool==='change_my_password' && target==='account_password')return '/account#password'
  return typeof tool==='string' && Object.hasOwn(formTargets,tool) && target===formTargets[tool]?formTargets[tool]:undefined
}
