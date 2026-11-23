import {useEffect,useId,useRef,useState,type FormEvent} from 'react'
import {Check,ListChecks} from 'lucide-react'
import {api,APIError} from './api'
import './AssistantRecommendation.css'

type Option={id:string;label:string;detail?:string;recommended?:boolean}
type Proposal={proposal_id:string;title:string;summary?:string;options:Option[];origin:'model';execution:'not_evaluated';expires_at_unix_ms:number}
const object=(value:unknown):value is Record<string,unknown>=>typeof value==='object'&&value!==null&&!Array.isArray(value)
const only=(value:Record<string,unknown>,keys:string[])=>Object.keys(value).every(key=>keys.includes(key))
const text=(value:unknown,max:number):value is string=>typeof value==='string'&&value.trim().length>0&&Array.from(value).length<=max

// The model supplies text only. IDs, destinations, and all action semantics are constrained here.
export function parseRecommendation(value:unknown):Proposal|undefined {
  if(!object(value)||!only(value,['proposal_id','title','summary','options','origin','execution','expires_at_unix_ms'])||
    typeof value.proposal_id!=='string'||!/^[a-f0-9]{48}$/.test(value.proposal_id)||!text(value.title,80)||
    (value.summary!==undefined&&!text(value.summary,240))||value.origin!=='model'||value.execution!=='not_evaluated'||
    typeof value.expires_at_unix_ms!=='number'||!Number.isSafeInteger(value.expires_at_unix_ms)||value.expires_at_unix_ms<0||
    !Array.isArray(value.options)||value.options.length<1||value.options.length>4)return
  const ids=new Set<string>()
  for(const option of value.options){
    if(!object(option)||!only(option,['id','label','detail','recommended'])||typeof option.id!=='string'||!/^[a-f0-9]{32}$/.test(option.id)||ids.has(option.id)||
      !text(option.label,80)||(option.detail!==undefined&&!text(option.detail,240))||
      (option.recommended!==undefined&&typeof option.recommended!=='boolean'))return
    ids.add(option.id)
  }
  return value as Proposal
}

export function AssistantRecommendation({result}:{result:unknown}) {
  const proposal=parseRecommendation(result)
  if(!proposal)return <article className="assistant-card"><p role="alert">建议内容无效，无法确认。请重新生成建议。</p></article>
  return <RecommendationForm key={proposal.proposal_id} proposal={proposal}/>
}

function RecommendationForm({proposal}:{proposal:Proposal}) {
  const initial=proposal.options.find(option=>option.recommended)?.id??proposal.options[0].id
  const [selected,setSelected]=useState(initial)
  const [state,setState]=useState<'ready'|'pending'|'unknown'|'confirmed'|'rejected'>('ready')
  const [expired,setExpired]=useState(()=>Date.now()>=proposal.expires_at_unix_ms)
  const lock=useRef(false)
  const formID=useId()
  useEffect(()=>{
    const remaining=proposal.expires_at_unix_ms-Date.now()
    if(remaining<=0){setExpired(true);return}
    const timer=window.setTimeout(()=>setExpired(true),Math.min(remaining,2147483647))
    return()=>window.clearTimeout(timer)
  },[proposal.expires_at_unix_ms])
  async function confirm(event:FormEvent<HTMLFormElement>){
    event.preventDefault()
    if(lock.current||state==='confirmed'||state==='rejected'||(state==='ready'&&(expired||Date.now()>=proposal.expires_at_unix_ms))) {
      if(state==='ready'&&Date.now()>=proposal.expires_at_unix_ms)setExpired(true)
      return
    }
    lock.current=true;setState('pending')
    try{
      const receipt=await api<unknown>('/assistant/recommendations/'+encodeURIComponent(proposal.proposal_id)+'/selection',{option_id:selected})
      if(!object(receipt)||receipt.proposal_id!==proposal.proposal_id||receipt.option_id!==selected||receipt.state!=='confirmed'||receipt.execution!=='not_evaluated')throw new Error('invalid receipt')
      setState('confirmed')
    }catch(error){
      setState(error instanceof APIError&&error.status!==undefined&&error.status>=400&&error.status<500?'rejected':'unknown')
    }finally{lock.current=false}
  }
  const status=state==='confirmed'?'已确认选择':state==='unknown'?'确认结果未知':state==='rejected'?'无法确认':expired&&state==='ready'?'建议已失效':'等待确认'
  return <article className="assistant-card assistant-recommendation">
    <div className="assistant-card-title"><h3><ListChecks size={15} aria-hidden="true"/>{proposal.title}</h3><span className="assistant-expiry">{status}</span></div>
    <p className="assistant-recommendation-origin">模型建议{proposal.summary&&<> · {proposal.summary}</>}</p>
    <form aria-labelledby={formID} onSubmit={event=>void confirm(event)}>
      <fieldset disabled={state!=='ready'||expired}>
        <legend id={formID}>选择建议</legend>
        {proposal.options.map(option=><label key={option.id} className="assistant-recommendation-option" data-selected={selected===option.id}>
          <input type="radio" name="option_id" value={option.id} checked={selected===option.id} onChange={()=>setSelected(option.id)}/>
          <span className="assistant-recommendation-copy"><strong>{option.label}</strong>{option.detail&&<span>{option.detail}</span>}</span>
          {option.recommended&&<span className="assistant-recommendation-badge">推荐</span>}
        </label>)}
      </fieldset>
      <p className="assistant-recommendation-state" role="status">{state==='confirmed'?'选择已记录。建议未绑定可核验的动作和目标，无法核验执行能力与权限。':<>{state==='unknown'?'未收到有效回执，可重试确认同一选择。':state==='rejected'?'建议可能已失效或状态不匹配，请重新生成建议。':expired?'请重新生成建议。':''} 执行能力与权限尚未核验。</>}</p>
      {(state==='ready'||state==='pending'||state==='unknown')&&<footer><button className="primary" type="submit" disabled={state==='pending'||(state==='ready'&&expired)}><Check size={13} aria-hidden="true"/>{state==='pending'?'正在确认…':state==='unknown'?'重试确认':'确认选择'}</button></footer>}
    </form>
  </article>
}
