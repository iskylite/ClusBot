import {useEffect,useLayoutEffect,useRef,useState} from 'react'
import {LoaderCircle} from 'lucide-react'
import {api,type Message,type MessagePage} from './api'
import {Messages} from './ExecutionText'

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

export function MessageHistory({conversationID,messages,live,liveRunID,remember,omitRunID}:{
  conversationID:string;messages:Message[];live?:string;liveRunID?:string;remember?:(id:number)=>void;omitRunID?:string
}) {
  const anchor=useMessageAnchor()
  const [page,setPage]=useState<MessagePage>()
  const [pending,setPending]=useState('')
  const [error,setError]=useState('')
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
      .catch(e=>{if(current===generation.current)setError(e instanceof Error?e.message:'历史消息未能加载')})
      .finally(()=>{if(current===generation.current)setPending('')})
    return()=>{generation.current++}
  },[conversationID,anchor])
  useEffect(()=>{
    setPage(current=>current?mergeLatest(current,messages):current)
  },[messages])
  useLayoutEffect(()=>{
    const saved=prepend.current
    if(!saved)return
    const node=document.getElementById(saved.id)
    if(node)window.scrollBy(0,node.getBoundingClientRect().top-saved.top)
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
    }catch(e){if(current===generation.current)setError(e instanceof Error?e.message:'历史消息未能加载')}
    finally{if(current===generation.current)setPending('')}
  }
  function latest() {
    history.replaceState(history.state,'',location.pathname+location.search)
    window.dispatchEvent(new Event('hashchange'))
  }
  const visible=(page?.items??(anchor?[]:messages)).filter(item=>!omitRunID||item.role!=='assistant'||item.run_id!==omitRunID)
  // stop saves the partial assistant message before the worker settles. Do not
  // repeat that same fragment in the live row while the stop is being applied.
  const last=visible.at(-1)
  const liveText=page?.after_cursor || (last?.incomplete&&last.run_id===liveRunID)?undefined:live
  return <div className="message-history" ref={root} aria-busy={Boolean(pending)}>
    {page?.before_cursor&&<button type="button" className="text-button history-message-more" disabled={Boolean(pending)} onClick={()=>void load('before')}>{pending==='before'?'正在加载…':'加载更早消息'}</button>}
    {pending==='initial'&&anchor&&<LoaderCircle size={18} className="spin" aria-label="正在加载来源消息"/>}
    {error&&<p className="error" role="alert">{error}</p>}
    <Messages messages={visible} live={liveText} remember={remember}/>
    {page?.after_cursor&&<div className="history-message-actions">
      <button type="button" className="text-button" disabled={Boolean(pending)} onClick={()=>void load('after')}>{pending==='after'?'正在加载…':'加载后续消息'}</button>
      <button type="button" className="text-button" onClick={latest}>回到最新消息</button>
    </div>}
  </div>
}
