import {useEffect,useState,type FormEvent} from 'react'
import {ArrowLeft,ChevronDown,ChevronRight,Globe,LoaderCircle,Play,Plug,Plus,Search,Terminal,TestTube2} from 'lucide-react'
import {api,type MCPConfig,type MCPConnection,type MCPResult,type MCPTool} from './api'
import {ToolResultDownload} from './TaskRead'

const transportName={http:'Streamable HTTP',stdio:'stdio'}
const checkTime=new Intl.DateTimeFormat('zh-CN',{month:'numeric',day:'numeric',hour:'numeric',minute:'numeric'})
const errorText=(error:unknown)=>error instanceof Error?error.message:'操作未完成'
const lines=(text:string)=>text.split('\n').filter(line=>line.length>0)
function statusLabel(connection:MCPConnection) {
  if(!connection.enabled)return '已停用'
  if(connection.status==='ready')return `已发现 ${connection.tool_count} 个工具`
  return connection.status==='failed'?'测试失败':'待测试'
}

export function ConnectionsPage({selected,navigate}:{selected?:string;navigate:(path:string)=>void}) {
  const [items,setItems]=useState<MCPConnection[]>()
  const [detail,setDetail]=useState<MCPConnection>()
  const [query,setQuery]=useState('')
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
  return <>
    <main className={'main memory-list '+(selected?'memory-list-hidden':'')}>
      <header className="page-heading"><h1>工具与连接</h1><button className="primary" onClick={()=>navigate('/connections/new')}><Plus size={17}/>添加连接</button></header>
      <div className="memory-toolbar"><label className="search"><Search size={17}/><input aria-label="搜索连接" placeholder="搜索连接" value={query} onChange={e=>setQuery(e.target.value)}/></label></div>
      {!items && !error && <LoaderCircle className="spin" aria-label="正在加载连接"/>}
      {visible?.map(item=>{
        const Icon=item.config.transport==='http'?Globe:Terminal
        return <button key={item.id} className={'memory-row connection-row '+(active===item.id?'selected':'')} onClick={()=>navigate('/connections/'+item.id)}>
          <span className="memory-icon"><Icon size={18}/></span>
          <span className="memory-row-content"><strong>{item.name}</strong><small>{transportName[item.config.transport]}</small></span>
          <span className={'connection-status '+(item.enabled?item.status:'disabled')}>{statusLabel(item)}</span>
          <ChevronRight size={16} className="memory-row-arrow"/>
        </button>
      })}
      {visible?.length===0 && <div className="empty"><span className="empty-icon"><Plug size={26}/></span><h2>{items?.length?'没有匹配的连接':'还没有连接'}</h2></div>}
      {error && <p className="error" role="alert">{error}</p>}
    </main>
    <aside className={'detail-pane memory-detail '+(selected?'memory-detail-open':'')}>
      <div className="detail-content">
        <button className="text-button memory-back" onClick={()=>navigate('/connections')}><ArrowLeft size={16}/>工具与连接</button>
        {fresh || (detail && detail.id===active) ?
          <ConnectionEditor key={fresh?'new':active} item={fresh?undefined:detail} updated={updated}/> :
          error ? <p className="error" role="alert">{error}</p> : active ? <LoaderCircle className="spin" aria-label="正在加载连接"/>
            : <div className="pane-empty"><Plug size={26}/><p>选择一个连接</p></div>}
      </div>
    </aside>
  </>
}

type Draft={name:string;enabled:boolean;transport:MCPConfig['transport'];endpoint:string;tokenEnv:string;command:string;args:string;env:string}
function draftFrom(item?:MCPConnection):Draft {
  return {name:item?.name??'',enabled:item?.enabled??true,transport:item?.config.transport??'stdio',
    endpoint:item?.config.endpoint??'',tokenEnv:item?.config.token_env??'',command:item?.config.command??'',
    args:item?.config.args?.join('\n')??'',env:item?.config.env?.join('\n')??''}
}

function ConnectionEditor({item,updated}:{item?:MCPConnection;updated:(result:MCPConnection)=>void}) {
  const [draft,setDraft]=useState(()=>draftFrom(item))
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
      setDraft(draftFrom(result));setDirty(false);updated(result)
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
    <form className="memory-form connection-form" onSubmit={save}>
      <header className="page-heading"><h2>{item?.name??'添加连接'}</h2></header>
      <label>名称<input aria-label="连接名称" maxLength={60} required value={draft.name} disabled={Boolean(busy)} onChange={e=>edit({name:e.target.value})}/></label>
      <label>连接方式<select aria-label="连接方式" value={draft.transport} disabled={Boolean(busy)} onChange={e=>edit({transport:e.target.value as MCPConfig['transport']})}>
        <option value="stdio">stdio</option><option value="http">Streamable HTTP</option>
      </select></label>
      {draft.transport==='http' ? <>
        <label>服务地址<input aria-label="服务地址" type="url" required placeholder="http://127.0.0.1:8811/mcp" value={draft.endpoint} disabled={Boolean(busy)} onChange={e=>edit({endpoint:e.target.value})}/></label>
        <label>Bearer 环境变量<input aria-label="Bearer 环境变量" placeholder="无认证时留空" value={draft.tokenEnv} disabled={Boolean(busy)} onChange={e=>edit({tokenEnv:e.target.value})}/></label>
      </> : <>
        <label>启动程序<input aria-label="启动程序" required placeholder="/usr/local/bin/example-mcp" value={draft.command} disabled={Boolean(busy)} onChange={e=>edit({command:e.target.value})}/></label>
        <label>参数<textarea aria-label="参数" rows={2} placeholder="每行一个参数" value={draft.args} disabled={Boolean(busy)} onChange={e=>edit({args:e.target.value})}/></label>
        <label>环境变量<textarea aria-label="环境变量" rows={2} placeholder="每行一个变量名" value={draft.env} disabled={Boolean(busy)} onChange={e=>edit({env:e.target.value})}/></label>
      </>}
      <label className="memory-pin"><input type="checkbox" checked={draft.enabled} disabled={Boolean(busy)} onChange={e=>edit({enabled:e.target.checked})}/>启用连接</label>
      <div className="memory-form-actions">
        {item && <button type="button" className="outline" disabled={Boolean(busy)||dirty||!item.enabled} onClick={()=>void probe()}>{busy==='test'?<LoaderCircle className="spin" size={16}/>:<TestTube2 size={16}/>}测试连接</button>}
        <button className="primary" disabled={Boolean(busy)||Boolean(item&&!dirty)}>{busy==='save'&&<LoaderCircle className="spin" size={16}/>}保存连接</button>
      </div>
    </form>
    {error && <p className="error" role="alert">{error}</p>}
    {item && <section className="connection-tools">
      <div className="connection-tools-heading"><h3>工具权限</h3><span className={'connection-status '+item.status}>{statusLabel(item)}</span></div>
      {item.detail && <p className="error" role="alert">{item.detail}</p>}
      {item.protocol && item.checked_at && <small>MCP {item.protocol} · {checkTime.format(item.checked_at)}</small>}
      {item.tools?.map(tool=><ToolCard key={tool.name+tool.version} connection={item} tool={tool} blocked={Boolean(busy)||dirty}
        grant={()=>void grant(tool)}/>)}
    </section>}
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
  return <details className="connection-tool">
    <summary><span><strong>{tool.title||tool.name}</strong>{tool.title&&tool.title!==tool.name&&<small>{tool.name}</small>}</span>
      <small className="connection-tool-status">{tool.schema_error?'定义不兼容':tool.allowed?'已授权读取':'未授权'}<ChevronDown size={15}/></small></summary>
    {tool.description&&<p>{tool.description}</p>}
    {tool.schema_error&&<p className="error" role="alert">{tool.schema_error}</p>}
    <details><summary>参数定义</summary><pre>{JSON.stringify(tool.input_schema,null,2)}</pre></details>
    {tool.output_schema!==undefined&&<details><summary>输出定义</summary><pre>{JSON.stringify(tool.output_schema,null,2)}</pre></details>}
    <button type="button" className="outline" disabled={blocked||busy||((!ready||Boolean(tool.schema_error))&&!tool.allowed)} onClick={grant}>{tool.allowed?'撤销授权':'授权读取'}</button>
    {tool.allowed && ready && !tool.schema_error && <form className="tool-call" onSubmit={call}>
      <label>调用参数<textarea aria-label="调用参数" rows={2} value={input} maxLength={24576} onChange={e=>setInput(e.target.value)}/></label>
      <button className="outline" disabled={blocked||busy}>{busy?<LoaderCircle className="spin" size={16}/>:<Play size={16}/>}手动调用</button>
    </form>}
    {error && <p className="error" role="alert">{error}</p>}
    {result && <div className="tool-result" role="status"><strong>{result.validation_error?'结果校验失败':result.is_error?'工具返回错误':'调用结果'}</strong>
      {result.validation_error&&<p className="error">{result.validation_error}</p>}
      <div className="tool-result-meta">
        <small>{checkTime.format(result.received_at)}{result.truncated?(result.reference?' · 预览已截断':' · 结果已截断'):''}{result.media_omitted?' · 媒体未展示':''}</small>
        {result.reference&&<ToolResultDownload reference={result.reference}/>}
      </div>
      {result.text && <pre>{result.text}</pre>}
      {result.structured!==undefined && <pre>{JSON.stringify(result.structured,null,2)}</pre>}
    </div>}
  </details>
}
