import {useEffect,useRef,useState} from 'react'
import {Activity,Check} from 'lucide-react'
import {api,APIError} from './api'
import {AssistantSurfaceView} from './AssistantSurfaceView'
import {benchmarkAdmissionAction,parseSurfaceReceipt,type Surface,type SurfaceAction} from './assistantSurface'
import './assistant-cards.css'
import './BenchmarkDispatchControl.css'

type ViewState='ready'|'pending'|'previewed'|'clarify'|'not_completed'|'stale'|'unavailable'|'accepted'
type Issue={code:string;target?:string;detail:string}
type Preview={preview_id:string;asset_count:number;can_queue:boolean;facility_status:'not_verified';execution_started:false;
  cases:{key:string;blockers:Issue[];warnings:Issue[]}[]}
type View={state:ViewState;target?:string;profile?:{profile:string;version:number};surface?:Surface;preview?:Preview;task?:string}

function object(value:unknown):value is Record<string,unknown>{return typeof value==='object'&&value!==null&&!Array.isArray(value)}
function id(value:unknown):value is string{return typeof value==='string'&&/^[a-f0-9]{32}$/.test(value)}
function text(value:unknown):value is string{return typeof value==='string'&&value.length>0&&value.length<=1000}

function issueList(value:unknown):value is Issue[]{
  return Array.isArray(value)&&value.length<=256&&value.every(item=>object(item)&&text(item.code)&&text(item.detail)&&(item.target===undefined||text(item.target)))
}

function checkedAdmission(raw:unknown,conversation:string,run:string):View{
  const bad=()=>new Error('无法核对原基准建议与回执。')
  if(!object(raw)||raw.conversation_id!==conversation||raw.run_id!==run||
    !['ready','pending','previewed','clarify','not_completed','stale','unavailable','accepted'].includes(String(raw.state)))throw bad()
  const state=raw.state as ViewState
  const view:View={state}
  if(state==='ready'||state==='pending'||state==='previewed'||state==='clarify'){
    if(!text(raw.target)||!object(raw.profile)||!text(raw.profile.profile)||!Number.isSafeInteger(raw.profile.version)||Number(raw.profile.version)<1)throw bad()
    view.target=raw.target;view.profile={profile:raw.profile.profile,version:Number(raw.profile.version)}
  }
  if(state==='ready'){
    const surfaces=parseSurfaceReceipt(raw,conversation)
    const surface=surfaces?.[0], button=surface?.nodes.get('preview_admission')
    if(!surface||surfaces?.length!==1||button?.component!=='Button'||button.action.event.name!==benchmarkAdmissionAction||
      button.action.event.context?.source_run_id!==run||[...surface.nodes.values()].filter(node=>node.component==='Button').length!==1)throw bad()
    view.surface=surface
  }else if(raw.surface!==undefined)throw bad()
  if(state==='previewed'){
    const p=raw.preview
    if(!object(p)||!id(p.preview_id)||!Number.isSafeInteger(p.asset_count)||Number(p.asset_count)<1||Number(p.asset_count)>64||
      typeof p.can_queue!=='boolean'||p.facility_status!=='not_verified'||p.execution_started!==false||!Array.isArray(p.cases)||
      p.cases.length>64||p.cases.length===0||p.cases.some(item=>!object(item)||!text(item.key)||!issueList(item.blockers)||!issueList(item.warnings)))throw bad()
    view.preview=p as Preview
  }else if(raw.preview!==undefined)throw bad()
  if(state==='accepted'){
    if(!id(raw.task_id))throw bad()
    view.task=raw.task_id
  }else if(raw.task_id!==undefined)throw bad()
  return view
}

export function BenchmarkAdmissionCard({conversation,run,archived,onAccepted}:{conversation:string;run:string;archived:boolean;onAccepted:(task:string)=>void}){
  const base=id(conversation)&&id(run)?'/conversations/'+conversation+'/a2ui/benchmark-admission/'+run:undefined
  const [view,setView]=useState<View>()
  const [busy,setBusy]=useState(false)
  const [error,setError]=useState('')
  const [uncertain,setUncertain]=useState<'preview'|'confirm'>()
  const lock=useRef(false)
  const mounted=useRef(false)
  const key=useRef<string|undefined>(undefined)
  const focus=useRef(false)
  const heading=useRef<HTMLHeadingElement>(null)

  useEffect(()=>{
    mounted.current=true
    void read()
    return()=>{mounted.current=false}
  },[base])
  useEffect(()=>{
    if(focus.current&&view){focus.current=false;heading.current?.focus({preventScroll:true})}
  },[view])

  async function runAction(go:()=>Promise<void>){
    if(!base||lock.current)return
    lock.current=true;setBusy(true);setError('')
    try{await go()}finally{lock.current=false;if(mounted.current)setBusy(false)}
  }
  async function read(){
    await runAction(async()=>{
      if(!base)return
      try{
        const value=checkedAdmission(await api<unknown>(base),conversation,run)
        if(!mounted.current)return
        if(uncertain==='confirm'&&value.state==='previewed'&&view?.preview?.preview_id!==value.preview?.preview_id)
          throw new Error('different preview')
        setView(value)
        if(uncertain){
          focus.current=true
          if(value.state==='ready'||value.state==='previewed'&&uncertain==='confirm')
            setError('暂未发现回执。如需继续，可按原请求重试；不会自动重发。')
        }
        if(value.state!=='pending')setUncertain(undefined)
        if(value.task&&uncertain==='confirm')onAccepted(value.task)
      }catch(e){
        if(!mounted.current)return
        setView(undefined)
        setError(e instanceof APIError&&e.status===503?'当前未启用基准受理，未创建任务。':'暂不能核对原建议或回执；不会自动重发。')
      }
    })
  }
  function request(event:SurfaceAction){
    if(!base||archived||uncertain||view?.state!=='ready'||event.action.name!==benchmarkAdmissionAction||
      event.action.context.source_run_id!==run||event.action.surfaceId!==view.surface?.id)return
    void runAction(async()=>{
      try{
        key.current??=crypto.randomUUID()
        const next=checkedAdmission(await api<unknown>(base,event,'POST',{'X-ClusBot-Request-Key':key.current}),conversation,run)
        if(next.state!=='previewed'&&next.state!=='clarify')throw new Error('unmatched preview')
        if(mounted.current){focus.current=true;setView(next)}
      }catch{
        if(mounted.current){setUncertain('preview');setError('未收到受理预览。请核对原回执，不会自动重发。')}
      }
    })
  }
  function confirm(){
    if(archived||uncertain||view?.state!=='previewed'||!view.preview?.can_queue)return
    const original=view.preview
    void runAction(async()=>{
      try{
        // This is a separate fixed human confirmation, not an A2UI action.
        const result=await api<unknown>('/domain/benchmark-admission/preview/'+original.preview_id+'/confirm',{action:'accept_benchmark_admission'})
        if(!object(result)||result.preview_id!==original.preview_id||result.state!=='accepted'||!id(result.task_id))throw new Error('not the original receipt')
        if(mounted.current){focus.current=true;setView({state:'accepted',task:result.task_id});onAccepted(result.task_id)}
      }catch{
        if(mounted.current){setUncertain('confirm');setError('未收到入队回执。请先核对原结果，不会自动重发。')}
      }
    })
  }

  return <section className="conversation-tasks benchmark-conversation" aria-label="基准受理" aria-busy={busy}>
    <header className="benchmark-conversation-heading"><h2 ref={heading} tabIndex={-1}><Activity size={15} aria-hidden="true"/>基准受理</h2></header>
    {view?.target&&view.profile&&<p>{view.target} · {view.profile.profile} · 方案第 {view.profile.version} 版</p>}
    {view?.state==='ready'&&view.surface&&!archived&&<div className="benchmark-conversation-actions">
      <AssistantSurfaceView key={view.surface.id} surface={view.surface} busy={busy||Boolean(uncertain)} onAgentAction={request}/>
    </div>}
    {view?.preview&&<><p role="status">受理预览已保存。没有创建任务，也未启动测试。</p>
      <p>已冻结 {view.preview.asset_count} 个资产目标，不代表当前可用节点或 GPU 数。</p>
      <p>{view.preview.can_queue?'通过本地入队检查；未核对现场容量。':'当前不能排队，请先检查预检。'}</p>
      {view.preview.cases.flatMap(item=>[...item.blockers,...item.warnings].map((issue,index)=>
        <p className="muted" key={item.key+':'+issue.code+':'+index}>{item.key}：{issue.detail}{issue.target&&'（'+issue.target+'）'}</p>))}
      {!archived&&<div className="benchmark-conversation-actions"><button type="button" className="primary"
        disabled={busy||Boolean(uncertain)||!view.preview.can_queue} onClick={confirm}><Check size={13} aria-hidden="true"/>受理并加入队列</button></div>}
    </>}
    {view?.state==='accepted'&&<p role="status">任务已进入队列，尚不代表执行已启动。</p>}
    {view?.state==='pending'&&<p role="status">本次预览仍在核对，没有创建任务。</p>}
    {view?.state==='clarify'&&<p role="status">请在原对话补充确认测试范围，未生成预览或创建任务。</p>}
    {view?.state==='not_completed'&&<p role="status">本次预览没有完成，未创建任务。请重新发起基准建议。</p>}
    {view?.state==='stale'&&<p role="alert">原方案绑定、授权或有效期已变，请重新发起基准建议。没有创建任务。</p>}
    {(view?.state==='unavailable'||archived&&view?.state==='ready')&&<p>对话已归档，不能发起操作。</p>}
    {!view&&!error&&<p role="status">正在读取原建议…</p>}
    {error&&<p role="alert" className="error">{error}</p>}
    {(uncertain||view?.state==='pending'||error&&!view)&&<button type="button" className="outline" disabled={busy} onClick={()=>void read()}>核对受理回执</button>}
  </section>
}
