import test from 'node:test'
import assert from 'node:assert/strict'
import {fileURLToPath,pathToFileURL} from 'node:url'
import {build} from 'esbuild'

// Optional real Chromium test. No artifacts or persistent profile: Playwright
// owns its ephemeral profile, assets are bundled in memory and all HTTP is
// intercepted. The production Go HTTP/MCP round trip is covered separately.
const modulePath=process.env.CLUSBOT_PLAYWRIGHT_MODULE
const executablePath=process.env.CLUSBOT_CHROMIUM
test('chat card sends only A2UI choices; trusted submit performs each separate MCP onboarding step', {skip:!modulePath||!executablePath},async()=>{
  const {chromium}=await import(pathToFileURL(modulePath).href)
  const c='1'.repeat(32),r='2'.repeat(32),cid='3'.repeat(32),token='4'.repeat(32),version='5'.repeat(64)
  const initial='mcp_initial',later=()=> 'mcp_postsave_'+connectionRevision
  const base='/api/v1/conversations/'+c+'/mcp-setup/'+r
  const bundle=await build({stdin:{contents:`import React from 'react';import {createRoot} from 'react-dom/client';import {MessageHistory} from './src/MessageHistory';createRoot(document.body.appendChild(document.createElement('main'))).render(<MessageHistory conversationID="${c}" messages={[{id:1,run_id:"${r}",role:"user",text:"接入 MCP"},{id:2,run_id:"${r}",role:"assistant",text:"请选择连接方式。",mcp_onboarding:true}]} mcpSetup/>);`,
    resolveDir:fileURLToPath(new URL('../',import.meta.url)),loader:'tsx'},write:false,bundle:true,format:'iife',jsx:'automatic',loader:{'.css':'empty','.svg':'dataurl','.png':'dataurl'},define:{'process.env.NODE_ENV':'"production"'}})
  const js=bundle.outputFiles[0].text,posts=[],bad=[]
  let connectionRevision=0,bound=false
  function surface(id,ops){
    const children=['title',...ops.map(o=>o[0])]
    return [{version:'1.0',createSurface:{surfaceId:id,catalogId:'https://a2ui.org/specification/v1_0/catalogs/basic/catalog.json',components:[
      {id:'root',component:'Column',children},{id:'title',component:'Text',text:'MCP 接入'},
      ...ops.flatMap(([key,label])=>[{id:'lbl_'+key,component:'Text',text:label},{id:key,component:'Button',child:'lbl_'+key,variant:'primary',
        action:{event:connectionRevision===0?{name:'choose_mcp_transport',context:{transport:key,revision:1}}:{name:'request_mcp_setup_step',context:{step:key,revision:1,connection_revision:connectionRevision}}}}]),
    ]}}]
  }
  const state=()=>({conversation_id:c,run_id:r,...(connectionRevision===0?{state:'choose_transport',surface:surface(initial,[['stdio','本地程序'],['http','远程服务']])}:
    {state:'saved',status:connectionRevision===1?'untested':'ready',revision:connectionRevision,connection_id:cid,...(bound?{bound_tool:'fixture_tool',binding_status:'locally_ready'}:{}),
      surface:surface(later(),[['probe','核对握手'],...(connectionRevision>=2?[['permissions','审阅授权']]:[]),...(connectionRevision>=3?[['binding','选择会话工具']]:[])])})})
  const browser=await chromium.launch({executablePath,headless:true,args:['--disable-background-networking']})
  const page=await browser.newPage()
  try{
    page.setDefaultTimeout(6000)
    await page.route('**/*',async route=>{
      const req=route.request(),u=new URL(req.url()),p=u.pathname
      if(u.origin!=='http://127.0.0.1:17396'){bad.push(u.origin);return route.abort()}
      if(p==='/')return route.fulfill({contentType:'text/html',body:'<!doctype html><html lang="zh"><body><script src="/app.js"></script></body></html>'})
      if(p==='/app.js')return route.fulfill({contentType:'application/javascript',body:js})
      if(p==='/api/v1/conversations/'+c+'/history')return route.fulfill({json:{items:[{id:1,run_id:r,role:'user',text:'接入 MCP'},{id:2,run_id:r,role:'assistant',text:'请选择连接方式。',mcp_onboarding:true}]}})
      if(p===base&&req.method()==='GET')return route.fulfill({json:state()})
      if(!p.startsWith(base+'/')){bad.push(p);return route.abort()}
      const b=req.postDataJSON();posts.push({p,b})
      if(p===base+'/actions'){
        const ctx=b.action.context
        const payload=ctx.transport?{transport:ctx.transport}:{step:ctx.step,connection_revision:connectionRevision}
        if(ctx.step==='probe')payload.probe_target={name:'fixture private name',transport:'http',target:'https://service.private.example/mcp',argument_count:0}
        if(ctx.step==='permissions'||ctx.step==='binding')payload.tools=[{name:'fixture_tool',version,allowed:connectionRevision>=3,read_only:connectionRevision>=3,can_read:true}]
        if(ctx.step==='binding')payload.binding_revision=0
        return route.fulfill({json:{conversation_id:c,run_id:r,state:'open_trusted_form',revision:1,request_key:req.headers()['x-clusbot-request-key'],form_proof:token,...payload}})
      }
      if(p===base+'/preview'){
        return route.fulfill({json:{state:'configuration_reviewed',conversation_id:c,run_id:r,request_key:b.request_key,revision:b.revision,
          preview_id:token,name:b.name,transport:b.config.transport,target:b.config.endpoint,argument_count:0,credential_reference_count:1}})
      }
      if(p===base+'/connection'){assert.equal(b.preview_id,token);connectionRevision=1}
      else if(p===base+'/operation'){if(b.step==='binding')bound=true;else connectionRevision++}else{bad.push(p);return route.abort()}
      return route.fulfill({json:state()})
    })
    await page.goto('http://127.0.0.1:17396/')
    await page.getByRole('button',{name:'远程服务'}).click()
    await page.getByLabel('连接名称').fill('fixture private name')
    await page.getByLabel('服务地址').fill('https://service.private.example/mcp')
    await page.getByLabel('令牌所在环境变量').fill('PRIVATE_TOKEN_NAME')
    assert.equal(posts.length,1)
    await page.getByRole('button',{name:'核对连接配置'}).click()
    await page.getByRole('heading',{name:'确认登记连接'}).waitFor()
    assert.equal(posts.filter(x=>x.p.endsWith('/connection')).length,0)
    await page.getByRole('button',{name:'确认登记连接'}).click()
    await page.getByRole('button',{name:'核对握手'}).click()
    await page.getByText('https://service.private.example/mcp',{exact:true}).waitFor()
    assert.equal(posts.filter(x=>x.p.endsWith('/operation')).length,0)
    await page.getByRole('button',{name:'开始握手'}).click()
    await page.getByRole('button',{name:'审阅授权'}).click()
    await page.getByLabel('工具',{exact:true}).selectOption('fixture_tool')
    await page.getByLabel('权限',{exact:true}).selectOption('read_only')
    await page.getByRole('button',{name:'确认修改实例授权'}).click()
    await page.getByRole('button',{name:'选择会话工具'}).click()
    await page.getByLabel('工具',{exact:true}).selectOption('fixture_tool')
    await page.getByRole('button',{name:'确认绑定本会话'}).click()
    await page.getByText('本会话已绑定只读工具：fixture_tool',{exact:false}).waitFor()
    await page.reload()
    await page.getByText('本会话已绑定只读工具：fixture_tool',{exact:false}).waitFor()
    assert.equal(posts.filter(x=>x.p.endsWith('/connection')).length,1)
    assert.deepEqual(posts.filter(x=>x.p.endsWith('/operation')).map(x=>x.b.step),['probe','permissions','binding'])
    const events=posts.filter(x=>x.p.endsWith('/actions')).map(x=>x.b)
    assert.equal(events.length,4)
    for(const item of events){assert.equal(item.version,'1.0');assert.deepEqual(Object.keys(item.action).sort(),['context','name','sourceComponentId','surfaceId','timestamp']);assert.doesNotMatch(JSON.stringify(item),/fixture_tool|service\.private|PRIVATE_TOKEN_NAME|fixture private name/)}
    assert.deepEqual(bad,[])
  }catch(e){throw new Error(String(e)+'\n'+await page.locator('main').innerText()+'\nPOST routes: '+posts.map(x=>x.p).join(', '),{cause:e})}finally{await browser.close()}
})
