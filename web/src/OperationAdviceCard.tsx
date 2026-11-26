import {useCallback,useEffect,useRef,useState} from 'react'
import {api} from './api'
import {AssistantCard} from './AssistantCards'
import {AssistantSurfaceView} from './AssistantSurfaceView'
import {isAssistantItem,parseSurfaceReceipt,type AssistantCardItem,type Surface,type SurfaceAction} from './assistantSurface'

type ActionState='ready'|'pending'|'recorded'|'not_completed'|'unavailable'
type AdviceFact=AssistantCardItem|{state:'failed';tool:string;message:string}
type AdviceView={surface:Surface;state:ActionState;choice?:{slot:string;decision:string};items:AdviceFact[];factsUnavailable:boolean}
const validID=(value:string)=>/^[a-f0-9]{32}$/.test(value)
const object=(value:unknown):value is Record<string,unknown>=>Boolean(value&&typeof value==='object'&&!Array.isArray(value))
const opFacts=new Set(['get_operation_evidence','get_cluster_inventory','get_token_usage_stats','get_incident_workflow','get_task'])
function checkedAdvice(raw:unknown,conversation:string,run:string):AdviceView {
  const invalid=()=>new Error('无法核对这条会话建议及回执。')
  if(!object(raw)||raw.run_id!==run||!['ready','pending','recorded','not_completed','unavailable'].includes(String(raw.state)))throw invalid()
  const surfaces=parseSurfaceReceipt(raw,conversation)
  if(surfaces?.length!==1)throw invalid()
  const surface=surfaces[0], state=raw.state as ActionState
  const actions=[...surface.nodes.values()].filter(node=>node.component==='Button')
  if(actions.length>4||state==='ready'&&actions.length<1||state!=='ready'&&actions.length!==0||
    actions.some(node=>node.action.event.name!=='review_operation_advice_choice'))throw invalid()
  let choice:AdviceView['choice']
  if(state==='recorded'){
    if(!object(raw.choice)||typeof raw.choice.slot!=='string'||!/^[1-4]$/.test(raw.choice.slot)||
      !['record_selection','request_details'].includes(String(raw.choice.decision))||
      !Number.isSafeInteger(raw.choice.recorded_at)||Number(raw.choice.recorded_at)<1)throw invalid()
    choice={slot:raw.choice.slot,decision:String(raw.choice.decision)}
  } else if(raw.choice!==undefined)throw invalid()
  const facts=raw.items??[]
  if(!Array.isArray(facts)||facts.length>5||facts.some(item=>!object(item)||typeof item.tool!=='string'||!opFacts.has(item.tool)||
    (item.state==='failed'?typeof item.message!=='string'||item.message.length>1000:item.state!=='completed'||!isAssistantItem(item))))throw invalid()
  return {surface,state,choice,items:facts as AdviceFact[],factsUnavailable:raw.facts_unavailable===true}
}

export function OperationAdviceCard({conversation,run,navigate}:{conversation:string;run:string;navigate:(path:string)=>void}){
  const path=validID(conversation)&&validID(run)?'/conversations/'+conversation+'/operation-advice/'+run:undefined
  const [view,setView]=useState<AdviceView>()
  const [error,setError]=useState('')
  const [uncertain,setUncertain]=useState(false)
  const [busy,setBusy]=useState(false)
  const [epoch,setEpoch]=useState(0)
  const mounted=useRef(false)
  const lock=useRef(false)
  const refresh=useCallback(async()=>{
    if(!path||lock.current)return
    lock.current=true;setBusy(true)
    try{
      const value=checkedAdvice(await api<unknown>(path),conversation,run)
      if(mounted.current){
        setView(value);setEpoch(n=>n+1)
        setError(uncertain&&value.state==='ready'?'尚未找到选择记录。不会自动重发。':'')
      }
    }catch{
      if(mounted.current){setView(undefined);setError('暂不能读取原建议，未发起运维操作。')}
    }finally{lock.current=false;if(mounted.current)setBusy(false)}
  },[path,conversation,run,uncertain])
  const initialRefresh=useRef(refresh)
  initialRefresh.current=refresh
  useEffect(()=>{
    mounted.current=true
    void initialRefresh.current()
    return()=>{mounted.current=false}
  },[path])

  function choose(action:SurfaceAction){
    if(!path||lock.current||view?.state!=='ready'||uncertain||action.action.name!=='review_operation_advice_choice'||action.action.surfaceId!==view.surface.id)return
    lock.current=true;setBusy(true);setError('')
    void (async()=>{
      try{
        const result=checkedAdvice(await api<unknown>(path+'/actions',action,'POST',{'X-ClusBot-Request-Key':crypto.randomUUID()}),conversation,run)
        if(result.state!=='recorded'||result.choice?.slot!==action.action.context.advice_slot)throw new Error('choice mismatch')
        if(mounted.current){setView(result);setEpoch(n=>n+1)}
      }catch{
        if(mounted.current){setUncertain(true);setError('未收到选择回执。请核对原建议，不会自动重发。')}
      }finally{lock.current=false;if(mounted.current)setBusy(false)}
    })()
  }

  const selected=view?.choice?view.surface.nodes.get('option_'+view.choice.slot):undefined
  const selectedLabel=selected?.component==='Text'&&typeof selected.text==='string'?selected.text:undefined
  return <>
    <article className="assistant-card" aria-label="运维建议选择" aria-busy={busy}>
      <h3>运维建议</h3>
      {view&&<AssistantSurfaceView key={view.surface.id} surface={view.surface} busy={busy||uncertain}
        onAgentAction={view.state==='ready'&&!uncertain?choose:undefined}/>}
      {view?.state==='recorded'&&<p role="status">已记录选择{selectedLabel?'：'+selectedLabel:''}。未批准或执行任何运维操作。{view.choice?.decision==='request_details'&&'需要更多信息时，请在原对话继续说明。'}</p>}
      {view?.state==='pending'&&<p role="status">选择尚未确认。请核对原回执，不会自动重发。</p>}
      {view?.state==='not_completed'&&<p role="status">未记录选择。可在原对话重新提出建议。</p>}
      {view?.state==='unavailable'&&<p>建议已失效或对话已归档，只可查看。</p>}
      {error&&<p role="alert" className="error">{error}</p>}
      {(error||view?.state==='pending')&&<button type="button" className="outline" disabled={busy} onClick={()=>void refresh()}>核对选择回执</button>}
      {!view&&!error&&<p className="muted">正在读取原建议…</p>}
    </article>
    {view?.factsUnavailable&&<p className="muted">暂不能刷新关联事实。历史建议不代表现场状态。</p>}
    {view?.items.map((item,index)=>!Array.isArray(item)&&item.state==='failed'?
      <p key={index} className="muted">{item.message}</p>:
      <AssistantCard key={epoch+':'+index} item={item} navigate={navigate} conversationReadOnly/>)}
  </>
}
