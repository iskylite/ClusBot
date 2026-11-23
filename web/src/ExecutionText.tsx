import {memo,useEffect,useMemo,useRef,useState,type ReactNode} from 'react'
import {Bookmark,BookmarkPlus,Gauge} from 'lucide-react'
import Markdown, {type Components} from 'react-markdown'
import remarkGfm from 'remark-gfm'
import {MessageAttachments,MessageContent,MessageReference} from './MessageContent'
import clusbotIcon from './clusbot-brand.svg'
import type {ExecutionUsage,Material,MemoryRef,Message} from './api'

const usageNumber = new Intl.NumberFormat('zh-CN', {maximumFractionDigits:1})

export function UsageDetails({current,cumulative,running,contextWindow,title='执行用量'}:{current?:ExecutionUsage;cumulative?:ExecutionUsage;running:boolean;contextWindow?:number;title?:string}) {
  if(!current?.model_requests && !current?.active_ms && !cumulative?.model_requests && !cumulative?.untracked_runs) return null
  function usageRow(label:string, usage:ExecutionUsage) {
    const duration=usage.active_ms<1000 ? `${usage.active_ms} 毫秒` : `${usageNumber.format(usage.active_ms/1000)} 秒`
    const lowerBound=running || usage.time_partial || usage.untracked_runs>0
    const tokens=(count:number|null)=>count===null?'未知':usageNumber.format(count)
    return <div className="usage-row">
      <strong>{label}</strong>
      <span>{usage.untracked_runs>0?'已记录 ':''}{usage.model_requests} 次模型尝试 · {lowerBound?'至少 ':''}{duration}</span>
      {usage.model_requests>0 && <span>Token 输入 {tokens(usage.input_tokens)} / 输出 {tokens(usage.output_tokens)}
        {usage.unreported_requests>0 && (usage.input_tokens!==null || usage.output_tokens!==null) && '（有请求未完整报告）'}</span>}
      {usage.untracked_runs>0 && <span>{usage.untracked_runs} 轮历史执行缺少用量记录</span>}
    </div>
  }
  return <details className="execution-usage">
    <summary><Gauge size={16}/>{title}</summary>
    {current && usageRow('本轮',current)}
    {cumulative && usageRow('事项累计',cumulative)}
    {contextWindow && contextWindow>0 && current?.last_input_tokens!=null && <ContextUsageBar used={current.last_input_tokens} window={contextWindow}/>}
  </details>
}

// ContextUsageBar shows how close the last request's input is to the context
// window, with a marker at the 90% threshold where the backend auto-compacts
// older history. The window shown is the one compaction actually enforces
// (configured value, or the default when unknown).
const COMPACTION_THRESHOLD=0.9
function ContextUsageBar({used,window}:{used:number;window:number}) {
  const ratio=used/window
  const percent=ratio<0.01 ? '<1%' : `${usageNumber.format(Math.min(ratio,9.99)*100)}%`
  const atLimit=ratio>=COMPACTION_THRESHOLD
  return <div className={'context-usage'+(atLimit?' at-limit':'')}>
    <div className="context-usage-head">
      <span>上下文占用</span>
      <span>{percent} · {usageNumber.format(used)} / {usageNumber.format(window)} Token</span>
    </div>
    <div className="context-usage-track" role="progressbar" aria-valuenow={Math.round(Math.min(ratio,1)*100)} aria-valuemin={0} aria-valuemax={100}
      aria-label={`上下文占用 ${percent}，${atLimit?'已达压缩阈值':'压缩阈值 90%'}`}>
      <div className="context-usage-fill" style={{width:`${Math.min(ratio,1)*100}%`}}/>
      <div className="context-usage-mark" style={{left:`${COMPACTION_THRESHOLD*100}%`}}/>
    </div>
    <small>{atLimit?'已达 90%，较早对话将在下次请求时自动压缩':'到 90% 自动压缩较早对话'}</small>
  </div>
}

export const markdownPlugins = [remarkGfm]

export const markdownComponents: Components = {
  a: ({children,href}) => <a href={href} target="_blank" rel="noopener noreferrer">{children}</a>,
  table: ({children}) => <div className="md-table-wrap"><table>{children}</table></div>,
}

function stabilizeStreamingMarkdown(text: string): string {
  const lines = text.split('\n')
  let openFence: {char: string; len: number} | null = null
  for (const line of lines) {
    if (!openFence) {
      const match = line.match(/^ {0,3}(`{3,}|~{3,})(.*)$/)
      if (match) {
        const fence = match[1]
        const rest = match[2]
        if (!(fence[0] === '`' && rest.includes('`'))) {
          openFence = {char: fence[0], len: fence.length}
        }
      }
    } else {
      const closeMatch = line.match(/^ {0,3}(`{3,}|~{3,})[ \t]*$/)
      if (closeMatch && closeMatch[1][0] === openFence.char && closeMatch[1].length >= openFence.len) {
        openFence = null
      }
    }
  }
  return openFence ? `${text}\n${openFence.char.repeat(openFence.len)}` : text
}

export const FormattedText = memo(function FormattedText({text, framed=false, streaming=false}:{text:string;framed?:boolean;streaming?:boolean}) {
  const content = streaming ? stabilizeStreamingMarkdown(text) : text
  return <div className={framed?'response-text formatted':'formatted'}>
    <Markdown skipHtml disallowedElements={['img']} remarkPlugins={markdownPlugins} components={markdownComponents}>{content}</Markdown>
  </div>
})

export function MemoryReferences({items}:{items?:MemoryRef[]}) {
  if (!items?.length) return null
  return <details className="execution-usage memory-references">
    <summary><Bookmark size={16} aria-hidden="true"/>本轮参考了 {items.length} 条记忆</summary>
    <ul>{items.map(item=><li key={item.id}>
      <span title={item.title || item.id}>{item.title || '记忆 '+item.id.slice(0,8)}</span>
      <small>第 {item.revision} 版{item.current_revision===0 ? ' · 已删除' : item.current_revision>item.revision ? ' · 当前第 '+item.current_revision+' 版' : ''}</small>
    </li>)}</ul>
  </details>
}

function AssistantReasoning({text, hasLive}:{text:string; hasLive:boolean}) {
  const [open, setOpen] = useState(!hasLive)
  const userToggled = useRef(false)
  const prevHasLive = useRef(hasLive)
  const scrollRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (!prevHasLive.current && hasLive && !userToggled.current) {
      setOpen(false)
    }
    prevHasLive.current = hasLive
  }, [hasLive])

  useEffect(() => {
    const el = scrollRef.current
    if (!el || !open) return
    if (el.scrollHeight - el.scrollTop - el.clientHeight <= 36) {
      el.scrollTop = el.scrollHeight
    }
  }, [text, open])

  return <details className="assistant-reasoning" open={open} onToggle={e => {
    const next = e.currentTarget.open
    if (next !== open) {
      userToggled.current = true
      setOpen(next)
    }
  }}>
    <summary>{hasLive ? '思考过程' : '正在思考…'}</summary>
    <div className="assistant-reasoning-text" ref={scrollRef}>{text}</div>
  </details>
}

export type UserIdentity = {display_name?:string; username?:string; avatar?:string}

const MessageItem = memo(function MessageItem({
  message,
  messageID,
  focused,
  anchors,
  assistantLabel,
  userLabel,
  remember,
  matchingReply,
  replyCardContent,
  previewMaterials,
}: {
  message: Message
  messageID?: number
  focused: boolean
  anchors: boolean
  assistantLabel: ReactNode
  userLabel: ReactNode
  remember?: (id: number) => void
  matchingReply: boolean
  replyCardContent?: ReactNode
  previewMaterials?: Material[]
}) {
  return <div className={'message message-'+message.role+(focused?' source-message':'')} tabIndex={focused?-1:undefined} id={!anchors||messageID===undefined?undefined:'message-'+messageID}>
    <span>{message.role==='user'?userLabel:assistantLabel}</span>
    {message.role==='assistant'
      ? <div><FormattedText text={message.text}/>{message.incomplete&&<p className="message-incomplete">已停止 · 回复未完成</p>}<MemoryReferences items={message.memory_refs}/>{matchingReply&&replyCardContent}</div>
      : <div className="user-message"><MessageContent
          reference={message.task_ref?.task_id?<MessageReference label={'@'+(message.task_ref.title||message.task_ref.task_id)+' 只读引用'}/>:
            message.asset_query_task_id&&<MessageReference label={'资产查询 · '+message.asset_query_task_id}/>}
          body={message.text||message.replace_materials ? <>{message.text}{message.replace_materials&&<p className="muted">仅用本轮材料</p>}</> : undefined}
          attachments={Boolean(message.materials?.length)&&<MessageAttachments items={message.materials!} previewItems={previewMaterials}/>}/>
          {messageID!==undefined && remember && <button type="button" className="icon-button remember-message" aria-label="从这条消息新增记忆" title="新增记忆" onClick={()=>remember(messageID)}><BookmarkPlus size={16}/></button>}
        </div>}
  </div>
})

export function Messages({messages,live,liveReasoning,liveThinking,remember,anchors=true,replyCard,previewMaterials,assistantName,userAccount}:{
  messages:Message[];live?:string;liveReasoning?:string;liveThinking?:boolean;remember?:(id:number)=>void;anchors?:boolean;replyCard?:{runID:string;content:ReactNode};previewMaterials?:Material[];assistantName?:string;userAccount?:UserIdentity
}) {
  const located=useRef('')
  const group=useRef<HTMLDivElement>(null)
  const target=anchors&&/^#message-\d+$/.test(location.hash)?location.hash.slice(1):''
  useEffect(()=>{
    const element=target?group.current?.querySelector<HTMLElement>('#'+target):undefined
    if(!element){located.current='';return}
    if(located.current!==target) {
      located.current=target
      element.scrollIntoView({block:'center'})
      element.focus({preventScroll:true})
    }
  },[target,messages])
  const matchingReply=replyCard?[...messages].reverse().find(message=>message.role==='assistant'&&message.run_id===replyCard.runID):undefined
  const assistantLabel=useMemo(()=><ReactLabel assistantName={assistantName}/>,[assistantName])
  const userLabel=useMemo(()=><UserLabel account={userAccount}/>,[userAccount])
  return <div className="messages" ref={group}>{messages.map((message,index)=>{
    const messageID=message.id
    const focused=target==='message-'+messageID
    return <MessageItem
      key={messageID??index}
      message={message}
      messageID={messageID}
      focused={focused}
      anchors={anchors}
      assistantLabel={assistantLabel}
      userLabel={userLabel}
      remember={remember}
      matchingReply={message===matchingReply}
      replyCardContent={message===matchingReply?replyCard?.content:undefined}
      previewMaterials={previewMaterials}
    />
  })}{replyCard&&!matchingReply&&<div className="message message-assistant">
    <span>{assistantLabel}</span>{replyCard.content}
  </div>}{(live||liveReasoning||liveThinking)&&<div className="message message-assistant is-streaming"><span>{assistantLabel}</span><div className="assistant-stream-body">
    {liveReasoning&&<AssistantReasoning text={liveReasoning} hasLive={Boolean(live)}/>}
    {live?<div className="assistant-live-text"><FormattedText text={live} streaming/></div>:(!liveReasoning&&liveThinking&&<div className="assistant-thinking" role="status"><span className="thinking-dots" aria-hidden="true"><i/><i/><i/></span><span>正在思考…</span></div>)}
  </div></div>}</div>
}

function ReactLabel({assistantName}:{assistantName?:string}) {
  return <><img className="chat-avatar" src={clusbotIcon} alt=""/>{assistantName?<strong>{assistantName}</strong>:'助手'}</>
}

function UserLabel({account}:{account?:UserIdentity}) {
  const name=account?.display_name||account?.username||'你'
  return <>{name}{account&&<i className="message-user-avatar" aria-hidden="true">{account.avatar||Array.from(name)[0]}</i>}</>
}
