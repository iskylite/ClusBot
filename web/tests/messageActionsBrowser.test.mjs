import assert from 'node:assert/strict'
import {createServer} from 'node:http'
import {test} from 'node:test'
import {fileURLToPath,pathToFileURL} from 'node:url'
import {build} from 'vite'

// Exercise the real bundle with fixture API replies and an isolated clipboard
// seam. No credentials, model calls, system clipboard or built assets on disk.
test('message copy and quote work using browser pointer, keyboard and touch targets',{
  skip:!process.env.CLUSBOT_PLAYWRIGHT_MODULE&&'Set CLUSBOT_PLAYWRIGHT_MODULE to installed Playwright',timeout:90000,
},async()=>{
  const outputs=await build({root:fileURLToPath(new URL('../',import.meta.url)),configFile:false,
    logLevel:'error',build:{write:false,reportCompressedSize:false}})
  const assets=new Map((Array.isArray(outputs)?outputs:[outputs]).flatMap(output=>output.output)
    .map(item=>[item.fileName,item.type==='chunk'?item.code:item.source]))
  const mime={js:'text/javascript',css:'text/css',html:'text/html',woff2:'font/woff2',svg:'image/svg+xml',png:'image/png'}
  const account={principal_id:'fixture-principal',username:'fixture-user',display_name:'局部测试',avatar:'局',
    status:'active',is_superadmin:false,must_change_password:false}
  const conversation={id:'message-actions-fixture',title:'消息操作测试',archived:false,revision:1}
  const path='/conversations/'+conversation.id
  const conversationURL='/chat/'+conversation.id
  const userText='请测试这个选段，然后保持草稿。'
  const selection='这个选段'
  const answer='**答案** \n\n不复制消息头。'
  const items=[{id:1,role:'user',text:userText,replace_materials:true},
    {id:2,role:'assistant',text:answer},{id:3,role:'user',text:'',asset_query_task_id:'fixture-asset'}]
  const requests=[]
  const server=createServer(async(req,res)=>{
    const url=new URL(req.url,'http://localhost')
    if(url.pathname.startsWith('/api/v1/')){
      const key=url.pathname.slice(7)
      const json=value=>{res.setHeader('Content-Type','application/json');res.end(JSON.stringify(value))}
      if(req.method==='POST'&&key===path+'/messages'){
        const chunks=[]
        for await(const c of req)chunks.push(c)
        const body=JSON.parse(Buffer.concat(chunks).toString())
        requests.push(body)
        if(!items.some(item=>item.id===4))items.push({id:4,role:'user',text:body.text})
        json({conversation_id:conversation.id})
      }else if(req.method!=='GET'){
        requests.push({unexpected:key,method:req.method})
        res.statusCode=405;json({error:'unexpected mutation'})
      }else if(key==='/session')json({authenticated:true,user:account})
      else if(key==='/preferences')json({language:'zh-CN',timezone:'system',appearance:'light',reduce_motion:true,
        send_shortcut:'enter',desktop_notifications:false,revision:1})
      else if(key===path)json({conversation,messages:items})
      else if(key===path+'/history')json({items})
      else if(key==='/conversations/history')json({items:[conversation]})
      else if(key==='/assistant/model-routing')json({roles:[]})
      else if(key==='/assistant/composer-options')json({commands:[],models:[],tasks:[],members:[]})
      else if(key.endsWith('/events')){res.setHeader('Content-Type','text/event-stream');res.end(': fixture\n\n')}
      else if(key==='/overview')json({scopes:[],groups:{waiting:[],active:[],completed:[]},counts:{waiting:0,active:0,completed:0},next_cursors:{},snapshot_at:1})
      else json([])
      return
    }
    const key=url.pathname.startsWith('/assets/')?url.pathname.slice(1):'index.html'
    if(!assets.has(key)){res.statusCode=404;res.end();return}
    res.setHeader('Content-Type',mime[key.split('.').at(-1)]??'application/octet-stream')
    res.end(assets.get(key))
  })
  await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve))
  const origin='http://127.0.0.1:'+server.address().port
  const engine=process.env.CLUSBOT_MESSAGE_ACTIONS_ENGINE??'chromium'
  const playwright=await import(pathToFileURL(process.env.CLUSBOT_PLAYWRIGHT_MODULE).href)
  let browser
  try {
    browser=await playwright[engine].launch({headless:true,executablePath:engine==='chromium'?process.env.CLUSBOT_CHROMIUM:undefined,
      ...(engine==='chromium'?{args:['--disable-background-networking','--disable-sync']}:{})})
    const outside=[]
    const pageErrors=[]
    const page=await browser.newPage({serviceWorkers:'block',viewport:{width:1360,height:960}})
    page.setDefaultTimeout(10000)
    page.on('pageerror',error=>pageErrors.push(error.stack??error.message))
    await page.route('**/*',route=>{
      const host=new URL(route.request().url()).origin
      if(host===origin)return route.continue()
      outside.push(host);return route.abort()
    })
    await page.addInitScript(()=>{
      window.fixtureClipboard=[]
      Object.defineProperty(navigator,'clipboard',{value:{writeText:async text=>{window.fixtureClipboard.push(text)}}})
    })
    await page.goto(origin+conversationURL)
    const source=page.locator('#message-1')
    const reply=page.locator('#message-2')
    const copy=source.getByRole('button',{name:'复制',exact:true})
    const quote=source.getByRole('button',{name:'引用回复',exact:true})
    const input=page.getByRole('textbox',{name:/输入消息，/})
    await copy.waitFor()
    await page.waitForFunction(()=>document.documentElement.dataset.appearance==='light')
    await page.evaluate(()=>document.fonts.ready)
    assert.equal(await page.locator('#message-3 .chat-message-actions').count(),0)
    await source.locator('.message-content-body').hover()
    await page.waitForFunction(()=>{const row=document.querySelector('#message-1 .chat-message-actions');return row&&getComputedStyle(row).opacity==='1'})
    const target=await copy.boundingBox()
    assert.ok(target.width>=32&&target.height>=32)
    // Click one pixel above the bottom of the button, outside the visible
    // 16px label, to verify the real hit box rather than the label alone.
    await page.mouse.click(target.x+target.width/2,target.y+target.height-1)
    await source.getByRole('status').filter({hasText:'已复制'}).waitFor()
    assert.deepEqual(await page.evaluate(()=>window.fixtureClipboard),[userText])
    await source.locator('.message-content-body > div').evaluate((node,text)=>{
      const at=node.textContent.indexOf(text)
      const selected=document.createRange()
      selected.setStart(node.firstChild,at);selected.setEnd(node.firstChild,at+text.length)
      window.getSelection().removeAllRanges();window.getSelection().addRange(selected)
    },selection)
    await copy.click()
    assert.deepEqual(await page.evaluate(()=>window.fixtureClipboard),[userText,selection])

    await input.fill('保留的原稿')
    await source.locator('.message-content-body > div').evaluate((node,text)=>{
      const at=node.textContent.indexOf(text)
      const selected=document.createRange()
      selected.setStart(node.firstChild,at);selected.setEnd(node.firstChild,at+text.length)
      window.getSelection().removeAllRanges();window.getSelection().addRange(selected)
    },selection)
    await quote.click()
    assert.equal(await input.inputValue(),'保留的原稿\n\n> '+selection+'\n\n')
    assert.equal(requests.length,0)
    assert.equal(await input.evaluate(node=>node===document.activeElement),true)
    await page.getByRole('button',{name:'返回原消息',exact:true}).click()
    await page.waitForFunction(()=>document.activeElement?.id==='message-1')
    assert.equal(new URL(page.url()).hash,'#message-1')
    await page.getByRole('button',{name:'移除消息引用',exact:true}).click()
    assert.equal(await input.inputValue(),'保留的原稿')
    assert.equal(requests.length,0)
    await page.evaluate(()=>window.getSelection().removeAllRanges())

    // Tab from preceding native message control and activate with Enter.
    await page.bringToFront()
    await source.getByRole('button',{name:'从这条消息新增记忆'}).focus()
    // Some WebKit platforms skip buttons on Tab unless the platform's
    // full keyboard access setting is on. Alt+Tab enables it for the test.
    await page.keyboard.press(engine==='webkit'?'Alt+Tab':'Tab')
    assert.equal(await quote.evaluate(node=>node===document.activeElement),true)
    await page.waitForFunction(()=>{const row=document.querySelector('#message-1 .chat-message-actions');return row&&getComputedStyle(row).opacity==='1'})
    await page.keyboard.press('Enter')
    assert.equal(await input.inputValue(),'保留的原稿\n\n> '+userText+'\n\n')
    assert.equal(requests.length,0)
    await input.fill('用户编辑了引用')
    await page.getByRole('button',{name:'移除消息引用',exact:true}).waitFor({state:'detached'})
    const copyReply=reply.getByRole('button',{name:'复制',exact:true})
    await copyReply.focus()
    await page.keyboard.press('Space')
    await reply.getByRole('status').filter({hasText:'已复制'}).waitFor()
    assert.deepEqual(await page.evaluate(()=>window.fixtureClipboard),[userText,selection,answer])
    await page.evaluate(()=>window.getSelection().removeAllRanges())
    await source.locator('.message-content-body').hover()
    await quote.click()
    const explicitDraft='用户编辑了引用\n\n> '+userText+'\n\n'
    assert.equal(await input.inputValue(),explicitDraft)
    assert.equal(requests.length,0)
    const sent=page.waitForResponse(response=>response.request().method()==='POST'&&
      new URL(response.url()).pathname==='/api/v1'+path+'/messages')
    await page.getByRole('button',{name:'发送消息',exact:true}).click()
    assert.equal((await sent).status(),200)
    assert.equal(requests.length,1)
    assert.equal(requests[0].text,explicitDraft)
    assert.equal(requests[0].task_ref,undefined)
    assert.deepEqual(Object.keys(requests[0]).sort(),['attachment_ids','client_message_id','text'])
    await page.waitForFunction(()=>document.querySelector('form.composer textarea')?.value==='').catch(async error=>{
      throw new Error(JSON.stringify({route:page.url(),pageErrors,info:await page.locator('body').innerText()}),{cause:error})
    })
    assert.equal(await input.inputValue(),'')
    await page.locator('#message-4').waitFor()
    assert.deepEqual(pageErrors,[])
    assert.deepEqual(outside,[])

    const touch=await browser.newPage({hasTouch:true,isMobile:true,viewport:{width:390,height:840}})
    await touch.route('**/*',route=>new URL(route.request().url()).origin===origin?route.continue():route.abort())
    await touch.goto(origin+conversationURL)
    const touchQuote=touch.locator('#message-1').getByRole('button',{name:'引用回复',exact:true})
    await touchQuote.waitFor()
    assert.ok(await touchQuote.evaluate(node=>node.getBoundingClientRect().height>=44&&node.getBoundingClientRect().width>=44))
    assert.equal(await touchQuote.evaluate(node=>getComputedStyle(node.closest('.chat-message-actions')).opacity),'1')
    await touchQuote.tap()
    assert.equal(await touch.getByRole('textbox',{name:/输入消息，/}).inputValue(),'> '+userText+'\n\n')
    assert.equal(requests.length,1)
  } finally {
    await browser?.close()
    await new Promise(resolve=>server.close(resolve))
  }
})
