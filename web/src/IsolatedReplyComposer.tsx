import {useCallback,useEffect,useId,useRef,useState,type FormEvent} from 'react'
import {api,APIError,type Receipt} from './api'
import {changeComposerDraft,discardComposerDraft,hasPinnedIsolatedRequest,useComposerDraft,type IsolatedReplyRequest} from './composerDrafts'
import type {ComposerOptions,ReplyModel} from './ComposerUI'

export const isolatedDraftKey=(conversationID:string)=>'isolated:'+conversationID
const profileKey=(profile:ReplyModel)=>profile.profile_id+':'+profile.revision
const validID=(id?:string)=>Boolean(id&&/^[a-f\d]{32}$/.test(id))

export function IsolatedReplyComposer({conversationID,blocked,archived,onClose,onReceipt,initialModel,initialText}:{
  conversationID?:string;blocked:boolean;archived:boolean;onClose:()=>void;onReceipt:(receipt:Receipt,request:IsolatedReplyRequest)=>void;initialModel?:ReplyModel;initialText?:string
}){
  const draftKey=isolatedDraftKey(conversationID||'new')
  const {text,isolatedRequest}=useComposerDraft(draftKey)
  const [profiles,setProfiles]=useState<ReplyModel[]>()
  const [profileError,setProfileError]=useState('')
  const [choice,setChoice]=useState(initialModel?profileKey(initialModel):'')
  const [consent,setConsent]=useState<{profile:string;text:string}>()
  const [error,setError]=useState('')
  const [busy,setBusy]=useState(false)
  const heading=useRef<HTMLHeadingElement>(null)
  const locked=useRef(false)
  const mounted=useRef(false)
  const generation=useRef(0)
  const title=useId()
  const listProfiles=useCallback(async()=>{
    const token=++generation.current
    setProfileError('')
    try{
      const result=await api<ComposerOptions>('/assistant/composer-options')
      if(token===generation.current)setProfiles(Array.isArray(result.reply_models)?result.reply_models.filter(p=>validID(p.profile_id)&&Number.isSafeInteger(p.revision)&&p.revision>0):[])
    }catch(e){if(token===generation.current){setProfiles(undefined);setProfileError(e instanceof Error?e.message:'无法读取已保存档案')}}
  },[])
  useEffect(()=>{
    mounted.current=true
    heading.current?.focus()
    if(initialText)changeComposerDraft(draftKey,previous=>!previous.text&&!previous.isolatedRequest?{text:initialText}:{})
    void listProfiles()
    return()=>{mounted.current=false;generation.current++}
  },[listProfiles,draftKey])
  const profile=profiles?.find(p=>profileKey(p)===choice)
  const agreed=Boolean(profile&&consent?.profile===choice&&consent.text===text)
  const validText=Boolean(text.trim()&&!text.trim().startsWith('/')&&Array.from(text).length<=16000)
  const maySend=Boolean(!blocked&&!archived&&!isolatedRequest&&!busy&&profile?.available&&validText&&agreed)
  async function send(request:IsolatedReplyRequest){
    if(locked.current)return
    locked.current=true;setBusy(true);setError('')
    try{
      const out=await api<Receipt & {status?:string}>(conversationID?'/conversations/'+encodeURIComponent(conversationID)+'/isolated-replies':'/conversations/isolated-replies',request)
      if(!validID(out?.conversation_id)||(conversationID&&out.conversation_id!==conversationID)||!validID(out.run_id)||out.task_id||out.model_pending||!['queued','running','completed','failed','cancelled','interrupted'].includes(out.status??''))throw new Error('未收到可核对的接收回执')
      // Do not change a newer draft if the conversation was closed or reset.
      const stored=hasPinnedIsolatedRequest(draftKey,request.client_request_id)
      if(stored&&mounted.current){discardComposerDraft(draftKey);setConsent(undefined);onReceipt(out,request);onClose()}
    }catch(e){
      if(e instanceof APIError&&['isolated_reply_unavailable','invalid_isolated_reply','conversation_conflict'].includes(e.code??'')){
        if(hasPinnedIsolatedRequest(draftKey,request.client_request_id)){
          changeComposerDraft(draftKey,()=>({isolatedRequest:undefined}));setConsent(undefined);void listProfiles()
        }
        setError(e.message)
      }else if(e instanceof APIError&&e.code==='run_active'){
        setError('会话正在处理另一条消息。请求保持不变，结束后可再次核对。')
      }else{
        setError('尚未核实该请求是否已受理。未自动重发；可用同一请求核对，内容和档案版本不变。')
      }
    }finally{locked.current=false;setBusy(false)}
  }
  function submit(e:FormEvent){
    e.preventDefault()
    if(!maySend||!profile||locked.current)return
    const request:IsolatedReplyRequest={mode:'only_this_message',profile_id:profile.profile_id,profile_revision:profile.revision,
      confirm_current_text_egress:true,client_request_id:crypto.randomUUID(),text}
    changeComposerDraft(draftKey,()=>({isolatedRequest:request}))
    void send(request)
  }
  return <form className="composer isolated-reply" aria-labelledby={title} onSubmit={submit}>
    <header className="settings-section-heading"><h2 id={title} ref={heading} tabIndex={-1}>单独问一个模型</h2>
      <button type="button" className="text-button" disabled={busy} onClick={onClose}>返回普通输入</button></header>
    <p>只发送这里写下的这一条文字，不发送对话历史、文件、任务引用、Skill 或 MCP。回复显示在本会话，但不自动带入后续模型上下文。</p>
    {!isolatedRequest&&<div className="settings-fields">
      <label className="settings-field"><span>模型档案</span>
        <select name="profile" value={choice} disabled={busy||blocked||archived||!profiles} onChange={e=>{setChoice(e.currentTarget.value);setConsent(undefined);setError('')}}>
          <option value="">选择已保存的档案</option>
          {choice&&!profile&&<option value={choice} disabled>此前档案版本已不可用</option>}
          {profiles?.map(p=><option value={profileKey(p)} key={profileKey(p)} disabled={!p.available}>
            {p.name} · {p.model_id} · v{p.revision}{!p.available?' · 凭据不可用':''}</option>)}
        </select></label>
      {profile&&<p className="muted">档案 v{profile.revision} · {profile.available?'凭据可用，未验证远端连接':'凭据不可用'}</p>}
      {profiles&&!profiles.some(p=>p.available)&&<p className="muted">没有凭据可用的模型档案。可到设置中的模型档案保存一份配置。</p>}
      {!profiles&&!profileError&&<p role="status">正在读取模型档案…</p>}
      {profileError&&<p className="error" role="alert">{profileError} <button type="button" className="text-button" disabled={busy} onClick={()=>void listProfiles()}>重新读取</button></p>}
    </div>}
    <label className="settings-field"><span>仅发送这条文字</span>
      <textarea name="isolated_text" value={isolatedRequest?.text??text} maxLength={16000} rows={3} readOnly={busy||Boolean(isolatedRequest)}
        onChange={e=>{const text=e.currentTarget.value;changeComposerDraft(draftKey,()=>({text}));setConsent(undefined);setError('')}}/>
    </label>
    {!isolatedRequest&&text.trim().startsWith('/')&&<p role="status">这里只接受文字提问，不执行斜杠命令。</p>}
    {isolatedRequest?<div className="settings-actions">
      <p className="muted">此请求等待核对。如果服务器已受理，会返回原回执；如果尚未受理，重试可能发送原文字。不会换模型或添加其他内容。</p>
      <button type="button" className="outline" disabled={busy} onClick={()=>void send(isolatedRequest)}>核对并重试原请求</button>
    </div>:<>
      {profile&&<label className="model-egress">
        <input type="checkbox" checked={agreed} disabled={busy||blocked||archived||!validText||!profile.available}
          onChange={e=>setConsent(e.currentTarget.checked?{profile:choice,text}:undefined)}/>
        <span>同意将这条文字发送给 {profile.name} · {profile.model_id}（档案 v{profile.revision}）。文字或档案变更时需重新同意。</span>
      </label>}
      {(blocked||archived)&&<p role="status">{archived?'会话已归档，不能发起新回复。':'会话正在处理，不能发起新回复。'}</p>}
      <div className="settings-actions"><button type="submit" className="primary" disabled={!maySend}>仅发送这条文字</button></div>
    </>}
    {busy&&<p role="status">正在核对接收状态…</p>}{error&&<p className="error" role="alert">{error}</p>}
  </form>
}
