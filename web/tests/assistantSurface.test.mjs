import assert from 'node:assert/strict'
import {readFile} from 'node:fs/promises'
import test from 'node:test'
import ts from 'typescript'

const source=await readFile(new URL('../src/assistantSurface.ts',import.meta.url),'utf8')
const js=ts.transpileModule(source,{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ESNext}}).outputText
const {parseSurfaces,assistantDetails,previewText,trustedForm,isAssistantItem}=await import('data:text/javascript;base64,'+Buffer.from(js).toString('base64'))
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
  assert.equal(trustedForm('change_my_password','account_password'),undefined)
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
