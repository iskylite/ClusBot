import {useEffect,useState} from 'react'
import {api,APIError} from './api'

type ConversationEvent = {id:string; run_id:string; kind:string; task_id?:string}
type EventPage = {events:ConversationEvent[];cursor:string;has_more:boolean}
export type TaskLink = {taskID:string; benchmark:boolean;sourceRunID?:string}

const isID=(id?:string):id is string=>Boolean(id && /^[a-f0-9]{32}$/.test(id))

export function useConversationTasks(conversationID?:string) {
  const [snapshot,setSnapshot]=useState<{conversationID:string;links:TaskLink[]}>()
  useEffect(()=>{
    if (!conversationID)return
    let disposed=false, cursor='', timer:number|undefined
    async function read() {
      if (disposed)return
      try {
        if (document.hidden)return
        const page=await api<EventPage>('/conversations/'+conversationID+'/events'+(cursor?'?cursor='+encodeURIComponent(cursor):''))
        if (disposed)return
        cursor=page.cursor
        const additions=page.events.flatMap(event=>isID(event.task_id)&&
          ['benchmark.admission_queued','run.state_changed'].includes(event.kind)
          ? [{taskID:event.task_id,benchmark:event.kind==='benchmark.admission_queued',sourceRunID:event.run_id}] : [])
        if(additions.length)setSnapshot(previous=>{
          const links=previous && previous.conversationID===conversationID?previous.links:[]
          const merged=new Map(links.map(link=>[link.taskID,link]))
          additions.forEach(link=>{
            const prior=merged.get(link.taskID)
            if(!prior || (!prior.benchmark&&link.benchmark)) merged.set(link.taskID,link)
          })
          return {conversationID:conversationID!,links:[...merged.values()].slice(-50)}
        })
        if (page.has_more) {
          timer=window.setTimeout(invoke,50)
          return
        }
      } catch(e) {
        if(disposed)return
        if (e instanceof APIError&&e.code==='conversation_event_cursor_reset') {
          cursor=''
          setSnapshot(undefined)
        }
      } finally {
        if (!disposed && timer===undefined) timer=window.setTimeout(()=>{
          timer=undefined
          void read()
        },3500)
      }
      // The next page can be fetched without waiting for the normal polling delay.
      // A page drained by read() is responsible for re-arming its own timer.
    }
    const invoke=()=>{timer=undefined;void read()}
    timer=window.setTimeout(invoke,0)
    return()=>{disposed=true;if(timer!==undefined)window.clearTimeout(timer)}
  },[conversationID])
  return conversationID&&snapshot?.conversationID===conversationID?snapshot.links:[]
}
