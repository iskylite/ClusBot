import { createContext, lazy, Suspense, useContext, useCallback, useEffect, useEffectEvent, useLayoutEffect, useRef, useState, type FormEvent } from 'react'
import { createRoot } from 'react-dom/client'
import { createPortal } from 'react-dom'
import {
  Activity, ArrowDown, ArrowDownToLine, ArrowLeft, ArrowUp, Bell, Check, ChevronDown, ChevronLeft, ChevronRight, CircleAlert, CirclePause, Clock3, FileOutput, FileText,
  FolderOpen, Inbox, LayoutList, LoaderCircle, MessageCircle, Paperclip, Plus,
  Search, Settings2, X, Pause, Play, Square, ListChecks, History, PanelLeft, PanelRight,
  Archive, ArchiveRestore, MessageCircleQuestionMark, RotateCcw, ShieldCheck, type LucideIcon,
} from 'lucide-react'
import { activeRun, api, APIError, awaitingPublication, type Artifact, type ConversationPage, type DeliveryUpdate, type Detail, type Overview, type OverviewGroup, type Receipt, type Material, type Run, type Settings, type Task } from './api'
import { FormattedText, MemoryReferences, Messages, UsageDetails } from './ExecutionText'
import { RunHistory } from './RunHistory'
import { TaskEditor } from './TaskEditor'
import { MaterialPicker } from './MaterialPicker'
import {MessageAttachments,MessageContent,MessageReference} from './MessageContent'
import { PublicationPanel } from './PublicationPanel'
import { MemoryPage } from './MemoryPage'
import { ExperienceCandidatesPage } from './ExperienceCandidatesPage'
import { SkillProposalsPage } from './SkillProposalsPage'
import {MessageHistory,useMessageAnchor} from './MessageHistory'
import {clearMemoryDrafts} from './memoryDrafts'
import { ConnectionsPage } from './ConnectionsPage'
import { ReadEvidence, TaskReadPicker, readRequest } from './TaskRead'
import { beginDraftRequest, changeComposerDraft, clearComposerDrafts, draftGeneration, enqueuePendingAdmission, finishDraftRequest, hasOtherDraftMaterial, releaseDraftRequest, removePendingAdmission, restoreRevisionDraft, useComposerDraft, usePendingAdmissions, type TaskReference } from './composerDrafts'
import {BenchmarkExecution,benchmarkReadiness,useBenchmarkProgress,type BenchmarkProgress} from './BenchmarkExecution'
import {benchmarkTaskTokens,isBenchmarkRevisionInstruction,namedBenchmarkTasks} from './benchmarkRevisionInput'
import {useConversationTasks,type TaskLink} from './ConversationTasks'
import {BenchmarkConversation,type RevisionPreview} from './BenchmarkConversation'
import {ChatTaskDrawer} from './ChatTaskDrawer'
import {ConversationConfiguration} from './ConversationConfiguration'
import {IsolatedReplyComposer,isolatedDraftKey} from './IsolatedReplyComposer'
import {BenchmarkExecutionConversation} from './BenchmarkExecutionConversation'
import {AssistantCard} from './AssistantCards'
import {assistantReplyCards,isAssistantItem,type AssistantItem} from './assistantSurface'
import {ReplyStrategy,useComposerOptions,useComposerSuggestions,type ReplyModel} from './ComposerUI'
import {AccountPage, ForcedPasswordChange, ServerLogin, type Account, type Session} from './AccountUI'
import {applyPreferences, displayDate, GeneralPreferencesPage, uiLanguage, useTaskNotifications, type Preferences} from './GeneralPreferences'
import {PreferencesController} from './preferencesController'
import {SettingsDiagnostics} from './SettingsDiagnostics'
import clusbotIcon from './clusbot-brand.svg'
import {WorkspaceSidebar} from './WorkspaceSidebar'
import {AssistantNoticeMessage, FeedbackCallout} from './Feedback'
import '@fontsource-variable/noto-sans-sc'
import '@fontsource-variable/jetbrains-mono'
import './styles.css'
import './v31.css'
import './Feedback.css'
import './Composer.css'

if (window.clusbotDrafts) document.documentElement.classList.add('desktop-app')

const SettingsPage = lazy(()=>import('./SettingsPage').then(module=>({default:module.SettingsPage})))
const OperationsPage = lazy(()=>import('./OperationsPage').then(module=>({default:module.OperationsPage})))
const TeamTasksPage = lazy(()=>import('./TeamTasksPage').then(module=>({default:module.TeamTasksPage})))
const TaskNotices = lazy(()=>import('./TeamTasksPage').then(module=>({default:module.TaskNotices})))
const loadingPage = <main className="main v31-wide"><LoaderCircle className="spin" aria-label="正在加载"/></main>

const VisionContext = createContext(false)

const time = (value: number) => displayDate(value, { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' })
const versionTime={format:(value:number)=>displayDate(value,{month:'numeric',day:'numeric',hour:'2-digit',minute:'2-digit',second:'2-digit'})}
const errorMessage = (error: unknown) => error instanceof Error ? error.message : '操作未完成'

function Brand() {
  return <div className="brand"><img src={clusbotIcon} width={28} height={28} alt=""/><span>ClusBot</span></div>
}

type CoreRoleStatus = {
  role: string
  status: string
  source?: string
  modelID?: string
}

type ModelRoutingSnapshot = {
  status: string
  source?: string
  modelID?: string
  role?: string
  enabledModelCount?: number
  sampleModelName?: string
  unreadyCoreRoles?: CoreRoleStatus[]
}

const coreModelRoles = [
  { role: 'general_router', zh: '对话路由', en: 'Conversation Router' },
  { role: 'conversation_reply', zh: '普通对话', en: 'Conversation Reply' },
  { role: 'task_worker', zh: '任务工作', en: 'Task Worker' },
] as const

const modelRouteReady = (status?: string) => status !== undefined && ['configured','locally_configured','local_demo','demo'].includes(status)
const modelRouteStatusLabels: Record<string,string> = {
  not_configured:'未配置',settings_unavailable:'无法读取模型设置',default_unavailable:'全局默认模型不可用',
  policy_unavailable:'阶段策略不可用',profile_missing:'关联模型已不存在',profile_disabled:'关联模型已停用',
  provider_disabled:'服务提供方已停用',credential_missing:'凭据不可用',credential_changed:'凭据已变化',
  profile_revision_changed:'模型配置版本已变化',profile_invalid:'模型配置无效',invalid_configuration:'模型配置无效',
  default_destination_mismatch:'模型与默认服务不一致',demo_profile_disabled:'演示模式不可用',demo_role_unavailable:'演示模式不可用',
}
const modelRouteSourceLabels: Record<string,string> = {
  business_override:'业务覆盖策略',role_policy:'阶段模型策略',global_default:'全局默认模型',explicit_profile:'旧角色绑定',
}

function App() {
  const [session, setSession] = useState<Session>()
  const [error, setError] = useState('')
  const logout = useCallback(() => {
    clearComposerDrafts()
    clearMemoryDrafts()
    setSession(previous => previous ? {mode:previous.mode,authenticated:false} : previous)
    const root = document.documentElement
    root.lang = 'zh-CN'
    for (const field of ['appearance','reduceMotion','timezone','sendShortcut']) delete root.dataset[field]
  }, [])
  const readSession = useCallback(() => {
    setError('')
    return api<Session>('/session').then(setSession).catch(e => setError(errorMessage(e)))
  }, [])
  useEffect(() => {
    void readSession()
    window.addEventListener('clusbot:logout', logout)
    return () => window.removeEventListener('clusbot:logout', logout)
  }, [logout,readSession])
  if (error) return <div className="login-shell"><p role="alert">{error}</p><button onClick={() => void readSession()}>重试</button></div>
  if (!session) return <div className="login-shell"><LoaderCircle className="spin" aria-label="正在加载"/></div>
  if (!session.authenticated) return session.mode === 'server' ? <ServerLogin onLogin={setSession}/> : <Login enter={() => void readSession()}/>
  if (session.user?.must_change_password) return <ForcedPasswordChange account={session.user} onLogout={logout}/>
  return <Workspace key={session.user?.principal_id ?? 'local'} session={session} logout={logout}
    onAccountChange={user=>setSession(previous=>previous ? {...previous,user} : previous)}/>
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
  if(url.pathname==='/overview' || /^\/(tasks|artifacts)(\/|$)/.test(url.pathname)) {
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
type ChatToolbarView='materials'|'operations'|'trends'

function Workspace({session,logout,onAccountChange}: {session:Session; logout:()=>void; onAccountChange:(account:Account)=>void}) {
  const [route,setRoute]=useState(location.pathname+location.search+location.hash)
  const [pathAndQuery,hash='']=route.split('#')
  const [path,query='']=pathAndQuery.split('?')
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
  const [preferencesController]=useState(()=>{
    let storage:Storage|undefined
    try {storage=window.sessionStorage} catch { /* Preferences remain available in this workspace. */ }
    return new PreferencesController({
      load:()=>api<Preferences>('/preferences'),
      save:value=>api<Preferences>('/preferences',value,'PUT'),
      isConflict:error=>error instanceof APIError&&error.status===409,
    },storage,'clusbot:preferences:'+encodeURIComponent(session.user?.principal_id??'local'))
  })
  const [preferenceState,setPreferenceState]=useState(()=>preferencesController.snapshot())
  const preferences=preferenceState.value
  const [sidebarCollapsed,setSidebarCollapsed]=useState(false)
  const [noticeOpen,setNoticeOpen]=useState(false)
  const [teamActivity,setTeamActivity]=useState(false)
  const [allTaskCount,setAllTaskCount]=useState<number>()
  const [currentItemsOpen,setCurrentItemsOpen]=useState(false)
  const currentItemsTrigger=useRef<HTMLButtonElement>(null)
  const currentItemsPopover=useRef<HTMLElement>(null)
  const [sidebarTask,setSidebarTask]=useState<{id:string;trigger:HTMLButtonElement}>()
  const [chatToolbarView,setChatToolbarView]=useState<ChatToolbarView>()
  const [chatToolbarActions,setChatToolbarActions]=useState<HTMLDivElement|null>(null)
  const clearSidebarTask=useCallback(()=>setSidebarTask(undefined),[])
  const [searchOpen,setSearchOpen]=useState(false)
  const [modelSnapshot,setModelSnapshot]=useState<ModelRoutingSnapshot>()
  const [modelStatusAttempt,setModelStatusAttempt]=useState(0)
  const [modelNoticeLifecycle,setModelNoticeLifecycle]=useState<'none'|'active'|'resolved'|'dismissed'>('none')
  const prevModelUnavailable=useRef(false)
  const multiuser=session.mode==='server'
  const superadmin=Boolean(session.user?.is_superadmin)
  const canManageModels=!multiuser||superadmin
  const english=uiLanguage(preferences?.language)==='en-US'
  const t=(zh:string,en:string)=>english?en:zh

  useEffect(()=>{
    const unsubscribe=preferencesController.subscribe(setPreferenceState)
    preferencesController.start()
    return()=>{unsubscribe();preferencesController.stop()}
  },[preferencesController])
  useLayoutEffect(()=>{if(preferences)applyPreferences(preferences)},[preferences])
  useTaskNotifications(Boolean(preferences?.desktop_notifications))

  const [selected, setSelected] = useState('')
  const [detailOpen, setDetailOpen] = useState(false)
  const [menu, setMenu] = useState(false)
  const [error, setError] = useState('')
  const justCreatedTask = useRef('')
  const lastChat = useRef('/chat')
  const compose = useRef<HTMLTextAreaElement>(null)
  const [area,id,source] = path.split('/').slice(1)
  const section = area || 'chat'
  useEffect(()=>{
    if(section==='chat'&&path!=='/')lastChat.current=route
  },[section,path,route])
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
    setRoute(location.pathname+location.search+location.hash)
    window.dispatchEvent(new Event('clusbot:navigate'))
    setMenu(false)
    setCurrentItemsOpen(false)
    setNoticeOpen(false)
  },[])
  useEffect(()=>{
    if(route!=='/')return
    let active=true
    api<ConversationPage>('/conversations/history?limit=1').then(({items})=>{
      if(active&&location.pathname==='/'&&items[0])openRoute('/chat/'+encodeURIComponent(items[0].id),true)
    }).catch(()=>undefined)
    return()=>{active=false}
  },[route,openRoute])
  const navigate=useCallback((next:string)=>openRoute(workRoute(next,scope,search)),[openRoute,scope,search])
  const returnToChat=()=>navigate(lastChat.current)
  const setSearch=(value:string)=>openRoute(isHistory?taskHistoryRoute('/tasks',scope,value,historyStatus):workRoute(path,scope,value),true)
  const navigateFromOverview=(next:string)=>{
    if(isHistory&&/^\/tasks\/[^/?]+$/.test(next))openRoute(taskHistoryRoute(next,scope,search,historyStatus))
    else navigate(next)
  }
  useEffect(() => {
    const pop = () => setRoute(location.pathname+location.search+location.hash)
    window.addEventListener('popstate',pop)
    window.addEventListener('hashchange',pop)
    return () => {
      window.removeEventListener('popstate',pop)
      window.removeEventListener('hashchange',pop)
    }
  },[])
  useEffect(() => {
    if (canManageModels) void api<Settings>('/settings').then(setSettings).catch(e => setError(errorMessage(e)))
  },[canManageModels])
  const refreshModelSnapshot=useCallback(async(signal?:{cancelled:boolean})=>{
    try {
      const [routing,catalog]=await Promise.all([
        api<{roles:{role:string;status:string;source?:string;model_id?:string}[]}>('/assistant/model-routing'),
        canManageModels?api<{name:string;model_id:string;enabled:boolean;provider_enabled:boolean}[]>('/settings/model/catalog').catch(()=>undefined):Promise.resolve(undefined),
      ])
      if(signal?.cancelled)return
      const coreRoles: CoreRoleStatus[] = coreModelRoles.map(spec => {
        const found = routing.roles?.find(role => role.role === spec.role)
        return {
          role: spec.role,
          status: found?.status ?? 'settings_unavailable',
          source: found?.source,
          modelID: found?.model_id,
        }
      })
      const unreadyCoreRoles = coreRoles.filter(item => !modelRouteReady(item.status))
      const primary =
        unreadyCoreRoles.find(item => item.status !== 'not_configured' && item.status !== 'settings_unavailable') ??
        unreadyCoreRoles[0] ??
        coreRoles.find(item => item.role === 'conversation_reply') ??
        coreRoles[0]
      const enabled=catalog?.filter(item=>item.enabled&&item.provider_enabled)
      setModelSnapshot({
        status:primary?.status??'settings_unavailable',
        source:primary?.source,
        modelID:primary?.modelID,
        role:primary?.role,
        enabledModelCount:enabled?.length,
        sampleModelName:enabled?.[0]?.name||enabled?.[0]?.model_id,
        unreadyCoreRoles,
      })
    } catch {
      if(!signal?.cancelled)setModelSnapshot({status:'settings_unavailable'})
    }
  },[canManageModels])
  useEffect(()=>{
    if(section!=='chat')return
    const signal={cancelled:false}
    void refreshModelSnapshot(signal)
    const onFocus=()=>{if(!document.hidden)void refreshModelSnapshot(signal)}
    window.addEventListener('focus',onFocus)
    document.addEventListener('visibilitychange',onFocus)
    return()=>{signal.cancelled=true;window.removeEventListener('focus',onFocus);document.removeEventListener('visibilitychange',onFocus)}
  },[section,modelStatusAttempt,refreshModelSnapshot])
  useEffect(()=>{
    if(section!=='chat'||!modelSnapshot||modelRouteReady(modelSnapshot.status))return
    const signal={cancelled:false}
    const timer=window.setInterval(()=>{if(!document.hidden)void refreshModelSnapshot(signal)},4000)
    return()=>{signal.cancelled=true;clearInterval(timer)}
  },[section,modelSnapshot,refreshModelSnapshot])
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
  const isOverview = section === 'overview' || (section === 'tasks' && isHistory)
  function openTask(task: Task) {
    if (matchMedia('(max-width: 1050px)').matches) navigateFromOverview('/tasks/'+task.id)
    else {
      setSelected(task.id)
      setDetailOpen(true)
    }
  }
  function onTask(path:string) {
    const taskID=path.slice('/tasks/'.length)
    justCreatedTask.current=taskID
    setSelected(taskID)
    navigate(path)
    void refresh()
  }
  function submitTask(text:string,key:string,scopeLabel:string,attachments:string[],delivery:DeliveryUpdate) {
    return api<Receipt>('/tasks',{text,client_request_id:key,scope_label:scopeLabel,attachment_ids:attachments,...delivery})
  }
  const focusNewTaskHeading=useCallback((heading:HTMLHeadingElement|null)=>{
    if(heading&&justCreatedTask.current===id){
      justCreatedTask.current=''
      heading.focus()
    }
  },[id])
  const nav = [
    { label:t('个人助手','Assistant'), path:'/chat', icon:MessageCircle, on:section==='chat' },
    { label:t('任务清单','Task list'), path:superadmin?'/team-tasks':'/tasks', icon:LayoutList, on:section==='tasks'||section==='team-tasks' },
    { label:t('运维趋势','Operations'), path:'/operations', icon:Activity, on:section==='operations' },
    { label:t('设置','Settings'), path:'/settings', icon:Settings2, on:section==='settings'||section==='connections' },
  ]
  const sectionTitle=nav.find(item=>item.on)?.label ??
    ({overview:t('工作总览','Overview'),account:t('账号与安全','Account and security'),memory:t('个人记忆','Memory'),
      connections:t('工具与连接','Connections'),artifacts:t('成果','Artifacts')} as Record<string,string>)[section] ?? 'ClusBot'
  const waitingItems=groups?.waiting??[]
  const personalTaskItems=[
    ...waitingItems.filter(task=>task.status_reason!=='acceptance_required'),
    ...(groups?.active??[]),
    ...waitingItems.filter(task=>task.status_reason==='acceptance_required'),
  ]
    .filter(task=>task.status!=='cancelled'&&task.status!=='completed')
  const hasCurrentItems=personalTaskItems.length>0
  const modelUnavailable=modelSnapshot!==undefined&&!modelRouteReady(modelSnapshot.status)
  const modelStatusUnknown=modelSnapshot?.status==='settings_unavailable'
  const modelUnselected=modelSnapshot?.status==='not_configured'
  useEffect(()=>{
    if(modelUnavailable){
      prevModelUnavailable.current=true
      setModelNoticeLifecycle(prev=>prev==='dismissed'?'dismissed':'active')
    } else if(modelSnapshot!==undefined && modelRouteReady(modelSnapshot.status)){
      if(prevModelUnavailable.current){
        prevModelUnavailable.current=false
        setModelNoticeLifecycle(prev=>prev==='active'?'resolved':'none')
      }
    }
  },[modelUnavailable,modelSnapshot])
  useEffect(()=>{
    if(!hasCurrentItems)setCurrentItemsOpen(false)
  },[hasCurrentItems])
  function toggleCurrentItems(trigger:HTMLButtonElement){
    currentItemsTrigger.current=trigger
    setCurrentItemsOpen(value=>!value)
  }
  function openSidebarTask(task:Task,trigger:HTMLButtonElement){
    const returnFocus=currentItemsPopover.current?.contains(trigger)?currentItemsTrigger.current??trigger:trigger
    setCurrentItemsOpen(false)
    setMenu(false)
    if(section!=='chat')returnToChat()
    setSidebarTask({id:task.id,trigger:returnFocus})
  }
  const settingsArea=section==='settings'||section==='connections'
  const requestedTab=params.get('tab')
  const settingsTab=section==='connections'?'connections':
    requestedTab==='execution'||requestedTab==='diagnostics'?requestedTab:
    !canManageModels?'general':
    requestedTab==='skills'||hash==='skill-directory'?'skills':
    requestedTab==='model'||hash.startsWith('model-')?'model':'general'
  const settingsTabs=[
    {id:'general',text:t('常规','General'),path:'/settings'},
    ...(canManageModels?[
      {id:'model',text:t('模型','Models'),path:'/settings?tab=model'},
      {id:'skills',text:'Skills',path:'/settings?tab=skills'},
    ]:[]),
    {id:'connections',text:t('MCP 连接与工具','MCP connections'),path:'/connections'},
    {id:'execution',text:t('执行与权限','Execution and access'),path:'/settings?tab=execution'},
    {id:'diagnostics',text:t('数据与诊断','Data and diagnostics'),path:'/settings?tab=diagnostics'},
  ]
  const showBackInHeader=section==='team-tasks'||section==='tasks'&&!id&&!isHistory
  const chatView=section==='chat'?chatToolbarView:undefined
  const chatBreadcrumb=chatView==='materials'?t('材料预览与输入交互','Materials'):
    chatView==='operations'?t('集群运维与交互卡片','Cluster operations'):
    chatView==='trends'?t('趋势分析与故障流程','Trends and incidents'):t('对话','Conversation')
  const opsRange=params.get('range')
  const viewRange=opsRange==='24h'||opsRange==='7d'||opsRange==='30d'?opsRange:undefined
  const searchRef = useRef<HTMLInputElement>(null)
  async function searchWorkspace(event:FormEvent<HTMLFormElement>){
    event.preventDefault()
    const value=searchRef.current?.value.trim()
    if(!value)return
    setSearchOpen(false)
    const query=value.toLocaleLowerCase()
    const view=({
      '任务管理':superadmin?'/team-tasks':'/tasks','任务清单':superadmin?'/team-tasks':'/tasks','运维趋势':'/operations','工作总览':'/overview','成果':'/artifacts','个人记忆':'/memory',
      ...(session.user?{'账号与安全':'/account'}:{}),
    } as Record<string,string>)[query]
    if(view){navigate(view);return}
    if(query==='资产发现'){
      changeComposerDraft('chat:new',draft=>draft.text||draft.request||draft.taskReference||draft.benchmarkPlan||draft.materials.length?{}:{text:'查看资产发现'})
      navigate('/chat')
      return
    }

    if(query==='设置'||query==='settings'){navigate('/settings');return}
    const setting=settingsTabs.find(item=>item.text.toLocaleLowerCase()===query)
    if(setting){navigate(setting.path);return}
    if(canManageModels){
      try{
        const profiles=await api<{name:string;model_id:string;provider_id:string}[]>('/settings/model/catalog')
        const profile=profiles.find(item=>[item.name,item.model_id].some(label=>label?.toLocaleLowerCase()===query))
        if(profile){navigate('/settings?tab=model&provider='+encodeURIComponent(profile.provider_id));return}
      }catch{ /* Task search remains available if the catalog is unavailable. */ }
    }
    try{
      const {items}=await api<ConversationPage>('/conversations/history')
      const conversation=items.find(item=>item.title.toLocaleLowerCase()===query)
      if(conversation){navigate('/chat/'+encodeURIComponent(conversation.id));return}
    }catch{ /* Task search remains available if conversation history is unavailable. */ }
    openRoute((superadmin?'/team-tasks':'/tasks')+'?'+new URLSearchParams({q:value}))
  }
  useEffect(()=>{
    const shortcut = (event:KeyboardEvent) => {
      if (showBackInHeader || chatView==='materials' || !(event.ctrlKey||event.metaKey) || event.key.toLowerCase()!=='k' || event.isComposing) return
      event.preventDefault()
      setSearchOpen(true)
      requestAnimationFrame(()=>searchRef.current?.focus())
    }
    window.addEventListener('keydown',shortcut)
    return()=>window.removeEventListener('keydown',shortcut)
  },[chatView,showBackInHeader])
  return <VisionContext.Provider value={Boolean(settings?.attributes?.vision && settings.mode !== 'demo' && settings.mode !== 'unconfigured')}>
    <div className={'workspace v31-workspace' + (sidebarCollapsed?' rail-collapsed':'') + (isOverview && !focused ? ' overview-without-detail' : '')} data-section={section} data-settings={settingsArea||undefined} data-chat-view={chatView}
      onKeyDown={event=>{
        if(!currentItemsOpen||event.key!=='Escape'||event.defaultPrevented||event.nativeEvent.isComposing)return
        const target=event.target as Node
        if(!currentItemsTrigger.current?.contains(target)&&!currentItemsPopover.current?.contains(target))return
        event.preventDefault()
        setCurrentItemsOpen(false)
        currentItemsTrigger.current?.focus()
      }}>
    {menu && <button className="scrim v31-scrim" onClick={() => setMenu(false)} aria-label={t('关闭导航','Close navigation')}/>}
    <WorkspaceSidebar menu={menu} collapsed={sidebarCollapsed} section={section} settingsArea={settingsArea}
      personalTaskItems={personalTaskItems} allTaskCount={allTaskCount} superadmin={superadmin} user={session.user}
      t={t} taskReasonIcons={taskReasonIcons} taskStatusIcons={taskStatusIcons}
      onToggleCollapsed={()=>setSidebarCollapsed(value=>!value)} onReturnToChat={returnToChat}
      onNavigate={navigate} onOpenTask={openSidebarTask}/>
    <header className="unified-toolbar">
      <div className="toolbar-title"><button type="button" className="icon-button v31-mobile-nav" aria-label={t('打开导航','Open navigation')} aria-controls="workspace-sidebar" aria-expanded={menu} onClick={()=>setMenu(!menu)}><PanelLeft size={14}/></button><strong>{sectionTitle}</strong>
        {settingsArea&&<Breadcrumb>{settingsTabs.find(item=>item.id===settingsTab)?.text}</Breadcrumb>}
        {section==='account'&&session.user&&<Breadcrumb>{superadmin&&hash==='users'?t('用户管理','User management'):t('本人账号','My account')}</Breadcrumb>}
        {section==='chat'&&<><Breadcrumb conversation>{chatBreadcrumb}</Breadcrumb>
          {hasCurrentItems&&<button className="toolbar-changes" type="button" onClick={event=>toggleCurrentItems(event.currentTarget)} aria-expanded={currentItemsOpen} aria-controls={currentItemsOpen?'current-items-popover':undefined}>
            <i aria-hidden="true"/>{t('当前事项','Current tasks')} {personalTaskItems.length}<ChevronDown size={10} aria-hidden="true"/>
          </button>}
          {superadmin&&(!chatView||teamActivity)&&<div className="toolbar-view" role="group" aria-label={t('会话视角','Conversation view')}>
            <button type="button" className="toolbar-chip" aria-pressed={!teamActivity} onClick={()=>setTeamActivity(false)}>{t('仅看我的','My activity')}</button>
            <button type="button" className="toolbar-chip" aria-pressed={teamActivity} onClick={()=>setTeamActivity(true)}>{t('全站动态','Team activity')}</button>
          </div>}</>}
      </div>
      <div className="toolbar-chat-actions" ref={setChatToolbarActions}/>
      <div className="toolbar-actions" hidden={showBackInHeader||chatView==='materials'}>
        <button type="button" className="icon-button toolbar-search-trigger" aria-label={t('搜索工作区','Search workspace')} aria-expanded={searchOpen} onClick={()=>{
          setSearchOpen(!searchOpen)
          if(!searchOpen)requestAnimationFrame(()=>searchRef.current?.focus())
        }}><Search size={14}/></button>
        <form className="toolbar-search" data-open={searchOpen} role="search" onKeyDown={event=>{if(event.key==='Escape')setSearchOpen(false)}} onSubmit={searchWorkspace}>
          <Search size={13}/><input ref={searchRef} aria-label={t('搜索工作区','Search workspace')} placeholder={canManageModels?t('搜索任务、模型、设置…','Search tasks, models, settings…'):t('搜索任务、设置…','Search tasks, settings…')}/><kbd>⌘K</kbd></form>
        <button type="button" className="icon-button toolbar-bell" aria-label={t('待处理事项','Updates')} onClick={()=>setNoticeOpen(!noticeOpen)} aria-expanded={noticeOpen}><Bell size={15}/></button>
      </div>
      {showBackInHeader&&<button type="button" className="outline settings-back" onClick={returnToChat}><ArrowLeft size={14}/>{t('回到个人助手','Back to assistant')}</button>}
      {currentItemsOpen&&hasCurrentItems&&<aside id="current-items-popover" ref={currentItemsPopover} className="workspace-notices current-items-popover" aria-label={t('当前事项','Current tasks')}>
        {personalTaskItems.map(task=><button type="button" className="sidebar-task" key={task.id} onClick={event=>openSidebarTask(task,event.currentTarget)}>{task.title}</button>)}
        <button type="button" className="text-button" onClick={()=>{setCurrentItemsOpen(false);navigate(superadmin?'/team-tasks':'/tasks')}}>{t('任务清单','Task list')}</button>
      </aside>}
      {noticeOpen&&<aside className="workspace-notices">
        {superadmin ? <Suspense fallback={<p className="muted">{t('正在加载…','Loading…')}</p>}><TaskNotices onOpen={(owner,id)=>{setNoticeOpen(false);navigate('/team-tasks?'+new URLSearchParams({owner,task:id}))}}/></Suspense> :
          <><h2>{t('待处理事项','Needs attention')}</h2>{groups?.waiting.length?groups.waiting.map(task=><button className="sidebar-task" key={task.id} onClick={()=>{setNoticeOpen(false);navigate('/tasks?'+new URLSearchParams({task:task.id}))}}>{task.title}</button>):<p className="muted">{t('暂无待处理事项','No tasks need attention')}</p>}</>}
      </aside>}
    </header>
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
        {!isHistory&&<Composer draftKey="new:overview" inputRef={compose} placeholder="接下来，需要我帮你做什么？" label="交给助手" initialScope={scope} assign onSend={submitTask} onReceipt={onTask}/>}
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
    {section==='chat' && <Chat key={id || 'new'} id={id} navigate={navigate} assistantManage={canManageModels} modelConcurrency={settings?.model_concurrency} account={session.user} sidebarTask={sidebarTask} onSidebarTaskOpened={clearSidebarTask}
      toolbarActions={chatToolbarActions} onToolbarView={setChatToolbarView}
      activity={superadmin&&teamActivity?<Suspense fallback={<p className="muted">{t('正在加载…','Loading…')}</p>}><TaskNotices limit={4} onOpen={(owner,id)=>navigate('/team-tasks?'+new URLSearchParams({owner,task:id}))}/></Suspense>:undefined}
      modelNotice={(() => {
        const coreRoleLabel = (role?: string) => {
          const spec = coreModelRoles.find(item => item.role === role)
          return spec ? (english ? spec.en : spec.zh) : (english ? 'Conversation' : '对话')
        }
        const unreadyRoles = modelSnapshot?.unreadyCoreRoles ?? []
        const unreadyRoleNames = unreadyRoles.map(item => coreRoleLabel(item.role)).join(english ? ', ' : '、')
        const targetStage = modelSnapshot?.role || unreadyRoles[0]?.role || 'conversation_reply'
        const partiallyConfigured = unreadyRoles.length > 0 && unreadyRoles.length < coreModelRoles.length
        if (modelNoticeLifecycle==='resolved' && modelSnapshot) return {
          kind: 'resolved',
          title: t('对话模型已就绪','Conversation model is ready'),
          message: `${t('当前生效','Active')}: ${modelSnapshot.modelID || t('已配置模型','Configured model')}（${(modelSnapshot.source && modelRouteSourceLabels[modelSnapshot.source]) || t('模型配置','Model configuration')}）。${t('此前的配置提示已过时，可直接继续对话。','The previous configuration prompt is now outdated; you can continue chatting.')}`,
          onDismiss: () => setModelNoticeLifecycle('dismissed'),
        }
        if (modelUnavailable && modelNoticeLifecycle !== 'dismissed' && modelSnapshot) return {
          kind: modelStatusUnknown ? 'unknown' : modelUnselected ? (modelSnapshot.enabledModelCount === 0 ? 'no_models' : 'unselected') : 'broken',
          title: modelStatusUnknown
            ? t('暂无法确认对话模型状态','Cannot verify conversation model')
            : modelUnselected
              ? (modelSnapshot.enabledModelCount === 0
                ? t('尚未添加可用模型','No enabled models yet')
                : partiallyConfigured
                  ? t('部分核心阶段尚未配置模型','Some core stages still need a model')
                  : t('尚未指定对话使用的模型或策略','No conversation model or policy selected'))
              : `${coreRoleLabel(modelSnapshot.role)}${t('阶段的',' stage ')}${modelSnapshot.source && modelRouteSourceLabels[modelSnapshot.source] ? modelRouteSourceLabels[modelSnapshot.source] : t('模型配置','model configuration')}${t('不可用',' is unavailable')}`,
          message: modelStatusUnknown
            ? t('请重试检查；已知斜杠指令与只读任务查询仍可使用。','Try checking again. Known slash commands and read-only task queries still work.')
            : !canManageModels
              ? t('请联系管理员检查对话模型与阶段策略。','Ask an administrator to check the conversation model and routing policy.')
              : modelUnselected
                ? (modelSnapshot.enabledModelCount === 0
                  ? t('当前还没有已启用的服务提供方或模型。请先添加提供方并启用模型，再设为全局默认或配置阶段策略。','No enabled provider or model is available yet. Add a provider and enable a model first, then set it as global default or configure a stage policy.')
                  : partiallyConfigured
                    ? `${t('未设置「全局默认模型」时，系统需要「对话路由、普通对话、任务工作」全部就绪才会处理对话。当前未配置的阶段：','Without a Global Default model, all core stages (Conversation Router, Conversation Reply, Task Worker) must be configured before messages are processed. Still unconfigured: ')}${unreadyRoleNames}。`
                    : `${t('检测到已有可用模型','Available model detected')}（${modelSnapshot.sampleModelName ?? t('已启用模型','enabled model')}），${t('请将其设为「全局默认模型」，或分别为「对话路由、普通对话、任务工作」3 个核心阶段配置阶段模型策略：','please either set it as Global Default or configure stage policies for all 3 core stages (Conversation Router, Conversation Reply, Task Worker):')}`)
                : `${t('阶段','Stage')}：${coreRoleLabel(modelSnapshot.role)} · ${t('状态','Status')}：${modelRouteStatusLabels[modelSnapshot.status] ?? modelSnapshot.status}${modelSnapshot.modelID ? `（${modelSnapshot.modelID}）` : ''}。${t('请检查对应模型、提供方启用状态与凭据。','Please check the model, provider status, and credentials.')}`,
          primaryAction: modelStatusUnknown
            ? { label: t('重新检查','Check again'), onClick: () => setModelStatusAttempt(attempt => attempt + 1) }
            : canManageModels
              ? (modelUnselected && modelSnapshot.enabledModelCount !== 0
                ? (partiallyConfigured
                  ? { label: t('继续配置阶段策略','Configure next stage'), onClick: () => navigate(`/settings?tab=model&stage=${encodeURIComponent(targetStage)}#model-routing`) }
                  : { label: t('去设为全局默认模型','Set as global default'), onClick: () => navigate('/settings?tab=model') })
                : !modelUnselected && (modelSnapshot.source === 'business_override' || modelSnapshot.source === 'role_policy')
                  ? { label: t('打开阶段模型策略','Open stage policy'), onClick: () => navigate(`/settings?tab=model&stage=${encodeURIComponent(targetStage)}#model-routing`) }
                  : { label: modelSnapshot.enabledModelCount === 0 ? t('添加提供方与模型','Add provider & model') : t('打开模型设置','Open model settings'), onClick: () => navigate('/settings?tab=model') })
              : undefined,
          secondaryAction: canManageModels && modelUnselected && modelSnapshot.enabledModelCount !== 0
            ? (partiallyConfigured
              ? { label: t('去设为全局默认模型','Set as global default'), onClick: () => navigate('/settings?tab=model') }
              : { label: t('配置阶段模型策略','Configure stage policy'), onClick: () => navigate(`/settings?tab=model&stage=${encodeURIComponent(targetStage)}#model-routing`) })
            : undefined,
          onDismiss: () => setModelNoticeLifecycle('dismissed'),
        }
        return undefined
      })()}/> }
    {section==='artifacts' && <Artifacts key={JSON.stringify([id,scope,artifactTask])} selected={id} scope={scope} taskID={artifactTask} navigate={navigate}/>}
    {section==='memory' && (id==='skill-proposals' ? <SkillProposalsPage navigate={navigate} onChange={()=>void refresh()}/> : id==='candidates' ? <ExperienceCandidatesPage navigate={navigate}/> : <MemoryPage selected={id} sourceId={source} navigate={navigate}/>)}
    {settingsArea&&<div className="settings-toolbar">
      <nav className="settings-tabs" aria-label={t('设置分类','Settings categories')}>
        {settingsTabs.map(item=><button type="button" key={item.id} aria-current={settingsTab===item.id?'page':undefined} onClick={()=>navigate(item.path)}>{item.text}</button>)}
      </nav>
      <select className="settings-section-select" aria-label={t('设置分类','Settings categories')} value={settingsTabs.find(item=>item.id===settingsTab)?.path} onChange={event=>navigate(event.target.value)}>
        {settingsTabs.map(item=><option key={item.id} value={item.path}>{item.text}</option>)}
      </select>
      <button type="button" className="outline settings-back" onClick={returnToChat}><ArrowLeft size={14}/>{t('回到个人助手','Back to assistant')}</button>
    </div>}
    {preferences&&(preferenceState.status==='error'||preferenceState.status==='conflict')&&!(section==='settings'&&(settingsTab==='general'||settingsTab==='execution'))&&
      <aside className="preference-outside-alert" role="alert"><CircleAlert size={15} aria-hidden="true"/><span>{t('偏好设置尚未保存。','Preferences are not saved yet.')}</span><button type="button" className="text-button" onClick={()=>navigate('/settings')}>{t('查看','Review')}</button></aside>}
    {section==='connections'&&<ConnectionsPage selected={id} navigate={navigate}/>}
    {section==='settings'&&(
      settingsTab==='diagnostics'?<main className="main v31-wide settings-diagnostics">
          <h1 className="settings-sr-only">{t('数据与诊断','Data and diagnostics')}</h1>
          <SettingsDiagnostics workspacePath={multiuser?undefined:settings?.workspace_path} multiuser={multiuser} english={english}/>
        </main> : settingsTab==='model'||settingsTab==='skills'?<Suspense fallback={loadingPage}><SettingsPage key={settingsTab+':'+(params.get('provider')??'')+':'+(params.get('model')??'')} view={settingsTab} onSaved={value=>{setSettings(value);setModelStatusAttempt(attempt=>attempt+1)}} openConnections={()=>navigate('/connections')} openProposals={()=>navigate('/memory/skill-proposals')}/></Suspense> :
        <main className="main v31-wide general-settings">
          <GeneralPreferencesPage view={settingsTab==='execution'?'execution':'general'} modelConcurrency={settings?.model_concurrency} state={preferenceState}
            onChange={(key,value)=>preferencesController.update(key,value)} onRetry={()=>preferencesController.retry()} onResolve={keep=>preferencesController.resolveConflicts(keep)} multiuser={multiuser}/>
        </main>
    )}
    {section==='account'&&session.user&&<AccountPage key={hash} account={session.user} onAccountChange={onAccountChange} onLogout={logout} onNavigate={navigate} initialSection={hash==='password'?'password':hash==='users'?'users':hash==='profile'?'profile':undefined}/>}
    {section==='operations'&&<Suspense fallback={loadingPage}><OperationsPage key={query} isAdmin={superadmin} initialIncident={params.get('incident')??undefined} initialRange={viewRange}/></Suspense>}
    {((section==='tasks'&&!id&&!isHistory)||section==='team-tasks')&&<Suspense fallback={loadingPage}>
      <TeamTasksPage key={section+':'+query} mode={section==='team-tasks'&&superadmin?'admin':'personal'} navigate={navigate}
        initialOwner={params.get('owner')??undefined} initialTaskID={params.get('task')??undefined}
        initialQuery={params.get('q')??undefined} onAllCount={section==='team-tasks'&&superadmin?setAllTaskCount:undefined}/>
    </Suspense>}

  </div></VisionContext.Provider>
}

function Breadcrumb({children,conversation=false}:{children:React.ReactNode;conversation?:boolean}) {
  return <span className={'muted toolbar-breadcrumb'+(conversation?' conversation-breadcrumb':'')}>
    <span className="toolbar-crumb-divider" aria-hidden="true"/><span className="toolbar-crumb-label">{children}</span>
  </span>
}

const taskReasonIcons:Record<string,LucideIcon>={
  input_required:MessageCircleQuestionMark,approval_required:ShieldCheck,acceptance_required:ListChecks,
  delivery_required:FileOutput,file_writing:FileOutput,reconciliation_required:CircleAlert,paused:CirclePause,
  benchmark_dispatch_paused:CirclePause,
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
    const tone=task.status_reason==='paused'||task.status_reason==='benchmark_dispatch_paused'?'paused':backgroundWait?'active':task.status
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

function receiptPath(receipt:Receipt,assign:boolean) {
  const validID=(id?:string)=>Boolean(id && /^[a-f\d]{32}$/.test(id))
  if(validID(receipt.task_id))return '/tasks/'+receipt.task_id
  if(!assign && receipt.conversation_id && /^[a-z\d_-]{1,128}$/i.test(receipt.conversation_id))return '/chat/'+receipt.conversation_id
}

type OptimisticStartItem = {
  key:string;text:string;createdAt:number;materials:Material[]
  taskRef?:{taskID:string;title:string}
}
type OptimisticFailInfo = {
  message:string;unconfirmed:boolean;retry:()=>void;restore:()=>void
}

type ComposerProps = {
  draftKey:string
  previewMaterials?:Material[]
  placeholder:string; label:string; onSend:(text:string,key:string,scope:string,attachments:string[],delivery:DeliveryUpdate,strategy?:'role_routing'|'deterministic_only')=>Promise<Receipt>
  onReceipt:(path:string,receipt?:Receipt)=>void|Promise<void>
  onAssistantReceipt?:(item:AssistantItem,text:string)=>void
  prepareDelivery?:(text:string,attachments:string[],delivery:DeliveryUpdate,strategy:'role_routing'|'deterministic_only')=>Promise<DeliveryUpdate>
  disabled?:boolean; disabledReason?:string; assign?:boolean; initialScope?:string; task?:Task; inputRef?:React.Ref<HTMLTextAreaElement>; reference?:React.ReactNode; openIsolated?:(model?:ReplyModel,text?:string)=>void; openModelSettings?:()=>void; busyAction?:React.ReactNode; isolatedRecovery?:boolean; hidden?:boolean; referenceConversationID?:string; onReferenceTask?:(reference:TaskReference)=>void
  onStatusNotice?:(notice:{message:string;unconfirmed?:boolean;retry?:()=>void;dismiss:()=>void}|null)=>void
  onAdmissionChange?:(admitting:boolean)=>void
  onOptimisticStart?:(item:OptimisticStartItem)=>void
  onOptimisticSettle?:(key:string)=>void
  onOptimisticFail?:(key:string,info:OptimisticFailInfo)=>void
}

function Composer({draftKey,previewMaterials,placeholder,label,onSend,onReceipt,onAssistantReceipt,prepareDelivery,disabled=false,disabledReason,assign=false,initialScope='',task,inputRef,reference,openIsolated,openModelSettings,busyAction,isolatedRecovery,hidden,referenceConversationID,onReferenceTask,onStatusNotice,onAdmissionChange,onOptimisticStart,onOptimisticSettle,onOptimisticFail}:ComposerProps) {
  const allowImages = useContext(VisionContext)
  const [sendShortcut,setSendShortcut]=useState(()=>document.documentElement.dataset.sendShortcut==='mod_enter'?'mod_enter':'enter')
  useEffect(()=>{
    const root=document.documentElement
    const update=()=>setSendShortcut(root.dataset.sendShortcut==='mod_enter'?'mod_enter':'enter')
    const observer=new MutationObserver(update)
    observer.observe(root,{attributes:true,attributeFilter:['data-send-shortcut']})
    update()
    return()=>observer.disconnect()
  },[])
  const draft=useComposerDraft(draftKey)
  const {text:value,materials,criteria:criteriaDraft}=draft
  const plan=typeof draft.benchmarkPlan==='string'?draft.benchmarkPlan:undefined
  const validPlan=plan!==undefined&&/^[^\s@\\/:]+@v?[1-9][0-9]*$/i.test(plan.trim())
  const hasBody=Boolean(value.trim())
  const exactCommand=value.trim().match(/^(\/benchmark[ \t]+[^\s@\\/:]+@)v([1-9][0-9]*)$/)
  const outgoingText=plan===undefined?(exactCommand?exactCommand[1]+exactCommand[2]:value):
    validPlan?'/benchmark '+(hasBody?plan.trim()+'\n'+value:plan.trim().replace(/@v/i,'@')):''
  const {options,error:optionsError}=useComposerOptions(referenceConversationID)
  const textarea=useRef<HTMLTextAreaElement>(null)
  const planInput=useRef<HTMLInputElement>(null)
  const compositionEnd=useRef<number|undefined>(undefined)
  const [strategy,setStrategy]=useState<'role_routing'|'deterministic_only'>('role_routing')
  const selectedStrategy=draft.request?.strategy??strategy
  const readDrafts=draft.reads??(draft.read?[draft.read]:[])
  const scope=draft.scope ?? draft.request?.scope ?? initialScope
  const edit=(patch:Partial<typeof draft>)=>{setUnconfirmed(false);changeComposerDraft(draftKey,()=>patch)}
  const suggestions=useComposerSuggestions({
    value,setValue:text=>edit({text}),input:textarea,options,canReference:Boolean(onReferenceTask),
    onReference:item=>onReferenceTask?.({taskID:item.id,title:item.title,passive:true}),
    onBenchmarkCommand:onAssistantReceipt?text=>{
      edit({text,benchmarkPlan:plan??''})
      requestAnimationFrame(()=>planInput.current?.focus())
    }:undefined,
  })
  const [pending,setPending]=useState(false)
  const [error,setError]=useState('')
  const [unconfirmed,setUnconfirmed]=useState(false)
  const [focusCriteria,setFocusCriteria]=useState(false)
  const locked=useRef(false)
  const captureLock=useRef(false)
  const [capturing,setCapturing]=useState(false)
  const pendingAdmissions=usePendingAdmissions(draftKey)
  const pendingAdmissionKeys=pendingAdmissions.map(item=>item.key).join(',')
  const legacyAdmissionKey=draft.request?.awaitingAdmission ? draft.request.key : undefined
  const submitting=pending
  useEffect(()=>{
    if(!legacyAdmissionKey)return
    enqueuePendingAdmission(draftKey,{key:legacyAdmissionKey,text:draft.text,createdAt:Date.now()})
    finishDraftRequest(draftKey,legacyAdmissionKey)
  },[draftKey,legacyAdmissionKey,draft.text])
  useEffect(()=>{onAdmissionChange?.(pendingAdmissions.length>0)},[pendingAdmissions.length,onAdmissionChange])
  const retrySubmit=useEffectEvent(()=>void submit(undefined,true))
  useEffect(()=>{
    if(!onStatusNotice)return
    if(!error){onStatusNotice(null);return}
    onStatusNotice({
      message:error,
      unconfirmed,
      retry:unconfirmed?()=>retrySubmit():undefined,
      dismiss:()=>{setError('');setUnconfirmed(false)},
    })
  },[error,unconfirmed,onStatusNotice])
  const mounted=useRef(false)
  useLayoutEffect(()=>{
    mounted.current=true
    return()=>{mounted.current=false}
  },[])
  const completeAdmission=useEffectEvent((path:string,receipt:Receipt)=>onReceipt(path,receipt))
  useEffect(()=>{
    if(!pendingAdmissionKeys)return
    const keys=pendingAdmissionKeys.split(',')
    let disposed=false
    let timer:number
    function fail(requestKey:string,message:string) {
      setError(message)
      removePendingAdmission(draftKey,requestKey)
    }
    async function check() {
      if(!document.hidden) {
        for(const requestKey of keys) {
          if(disposed)return
          let receipt:Receipt|undefined
          try {
            receipt=await api<Receipt>('/admissions/'+encodeURIComponent(requestKey))
          } catch(e) {
            if(!disposed && e instanceof APIError && e.code==='admission_not_found') {
              fail(requestKey,errorMessage(e))
              continue
            }
          }
          if(disposed)return
          if(receipt) {
            const path=receiptPath(receipt,assign)
            if(path) {
              removePendingAdmission(draftKey,requestKey)
              try {await completeAdmission(path,receipt)}
              catch(e) {if(!disposed)setError(errorMessage(e))}
              if(!path.startsWith('/tasks/'))return
              continue
            }
            if(receipt.state==='failed'||receipt.state==='quarantined'||receipt.state==='applied') {
              fail(requestKey,receipt.state==='applied'?'已处理，但未找到对应的事项或对话':receipt.reason||'处理失败，请重试')
            }
          }
        }
      }
      if(!disposed)timer=window.setTimeout(check,2000)
    }
    void check()
    return()=>{disposed=true;clearTimeout(timer)}
  },[pendingAdmissionKeys,assign,draftKey])
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
    if (locked.current || captureLock.current || disabled) return
    if(plan!==undefined&&!validPlan){setError('请输入方案和版本，例如 pod7@v4。');planInput.current?.focus();return}
    if (!outgoingText.trim()) return
    if(plan!==undefined&&selectedStrategy==='deterministic_only'){setError('切换到按角色路由后再发送基准指令。');return}
    if(plan!==undefined&&!hasBody&&(materials.length||draft.taskReference)){
      setError('请填写这次基准的要求，或移除材料和任务引用后再发送固定指令。');return
    }
    if(reclassify&&!unconfirmed)return
    locked.current=true
    setPending(true);setError('');setUnconfirmed(false)
    if(reclassify)releaseDraftRequest(draftKey,draft.request?.key)
    const delivery:DeliveryUpdate=draft.taskReference?.passive&&!assign&&!task?{task_ref:{task_id:draft.taskReference.taskID}}:{}
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
    let revisionRequest=false
    const snapshot={
      text:value,
      materials:[...materials],
      taskReference:draft.taskReference,
      benchmarkPlan:plan,
      outgoingText,
    }
    let optimisticKey:string|undefined
    const executeSend=async(sendKey:string,sendScope:string,sendDelivery:DeliveryUpdate,sendStrategy?:'role_routing'|'deterministic_only',uiKey=sendKey)=>{
      const receipt=await onSend(snapshot.outgoingText,sendKey,sendScope,snapshot.materials.map(item=>item.id),sendDelivery,sendStrategy)
      if(onAssistantReceipt && isAssistantItem(receipt)) {
        if(mounted.current){onAssistantReceipt(receipt,snapshot.outgoingText);finishDraftRequest(draftKey,sendKey);onOptimisticSettle?.(uiKey)}
        else releaseDraftRequest(draftKey,sendKey)
        return
      }
      const path=receiptPath(receipt,assign)
      if(!path && receipt.model_pending) {
        enqueuePendingAdmission(draftKey,{key:sendKey,text:snapshot.outgoingText,createdAt:Date.now()})
        finishDraftRequest(draftKey,sendKey)
        onOptimisticSettle?.(uiKey)
        return
      }
      if(!path)throw new Error('未收到有效回执。草稿已保留，可以重试。')
      finishDraftRequest(draftKey,sendKey)
      if(mounted.current)await onReceipt(path,receipt)
      onOptimisticSettle?.(uiKey)
    }
    try {
      if((!draft.request || reclassify) && (assign || task)) {
        const reads=readDrafts.map(readRequest)
        if(reads.length===1) delivery.mcp_read=reads[0]
        else if(reads.length>1) delivery.mcp_reads=reads
      }
      const prepared=draft.request?.delivery??(prepareDelivery?await prepareDelivery(snapshot.outgoingText,snapshot.materials.map(item=>item.id),delivery,selectedStrategy):delivery)
      const request=beginDraftRequest(draftKey,scope,prepared,selectedStrategy)
      requestID=request.key
      revisionRequest=Boolean(request.delivery.benchmark_revision_target)
      if(onOptimisticStart) {
        optimisticKey=request.key
        onOptimisticStart({
          key:request.key,
          text:snapshot.outgoingText,
          createdAt:Date.now(),
          materials:snapshot.materials,
          taskRef:snapshot.taskReference?{taskID:snapshot.taskReference.taskID,title:snapshot.taskReference.title}:undefined,
        })
        changeComposerDraft(draftKey,()=>({text:'',materials:[],taskReference:undefined,benchmarkPlan:undefined}))
      }
      await executeSend(request.key,request.scope,request.delivery,request.strategy)
    }
    catch(e){
      if(e instanceof APIError&&requestID&&(e.code==='classification_failed'||
        (plan!==undefined&&delivery.task_ref&&e.code==='task_revision')||(e.status===409&&revisionRequest)))releaseDraftRequest(draftKey,requestID)
      const isUnconfirmed=e instanceof APIError&&e.code==='classification_unconfirmed'
      const msg=revisionRequest&&e instanceof APIError&&e.status===409?'任务状态或方案版本已变化，草稿仍在。请重新选择任务后再发送。':snapshot.taskReference?.passive&&e instanceof APIError&&e.status===404?'此任务引用已不可用。草稿仍在，请移除引用后再发送。':e instanceof TypeError?'未收到提交回执。草稿已保留，可以重试。':errorMessage(e)
      if(optimisticKey && onOptimisticFail) {
        const currentKey=optimisticKey
        const reqScope=scope
        const reqStrategy=selectedStrategy
        const reqDelivery=delivery
        const restoreFn=()=>{
          onOptimisticSettle?.(currentKey)
          changeComposerDraft(draftKey,()=>({text:snapshot.text,materials:snapshot.materials,taskReference:snapshot.taskReference,benchmarkPlan:snapshot.benchmarkPlan}))
          textarea.current?.focus()
        }
        const retryFn=(unconfirmedRetry:boolean)=>{
          if(locked.current)return
          locked.current=true;setPending(true)
          const retryKey=unconfirmedRetry?crypto.randomUUID():currentKey
          onOptimisticStart?.({
            key:currentKey,
            text:snapshot.outgoingText,
            createdAt:Date.now(),
            materials:snapshot.materials,
            taskRef:snapshot.taskReference?{taskID:snapshot.taskReference.taskID,title:snapshot.taskReference.title}:undefined,
          })
          void executeSend(retryKey,reqScope,reqDelivery,reqStrategy,currentKey).catch(err=>{
            const retryUnconfirmed=err instanceof APIError&&err.code==='classification_unconfirmed'
            const retryMsg=err instanceof TypeError?'未收到提交回执。草稿已保留，可以重试。':errorMessage(err)
            onOptimisticFail(currentKey,{message:retryMsg,unconfirmed:retryUnconfirmed,retry:()=>retryFn(retryUnconfirmed),restore:restoreFn})
          }).finally(()=>{locked.current=false;setPending(false)})
        }
        onOptimisticFail(currentKey,{
          message:msg,
          unconfirmed:isUnconfirmed,
          retry:()=>retryFn(isUnconfirmed),
          restore:restoreFn,
        })
      } else {
        setUnconfirmed(isUnconfirmed)
        setError(msg)
      }
    }
    finally{locked.current=false;setPending(false)}
  }
  async function removeMaterial(id:string) {
    const item=materials.find(material=>material.id===id)
    if(!item||captureLock.current||locked.current)return
    if((item.upload||item.path.startsWith('images/'))&&!hasOtherDraftMaterial(draftKey,id)) {
      captureLock.current=true
      setCapturing(true);setError('')
      try {await api('/materials/'+encodeURIComponent(id),undefined,'DELETE')}
      catch(e){
        // A previously bound copy is retained by the server; it can still be removed from this draft.
        if(!(e instanceof APIError&&(e.status===404||(e.status===409&&e.code==='conflict')))){setError(errorMessage(e));return}
      }
      finally{captureLock.current=false;setCapturing(false)}
    }
    changeComposerDraft(draftKey,previous=>({materials:previous.materials.filter(material=>material.id!==id)}))
  }
  async function uploadMaterial(file:File) {
    await capture(()=>{
      if(file.size>10*1024*1024)throw new Error('文件超过 10 MiB')
      const form=new FormData()
      form.set('file',file)
      return api<Material>('/attachments',form)
    })
  }
  async function uploadDropped(files:File[]) {
    if(disabled||submitting||capturing)return
    const room=Math.max(0,4-materials.length)
    for(const file of files.slice(0,room))await uploadMaterial(file)
    if(files.length>room)setError('每次最多选择四份材料')
  }
  const commandChip=plan===undefined?undefined:<div className="composer-command">
    <span>/benchmark</span>
    <input ref={planInput} aria-label="基准方案和版本" placeholder="方案@版本" style={{width:`${plan?Math.max(7,Math.min(20,plan.length+1)):10}ch`}} maxLength={160} value={plan} readOnly={submitting}
      onChange={event=>edit({benchmarkPlan:event.currentTarget.value})} onKeyDown={event=>{
        if(event.key==='Enter'&&!event.nativeEvent.isComposing&&event.nativeEvent.keyCode!==229){event.preventDefault();textarea.current?.focus()}
      }}/>
    <button type="button" className="message-content-action" aria-label="移除基准指令" disabled={submitting||disabled}
      onClick={()=>{edit({benchmarkPlan:undefined});textarea.current?.focus()}}><X size={12} aria-hidden="true"/></button>
  </div>
  return <>
  {!onStatusNotice && error && <FeedbackCallout tone="error" compact role="alert" message={error}
    actions={unconfirmed ? <button type="button" disabled={disabled||submitting||capturing} onClick={()=>void submit(undefined,true)}>重新判断并发送</button> : undefined}
    onDismiss={()=>{setError('');setUnconfirmed(false)}}/>}
  {!onStatusNotice && pendingAdmissions.length>0 && <FeedbackCallout tone="warning" compact role="status" message="消息已接收，配置模型后将自动继续"
    actions={openModelSettings ? <button type="button" onClick={openModelSettings}>打开模型设置</button> : undefined}/>}
  <form className="composer" hidden={hidden} onSubmit={submit}
    onDragOver={event=>{if(Array.from(event.dataTransfer.types).includes('Files'))event.preventDefault()}}
    onDrop={event=>{if(event.dataTransfer.files.length){event.preventDefault();void uploadDropped(Array.from(event.dataTransfer.files))}}}
    onPaste={event=>{if(event.clipboardData.files.length){event.preventDefault();void uploadDropped(Array.from(event.clipboardData.files))}}}>
    {suggestions.list}
    <MessageContent reference={reference} command={commandChip} body={<textarea aria-label={placeholder} placeholder={placeholder} value={value} ref={node=>{textarea.current=node;if(typeof inputRef==='function')inputRef(node);else if(inputRef)inputRef.current=node}} rows={2} maxLength={16000}
      spellCheck={false} autoCorrect="off" autoCapitalize="off"
      aria-autocomplete="list" aria-controls={suggestions.controls} aria-activedescendant={suggestions.activeDescendant}
      onChange={event=>{
        const text=event.currentTarget.value
        const typed=onAssistantReceipt&&plan===undefined&&!(event.nativeEvent as InputEvent).isComposing?text.match(/^\/benchmark[ \t]+([^\s@\\/:]+@v?[1-9][0-9]*)(?:\r?\n|[ \t]+)([\s\S]*)$/):null
        if(typed)edit({text:typed[2],benchmarkPlan:typed[1]})
        else edit({text})
        suggestions.updateCursor(event.currentTarget,true)
      }} onClick={e=>suggestions.updateCursor(e.currentTarget,true)} onSelect={e=>suggestions.updateCursor(e.currentTarget)} readOnly={submitting}
      onCompositionEnd={e=>{compositionEnd.current=e.timeStamp}}
      onKeyDown={e=>{
        const composing=e.nativeEvent.isComposing||e.nativeEvent.keyCode===229||(compositionEnd.current!==undefined&&Math.abs(e.timeStamp-compositionEnd.current)<50)
        if(composing)return
        if(suggestions.keyDown(e))return
        if(e.key==='Enter'&&!e.shiftKey&&!e.altKey&&((e.ctrlKey||e.metaKey)||document.documentElement.dataset.sendShortcut!=='mod_enter')){e.preventDefault();e.currentTarget.form?.requestSubmit()}
      }}/>}
      attachments={materials.length>0&&<MessageAttachments items={materials} previewItems={previewMaterials} draft={{
        disabled:submitting||capturing||disabled,
        choosePart:(id,part)=>void capture(()=>api<Material>('/attachments/uploads/'+encodeURIComponent(id)+'/part',{part})),
        remove:id=>void removeMaterial(id),
      }}/>}/>
    <div className="composer-tools">
      <MaterialPicker allowImages={allowImages} limitReached={materials.length>=4} disabled={submitting||capturing||disabled}
        replacement={task?{enabled:Boolean(draft.replaceMaterials),change:enabled=>edit({replaceMaterials:enabled})}:undefined}
        choose={path=>capture(()=>api<Material>('/attachments/workspace',{path}))}
        upload={uploadMaterial}/>
      {(assign || task) && <button type="button" className="text-button criteria-toggle" aria-expanded={Boolean(criteriaDraft)} disabled={submitting||disabled}
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
      {(assign || task) && <TaskReadPicker drafts={readDrafts} disabled={submitting||disabled} change={reads=>edit({read:undefined,reads})}/>}
      {capturing && <span className="composer-capturing-inline" role="status"><LoaderCircle size={13} className="spin" aria-hidden="true"/>正在准备材料…</span>}
    </div>
    {criteriaDraft && <div className="criteria-editor">
      <label>验收条件<textarea aria-label="验收条件" autoFocus={focusCriteria} maxLength={1000} rows={2} placeholder="例如：列出每项决策的负责人和截止日期" value={criteriaDraft.text} readOnly={submitting}
        onChange={event=>edit({criteria:{...criteriaDraft,text:event.target.value}})}/></label>
      <label>交付文件（可选）<span className="filename-input"><span>outputs/</span><input aria-label="交付文件名" maxLength={100} placeholder="会议纪要.md" value={criteriaDraft.filename} readOnly={submitting}
        onChange={event=>edit({criteria:{...criteriaDraft,filename:event.target.value}})}/></span></label>
    </div>}
    <div className="composer-bar">
      {assign ? <label className="scope-input"><FolderOpen size={16}/><input maxLength={40} aria-label="工作范围" placeholder="工作范围（可选）" value={scope} readOnly={submitting} onChange={e=>edit({scope:e.target.value})}/></label> : <div className="composer-secondary"><span className="muted">{disabled?(disabledReason??'暂时无法发送'):''}</span>
        {openIsolated&&isolatedRecovery&&<button type="button" className="text-button" disabled={submitting||capturing} onClick={()=>openIsolated()}>
          {isolatedRecovery?'核对单条回复请求':'单独问一个模型'}</button>}</div>}
      {busyAction??(onAssistantReceipt&&openIsolated&&<ReplyStrategy options={options} error={optionsError} strategy={selectedStrategy} onStrategy={setStrategy} onModel={model=>openIsolated(model,value)} openSettings={openModelSettings} disabled={disabled||submitting||capturing||Boolean(draft.request)} benchmarkCommand={plan!==undefined} benchmarkAdvisory={plan!==undefined&&hasBody}/>)}
      {!busyAction&&<button type="submit" className={assign?'primary':'send'} aria-label={label} title={sendShortcut==='mod_enter'?'Ctrl/⌘+Enter 发送 · Shift+Enter 换行':'Enter 发送 · Shift+Enter 换行'} disabled={disabled||submitting||capturing||(!outgoingText.trim()&&plan===undefined)}>{pending ? <LoaderCircle size={16} className="spin"/> : assign ? <>{label}<ArrowUp size={16}/></> : <ArrowUp size={18}/>}</button>}
    </div>
  </form>
  </>
}

function resolveLiveText(live: string, fallback: string | undefined, reset: boolean): string {
  if (!reset && fallback && fallback.length > live.length) return fallback
  return live || (reset ? '' : fallback || '')
}

function useDetail(path: string | undefined, onChange?: () => void) {
  const [data,setData]=useState<Detail>()
  const [live,setLive]=useState('')
  const [reasoning,setReasoning]=useState('')
  const [liveReset,setLiveReset]=useState(false)
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
    setLive('');setReasoning('');setLiveReset(false);setReconnecting(false)
    const signal={cancelled:false}
    let pendingLive='', pendingReasoning='', resetPending=false, rafID:number|null=null
    const flush=()=>{
      rafID=null
      if(resetPending){
        resetPending=false
        setLive(pendingLive)
        setReasoning(pendingReasoning)
        pendingLive=''
        pendingReasoning=''
        return
      }
      if(pendingLive){
        const chunk=pendingLive
        pendingLive=''
        setLive(previous=>previous+chunk)
      }
      if(pendingReasoning){
        const chunk=pendingReasoning
        pendingReasoning=''
        setReasoning(previous=>previous+chunk)
      }
    }
    const scheduleFlush=()=>{if(rafID===null)rafID=window.requestAnimationFrame(flush)}
    const stream=new EventSource('/api/v1/runs/'+runID+'/events')
    stream.addEventListener('message.delta', event => {
      const chunk=JSON.parse((event as MessageEvent).data) as {delta:string;execution_generation?:number}
      if((chunk.execution_generation??0)===(generation??0)){pendingLive+=chunk.delta;scheduleFlush()}
    })
    stream.addEventListener('reasoning.delta', event => {
      const chunk=JSON.parse((event as MessageEvent).data) as {delta:string;execution_generation?:number}
      if((chunk.execution_generation??0)===(generation??0)){pendingReasoning+=chunk.delta;scheduleFlush()}
    })
    stream.addEventListener('message.reset',event=>{
      const reset=JSON.parse((event as MessageEvent).data) as {execution_generation?:number}
      if((reset.execution_generation??0)===(generation??0)){
        pendingLive='';pendingReasoning='';resetPending=true
        if(rafID!==null){window.cancelAnimationFrame(rafID);rafID=null}
        setLiveReset(true);setLive('');setReasoning('')
        void reload(signal)
      }
    })
    stream.addEventListener('run.state_changed',()=>void reload(signal))
    stream.addEventListener('tool.read_changed',()=>void reload(signal))
    stream.addEventListener('done',()=>{
      if(rafID!==null){window.cancelAnimationFrame(rafID);flush()}
      stream.close();void reload(signal);notify.current?.()
    })
    stream.onopen=()=>setReconnecting(false)
    stream.onerror=()=>{setReconnecting(true);void reload(signal)}
    const poll=window.setInterval(()=>void reload(signal),2500)
    return ()=>{
      signal.cancelled=true
      if(rafID!==null)window.cancelAnimationFrame(rafID)
      stream.close();clearInterval(poll)
    }
  },[runID,generation,active,reload])
  useEffect(()=>{
    if (!waitingForOperation) return
    const signal={cancelled:false}
    const poll=window.setInterval(()=>void reload(signal),2000)
    return ()=>{signal.cancelled=true;clearInterval(poll)}
  },[waitingForOperation,reload])
  return {data,live,reasoning,liveReset,error,reload,reconnecting:active&&reconnecting}
}

function TaskDetail({taskID,compact,fromChat,sourceConversationID,sourceConversationBusy,onReference,onChange,navigate,historyBack,selectedRun,selectRun,headingRef,contextWindow}:{taskID:string;compact?:boolean;fromChat?:boolean;sourceConversationID?:string;sourceConversationBusy?:boolean;onReference?:(task:TaskReference)=>void;onChange:()=>void;navigate:(path:string)=>void;historyBack?:string;selectedRun?:string;selectRun?:(runID:string)=>void;headingRef?:React.Ref<HTMLHeadingElement>;contextWindow?:number}) {
  const {data,live,liveReset,error,reload,reconnecting}=useDetail('/tasks/'+taskID,onChange)
  const rememberMessage=useCallback((id:number)=>navigate('/memory/new/'+id),[navigate])
  const {progress:benchmark,unavailable:nonBenchmark,checking:checkingBenchmark,error:benchmarkError,refresh:refreshBenchmark}=useBenchmarkProgress(taskID)
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
      await Promise.all([reload(),refreshBenchmark()]);onChange()
    } catch(e) {setActionError(errorMessage(e));await Promise.all([reload(),refreshBenchmark()])}
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
  const status=benchmark ? benchmarkReadiness(benchmark) : task.status_reason==='stop_requested'?'停止请求处理中':task.status_reason==='pause_requested'?'暂停请求处理中':writing ? ({pending:'待确认写入',dispatched:'正在保存成果',unknown:'写入待核对'}[data.publication?.status??'']??task.reason) : {
    active:task.status_reason==='waiting_dependencies'?'等待依赖成果':task.status_reason==='queued'?'等待执行':'正在处理',waiting_user:question?'等你补充':needsFile?'待保存成果':'等你验收',completed:'已完成',
    interrupted:task.status_reason==='paused'?'已暂停':task.requirements_changed?'待按新要求执行':'执行中断',cancelled:'已停止',
  }[task.status]
  const controlling = task.status_reason === 'pause_requested' || task.status_reason === 'stop_requested'
  const archived=data.conversation.archived
  const readUnconfirmed=Boolean(data.unknown_read_run_id)
  const composerBlocked=!nonBenchmark||busy||writing||archived||readUnconfirmed||task.status==='cancelled'
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
  const canStopBenchmark=benchmark && !benchmarkError && !benchmark.stop_requested && !['succeeded','failed','cancelled','finalizing'].includes(benchmark.state)
  const canStopOrdinary=nonBenchmark && ['active','waiting_user','interrupted'].includes(task.status)
  const actions = <div className="task-actions" aria-label="任务操作">
    {onReference&&(task.conversation_id===sourceConversationID||Boolean(benchmark&&!benchmarkError))&&<button type="button" className="text-button" onClick={()=>onReference({
      taskID:task.id,title:task.title,passive:task.conversation_id===sourceConversationID,planRevision:benchmarkError?undefined:benchmark?.plan_revision,
    })}>{task.conversation_id===sourceConversationID?'在对话中引用':'选择基准控制目标'}</button>}
    {onReference&&benchmark&&!benchmarkError&&<button type="button" className="text-button" onClick={()=>onReference({
      taskID:task.id,title:task.title,passive:false,purpose:'revision',planRevision:benchmark.plan_revision,
    })}>修订方案</button>}
    {nonBenchmark&&(!compact||fromChat)&&!editBlocked&&<button type="button" className="text-button" onClick={()=>setEditing(!editing)}>编辑事项</button>}
    {nonBenchmark&&task.status==='active' && <button type="button" className="text-button" disabled={pending||controlling} onClick={()=>void control('pause')}>暂停</button>}
    {(canStopBenchmark||canStopOrdinary) && <button type="button" className="text-button" disabled={pending||controlling||writing||readUnconfirmed} onClick={()=>void control('cancel')}>请求停止</button>}
    {nonBenchmark&&(task.status==='cancelled'||(task.status==='interrupted'&&(canResume||canRetry))) &&
      <button type="button" className="text-button" disabled={pending||readUnconfirmed} onClick={()=>void control('resume',canRetry)}>{task.status==='cancelled'?'重新开启':task.requirements_changed?'按新要求执行':canRetry?'重新执行':'继续工作'}</button>}
  </div>
  const taskStatus=<p className="status-line"><span className={'status-dot '+task.status} aria-hidden="true"/>{status}</p>
  const benchmarkSection=benchmark
    ? <><BenchmarkExecution key={taskID} progress={benchmark} stale={Boolean(benchmarkError)} conversationID={sourceConversationID} conversationBusy={sourceConversationBusy} refresh={refreshBenchmark} onChange={()=>{void reload();onChange()}} compact={compact}/>
      {benchmarkError&&<p className="error" role="alert">未能核对最新基准进度：{benchmarkError}</p>}</>
    : benchmarkError
      ? <p className="error" role="alert">{benchmarkError} <button type="button" className="text-button" onClick={()=>void refreshBenchmark()}>重新核对</button></p>
      : checkingBenchmark?<p className="muted">正在核对执行状态…</p>:null
  const artifactActions=artifact && !busy && <div className="artifact-card"><span className="artifact-icon"><FileText size={20}/></span><div><strong>{artifact.name}</strong><small>{task.status==='interrupted'||requestedInput?'先前成果 · ':''}{time(artifact.created_at)}</small></div><button className="text-button" onClick={()=>setPreviewID(preview?'':artifact.id)}>{preview?'收起':'预览'}</button><a className="icon-button" aria-label="下载成果" href={'/api/v1/artifacts/'+artifact.id+'/content'}><ArrowDownToLine size={17}/></a></div>
  const previous=(data.artifact_versions??[]).filter(version=>busy||task.requirements_changed||version.id!==artifact?.id)
  const history=previous.length>0 && <details className="artifact-history">
    <summary><History size={17}/>历史成果</summary>
    {previous.map(version=><button type="button" key={version.id} className="history-row" onClick={()=>navigate(artifactRoute(version.id,task.scope_label,task.id))}>
      <FileText size={16}/><span>{versionTime.format(version.created_at)}{version.accepted_at?' · 已验收':''}</span><ChevronRight size={16}/>
    </button>)}
    <button type="button" className="text-button" onClick={()=>navigate(artifactRoute(undefined,task.scope_label,task.id))}>查看该事项全部成果</button>
  </details>
  const liveText=busy?resolveLiveText(live,data.run?.response,liveReset):''
  const output=<>{reconnecting && <p className="muted">正在重新连接…</p>}{busy && liveText && <FormattedText text={liveText} framed streaming/>}{artifactActions}{preview && artifact && !busy && <FormattedText text={artifact.content || ''} framed/>}</>
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
  if (compact) return <div className="detail-content task-detail-content">
    <header className="task-detail-header"><div><h2>{task.title}</h2>{task.scope_label && <p className="muted">{task.scope_label}</p>}{taskStatus}</div>{actions}</header>
    {editing&&<TaskEditor task={task} blocked={editBlocked} reload={reload} onSaved={async()=>{await reload();onChange()}} onClose={()=>setEditing(false)}/> }
    {task.goal!==task.title && <p className="goal">{task.goal}</p>}
    {(task.acceptance_criteria||task.required_filename) && <><hr/><h3>验收条件</h3>{task.acceptance_criteria && <p className="criteria-text">{task.acceptance_criteria}</p>}{task.required_filename && <p className="criteria-text"><code>outputs/{task.required_filename}</code></p>}</>}
    <hr/><h3>当前进展</h3>{inputRequest||<p className="progress-line">{busy?<LoaderCircle size={17} className="spin" aria-hidden="true"/>:<ProgressIcon size={17} aria-hidden="true"/>} {task.reason}</p>}
    {readEvidence}
    {readConfirmation}
    {benchmarkSection}
    {output}{action}{(actionError||error) && <p className="error" role="alert">{actionError||error}</p>}
    <button className="detail-link" onClick={()=>navigate('/tasks/'+taskID)}>{question?'补充信息':'查看完整工作与交流'}<ChevronRight size={17}/></button><p className="updated">更新于 {time(task.updated_at)}</p></div>
  return <>
    <main className="main work-detail"><button className="text-button back" onClick={()=>navigate(historyBack??'/overview')}><ArrowLeft size={16}/>{historyBack?'事项历史':'工作总览'}</button>
      <header className="page-heading task-detail-header"><div><h1 tabIndex={-1} ref={headingRef}>{task.title}</h1>{task.scope_label && <p>{task.scope_label}</p>}{taskStatus}</div>{actions}</header>
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
      {benchmarkSection}
      <section className="discussion"><header className="discussion-heading"><h2>相关交流</h2>
        <button className="text-button" disabled={pending||(!archived&&(busy||writing||readUnconfirmed))} onClick={()=>void changeDiscussion()}>
          {archived?<ArchiveRestore size={16}/>:<Archive size={16}/>} {archived?'恢复交流':'归档交流'}
        </button></header>
        {archived&&!sourceMessage?<p className="muted">交流已归档，可恢复原有消息。</p>:<MessageHistory conversationID={task.conversation_id} messages={discussion}
          omitRunID={question?data.run?.id:undefined}
          live={busy?liveText:undefined} liveRunID={busy?data.run?.id:undefined} remember={rememberMessage}/>}
      </section>
      <div className="mobile-outcome">{artifactActions}{preview&&artifact&&!busy&&<FormattedText text={artifact.content || ''} framed/>}{action}{history}</div>
      {(actionError||error) && <p className="error" role="alert">{actionError||error}</p>}
      {nonBenchmark&&<Composer draftKey={'task:'+task.id} placeholder={question?'补充助手需要的信息…':'补充这项工作的要求…'} label={question?'发送补充信息':'发送补充要求'} task={task} inputRef={followUp} disabled={composerBlocked}
        disabledReason={archived?'恢复交流后可发送':readUnconfirmed?'请先确认读取结果':writing?'写入待处理，草稿可继续编辑':busy?'本轮处理中，草稿可继续编辑':'事项已停止'}
        onSend={(text,key,_scope,attachments,delivery)=>api('/conversations/'+task.conversation_id+'/messages',{text,client_message_id:key,attachment_ids:attachments,...delivery})}
        onReceipt={async()=>{setAcceptanceError('');await reload();onChange()}}/>}
    </main>
    <aside className="detail-pane full-outcome"><div className="detail-content"><div className="status-line"><span className={'status-dot '+task.status}/>{status}</div><h2>工作成果</h2>{output}{!artifact&&!busy&&<p className="muted">{requestedInput?'本轮暂无成果。':task.status==='interrupted'||task.status==='cancelled'?'本轮未生成可验收的成果。':'成果将在执行结束后显示。'}</p>}{action}{history}</div></aside>
  </>
}

type ModelNoticeInfo = {
  kind: 'no_models' | 'unselected' | 'broken' | 'unknown' | 'resolved'
  title: string
  message: string
  primaryAction?: { label: string; onClick: () => void }
  secondaryAction?: { label: string; onClick: () => void }
  onDismiss?: () => void
}

function Chat({id,navigate,modelNotice,assistantManage,modelConcurrency,account,activity,sidebarTask,onSidebarTaskOpened,toolbarActions,onToolbarView}:{
  id?:string;navigate:(path:string)=>void;modelNotice?:ModelNoticeInfo;assistantManage:boolean;modelConcurrency?:number;account?:Account;activity?:React.ReactNode;
  sidebarTask?:{id:string;trigger:HTMLButtonElement};onSidebarTaskOpened:()=>void;toolbarActions:HTMLDivElement|null;onToolbarView:(view?:ChatToolbarView)=>void
}) {
  const {data,live,reasoning,liveReset,error,reload}=useDetail(id?'/conversations/'+id:undefined)
  const rememberMessage=useCallback((messageID:number)=>navigate('/memory/new/'+messageID),[navigate])
  const [saving,setSaving]=useState(false)
  const [actionError,setActionError]=useState('')
  const [composerNotice,setComposerNotice]=useState<{message:string;unconfirmed?:boolean;retry?:()=>void;dismiss:()=>void}|null>(null)
  const [composerAdmitting,setComposerAdmitting]=useState(false)
  const [optimisticMessages,setOptimisticMessages]=useState<(OptimisticStartItem & {status:'sending'|'routing'|'failed';error?:string;unconfirmed?:boolean;retry?:()=>void;restore?:()=>void})[]>([])
  useEffect(()=>{
    const pending=optimisticMessages.filter(item=>item.status==='sending')
    if(!pending.length)return
    const timer=window.setTimeout(()=>{
      setOptimisticMessages(prev=>prev.map(item=>item.status==='sending'?{...item,status:'routing'}:item))
    },400)
    return()=>clearTimeout(timer)
  },[optimisticMessages])
  const [stopping,setStopping]=useState<string>()
  const [isolatedOpen,setIsolatedOpen]=useState(false)
  const [isolatedModel,setIsolatedModel]=useState<ReplyModel>()
  const [isolatedText,setIsolatedText]=useState<string>()
  const {isolatedRequest}=useComposerDraft(isolatedDraftKey(id||'new'))
  const returnToComposer=useRef(false)
  const [configEntry,setConfigEntry]=useState<string>()
  const closeConfig=()=>{setConfigEntry(undefined);chatInput.current?.focus()}
  const [selectedTask,setSelectedTask]=useState<string>()
  const ordinaryDraftKey='chat:'+(id||'new')
  const {taskReference,materials:draftMaterials}=useComposerDraft(ordinaryDraftKey)
  const pendingAdmissions=usePendingAdmissions(ordinaryDraftKey)
  const chatInput=useRef<HTMLTextAreaElement>(null)
  const [latestReceipt,setLatestReceipt]=useState<TaskLink>()
  const [reopenedRevision,setReopenedRevision]=useState<{anchorRunID:string;source:Run;preview?:RevisionPreview;stale?:boolean;applied?:number}>()
  const [assistantItems,setAssistantItems]=useState<{id:string;cards:ReturnType<typeof assistantReplyCards>;text:string;createdAt:number;afterMessageID:number}[]>([])
  const latestUploads=data?.messages.slice().reverse().find(message=>message.role==='user')?.materials??[]
  const latestExchange=assistantItems.at(-1)
  const latestCards=latestExchange?.cards??[]
  const latestTools=latestCards.flatMap(card=>!Array.isArray(card)&&card.state==='completed'?[card.tool]:[])
  const toolbarView=latestTools.some(tool=>['get_cluster_trends','get_token_usage_stats','get_incident_workflow','get_incident_step_logs'].includes(tool))?'trends':
    latestTools.includes('get_cluster_inventory')?'operations':latestUploads.length?'materials':undefined
  useLayoutEffect(()=>{
    onToolbarView(toolbarView)
    return()=>onToolbarView(undefined)
  },[onToolbarView,toolbarView])
  const eventTasks=useConversationTasks(id)
  const detailDialog=useRef<HTMLDialogElement>(null)
  const taskTrigger=useRef<HTMLButtonElement|null>(null)
  const detailClose=useRef<HTMLButtonElement>(null)
  useLayoutEffect(()=>{
    if(!sidebarTask)return
    taskTrigger.current=sidebarTask.trigger
    setSelectedTask(sidebarTask.id)
    onSidebarTaskOpened()
  },[sidebarTask,onSidebarTaskOpened])
  useLayoutEffect(()=>{
    const element=detailDialog.current
    if(!selectedTask||!element)return
    const narrow=matchMedia('(max-width: 820px)')
    function openDrawer(){
      if(!element)return
      if(element.open)element.close()
      if(narrow.matches)element.showModal()
      else element.show()
      detailClose.current?.focus({preventScroll:true})
    }
    openDrawer()
    narrow.addEventListener('change',openDrawer)
    return()=>narrow.removeEventListener('change',openDrawer)
  },[selectedTask])
  useLayoutEffect(()=>{
    if(!isolatedOpen&&returnToComposer.current){returnToComposer.current=false;chatInput.current?.focus()}
  },[isolatedOpen])
  function closeIsolated(){returnToComposer.current=true;setIsolatedOpen(false)}
  function closeTaskDetail(input=false) {
    detailDialog.current?.close()
    setSelectedTask(undefined)
    if(input)chatInput.current?.focus()
    else (taskTrigger.current?.getClientRects().length?taskTrigger.current:chatInput.current)?.focus({preventScroll:true})
  }
  function referenceTask(reference:TaskReference) {
    if(JSON.stringify(taskReference)!==JSON.stringify(reference))changeComposerDraft(ordinaryDraftKey,()=>({taskReference:reference}))
    closeTaskDetail(true)
  }
  const resumeKey=useRef<{runID:string;key:string}|undefined>(undefined)
  const busy=activeRun(data?.run)
  const conversation=data?.conversation
  const run=data?.run
  const messageAnchor=useMessageAnchor()
  const chatLogRef=useRef<HTMLDivElement>(null)
  const chatContentRef=useRef<HTMLDivElement>(null)
  const pinnedToBottom=useRef(true)
  const [awayFromBottom,setAwayFromBottom]=useState(false)
  const [unseenCount,setUnseenCount]=useState(0)
  const [hasNewActivity,setHasNewActivity]=useState(false)
  const scrollSnapshot=useRef<{
    conversationID?:string
    messageCount:number
    lastMessageID:number
    liveLength:number
    reasoningLength:number
    pendingCount:number
    optimisticCount:number
    localCount:number
    noticeKey:string
  }>({conversationID:id,messageCount:0,lastMessageID:0,liveLength:0,reasoningLength:0,pendingCount:0,optimisticCount:0,localCount:0,noticeKey:''})

  const isNearBottom=(el:HTMLDivElement)=>el.scrollHeight-el.scrollTop-el.clientHeight<=48

  const scrollChatToBottom=useCallback((behavior:ScrollBehavior='auto')=>{
    const el=chatLogRef.current
    if(!el)return
    pinnedToBottom.current=true
    setAwayFromBottom(false)
    setUnseenCount(0)
    setHasNewActivity(false)
    if(behavior==='auto')el.scrollTop=el.scrollHeight
    else el.scrollTo({top:el.scrollHeight,behavior})
  },[])

  const handleChatScroll=useCallback(()=>{
    const el=chatLogRef.current
    if(!el)return
    if(isNearBottom(el)){
      pinnedToBottom.current=true
      setAwayFromBottom(false)
      setUnseenCount(0)
      setHasNewActivity(false)
    }else if(el.scrollHeight>el.clientHeight+24){
      pinnedToBottom.current=false
      setAwayFromBottom(true)
    }
  },[])

  useEffect(()=>{
    pinnedToBottom.current=true
    setAwayFromBottom(false)
    setUnseenCount(0)
    setHasNewActivity(false)
    scrollSnapshot.current={conversationID:id,messageCount:0,lastMessageID:0,liveLength:0,reasoningLength:0,pendingCount:0,optimisticCount:0,localCount:0,noticeKey:''}
  },[id])

  useEffect(()=>{
    const content=chatContentRef.current
    if(!content || typeof ResizeObserver==='undefined')return
    const observer=new ResizeObserver(()=>{
      if(pinnedToBottom.current && !messageAnchor && chatLogRef.current){
        chatLogRef.current.scrollTop=chatLogRef.current.scrollHeight
      }
    })
    observer.observe(content)
    return()=>observer.disconnect()
  },[messageAnchor])
  useEffect(()=>{
    setReopenedRevision(undefined)
    if(!id||!run||!run.response.startsWith('已生成基准任务 '))return
    let active=true
    const base='/conversations/'+encodeURIComponent(id)+'/benchmark-revision-preview/'+encodeURIComponent(run.id)
    void (async()=>{
      try {
        const meta=await api<{task_id:string;source_run_id?:string;expected_revision:number;applied_revision?:number}>(base+'?metadata=1')
        if(!active||!meta.task_id||meta.expected_revision<1)return
        const source:Run={...run,id:meta.source_run_id||run.id,benchmark_revision_suggestion:{task_id:meta.task_id,expected_revision:meta.expected_revision}}
        if(meta.applied_revision){setReopenedRevision({anchorRunID:run.id,source,applied:meta.applied_revision});return}
        try {
          const preview=await api<RevisionPreview>(base)
          if(active&&preview.task_id===meta.task_id&&preview.proposal.expected_revision===meta.expected_revision)
            setReopenedRevision({anchorRunID:run.id,source,preview})
        } catch(e) {
          if(active&&e instanceof APIError&&e.status===409)setReopenedRevision({anchorRunID:run.id,source,stale:true})
          else if(active)setActionError(errorMessage(e))
        }
      } catch(e) {if(active)setActionError(errorMessage(e))}
    })()
    return()=>{active=false}
  },[id,run?.id])
  const executionSuggestion=run?.benchmark_execution_suggestion
  const executionTarget=taskReference?.planRevision!==undefined ? taskReference :
    executionSuggestion ? {taskID:executionSuggestion.task_id,title:'任务 '+executionSuggestion.task_id,planRevision:executionSuggestion.plan_revision} : undefined
  const taskLinks=new Map(eventTasks.map(link=>[link.taskID,link]))
  if(run?.task_id && !taskLinks.has(run.task_id))taskLinks.set(run.task_id,{taskID:run.task_id,benchmark:false})
  if(latestReceipt&&!taskLinks.has(latestReceipt.taskID))taskLinks.set(latestReceipt.taskID,latestReceipt)
  async function prepareChatDelivery(text:string,attachments:string[],delivery:DeliveryUpdate,strategy:'role_routing'|'deterministic_only') {
    // An @ reference is explicitly read-only and must not become an action.
    if(!id||taskReference?.passive||!isBenchmarkRevisionInstruction(text))return delivery
    const selected=taskReference&&!taskReference.passive&&taskReference.planRevision!==undefined?taskReference:undefined
    const tokens=benchmarkTaskTokens(text)
    const lists=await Promise.all(tokens.map(token=>api<Task[]>('/tasks?q='+encodeURIComponent(token)+'&limit=200')))
    if(lists.some((items,index)=>items.length>=200&&tokens[index].length<32))
      throw new Error('任务候选过多，请使用完整任务 ID 或选择任务。草稿已保留。')
    const named=namedBenchmarkTasks(text,[...taskLinks.keys(),...lists.flatMap(items=>items.map(item=>item.id))])
    if(named.length>1||selected&&named.length===1&&named[0]!==selected.taskID)
      throw new Error('消息涉及多个任务，请先明确选择要修订的任务。草稿已保留。')
    if(!selected&&named.length===0){
      if(taskLinks.size||tokens.length)throw new Error('请在消息中写明可读取的任务 ID，或打开任务后选择“修订方案”。草稿已保留。')
      return delivery
    }
    if(attachments.length)throw new Error('修订预览暂不支持附带材料，请移除材料后重试。草稿已保留。')
    if(strategy==='deterministic_only')throw new Error('请切换到按角色路由后再请求修订预览。草稿已保留。')
    const taskID=selected?.taskID??named[0]
    const progress=await api<BenchmarkProgress>('/domain/benchmarks/'+encodeURIComponent(taskID))
    if(progress.task_id!==taskID||!Number.isInteger(progress.plan_revision)||progress.plan_revision<1)
      throw new Error('无法核对基准任务和方案版本。草稿已保留。')
    if(selected&&selected.planRevision!==progress.plan_revision)
      throw new Error('任务方案版本已变化，请重新选择“修订方案”。草稿已保留。')
    return {...delivery,benchmark_revision_target:{task_id:taskID,expected_revision:progress.plan_revision}}
  }
  const canResume=Boolean(conversation && !conversation.archived && run && !run.task_id && data?.read_checkpoint==='ready')
  const chatError=actionError||error||(run?.error_code==='stop'?'':run?.error)
  const cardRun=run?.benchmark_suggestion||run?.benchmark_revision_suggestion?run:reopenedRevision?.source
  const benchmarkReply=id&&cardRun&&!busy?{
    runID:reopenedRevision?.source===cardRun?reopenedRevision.anchorRunID:cardRun.id,
    content:<BenchmarkConversation key={cardRun.id} conversationID={id} run={cardRun} archived={Boolean(conversation?.archived)}
      initialRevision={reopenedRevision?.source===cardRun?reopenedRevision.preview:undefined} initialStale={reopenedRevision?.source===cardRun?reopenedRevision.stale:undefined}
      initialAppliedRevision={reopenedRevision?.source===cardRun?reopenedRevision.applied:undefined}
      alreadyAccepted={[...taskLinks.values()].some(link=>link.benchmark&&link.sourceRunID===cardRun.id)}
      onAccepted={taskID=>{setLatestReceipt({taskID,benchmark:true,sourceRunID:cardRun.id});void reload()}} onApplied={()=>{if(!cardRun.benchmark_revision_suggestion)void reload()}}
      onRestore={async(taskID,title)=>{
        const text=data?.messages.find(message=>message.run_id===cardRun.id&&message.role==='user')?.text
        if(!text)throw new Error('无法读取原修订要求，请从对话中复制后重新选择任务。')
        const latest=await api<BenchmarkProgress>('/domain/benchmarks/'+encodeURIComponent(taskID))
        if(latest.task_id!==taskID||latest.plan_revision<1)throw new Error('无法核对最新方案版本。')
        if(!restoreRevisionDraft(ordinaryDraftKey,text,{taskID,title,passive:false,purpose:'revision',planRevision:latest.plan_revision}))
          throw new Error('输入框已有其他草稿，请先处理该草稿，再恢复修订要求。')
        chatInput.current?.focus()
      }}/>,
  }:undefined
  useEffect(()=>{if(!busy||run?.id!==stopping)setStopping(undefined)},[busy,run?.id,stopping])
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
  const localExchanges=assistantItems.map(entry=>{
    const configIndex=entry.cards.findIndex(isConversationConfiguration)
    const showConfig=Boolean(conversation&&configEntry===entry.id)
    return {id:entry.id,afterMessageID:entry.afterMessageID,content:<div className="assistant-exchange">
      <div className="message message-user"><span><time dateTime={new Date(entry.createdAt).toISOString()}>{displayDate(entry.createdAt,{hour:'2-digit',minute:'2-digit'})}</time>{account?.display_name||'你'}
        {account&&<i className="message-user-avatar" aria-hidden="true">{account.avatar||Array.from(account.display_name||account.username)[0]}</i>}</span><div className="user-message">{entry.text}</div></div>
      {(entry.cards.some(card=>!isConversationConfiguration(card))||showConfig)&&<div className="message message-assistant"><span><img src={clusbotIcon} alt=""/><strong>ClusBot</strong><time dateTime={new Date(entry.createdAt).toISOString()}>{displayDate(entry.createdAt,{hour:'2-digit',minute:'2-digit'})}</time></span>
        <div className="assistant-reply-cards" data-multiple={entry.cards.length>1||undefined}>{entry.cards.map((card,index)=>{
          if(isConversationConfiguration(card))return showConfig&&conversation&&index===configIndex?
            <ConversationConfiguration key={index} id={conversation.id} panelID="conversation-configuration" open disabled={busy||saving||Boolean(conversation.archived)}
              archived={Boolean(conversation.archived)} onClose={closeConfig}/>:null
          if(!Array.isArray(card)&&card.state==='failed')return <p key={index} className="error" role="status">{card.message}</p>
          return <AssistantCard key={index} item={card} navigate={navigate} canManageModels={assistantManage} modelConcurrency={modelConcurrency}
            openTask={(taskID,trigger)=>{taskTrigger.current=trigger;setSelectedTask(taskID)}}/>
        })}</div>
      </div>}
    </div>}
  })
  const assistantModelNotice = (() => {
    if (composerAdmitting && modelNotice && modelNotice.kind !== 'resolved') {
      return (
        <AssistantNoticeMessage
          iconSrc={clusbotIcon}
          tone="warning"
          role="status"
          badge="等待模型就绪"
          title="消息已接收，配置模型后将自动继续"
          message={modelNotice.message}
          actions={
            <>
              {modelNotice.primaryAction && (
                <button type="button" className="primary" onClick={modelNotice.primaryAction.onClick}>
                  <Settings2 size={13} aria-hidden="true" />
                  {modelNotice.primaryAction.label}
                </button>
              )}
              {modelNotice.secondaryAction && (
                <button type="button" onClick={modelNotice.secondaryAction.onClick}>
                  {modelNotice.secondaryAction.label}
                </button>
              )}
            </>
          }
        />
      )
    }
    if (composerAdmitting) {
      return (
        <AssistantNoticeMessage
          iconSrc={clusbotIcon}
          tone="info"
          role="status"
          badge="已接收"
          title="消息已接收，正在等待处理"
          message="消息已进入受理队列，模型完成响应后将自动在此处显示回复。"
        />
      )
    }
    if (!modelNotice) return null
    const tone =
      modelNotice.kind === 'resolved'
        ? 'resolved'
        : modelNotice.kind === 'broken'
          ? 'error'
          : 'warning'
    const badge =
      modelNotice.kind === 'resolved'
        ? '已配置 · 原提示已过时'
        : modelNotice.kind === 'broken'
          ? '模型不可用'
          : modelNotice.kind === 'unknown'
            ? '状态未知'
            : '待配置'
    return (
      <AssistantNoticeMessage
        iconSrc={clusbotIcon}
        tone={tone}
        role={tone === 'error' ? 'alert' : 'status'}
        badge={badge}
        title={modelNotice.title}
        message={modelNotice.message}
        onDismiss={modelNotice.onDismiss}
        actions={
          (modelNotice.primaryAction || modelNotice.secondaryAction) && (
            <>
              {modelNotice.primaryAction && (
                <button type="button" className="primary" onClick={modelNotice.primaryAction.onClick}>
                  {modelNotice.kind !== 'unknown' && <Settings2 size={13} aria-hidden="true" />}
                  {modelNotice.primaryAction.label}
                </button>
              )}
              {modelNotice.secondaryAction && (
                <button type="button" onClick={modelNotice.secondaryAction.onClick}>
                  {modelNotice.secondaryAction.label}
                </button>
              )}
            </>
          )
        }
      />
    )
  })()
  const chatNoticeKey=[
    composerAdmitting?'admitting':'',
    modelNotice?.kind??'',
    composerNotice?.message??'',
    chatError??'',
    busy&&!run?.task_id?'streaming':'',
  ].join('|')
  const liveOutputText=busy?resolveLiveText(live,run?.response,liveReset):''

  useEffect(()=>{
    const messages=data?.messages??[]
    const prev=scrollSnapshot.current
    const last=messages.at(-1)
    const lastID=last?.id??0
    const nextSnapshot={
      conversationID:id,
      messageCount:messages.length,
      lastMessageID:lastID,
      liveLength:liveOutputText.length,
      reasoningLength:reasoning.length,
      pendingCount:pendingAdmissions.length,
      optimisticCount:optimisticMessages.length,
      localCount:localExchanges.length,
      noticeKey:chatNoticeKey,
    }

    if(prev.conversationID!==id){
      scrollSnapshot.current=nextSnapshot
      if(!messageAnchor)scrollChatToBottom('auto')
      return
    }

    const addedMessages=messages.length>prev.messageCount
      ? (prev.lastMessageID>0 ? messages.filter(m=>(m.id??0)>prev.lastMessageID).length : messages.length-prev.messageCount)
      : 0
    const userSent=optimisticMessages.length>prev.optimisticCount
      || pendingAdmissions.length>prev.pendingCount
      || localExchanges.length>prev.localCount
      || (addedMessages>0 && last?.role==='user')
    const hasStreamDelta=liveOutputText.length>prev.liveLength || reasoning.length>prev.reasoningLength
    const hasNoticeDelta=chatNoticeKey!==prev.noticeKey && Boolean(composerNotice || chatError || composerAdmitting || (modelNotice && modelNotice.kind!=='resolved'))

    scrollSnapshot.current=nextSnapshot

    if(userSent){
      scrollChatToBottom('auto')
      return
    }
    if(addedMessages>0 || hasStreamDelta || hasNoticeDelta){
      if(pinnedToBottom.current && !messageAnchor){
        scrollChatToBottom('auto')
      }else if(chatLogRef.current && chatLogRef.current.scrollHeight>chatLogRef.current.clientHeight+24){
        if(addedMessages>0)setUnseenCount(c=>c+addedMessages)
        setHasNewActivity(true)
      }
    }
  },[id,data?.messages,liveOutputText,reasoning.length,pendingAdmissions.length,optimisticMessages.length,localExchanges.length,chatNoticeKey,composerNotice,chatError,composerAdmitting,modelNotice,messageAnchor,scrollChatToBottom])

  const hasUnread=unseenCount>0 || hasNewActivity

  return <>
    {toolbarActions&&toolbarView==='materials'&&createPortal(<details className="chat-uploaded-materials">
      <summary className="outline"><Paperclip size={13} aria-hidden="true"/>已上传 {latestUploads.length} 份材料</summary>
      <div className="workspace-notices"><MessageAttachments items={latestUploads} previewItems={draftMaterials}/></div>
    </details>,toolbarActions)}
    {toolbarActions&&toolbarView==='operations'&&taskReference&&createPortal(
      <button type="button" className="outline" onClick={event=>{taskTrigger.current=event.currentTarget;setSelectedTask(taskReference.taskID)}} aria-haspopup="dialog">
        <PanelRight size={13} aria-hidden="true"/>当前任务 <span>{taskReference.title}</span>
      </button>,toolbarActions)}
    <main className="main work-detail"><h1 className="v31-screen-reader">{conversation?.title||'对话'}</h1>
      <div className="chat-log-wrap">
        <div className="discussion chat-log" ref={chatLogRef} onScroll={handleChatScroll}>
          <div className="chat-log-content" ref={chatContentRef}>
            {data?<MessageHistory conversationID={data.conversation.id} messages={data.messages} live={busy?liveOutputText:undefined} liveReasoning={busy&&!run?.task_id?reasoning:undefined} liveThinking={Boolean(busy&&!run?.task_id)} liveRunID={busy?run?.id:undefined} remember={rememberMessage} replyCard={benchmarkReply} omitAutomaticRevisionRunID={reopenedRevision&&reopenedRevision.source.id!==reopenedRevision.anchorRunID?reopenedRevision.anchorRunID:undefined} previewMaterials={draftMaterials} assistantName="ClusBot" userAccount={account} localExchanges={localExchanges}/>:localExchanges.map(entry=><div className="messages" key={entry.id}>{entry.content}</div>)}
            {optimisticMessages.map(item=><div className="messages" key={'opt:'+item.key}><div className="message message-user"><span><time dateTime={new Date(item.createdAt).toISOString()}>{displayDate(item.createdAt,{hour:'2-digit',minute:'2-digit'})}</time>{account?.display_name||'你'}{account&&<i className="message-user-avatar" aria-hidden="true">{account.avatar||Array.from(account.display_name||account.username)[0]}</i>}</span><div className="user-message"><MessageContent
              reference={item.taskRef?<MessageReference label={'@'+item.taskRef.title+' 只读引用'}/>:undefined}
              body={item.text||undefined}
              attachments={Boolean(item.materials.length)&&<MessageAttachments items={item.materials} previewItems={draftMaterials}/>}/></div><div className={'message-user-status'+(item.status==='failed'?' is-error':'')} role="status">
              {item.status==='sending'&&<><LoaderCircle size={12} className="spin" aria-hidden="true"/><span>发送中…</span></>}
              {item.status==='routing'&&<><LoaderCircle size={12} className="spin" aria-hidden="true"/><span>正在理解意图…</span></>}
              {item.status==='failed'&&<><CircleAlert size={12} aria-hidden="true"/><span>{item.error}</span>{item.retry&&<button type="button" className="text-button" onClick={item.retry}>{item.unconfirmed?'重新判断并发送':'重试'}</button>}{item.restore&&<button type="button" className="text-button" onClick={item.restore}>放回输入框</button>}<button type="button" className="text-button" onClick={()=>setOptimisticMessages(prev=>prev.filter(x=>x.key!==item.key))}>移除</button></>}
            </div></div></div>)}
            {pendingAdmissions.map(item=><div className="messages" key={'pending:'+item.key}><div className="message message-user"><span><time dateTime={new Date(item.createdAt).toISOString()}>{displayDate(item.createdAt,{hour:'2-digit',minute:'2-digit'})}</time>{account?.display_name||'你'}{account&&<i className="message-user-avatar" aria-hidden="true">{account.avatar||Array.from(account.display_name||account.username)[0]}</i>}</span><div className="user-message">{item.text}</div></div></div>)}
            {assistantModelNotice}
            {composerNotice && (
              <AssistantNoticeMessage
                iconSrc={clusbotIcon}
                tone="error"
                role="alert"
                badge="发送受阻"
                title="消息未能发送（草稿已保留）"
                message={composerNotice.message}
                onDismiss={composerNotice.dismiss}
                actions={composerNotice.unconfirmed && composerNotice.retry ? <button type="button" className="primary" onClick={composerNotice.retry}>重新判断并发送</button> : undefined}
              />
            )}
            {chatError && (
              <AssistantNoticeMessage
                iconSrc={clusbotIcon}
                tone="error"
                role="alert"
                badge="执行异常"
                title="本轮处理出现异常"
                message={chatError}
                onDismiss={actionError ? () => setActionError('') : undefined}
                actions={canResume ? <button type="button" className="primary" disabled={saving} onClick={() => void resumeConversation()}><Play size={13} />继续回复</button> : undefined}
              />
            )}
            {activity&&<div className="chat-team-activity">{activity}</div>}
          </div>
        </div>
        {(hasUnread || awayFromBottom) && (
          <button
            type="button"
            className="chat-scroll-pill"
            data-unread={hasUnread ? 'true' : undefined}
            onClick={() => scrollChatToBottom('smooth')}
            aria-label={hasUnread ? (unseenCount > 1 ? `查看 ${unseenCount} 条新消息` : '查看新消息') : '回到底部'}
          >
            <ArrowDown size={14} aria-hidden="true" />
            <span>{hasUnread ? (unseenCount > 1 ? `${unseenCount} 条新消息` : '新消息') : '回到底部'}</span>
          </button>
        )}
      </div>
      {taskLinks.size>0&&<section className="conversation-tasks" aria-label="会话已受理工作">
        <h2>这段对话关联的工作</h2>
        {[...taskLinks.values()].reverse().slice(0,8).map(link=><div key={link.taskID} className="conversation-task-link">
          <div><strong>{link.benchmark?'基准任务':'任务'}</strong><small>{link.taskID}</small></div>
          <button type="button" className="text-button" aria-haspopup="dialog" onClick={event=>{
            taskTrigger.current=event.currentTarget;setSelectedTask(link.taskID)
          }}>查看任务 <ChevronRight size={16} aria-hidden="true"/></button>
        </div>)}
      </section>}
      {run?.status==='cancelled'&&!run.response&&<p className="muted" role="status">已停止</p>}
      {!chatError && canResume && <div className="chat-actions">
        <button type="button" className="outline" disabled={saving} onClick={()=>void resumeConversation()}><Play size={16}/>继续回复</button>
      </div>}
      {id&&executionTarget&&<BenchmarkExecutionConversation key={executionTarget.taskID+':'+executionTarget.planRevision} conversationID={id}
        target={executionTarget} run={run} disabled={Boolean(busy||conversation?.archived)}
        canSuggest={taskReference?.taskID===executionTarget.taskID&&taskReference?.planRevision!==undefined} onChange={()=>void reload()}/>}
      <Composer inputRef={chatInput} previewMaterials={latestUploads} busyAction={busy&&!run?.task_id?<button type="button" className="composer-stop" disabled={stopping===run?.id} onClick={()=>void stopConversation()}>
        {stopping===run?.id?<LoaderCircle size={13} className="spin"/>:<Square size={10} fill="currentColor"/>}{stopping===run?.id?'正在停止…':'停止回复'}
      </button>:undefined} openModelSettings={assistantManage?()=>navigate('/settings?tab=model'):undefined} hidden={isolatedOpen} isolatedRecovery={Boolean(isolatedRequest)}
        onStatusNotice={setComposerNotice} onAdmissionChange={setComposerAdmitting}
        onOptimisticStart={item=>{
          setOptimisticMessages(prev=>[...prev.filter(x=>x.key!==item.key),{...item,status:'sending'}])
          scrollChatToBottom('auto')
        }}
        onOptimisticSettle={key=>setOptimisticMessages(prev=>prev.filter(x=>x.key!==key))}
        onOptimisticFail={(key,info)=>setOptimisticMessages(prev=>prev.map(x=>x.key===key?{...x,status:'failed',error:info.message,unconfirmed:info.unconfirmed,retry:info.retry,restore:info.restore}:x))}
        onAssistantReceipt={(item,text)=>{
          const entryID=crypto.randomUUID()
          const cards=assistantReplyCards(item)
          if(cards.some(isConversationConfiguration)){
            if(conversation)setConfigEntry(entryID)
            else setActionError('请先发送一条消息，再为这段对话绑定能力。')
          }
          const afterMessageID=Math.max(0,...(data?.messages.map(message=>message.id??0)??[]))
          setAssistantItems(previous=>[...previous,{id:entryID,cards,createdAt:Date.now(),afterMessageID,text:/(?:密码|密钥|凭据|password|passwd|secret|credential|token|api[_ -]?key|authorization)/i.test(text)?'助手工具请求（内容已隐藏）':text}])
        }}
        openIsolated={!conversation?.archived||isolatedRequest?(model,text)=>{setIsolatedModel(model);setIsolatedText(text);setIsolatedOpen(true)}:undefined}
        referenceConversationID={id} onReferenceTask={id?reference=>{changeComposerDraft(ordinaryDraftKey,()=>({taskReference:reference}));chatInput.current?.focus()}:undefined} reference={taskReference&&<MessageReference
        label={(taskReference.passive?'引用任务 · ':taskReference.purpose==='revision'?'修订目标 · ':'基准控制目标 · ')+taskReference.title+' · '+taskReference.taskID+(taskReference.planRevision!==undefined?' · 修订 '+taskReference.planRevision:'')}
        onOpen={button=>{taskTrigger.current=button;setSelectedTask(taskReference.taskID)}}
        onRemove={()=>{changeComposerDraft(ordinaryDraftKey,()=>({taskReference:undefined}));chatInput.current?.focus()}}/>} draftKey={ordinaryDraftKey}
        placeholder="输入消息，输入 / 调用指令，输入 @ 引用任务或成员…" label="发送消息" disabled={busy||saving||conversation?.archived||Boolean(id&&!data)}
        disabledReason={conversation?.archived?'恢复对话后可发送':busy?'本轮处理中，草稿可继续编辑':undefined} prepareDelivery={prepareChatDelivery} onSend={async(text,key,_scope,attachments,delivery,strategy)=>{
        if(/^\/(?:助手|assistant)(?: |$)/.test(text.trimStart()) && (attachments.length>0||delivery.task_ref))throw new Error('助手工具指令不能同时携带附件或任务引用，请移除后发送。')
        const receipt=await api<Receipt>(id?'/conversations/'+id+'/messages':'/conversations',{text,client_message_id:key,attachment_ids:attachments,...(id&&delivery.task_ref?{task_ref:delivery.task_ref}:{}),...(id&&delivery.benchmark_revision_target?{benchmark_revision_target:delivery.benchmark_revision_target}:{})},'POST',{'X-ClusBot-Assistant-Cards':'1',...(strategy==='deterministic_only'?{'X-ClusBot-Reply-Strategy':'deterministic_only'}:{})})
        if(delivery.benchmark_revision_target&&(!receipt.run_id||receipt.conversation_id!==id||receipt.task_id||
          receipt.benchmark_revision_suggestion?.task_id!==delivery.benchmark_revision_target.task_id||
          receipt.benchmark_revision_suggestion?.expected_revision!==delivery.benchmark_revision_target.expected_revision))throw new Error('修订请求回执无法核对，草稿已保留。')
        return receipt
      }} onReceipt={async(path,receipt)=>{
        if (receipt?.task_id && path.startsWith('/tasks/')) {
          if (id) {
            setLatestReceipt({taskID:receipt.task_id,benchmark:false})
            await reload()
            return
          }
          if(receipt.conversation_id) {
            navigate('/chat/'+receipt.conversation_id)
            return
          }
        }
        if (id && path==='/chat/'+id) await reload()
        else navigate(path)
      }}/>
      {isolatedOpen&&<IsolatedReplyComposer conversationID={id} blocked={busy||saving||Boolean(id&&!data)} initialModel={isolatedModel} initialText={isolatedText}
        archived={Boolean(conversation?.archived)} onClose={closeIsolated} onReceipt={(receipt,request)=>{
          if(isolatedText===request.text)changeComposerDraft(ordinaryDraftKey,previous=>previous.text===request.text?{text:''}:{})
          if(id)void reload();else if(receipt.conversation_id)navigate('/chat/'+receipt.conversation_id)
        }}/>}
    </main>
    {selectedTask&&<dialog className="chat-task-dialog" aria-label="任务详情" ref={detailDialog}
      onCancel={event=>{event.preventDefault();closeTaskDetail()}} onKeyDown={event=>{if(event.key==='Escape'){event.preventDefault();closeTaskDetail()}}}>
      <ChatTaskDrawer key={selectedTask} taskID={selectedTask} onChange={()=>void reload()} navigate={path=>{closeTaskDetail();navigate(path)}}
        sourceConversationID={conversation?.archived?undefined:id} onReference={conversation?.archived?undefined:referenceTask}
        closeAction={<button type="button" className="icon-button" aria-label="关闭任务详情" ref={detailClose} onClick={()=>closeTaskDetail()}><X size={16} aria-hidden="true"/></button>}>
        <TaskDetail taskID={selectedTask} compact fromChat onReference={conversation?.archived?undefined:referenceTask}
          sourceConversationID={conversation?.archived?undefined:id} sourceConversationBusy={Boolean(!conversation?.archived&&busy)}
          onChange={()=>void reload()} navigate={path=>{closeTaskDetail();navigate(path)}}/>
      </ChatTaskDrawer>
    </dialog>}
  </>
}

function isConversationConfiguration(item:ReturnType<typeof assistantReplyCards>[number]) {
  return !Array.isArray(item)&&item.state==='form_required'&&item.tool==='configure_conversation'&&item.form==='conversation_configuration'
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
