import {useEffect,useId,useRef,useState,type KeyboardEvent,type RefObject} from 'react'
import {Check,ChevronDown,GitBranch,SlidersHorizontal} from 'lucide-react'
import {api} from './api'

export type ReplyModel={profile_id:string;name:string;model_id:string;brand:string;is_default:boolean;revision:number;available:boolean;vision?:boolean;reasoning?:boolean}
type SlashCommand={command:string;usage:string;description:string;mode:string}
type TaskOption={id:string;title:string;status:string;referenceable?:boolean}
type UserOption={principal_id:string;username:string;display_name:string}
type ReplyStrategy={id:string;label:string;description:string;default?:boolean;calls_model:boolean;scope:string}
export type ComposerOptions={slash_commands:SlashCommand[];tasks:TaskOption[];users:UserOption[];reply_strategies:ReplyStrategy[];reply_models:ReplyModel[]}
const brandLogos=import.meta.glob('./ai-brands/*.svg',{eager:true,query:'?url',import:'default'}) as Record<string,string>

export function useComposerOptions(conversationID?:string){
  const [options,setOptions]=useState<ComposerOptions>()
  const [error,setError]=useState('')
  useEffect(()=>{
    let active=true
    api<ComposerOptions>('/assistant/composer-options'+(conversationID?'?conversation_id='+encodeURIComponent(conversationID):'')).then(data=>{if(active)setOptions(data)}).catch(e=>{if(active)setError(e instanceof Error?e.message:'无法读取输入选项')})
    return()=>{active=false}
  },[conversationID])
  return {options,error}
}

type Suggestion={key:string;title:string;detail:string;value:string;badge:string;longTitle?:string;task?:TaskOption;disabled?:boolean}
export function useComposerSuggestions({value,setValue,input,options,canReference,onReference,onBenchmarkCommand}:{value:string;setValue:(text:string)=>void;input:RefObject<HTMLTextAreaElement|null>;options?:ComposerOptions;canReference:boolean;onReference?:(task:TaskOption)=>void;onBenchmarkCommand?:(body:string)=>void}){
  const id=useId()
  const [cursor,setCursor]=useState(0)
  const [dismissed,setDismissed]=useState(false)
  const [active,setActive]=useState(0)
  const before=value.slice(0,cursor)
  const match=before.match(/(?:^|\s)([\/@])([^\s\/@]*)$/)
  const trigger=match?.[1]
  const query=(match?.[2]??'').toLocaleLowerCase()
  const start=match?cursor-match[0].trimStart().length:cursor
  const end=match?cursor+(value.slice(cursor).match(/^[^\s\/@]*/)?.[0].length??0):cursor
  const contains=(text:string)=>text.toLocaleLowerCase().includes(query)
  const commandDescriptions:Record<string,string>={'/benchmark':'发起基准压测','/benchmark-status':'查询任务进度','/assistant':'助手工具指令'}
  const commands:Suggestion[]=trigger==='/'?(options?.slash_commands??[]).filter(item=>item.command.startsWith('/')&&contains(item.command+' '+item.description)).map(item=>({
    key:item.command,title:item.command,detail:commandDescriptions[item.command]??item.description,badge:'回车',value:item.command+' ',
  })):[]
  const references:Suggestion[]=trigger==='@'?[
    ...(options?.tasks??[]).filter(item=>contains(item.title+' '+item.id)).map(task=>{
      const shortID=task.id.length>12?task.id.slice(0,8)+'…':task.id
      const title='任务 · '+shortID+' '+task.title
      const longTitle='任务 · '+task.id+' '+task.title
      if(canReference&&task.referenceable)return {key:'task:'+task.id,title,longTitle,detail:'',badge:'只读引用',value:'',task}
      return {key:'task:'+task.id,title,longTitle,detail:'',badge:'文本提及',value:'@'+task.id+' '}
    }),
    ...(options?.users??[]).filter(item=>contains(item.username+' '+item.display_name)).map(user=>({
      key:'user:'+user.principal_id,title:'成员 · '+(user.display_name||user.username)+' (@'+user.username+')',detail:'',badge:'活跃',value:'@'+user.username+' ',
    })),
  ]:[]
  const matching=trigger==='/'?commands:references
  const visible=Boolean(match&&!dismissed)
  const enabled=matching.filter(item=>!item.disabled)
  const selected=enabled[Math.min(active,Math.max(0,enabled.length-1))]
  function updateCursor(element:HTMLTextAreaElement,reset=false){
    const position=element.selectionStart
    if(reset||position!==cursor){setDismissed(false);setActive(0)}
    setCursor(position)
  }
  function choose(item:Suggestion){
    if(item.disabled||!input.current||input.current.value!==value||input.current.selectionStart!==cursor)return
    if(item.key==='/benchmark'&&onBenchmarkCommand){
      onBenchmarkCommand(value.slice(0,start)+value.slice(end))
      setDismissed(true)
      return
    }
    const nextValue=value.slice(0,start)+item.value+value.slice(end)
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
    if((event.key==='Enter'||event.key==='Tab')&&!event.shiftKey&&!event.ctrlKey&&!event.metaKey&&!event.altKey){
      event.preventDefault();choose(selected);return true
    }
    return false
  }
  const optionID=(item:Suggestion)=>id+'-'+encodeURIComponent(item.key)
  const activeDescendant=visible&&selected?optionID(selected):undefined
  useEffect(()=>{
    if(!visible)return
    const dismissOutside=(event:Event)=>{
      if(event.target instanceof Node && event.target!==input.current && !document.getElementById(id+'-panel')?.contains(event.target))setDismissed(true)
    }
    document.addEventListener('pointerdown',dismissOutside)
    document.addEventListener('focusin',dismissOutside)
    return()=>{
      document.removeEventListener('pointerdown',dismissOutside)
      document.removeEventListener('focusin',dismissOutside)
    }
  },[id,input,visible])
  useEffect(()=>{
    if(activeDescendant)document.getElementById(activeDescendant)?.scrollIntoView({block:'nearest'})
  },[activeDescendant])
  const label=trigger==='/'?'/ 快捷指令':'@ 引用任务或成员'
  const list=visible&&<div className="composer-suggestions" id={id+'-panel'} data-trigger={trigger}>
    <div className="composer-suggestions-heading"><span id={id+'-heading'}>{label}</span><small>↑↓ 选择 · Tab/Enter 填充</small></div>
    <div className="composer-suggestions-list" id={id} role="listbox" aria-labelledby={id+'-heading'}>
      {matching.map(item=><button type="button" role="option" tabIndex={-1} id={optionID(item)} key={item.key} aria-selected={selected?.key===item.key} aria-label={item.longTitle?item.longTitle+' · '+item.badge:undefined} title={item.longTitle} disabled={item.disabled}
        onMouseDown={event=>event.preventDefault()} onClick={()=>choose(item)}><strong>{item.title}</strong>{item.detail&&<small>{item.detail}</small>}<span className="composer-suggestion-badge" data-badge={item.badge}>{item.badge}</span></button>)}
    </div>
    {!matching.length&&<p className="composer-suggestions-empty" role="status">{options?'没有匹配的选项':'正在读取选项…'}</p>}
  </div>
  return {list,keyDown,updateCursor,controls:visible?id:undefined,activeDescendant}
}

export function ReplyStrategy({options,error,strategy,onStrategy,onModel,openSettings,disabled,benchmarkCommand,benchmarkAdvisory}:{options?:ComposerOptions;error:string;strategy:'role_routing'|'deterministic_only';onStrategy:(strategy:'role_routing'|'deterministic_only')=>void;onModel:(model:ReplyModel)=>void;openSettings?:()=>void;disabled:boolean;benchmarkCommand?:boolean;benchmarkAdvisory?:boolean}){
  const [open,setOpen]=useState(false)
  const root=useRef<HTMLDivElement>(null)
  useEffect(()=>{
    if(!open)return
    const dismiss=(event:PointerEvent)=>{if(event.target instanceof Node&&!root.current?.contains(event.target))setOpen(false)}
    document.addEventListener('pointerdown',dismiss)
    return()=>document.removeEventListener('pointerdown',dismiss)
  },[open])
  return <div className="reply-strategy" ref={root} onKeyDown={event=>{if(event.key==='Escape'){setOpen(false);(event.currentTarget.querySelector('button') as HTMLButtonElement|null)?.focus()}}}>
    <button type="button" className="reply-strategy-trigger" aria-expanded={open} disabled={disabled} onClick={()=>setOpen(!open)}><GitBranch size={12}/><span>模型策略：{strategy==='role_routing'?(benchmarkAdvisory?'基准规划默认模型':'按角色路由'):'仅确定性查询'}</span><ChevronDown size={12}/></button>
    {open&&<div className="reply-strategy-menu">
      <small>模型策略（路由作用域）</small>
      {(options?.reply_strategies??[]).filter(item=>item.id==='role_routing'||item.id==='deterministic_only').map(item=><button type="button" key={item.id} className={strategy===item.id?'selected':''} onClick={()=>{onStrategy(item.id as 'role_routing'|'deterministic_only');setOpen(false)}}>
        {strategy===item.id&&<Check size={13}/>}<span title={item.description}>{item.id==='role_routing'&&benchmarkAdvisory?'基准规划使用默认模型':item.label}{item.id==='deterministic_only'&&!item.label.includes('不调模型')?'（不调模型）':''}</span><small className="reply-badge">{item.calls_model?'默认':'零消耗'}</small></button>)}
      {error&&<p className="error" role="alert">{error}</p>}
      {!options&&!error&&<p>正在读取策略…</p>}
      <small className="reply-model-heading">仅本轮回答临时指定模型</small>
      {options&&!options.reply_models.length&&<p>暂无可选模型</p>}
      {options?.reply_models.map(model=><button type="button" key={model.profile_id+':'+model.revision} disabled={!model.available||benchmarkCommand} onClick={()=>{onModel(model);setOpen(false)}}>
        {brandLogos['./ai-brands/'+model.brand+'.svg']&&<span className="reply-brand" data-brand={model.brand} aria-hidden="true" style={{maskImage:'url('+JSON.stringify(brandLogos['./ai-brands/'+model.brand+'.svg'])+')'}}/>}
        <span title={model.model_id}>{model.name}</span>{model.reasoning&&<small className="reply-badge">推理</small>}{model.vision&&<small className="reply-badge" data-capability="vision">识图 ✓</small>}{!model.available&&<small>凭据不可用</small>}</button>)}
      {openSettings&&<button type="button" className="reply-settings-link" onClick={()=>{setOpen(false);openSettings()}}><SlidersHorizontal size={12}/>角色路由与模型策略设置 →</button>}
    </div>}
  </div>
}
