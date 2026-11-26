import {Fragment,useEffect,useLayoutEffect,useRef,useState,type ReactNode} from 'react'
import {LoaderCircle} from 'lucide-react'
import {api,type Material,type Message,type MessagePage} from './api'
import {Messages,type UserIdentity} from './ExecutionText'
import type {QuoteMessage} from './MessageActions'
import {MCPSetupCard} from './MCPSetupCard'
import {OperationAdviceCard} from './OperationAdviceCard'
import {BenchmarkAdmissionCard} from './BenchmarkAdmissionCard'
import {ArtifactPreviewCard,AssetLedgerCard,BenchmarkComparisonCard,DiscoveryScheduleCard,AttentionListCard,SkillBindingPreviewCard,TaskListProgressCard} from './TaskListProgressCard'
import {FeedbackCallout} from './Feedback'

const anchorFromLocation=()=>{
  const match=location.hash.match(/^#message-(\d+)$/)
  return match?Number(match[1]):undefined
}

export function useMessageAnchor() {
  const [anchor,setAnchor]=useState(anchorFromLocation)
  useEffect(()=>{
    const changed=()=>setAnchor(anchorFromLocation())
    for(const event of ['hashchange','popstate','clusbot:navigate'])window.addEventListener(event,changed)
    return()=>{for(const event of ['hashchange','popstate','clusbot:navigate'])window.removeEventListener(event,changed)}
  },[])
  return anchor
}

function mergeMessages(previous:Message[],incoming:Message[]) {
  const byID=new Map(previous.map(item=>[item.id,item]))
  let changed=false
  for(const item of incoming) {
    if(JSON.stringify(byID.get(item.id))!==JSON.stringify(item)) {
      byID.set(item.id,item)
      changed=true
    }
  }
  return changed?[...byID.values()].sort((a,b)=>(a.id??0)-(b.id??0)):previous
}

function mergeLatest(page:MessagePage,messages:Message[]):MessagePage {
  // A source window can be separated from the newest window by an unknown gap.
  const incoming=page.after_cursor?messages.filter(item=>page.items.some(old=>old.id===item.id)):messages
  const items=mergeMessages(page.items,incoming)
  return items===page.items?page:{...page,items,before_cursor:page.before_cursor?items[0]?.id:undefined}
}

export type LocalMessageExchange = {id:string;afterMessageID:number;content:ReactNode}

export function MessageHistory({conversationID,messages,live,liveReasoning,liveThinking,liveRunID,remember,omitRunID,omitAutomaticRevisionRunID,replyCard,previewMaterials,assistantName,userAccount,localExchanges=[],mcpSetup=false,personalReadCards=false,adviceNavigate,admissionArchived=false,admissionAccepted,messageActions,onQuoteMessage}:{
  conversationID:string;messages:Message[];live?:string;liveReasoning?:string;liveThinking?:boolean;liveRunID?:string;remember?:(id:number)=>void;omitRunID?:string;omitAutomaticRevisionRunID?:string;replyCard?:{runID:string;content:ReactNode};previewMaterials?:Material[];assistantName?:string;userAccount?:UserIdentity;localExchanges?:LocalMessageExchange[];mcpSetup?:boolean;personalReadCards?:boolean;adviceNavigate?:(path:string)=>void;admissionArchived?:boolean;admissionAccepted?:(task:string,run:string)=>void
  messageActions?:boolean;onQuoteMessage?:QuoteMessage
}) {
  const anchor=useMessageAnchor()
  const [page,setPage]=useState<MessagePage>()
  const [pending,setPending]=useState('')
  const [error,setError]=useState('')
  const [failedDirection,setFailedDirection]=useState<'initial'|'before'|'after'>('initial')
  const [attempt,setAttempt]=useState(0)
  const generation=useRef(0)
  const latestMessages=useRef(messages)
  latestMessages.current=messages
  const root=useRef<HTMLDivElement>(null)
  const prepend=useRef<{id:string;top:number}|undefined>(undefined)
  useEffect(()=>{
    const current=++generation.current
    setPage(undefined);setError('');setPending('initial')
    api<MessagePage>('/conversations/'+conversationID+'/history'+(anchor?'?around='+anchor:''))
      .then(result=>{if(current===generation.current)setPage(mergeLatest(result,latestMessages.current))})
      .catch(()=>{if(current===generation.current){setFailedDirection('initial');setError('历史消息暂时无法加载，请重试。')}})
      .finally(()=>{if(current===generation.current)setPending('')})
    return()=>{generation.current++}
  },[conversationID,anchor,attempt])
  useEffect(()=>{
    setPage(current=>current?mergeLatest(current,messages):current)
  },[messages])
  useLayoutEffect(()=>{
    const saved=prepend.current
    if(!saved)return
    const node=document.getElementById(saved.id)
    if(node){
      const delta=node.getBoundingClientRect().top-saved.top
      const scroller=root.current?.closest<HTMLElement>('.chat-log')
      if(scroller && scroller.scrollHeight>scroller.clientHeight)scroller.scrollTop+=delta
      else window.scrollBy(0,delta)
    }
    prepend.current=undefined
  },[page])
  async function load(direction:'before'|'after') {
    const cursor=direction==='before'?page?.before_cursor:page?.after_cursor
    if(!cursor || pending)return
    const current=generation.current
    setPending(direction);setError('')
    try {
      const result=await api<MessagePage>('/conversations/'+conversationID+'/history?'+direction+'='+cursor)
      if(current!==generation.current)return
      if(direction==='before') {
        const first=root.current?.querySelector<HTMLElement>('.message[id]')
        if(first)prepend.current={id:first.id,top:first.getBoundingClientRect().top}
      }
      setPage(old=>old?{
        items:mergeMessages(old.items,result.items),
        before_cursor:direction==='before'?result.before_cursor:old.before_cursor,
        after_cursor:direction==='after'?result.after_cursor:old.after_cursor,
      }:result)
    }catch{if(current===generation.current){setFailedDirection(direction);setError(direction==='before'?'更早消息暂时无法加载，请重试。':'后续消息暂时无法加载，请重试。')}}
    finally{if(current===generation.current)setPending('')}
  }
  function latest() {
    history.replaceState(history.state,'',location.pathname+location.search)
    window.dispatchEvent(new Event('hashchange'))
  }
  const visible=(page?.items??(anchor?[]:messages)).filter(item=>(!omitRunID||item.role!=='assistant'||item.run_id!==omitRunID)&&
    !(item.run_id===omitAutomaticRevisionRunID&&item.role==='user'&&item.text==='查看刚才标注的基准修订预演'))
  // stop saves the partial assistant message before the worker settles. Do not
  // repeat that same fragment in the live row while the stop is being applied.
  const last=visible.at(-1)
  const suppressLive=Boolean(page?.after_cursor || (last?.incomplete&&last.run_id===liveRunID))
  const liveText=suppressLive?undefined:live
  const activeReasoning=suppressLive?undefined:liveReasoning
  const activeThinking=suppressLive?false:liveThinking
  // Tool receipts are local to this open chat. Record the last persisted
  // message seen at receipt time so later persisted messages do not move in
  // front of an earlier card. History cursors still govern persisted messages.
  const segments:{key:string;items:Message[];exchange?:LocalMessageExchange}[]=[]
  let start=0, key='history-start'
  const cards=mcpSetup?visible.filter(m=>m.role==='assistant'&&m.mcp_onboarding&&m.id&&m.run_id).map(m=>({
    id:'mcp:'+m.run_id,afterMessageID:m.id!,content:<MCPSetupCard conversation={conversationID} run={m.run_id!}/>
  })):[]
  const readCards=personalReadCards?visible.flatMap(m=>{
    if(m.role!=='assistant'||!m.id||!m.run_id)return []
    if(m.assistant_task_list)return [{id:'tasks:'+m.run_id,afterMessageID:m.id,content:<TaskListProgressCard conversation={conversationID} run={m.run_id}/>}]
    if(m.assistant_asset_list)return [{id:'assets:'+m.run_id,afterMessageID:m.id,content:<AssetLedgerCard conversation={conversationID} run={m.run_id}/>}]
    if(m.assistant_attention_list)return [{id:'attention:'+m.run_id,afterMessageID:m.id,content:<AttentionListCard conversation={conversationID} run={m.run_id}/>}]
    if(m.assistant_skill_options)return [{id:'skills:'+m.run_id,afterMessageID:m.id,content:<SkillBindingPreviewCard conversation={conversationID} run={m.run_id}/>}]
    if(m.assistant_benchmark_comparison)return [{id:'comparison:'+m.run_id,afterMessageID:m.id,content:<BenchmarkComparisonCard conversation={conversationID} run={m.run_id}/>}]
    if(m.assistant_discovery_schedule)return [{id:'schedule:'+m.run_id,afterMessageID:m.id,content:<DiscoveryScheduleCard conversation={conversationID} run={m.run_id}/>}]
    if(m.assistant_artifact_list)return [{id:'artifact:'+m.run_id,afterMessageID:m.id,content:<ArtifactPreviewCard conversation={conversationID} run={m.run_id}/>}]
    return []
  }):[]
  const adviceCards=adviceNavigate?visible.filter(m=>m.role==='assistant'&&m.assistant_operation_advice&&m.id&&m.run_id).map(m=>({
    id:'advice:'+m.run_id,afterMessageID:m.id!,content:<OperationAdviceCard conversation={conversationID} run={m.run_id!} navigate={adviceNavigate}/>
  })):[]
  const admissionCards=admissionAccepted?visible.filter(m=>m.role==='assistant'&&m.benchmark_admission&&m.id&&m.run_id).map(m=>({
    id:'benchmark-admission:'+m.run_id,afterMessageID:m.id!,content:<BenchmarkAdmissionCard conversation={conversationID}
      run={m.run_id!} archived={admissionArchived} onAccepted={task=>admissionAccepted(task,m.run_id!)}/>
  })):[]
  for(const exchange of [...localExchanges,...cards,...readCards,...adviceCards,...admissionCards].sort((a,b)=>a.afterMessageID-b.afterMessageID)) {
    const next=visible.findIndex((message,index)=>index>=start&&message.id!==undefined&&message.id>exchange.afterMessageID)
    const end=next<0?visible.length:next
    segments.push({key,items:visible.slice(start,end),exchange})
    start=end
    key='history-after-'+exchange.id
  }
  segments.push({key,items:visible.slice(start)})
  const visibleReply=page?.after_cursor||anchor?undefined:replyCard
  const matchingReply=visibleReply?[...visible].reverse().find(message=>message.role==='assistant'&&message.run_id===visibleReply.runID):undefined
  return <div className="message-history" ref={root} aria-busy={Boolean(pending)}>
    {page?.before_cursor&&<button type="button" className="text-button history-message-more" disabled={Boolean(pending)} onClick={()=>void load('before')}>{pending==='before'?'正在加载…':'加载更早消息'}</button>}
    {pending==='initial'&&anchor&&<LoaderCircle size={18} className="spin" aria-label="正在加载来源消息"/>}
    {error&&<FeedbackCallout tone="error" compact role="alert" message={error}
      actions={<button type="button" onClick={()=>failedDirection==='initial'?setAttempt(value=>value+1):void load(failedDirection)}>重试</button>}/>}
    {segments.map((segment,index)=><Fragment key={segment.key}>
      <Messages messages={segment.items} live={index===segments.length-1?liveText:undefined}
        liveReasoning={index===segments.length-1?activeReasoning:undefined}
        liveThinking={index===segments.length-1?activeThinking:false}
        remember={remember}
        messageActions={messageActions} onQuoteMessage={onQuoteMessage}
        replyCard={matchingReply?segment.items.includes(matchingReply)?visibleReply:undefined:index===segments.length-1?visibleReply:undefined}
        previewMaterials={previewMaterials} assistantName={assistantName} userAccount={userAccount}
        memoryConversation={personalReadCards?conversationID:undefined}
        modelReceiptConversation={userAccount?conversationID:undefined}/>
      {segment.exchange&&<div className="messages">{segment.exchange.content}</div>}
    </Fragment>)}
    {page?.after_cursor&&<div className="history-message-actions">
      <button type="button" className="text-button" disabled={Boolean(pending)} onClick={()=>void load('after')}>{pending==='after'?'正在加载…':'加载后续消息'}</button>
      <button type="button" className="text-button" onClick={latest}>回到最新消息</button>
    </div>}
  </div>
}
