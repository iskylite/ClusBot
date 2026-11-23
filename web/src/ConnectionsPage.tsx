import {useEffect,useState,type FormEvent} from 'react'
import {ArrowLeft,KeyRound,LoaderCircle,Play,Plug,Plus,RefreshCw,Search,Wrench} from 'lucide-react'
import {api,type MCPConfig,type MCPConnection,type MCPResult,type MCPTool} from './api'
import {ToolResultDownload} from './TaskRead'
import './ConnectionSkillSettings.css'

const transportName={http:'Streamable HTTP',stdio:'stdio'}
const checkTime=new Intl.DateTimeFormat('zh-CN',{month:'numeric',day:'numeric',hour:'numeric',minute:'numeric'})
const errorText=(error:unknown)=>error instanceof Error?error.message:'操作未完成'
const lines=(text:string)=>text.split('\n').filter(line=>line.length>0)
function schemaSummary(schema:unknown) {
  if(!schema||typeof schema!=='object'||Array.isArray(schema))return JSON.stringify(schema)
  const value=schema as {type?:unknown;properties?:unknown;required?:unknown}
  if(value.type!=='object'||!value.properties||typeof value.properties!=='object'||Array.isArray(value.properties))return JSON.stringify(schema)
  const required=Array.isArray(value.required)?value.required:[]
  const fields=Object.entries(value.properties).map(([name,definition])=>{
    const type=definition&&typeof definition==='object'&&!Array.isArray(definition)?(definition as {type?:unknown}).type:undefined
    return `${name}${required.includes(name)?'':'?'}: ${typeof type==='string'?type:'…'}`
  })
  return `{ ${fields.join(', ')} }`
}
function statusLabel(connection:MCPConnection) {
  if(!connection.enabled)return '已停用'
  if(connection.status==='ready')return `已握手 · ${connection.tool_count} 工具`
  return connection.status==='failed'?'探测失败':'待握手'
}

export function ConnectionsPage({selected,navigate}:{selected?:string;navigate:(path:string)=>void}) {
  const [items,setItems]=useState<MCPConnection[]>()
  const [detail,setDetail]=useState<MCPConnection>()
  const [query,setQuery]=useState('')
  const [showSearch,setShowSearch]=useState(false)
  const [error,setError]=useState('')
  const fresh=selected==='new'
  const active=fresh?undefined:selected??items?.[0]?.id
  useEffect(()=>{
    let cancelled=false
    api<MCPConnection[]>('/connections').then(entries=>{if(!cancelled)setItems(entries)}).catch(e=>{if(!cancelled)setError(errorText(e))})
    return()=>{cancelled=true}
  },[])
  useEffect(()=>{
    if(!active)return
    let cancelled=false
    setError('')
    api<MCPConnection>('/connections/'+active).then(value=>{if(!cancelled)setDetail(value)}).catch(e=>{if(!cancelled)setError(errorText(e))})
    return()=>{cancelled=true}
  },[active])
  function updated(value:MCPConnection) {
    setDetail(value)
    setItems(previous=>[...(previous??[]).filter(item=>item.id!==value.id),value].sort((a,b)=>a.name.localeCompare(b.name,'zh-CN')))
    if(fresh)navigate('/connections/'+value.id)
  }
  const visible=items?.filter(item=>item.name.toLowerCase().includes(query.trim().toLowerCase()))
  const canSearch=showSearch||Boolean(items&&items.length>8)
  return <main className="main connections-workspace">
    <h1 className="settings-sr-only">MCP 连接与工具</h1>
    <section className={'connection-list-panel settings-split-panel '+(selected?'connection-mobile-hidden':'')} aria-labelledby="connections-title">
      <header className="settings-split-heading"><h2 id="connections-title">MCP 连接{items&&` (${items.length})`}</h2><div className="settings-split-actions">{canSearch&&<button type="button" className="connection-search-button" aria-label="搜索连接" aria-expanded={showSearch} onClick={()=>{setShowSearch(!showSearch);if(showSearch)setQuery('')}}><Search size={14} aria-hidden="true"/></button>}<button type="button" className="primary" onClick={()=>navigate('/connections/new')}><Plus size={14} aria-hidden="true"/>添加连接</button></div></header>
      {showSearch&&<label className="connection-search"><Search size={14}/><input type="search" aria-label="搜索连接" placeholder="搜索连接" value={query} onChange={e=>setQuery(e.target.value)}/></label>}
      {!items && !error && <LoaderCircle className="spin" aria-label="正在加载连接"/>}
      <div className="connection-list">{visible?.map(item=><button type="button" key={item.id} className={'connection-list-item '+(active===item.id?'selected':'')} aria-pressed={active===item.id} onClick={()=>navigate('/connections/'+item.id)}>
        <span className="connection-list-top"><strong>{item.name}</strong><span className={'connection-status '+(item.enabled?item.status:'disabled')}>{statusLabel(item)}</span></span>
        <small>{item.config.transport==='http'?item.config.endpoint:transportName.stdio+' · 本地受控进程'}</small>
      </button>)}</div>
      {visible?.length===0 && <p className="settings-split-empty">{items?.length?'没有匹配的连接':'还没有连接'}</p>}
      {error && <p className="error" role="alert">{error}</p>}
    </section>
    <section className={'connection-detail-panel settings-split-panel '+(selected?'connection-mobile-open':'')} aria-label="连接详情">
      <button type="button" className="text-button connection-back" onClick={()=>navigate('/connections')}><ArrowLeft size={16}/>MCP 连接</button>
      {fresh || (detail && detail.id===active) ?
        <ConnectionEditor key={fresh?'new':active} item={fresh?undefined:detail} updated={updated}/> :
        error ? <p className="error" role="alert">{error}</p> : active ? <LoaderCircle className="spin" aria-label="正在加载连接"/>
          : <div className="settings-split-empty"><Plug size={24}/><p>选择一个连接</p></div>}
    </section>
  </main>
}

type Draft={name:string;enabled:boolean;transport:MCPConfig['transport'];endpoint:string;tokenEnv:string;command:string;args:string;env:string}
function draftFrom(item?:MCPConnection):Draft {
  return {name:item?.name??'',enabled:item?.enabled??true,transport:item?.config.transport??'stdio',
    endpoint:item?.config.endpoint??'',tokenEnv:item?.config.token_env??'',command:item?.config.command??'',
    args:item?.config.args?.join('\n')??'',env:item?.config.env?.join('\n')??''}
}

function ConnectionEditor({item,updated}:{item?:MCPConnection;updated:(result:MCPConnection)=>void}) {
  const [draft,setDraft]=useState(()=>draftFrom(item))
  const [editing,setEditing]=useState(!item)
  const [dirty,setDirty]=useState(false)
  const [busy,setBusy]=useState('')
  const [error,setError]=useState('')
  function edit(value:Partial<Draft>) {setDraft({...draft,...value});setDirty(true);setError('')}
  async function save(event:FormEvent) {
    event.preventDefault()
    if(busy)return
    const config:MCPConfig=draft.transport==='http'?{transport:'http',endpoint:draft.endpoint.trim(),token_env:draft.tokenEnv.trim()}
      :{transport:'stdio',command:draft.command.trim(),args:lines(draft.args),env:lines(draft.env).map(line=>line.trim()).filter(Boolean)}
    setBusy('save');setError('')
    try {
      const result=await api<MCPConnection>(item?'/connections/'+item.id:'/connections',
        {name:draft.name,config,enabled:draft.enabled,revision:item?.revision},item?'PATCH':'POST')
      setDraft(draftFrom(result));setDirty(false);setEditing(false);updated(result)
    }catch(e){setError(errorText(e))}
    finally{setBusy('')}
  }
  async function probe() {
    if(!item || busy || dirty)return
    setBusy('test');setError('')
    try {updated(await api<MCPConnection>('/connections/'+item.id+'/test',{revision:item.revision}))}
    catch(e){setError(errorText(e))}
    finally{setBusy('')}
  }
  async function grant(tool:MCPTool) {
    if(!item || busy || dirty)return
    if(!tool.allowed && !window.confirm(`允许调用 ${tool.name}？请确认这个工具只读取数据。`))return
    setBusy('permission');setError('')
    try {
      await api('/connections/'+item.id+'/permission',{name:tool.name,version:tool.version,revision:item.revision,allowed:!tool.allowed})
      updated(await api<MCPConnection>('/connections/'+item.id))
    }catch(e){setError(errorText(e))}
    finally{setBusy('')}
  }
  return <>
    {editing ? <form className="memory-form connection-form" onSubmit={save}>
      <header className="settings-split-heading"><h2>{item?'编辑连接':'添加连接'}</h2></header>
      <label>名称<input name="name" maxLength={60} required value={draft.name} disabled={Boolean(busy)} onChange={e=>edit({name:e.target.value})}/></label>
      <label>连接方式<select name="transport" value={draft.transport} disabled={Boolean(busy)} onChange={e=>edit({transport:e.target.value as MCPConfig['transport']})}>
        <option value="stdio">stdio</option><option value="http">Streamable HTTP</option>
      </select></label>
      {draft.transport==='http' ? <>
        <label>服务地址<input name="endpoint" type="url" required placeholder="http://127.0.0.1:8811/mcp" value={draft.endpoint} disabled={Boolean(busy)} onChange={e=>edit({endpoint:e.target.value})}/></label>
        <label>Bearer 环境变量<input name="token_env" placeholder="无认证时留空" value={draft.tokenEnv} disabled={Boolean(busy)} onChange={e=>edit({tokenEnv:e.target.value})}/></label>
      </> : <>
        <label>启动程序<input name="command" required placeholder="/usr/local/bin/example-mcp" value={draft.command} disabled={Boolean(busy)} onChange={e=>edit({command:e.target.value})}/></label>
        <label>参数<textarea name="args" rows={2} placeholder="每行一个参数" value={draft.args} disabled={Boolean(busy)} onChange={e=>edit({args:e.target.value})}/></label>
        <label>环境变量<textarea name="env" rows={2} placeholder="每行一个变量名" value={draft.env} disabled={Boolean(busy)} onChange={e=>edit({env:e.target.value})}/></label>
      </>}
      <label className="memory-pin"><input type="checkbox" checked={draft.enabled} disabled={Boolean(busy)} onChange={e=>edit({enabled:e.target.checked})}/>启用连接</label>
      <div className="memory-form-actions">
        {item && <button type="button" className="outline" disabled={Boolean(busy)} onClick={()=>{setDraft(draftFrom(item));setDirty(false);setError('');setEditing(false)}}>取消</button>}
        <button type="submit" className="primary" disabled={Boolean(busy)||Boolean(item&&!dirty)}>{busy==='save'&&<LoaderCircle className="spin" size={16}/>}保存连接</button>
      </div>
    </form> : item && <section className="connection-tools">
      <header className="settings-split-heading connection-detail-heading"><div><h2>{item.name} · 工具与权限</h2>{item.protocol&&<span className="settings-tag" title={item.checked_at?`上次探测：${checkTime.format(item.checked_at)}`:undefined}>{item.protocol}</span>}</div>
        <div className="settings-split-actions"><button type="button" className="outline" disabled={Boolean(busy)||!item.enabled} onClick={()=>void probe()}>{busy==='test'?<LoaderCircle className="spin" size={14} aria-hidden="true"/>:<RefreshCw size={14} aria-hidden="true"/>}重新探测 Schema</button>
          <button type="button" className="outline" disabled={Boolean(busy)} onClick={()=>setEditing(true)}><KeyRound size={14} aria-hidden="true"/>编辑凭据</button></div></header>
      {item.detail && <p className="error" role="alert">{item.detail}</p>}
      {!item.tools?.length && <p className="settings-split-empty">{item.enabled?'暂无已发现的工具。':'连接已停用。'}</p>}
      {item.tools?.map(tool=><ToolCard key={tool.name+tool.version} connection={item} tool={tool} blocked={Boolean(busy)||dirty} grant={()=>void grant(tool)}/>)}
    </section>}
    {error && <p className="error" role="alert">{error}</p>}
  </>
}

function ToolCard({connection,tool,blocked,grant}:{connection:MCPConnection;tool:MCPTool;blocked:boolean;grant:()=>void}) {
  const [input,setInput]=useState('{}')
  const [result,setResult]=useState<MCPResult>()
  const [error,setError]=useState('')
  const [busy,setBusy]=useState(false)
  async function call(event:FormEvent) {
    event.preventDefault()
    if(busy||blocked||!tool.allowed||tool.schema_error)return
    setBusy(true);setError('');setResult(undefined)
    try {
      let args:unknown
      try {args=JSON.parse(input)}catch{throw new Error('请输入有效的 JSON 参数')}
      setResult(await api<MCPResult>('/connections/'+connection.id+'/call',{name:tool.name,revision:connection.revision,arguments:args}))
    }catch(e){setError(errorText(e))}
    finally{setBusy(false)}
  }
  const ready=connection.enabled && connection.status==='ready'
  return <article className="connection-tool-card">
    <div className="connection-tool-top"><div><Wrench size={14} aria-hidden="true"/><strong>{tool.name}</strong><span className={'settings-tag'+(tool.allowed&&!tool.schema_error?' connection-read-only':'')}>{tool.schema_error?'定义不兼容':tool.allowed?'只读':'未授权'}</span></div>
      <label className="settings-switch"><input type="checkbox" checked={tool.allowed} aria-label={`${tool.allowed?'撤销':'允许'} ${tool.name} 的读取权限`} disabled={blocked||busy||((!ready||Boolean(tool.schema_error))&&!tool.allowed)} onChange={grant}/><span/></label></div>
    {(tool.description||tool.title!==tool.name&&tool.title)&&<p>{tool.description||tool.title}</p>}
    {tool.schema_error&&<p className="error" role="alert">{tool.schema_error}</p>}
    <details className="connection-tool-details"><summary aria-label="展开参数定义与手动调用">Schema: <code>{schemaSummary(tool.input_schema)}</code></summary>
      <pre>{JSON.stringify(tool.input_schema,null,2)}</pre>
      {tool.output_schema!==undefined&&<details><summary>输出定义</summary><pre>{JSON.stringify(tool.output_schema,null,2)}</pre></details>}
      {tool.allowed && ready && !tool.schema_error && <form className="tool-call" onSubmit={call}>
        <label>调用参数<textarea name="arguments" rows={2} value={input} maxLength={24576} onChange={e=>setInput(e.target.value)}/></label>
        <button type="submit" className="outline" disabled={blocked||busy}>{busy?<LoaderCircle className="spin" size={16}/>:<Play size={16}/>}手动调用</button>
      </form>}
      {error && <p className="error" role="alert">{error}</p>}
      {result && <div className="tool-result" role="status"><strong>{result.validation_error?'结果校验失败':result.is_error?'工具返回错误':'调用结果'}</strong>
        {result.validation_error&&<p className="error">{result.validation_error}</p>}
        <div className="tool-result-meta"><small>{checkTime.format(result.received_at)}{result.truncated?(result.reference?' · 预览已截断':' · 结果已截断'):''}{result.media_omitted?' · 媒体未展示':''}</small>
          {result.reference&&<ToolResultDownload reference={result.reference}/>}</div>
        {result.text && <pre>{result.text}</pre>}{result.structured!==undefined && <pre>{JSON.stringify(result.structured,null,2)}</pre>}
      </div>}
    </details>
  </article>
}
