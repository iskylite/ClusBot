// A deliberately small A2UI 1.0 subset. Surface data is presentation only;
// actions and destinations are decided by the client, never by this payload.
const catalog = 'https://a2ui.org/specification/v1_0/catalogs/basic/catalog.json'
const idPattern = /^[A-Za-z0-9_-]{1,80}$/

type ContainerNode = {id:string;component:'Column'|'Row';children:string[]}
type TextNode = {id:string;component:'Text';text:string}
type CardNode = {id:string;component:'Card';child:string}
type ButtonNode = {id:string;component:'Button';child:string;variant:'borderless';action:{event:{name:'toggle_details'}}}
export type SurfaceNode = ContainerNode|TextNode|CardNode|ButtonNode
export type Surface = {id:string;nodes:Map<string,SurfaceNode>}

const object = (value:unknown):value is Record<string,unknown> => typeof value==='object' && value!==null && !Array.isArray(value)
const keysAre = (value:Record<string,unknown>,keys:string[]) => Object.keys(value).length===keys.length && keys.every(key=>Object.hasOwn(value,key))
const validID = (value:unknown):value is string => typeof value==='string' && idPattern.test(value)

export type AssistantTool = {name:string;description:string;mode:'read'|'confirm'|'form';params?:string[];label?:string}
export type AssistantReply = {state:'completed';tool:string;result:unknown;surface?:unknown} |
  {state:'confirmation_required';confirmation_id:string;expires_at_unix_ms:number;input:Record<string,unknown>;target?:unknown;surface?:unknown} |
  {state:'form_required';tool:string;form:string}
export type AssistantItem = AssistantReply|AssistantTool[]

// Only a recognizable receipt may consume the composer's draft.
export function isAssistantItem(value:unknown):value is AssistantItem {
  if(Array.isArray(value))return value.length<=200 && value.every(item=>object(item) && typeof item.name==='string' && /^[a-z0-9_]{1,80}$/.test(item.name) && typeof item.description==='string' && ['read','confirm','form'].includes(String(item.mode)) && (item.label===undefined||typeof item.label==='string') && (item.params===undefined||(Array.isArray(item.params)&&item.params.every((p:unknown)=>typeof p==='string'))))
  if(!object(value))return false
  if(value.state==='completed')return typeof value.tool==='string' && value.tool.length>0 && Object.hasOwn(value,'result')
  if(value.state==='form_required')return typeof value.tool==='string' && typeof value.form==='string'
  return value.state==='confirmation_required' && typeof value.confirmation_id==='string' && /^[a-f\d]{48}$/.test(value.confirmation_id) && typeof value.expires_at_unix_ms==='number' && Number.isSafeInteger(value.expires_at_unix_ms) && value.expires_at_unix_ms>=0 && value.expires_at_unix_ms<=8640000000000000 && object(value.input) && typeof value.input.tool==='string'
}

export function parseSurfaces(value:unknown):Surface[]|undefined {
  if(!Array.isArray(value) || !value.length || value.length>16)return
  const created=new Set<string>()
  const surfaces=new Map<string,Surface>()
  let textLength=0
  for(const message of value) {
    if(!object(message) || message.version!=='1.0')return
    if(keysAre(message,['version','createSurface'])) {
      const create=message.createSurface
      if(!object(create) || !keysAre(create,['surfaceId','catalogId']) || !validID(create.surfaceId) || create.catalogId!==catalog || created.has(create.surfaceId))return
      created.add(create.surfaceId)
      continue
    }
    if(!keysAre(message,['version','updateComponents']))return
    const update=message.updateComponents
    if(!object(update) || !keysAre(update,['surfaceId','components']) || !validID(update.surfaceId) || !created.has(update.surfaceId) || surfaces.has(update.surfaceId) || !Array.isArray(update.components) || !update.components.length || update.components.length>100)return
    const nodes=new Map<string,SurfaceNode>()
    for(const item of update.components) {
      if(!object(item) || !validID(item.id) || nodes.has(item.id))return
      if(item.component==='Text' && keysAre(item,['id','component','text']) && typeof item.text==='string' && item.text.length<=4000) {
        textLength+=item.text.length
        if(textLength>24000)return
        nodes.set(item.id,{id:item.id,component:'Text',text:item.text})
      } else if((item.component==='Column' || item.component==='Row') && keysAre(item,['id','component','children']) && Array.isArray(item.children) && item.children.length<=50 && item.children.every(validID)) {
        nodes.set(item.id,{id:item.id,component:item.component,children:item.children})
      } else if(item.component==='Card' && keysAre(item,['id','component','child']) && validID(item.child)) {
        nodes.set(item.id,{id:item.id,component:'Card',child:item.child})
      } else if(item.component==='Button' && keysAre(item,['id','component','child','variant','action']) && validID(item.child) && item.variant==='borderless' && object(item.action) && keysAre(item.action,['event']) && object(item.action.event) && keysAre(item.action.event,['name']) && item.action.event.name==='toggle_details') {
        nodes.set(item.id,{id:item.id,component:'Button',child:item.child,variant:'borderless',action:{event:{name:'toggle_details'}}})
      } else return
    }
    if(!nodes.has('root'))return
    const visited=new Set<string>()
    function visit(id:string,depth:number):boolean {
      if(depth>12 || visited.has(id))return false
      const node=nodes.get(id)
      if(!node)return false
      visited.add(id)
      if(node.component==='Text')return true
      if(node.component==='Button' && nodes.get(node.child)?.component!=='Text')return false
      return ('children' in node ? node.children : [node.child]).every(child=>visit(child,depth+1))
    }
    if(!visit('root',0) || visited.size!==nodes.size)return
    surfaces.set(update.surfaceId,{id:update.surfaceId,nodes})
  }
  if(created.size!==surfaces.size)return
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
  profile:'方案',version:'方案版本',steps:'步骤',depends_on:'依赖',optional:'可选',
}
const operationLabels:Record<string,string> = {
  create_task:'创建任务',edit_task:'修改任务',pause_task:'暂停任务',cancel_task:'取消任务',
  resume_task:'继续任务',accept_task:'验收任务',create_workflow:'创建工作计划',
  request_task_publication:'申请保存成果',decide_publication:'处理保存申请',
  decide_approval:'处理审批',request_ticket:'提交工单',
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
  configure_mcp_connection:'/connections/new',
  configure_mcp_permission:'/connections',
}
export function trustedForm(tool:unknown,target:unknown):string|undefined {
  return typeof tool==='string' && Object.hasOwn(formTargets,tool) && target===formTargets[tool]?formTargets[tool]:undefined
}
