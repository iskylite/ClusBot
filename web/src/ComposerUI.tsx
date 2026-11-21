import {useEffect,useId,useRef,useState,type KeyboardEvent,type RefObject} from 'react'
import {Check,ChevronDown,GitBranch} from 'lucide-react'
import {api} from './api'

export type ReplyModel={profile_id:string;name:string;model_id:string;brand:string;is_default:boolean;revision:number;available:boolean}
type SlashCommand={command:string;usage:string;description:string;mode:string}
type TaskOption={id:string;title:string;status:string}
type UserOption={principal_id:string;username:string;display_name:string}
type ReplyStrategy={id:string;label:string;description:string;default?:boolean;calls_model:boolean;scope:string}
export type ComposerOptions={slash_commands:SlashCommand[];tasks:TaskOption[];users:UserOption[];reply_strategies:ReplyStrategy[];reply_models:ReplyModel[]}
const brandLogos=import.meta.glob('./ai-brands/*.svg',{eager:true,query:'?url',import:'default'}) as Record<string,string>

export function useComposerOptions(){
  const [options,setOptions]=useState<ComposerOptions>()
  const [error,setError]=useState('')
  useEffect(()=>{
    let active=true
    api<ComposerOptions>('/assistant/composer-options').then(data=>{if(active)setOptions(data)}).catch(e=>{if(active)setError(e instanceof Error?e.message:'无法读取输入选项')})
    return()=>{active=false}
  },[])
  return {options,error}
}

type Suggestion={key:string;title:string;detail:string;value:string;task?:TaskOption;disabled?:boolean}
export function useComposerSuggestions({value,setValue,input,options,canReference,onReference}:{value:string;setValue:(text:string)=>void;input:RefObject<HTMLTextAreaElement|null>;options?:ComposerOptions;canReference:boolean;onReference?:(task:TaskOption)=>void}){
  const id=useId()
  const [cursor,setCursor]=useState(0)
  const [dismissed,setDismissed]=useState(false)
  const [active,setActive]=useState(0)
  const before=value.slice(0,cursor)
  const match=before.match(/(?:^|\s)([\/@])([^\s\/@]*)$/)
  const trigger=match?.[1]
  const query=(match?.[2]??'').toLocaleLowerCase()
  const start=match?cursor-match[0].trimStart().length:cursor
  const suggestions:Suggestion[]=trigger==='/'?(options?.slash_commands??[]).filter(item=>(item.command+' '+item.description).toLocaleLowerCase().includes(query)).map(item=>({key:item.command,title:item.command,detail:item.description,value:item.command==='记住'?'记住：':item.command+' '})):
    trigger==='@'?[...(options?.tasks??[]).filter(item=>(item.title+' '+item.id).toLocaleLowerCase().includes(query)).map(task=>({key:'task:'+task.id,title:task.title,detail:canReference?'任务 · '+task.id:'请在已有对话中引用任务',value:'',task,disabled:!canReference})),
      ...(options?.users??[]).filter(item=>(item.username+' '+item.display_name).toLocaleLowerCase().includes(query)).map(user=>({key:'user:'+user.principal_id,title:user.display_name||user.username,detail:'@'+user.username+' · 文本提及',value:'@'+user.username+' '}))]:[]
  const visible=Boolean(match&&!dismissed)
  const enabled=suggestions.filter(item=>!item.disabled)
  const selected=enabled[Math.min(active,Math.max(0,enabled.length-1))]
  function updateCursor(element:HTMLTextAreaElement,reset=false){
    const position=element.selectionStart
    if(reset||position!==cursor){setDismissed(false);setActive(0)}
    setCursor(position)
  }
  function choose(item:Suggestion){
    if(item.disabled||!input.current||input.current.value!==value||input.current.selectionStart!==cursor)return
    const nextValue=value.slice(0,start)+item.value+value.slice(cursor)
    setValue(nextValue)
    if(item.task)onReference?.(item.task)
    setDismissed(true)
    requestAnimationFrame(()=>{
      if(!input.current||input.current.value!==nextValue)return
      input.current.focus();const position=start+item.value.length;input.current.setSelectionRange(position,position)
    })
  }
  function keyDown(event:KeyboardEvent<HTMLTextAreaElement>){
    if(!visible||event.nativeEvent.isComposing||event.nativeEvent.keyCode===229)return false
    if(event.key==='Escape'){event.preventDefault();setDismissed(true);return true}
    if(!enabled.length)return false
    if(event.key==='ArrowDown'||event.key==='ArrowUp'){
      event.preventDefault();setActive(current=>(current+(event.key==='ArrowDown'?1:-1)+enabled.length)%enabled.length);return true
    }
    if((event.key==='Enter'&&!event.shiftKey&&!event.ctrlKey&&!event.metaKey)||event.key==='Tab'){
      event.preventDefault();choose(selected);return true
    }
    return false
  }
  const list=visible&&<div className="composer-suggestions" id={id} role="listbox" aria-label={trigger==='/'?'快捷指令':'引用任务或成员'}>
    <div className="composer-suggestions-heading"><strong>{trigger==='/'?'/ 快捷指令':'@ 引用任务或成员'}</strong><small>方向键选择 · Tab 或 Enter 填充</small></div>
    <div className="composer-suggestions-list">{suggestions.length?suggestions.map(item=><button type="button" role="option" id={id+'-'+suggestions.indexOf(item)} key={item.key} aria-selected={selected?.key===item.key} disabled={item.disabled}
      onMouseDown={event=>event.preventDefault()} onClick={()=>choose(item)}><strong>{item.title}</strong><small>{item.detail}</small></button>):<p>{options?'没有匹配的选项':'正在读取选项…'}</p>}</div>
  </div>
  return {list,keyDown,updateCursor,controls:visible?id:undefined,activeDescendant:visible&&selected?id+'-'+suggestions.indexOf(selected):undefined}
}

export function ReplyStrategy({options,error,strategy,onStrategy,onModel,disabled}:{options?:ComposerOptions;error:string;strategy:'role_routing'|'deterministic_only';onStrategy:(strategy:'role_routing'|'deterministic_only')=>void;onModel:(model:ReplyModel)=>void;disabled:boolean}){
  const [open,setOpen]=useState(false)
  const root=useRef<HTMLDivElement>(null)
  useEffect(()=>{
    if(!open)return
    const dismiss=(event:PointerEvent)=>{if(event.target instanceof Node&&!root.current?.contains(event.target))setOpen(false)}
    document.addEventListener('pointerdown',dismiss)
    return()=>document.removeEventListener('pointerdown',dismiss)
  },[open])
  return <div className="reply-strategy" ref={root} onKeyDown={event=>{if(event.key==='Escape'){setOpen(false);(event.currentTarget.querySelector('button') as HTMLButtonElement|null)?.focus()}}}>
    <button type="button" className="reply-strategy-trigger" aria-expanded={open} disabled={disabled} onClick={()=>setOpen(!open)}><GitBranch size={15}/><span>模型策略：{strategy==='role_routing'?'按角色路由':'仅确定性查询'}</span><ChevronDown size={14}/></button>
    {open&&<div className="reply-strategy-menu">
      <small>模型策略</small>
      {(options?.reply_strategies??[]).filter(item=>item.id==='role_routing'||item.id==='deterministic_only').map(item=><button type="button" key={item.id} className={strategy===item.id?'selected':''} onClick={()=>{onStrategy(item.id as 'role_routing'|'deterministic_only');setOpen(false)}}>
        <span>{item.label}<small>{item.description}</small></span>{strategy===item.id&&<Check size={15}/>}</button>)}
      {error&&<p className="error" role="alert">{error}</p>}
      {!options&&!error&&<p>正在读取策略…</p>}
      <small className="reply-model-heading">仅发送这条文字给指定模型</small>
      {options&&!options.reply_models.length&&<p>暂无可选模型</p>}
      {options?.reply_models.map(model=><button type="button" key={model.profile_id+':'+model.revision} disabled={!model.available} onClick={()=>{onModel(model);setOpen(false)}}>
        {brandLogos['./ai-brands/'+model.brand+'.svg']&&<img className="reply-brand" src={brandLogos['./ai-brands/'+model.brand+'.svg']} alt=""/>}
        <span>{model.name}<small>{model.model_id}{!model.available?' · 凭据不可用':''}</small></span></button>)}
    </div>}
  </div>
}
