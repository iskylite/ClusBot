import {useCallback,useEffect,useRef,useState,type FormEvent} from 'react'
import {ArrowDownToLine, Check, ChevronLeft, ChevronRight, FileText, LoaderCircle, Pause, Pencil, Play, RefreshCw, Search, ShieldCheck, Square, X} from 'lucide-react'
import {api,APIError,type Artifact} from './api'
import './TeamTasksPage.css'

type AdminTask = {owner_id:string;id:string;title:string;goal:string;scope_label:string;acceptance_criteria?:string;required_filename?:string;revision:number;status:string;status_reason:string;updated_at:number}
type TaskPage = {items:AdminTask[];next_cursor?:string}
type RunState = {id:string;status:string;created_at:number;execution_generation:number;error_code?:string}
type RunPage = {items:RunState[];next_cursor?:string}
type Account = {principal_id:string;display_name:string;username:string}
type AdminArtifact = Artifact & {content_unavailable?:boolean}
type Notice = {owner_id:string;task_id:string;title:string;status:string;status_reason?:string;updated_at:number}
type Action = 'rename'|'pause'|'cancel'|'resume'|'retry'|'accept'
type Confirmation = {action:Action;task:AdminTask;artifact?:AdminArtifact;requestID:string}
type Target = {owner:string;id:string}

const pageSize=20
const taskPath=({owner,id}:Target)=>'/admin/tasks/'+encodeURIComponent(owner)+'/'+encodeURIComponent(id)
const targetOf=(task:AdminTask):Target=>({owner:task.owner_id,id:task.id})
const dateTime=new Intl.DateTimeFormat('zh-CN',{year:'numeric',month:'numeric',day:'numeric',hour:'2-digit',minute:'2-digit'})
const date=(value:number)=>value?dateTime.format(value):'—'
const message=(error:unknown)=>error instanceof Error?error.message:'请求未完成，请稍后重试'
const statusText=(status:string,reason?:string)=>{
  if(reason==='pause_requested')return '暂停请求处理中'
  if(reason==='stop_requested')return '停止请求处理中'
  if(status==='interrupted'&&reason==='paused')return '已暂停'
  if(status==='waiting_user'&&reason==='acceptance_required')return '待验收'
  return ({active:'处理中',waiting_user:'待处理',interrupted:'已中断',completed:'已完成',cancelled:'已停止'} as Record<string,string>)[status]??status
}
const runStatus=(status:string)=>({queued:'等待执行',running:'运行中',completed:'已完成',interrupted:'已中断',cancelled:'已停止',failed:'失败',input_required:'需要补充'} as Record<string,string>)[status]??status
const actionLabels:Record<Action,string>={rename:'修改标题',pause:'暂停后续调度',cancel:'请求停止',resume:'继续任务',retry:'重新执行',accept:'验收成果'}
const actionDescriptions:Record<Action,string>={
  rename:'只修改任务标题，目标、范围和交付要求保持不变。',
  pause:'将请求暂停后续调度；已派发的远端作业是否停止仍需核对。',
  cancel:'将请求停止任务；已派发的远端作业及其结果仍需核对。',
  resume:'将沿用原任务输入继续执行，任务仍归原负责人。',
  retry:'将按原任务输入重新执行，任务仍归原负责人；是否允许重试由当前读取状态决定。',
  accept:'确认选中的成果符合任务要求。验收不代表已向外发送。',
}

function useAccounts() {
  const [accounts,setAccounts]=useState<Account[]>([])
  useEffect(()=>{let alive=true;api<Account[]>('/admin/users').then(items=>{if(alive)setAccounts(items)}).catch(()=>{});return()=>{alive=false}},[])
  return accounts
}
function accountName(accounts:Account[],owner:string) {
  const account=accounts.find(item=>item.principal_id===owner)
  return account?.display_name||account?.username||owner
}
function Badge({status,reason}:{status:string;reason?:string}) {
  return <span className={'team-status team-status-'+status}>{statusText(status,reason)}</span>
}
function ButtonIcon({action}:{action:Action}) {
  const Icon=action==='rename'?Pencil:action==='pause'?Pause:action==='cancel'?Square:action==='accept'?Check:action==='retry'?RefreshCw:Play
  return <Icon size={14} aria-hidden="true"/>
}

export function TeamTasksPage({initialOwner,initialTaskID}:{initialOwner?:string;initialTaskID?:string}) {
  const accounts=useAccounts()
  const [scope,setScope]=useState<'all'|'mine'|'others'>('all')
  const [status,setStatus]=useState('')
  const [owner,setOwner]=useState('')
  const [query,setQuery]=useState('')
  const [cursors,setCursors]=useState<string[]>([''])
  const [page,setPage]=useState<TaskPage>()
  const [loading,setLoading]=useState(true)
  const [error,setError]=useState('')
  const [revision,setRevision]=useState(0)
  const [target,setTarget]=useState<Target>()
  const [confirmation,setConfirmation]=useState<Confirmation>()
  const [preparing,setPreparing]=useState(false)
  const [actionError,setActionError]=useState('')
  const generation=useRef(0)
  const reload=useCallback(()=>setRevision(value=>value+1),[])
  useEffect(()=>{if(initialOwner&&initialTaskID)setTarget({owner:initialOwner,id:initialTaskID})},[initialOwner,initialTaskID])
  useEffect(()=>{
    const request=++generation.current
    const params=new URLSearchParams({scope,limit:String(pageSize)})
    if(status)params.set('status',status)
    if(owner)params.set('owner',owner)
    if(query.trim())params.set('q',query.trim())
    if(cursors.at(-1))params.set('after',cursors.at(-1)!)
    setLoading(true);setError('');setPage(undefined)
    api<TaskPage>('/admin/tasks?'+params).then(result=>{if(request===generation.current)setPage(result)})
      .catch(e=>{if(request===generation.current){setError(message(e));setPage(undefined)}})
      .finally(()=>{if(request===generation.current)setLoading(false)})
  },[scope,status,owner,query,cursors,revision])
  function setFilter(change:()=>void) {setCursors(['']);change()}
  async function prepare(action:Action,item:AdminTask,artifact?:AdminArtifact) {
    if(preparing)return
    setPreparing(true);setActionError('')
    try {
      const latest=await api<AdminTask>(taskPath(targetOf(item)))
      setConfirmation({action,task:latest,artifact,requestID:crypto.randomUUID()})
    } catch(e) {setActionError(message(e));reload()}
    finally {setPreparing(false)}
  }
  function refreshDetails() {reload();setDetailRevision(value=>value+1)}
  function changed() {setConfirmation(undefined);refreshDetails()}
  const [detailRevision,setDetailRevision]=useState(0)
  const open=(task:AdminTask)=>{setActionError('');setTarget(targetOf(task))}
  return <>
    <main className="main team-tasks-page">
      <header className="page-heading team-tasks-heading"><h1>任务</h1><button type="button" className="team-button" onClick={reload} disabled={loading}><RefreshCw size={14} aria-hidden="true"/>刷新</button></header>
      <div className="team-tasks-switches">
        <div className="team-segments" aria-label="任务范围">
          {([['all','全部成员'],['mine','仅看我的'],['others','其他成员']] as const).map(([value,label])=><button key={value} type="button" aria-pressed={scope===value} onClick={()=>setFilter(()=>{setScope(value);setOwner('')})}>{label}</button>)}
        </div>
        <p className="team-privacy"><ShieldCheck size={15} aria-hidden="true"/>仅管理任务进度与产物，不读取成员私有对话</p>
      </div>
      <div className="team-tasks-filters">
        <div className="team-segments team-status-filters" aria-label="任务状态">
          {([['','全部'],['active','处理中'],['waiting_user','待处理'],['interrupted','已中断'],['completed','已完成'],['cancelled','已停止']] as const).map(([value,label])=><button key={value} type="button" aria-pressed={status===value} onClick={()=>setFilter(()=>setStatus(value))}>{label}</button>)}
        </div>
        <div className="team-filter-inputs">
          <label className="team-owner-filter"><span className="team-sr-only">负责人</span><select value={owner} onChange={e=>setFilter(()=>setOwner(e.target.value))}><option value="">账号：全部成员</option>{accounts.map(account=><option key={account.principal_id} value={account.principal_id}>{account.display_name||account.username}</option>)}</select></label>
          <label className="team-search"><Search size={15} aria-hidden="true"/><span className="team-sr-only">搜索任务标题或目标</span><input type="search" value={query} placeholder="搜索任务标题或目标" onChange={e=>setFilter(()=>setQuery(e.target.value))}/></label>
        </div>
      </div>
      {actionError&&<p className="error" role="alert">{actionError}</p>}
      <section className="team-task-list" aria-label="任务列表">
        {error?<div className="team-list-message"><p role="alert">{error}</p><button type="button" className="team-button" onClick={reload}>重试</button></div>:
          loading&&!page?<div className="team-list-message"><LoaderCircle className="spin" aria-label="正在加载任务"/></div>:
          <div className="team-table-wrap" role="region" aria-label="任务列表" tabIndex={0}>
            <table className="team-table"><thead><tr><th scope="col">任务编号与标题</th><th scope="col">负责人</th><th scope="col">状态</th><th scope="col">更新时间</th><th scope="col">任务操作</th></tr></thead>
              <tbody>{page?.items.map(task=><tr key={task.owner_id+'/'+task.id}>
                <th scope="row"><button type="button" className="team-task-title" onClick={()=>open(task)}><span className="team-task-id" title={task.id}>{task.id.slice(0,8)}</span><strong>{task.title}</strong></button><span className="team-task-subtitle">{task.scope_label||'未设置工作范围'}</span></th>
                <td><span className="team-owner" title={task.owner_id}>{accountName(accounts,task.owner_id)}</span></td><td><Badge status={task.status} reason={task.status_reason}/></td><td className="team-updated">{date(task.updated_at)}</td>
                <td><div className="team-row-actions"><button type="button" className="team-button" disabled={preparing} onClick={()=>void prepare('rename',task)}><Pencil size={14}/>改标题</button><button type="button" className="team-button" onClick={()=>open(task)}><FileText size={14}/>运行与产物</button>
                  {task.status==='active'&&!['pause_requested','stop_requested'].includes(task.status_reason)&&<button type="button" className="team-button team-warning" disabled={preparing} onClick={()=>void prepare('pause',task)}><Pause size={14}/>暂停</button>}
                  {task.status==='waiting_user'&&<button type="button" className="team-button team-accent" onClick={()=>open(task)}><Check size={14}/>查看成果</button>}
                  {task.status==='interrupted'&&<button type="button" className="team-button team-accent" disabled={preparing} onClick={()=>void prepare('resume',task)}><Play size={14}/>继续</button>}
                </div></td>
              </tr>)}</tbody>
            </table>
            {!loading&&!page?.items.length&&<p className="team-list-message">没有符合条件的任务</p>}
          </div>}
        <footer className="team-pagination"><span>{loading?'正在更新…':`本页 ${page?.items.length??0} 项`}</span><div><button type="button" className="team-button" disabled={loading||cursors.length===1} onClick={()=>setCursors(current=>current.slice(0,-1))}><ChevronLeft size={14}/>上一页</button><button type="button" className="team-button" disabled={loading||!page?.next_cursor} onClick={()=>page?.next_cursor&&setCursors(current=>[...current,page.next_cursor!])}>下一页<ChevronRight size={14}/></button></div></footer>
      </section>
    </main>
    {target&&<TeamTaskDetail key={target.owner+'/'+target.id} target={target} revision={detailRevision} ownerName={accountName(accounts,target.owner)} close={()=>setTarget(undefined)} prepare={prepare} preparing={preparing} actionError={actionError}/>}
    {confirmation&&<TaskConfirmation value={confirmation} ownerName={accountName(accounts,confirmation.task.owner_id)} close={()=>setConfirmation(undefined)} changed={changed} onConflict={refreshDetails}/>}
  </>
}

function TeamTaskDetail({target,revision,ownerName,close,prepare,preparing,actionError}:{target:Target;revision:number;ownerName:string;close:()=>void;prepare:(action:Action,task:AdminTask,artifact?:AdminArtifact)=>Promise<void>;preparing:boolean;actionError:string}) {
  const dialog=useRef<HTMLDialogElement>(null)
  const [task,setTask]=useState<AdminTask>()
  const [runs,setRuns]=useState<RunPage>()
  const [artifacts,setArtifacts]=useState<AdminArtifact[]>()
  const [error,setError]=useState('')
  const [moreError,setMoreError]=useState('')
  const [loading,setLoading]=useState(true)
  const [more,setMore]=useState(false)
  const [preview,setPreview]=useState<{id:string;content:string}>()
  const [previewError,setPreviewError]=useState('')
  const [previewLoading,setPreviewLoading]=useState(false)
  const [attempt,setAttempt]=useState(0)
  const base=taskPath(target)
  useEffect(()=>{const node=dialog.current;if(!node)return;node.showModal();return()=>{if(node.open)node.close()}},[])
  useEffect(()=>{
    let alive=true;setLoading(true);setError('');setPreview(undefined);setArtifactsComplete(false)
    Promise.all([api<AdminTask>(base),api<RunPage>(base+'/runs'),api<AdminArtifact[]>(base+'/artifacts')]).then(([nextTask,nextRuns,nextArtifacts])=>{if(alive){setTask(nextTask);setRuns(nextRuns);setArtifacts(nextArtifacts)}})
      .catch(e=>{if(alive)setError(message(e))}).finally(()=>{if(alive)setLoading(false)})
    return()=>{alive=false}
  },[base,revision,attempt])
  async function moreRuns() {
    if(!runs?.next_cursor||more)return
    setMore(true);setMoreError('')
    try{const result=await api<RunPage>(base+'/runs?after='+encodeURIComponent(runs.next_cursor));setRuns({items:[...runs.items,...result.items],next_cursor:result.next_cursor})}catch(e){setMoreError(message(e))}finally{setMore(false)}
  }
  async function moreArtifacts() {
    if(!artifacts?.length||more)return
    setMore(true);setMoreError('')
    try{const result=await api<AdminArtifact[]>(base+'/artifacts?after='+encodeURIComponent(artifacts.at(-1)!.id));setArtifacts([...artifacts,...result]);if(result.length<100)setArtifactsComplete(true)}catch(e){setMoreError(message(e))}finally{setMore(false)}
  }
  const [artifactsComplete,setArtifactsComplete]=useState(false)
  async function readArtifact(artifact:AdminArtifact) {
    if(preview?.id===artifact.id){setPreview(undefined);return}
    setPreviewLoading(true);setPreviewError('')
    try {
      const response=await fetch('/api/v1'+base+'/artifacts/'+encodeURIComponent(artifact.id)+'/content')
      if(response.status===401)window.dispatchEvent(new Event('clusbot:logout'))
      if(!response.ok)throw new Error('无法读取成果，请重试或下载')
      setPreview({id:artifact.id,content:await response.text()})
    }catch(e){setPreviewError(message(e))}finally{setPreviewLoading(false)}
  }
  const currentRun=runs?.items[0]
  return <dialog ref={dialog} className="team-detail-dialog" aria-labelledby="team-detail-title" onCancel={event=>{event.preventDefault();close()}}>
    <header><div><span className="team-detail-kicker">任务详情</span><h2 id="team-detail-title">{task?.title||'正在读取任务'}</h2></div><button type="button" className="icon-button" aria-label="关闭任务详情" onClick={close}><X size={20}/></button></header>
    {loading?<div className="team-detail-empty"><LoaderCircle className="spin" aria-label="正在加载详情"/></div>:error?<div className="team-detail-body"><p className="error" role="alert">{error}</p><button type="button" className="team-button" onClick={()=>setAttempt(value=>value+1)}>重试</button></div>:task&&<div className="team-detail-body">
      <div className="team-detail-meta"><Badge status={task.status} reason={task.status_reason}/><span title={target.owner}>{ownerName}</span><span title={task.id}>#{task.id.slice(0,8)}</span></div>
      <section><h3>目标</h3><p className="team-detail-text">{task.goal}</p>{task.scope_label&&<p className="team-detail-muted">范围：{task.scope_label}</p>}</section>
      {(task.acceptance_criteria||task.required_filename)&&<section><h3>验收条件</h3>{task.acceptance_criteria&&<p className="team-detail-text">{task.acceptance_criteria}</p>}{task.required_filename&&<p className="team-detail-muted">交付文件：outputs/{task.required_filename}</p>}</section>}
      <section><h3>任务操作</h3><div className="team-detail-actions"><button type="button" className="team-button" disabled={preparing} onClick={()=>void prepare('rename',task)}><Pencil size={14}/>改标题</button>
        {task.status==='active'&&!['pause_requested','stop_requested'].includes(task.status_reason)&&<button type="button" className="team-button" disabled={preparing} onClick={()=>void prepare('pause',task)}><Pause size={14}/>暂停后续调度</button>}
        {['active','waiting_user','interrupted'].includes(task.status)&&!['pause_requested','stop_requested'].includes(task.status_reason)&&<button type="button" className="team-button team-warning" disabled={preparing} onClick={()=>void prepare('cancel',task)}><Square size={14}/>请求停止</button>}
        {['interrupted','cancelled'].includes(task.status)&&<button type="button" className="team-button team-accent" disabled={preparing} onClick={()=>void prepare('resume',task)}><Play size={14}/>继续任务</button>}
        {task.status==='interrupted'&&<button type="button" className="team-button" disabled={preparing} onClick={()=>void prepare('retry',task)}><RefreshCw size={14}/>重新执行</button>}
      </div></section>
      {actionError&&<p className="error" role="alert">{actionError}</p>}
      <section><h3>运行记录</h3>{!runs?.items.length?<p className="team-detail-muted">暂无运行记录</p>:<ul className="team-detail-list">{runs.items.map(run=><li key={run.id}><div><strong>{runStatus(run.status)}</strong><small title={run.id}>#{run.id.slice(0,8)} · {date(run.created_at)}</small></div>{run.error_code&&<span className="team-detail-muted">{run.error_code}</span>}</li>)}</ul>}{runs?.next_cursor&&<button type="button" className="team-link" disabled={more} onClick={()=>void moreRuns()}>加载更多运行</button>}</section>
      <section><h3>成果</h3>{!artifacts?.length?<p className="team-detail-muted">暂无成果</p>:<ul className="team-detail-list">{artifacts.map(artifact=><li key={artifact.id} className="team-artifact"><div><strong>{artifact.name}</strong><small>{date(artifact.created_at)}{artifact.accepted_at?' · 已验收':''}</small></div><div className="team-artifact-actions"><button type="button" className="team-link" disabled={previewLoading||artifact.content_unavailable} onClick={()=>void readArtifact(artifact)}>{preview?.id===artifact.id?'收起':'预览'}</button>{!artifact.content_unavailable&&<a className="team-link" href={'/api/v1'+base+'/artifacts/'+encodeURIComponent(artifact.id)+'/content'} download><ArrowDownToLine size={14}/>下载</a>}{task.status==='waiting_user'&&currentRun?.status==='completed'&&artifact.run_id===currentRun.id&&!artifact.content_unavailable&&<button type="button" className="team-link" disabled={preparing} onClick={()=>void prepare('accept',task,artifact)}>验收</button>}</div>{preview?.id===artifact.id&&<pre className="team-artifact-preview">{preview.content}</pre>}</li>)}</ul>}{artifacts&&artifacts.length>=100&&!artifactsComplete&&<button type="button" className="team-link" disabled={more} onClick={()=>void moreArtifacts()}>加载更多成果</button>}{previewError&&<p className="error" role="alert">{previewError}</p>}</section>
      {moreError&&<p className="error" role="alert">{moreError}</p>}
      <p className="team-detail-muted">更新于 {date(task.updated_at)}</p>
    </div>}
  </dialog>
}

function TaskConfirmation({value,ownerName,close,changed,onConflict}:{value:Confirmation;ownerName:string;close:()=>void;changed:()=>void;onConflict:()=>void}) {
  const dialog=useRef<HTMLDialogElement>(null)
  const [title,setTitle]=useState(value.task.title)
  const [pending,setPending]=useState(false)
  const [error,setError]=useState('')
  const [conflict,setConflict]=useState(false)
  useEffect(()=>{const node=dialog.current;if(!node)return;node.showModal();return()=>{if(node.open)node.close()}},[])
  async function submit(event:FormEvent) {
    event.preventDefault()
    if(pending||conflict)return
    setPending(true);setError('')
    const base=taskPath(targetOf(value.task))
    try {
      if(value.action==='rename')await api(base,{revision:value.task.revision,title:title.trim()},'PATCH')
      else if(value.action==='accept')await api(base+'/acceptance',{revision:value.task.revision,artifact_id:value.artifact?.id})
      else if(value.action==='resume'||value.action==='retry')await api(base+'/resume',{revision:value.task.revision,client_request_id:value.requestID,...(value.action==='retry'?{retry:true}:{})})
      else await api(base+'/'+value.action,{revision:value.task.revision})
      changed()
    }catch(e){setError(message(e));if(e instanceof APIError&&e.status===409){setConflict(true);onConflict()}}finally{setPending(false)}
  }
  return <dialog ref={dialog} className="new-dialog team-confirm-dialog" aria-labelledby="team-confirm-title" onCancel={event=>{event.preventDefault();if(!pending)close()}}>
    <header><h2 id="team-confirm-title">{actionLabels[value.action]}</h2><button type="button" className="icon-button" aria-label="关闭确认" disabled={pending} onClick={close}><X size={20}/></button></header>
    <form onSubmit={submit}>
      <p>{actionDescriptions[value.action]}</p><dl><div><dt>负责人</dt><dd>{ownerName}<small className="team-confirm-id">{value.task.owner_id}</small></dd></div><div><dt>任务</dt><dd>{value.task.title}<small className="team-confirm-id">{value.task.id}</small></dd></div>{value.artifact&&<div><dt>成果</dt><dd>{value.artifact.name}<small className="team-confirm-id">{value.artifact.id}</small></dd></div>}</dl>
      {value.action==='accept'&&value.task.acceptance_criteria&&<div className="team-confirm-criteria"><strong>验收条件</strong><p>{value.task.acceptance_criteria}</p></div>}
      {value.action==='accept'&&value.task.required_filename&&<div className="team-confirm-criteria"><strong>交付文件</strong><p>outputs/{value.task.required_filename}</p></div>}
      {value.action==='rename'&&<label>新标题<input autoFocus maxLength={60} required value={title} onChange={event=>setTitle(event.target.value)}/></label>}
      {error&&<p className="error" role="alert">{error}{conflict?' 任务状态或版本可能已变化，请关闭并重新核对。':''}</p>}
      <footer><button type="button" className="outline" disabled={pending} onClick={close}>取消</button><button className="primary" disabled={pending||conflict||(value.action==='rename'&&(!title.trim()||title.trim()===value.task.title))}><ButtonIcon action={value.action}/>{pending?'正在处理…':'确认'+actionLabels[value.action]}</button></footer>
    </form>
  </dialog>
}

export function TaskNotices({onOpen}:{onOpen:(owner:string,id:string)=>void}) {
  const accounts=useAccounts()
  const [notices,setNotices]=useState<Notice[]>()
  const [error,setError]=useState('')
  useEffect(()=>{
    let alive=true,timer:number|undefined
    async function read(){
      try{if(!document.hidden){const result=await api<{items:Notice[]}>('/admin/task-notices?scope=others&limit=50');if(alive){setNotices(result.items);setError('')}}}
      catch(e){if(alive)setError(message(e))}
      finally{if(alive)timer=window.setTimeout(()=>void read(),20000)}
    }
    void read()
    return()=>{alive=false;if(timer!==undefined)window.clearTimeout(timer)}
  },[])
  return <section className="team-notices" aria-label="全站任务动态">
    {error&&<p className="error" role="alert">{error}</p>}
    {!notices&&!error&&<p className="team-detail-muted">正在读取任务动态…</p>}
    {notices?.length===0&&<p className="team-detail-muted">暂无其他成员的任务动态</p>}
    {notices?.map(notice=><article className="team-notice" key={notice.owner_id+'/'+notice.task_id}><span className="team-notice-owner" title={notice.owner_id}>来自 {accountName(accounts,notice.owner_id)}</span><strong title={notice.title}>{notice.title}</strong><Badge status={notice.status} reason={notice.status_reason}/><button type="button" className="team-button" onClick={()=>onOpen(notice.owner_id,notice.task_id)}>查看详情</button></article>)}
    {notices?.length===50&&<p className="team-detail-muted">显示最近 50 条动态</p>}
  </section>
}
