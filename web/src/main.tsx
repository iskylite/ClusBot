import { createContext, useContext, useCallback, useEffect, useLayoutEffect, useRef, useState, type FormEvent } from 'react'
import { createRoot } from 'react-dom/client'
import {
  ArrowDownToLine, ArrowLeft, ArrowUp, Check, ChevronLeft, ChevronRight, CircleAlert, CirclePause, Clock3, FileOutput, FileText,
  FolderOpen, Inbox, Layers3, LayoutList, LoaderCircle, LogOut, Menu, MessageCircle, Plus,
  Search, Settings2, X, Pause, Play, Square, ListChecks, Bookmark, Plug, History, PanelRight,
  Archive, ArchiveRestore, Ellipsis, MessageCircleQuestionMark, Pencil, RotateCcw, ShieldCheck, Trash2, type LucideIcon,
} from 'lucide-react'
import { activeRun, api, APIError, awaitingPublication, type Artifact, type Conversation, type ConversationPage, type DeliveryUpdate, type Detail, type Overview, type OverviewGroup, type Receipt, type Material, type Settings, type Task } from './api'
import { FormattedText, MemoryReferences, Messages, UsageDetails } from './ExecutionText'
import { RunHistory } from './RunHistory'
import { TaskEditor } from './TaskEditor'
import { MaterialPicker } from './MaterialPicker'
import { PublicationPanel } from './PublicationPanel'
import { MemoryPage } from './MemoryPage'
import { ExperienceCandidatesPage } from './ExperienceCandidatesPage'
import { SkillProposalsPage } from './SkillProposalsPage'
import {MessageHistory,useMessageAnchor} from './MessageHistory'
import {clearMemoryDrafts} from './memoryDrafts'
import { ConnectionsPage } from './ConnectionsPage'
import { SettingsPage } from './SettingsPage'
import { ReadEvidence, TaskReadPicker, readRequest } from './TaskRead'
import { beginDraftRequest, changeComposerDraft, clearComposerDrafts, discardComposerDraft, draftGeneration, finishDraftRequest, releaseDraftRequest, useComposerDraft } from './composerDrafts'
import './styles.css'

if (window.clusbotDrafts) document.documentElement.classList.add('desktop-app')

const VisionContext = createContext(false)

const time = (value: number) => new Intl.DateTimeFormat('zh-CN', { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' }).format(value)
const versionTime=new Intl.DateTimeFormat('zh-CN',{month:'numeric',day:'numeric',hour:'2-digit',minute:'2-digit',second:'2-digit'})
const errorMessage = (error: unknown) => error instanceof Error ? error.message : '操作未完成'

function Brand() {
  return <div className="brand"><span className="brand-mark"><Layers3 size={19}/></span>ClusBot</div>
}

function App() {
  const [authenticated, setAuthenticated] = useState<boolean>()
  const [error, setError] = useState('')
  useEffect(() => {
    api<{authenticated:boolean}>('/session').then(result => setAuthenticated(result.authenticated)).catch(e => setError(errorMessage(e)))
    const logout = () => {clearMemoryDrafts();setAuthenticated(false)}
    window.addEventListener('clusbot:logout', logout)
    return () => window.removeEventListener('clusbot:logout', logout)
  }, [])
  if (error) return <div className="login-shell"><p role="alert">{error}</p><button onClick={() => location.reload()}>重试</button></div>
  if (authenticated === undefined) return <div className="login-shell"><LoaderCircle className="spin" aria-label="正在加载"/></div>
  return authenticated ? <Workspace logout={() => {clearComposerDrafts();clearMemoryDrafts();setAuthenticated(false)}}/> : <Login enter={() => setAuthenticated(true)}/>
}

function Login({enter}: {enter: () => void}) {
  const [token, setToken] = useState('')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState('')
  async function submit(event: FormEvent) {
    event.preventDefault()
    if (pending) return
    setPending(true)
    setError('')
    try { await api('/session', {token:token.trim()}); enter() }
    catch (e) {setError(errorMessage(e))}
    finally {setPending(false)}
  }
  return <div className="login-shell"><form className="login-card" onSubmit={submit}>
    <Brand/><h1>打开你的工作台</h1>
    <label htmlFor="access-token">本机访问口令</label>
    <input id="access-token" type="password" autoComplete="current-password" value={token} onChange={e => setToken(e.target.value)} required/>
    <p className="muted">口令保存在启动日志所示的 access-token 文件中。</p>
    {error && <p className="error" role="alert">{error}</p>}
    <button className="primary" disabled={pending}>{pending ? '正在登录…' : '进入工作台'}</button>
  </form></div>
}

function workRoute(next:string,scope:string,search:string) {
  const url=new URL(next,location.origin)
  if(url.pathname==='/' || /^\/(tasks|artifacts)(\/|$)/.test(url.pathname)) {
    if(scope && !url.searchParams.has('scope_label'))url.searchParams.set('scope_label',scope)
    if(search && !url.searchParams.has('q'))url.searchParams.set('q',search)
  }
  return url.pathname+url.search+url.hash
}

function artifactRoute(id:string|undefined,scope:string,taskID:string) {
  const filters=new URLSearchParams()
  if(taskID)filters.set('task_id',taskID)
  if(scope || taskID)filters.set('scope_label',scope)
  const query=filters.toString()
  return '/artifacts'+(id?'/'+id:'')+(query?'?'+query:'')
}

const taskHistoryPageSize=50

function taskHistoryRoute(path:string,scope:string,search:string,status:string) {
  const query=new URLSearchParams({view:'history'})
  if(status)query.set('status',status)
  return workRoute(path+'?'+query,scope,search)
}

const overviewPageSize=8
const overviewGroupKeys:OverviewGroup[]=['waiting','active','completed']
const overviewGroupTitles:Record<OverviewGroup,string>={waiting:'需要你决定',active:'正在处理',completed:'最近完成'}
type OverviewPages=Partial<Record<OverviewGroup,string[]>>

function Workspace({logout}: {logout: () => void}) {
  const [route,setRoute]=useState(location.pathname+location.search)
  const [path,query='']=route.split('?')
  const params=new URLSearchParams(query)
  const scope=params.get('scope_label')??''
  const search=params.get('q')??''
  const artifactTask=params.get('task_id')??''
  const selectedRun=params.get('run')??''
  const filter=new URLSearchParams({scope_label:scope,q:search}).toString()
  const [overview, setOverview] = useState<{filter:string;query:string;data:Overview}>()
  const [overviewPages,setOverviewPages]=useState<{filter:string;pages:OverviewPages}>()
  const pages=overviewPages?.filter===filter?overviewPages.pages:undefined
  const overviewParams=new URLSearchParams(filter)
  overviewParams.set('limit',String(overviewPageSize))
  for(const group of overviewGroupKeys) {
    const after=pages?.[group]?.at(-1)
    if(after)overviewParams.set(group+'_after',after)
  }
  const overviewQuery=overviewParams.toString()
  const overviewQueryRef=useRef(overviewQuery)
  overviewQueryRef.current=overviewQuery
  const requestSeq=useRef(0)
  const appliedSeq=useRef(0)
  const [settings, setSettings] = useState<Settings>()
  const [selected, setSelected] = useState('')
  const [detailOpen, setDetailOpen] = useState(false)
  const [menu, setMenu] = useState(false)
  const [error, setError] = useState('')
  const [newWork, setNewWork] = useState(false)
  const newWorkTrigger = useRef<HTMLButtonElement>(null)
  const mobileNewWork = useRef<HTMLButtonElement>(null)
  const justCreatedTask = useRef('')
  const compose = useRef<HTMLTextAreaElement>(null)
  const [area,id,source] = path.split('/').slice(1)
  const section = area || 'home'
  const historyView=section==='tasks'&&params.get('view')==='history'
  const isHistory=historyView&&!id
  const historyStatus=historyView?params.get('status')??'':''
  const historyFilter=new URLSearchParams({scope_label:scope,q:search,status:historyStatus,limit:String(taskHistoryPageSize+1)}).toString()
  const [taskHistory,setTaskHistory]=useState<{filter:string;items:Task[];next?:string}>()
  const [historyPending,setHistoryPending]=useState(false)
  const [historyError,setHistoryError]=useState('')
  const historyRequest=useRef(0)
  const openRoute=useCallback((next:string,replace=false)=>{
    if(location.pathname+location.search+location.hash!==next) {
      if(replace)history.replaceState(null,'',next)
      else history.pushState(null,'',next)
    }
    setRoute(location.pathname+location.search)
    window.dispatchEvent(new Event('clusbot:navigate'))
    setMenu(false)
  },[])
  const navigate=useCallback((next:string)=>openRoute(workRoute(next,scope,search)),[openRoute,scope,search])
  const setSearch=(value:string)=>openRoute(isHistory?taskHistoryRoute('/tasks',scope,value,historyStatus):workRoute(path,scope,value),true)
  const navigateFromOverview=(next:string)=>{
    if(isHistory&&/^\/tasks\/[^/?]+$/.test(next))openRoute(taskHistoryRoute(next,scope,search,historyStatus))
    else navigate(next)
  }
  useEffect(() => {
    const pop = () => setRoute(location.pathname+location.search)
    window.addEventListener('popstate',pop)
    return () => window.removeEventListener('popstate',pop)
  },[])
  useEffect(() => {api<Settings>('/settings').then(setSettings).catch(e => setError(errorMessage(e)))},[])
  useEffect(()=>setOverviewPages(previous=>previous?.filter===filter?previous:undefined),[filter])
  const refresh = useCallback(async (signal?: {cancelled:boolean}) => {
    const sequence=++requestSeq.current
    const current=()=>!signal?.cancelled && overviewQueryRef.current===overviewQuery && sequence>appliedSeq.current
    try {
      const result = await api<Overview>('/overview?' + overviewQuery)
      if (current()) { appliedSeq.current=sequence;setOverview({filter,query:overviewQuery,data:result});setError('') }
    } catch (e) { if (current()) {appliedSeq.current=sequence;setError(errorMessage(e))} }
  },[filter,overviewQuery])
  useEffect(() => {
    const signal = {cancelled:false}
    const initial = window.setTimeout(() => void refresh(signal),search ? 180 : 0)
    const interval = window.setInterval(() => {if (!document.hidden) void refresh(signal)},3000)
    return () => {signal.cancelled=true; clearTimeout(initial); clearInterval(interval)}
  },[refresh,section])
  const loadHistory=useCallback(async (after?:string)=>{
    const sequence=++historyRequest.current
    const query=new URLSearchParams(historyFilter)
    if(after)query.set('after',after)
    setHistoryPending(true);setHistoryError('')
    try {
      const page=await api<Task[]>('/tasks?'+query)
      if(sequence!==historyRequest.current)return
      const items=page.slice(0,taskHistoryPageSize)
      const last=items.at(-1)
      const next=page.length>taskHistoryPageSize&&last?`${last.updated_at}:${last.id}`:undefined
      setTaskHistory(previous=>{
        if(!after||previous?.filter!==historyFilter)return {filter:historyFilter,items,next}
        const seen=new Set(previous.items.map(item=>item.id))
        return {filter:historyFilter,items:[...previous.items,...items.filter(item=>!seen.has(item.id))],next}
      })
    }catch(e){if(sequence===historyRequest.current)setHistoryError(errorMessage(e))}
    finally{if(sequence===historyRequest.current)setHistoryPending(false)}
  },[historyFilter])
  useEffect(()=>{
    if(!isHistory)return
    void loadHistory()
    return()=>{historyRequest.current++}
  },[isHistory,loadHistory])
  const historyPage=taskHistory?.filter===historyFilter?taskHistory:undefined
  const currentOverview=overview?.filter===filter?overview:undefined
  const groups=currentOverview?.data.groups
  const visibleGroups=section==='tasks'?overviewGroupKeys.slice(0,2):overviewGroupKeys
  const hasPageHistory=visibleGroups.some(group=>Boolean(pages?.[group]?.length))
  const pagePending=currentOverview?.query!==overviewQuery
  function changeOverviewPage(group:OverviewGroup,next?:string) {
    setOverviewPages(previous=>{
      const current=previous?.filter===filter?previous.pages:{}
      const trail=current[group]??[]
      if(trail.at(-1)!==pages?.[group]?.at(-1))return previous
      return {filter,pages:{...current,[group]:next?[...trail,next]:trail.slice(0,-1)}}
    })
  }
  const tasks = groups ? [...groups.waiting,...groups.active,...groups.completed] : []
  const visibleTasks = isHistory ? historyPage?.items??[] : section === 'tasks' ? tasks.filter(task => task.status !== 'completed') : tasks
  const previewTask = visibleTasks.find(task => task.id === selected) || visibleTasks[0]
  const focused = detailOpen ? previewTask : undefined
  const isOverview = section === 'home' || (section === 'tasks' && !id)
  function openTask(task: Task) {
    if (matchMedia('(max-width: 1050px)').matches) navigateFromOverview('/tasks/'+task.id)
    else {
      setSelected(task.id)
      setDetailOpen(true)
    }
  }
  function onTask(receipt: Receipt) {
    justCreatedTask.current=receipt.task_id
    setNewWork(false)
    setSelected(receipt.task_id)
    navigate('/tasks/'+receipt.task_id)
    void refresh()
  }
  async function submitTask(text:string,key:string,scopeLabel:string,attachments:string[],delivery:DeliveryUpdate) {
    onTask(await api('/tasks',{text,client_request_id:key,scope_label:scopeLabel,attachment_ids:attachments,...delivery}))
  }
  function openNewWork(trigger:HTMLButtonElement) {
    newWorkTrigger.current=trigger
    setNewWork(true)
    setMenu(false)
  }
  function dismissNewWork() {
    setNewWork(false)
    const trigger=newWorkTrigger.current
    const target=trigger?.getClientRects().length?trigger:mobileNewWork.current
    target?.focus({preventScroll:true})
  }
  const focusNewTaskHeading=useCallback((heading:HTMLHeadingElement|null)=>{
    if(heading&&justCreatedTask.current===id){
      justCreatedTask.current=''
      heading.focus()
    }
  },[id])
  const nav = [
    { label:'总览', path:'/', icon:Inbox, on:section==='home' },
    { label:'进行中', path:'/tasks', icon:LayoutList, on:section==='tasks' },
    { label:'成果', path:'/artifacts', icon:FileText, on:section==='artifacts' },
    { label:'对话', path:'/chat', icon:MessageCircle, on:section==='chat' },
  ]
  return <VisionContext.Provider value={Boolean(settings?.attributes?.vision && settings.mode !== 'demo' && settings.mode !== 'unconfigured')}><div className={'workspace' + (isOverview && !focused ? ' overview-without-detail' : '')}>
    {menu && <button className="scrim" onClick={() => setMenu(false)} aria-label="关闭导航"/>}
    <aside className={'sidebar ' + (menu ? 'open' : '')}>
      <Brand/>
      <button className="new-work" aria-haspopup="dialog" onClick={event=>openNewWork(event.currentTarget)}><Plus size={17}/>交代一件事</button>
      <nav aria-label="主导航">{nav.map(({label,path:target,icon:Icon,on}) =>
        <button key={target} className={on ? 'nav-item current' : 'nav-item'} aria-current={on ? 'page' : undefined} onClick={() => navigate(target)}><Icon size={19}/>{label}</button>)}</nav>
      {Boolean(overview?.data.scopes.length) && <div className="scopes"><h2>工作范围</h2>
        {overview?.data.scopes.map((item,i) => <button key={item} className={scope===item?'scope current':'scope'} aria-pressed={scope===item}
          onClick={()=>openRoute(historyView?taskHistoryRoute('/tasks',scope===item?'':item,'',historyStatus):workRoute(section==='artifacts'?'/artifacts':section==='tasks'&&!id?'/tasks':'/',scope===item?'':item,''))}>
          <span className={'scope-dot tone-'+i%3}/>{item}</button>)}
      </div>}
      <div className="sidebar-footer">
        <button className={'nav-item '+(section==='memory'?'current':'')} aria-current={section==='memory'?'page':undefined} onClick={() => navigate('/memory')}><Bookmark size={18}/>个人记忆</button>
        <button className={'nav-item '+(section==='connections'?'current':'')} aria-current={section==='connections'?'page':undefined} onClick={() => navigate('/connections')}><Plug size={18}/>工具与连接</button>
        <button className={'nav-item '+(section==='settings'?'current':'')} onClick={() => navigate('/settings')}><Settings2 size={18}/>设置</button>
        <button className="nav-item" onClick={async () => {try {await api('/session',undefined,'DELETE');logout()} catch(e){setError(errorMessage(e))}}}><LogOut size={18}/>退出</button>
      </div>
    </aside>
    <div className="mobile-header"><button className="icon-button" aria-label="打开导航" onClick={() => setMenu(true)}><Menu size={21}/></button><Brand/><button ref={mobileNewWork} className="icon-button" aria-label="交代一件事" aria-haspopup="dialog" onClick={event=>openNewWork(event.currentTarget)}><Plus size={22}/></button></div>
    {isOverview && <>
      <main className="main overview">
        <header className="page-heading"><div className="overview-heading-title"><h1>{isHistory?'事项历史':section==='tasks' ? '进行中' : scope || '工作总览'}</h1>{!isHistory&&<p>{new Intl.DateTimeFormat('zh-CN',{month:'long',day:'numeric',weekday:'long'}).format(new Date())}</p>}</div>
          <div className="overview-actions">
            <label className="search"><Search size={17}/><input aria-label="查找工作" placeholder="查找工作" value={search} onChange={e => setSearch(e.target.value)}/>{search && <button className="icon-button" aria-label="清除搜索" onClick={() => setSearch('')}><X size={14}/></button>}</label>
            <button type="button" className="outline" onClick={()=>openRoute(isHistory?workRoute('/tasks',scope,search):taskHistoryRoute('/tasks',scope,search,''))}>{isHistory?<LayoutList size={16}/>:<History size={16}/>} {isHistory?'进行中':'事项历史'}</button>
            {previewTask && <button type="button" className="outline overview-detail-toggle" aria-label={focused?'关闭工作详情':'打开工作详情'}
              title={focused?'关闭工作详情':'打开工作详情'} aria-expanded={Boolean(focused)} onClick={()=>setDetailOpen(!focused)}><PanelRight size={17}/></button>}
          </div>
        </header>
        {isHistory&&<select className="task-status-filter" aria-label="事项状态" value={historyStatus} onChange={event=>openRoute(taskHistoryRoute('/tasks',scope,search,event.target.value))}>
          <option value="">全部状态</option><option value="active">正在处理</option><option value="waiting_user">等待中</option><option value="interrupted">已中断</option><option value="completed">已完成</option><option value="cancelled">已停止</option>
        </select>}
        {!isHistory && !!currentOverview?.data.pending_skill_proposals && <button type="button" className="outline"
          onClick={()=>navigate('/memory/skill-proposals')}>待审阅 Skill 提案 · {currentOverview.data.pending_skill_proposals}</button>}
        {settings?.mode==='demo' && <div className="mode-note">本地演示</div>}
        {(isHistory?historyError:error) && <p className="error" role="alert">{isHistory?historyError:error}</p>}
        {isHistory?<div className="task-list">
          {!historyPage&&!historyError&&<LoaderCircle className="spin" aria-label="正在加载"/>}
          {historyPage&&!visibleTasks.length&&<div className="empty"><span className="empty-icon"><History size={27}/></span><h2>没有匹配的事项</h2></div>}
          <TaskGroup tasks={visibleTasks} selected={focused?.id} choose={openTask}/>
          {historyPage?.next&&<button type="button" className="outline history-more" disabled={historyPending} onClick={()=>void loadHistory(historyPage.next)}>{historyPending?'正在加载…':'加载更多'}</button>}
        </div>:<div className="task-list">
          {!groups && !error && <LoaderCircle className="spin" aria-label="正在加载"/>}
          {groups && !visibleTasks.length && !hasPageHistory && <div className="empty"><span className="empty-icon"><Inbox size={28}/></span><h2>{search||scope ? '没有匹配的工作' : section==='tasks' ? '暂无进行中的工作' : '工作台已就绪'}</h2><p>{search ? '试试其他关键词。' : '交代一件事，进展和成果会显示在这里。'}</p></div>}
          {currentOverview&&visibleGroups.map(group=>{
            const next=currentOverview.data.next_cursors[group]
            const previous=pages?.[group]?.length?()=>changeOverviewPage(group):undefined
            return <TaskGroup key={group} title={overviewGroupTitles[group]} tasks={currentOverview.data.groups[group]} count={currentOverview.data.counts[group]}
              selected={focused?.id} choose={openTask} waitingAt={group==='waiting'?currentOverview.data.snapshot_at:undefined}
              pagination={{previous,next:next?()=>changeOverviewPage(group,next):undefined,pending:pagePending}}/>
          })}
        </div>}
        {!isHistory&&<Composer draftKey="new:overview" inputRef={compose} placeholder="接下来，需要我帮你做什么？" label="交给助手" initialScope={scope} assign onSend={submitTask}/>}
      </main>
      {focused && <aside className="detail-pane overview-detail" aria-label="工作详情">
        <TaskDetail key={focused.id} taskID={focused.id} compact onChange={()=>{void refresh();if(isHistory)void loadHistory()}} navigate={navigateFromOverview} contextWindow={settings?.effective_context_tokens}/>
      </aside>}
    </>}
    {section==='tasks' && id && <TaskDetail key={id} taskID={id} onChange={refresh} navigate={navigate} headingRef={focusNewTaskHeading} contextWindow={settings?.effective_context_tokens}
      historyBack={historyView?taskHistoryRoute('/tasks',scope,search,historyStatus):undefined}
      selectedRun={selectedRun} selectRun={runID=>{
        const next=new URLSearchParams(query)
        if(runID)next.set('run',runID)
        else next.delete('run')
        openRoute(next.size?path+'?'+next.toString():path)
      }}/>}
    {section==='chat' && <Chat key={id || 'new'} id={id} navigate={navigate}/>}
    {section==='artifacts' && <Artifacts key={JSON.stringify([id,scope,artifactTask])} selected={id} scope={scope} taskID={artifactTask} navigate={navigate}/>}
    {section==='memory' && (id==='skill-proposals' ? <SkillProposalsPage navigate={navigate} onChange={()=>void refresh()}/> : id==='candidates' ? <ExperienceCandidatesPage navigate={navigate}/> : <MemoryPage selected={id} sourceId={source} navigate={navigate}/>)}
    {section==='connections' && <ConnectionsPage selected={id} navigate={navigate}/>}
    {section==='settings' && <SettingsPage onSaved={setSettings}/>}
    {newWork&&<NewWorkDialog scope={scope} send={submitTask} onDismiss={dismissNewWork}/>}
  </div></VisionContext.Provider>
}

function NewWorkDialog({scope,send,onDismiss}:{scope:string;send:Parameters<typeof Composer>[0]['onSend'];onDismiss:()=>void}) {
  const dialog=useRef<HTMLDialogElement>(null)
  const input=useRef<HTMLTextAreaElement>(null)
  useLayoutEffect(()=>{
    const modal=dialog.current
    if(!modal)return
    modal.showModal()
    input.current?.focus()
    return()=>{if(modal.open)modal.close()}
  },[])
  function dismiss() {
    dialog.current?.close()
    onDismiss()
  }
  return <dialog className="new-dialog new-work-dialog" aria-labelledby="new-work-title" ref={dialog}
    onCancel={event=>{event.preventDefault();dismiss()}}
    onClick={event=>{
      if(event.target!==event.currentTarget)return
      const {left,right,top,bottom}=event.currentTarget.getBoundingClientRect()
      if(event.clientX<left||event.clientX>right||event.clientY<top||event.clientY>bottom)dismiss()
    }}>
    <header><h2 id="new-work-title">交代一件事</h2><button type="button" className="icon-button" aria-label="关闭" onClick={dismiss}><X size={20} aria-hidden="true"/></button></header>
    <Composer draftKey="new:dialog" inputRef={input} placeholder="描述你想完成的事" label="交给助手" assign initialScope={scope} onSend={send}/>
  </dialog>
}

const taskReasonIcons:Record<string,LucideIcon>={
  input_required:MessageCircleQuestionMark,approval_required:ShieldCheck,acceptance_required:ListChecks,
  delivery_required:FileOutput,file_writing:FileOutput,reconciliation_required:CircleAlert,paused:CirclePause,
}
const taskStatusIcons:Record<Task['status'],LucideIcon>={
  completed:Check,interrupted:CircleAlert,waiting_user:Inbox,cancelled:Square,active:Clock3,
}

function waitDuration(stateChangedAt:number,snapshotAt:number) {
  const minutes=Math.max(0,Math.floor((snapshotAt-stateChangedAt)/60000))
  if(minutes===0)return '刚刚'
  const duration=minutes<60?`${minutes} 分钟`:minutes<1440?`${Math.floor(minutes/60)} 小时`:`${Math.floor(minutes/1440)} 天`
  return '等了 '+duration
}

function TaskGroup({title,tasks,count,selected,choose,waitingAt,pagination}:{
  title?:string; tasks:Task[];count?:number;selected?:string;choose:(task:Task)=>void;waitingAt?:number
  pagination?:{previous?:()=>void;next?:()=>void;pending:boolean}
}) {
  const paginated=Boolean(pagination?.previous||pagination?.next)
  if (!tasks.length&&!paginated) return null
  return <section className="task-group" aria-busy={paginated&&pagination?.pending||undefined}>
    {title&&<div className="task-group-heading"><h2>{title} <span>{count??tasks.length}</span></h2>
      {paginated&&<nav className="task-group-pages" aria-label={title+'分页'}>
        <button type="button" className="icon-button" title="上一页" aria-label={title+'上一页'} disabled={!pagination?.previous}
          onClick={pagination?.previous}><ChevronLeft size={16} aria-hidden="true"/></button>
        <button type="button" className="icon-button" title="下一页" aria-label={title+'下一页'} disabled={!pagination?.next||pagination.pending}
          onClick={pagination?.next}><ChevronRight size={16} aria-hidden="true"/></button>
      </nav>}
    </div>}
    {!tasks.length&&<p className="muted task-page-empty">这一页暂无事项</p>}
    {tasks.map(task => {
    const Icon = taskReasonIcons[task.status_reason] ?? taskStatusIcons[task.status]
    const backgroundWait=task.status_reason==='file_writing'
    const tone=task.status_reason==='paused'?'paused':backgroundWait?'active':task.status
    return <button key={task.id} className={'task-row '+(task.id===selected?'selected':'')} onClick={() => choose(task)}>
      <span className={'task-icon '+tone}><Icon size={17} aria-hidden="true"/></span>
      <span className="task-copy"><strong>{task.title}</strong><small>{task.reason}</small></span>
      {task.scope_label && <span className="task-scope">{task.scope_label}</span>}
      {waitingAt!==undefined&&task.state_changed_at>0&&<time className="task-wait" dateTime={new Date(task.state_changed_at).toISOString()}
        title={'自 '+versionTime.format(task.state_changed_at)}>{waitDuration(task.state_changed_at,waitingAt)}</time>}
      <ChevronRight size={16} className="row-arrow" aria-hidden="true"/>
    </button>
  })}</section>
}

function Composer({draftKey,placeholder,label,onSend,disabled=false,disabledReason,assign=false,initialScope='',task,inputRef}:{
  draftKey:string
  placeholder:string; label:string; onSend:(text:string,key:string,scope:string,attachments:string[],delivery:DeliveryUpdate)=>Promise<void>
  disabled?:boolean; disabledReason?:string; assign?:boolean; initialScope?:string; task?:Task; inputRef?:React.Ref<HTMLTextAreaElement>
}) {
  const allowImages = useContext(VisionContext)
  const draft=useComposerDraft(draftKey)
  const {text:value,materials,criteria:criteriaDraft}=draft
  const readDrafts=draft.reads??(draft.read?[draft.read]:[])
  const scope=draft.scope ?? draft.request?.scope ?? initialScope
  const edit=(patch:Partial<typeof draft>)=>{setUnconfirmed(false);changeComposerDraft(draftKey,()=>patch)}
  const [pending,setPending]=useState(false)
  const [error,setError]=useState('')
  const [unconfirmed,setUnconfirmed]=useState(false)
  const [focusCriteria,setFocusCriteria]=useState(false)
  const locked=useRef(false)
  const captureLock=useRef(false)
  const [capturing,setCapturing]=useState(false)
  async function capture(load:()=>Promise<Material>) {
    if (captureLock.current || locked.current || disabled) return
    captureLock.current=true
    setCapturing(true);setError('')
    const generation=draftGeneration()
    try {
      const material=await load()
      if(generation===draftGeneration())changeComposerDraft(draftKey,previous=>{
        const matches=(item:Material)=>material.upload ? item.upload?.id===material.upload.id : item.path===material.path
        const materials=previous.materials.some(matches)?previous.materials.map(item=>matches(item)?material:item):[...previous.materials,material]
        return {materials}
      })
    } catch(e){setError(errorMessage(e))}
    finally{captureLock.current=false;setCapturing(false)}
  }
  async function submit(event?: FormEvent,reclassify=false) {
    event?.preventDefault()
    if (locked.current || captureLock.current || disabled || !value.trim()) return
    if(reclassify&&!unconfirmed)return
    locked.current=true
    setPending(true);setError('');setUnconfirmed(false)
    if(reclassify)releaseDraftRequest(draftKey,draft.request?.key)
    const delivery:DeliveryUpdate={}
    if (criteriaDraft) {
      const criteria=criteriaDraft.text.trim()
      const filename=criteriaDraft.filename.trim()
      if(criteria!==criteriaDraft.baseText) delivery.acceptance_criteria=criteria
      if(filename!==criteriaDraft.baseFilename) delivery.required_filename=filename
      if(Object.keys(delivery).length>0) {
        const sameConditions=(task?.acceptance_criteria??'')===criteriaDraft.baseText && (task?.required_filename??'')===criteriaDraft.baseFilename
        delivery.revision=sameConditions?task?.revision:criteriaDraft.revision
      }
    }
    if(task&&draft.replaceMaterials)delivery.replace_materials=true
    let requestID:string|undefined
    try {
      if((!draft.request || reclassify) && (assign || task)) {
        const reads=readDrafts.map(readRequest)
        if(reads.length===1) delivery.mcp_read=reads[0]
        else if(reads.length>1) delivery.mcp_reads=reads
      }
      const request=beginDraftRequest(draftKey,scope,delivery)
      requestID=request.key
      await onSend(value,request.key,request.scope,materials.map(item=>item.id),request.delivery)
      finishDraftRequest(draftKey,request.key)
    }
    catch(e){
      if(e instanceof APIError&&e.code==='classification_failed'&&requestID)releaseDraftRequest(draftKey,requestID)
      setUnconfirmed(e instanceof APIError&&e.code==='classification_unconfirmed')
      setError(e instanceof TypeError?'未收到提交回执。草稿已保留，可以重试。':errorMessage(e))
    }
    finally{locked.current=false;setPending(false)}
  }
  return <form className="composer" onSubmit={submit}>
    <textarea aria-label={placeholder} placeholder={placeholder} value={value} ref={inputRef} rows={2} maxLength={16000}
      onChange={e=>edit({text:e.target.value})} readOnly={pending}
      onKeyDown={e => {if ((e.ctrlKey||e.metaKey) && e.key==='Enter' && !e.nativeEvent.isComposing) e.currentTarget.form?.requestSubmit()}}/>
    <div className="composer-tools">
      <MaterialPicker allowImages={allowImages} selected={materials} disabled={pending||capturing||disabled}
        replacement={task?{enabled:Boolean(draft.replaceMaterials),change:enabled=>edit({replaceMaterials:enabled})}:undefined}
        choose={path=>capture(()=>api<Material>('/attachments/workspace',{path}))}
        upload={file=>capture(()=>{
          if(file.size>10*1024*1024)throw new Error('文件超过 10 MiB')
          const form=new FormData()
          form.set('file',file)
          return api<Material>('/attachments',form)
        })}
        choosePart={(id,part)=>capture(()=>api<Material>('/attachments/uploads/'+id+'/part',{part}))}
        remove={id=>edit({materials:materials.filter(item=>item.id!==id)})}/>
      {(assign || task) && <button type="button" className="text-button criteria-toggle" aria-expanded={Boolean(criteriaDraft)} disabled={pending||disabled}
        onClick={()=>{
          if(criteriaDraft){
            setFocusCriteria(false)
            edit({criteria:undefined})
          }
          else {
            const text=task?.acceptance_criteria??''
            const filename=task?.required_filename??''
            setFocusCriteria(true)
            edit({criteria:{text,filename,baseText:text,baseFilename:filename,revision:task?.revision}})
          }
        }}><ListChecks size={16}/>{criteriaDraft?'取消编辑':task?.acceptance_criteria||task?.required_filename?'修改验收条件':'添加验收条件'}</button>}
      {(assign || task) && <TaskReadPicker drafts={readDrafts} disabled={pending||disabled} change={reads=>edit({read:undefined,reads})}/>}
    </div>
    {capturing && <p className="material-progress muted" role="status"><LoaderCircle size={15} className="spin"/>正在准备材料…</p>}
    {criteriaDraft && <div className="criteria-editor">
      <label>验收条件<textarea aria-label="验收条件" autoFocus={focusCriteria} maxLength={1000} rows={2} placeholder="例如：列出每项决策的负责人和截止日期" value={criteriaDraft.text} readOnly={pending}
        onChange={event=>edit({criteria:{...criteriaDraft,text:event.target.value}})}/></label>
      <label>交付文件（可选）<span className="filename-input"><span>outputs/</span><input aria-label="交付文件名" maxLength={100} placeholder="会议纪要.md" value={criteriaDraft.filename} readOnly={pending}
        onChange={event=>edit({criteria:{...criteriaDraft,filename:event.target.value}})}/></span></label>
    </div>}
    {error && <div><p className="error" role="alert">{error}</p>{unconfirmed&&<button type="button" className="text-button composer-retry" disabled={disabled||pending||capturing} onClick={()=>void submit(undefined,true)}>重新判断并发送</button>}</div>}
    <div className="composer-bar">
      {assign ? <label className="scope-input"><FolderOpen size={16}/><input maxLength={40} aria-label="工作范围" placeholder="工作范围（可选）" value={scope} readOnly={pending} onChange={e=>edit({scope:e.target.value})}/></label> : <span className="muted">{disabled?(disabledReason??'暂时无法发送'):''}</span>}
      <button className={assign?'primary':'send'} aria-label={label} disabled={disabled||pending||capturing||!value.trim()}>{pending ? <LoaderCircle size={16} className="spin"/> : assign ? <>{label}<ArrowUp size={16}/></> : <ArrowUp size={18}/>}</button>
    </div>
  </form>
}

function useDetail(path: string | undefined, onChange?: () => void) {
  const [data,setData]=useState<Detail>()
  const [live,setLive]=useState('')
  const [error,setError]=useState('')
  const [reconnecting,setReconnecting]=useState(false)
  const requested=useRef(0)
  const applied=useRef(0)
  const reload=useCallback(async (signal?:{cancelled:boolean})=>{
    if (!path) return
    const sequence=++requested.current
    const current=()=>!signal?.cancelled && sequence>applied.current
    try {
      const result=await api<Detail>(path)
      if(current()){applied.current=sequence;setData(result);setError('')}
    } catch(e){if(current()){applied.current=sequence;setError(errorMessage(e))}}
  },[path])
  useEffect(()=>{const signal={cancelled:false};void reload(signal);return()=>{signal.cancelled=true}},[reload])
  const runID=data?.run?.id
  const generation=data?.run?.execution_generation
  const active=activeRun(data?.run)
  const waitingForOperation=awaitingPublication(data?.publication)
  const notify=useRef(onChange)
  notify.current=onChange
  useEffect(()=>{
    if (!active || !runID) return
    setLive('');setReconnecting(false)
    const signal={cancelled:false}
    const stream=new EventSource('/api/v1/runs/'+runID+'/events')
    stream.addEventListener('message.delta', event => {
      const chunk=JSON.parse((event as MessageEvent).data) as {delta:string;execution_generation?:number}
      if((chunk.execution_generation??0)===(generation??0))setLive(previous=>previous+chunk.delta)
    })
    stream.addEventListener('message.reset',event=>{
      const reset=JSON.parse((event as MessageEvent).data) as {execution_generation?:number}
      if((reset.execution_generation??0)===(generation??0))setLive('')
    })
    stream.addEventListener('run.state_changed',()=>void reload(signal))
    stream.addEventListener('tool.read_changed',()=>void reload(signal))
    stream.addEventListener('done',()=>{stream.close();void reload(signal);notify.current?.()})
    stream.onopen=()=>setReconnecting(false)
    stream.onerror=()=>{setReconnecting(true);void reload(signal)}
    const poll=window.setInterval(()=>void reload(signal),2500)
    return ()=>{signal.cancelled=true;stream.close();clearInterval(poll)}
  },[runID,generation,active,reload])
  useEffect(()=>{
    if (!waitingForOperation) return
    const signal={cancelled:false}
    const poll=window.setInterval(()=>void reload(signal),2000)
    return ()=>{signal.cancelled=true;clearInterval(poll)}
  },[waitingForOperation,reload])
  return {data,live,error,reload,reconnecting:active&&reconnecting}
}

function TaskDetail({taskID,compact,onChange,navigate,historyBack,selectedRun,selectRun,headingRef,contextWindow}:{taskID:string;compact?:boolean;onChange:()=>void;navigate:(path:string)=>void;historyBack?:string;selectedRun?:string;selectRun?:(runID:string)=>void;headingRef?:React.Ref<HTMLHeadingElement>;contextWindow?:number}) {
  const {data,live,error,reload,reconnecting}=useDetail('/tasks/'+taskID,onChange)
  const sourceMessage=useMessageAnchor()
  const [actionError,setActionError]=useState('')
  const [acceptanceError,setAcceptanceError]=useState('')
  const [pending,setPending]=useState(false)
  const [previewID,setPreviewID]=useState('')
  const [editing,setEditing]=useState(false)
  const followUp=useRef<HTMLTextAreaElement>(null)
  const resumeKey=useRef<{retry:boolean;key:string}|undefined>(undefined)
  const task=data?.task
  const artifact=data?.artifact
  const preview=Boolean(artifact && previewID===artifact.id)
  const busy=activeRun(data?.run)
  const writing=awaitingPublication(data?.publication)
  async function accept() {
    if (!task || !artifact || pending) return
    setPending(true);setAcceptanceError('')
    try {await api('/tasks/'+taskID+'/acceptance',{revision:task.revision,artifact_id:artifact.id});await reload();onChange()}
    catch(e){setAcceptanceError(errorMessage(e));void reload()}
    finally{setPending(false)}
  }
  async function control(action: 'pause' | 'cancel' | 'resume', retry=false) {
    if (!task || pending) return
    setPending(true);setActionError('')
    try {
      const payload:{revision:number;client_request_id?:string;retry?:boolean}={revision:task.revision}
      if (action === 'resume') {
        if(resumeKey.current?.retry!==retry)resumeKey.current={retry,key:crypto.randomUUID()}
        payload.client_request_id=resumeKey.current.key
        if(retry)payload.retry=true
      }
      await api('/tasks/'+taskID+'/'+action,payload)
      resumeKey.current=undefined
      await reload();onChange()
    } catch(e) {setActionError(errorMessage(e));await reload()}
    finally{setPending(false)}
  }
  async function changeDiscussion() {
    if(!data || pending)return
    setPending(true);setActionError('')
    try {
      const conversation=data.conversation
      await api('/conversations/'+conversation.id,{revision:conversation.revision,archived:!conversation.archived},'PATCH')
      await reload()
    }catch(e){setActionError(errorMessage(e));await reload()}
    finally{setPending(false)}
  }
  async function confirmRead() {
    if(!task || !data?.unknown_read_run_id || pending)return
    setPending(true);setActionError('')
    try {
      await api('/tasks/'+taskID+'/ack-read',{revision:task.revision,run_id:data.unknown_read_run_id})
      await reload();onChange()
    }catch(e){setActionError(errorMessage(e));await reload()}
    finally{setPending(false)}
  }
  if (!task || !data) return <div className="pane-empty">{error?<p role="alert">{error}</p>:<LoaderCircle className="spin" aria-label="正在加载工作"/>}</div>
  const requestedInput=data.run?.status==='input_required'
  const question=task.status==='waiting_user'&&requestedInput?data.run?.response:undefined
  const ProgressIcon=taskReasonIcons[task.status_reason]??taskStatusIcons[task.status]
  const needsFile=Boolean(task.required_filename) && (data.publication?.status!=='succeeded' || data.publication.filename!==task.required_filename)
  const status=writing ? ({pending:'待确认写入',dispatched:'正在保存成果',unknown:'写入待核对'}[data.publication?.status??'']??task.reason) : {
    active:task.status_reason==='waiting_dependencies'?'等待依赖成果':task.status_reason==='queued'?'等待执行':'正在处理',waiting_user:question?'等你补充':needsFile?'待保存成果':'等你验收',completed:'已完成',
    interrupted:task.status_reason==='paused'?'已暂停':task.requirements_changed?'待按新要求执行':'执行中断',cancelled:'已停止',
  }[task.status]
  const controlling = task.status_reason === 'pause_requested' || task.status_reason === 'stop_requested'
  const archived=data.conversation.archived
  const readUnconfirmed=Boolean(data.unknown_read_run_id)
  const composerBlocked=busy||writing||archived||readUnconfirmed||task.status==='cancelled'
  const discussion=question?data.messages.filter(message=>message.role!=='assistant'||message.run_id!==data.run?.id):data.messages
  const reads=data.reads??(data.read?[data.read]:[])
  const readEvidence=reads.map(read=><ReadEvidence key={read.position} read={read} showSource={reads.length>1} openRun={selectRun}
    distinguish={reads.some(other=>other.position!==read.position&&other.connection_name===read.connection_name&&other.tool_name===read.tool_name)}/>)
  const editBlocked=Boolean(busy||writing||readUnconfirmed||pending)
  const canResume=!writing && !readUnconfirmed && (task.requirements_changed||(data.read_checkpoint
    ? data.read_checkpoint==='ready'
    : reads.every(read=>read.status==='completed'||read.status==='reused')))
  const canRetry=!writing && !readUnconfirmed && !task.requirements_changed && data.read_checkpoint==='retry'
  const readConfirmation=readUnconfirmed && <div className="read-confirmation">
    <p>这次读取的结果尚未取得。</p>
    <button type="button" className="outline" disabled={pending||busy} onClick={()=>void confirmRead()}>确认未取得结果</button>
  </div>
  const actions = <div className="task-actions">
    {!compact&&!editBlocked&&<button type="button" className="outline" onClick={()=>setEditing(true)}><Pencil size={15}/>编辑事项</button>}
    {task.status==='active' && <button className="outline" disabled={pending||controlling} onClick={()=>void control('pause')}><Pause size={15}/>暂停</button>}
    {['active','waiting_user','interrupted'].includes(task.status) && <button className="outline" disabled={pending||controlling||writing||readUnconfirmed} onClick={()=>void control('cancel')}><Square size={14}/>停止</button>}
    {(task.status==='cancelled'||(task.status==='interrupted'&&(canResume||canRetry))) && <button className="primary" disabled={pending||readUnconfirmed} onClick={()=>void control('resume',canRetry)}>{canRetry?<RotateCcw size={16}/>:<Play size={16}/>} {task.status==='cancelled'?'重新开启':task.requirements_changed?'按新要求执行':canRetry?'重新执行':'继续工作'}</button>}
  </div>
  const artifactActions=artifact && !busy && <div className="artifact-card"><span className="artifact-icon"><FileText size={20}/></span><div><strong>{artifact.name}</strong><small>{task.status==='interrupted'||requestedInput?'先前成果 · ':''}{time(artifact.created_at)}</small></div><button className="text-button" onClick={()=>setPreviewID(preview?'':artifact.id)}>{preview?'收起':'预览'}</button><a className="icon-button" aria-label="下载成果" href={'/api/v1/artifacts/'+artifact.id+'/content'}><ArrowDownToLine size={17}/></a></div>
  const previous=(data.artifact_versions??[]).filter(version=>busy||task.requirements_changed||version.id!==artifact?.id)
  const history=previous.length>0 && <details className="artifact-history">
    <summary><History size={17}/>历史成果</summary>
    {previous.map(version=><button type="button" key={version.id} className="history-row" onClick={()=>navigate(artifactRoute(version.id,task.scope_label,task.id))}>
      <FileText size={16}/><span>{versionTime.format(version.created_at)}{version.accepted_at?' · 已验收':''}</span><ChevronRight size={16}/>
    </button>)}
    <button type="button" className="text-button" onClick={()=>navigate(artifactRoute(undefined,task.scope_label,task.id))}>查看该事项全部成果</button>
  </details>
  const output=<>{reconnecting && <p className="muted">正在重新连接…</p>}{busy && (live || data.run?.response) && <div className="response-text" aria-live="polite">{live || data.run?.response}</div>}{artifactActions}{preview && artifact && !busy && <FormattedText text={artifact.content || ''} framed/>}</>
  const action=artifact && !busy && !requestedInput && <>{(task.status==='waiting_user' || data.publication?.status==='succeeded') &&
    <PublicationPanel key={artifact.id} task={task} artifact={artifact} publication={data.publication} reload={async()=>{await reload();onChange()}}/>}
    {task.status==='waiting_user' && !writing && !needsFile && !readUnconfirmed &&
      <div className="decision"><h3>验收成果</h3>{acceptanceError && <p className="error" role="alert">{acceptanceError}</p>}<button className="primary" disabled={pending} onClick={accept}><Check size={17}/>确认完成</button></div>}
    {data.acceptance && <p className="acceptance-note"><Check size={16}/>已验收 · {time(data.acceptance.accepted_at)}</p>}
  </>
  const inputRequest=question&&<div className="input-request">
    <MessageCircleQuestionMark size={19} aria-hidden="true"/>
    <div><p>{question}</p>{!compact&&!archived&&<MemoryReferences items={data.memory_refs}/>}
      {!compact&&!composerBlocked&&<button type="button" className="text-button" onClick={()=>followUp.current?.focus()}>补充信息<ChevronRight size={15} aria-hidden="true"/></button>}</div>
  </div>
  if (compact) return <div className="detail-content"><div className="status-line"><span className={'status-dot '+task.status}/>{status}</div>
    <h2>{task.title}</h2>{task.scope_label && <p className="muted">{task.scope_label}</p>}
    {task.goal!==task.title && <p className="goal">{task.goal}</p>}
    {(task.acceptance_criteria||task.required_filename) && <><hr/><h3>验收条件</h3>{task.acceptance_criteria && <p className="criteria-text">{task.acceptance_criteria}</p>}{task.required_filename && <p className="criteria-text"><code>outputs/{task.required_filename}</code></p>}</>}
    <hr/><h3>当前进展</h3>{inputRequest||<p className="progress-line">{busy?<LoaderCircle size={17} className="spin" aria-hidden="true"/>:<ProgressIcon size={17} aria-hidden="true"/>} {task.reason}</p>}
    {readEvidence}
    {readConfirmation}
    {output}{action}{task.status==='interrupted'&&actions}{(actionError||error) && <p className="error" role="alert">{actionError||error}</p>}
    <button className="detail-link" onClick={()=>navigate('/tasks/'+taskID)}>{question?'补充信息':'查看完整工作与交流'}<ChevronRight size={17}/></button><p className="updated">更新于 {time(task.updated_at)}</p></div>
  return <>
    <main className="main work-detail"><button className="text-button back" onClick={()=>navigate(historyBack??'/')}><ArrowLeft size={16}/>{historyBack?'事项历史':'工作总览'}</button>
      <header className="page-heading"><div><h1 tabIndex={-1} ref={headingRef}>{task.title}</h1><p>{task.scope_label && task.scope_label+' · '}{status}</p></div>{actions}</header>
      {data.workflow_parent_id && <button type="button" className="text-button" onClick={()=>navigate('/tasks/'+data.workflow_parent_id)}><ArrowLeft size={16}/>查看父任务</button>}
      {editing&&<TaskEditor task={task} blocked={editBlocked} reload={reload} onSaved={async()=>{await reload();onChange()}} onClose={()=>setEditing(false)}/>}
      {task.goal!==task.title && <section className="work-goal"><h2>目标</h2><p>{task.goal}</p></section>}
      {(task.acceptance_criteria||task.required_filename) && <section className="work-goal"><h2>验收条件</h2>{task.acceptance_criteria && <p>{task.acceptance_criteria}</p>}{task.required_filename && <p><code>outputs/{task.required_filename}</code></p>}</section>}
      {data.workflow && <section className="work-goal"><h2>子任务</h2>
        {data.workflow.children.map(child=><button type="button" className="history-row workflow-row" key={child.task_id} onClick={()=>navigate('/tasks/'+child.task_id)}>
          <span>{child.title}<small className="muted">{child.reason}</small></span><ChevronRight size={16} aria-hidden="true"/>
        </button>)}
      </section>}
      <section className="work-goal"><h2>进展</h2>{inputRequest||<p className="progress-line">{busy?<LoaderCircle size={17} className="spin" aria-hidden="true"/>:<ProgressIcon size={17} aria-hidden="true"/>} {task.reason}</p>}
        {readEvidence}
        {readConfirmation}
        <UsageDetails current={data.run?.usage} cumulative={data.task_usage} running={busy} contextWindow={contextWindow}/>
        {selectRun && <RunHistory taskID={taskID} latest={data.run} selected={selectedRun??''} select={selectRun}
          revision={task.revision} archived={archived} openArtifact={artifactID=>navigate(artifactRoute(artifactID,task.scope_label,task.id))}/>}
      </section>
      <section className="discussion"><header className="discussion-heading"><h2>相关交流</h2>
        <button className="text-button" disabled={pending||(!archived&&(busy||writing||readUnconfirmed))} onClick={()=>void changeDiscussion()}>
          {archived?<ArchiveRestore size={16}/>:<Archive size={16}/>} {archived?'恢复交流':'归档交流'}
        </button></header>
        {archived&&!sourceMessage?<p className="muted">交流已归档，可恢复原有消息。</p>:<MessageHistory conversationID={task.conversation_id} messages={discussion}
          omitRunID={question?data.run?.id:undefined}
          live={busy?(live||data.run?.response):undefined} liveRunID={busy?data.run?.id:undefined} remember={id=>navigate('/memory/new/'+id)}/>}
      </section>
      <div className="mobile-outcome">{artifactActions}{preview&&artifact&&!busy&&<FormattedText text={artifact.content || ''} framed/>}{action}{history}</div>
      {(actionError||error) && <p className="error" role="alert">{actionError||error}</p>}
      <Composer draftKey={'task:'+task.id} placeholder={question?'补充助手需要的信息…':'补充这项工作的要求…'} label={question?'发送补充信息':'发送补充要求'} task={task} inputRef={followUp} disabled={composerBlocked}
        disabledReason={archived?'恢复交流后可发送':readUnconfirmed?'请先确认读取结果':writing?'写入待处理，草稿可继续编辑':busy?'本轮处理中，草稿可继续编辑':'事项已停止'}
        onSend={async (text,key,_scope,attachments,delivery)=>{await api('/conversations/'+task.conversation_id+'/messages',{text,client_message_id:key,attachment_ids:attachments,...delivery});setAcceptanceError('');await reload();onChange()}}/>
    </main>
    <aside className="detail-pane full-outcome"><div className="detail-content"><div className="status-line"><span className={'status-dot '+task.status}/>{status}</div><h2>工作成果</h2>{output}{!artifact&&!busy&&<p className="muted">{requestedInput?'本轮暂无成果。':task.status==='interrupted'||task.status==='cancelled'?'本轮未生成可验收的成果。':'成果将在执行结束后显示。'}</p>}{action}{history}</div></aside>
  </>
}

function Chat({id,navigate}:{id?:string;navigate:(path:string)=>void}) {
  const {data,live,error,reload}=useDetail(id?'/conversations/'+id:undefined)
  const [items,setItems]=useState<Conversation[]>([])
  const [archivedItems,setArchivedItems]=useState<Conversation[]>([])
  const [next,setNext]=useState<string>()
  const [archiveNext,setArchiveNext]=useState<string>()
  const [listPending,setListPending]=useState(false)
  const [archiveView,setShowArchived]=useState<boolean>()
  const [listError,setListError]=useState('')
  const [promote,setPromote]=useState(false)
  const [name,setName]=useState<string>()
  const [saving,setSaving]=useState(false)
  const [actionError,setActionError]=useState('')
  const [stopping,setStopping]=useState<string>()
  const resumeKey=useRef<{runID:string;key:string}|undefined>(undefined)
  const refreshList=useCallback(async()=>{
    try {
      const [recent,archive]=await Promise.all([api<ConversationPage>('/conversations/history'),api<ConversationPage>('/conversations/history?archived=1')])
      setItems(recent.items);setNext(recent.next_cursor)
      setArchivedItems(archive.items);setArchiveNext(archive.next_cursor);setListError('')
    }catch(e){setListError(errorMessage(e))}
  },[])
  useEffect(()=>{void refreshList()},[refreshList])
  const busy=activeRun(data?.run)
  const conversation=data?.conversation
  const run=data?.run
  const canResume=Boolean(conversation && !conversation.archived && run && !run.task_id && data?.read_checkpoint==='ready')
  const chatError=actionError||error||listError||(run?.error_code==='stop'?'':run?.error)
  const showArchived=archiveView??conversation?.archived??false
  useEffect(()=>{if(!busy||run?.id!==stopping)setStopping(undefined)},[busy,run?.id,stopping])
  async function loadConversations(archived:boolean) {
    const cursor=archived?archiveNext:next
    if(!cursor||listPending)return
    setListPending(true);setListError('')
    try {
      const page=await api<ConversationPage>('/conversations/history?archived='+(archived?'1':'0')+'&after='+encodeURIComponent(cursor))
      const merge=(previous:Conversation[])=>[...previous,...page.items.filter(item=>!previous.some(old=>old.id===item.id))]
      if(archived){setArchivedItems(merge);setArchiveNext(page.next_cursor)}
      else {setItems(merge);setNext(page.next_cursor)}
    }catch(e){setListError(errorMessage(e))}
    finally{setListPending(false)}
  }
  async function stopConversation() {
    if(!conversation||!run||run.task_id||!busy||stopping)return
    setStopping(run.id);setActionError('')
    try {
      await api('/conversations/'+conversation.id+'/cancel',{run_id:run.id})
      await reload()
    }catch(e){setStopping(undefined);setActionError(errorMessage(e));await reload()}
  }
  async function resumeConversation() {
    if(!conversation || !run || !canResume || saving)return
    setSaving(true);setActionError('')
    if(resumeKey.current?.runID!==run.id)resumeKey.current={runID:run.id,key:crypto.randomUUID()}
    try {
      await api<Receipt>('/conversations/'+conversation.id+'/resume',{run_id:run.id,revision:conversation.revision,client_request_id:resumeKey.current.key})
      resumeKey.current=undefined
      await reload()
    }catch(e){setActionError(errorMessage(e));await reload()}
    finally{setSaving(false)}
  }
  async function changeConversation(patch:{title?:string;archived?:boolean}) {
    if(!conversation || saving)return
    setSaving(true);setActionError('')
    try {
      await api('/conversations/'+conversation.id,{revision:conversation.revision,...patch},'PATCH')
      if(patch.archived!==undefined){setPromote(false);setShowArchived(patch.archived)}
      setName(undefined)
      await Promise.all([reload(),refreshList()])
    }catch(e){setActionError(errorMessage(e));await reload()}
    finally{setSaving(false)}
  }
  async function removeConversation() {
    if(!conversation || busy || saving || !window.confirm('删除这段对话及其消息？已保存的个人记忆会保留。'))return
    setSaving(true);setActionError('')
    try {
      await api('/conversations/'+conversation.id,{revision:conversation.revision},'DELETE')
      discardComposerDraft('chat:'+conversation.id)
      discardComposerDraft('promotion:'+conversation.id)
      navigate('/chat')
    }catch(e){setActionError(errorMessage(e));await reload()}
    finally{setSaving(false)}
  }
  return <>
    <main className="main work-detail"><header className="page-heading"><h1>{conversation?.title||'对话'}</h1>
      <div className="chat-actions">
        {conversation && <details className="chat-menu"><summary className="icon-button" aria-label="对话操作"><Ellipsis size={19}/></summary><div className="chat-menu-panel">
          <button type="button" disabled={saving} onClick={event=>{event.currentTarget.closest('details')?.removeAttribute('open');setName(conversation.title)}}><Pencil size={16}/>重命名</button>
          <button type="button" disabled={saving||(!conversation.archived&&busy)} onClick={event=>{event.currentTarget.closest('details')?.removeAttribute('open');void changeConversation({archived:!conversation.archived})}}>
            {conversation.archived?<ArchiveRestore size={16}/>:<Archive size={16}/>} {conversation.archived?'恢复对话':'归档对话'}
          </button>
          <button type="button" className="danger" disabled={saving||busy} onClick={event=>{event.currentTarget.closest('details')?.removeAttribute('open');void removeConversation()}}><Trash2 size={16}/>删除对话</button>
        </div></details>}
        <button className="outline" onClick={()=>navigate('/chat')}><Plus size={16}/>新对话</button>
      </div></header>
      {name!==undefined && <form className="chat-rename" onSubmit={event=>{event.preventDefault();void changeConversation({title:name})}}>
        <input aria-label="对话名称" autoFocus maxLength={60} value={name} readOnly={saving} onChange={event=>setName(event.target.value)}/>
        <button type="button" className="text-button" disabled={saving} onClick={()=>setName(undefined)}>取消</button>
        <button className="primary" disabled={saving||!name.trim()}>保存名称</button>
      </form>}
      <select className="mobile-chat-select" aria-label="选择历史对话" value={id??''} onChange={e=>navigate(e.target.value?'/chat/'+e.target.value:'/chat')}>
        <option value="">新对话</option>
        {conversation&&!items.some(item=>item.id===conversation.id)&&!archivedItems.some(item=>item.id===conversation.id)&&<option value={conversation.id}>{conversation.title}</option>}
        {items.length>0 && <optgroup label="最近对话">{items.map(item=><option value={item.id} key={item.id}>{item.title}</option>)}</optgroup>}
        {archivedItems.length>0 && <optgroup label="已归档">{archivedItems.map(item=><option value={item.id} key={item.id}>{item.title}</option>)}</optgroup>}
      </select>
      {(next||archiveNext)&&<div className="mobile-chat-pages">
        {next&&<button type="button" className="text-button" disabled={listPending} onClick={()=>void loadConversations(false)}>加载更多对话</button>}
        {archiveNext&&<button type="button" className="text-button" disabled={listPending} onClick={()=>void loadConversations(true)}>加载更多归档</button>}
      </div>}
      <div className="discussion chat-log">{data?<MessageHistory conversationID={data.conversation.id} messages={data.messages} live={busy?(live||data.run?.response):undefined} liveRunID={busy?data.run?.id:undefined} remember={id=>navigate('/memory/new/'+id)}/>:!id&&<div className="empty"><MessageCircle size={26}/><h2>聊聊你的问题</h2></div>}</div>
      {busy&&!run?.task_id&&<div className="chat-actions"><button type="button" className="outline chat-stop" disabled={stopping===run?.id} onClick={()=>void stopConversation()}>
        {stopping===run?.id?<LoaderCircle size={16} className="spin"/>:<Square size={16}/>} {stopping===run?.id?'正在停止…':'停止回复'}
      </button></div>}
      {run?.status==='cancelled'&&!run.response&&<p className="muted" role="status">已停止</p>}
      {(chatError||canResume) && <div className="chat-actions">
        {chatError && <p className="error" role="alert">{chatError}</p>}
        {canResume && <button type="button" className="outline" disabled={saving} onClick={()=>void resumeConversation()}><Play size={16}/>继续回复</button>}
      </div>}
      {id && data && !busy && !conversation?.archived && <button className="text-button" onClick={()=>setPromote(!promote)}>{promote?'继续普通对话':'把这段对话交给助手继续处理'}</button>}
      <Composer key={promote?'promotion':'chat'} draftKey={(promote?'promotion:':'chat:')+(id||'new')} placeholder={promote?'描述需要继续完成的工作…':'发送消息…'} label={promote?'交给助手':'发送消息'} assign={promote} disabled={busy||saving||conversation?.archived||Boolean(id&&!data)} disabledReason={conversation?.archived?'恢复对话后可发送':busy?'本轮处理中，草稿可继续编辑':undefined} onSend={async(text,key,scopeLabel,attachments,delivery)=>{
        if (promote && id) {
          const receipt=await api<Receipt>('/conversations/'+id+'/task',{text,client_request_id:key,scope_label:scopeLabel,attachment_ids:attachments,...delivery})
          navigate('/tasks/'+receipt.task_id)
          return
        }
        const receipt=await api<Receipt>(id?'/conversations/'+id+'/messages':'/conversations',{text,client_message_id:key,attachment_ids:attachments})
        if (receipt.task_id) navigate('/tasks/'+receipt.task_id)
        else if (id) await reload()
        else navigate('/chat/'+receipt.conversation_id)
      }}/>
    </main>
    <aside className="detail-pane chat-history"><div className="detail-content">
      <header className="discussion-heading"><h2>{showArchived?'已归档':'最近对话'}</h2><button type="button" className="text-button" onClick={()=>setShowArchived(!showArchived)}>{showArchived?'最近对话':'查看归档'}</button></header>
      {(showArchived?archivedItems:items).map(item=><button key={item.id} className={'history-row '+(id===item.id?'selected':'')} onClick={()=>navigate('/chat/'+item.id)}>{item.archived?<Archive size={17}/>:<MessageCircle size={17}/>} {item.title}</button>)}
      {(showArchived?archiveNext:next)&&<button type="button" className="text-button history-more" disabled={listPending} onClick={()=>void loadConversations(showArchived)}>{listPending?'正在加载…':showArchived?'加载更多归档':'加载更多对话'}</button>}
      {showArchived && archivedItems.length===0 && <p className="muted">暂无归档对话</p>}
    </div></aside>
  </>
}

const artifactPageSize=100

function Artifacts({selected,scope,taskID,navigate}:{selected?:string;scope:string;taskID:string;navigate:(path:string)=>void}) {
  const [items,setItems]=useState<Artifact[]>()
  const [artifact,setArtifact]=useState<Artifact>()
  const [error,setError]=useState('')
  const [more,setMore]=useState(false)
  const [loading,setLoading]=useState(false)
  const filters=new URLSearchParams({scope_label:scope,task_id:taskID}).toString()
  const endpoint='/artifacts?'+filters
  useEffect(()=>{
    let cancelled=false
    api<Artifact[]>(endpoint).then(items=>{
      if(cancelled)return
      setItems(items)
      setMore(items.length===artifactPageSize)
      const key=selected||items[0]?.id
      if(key)return api<Artifact>('/artifacts/'+key).then(a=>{
        if(cancelled)return
        if((scope && a.scope_label!==scope) || (taskID && a.task_id!==taskID))setError('这份成果不在当前筛选范围内。')
        else setArtifact(a)
      })
    }).catch(e=>{if(!cancelled)setError(errorMessage(e))})
    return()=>{cancelled=true}
  },[endpoint,selected,scope,taskID])
  async function loadMore() {
    const last=items?.at(-1)
    if(loading || !last)return
    setLoading(true)
    try {
      const page=await api<Artifact[]>(endpoint+'&after='+last.id)
      setItems(current=>[...(current??[]),...page])
      setMore(page.length===artifactPageSize)
    }catch(e){setError(errorMessage(e))}
    finally{setLoading(false)}
  }
  return <>
    <main className={'main artifacts-list '+(selected?'artifacts-list-selected':'')}><header className="page-heading"><div><h1>成果</h1>{(scope||taskID) && <p>{taskID?(items?.[0]?.task_title||'指定事项'):scope}</p>}</div>
      {taskID && <button className="outline" onClick={()=>navigate(artifactRoute(undefined,scope,''))}>返回成果列表</button>}</header>
      {error&&<p className="error artifact-list-error" role="alert">{error}</p>}
      {!items&&!error&&<LoaderCircle className="spin" aria-label="正在加载成果"/>}
      {items?.length===0&&!error&&<div className="empty"><FileText size={28}/><h2>暂无成果</h2></div>}
      {items?.map(item=><button key={item.id} className={'task-row '+(item.id===artifact?.id?'selected':'')} onClick={()=>navigate(artifactRoute(item.id,scope,taskID))}><span className="artifact-icon"><FileText size={19}/></span><span className="task-copy"><strong>{item.task_title||item.name}</strong><small>{item.name} · {versionTime.format(item.created_at)}{item.accepted_at?' · 已验收':''}</small></span><ChevronRight size={17}/></button>)}
      {more&&<button className="outline" disabled={loading} onClick={()=>void loadMore()}>加载更多</button>}</main>
    <aside className={'detail-pane artifacts-pane '+(selected?'artifacts-pane-selected':'')}><div className="detail-content">
      <button className="text-button artifacts-back" onClick={()=>navigate(artifactRoute(undefined,scope,taskID))}><ArrowLeft size={16}/>返回成果</button>
      {error&&<p className="error artifact-detail-error" role="alert">{error}</p>}
      {selected&&!artifact&&!error&&<LoaderCircle className="spin" aria-label="正在加载成果详情"/>}
      {artifact&&<><h2>{artifact.name}</h2><p className="muted">{artifact.task_title} · {versionTime.format(artifact.created_at)}{artifact.accepted_at?' · 已验收':''}</p>
        {artifact.acceptance&&<details className="artifact-history"><summary><Check size={16}/>验收记录</summary>
          <p className="muted">{versionTime.format(artifact.acceptance.accepted_at)}</p>
          {artifact.acceptance.criteria&&<p className="criteria-text">{artifact.acceptance.criteria}</p>}
          {artifact.acceptance.filename&&<p className="criteria-text"><code>outputs/{artifact.acceptance.filename}</code></p>}
        </details>}
        <FormattedText text={artifact.content || ''} framed/><button className="detail-link" onClick={()=>navigate('/tasks/'+artifact.task_id)}>查看来源工作<ChevronRight size={17}/></button><a className="outline" href={'/api/v1/artifacts/'+artifact.id+'/content'}><ArrowDownToLine size={17}/>下载成果</a></>}</div></aside>
  </>
}

createRoot(document.getElementById('root')!).render(<App/>)
