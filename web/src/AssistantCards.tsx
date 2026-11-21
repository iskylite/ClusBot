import {useEffect,useId,useRef,useState} from 'react'
import {api,APIError} from './api'
import {assistantDetails,isAssistantItem,parseSurfaces,previewText,trustedForm,type AssistantItem,type DetailRow,type Surface} from './assistantSurface'

function SurfaceView({surface,expanded,detailsID,toggle,canToggle}:{surface:Surface;expanded:boolean;detailsID:string;toggle:()=>void;canToggle:boolean}) {
  function firstText(id:string):string|undefined {
    const node=surface.nodes.get(id)!
    if(node.component==='Text')return id
    if(node.component==='Button')return
    for(const child of 'children' in node?node.children:[node.child]) {
      const match=firstText(child)
      if(match)return match
    }
  }
  const headingID=firstText('root')
  function render(id:string):React.ReactNode {
    const node=surface.nodes.get(id)!
    if(node.component==='Text')return id===headingID?<h3 key={id}>{node.text}</h3>:<p key={id}>{node.text}</p>
    if(node.component==='Button') {
      const label=surface.nodes.get(node.child)
      return <button key={id} type="button" className="text-button assistant-detail-toggle" aria-expanded={expanded} aria-controls={detailsID} disabled={!canToggle} onClick={toggle}>
        {expanded?'收起详情':label?.component==='Text'?label.text:'展开详情'}
      </button>
    }
    if(node.component==='Card')return <div key={id} className="assistant-surface-card">{render(node.child)}</div>
    return <div key={id} className={node.component==='Row'?'assistant-surface-row':'assistant-surface-column'}>{node.children.map(render)}</div>
  }
  return <>{render('root')}</>
}

function LocalDetails({items,navigate}:{items:DetailRow[];navigate:(path:string)=>void}) {
  return <ul className="assistant-details">{items.map((item,index)=><li key={index}>
    <dl>{item.fields.map(field=><div key={field.label}><dt>{field.label}</dt><dd>{field.value}</dd></div>)}</dl>
    {item.taskID&&<button type="button" className="text-button" onClick={()=>navigate('/tasks/'+item.taskID)}>查看任务</button>}
  </li>)}</ul>
}

export function AssistantCard({item,navigate}:{item:AssistantItem;navigate:(path:string)=>void}) {
  const [current,setCurrent]=useState(item)
  const [expanded,setExpanded]=useState(false)
  const [pending,setPending]=useState(false)
  const [outcome,setOutcome]=useState('')
  const [expired,setExpired]=useState(false)
  const lock=useRef(false)
  const mounted=useRef(true)
  const detailsID=useId()
  useEffect(()=>{mounted.current=true;return()=>{mounted.current=false}},[])
  const preview=!Array.isArray(current)&&current.state==='confirmation_required'?current:undefined
  useEffect(()=>{
    if(!preview)return
    const remaining=preview.expires_at_unix_ms-Date.now()
    if(remaining<=0){setExpired(true);return}
    const timer=window.setTimeout(()=>setExpired(true),Math.min(remaining,2147483647))
    return()=>window.clearTimeout(timer)
  },[preview])
  async function decide(confirm:boolean) {
    if(!preview||lock.current||expired)return
    if(Date.now()>=preview.expires_at_unix_ms){setExpired(true);return}
    if(confirm&&!previewText({input:preview.input,...(preview.target===undefined?{}:{target:preview.target})}).safe)return
    lock.current=true;setPending(true);setOutcome('')
    try {
      if(confirm) {
        const result=await api<unknown>('/assistant/confirm',{confirmation_id:preview.confirmation_id})
        if(!isAssistantItem(result) || Array.isArray(result) || result.state!=='completed')throw new Error('未收到有效操作回执，请核对目标状态')
        if(mounted.current){setCurrent(result);setExpanded(false)}
      } else {
        await api('/assistant/confirm/'+encodeURIComponent(preview.confirmation_id),undefined,'DELETE')
        if(mounted.current)setOutcome('已取消')
      }
    } catch(error) {
      if(mounted.current)setOutcome(error instanceof APIError?error.message:'未收到操作回执，请核对目标状态后重新预览')
    } finally {
      if(mounted.current){setPending(false);setExpired(true)}
    }
  }
  if(Array.isArray(current))return <article className="assistant-card">
    <h3>助手工具</h3><p>输入 /助手 后接问题，或使用工具名称和参数。</p>
    <details><summary>查看工具目录（{current.length}）</summary><ul className="assistant-tool-list">{current.map(tool=><li key={tool.name}>
      <strong>{tool.label||tool.name}</strong><code>{tool.name}</code><p>{tool.description}</p>{tool.params?.length?<small>需要：{tool.params.join('、')}</small>:null}
    </li>)}</ul></details>
  </article>
  const surfaces='surface' in current?parseSurfaces(current.surface):undefined
  const details=current.state==='completed'?assistantDetails(current.tool,current.result):[]
  const hasToggle=surfaces?.some(surface=>[...surface.nodes.values()].some(node=>node.component==='Button'))
  const request=preview?previewText({input:preview.input,...(preview.target===undefined?{}:{target:preview.target})}):undefined
  const result=current.state==='completed'?previewText(current.result):undefined
  const form=current.state==='form_required'?trustedForm(current.tool,current.form):undefined
  return <article className="assistant-card">
    {surfaces?surfaces.map(surface=><SurfaceView key={surface.id} surface={surface} expanded={expanded} detailsID={detailsID} canToggle={details.length>0} toggle={()=>setExpanded(value=>!value)}/>):
      <h3>{current.state==='completed'?'工具已返回结果':current.state==='confirmation_required'?'请核对操作后决定是否确认':'此操作需要在设置中完成'}</h3>}
    {current.state==='completed'&&<>
      {details.length>0&&!hasToggle&&<button type="button" className="text-button assistant-detail-toggle" aria-expanded={expanded} aria-controls={detailsID} onClick={()=>setExpanded(value=>!value)}>{expanded?'收起详情':'展开详情'}</button>}
      {details.length>0&&<div id={detailsID} hidden={!expanded}><LocalDetails items={details} navigate={navigate}/></div>}
      {!details.length&&result&&<details className="assistant-result"><summary>查看返回数据</summary><pre>{result.text}</pre></details>}
    </>}
    {preview&&request&&<>
      <dl className="assistant-preview-fields">{request.fields.map((field,index)=><div key={index}><dt>{field.label}</dt><dd>{field.value}</dd></div>)}</dl>
      <details><summary>查看完整参数</summary><pre className="assistant-preview">{request.text}</pre></details>
      <p className="muted">有效至 {new Intl.DateTimeFormat('zh-CN',{hour:'2-digit',minute:'2-digit',second:'2-digit'}).format(preview.expires_at_unix_ms)}</p>
      {!request.safe&&<p className="error">内容含敏感字段或无法完整显示，请在对应页面处理。</p>}
      {outcome?<p role="status">{outcome}</p>:expired?<p role="status">确认已失效，请重新预览。</p>:
        <footer><button type="button" className="outline" disabled={pending} onClick={()=>void decide(false)}>取消</button>
          <button type="button" className="primary" disabled={pending||!request.safe} onClick={()=>void decide(true)}>{pending?'正在处理…':'确认操作'}</button></footer>}
    </>}
    {current.state==='form_required'&&(form?<button type="button" className="outline" onClick={()=>navigate(form)}>打开设置</button>:<p className="muted">当前版本暂无可用的设置入口。</p>)}
  </article>
}
