import {useEffect,useRef} from 'react'
import {Bookmark,BookmarkPlus,Gauge} from 'lucide-react'
import Markdown from 'react-markdown'
import {MaterialTypeIcon} from './MaterialPicker'
import type {ExecutionUsage,Material,MemoryRef,Message} from './api'

const usageNumber = new Intl.NumberFormat('zh-CN', {maximumFractionDigits:1})

function MessageMaterials({items}:{items:Material[]}) {
  const card=(item:Material)=>{
    const filename=item.upload?.name??item.path.split('/').pop()??item.path
    return <li className="message-material" key={item.id} title={filename}>
      <MaterialTypeIcon material={item} size={20}/>
      <span><strong>{filename}</strong>{item.upload&&item.upload.parts>1&&<small>第 {item.upload.part} / {item.upload.parts} 段</small>}</span>
    </li>
  }
  return <div className="message-attachments" aria-label="本条消息附件">
    <ul className="message-attachment-grid">{items.slice(0,3).map(card)}</ul>
    {items.length>3&&<details><summary>另有 {items.length-3} 个附件</summary>
      <ul className="message-attachment-grid">{items.slice(3).map(card)}</ul></details>}
  </div>
}

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

export function FormattedText({text, framed=false}:{text:string;framed?:boolean}) {
  return <div className={framed?'response-text formatted':'formatted'}>
    <Markdown skipHtml disallowedElements={['img']} components={{
      a: ({children,href}) => <a href={href} target="_blank" rel="noopener noreferrer">{children}</a>,
    }}>{text}</Markdown>
  </div>
}

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

export function Messages({messages,live,remember,anchors=true}:{messages:Message[];live?:string;remember?:(id:number)=>void;anchors?:boolean}) {
  const located=useRef('')
  const target=anchors&&/^#message-\d+$/.test(location.hash)?location.hash.slice(1):''
  useEffect(()=>{
    if(!target){located.current='';return}
    const element=document.getElementById(target)
    if(element&&located.current!==target) {
      located.current=target
      element.scrollIntoView({block:'center'})
      element.focus({preventScroll:true})
    }
  },[target,messages])
  return <div className="messages">{messages.map((message,index)=>{
    const messageID=message.id
    const focused=target==='message-'+messageID
    return <div className={'message'+(focused?' source-message':'')} key={messageID??index} tabIndex={focused?-1:undefined} id={!anchors||messageID===undefined?undefined:'message-'+messageID}><span>{message.role==='user'?'你':'助手'}</span>{message.role==='assistant'?<div><FormattedText text={message.text}/>{message.incomplete&&<p className="message-incomplete">已停止 · 回复未完成</p>}<MemoryReferences items={message.memory_refs}/></div>:<div className="user-message"><div>{message.text}{message.replace_materials&&<p className="muted">仅用本轮材料</p>}{Boolean(message.materials?.length)&&<MessageMaterials items={message.materials!}/>}</div>
      {messageID!==undefined && remember && <button type="button" className="icon-button remember-message" aria-label="从这条消息新增记忆" title="新增记忆" onClick={()=>remember(messageID)}><BookmarkPlus size={16}/></button>}</div>}</div>
  })}{live&&<div className="message"><span>助手</span><FormattedText text={live}/></div>}</div>
}
