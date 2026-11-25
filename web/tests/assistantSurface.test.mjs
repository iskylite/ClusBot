import assert from 'node:assert/strict'
import {readFile} from 'node:fs/promises'
import test from 'node:test'
import ts from 'typescript'

const source=await readFile(new URL('../src/assistantSurface.ts',import.meta.url),'utf8')
const js=ts.transpileModule(source,{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ESNext}}).outputText
const assistantSurfaceModule='data:text/javascript;base64,'+Buffer.from(js).toString('base64')
const {parseSurfaces,assistantDetails,previewText,trustedForm,isAssistantItem,assistantReplyCards,createSurfaceAction,
  resolveSurfaceText,parseSurfaceReceipt,sendSurfaceAction,readSurfaceAction,confirmDiscoveryAdmission,
  recoverDiscoveryConfirmation,discoveryConfirmationRef}=await import(assistantSurfaceModule)
const catalog='https://a2ui.org/specification/v1_0/catalogs/basic/catalog.json'
const makeSurface=()=>[
  {version:'1.0',createSurface:{surfaceId:'a',catalogId:catalog}},
  {version:'1.0',updateComponents:{surfaceId:'a',components:[
    {id:'root',component:'Column',children:['title','button']},
    {id:'title',component:'Text',text:'<img src=x onerror=alert(1)>'},
    {id:'button',component:'Button',child:'label',variant:'borderless',action:{event:{name:'toggle_details'}}},
    {id:'label',component:'Text',text:'展开详情'},
  ]}},
]

const candidate='c'.repeat(64), conversation='a'.repeat(32), otherConversation='b'.repeat(32)
const revision=1700000000000
const time=new Date('2026-11-25T10:15:30.000Z')
const binding=key=>({'@path':'/form/'+key})
const formFields=['owner','location','role']
const makeReview=(candidateID=candidate)=>[{version:'1.0',createSurface:{
  surfaceId:'discovery_review',catalogId:catalog, dataModel:{form:{owner:'',location:'',role:''},candidate:{serial:'TRACE-42'}},
  components:[
    {id:'root',component:'Column',children:['title',...formFields,'request_admission']},
    {id:'title',component:'Text',text:{'@path':'/candidate/serial'}},
    ...formFields.map(key=>({id:key,component:'TextField',label:key,value:binding(key)})),
    {id:'request_admission',component:'Button',child:'confirm_label',variant:'primary',action:{event:{
      name:'request_discovery_admission',context:{candidate_id:candidateID,revision,owner:binding('owner'),
        location:binding('location'),role:binding('role')},
    }}},
    {id:'confirm_label',component:'Text',text:'生成纳管预览'},
  ],
}}]
const makeList=()=>[{version:'1.0',createSurface:{surfaceId:'discovery_list',catalogId:catalog,
  components:[
    {id:'root',component:'Column',children:['review_row']},
    {id:'review_row',component:'Button',child:'row_label',action:{event:{name:'review_discovery_candidate',context:{candidate_id:candidate,revision}}}},
    {id:'row_label',component:'Text',text:'核对服务器'},
  ],
}}]

test('A2UI resolves current local field values and emits only the official action envelope',()=>{
  const [surface]=parseSurfaces(makeReview())
  assert.equal(resolveSurfaceText(surface.nodes.get('title').text,surface.data),'TRACE-42')
  assert.equal(createSurfaceAction(surface,'request_admission',surface.data,time),undefined)
  assert.equal(createSurfaceAction(surface,'title',surface.data,time),undefined)
  const current=new Map(surface.data).set('/form/owner',' 值班工程师 ').set('/form/location','机房 A').set('/form/role','测试节点')
  const action=createSurfaceAction(surface,'request_admission',current,time)
  assert.deepEqual(action,{version:'1.0',action:{
    name:'request_discovery_admission',surfaceId:'discovery_review',sourceComponentId:'request_admission',timestamp:time.toISOString(),
    context:{candidate_id:candidate,revision,owner:'值班工程师',location:'机房 A',role:'测试节点'},
  }})
  assert.equal(surface.data.get('/form/owner'),'')
  current.set('/form/owner','用'.repeat(100))
  assert.equal(createSurfaceAction(surface,'request_admission',current,time),undefined)
})

test('A2UI supports bounded server data updates without enabling extra fields or functions',()=>{
  const messages=makeReview()
  messages.push({version:'1.0',updateDataModel:{surfaceId:'discovery_review',path:'/form/owner',value:'值班人员'}})
  const [surface]=parseSurfaces(messages)
  assert.equal(surface.data.get('/form/owner'),'值班人员')
  for(const mutate of [
    create=>create.sendDataModel=true,
    create=>create.components[2].variant='obscured',
    create=>create.components[2].value={'@path':'/form/password'},
    create=>create.components[5].action.event.context.owner={'@path':'/candidate/serial'},
    create=>create.components[5].action.event.context.url='https://example.invalid',
    create=>create.components[5].action.function={call:'allow_anything'},
    create=>create.components[5].action.event.context.candidate_id='bad',
    create=>create.components[5].action.event.userMessage='Untrusted message',
    create=>create.dataModel.api_key='unwanted',
  ]){
    const value=makeReview();mutate(value[0].createSurface)
    assert.equal(parseSurfaces(value),undefined)
  }
  messages[1].updateDataModel.path='/constructor/prototype'
  assert.equal(parseSurfaces(messages),undefined)
})

test('renderer transport uses the current agent route and rejects mismatched candidates before showing confirmation',async()=>{
  const [list]=parseSurfaces(makeList()), [surface]=parseSurfaces(makeReview())
  const review=createSurfaceAction(list,'review_row',list.data,time)
  const confirmation=()=>({
    conversation_id:conversation,state:'confirmation_required',confirmation_id:'f'.repeat(48),expires_at_unix_ms:revision+300000,
    input:{tool:'admit_discovery_candidate',params:{id:candidate},body:{confirmed_serial:'TRACE-42',expected_seen_at:revision,
      owner:'班组',location:'A区',role:'调试'}},
  })
  let receipt={conversation_id:conversation,state:'completed',tool:'review_discovery_candidate',outcome:'show_review',result:{},surface:makeReview()}
  const calls=[]
  const transport=async (...args)=>{calls.push(args);return receipt}
  assert.equal(await sendSurfaceAction(transport,conversation,review,'request-key'),receipt)
  assert.deepEqual(calls[0],['/conversations/'+conversation+'/a2ui/actions',review,'POST',{'X-ClusBot-Request-Key':'request-key'}])
  receipt={...receipt,surface:makeReview('d'.repeat(64))}
  await assert.rejects(sendSurfaceAction(transport,conversation,review,'request-new'),/没有收到与本次操作匹配的回执/)
  assert.equal(parseSurfaceReceipt({conversation_id:otherConversation,surface:makeList()},conversation),undefined)
  const data=new Map(surface.data).set('/form/owner','班组').set('/form/location','A区').set('/form/role','调试')
  const request=createSurfaceAction(surface,'request_admission',data,time)
  receipt=confirmation()
  assert.equal(await sendSurfaceAction(transport,conversation,request,'request-conf'),receipt)
  const readback=async path=>{
    assert.equal(path,'/conversations/'+conversation+'/a2ui/actions/request-conf')
    return receipt
  }
  await assert.rejects(readSurfaceAction(readback,conversation,request,'request-conf'),/没有收到与本次操作匹配的回执/)
  receipt={state:'completed',conversation_id:conversation,tool:'request_discovery_admission',outcome:'show_admission_preview',result:{},surface:makeSurface()}
  assert.equal(await readSurfaceAction(readback,conversation,request,'request-conf'),receipt)
  await assert.rejects(sendSurfaceAction(transport,conversation,request,'request-conf'),/没有收到与本次操作匹配的回执/)
  receipt={...receipt,outcome:'ask_for_details'}
  assert.equal(await sendSurfaceAction(transport,conversation,request,'request-conf'),receipt)
  const mismatch=confirmation()
  mismatch.input.body.owner='另外一个'
  receipt=mismatch
  await assert.rejects(sendSurfaceAction(transport,conversation,request,'request-conf'),/没有收到与本次操作匹配的回执/)
  receipt=confirmation()
  receipt.conversation_id=otherConversation
  await assert.rejects(sendSurfaceAction(transport,conversation,request,'request-conf'),/没有收到与本次操作匹配的回执/)
  const before=calls.length
  await assert.rejects(sendSurfaceAction(transport,'../settings',request,'request-conf'),/无法核对/)
  await assert.rejects(sendSurfaceAction(transport,conversation,{...request,action:{...request.action,name:'delete_asset'}},'request-key'),/无法核对/)
  assert.equal(calls.length,before)
})

const confirmationKey='confirm-request'
const previewID='f'.repeat(48)
const confirmationRun='b'.repeat(32)
const asset='node:discovered:'+'a'.repeat(32)
const confirmationPointer={key:confirmationKey,candidateID:candidate,revision}
const admitted=()=>({conversation_id:conversation,run_id:confirmationRun,tool:'admit_discovery_candidate',state:'completed',
  outcome:'admitted',result:{id:candidate,state:'admitted',asset,revision}})

test('final confirmation sends only the frozen preview ID to the original scoped request; lost ACK is reconciled by GET',async()=>{
  const [surface]=parseSurfaces(makeReview())
  const data=new Map(surface.data).set('/form/owner','班组').set('/form/location','A区').set('/form/role','调试')
  const original=createSurfaceAction(surface,'request_admission',data,time)
  assert.deepEqual(discoveryConfirmationRef(original,confirmationKey),confirmationPointer)
  const originalPath='/conversations/'+conversation+'/a2ui/actions/'+confirmationKey
  const requests=[]
  let terminal
  const confirm=async (path,body)=>{
    requests.push(['POST',path,body])
    terminal=admitted()
    throw new Error('lost ACK')
  }
  await assert.rejects(confirmDiscoveryAdmission(confirm,conversation,confirmationPointer,previewID),/lost ACK/)
  const reread=async path=>{requests.push(['GET',path]);return terminal}
  const recovered=await recoverDiscoveryConfirmation(reread,conversation,confirmationPointer)
  assert.equal(recovered.state,'admitted')
  assert.deepEqual(recovered.receipt.result,terminal.result)
  assert.deepEqual(requests,[['POST',originalPath+'/confirm',{confirmation_id:previewID}],['GET',originalPath]])
})

test('unconfirmed, stale, malformed or cross-conversation readback cannot claim a confirmed write',async()=>{
  const raw={conversation_id:conversation,run_id:confirmationRun,tool:'request_discovery_admission',state:'completed',outcome:'show_admission_preview'}
  let fetch=async ()=>raw
  assert.deepEqual(await recoverDiscoveryConfirmation(fetch,conversation,confirmationPointer),{state:'not_confirmed'})
  await assert.rejects(recoverDiscoveryConfirmation(async()=>({...raw,confirmation_id:previewID}),conversation,confirmationPointer),/没有可核对/)
  for(const mutate of [
    value=>{value.conversation_id=otherConversation},
    value=>{value.result.revision+=1},
    value=>{value.result.id='d'.repeat(64)},
    value=>{value.result.asset='https://example.invalid'},
    value=>{value.result.check_task_id='../task'},
    value=>{value.run_id='bad'},
    value=>{value.confirmation_id=previewID},
  ]){
    const receipt=admitted();mutate(receipt)
    await assert.rejects(recoverDiscoveryConfirmation(async()=>receipt,conversation,confirmationPointer),/没有可核对/)
  }
  const forbidden=async()=>{throw new Error('transport must not run')}
  await assert.rejects(confirmDiscoveryAdmission(forbidden,conversation,confirmationPointer,'bad'),/确认凭据无效/)
  await assert.rejects(confirmDiscoveryAdmission(forbidden,conversation,{...confirmationPointer,key:'../unsafe'},previewID),/原纳管操作/)
})

test('reload restores only the scoped lookup pointer, not approval or private form fields',async()=>{
  async function moduleURL(file,rewrite=code=>code){
    const source=await readFile(new URL('../src/'+file,import.meta.url),'utf8')
    const js=ts.transpileModule(rewrite(source),{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ESNext}}).outputText
    return 'data:text/javascript;base64,'+Buffer.from(js).toString('base64')
  }
  const draftModule=await moduleURL('draftStorage.ts')
  const recoveryModule=await moduleURL('discoveryConfirmation.ts',code=>code.replace("'./draftStorage'",JSON.stringify(draftModule))
    .replace("'./assistantSurface'",JSON.stringify(assistantSurfaceModule)))
  const recovery=await import(recoveryModule)
  const entries=new Map()
  globalThis.window={}
  globalThis.sessionStorage={
    getItem:key=>entries.get(key),setItem:(key,value)=>entries.set(key,value),removeItem:key=>entries.delete(key),
  }
  try{
    recovery.rememberDiscoveryConfirmation('user:alice',conversation,confirmationPointer)
    recovery.rememberDiscoveryConfirmation('user:alice',conversation,confirmationPointer)
    assert.deepEqual(recovery.pendingDiscoveryConfirmations('user:alice',conversation),[confirmationPointer])
    assert.deepEqual(recovery.pendingDiscoveryConfirmations('user:bob',conversation),[])
    assert.deepEqual(recovery.pendingDiscoveryConfirmations('user:alice',otherConversation),[])
    assert.throws(()=>recovery.rememberDiscoveryConfirmation('user:alice',conversation,{...confirmationPointer,confirmation_id:previewID}),/无法安全记录/)
    assert.deepEqual([...entries.values()].map(value=>JSON.parse(value)),[[confirmationPointer]])
    recovery.forgetDiscoveryConfirmation('user:alice',conversation,confirmationPointer)
    assert.deepEqual(recovery.pendingDiscoveryConfirmations('user:alice',conversation),[])
  }finally{
    delete globalThis.window
    delete globalThis.sessionStorage
  }
})

test('accepts only the bounded presentation subset',()=>{
  const parsed=parseSurfaces(makeSurface())
  assert.equal(parsed?.[0].nodes.get('title').text,'<img src=x onerror=alert(1)>')
  for(const mutate of [
    value=>value[0].version='0.9',
    value=>value[0].createSurface.catalogId='https://attacker.example/catalog.json',
    value=>value[1].updateComponents.components[2].action.event.name='delete',
    value=>value[1].updateComponents.components[2].action.event.context={url:'https://attacker.example'},
    value=>value[1].updateComponents.components[2].action.userMessage='send',
    value=>value[1].updateComponents.components[1].text={path:'/secret'},
    value=>value[1].updateComponents.components[1].html='<script>bad()</script>',
    value=>value[1].updateComponents.components[0].children.push('root'),
    value=>value[1].updateComponents.components[0].children.push('missing'),
  ]) {
    const value=makeSurface();mutate(value)
    assert.equal(parseSurfaces(value),undefined)
  }
})

test('details expose only permitted fields and validated local task IDs',()=>{
  const id='a'.repeat(32)
  const rows=assistantDetails('list_tasks',[{id,title:'安全任务',status:'active',scope_label:'测试',password:'secret',url:'https://attacker.example'}])
  assert.deepEqual(rows[0].fields.map(field=>field.label),['标题','状态','范围','任务 ID'])
  assert.equal(rows[0].taskID,id)
  assert.equal(assistantDetails('list_tasks',[{id:'../../settings',title:'任务'}])[0].taskID,undefined)
  assert.deepEqual(assistantDetails('other_tool',[{id,title:'任务'}]),[])
  assert.equal(assistantDetails('list_tasks',Array.from({length:50},()=>({id,title:'任务'}))).length,30)
})

test('confirmation preview describes the exact input and blocks hidden or incomplete data',()=>{
  const preview=previewText({input:{tool:'create_task',body:{text:'整理报告',revision:3,client_request_id:'generated'}}})
  assert.equal(preview.safe,true)
  assert.deepEqual(preview.fields,[{label:'操作',value:'创建任务'},{label:'任务内容',value:'整理报告'},{label:'版本',value:'3'}])
  assert.match(preview.text,/generated/)
  const hidden=previewText({input:{body:{password:'do-not-show'}}})
  assert.equal(hidden.safe,false)
  assert.doesNotMatch(hidden.text,/do-not-show/)
  assert.equal(previewText({input:{body:{text:'x'.repeat(4001)}}}).safe,false)
})

test('form destinations require both a known tool and an exact local target',()=>{
  assert.equal(trustedForm('configure_model_provider','/settings#model-default'),'/settings#model-default')
  assert.equal(trustedForm('configure_model_provider','https://attacker.example'),undefined)
  assert.equal(trustedForm('configure_model_provider','/connections/new'),undefined)
  assert.equal(trustedForm('change_my_password','account_password'),'/account#password')
  assert.equal(trustedForm('manage_accounts','account_management'),'/account#users')
  assert.equal(trustedForm('change_my_password','/account#password'),undefined)
  assert.equal(trustedForm('__proto__','/settings#model-default'),undefined)
})

test('only recognizable tool responses are considered receipts',()=>{
  assert.equal(isAssistantItem({state:'completed',tool:'list_tasks',result:[]}),true)
  assert.equal(isAssistantItem({state:'completed',tool:'list_tasks'}),false)
  assert.equal(isAssistantItem({state:'confirmation_required',confirmation_id:'bad',expires_at_unix_ms:1,input:{tool:'create_task'}}),false)
  assert.equal(isAssistantItem({state:'confirmation_required',confirmation_id:'a'.repeat(48),expires_at_unix_ms:1,input:{tool:'create_task'}}),true)
  assert.equal(isAssistantItem([{name:'list_tasks',description:'查看任务',mode:'read'}]),true)
  assert.equal(isAssistantItem([{name:'../../bad',description:'x',mode:'read'}]),false)
})

test('independent card receipts stay ordered and invalid batches do not consume a draft',()=>{
  const completed={state:'completed',tool:'list_tasks',result:[]}
  const failed={state:'failed',tool:'get_task',message:'未能读取该任务'}
  const confirmation={state:'confirmation_required',confirmation_id:'a'.repeat(48),expires_at_unix_ms:1,input:{tool:'pause_task'}}
  const receipt={state:'batch_completed',items:[completed,failed,confirmation]}
  assert.equal(isAssistantItem(receipt),true)
  assert.deepEqual(assistantReplyCards(receipt),[completed,failed,confirmation])
  assert.deepEqual(assistantReplyCards(completed),[completed])
  assert.equal(isAssistantItem({...receipt,items:[completed]}),false)
  assert.equal(isAssistantItem({...receipt,items:Array(7).fill(completed)}),false)
  assert.equal(isAssistantItem({...receipt,items:[completed,{...confirmation,confirmation_id:'wrong'}]}),false)
  assert.equal(isAssistantItem({...receipt,items:[completed,{...failed,message:{text:'invalid'}}]}),false)
})

test('model setup receipts accept only local identifiers and revisions',()=>{
  const id='a'.repeat(32)
  const role={state:'form_required',tool:'role_binding_prefill',form:'model_role_binding',prefill:{role:'task_worker',profile_id:id,profile_revision:2,provider_revision:3}}
  const provider={state:'form_required',tool:'provider_credentials_prefill',form:'model_provider_credentials',prefill:{provider_id:id,provider_revision:3}}
  assert.equal(isAssistantItem({state:'batch_completed',items:[role,provider]}),true)
  assert.equal(isAssistantItem({...role,prefill:{role:'unknown',profile_id:id,profile_revision:2,provider_revision:3}}),false)
  assert.equal(isAssistantItem({...role,prefill:{role:'task_worker',profile_id:id,profile_revision:2}}),false)
  assert.equal(isAssistantItem({...provider,prefill:{provider_id:'../../settings',provider_revision:3}}),false)
  assert.equal(isAssistantItem({...provider,prefill:{provider_id:id,provider_revision:3,api_key:'secret'}}),false)
  assert.equal(isAssistantItem({...provider,form:'https://attacker.example'}),false)
  assert.equal(isAssistantItem({...provider,prefill:{}}),true)
})
