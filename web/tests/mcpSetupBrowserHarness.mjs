import assert from 'node:assert/strict'
import {fileURLToPath,pathToFileURL} from 'node:url'

// Invoked by TestMCPOnboardingBrowserRoundTrip. The production frontend is
// built by Vite but kept in memory; the Go handler serves it and all real APIs.
if(process.argv[2]==='bundle'){
  const {build}=await import('vite')
  const root=fileURLToPath(new URL('../',import.meta.url))
  const outputs=await build({root,configFile:false,logLevel:'error',build:{write:false,reportCompressedSize:false}})
  const assets={}
  for(const generation of Array.isArray(outputs)?outputs:[outputs]){
    for(const item of generation.output){
      assets[item.fileName]=Buffer.from(item.type==='chunk'?item.code:item.source).toString('base64')
    }
  }
  assert.ok(assets['index.html'])
  process.stdout.write(JSON.stringify(assets))
}else if(process.argv[2]==='browse'){
  const sink=[]
  for await(const chunk of process.stdin)sink.push(chunk)
  const {origin,target,username,password,transport,readTool,writeTool,arguments:commandArgs,envNames,lookupKey}=JSON.parse(Buffer.concat(sink).toString())
  assert.match(origin,/^http:\/\/127\.0\.0\.1:\d+$/)
  assert.ok(transport==='stdio'||transport==='http')
  if(transport==='http')assert.match(target,/^http:\/\/127\.0\.0\.1:\d+$/)
  else assert.ok(target.startsWith('/')&&commandArgs[0]==='mcp')
  const {chromium}=await import(pathToFileURL(process.env.CLUSBOT_PLAYWRIGHT_MODULE).href)
  const browser=await chromium.launch({headless:true,executablePath:process.env.CLUSBOT_CHROMIUM,args:['--disable-background-networking','--disable-sync']})
  let page
  const requests=[],failed=[],outside=[]
  try{
    page=await browser.newPage({serviceWorkers:'block',viewport:{width:1360,height:960}})
    page.setDefaultTimeout(11000)
    await page.route('**/*',route=>{
      const url=new URL(route.request().url())
      if(url.origin===origin)return route.continue()
      outside.push(url.origin)
      return route.abort()
    })
    page.on('request',req=>{
      const url=new URL(req.url())
      if(req.method()==='POST'&&url.pathname.startsWith('/api/v1/conversations')){
        requests.push({path:url.pathname,body:req.postDataJSON()})
      }
    })
    page.on('response',response=>{
      const path=new URL(response.url()).pathname
      if(path.startsWith('/api/')&&response.status()>=400&&!(path==='/api/v1/session'&&response.status()===401))failed.push(path+':'+response.status())
    })
    await page.goto(origin+'/chat')
    await page.getByLabel('用户名',{exact:true}).fill(username)
    await page.getByLabel('密码',{exact:true}).fill(password)
    await page.getByRole('button',{name:'登录进入个人助手'}).click()
    // Real UI entry point and production composer submit, not test mounting
    // an isolated card or pre-seeding the conversation.
    const text=page.getByRole('textbox',{name:/输入消息/})
    await text.fill('我有一个 MCP 要接入')
    await page.getByRole('button',{name:'发送消息',exact:true}).click()
    await page.waitForURL(/\/chat\/[0-9a-f]{32}$/)
    const conv=new URL(page.url()).pathname.split('/').pop()
    const card=page.getByRole('article',{name:'会话 MCP 接入'})
    await card.getByRole('button',{name:transport==='stdio'?/本地程序/:/远程服务/}).click()
    await card.getByLabel('连接名称').fill('只在受信表单可见的测试连接')
    if(transport==='stdio'){
      await card.getByLabel('程序绝对路径').fill(target)
      await card.getByLabel('参数（每行一个）',{exact:true}).fill(commandArgs.join('\n'))
      await card.getByLabel('环境变量名（每行一个，不填值）',{exact:true}).fill(envNames.join('\n'))
    }else{
      await card.getByLabel('服务地址').fill(target)
      await card.getByLabel('令牌所在环境变量').fill('CLUSBOT_TEST_BROWSER_MCP_TOKEN')
    }
    assert.equal(requests.filter(r=>r.path.endsWith('/connection')).length,0)
    const field=card.getByLabel(transport==='stdio'?'程序绝对路径':'服务地址')
    await field.fill(transport==='stdio'?'invalid-relative-path':'http://example.invalid/mcp')
    await card.getByRole('button',{name:'核对连接配置'}).click()
    await card.getByRole('alert').waitFor()
    await page.waitForFunction(()=>document.activeElement?.getAttribute('role')==='alert')
    assert.equal(requests.filter(r=>r.path.endsWith('/connection')||r.path.endsWith('/operation')).length,0)
    await field.fill(target)
    await card.getByRole('button',{name:'核对连接配置'}).click()
    await card.getByRole('heading',{name:'确认登记连接'}).waitFor()
    await card.getByText(target,{exact:true}).waitFor()
    await page.waitForFunction(()=>document.activeElement?.textContent==='确认登记连接')
    assert.equal(requests.filter(r=>r.path.endsWith('/connection')||r.path.endsWith('/operation')).length,0)
    await card.getByRole('button',{name:'返回修改'}).click()
    assert.equal(await card.getByLabel('连接名称').inputValue(),'只在受信表单可见的测试连接')
    await card.getByRole('button',{name:'核对连接配置'}).click()
    await card.getByRole('button',{name:'确认登记连接'}).click()
    await card.getByText(/已登记连接，尚未握手/).waitFor()
    await card.getByRole('button',{name:'核对握手与工具目录'}).click()
    await card.getByText(target,{exact:true}).waitFor()
    assert.equal(requests.filter(r=>r.path.endsWith('/operation')).length,0)
    await card.getByRole('button',{name:'开始握手'}).click()
    await card.getByText(/已取得工具目录/).waitFor()
    await card.getByRole('button',{name:'审阅实例工具授权'}).click()
    await card.getByRole('combobox',{name:'工具',exact:true}).selectOption(writeTool)
    await card.getByRole('combobox',{name:'权限',exact:true}).locator('option[value="read_only"]').waitFor({state:'detached'})
    await card.getByRole('combobox',{name:'工具',exact:true}).selectOption(readTool)
    await card.getByRole('combobox',{name:'权限',exact:true}).selectOption('read_only')
    await card.getByRole('button',{name:'确认修改实例授权'}).click()
    await card.getByRole('button',{name:'选择本会话的只读工具'}).click()
    await card.getByRole('combobox',{name:'工具',exact:true}).selectOption(readTool)
    await card.getByRole('button',{name:'确认绑定本会话'}).click()
    await card.getByText('本会话已绑定只读工具：'+readTool,{exact:false}).waitFor()
    await page.reload()
    await card.getByText('本会话已绑定只读工具：'+readTool,{exact:false}).waitFor()
    const selection=await page.evaluate(async id=>{
      const r=await fetch('/api/v1/conversations/'+id+'/mcp-selection')
      return r.json()
    },conv)
    assert.equal(selection.selection.tool_name,readTool)
    assert.equal(selection.local_validity,'locally_ready')
    assert.equal(requests.filter(r=>r.path==='/api/v1/conversations').length,1)
    assert.equal(requests.filter(r=>r.path.endsWith('/connection')).length,1)
    assert.equal(requests.filter(r=>r.path.endsWith('/preview')).length,3)
    assert.match(requests.find(r=>r.path.endsWith('/connection')).body.preview_id,/^[a-f0-9]{32}$/)
    assert.deepEqual(requests.filter(r=>r.path.endsWith('/operation')).map(r=>r.body.step),['probe','permissions','binding'])
    const events=requests.filter(r=>r.path.endsWith('/actions'))
    assert.equal(events.length,4)
    const expects=[
      ['choose_mcp_transport',{transport}],
      ['request_mcp_setup_step',{step:'probe'}],
      ['request_mcp_setup_step',{step:'permissions'}],
      ['request_mcp_setup_step',{step:'binding'}],
    ]
    for(const [idx,{body}] of events.entries()){
      assert.equal(body.version,'1.0')
      assert.equal(body.action.name,expects[idx][0])
      const {revision,connection_revision,...choice}=body.action.context
      assert.ok(Number.isSafeInteger(revision)&&revision>0)
      if(idx===0)assert.equal(connection_revision,undefined)
      else assert.ok(Number.isSafeInteger(connection_revision)&&connection_revision>0)
      assert.deepEqual(choice,expects[idx][1])
      assert.deepEqual(Object.keys(body.action).sort(),['context','name','sourceComponentId','surfaceId','timestamp'])
      assert.doesNotMatch(JSON.stringify(body),/fixture_(read|write)|task_status|CLUS_ROOT|CLUS_CONFIG_FILE|CLUSBOT_TEST_BROWSER_MCP_TOKEN|只在受信表单可见/)
      assert.ok(!JSON.stringify(body).includes(target))
    }
    let run
    if(lookupKey){
      // No approval is required to actually use the granted, conversation-pinned
      // read. This only does a local lookup of an absent fixture key.
      const resultText='Cluskit2 本地只读查询：未找到该幂等键（NOT_FOUND）。'
      await text.fill('请在当前对话安排一个只读查询：核对本地 Cluskit2 测试幂等键 '+lookupKey+' 是否存在，并汇报原始结果。')
      const started=page.waitForResponse(r=>new URL(r.url()).pathname==='/api/v1/conversations/'+conv+'/messages'&&r.request().method()==='POST')
      await page.getByRole('button',{name:'发送消息',exact:true}).click()
      const accepted=await started
      assert.equal(accepted.status(),202)
      const receipt=await accepted.json()
      assert.equal(receipt.conversation_id,conv)
      assert.match(receipt.task_id,/^[0-9a-f]{32}$/)
      assert.match(receipt.run_id,/^[0-9a-f]{32}$/)
      run=receipt.run_id
      await page.getByText(resultText,{exact:true}).first().waitFor({timeout:21000})
      await page.reload()
      await page.getByText(resultText,{exact:true}).first().waitFor()
      const record=await page.evaluate(async id=>{
        const response=await fetch('/api/v1/runs/'+id)
        return response.json()
      },run)
      assert.equal(record.conversation_id,conv)
      assert.equal(record.status,'completed')
      assert.equal(record.reads.length,1)
      const [read]=record.reads
      assert.equal(read.tool_name,readTool)
      assert.equal(read.status,'completed')
      assert.notEqual(read.result.is_error,true)
      assert.deepEqual(read.result.structured,{found:false,state:'NOT_FOUND',idempotency_key:lookupKey})
      assert.ok(record.messages.some(m=>m.role==='assistant'&&m.text===resultText))
    }
    assert.deepEqual(outside,[])
    assert.equal(failed.length,1)
    assert.match(failed[0],/\/preview:400$/)
    process.stdout.write(JSON.stringify({conversation:conv,run,posts:requests.length}))
  }catch(e){
    // Fixture-only data. Never echo submitted form or model payloads.
    const alerts=page?await page.getByRole('alert').allTextContents():[]
    if(page)alerts.push(...await page.getByRole('article',{name:'会话 MCP 接入'}).getByRole('status').allTextContents())
    throw new Error(String(e)+'; URL: '+page?.url()+'; API: '+JSON.stringify(failed)+'; alerts: '+JSON.stringify(alerts),{cause:e})
  }finally{await browser.close()}
}else if(process.argv[2]==='browse-discovery-schedule'){
  const chunks=[]
  for await(const chunk of process.stdin)chunks.push(chunk)
  const {origin,prompt}=JSON.parse(Buffer.concat(chunks).toString())
  assert.match(origin,/^http:\/\/127\.0\.0\.1:\d+$/)
  const {chromium}=await import(pathToFileURL(process.env.CLUSBOT_PLAYWRIGHT_MODULE).href)
  const browser=await chromium.launch({headless:true,executablePath:process.env.CLUSBOT_CHROMIUM,args:['--disable-background-networking','--disable-sync']})
  try{
    const page=await browser.newPage({serviceWorkers:'block',viewport:{width:1280,height:900}})
    page.setDefaultTimeout(12000)
    const actions=[],outside=[],unexpected=[]
    await page.route('**/*',async route=>{
      const request=route.request(),url=new URL(request.url())
      if(url.origin!==origin){outside.push(url.origin);return route.abort()}
      if(request.method()==='POST'&&/\/schedule-card\/[a-f0-9]{32}\/actions$/.test(url.pathname))
        actions.push({path:url.pathname,data:request.postDataJSON()})
      else if(url.pathname.startsWith('/api/v1/')&&request.method()!=='GET'&&!['/api/v1/session','/api/v1/conversations'].includes(url.pathname))
        unexpected.push(url.pathname)
      return route.continue()
    })
    await page.goto(origin+'/chat')
    await page.getByLabel('用户名',{exact:true}).fill('admin')
    await page.getByLabel('密码',{exact:true}).fill('correct horse battery staple')
    await page.getByRole('button',{name:'登录进入个人助手'}).click()
    await page.getByRole('textbox',{name:/输入消息/}).fill(prompt)
    await page.getByRole('button',{name:'发送消息',exact:true}).click()
    await page.waitForURL(/\/chat\/[a-f0-9]{32}$/)
    const conversation=new URL(page.url()).pathname.split('/').pop()
    const card=page.getByRole('article',{name:'对话定时发现状态',exact:true})
    const show=card.getByRole('button',{name:'展开计划和最近回执',exact:true})
    await show.waitFor()
    const result=card.getByText(/这次发现已记录 1 条观测/)
    assert.equal(await result.count(),0)
    await show.press('Enter')
    await result.waitFor()
    await card.getByText(/记录中的下一计划时点：.+ UTC/).waitFor()
    await card.getByText(/每 60 秒/).waitFor()
    assert.equal(await card.getByRole('link').count(),0)
    assert.equal(await card.getByText(/fixture-private-scope|192\.0\.2\.184/).count(),0)
    await page.reload()
    await show.waitFor()
    assert.equal(await result.count(),0)
    assert.equal(actions.length,1)
    const [event]=actions
    assert.match(event.path,new RegExp('^/api/v1/conversations/'+conversation+'/schedule-card/[a-f0-9]{32}/actions$'))
    assert.equal(event.data.version,'1.0')
    assert.equal(event.data.action.name,'preview_discovery_schedule')
    assert.match(event.data.action.sourceComponentId,/^scheduleview_[a-f0-9]{32}$/)
    assert.deepEqual(Object.keys(event.data.action.context).sort(),['revision','schedule_slot'])
    assert.equal(event.data.action.context.schedule_slot,'1')
    assert.deepEqual(unexpected,[])
    assert.deepEqual(outside,[])
    process.stdout.write(JSON.stringify({verified:true,actions:actions.length}))
  }finally{await browser.close()}
}else if(process.argv[2]==='browse-benchmark-comparison'){
  const chunks=[]
  for await(const chunk of process.stdin)chunks.push(chunk)
  const {origin,prompt}=JSON.parse(Buffer.concat(chunks).toString())
  assert.match(origin,/^http:\/\/127\.0\.0\.1:\d+$/)
  const {chromium}=await import(pathToFileURL(process.env.CLUSBOT_PLAYWRIGHT_MODULE).href)
  const browser=await chromium.launch({headless:true,executablePath:process.env.CLUSBOT_CHROMIUM,args:['--disable-background-networking','--disable-sync']})
  try{
    const page=await browser.newPage({serviceWorkers:'block',viewport:{width:1280,height:900}})
    page.setDefaultTimeout(12000)
    const actions=[],outside=[],unexpected=[]
    await page.route('**/*',async route=>{
      const request=route.request(),url=new URL(request.url())
      if(url.origin!==origin){outside.push(url.origin);return route.abort()}
      if(request.method()==='POST'&&/\/report-card\/[a-f0-9]{32}\/actions$/.test(url.pathname))
        actions.push({path:url.pathname,data:request.postDataJSON()})
      else if(url.pathname.startsWith('/api/v1/')&&request.method()!=='GET'&&!['/api/v1/session','/api/v1/conversations'].includes(url.pathname))
        unexpected.push(url.pathname)
      return route.continue()
    })
    await page.goto(origin+'/chat')
    await page.getByLabel('用户名',{exact:true}).fill('admin')
    await page.getByLabel('密码',{exact:true}).fill('correct horse battery staple')
    await page.getByRole('button',{name:'登录进入个人助手'}).click()
    await page.getByRole('textbox',{name:/输入消息/}).fill(prompt)
    await page.getByRole('button',{name:'发送消息',exact:true}).click()
    await page.waitForURL(/\/chat\/[a-f0-9]{32}$/)
    const conversation=new URL(page.url()).pathname.split('/').pop()
    const card=page.getByRole('article',{name:'对话基准报告对比',exact:true})
    const show=card.getByRole('button',{name:'核对并展开报告对比',exact:true})
    await show.waitFor()
    const result=card.getByText('基线 100 GB/s；本次 125 GB/s；差值 25 GB/s',{exact:true})
    assert.equal(await result.count(),0)
    assert.equal(actions.length,0)
    await show.press('Enter')
    await result.waitFor()
    await card.getByText(/差值不代表原因/).waitFor()
    assert.equal(await card.getByRole('link').count(),0)
    assert.equal(await card.getByText(/private normalized synthetic/).count(),0)
    await page.reload()
    await show.waitFor()
    assert.equal(await result.count(),0)
    assert.equal(actions.length,1)
    const [event]=actions
    assert.match(event.path,new RegExp('^/api/v1/conversations/'+conversation+'/report-card/[a-f0-9]{32}/actions$'))
    assert.equal(event.data.version,'1.0')
    assert.equal(event.data.action.name,'preview_benchmark_comparison')
    assert.match(event.data.action.sourceComponentId,/^reportcompare_[a-f0-9]{32}$/)
    assert.deepEqual(Object.keys(event.data.action.context).sort(),['report_slot','revision'])
    assert.equal(event.data.action.context.report_slot,'1')
    assert.deepEqual(unexpected,[])
    assert.deepEqual(outside,[])
    process.stdout.write(JSON.stringify({verified:true,actions:actions.length}))
  }finally{await browser.close()}
}else if(process.argv[2]==='browse-memory-provenance'){
  const chunks=[]
  for await(const chunk of process.stdin)chunks.push(chunk)
  const {origin,conversation,run,title}=JSON.parse(Buffer.concat(chunks).toString())
  assert.match(origin,/^http:\/\/127\.0\.0\.1:\d+$/)
  assert.match(conversation,/^[a-f0-9]{32}$/)
  assert.match(run,/^[a-f0-9]{32}$/)
  const {chromium}=await import(pathToFileURL(process.env.CLUSBOT_PLAYWRIGHT_MODULE).href)
  const browser=await chromium.launch({headless:true,executablePath:process.env.CLUSBOT_CHROMIUM,args:['--disable-background-networking','--disable-sync']})
  try{
    const page=await browser.newPage({serviceWorkers:'block',viewport:{width:1280,height:900}})
    page.setDefaultTimeout(12000)
    const reads=[],actions=[],outside=[],writes=[]
    const expected='/api/v1/conversations/'+conversation+'/memory-card/'+run
    await page.route('**/*',async route=>{
      const request=route.request(),url=new URL(request.url())
      if(url.origin!==origin){outside.push(url.origin);return route.abort()}
      if(url.pathname===expected&&request.method()==='GET')reads.push(url.pathname)
      else if(url.pathname===expected+'/actions'&&request.method()==='POST')actions.push(request.postDataJSON())
      else if(url.pathname.startsWith('/api/v1/')&&request.method()!=='GET'&&url.pathname!=='/api/v1/session')writes.push(url.pathname)
      return route.continue()
    })
    await page.goto(origin+'/chat')
    await page.getByLabel('用户名',{exact:true}).fill('admin')
    await page.getByLabel('密码',{exact:true}).fill('correct horse battery staple')
    await page.getByRole('button',{name:'登录进入个人助手'}).click()
    await page.getByRole('textbox',{name:/输入消息/}).waitFor()
    await page.goto(origin+'/chat/'+conversation)
    const summary=page.locator('summary').filter({hasText:'本轮参考了 3 条记忆'})
    const card=page.getByRole('article',{name:'对话记忆来源',exact:true})
    await summary.waitFor()
    assert.equal(reads.length,0)
    assert.equal(actions.length,0)
    await summary.focus()
    await page.keyboard.press('Enter')
    await card.getByText(/来源位于其他会话，此处不展开/).waitFor()
    assert.equal(await card.getByText(/不应解释为 HTML/).count(),0)
    const choose=card.getByRole('button',{name:new RegExp('核对来源：'+title)})
    await choose.focus()
    await page.keyboard.press('Enter')
    await card.getByText(/不应解释为 HTML/).waitFor()
    await card.getByText(/仅显示前 180 个字符/).waitFor()
    assert.equal(await card.locator('em').count(),0)
    assert.equal(await card.getByText(/另一个会话的合成来源|END_AFTER_FRAGMENT/).count(),0)
    assert.equal(actions.length,1)
    const [event]=actions
    assert.equal(event.version,'1.0')
    assert.equal(event.action.name,'review_memory_provenance')
    assert.match(event.action.surfaceId,/^memorycard_/)
    assert.match(event.action.sourceComponentId,/^memorysource_[a-f0-9]{32}$/)
    assert.deepEqual(Object.keys(event.action.context).sort(),['memory_slot','revision'])
    assert.ok(!JSON.stringify(event).includes(title))
    await summary.press('Enter')
    await page.locator('[data-read-card="memory-card"]').waitFor({state:'detached'})
    await summary.press('Enter')
    await choose.waitFor()
    assert.equal(await card.getByText(/不应解释为 HTML/).count(),0)
    await page.reload()
    await summary.waitFor()
    assert.equal(await card.count(),0)
    assert.equal(actions.length,1)
    assert.ok(reads.length>=2)
    assert.deepEqual(writes,[])
    assert.deepEqual(outside,[])
    process.stdout.write(JSON.stringify({verified:true,reads:reads.length,actions:actions.length}))
  }finally{await browser.close()}
}else if(process.argv[2]==='browse-skill-binding-preview'){
  const chunks=[]
  for await(const chunk of process.stdin)chunks.push(chunk)
  const {origin,title,prompt}=JSON.parse(Buffer.concat(chunks).toString())
  assert.match(origin,/^http:\/\/127\.0\.0\.1:\d+$/)
  const {chromium}=await import(pathToFileURL(process.env.CLUSBOT_PLAYWRIGHT_MODULE).href)
  const browser=await chromium.launch({headless:true,executablePath:process.env.CLUSBOT_CHROMIUM,args:['--disable-background-networking','--disable-sync']})
  try{
    const page=await browser.newPage({serviceWorkers:'block',viewport:{width:1280,height:900}})
    page.setDefaultTimeout(12000)
    const actions=[],confirmations=[],unexpected=[],outside=[]
    await page.route('**/*',async route=>{
      const request=route.request(),url=new URL(request.url())
      if(url.origin!==origin){outside.push(url.origin);return route.abort()}
      if(request.method()==='POST'&&url.pathname.includes('/skill-card/')&&url.pathname.endsWith('/actions')){
        actions.push({path:url.pathname,event:request.postDataJSON()})
        const saved=await route.fetch()
        assert.equal(saved.status(),200)
        assert.match(await saved.text(),/仅影响以后此会话；尚未绑定/)
        return route.abort('failed')
      }
      if(request.method()==='PUT'&&url.pathname.endsWith('/skill-binding')){
        confirmations.push({path:url.pathname,input:request.postDataJSON()})
        const saved=await route.fetch()
        assert.equal(saved.status(),200)
        const ack=await saved.json()
        assert.equal(ack.binding_revision,1)
        // The database committed but the browser never saw the response.
        return route.abort('failed')
      }
      if(request.method()!=='GET'&&url.pathname.startsWith('/api/v1/')&&!['/api/v1/session','/api/v1/conversations'].includes(url.pathname))
        unexpected.push(url.pathname)
      return route.continue()
    })
    await page.goto(origin+'/chat')
    await page.getByLabel('用户名',{exact:true}).fill('admin')
    await page.getByLabel('密码',{exact:true}).fill('correct horse battery staple')
    await page.getByRole('button',{name:'登录进入个人助手'}).click()
    await page.getByRole('textbox',{name:/输入消息/}).fill(prompt)
    await page.getByRole('button',{name:'发送消息',exact:true}).click()
    await page.waitForURL(/\/chat\/[a-f0-9]{32}$/)
    const conversation=new URL(page.url()).pathname.split('/').pop()
    const card=page.getByRole('article',{name:'对话Skill 绑定预览'})
    const select=card.getByRole('button',{name:'预览绑定：'+title+' · 第 1 版（1）',exact:true})
    await select.waitFor()
    assert.equal(await card.getByText(/拟选用/).count(),0)
    await page.reload()
    await select.waitFor()
    assert.equal(actions.length,0)
    await select.focus()
    await page.keyboard.press('Enter')
    await card.getByRole('button',{name:'重新读取Skill 绑定预览与回执',exact:true}).click()
    const receipt=card.getByText('拟选用：'+title+' · 第 1 版。仅影响以后此会话；尚未绑定。',{exact:true})
    await receipt.waitFor()
    await page.reload()
    await receipt.waitFor()
    assert.equal(await select.count(),0)
    assert.equal(actions.length,1)
    const [{path,event}]=actions
    assert.equal(event.version,'1.0')
    assert.equal(event.action.name,'preview_conversation_skill')
    assert.match(event.action.sourceComponentId,/^skillpreview_[a-f0-9]{32}$/)
    assert.deepEqual(Object.keys(event.action.context).sort(),['revision','skill_slot'])
    assert.equal(event.action.context.skill_slot,'1')
    assert.ok(!JSON.stringify(event).includes(title))
    assert.equal(confirmations.length,0)
    const match=path.match(/^\/api\/v1\/conversations\/([a-f0-9]{32})\/skill-card\/([a-f0-9]{32})\/actions$/)
    assert.equal(match?.[1],conversation)
    const save=card.getByRole('button',{name:'确认绑定：'+title+' · 第 1 版',exact:true})
    await save.waitFor()
    assert.equal(confirmations.length,0)
    await save.focus()
    await page.keyboard.press('Enter')
    await card.getByRole('button',{name:'重新读取Skill 绑定预览与回执',exact:true}).click()
    const applied=card.getByText('已保存本次 Skill 绑定。仅影响以后此会话。',{exact:true})
    await applied.waitFor()
    await page.reload()
    await applied.waitFor()
    assert.equal(await save.count(),0)
    assert.equal(actions.length,1)
    const [{path:savePath,input:confirm}]=confirmations
    assert.equal(confirmations.length,1)
    assert.equal(savePath,'/api/v1/conversations/'+conversation+'/skill-binding')
    assert.deepEqual(Object.keys(confirm).sort(),['action','preview_confirmation'])
    assert.equal(confirm.action,'confirm_skill_preview')
    assert.equal(confirm.preview_confirmation.run_id,match[2])
    assert.equal(confirm.preview_confirmation.slot,'1')
    assert.ok(!JSON.stringify(confirm).includes(title))
    assert.deepEqual(unexpected,[])
    assert.deepEqual(outside,[])
    process.stdout.write(JSON.stringify({conversation,run:match[2]}))
  }finally{await browser.close()}
}else if(process.argv[2]==='browse-approval-preview'){
  const chunks=[]
  for await(const chunk of process.stdin)chunks.push(chunk)
  const {origin,title}=JSON.parse(Buffer.concat(chunks).toString())
  assert.match(origin,/^http:\/\/127\.0\.0\.1:\d+$/)
  const {chromium}=await import(pathToFileURL(process.env.CLUSBOT_PLAYWRIGHT_MODULE).href)
  const browser=await chromium.launch({headless:true,executablePath:process.env.CLUSBOT_CHROMIUM,args:['--disable-background-networking','--disable-sync']})
  try{
    const page=await browser.newPage({serviceWorkers:'block',viewport:{width:1280,height:900}})
    page.setDefaultTimeout(11000)
    const actions=[],unexpected=[],outside=[]
    await page.route('**/*',async route=>{
      const request=route.request(),url=new URL(request.url())
      if(url.origin!==origin){outside.push(url.origin);return route.abort()}
      if(request.method()==='POST'&&url.pathname.endsWith('/approval-preview')){
        actions.push(request.postDataJSON())
        const res=await route.fetch()
        assert.equal(res.status(),200)
        assert.match(await res.text(),/审批只读预览/)
        return route.abort('failed')
      }
      if(request.method()==='POST'&&url.pathname.startsWith('/api/v1/')&&!['/api/v1/session','/api/v1/conversations'].includes(url.pathname))
        unexpected.push(url.pathname)
      return route.continue()
    })
    await page.goto(origin+'/chat')
    await page.getByLabel('用户名',{exact:true}).fill('admin')
    await page.getByLabel('密码',{exact:true}).fill('correct horse battery staple')
    await page.getByRole('button',{name:'登录进入个人助手'}).click()
    await page.getByRole('textbox',{name:/输入消息/}).fill('查看待我处理')
    await page.getByRole('button',{name:'发送消息',exact:true}).click()
    await page.waitForURL(/\/chat\/[a-f0-9]{32}$/)
    const card=page.getByRole('article',{name:'对话待处理事项'})
    const button=card.getByRole('button',{name:'查看审批预览：'+title+'（1）',exact:true})
    await button.waitFor()
    assert.equal(await card.getByText(/待确认动作/).count(),0)
    await page.reload()
    await button.waitFor()
    assert.equal(actions.length,0)
    await button.focus()
    await page.keyboard.press('Enter')
    await card.getByRole('button',{name:'重新读取待处理事项与回执',exact:true}).click()
    const result=card.getByText(/审批只读预览.*未批准、拒绝或派发。/)
    await result.waitFor()
    await card.getByText('待确认动作：maintain。对象：'+title+'。',{exact:true}).waitFor()
    await card.getByText(/到期：.*合同修订 4 · 策略 5 · 资产修订 6/).waitFor()
    assert.equal(await card.getByRole('button',{name:/^(同意|拒绝|批准)$/}).count(),0)
    await page.reload()
    await result.waitFor()
    assert.equal(await button.count(),0)
    assert.equal(actions.length,1)
    const [event]=actions
    assert.equal(event.version,'1.0')
    assert.equal(event.action.name,'preview_pending_approval')
    assert.match(event.action.sourceComponentId,/^approvalview_[a-f0-9]{32}$/)
    assert.deepEqual(Object.keys(event.action.context).sort(),['attention_slot','revision'])
    assert.equal(event.action.context.attention_slot,'1')
    assert.ok(!JSON.stringify(event).includes(title))
    assert.deepEqual(unexpected,[])
    assert.deepEqual(outside,[])
  }finally{await browser.close()}
}else if(['browse-task-progress','browse-task-records','browse-asset-ledger','browse-attention-reason','browse-artifact-preview'].includes(process.argv[2])){
  const records=process.argv[2]==='browse-task-records'
  const ledger=process.argv[2]==='browse-asset-ledger'
  const attention=process.argv[2]==='browse-attention-reason'
  const artifact=process.argv[2]==='browse-artifact-preview'
  const fixture=ledger?{
    prompt:'查看我的资产',card:'对话资产台账',action:'query_asset_ledger',resource:'asset-card',target:'asset_slot',
    button:'核对台账状态：',component:/^asset_[a-f0-9]{32}$/,
    receipt:'已核对台账（记录 1）：在役 · 健康：未记录 · 可达性：未记录 · 业务占用：未记录',
  }:attention?{
    prompt:'查看待我处理',card:'对话待处理事项',action:'query_attention_reason',resource:'attention-card',target:'attention_slot',
    button:'核对原因：审批 · ',component:/^attention_[a-f0-9]{32}$/,
    receipt:/等待你的决定。依据：本人审批记录。未提交决定或重新执行任务。$/,
  }:artifact?{
    prompt:'查看我的成果',card:'对话成果清单',action:'preview_artifact_text',resource:'artifact-card',target:'artifact_slot',
    button:'预览正文：助手成果.md · ',component:/^artifact_[a-f0-9]{32}$/,
    receipt:'纯文本预览',
  }:{
    prompt:'查看我的任务',card:'对话任务清单',action:'query_task_progress',resource:'task-card',target:'task_id',
    button:'查看进度：',component:/^progress_[a-f0-9]{32}$/,
    receipt:records?'核对时：任务已完成，最近一轮已完成。这是本地记录，不代表远端作业或验收。':'核对时：任务处理中，最近一轮排队中。这是本地记录，不代表远端作业或验收。',
  }
  const chunks=[]
  for await(const chunk of process.stdin)chunks.push(chunk)
  const {origin,title,task,text}=JSON.parse(Buffer.concat(chunks).toString())
  assert.match(origin,/^http:\/\/127\.0\.0\.1:\d+$/)
  const {chromium}=await import(pathToFileURL(process.env.CLUSBOT_PLAYWRIGHT_MODULE).href)
  const browser=await chromium.launch({headless:true,executablePath:process.env.CLUSBOT_CHROMIUM,args:['--disable-background-networking','--disable-sync']})
  try{
    const page=await browser.newPage({serviceWorkers:'block',viewport:{width:1280,height:900}})
    page.setDefaultTimeout(10000)
    const posts=[],outside=[],unexpected=[],detailReads=[]
    await page.route('**/*',async route=>{
      const req=route.request(),url=new URL(req.url())
      if(url.origin!==origin){outside.push(url.origin);return route.abort()}
      if(req.method()==='GET'&&records&&/\/task-card\/[a-f0-9]{32}\/records\/[a-f0-9]{32}$/.test(url.pathname)){
        detailReads.push(url.pathname)
        if(detailReads.length===3){
          const response=await route.fetch()
          assert.equal(response.status(),200)
          return route.fulfill({response,json:{...await response.json(),task_id:'f'.repeat(32)}})
        }
      }
      return route.continue()
    })
    page.on('request',req=>{
      const path=new URL(req.url()).pathname
      if(req.method()==='POST'&&path.includes('/'+fixture.resource+'/')&&path.endsWith('/actions'))posts.push(req.postDataJSON())
      if(path.startsWith('/api/v1/domain/')||path.startsWith('/api/v1/artifacts')||req.method()==='POST'&&path.startsWith('/api/v1/approvals/'))unexpected.push(path)
    })
    await page.goto(origin+'/chat')
    await page.getByLabel('用户名',{exact:true}).fill('admin')
    await page.getByLabel('密码',{exact:true}).fill('correct horse battery staple')
    await page.getByRole('button',{name:'登录进入个人助手'}).click()
    await page.getByRole('textbox',{name:/输入消息/}).fill(fixture.prompt)
    await page.getByRole('button',{name:'发送消息',exact:true}).click()
    await page.waitForURL(/\/chat\/[a-f0-9]{32}$/)
    const card=page.getByRole('article',{name:fixture.card})
    const select=card.getByRole('button',{name:fixture.button+title+(attention||artifact?'（1）':''),exact:true})
    await select.waitFor()
    await page.reload()
    await select.waitFor()
    assert.equal(posts.length,0)
    if(artifact)assert.equal(await card.getByText(/不要解析/).count(),0)
    await select.focus()
    await page.keyboard.press('Enter')
    const result=card.getByText(fixture.receipt,{exact:true})
    await result.waitFor()
    await card.getByRole('status').filter({hasText:/已收到/}).waitFor()
    if(records){
      const details=card.locator('summary').filter({hasText:'步骤与证据：'+title})
      await details.waitFor()
      assert.deepEqual(detailReads,[])
      await details.focus()
      await page.keyboard.press('Enter')
      await card.getByText('步骤 1 · 已完成 · 终态时间未记录',{exact:true}).waitFor()
      await card.getByText(/已验收成果 · 助手成果.md · 验收登记/).waitFor()
      assert.equal(await card.getByText(/case-report-body-private|2099-01-01/).count(),0)
      assert.equal(detailReads.length,1)
      assert.equal(detailReads[0].split('/').pop(), task)
      await page.reload()
      await details.waitFor()
      assert.equal(detailReads.length,1)
      assert.equal(await card.getByText(/已验收成果 · 助手成果.md/).count(),0)
      await details.click()
      await card.getByText(/已验收成果 · 助手成果.md/).waitFor()
      await details.click()
      await details.click()
      await card.getByText('此任务的步骤与证据暂不可读。',{exact:true}).waitFor()
      assert.equal(await card.getByText(/已验收成果 · 助手成果.md/).count(),0)
      assert.deepEqual(detailReads,[detailReads[0],detailReads[0],detailReads[0]])
      assert.deepEqual(unexpected,[])
    }
    await page.reload()
    await result.waitFor()
    if(artifact){
      const paragraph=card.locator('p').filter({hasText:'不要解析'})
      assert.equal(await paragraph.textContent(),text)
      assert.equal(await paragraph.evaluate(el=>getComputedStyle(el).whiteSpace),'pre-wrap')
      assert.equal(await card.locator('strong, a').count(),0)
      assert.ok(await card.getByText(/尚未验收/).isVisible())
    }
    assert.equal(await select.count(),0)
    assert.equal(posts.length,1)
    const [action]=posts
    assert.equal(action.version,'1.0')
    assert.equal(action.action.name,fixture.action)
    assert.match(action.action.sourceComponentId,fixture.component)
    assert.deepEqual(Object.keys(action.action.context).sort(),[fixture.target,'revision'].sort())
    assert.equal(action.action.context[fixture.target],ledger||attention||artifact?'1':task)
    if(ledger||attention||artifact)assert.deepEqual(unexpected,[])
    assert.ok(!JSON.stringify(action).includes(title))
    assert.deepEqual(outside,[])
  }finally{await browser.close()}
}else if(process.argv[2]==='browse-operation-advice'){
  const chunks=[]
  for await(const chunk of process.stdin)chunks.push(chunk)
  const {origin,prompt,label}=JSON.parse(Buffer.concat(chunks).toString())
  assert.match(origin,/^http:\/\/127\.0\.0\.1:\d+$/)
  const {chromium}=await import(pathToFileURL(process.env.CLUSBOT_PLAYWRIGHT_MODULE).href)
  const browser=await chromium.launch({headless:true,executablePath:process.env.CLUSBOT_CHROMIUM,args:['--disable-background-networking','--disable-sync']})
  try{
    const page=await browser.newPage({serviceWorkers:'block',viewport:{width:1280,height:900}})
    page.setDefaultTimeout(12000)
    const actions=[],unexpected=[],outside=[]
    await page.route('**/*',async route=>{
      const request=route.request(), url=new URL(request.url())
      if(url.origin!==origin){outside.push(url.origin);await route.abort();return}
      if(request.method()==='POST'&&url.pathname.includes('/operation-advice/')&&url.pathname.endsWith('/actions')){
        actions.push(request.postDataJSON())
        // Commit via the ordinary Go handler, then lose the browser response.
        // There must be no retry of this POST during readback or reload.
        const actual=await route.fetch()
        assert.equal(actual.status(),200)
        const receipt=await actual.json()
        assert.equal(receipt.state,'recorded')
        assert.equal(receipt.choice.slot,'2')
        await route.abort()
        return
      }
      if(!['GET','HEAD'].includes(request.method())&&!['/api/v1/session','/api/v1/conversations'].includes(url.pathname))unexpected.push(url.pathname)
      await route.continue()
    })
    await page.goto(origin+'/chat')
    await page.getByLabel('用户名',{exact:true}).fill('admin')
    await page.getByLabel('密码',{exact:true}).fill('correct horse battery staple')
    await page.getByRole('button',{name:'登录进入个人助手'}).click()
    await page.getByRole('textbox',{name:/输入消息/}).fill(prompt)
    await page.getByRole('button',{name:'发送消息',exact:true}).click()
    await page.waitForURL(/\/chat\/[a-f0-9]{32}$/)
    const card=page.getByRole('article',{name:'运维建议选择'})
    const button=card.getByRole('button',{name:'记录选择：'+label,exact:true})
    await button.waitFor()
    await card.getByText('这些是供选择的文字建议。选择不会批准、启动或修改任何运维操作。',{exact:true}).waitFor()
    await page.reload()
    await button.waitFor()
    assert.equal(actions.length,0)
    await button.focus()
    await page.keyboard.press('Enter')
    const check=card.getByRole('button',{name:'核对选择回执',exact:true})
    await check.waitFor()
    assert.equal((await check.isEnabled()),true)
    await check.click()
    const acknowledged=card.getByRole('status').filter({hasText:'已记录选择：'+label})
    await acknowledged.waitFor()
    assert.match(await acknowledged.textContent(),/未批准或执行任何运维操作/)
    await page.reload()
    await acknowledged.waitFor()
    assert.equal(await button.count(),0)
    assert.equal(actions.length,1)
    const event=actions[0]
    assert.equal(event.version,'1.0')
    assert.equal(event.action.name,'review_operation_advice_choice')
    assert.match(event.action.sourceComponentId,/^choose_[a-f0-9]{32}$/)
    assert.deepEqual(Object.keys(event.action.context).sort(),['advice_slot','revision'])
    assert.equal(event.action.context.advice_slot,'2')
    assert.ok(!JSON.stringify(event).includes(label))
    assert.deepEqual(unexpected,[])
    assert.deepEqual(outside,[])
  }finally{await browser.close()}
}else if(process.argv[2]==='browse-benchmark-admission'){
  const input=[]
  for await(const chunk of process.stdin)input.push(chunk)
  const {origin,prompt}=JSON.parse(Buffer.concat(input).toString())
  assert.match(origin,/^http:\/\/127\.0\.0\.1:\d+$/)
  const {chromium}=await import(pathToFileURL(process.env.CLUSBOT_PLAYWRIGHT_MODULE).href)
  const browser=await chromium.launch({headless:true,executablePath:process.env.CLUSBOT_CHROMIUM,args:['--disable-background-networking','--disable-sync']})
  const action=[],otherWrites=[],outside=[]
  try{
    const page=await browser.newPage({serviceWorkers:'block',viewport:{width:1360,height:960}})
    page.setDefaultTimeout(12000)
    await page.route('**/*',async route=>{
      const request=route.request(),path=new URL(request.url())
      if(path.origin!==origin){outside.push(path.origin);return route.abort()}
      const method=request.method()
      if(method==='POST'&&path.pathname.includes('/a2ui/benchmark-admission/')){
        const event=request.postDataJSON()
        action.push({event,path:path.pathname})
        // A real committed POST, but the client never sees the acknowledgment.
        const res=await route.fetch()
        assert.equal(res.status(),200)
        assert.equal((await res.json()).state,'previewed')
        return route.abort('failed')
      }
      if(method==='POST'&&path.pathname.startsWith('/api/')&&path.pathname!=='/api/v1/session'&&
        !/^\/api\/v1\/conversations(?:\/[a-f0-9]{32}\/messages)?$/.test(path.pathname))otherWrites.push(path.pathname)
      return route.continue()
    })
    await page.goto(origin+'/chat')
    await page.getByLabel('用户名',{exact:true}).fill('admin')
    await page.getByLabel('密码',{exact:true}).fill('correct horse battery staple')
    await page.getByRole('button',{name:'登录进入个人助手'}).click()
    const composer=page.getByRole('textbox',{name:/输入消息/})
    const send=page.getByRole('button',{name:'发送消息',exact:true})
    await composer.fill(prompt)
    await send.click()
    await page.waitForURL(/\/chat\/[a-f0-9]{32}$/)
    const conversation=new URL(page.url()).pathname.split('/').pop()
    const card=page.getByRole('region',{name:'基准受理',exact:true}).first()
    const button=card.getByRole('button',{name:'查看受理预览',exact:true})
    await button.waitFor()
    await page.reload()
    await button.waitFor()
    assert.equal(action.length,0)
    await button.focus()
    await page.keyboard.press('Enter')
    await card.getByRole('button',{name:'核对受理回执',exact:true}).click()
    const restored=card.getByRole('status').filter({hasText:'受理预览已保存。没有创建任务，也未启动测试。'})
    await restored.waitFor()
    const explicit=card.getByRole('button',{name:'受理并加入队列',exact:true})
    assert.ok(await explicit.isEnabled())
    assert.match(await card.textContent(),/未核对现场容量/)
    await page.reload()
    await restored.waitFor()
    assert.equal(action.length,1)
    // A newer turn must not move the first preview behind a different Run.
    await composer.fill(prompt)
    await send.click()
    await page.getByRole('region',{name:'基准受理',exact:true}).nth(1).getByRole('button',{name:'查看受理预览',exact:true}).waitFor()
    await page.reload()
    await restored.waitFor()
    assert.equal(action.length,1)
    const [{event,path}]=action
    assert.equal(event.version,'1.0')
    assert.equal(event.action.name,'preview_benchmark_admission')
    assert.equal(event.action.sourceComponentId,'preview_admission')
    assert.deepEqual(Object.keys(event.action.context).sort(),['revision','source_run_id'])
    assert.equal(path,'/api/v1/conversations/'+conversation+'/a2ui/benchmark-admission/'+event.action.context.source_run_id)
    assert.ok(!JSON.stringify(event).includes('POD7'))
    assert.deepEqual(otherWrites,[])
    assert.deepEqual(outside,[])
  }finally{await browser.close()}
}else if(process.argv[2]==='browse-run-model-receipt'){
  const input=[]
  for await(const chunk of process.stdin)input.push(chunk)
  const {origin,prompt,reply}=JSON.parse(Buffer.concat(input).toString())
  assert.match(origin,/^http:\/\/127\.0\.0\.1:\d+$/)
  const {chromium}=await import(pathToFileURL(process.env.CLUSBOT_PLAYWRIGHT_MODULE).href)
  const browser=await chromium.launch({headless:true,executablePath:process.env.CLUSBOT_CHROMIUM,args:['--disable-background-networking','--disable-sync']})
  const reads=[],writes=[],outside=[]
  try{
    const page=await browser.newPage({serviceWorkers:'block',viewport:{width:1360,height:960}})
    page.setDefaultTimeout(12000)
    await page.route('**/*',async route=>{
      const request=route.request(),path=new URL(request.url()),method=request.method()
      if(path.origin!==origin){outside.push(path.origin);return route.abort()}
      if(method==='GET'&&/^\/api\/v1\/conversations\/[a-f0-9]{32}\/runs\/[a-f0-9]{32}\/model-receipt\/[1-9][0-9]*$/.test(path.pathname)){
        reads.push(path.pathname)
        if(reads.length===3){
          // Simulate out-of-order/misrouted transport with a real, well-formed
          // read of another turn: the client must not adopt the body.
          const res=await route.fetch()
          assert.equal(res.status(),200)
          const payload=await res.json()
          return route.fulfill({response:res,json:{...payload,message_id:payload.message_id+1}})
        }
      }
      if(path.pathname.startsWith('/api/')&&!['GET','HEAD'].includes(method))writes.push(method+' '+path.pathname)
      return route.continue()
    })
    await page.goto(origin+'/chat')
    await page.getByLabel('用户名',{exact:true}).fill('admin')
    await page.getByLabel('密码',{exact:true}).fill('correct horse battery staple')
    await page.getByRole('button',{name:'登录进入个人助手'}).click()
    await page.getByRole('textbox',{name:/输入消息/}).fill(prompt)
    await page.getByRole('button',{name:'发送消息',exact:true}).click()
    await page.waitForURL(/\/chat\/[a-f0-9]{32}$/)
    const message=page.locator('.message-assistant').filter({hasText:reply}).last()
    const opener=message.locator('summary').filter({hasText:'模型与用量'})
    const tokenText=message.getByText('2 次模型尝试 · Token 输入 46 / 输出 4',{exact:true})
    await opener.waitFor()
    assert.deepEqual(reads,[])
    const original=await message.getAttribute('id')
    const conversation=new URL(page.url()).pathname.split('/').pop()
    await opener.focus()
    await page.keyboard.press('Enter')
    await tokenText.waitFor()
    await message.getByText('请求模型：fixture-requested-model',{exact:true}).waitFor()
    await message.getByText('提供方报告：vendor/reply-actual',{exact:true}).waitFor()
    assert.equal(reads.length,1)
    assert.equal(reads[0].split('/')[4],conversation)
    assert.equal(reads[0].split('/').pop(),original.slice('message-'.length))
    await page.reload()
    await opener.waitFor()
    assert.equal(reads.length,1)
    assert.equal(await tokenText.count(),0)
    await opener.click()
    await tokenText.waitFor()
    assert.equal(reads[1],reads[0])
    await opener.click()
    assert.equal(await tokenText.count(),0)
    await opener.click()
    await message.getByRole('status').filter({hasText:'本条回复的模型与用量暂不可读。'}).waitFor()
    assert.deepEqual(reads,[reads[0],reads[0],reads[0]])
    assert.equal(await tokenText.count(),0)
    assert.equal(await message.getByText('提供方报告：vendor/reply-actual',{exact:true}).count(),0)
    assert.equal(writes.filter(item=>item==='POST /api/v1/session').length,1)
    assert.equal(writes.filter(item=>/^POST \/api\/v1\/conversations(?:\/[a-f0-9]{32}\/messages)?$/.test(item)).length,1)
    assert.equal(writes.length,2)
    assert.deepEqual(outside,[])
  }finally{await browser.close()}
}else if(process.argv[2]==='browse-integrated-read-scenarios'){
  const chunks=[]
  for await(const chunk of process.stdin)chunks.push(chunk)
  const {scenario,origin,conversation,task,title,other,asset,message,draft,cursor,prompt}=JSON.parse(Buffer.concat(chunks).toString())
  assert.match(origin,/^http:\/\/127\.0\.0\.1:\d+$/)
  const {chromium}=await import(pathToFileURL(process.env.CLUSBOT_PLAYWRIGHT_MODULE).href)
  const browser=await chromium.launch({headless:true,executablePath:process.env.CLUSBOT_CHROMIUM,args:['--disable-background-networking','--disable-sync']})
  const outside=[],posts=[],writes=[],actions=[]
  let page
  let phase='sign-in'
  try{
    page=await browser.newPage({serviceWorkers:'block',viewport:scenario==='narrow'?{width:390,height:780}:{width:1280,height:900}})
    page.setDefaultTimeout(12000)
    const events='/api/v1/conversations/'+conversation+'/events'
    await page.route('**/*',route=>{
      const req=route.request(),u=new URL(req.url()),method=req.method()
      if(u.origin!==origin){outside.push(u.origin);return route.abort()}
      if(u.pathname.startsWith('/api/')&&!['GET','HEAD'].includes(method)){
        writes.push(method+' '+u.pathname)
        if(method==='POST'&&u.pathname.endsWith('/actions'))actions.push({path:u.pathname,value:req.postDataJSON()})
        if(method==='POST'&&u.pathname.endsWith('/messages'))posts.push({headers:req.headers(),body:req.postDataJSON()})
      }
      return route.continue()
    })
    await page.goto(origin+'/chat')
    await page.getByLabel('用户名',{exact:true}).fill('admin')
    await page.getByLabel('密码',{exact:true}).fill('correct horse battery staple')
    await page.getByRole('button',{name:'登录进入个人助手'}).click()
    const composer=page.getByRole('textbox',{name:/输入消息/})
    await composer.waitFor()
    const scenarioPath=origin+'/chat/'+conversation+(message?'#message-'+message:'')
    await page.goto(scenarioPath)
    const tasks=page.getByRole('article',{name:'对话任务清单',exact:true})
    const ledger=page.getByRole('article',{name:'对话资产台账',exact:true})
    const taskLink=page.getByRole('region',{name:'会话已受理工作',exact:true}).getByText(task,{exact:true})
    const pickAsset=ledger.getByRole('button',{name:'核对台账状态：'+asset,exact:true})
    const assetReceipt=ledger.getByText(/已核对台账（记录 1）：在役/)
    const pickTask=tasks.getByRole('button',{name:'查看进度：'+title,exact:true})
    const taskReceipt=tasks.getByText(/核对时：任务/)
    const send=page.getByRole('button',{name:'发送消息',exact:true})
    if(scenario==='narrow'){
      phase='anchor and independent cards'
      await page.waitForFunction(id=>document.activeElement?.id==='message-'+id,message)
      await pickTask.waitFor()
      await pickAsset.waitFor()
      const originalAssetCard=await ledger.innerText()
      await pickTask.click()
      await taskReceipt.waitFor()
      assert.equal(await ledger.innerText(),originalAssetCard)
      const originalTaskCard=await tasks.innerText()
      await pickAsset.click()
      await assetReceipt.waitFor()
      assert.equal(await tasks.innerText(),originalTaskCard)
      assert.deepEqual(actions.map(a=>a.value.action.name),['query_task_progress','query_asset_ledger'])
      assert.notEqual(actions[0].value.action.surfaceId,actions[1].value.action.surfaceId)
      assert.match(actions[0].path,new RegExp('/'+conversation+'/task-card/'))
      assert.match(actions[1].path,new RegExp('/'+conversation+'/asset-card/'))
      assert.equal(actions[0].value.action.context.task_id,task)
      assert.equal(actions[1].value.action.context.asset_slot,'1')
      phase='narrow drawer, draft and focus'
      await composer.fill(draft)
      await taskLink.waitFor()
      const trigger=taskLink.locator('xpath=../..').getByRole('button',{name:'查看任务',exact:true})
      // Exercise the production drawer with a transient read failure only.
      // No mutation or new assistant turn is a valid recovery mechanism.
      const detailURL=origin+'/api/v1/tasks/'+task
      let detailUnavailable=true
      await page.route(detailURL,route=>route.request().method()==='GET'&&detailUnavailable?
        route.fulfill({status:503,json:{message:'样例服务暂时不可用'}}):route.fallback())
      await trigger.click()
      const dialog=page.getByRole('dialog',{name:'任务详情',exact:true})
      const drawerTitle=dialog.getByRole('heading',{name:title,exact:true})
      const retry=dialog.getByRole('button',{name:'重新读取任务',exact:true})
      await retry.waitFor()
      assert.equal(await drawerTitle.count(),0)
      assert.equal(await dialog.getByRole('alert').count(),1)
      const writesBeforeRecovery=writes.length
      detailUnavailable=false
      await retry.focus()
      await page.keyboard.press('Enter')
      await drawerTitle.waitFor()
      assert.ok(await drawerTitle.evaluate(el=>el===document.activeElement),'keyboard retry should land at the fresh task heading')
      assert.equal(await dialog.evaluate(el=>el.matches(':modal')),true)
      detailUnavailable=true
      await dialog.getByText('查看完整工作与交流',{exact:true}).click()
      await retry.waitFor()
      assert.equal(await drawerTitle.count(),0)
      assert.equal(await dialog.getByRole('button',{name:'引用',exact:true}).count(),0)
      assert.equal(await dialog.getByRole('alert').count(),1)
      detailUnavailable=false
      await retry.click()
      await drawerTitle.waitFor()
      assert.equal(writes.length,writesBeforeRecovery)
      await page.unroute(detailURL)
      await dialog.getByRole('button',{name:'引用',exact:true}).click()
      await dialog.waitFor({state:'detached'})
      await page.waitForFunction(()=>document.activeElement?.tagName==='TEXTAREA')
      const reference=page.getByRole('button',{name:new RegExp('引用任务 · '+title)})
      await reference.waitFor()
      assert.equal(await composer.inputValue(),draft)
      const log=page.locator('.chat-log')
      await log.evaluate(el=>{el.scrollTop=Math.min(160,el.scrollHeight-el.clientHeight)})
      const originalScroll=await log.evaluate(el=>el.scrollTop)
      assert.ok(originalScroll>20,'the fixture must exercise an already scrolled chat, not a chat at the top')
      await trigger.click()
      await dialog.getByRole('heading',{name:title,exact:true}).waitFor()
      await page.keyboard.press('Escape')
      await dialog.waitFor({state:'detached'})
      assert.equal(await trigger.evaluate(el=>el===document.activeElement),true)
      assert.equal(new URL(page.url()).hash,'#message-'+message)
      assert.ok(Math.abs((await log.evaluate(el=>el.scrollTop))-originalScroll)<=5,'restored chat log did not move away from its position')
      // The toolbar's task popover is intentionally hidden at this width;
      // the same current-task list lives in the mobile navigation drawer.
      await page.getByRole('button',{name:/^(打开导航|Open navigation)$/}).click()
      await page.locator('#workspace-sidebar .sidebar-task').filter({hasText:other}).click()
      await dialog.getByRole('heading',{name:other,exact:true}).waitFor()
      await dialog.getByRole('button',{name:'关闭任务详情',exact:true}).click()
      await dialog.waitFor({state:'detached'})
      assert.equal(await composer.inputValue(),draft)
      assert.ok((await reference.innerText()).includes(task))
      assert.equal(await reference.count(),1)
      assert.equal(new URL(page.url()).hash,'#message-'+message)
      assert.equal(await page.locator('#message-'+message).count(),1)
      phase='narrow reopen'
      await page.reload()
      await taskReceipt.waitFor()
      await assetReceipt.waitFor()
      assert.equal(await composer.inputValue(),draft)
      assert.ok((await reference.innerText()).includes(task))
      assert.equal(actions.length,2)
      assert.equal(writes.length,3)
    }else if(scenario==='offline'){
      phase='establish actual durable cursor and record'
      await taskLink.waitFor()
      await composer.fill(draft)
      await pickAsset.click()
      await assetReceipt.waitFor()
      const reqPath=request=>new URL(request.url())
      await page.waitForResponse(res=>{const u=reqPath(res);return u.pathname===events&&u.searchParams.get('cursor')===cursor&&res.status()===200})
      phase='disconnect then drop original cursor and revoke grant'
      const lost=page.waitForEvent('requestfailed',request=>reqPath(request).pathname===events)
      await page.context().setOffline(true)
      await lost
      const mutation=await fetch(origin+'/test-fixture/drop-retained-cursor-and-revoke',{method:'POST',headers:{'X-ClusBot-Test-Anchor':cursor}})
      assert.equal(mutation.status,204)
      phase='reconnect and validate real 409 plus current snapshot'
      const rejected=page.waitForResponse(res=>{const u=reqPath(res);return u.pathname===events&&u.searchParams.get('cursor')===cursor&&res.status()===409})
      const reset=page.waitForResponse(res=>{const u=reqPath(res);return u.pathname===events&&!u.searchParams.has('cursor')&&res.status()===200})
      await page.context().setOffline(false)
      assert.equal((await (await rejected).json()).code,'conversation_event_cursor_reset')
      const retained=await (await reset).json()
      assert.ok(retained.events.some(event=>event.task_id===task))
      await taskLink.waitFor()
      assert.equal(await taskLink.count(),1)
      assert.equal(await composer.inputValue(),draft)
      assert.equal(await assetReceipt.count(),0,'reconnect reused the previously visible revoked asset receipt')
      await ledger.getByText('有资产已变更或不可查看。请重新查询当前台账。',{exact:true}).waitFor()
      assert.equal(await ledger.getByText(new RegExp(asset)).count(),0)
      phase='refresh with grants withdrawn'
      await page.reload()
      await ledger.getByText('有资产已变更或不可查看。请重新查询当前台账。',{exact:true}).waitFor()
      assert.equal(await assetReceipt.count(),0)
      assert.equal(await ledger.getByText(new RegExp(asset)).count(),0)
      assert.equal(await composer.inputValue(),draft)
      assert.equal(actions.length,1)
      assert.equal(writes.length,2)
    }else if(scenario==='no_model'){
      phase='no commissioning and saved facts still visible'
      await page.getByText(/^(尚未添加可用模型|No enabled models yet)$/).waitFor()
      await taskReceipt.waitFor()
      const details=tasks.locator('summary').filter({hasText:'步骤与证据：'+title})
      await details.click()
      await tasks.getByText(/已验收成果 · 助手成果.md/).waitFor()
      phase='ordinary prompt is durably pending, not a fabricated reply'
      await composer.fill(prompt)
      const wait=page.waitForResponse(res=>res.request().method()==='POST'&&new URL(res.url()).pathname==='/api/v1/conversations/'+conversation+'/messages')
      await send.click()
      const response=await wait
      assert.equal(response.status(),202)
      const pending=await response.json()
      assert.equal(pending.model_pending,true)
      assert.ok(pending.request_key)
      assert.equal(pending.run_id,undefined)
      const admission=await page.request.get(origin+'/api/v1/admissions/'+pending.request_key)
      assert.equal(admission.status(),200)
      const saved=await admission.json()
      assert.equal(saved.state,'received')
      assert.ok(!saved.run_id)
      await page.locator('.message-user').filter({hasText:prompt}).waitFor()
      phase='explicit deterministic query still works apply no task or model'
      await page.getByRole('button',{name:/模型策略：按角色路由/}).click()
      await page.getByRole('button',{name:/仅确定性查询/}).click()
      await composer.fill('查看我的任务')
      const read=page.waitForResponse(res=>res.request().method()==='POST'&&new URL(res.url()).pathname==='/api/v1/conversations/'+conversation+'/messages')
      await send.click()
      const readResponse=await read
      assert.equal(readResponse.status(),200)
      const facts=await readResponse.json()
      assert.equal(facts.tool,'list_tasks')
      assert.equal(facts.state,'completed')
      assert.deepEqual(posts.map(post=>post.headers['x-clusbot-reply-strategy']??''),['','deterministic_only'])
      assert.deepEqual(actions,[])
      assert.equal(writes.length,3)
    }else throw new Error('unknown integrated scenario')
    assert.deepEqual(outside,[])
    process.stdout.write(JSON.stringify({scenario,writes:writes.length,selectedA2UI:actions.length,verified:true}))
  }catch(error){
    const body=page?await page.locator('main').innerText().catch(()=>''):'none'
    throw new Error(phase+': '+String(error)+'\n'+body,{cause:error})
  }finally{await browser.close()}
}else if(process.argv[2]==='browse-asset-bfcache'){
  const chunks=[]
  for await(const chunk of process.stdin)chunks.push(chunk)
  const {origin,conversation,asset,draft,proof}=JSON.parse(Buffer.concat(chunks).toString())
  assert.match(origin,/^http:\/\/127\.0\.0\.1:\d+$/)
  const {chromium}=await import(pathToFileURL(process.env.CLUSBOT_PLAYWRIGHT_MODULE).href)
  const browser=await chromium.launch({headless:true,executablePath:process.env.CLUSBOT_CHROMIUM,
    args:['--disable-background-networking','--disable-sync'],ignoreDefaultArgs:['--disable-back-forward-cache']})
  const outside=[], writes=[], recoveryReads=[]
  let page, withdrawn=false, phase='sign-in'
  try{
    const engine=browser.browserType().name()
    if(engine!=='chromium'){
      process.stdout.write(JSON.stringify({persisted:false,engine,reason:'Chromium-specific acceptance case'}))
    }else{
      page=await browser.newPage({serviceWorkers:'block',viewport:{width:1280,height:900}})
      page.setDefaultTimeout(9000)
      const chat=origin+'/chat/'+conversation
      const prefix='/api/v1/conversations/'+conversation+'/asset-card/'
      // No request interception or cache manipulation while navigating. The
      // Go HTML is CSP-restricted to same-origin sources, and we observe the
      // test tab's traffic to confirm nothing leaves the local fixture.
      page.on('request',request=>{
        const u=new URL(request.url()),method=request.method()
        if(u.origin!==origin){outside.push(u.origin);return}
        if(!['GET','HEAD'].includes(method))writes.push(method+' '+u.pathname)
        if(withdrawn&&method==='GET'&&u.pathname.startsWith(prefix))recoveryReads.push(u.pathname)
      })
      await page.addInitScript(({needle})=>{
        const probe={marker:crypto.randomUUID(),shows:[],mutations:[],frame:null}
        window.__clusbotHistoryProbe=probe
        const inspect=()=>{
          const card=document.querySelector('[data-read-card="asset-card"]')
          const text=card?.textContent??''
          return {label:text.includes(needle),receipt:text.includes('已核对台账（记录 1）：在役')}
        }
        addEventListener('pageshow',event=>{
          const stamp={persisted:event.persisted,...inspect()}
          probe.shows.push(stamp)
          if(event.persisted){
            requestAnimationFrame(()=>{probe.frame=inspect()})
            const observer=new MutationObserver(()=>{
              const snap=inspect()
              if(snap.label||snap.receipt)probe.mutations.push(snap)
            })
            observer.observe(document.body,{subtree:true,childList:true,characterData:true,attributes:true})
          }
        })
      },{needle:asset})
      await page.goto(origin+'/chat')
      await page.getByLabel('用户名',{exact:true}).fill('admin')
      await page.getByLabel('密码',{exact:true}).fill('correct horse battery staple')
      await page.getByRole('button',{name:'登录进入个人助手'}).click()
      await page.getByRole('textbox',{name:/输入消息/}).waitFor()
      await page.goto(chat)
      const composer=page.getByRole('textbox',{name:/输入消息/})
      const ledger=page.getByRole('article',{name:'对话资产台账',exact:true})
      const receipt=ledger.getByText(/已核对台账（记录 1）：在役/)
      await composer.fill(draft)
      await ledger.getByRole('button',{name:'核对台账状态：'+asset,exact:true}).click()
      await receipt.waitFor()
      const marker=await page.evaluate(()=>window.__clusbotHistoryProbe.marker)
      const cookies=await page.context().cookies(origin)
      assert.ok(cookies.length>0)
      phase='navigate to an independent document and revoke one grant without changing the cookie'
      await page.goto(origin+'/test-fixture/bfcache-away')
      await page.getByText('Local history test',{exact:true}).waitFor()
      const control=(action)=>fetch(origin+'/test-fixture/bfcache-'+action,{method:'POST',headers:{'X-ClusBot-Test-Proof':proof}})
      assert.equal((await control('revoke')).status,204)
      withdrawn=true
      phase='return through the actual Chromium history cache'
      await page.goBack({waitUntil:'commit'})
      // BFCache does not fire a fresh load event. Observe the real document
      // URL and the resumed page's event rather than waiting for another load.
      await page.waitForFunction(({url,original})=>{
        const p=window.__clusbotHistoryProbe
        return location.href===url&&p?.shows.length&&(p.marker!==original||p.shows.at(-1)?.persisted)
      },{url:chat,original:marker})
      const state=await page.evaluate(()=>{
        const nav=performance.getEntriesByType('navigation')[0]
        const reasons=nav?.notRestoredReasons
        return {marker:window.__clusbotHistoryProbe.marker,shows:window.__clusbotHistoryProbe.shows,navType:nav?.type,reasons:reasons?.toJSON?.()??reasons??null}
      })
      const resumed=state.shows.at(-1)
      assert.deepEqual(await page.context().cookies(origin),cookies,'session cookies changed between navigations')
      if(resumed?.persisted!==true){
        assert.deepEqual(outside,[])
        process.stdout.write(JSON.stringify({persisted:false,engine,version:browser.version(),navType:state.navType,
          reason:state.reasons??'Chromium did not expose notRestoredReasons; a new document loaded',newDocument:state.marker!==marker}))
      }else{
        assert.equal(state.marker,marker,'a cache restore must use the original JS heap')
        phase='restored document awaiting completed Go revalidation'
        await page.waitForFunction(()=>window.__clusbotHistoryProbe?.frame!==null)
        const deadline=Date.now()+5000
        while(!recoveryReads.length&&Date.now()<deadline)await new Promise(resolve=>setTimeout(resolve,25))
        assert.ok(recoveryReads.length,'restored page did not read its original asset card')
        assert.equal(await receipt.count(),0,'revoked result was present before the protected GET completed')
        assert.equal(await ledger.getByText(new RegExp(asset)).count(),0,'revoked asset identity was present before the protected GET completed')
        const probe=await page.evaluate(()=>window.__clusbotHistoryProbe)
        assert.deepEqual(resumed,{persisted:true,label:false,receipt:false},'old sensitive text reappeared on pageshow')
        assert.deepEqual(probe.frame,{label:false,receipt:false},'old sensitive text was present in the first restored frame')
        assert.deepEqual(probe.mutations,[],'old sensitive text reappeared while rechecking')
        assert.equal(await composer.inputValue(),draft)
        phase='release the protected GET and verify Go response, not a repeated action'
        assert.equal((await control('release')).status,204)
        await ledger.getByText('有资产已变更或不可查看。请重新查询当前台账。',{exact:true}).waitFor()
        assert.equal(await receipt.count(),0)
        assert.equal(await ledger.getByText(new RegExp(asset)).count(),0)
        assert.deepEqual((await page.evaluate(()=>window.__clusbotHistoryProbe)).mutations,[])
        assert.equal(writes.length,2)
        assert.ok(writes.includes('POST /api/v1/session'))
        assert.equal(writes.filter(path=>path.startsWith('POST '+prefix)&&path.endsWith('/actions')).length,1)
        assert.deepEqual(outside,[])
        process.stdout.write(JSON.stringify({persisted:true,engine,version:browser.version(),pendingProtectedReads:recoveryReads.length,
          firstFrameMasked:true,sameCookie:true,draftRetained:true,replayedActions:0}))
      }
    }
  }catch(error){
    const body=page?await page.locator('main').first().innerText().catch(()=>''):'none'
    throw new Error(phase+': '+String(error)+'\n'+body,{cause:error})
  }finally{await browser.close()}
}else if(process.argv[2]==='browse-settings-scenarios'){
  const chunks=[]
  for await(const chunk of process.stdin)chunks.push(chunk)
  const {scenario,origin,provider,model,key,badKey,prompt,reply,skill,bodyOne,bodyTwo}=JSON.parse(Buffer.concat(chunks).toString())
  for(const local of [origin,provider])assert.match(local,/^http:\/\/127\.0\.0\.1:\d+$/)
  const {chromium}=await import(pathToFileURL(process.env.CLUSBOT_PLAYWRIGHT_MODULE).href)
  const browser=await chromium.launch({headless:true,executablePath:process.env.CLUSBOT_CHROMIUM,args:['--disable-background-networking','--disable-sync']})
  const outside=[], writes=[], exposed=[]
  const base='/api/v1', settings=base+'/settings/model', skills=base+'/skills'
  const navSettings='/settings?tab=model', navSkills='/settings?tab=skills'
  const pass='synthetic settings admin password', memberPass='synthetic settings reader password'
  let page, phase='cold boot'
  try{
    const context=await browser.newContext({serviceWorkers:'block',viewport:{width:1280,height:900}})
    page=await context.newPage()
    page.setDefaultTimeout(11000)
    await context.route('**/*',route=>{
      const path=new URL(route.request().url())
      if(path.origin!==origin){outside.push(path.origin);return route.abort()}
      if(path.pathname.startsWith(base)&&!['GET','HEAD'].includes(route.request().method()))writes.push(route.request().method()+' '+path.pathname)
      return route.continue()
    })
    page.on('response',async res=>{
      const path=new URL(res.url()).pathname
      if(path.startsWith(settings)){
        const raw=await res.text().catch(()=>'')
        if(raw.includes(key)||raw.includes(badKey))exposed.push(path)
      }
    })
    const api=async(method,path,body,client=context.request)=>{
      const result=await client.fetch(origin+path,{method,headers:{Origin:origin},...(body===undefined?{}:{data:body})})
      const raw=await result.text()
      if(path.startsWith(settings))assert.ok(!raw.includes(key)&&!raw.includes(badKey),'model API returned a credential')
      return {status:result.status(),json:raw?JSON.parse(raw):null}
    }
    const exactResponse=(method,path)=>page.waitForResponse(res=>res.request().method()===method&&new URL(res.url()).pathname===path)
    const submitChat=async()=>{
      await page.getByRole('textbox',{name:/输入消息/}).fill(prompt)
      const pending=page.waitForResponse(r=>r.request().method()==='POST'&&/^\/api\/v1\/conversations(?:\/[a-f0-9]{32}\/messages)?$/.test(new URL(r.url()).pathname))
      await page.getByRole('button',{name:'发送消息',exact:true}).click()
      const accepted=await pending
      assert.equal(accepted.status(),202)
      const out=await accepted.json()
      assert.match(out.run_id,/^[a-f0-9]{32}$/)
      assert.ok(!out.task_id&&!out.model_pending)
      await page.getByText(reply,{exact:true}).first().waitFor({timeout:18000})
      return out
    }
    const session=await api('GET',settings+'/providers')
    assert.equal(session.status,401)
    assert.equal((await api('POST',skills+'/import-preview',{})).status,401)
    await page.goto(origin+'/chat')
    await page.getByLabel('超级管理员用户名').fill('admin')
    await page.getByLabel('设置密码',{exact:true}).fill(pass)
    await page.getByLabel('确认密码',{exact:true}).fill(pass)
    const boot=exactResponse('POST',base+'/bootstrap')
    await page.getByRole('button',{name:'创建首位超级管理员并进入'}).click()
    assert.equal((await boot).status(),201)
    await page.getByRole('textbox',{name:/输入消息/}).waitFor()
    assert.equal((await api('POST',base+'/admin/users',{username:'reader',display_name:'Synthetic Reader',password:memberPass})).status,201)
    const member=await browser.newContext({serviceWorkers:'block'})
    assert.equal((await api('POST',base+'/session',{username:'reader',password:memberPass},member.request)).status,200)
    assert.equal((await api('POST',settings+'/providers',{name:'denied'},member.request)).status,403)
    assert.equal((await api('POST',skills+'/import-preview',{},member.request)).status,403)
    assert.equal((await api('POST',skills+'/import',{},member.request)).status,403)
    if(scenario==='empty_model_recovery'){
      phase='empty configuration entry and initial connection'
      await page.getByText(/尚未添加可用模型|No enabled models yet/).waitFor()
      const noModel=await api('GET',settings+'/providers')
      assert.equal(noModel.status,200);assert.deepEqual(noModel.json,[])
      await page.getByRole('button',{name:/添加提供方与模型|Add provider & model/}).click()
      await page.waitForURL('**'+navSettings)
      const providers=page.getByRole('region',{name:'模型提供方和目录'})
      await providers.getByText('还没有提供方。',{exact:true}).waitFor()
      await providers.getByRole('button',{name:'添加',exact:true}).click()
      const add=providers.getByRole('form',{name:'添加服务提供方'})
      await add.getByLabel('名称',{exact:true}).fill('本机模型验证')
      await add.getByLabel('服务地址',{exact:true}).fill(provider)
      await add.getByLabel('API Key',{exact:true}).fill(badKey)
      const save=exactResponse('POST',settings+'/providers')
      await add.getByRole('button',{name:'保存连接'}).click()
      const saved=await save
      assert.equal(saved.status(),201)
      const wrong=await saved.json()
      assert.match(wrong.id,/^[a-f0-9]{32}$/)
      assert.equal(wrong.credential_status,'configured')
      phase='diagnose rejected, edit, diagnose corrected'
      await providers.getByRole('button',{name:'诊断连接'}).click()
      await providers.getByText(/连接：可达；认证：被拒绝.*401.*不代表模型试答成功/).waitFor()
      await providers.getByRole('button',{name:'编辑连接/凭据'}).click()
      const edit=providers.getByRole('form',{name:'编辑提供方连接'})
      assert.equal(await edit.getByLabel('API Key',{exact:true}).inputValue(),'')
      await edit.getByLabel('API Key',{exact:true}).fill(key)
      const savereplacement=exactResponse('PUT',settings+'/providers/'+wrong.id)
      await edit.getByRole('button',{name:'保存连接'}).click()
      const corrected=await savereplacement
      assert.equal(corrected.status(),200)
      const right=await corrected.json()
      assert.equal(right.revision,wrong.revision+1)
      const stale=await api('PUT',settings+'/providers/'+wrong.id,{name:wrong.name,protocol:wrong.protocol,base_url:provider,auth_mode:'api_key',revision:wrong.revision})
      assert.equal(stale.status,409)
      await providers.getByRole('button',{name:'诊断连接'}).click()
      await providers.getByText(/连接：可达；认证：未被拒绝.*200.*不代表模型试答成功/).waitFor()
      phase='discover model, fixed probe, select default explicitly'
      await providers.getByRole('button',{name:'自动拉取模型'}).click()
      const fetched=providers.getByRole('listitem').filter({hasText:model})
      await fetched.getByRole('button',{name:'添加',exact:true}).click()
      const addModel=providers.getByRole('form',{name:'添加 '+model})
      if(await addModel.getByLabel('显示名称',{exact:true}).inputValue()!==model)await addModel.getByLabel('显示名称',{exact:true}).fill(model)
      const saveModel=exactResponse('POST',settings+'/catalog')
      await addModel.getByRole('button',{name:'保存修改'}).click()
      const created=await saveModel
      assert.equal(created.status(),201)
      const item=await created.json()
      assert.equal(item.provider_revision,right.revision)
      const editor=providers.getByRole('form',{name:'编辑模型 '+model})
      await editor.getByRole('button',{name:'试答测试'}).click()
      await editor.getByText(/已保存版本试答通过.*不会切换对话路由/).waitFor()
      assert.notEqual((await api('GET',settings+'/default')).json.profile_id,item.id)
      page.once('dialog',dialog=>dialog.dismiss())
      await editor.getByRole('button',{name:'设为全局默认'}).click()
      assert.notEqual((await api('GET',settings+'/default')).json.profile_id,item.id)
      page.once('dialog',dialog=>dialog.accept())
      const setDefault=exactResponse('PUT',settings+'/default')
      await editor.getByRole('button',{name:'设为全局默认'}).click()
      assert.equal((await setDefault).status(),200)
      await providers.getByText(/全局默认模型已更新/).waitFor()
      assert.equal((await api('GET',settings+'/default')).json.profile_id,item.id)
      assert.equal((await api('GET',settings+'/providers',undefined,member.request)).status,403)
      phase='reload and submit a real user message'
      await page.goto(origin+'/chat')
      const run=await submitChat()
      await page.waitForURL('**/chat/'+run.conversation_id)
      await page.reload()
      await page.getByText(reply,{exact:true}).first().waitFor()
      assert.equal(writes.filter(s=>s==='PUT '+settings+'/default').length,1)
      assert.equal(writes.filter(s=>s==='POST '+settings+'/catalog/'+item.id+'/test').length,1)
      assert.ok(!(await page.locator('body').innerText()).includes(key))
    }else if(scenario==='skill_draft_update_disable'){
      const promptSkill='查看本会话可绑定的 Skill'
      phase='empty Skill catalog, preview and exact first import'
      await page.goto(origin+navSkills)
      await page.getByText('暂无 Skill，可新建文字草稿。',{exact:true}).waitFor()
      const document=name=>`---\nname: settings-fixture\ndescription: Synthetic main document test\n---\n${name}\n`
      const importDocument=async(body,version)=>{
        const allowed=page.getByRole('button',{name:'导入 SKILL.md',exact:true})
        if(await allowed.getAttribute('aria-expanded')==='false')await allowed.click()
        const source=page.getByRole('region',{name:'导入 SKILL.md 主文档'})
        const file={name:'SKILL.md',mimeType:'text/markdown',buffer:Buffer.from(document(body))}
        await source.getByLabel('工作范围').fill(skill)
        await source.locator('input[type=file]').setInputFiles(file)
        const preview=exactResponse('POST',skills+'/import-preview')
        await source.getByRole('button',{name:'预览主文档'}).click()
        const previewResponse=await preview
        assert.equal(previewResponse.status(),200)
        const proof=await previewResponse.json()
        assert.match(proof.source.source_sha256,/^[a-f0-9]{64}$/)
        assert.match(proof.body_sha256,/^[a-f0-9]{64}$/)
        assert.equal(proof.instructions,body)
        const checked=source.getByRole('region',{name:'服务端检查的导入预览'})
        await checked.getByText(body,{exact:true}).waitFor()
        if(version===1){
          assert.deepEqual((await api('GET',skills)).json,[])
          const mismatch=await api('POST',skills+'/import',{filename:'SKILL.md',document:document(bodyTwo),scope_label:skill,revision:proof.revision,expected_source_sha256:proof.source.source_sha256,preview_binding:proof.preview_binding})
          assert.equal(mismatch.status,409)
          assert.deepEqual((await api('GET',skills)).json,[])
        }
        const importing=exactResponse('POST',skills+'/import')
        await checked.getByRole('button',{name:'确认导入为草稿'}).click()
        const imported=await importing
        assert.equal(imported.status(),201)
        const row=await imported.json()
        assert.equal(row.version,version)
        assert.equal(row.active,false)
        assert.ok(!row.approved_at)
        assert.equal(row.sha256,proof.body_sha256)
        await source.getByText(new RegExp('已核对 v'+version+'。导入本身不启用；服务器返回时当前未启用。')).waitFor()
        await source.getByRole('button',{name:'收起'}).click()
        return row
      }
      const first=await importDocument(bodyOne,1)
      assert.deepEqual((await api('GET',skills,undefined,member.request)).json,[])
      assert.equal((await api('POST',skills+'/'+first.skill_id+'/activate',{version:1,revision:first.revision,sha256:first.sha256},member.request)).status,403)
      phase='explicit enable is not conversation binding'
      const activated=exactResponse('POST',skills+'/'+first.skill_id+'/activate')
      await page.getByRole('button',{name:'启用 v1',exact:true}).click()
      assert.equal((await activated).status(),204)
      await page.getByText(/服务器已确认启用该版本。未自动设定会话绑定或执行/).waitFor()
      await page.goto(origin+'/chat')
      await page.getByRole('textbox',{name:/输入消息/}).fill(promptSkill)
      const open=exactResponse('POST',base+'/conversations')
      await page.getByRole('button',{name:'发送消息',exact:true}).click()
      const opened=await open
      assert.equal(opened.status(),202)
      const original=await opened.json()
      const conv=base+'/conversations/'+original.conversation_id
      const readBinding=()=>api('GET',conv+'/skill-binding')
      assert.equal((await readBinding()).json.binding,null)
      const card=page.getByRole('article',{name:'对话Skill 绑定预览'})
      const select=card.getByRole('button',{name:'预览绑定：'+skill+' · 第 1 版（1）',exact:true})
      await select.click()
      await card.getByText(/仅影响以后此会话；尚未绑定/).waitFor()
      assert.equal((await readBinding()).json.binding,null)
      phase='separate human confirmation saves only the original v1'
      const confirm=exactResponse('PUT',conv+'/skill-binding')
      await card.getByRole('button',{name:'确认绑定：'+skill+' · 第 1 版',exact:true}).click()
      assert.equal((await confirm).status(),200)
      await card.getByText(/已保存本次 Skill 绑定/).waitFor()
      const bound=(await readBinding()).json
      assert.equal(bound.binding.version,1);assert.equal(bound.binding.validity,'valid')
      assert.equal(bound.binding_revision,1)
      assert.equal((await api('GET',conv+'/skill-binding',undefined,member.request)).status,404)
      const ordinary=await submitChat()
      assert.equal(ordinary.conversation_id,original.conversation_id)
      const used=await api('GET',base+'/runs/'+ordinary.run_id+'/skill-binding')
      assert.equal(used.status,200);assert.equal(used.json.binding,null)
      phase='import updated draft does not repoint or load previous selected version'
      const convURL='/chat/'+original.conversation_id
      await page.goto(origin+navSkills)
      const second=await importDocument(bodyTwo,2)
      assert.equal(second.skill_id,first.skill_id)
      const rows=(await api('GET',skills)).json
      assert.deepEqual(rows.map(x=>[x.version,x.active]).sort((a,b)=>a[0]-b[0]),[[1,true],[2,false]])
      assert.deepEqual((await readBinding()).json,bound)
      assert.equal((await api('PUT',conv+'/skill-binding',{action:'bind_skill',skill_id:second.skill_id,version:2,expected_binding_revision:bound.binding_revision})).status,409)
      assert.deepEqual((await readBinding()).json,bound)
      phase='disable revokes the existing binding without enabling draft'
      const disabled=exactResponse('POST',skills+'/'+first.skill_id+'/disable')
      await page.getByRole('checkbox',{name:'停用 '+skill+' v1',exact:true}).click()
      assert.equal((await disabled).status(),204)
      await page.getByText(/服务器已确认停用。该版本不会再成为新的选用项/).waitFor()
      assert.ok((await api('GET',skills)).json.every(row=>!row.active))
      assert.deepEqual((await api('GET',skills,undefined,member.request)).json,[])
      await page.goto(origin+convURL)
      await card.getByText(/本次 Skill 绑定曾保存；当前选用已变化或失效/).waitFor()
      assert.equal((await readBinding()).json.binding.validity,'revoked')
      assert.equal(writes.filter(x=>x==='POST '+conv+'/skill-card/'+original.run_id+'/actions').length,1)
      assert.equal(writes.filter(x=>x==='PUT '+conv+'/skill-binding').length,1)
    }else throw new Error('unknown settings scenario')
    assert.deepEqual(outside,[])
    assert.deepEqual(exposed,[])
    process.stdout.write(JSON.stringify({scenario,verified:true,settingsAndChatOnly:true}))
  }catch(error){
    const body=page?await page.locator('main').first().innerText().catch(()=>''):'none'
    throw new Error(phase+': '+String(error)+'\n'+body,{cause:error})
  }finally{await browser.close()}
}else{
  throw new Error('unknown browser fixture mode')
}
