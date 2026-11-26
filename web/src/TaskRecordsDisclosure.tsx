import {useEffect,useState} from 'react'
import {api} from './api'
import {AssistantSurfaceView} from './AssistantSurfaceView'
import {parseSurfaces,type Surface} from './assistantSurface'

export type TaskRecordLink={task_id:string;label:string}
type Origin={conversation:string;run:string;task:TaskRecordLink}

function RecordedTaskEvidence({conversation,run,task}:Origin) {
  const [surface,setSurface]=useState<Surface>()
  const [failed,setFailed]=useState(false)
  const taskID=task.task_id
  useEffect(()=>{
    let current=true
    const path='/conversations/'+encodeURIComponent(conversation)+'/task-card/'+encodeURIComponent(run)+'/records/'+encodeURIComponent(taskID)
    api<Record<string,unknown>>(path).then(result=>{
      const views=parseSurfaces(result?.surface)
      if(result?.conversation_id!==conversation||result?.run_id!==run||result?.task_id!==taskID||views?.length!==1||
        !/^assistant_[a-f\d]{32}$/.test(views[0].id)||views[0].data.size!==0||
        [...views[0].nodes.values()].some(node=>node.component!=='Column'&&(node.component!=='Text'||typeof node.text!=='string')))
        throw new Error('task record scope does not match the selected task')
      if(current)setSurface(views[0])
    }).catch(()=>{if(current)setFailed(true)})
    return()=>{current=false}
  },[conversation,run,taskID])
  if(surface)return <AssistantSurfaceView surface={surface}/>
  return <p role="status">{failed?'此任务的步骤与证据暂不可读。':'正在读取此任务的步骤与证据…'}</p>
}

export function TaskRecordsDisclosure(origin:Origin) {
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
    <summary onClick={event=>{event.preventDefault();setOpen(value=>!value)}}>步骤与证据：{origin.task.label}</summary>
    {open&&<RecordedTaskEvidence key={`${origin.conversation}:${origin.run}:${origin.task.task_id}`} {...origin}/>}
  </details>
}
