import {useEffect,useState} from 'react'
import {Gauge} from 'lucide-react'
import {api} from './api'
import {AssistantSurfaceView} from './AssistantSurfaceView'
import {parseSurfaces,type Surface} from './assistantSurface'

type Origin={conversation:string;run:string;message:number}

function RecordedRunModelUsage({conversation,run,message}:Origin) {
  const [surface,setSurface]=useState<Surface>()
  const [failed,setFailed]=useState(false)
  useEffect(()=>{
    let current=true
    const path='/conversations/'+encodeURIComponent(conversation)+'/runs/'+encodeURIComponent(run)+'/model-receipt/'+message
    api<Record<string,unknown>>(path).then(result=>{
      const receipt=parseSurfaces(result?.surface)
      if(result?.conversation_id!==conversation||result?.run_id!==run||result?.message_id!==message||receipt?.length!==1||
        !/^assistant_[a-f\d]{32}$/.test(receipt[0].id)||receipt[0].data.size!==0||
        [...receipt[0].nodes.values()].some(node=>node.component!=='Column'&&(node.component!=='Text'||typeof node.text!=='string')))
        throw new Error('model receipt scope does not match the original reply')
      if(current)setSurface(receipt[0])
    }).catch(()=>{if(current)setFailed(true)})
    return()=>{current=false}
  },[conversation,run,message])
  if(surface)return <AssistantSurfaceView surface={surface}/>
  return <p role="status">{failed?'本条回复的模型与用量暂不可读。':'正在读取本轮模型记录…'}</p>
}

export function RunModelUsageDisclosure(origin:Origin) {
  const [open,setOpen]=useState(false)
  useEffect(()=>{
    const hide=()=>{if(document.hidden)setOpen(false)}
    const dismiss=()=>setOpen(false)
    document.addEventListener('visibilitychange',hide)
    window.addEventListener('pagehide',dismiss)
    return()=>{
      document.removeEventListener('visibilitychange',hide)
      window.removeEventListener('pagehide',dismiss)
    }
  },[])
  return <details className="execution-usage" open={open}>
    <summary onClick={event=>{
      // Summary preserves native keyboard semantics. A discrete click lets
      // rapid closing and reopening discard, rather than reuse, a loaded view.
      event.preventDefault();setOpen(value=>!value)
    }}><Gauge size={16} aria-hidden="true"/>模型与用量</summary>
    {open&&<RecordedRunModelUsage key={`${origin.conversation}:${origin.run}:${origin.message}`} {...origin}/>}
  </details>
}
